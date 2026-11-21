package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
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
	toolFieldManager  = "rhoai-nightly-updater"
	legacyPostManager = "Go-http-client"
)

// toolLabels returns the ownership labels set on every object the tool creates.
func toolLabels() map[string]any {
	return map[string]any{kube.ManagedByLabelKey: kube.ManagedByLabelValue}
}

// toolManagers are the field managers this tool's versions wrote with:
// "rhoai-nightly-updater" (server-side apply and POSTs with fieldManager),
// and "Go-http-client", which the API server derives from the User-Agent of
// the earliest versions' plain POSTs and patches (Go's default; the first
// version applied with fieldManager rhoai-nightly-updater and fell back to
// such a POST, pkg/cluster/client.go:193 and pipeline_server.go:262-266 at
// 5051672). odh-dashboard never writes as Go-http-client: its Node backend
// sends no User-Agent (backend/src/utils/directCallUtils.ts), its Go BFF
// forwards the browser's (distributions/core-bff, httputil.ReverseProxy).
var toolManagers = []string{toolFieldManager, legacyPostManager}

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
type toolObjectReader func() (kube.ObjectMeta, bool, error)

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
func ensureToolObject(c *Client, desc, path, createPath string, obj map[string]any, current kube.ObjectMeta, found bool, read toolObjectReader, owned func(kube.ObjectMeta) bool, dryRun bool) (string, kube.ObjectMeta, bool, error) {
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
			return "", current, found, &foreignObjectError{Desc: desc}
		}
		if current.Terminating() {
			return "", current, found, fmt.Errorf("%s is being deleted; wait until it is gone, then retry", desc)
		}
		upgraded, upErr := upgradeToolFieldOwnership(c, path, current)
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
	return "", current, found, fmt.Errorf("%s: %w", desc, errObjectChanged)
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
func upgradeToolFieldOwnership(c *Client, path string, current kube.ObjectMeta) (kube.ObjectMeta, error) {
	ours := func(e kube.ManagedFieldsEntry, op string) bool {
		return e.Manager == toolFieldManager && e.Operation == op && e.Subresource == ""
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
	var items []struct {
		Metadata kube.ObjectMeta `json:"metadata"`
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
	}
	if err := c.ListInto(namespacedPath("v1", "pods", namespace, "")+"?labelSelector="+url.QueryEscape(selector), &items); err != nil {
		if kube.IsDecodeError(err) {
			return workloadPodIssue{}, fmt.Errorf("parse pod list: %w", err)
		}
		return workloadPodIssue{}, err
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
	for _, pod := range items {
		if pod.Metadata.Terminating() {
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
				consider(workloadPodIssue{Reason: kube.FirstNonEmpty(cond.Reason, "Unschedulable"), Message: cond.Message})
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

// applyPodIssue copies a pod issue into a resource state.
func applyPodIssue(state *types.ResourceState, issue workloadPodIssue) {
	if issue.Reason == "" {
		return
	}
	state.WaitingReason = issue.Reason
	state.TerminalError = issue.Terminal
	state.Message = issue.Reason
	if issue.Message != "" {
		state.Message = issue.Reason + ": " + kube.TruncateText(issue.Message, 300)
	}
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
		s3EP        s3Endpoints
		s3EPErr     error
		legacy      *legacyStorage
		legacyErr   error
		foreignNS   bool
		dsc         dscRead
		dscErr      error
		models      []llmisvcObject
		modelsErr   error
		status      = &types.ResourcesStatus{}
	)
	wg.Add(8)
	go func() { defer wg.Done(); models, modelsErr = listLLMInferenceServices(c) }()
	go func() { defer wg.Done(); projects, projectsErr = GetDSProjects(c) }()
	go func() { defer wg.Done(); dspas, dspasErr = listDSPAs(c, "") }()
	go func() { defer wg.Done(); status.MinIO, s3EP, s3EPErr = s3StatusAndEndpoints(c) }()
	go func() { defer wg.Done(); legacy, foreignNS, legacyErr = readLegacyStorageOwner(c) }()
	go func() { defer wg.Done(); status.MLflow = getMLflowStatus(c) }()
	go func() { defer wg.Done(); dashURL = dashboardBaseURL(c) }()
	go func() { defer wg.Done(); dsc, dscErr = readFirstDSC(c) }()
	wg.Wait()
	if projectsErr != nil {
		return nil, projectsErr
	}
	if dspasErr != nil && !errors.Is(dspasErr, errDSPACRDMissing) {
		return nil, dspasErr
	}

	status.DashboardURL = dashURL
	status.ForeignLegacyNamespace = foreignNS
	// What the features need from the DataScienceCluster. One that cannot
	// be read blocks nothing; setup still checks the CRD.
	if dscErr != nil {
		slog.Debug("cannot read the DataScienceCluster for the Environment page", "error", dscErr)
	} else {
		status.PipelinesBlocked = componentBlock(dsc, pipelinesComponent)
		status.MLflowBlocked = componentBlock(dsc, mlflowComponent)
	}
	if status.PipelinesBlocked == nil && errors.Is(dspasErr, errDSPACRDMissing) {
		status.PipelinesBlocked = &types.ComponentBlock{Component: pipelinesComponent.keys[0],
			Reason: "Data Science Pipelines is not installed yet: this cluster has no DataSciencePipelinesApplication API. If the component was just turned on, wait for the operator; otherwise check the Components page."}
	}

	visible := map[string]bool{}
	for _, p := range projects {
		visible[p] = true
	}
	// Models: the inference gateway (which models use it) and the
	// catalog's models in the visible projects.
	var mwg sync.WaitGroup
	mwg.Add(2)
	go func() { defer mwg.Done(); status.InferenceGateway = getInferenceGatewayStatus(c, models, modelsErr) }()
	go func() { defer mwg.Done(); status.Models = getModelsStatus(c, dsc, dscErr, models, modelsErr, visible) }()
	mwg.Wait()

	// A record that cannot be read leaves every pipeline server unrecorded
	// (fail closed: Migrate storage then asks before moving one).
	pipelineRec, recErr := readPipelineRecord(c)
	if recErr != nil {
		slog.Debug("cannot read the pipeline server record", "error", recErr)
		pipelineRec = pipelineRecord{}
	}
	unmanaged := map[string]bool{}
	for _, d := range dspas {
		if !visible[d.Meta.Namespace] {
			continue
		}
		if !d.managedByTool() {
			unmanaged[d.Meta.Namespace] = true
			foreign := pipelineServerState(d, dashURL)
			foreign.ManagedByTool = false
			foreign.DataPVCs = nil
			foreign.TeardownBlockedReason = "This pipeline server was not created by this tool, so the tool will not delete it."
			status.UnmanagedPipelineServers = append(status.UnmanagedPipelineServers, foreign)
			continue
		}
		ps := pipelineServerState(d, dashURL)
		ps.Unrecorded = !pipelineRec.owns(d.Meta)
		status.PipelineServers = append(status.PipelineServers, ps)
	}
	for ns := range unmanaged {
		status.UnmanagedPipelineProjects = append(status.UnmanagedPipelineProjects, ns)
	}
	sort.Strings(status.UnmanagedPipelineProjects)
	sort.Slice(status.UnmanagedPipelineServers, func(i, j int) bool {
		a, b := status.UnmanagedPipelineServers[i], status.UnmanagedPipelineServers[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	sort.Slice(status.PipelineServers, func(i, j int) bool {
		return status.PipelineServers[i].Namespace < status.PipelineServers[j].Namespace
	})

	// Storage an earlier version left in namespace minio: Migrate storage
	// replaces Set up and Repair while it is there (SetupS3Storage refuses),
	// and teardown removes it with the current storage.
	switch {
	case legacyErr != nil:
		status.MinIO.Warning = strings.TrimSpace(fmt.Sprintf("Cannot check namespace %s for storage from an earlier version: %v. %s", legacyNamespace, legacyErr, status.MinIO.Warning))
	case legacy != nil:
		newReady := status.MinIO.Ready && status.MinIO.ManagedByTool && status.MinIO.RepairNeeded == ""
		// Unreadable counts as not copied: the stage stays "migrate".
		copied, _ := migrationCopied(c)
		status.LegacyStorage = legacyStorageState(legacy, dspas, pipelineRec, newReady, copied)
		status.MinIO.MigrationPending = true
	}

	// Also for a storage that is gone but left its data PVC: teardown
	// deletes it under the same guard.
	current := status.MinIO.ManagedByTool && (status.MinIO.Deployed || len(status.MinIO.DataPVCs) > 0)
	if current || legacy != nil {
		var addresses []storageAddress
		var addrErr error
		if current {
			addresses, addrErr = append(addresses, s3EP.address), s3EPErr
		}
		if legacy != nil {
			addresses = append(addresses, legacy.address)
			if addrErr == nil {
				addrErr = legacy.addressErr
			}
		}
		if addrErr != nil && len(dspas) > 0 {
			status.MinIO.TeardownBlockedReason = "Cannot verify which pipeline servers use the S3 storage: " + addrErr.Error()
		} else if reason := s3TeardownBlocker(dspas, addresses...); reason != "" {
			status.MinIO.TeardownBlockedReason = reason
		}
		if addrErr == nil {
			live, deleting := storageUsers(dspas, addresses...)
			status.MinIO.UsedBy = append(live, deleting...)
		}
	}
	return status, nil
}

// routeHost returns a route's host, or "" when it cannot be read.
func routeHost(c *Client, namespace, name string) string {
	var route struct {
		Spec struct {
			Host string `json:"host"`
		} `json:"spec"`
	}
	if c.GetInto(namespacedPath("route.openshift.io/v1", "routes", namespace, name), &route) != nil {
		return ""
	}
	return route.Spec.Host
}

// GetDSProjects returns namespaces with the opendatahub.io/dashboard label.
func GetDSProjects(c *Client) ([]string, error) {
	path := "/api/v1/namespaces?labelSelector=opendatahub.io/dashboard=true"
	var items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if err := c.ListInto(path, &items); err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse namespace list: %w", err)
		}
		return nil, fmt.Errorf("list DS projects: %w", err)
	}

	var projects []string
	for _, ns := range items {
		if ns.Status.Phase != "Terminating" {
			projects = append(projects, ns.Metadata.Name)
		}
	}
	return projects, nil
}
