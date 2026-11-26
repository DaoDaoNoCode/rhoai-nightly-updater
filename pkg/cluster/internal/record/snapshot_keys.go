package record

import (
	"fmt"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// ReadSnapshotKey reads one key of this tool's snapshot ConfigMap ("" when
// it or the ConfigMap is missing).
func ReadSnapshotKey(c *kube.Client, key string) (string, error) {
	data, found, err := ReadSnapshotData(c)
	if err != nil {
		if kube.IsDecodeError(err) {
			return "", fmt.Errorf("parse the snapshot ConfigMap: %w", err)
		}
		return "", err
	}
	if !found {
		return "", nil
	}
	return data[key], nil
}

// WriteSnapshotKey applies one key of the snapshot ConfigMap as manager:
// server-side apply keeps the keys other managers own.
func WriteSnapshotKey(c *kube.Client, key, manager, value string) error {
	return ApplySnapshotKeys(c, manager, map[string]any{key: value})
}
