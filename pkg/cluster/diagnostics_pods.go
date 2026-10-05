package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Pod classification shared by the "Operator pods" and "RHOAI pods" checks
// (and therefore by the stuck-reconcile guidance on the Status page, which
// shows the top diagnostics problem).
//
// Kubernetes reports phase=Pending both for pods the scheduler cannot place
// and for scheduled pods whose containers have not started yet ("This
// includes time a Pod spends waiting to be scheduled as well as the time
// spent downloading container images", kubernetes.io/docs/concepts/workloads/
// pods/pod-lifecycle). Only PodScheduled=False with reason Unschedulable is a
// capacity problem; everything else is classified by the container waiting
// reason and, for ContainerCreating, by the pod's latest Warning event
// (FailedMount names the missing Secret or ConfigMap).

// podIssueGrace is how long a pod may be Pending or ContainerCreating before
// diagnostics reports it. Rollouts normally pass through both states.
var podIssueGrace = 60 * time.Second

const (
	// restartThreshold and restartWindow flag containers that keep restarting
	// even when they happen to be Running at the moment of the scan.
	restartThreshold = 5
	restartWindow    = 30 * time.Minute
	// maxEventLookups bounds the per-check event queries for stuck pods.
	maxEventLookups = 10
)

type podIssueKind string

const (
	issueUnschedulable   podIssueKind = "unschedulable"
	issueNotScheduled    podIssueKind = "not-scheduled"
	issueStuckCreating   podIssueKind = "stuck-creating"
	issueCrashLoop       podIssueKind = "crashloop"
	issueRestarting      podIssueKind = "restarting"
	issueImagePull       podIssueKind = "image-pull"
	issueContainerConfig podIssueKind = "container-config"
)

// podIssue is one pod's most important problem.
type podIssue struct {
	Namespace string
	Pod       string
	Owner     string // Deployment name for ReplicaSet pods, else the owner or pod name
	OwnerKind string // "Deployment", the owner's kind, or "Pod"
	Kind      podIssueKind
	Container string
	Reason    string // Kubernetes reason, e.g. ImagePullBackOff, FailedMount
	Message   string // detail from the status or event
	Restarts  int
	Age       time.Duration
}

type diagContainerState struct {
	Waiting *struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"waiting"`
	Running *struct {
		StartedAt string `json:"startedAt"`
	} `json:"running"`
	Terminated *diagTerminated `json:"terminated"`
}

type diagTerminated struct {
	Reason     string `json:"reason"`
	Message    string `json:"message"`
	ExitCode   int    `json:"exitCode"`
	FinishedAt string `json:"finishedAt"`
}

type diagContainerStatus struct {
	Name         string             `json:"name"`
	Ready        bool               `json:"ready"`
	RestartCount int                `json:"restartCount"`
	State        diagContainerState `json:"state"`
	LastState    struct {
		Terminated *diagTerminated `json:"terminated"`
	} `json:"lastState"`
}

type diagPod struct {
	Metadata struct {
		Name              string            `json:"name"`
		Namespace         string            `json:"namespace"`
		UID               string            `json:"uid"`
		CreationTimestamp string            `json:"creationTimestamp"`
		DeletionTimestamp string            `json:"deletionTimestamp"`
		Labels            map[string]string `json:"labels"`
		OwnerReferences   []struct {
			Kind       string `json:"kind"`
			Name       string `json:"name"`
			UID        string `json:"uid"`
			Controller *bool  `json:"controller"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Status struct {
		Phase      string `json:"phase"`
		Conditions []struct {
			Type               string `json:"type"`
			Status             string `json:"status"`
			Reason             string `json:"reason"`
			Message            string `json:"message"`
			LastTransitionTime string `json:"lastTransitionTime"`
		} `json:"conditions"`
		InitContainerStatuses []diagContainerStatus `json:"initContainerStatuses"`
		ContainerStatuses     []diagContainerStatus `json:"containerStatuses"`
	} `json:"status"`
}

// listDiagPods lists pods in namespace, optionally filtered by a label selector.
func listDiagPods(c *Client, namespace, labelSelector string) ([]diagPod, error) {
	var query url.Values
	if labelSelector != "" {
		query = url.Values{"labelSelector": {labelSelector}}
	}
	body, _, err := c.do(http.MethodGet, namespacedPath("v1", "pods", namespace, ""), "", nil, query)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []diagPod `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse pods in %s: %w", namespace, err)
	}
	return list.Items, nil
}

func parseK8sTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// podOwner returns the workload that owns the pod. ReplicaSet pods map to
// their Deployment: the ReplicaSet name is "<deployment>-<pod-template-hash>".
func podOwner(p diagPod) (string, string) {
	for _, ref := range p.Metadata.OwnerReferences {
		if ref.Controller != nil && !*ref.Controller {
			continue
		}
		if ref.Kind == "ReplicaSet" {
			if hash := p.Metadata.Labels["pod-template-hash"]; hash != "" && strings.HasSuffix(ref.Name, "-"+hash) {
				return strings.TrimSuffix(ref.Name, "-"+hash), "Deployment"
			}
		}
		return ref.Name, ref.Kind
	}
	return p.Metadata.Name, "Pod"
}

var (
	imagePullReasons = map[string]bool{
		"ImagePullBackOff": true, "ErrImagePull": true, "InvalidImageName": true, "ErrImageNeverPull": true,
	}
	containerConfigReasons = map[string]bool{
		"CreateContainerConfigError": true, "CreateContainerError": true, "RunContainerError": true,
		"PreStartHookError": true, "PostStartHookError": true,
	}
)

// classifyPod returns the pod's most important problem, or nil when the pod
// is healthy, finished, terminating, or still inside the grace period.
func classifyPod(p diagPod, now time.Time) *podIssue {
	if p.Metadata.DeletionTimestamp != "" || p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed" {
		return nil
	}
	created, _ := parseK8sTime(p.Metadata.CreationTimestamp)
	owner, ownerKind := podOwner(p)
	issue := func(kind podIssueKind, container, reason, message string, restarts int) *podIssue {
		return &podIssue{
			Namespace: p.Metadata.Namespace, Pod: p.Metadata.Name, Owner: owner, OwnerKind: ownerKind,
			Kind: kind, Container: container, Reason: reason, Message: strings.TrimSpace(message),
			Restarts: restarts, Age: now.Sub(created),
		}
	}

	statuses := append(append([]diagContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)

	// Hard container errors are reported immediately: they do not resolve on
	// their own within a rollout.
	for _, cs := range statuses {
		w := cs.State.Waiting
		if w == nil {
			continue
		}
		switch {
		case w.Reason == "CrashLoopBackOff":
			return issue(issueCrashLoop, cs.Name, w.Reason, describeTermination(cs.LastState.Terminated, w.Message), cs.RestartCount)
		case imagePullReasons[w.Reason]:
			return issue(issueImagePull, cs.Name, w.Reason, w.Message, cs.RestartCount)
		case containerConfigReasons[w.Reason]:
			return issue(issueContainerConfig, cs.Name, w.Reason, w.Message, cs.RestartCount)
		}
	}

	// A container that is up right now but keeps dying.
	for _, cs := range statuses {
		last := cs.LastState.Terminated
		if cs.RestartCount < restartThreshold || last == nil {
			continue
		}
		if finished, ok := parseK8sTime(last.FinishedAt); ok && now.Sub(finished) <= restartWindow {
			return issue(issueRestarting, cs.Name, last.Reason, describeTermination(last, ""), cs.RestartCount)
		}
	}

	if p.Status.Phase != "Pending" || now.Sub(created) < podIssueGrace {
		return nil
	}

	scheduled := false
	for _, cond := range p.Status.Conditions {
		if cond.Type != "PodScheduled" {
			continue
		}
		if cond.Status == "True" {
			scheduled = true
			break
		}
		if cond.Reason == "Unschedulable" {
			return issue(issueUnschedulable, "", cond.Reason, cond.Message, 0)
		}
		msg := cond.Message
		if msg == "" {
			msg = fmt.Sprintf("PodScheduled=False (%s)", cond.Reason)
		}
		return issue(issueNotScheduled, "", cond.Reason, msg, 0)
	}
	if !scheduled {
		return issue(issueNotScheduled, "", "", "The scheduler has not placed this pod on a node yet.", 0)
	}

	// Scheduled but containers have not started (ContainerCreating,
	// PodInitializing, or no status yet). The cause is in the events.
	reason := "ContainerCreating"
	for _, cs := range statuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			reason = cs.State.Waiting.Reason
			break
		}
	}
	return issue(issueStuckCreating, "", reason, "", 0)
}

func describeTermination(t *diagTerminated, fallback string) string {
	if t == nil {
		return fallback
	}
	parts := []string{fmt.Sprintf("last exit: %s (exit code %d)", nonEmpty(t.Reason, "Terminated"), t.ExitCode)}
	if msg := strings.TrimSpace(t.Message); msg != "" {
		parts = append(parts, truncate(msg, 300))
	}
	return strings.Join(parts, ": ")
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// errEventsForbidden marks an events lookup refused by RBAC.
var errEventsForbidden = errors.New("events are not readable")

// latestWarningEvent returns "Reason: message" of the newest Warning event for
// a pod, or "" when there is none.
func latestWarningEvent(c *Client, namespace, podName string) (string, error) {
	query := url.Values{"fieldSelector": {"involvedObject.kind=Pod,involvedObject.name=" + podName}}
	body, _, err := c.do(http.MethodGet, namespacedPath("v1", "events", namespace, ""), "", nil, query)
	if err != nil {
		if IsK8sError(err, http.StatusForbidden) {
			return "", errEventsForbidden
		}
		return "", err
	}
	var list struct {
		Items []struct {
			Type           string `json:"type"`
			Reason         string `json:"reason"`
			Message        string `json:"message"`
			LastTimestamp  string `json:"lastTimestamp"`
			EventTime      string `json:"eventTime"`
			FirstTimestamp string `json:"firstTimestamp"`
			Series         *struct {
				LastObservedTime string `json:"lastObservedTime"`
			} `json:"series"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return "", fmt.Errorf("parse events: %w", err)
	}
	var best string
	var bestTime time.Time
	for _, ev := range list.Items {
		if ev.Type != "Warning" {
			continue
		}
		var when time.Time
		candidates := []string{ev.LastTimestamp, ev.EventTime, ev.FirstTimestamp}
		if ev.Series != nil {
			candidates = append([]string{ev.Series.LastObservedTime}, candidates...)
		}
		// eventTime is a MicroTime (fractional seconds); time.Parse with
		// RFC3339 accepts those too.
		for _, s := range candidates {
			if t, ok := parseK8sTime(s); ok {
				when = t
				break
			}
		}
		if best == "" || when.After(bestTime) {
			best = fmt.Sprintf("%s: %s", ev.Reason, strings.TrimSpace(ev.Message))
			bestTime = when
		}
	}
	return best, nil
}

// missingObjectPattern extracts the object named in kubelet errors such as
// `secret "odh-observability-webhook-cert" not found`.
var missingObjectPattern = regexp.MustCompile(`(secret|configmap|persistentvolumeclaim|serviceaccount) "([^"]+)" not found`)

// shortCause condenses a pod issue into the phrase used in problem titles.
func shortCause(is podIssue) string {
	if m := missingObjectPattern.FindStringSubmatch(strings.ToLower(is.Message)); m != nil {
		// Keep the original casing of the object name.
		idx := strings.Index(strings.ToLower(is.Message), m[0])
		return is.Message[idx : idx+len(m[0])]
	}
	switch is.Kind {
	case issueUnschedulable:
		return truncate(is.Message, 160)
	case issueImagePull, issueContainerConfig:
		return is.Reason
	}
	return ""
}

// enrichStuckPods fills in the Warning event that explains pods stuck in
// ContainerCreating. At most maxEventLookups pods are queried.
func enrichStuckPods(c *Client, issues []podIssue) {
	looked := 0
	for i := range issues {
		if issues[i].Kind != issueStuckCreating {
			continue
		}
		if looked >= maxEventLookups {
			issues[i].Message = fmt.Sprintf("Containers have not started (%s). Run `oc describe pod %s -n %s` to see why.", issues[i].Reason, issues[i].Pod, issues[i].Namespace)
			continue
		}
		looked++
		ev, err := latestWarningEvent(c, issues[i].Namespace, issues[i].Pod)
		switch {
		case errors.Is(err, errEventsForbidden):
			issues[i].Message = fmt.Sprintf("Containers have not started (%s). This app cannot read events (RBAC), so the cause is unknown here; run `oc describe pod %s -n %s`.", issues[i].Reason, issues[i].Pod, issues[i].Namespace)
		case err != nil:
			issues[i].Message = fmt.Sprintf("Containers have not started (%s). Could not read events: %v", issues[i].Reason, err)
		case ev == "":
			issues[i].Message = fmt.Sprintf("Containers have not started (%s) and there are no Warning events.", issues[i].Reason)
		default:
			issues[i].Message = ev
			if colon := strings.Index(ev, ":"); colon > 0 {
				issues[i].Reason = ev[:colon]
			}
		}
	}
}

// podIssueGroup is the set of pods of one workload that share a problem kind.
type podIssueGroup struct {
	Namespace, Owner, OwnerKind string
	Kind                        podIssueKind
	Issues                      []podIssue
}

func groupPodIssues(issues []podIssue) []podIssueGroup {
	index := map[string]int{}
	var groups []podIssueGroup
	for _, is := range issues {
		key := is.Namespace + "/" + is.OwnerKind + "/" + is.Owner + "/" + string(is.Kind)
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			groups = append(groups, podIssueGroup{Namespace: is.Namespace, Owner: is.Owner, OwnerKind: is.OwnerKind, Kind: is.Kind})
		}
		groups[i].Issues = append(groups[i].Issues, is)
	}
	for i := range groups {
		sort.Slice(groups[i].Issues, func(a, b int) bool { return groups[i].Issues[a].Pod < groups[i].Issues[b].Pod })
	}
	sort.SliceStable(groups, func(a, b int) bool {
		if groups[a].Namespace != groups[b].Namespace {
			return groups[a].Namespace < groups[b].Namespace
		}
		if groups[a].Owner != groups[b].Owner {
			return groups[a].Owner < groups[b].Owner
		}
		return groups[a].Kind < groups[b].Kind
	})
	return groups
}

func formatDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

// podGroupProblem turns a group of pod issues into a diagnostics problem.
// critical marks groups in the operator namespace.
func podGroupProblem(c *Client, g podIssueGroup, critical bool) Problem {
	first := g.Issues[0]
	n := len(g.Issues)
	podWord := "pod"
	if n > 1 {
		podWord = "pods"
	}
	severity := "warning"
	if critical {
		severity = "critical"
	}

	var evidence, affected []string
	maxRestarts := 0
	for _, is := range g.Issues {
		line := fmt.Sprintf("Pod %s/%s", is.Namespace, is.Pod)
		if is.Container != "" {
			line += fmt.Sprintf(", container %s", is.Container)
		}
		if is.Reason != "" {
			line += ", " + is.Reason
		}
		if is.Restarts > 0 {
			line += fmt.Sprintf(", %d restarts", is.Restarts)
		}
		line += fmt.Sprintf(", age %s", formatDuration(is.Age))
		if is.Message != "" {
			line += ": " + truncate(is.Message, 400)
		}
		evidence = append(evidence, line)
		affected = append(affected, fmt.Sprintf("Pod %s/%s", is.Namespace, is.Pod))
		if is.Restarts > maxRestarts {
			maxRestarts = is.Restarts
		}
	}

	cause := shortCause(first)
	withCause := func(s string) string {
		if cause == "" {
			return s
		}
		return fmt.Sprintf("%s: %s", s, cause)
	}
	describeCmd := fmt.Sprintf("oc describe pod %s -n %s", first.Pod, first.Namespace)

	p := Problem{
		ID:              fmt.Sprintf("pod-%s-%s-%s", g.Kind, g.Namespace, g.Owner),
		Severity:        severity,
		Evidence:        evidence,
		AffectedObjects: affected,
		TechnicalCmd:    describeCmd,
	}

	switch g.Kind {
	case issueStuckCreating:
		p.Title = withCause(fmt.Sprintf("%s: %d %s stuck in %s", g.Owner, n, podWord, nonEmpty(stuckState(g.Issues), "ContainerCreating")))
		p.Description = fmt.Sprintf("The %s %s scheduled on a node, but the containers have not started for %s. This is not a capacity problem: the kubelet is waiting for something the pod needs.", podWord, isAre(n), formatDuration(first.Age))
		if strings.Contains(strings.ToLower(first.Reason+first.Message), "failedmount") || missingObjectPattern.MatchString(strings.ToLower(first.Message)) {
			p.Fix = "The pod mounts an object that does not exist yet (see the message). The pod starts on its own once the object exists. Check which component should create it in the operator logs and on the Components page."
		} else {
			p.Fix = "Read the pod events for the cause."
		}
	case issueUnschedulable:
		p.Title = withCause(fmt.Sprintf("%s: %d %s cannot be scheduled", g.Owner, n, podWord))
		p.Description = "The scheduler reports that no node can run the pod. The message lists the reason for each node (for example Insufficient cpu or memory, taints, or node selectors)."
		p.Fix = "Free capacity on the nodes or add nodes. If a rollout is waiting for old pods to release resources, the tool can offer to unblock it."
		if g.OwnerKind == "Deployment" {
			attachRolloutAssist(c, &p, g.Namespace, g.Owner)
		}
	case issueNotScheduled:
		p.Title = fmt.Sprintf("%s: %d %s waiting for the scheduler", g.Owner, n, podWord)
		p.Description = "The pod has not been placed on a node yet and the scheduler has not reported it as unschedulable."
		p.Fix = "Wait a minute and re-scan. If it persists, check the pod's scheduling gates and the scheduler."
	case issueCrashLoop:
		p.Title = fmt.Sprintf("%s is crash-looping (%d restarts)", g.Owner, maxRestarts)
		p.Description = fmt.Sprintf("Container %s keeps exiting and Kubernetes is backing off before restarting it.", first.Container)
		p.Fix = "Read the logs of the previous run of the container (command below). A crash right after an update usually means the build is broken or a dependency is missing."
		p.TechnicalCmd = fmt.Sprintf("oc logs -n %s %s -c %s --previous --tail=50", first.Namespace, first.Pod, first.Container)
	case issueRestarting:
		p.Title = fmt.Sprintf("%s keeps restarting (%d restarts)", g.Owner, maxRestarts)
		p.Description = fmt.Sprintf("Container %s is running now but restarted recently and has restarted %d times in total.", first.Container, maxRestarts)
		p.Fix = "Read the logs of the previous run of the container (command below)."
		p.TechnicalCmd = fmt.Sprintf("oc logs -n %s %s -c %s --previous --tail=50", first.Namespace, first.Pod, first.Container)
	case issueImagePull:
		p.Title = fmt.Sprintf("%s cannot pull its image (%s)", g.Owner, first.Reason)
		p.Description = "The node cannot pull the container image. The message below is the registry's answer."
		p.Fix = "Check the pull secret and the image mirror (IDMS) in Setup. After changing them, nodes need 2-3 minutes to pick them up."
	case issueContainerConfig:
		p.Title = withCause(fmt.Sprintf("%s cannot start its container", g.Owner))
		p.Description = fmt.Sprintf("Kubernetes cannot create container %s (%s).", first.Container, first.Reason)
		p.Fix = "Create the missing object, or check what the container configuration references (see the message)."
	}
	return p
}

func stuckState(issues []podIssue) string {
	for _, is := range issues {
		if is.Reason == "ContainerCreating" || is.Reason == "PodInitializing" {
			return is.Reason
		}
	}
	return ""
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// scanPods lists pods in the given namespaces and returns their problems.
// Namespaces that do not exist are skipped. The returned counts report how
// many pods and namespaces were inspected.
func scanPods(c *Client, namespaces []string, now time.Time) (issues []podIssue, podCount int, scanned []string, errs []string) {
	for _, ns := range namespaces {
		pods, err := listDiagPods(c, ns, "")
		if err != nil {
			if IsK8sError(err, http.StatusNotFound) {
				continue
			}
			errs = append(errs, fmt.Sprintf("%s: %v", ns, err))
			continue
		}
		scanned = append(scanned, ns)
		podCount += len(pods)
		for _, p := range pods {
			if p.Metadata.Namespace == "" {
				p.Metadata.Namespace = ns
			}
			if is := classifyPod(p, now); is != nil {
				issues = append(issues, *is)
			}
		}
	}
	enrichStuckPods(c, issues)
	return issues, podCount, scanned, errs
}
