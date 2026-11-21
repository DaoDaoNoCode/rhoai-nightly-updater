package cluster

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

const componentsV1 = "/apis/components.platform.opendatahub.io/v1alpha1/"

// deletingCR is a component CR deleting since the given time, with the
// module operators' finalizer.
func deletingCR(name string, since time.Time) map[string]any {
	return map[string]any{"metadata": map[string]any{
		"name": name, "uid": "uid-" + name, "resourceVersion": "7",
		"finalizers": []string{"platform.opendatahub.io/finalizer"}, "deletionTimestamp": since.UTC().Format(time.RFC3339),
	}}
}

// serveComponentCRs serves every live 3.6 kind, Kueue and NewModule (the
// items given by plural, none otherwise), and accepts patches of each.
func serveComponentCRs(f *fakeAPI, items map[string][]map[string]any) {
	plurals := []string{"kueues", "newmodules"}
	for _, o := range live36Operators {
		plurals = append(plurals, o.Plural)
	}
	for _, p := range plurals {
		list := items[p]
		if list == nil {
			list = []map[string]any{}
		}
		f.obj(componentsV1+p, map[string]any{"items": list})
		for _, it := range list {
			f.json("PATCH", componentsV1+p+"/"+it["metadata"].(map[string]any)["name"].(string), http.StatusOK, `{}`)
		}
	}
}

// crName is the name the platform gives a module's CR.
func crName(module string) string {
	if module == "mcplifecycleoperator" {
		return "default"
	}
	return "default-" + module
}

func cleanupWarnings(t *testing.T, warnings []string, want ...string) {
	t.Helper()
	text := strings.Join(warnings, "\n")
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("warnings missing %q:\n%s", w, text)
		}
	}
}

// On the live 3.6 data every module operator runs, so no finalizer is
// touched: each stuck CR names its discovered operator, Kueue (the RHOAI
// operator's own) and a kind no RBAC names are unknown, and a list error
// skips only its kind. The RBAC is read once for the whole scan.
func TestCleanupStuckComponentCRsLive36LeavesRunningOperatorsAlone(t *testing.T) {
	f, c := newFakeAPI(t)
	serveLive36Kinds(f)
	newLive36ModuleOperatorWorld().serve(f)
	old := time.Now().Add(-time.Hour)
	items := map[string][]map[string]any{
		"kueues":     {deletingCR("default-kueue", old)},
		"newmodules": {deletingCR("default", old)},
	}
	for _, o := range live36Operators {
		items[o.Plural] = []map[string]any{deletingCR(crName(o.Module), old)}
	}
	serveComponentCRs(f, items)
	f.status("GET", componentsV1+"rays", http.StatusInternalServerError, "InternalError")

	n, warnings := cleanupStuckComponentCRs(c)
	if n != 0 {
		t.Fatalf("unstuck %d", n)
	}
	assertWrites(t, f)
	want := []string{
		"kueues/default-kueue has been deleting since", "is unknown (no ServiceAccount in redhat-ods-applications may update kueues and kueues/finalizers or kueues/status)",
		"newmodules/default has been deleting since", "list rays:",
	}
	for _, o := range live36Operators {
		if o.Module == "ray" {
			continue
		}
		want = append(want, fmt.Sprintf("%s/%s has been deleting since %s, but its operator redhat-ods-applications/%s still exists and will finish the deletion; left untouched",
			o.Plural, crName(o.Module), old.UTC().Format(time.RFC3339), o.Deployment))
	}
	cleanupWarnings(t, warnings, want...)
	for _, p := range []string{"/apis/rbac.authorization.k8s.io/v1/clusterroles", "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings", "/apis/apps/v1/namespaces/" + liveAppsNS + "/deployments"} {
		if got := len(f.requests("GET", p)); got != 1 {
			t.Errorf("%s read %d times", p, got)
		}
	}
}

// With its operator's Deployment gone (its ClusterRoleBinding stays until
// the CR is gone), a stuck CR's finalizers are removed under the inspected
// uid and resourceVersion, including aigateway and mcplifecycleoperator,
// which the former fixed list never unblocked. CRs whose operator runs are
// left alone.
func TestCleanupStuckComponentCRsOperatorGone(t *testing.T) {
	f, c := newFakeAPI(t)
	serveLive36Kinds(f)
	w := newLive36ModuleOperatorWorld()
	gone := []string{"aigateway", "mcplifecycleoperator", "dashboard", "kserve"}
	for _, m := range gone {
		w.dropDeployment(liveOperator(m).Deployment)
	}
	w.serve(f)
	old := time.Now().Add(-time.Hour)
	items := map[string][]map[string]any{}
	for _, o := range live36Operators {
		items[o.Plural] = []map[string]any{deletingCR(crName(o.Module), old)}
	}
	serveComponentCRs(f, items)

	n, warnings := cleanupStuckComponentCRs(c)
	var want []string
	for _, m := range gone {
		o := liveOperator(m)
		path := componentsV1 + o.Plural + "/" + crName(m)
		want = append(want, "PATCH "+path)
		reqs := f.requests("PATCH", path)
		if len(reqs) != 1 || reqs[0].Body != fmt.Sprintf(`{"metadata":{"finalizers":[],"resourceVersion":"7","uid":"uid-%s"}}`, crName(m)) {
			t.Errorf("%s: the finalizer patch must carry the inspected uid and resourceVersion: %+v", m, reqs)
		}
	}
	assertWrites(t, f, want...)
	if n != len(gone) {
		t.Fatalf("unstuck %d, warnings %v", n, warnings)
	}
	cleanupWarnings(t, warnings, "trustyais/default-trustyai has been deleting since", "its operator redhat-ods-applications/trustyai-operator-module-controller-manager still exists")
	if strings.Contains(strings.Join(warnings, "\n"), "aigateways/") {
		t.Errorf("aigateway was unblocked, yet warned: %v", warnings)
	}
}

// Every case in which the operator may still remove the finalizers, or
// cannot be identified, leaves the CR alone; the resolved cases next to
// them show what does let the cleanup through.
func TestCleanupStuckComponentCRsRefusals(t *testing.T) {
	trusty := liveOperator("trustyai")
	old := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name, module, cr, want string
		since                  time.Time
		patch                  bool
		edit                   func(w *moduleOperatorWorld)
		api                    func(f *fakeAPI)
	}{
		{name: "deleting for less than StuckComponentCRAge", module: "trustyai", since: time.Now(),
			want: "trustyais/default-trustyai is being deleted; its finalizers were left for its operator",
			edit: func(w *moduleOperatorWorld) { w.dropDeployment(trusty.Deployment) }},
		{name: "operator scaled to zero still exists", module: "trustyai",
			want: "its operator redhat-ods-applications/trustyai-operator-module-controller-manager still exists",
			edit: func(w *moduleOperatorWorld) {
				for _, d := range w.deployments {
					if d["metadata"].(map[string]any)["name"] == trusty.Deployment {
						d["spec"].(map[string]any)["replicas"] = 0
						d["status"] = map[string]any{}
					}
				}
			}},
		{name: "a Deployment the Platform does not own runs as the operator's ServiceAccount", module: "trustyai",
			want: "its operator redhat-ods-applications/hand-made still exists",
			edit: func(w *moduleOperatorWorld) {
				w.dropDeployment(trusty.Deployment)
				w.deployments = append(w.deployments, platformDeployment("hand-made", trusty.ServiceAccount, "", "", ""))
			}},
		{name: "another ServiceAccount that may update the CR and its status runs", module: "trustyai",
			want: "its operator redhat-ods-applications/status-writer still exists",
			edit: func(w *moduleOperatorWorld) {
				w.dropDeployment(trusty.Deployment)
				w.roles = append(w.roles, map[string]any{"metadata": map[string]any{"name": "status-writer"}, "rules": []any{
					rule(componentsGroup, []string{"trustyais"}, []string{"patch"}), rule(componentsGroup, []string{"trustyais/status"}, []string{"update"})}})
				w.bindings = append(w.bindings, crb("status-writer", "status-writer", saSubject(liveAppsNS, "status-writer")))
				w.deployments = append(w.deployments, platformDeployment("status-writer", "status-writer", "", "", ""))
			}},
		{name: "Kueue is the RHOAI operator's own", module: "kueue", cr: "default-kueue",
			want: "kueues/default-kueue has been deleting since", edit: func(*moduleOperatorWorld) {}},
		{name: "a kind no RBAC names", module: "newmodule", cr: "default",
			want: "is unknown (no ServiceAccount in redhat-ods-applications may update newmodules", edit: func(*moduleOperatorWorld) {}},
		{name: "a group binding identifies nothing", module: "trustyai",
			want: "is unknown (no ServiceAccount in redhat-ods-applications may update trustyais",
			edit: func(w *moduleOperatorWorld) {
				w.dropDeployment(trusty.Deployment)
				w.bindings = slices.DeleteFunc(w.bindings, func(b map[string]any) bool { return b["metadata"].(map[string]any)["name"] == trusty.Role+"binding" })
				w.bindings = append(w.bindings, crb("all-sas", trusty.Role, map[string]any{"kind": "Group", "name": "system:serviceaccounts"}))
			}},
		{name: "operator RBAC gone with the Deployment", module: "trustyai",
			want: "is unknown (no ServiceAccount in redhat-ods-applications may update trustyais",
			edit: func(w *moduleOperatorWorld) {
				w.dropDeployment(trusty.Deployment)
				w.bindings = slices.DeleteFunc(w.bindings, func(b map[string]any) bool { return b["metadata"].(map[string]any)["name"] == trusty.Role+"binding" })
			}},
		{name: "a kserve CR its operator's rules do not name", module: "kserve", cr: "other-kserve",
			want: "is unknown (no ServiceAccount in redhat-ods-applications may update kserves",
			edit: func(w *moduleOperatorWorld) { w.dropDeployment(liveOperator("kserve").Deployment) }},
		{name: "ClusterRoles forbidden", module: "trustyai",
			want: "cannot identify the operator that owns the finalizers of trustyais/default-trustyai: list ClusterRoles",
			edit: func(w *moduleOperatorWorld) { w.dropDeployment(trusty.Deployment) },
			api: func(f *fakeAPI) {
				f.status("GET", "/apis/rbac.authorization.k8s.io/v1/clusterroles", http.StatusForbidden, "Forbidden")
			}},
		{name: "ClusterRoleBindings fail", module: "trustyai", want: "list ClusterRoleBindings",
			edit: func(w *moduleOperatorWorld) { w.dropDeployment(trusty.Deployment) },
			api: func(f *fakeAPI) {
				f.status("GET", "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings", http.StatusInternalServerError, "InternalError")
			}},
		{name: "Deployments forbidden", module: "trustyai", want: "list Deployments in redhat-ods-applications",
			edit: func(w *moduleOperatorWorld) { w.dropDeployment(trusty.Deployment) },
			api: func(f *fakeAPI) {
				f.status("GET", "/apis/apps/v1/namespaces/"+liveAppsNS+"/deployments", http.StatusForbidden, "Forbidden")
			}},
		{name: "no DSCInitialization", module: "trustyai", want: "no DSCInitialization exists",
			edit: func(w *moduleOperatorWorld) { w.dropDeployment(trusty.Deployment) },
			api:  func(f *fakeAPI) { f.json("GET", dsciV2List, 200, `{"items":[]}`) }},
		// Resolved cases next to them.
		{name: "two ServiceAccounts may be the operator and neither runs", module: "trustyai", patch: true,
			edit: func(w *moduleOperatorWorld) {
				w.dropDeployment(trusty.Deployment)
				w.bindings = append(w.bindings, crb("copy", trusty.Role, saSubject(liveAppsNS, "trustyai-copy")))
			}},
		{name: "a cluster-admin Deployment is not the operator", module: "trustyai", patch: true,
			edit: func(w *moduleOperatorWorld) {
				w.dropDeployment(trusty.Deployment)
				w.bindings = append(w.bindings, crb("wild", "cluster-admin", saSubject(liveAppsNS, "wild")))
				w.deployments = append(w.deployments, platformDeployment("wild-operator", "wild", "config.opendatahub.io/v1alpha1", "Platform", "default"))
			}},
		{name: "the status grant identifies it when no ServiceAccount may update finalizers", module: "trainer", patch: true,
			edit: func(w *moduleOperatorWorld) {
				o := liveOperator("trainer")
				w.dropDeployment(o.Deployment)
				for _, r := range w.roles {
					if r["metadata"].(map[string]any)["name"] == o.Role {
						r["rules"] = []any{rule(componentsGroup, []string{"trainers"}, []string{"update"}), rule(componentsGroup, []string{"trainers/status"}, []string{"update"})}
					}
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFakeAPI(t)
			serveLive36Kinds(f)
			w := newLive36ModuleOperatorWorld()
			tc.edit(w)
			w.serve(f)
			if tc.api != nil {
				tc.api(f)
			}
			name := kube.FirstNonEmpty(tc.cr, crName(tc.module))
			since := old
			if !tc.since.IsZero() {
				since = tc.since
			}
			plural := tc.module + "s"
			if tc.module != "kueue" && tc.module != "newmodule" {
				plural = liveOperator(tc.module).Plural
			}
			serveComponentCRs(f, map[string][]map[string]any{plural: {deletingCR(name, since)}})

			n, warnings := cleanupStuckComponentCRs(c)
			if tc.patch {
				assertWrites(t, f, "PATCH "+componentsV1+plural+"/"+name)
				if n != 1 || len(warnings) != 0 {
					t.Fatalf("unstuck %d, warnings %v", n, warnings)
				}
				return
			}
			assertWrites(t, f)
			if n != 0 {
				t.Fatalf("unstuck %d", n)
			}
			cleanupWarnings(t, warnings, tc.want, "left")
		})
	}
}

// A CR that is not stuck long enough reads no RBAC at all.
func TestCleanupStuckComponentCRsRecentReadsNoRBAC(t *testing.T) {
	f, c := newFakeAPI(t)
	serveLive36Kinds(f)
	serveLive36ModuleOperators(f)
	serveComponentCRs(f, map[string][]map[string]any{"trustyais": {deletingCR("default-trustyai", time.Now())}})
	if n, _ := cleanupStuckComponentCRs(c); n != 0 {
		t.Fatalf("unstuck %d", n)
	}
	for _, p := range []string{"/apis/rbac.authorization.k8s.io/v1/clusterroles", "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings", "/apis/apps/v1/namespaces/" + liveAppsNS + "/deployments", dsciV2List} {
		if got := len(f.requests("GET", p)); got != 0 {
			t.Errorf("%s read %d times", p, got)
		}
	}
}

func TestCleanupStuckComponentCRs_NoComponentAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = fmt.Fprint(w, `{"kind":"Status","code":404}`)
	}))
	defer srv.Close()
	c := newTestClient(testClientOptions{BaseURL: srv.URL, HTTPClient: srv.Client(), Ctx: context.Background()})
	if n, warnings := cleanupStuckComponentCRs(c); n != 0 || len(warnings) != 0 {
		t.Fatalf("fresh cluster: unstuck=%d warnings=%v", n, warnings)
	}
}

// R1-3: between the list and the patch, the CR can finish deleting and be
// recreated under the same name, or its finalizers can change. The patch is
// guarded by the inspected resourceVersion and uid, and the API server's
// 409 Conflict means "leave it alone", never a retry without the guard.
func TestCleanupStuckComponentCRs_ReplacedCRIsLeftAlone(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	o := liveOperator("aipipelines")
	noGuard := deletingCR("default-aipipelines", old)
	delete(noGuard["metadata"].(map[string]any), "resourceVersion")
	for _, tt := range []struct {
		name   string
		status int
		item   map[string]any
		want   string
		calls  int
	}{
		{"recreated or changed (409)", http.StatusConflict, deletingCR("default-aipipelines", old), "changed or was replaced", 1},
		{"gone (404)", http.StatusNotFound, deletingCR("default-aipipelines", old), "changed or was replaced", 1},
		{"invalid (422)", http.StatusUnprocessableEntity, deletingCR("default-aipipelines", old), "changed or was replaced", 1},
		{"other failure (500)", http.StatusInternalServerError, deletingCR("default-aipipelines", old), "remove finalizers from aipipelines/default-aipipelines", 1},
		{"no resourceVersion", http.StatusOK, noGuard, "no uid or resourceVersion", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, c := newFakeAPI(t)
			serveLive36Kinds(f)
			w := newLive36ModuleOperatorWorld()
			w.dropDeployment(o.Deployment)
			w.serve(f)
			serveComponentCRs(f, map[string][]map[string]any{o.Plural: {tt.item}})
			f.status("PATCH", componentsV1+o.Plural+"/default-aipipelines", tt.status, "Status")
			n, warnings := cleanupStuckComponentCRs(c)
			if patches := len(f.requests("PATCH", componentsV1+o.Plural+"/default-aipipelines")); n != 0 || patches != tt.calls || !strings.Contains(strings.Join(warnings, "\n"), tt.want) {
				t.Fatalf("unstuck=%d patches=%d warnings=%v", n, patches, warnings)
			}
		})
	}
}
