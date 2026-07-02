package cluster

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const snapshotConfigMapName = "rhoai-nightly-updater-snapshot"

type deploymentSnapshot struct {
	Timestamp string            `json:"timestamp"`
	Images    map[string]string `json:"images"` // deployment name → image digest
}

// SaveDeploymentSnapshot captures the current deployment image digests into a ConfigMap.
func SaveDeploymentSnapshot(c *Client) error {
	appDeps, err := getDeployments(c, "redhat-ods-applications")
	if err != nil {
		return fmt.Errorf("failed to get app deployments: %w", err)
	}
	opDeps, err := getDeployments(c, "redhat-ods-operator")
	if err != nil {
		return fmt.Errorf("failed to get operator deployments: %w", err)
	}

	images := make(map[string]string)
	for _, dep := range append(appDeps, opDeps...) {
		images[dep.Name] = dep.Image
	}

	snap := deploymentSnapshot{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Images:    images,
	}

	snapJSON, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("failed to marshal snapshot: %w", err)
	}

	ns := getActivityNamespace()
	cmPath := namespacedPath("v1", "configmaps", ns, snapshotConfigMapName)
	cm := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      snapshotConfigMapName,
			"namespace": ns,
		},
		"data": map[string]interface{}{
			"snapshot": string(snapJSON),
		},
	}

	_, _, err = c.apply(cmPath, cm)
	if err != nil {
		return fmt.Errorf("failed to save snapshot: %w", err)
	}
	return nil
}

// GetDeploymentSnapshot reads the last saved snapshot from the ConfigMap.
func GetDeploymentSnapshot(c *Client) (map[string]string, string) {
	ns := getActivityNamespace()
	cmPath := namespacedPath("v1", "configmaps", ns, snapshotConfigMapName)

	body, _, err := c.get(cmPath)
	if err != nil {
		if !IsK8sError(err, 404) {
			slog.Warn("snapshot: failed to read", "error", err)
		}
		return nil, ""
	}

	var cm map[string]interface{}
	if err := json.Unmarshal(body, &cm); err != nil {
		slog.Warn("snapshot: failed to parse configmap", "error", err)
		return nil, ""
	}

	data, ok := cm["data"].(map[string]interface{})
	if !ok {
		return nil, ""
	}
	raw, ok := data["snapshot"].(string)
	if !ok || raw == "" {
		return nil, ""
	}

	var snap deploymentSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		slog.Warn("snapshot: failed to parse snapshot data", "error", err)
		return nil, ""
	}

	return snap.Images, snap.Timestamp
}

// CompareDeployments marks each deployment's change status relative to the snapshot.
// Returns the count of changed (updated + new) deployments.
func CompareDeployments(deployments []types.DeploymentInfo, snapshot map[string]string) int {
	if len(snapshot) == 0 {
		return 0
	}
	changed := 0
	for i := range deployments {
		prevImage, existed := snapshot[deployments[i].Name]
		if !existed {
			deployments[i].ChangeStatus = "new"
			changed++
		} else if prevImage != deployments[i].Image {
			deployments[i].ChangeStatus = "updated"
			changed++
		}
	}
	return changed
}
