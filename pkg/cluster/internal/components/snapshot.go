package components

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

type deploymentSnapshot struct {
	Timestamp string            `json:"timestamp"`
	Images    map[string]string `json:"images"` // deployment name → image digest
}

// SaveDeploymentSnapshot captures the current deployment image digests into a ConfigMap.
func SaveDeploymentSnapshot(c *kube.Client) error {
	// No DSCInitialization: RHOAI has no applications yet, so only the
	// operator's Deployments are recorded.
	var appDeps []types.DeploymentInfo
	appNS, err := rhoai.AppsNamespace(c)
	switch {
	case err == nil:
		if appDeps, err = Deployments(c, appNS); err != nil {
			return fmt.Errorf("failed to get app deployments: %w", err)
		}
	case !errors.Is(err, rhoai.ErrNoDSCInitialization):
		return fmt.Errorf("failed to get app deployments: %w", err)
	}
	opDeps, err := Deployments(c, rhoai.SubNS)
	if err != nil {
		return fmt.Errorf("failed to get operator deployments: %w", err)
	}

	images := make(map[string]string)
	for _, dep := range append(appDeps, opDeps...) {
		images[dep.Name] = dep.Image
	}

	snap := deploymentSnapshot{
		Timestamp: record.NowRFC3339(),
		Images:    images,
	}

	snapJSON, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("failed to marshal snapshot: %w", err)
	}

	ns := record.GetActivityNamespace()
	cmPath := rhoai.NamespacedPath("v1", "configmaps", ns, record.SnapshotConfigMapName)
	cm := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      record.SnapshotConfigMapName,
			"namespace": ns,
		},
		"data": map[string]any{
			"snapshot": string(snapJSON),
		},
	}

	_, _, err = c.Bookkeeping().Apply(cmPath, cm)
	if err != nil {
		return fmt.Errorf("failed to save snapshot: %w", err)
	}
	return nil
}

// ReadDeploymentSnapshot reads the last saved snapshot from the ConfigMap.
func ReadDeploymentSnapshot(c *kube.Client) (map[string]string, string) {
	ns := record.GetActivityNamespace()
	cmPath := rhoai.NamespacedPath("v1", "configmaps", ns, record.SnapshotConfigMapName)

	body, _, err := c.Bookkeeping().Get(cmPath)
	if err != nil {
		if !kube.IsK8sNotFound(err) {
			slog.Warn("snapshot: failed to read", "error", err)
		}
		return nil, ""
	}

	var cm map[string]any
	if err := json.Unmarshal(body, &cm); err != nil {
		slog.Warn("snapshot: failed to parse configmap", "error", err)
		return nil, ""
	}

	data, ok := cm["data"].(map[string]any)
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
