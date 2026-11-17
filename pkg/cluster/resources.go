package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Ownership of the quick resources (S3 storage, pipeline servers, MLflow).
//
// Everything the tool creates carries managedByLabelKey=managedByLabelValue,
// and only labelled objects are listed for teardown. Objects created by older
// versions carry no label; they are recognised by the managedFields entry the
// API server recorded when the tool created them (see createdBy): a
// server-side apply with fieldManager "rhoai-nightly-updater", or a POST with
// no User-Agent, which the API server records as manager "Go-http-client".
const (
	managedByLabelKey   = "app.kubernetes.io/managed-by"
	managedByLabelValue = "rhoai-nightly-updater"
	toolFieldManager    = "rhoai-nightly-updater"
	legacyPostManager   = "Go-http-client"
)

// toolLabels returns the ownership labels set on every object the tool creates.
func toolLabels() map[string]interface{} {
	return map[string]interface{}{managedByLabelKey: managedByLabelValue}
}

// objectMeta is the subset of metadata the ownership checks read.
type objectMeta struct {
	Name              string               `json:"name"`
	Namespace         string               `json:"namespace"`
	UID               string               `json:"uid"`
	ResourceVersion   string               `json:"resourceVersion"`
	CreationTimestamp string               `json:"creationTimestamp"`
	DeletionTimestamp string               `json:"deletionTimestamp"`
	Labels            map[string]string    `json:"labels"`
	Annotations       map[string]string    `json:"annotations"`
	Finalizers        []string             `json:"finalizers"`
	ManagedFields     []managedFieldsEntry `json:"managedFields"`
	OwnerReferences   []struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
		UID  string `json:"uid"`
	} `json:"ownerReferences"`
}

// managedFieldsEntry is one metadata.managedFields entry, with every field of
// the Kubernetes type (metav1.ManagedFieldsEntry), so an entry read here
// can be written back unchanged.
type managedFieldsEntry struct {
	Manager    string `json:"manager,omitempty"`
	Operation  string `json:"operation,omitempty"`
	APIVersion string `json:"apiVersion,omitempty"`
	Time       string `json:"time,omitempty"`
	FieldsType string `json:"fieldsType,omitempty"`
	// FieldsV1 is the set of fields the manager owns, as a tree keyed by
	// "f:<field>", "k:<list key>", "v:<value>" or "." (the node itself).
	FieldsV1    map[string]json.RawMessage `json:"fieldsV1,omitempty"`
	Subresource string                     `json:"subresource,omitempty"`
}

func (m objectMeta) hasToolLabel() bool {
	return m.Labels[managedByLabelKey] == managedByLabelValue
}

func (m objectMeta) terminating() bool {
	return m.DeletionTimestamp != ""
}

// createdBy reports whether manager wrote the object at creation time with the
// given operation. An entry's time is the last time that manager changed the
// object, so a later edit by the same manager makes this false; that errs on
// the side of not claiming the object.
func (m objectMeta) createdBy(manager, operation string) bool {
	if m.CreationTimestamp == "" {
		return false
	}
	for _, f := range m.ManagedFields {
		if f.Manager == manager && f.Operation == operation && f.Subresource == "" && f.Time == m.CreationTimestamp {
			return true
		}
	}
	return false
}

// createdByToolApply reports whether an older version of the tool created the
// object with server-side apply.
func (m objectMeta) createdByToolApply() bool {
	return m.createdBy(toolFieldManager, "Apply")
}

// ownsField reports whether manager owns the top-level field (a fieldsV1
// key such as "f:spec") through the object itself, not a subresource.
func (m objectMeta) ownsField(manager, field string) bool {
	for _, f := range m.ManagedFields {
		if _, owns := f.FieldsV1[field]; owns && f.Manager == manager && f.Subresource == "" {
			return true
		}
	}
	return false
}

func (m objectMeta) ownedBy(uid string) bool {
	for _, o := range m.OwnerReferences {
		if o.UID == uid {
			return true
		}
	}
	return false
}

func parseObjectMeta(body []byte) (objectMeta, error) {
	var obj struct {
		Metadata objectMeta `json:"metadata"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return objectMeta{}, err
	}
	return obj.Metadata, nil
}

// readObjectMeta reads the metadata of the object at path. found is false on
// 404; any other error is returned.
func readObjectMeta(c *Client, path string) (meta objectMeta, found bool, err error) {
	body, _, err := c.get(path)
	if IsK8sError(err, 404) {
		return objectMeta{}, false, nil
	}
	if err != nil {
		return objectMeta{}, false, err
	}
	meta, err = parseObjectMeta(body)
	return meta, err == nil, err
}

// deleteWithUID deletes path only if the object still has the given UID, so a
// check-then-delete never removes an object recreated in between.
func deleteWithUID(c *Client, path, uid string) (int, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"apiVersion":        "v1",
		"kind":              "DeleteOptions",
		"preconditions":     map[string]interface{}{"uid": uid},
		"propagationPolicy": "Background",
	})
	_, status, err := c.do(http.MethodDelete, path, "application/json", body, nil)
	return status, err
}

// withToolFieldManager adds fieldManager to a POST path so created objects
// record this tool as their manager.
func withToolFieldManager(path string) string {
	return path + "?fieldManager=" + url.QueryEscape(toolFieldManager)
}

// foreignObjectError reports an object that has a name the tool uses but was
// not created by the tool, so the tool refuses to change it.
type foreignObjectError struct {
	Desc string // for example "Secret minio/minio-secret"
}

func (e *foreignObjectError) Error() string {
	return e.Desc + " already exists and was not created by this tool"
}

// errObjectChanged means an object the tool owns kept changing between the
// ownership check and the write, so the write was not made.
var errObjectChanged = errors.New("it changed while the tool was updating it; re-run to retry")

// toolObjectReader returns the current metadata of one object, and whether it
// exists.
type toolObjectReader func() (objectMeta, bool, error)

// ensureToolObject creates obj when it is absent and updates it when this
// tool owns it, without ever writing to an object someone else created:
//   - An absent object is created with POST, which fails with 409 instead of
//     overwriting an object created concurrently. On 409 the object is read
//     again and its ownership decided again. (A server-side apply would
//     create it too, but it would also take over an object someone created
//     in between, since the tool applies with force.)
//   - An owned object first gets its field ownership upgraded to
//     server-side apply (upgradeToolFieldOwnership), then is server-side
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
func ensureToolObject(c *Client, desc, path, createPath string, obj map[string]interface{}, current objectMeta, found bool, read toolObjectReader, owned func(objectMeta) bool, dryRun bool) (string, objectMeta, bool, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return "", current, found, err
	}
	var query url.Values
	if dryRun {
		query = url.Values{"dryRun": {"All"}}
	}
	for attempt := 0; attempt < 3; attempt++ {
		if !found {
			created, _, postErr := c.do(http.MethodPost, createPath, "application/json", data, query)
			if postErr == nil {
				current, found = afterWrite(created, current, found, dryRun)
				return "created", current, found, nil
			}
			if !IsK8sError(postErr, http.StatusConflict) {
				return "", current, found, postErr
			}
			if current, found, err = read(); err != nil {
				return "", current, found, err
			}
			continue
		}
		if !owned(current) {
			return "", current, found, &foreignObjectError{Desc: desc}
		}
		if current.terminating() {
			return "", current, found, fmt.Errorf("%s is being deleted; wait until it is gone, then retry", desc)
		}
		upgraded, upErr := upgradeToolFieldOwnership(c, path, current)
		if upErr != nil {
			if !IsK8sError(upErr, http.StatusConflict) {
				return "", current, found, fmt.Errorf("upgrade the field ownership of %s to server-side apply: %w", desc, upErr)
			}
			if current, found, err = read(); err != nil {
				return "", current, found, err
			}
			continue
		}
		current = upgraded
		guarded := make(map[string]interface{}, len(obj))
		for k, v := range obj {
			guarded[k] = v
		}
		meta := map[string]interface{}{}
		if m, ok := obj["metadata"].(map[string]interface{}); ok {
			for k, v := range m {
				meta[k] = v
			}
		}
		meta["resourceVersion"] = current.ResourceVersion
		guarded["metadata"] = meta
		var (
			appliedBody []byte
			applyErr    error
		)
		if dryRun {
			appliedBody, _, applyErr = c.dryRunApply(path, guarded)
		} else {
			appliedBody, _, applyErr = c.apply(path, guarded)
		}
		if applyErr == nil {
			current, found = afterWrite(appliedBody, current, found, dryRun)
			return "updated", current, found, nil
		}
		if !IsK8sError(applyErr, http.StatusConflict) {
			return "", current, found, applyErr
		}
		if current, found, err = read(); err != nil {
			return "", current, found, err
		}
	}
	return "", current, found, fmt.Errorf("%s: %w", desc, errObjectChanged)
}

// afterWrite returns the metadata of the object a write returned, so a later
// write of the same object is guarded by its new resourceVersion. A dry run
// persisted nothing: the object is still as it was.
func afterWrite(body []byte, current objectMeta, found, dryRun bool) (objectMeta, bool) {
	if dryRun {
		return current, found
	}
	if m, err := parseObjectMeta(body); err == nil && m.ResourceVersion != "" {
		return m, true
	}
	return current, found
}

// upgradeToolFieldOwnership moves this tool's field ownership of the object
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
func upgradeToolFieldOwnership(c *Client, path string, current objectMeta) (objectMeta, error) {
	ours := func(e managedFieldsEntry, op string) bool {
		return e.Manager == toolFieldManager && e.Operation == op && e.Subresource == ""
	}
	entries := make([]managedFieldsEntry, len(current.ManagedFields))
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
	kept := make([]managedFieldsEntry, 0, len(entries))
	for _, e := range entries {
		if !ours(e, "Update") {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(current.ManagedFields) && current.ManagedFields[target].Operation == "Apply" {
		return current, nil // already server-side only
	}
	patch, err := json.Marshal([]map[string]interface{}{
		{"op": "replace", "path": "/metadata/managedFields", "value": kept},
		{"op": "replace", "path": "/metadata/resourceVersion", "value": current.ResourceVersion},
	})
	if err != nil {
		return current, err
	}
	body, _, err := c.do(http.MethodPatch, path, "application/json-patch+json", patch, nil)
	if err != nil {
		return current, err
	}
	return parseObjectMeta(body)
}

// unionFieldsV1 returns the union of two field sets in their fieldsV1 form:
// a tree whose nodes are JSON objects, so the union of the sets is the deep
// merge of the trees.
func unionFieldsV1(a, b map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	var merge func(x, y map[string]interface{})
	merge = func(x, y map[string]interface{}) {
		for k, v := range y {
			xv, okx := x[k].(map[string]interface{})
			yv, oky := v.(map[string]interface{})
			if okx && oky {
				merge(xv, yv)
				continue
			}
			if _, exists := x[k]; !exists {
				x[k] = v
			}
		}
	}
	decode := func(m map[string]json.RawMessage) (map[string]interface{}, error) {
		out := map[string]interface{}{}
		for k, raw := range m {
			var v interface{}
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

// waitForGone polls until gone reports true, an error occurs, or the timeout
// passes. Every poll runs under one context bounded by the timeout, so a call
// that starts just before the deadline cannot overrun it. It returns
// (false, nil) on timeout and (false, err) on any read error, including
// 401/403: an unreadable object is not "still being deleted".
func waitForGone(c *Client, timeout, interval time.Duration, gone func(cc *Client) (bool, error)) (bool, error) {
	ctx, cancel := context.WithTimeout(c.ctx, timeout)
	defer cancel()
	cc := c.WithContext(ctx)
	for {
		done, err := gone(cc)
		if done && err == nil {
			return true, nil
		}
		if ctx.Err() != nil {
			// The caller's own context ended (client gone): report it. The
			// local deadline only means "not gone yet".
			if c.ctx.Err() != nil {
				return false, c.ctx.Err()
			}
			return false, nil
		}
		if err != nil {
			return false, err
		}
		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}
}

// waitForDeletion polls path until it returns 404. See waitForGone.
func waitForDeletion(c *Client, path string, timeout, interval time.Duration) (bool, error) {
	return waitForGone(c, timeout, interval, func(cc *Client) (bool, error) {
		_, _, err := cc.get(path)
		if IsK8sError(err, http.StatusNotFound) {
			return true, nil
		}
		return false, err
	})
}

// workloadPodIssue is the most relevant reason a workload's pods are not ready.
type workloadPodIssue struct {
	Reason   string
	Message  string
	Terminal bool
}

// terminalWaitingReasons are kubelet container waiting reasons that do not
// clear without a change (image, config or crash fix). ErrImagePull is not
// included: kubelet retries it and moves to ImagePullBackOff when it persists.
var terminalWaitingReasons = map[string]bool{
	"ImagePullBackOff":           true,
	"InvalidImageName":           true,
	"ErrImageNeverPull":          true,
	"CrashLoopBackOff":           true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"RunContainerError":          true,
}

// inspectPods returns why the pods matching selector are not ready, preferring
// terminal reasons. An empty Reason means nothing specific was found. When
// wantImage is set, only pods with a container running that image count.
func inspectPods(c *Client, namespace, selector, wantImage string) (workloadPodIssue, error) {
	body, _, err := c.get(namespacedPath("v1", "pods", namespace, "") + "?labelSelector=" + url.QueryEscape(selector))
	if err != nil {
		return workloadPodIssue{}, err
	}
	var list struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
			Spec     struct {
				Containers []struct {
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
			Status struct {
				Conditions []struct {
					Type    string `json:"type"`
					Status  string `json:"status"`
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"conditions"`
				InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
				ContainerStatuses     []containerStatus `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return workloadPodIssue{}, fmt.Errorf("parse pod list: %w", err)
	}
	var best workloadPodIssue
	consider := func(p workloadPodIssue) {
		if p.Reason == "" {
			return
		}
		if best.Reason == "" || (p.Terminal && !best.Terminal) {
			best = p
		}
	}
	for _, pod := range list.Items {
		if pod.Metadata.terminating() {
			continue
		}
		if wantImage != "" {
			match := false
			for _, ctr := range pod.Spec.Containers {
				match = match || ctr.Image == wantImage
			}
			if !match {
				continue
			}
		}
		for _, cond := range pod.Status.Conditions {
			if cond.Type == "PodScheduled" && cond.Status == "False" {
				consider(workloadPodIssue{Reason: firstNonEmpty(cond.Reason, "Unschedulable"), Message: cond.Message})
			}
		}
		statuses := append(append([]containerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
		for _, cs := range statuses {
			if w := cs.State.Waiting; w != nil && w.Reason != "" {
				msg := w.Message
				if t := cs.LastState.Terminated; t != nil && w.Reason == "CrashLoopBackOff" {
					msg = strings.TrimSpace(fmt.Sprintf("%s (last exit: %s, code %d)", msg, t.Reason, t.ExitCode))
				}
				consider(workloadPodIssue{Reason: w.Reason, Message: msg, Terminal: terminalWaitingReasons[w.Reason]})
			}
		}
	}
	return best, nil
}

type containerStatus struct {
	State struct {
		Waiting *struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"waiting"`
	} `json:"state"`
	LastState struct {
		Terminated *struct {
			Reason   string `json:"reason"`
			ExitCode int    `json:"exitCode"`
		} `json:"terminated"`
	} `json:"lastState"`
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// applyPodIssue copies a pod issue into a resource state.
func applyPodIssue(state *types.ResourceState, issue workloadPodIssue) {
	if issue.Reason == "" {
		return
	}
	state.WaitingReason = issue.Reason
	state.TerminalError = issue.Terminal
	state.Message = issue.Reason
	if issue.Message != "" {
		state.Message = issue.Reason + ": " + truncateMessage(issue.Message)
	}
}

func truncateMessage(s string) string {
	s = strings.TrimSpace(s)
	const maxLen = 300
	if r := []rune(s); len(r) > maxLen {
		return string(r[:maxLen]) + "…"
	}
	return s
}

// GetResourcesStatus checks the state of test infrastructure resources. It
// lists DS projects, pipeline servers (one cluster-wide LIST), S3 storage, MLflow
// and the dashboard route concurrently, so the cost no longer grows with the
// number of projects. Pipeline servers are reported only for projects the
// user can see.
func GetResourcesStatus(c *Client) (*types.ResourcesStatus, error) {
	var (
		wg          sync.WaitGroup
		projects    []string
		projectsErr error
		dspas       []dspaInfo
		dspasErr    error
		dashURL     string
		minioEP     minioEndpoints
		minioEPErr  error
		status      = &types.ResourcesStatus{}
	)
	wg.Add(5)
	go func() { defer wg.Done(); projects, projectsErr = GetDSProjects(c) }()
	go func() { defer wg.Done(); dspas, dspasErr = listDSPAs(c, "") }()
	go func() { defer wg.Done(); status.MinIO, minioEP, minioEPErr = minioStatusAndEndpoints(c) }()
	go func() { defer wg.Done(); status.MLflow = getMLflowStatus(c) }()
	go func() { defer wg.Done(); dashURL = dashboardBaseURL(c) }()
	wg.Wait()
	if projectsErr != nil {
		return nil, projectsErr
	}
	if dspasErr != nil && !errors.Is(dspasErr, errDSPACRDMissing) {
		return nil, dspasErr
	}

	visible := map[string]bool{}
	for _, p := range projects {
		visible[p] = true
	}
	unmanaged := map[string]bool{}
	for _, d := range dspas {
		if !visible[d.Meta.Namespace] {
			continue
		}
		if !d.managedByTool() {
			unmanaged[d.Meta.Namespace] = true
			continue
		}
		status.PipelineServers = append(status.PipelineServers, pipelineServerState(d, dashURL))
	}
	for ns := range unmanaged {
		status.UnmanagedPipelineProjects = append(status.UnmanagedPipelineProjects, ns)
	}
	sort.Strings(status.UnmanagedPipelineProjects)
	sort.Slice(status.PipelineServers, func(i, j int) bool {
		return status.PipelineServers[i].Namespace < status.PipelineServers[j].Namespace
	})

	// Also for a storage that is gone but left its data PVCs (the kept MinIO
	// volume): teardown deletes them under the same guard.
	if status.MinIO.ManagedByTool && (status.MinIO.Deployed || len(status.MinIO.DataPVCs) > 0) {
		if minioEPErr != nil && len(dspas) > 0 {
			status.MinIO.TeardownBlockedReason = "Cannot verify which pipeline servers use the S3 storage: " + minioEPErr.Error()
		} else if reason := minioTeardownBlocker(dspas, minioEP); reason != "" {
			status.MinIO.TeardownBlockedReason = reason
		}
	}
	return status, nil
}

// routeHost returns a route's host, or "" when it cannot be read.
func routeHost(c *Client, namespace, name string) string {
	body, _, err := c.get(namespacedPath("route.openshift.io/v1", "routes", namespace, name))
	if err != nil {
		return ""
	}
	var route struct {
		Spec struct {
			Host string `json:"host"`
		} `json:"spec"`
	}
	if json.Unmarshal(body, &route) != nil {
		return ""
	}
	return route.Spec.Host
}

// GetDSProjects returns namespaces with the opendatahub.io/dashboard label.
func GetDSProjects(c *Client) ([]string, error) {
	path := "/api/v1/namespaces?labelSelector=opendatahub.io/dashboard=true"
	body, _, err := c.get(path)
	if err != nil {
		return nil, fmt.Errorf("list DS projects: %w", err)
	}

	var nsList struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &nsList); err != nil {
		return nil, fmt.Errorf("parse namespace list: %w", err)
	}

	var projects []string
	for _, ns := range nsList.Items {
		if ns.Status.Phase != "Terminating" {
			projects = append(projects, ns.Metadata.Name)
		}
	}
	return projects, nil
}
