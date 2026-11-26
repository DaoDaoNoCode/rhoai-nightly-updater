package record

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// The snapshot ConfigMap's read-change-write cycles, safe across updater
// pods (UpdateSnapshot).
//
// Each record key is applied whole (server-side apply, a field manager
// per key), so two writers that read the same old value of a key and each
// add an entry would keep only the second one's. Within one pod the
// snapshot writer serializes the cycles; across pods (two run while a
// replaced pod drains) the write is conditional: UpdateSnapshot gives its
// change a client that carries a transaction, the change's first read of
// the ConfigMap (ReadSnapshotData) notes its resourceVersion, and every
// apply of the change (ApplySnapshotKeys) carries it, which the API server
// takes as a precondition (a 409 Conflict when the ConfigMap changed
// since). On a conflict UpdateSnapshot runs the change again from a fresh
// read: read, admit (the users' caps) and merge, every time. When the
// ConfigMap does not exist yet the first write cannot be conditional on
// it (it creates it).

type snapshotTxnKey struct{}

// snapshotTxn is one attempt of an UpdateSnapshot change.
type snapshotTxn struct {
	read bool
	rv   string
}

func txnOf(c *kube.Client) *snapshotTxn {
	if c == nil || c.Context() == nil {
		return nil
	}
	t, _ := c.Context().Value(snapshotTxnKey{}).(*snapshotTxn)
	return t
}

// snapshotConflictRetries bounds the attempts of one change: each conflict
// means another pod wrote the ConfigMap meanwhile.
const snapshotConflictRetries = 8

// ReadSnapshotData reads the snapshot ConfigMap's data (found false when
// it does not exist) with c's bookkeeping client. Inside UpdateSnapshot the
// first read notes the ConfigMap's resourceVersion: the change's writes are
// conditional on it. Errors are the read's own (kube.IsDecodeError tells a
// parse error).
func ReadSnapshotData(c *kube.Client) (map[string]string, bool, error) {
	var cm struct {
		Metadata kube.ObjectMeta   `json:"metadata"`
		Data     map[string]string `json:"data"`
	}
	found, err := c.Bookkeeping().GetIntoOptional(rhoai.NamespacedPath("v1", "configmaps", GetActivityNamespace(), SnapshotConfigMapName), &cm)
	if err != nil {
		return nil, false, err
	}
	if t := txnOf(c); t != nil && !t.read {
		t.read, t.rv = true, cm.Metadata.ResourceVersion
	}
	return cm.Data, found, nil
}

// ApplySnapshotKeys server-side applies data (keys of the snapshot
// ConfigMap) as manager; other managers' keys stay. Inside UpdateSnapshot
// it is conditional on the change's first read.
func ApplySnapshotKeys(c *kube.Client, manager string, data map[string]any) error {
	ns := GetActivityNamespace()
	meta := map[string]any{"name": SnapshotConfigMapName, "namespace": ns}
	if t := txnOf(c); t != nil && t.rv != "" {
		meta["resourceVersion"] = t.rv
	}
	cm, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta, "data": data})
	query := url.Values{"fieldManager": {manager}, "force": {"true"}}
	_, _, err := c.Bookkeeping().Do(http.MethodPatch, rhoai.NamespacedPath("v1", "configmaps", ns, SnapshotConfigMapName), "application/apply-patch+yaml", cm, query)
	return err
}

// UpdateSnapshot runs change (reads of snapshot keys, their change and
// their writes, with the client it is given) once this process's other
// changes of the snapshot ended, so two project actions of one pod never
// write a key from the same old value; and again from a fresh read when
// another pod wrote the ConfigMap meanwhile (a conflict of the conditional
// write), at most snapshotConflictRetries times.
func UpdateSnapshot(c *kube.Client, change func(tc *kube.Client) error) error {
	return snapshotWriter.do(c.Context(), func() error {
		var err error
		for range snapshotConflictRetries {
			tc := c.WithContext(context.WithValue(c.Context(), snapshotTxnKey{}, &snapshotTxn{}))
			if err = change(tc); err == nil || !kube.IsK8sConflict(err) {
				return err
			}
		}
		return err
	})
}
