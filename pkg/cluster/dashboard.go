package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	dashboardDeploymentName = "rhods-dashboard"
	dashboardContainerName  = "rhods-dashboard"
	dashboardNamespace      = "redhat-ods-applications"
	prImagePrefix           = "quay.io/opendatahub/odh-dashboard:pr-"
)

// prContainerRepos maps container names in the rhods-dashboard deployment
// to their Quay repos where PR images are published.
var prContainerRepos = map[string]string{
	"rhods-dashboard":   "opendatahub/odh-dashboard",
	"model-registry-ui": "opendatahub/odh-mod-arch-modular-architecture",
	"gen-ai-ui":         "opendatahub/odh-mod-arch-gen-ai",
	"maas-ui":           "opendatahub/mod-arch-maas",
	"mlflow-ui":         "opendatahub/odh-mod-arch-mlflow",
	"eval-hub-ui":       "opendatahub/odh-mod-arch-eval-hub",
	"automl-ui":         "opendatahub/odh-mod-arch-automl",
	"autorag-ui":        "opendatahub/odh-mod-arch-autorag",
}

// containerEnvVarMap maps container names to their RELATED_IMAGE env var
// names in the operator deployment (for reverting).
var containerEnvVarMap = map[string]string{
	"rhods-dashboard":   "RELATED_IMAGE_ODH_DASHBOARD_IMAGE",
	"model-registry-ui": "RELATED_IMAGE_ODH_MOD_ARCH_MODEL_REGISTRY_IMAGE",
	"gen-ai-ui":         "RELATED_IMAGE_ODH_MOD_ARCH_GEN_AI_IMAGE",
	"maas-ui":           "RELATED_IMAGE_ODH_MOD_ARCH_MAAS_IMAGE",
	"mlflow-ui":         "RELATED_IMAGE_ODH_MOD_ARCH_MLFLOW_IMAGE",
	"eval-hub-ui":       "RELATED_IMAGE_ODH_MOD_ARCH_EVAL_HUB_IMAGE",
	"automl-ui":         "RELATED_IMAGE_ODH_MOD_ARCH_AUTOML_IMAGE",
	"autorag-ui":        "RELATED_IMAGE_ODH_MOD_ARCH_AUTORAG_IMAGE",
}

// GetDashboardState returns the current state of the rhods-dashboard deployment.
func GetDashboardState(c *Client) (*types.DashboardState, error) {
	deployPath := namespacedPath("apps/v1", "deployments", dashboardNamespace, dashboardDeploymentName)
	body, _, err := c.get(deployPath)
	if err != nil {
		return nil, fmt.Errorf("get dashboard deployment: %w", err)
	}

	var deploy struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			Replicas *int `json:"replicas"`
			Selector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"selector"`
			Template struct {
				Spec struct {
					Containers []struct {
						Name  string `json:"name"`
						Image string `json:"image"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &deploy); err != nil {
		return nil, fmt.Errorf("parse dashboard deployment: %w", err)
	}

	state := &types.DashboardState{}

	// Check ALL containers for PR images
	for _, container := range deploy.Spec.Template.Spec.Containers {
		if container.Name == dashboardContainerName {
			state.CurrentImage = container.Image
		}
		// Check if this container is running a PR image
		if repo, ok := prContainerRepos[container.Name]; ok {
			prPrefix := fmt.Sprintf("quay.io/%s:pr-", repo)
			if strings.HasPrefix(container.Image, prPrefix) {
				state.PRContainers = append(state.PRContainers, container.Name)
				// Extract PR number from any PR container
				tagPart := container.Image[strings.LastIndex(container.Image, ":")+1:]
				prStr := strings.TrimPrefix(tagPart, "pr-")
				if n, err := strconv.Atoi(prStr); err == nil && state.PRNumber == 0 {
					state.PRNumber = n
				}
			}
		}
	}
	state.IsCustomPR = len(state.PRContainers) > 0

	// Check managed annotation (nil-safe)
	state.Managed = true
	if deploy.Metadata.Annotations != nil {
		if val, ok := deploy.Metadata.Annotations["opendatahub.io/managed"]; ok {
			state.Managed = val != "false"
		}
	}

	// Get dashboard pods using the deployment's label selector
	dashPods := getDashboardPods(c, deploy.Spec.Selector.MatchLabels)
	state.Pods = dashPods

	// Build the expected image for the primary dashboard container from the deployment spec
	specImage := ""
	for _, c := range deploy.Spec.Template.Spec.Containers {
		if c.Name == dashboardContainerName {
			specImage = c.Image
			break
		}
	}

	expectedReplicas := 1
	if deploy.Spec.Replicas != nil {
		expectedReplicas = *deploy.Spec.Replicas
	}

	if len(dashPods) == 0 {
		state.PodStatus = "No pods"
		state.RolloutPending = specImage != ""
	} else {
		allReady := true
		imageMatch := true
		for _, pod := range dashPods {
			if !pod.Ready {
				allReady = false
			}
			if specImage != "" && pod.Image != "" && pod.Image != specImage {
				imageMatch = false
			}
		}

		if !allReady || !imageMatch || len(dashPods) > expectedReplicas {
			state.RolloutPending = true
			for _, pod := range dashPods {
				if !pod.Ready || (specImage != "" && pod.Image != specImage) {
					state.PodStatus = pod.Phase
					state.ContainersTotal = len(pod.Containers)
					if state.ContainersTotal == 0 {
						state.ContainersTotal = len(deploy.Spec.Template.Spec.Containers)
					}
					ready := 0
					for _, ct := range pod.Containers {
						if ct.Ready {
							ready++
						}
					}
					state.ContainersReady = ready
					break
				}
			}
			if state.PodStatus == "" {
				state.PodStatus = "Rolling out"
			}
			state.PodReady = false
		} else {
			// Single pod, ready, image matches spec — truly ready
			pod := dashPods[0]
			state.PodStatus = pod.Phase
			state.PodReady = pod.Ready
			state.ContainersTotal = len(pod.Containers)
			ready := 0
			for _, ct := range pod.Containers {
				if ct.Ready {
					ready++
				}
			}
			state.ContainersReady = ready
		}
	}

	// Check for scheduling failures when rollout is pending
	if state.RolloutPending {
		hasSchedulingFailure := false
		hasReadyPod := false
		for _, pod := range dashPods {
			if pod.SchedulingReason != "" {
				state.SchedulingFailureReason = pod.SchedulingReason
				hasSchedulingFailure = true
			}
			if pod.Ready {
				hasReadyPod = true
			}
		}
		state.CanAssistRollout = hasSchedulingFailure && hasReadyPod
	}

	// Get dashboard URL from the route
	routePath := namespacedPath("route.openshift.io/v1", "routes", dashboardNamespace, "rhods-dashboard")
	routeBody, _, routeErr := c.get(routePath)
	if routeErr == nil {
		var route struct {
			Spec struct {
				Host string `json:"host"`
			} `json:"spec"`
		}
		if json.Unmarshal(routeBody, &route) == nil && route.Spec.Host != "" {
			state.DashboardURL = "https://" + route.Spec.Host
		}
	}

	return state, nil
}

// DeployPRImage patches the rhods-dashboard deployment with PR images.
// It checks all 8 dashboard container repos on Quay for pr-N tags and
// patches every container that has a published image.
func DeployPRImage(c *Client, prNumber int) (*types.OperationResponse, error) {
	logs := []string{}

	if prNumber <= 0 {
		return &types.OperationResponse{
			Success: false, Message: "PR number must be greater than 0",
			Logs: logs, ErrorCode: "validation",
		}, nil
	}

	// Check if this PR is already deployed and running
	state, stateErr := GetDashboardState(c)
	if stateErr == nil && state.PRNumber == prNumber && state.PodReady && !state.RolloutPending {
		logs = append(logs, fmt.Sprintf("PR #%d is already deployed and running.", prNumber))
		return &types.OperationResponse{
			Success: true,
			Message: fmt.Sprintf("PR #%d is already deployed and running.", prNumber),
			Logs:    logs,
		}, nil
	}

	tag := fmt.Sprintf("pr-%d", prNumber)
	logs = append(logs, fmt.Sprintf("Checking Quay for PR #%d images...", prNumber))

	// Check all repos in parallel
	type checkResult struct {
		container string
		image     string
		status    string // "found", "not built", "error"
	}
	resultCh := make(chan checkResult, len(prContainerRepos))
	for containerName, repo := range prContainerRepos {
		go func(name, r string) {
			image := fmt.Sprintf("quay.io/%s:%s", r, tag)
			manifestURL := fmt.Sprintf("https://quay.io/v2/%s/manifests/%s", r, tag)
			ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "HEAD", manifestURL, nil)
			if err != nil {
				resultCh <- checkResult{name, "", "error"}
				return
			}
			req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.v2+json,application/vnd.docker.distribution.manifest.list.v2+json")
			resp, err := quayHTTPClient.Do(req)
			if err != nil {
				resultCh <- checkResult{name, "", "error"}
				return
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				resultCh <- checkResult{name, image, "found"}
			} else {
				resultCh <- checkResult{name, "", "not built"}
			}
		}(containerName, repo)
	}

	found := map[string]string{}
	var checkErrors int
	for i := 0; i < len(prContainerRepos); i++ {
		r := <-resultCh
		switch r.status {
		case "found":
			found[r.container] = r.image
			logs = append(logs, fmt.Sprintf("  %s: found", r.container))
		case "not built":
			logs = append(logs, fmt.Sprintf("  %s: not built", r.container))
		case "error":
			checkErrors++
			logs = append(logs, fmt.Sprintf("  %s: check failed (network error)", r.container))
		}
	}

	if len(found) == 0 {
		RecordActivity(c, types.ActivityEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			User:      getUser(c),
			Action:    "deploy-pr",
			Detail:    fmt.Sprintf("PR #%d (no images found)", prNumber),
			Success:   false,
		})
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("No PR images found for pr-%d on Quay. Ensure the PR CI has completed.", prNumber),
			Logs: logs, ErrorCode: "validation",
		}, nil
	}

	logs = append(logs, fmt.Sprintf("Found %d/%d images", len(found), len(prContainerRepos)))

	// Build strategic merge patch with all found containers
	var containerPatches []map[string]interface{}
	for name, image := range found {
		containerPatches = append(containerPatches, map[string]interface{}{
			"name":  name,
			"image": image,
		})
	}

	patchData, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{
				"opendatahub.io/managed": "false",
			},
		},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": containerPatches,
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal patch: %w", err)
	}

	deployPath := namespacedPath("apps/v1", "deployments", dashboardNamespace, dashboardDeploymentName)

	_, _, err = c.strategicPatch(deployPath, patchData)
	if err != nil {
		RecordActivity(c, types.ActivityEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			User:      getUser(c),
			Action:    "deploy-pr",
			Detail:    fmt.Sprintf("PR #%d (patch failed: %v)", prNumber, err),
			Success:   false,
		})
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("Failed to patch dashboard deployment: %v", err),
			Logs: logs, ErrorCode: errorCodeFromK8sErr(err),
		}, nil
	}

	containerNames := make([]string, 0, len(found))
	for name := range found {
		containerNames = append(containerNames, name)
	}
	sort.Strings(containerNames)
	logs = append(logs, fmt.Sprintf("OK: Patched containers: %s", strings.Join(containerNames, ", ")))

	slog.Info("dashboard PR deployed", "pr", prNumber, "containers", len(found), "user", getUser(c))

	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "deploy-pr",
		Detail:    fmt.Sprintf("PR #%d (%d containers)", prNumber, len(found)),
		Success:   true,
	})

	return &types.OperationResponse{
		Success: true,
		Message: fmt.Sprintf("PR #%d deployed (%d containers patched).", prNumber, len(found)),
		Logs:    logs,
	}, nil
}

// RevertDashboardImage reverts ALL dashboard containers to their operator-managed images.
func RevertDashboardImage(c *Client) (*types.OperationResponse, error) {
	logs := []string{}

	logs = append(logs, "Looking up original images from operator env vars...")
	originalImages, imgErr := getOriginalImages(c)
	if imgErr != nil {
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("Failed to read operator images: %v", imgErr),
			Logs: logs, ErrorCode: "prerequisites",
		}, nil
	}
	logs = append(logs, fmt.Sprintf("Found %d original images", len(originalImages)))

	// Build patch with all original container images
	var containerPatches []map[string]interface{}
	for name, image := range originalImages {
		containerPatches = append(containerPatches, map[string]interface{}{
			"name":  name,
			"image": image,
		})
		logs = append(logs, fmt.Sprintf("  %s: %s", name, truncateForLog(image)))
	}

	deployPath := namespacedPath("apps/v1", "deployments", dashboardNamespace, dashboardDeploymentName)

	// Single atomic patch: restore all container images AND re-enable operator management.
	// Combining both in one strategic merge patch eliminates the race window where the
	// operator could detect the annotation change and start its own reconciliation
	// before the images are updated.
	patchData, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{
				"opendatahub.io/managed": "true",
			},
		},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": containerPatches,
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal revert patch: %w", err)
	}
	_, _, err = c.strategicPatch(deployPath, patchData)
	if err != nil {
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("Failed to patch deployment: %v", err),
			Logs: logs, ErrorCode: errorCodeFromK8sErr(err),
		}, nil
	}
	logs = append(logs, fmt.Sprintf("OK: %d container images restored", len(containerPatches)))
	logs = append(logs, "OK: Operator management re-enabled")

	slog.Info("dashboard reverted", "containers", len(containerPatches), "user", getUser(c))

	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "revert-dashboard",
		Detail:    fmt.Sprintf("%d containers restored", len(containerPatches)),
		Success:   true,
	})

	return &types.OperationResponse{
		Success: true,
		Message: fmt.Sprintf("All %d containers restored to operator-managed images.", len(containerPatches)),
		Logs:    logs,
	}, nil
}

// getOriginalImages reads all dashboard container images from the operator's
// RELATED_IMAGE_* env vars.
func getOriginalImages(c *Client) (map[string]string, error) {
	operatorPath := namespacedPath("apps/v1", "deployments", "redhat-ods-operator", "rhods-operator")
	body, _, err := c.get(operatorPath)
	if err != nil {
		return nil, fmt.Errorf("get operator deployment: %w", err)
	}

	var deploy struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Env []struct {
							Name  string `json:"name"`
							Value string `json:"value"`
						} `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &deploy); err != nil {
		return nil, fmt.Errorf("parse operator deployment: %w", err)
	}

	// Build env var lookup
	envVars := map[string]string{}
	for _, container := range deploy.Spec.Template.Spec.Containers {
		for _, env := range container.Env {
			if env.Value != "" {
				envVars[env.Name] = env.Value
			}
		}
	}

	// Map container names to their original images
	result := map[string]string{}
	for containerName, envVarName := range containerEnvVarMap {
		if image, ok := envVars[envVarName]; ok {
			result[containerName] = image
		}
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("no RELATED_IMAGE env vars found in operator")
	}

	// Warn about missing containers
	for containerName := range containerEnvVarMap {
		if _, ok := result[containerName]; !ok {
			slog.Warn("no operator env var for container, will not be reverted", "container", containerName, "envVar", containerEnvVarMap[containerName])
		}
	}

	return result, nil
}

// getDashboardPods returns pods matching the given label selector, with
// pod.Image set to the rhods-dashboard container (not the first container).
func getDashboardPods(c *Client, matchLabels map[string]string) []types.PodInfo {
	// Build label selector query string
	var parts []string
	for k, v := range matchLabels {
		parts = append(parts, k+"="+v)
	}
	selector := strings.Join(parts, ",")

	path := fmt.Sprintf("/api/v1/namespaces/%s/pods?labelSelector=%s", dashboardNamespace, url.QueryEscape(selector))
	body, _, err := c.get(path)
	if err != nil {
		slog.Warn("failed to list dashboard pods", "error", err)
		return nil
	}

	var podList struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
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
		slog.Warn("failed to parse dashboard pod list", "error", err)
		return nil
	}

	var pods []types.PodInfo
	for _, item := range podList.Items {
		pod := types.PodInfo{
			Name:      item.Metadata.Name,
			Namespace: item.Metadata.Namespace,
			Phase:     item.Status.Phase,
			Node:      item.Spec.NodeName,
		}

		// Use the rhods-dashboard container image specifically
		for _, ct := range item.Spec.Containers {
			if ct.Name == dashboardContainerName {
				pod.Image = ct.Image
				break
			}
		}

		allReady := true
		totalRestarts := 0
		if len(item.Status.ContainerStatuses) > 0 {
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
		} else {
			// containerStatuses is empty for Pending pods before kubelet reports;
			// fall back to spec containers so the UI shows "0/N" instead of "0/0".
			allReady = false
			for _, ct := range item.Spec.Containers {
				pod.Containers = append(pod.Containers, types.ContainerInfo{
					Name:  ct.Name,
					Ready: false,
					State: "waiting",
				})
			}
		}

		// Extract scheduling failure reason from pod conditions
		for _, cond := range item.Status.Conditions {
			if cond.Type == "PodScheduled" && cond.Status == "False" {
				pod.SchedulingReason = cond.Reason
				pod.SchedulingMessage = cond.Message
				break
			}
		}

		pod.Ready = allReady && len(item.Status.ContainerStatuses) > 0
		pod.Restarts = totalRestarts
		pods = append(pods, pod)
	}

	return pods
}
