package cluster

import (
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
	at := established.UTC().Format(time.RFC3339)
	conds := []any{
		map[string]any{"type": "NamesAccepted", "status": "True", "reason": "NoConflicts", "message": "no conflicts found", "lastTransitionTime": at},
	}
	if status != "" {
		est := map[string]any{"type": "Established", "status": status, "reason": "InitialNamesAccepted", "message": "the initial names have been accepted", "lastTransitionTime": at}
		if status != "True" {
			est["reason"], est["message"] = "Installing", "the initial names have not been accepted yet"
		}
		conds = append(conds, est)
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
	for _, tc := range []struct {
		name     string
		age      time.Duration
		min, max time.Duration
	}{
		// The CRD's time is kept to the second, so it may read up to a
		// second older than it is.
		{"just established", 0, 43 * time.Second, 45 * time.Second},
		{"established 20 seconds ago", 20 * time.Second, 23 * time.Second, 25 * time.Second},
		{"established a day ago", 24 * time.Hour, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mc := maasClusterWithoutKuadrant(t, time.Now().Add(-tc.age), "True")
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

func TestMaaSSetup_KuadrantCRIsNotCreatedWhenItsCRDCannotBeTrusted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string // the CRD's Established status; "" no CRD
		fail   bool   // the CRD read is refused
		want   string
		code   string
	}{
		{"read refused", "True", true, "Cannot read CRD kuadrants.kuadrant.io, so this tool cannot tell whether the API server accepts the new Kuadrant type yet: ", "forbidden"},
		{"no CRD", "", false, "CRD kuadrants.kuadrant.io is not there although Kuadrant's API is served. The Kuadrant CR was not created; Set up again to retry.", "not_ready"},
		{"not established", "False", false, "CRD kuadrants.kuadrant.io is not Established (Established is False: the initial names have not been accepted yet). The Kuadrant CR was not created; Set up again to retry.", "not_ready"},
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
		{"not established", false, "CRD kuadrants.kuadrant.io is not Established (Established is False: the initial names have not been accepted yet). The Kuadrant CR was not created; Set up again to retry."},
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

// Authorino's wait, when Kuadrant has not made Authorino: the message says
// what the polls found, not the wait's own deadline, quotes Kuadrant's
// Ready condition, and names the operator restart only when Kuadrant is
// not Ready and made no Authorino CR (its operator may not try again after
// the API server refused its first try). The tool restarts nothing.
func TestMaaSSetup_AuthorinoMissingSaysWhy(t *testing.T) {
	const limitador = `{"status":{"conditions":[{"type":"Ready","status":"False","reason":"LimitadorNotReady","message":"limitador resource not in topology"}]}}`
	for _, tc := range []struct {
		name     string
		kuadrant string
		keepCR   bool // the Authorino CR and Service are there (the Deployment is not)
		want     []string
		restart  bool
	}{
		{"not ready, no Authorino", limitador, false, []string{
			"Authorino in kuadrant-system is not there after 5:00 (kuadrant has not made Authorino yet: no Authorino authorino in kuadrant-system).",
			" Kuadrant kuadrant-system/kuadrant is not Ready (LimitadorNotReady: limitador resource not in topology) and has not made Authorino; its operator may not retry by itself (this happens when the Kuadrant CR is created right after Connectivity Link is installed).",
			" A cluster-admin can restart it (oc rollout restart deployment/kuadrant-operator-controller-manager -n openshift-operators). Once Kuadrant is Ready, Set up again to continue.",
		}, true},
		{"not ready, Authorino without its Deployment", limitador, true, []string{
			"Authorino in kuadrant-system is not there after 5:00 (kuadrant has not made Authorino yet: no Deployment kuadrant-system/authorino).",
			" Kuadrant kuadrant-system/kuadrant is not Ready (LimitadorNotReady: limitador resource not in topology). Once Kuadrant is Ready, Set up again to continue.",
		}, false},
		{"ready, no Authorino", `{"status":{"conditions":[{"type":"Ready","status":"True","reason":"Ready"}]}}`, false, []string{
			"(kuadrant has not made Authorino yet: no Authorino authorino in kuadrant-system). Kuadrant kuadrant-system/kuadrant is Ready; Set up again to continue.",
		}, false},
		{"no Ready condition, no Authorino", `{"status":{}}`, false, []string{
			" Kuadrant kuadrant-system/kuadrant is not Ready yet (it reports no Ready condition). Once Kuadrant is Ready, Set up again to continue.",
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mc := newMaaSCluster(t)
			mc.f.putJSON("/apis/kuadrant.io/v1beta1/namespaces/kuadrant-system/kuadrants/kuadrant", tc.kuadrant)
			mc.f.mu.Lock()
			for k := range mc.f.objects {
				if k.ns == "kuadrant-system" && (k.plural == "deployments" || (!tc.keepCR && (k.plural == "services" || k.plural == "authorinos"))) {
					delete(mc.f.objects, k)
				}
			}
			mc.f.mu.Unlock()
			res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG})
			if res.Success || res.ErrorCode != "not_ready" || !strings.HasSuffix(finalSteps(events), "authorino-tls=failed") {
				t.Fatalf("setup: %+v (%s)", res, finalSteps(events))
			}
			for _, want := range tc.want {
				if !strings.Contains(res.Message, want) {
					t.Errorf("message lacks %q:\n%s", want, res.Message)
				}
			}
			for _, banned := range []string{"deadline", "network error", "context"} {
				if strings.Contains(res.Message, banned) {
					t.Errorf("message shows the wait's own end (%q):\n%s", banned, res.Message)
				}
			}
			if got := strings.Contains(res.Message, "rollout restart"); got != tc.restart {
				t.Errorf("restart named %t, want %t:\n%s", got, tc.restart, res.Message)
			}
			for _, m := range mc.f.mutations() {
				if strings.Contains(m, serving.KuadrantController) || strings.Contains(m, "/pods") {
					t.Errorf("the tool changed Kuadrant's operator itself: %s", m)
				}
			}
		})
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
