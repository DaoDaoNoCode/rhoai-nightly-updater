package cluster

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The Kuadrant CR is created only once the API servers can admit what it
// owns (serving.WaitKuadrantKindAdmissible): 45 seconds after its CRD
// turned Established. A CRD Established long ago needs no wait; a CRD that
// cannot be read, is not there or is not Established stops the step
// before the CR is created.

const (
	kuadrantCRDPath   = "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/" + serving.KuadrantCRD
	kuadrantCRDList   = "/apis/apiextensions.k8s.io/v1/customresourcedefinitions"
	kuadrantCRCreate  = "POST /apis/kuadrant.io/v1beta1/namespaces/kuadrant-system/kuadrants"
	kuadrantCRDWaitUp = "Waiting for the API server to accept the new Kuadrant type (up to "
)

// kuadrantCRD is CRD kuadrants.kuadrant.io as the API server serves it,
// its Established condition at status since established (the API server
// keeps condition times to the second, as RFC 3339).
func kuadrantCRD(established time.Time, status string) map[string]any {
	return kuadrantCRDAt(established.UTC().Format(time.RFC3339), status)
}

// kuadrantCRDAt is kuadrantCRD with the conditions' lastTransitionTime as
// it is written ("" none).
func kuadrantCRDAt(at, status string) map[string]any {
	cond := func(c map[string]any) map[string]any {
		if at != "" {
			c["lastTransitionTime"] = at
		}
		return c
	}
	conds := []any{cond(map[string]any{"type": "NamesAccepted", "status": "True", "reason": "NoConflicts", "message": "no conflicts found"})}
	if status != "" {
		est := map[string]any{"type": "Established", "status": status, "reason": "InitialNamesAccepted", "message": "the initial names have been accepted"}
		if status != "True" {
			est["reason"], est["message"] = "Installing", "the initial names have not been accepted yet"
		}
		conds = append(conds, cond(est))
	}
	return map[string]any{
		"metadata": map[string]any{"name": serving.KuadrantCRD},
		"spec": map[string]any{"group": "kuadrant.io", "scope": "Namespaced",
			"names":    map[string]any{"kind": "Kuadrant", "plural": "kuadrants", "singular": "kuadrant", "listKind": "KuadrantList"},
			"versions": []any{map[string]any{"name": "v1beta1", "served": true, "storage": true}}},
		"status": map[string]any{"conditions": conds, "acceptedNames": map[string]any{"kind": "Kuadrant", "plural": "kuadrants"}},
	}
}

// installKuadrantCRDLocked is OLM installing Connectivity Link's Kuadrant
// CRD, which turns Established now; a CRD left by an earlier install is
// adopted as it is. The fake's lock is held (fakeOLMSim.onInstalled).
func installKuadrantCRDLocked(f *resourceFake) {
	k, _ := parseFakePath(kuadrantCRDPath)
	if f.objects[k] == nil {
		f.objects[k] = kuadrantCRD(time.Now(), "True")
	}
}

// kuadrantCreateTiming records, on the wait clock, when the Kuadrant CRD was
// read and when the Kuadrant CR was created (not its dry run).
type kuadrantCreateTiming struct {
	mu              sync.Mutex
	crdRead, create time.Time
}

func watchKuadrantCreate(f *resourceFake) *kuadrantCreateTiming {
	tm := &kuadrantCreateTiming{}
	prev := f.beforeRequest
	f.beforeRequest = func(r *http.Request) {
		if prev != nil {
			prev(r)
		}
		tm.mu.Lock()
		defer tm.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == kuadrantCRDList && r.URL.Query().Get("fieldSelector") == "metadata.name="+serving.KuadrantCRD:
			tm.crdRead = kube.Clock().Now()
		case r.Method+" "+r.URL.Path == kuadrantCRCreate && r.URL.Query().Get("dryRun") == "":
			tm.create = kube.Clock().Now()
		}
	}
	return tm
}

// waited is the wait-clock time between the CRD read and the create.
func (tm *kuadrantCreateTiming) waited(t *testing.T) time.Duration {
	t.Helper()
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.crdRead.IsZero() || tm.create.IsZero() {
		t.Fatalf("CRD read at %v, CR created at %v", tm.crdRead, tm.create)
	}
	return tm.create.Sub(tm.crdRead)
}

// failKuadrantCRDRead makes the CRD list by the Kuadrant CRD's name answer
// as RBAC refuses it (other CRD reads go on).
func failKuadrantCRDRead(f *resourceFake) {
	f.intercept = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.Method != http.MethodGet || r.URL.Path != kuadrantCRDList || r.URL.Query().Get("fieldSelector") != "metadata.name="+serving.KuadrantCRD {
			return false
		}
		f.status(w, http.StatusForbidden, `customresourcedefinitions.apiextensions.k8s.io is forbidden: User "system:serviceaccount:rhoai-nightly-updater:rhoai-nightly-updater" cannot list resource "customresourcedefinitions" in API group "apiextensions.k8s.io" at the cluster scope`)
		return true
	}
}

// kuadrantMessages are the messages of the kuadrant step's events.
func kuadrantMessages(events []UpdateStepEvent) []string {
	var out []string
	for _, e := range events {
		if e.Step == "kuadrant" {
			out = append(out, e.Message)
		}
	}
	return out
}

func hasPrefixIn(list []string, prefix string) bool {
	return indexOfPrefix(list, prefix) >= 0
}

// maasClusterWithoutKuadrant is newMaaSCluster without the Kuadrant CR,
// its CRD Established at established (status "True"; "" none: no CRD).
func maasClusterWithoutKuadrant(t *testing.T, established time.Time, status string) *modelsCluster {
	t.Helper()
	mc := newMaaSCluster(t)
	mc.f.mu.Lock()
	delete(mc.f.objects, fakeKey{gv: "kuadrant.io/v1beta1", ns: "kuadrant-system", plural: "kuadrants", name: "kuadrant"})
	k, _ := parseFakePath(kuadrantCRDPath)
	delete(mc.f.objects, k)
	mc.f.mu.Unlock()
	if status != "" {
		mc.f.put(kuadrantCRDPath, kuadrantCRD(established, status))
	}
	return mc
}

func TestMaaSSetup_KuadrantCRWaitsUntilTheAPIServerKnowsItsKind(t *testing.T) {
	stamp := func(age time.Duration) string { return time.Now().Add(-age).UTC().Format(time.RFC3339) }
	for _, tc := range []struct {
		name string
		at   string // the CRD's lastTransitionTime
		// serverClock is how far the API server's clock is from this
		// tool's.
		serverClock time.Duration
		noDate      bool // the API server sends no Date header: the local clock
		min, max    time.Duration
	}{
		// The CRD's time and the Date header are kept to the second, so
		// the CRD may read up to a second older than it is.
		{"just established", stamp(0), 0, false, 43 * time.Second, 45 * time.Second},
		{"established 20 seconds ago", stamp(20 * time.Second), 0, false, 23 * time.Second, 25 * time.Second},
		{"established a day ago", stamp(24 * time.Hour), 0, false, 0, 0},
		{"just established, no Date header", stamp(0), 0, true, 43 * time.Second, 45 * time.Second},
		{"established a day ago, no Date header", stamp(24 * time.Hour), 0, true, 0, 0},
		// This tool's clock is 45 seconds ahead of the API server's (the
		// tool runs outside the cluster): by its own clock the CRD looks 45
		// seconds old, by the server's it is new.
		{"just established, the server's clock 45 seconds behind", stamp(45 * time.Second), -45 * time.Second, false, 43 * time.Second, 45 * time.Second},
		{"established a day ago, the server's clock 45 seconds ahead", stamp(24 * time.Hour), 45 * time.Second, false, 0, 0},
		// A time that cannot be trusted waits the whole time.
		{"no lastTransitionTime", "", 0, false, 45 * time.Second, 45 * time.Second},
		{"malformed lastTransitionTime", "yesterday", 0, false, 45 * time.Second, 45 * time.Second},
		{"lastTransitionTime in the future", time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339), 0, false, 45 * time.Second, 45 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mc := maasClusterWithoutKuadrant(t, time.Time{}, "")
			mc.f.put(kuadrantCRDPath, kuadrantCRDAt(tc.at, "True"))
			mc.f.serverClockOffset, mc.f.noDate = tc.serverClock, tc.noDate
			tm := watchKuadrantCreate(mc.f)
			res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG})
			if !res.Success {
				t.Fatalf("setup: %+v", res)
			}
			if !mc.f.has("/apis/kuadrant.io/v1beta1/namespaces/kuadrant-system/kuadrants/kuadrant") {
				t.Fatal("no Kuadrant CR")
			}
			if got := tm.waited(t); got < tc.min || got > tc.max {
				t.Fatalf("the CR was created %v after the CRD was read, want %v to %v", got, tc.min, tc.max)
			}
			waitShown := hasPrefixIn(kuadrantMessages(events), kuadrantCRDWaitUp)
			if waitShown != (tc.max > 0) {
				t.Fatalf("wait shown %t; kuadrant step: %q", waitShown, kuadrantMessages(events))
			}
		})
	}
}

// A Set up whose request ends while it waits stops there: the Kuadrant CR
// is not created.
func TestMaaSSetup_KuadrantCRIsNotCreatedWhenTheWaitIsCancelled(t *testing.T) {
	mc := maasClusterWithoutKuadrant(t, time.Now(), "True")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	editTestClient(mc.c, func(o *testClientOptions) { o.Ctx = ctx })
	var events []UpdateStepEvent
	res, err := RunMaaS(mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}, func(e UpdateStepEvent) {
		events = append(events, e)
		if e.Step == "kuadrant" && strings.HasPrefix(e.Message, kuadrantCRDWaitUp) {
			cancel() // the user's request ends while the wait runs
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "Stopped waiting for the API server to accept the new Kuadrant type: context canceled. The Kuadrant CR was not created."
	if res.Success || res.Message != want || res.ErrorCode != "cancelled" {
		t.Fatalf("setup: %+v", res)
	}
	if !strings.HasSuffix(finalSteps(events), "kuadrant=failed") {
		t.Fatalf("steps = %s", finalSteps(events))
	}
	if hasPrefixIn(mc.f.mutations(), kuadrantCRCreate) {
		t.Fatalf("the Kuadrant CR was created: %v", mc.f.mutations())
	}
}

func TestMaaSSetup_KuadrantCRIsNotCreatedWhenItsCRDCannotBeTrusted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string // the CRD's Established status; "" no CRD
		fail   bool   // the CRD read is refused
		want   string
		code   string
	}{
		{"read refused", "True", true, "at the cluster scope. The Kuadrant CR was not created; Set up again to retry. The updater's ServiceAccount is not allowed to do this.", "forbidden"},
		{"no CRD", "", false, "CRD kuadrants.kuadrant.io is not there after 0:30 although Kuadrant's API is served. The Kuadrant CR was not created; Set up again to retry.", "not_ready"},
		{"not established", "False", false, "CRD kuadrants.kuadrant.io is not Established after 0:30 (Established is False: the initial names have not been accepted yet). The Kuadrant CR was not created; Set up again to retry.", "not_ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mc := maasClusterWithoutKuadrant(t, time.Now(), tc.status)
			if tc.fail {
				failKuadrantCRDRead(mc.f)
			}
			res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG})
			if res.Success || !strings.Contains(res.Message, tc.want) || res.ErrorCode != tc.code {
				t.Fatalf("setup: %+v", res)
			}
			if !strings.HasSuffix(finalSteps(events), "kuadrant=failed") {
				t.Fatalf("steps = %s", finalSteps(events))
			}
			if hasPrefixIn(mc.f.mutations(), kuadrantCRCreate) {
				t.Fatalf("the Kuadrant CR was created: %v", mc.f.mutations())
			}
		})
	}
}

// prereqClusterInstallingRHCL is newPrereqCluster where Set up for models
// through a gateway installs Connectivity Link (cert-manager first).
func prereqClusterInstallingRHCL(t *testing.T) *prereqCluster {
	t.Helper()
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
	return pc
}

func runModelsPrerequisites(t *testing.T, pc *prereqCluster) (*types.OperationResponse, []UpdateStepEvent) {
	t.Helper()
	var events []UpdateStepEvent
	res, err := RunPrerequisites(pc.c, types.PrerequisitesRequest{Feature: "models", Exposure: serving.ExposureDefault}, func(e UpdateStepEvent) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	return res, events
}

func TestPrerequisites_KuadrantCRWaitsUntilTheAPIServerKnowsItsKind(t *testing.T) {
	for _, tc := range []struct {
		name     string
		leftover time.Duration // age of a CRD left by an earlier install; 0 none
		min, max time.Duration
	}{
		{"installed now", 0, 43 * time.Second, 45 * time.Second},
		{"CRD left by an install a day ago", 24 * time.Hour, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pc := prereqClusterInstallingRHCL(t)
			if tc.leftover > 0 {
				pc.f.put(kuadrantCRDPath, kuadrantCRD(time.Now().Add(-tc.leftover), "True"))
			}
			tm := watchKuadrantCreate(pc.f)
			res, events := runModelsPrerequisites(t, pc)
			if !res.Success {
				t.Fatalf("setup: %+v (%s)", res, finalSteps(events))
			}
			if !pc.f.has("/apis/kuadrant.io/v1beta1/namespaces/kuadrant-system/kuadrants/kuadrant") {
				t.Fatal("no Kuadrant CR")
			}
			if got := tm.waited(t); got < tc.min || got > tc.max {
				t.Fatalf("the CR was created %v after the CRD was read, want %v to %v", got, tc.min, tc.max)
			}
			waitShown := hasPrefixIn(kuadrantMessages(events), kuadrantCRDWaitUp)
			if waitShown != (tc.max > 0) {
				t.Fatalf("wait shown %t; kuadrant step: %q", waitShown, kuadrantMessages(events))
			}
		})
	}
}

func TestPrerequisites_KuadrantCRIsNotCreatedWhenItsCRDCannotBeTrusted(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail bool
		want string
	}{
		{"read refused", true, "Cannot read CRD kuadrants.kuadrant.io, so this tool cannot tell whether the API server accepts the new Kuadrant type yet: "},
		{"not established", false, "CRD kuadrants.kuadrant.io is not Established after 0:30 (Established is False: the initial names have not been accepted yet). The Kuadrant CR was not created; Set up again to retry."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pc := prereqClusterInstallingRHCL(t)
			if tc.fail {
				failKuadrantCRDRead(pc.f)
			} else {
				pc.f.put(kuadrantCRDPath, kuadrantCRD(time.Now(), "False"))
			}
			res, events := runModelsPrerequisites(t, pc)
			if res.Success || !strings.Contains(res.Message, tc.want) {
				t.Fatalf("setup: %+v", res)
			}
			if !strings.Contains(finalSteps(events), "kuadrant=failed") {
				t.Fatalf("steps = %s", finalSteps(events))
			}
			if hasPrefixIn(pc.f.mutations(), kuadrantCRCreate) {
				t.Fatalf("the Kuadrant CR was created: %v", pc.f.mutations())
			}
		})
	}
}

// A CRD that is served but not Established yet (an HA control plane marks
// it Established 5 seconds later) is read again until it is.
func TestMaaSSetup_KuadrantCRWaitsForItsCRDToBeEstablished(t *testing.T) {
	mc := maasClusterWithoutKuadrant(t, time.Now(), "False")
	reads := 0
	prev := mc.f.beforeRequest
	mc.f.beforeRequest = func(r *http.Request) {
		if prev != nil {
			prev(r)
		}
		if r.Method == http.MethodGet && r.URL.Path == kuadrantCRDList && r.URL.Query().Get("fieldSelector") == "metadata.name="+serving.KuadrantCRD {
			if reads++; reads == 3 {
				mc.f.put(kuadrantCRDPath, kuadrantCRD(time.Now(), "True"))
			}
		}
	}
	res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG})
	if !res.Success || !hasPrefixIn(mc.f.mutations(), kuadrantCRCreate) {
		t.Fatalf("setup: %+v", res)
	}
	if !hasPrefixIn(kuadrantMessages(events), "Waiting for CRD kuadrants.kuadrant.io to be Established (up to 0:30)") || !hasPrefixIn(kuadrantMessages(events), kuadrantCRDWaitUp) {
		t.Fatalf("kuadrant step: %q", kuadrantMessages(events))
	}
}

// Set up with the signed-in user's token: a refused CRD read is the
// user's own RBAC, so the message names no ServiceAccount.
func TestMaaSSetup_KuadrantCRDReadRefusedForTheUser(t *testing.T) {
	mc := maasClusterWithoutKuadrant(t, time.Now(), "True")
	o := mc.c.TestOptions()
	user := newTestClient(testClientOptions{BaseURL: o.BaseURL, Token: fakeUserToken, HTTPClient: o.HTTPClient, Ctx: context.Background(), AsUser: true})
	user.SetBookkeeping(mc.c)
	user.SetFacts(mc.c)
	failKuadrantCRDRead(mc.f)
	res, _ := runMaaSOp(t, user, types.MaaSRequest{Action: "setup", Database: tCNPG})
	if res.Success || res.ErrorCode != "forbidden" || !strings.Contains(res.Message, "Cannot read CRD kuadrants.kuadrant.io") || strings.Contains(res.Message, "ServiceAccount") {
		t.Fatalf("setup: %+v", res)
	}
	if hasPrefixIn(mc.f.mutations(), kuadrantCRCreate) {
		t.Fatal("the Kuadrant CR was created")
	}
}

const (
	kuadrantLimitador  = `{"status":{"conditions":[{"type":"Ready","status":"False","reason":"LimitadorNotReady","message":"limitador resource not in topology"}]}}`
	kuadrantMissingDep = `{"status":{"conditions":[{"type":"Ready","status":"False","reason":"MissingDependency","message":"[Gateway API provider (istio / envoy gateway)] is not installed, please restart Kuadrant Operator pod once dependency has been installed"}]}}`
	kuadrantUnknown    = `{"status":{"conditions":[{"type":"Ready","status":"Unknown","reason":"Reconciling"}]}}`
	kuadrantReady      = `{"status":{"conditions":[{"type":"Ready","status":"True","reason":"Ready"}]}}`
	tKuadrantCR        = "/apis/kuadrant.io/v1beta1/namespaces/kuadrant-system/kuadrants/kuadrant"
	restartHint        = " and has not made Authorino; its operator can drop its own retry (Kuadrant/kuadrant-operator#2102), so a cluster-admin can restart it (oc rollout restart deployment/kuadrant-operator-controller-manager -n "
)

// dropAuthorino takes away what Kuadrant made of Authorino in
// kuadrant-system: its Deployment, and, unless keepCR, its CR and Service.
func dropAuthorino(f *resourceFake, keepCR bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.objects {
		if k.ns == "kuadrant-system" && (k.plural == "deployments" || (!keepCR && (k.plural == "services" || k.plural == "authorinos"))) {
			delete(f.objects, k)
		}
	}
}

// runMaaSWhen runs req and calls hook once, when step first reports a
// message starting with prefix (before the step goes on).
func runMaaSWhen(t *testing.T, c *Client, req types.MaaSRequest, step, prefix string, hook func()) (*types.OperationResponse, []UpdateStepEvent) {
	t.Helper()
	var events []UpdateStepEvent
	done := false
	res, err := RunMaaS(c, req, func(e UpdateStepEvent) {
		events = append(events, e)
		if !done && e.Step == step && strings.HasPrefix(e.Message, prefix) {
			done = true
			hook()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return res, events
}

// noOperatorChange fails the test when the tool changed Kuadrant's
// operator itself.
func noOperatorChange(t *testing.T, f *resourceFake) {
	t.Helper()
	for _, m := range f.mutations() {
		if strings.Contains(m, serving.KuadrantController) || strings.Contains(m, "/pods") {
			t.Errorf("the tool changed Kuadrant's operator itself: %s", m)
		}
	}
}

// Set up's wait for Kuadrant to be Ready ends without even the Authorino
// CR: Set up stops there, before CloudNativePG, the database and the
// gateway, and says why; the operator restart only for the reasons
// Kuadrant gives when Authorino or Limitador is missing from its topology.
func TestMaaSSetup_KuadrantNotReadyWithoutAuthorinoStopsSetUp(t *testing.T) {
	for _, tc := range []struct {
		name, kuadrant, want string
		restart              bool
	}{
		{"limitador not in topology", kuadrantLimitador, "Stopped after waiting 5:00 for Kuadrant to be Ready. Kuadrant kuadrant-system/kuadrant is not Ready (LimitadorNotReady: limitador resource not in topology)" + restartHint + "openshift-operators). Once Kuadrant is Ready, Set up again to continue.", true},
		{"missing dependency", kuadrantMissingDep, "Stopped after waiting 5:00 for Kuadrant to be Ready. Kuadrant kuadrant-system/kuadrant is not Ready (MissingDependency: [Gateway API provider (istio / envoy gateway)] is not installed, please restart Kuadrant Operator pod once dependency has been installed) and has not made Authorino. Once Kuadrant is Ready, Set up again to continue.", false},
		{"ready unknown", kuadrantUnknown, "Stopped after waiting 5:00 for Kuadrant to be Ready. Kuadrant kuadrant-system/kuadrant is not Ready yet (Reconciling) and has not made Authorino. Once Kuadrant is Ready, Set up again to continue.", false},
		{"no Ready condition", `{"status":{}}`, "Stopped after waiting 5:00 for Kuadrant to be Ready. Kuadrant kuadrant-system/kuadrant is not Ready yet (it reports no Ready condition) and has not made Authorino. Once Kuadrant is Ready, Set up again to continue.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mc := newMaaSCluster(t)
			mc.f.putJSON(tKuadrantCR, tc.kuadrant)
			dropAuthorino(mc.f, false)
			res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG})
			if res.Success || res.ErrorCode != "not_ready" || res.Message != tc.want || !strings.HasSuffix(finalSteps(events), "kuadrant=failed") {
				t.Fatalf("setup: %+v (%s)\nwant %q", res, finalSteps(events), tc.want)
			}
			for _, later := range []string{"POST /apis/postgresql.cnpg.io", "POST /apis/gateway.networking.k8s.io", "POST /api/v1/namespaces"} {
				if hasPrefixIn(mc.f.mutations(), later) {
					t.Errorf("Set up went on after Kuadrant: %v", mc.f.mutations())
				}
			}
			noOperatorChange(t, mc.f)
		})
	}
}

// Authorino's wait, when Kuadrant has not made Authorino: the message says
// what the polls found, not the wait's own deadline, then what the
// Kuadrant CR says, read again on its own context.
func TestMaaSAuthorinoTLS_MissingAuthorinoSaysWhy(t *testing.T) {
	const lead = "Authorino in kuadrant-system is not there after 5:00 (kuadrant has not made Authorino yet: no Authorino authorino in kuadrant-system)."
	for _, tc := range []struct {
		name   string
		action string
		// flip changes the cluster once the Authorino TLS step starts
		// waiting (Kuadrant was Ready with Authorino before).
		flip    func(f *resourceFake)
		step    string // the step event that triggers flip ("" the Authorino TLS wait)
		prefix  string
		want    string
		restart bool
	}{
		{name: "limitador not in topology", action: "setup", flip: func(f *resourceFake) { f.putJSON(tKuadrantCR, kuadrantLimitador); dropAuthorino(f, false) },
			want: lead + " Kuadrant kuadrant-system/kuadrant is not Ready (LimitadorNotReady: limitador resource not in topology)" + restartHint + "openshift-operators). Once Kuadrant is Ready, Set up again to continue.", restart: true},
		{name: "the operator's Deployment cannot be listed", action: "setup", flip: func(f *resourceFake) {
			f.putJSON(tKuadrantCR, kuadrantLimitador)
			dropAuthorino(f, false)
			f.mu.Lock()
			f.fail["GET /apis/apps/v1/deployments"] = http.StatusForbidden
			f.mu.Unlock()
		}, want: restartHint + "kuadrant-system). Once Kuadrant is Ready, Set up again to continue.", restart: true},
		{name: "Authorino without its Deployment", action: "setup", flip: func(f *resourceFake) { f.putJSON(tKuadrantCR, kuadrantLimitador); dropAuthorino(f, true) },
			want: "Authorino in kuadrant-system is not there after 5:00 (kuadrant has not made Authorino yet: no Deployment kuadrant-system/authorino). Kuadrant kuadrant-system/kuadrant is not Ready (LimitadorNotReady: limitador resource not in topology). Once Kuadrant is Ready, Set up again to continue."},
		{name: "ready", action: "setup", flip: func(f *resourceFake) { f.putJSON(tKuadrantCR, kuadrantReady); dropAuthorino(f, false) },
			want: lead + " Kuadrant kuadrant-system/kuadrant is Ready; Set up again to continue."},
		{name: "ready unknown", action: "setup", flip: func(f *resourceFake) { f.putJSON(tKuadrantCR, kuadrantUnknown); dropAuthorino(f, false) },
			want: lead + " Kuadrant kuadrant-system/kuadrant is not Ready yet (Reconciling) and has not made Authorino. Once Kuadrant is Ready, Set up again to continue."},
		{name: "the Kuadrant CR is gone", action: "setup", flip: func(f *resourceFake) {
			dropAuthorino(f, false)
			f.mu.Lock()
			delete(f.objects, fakeKey{gv: "kuadrant.io/v1beta1", ns: "kuadrant-system", plural: "kuadrants", name: "kuadrant"})
			f.mu.Unlock()
		}, want: lead + " There is no Kuadrant CR any more; Set up creates it."},
		{name: "the Kuadrant CR cannot be read", action: "setup", flip: func(f *resourceFake) {
			dropAuthorino(f, false)
			f.mu.Lock()
			f.fail["GET /apis/kuadrant.io/v1beta1/kuadrants"] = http.StatusInternalServerError
			f.mu.Unlock()
		}, want: lead + " The Kuadrant CR cannot be read: k8s API error 500"},
		{name: "Configure Authorino TLS", action: "authorino-tls", step: "check", prefix: "Authorino kuadrant-system/authorino can be configured.",
			flip: func(f *resourceFake) { f.putJSON(tKuadrantCR, kuadrantLimitador); dropAuthorino(f, false) },
			want: restartHint + "openshift-operators). Once Kuadrant is Ready, run it again.", restart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mc := newMaaSCluster(t)
			step, prefix := kube.FirstNonEmpty(tc.step, "authorino-tls"), kube.FirstNonEmpty(tc.prefix, "Configuring Authorino TLS in ")
			if tc.action == "setup" {
				// Authorino TLS is not set up yet, so the step waits.
				mc.f.putJSON("/apis/operator.authorino.kuadrant.io/v1beta1/namespaces/kuadrant-system/authorinos/authorino", `{"spec":{}}`)
			}
			req := types.MaaSRequest{Action: tc.action}
			if tc.action == "setup" {
				req.Database = tCNPG
			}
			res, events := runMaaSWhen(t, mc.c, req, step, prefix, func() { tc.flip(mc.f) })
			if res.Success || !strings.HasSuffix(finalSteps(events), "authorino-tls=failed") || !strings.Contains(res.Message, tc.want) {
				t.Fatalf("%s: %+v (%s)\nwant %q", tc.action, res, finalSteps(events), tc.want)
			}
			for _, banned := range []string{"deadline", "network error", "context"} {
				if strings.Contains(res.Message, banned) {
					t.Errorf("message shows the wait's own end (%q):\n%s", banned, res.Message)
				}
			}
			if got := strings.Contains(res.Message, "rollout restart"); got != tc.restart {
				t.Errorf("restart named %t, want %t:\n%s", got, tc.restart, res.Message)
			}
			noOperatorChange(t, mc.f)
		})
	}
}

// The upgrade of Connectivity Link words the retry as its own.
func TestMaaSUpgradeRHCL_MissingAuthorinoSaysRunTheUpgradeAgain(t *testing.T) {
	mc := newMaaSCluster(t)
	sim := newFakeOLMSim(mc.f)
	sim.installed("rhcl-operator", "kuadrant-system", "kuadrant-operator", "stable", "1.4.3")
	sim.onInstalled = func(pkg, ns, from, to string) {
		if to == "1.4.4" {
			mc.f.objects[fakeKey{gv: "v1", ns: "kuadrant-system", plural: "services", name: "authorino-authorino-authorization"}] = map[string]any{"metadata": map[string]any{"name": "authorino-authorino-authorization", "uid": "svc-2", "resourceVersion": "1"}, "spec": map[string]any{"clusterIP": "None"}}
		}
	}
	mc.f.beforeServe = func(method, path string) { sim.step(method, path) }
	res, events := runMaaSWhen(t, mc.c, types.MaaSRequest{Action: "upgrade-rhcl"}, "authorino-tls", "Configuring Authorino TLS in ", func() {
		mc.f.putJSON(tKuadrantCR, kuadrantLimitador)
		dropAuthorino(mc.f, false)
	})
	want := restartHint + "openshift-operators). Once Kuadrant is Ready, run the upgrade again to continue."
	if res.Success || !strings.HasSuffix(finalSteps(events), "authorino-tls=failed") || !strings.Contains(res.Message, want) {
		t.Fatalf("upgrade: %+v (%s)\nwant %q", res, finalSteps(events), want)
	}
}

// The operation ends (its deadline, or the user) while Kuadrant has not
// made Authorino: the message still says what the wait found and what the
// Kuadrant CR says, read on its own context.
func TestMaaSSetup_AuthorinoWaitCancelledSaysWhy(t *testing.T) {
	mc := newMaaSCluster(t)
	mc.f.putJSON("/apis/operator.authorino.kuadrant.io/v1beta1/namespaces/kuadrant-system/authorinos/authorino", `{"spec":{}}`)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	editTestClient(mc.c, func(o *testClientOptions) { o.Ctx = ctx })
	var waiting sync.Mutex
	reads, armed := 0, false
	mc.f.beforeRequest = func(r *http.Request) {
		waiting.Lock()
		defer waiting.Unlock()
		if armed && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/authorinos/authorino") {
			if reads++; reads == 2 {
				cancel() // after one poll found no Authorino
			}
		}
	}
	res, events := runMaaSWhen(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}, "authorino-tls", "Configuring Authorino TLS in ", func() {
		mc.f.putJSON(tKuadrantCR, kuadrantLimitador)
		dropAuthorino(mc.f, false)
		waiting.Lock()
		armed = true
		waiting.Unlock()
	})
	want := "Stopped waiting for Authorino in kuadrant-system: context canceled; it is not there yet (kuadrant has not made Authorino yet: no Authorino authorino in kuadrant-system). Kuadrant kuadrant-system/kuadrant is not Ready (LimitadorNotReady: limitador resource not in topology)" + restartHint + "openshift-operators). Once Kuadrant is Ready, Set up again to continue."
	if res.Success || res.ErrorCode != "cancelled" || res.Message != want || !strings.HasSuffix(finalSteps(events), "authorino-tls=failed") {
		t.Fatalf("setup: %+v (%s)\nwant %q", res, finalSteps(events), want)
	}
}

// Set up's wait for Kuadrant to be Ready quotes its Ready condition's
// reason and message when it gives up waiting.
func TestMaaSSetup_KuadrantNotReadyQuotesItsCondition(t *testing.T) {
	mc := newMaaSCluster(t)
	mc.f.putJSON("/apis/kuadrant.io/v1beta1/namespaces/kuadrant-system/kuadrants/kuadrant", `{"status":{"conditions":[{"type":"Ready","status":"False","reason":"LimitadorNotReady","message":"limitador resource not in topology"}]}}`)
	_, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG})
	want := "Kuadrant is not Ready after 5:00 (LimitadorNotReady: limitador resource not in topology); continuing."
	if msgs := kuadrantMessages(events); len(msgs) == 0 || msgs[len(msgs)-1] != want {
		t.Fatalf("kuadrant step: %q, want last %q", msgs, want)
	}
}
