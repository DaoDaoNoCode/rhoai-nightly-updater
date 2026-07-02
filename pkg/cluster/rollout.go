package cluster

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

var rolloutNamespaces = []string{"redhat-ods-operator", "redhat-ods-applications"}

type replicaSetInfo struct {
	Name     string
	Desired  int
	Ready    int
	Revision string
	Labels   map[string]string
}

func AssistRollout(c *Client) (*types.OperationResponse, error) {
	logs := []string{}
	assisted := 0

	for _, ns := range rolloutNamespaces {
		deps, err := getDeployments(c, ns)
		if err != nil {
			logs = append(logs, fmt.Sprintf("Warning: could not list deployments in %s: %v", ns, err))
			continue
		}

		for _, dep := range deps {
			actions, scaled, err := assistDeploymentRollout(c, ns, dep.Name)
			if err != nil {
				logs = append(logs, fmt.Sprintf("Warning: %s/%s: %v", ns, dep.Name, err))
				continue
			}
			if len(actions) > 0 {
				logs = append(logs, actions...)
			}
			if scaled {
				assisted++
			}
		}
	}

	if assisted == 0 {
		return &types.OperationResponse{
			Success: true,
			Message: "No stuck rollouts detected. All deployments are either progressing normally or have no Pending pods blocked by old ReplicaSets.",
			Logs:    logs,
		}, nil
	}

	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "assist-rollout",
		Detail:    fmt.Sprintf("assisted %d deployment(s)", assisted),
		Success:   true,
	})

	return &types.OperationResponse{
		Success: true,
		Message: fmt.Sprintf("Assisted %d stuck rollout(s). Patched maxUnavailable strategy to unblock rollouts.", assisted),
		Logs:    logs,
	}, nil
}

func assistDeploymentRollout(c *Client, namespace, deployName string) ([]string, bool, error) {
	// Scope the RS query to this deployment using the app label, avoiding
	// fetching every ReplicaSet in the namespace. RHOAI operator deployments
	// set app=<deploymentName> on their pods and ReplicaSets.
	rsPath := fmt.Sprintf("/apis/apps/v1/namespaces/%s/replicasets?labelSelector=app=%s", namespace, deployName)
	body, _, err := c.get(rsPath)
	if err != nil {
		return nil, false, fmt.Errorf("list replicasets: %w", err)
	}

	var rsList struct {
		Items []struct {
			Metadata struct {
				Name            string            `json:"name"`
				Labels          map[string]string `json:"labels"`
				Annotations     map[string]string `json:"annotations"`
				OwnerReferences []struct {
					Kind string `json:"kind"`
					Name string `json:"name"`
				} `json:"ownerReferences"`
			} `json:"metadata"`
			Spec struct {
				Replicas *int `json:"replicas"`
			} `json:"spec"`
			Status struct {
				Replicas      int `json:"replicas"`
				ReadyReplicas int `json:"readyReplicas"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &rsList); err != nil {
		return nil, false, fmt.Errorf("parse replicasets: %w", err)
	}

	// Filter RSs owned by this deployment
	var ownedRS []replicaSetInfo
	for _, rs := range rsList.Items {
		ownedByDeploy := false
		for _, ref := range rs.Metadata.OwnerReferences {
			if ref.Kind == "Deployment" {
				if ref.Name == deployName {
					ownedByDeploy = true
				}
			}
		}
		if !ownedByDeploy {
			continue
		}

		desired := 0
		if rs.Spec.Replicas != nil {
			desired = *rs.Spec.Replicas
		}
		if desired == 0 && rs.Status.Replicas == 0 {
			continue
		}

		ownedRS = append(ownedRS, replicaSetInfo{
			Name:     rs.Metadata.Name,
			Desired:  desired,
			Ready:    rs.Status.ReadyReplicas,
			Revision: rs.Metadata.Annotations["deployment.kubernetes.io/revision"],
			Labels:   rs.Metadata.Labels,
		})
	}

	if len(ownedRS) < 2 {
		return nil, false, nil
	}

	// Find the newest RS (highest revision) and old RSs.
	// Parse revisions as integers; Kubernetes revision annotations are numeric
	// strings and string comparison breaks once revisions exceed single digits
	// (e.g. "9" > "10" lexicographically).
	var newest *replicaSetInfo
	var oldRS []replicaSetInfo
	newestRev := 0
	for i := range ownedRS {
		rev, err := strconv.Atoi(ownedRS[i].Revision)
		if err != nil || rev == 0 {
			slog.Warn("assist-rollout: skipping RS with missing/invalid revision annotation",
				"replicaSet", ownedRS[i].Name,
				"revision", ownedRS[i].Revision,
			)
			continue
		}
		if newest == nil || rev > newestRev {
			if newest != nil {
				oldRS = append(oldRS, *newest)
			}
			newest = &ownedRS[i]
			newestRev = rev
		} else {
			oldRS = append(oldRS, ownedRS[i])
		}
	}

	if newest == nil || newest.Desired == 0 {
		return nil, false, nil
	}

	// Detect stuck: new RS has desired > ready, AND there are old RSs with ready pods
	if newest.Ready >= newest.Desired {
		return nil, false, nil
	}

	// Check if new RS has Pending pods (scheduling failure) and collect their names
	var pendingPodName string
	podPath := fmt.Sprintf("/api/v1/namespaces/%s/pods?labelSelector=pod-template-hash=%s",
		namespace, newest.Labels["pod-template-hash"])
	podBody, _, podErr := c.get(podPath)
	if podErr == nil {
		var podList struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Status struct {
					Phase string `json:"phase"`
				} `json:"status"`
			} `json:"items"`
		}
		if json.Unmarshal(podBody, &podList) == nil {
			for _, pod := range podList.Items {
				if pod.Status.Phase == "Pending" {
					pendingPodName = pod.Metadata.Name
					break
				}
			}
		}
	}

	if pendingPodName == "" {
		return nil, false, nil
	}

	// Check that old RSs actually have ready pods holding resources
	hasOldReady := false
	for _, old := range oldRS {
		if old.Ready > 0 {
			hasOldReady = true
			break
		}
	}
	if !hasOldReady {
		return nil, false, nil
	}

	// We have a stuck rollout: new RS has Pending pods while old RSs still hold
	// resources. Patch the Deployment's maxUnavailable to 1 so K8s is allowed to
	// terminate an old pod, then delete the Pending pod to trigger re-evaluation.
	// The RHOAI operator will eventually reconcile the strategy back.
	var logs []string
	assisted := false
	logs = append(logs, fmt.Sprintf("Stuck rollout detected: %s/%s (new RS %s: %d/%d ready, pod %s Pending)",
		namespace, deployName, newest.Name, newest.Ready, newest.Desired, pendingPodName))

	slog.Info("assist-rollout: patching Deployment maxUnavailable to unblock rollout",
		"namespace", namespace,
		"deployment", deployName,
		"pendingPod", pendingPodName,
	)

	// Step 1: Patch the Deployment strategy to allow termination of old pods
	patchData, _ := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{
			"strategy": map[string]interface{}{
				"rollingUpdate": map[string]interface{}{
					"maxUnavailable": 1,
				},
			},
		},
	})
	depPath := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", namespace, deployName)
	_, _, err = c.strategicPatch(depPath, patchData)
	if err != nil {
		logs = append(logs, fmt.Sprintf("  Warning: failed to patch maxUnavailable on %s: %v", deployName, err))
		return logs, false, nil
	}

	// Step 2: Delete the Pending pod so K8s re-evaluates scheduling with the new strategy
	deletePath := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", namespace, pendingPodName)
	_, delErr := c.delete(deletePath)
	if delErr != nil {
		logs = append(logs, fmt.Sprintf("  Warning: failed to delete Pending pod %s: %v", pendingPodName, delErr))
		// Still count as assisted since the strategy patch alone may help
	} else {
		logs = append(logs, fmt.Sprintf("  Deleted Pending pod %s", pendingPodName))
	}

	assisted = true
	logs = append(logs, fmt.Sprintf("  Patched maxUnavailable to 1 and deleted Pending pod to unblock rollout"))

	return logs, assisted, nil
}
