package cluster

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// GetDebugInfo collects pod details from RHOAI-related namespaces for debugging.
// If some namespace queries fail, partial data is returned with warnings.
// If all namespace queries fail, an error is returned.
func GetDebugInfo(c *Client) (*types.DebugResponse, error) {
	resp := &types.DebugResponse{}
	var warnings []string
	failCount := 0
	totalNamespaces := 3

	// Get pods from redhat-ods-operator
	pods, err := getPodsInNamespace(c, "redhat-ods-operator")
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("failed to list pods in redhat-ods-operator: %v", err))
		failCount++
	} else {
		resp.OperatorPods = pods
	}

	// Get pods from redhat-ods-applications
	pods, err = getPodsInNamespace(c, "redhat-ods-applications")
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("failed to list pods in redhat-ods-applications: %v", err))
		failCount++
	} else {
		resp.ApplicationPods = pods
	}

	// Get catalog pods from openshift-marketplace (filtered to rhoai)
	allMarketplace, err := getPodsInNamespace(c, "openshift-marketplace")
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("failed to list pods in openshift-marketplace: %v", err))
		failCount++
	} else {
		for _, p := range allMarketplace {
			if strings.Contains(p.Name, "rhoai") {
				resp.MarketplacePods = append(resp.MarketplacePods, p)
			}
		}
	}

	if failCount == totalNamespaces {
		return nil, fmt.Errorf("failed to list pods in all namespaces: %s", strings.Join(warnings, "; "))
	}

	resp.Warnings = warnings
	return resp, nil
}

func getPodsInNamespace(c *Client, namespace string) ([]types.PodInfo, error) {
	path := namespacedPath("v1", "pods", namespace, "")
	body, _, err := c.get(path)
	if err != nil {
		return nil, fmt.Errorf("API request failed: %w", err)
	}

	var podList struct {
		Items []struct {
			Metadata struct {
				Name              string            `json:"name"`
				Namespace         string            `json:"namespace"`
				CreationTimestamp string            `json:"creationTimestamp"`
				Labels            map[string]string `json:"labels"`
				OwnerReferences   []struct {
					Kind string `json:"kind"`
				} `json:"ownerReferences"`
			} `json:"metadata"`
			Spec struct {
				NodeName   string `json:"nodeName"`
				Containers []struct {
					Name  string `json:"name"`
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
			Status struct {
				Phase      string `json:"phase"`
				Conditions []struct {
					Type    string `json:"type"`
					Status  string `json:"status"`
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"conditions"`
				ContainerStatuses []struct {
					Name         string `json:"name"`
					Ready        bool   `json:"ready"`
					RestartCount int    `json:"restartCount"`
					ImageID      string `json:"imageID"`
					State        struct {
						Running    *struct{} `json:"running"`
						Waiting    *struct {
							Reason string `json:"reason"`
						} `json:"waiting"`
						Terminated *struct {
							Reason string `json:"reason"`
						} `json:"terminated"`
					} `json:"state"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}

	if err := json.Unmarshal(body, &podList); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	var pods []types.PodInfo
	for _, item := range podList.Items {
		pod := types.PodInfo{
			Name:      item.Metadata.Name,
			Namespace: item.Metadata.Namespace,
			Phase:     item.Status.Phase,
			Node:      item.Spec.NodeName,
		}

		if len(item.Spec.Containers) > 0 {
			pod.Image = item.Spec.Containers[0].Image
		}

		// Aggregate container statuses
		allReady := true
		totalRestarts := 0
		for _, cs := range item.Status.ContainerStatuses {
			state := "running"
			reason := ""
			if cs.State.Waiting != nil {
				state = "waiting"
				reason = cs.State.Waiting.Reason
			} else if cs.State.Terminated != nil {
				state = "terminated"
				reason = cs.State.Terminated.Reason
			}

			pod.Containers = append(pod.Containers, types.ContainerInfo{
				Name:     cs.Name,
				Ready:    cs.Ready,
				Restarts: cs.RestartCount,
				State:    state,
				Reason:   reason,
			})

			if !cs.Ready {
				allReady = false
			}
			totalRestarts += cs.RestartCount
		}

		// Fill missing container names from spec if status is empty
		if len(pod.Containers) == 0 {
			for _, c := range item.Spec.Containers {
				pod.Containers = append(pod.Containers, types.ContainerInfo{
					Name: c.Name,
				})
			}
		}

		pod.Ready = allReady && len(item.Status.ContainerStatuses) > 0
		pod.Restarts = totalRestarts

		if len(item.Status.ContainerStatuses) > 0 {
			pod.ImageID = item.Status.ContainerStatuses[0].ImageID
		}

		pod.Age = formatAge(item.Metadata.CreationTimestamp)
		pod.Labels = item.Metadata.Labels
		pod.PodTemplateHash = item.Metadata.Labels["pod-template-hash"]
		if len(item.Metadata.OwnerReferences) > 0 {
			pod.OwnerKind = item.Metadata.OwnerReferences[0].Kind
		}

		// Extract scheduling failure reason from pod conditions
		if item.Status.Phase == "Pending" {
			for _, cond := range item.Status.Conditions {
				if cond.Type == "PodScheduled" && cond.Status == "False" && cond.Reason != "" {
					pod.SchedulingReason = cond.Message
					if pod.SchedulingReason == "" {
						pod.SchedulingReason = cond.Reason
					}
					break
				}
			}
		}

		pods = append(pods, pod)
	}

	return pods, nil
}

func formatAge(timestamp string) string {
	t, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return "unknown"
	}
	d := time.Since(t)
	if d < 0 {
		return "0s"
	}

	days := int(math.Floor(d.Hours() / 24))
	if days > 0 {
		return fmt.Sprintf("%dd", days)
	}
	hours := int(math.Floor(d.Hours()))
	if hours > 0 {
		return fmt.Sprintf("%dh", hours)
	}
	minutes := int(math.Floor(d.Minutes()))
	if minutes > 0 {
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%ds", int(math.Floor(d.Seconds())))
}
