package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Ownership of the quick resources (MinIO, pipeline servers, MLflow).
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

// podIssue is the most relevant reason a workload's pods are not ready.
type podIssue struct {
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
func inspectPods(c *Client, namespace, selector, wantImage string) (podIssue, error) {
	body, _, err := c.get(namespacedPath("v1", "pods", namespace, "") + "?labelSelector=" + url.QueryEscape(selector))
	if err != nil {
		return podIssue{}, err
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
		return podIssue{}, fmt.Errorf("parse pod list: %w", err)
	}
	var best podIssue
	consider := func(p podIssue) {
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
				consider(podIssue{Reason: firstNonEmpty(cond.Reason, "Unschedulable"), Message: cond.Message})
			}
		}
		statuses := append(append([]containerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
		for _, cs := range statuses {
			if w := cs.State.Waiting; w != nil && w.Reason != "" {
				msg := w.Message
				if t := cs.LastState.Terminated; t != nil && w.Reason == "CrashLoopBackOff" {
					msg = strings.TrimSpace(fmt.Sprintf("%s (last exit: %s, code %d)", msg, t.Reason, t.ExitCode))
				}
				consider(podIssue{Reason: w.Reason, Message: msg, Terminal: terminalWaitingReasons[w.Reason]})
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
func applyPodIssue(state *types.ResourceState, issue podIssue) {
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
// lists DS projects, pipeline servers (one cluster-wide LIST), MinIO, MLflow
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
		status      = &types.ResourcesStatus{}
	)
	wg.Add(5)
	go func() { defer wg.Done(); projects, projectsErr = GetDSProjects(c) }()
	go func() { defer wg.Done(); dspas, dspasErr = listDSPAs(c, "") }()
	go func() { defer wg.Done(); status.MinIO = getMinIOStatus(c) }()
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

	if status.MinIO.Deployed && status.MinIO.ManagedByTool {
		if reason := minioTeardownBlocker(dspas); reason != "" {
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

// brokenConversionWebhooks returns namespaced CRDs whose conversion webhook
// Service is missing or has no ready endpoints. The namespace controller has
// to list every namespaced type, and requests that need conversion fail
// while the webhook is down; OLM's source notes this "breaks kubernetes
// garbage collection" (operator-lifecycle-manager pkg/controller/operators/
// olm/operator.go), so a namespace deleted then can hang in Terminating.
// Any read error is returned so callers fail closed.
//
// The diagnostics stale-webhook check (agent B2) implements the same §3.4
// rule set; this read-only copy keeps teardown independent of it.
func brokenConversionWebhooks(c *Client) ([]string, error) {
	type svcRef struct{ ns, name string }
	var broken []string
	health := map[svcRef]string{}
	cont := ""
	for {
		path := "/apis/apiextensions.k8s.io/v1/customresourcedefinitions?limit=100"
		if cont != "" {
			path += "&continue=" + url.QueryEscape(cont)
		}
		body, _, err := c.get(path)
		if err != nil {
			return nil, fmt.Errorf("list CRDs: %w", err)
		}
		var list struct {
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Spec struct {
					Scope      string `json:"scope"`
					Conversion struct {
						Strategy string `json:"strategy"`
						Webhook  struct {
							ClientConfig struct {
								Service *struct {
									Namespace string `json:"namespace"`
									Name      string `json:"name"`
								} `json:"service"`
							} `json:"clientConfig"`
						} `json:"webhook"`
					} `json:"conversion"`
				} `json:"spec"`
			} `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("parse CRD list: %w", err)
		}
		for _, crd := range list.Items {
			svc := crd.Spec.Conversion.Webhook.ClientConfig.Service
			if crd.Spec.Scope != "Namespaced" || crd.Spec.Conversion.Strategy != "Webhook" || svc == nil {
				continue
			}
			ref := svcRef{svc.Namespace, svc.Name}
			problem, seen := health[ref]
			if !seen {
				if problem, err = serviceProblem(c, svc.Namespace, svc.Name); err != nil {
					return nil, err
				}
				health[ref] = problem
			}
			if problem != "" {
				broken = append(broken, fmt.Sprintf("%s (Service %s/%s %s)", crd.Metadata.Name, svc.Namespace, svc.Name, problem))
			}
		}
		if list.Metadata.Continue == "" {
			break
		}
		cont = list.Metadata.Continue
	}
	sort.Strings(broken)
	return broken, nil
}

// serviceProblem returns "not found" or "has no ready endpoints" for a
// Service that cannot serve, and "" when it has a ready endpoint.
func serviceProblem(c *Client, namespace, name string) (string, error) {
	if _, _, err := c.get(namespacedPath("v1", "services", namespace, name)); err != nil {
		if IsK8sError(err, 404) {
			return "not found", nil
		}
		return "", fmt.Errorf("read Service %s/%s: %w", namespace, name, err)
	}
	body, _, err := c.get(namespacedPath("discovery.k8s.io/v1", "endpointslices", namespace, "") + "?labelSelector=" + url.QueryEscape("kubernetes.io/service-name="+name))
	if err != nil {
		return "", fmt.Errorf("list endpoints of Service %s/%s: %w", namespace, name, err)
	}
	var slices struct {
		Items []struct {
			Endpoints []struct {
				Conditions struct {
					// A nil ready condition means ready (EndpointConditions API).
					Ready *bool `json:"ready"`
				} `json:"conditions"`
			} `json:"endpoints"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &slices); err != nil {
		return "", fmt.Errorf("parse endpoints of Service %s/%s: %w", namespace, name, err)
	}
	for _, s := range slices.Items {
		for _, ep := range s.Endpoints {
			if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
				return "", nil
			}
		}
	}
	return "has no ready endpoints", nil
}
