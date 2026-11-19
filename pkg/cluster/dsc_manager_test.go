package cluster

import (
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The fixtures are real objects from a RHOAI 3.6 nightly cluster
// (2026-11-18): the DataScienceCluster CRD (descriptions stripped; v2
// served and deprecated, v3 stored, conversion webhook), the DSC
// default-dsc with its status, and the CSV's alm-examples.
const (
	dscCRDFixture  = "testdata/dsc/crd-3.6.json"
	dscLiveFixture = "testdata/dsc/dsc-3.6.json"
	dscALMFixture  = "testdata/dsc/alm-3.6.json"
	dscV3List      = "/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters"
	dscV3Path      = dscV3List + "/default-dsc"
	rhodsOperator  = "/apis/apps/v1/namespaces/redhat-ods-operator/deployments/rhods-operator"
)

func readFixture(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

// managerWorld is a resourceFake holding the 3.6 fixtures, the installed CSV
// and Subscription, and a ready rhods-operator. edit changes the live DSC
// before it is stored.
func managerWorld(t *testing.T, edit func(dsc map[string]interface{})) (*resourceFake, *Client) {
	t.Helper()
	f, c := newResourceFake(t)
	f.groups = map[string]string{"/apis/datasciencecluster.opendatahub.io": dscGroupDoc}
	f.put(dscCRDPath, readFixture(t, dscCRDFixture))
	dsc := readFixture(t, dscLiveFixture)
	if edit != nil {
		edit(dsc)
	}
	f.put(dscV3Path, dsc)
	alm, err := os.ReadFile(dscALMFixture)
	if err != nil {
		t.Fatal(err)
	}
	f.put(namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName), map[string]interface{}{"status": map[string]interface{}{"installedCSV": "rhods-operator.3.6.0"}})
	f.put(namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "rhods-operator.3.6.0"), map[string]interface{}{
		"metadata": map[string]interface{}{"annotations": map[string]interface{}{"alm-examples": string(alm)}},
		"spec":     map[string]interface{}{"version": "3.6.0"},
		"status":   map[string]interface{}{"phase": "Succeeded"},
	})
	f.put(rhodsOperator, map[string]interface{}{"status": map[string]interface{}{"readyReplicas": 1}})
	return f, c
}

// comps returns spec.components of a stored DSC.
func comps(obj map[string]interface{}) map[string]interface{} {
	spec, _ := obj["spec"].(map[string]interface{})
	c, _ := spec["components"].(map[string]interface{})
	return c
}

func setPath(t *testing.T, obj map[string]interface{}, path string, value interface{}) {
	t.Helper()
	keys := strings.Split(path, ".")
	m := obj
	for _, k := range keys[:len(keys)-1] {
		next, ok := m[k].(map[string]interface{})
		if !ok {
			next = map[string]interface{}{}
			m[k] = next
		}
		m = next
	}
	if value == nil {
		delete(m, keys[len(keys)-1])
		return
	}
	m[keys[len(keys)-1]] = value
}

// patchesTo returns the JSON patches sent to path, dry runs separately.
func patchesTo(f *resourceFake, path string) (dry, real []fakeCall) {
	for _, c := range f.callsTo(http.MethodPatch, path) {
		if c.Query.Get("dryRun") == "All" {
			dry = append(dry, c)
		} else {
			real = append(real, c)
		}
	}
	return dry, real
}

func decodeOps(t *testing.T, body string) []map[string]interface{} {
	t.Helper()
	var ops []map[string]interface{}
	if err := json.Unmarshal([]byte(body), &ops); err != nil {
		t.Fatalf("not a JSON patch: %s", body)
	}
	return ops
}

func TestDSCToggleTreeFromTheInstalledCRD(t *testing.T) {
	f, c := newResourceFake(t)
	f.put(dscCRDPath, readFixture(t, dscCRDFixture))
	crd, err := readDSCCRD(c)
	if err != nil {
		t.Fatal(err)
	}
	if crd.storage() != "v3" || crd.version("v2") == nil || !crd.version("v2").Deprecated {
		t.Fatalf("versions: %+v", crd.Versions)
	}
	tree := dscToggleTree(crd.version("v3").Spec)
	byKey := map[string]dscToggle{}
	var order []string
	for _, n := range tree {
		byKey[n.key()] = n
		order = append(order, n.key())
	}
	for key, want := range map[string]struct {
		toggle  bool
		allowed []string
		def     string
	}{
		"dashboard":                  {false, nil, ""},
		"dashboard.standard":         {true, []string{"Managed", "Removed"}, ""},
		"dashboard.maasPortal":       {true, []string{"Managed", "Removed"}, ""},
		"data.featureStore":          {true, []string{"Managed", "Removed"}, ""},
		"kserve":                     {true, []string{"Managed", "Removed"}, ""},
		"kserve.nim":                 {true, []string{"Managed", "Removed"}, "Managed"},
		"kserve.modelExpress":        {true, []string{"Managed", "Removed"}, "Removed"},
		"aigateway.modelsAsAService": {true, []string{"Managed", "Removed"}, "Removed"},
		"kueue":                      {true, []string{"Managed", "Unmanaged", "Removed"}, ""},
		"workbenches.workbenchesV2":  {true, []string{"Managed", "Removed"}, "Removed"},
	} {
		n, ok := byKey[key]
		if !ok || n.Toggle != want.toggle || !reflect.DeepEqual(n.Allowed, want.allowed) || n.SchemaDefault != want.def {
			t.Errorf("%s = %+v (present %v), want %+v", key, n, ok, want)
		}
	}
	// Only objects that are or hold a toggle; v2-only components are not v3's.
	for _, absent := range []string{"trustyai.eval", "kserve.wva", "kserve.modelsAsService", "trainingoperator", "modelregistry", "workbenches.workbenchNamespace"} {
		if _, ok := byKey[absent]; ok {
			t.Errorf("%s is in the v3 tree", absent)
		}
	}
	// Parents before their children.
	idx := map[string]int{}
	for i, k := range order {
		idx[k] = i
	}
	if idx["kserve"] > idx["kserve.nim"] || idx["dashboard"] > idx["dashboard.standard"] {
		t.Fatalf("order: %v", order)
	}
	// The deprecated v2 view has its own tree: wva accepts only Removed.
	v2 := map[string]dscToggle{}
	for _, n := range dscToggleTree(crd.version("v2").Spec) {
		v2[n.key()] = n
	}
	if w := v2["kserve.wva"]; !reflect.DeepEqual(w.Allowed, []string{"Removed"}) {
		t.Fatalf("v2 kserve.wva = %+v", w)
	}
	if got := dscProtectedFields(crd.version("v3").Spec); !reflect.DeepEqual(got, map[string]bool{"workbenches.workbenchNamespace": true, "aiHub.instancesNamespace": true}) {
		t.Fatalf("protected fields = %v", got)
	}
}

func TestAllowedStatesFallsBackToThePatternOnlyWithoutEnum(t *testing.T) {
	if got := allowedStates(map[string]interface{}{"enum": []interface{}{"Managed", "Removed"}, "pattern": "^(Managed|Unmanaged|Force|Removed)$"}); !reflect.DeepEqual(got, []string{"Managed", "Removed"}) {
		t.Fatalf("enum: %v", got)
	}
	if got := allowedStates(map[string]interface{}{"pattern": "^(Managed|Unmanaged|Force|Removed)$"}); !reflect.DeepEqual(got, []string{"Managed", "Unmanaged", "Force", "Removed"}) {
		t.Fatalf("pattern: %v", got)
	}
	if got := allowedStates(map[string]interface{}{"pattern": "^[a-z]+$"}); got != nil {
		t.Fatalf("an unparsable pattern gives no values, got %v", got)
	}
}

func nodeOf(t *testing.T, m *types.DSCManager, path string) types.DSCNode {
	t.Helper()
	for _, n := range m.Nodes {
		if n.Path == path {
			return n
		}
	}
	t.Fatalf("no node %s", path)
	return types.DSCNode{}
}

// The live 3.6 DSC differs from the release default only by user choices
// and values the API server's defaulting fills in: no mismatch.
func TestDSCManagerOnTheLiveDSC(t *testing.T) {
	_, c := managerWorld(t, nil)
	m, err := GetDSCManager(c)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Exists || m.Name != "default-dsc" || m.APIVersion != "datasciencecluster.opendatahub.io/v3" || m.StorageVersion != "v3" || m.ReadOnlyReason != "" || m.DefaultsError != "" || m.OperatorVersion != "3.6.0" {
		t.Fatalf("manager = %+v", m)
	}
	if len(m.Mismatches) != 0 {
		t.Fatalf("schema defaults or user choices were flagged: %+v", m.Mismatches)
	}
	ag := nodeOf(t, m, "aigateway")
	if ag.Spec != "Managed" || ag.Default != "Removed" || !ag.Changed || ag.Actual != "Managed" || ag.Condition == nil || ag.Condition.Status != "True" {
		t.Fatalf("aigateway = %+v", ag)
	}
	maas := nodeOf(t, m, "aigateway.modelsAsAService")
	if maas.Actual != "Managed" || maas.Condition == nil || maas.Condition.Type != "ModelsAsAServiceReady" || !maas.Changed {
		t.Fatalf("modelsAsAService = %+v", maas)
	}
	portal := nodeOf(t, m, "dashboard.maasPortal")
	if portal.Spec != "Removed" || portal.Changed || portal.Condition == nil || portal.Condition.Type != "MaaSPortalAvailable" || portal.Actual != "Removed" {
		t.Fatalf("maasPortal = %+v", portal)
	}
	if d := nodeOf(t, m, "dashboard"); d.Toggle || d.Actual != "Managed" || d.Condition == nil || d.Condition.Type != "DashboardReady" {
		t.Fatalf("dashboard = %+v", d)
	}
	if hub := nodeOf(t, m, "aiHub"); hub.Condition == nil || hub.Condition.Type != "AIHubReady" || hub.Changed {
		t.Fatalf("aiHub = %+v", hub)
	}
	if k := nodeOf(t, m, "kueue"); !reflect.DeepEqual(k.Allowed, []string{"Managed", "Unmanaged", "Removed"}) || k.Changed {
		t.Fatalf("kueue = %+v", k)
	}
	var types []string
	for _, cd := range m.Conditions {
		types = append(types, cd.Type)
	}
	if strings.Join(types, ",") != "Ready,ProvisioningSucceeded,ComponentsReady,ModulesReady" {
		t.Fatalf("conditions = %v", types)
	}
}

func TestDSCManagerMismatches(t *testing.T) {
	_, c := managerWorld(t, func(dsc map[string]interface{}) {
		setPath(t, dsc, "spec.components.dashboard.standard", nil) // an upgraded DSC that lost it
		setPath(t, dsc, "spec.components.databaseservice", nil)    // added by a later release
		setPath(t, dsc, "spec.components.aigateway.managementState", "Removed")
		setPath(t, dsc, "spec.components.kserve.rawDeploymentServiceConfig", "Headed") // a user choice
	})
	m, err := GetDSCManager(c)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]types.DSCMismatch{}
	for _, mm := range m.Mismatches {
		got[mm.Kind+" "+mm.Path] = mm
	}
	if len(got) != 3 {
		t.Fatalf("mismatches = %+v", m.Mismatches)
	}
	std := got["unset dashboard.standard"]
	if std.Default != "Managed" || !std.Fixable || !strings.Contains(std.Message, "treats it as Removed, but the release default is Managed") {
		t.Fatalf("dashboard.standard = %+v", std)
	}
	if db := got["unset databaseservice"]; db.Default != "Removed" || !strings.Contains(db.Message, "also the release default") {
		t.Fatalf("databaseservice = %+v", db)
	}
	if p := got["parent-removed aigateway.modelsAsAService"]; p.Parent != "aigateway" || p.Fixable {
		t.Fatalf("modelsAsAService = %+v", p)
	}
}

// --- toggle --------------------------------------------------------------------

func TestToggleReplacesOnlyThatFieldAfterADryRun(t *testing.T) {
	f, c := managerWorld(t, nil)
	res, err := SetDSCManagementState(c, types.DSCToggleRequest{Name: "default-dsc", Path: "kserve.modelExpress", From: "Removed", To: "Managed"})
	if err != nil || !res.Success {
		t.Fatalf("result = %+v, %v", res, err)
	}
	dry, real := patchesTo(f, dscV3Path)
	if len(dry) != 1 || len(real) != 1 {
		t.Fatalf("patches: dry %d, real %d", len(dry), len(real))
	}
	var order []string
	for _, r := range f.mutations() {
		order = append(order, r)
	}
	if i := strings.Join(f.requests, "\n"); strings.Index(i, "DRYRUN PATCH "+dscV3Path) > strings.Index(i, "\nPATCH "+dscV3Path) {
		t.Fatalf("the dry run did not come first: %v", f.requests)
	}
	for _, call := range append(dry, real...) {
		if call.ContentType != "application/json-patch+json" || call.Query.Get("fieldManager") != dscComponentsManager || call.Query.Get("fieldValidation") != "Strict" {
			t.Fatalf("call = %+v", call)
		}
		want := `[{"op":"test","path":"/spec/components/kserve/modelExpress/managementState","value":"Removed"},{"op":"replace","path":"/spec/components/kserve/modelExpress/managementState","value":"Managed"}]`
		if call.Body != want {
			t.Fatalf("body = %s", call.Body)
		}
	}
	if s, _ := lookupState(comps(f.get(dscV3Path)), []string{"kserve", "modelExpress"}); s != "Managed" {
		t.Fatalf("stored %q", s)
	}
}

func TestToggleAddsMissingObjectsWithATestOfTheirParent(t *testing.T) {
	f, c := managerWorld(t, func(dsc map[string]interface{}) {
		setPath(t, dsc, "spec.components.databaseservice", nil)
		setPath(t, dsc, "spec.components.dashboard.maasPortal", nil)
	})
	before := comps(f.get(dscV3Path))
	res, err := SetDSCManagementState(c, types.DSCToggleRequest{Name: "default-dsc", Path: "databaseservice", From: "", To: "Managed"})
	if err != nil || !res.Success {
		t.Fatalf("result = %+v, %v", res, err)
	}
	_, real := patchesTo(f, dscV3Path)
	ops := decodeOps(t, real[0].Body)
	if len(ops) != 2 || ops[0]["op"] != "test" || ops[0]["path"] != "/spec/components" || !jsonEqual(ops[0]["value"], before) ||
		ops[1]["op"] != "add" || ops[1]["path"] != "/spec/components/databaseservice" || !jsonEqual(ops[1]["value"], map[string]interface{}{"managementState": "Managed"}) {
		t.Fatalf("ops = %v", ops)
	}

	dashboard, _ := comps(f.get(dscV3Path))["dashboard"].(map[string]interface{})
	res, err = SetDSCManagementState(c, types.DSCToggleRequest{Name: "default-dsc", Path: "dashboard.maasPortal", From: "", To: "Managed"})
	if err != nil || !res.Success {
		t.Fatalf("result = %+v, %v", res, err)
	}
	_, real = patchesTo(f, dscV3Path)
	ops = decodeOps(t, real[1].Body)
	if len(ops) != 2 || ops[0]["path"] != "/spec/components/dashboard" || !jsonEqual(ops[0]["value"], dashboard) ||
		ops[1]["op"] != "add" || ops[1]["path"] != "/spec/components/dashboard/maasPortal" {
		t.Fatalf("ops = %v", ops)
	}
}

// Someone changes the field between the dry run and the patch: the test
// operation fails, nothing is overwritten.
func TestToggleRefusesWhenTheFieldChangedMeanwhile(t *testing.T) {
	f, c := managerWorld(t, nil)
	var once sync.Once
	f.beforeRequest = func(r *http.Request) {
		if r.Method == http.MethodPatch && r.URL.Query().Get("dryRun") == "" {
			once.Do(func() {
				f.mu.Lock()
				k, _ := parseFakePath(dscV3Path)
				setPath(t, f.objects[k], "spec.components.ray.managementState", "Removed")
				f.mu.Unlock()
			})
		}
	}
	res, err := SetDSCManagementState(c, types.DSCToggleRequest{Name: "default-dsc", Path: "ray", From: "Managed", To: "Removed"})
	if err != nil || res.Success || res.ErrorCode != "conflict" || !strings.Contains(res.Message, "changed since you looked") {
		t.Fatalf("result = %+v, %v", res, err)
	}
	// And a stale "from" is refused before anything is sent.
	f.calls = nil
	res, _ = SetDSCManagementState(c, types.DSCToggleRequest{Name: "default-dsc", Path: "trainer", From: "Removed", To: "Managed"})
	if res.Success || res.ErrorCode != "conflict" {
		t.Fatalf("stale from = %+v", res)
	}
	if dry, real := patchesTo(f, dscV3Path); len(dry)+len(real) != 0 {
		t.Fatal("a stale from was sent")
	}
}

func TestToggleShowsAWebhookRejectionVerbatim(t *testing.T) {
	f, c := managerWorld(t, nil)
	f.failDryRun["PATCH "+dscV3Path] = http.StatusForbidden
	const denial = `admission webhook "datasciencecluster-v3-validator.opendatahub.io" denied the request: Kueue managementState Managed is no longer supported`
	f.failMessage["DRYRUN PATCH "+dscV3Path] = denial
	res, err := SetDSCManagementState(c, types.DSCToggleRequest{Name: "default-dsc", Path: "kueue", From: "Removed", To: "Managed"})
	if err != nil || res.Success || !strings.Contains(res.Message, denial) || !strings.Contains(res.Message, "dry run") {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if _, real := patchesTo(f, dscV3Path); len(real) != 0 {
		t.Fatal("patched after the dry run was refused")
	}
}

func TestToggleValidatesAgainstTheSchema(t *testing.T) {
	f, c := managerWorld(t, nil)
	for _, tc := range []struct{ path, from, to, code string }{
		{"ray", "Managed", "Unmanaged", "validation"},    // not in ray's enum
		{"modelregistry", "", "Removed", "validation"},   // v2 only
		{"trustyai.eval", "", "Removed", "validation"},   // no managementState
		{"kueue", "Removed", "Removed", "nothing_to_do"}, // already
		{"workbenches.workbenchesV2", "Removed", "Force", "validation"},
	} {
		res, err := SetDSCManagementState(c, types.DSCToggleRequest{Name: "default-dsc", Path: tc.path, From: tc.from, To: tc.to})
		if err != nil || res.Success || res.ErrorCode != tc.code {
			t.Errorf("%s to %s: %+v, %v", tc.path, tc.to, res, err)
		}
	}
	if dry, real := patchesTo(f, dscV3Path); len(dry)+len(real) != 0 {
		t.Fatal("an invalid toggle was sent")
	}
	// Kueue accepts Unmanaged.
	res, _ := SetDSCManagementState(c, types.DSCToggleRequest{Name: "default-dsc", Path: "kueue", From: "Removed", To: "Unmanaged"})
	if !res.Success {
		t.Fatalf("kueue Unmanaged: %+v", res)
	}
}

func TestToggleToManagedUnderARemovedParent(t *testing.T) {
	f, c := managerWorld(t, func(dsc map[string]interface{}) {
		setPath(t, dsc, "spec.components.aigateway.managementState", "Removed")
		setPath(t, dsc, "spec.components.aigateway.modelsAsAService.managementState", "Removed")
	})
	plan, err := PlanDSCChange(c, "toggle", "aigateway.batchGateway", "Managed", false)
	if err != nil || plan.Code != "parent_removed" || plan.ParentPath != "aigateway" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	res, _ := SetDSCManagementState(c, types.DSCToggleRequest{Name: "default-dsc", Path: "aigateway.batchGateway", From: "Removed", To: "Managed"})
	if res.Success || res.ErrorCode != "parent_removed" {
		t.Fatalf("without parent: %+v", res)
	}
	if dry, real := patchesTo(f, dscV3Path); len(dry)+len(real) != 0 {
		t.Fatal("patched without the parent")
	}
	plan, _ = PlanDSCChange(c, "toggle", "aigateway.batchGateway", "Managed", true)
	effects := map[string]string{}
	for _, ch := range plan.Changes {
		effects[ch.Path] = ch.Effect
	}
	if plan.Refusal != "" || effects["aigateway"] != "deploys" || effects["aigateway.batchGateway"] != "deploys" {
		t.Fatalf("plan with parent = %+v", plan)
	}
	res, _ = SetDSCManagementState(c, types.DSCToggleRequest{Name: "default-dsc", Path: "aigateway.batchGateway", From: "Removed", To: "Managed", WithParent: true})
	if !res.Success {
		t.Fatalf("with parent: %+v", res)
	}
	_, real := patchesTo(f, dscV3Path)
	if ops := decodeOps(t, real[0].Body); len(ops) != 4 || ops[0]["op"] != "test" || ops[1]["op"] != "test" || ops[2]["op"] != "replace" || ops[3]["op"] != "replace" {
		t.Fatalf("ops = %v", ops)
	}
	stored := comps(f.get(dscV3Path))
	if a, _ := lookupState(stored, []string{"aigateway"}); a != "Managed" {
		t.Fatalf("aigateway = %s", a)
	}
}

// Removing a component runs the removal preconditions and refuses when one
// is unmet or cannot be read.
func TestToggleToRemovedFailsClosed(t *testing.T) {
	f, c := managerWorld(t, nil)
	f.put(rhodsOperator, map[string]interface{}{"status": map[string]interface{}{"readyReplicas": 0}})
	plan, err := PlanDSCChange(c, "toggle", "ray", "Removed", false)
	if err != nil || len(plan.Blocked) != 1 || plan.Blocked[0].Component != "ray" || !strings.Contains(strings.Join(plan.Blocked[0].Reasons, " "), "rhods-operator must be running") {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	res, _ := SetDSCManagementState(c, types.DSCToggleRequest{Name: "default-dsc", Path: "ray", From: "Managed", To: "Removed"})
	if res.Success || res.ErrorCode != "prerequisites" {
		t.Fatalf("result = %+v", res)
	}
	// The module API cannot be discovered: refused, not skipped.
	f.put(rhodsOperator, map[string]interface{}{"status": map[string]interface{}{"readyReplicas": 1}})
	f.fail["GET /apis/components.platform.opendatahub.io"] = http.StatusServiceUnavailable
	res, _ = SetDSCManagementState(c, types.DSCToggleRequest{Name: "default-dsc", Path: "ray", From: "Managed", To: "Removed"})
	if res.Success || !strings.Contains(res.Message, "could not read the ray module CR") {
		t.Fatalf("discovery error: %+v", res)
	}
	// The tool's standing refusals still apply to a toggle.
	res, _ = SetDSCManagementState(c, types.DSCToggleRequest{Name: "default-dsc", Path: "aipipelines", From: "Managed", To: "Removed"})
	if res.Success || !strings.Contains(res.Message, "DSPAs") {
		t.Fatalf("aipipelines: %+v", res)
	}
	if dry, real := patchesTo(f, dscV3Path); len(dry)+len(real) != 0 {
		t.Fatal("patched a blocked removal")
	}
	delete(f.fail, "GET /apis/components.platform.opendatahub.io")
	res, _ = SetDSCManagementState(c, types.DSCToggleRequest{Name: "default-dsc", Path: "ray", From: "Managed", To: "Removed"})
	if !res.Success {
		t.Fatalf("preconditions met: %+v", res)
	}
}

func TestDSCChangesRefusedUnderArgoCDAndACM(t *testing.T) {
	for name, edit := range map[string]func(map[string]interface{}){
		"argo": func(dsc map[string]interface{}) {
			setPath(t, dsc, "metadata.annotations", map[string]interface{}{argoTrackingAnnotation: testArgoApp + ":datasciencecluster.opendatahub.io/DataScienceCluster:odh-gitops/default-dsc"})
		},
		"acm": func(dsc map[string]interface{}) {
			setPath(t, dsc, "metadata.managedFields", []interface{}{map[string]interface{}{"manager": acmPolicyManager, "operation": "Update", "fieldsType": "FieldsV1", "fieldsV1": map[string]interface{}{"f:spec": map[string]interface{}{}}}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, c := managerWorld(t, edit)
			m, _ := GetDSCManager(c)
			if m.ReadOnlyReason == "" {
				t.Fatal("no read-only reason")
			}
			res, _ := SetDSCManagementState(c, types.DSCToggleRequest{Name: "default-dsc", Path: "ray", From: "Managed", To: "Removed"})
			if res.Success || res.ErrorCode != ErrorCodeGitOpsManaged {
				t.Fatalf("toggle = %+v", res)
			}
			for _, action := range []string{"fix", "soft-reset", "hard-reset"} {
				if p, _ := PlanDSCChange(c, action, "", "", false); p.Code != ErrorCodeGitOpsManaged {
					t.Fatalf("%s plan = %+v", action, p)
				}
			}
			if len(f.mutations()) != 0 {
				t.Fatalf("writes: %v", f.mutations())
			}
		})
	}
}

// --- fix -------------------------------------------------------------------

func TestFixAddsExactlyThePreviewedToggles(t *testing.T) {
	f, c := managerWorld(t, func(dsc map[string]interface{}) {
		setPath(t, dsc, "spec.components.dashboard.standard", nil)
		setPath(t, dsc, "spec.components.databaseservice", nil)
	})
	plan, err := PlanDSCChange(c, "fix", "", "", false)
	if err != nil || plan.Refusal != "" || plan.Token == "" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	got := map[string]types.DSCChange{}
	for _, ch := range plan.Changes {
		got[ch.Path] = ch
	}
	if len(got) != 2 || got["dashboard.standard"].To != "Managed" || got["dashboard.standard"].Effect != "deploys" || got["databaseservice"].To != "Removed" || got["databaseservice"].Effect != "" {
		t.Fatalf("changes = %+v", plan.Changes)
	}
	// A plan that is not the current one is refused.
	res, _ := FixDSC(c, types.DSCPlanRequest{Name: "default-dsc", UID: plan.UID, Token: strings.Repeat("0", 24)})
	if res.Success || res.ErrorCode != "conflict" {
		t.Fatalf("stale token: %+v", res)
	}
	if dry, real := patchesTo(f, dscV3Path); len(dry)+len(real) != 0 {
		t.Fatal("patched a stale plan")
	}
	res, err = FixDSC(c, types.DSCPlanRequest{Name: "default-dsc", UID: plan.UID, Token: plan.Token})
	if err != nil || !res.Success {
		t.Fatalf("fix = %+v, %v", res, err)
	}
	stored := comps(f.get(dscV3Path))
	if s, _ := lookupState(stored, []string{"dashboard", "standard"}); s != "Managed" {
		t.Fatalf("dashboard.standard = %q", s)
	}
	if s, _ := lookupState(stored, []string{"databaseservice"}); s != "Removed" {
		t.Fatalf("databaseservice = %q", s)
	}
	if m, _ := GetDSCManager(c); len(m.Mismatches) != 0 {
		t.Fatalf("still mismatched: %+v", m.Mismatches)
	}
	// The DSC changed after the preview: refused.
	plan2, _ := PlanDSCChange(c, "fix", "", "", false)
	if plan2.Code != "nothing_to_do" {
		t.Fatalf("plan after fix = %+v", plan2)
	}
}

// --- soft reset --------------------------------------------------------------

func TestSoftResetChangesOnlyWhatDiffersAndKeepsProtectedFields(t *testing.T) {
	f, c := managerWorld(t, func(dsc map[string]interface{}) {
		setPath(t, dsc, "spec.components.workbenches.workbenchNamespace", "my-notebooks")
		setPath(t, dsc, "spec.components.aiHub.instancesNamespace", "my-registries")
		setPath(t, dsc, "spec.components.kserve.rawDeploymentServiceConfig", "Headed")
	})
	plan, err := PlanDSCChange(c, "soft-reset", "", "", false)
	if err != nil || plan.Refusal != "" || len(plan.Blocked) != 0 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	var paths []string
	for _, ch := range plan.Changes {
		paths = append(paths, ch.Path+":"+ch.From+">"+ch.To+":"+ch.Effect)
	}
	want := []string{"aigateway:Managed>Removed:removes", "aigateway.modelsAsAService:Managed>Removed:removes", "kserve.rawDeploymentServiceConfig:Headed>:"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("changes = %v, want %v", paths, want)
	}
	res, err := SoftResetDSC(c, types.DSCPlanRequest{Name: "default-dsc", UID: plan.UID, Token: plan.Token})
	if err != nil || !res.Success {
		t.Fatalf("reset = %+v, %v", res, err)
	}
	_, real := patchesTo(f, dscV3Path)
	body := real[0].Body
	for _, kept := range []string{"workbenchNamespace", "instancesNamespace", "defaultLocalQueueName", "airGapped", "mcpGuardrailsMode"} {
		if strings.Contains(body, kept) {
			t.Errorf("the reset touched %s: %s", kept, body)
		}
	}
	stored := comps(f.get(dscV3Path))
	if v, _ := lookupPath(stored, []string{"workbenches", "workbenchNamespace"}); v != "my-notebooks" {
		t.Fatalf("workbenchNamespace = %v", v)
	}
	if v, _ := lookupPath(stored, []string{"aiHub", "instancesNamespace"}); v != "my-registries" {
		t.Fatalf("instancesNamespace = %v", v)
	}
	if _, ok := lookupPath(stored, []string{"kserve", "rawDeploymentServiceConfig"}); ok {
		t.Fatal("rawDeploymentServiceConfig kept")
	}
	if a, _ := lookupState(stored, []string{"aigateway"}); a != "Removed" {
		t.Fatalf("aigateway = %s", a)
	}
}

func TestSoftResetRefusesWhileARemovalIsBlocked(t *testing.T) {
	f, c := managerWorld(t, nil)
	f.put(rhodsOperator, map[string]interface{}{"status": map[string]interface{}{"readyReplicas": 0}})
	plan, _ := PlanDSCChange(c, "soft-reset", "", "", false)
	if len(plan.Blocked) == 0 || plan.Blocked[0].Component != "aigateway" {
		t.Fatalf("plan = %+v", plan)
	}
	res, _ := SoftResetDSC(c, types.DSCPlanRequest{Name: "default-dsc", UID: plan.UID, Token: plan.Token})
	if res.Success || res.ErrorCode != "prerequisites" {
		t.Fatalf("reset = %+v", res)
	}
	if dry, real := patchesTo(f, dscV3Path); len(dry)+len(real) != 0 {
		t.Fatal("patched a blocked reset")
	}
}

// --- JSON patch builder -------------------------------------------------------

func TestBuildJSONPatchGroupsMissingObjects(t *testing.T) {
	obj := map[string]interface{}{"spec": map[string]interface{}{"components": map[string]interface{}{"kserve": map[string]interface{}{"managementState": "Managed"}}}}
	ops, err := buildJSONPatch(obj, []dscAssign{
		{Path: componentsPath("aigateway", "managementState"), Value: "Removed"},
		{Path: componentsPath("aigateway", "batchGateway", "managementState"), Value: "Removed"},
		{Path: componentsPath("kserve", "managementState"), Value: "Removed"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(ops)
	want := `[{"op":"test","path":"/spec/components","value":{"kserve":{"managementState":"Managed"}}},{"op":"test","path":"/spec/components/kserve/managementState","value":"Managed"},` +
		`{"op":"replace","path":"/spec/components/kserve/managementState","value":"Removed"},{"op":"add","path":"/spec/components/aigateway","value":{"batchGateway":{"managementState":"Removed"},"managementState":"Removed"}}]`
	if string(raw) != want {
		t.Fatalf("ops = %s", raw)
	}
	after, err := applyJSONPatch(obj, ops)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := lookupState(comps(after), []string{"aigateway", "batchGateway"}); s != "Removed" {
		t.Fatalf("after = %v", after)
	}
	// The original is unchanged; a failed test applies nothing.
	if _, err := applyJSONPatch(obj, []jsonPatchOp{patchOp("test", componentsPath("kserve", "managementState"), "Removed")}); err == nil {
		t.Fatal("test passed on another value")
	}
	if s, _ := lookupState(comps(obj), []string{"kserve"}); s != "Managed" {
		t.Fatal("applyJSONPatch changed its input")
	}
}

// --- RBAC ----------------------------------------------------------------------

// The hard reset's delete is granted only by the full profile's template,
// cluster-wide (the DSC is a singleton named by its creator), and never by
// the gitops profile.
func TestHardResetDeleteRule(t *testing.T) {
	has := func(path string) bool {
		for _, g := range templateGrantsFrom(t, path) {
			if containsString(g.rule.APIGroups, dscGroup) && containsString(g.rule.Resources, "datascienceclusters") && containsString(g.rule.Verbs, "delete") {
				if len(g.rule.ResourceNames) != 0 || g.namespace != "" {
					t.Fatalf("%s: unexpected delete rule %+v in %q", path, g.rule, g.namespace)
				}
				return true
			}
		}
		return false
	}
	if !has(templateRel) {
		t.Fatal("deploy/template.yaml does not grant delete on datascienceclusters")
	}
	if has(rbacGitOpsRel) {
		t.Fatal("deploy/rbac-gitops.yaml grants delete on datascienceclusters")
	}
}
