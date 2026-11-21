package record

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

const SnapshotConfigMapName = "rhoai-nightly-updater-snapshot"

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
	SubscriptionSnapshotKey     = "operator-subscription"
	SubscriptionSnapshotManager = "rhoai-nightly-updater-subscription"
)

type SubscriptionSnapshot struct {
	RecordedAt   string         `json:"recordedAt"`
	Spec         map[string]any `json:"spec"`
	InstalledCSV string         `json:"installedCSV,omitempty"`
}

// SaveSubscriptionSnapshot records the spec and installed CSV of sub (a
// Subscription object).
func SaveSubscriptionSnapshot(c *kube.Client, sub map[string]any) error {
	spec, _ := sub["spec"].(map[string]any)
	if spec == nil {
		return fmt.Errorf("the Subscription has no spec")
	}
	status, _ := sub["status"].(map[string]any)
	installed, _ := status["installedCSV"].(string)
	data, err := json.Marshal(SubscriptionSnapshot{RecordedAt: kube.NowRFC3339(), Spec: spec, InstalledCSV: installed})
	if err != nil {
		return err
	}
	ns := GetActivityNamespace()
	cm, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": SnapshotConfigMapName, "namespace": ns},
		"data":       map[string]any{SubscriptionSnapshotKey: string(data)},
	})
	query := url.Values{"fieldManager": {SubscriptionSnapshotManager}, "force": {"true"}}
	if _, _, err := c.Bookkeeping().Do(http.MethodPatch, rhoai.NamespacedPath("v1", "configmaps", ns, SnapshotConfigMapName), "application/apply-patch+yaml", cm, query); err != nil {
		return fmt.Errorf("record the operator Subscription: %w", err)
	}
	return nil
}

// LoadSubscriptionSnapshot returns the recorded Subscription, or nil when
// none was recorded.
func LoadSubscriptionSnapshot(c *kube.Client) (*SubscriptionSnapshot, error) {
	var cm struct {
		Data map[string]string `json:"data"`
	}
	found, err := c.Bookkeeping().GetIntoOptional(rhoai.NamespacedPath("v1", "configmaps", GetActivityNamespace(), SnapshotConfigMapName), &cm)
	if err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse the snapshot ConfigMap: %w", err)
		}
		return nil, fmt.Errorf("read the recorded operator Subscription: %w", err)
	}
	if !found {
		return nil, nil
	}
	raw := cm.Data[SubscriptionSnapshotKey]
	if raw == "" {
		return nil, nil
	}
	var snap SubscriptionSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return nil, fmt.Errorf("parse the recorded operator Subscription: %w", err)
	}
	if snap.Spec == nil {
		return nil, nil
	}
	return &snap, nil
}

// LiveSubscription records the current Subscription after a
// successful operation, on its own short context (the operation's may have
// ended; see postOperationContext). Errors are only logged.
func LiveSubscription(c *kube.Client) {
	ctx, cancel := PostOperationContext(c, subscriptionRecordTimeout)
	defer cancel()
	c = c.WithContext(ctx)
	body, _, err := c.Get(rhoai.SubscriptionPath())
	if err != nil {
		slog.Warn("could not read the operator Subscription to record it", "error", err)
		return
	}
	var sub map[string]any
	if err := json.Unmarshal(body, &sub); err != nil {
		return
	}
	if err := SaveSubscriptionSnapshot(c, sub); err != nil {
		slog.Warn("could not record the operator Subscription", "error", err)
	}
}
