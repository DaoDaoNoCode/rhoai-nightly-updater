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
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	UID               string            `json:"uid"`
	ResourceVersion   string            `json:"resourceVersion"`
	CreationTimestamp string            `json:"creationTimestamp"`
	DeletionTimestamp string            `json:"deletionTimestamp"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	Finalizers        []string          `json:"finalizers"`
	ManagedFields     []struct {
		Manager     string `json:"manager"`
		Operation   string `json:"operation"`
		Time        string `json:"time"`
		Subresource string `json:"subresource"`
	} `json:"managedFields"`
	OwnerReferences []struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
		UID  string `json:"uid"`
	} `json:"ownerReferences"`
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
//     again and its ownership decided again.
//   - An owned object is server-side applied with metadata.resourceVersion
//     set to the version that was checked. The API server rejects the apply
//     with 409 when the object changed in between, including when it was
//     deleted and recreated by someone else (verified with
//     `oc apply --server-side --force-conflicts --dry-run=server` and a stale
//     resourceVersion on OpenShift 4.22: "the object has been modified").
//
// current is the object as already read by the caller (found=false when it
// was absent). createPath is the collection POST path (query allowed), path
// the object path. It returns "created" or "updated".
func ensureToolObject(c *Client, desc, path, createPath string, obj map[string]interface{}, current objectMeta, found bool, read toolObjectReader, owned func(objectMeta) bool) (string, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	for attempt := 0; attempt < 3; attempt++ {
		if !found {
			_, _, postErr := c.post(createPath, data)
			if postErr == nil {
				return "created", nil
			}
			if !IsK8sError(postErr, http.StatusConflict) {
				return "", postErr
			}
			if current, found, err = read(); err != nil {
				return "", err
			}
			continue
		}
		if !owned(current) {
			return "", &foreignObjectError{Desc: desc}
		}
		if current.terminating() {
			return "", fmt.Errorf("%s is being deleted; wait until it is gone, then retry", desc)
		}
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
		_, _, applyErr := c.apply(path, guarded)
		if applyErr == nil {
			return "updated", nil
		}
		if !IsK8sError(applyErr, http.StatusConflict) {
			return "", applyErr
		}
		if current, found, err = read(); err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("%s: %w", desc, errObjectChanged)
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
		dashHost    string
		minioEP     minioEndpoints
		minioEPErr  error
		status      = &types.ResourcesStatus{}
	)
	wg.Add(5)
	go func() { defer wg.Done(); projects, projectsErr = GetDSProjects(c) }()
	go func() { defer wg.Done(); dspas, dspasErr = listDSPAs(c, "") }()
	go func() { defer wg.Done(); status.MinIO, minioEP, minioEPErr = minioStatusAndEndpoints(c) }()
	go func() { defer wg.Done(); status.MLflow = getMLflowStatus(c) }()
	go func() { defer wg.Done(); dashHost = routeHost(c, dashboardNamespace, "rhods-dashboard") }()
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
		status.PipelineServers = append(status.PipelineServers, pipelineServerState(d, dashHost))
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
