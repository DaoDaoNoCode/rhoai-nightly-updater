package cluster

import (
	"net/http"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	liveDSCUID   = "ba54ddbf-ad4a-4257-9ab4-977eb541dc1c"
	kueueCRPath  = "/apis/components.platform.opendatahub.io/v1alpha1/kueues/default-kueue"
	platformPath = "/apis/config.opendatahub.io/v1alpha1/platforms/default"
)

// teardownWorld is managerWorld with the Platform default and a Kueue CR
// owned by the DSC. The components group (serving Kueue) is served once
// serveModules is called: the plan's removal checks of every module run
// before the delete and are tested elsewhere, so the group appears only
// after the delete here.
func teardownWorld(t *testing.T) (*resourceFake, *Client) {
	t.Helper()
	f, c := managerWorld(t, nil)
	f.put(kueueCRPath, map[string]any{"metadata": map[string]any{"name": "default-kueue", "finalizers": []any{"platform.opendatahub.io/finalizer"},
		"ownerReferences": []any{map[string]any{"apiVersion": "datasciencecluster.opendatahub.io/v2", "kind": "DataScienceCluster", "name": "default-dsc", "uid": liveDSCUID, "blockOwnerDeletion": true, "controller": true}}}})
	setPlatform(f, 3, 3, "True", "", "")
	return f, c
}

// serveModules serves the components group with the Kueue kind.
func serveModules(f *resourceFake) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups = map[string]string{
		"/apis/datasciencecluster.opendatahub.io":           dscGroupDoc,
		"/apis/" + componentAPIGroup:                        `{"kind":"APIGroup","name":"components.platform.opendatahub.io","preferredVersion":{"groupVersion":"components.platform.opendatahub.io/v1alpha1","version":"v1alpha1"}}`,
		"/apis/components.platform.opendatahub.io/v1alpha1": `{"resources":[{"name":"kueues","kind":"Kueue","verbs":["list","get","patch"]}]}`,
	}
}

// serveModulesOnceDeleted serves the module group after the real delete
// of the DSC.
func serveModulesOnceDeleted(f *resourceFake) {
	f.beforeRequest = func(r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Query().Get("dryRun") == "" {
			serveModules(f)
		}
	}
}

// setPlatform stores the Platform default at generation gen whose
// ProvisioningSucceeded observed observed, with ModulesReady status,
// reason and message ("" status: no ModulesReady).
func setPlatform(f *resourceFake, gen, observed int, status, reason, msg string) {
	conds := []any{map[string]any{"type": "ProvisioningSucceeded", "status": "True", "observedGeneration": observed}}
	if status != "" {
		conds = append(conds, map[string]any{"type": "ModulesReady", "status": status, "reason": reason, "message": msg})
	}
	f.put(platformPath, map[string]any{"metadata": map[string]any{"name": "default", "generation": gen}, "status": map[string]any{"conditions": conds}})
}

// The DSC goes at once (its finalizer only marks the modules Removed); the
// reset then waits for the Kueue CR it owned and for the Platform to report
// nothing pending, and creates the new DSC only after that.
func TestHardResetWaitsForTheModuleTeardown(t *testing.T) {
	f, c := teardownWorld(t)
	setPlatform(f, 4, 3, "True", "Removed", "Modules pending deletion: kueue")
	operatorRemovesDSCAfter(f, 1)
	serveModulesOnceDeleted(f)
	polls := 0
	prev := f.beforeServe
	f.beforeServe = func(method, path string) {
		prev(method, path)
		if method != http.MethodGet || path != dscV3Path {
			return
		}
		f.mu.Lock()
		k, _ := parseFakePath(dscV3Path)
		dscGone := f.objects[k] == nil
		f.mu.Unlock()
		if !dscGone {
			return
		}
		switch polls++; polls {
		case 2:
			f.mu.Lock()
			kk, _ := parseFakePath(kueueCRPath)
			delete(f.objects, kk)
			f.mu.Unlock()
		case 4:
			setPlatform(f, 4, 4, "True", "NoManagedModules", "All registered modules have ManagementState Removed or are not configured")
		}
	}
	res, steps := runHardReset(t, c, nil)
	if !res.Success {
		t.Fatalf("result = %+v", res)
	}
	text := stepsText(steps)
	for _, want := range []string{"DataScienceCluster default-dsc is gone", "Waiting for the module CRs the DataScienceCluster owned to be deleted: Kueue default-kueue",
		"Waiting for the Platform to report the modules removed: Platform default has not reported a reconcile of its generation 4 yet",
		"is gone and the platform has cleaned up its modules"} {
		if !strings.Contains(text, want) {
			t.Errorf("steps lack %q:\n%s", want, text)
		}
	}
	posts := f.callsTo(http.MethodPost, dscV3List)
	if len(posts) != 2 || polls < 4 {
		t.Fatalf("created %d times after %d polls", len(posts), polls)
	}
	for _, d := range f.callsTo(http.MethodDelete, dscV3Path) {
		if !strings.Contains(d.Body, `"propagationPolicy":"Background"`) {
			t.Fatalf("delete %s", d.Body)
		}
	}
}

// A module CR that stays: the reset stops, creates nothing, names it and
// says what to check; Finish refuses while it is there.
func TestHardResetTimesOutOnAModuleCRAndFinishWaits(t *testing.T) {
	f, c := teardownWorld(t)
	operatorRemovesDSCAfter(f, 1)
	serveModulesOnceDeleted(f)
	obj := f.get(kueueCRPath)
	ensureMap(obj, "metadata")["deletionTimestamp"] = "2026-11-19T00:00:00Z"
	f.put(kueueCRPath, obj)
	res, _ := runHardReset(t, c, nil)
	if res.Success || res.ErrorCode != "timeout" || !strings.Contains(res.Message, "DataScienceCluster default-dsc is gone, but after 10m0s these module CRs it owned are still there: Kueue default-kueue (deleting since 2026-11-19T00:00:00Z, finalizers platform.opendatahub.io/finalizer)") ||
		!strings.Contains(res.Message, "Diagnostics (Platform modules)") || !strings.Contains(res.Message, "Finish the hard reset") {
		t.Fatalf("result = %+v", res)
	}
	if posts := f.callsTo(http.MethodPost, dscV3List); len(posts) != 0 {
		t.Fatalf("created: %+v", posts)
	}
	pending, err := pendingHardReset(c)
	if err != nil || pending == nil {
		t.Fatalf("pending %+v %v", pending, err)
	}
	f.beforeServe = nil
	f.put("/apis/dscinitialization.opendatahub.io/v2/dscinitializations/default-dsci", map[string]any{
		"metadata": map[string]any{"name": "default-dsci"}, "spec": map[string]any{"applicationsNamespace": "redhat-ods-applications"},
		"status": map[string]any{"release": map[string]any{"name": selfManagedPlatform}}})
	fin, _ := FinishDSCHardReset(c, types.DSCFinishRequest{Name: pending.Name, Token: pending.Token})
	if fin.Success || fin.ErrorCode != "prerequisites" || !strings.Contains(fin.Message, "Kueue default-kueue") {
		t.Fatalf("finish = %+v", fin)
	}
	kk, _ := parseFakePath(kueueCRPath)
	delete(f.objects, kk)
	if fin, _ := FinishDSCHardReset(c, types.DSCFinishRequest{Name: pending.Name, Token: pending.Token}); !fin.Success {
		t.Fatalf("finish after the teardown = %+v", fin)
	}
}

// A module CR the DSC owns whose operator the tool cannot identify (the CSV
// does not own the Kueue CRD here and no ServiceAccount may update it):
// refused before anything is deleted.
func TestHardResetRefusesAModuleCRWithAnUnknownOperator(t *testing.T) {
	f, c := teardownWorld(t)
	serveModules(f)
	ops, refusal, err := hardResetOperators(c, liveDSCUID)
	if err != nil || len(ops) != 0 || !strings.Contains(refusal, "Kueue default-kueue: the tool cannot identify its module operator") {
		t.Fatalf("ops %+v, refusal %q, err %v", ops, refusal, err)
	}
	// The installed CSV owns the Kueue CRD: the RHOAI operator reconciles
	// it, and there is no module operator to wait for.
	csv := f.get(namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "rhods-operator.3.6.0"))
	ensureMap(csv, "spec")["customresourcedefinitions"] = map[string]any{"owned": []any{map[string]any{"name": "kueues.components.platform.opendatahub.io"}}}
	f.put(namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "rhods-operator.3.6.0"), csv)
	if ops, refusal, err := hardResetOperators(c, liveDSCUID); err != nil || len(ops) != 0 || refusal != "" {
		t.Fatalf("ops %+v, refusal %q, err %v", ops, refusal, err)
	}
}

// A4: a namespace whose owner chain reaches an *.opendatahub.io object
// refuses the hard reset; so does an owner the tool cannot read. Owners
// that lead elsewhere, or are gone, do not.
func TestHardResetRefusesNamespacesOwnedThroughRHOAI(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(f *resourceFake)
		want string
	}{
		{"owned by the DSC", func(f *resourceFake) {
			f.put("/api/v1/namespaces/my-notebooks", map[string]any{"metadata": map[string]any{"name": "my-notebooks", "ownerReferences": []any{
				map[string]any{"apiVersion": "datasciencecluster.opendatahub.io/v2", "kind": "DataScienceCluster", "name": "default-dsc", "uid": liveDSCUID}}}})
		}, "namespace my-notebooks: owned through DataScienceCluster default-dsc (datasciencecluster.opendatahub.io/v2)"},
		{"owned by a ClusterRole the Platform owns", func(f *resourceFake) {
			f.put("/api/v1/namespaces/maas", map[string]any{"metadata": map[string]any{"name": "maas", "ownerReferences": []any{
				map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "name": "r", "uid": "role-uid"}}}})
			f.groups["/apis/rbac.authorization.k8s.io/v1"] = `{"resources":[{"name":"clusterroles","kind":"ClusterRole","namespaced":false}]}`
			f.put("/apis/rbac.authorization.k8s.io/v1/clusterroles/r", map[string]any{"metadata": map[string]any{"name": "r", "uid": "role-uid", "ownerReferences": []any{
				map[string]any{"apiVersion": "config.opendatahub.io/v1alpha1", "kind": "Platform", "name": "default", "uid": "p"}}}})
		}, "namespace maas: ClusterRole r (rbac.authorization.k8s.io/v1) is owned through Platform default (config.opendatahub.io/v1alpha1)"},
		{"owner unreadable", func(f *resourceFake) {
			f.put("/api/v1/namespaces/x", map[string]any{"metadata": map[string]any{"name": "x", "ownerReferences": []any{
				map[string]any{"apiVersion": "example.com/v1", "kind": "Thing", "name": "t", "uid": "t"}}}})
		}, "namespace x: the tool cannot tell whether its owner Thing t (example.com/v1) leads to the DataScienceCluster"},
		{"namespaces unreadable", func(f *resourceFake) { f.fail["GET /api/v1/namespaces"] = http.StatusForbidden },
			"the namespaces could not be listed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := managerWorld(t, nil)
			tc.edit(f)
			plan, err := PlanDSCChange(c, "hard-reset", "", "", false, false)
			if err != nil || !strings.Contains(plan.Refusal, tc.want) || !strings.Contains(plan.Refusal, "Nothing was changed") {
				t.Fatalf("refusal %q, err %v", plan.Refusal, err)
			}
			if d := f.callsTo(http.MethodDelete, dscV3Path); len(d) != 0 {
				t.Fatalf("deletes %+v", d)
			}
		})
	}
	// A gone owner, and one that leads elsewhere, do not refuse.
	f, c := managerWorld(t, nil)
	f.put("/api/v1/namespaces/a", map[string]any{"metadata": map[string]any{"name": "a", "ownerReferences": []any{
		map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "name": "gone", "uid": "g"},
		map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "name": "plain", "uid": "pl"}}}})
	f.groups["/apis/rbac.authorization.k8s.io/v1"] = `{"resources":[{"name":"clusterroles","kind":"ClusterRole","namespaced":false}]}`
	f.put("/apis/rbac.authorization.k8s.io/v1/clusterroles/plain", map[string]any{"metadata": map[string]any{"name": "plain", "uid": "pl"}})
	if why := namespaceOwnerBlocker(c); why != "" {
		t.Fatalf("refused: %s", why)
	}
}

// What the Platform default says decides (d), from the semantics of
// writeAggregateCondition (rhods-operator rhoai-3.6
// modules_controller_actions.go:763-806).
func TestPlatformModulesSettled(t *testing.T) {
	for _, tc := range []struct {
		name                string
		gen, observed       int
		status, reason, msg string
		settled             bool
		want                string
	}{
		{"nothing managed", 4, 4, "True", "NoManagedModules", "All registered modules have ManagementState Removed or are not configured", true, "no module pending deletion"},
		{"monitoring ready", 4, 4, "True", "", "", true, "no module pending deletion"},
		{"monitoring not ready, nothing pending", 4, 4, "False", "NotReady", "Some modules are not ready: monitoring", true, ""},
		{"pending (True, Removed)", 4, 4, "True", "Removed", "Modules pending deletion: kserve", false, "Modules pending deletion: kserve"},
		{"pending behind another error", 4, 4, "False", "NotReady", "Some modules are not ready: monitoring; pending deletion: ray", false, "pending deletion: ray"},
		{"stale generation", 5, 4, "True", "NoManagedModules", "", false, "generation 5"},
		{"reconcile stopped early", 4, 4, "False", "ConditionNotSet", "condition ModulesReady was not set during reconciliation", false, "stopped before it checked the modules"},
		{"no ModulesReady", 4, 4, "", "", "", false, "no ModulesReady"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newResourceFake(t)
			setPlatform(f, tc.gen, tc.observed, tc.status, tc.reason, tc.msg)
			settled, why, err := platformModulesSettled(c)
			if err != nil || settled != tc.settled || !strings.Contains(why, tc.want) {
				t.Fatalf("settled %v, %q, %v", settled, why, err)
			}
		})
	}
	f, c := newResourceFake(t)
	if settled, why, err := platformModulesSettled(c); !settled || err != nil || why != "no Platform default exists" {
		t.Fatalf("no Platform: %v %q %v", settled, why, err)
	}
	setPlatform(f, 1, 1, "True", "", "")
	p := f.get(platformPath)
	ensureMap(p, "metadata")["deletionTimestamp"] = "2026-11-19T00:00:00Z"
	f.put(platformPath, p)
	if settled, why, _ := platformModulesSettled(c); settled || !strings.Contains(why, "being deleted") {
		t.Fatalf("deleting Platform: %v %q", settled, why)
	}
}

// Phase 2: a recorded module operator Deployment that still exists with
// its UID is waited for; one deleted, or created again (another UID), is
// not.
func TestOperatorsLeft(t *testing.T) {
	f, c := newResourceFake(t)
	f.put("/apis/apps/v1/namespaces/redhat-ods-applications/deployments/kept", map[string]any{"metadata": map[string]any{"name": "kept", "uid": "u1"}})
	f.put("/apis/apps/v1/namespaces/redhat-ods-applications/deployments/again", map[string]any{"metadata": map[string]any{"name": "again", "uid": "new"}})
	left, err := operatorsLeft(c, []hardResetOperator{
		{Namespace: "redhat-ods-applications", Name: "kept", UID: "u1"},
		{Namespace: "redhat-ods-applications", Name: "again", UID: "old"},
		{Namespace: "redhat-ods-applications", Name: "gone", UID: "u3"},
	})
	if err != nil || len(left) != 1 || left[0].Name != "kept" {
		t.Fatalf("left %+v %v", left, err)
	}
	st := teardownState{Operators: left}
	if !strings.Contains(st.stuck(), "Deployment redhat-ods-applications/kept") || !strings.Contains(st.progress(), "module operators") {
		t.Fatalf("%q / %q", st.stuck(), st.progress())
	}
	if o, ok := parseHardResetOperator(hardResetOperator{Namespace: "n", Name: "d", UID: "u"}.record()); !ok || o.UID != "u" {
		t.Fatalf("record round trip %+v", o)
	}
}
