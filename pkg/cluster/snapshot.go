package cluster

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
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

// The operator Subscription recorded before each Update, Reinstall and
// Refresh, and after each successful one, in the snapshot ConfigMap. Those
// operations delete the Subscription before they recreate it; if the pod
// is killed in between, the cluster has no Subscription and its source,
// channel, approval mode and spec.config would be lost. Refresh and
// Reinstall read the record when no Subscription exists.
//
// It is written by server-side apply under its own field manager, so
// SaveDeploymentSnapshot (manager rhoai-nightly-updater, which applies only
// data.snapshot) never removes it, and the RBAC is the snapshot ConfigMap's
// existing patch grant (create when the ConfigMap does not exist yet).
const (
	subscriptionSnapshotKey     = "operator-subscription"
	subscriptionSnapshotManager = "rhoai-nightly-updater-subscription"
)

type subscriptionSnapshot struct {
	RecordedAt   string                 `json:"recordedAt"`
	Spec         map[string]interface{} `json:"spec"`
	InstalledCSV string                 `json:"installedCSV,omitempty"`
}

// saveSubscriptionSnapshot records the spec and installed CSV of sub (a
// Subscription object).
func saveSubscriptionSnapshot(c *Client, sub map[string]interface{}) error {
	spec, _ := sub["spec"].(map[string]interface{})
	if spec == nil {
		return fmt.Errorf("the Subscription has no spec")
	}
	status, _ := sub["status"].(map[string]interface{})
	installed, _ := status["installedCSV"].(string)
	data, err := json.Marshal(subscriptionSnapshot{RecordedAt: time.Now().UTC().Format(time.RFC3339), Spec: spec, InstalledCSV: installed})
	if err != nil {
		return err
	}
	ns := getActivityNamespace()
	cm, _ := json.Marshal(map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": snapshotConfigMapName, "namespace": ns},
		"data":       map[string]interface{}{subscriptionSnapshotKey: string(data)},
	})
	query := url.Values{"fieldManager": {subscriptionSnapshotManager}, "force": {"true"}}
	if _, _, err := c.do(http.MethodPatch, namespacedPath("v1", "configmaps", ns, snapshotConfigMapName), "application/apply-patch+yaml", cm, query); err != nil {
		return fmt.Errorf("record the operator Subscription: %w", err)
	}
	return nil
}

// loadSubscriptionSnapshot returns the recorded Subscription, or nil when
// none was recorded.
func loadSubscriptionSnapshot(c *Client) (*subscriptionSnapshot, error) {
	body, _, err := c.get(namespacedPath("v1", "configmaps", getActivityNamespace(), snapshotConfigMapName))
	if IsK8sError(err, 404) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the recorded operator Subscription: %w", err)
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &cm); err != nil {
		return nil, fmt.Errorf("parse the snapshot ConfigMap: %w", err)
	}
	raw := cm.Data[subscriptionSnapshotKey]
	if raw == "" {
		return nil, nil
	}
	var snap subscriptionSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return nil, fmt.Errorf("parse the recorded operator Subscription: %w", err)
	}
	if snap.Spec == nil {
		return nil, nil
	}
	return &snap, nil
}

// recordLiveSubscription records the current Subscription after a
// successful operation, on its own short context (the operation's may have
// ended; see postOperationContext). Errors are only logged.
func recordLiveSubscription(c *Client) {
	ctx, cancel := postOperationContext(c, subscriptionRecordTimeout)
	defer cancel()
	c = c.WithContext(ctx)
	body, _, err := c.get(subscriptionPath())
	if err != nil {
		slog.Warn("could not read the operator Subscription to record it", "error", err)
		return
	}
	var sub map[string]interface{}
	if err := json.Unmarshal(body, &sub); err != nil {
		return
	}
	if err := saveSubscriptionSnapshot(c, sub); err != nil {
		slog.Warn("could not record the operator Subscription", "error", err)
	}
}
