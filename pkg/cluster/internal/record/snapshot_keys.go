package record

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// ReadSnapshotKey reads one key of this tool's snapshot ConfigMap ("" when
// it or the ConfigMap is missing).
func ReadSnapshotKey(c *kube.Client, key string) (string, error) {
	var cm struct {
		Data map[string]string `json:"data"`
	}
	found, err := c.Bookkeeping().GetIntoOptional(rhoai.NamespacedPath("v1", "configmaps", GetActivityNamespace(), SnapshotConfigMapName), &cm)
	if err != nil {
		if kube.IsDecodeError(err) {
			return "", fmt.Errorf("parse the snapshot ConfigMap: %w", err)
		}
		return "", err
	}
	if !found {
		return "", nil
	}
	return cm.Data[key], nil
}

// WriteSnapshotKey applies one key of the snapshot ConfigMap as manager:
// server-side apply keeps the keys other managers own.
func WriteSnapshotKey(c *kube.Client, key, manager, value string) error {
	ns := GetActivityNamespace()
	cm, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": SnapshotConfigMapName, "namespace": ns},
		"data":       map[string]any{key: value},
	})
	query := url.Values{"fieldManager": {manager}, "force": {"true"}}
	_, _, err := c.Bookkeeping().Do(http.MethodPatch, rhoai.NamespacedPath("v1", "configmaps", ns, SnapshotConfigMapName), "application/apply-patch+yaml", cm, query)
	return err
}
