package cluster

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// rolloutNamespaces are the only namespaces assist-rollout may patch.
var rolloutNamespaces = []string{"redhat-ods-operator", "redhat-ods-applications"}

// Assist-rollout unblocks exactly one situation: a RollingUpdate Deployment
// whose new pods are Unschedulable while its old pods still hold the
// resources, and whose strategy allows zero unavailable pods (the default
// 25% rounds down to 0 for fewer than 4 replicas). Setting maxUnavailable=1
// lets the Deployment controller stop one old pod; the scheduler then retries
// the Unschedulable pod because a pod deletion frees node resources.
//
// The patch is not reverted by OLM: OLM only compares the
// olm.deployment-spec-hash label with the CSV's deployment spec
// (operator-lifecycle-manager pkg/controller/install/deployment.go,
// checkForDeployments). Component operators server-side apply their
// manifests with ForceOwnership, so they restore the strategy only when their
// manifest sets it. The tool patches once per user action, so there is no
// patch loop; the result message says that the setting can stay.

type rolloutAssessment struct {
	Namespace       string
	Deployment      string
	Applicable      bool
	Reason          string // why it is not applicable
	ResourceVersion string
	Replicas        int
	MaxUnavailable  string // current value as written in the spec ("25%" when unset)
	NewReplicaSet   string
	PendingPods     []string
	OldReadyPods    int
}

func (a rolloutAssessment) target() string { return a.Namespace + "/" + a.Deployment }

func notApplicable(a rolloutAssessment, reason string) rolloutAssessment {
	a.Applicable = false
	a.Reason = reason
	return a
}

// resolveIntOrPercent mirrors intstr.GetScaledValueFromIntOrPercent.
func resolveIntOrPercent(v interface{}, total int, roundUp bool, def string) (int, string, error) {
	if v == nil {
		v = def
	}
	switch val := v.(type) {
	case float64:
		return int(val), strconv.Itoa(int(val)), nil
	case string:
		if !strings.HasSuffix(val, "%") {
			return 0, val, fmt.Errorf("invalid value %q", val)
		}
		pct, err := strconv.Atoi(strings.TrimSuffix(val, "%"))
		if err != nil {
			return 0, val, fmt.Errorf("invalid value %q", val)
		}
		scaled := float64(pct) * float64(total) / 100
		if roundUp {
			return int(math.Ceil(scaled)), val, nil
		}
		return int(math.Floor(scaled)), val, nil
	}
	return 0, fmt.Sprint(v), fmt.Errorf("invalid value %v", v)
}

func labelSelectorString(matchLabels map[string]string) string {
	keys := make([]string, 0, len(matchLabels))
	for k := range matchLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+matchLabels[k])
	}
	return strings.Join(parts, ",")
}

// assessRolloutBlock reads the Deployment, its ReplicaSets (selected by the
// Deployment's own spec.selector.matchLabels and filtered by controller
// ownerReference UID, because several RHOAI Deployments share selector
// labels such as control-plane=controller-manager) and the new ReplicaSet's
// pods, and decides whether a maxUnavailable=1 patch would unblock it.
func assessRolloutBlock(c *Client, namespace, name string) (rolloutAssessment, error) {
	a := rolloutAssessment{Namespace: namespace, Deployment: name}

	body, _, err := c.get(namespacedPath("apps/v1", "deployments", namespace, name))
	if err != nil {
		if IsK8sError(err, http.StatusNotFound) {
			return notApplicable(a, "the Deployment no longer exists"), nil
		}
		return a, fmt.Errorf("read deployment: %w", err)
	}
	var dep struct {
		Metadata struct {
			UID             string `json:"uid"`
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Spec struct {
			Replicas *int `json:"replicas"`
			Selector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"selector"`
			Strategy struct {
				Type          string `json:"type"`
				RollingUpdate *struct {
					MaxUnavailable interface{} `json:"maxUnavailable"`
					MaxSurge       interface{} `json:"maxSurge"`
				} `json:"rollingUpdate"`
			} `json:"strategy"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &dep); err != nil {
		return a, fmt.Errorf("parse deployment: %w", err)
	}
	a.ResourceVersion = dep.Metadata.ResourceVersion
	a.Replicas = 1
	if dep.Spec.Replicas != nil {
		a.Replicas = *dep.Spec.Replicas
	}
	if dep.Spec.Strategy.Type != "" && dep.Spec.Strategy.Type != "RollingUpdate" {
		return notApplicable(a, fmt.Sprintf("the Deployment uses the %s strategy", dep.Spec.Strategy.Type)), nil
	}
	var rawUnavailable, rawSurge interface{}
	if ru := dep.Spec.Strategy.RollingUpdate; ru != nil {
		rawUnavailable, rawSurge = ru.MaxUnavailable, ru.MaxSurge
	}
	unavailable, unavailableStr, err := resolveIntOrPercent(rawUnavailable, a.Replicas, false, "25%")
	if err != nil {
		return notApplicable(a, "cannot read maxUnavailable: "+err.Error()), nil
	}
	surge, _, err := resolveIntOrPercent(rawSurge, a.Replicas, true, "25%")
	if err != nil {
		return notApplicable(a, "cannot read maxSurge: "+err.Error()), nil
	}
	a.MaxUnavailable = unavailableStr
	// The Deployment controller allows one unavailable pod when both resolve
	// to zero (deployment util ResolveFenceposts).
	if surge == 0 && unavailable == 0 {
		unavailable = 1
	}
	if unavailable >= 1 {
		return notApplicable(a, fmt.Sprintf("the strategy already allows %d unavailable pod(s) (maxUnavailable %s of %d replicas), so it is not what blocks the rollout", unavailable, unavailableStr, a.Replicas)), nil
	}

	var rsQuery url.Values
	if len(dep.Spec.Selector.MatchLabels) > 0 {
		rsQuery = url.Values{"labelSelector": {labelSelectorString(dep.Spec.Selector.MatchLabels)}}
	}
	rsBody, _, err := c.do(http.MethodGet, namespacedPath("apps/v1", "replicasets", namespace, ""), "", nil, rsQuery)
	if err != nil {
		return a, fmt.Errorf("list replicasets: %w", err)
	}
	var rsList struct {
		Items []struct {
			Metadata struct {
				Name            string            `json:"name"`
				UID             string            `json:"uid"`
				Labels          map[string]string `json:"labels"`
				Annotations     map[string]string `json:"annotations"`
				OwnerReferences []struct {
					UID        string `json:"uid"`
					Controller *bool  `json:"controller"`
				} `json:"ownerReferences"`
			} `json:"metadata"`
			Spec struct {
				Replicas *int `json:"replicas"`
			} `json:"spec"`
			Status struct {
				ReadyReplicas int `json:"readyReplicas"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rsBody, &rsList); err != nil {
		return a, fmt.Errorf("parse replicasets: %w", err)
	}

	type rsInfo struct {
		name, uid, hash string
		revision        int
		desired, ready  int
	}
	var owned []rsInfo
	for _, rs := range rsList.Items {
		ownedByDep := false
		for _, ref := range rs.Metadata.OwnerReferences {
			if ref.UID == dep.Metadata.UID && ref.Controller != nil && *ref.Controller {
				ownedByDep = true
			}
		}
		if !ownedByDep {
			continue
		}
		rev, err := strconv.Atoi(rs.Metadata.Annotations["deployment.kubernetes.io/revision"])
		if err != nil {
			slog.Warn("assist-rollout: ReplicaSet without a numeric revision", "replicaSet", rs.Metadata.Name)
			continue
		}
		desired := 0
		if rs.Spec.Replicas != nil {
			desired = *rs.Spec.Replicas
		}
		owned = append(owned, rsInfo{
			name: rs.Metadata.Name, uid: rs.Metadata.UID, hash: rs.Metadata.Labels["pod-template-hash"],
			revision: rev, desired: desired, ready: rs.Status.ReadyReplicas,
		})
	}
	if len(owned) == 0 {
		return notApplicable(a, "no ReplicaSets belong to the Deployment"), nil
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].revision > owned[j].revision })
	newest := owned[0]
	a.NewReplicaSet = newest.name
	for _, old := range owned[1:] {
		a.OldReadyPods += old.ready
	}
	if newest.desired == 0 || newest.ready >= newest.desired {
		return notApplicable(a, "the newest ReplicaSet is not waiting for pods"), nil
	}
	if a.OldReadyPods == 0 {
		return notApplicable(a, "no old pods are holding resources, so stopping one would not help"), nil
	}

	podSelector := ""
	if newest.hash != "" {
		podSelector = "pod-template-hash=" + newest.hash
	}
	pods, err := listDiagPods(c, namespace, podSelector)
	if err != nil {
		return a, fmt.Errorf("list pods: %w", err)
	}
	for _, p := range pods {
		ownedByRS := false
		for _, ref := range p.Metadata.OwnerReferences {
			if ref.UID == newest.uid {
				ownedByRS = true
			}
		}
		if !ownedByRS || p.Metadata.DeletionTimestamp != "" || p.Status.Phase != "Pending" {
			continue
		}
		for _, cond := range p.Status.Conditions {
			if cond.Type == "PodScheduled" && cond.Status == "False" && cond.Reason == "Unschedulable" {
				a.PendingPods = append(a.PendingPods, p.Metadata.Name)
			}
		}
	}
	if len(a.PendingPods) == 0 {
		return notApplicable(a, "the new pods are not Unschedulable, so the cause is not node capacity"), nil
	}
	sort.Strings(a.PendingPods)
	a.Applicable = true
	return a, nil
}

// assistConfirmMessage is the confirmation text for one applicable assessment.
func assistConfirmMessage(a rolloutAssessment) string {
	return fmt.Sprintf("This patches Deployment %s: spec.strategy.rollingUpdate.maxUnavailable %s -> 1 (%d replicas). "+
		"Kubernetes then stops one of the %d old pod(s) so that the Unschedulable pod(s) %s can use its resources. "+
		"One replica is unavailable during the switch. No pods are deleted by the tool.\n\n"+
		"The new value can stay on the Deployment until its owner rewrites it (OLM on the next operator update, or the component operator if its manifest sets the strategy).",
		a.target(), a.MaxUnavailable, a.Replicas, a.OldReadyPods, strings.Join(a.PendingPods, ", "))
}

// attachRolloutAssist offers the assist-rollout fix on an Unschedulable
// problem only when the precondition holds right now.
func attachRolloutAssist(c *Client, p *Problem, namespace, deployment string) {
	if !isRolloutNamespace(namespace) {
		return
	}
	a, err := assessRolloutBlock(c, namespace, deployment)
	if err != nil {
		p.Evidence = append(p.Evidence, fmt.Sprintf("Could not check the rollout of %s/%s: %v", namespace, deployment, err))
		return
	}
	if !a.Applicable {
		p.Evidence = append(p.Evidence, fmt.Sprintf("Automatic unblock not offered for Deployment %s: %s.", a.target(), a.Reason))
		return
	}
	p.Fix = fmt.Sprintf("The rollout of %s is waiting for old pods to release resources. The tool can allow Kubernetes to replace one old pod at a time (maxUnavailable=1).", a.target())
	p.AutoFixable = true
	p.AutoFixAction = "assist-rollout:" + a.target()
	p.ConfirmMessage = assistConfirmMessage(a)
	p.AffectedObjects = append([]string{"Deployment " + a.target()}, p.AffectedObjects...)
}

func isRolloutNamespace(ns string) bool {
	for _, n := range rolloutNamespaces {
		if n == ns {
			return true
		}
	}
	return false
}

var dns1123Name = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// patchMaxUnavailable re-checks the precondition and sets maxUnavailable=1,
// guarded by the resourceVersion that was assessed.
func patchMaxUnavailable(c *Client, namespace, name string) (rolloutAssessment, bool, string) {
	a, err := assessRolloutBlock(c, namespace, name)
	if err != nil {
		return a, false, fmt.Sprintf("%s/%s: could not check the rollout: %v", namespace, name, err)
	}
	if !a.Applicable {
		return a, false, ""
	}
	patch, _ := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"resourceVersion": a.ResourceVersion},
		"spec": map[string]interface{}{
			"strategy": map[string]interface{}{"rollingUpdate": map[string]interface{}{"maxUnavailable": 1}},
		},
	})
	respBody, _, err := c.strategicPatch(namespacedPath("apps/v1", "deployments", namespace, name), patch)
	if err != nil {
		if IsK8sError(err, http.StatusConflict) {
			return a, false, fmt.Sprintf("%s: the Deployment changed while it was being checked; nothing was patched. Run diagnostics again.", a.target())
		}
		return a, false, fmt.Sprintf("%s: patch failed: %v", a.target(), err)
	}
	var patched struct {
		Spec struct {
			Strategy struct {
				RollingUpdate *struct {
					MaxUnavailable interface{} `json:"maxUnavailable"`
				} `json:"rollingUpdate"`
			} `json:"strategy"`
		} `json:"spec"`
	}
	if json.Unmarshal(respBody, &patched) != nil || patched.Spec.Strategy.RollingUpdate == nil ||
		fmt.Sprint(patched.Spec.Strategy.RollingUpdate.MaxUnavailable) != "1" {
		return a, false, fmt.Sprintf("%s: the API accepted the patch but the returned Deployment does not show maxUnavailable=1", a.target())
	}
	slog.Info("assist-rollout: set maxUnavailable=1", "deployment", a.target(), "pendingPods", a.PendingPods)
	return a, true, ""
}

// AssistRollout unblocks every Deployment in the RHOAI namespaces whose
// rollout is blocked by Unschedulable new pods and a zero maxUnavailable.
func AssistRollout(c *Client) (*types.OperationResponse, error) {
	var targets []string
	var logs []string
	for _, ns := range rolloutNamespaces {
		deps, err := getDeployments(c, ns)
		if err != nil {
			logs = append(logs, fmt.Sprintf("Warning: could not list deployments in %s: %v", ns, err))
			continue
		}
		for _, dep := range deps {
			// Only Deployments that are short of updated or ready pods can
			// have a blocked rollout.
			if dep.Desired > 0 && (dep.UpdatedReplicas < dep.Desired || dep.Ready < dep.Desired) {
				targets = append(targets, ns+"/"+dep.Name)
			}
		}
	}
	return assistTargets(c, targets, logs)
}

// AssistRolloutFor unblocks one Deployment, if its rollout is blocked.
func AssistRolloutFor(c *Client, namespace, name string) (*types.OperationResponse, error) {
	if !isRolloutNamespace(namespace) || !dns1123Name.MatchString(name) || len(name) > 253 {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Assist rollout only works on Deployments in %s.", strings.Join(rolloutNamespaces, " or ")),
			ErrorCode: "validation",
		}, nil
	}
	return assistTargets(c, []string{namespace + "/" + name}, nil)
}

func assistTargets(c *Client, targets []string, logs []string) (*types.OperationResponse, error) {
	var patched, failed []string
	for _, t := range targets {
		ns, name, _ := strings.Cut(t, "/")
		a, ok, failure := patchMaxUnavailable(c, ns, name)
		switch {
		case ok:
			patched = append(patched, a.target())
			logs = append(logs, fmt.Sprintf("Patched Deployment %s: maxUnavailable %s -> 1 (Unschedulable pods: %s)", a.target(), a.MaxUnavailable, strings.Join(a.PendingPods, ", ")))
		case failure != "":
			failed = append(failed, t)
			logs = append(logs, "Error: "+failure)
		default:
			logs = append(logs, fmt.Sprintf("Skipped %s: %s", t, a.Reason))
		}
	}

	if len(patched) > 0 {
		RecordActivity(c, types.ActivityEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			User:      getUser(c),
			Action:    "assist-rollout",
			Detail:    "maxUnavailable=1 on " + strings.Join(patched, ", "),
			Success:   len(failed) == 0,
		})
	}

	switch {
	case len(patched) == 0 && len(failed) == 0:
		msg := "Nothing to do: no rollout is blocked by Unschedulable pods and a zero maxUnavailable. Nothing was changed."
		if len(targets) == 1 && len(logs) > 0 {
			msg = "Nothing to do: " + strings.TrimPrefix(logs[len(logs)-1], "Skipped ") + ". Nothing was changed."
		}
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: "nothing_to_do"}, nil
	case len(failed) > 0 && len(patched) == 0:
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Could not unblock %s. Nothing was changed.", strings.Join(failed, ", ")), Logs: logs}, nil
	case len(failed) > 0:
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Unblocked %s; failed on %s.", strings.Join(patched, ", "), strings.Join(failed, ", ")),
			Logs:      logs,
			ErrorCode: "partial_failure",
		}, nil
	}
	return &types.OperationResponse{
		Success: true,
		Message: fmt.Sprintf("Set maxUnavailable=1 on %s. Kubernetes now replaces one old pod at a time; the rollout should finish within a few minutes.", strings.Join(patched, ", ")),
		Logs:    logs,
	}, nil
}
