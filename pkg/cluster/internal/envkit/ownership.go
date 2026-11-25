package envkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// Ownership of the quick resources (S3 storage, pipeline servers, MLflow).
//
// Everything the tool creates carries kube.ManagedByLabelKey=ManagedByLabelValue,
// and only labelled objects are listed for teardown. Objects created by older
// versions carry no label; they are recognised by the managedFields entry the
// API server recorded when the tool created them (see createdBy): a
// server-side apply with fieldManager "rhoai-nightly-updater", or a POST with
// no User-Agent, which the API server records as manager "Go-http-client".
const (
	ToolFieldManager  = "rhoai-nightly-updater"
	LegacyPostManager = "Go-http-client"
	// ClientManager is the manager of this tool's writes without a
	// fieldManager since it sends its own User-Agent
	// (kube.UserAgentProduct): plain patches and updates.
	ClientManager = kube.UserAgentProduct
)

// ToolLabels returns the ownership labels set on every object the tool creates.
func ToolLabels() map[string]any {
	return map[string]any{kube.ManagedByLabelKey: kube.ManagedByLabelValue}
}

// ToolManagers are the field managers of this tool's writes:
// "rhoai-nightly-updater" (server-side apply, which always names its
// fieldManager, and POSTs with fieldManager) and ClientManager (writes
// without one, named after the User-Agent).
var ToolManagers = []string{ToolFieldManager, ClientManager}

// LegacyToolManagers are ToolManagers and "Go-http-client", which the API
// server derives from Go's default User-Agent (net/http defaultUserAgent
// "Go-http-client/1.1"). Versions before the tool sent its own User-Agent
// wrote with it: the earliest versions' plain POSTs and patches (the first
// version applied with fieldManager rhoai-nightly-updater and fell back to
// such a POST, pkg/cluster/client.go:193 and pipeline_server.go:262-266 at
// 5051672), and every later plain patch. Use it only to recognise objects
// those versions made under names no current version writes: namespace
// minio, the pipeline server "dspa" on that storage and its Secret
// dashboard-dspa-secret. odh-dashboard never writes as Go-http-client: its
// Node backend sends no User-Agent (backend/src/utils/directCallUtils.ts),
// its Go BFF forwards the browser's (distributions/core-bff,
// httputil.ReverseProxy); any other Go client without a User-Agent does.
var LegacyToolManagers = []string{ToolFieldManager, ClientManager, LegacyPostManager}

// WithToolFieldManager adds fieldManager to a POST path so created objects
// record this tool as their manager.
func WithToolFieldManager(path string) string {
	return path + "?fieldManager=" + url.QueryEscape(ToolFieldManager)
}

// ForeignObjectError reports an object that has a name the tool uses but was
// not created by the tool, so the tool refuses to change it.
type ForeignObjectError struct {
	Desc string // for example "Secret minio/minio-secret"
}

func (e *ForeignObjectError) Error() string {
	return e.Desc + " already exists and was not created by this tool"
}

// ErrObjectChanged means an object the tool owns kept changing between the
// ownership check and the write, so the write was not made.
var ErrObjectChanged = errors.New("it changed while the tool was updating it; re-run to retry")

// ToolObjectReader returns the current metadata of one object, and whether it
// exists.
type ToolObjectReader func() (kube.ObjectMeta, bool, error)

// EnsureToolObject creates obj when it is absent and updates it when this
// tool owns it, without ever writing to an object someone else created:
//   - An absent object is created with POST, which fails with 409 instead of
//     overwriting an object created concurrently. On 409 the object is read
//     again and its ownership decided again. (A server-side apply would
//     create it too, but it would also take over an object someone created
//     in between, since the tool applies with force.)
//   - An owned object first gets its field ownership upgraded to
//     server-side apply (UpgradeToolFieldOwnership), then is server-side
//     applied with metadata.resourceVersion set to the version that was
//     checked. The API server rejects the apply with 409 when the object
//     changed in between, including when it was deleted and recreated by
//     someone else (verified with `oc apply --server-side --force-conflicts
//     --dry-run=server` and a stale resourceVersion on OpenShift 4.22: "the
//     object has been modified").
//
// With dryRun the POST and the apply are server-side dry runs
// (?dryRun=All): the API server runs admission, validation and the apply
// merge and persists nothing (Kubernetes API concepts, "Dry-run"). The
// ownership upgrade is still made for real, because the dry-run apply must
// see it: it changes only metadata.managedFields, no spec. Callers dry-run
// every write of a setup before making any, so an object the API server
// rejects stops the setup before anything changed.
//
// current is the object as already read by the caller (found=false when it
// was absent). createPath is the collection POST path (query allowed), path
// the object path. It returns "created" or "updated", and the object's
// metadata and existence as last seen, which change when the ownership was
// upgraded: pass them to the real write after a dry run.
func EnsureToolObject(c *kube.Client, desc, path, createPath string, obj map[string]any, current kube.ObjectMeta, found bool, read ToolObjectReader, owned func(kube.ObjectMeta) bool, dryRun bool) (string, kube.ObjectMeta, bool, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return "", current, found, err
	}
	var query url.Values
	if dryRun {
		query = url.Values{"dryRun": {"All"}}
	}
	for range 3 {
		if !found {
			created, _, postErr := c.Do(http.MethodPost, createPath, "application/json", data, query)
			if postErr == nil {
				current, found = afterWrite(created, current, found, dryRun)
				return "created", current, found, nil
			}
			if !kube.IsK8sConflict(postErr) {
				return "", current, found, postErr
			}
			if current, found, err = read(); err != nil {
				return "", current, found, err
			}
			continue
		}
		if !owned(current) {
			return "", current, found, &ForeignObjectError{Desc: desc}
		}
		if current.Terminating() {
			return "", current, found, fmt.Errorf("%s is being deleted; wait until it is gone, then retry", desc)
		}
		upgraded, upErr := UpgradeToolFieldOwnership(c, path, current)
		if upErr != nil {
			if !kube.IsK8sConflict(upErr) {
				return "", current, found, fmt.Errorf("upgrade the field ownership of %s to server-side apply: %w", desc, upErr)
			}
			if current, found, err = read(); err != nil {
				return "", current, found, err
			}
			continue
		}
		current = upgraded
		guarded := make(map[string]any, len(obj))
		maps.Copy(guarded, obj)
		meta := map[string]any{}
		if m, ok := obj["metadata"].(map[string]any); ok {
			maps.Copy(meta, m)
		}
		meta["resourceVersion"] = current.ResourceVersion
		guarded["metadata"] = meta
		var (
			appliedBody []byte
			applyErr    error
		)
		if dryRun {
			appliedBody, _, applyErr = c.DryRunApply(path, guarded)
		} else {
			appliedBody, _, applyErr = c.Apply(path, guarded)
		}
		if applyErr == nil {
			current, found = afterWrite(appliedBody, current, found, dryRun)
			return "updated", current, found, nil
		}
		if !kube.IsK8sConflict(applyErr) {
			return "", current, found, applyErr
		}
		if current, found, err = read(); err != nil {
			return "", current, found, err
		}
	}
	return "", current, found, fmt.Errorf("%s: %w", desc, ErrObjectChanged)
}

// afterWrite returns the metadata of the object a write returned, so a later
// write of the same object is guarded by its new resourceVersion. A dry run
// persisted nothing: the object is still as it was.
func afterWrite(body []byte, current kube.ObjectMeta, found, dryRun bool) (kube.ObjectMeta, bool) {
	if dryRun {
		return current, found
	}
	if m, err := kube.ParseObjectMeta(body); err == nil && m.ResourceVersion != "" {
		return m, true
	}
	return current, found
}

// UpgradeToolFieldOwnership moves this tool's field ownership of the object
// at path from client-side to server-side apply, with the semantics of
// k8s.io/client-go/util/csaupgrade UpgradeManagedFields (what kubectl apply
// --server-side does for its own managers; Kubernetes docs "Server-Side
// Apply", "Upgrading from client-side apply to server-side apply"):
//   - the fieldsV1 of the tool's Update entry (recorded when the tool, or an
//     earlier version, created the object with POST) of the same API version
//     is folded into the tool's Apply entry; when there is no Apply entry
//     yet, the newest Update entry becomes it;
//   - every Update entry of the tool is then dropped;
//   - entries of other managers, and of subresources, are left as they are.
//
// Without it the Update entry keeps owning what the tool once created, so
// server-side apply never removes a field or list item the tool stopped
// applying: a container port that moved from 23646 to 12535 was kept next to
// the new one, and the apply failed with 422 "ports[2].name: Duplicate
// value: admin".
//
// The change is a JSON patch that also replaces metadata.resourceVersion
// with the checked one, so it fails with 409 when the object changed since
// current was read (csaupgrade.UpgradeManagedFieldsPatch does the same). It
// writes nothing when the tool has no Update entry, and returns the
// object's metadata after the change.
func UpgradeToolFieldOwnership(c *kube.Client, path string, current kube.ObjectMeta) (kube.ObjectMeta, error) {
	ours := func(e kube.ManagedFieldsEntry, op string) bool {
		return e.Manager == ToolFieldManager && e.Operation == op && e.Subresource == ""
	}
	entries := make([]kube.ManagedFieldsEntry, len(current.ManagedFields))
	copy(entries, current.ManagedFields)
	target := -1
	for i, e := range entries {
		if ours(e, "Apply") {
			target = i
			break
		}
	}
	if target < 0 {
		// The entries are sorted newest first; convert the newest Update.
		for i, e := range entries {
			if ours(e, "Update") {
				target = i
				entries[i].Operation = "Apply"
				break
			}
		}
		if target < 0 {
			return current, nil // nothing to upgrade
		}
	}
	update := -1
	for i, e := range entries {
		if ours(e, "Update") && e.APIVersion == entries[target].APIVersion {
			update = i
			break
		}
	}
	if update >= 0 {
		merged, err := unionFieldsV1(entries[target].FieldsV1, entries[update].FieldsV1)
		if err != nil {
			return current, err
		}
		entries[target].FieldsV1 = merged
	}
	kept := make([]kube.ManagedFieldsEntry, 0, len(entries))
	for _, e := range entries {
		if !ours(e, "Update") {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(current.ManagedFields) && current.ManagedFields[target].Operation == "Apply" {
		return current, nil // already server-side only
	}
	patch, err := json.Marshal([]map[string]any{
		{"op": "replace", "path": "/metadata/managedFields", "value": kept},
		{"op": "replace", "path": "/metadata/resourceVersion", "value": current.ResourceVersion},
	})
	if err != nil {
		return current, err
	}
	body, _, err := c.Do(http.MethodPatch, path, "application/json-patch+json", patch, nil)
	if err != nil {
		return current, err
	}
	return kube.ParseObjectMeta(body)
}

// unionFieldsV1 returns the union of two field sets in their fieldsV1 form:
// a tree whose nodes are JSON objects, so the union of the sets is the deep
// merge of the trees.
func unionFieldsV1(a, b map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	var merge func(x, y map[string]any)
	merge = func(x, y map[string]any) {
		for k, v := range y {
			xv, okx := x[k].(map[string]any)
			yv, oky := v.(map[string]any)
			if okx && oky {
				merge(xv, yv)
				continue
			}
			if _, exists := x[k]; !exists {
				x[k] = v
			}
		}
	}
	decode := func(m map[string]json.RawMessage) (map[string]any, error) {
		out := map[string]any{}
		for k, raw := range m {
			var v any
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, fmt.Errorf("fieldsV1 %s: %w", k, err)
			}
			out[k] = v
		}
		return out, nil
	}
	x, err := decode(a)
	if err != nil {
		return nil, err
	}
	y, err := decode(b)
	if err != nil {
		return nil, err
	}
	merge(x, y)
	out := make(map[string]json.RawMessage, len(x))
	for k, v := range x {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		out[k] = raw
	}
	return out, nil
}
