package cluster

import (
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/clock/clocktest"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const tKueueSub = "/apis/operators.coreos.com/v1alpha1/namespaces/openshift-kueue-operator/subscriptions/kueue-operator"

func runPrereqSet(t *testing.T, c *Client, items []features.SetItem) (*types.OperationResponse, []UpdateStepEvent) {
	t.Helper()
	var events []UpdateStepEvent
	res, err := features.RunPrerequisitesSet(c, items, func(e UpdateStepEvent) { events = append(events, e) })
	if err != nil {
		t.Fatalf("RunPrerequisitesSet: %v", err)
	}
	return res, events
}

// recordWrites counts the writes of the snapshot ConfigMap that holds the
// prerequisites record (dry runs aside).
func recordWrites(f *resourceFake) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Method == http.MethodGet || c.Query.Get("dryRun") != "" {
			continue
		}
		if strings.HasSuffix(c.Path, "/configmaps/rhoai-nightly-updater-snapshot") || (strings.HasSuffix(c.Path, "/configmaps") && strings.Contains(c.Body, "rhoai-nightly-updater-snapshot")) {
			n++
		}
	}
	return n
}

// readyWaits are the "ready" step's starts.
func readyWaits(events []UpdateStepEvent) []string {
	var out []string
	for _, e := range events {
		if e.Step == "ready" && strings.HasPrefix(e.Message, "Waiting for RHOAI's operator to report") {
			out = append(out, e.Message)
		}
	}
	return out
}

// On a fresh cluster a set of features is set up in one run: one check
// with every dry run first, the record written once, every component
// turned on (one patch each, under its own manager), cert-manager
// installed, and one wait for all of them to be ready. Each component is
// recorded for its own feature, so the tabs' Clean up keeps working. Run
// again, it changes nothing.
func TestPrerequisitesSet_FreshClusterInOneRun(t *testing.T) {
	pc := newPrereqCluster(t)
	items := []features.SetItem{{Feature: "pipelines"}, {Feature: "mlflow"}, {Feature: "models"}, {Feature: "playground"}}
	st, err := features.GetPrerequisitesSetStatus(pc.c, items)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if fp := st.Features[it.Feature]; fp.Ready || fp.SetupBlockedReason != "" {
			t.Fatalf("%s before: %+v", it.Feature, fp)
		}
	}
	res, events := runPrereqSet(t, pc.c, items)
	if !res.Success || res.Message != "What Pipelines, MLflow, Models and Playground need on the cluster is set up." {
		t.Fatalf("setup: %+v (%s)", res, finalSteps(events))
	}
	if got := finalSteps(events); got != "prerequisites=success dsc=success cert-manager=success ready=success" {
		t.Errorf("steps = %s", got)
	}
	want := []string{"PATCH " + tDSC, "PATCH " + tDSC, "PATCH " + tDSC, "PATCH " + tDSC, "PATCH " + tDSC,
		"POST /api/v1/namespaces", "POST " + tCertGroups, "POST /apis/operators.coreos.com/v1alpha1/namespaces/cert-manager-operator/subscriptions"}
	if got := pc.f.mutations(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("writes =\n%s", strings.Join(got, "\n"))
	}
	if n := recordWrites(pc.f); n != 1 {
		t.Errorf("the record was written %d times", n)
	}
	rec, err := features.ReadPrereqRecord(pc.c)
	if err != nil {
		t.Fatal(err)
	}
	for key, feature := range map[string]string{"aipipelines": "pipelines", "mlflowoperator": "mlflow", "kserve": "models", "ogx": "playground", "dashboard": "playground"} {
		if rc := rec.Components[key]; rc.Feature != feature || rc.Previous != "Removed" || componentState(pc.f, key) != "Managed" {
			t.Errorf("%s: record %+v, state %s", key, rc, componentState(pc.f, key))
		}
	}
	if rec.Shared["cert-manager"] == "" {
		t.Errorf("shared %v", rec.Shared)
	}
	if w := readyWaits(events); len(w) != 1 || !strings.Contains(w[0], "aipipelines, mlflowoperator, kserve, ogx and dashboard") {
		t.Errorf("ready waits %q", w)
	}
	// Each tab sees its feature set up, with its own components to turn back.
	all, err := GetPrerequisitesStatusFor(pc.c, "")
	if err != nil {
		t.Fatal(err)
	}
	for feature, n := range map[string]int{"pipelines": 1, "mlflow": 1, "models": 1, "playground": 2} {
		if fp := all.Features[feature]; !fp.Ready || len(fp.Restorable) != n || fp.Restorable[0].Feature != feature {
			t.Errorf("%s after: %+v", feature, fp)
		}
	}
	// Again: nothing to do.
	writes := len(pc.f.mutations())
	res, events = runPrereqSet(t, pc.c, items)
	if !res.Success || len(pc.f.mutations()) != writes || recordWrites(pc.f) != 1 {
		t.Errorf("again: %+v (%s), writes %v", res, finalSteps(events), pc.f.mutations())
	}
}

// A dry run the API server refuses, for the last feature of the set,
// stops the set before any write: no component of the features before it
// is turned on, and the record is not written.
func TestPrerequisitesSet_ADryRunRefusalChangesNothing(t *testing.T) {
	pc := newPrereqCluster(t)
	pc.f.mu.Lock()
	pc.f.failDryRun["POST /api/v1/namespaces"] = http.StatusForbidden
	pc.f.mu.Unlock()
	res, events := runPrereqSet(t, pc.c, []features.SetItem{{Feature: "pipelines"}, {Feature: "mlflow"}, {Feature: "models"}})
	if res.Success || !strings.Contains(res.Message, "The API server rejects cert-manager Operator for Red Hat OpenShift's install") || !strings.HasSuffix(strings.TrimSpace(strings.SplitAfter(res.Message, "Nothing was changed.")[0]), "Nothing was changed.") {
		t.Fatalf("setup: %+v", res)
	}
	if got := finalSteps(events); got != "prerequisites=failed" {
		t.Errorf("steps = %s", got)
	}
	if m := pc.f.mutations(); len(m) != 0 {
		t.Errorf("writes %v", m)
	}
	if n := recordWrites(pc.f); n != 0 {
		t.Errorf("the record was written %d times", n)
	}
	// The other features' writes were dry-run first.
	pc.f.mu.Lock()
	dry := strings.Join(pc.f.requests, "\n")
	pc.f.mu.Unlock()
	if strings.Count(dry, "DRYRUN PATCH "+tDSC) != 3 {
		t.Errorf("requests:\n%s", dry)
	}
	for _, key := range []string{"aipipelines", "mlflowoperator", "kserve"} {
		if componentState(pc.f, key) != "Removed" {
			t.Errorf("%s changed", key)
		}
	}
}

// The operators of a set are waited for at once, under one deadline: each
// installs only once its own wait has watched its Subscription for a
// while (cert-manager 3 minutes, Kueue 4), so waiting one after the other
// would take 7 minutes, together only 4. The test moves a manual fake
// clock in lockstep with the code: by one poll interval, only once every
// wait that runs sleeps (one per operator Subscription created and not
// installed yet, else the run's one wait), so how the goroutines are
// scheduled does not change what the clock shows. Kueue joins with its
// operators only: its component stays as it is (kueue-prereq turns it on
// later), and cert-manager, which both need, is installed once.
func TestPrerequisitesSet_OperatorsAreWaitedForTogether(t *testing.T) {
	pc := newPrereqCluster(t)
	f := pc.f
	f.putJSON(tDSC, `{"metadata":{"name":"default-dsc"},"spec":{"components":{"kserve":{"managementState":"Removed"},"kueue":{"managementState":"Removed"}}},
		"status":{"conditions":[{"type":"KserveLLMInferenceServiceDependencies","status":"False","message":"cert-manager operator not installed"}]}}`)
	pc.sim.packages["kueue-operator"] = fakePackage{source: "redhat-operators", defaultChannel: "stable-v1.5", order: []string{"stable-v1.5"},
		installModes: []string{"AllNamespaces"}, channels: map[string]fakeChannel{"stable-v1.5": {head: "1.5.1", entries: []string{"1.5.1"}}}}
	pc.putPackage("kueue-operator")
	clk := clocktest.NewFake()
	t.Cleanup(kube.SetClockForTest(clk))
	t0 := clk.Now()
	// The waits read each Subscription in its namespace's list.
	type watched struct {
		pkg, sub string
		delay    time.Duration
	}
	delay := map[string]watched{
		strings.TrimSuffix(tCertSub, "/openshift-cert-manager-operator"): {"openshift-cert-manager-operator", tCertSub, 3 * time.Minute},
		strings.TrimSuffix(tKueueSub, "/kueue-operator"):                 {"kueue-operator", tKueueSub, 4 * time.Minute},
	}
	var mu sync.Mutex
	firstRead, installedAt := map[string]time.Time{}, map[string]time.Time{}
	f.beforeServe = func(method, list string) {
		if w, ok := delay[list]; ok && method == http.MethodGet {
			if !f.has(w.sub) {
				return // not created yet: the plan's read
			}
			now := kube.Clock().Now()
			mu.Lock()
			if firstRead[w.pkg].IsZero() {
				firstRead[w.pkg] = now
			}
			due := now.Sub(firstRead[w.pkg]) >= w.delay
			mu.Unlock()
			if !due {
				return
			}
		}
		pc.sim.step(method, list)
		if method == http.MethodGet && strings.Contains(list, "/datascienceclusters") {
			pc.kserveChecks()
		}
	}
	prev := pc.sim.onInstalled
	pc.sim.onInstalled = func(pkg, ns, from, to string) {
		prev(pkg, ns, from, to)
		mu.Lock()
		installedAt[pkg] = kube.Clock().Now()
		mu.Unlock()
	}
	// waits is how many waits run: one per Subscription created and not
	// installed yet, else the run's own.
	waits := func() int {
		n := 0
		for _, w := range delay {
			mu.Lock()
			installed := !installedAt[w.pkg].IsZero()
			mu.Unlock()
			if !installed && f.has(w.sub) {
				n++
			}
		}
		return max(n, 1)
	}
	var (
		res    *types.OperationResponse
		runErr error
		events []UpdateStepEvent
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, runErr = features.RunPrerequisitesSet(pc.c, []features.SetItem{{Feature: "models"}, {Feature: "kueue", OperatorsOnly: true}}, func(e UpdateStepEvent) { events = append(events, e) })
	}()
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
			if clk.Sleepers() >= waits() {
				clk.Advance(envkit.PollInterval)
			} else {
				runtime.Gosched()
			}
		}
	}
	if runErr != nil || !res.Success {
		t.Fatalf("setup: %+v %v (%s)", res, runErr, finalSteps(events))
	}
	if got := finalSteps(events); got != "prerequisites=success dsc=success cert-manager=success kueue-operator=success ready=success" {
		t.Errorf("steps = %s", got)
	}
	mu.Lock()
	got := map[string]time.Duration{}
	for pkg, at := range installedAt {
		got[pkg] = at.Sub(t0)
	}
	started := firstRead["openshift-cert-manager-operator"].Equal(t0) && firstRead["kueue-operator"].Equal(t0)
	mu.Unlock()
	if !started || got["openshift-cert-manager-operator"] != 3*time.Minute || got["kueue-operator"] != 4*time.Minute {
		t.Errorf("both waits began at once: %v; installed after %v, want cert-manager 3m and Kueue 4m (one after the other: 7m)", started, got)
	}
	creates := 0
	for _, c := range f.callsTo(http.MethodPost, "/apis/operators.coreos.com/v1alpha1/namespaces/cert-manager-operator/subscriptions") {
		if c.Query.Get("dryRun") == "" {
			creates++
		}
	}
	if creates != 1 {
		t.Errorf("cert-manager subscribed %d times", creates)
	}
	rec, err := features.ReadPrereqRecord(pc.c)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.Components["kueue"]; ok || componentState(f, "kueue") != "Removed" || rec.Shared["kueue-operator"] == "" || rec.Shared["cert-manager"] == "" {
		t.Errorf("kueue %s, record %+v", componentState(f, "kueue"), rec)
	}
	// Its status in the set: its operators only, which are ready.
	st, err := features.GetPrerequisitesSetStatus(pc.c, []features.SetItem{{Feature: "kueue", OperatorsOnly: true}})
	if err != nil || !st.Features["kueue"].Ready {
		t.Errorf("kueue operators: %+v %v", st, err)
	}
}

// Models reached through a gateway brings Connectivity Link and its
// Kuadrant CR into the set: the Kuadrant CR is made once, and one ready
// wait covers every feature's components. The record, written once before
// any change, already names the Kuadrant CR.
func TestPrerequisitesSet_ConnectivityLinkOnceAndOneReadyWait(t *testing.T) {
	pc := newPrereqCluster(t)
	pc.sim.packages["rhcl-operator"] = rhclFakePackage()
	pc.putPackage("rhcl-operator")
	prev := pc.sim.onInstalled
	pc.sim.onInstalled = func(pkg, ns, from, to string) {
		prev(pkg, ns, from, to)
		if pkg == "rhcl-operator" {
			pc.f.groups = map[string]string{"/apis/kuadrant.io": kuadrantGroupDoc}
			installKuadrantCRDLocked(pc.f)
		}
	}
	pc.f.fail["GET /apis/kuadrant.io/v1/kuadrants"] = 404
	res, events := runPrereqSet(t, pc.c, []features.SetItem{{Feature: "pipelines"}, {Feature: "models", Exposure: serving.ExposureDefault}})
	if !res.Success {
		t.Fatalf("setup: %+v (%s)", res, finalSteps(events))
	}
	if got := finalSteps(events); got != "prerequisites=success dsc=success cert-manager=success rhcl=success kuadrant=success ready=success" {
		t.Errorf("steps = %s", got)
	}
	creates := 0
	for _, c := range pc.f.callsTo(http.MethodPost, "/apis/kuadrant.io/v1beta1/namespaces/kuadrant-system/kuadrants") {
		if c.Query.Get("dryRun") == "" {
			creates++
		}
	}
	if creates != 1 || !pc.f.has("/apis/kuadrant.io/v1beta1/namespaces/kuadrant-system/kuadrants/kuadrant") {
		t.Errorf("%d Kuadrant creates", creates)
	}
	if w := readyWaits(events); len(w) != 1 || !strings.Contains(w[0], "aipipelines and kserve") {
		t.Errorf("ready waits %q", w)
	}
	rec, err := features.ReadPrereqRecord(pc.c)
	if err != nil || rec.Shared["kuadrant"] != "Kuadrant kuadrant-system/kuadrant" || rec.Shared["rhcl"] == "" || recordWrites(pc.f) != 1 {
		t.Errorf("record %+v %v, %d writes", rec, err, recordWrites(pc.f))
	}
}

// A set is refused before any read when it cannot run as one: no
// feature, a feature named twice, Kueue with its component (its operators
// come first), an exposure for anything but Models.
func TestPrerequisitesSet_Validation(t *testing.T) {
	for _, tc := range []struct {
		items []features.SetItem
		want  string
	}{
		{nil, "at least one feature"},
		{[]features.SetItem{{Feature: "pipelines"}, {Feature: "pipelines"}}, "named twice"},
		{[]features.SetItem{{Feature: "kueue"}}, "takes only its operators"},
		{[]features.SetItem{{Feature: "pipelines", Exposure: serving.ExposureDefault}}, "models only"},
		{[]features.SetItem{{Feature: "notebooks"}}, "feature must be"},
	} {
		if got := features.ValidatePrerequisitesSet(tc.items); !strings.Contains(got, tc.want) {
			t.Errorf("%+v: %q", tc.items, got)
		}
	}
	pc := newPrereqCluster(t)
	res, _ := runPrereqSet(t, pc.c, []features.SetItem{{Feature: "kueue"}})
	if res.Success || res.ErrorCode != "validation" || len(pc.f.mutations()) != 0 {
		t.Errorf("%+v", res)
	}
}
