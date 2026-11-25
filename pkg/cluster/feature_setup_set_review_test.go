package cluster

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
)

// withKueuePackage makes the Red Hat build of Kueue installable on pc.
func withKueuePackage(pc *prereqCluster) {
	pc.sim.packages["kueue-operator"] = fakePackage{source: "redhat-operators", defaultChannel: "stable-v1.5", order: []string{"stable-v1.5"},
		installModes: []string{"AllNamespaces"}, channels: map[string]fakeChannel{"stable-v1.5": {head: "1.5.1", entries: []string{"1.5.1"}}}}
	pc.putPackage("kueue-operator")
}

var modelsAndKueue = []features.SetItem{{Feature: "models"}, {Feature: "kueue", OperatorsOnly: true}}

// When Connectivity Link already serves Kuadrant's API, the Kuadrant CR is
// dry-run with every other write, before any change: a refusal changes
// nothing.
func TestPrerequisitesSet_KuadrantDryRunFirstWhenItsAPIIsServed(t *testing.T) {
	pc := newPrereqCluster(t)
	pc.sim.installed("rhcl-operator", "kuadrant-system", "rhcl-operator", "stable", "1.4.3")
	pc.f.mu.Lock()
	pc.f.groups = map[string]string{"/apis/kuadrant.io": kuadrantGroupDoc}
	installKuadrantCRDLocked(pc.f)
	for _, v := range []string{"v1", "v1beta1"} {
		pc.f.failDryRun["POST /apis/kuadrant.io/"+v+"/namespaces/kuadrant-system/kuadrants"] = http.StatusUnprocessableEntity
	}
	pc.f.mu.Unlock()
	res, events := runPrereqSet(t, pc.c, []features.SetItem{{Feature: "pipelines"}, {Feature: "models", Exposure: serving.ExposureDefault}})
	if res.Success || !strings.Contains(res.Message, "The API server rejects the Kuadrant CR kuadrant-system/kuadrant") || !strings.Contains(res.Message, "Nothing was changed.") {
		t.Fatalf("setup: %+v (%s)", res, finalSteps(events))
	}
	if m := pc.f.mutations(); len(m) != 0 || recordWrites(pc.f) != 0 {
		t.Errorf("writes %v, record writes %d", m, recordWrites(pc.f))
	}
}

// The waits of a set share its operation's deadline: each ends
// setWaitMargin before it, with what is not ready (and the step's own
// message), instead of being cut off by the deadline.
func TestPrerequisitesSet_WaitsEndBeforeTheOperationDeadline(t *testing.T) {
	pc := newPrereqCluster(t)
	prev := pc.f.onMergePatch
	pc.f.onMergePatch = func(r *http.Request, k fakeKey, patch, next map[string]any) {
		prev(r, k, patch, next)
		if k.plural == "datascienceclusters" && r.URL.Query().Get("dryRun") == "" {
			status := ensureMap(next, "status")
			conds, _ := status["conditions"].([]any)
			status["conditions"] = setFakeCondition(conds, "aipipelinesReady", "False", "its pod is pending")
		}
	}
	ctx, cancel := kube.Clock().WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	res, events := runPrereqSet(t, pc.c.WithContext(ctx), []features.SetItem{{Feature: "pipelines"}})
	want := "Not ready after " + serving.ClockText(4*time.Minute-30*time.Second) + " (aipipelines: its pod is pending)."
	if res.Success || res.ErrorCode != "not_ready" || !strings.Contains(res.Message, want) || ctx.Err() != nil {
		t.Fatalf("setup: %+v (%s), context %v; want %q", res, finalSteps(events), ctx.Err(), want)
	}
}

// A Subscription the set created stays when a later operator's install
// fails; its step ends, saying so, instead of staying "running".
func TestPrerequisitesSet_ALaterInstallFailureEndsTheEarlierSteps(t *testing.T) {
	pc := newPrereqCluster(t)
	withKueuePackage(pc)
	pc.f.mu.Lock()
	pc.f.fail["POST /apis/operators.coreos.com/v1alpha1/namespaces/openshift-kueue-operator/subscriptions"] = http.StatusInternalServerError
	pc.f.mu.Unlock()
	res, events := runPrereqSet(t, pc.c, modelsAndKueue)
	if res.Success || !strings.Contains(res.Message, "Failed to") {
		t.Fatalf("setup: %+v", res)
	}
	if got := finalSteps(events); got != "prerequisites=success dsc=success cert-manager=skipped kueue-operator=failed" {
		t.Errorf("steps = %s", got)
	}
	last := ""
	for _, e := range events {
		if e.Step == "cert-manager" {
			last = e.Message
		}
	}
	if last != "cert-manager Operator for Red Hat OpenShift: Subscription cert-manager-operator/openshift-cert-manager-operator created; Set up again to wait for it." {
		t.Errorf("cert-manager's last message %q", last)
	}
}

// One operator whose resolution keeps failing fails the set on its own
// step, with OLM's reason; the other, which installs, ends on its own
// step as installed. Progress lines name their operator.
func TestPrerequisitesSet_OneOperatorDoesNotInstall(t *testing.T) {
	pc := newPrereqCluster(t)
	withKueuePackage(pc)
	// The catalog lists the channel, but OLM cannot resolve it.
	p := pc.sim.packages["openshift-cert-manager-operator"]
	p.channels = map[string]fakeChannel{}
	pc.sim.packages["openshift-cert-manager-operator"] = p
	res, events := runPrereqSet(t, pc.c, modelsAndKueue)
	if res.Success || res.ErrorCode != "not_ready" || !strings.Contains(res.Message, "cert-manager Operator for Red Hat OpenShift does not install: OLM cannot resolve it") {
		t.Fatalf("setup: %+v (%s)", res, finalSteps(events))
	}
	if got := finalSteps(events); got != "prerequisites=success dsc=success cert-manager=failed kueue-operator=success" {
		t.Errorf("steps = %s", got)
	}
	prefixed := false
	for _, e := range events {
		if e.Step == "kueue-operator" && strings.HasPrefix(e.Message, "Red Hat build of Kueue: ") {
			prefixed = true
		}
	}
	if !prefixed {
		t.Errorf("no progress line names its operator: %+v", events)
	}
}

// A run cancelled while several operators are awaited ends every wait:
// each step fails, and the set fails as cancelled.
func TestPrerequisitesSet_CancelledWhileWaiting(t *testing.T) {
	pc := newPrereqCluster(t)
	withKueuePackage(pc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lists := map[string]bool{
		strings.TrimSuffix(tCertSub, "/openshift-cert-manager-operator"): true,
		strings.TrimSuffix(tKueueSub, "/kueue-operator"):                 true,
	}
	serve := pc.f.beforeServe
	pc.f.beforeServe = func(method, path string) {
		if method == http.MethodGet && lists[path] && pc.f.has(tCertSub) && pc.f.has(tKueueSub) {
			cancel() // the first poll of a wait: nothing is installed yet
			return
		}
		serve(method, path)
	}
	res, events := runPrereqSet(t, pc.c.WithContext(ctx), modelsAndKueue)
	if res.Success || res.ErrorCode != "cancelled" || !strings.HasPrefix(res.Message, "Stopped waiting for cert-manager Operator for Red Hat OpenShift") {
		t.Fatalf("setup: %+v (%s)", res, finalSteps(events))
	}
	if got := finalSteps(events); got != "prerequisites=success dsc=success cert-manager=failed kueue-operator=failed" {
		t.Errorf("steps = %s", got)
	}
}

// A wait that panics fails its operator's step with an internal error;
// the others still end as they do.
func TestPrerequisitesSet_AWaitThatPanics(t *testing.T) {
	pc := newPrereqCluster(t)
	withKueuePackage(pc)
	var events []UpdateStepEvent
	res, err := features.RunPrerequisitesSet(pc.c, modelsAndKueue, func(e UpdateStepEvent) {
		events = append(events, e)
		if e.Step == "kueue-operator" && strings.HasPrefix(e.Message, "Waiting for OLM") {
			panic("boom")
		}
	})
	if err != nil || res.Success || res.ErrorCode != "internal" || res.Message != "The wait for Red Hat build of Kueue stopped on an internal error. Its Subscription stays; Set up again to continue." {
		t.Fatalf("setup: %+v %v", res, err)
	}
	if got := finalSteps(events); got != "prerequisites=success dsc=success cert-manager=success kueue-operator=failed" {
		t.Errorf("steps = %s", got)
	}
}
