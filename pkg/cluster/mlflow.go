package cluster

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	mlflowNamespace = "redhat-ods-applications"
	mlflowCRName    = "mlflow"
	mlflowAPIGroup  = "mlflow.opendatahub.io/v1"
)

func getMLflowStatus(c *Client) types.ResourceState {
	state := types.ResourceState{Namespace: mlflowNamespace}

	crPath := clusterPath(mlflowAPIGroup, "mlflows", mlflowCRName)
	body, _, err := c.get(crPath)
	if err != nil {
		state.Message = "Not deployed"
		return state
	}
	state.Deployed = true

	// Check current image for PR detection
	var crSpec struct {
		Spec struct {
			Image struct {
				Image string `json:"image"`
			} `json:"image"`
		} `json:"spec"`
	}
	if json.Unmarshal(body, &crSpec) == nil {
		state.CurrentImage = crSpec.Spec.Image.Image
	}

	var cr struct {
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
				Reason string `json:"reason"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if json.Unmarshal(body, &cr) == nil {
		for _, cond := range cr.Status.Conditions {
			if cond.Type == "Ready" || cond.Type == "Available" {
				if cond.Status == "True" {
					state.Ready = true
					state.Message = "Running"
				} else if cond.Reason != "" {
					state.Message = cond.Reason
				} else {
					state.Message = "Not ready"
				}
				break
			}
		}
	}
	if state.Message == "" {
		state.Message = "Provisioning"
	}

	// Get MLflow route
	routePath := namespacedPath("route.openshift.io/v1", "routes", mlflowNamespace, "mlflow")
	if routeBody, _, err := c.get(routePath); err == nil {
		var route struct {
			Spec struct{ Host string `json:"host"` } `json:"spec"`
		}
		if json.Unmarshal(routeBody, &route) == nil && route.Spec.Host != "" {
			state.UIRoute = "https://" + route.Spec.Host
		}
	}

	return state
}

// SetupMLflow creates the MLflow CR in redhat-ods-applications.
func SetupMLflow(c *Client) (*types.OperationResponse, error) {
	logs := []string{}

	// Check if already deployed
	existing := getMLflowStatus(c)
	if existing.Deployed {
		return &types.OperationResponse{
			Success: false, Message: "MLflow is already deployed.",
			Logs: logs, ErrorCode: "validation",
		}, nil
	}

	logs = append(logs, "Creating MLflow CR...")

	mlflowCR := map[string]interface{}{
		"apiVersion": mlflowAPIGroup,
		"kind":       "MLflow",
		"metadata": map[string]interface{}{
			"name": mlflowCRName,
		},
		"spec": map[string]interface{}{
			"image": map[string]interface{}{
				"image":           "quay.io/opendatahub/mlflow:odh-stable",
				"imagePullPolicy": "Always",
			},
			"replicas": 1,
			"migration": map[string]interface{}{
				"mode": "Automatic",
			},
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{"cpu": "500m", "memory": "1Gi"},
				"limits":   map[string]interface{}{"cpu": "2", "memory": "2Gi"},
			},
			"storage": map[string]interface{}{
				"accessModes": []string{"ReadWriteOnce"},
				"resources": map[string]interface{}{
					"requests": map[string]interface{}{"storage": "10Gi"},
				},
			},
			"backendStoreUri":      "sqlite:////mlflow/mlflow.db",
			"registryStoreUri":     "sqlite:////mlflow/mlflow.db",
			"artifactsDestination": "file:///mlflow/artifacts",
			"serveArtifacts":       true,
		},
	}

	crPath := clusterPath(mlflowAPIGroup, "mlflows", mlflowCRName)
	_, _, applyErr := c.apply(crPath, mlflowCR)
	if applyErr != nil {
		data, _ := json.Marshal(mlflowCR)
		collectionPath := clusterPath(mlflowAPIGroup, "mlflows", "")
		_, _, createErr := c.post(collectionPath, data)
		if createErr != nil && !IsK8sError(createErr, 409) {
			RecordActivity(c, types.ActivityEntry{
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				User:      getUser(c),
				Action:    "setup-mlflow",
				Detail:    fmt.Sprintf("CR creation failed: %v", createErr),
				Success:   false,
			})
			return &types.OperationResponse{
				Success: false, Message: fmt.Sprintf("Failed to create MLflow CR: %v", createErr),
				Logs: logs, ErrorCode: errorCodeFromK8sErr(createErr),
			}, nil
		}
	}
	logs = append(logs, "OK: MLflow CR created")

	slog.Info("mlflow setup", "user", getUser(c))
	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "setup-mlflow",
		Detail:    "MLflow CR created in " + mlflowNamespace,
		Success:   true,
	})

	return &types.OperationResponse{
		Success: true, Message: "MLflow CR created. The operator will deploy MLflow shortly.",
		Logs: logs,
	}, nil
}

// TeardownMLflow deletes the MLflow CR.
func TeardownMLflow(c *Client) (*types.OperationResponse, error) {
	logs := []string{}

	logs = append(logs, "Deleting MLflow CR...")
	crPath := clusterPath(mlflowAPIGroup, "mlflows", mlflowCRName)
	_, delErr := c.delete(crPath)
	if delErr != nil && !IsK8sError(delErr, 404) {
		RecordActivity(c, types.ActivityEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			User:      getUser(c),
			Action:    "teardown-mlflow",
			Detail:    fmt.Sprintf("CR delete failed: %v", delErr),
			Success:   false,
		})
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("Failed to delete MLflow CR: %v", delErr),
			Logs: logs, ErrorCode: errorCodeFromK8sErr(delErr),
		}, nil
	}
	logs = append(logs, "OK: MLflow CR deleted")

	slog.Info("mlflow teardown", "user", getUser(c))
	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "teardown-mlflow",
		Detail:    mlflowNamespace,
		Success:   true,
	})

	return &types.OperationResponse{
		Success: true, Message: "MLflow removed. The operator will clean up resources.",
		Logs: logs,
	}, nil
}

// DeployMLflowPR patches the MLflow CR with a PR image.
func DeployMLflowPR(c *Client, prNumber int) (*types.OperationResponse, error) {
	logs := []string{}

	if prNumber <= 0 {
		return &types.OperationResponse{
			Success: false, Message: "PR number must be greater than 0",
			Logs: logs, ErrorCode: "validation",
		}, nil
	}

	image := fmt.Sprintf("quay.io/opendatahub/mlflow:odh-pr-%d", prNumber)
	logs = append(logs, fmt.Sprintf("Target image: %s", image))

	// Verify image exists on Quay
	manifestURL := fmt.Sprintf("https://quay.io/v2/opendatahub/mlflow/manifests/odh-pr-%d", prNumber)
	req, err := http.NewRequestWithContext(c.ctx, "HEAD", manifestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create manifest request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.v2+json,application/vnd.docker.distribution.manifest.list.v2+json")
	resp, err := quayHTTPClient.Do(req)
	if err != nil {
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("Failed to verify image on Quay: %v", err),
			Logs: logs, ErrorCode: "network",
		}, nil
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized {
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("PR image not found: odh-pr-%d. Ensure the PR has a published image.", prNumber),
			Logs: logs, ErrorCode: "validation",
		}, nil
	}
	logs = append(logs, "OK: Image exists on Quay")

	// Patch the MLflow CR
	patchData, _ := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{
			"image": map[string]interface{}{
				"image": image,
			},
		},
	})
	crPath := clusterPath(mlflowAPIGroup, "mlflows", mlflowCRName)
	_, _, err = c.patch(crPath, patchData)
	if err != nil {
		RecordActivity(c, types.ActivityEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			User:      getUser(c),
			Action:    "deploy-mlflow-pr",
			Detail:    fmt.Sprintf("PR #%d (patch failed: %v)", prNumber, err),
			Success:   false,
		})
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("Failed to patch MLflow CR: %v", err),
			Logs: logs, ErrorCode: errorCodeFromK8sErr(err),
		}, nil
	}
	logs = append(logs, "OK: MLflow CR patched with PR image")

	slog.Info("mlflow PR deployed", "pr", prNumber, "image", image, "user", getUser(c))
	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "deploy-mlflow-pr",
		Detail:    fmt.Sprintf("PR #%d (%s)", prNumber, image),
		Success:   true,
	})

	return &types.OperationResponse{
		Success: true, Message: fmt.Sprintf("MLflow PR #%d image deployed.", prNumber),
		Logs: logs,
	}, nil
}

// RevertMLflowImage resets the MLflow CR image to the default.
func RevertMLflowImage(c *Client) (*types.OperationResponse, error) {
	logs := []string{}
	defaultImage := "quay.io/opendatahub/mlflow:odh-stable"

	patchData, _ := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{
			"image": map[string]interface{}{
				"image": defaultImage,
			},
		},
	})
	crPath := clusterPath(mlflowAPIGroup, "mlflows", mlflowCRName)
	_, _, err := c.patch(crPath, patchData)
	if err != nil {
		RecordActivity(c, types.ActivityEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			User:      getUser(c),
			Action:    "revert-mlflow",
			Detail:    fmt.Sprintf("patch failed: %v", err),
			Success:   false,
		})
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("Failed to revert MLflow image: %v", err),
			Logs: logs, ErrorCode: errorCodeFromK8sErr(err),
		}, nil
	}
	logs = append(logs, fmt.Sprintf("OK: Image reverted to %s", defaultImage))

	slog.Info("mlflow image reverted", "user", getUser(c))
	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "revert-mlflow",
		Detail:    defaultImage,
		Success:   true,
	})

	return &types.OperationResponse{
		Success: true, Message: "MLflow image reverted to default.",
		Logs: logs,
	}, nil
}
