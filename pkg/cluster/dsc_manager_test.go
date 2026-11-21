package cluster

import (
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/argo"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dsc"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
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

func readFixture(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

// managerWorld is a resourceFake holding the 3.6 fixtures, the installed CSV
// and Subscription, and a ready rhods-operator. edit changes the live DSC
// before it is stored.
func managerWorld(t *testing.T, edit func(dsc map[string]any)) (*resourceFake, *Client) {
	t.Helper()
	f, c := newResourceFake(t)
	f.groups = map[string]string{"/apis/datasciencecluster.opendatahub.io": dscGroupDoc}
	f.put(components.DSCCRDPath, readFixture(t, dscCRDFixture))
	dsc := readFixture(t, dscLiveFixture)
	if edit != nil {
		edit(dsc)
	}
	f.put(dscV3Path, dsc)
	alm, err := os.ReadFile(dscALMFixture)
	if err != nil {
		t.Fatal(err)
	}
	f.put(rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "subscriptions", rhoai.SubNS, rhoai.SubName), map[string]any{"status": map[string]any{"installedCSV": "rhods-operator.3.6.0"}})
	f.put(rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", rhoai.SubNS, "rhods-operator.3.6.0"), map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{"alm-examples": string(alm)}},
		"spec": map[string]any{"version": "3.6.0", "install": map[string]any{"spec": map[string]any{
			"deployments": []any{map[string]any{"name": "rhods-operator"}}}}},
		"status": map[string]any{"phase": "Succeeded"},
	})
	f.put(rhodsOperator, map[string]any{"status": map[string]any{"readyReplicas": 1}})
	return f, c
}

// comps returns spec.components of a stored DSC.
func comps(obj map[string]any) map[string]any {
	spec, _ := obj["spec"].(map[string]any)
	c, _ := spec["components"].(map[string]any)
	return c
}

func setPath(t *testing.T, obj map[string]any, path string, value any) {
	t.Helper()
	keys := strings.Split(path, ".")
	m := obj
	for _, k := range keys[:len(keys)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
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
func patchesTo(f *resourceFake) (dry, real []fakeCall) {
	path := dscV3Path
	for _, c := range f.callsTo(http.MethodPatch, path) {
		if c.Query.Get("dryRun") == "All" {
			dry = append(dry, c)
		} else {
			real = append(real, c)
		}
	}
	return dry, real
}

func decodeOps(t *testing.T, body string) []map[string]any {
	t.Helper()
	var ops []map[string]any
	if err := json.Unmarshal([]byte(body), &ops); err != nil {
		t.Fatalf("not a JSON patch: %s", body)
	}
	return ops
}

// fieldOps decodes a JSON patch of the fixture DSC, checks that it starts
// with the tests of the DSC's uid and resourceVersion (write) and returns
// the operations after them.
func fieldOps(t *testing.T, body string) []map[string]any {
	t.Helper()
	ops := decodeOps(t, body)
	if len(ops) < 2 || ops[0]["op"] != "test" || ops[0]["path"] != "/metadata/uid" || ops[0]["value"] != dscFixtureUID ||
		ops[1]["op"] != "test" || ops[1]["path"] != "/metadata/resourceVersion" {
		t.Fatalf("the patch does not start with the tests of the DSC's uid and resourceVersion: %s", body)
	}
	return ops[2:]
}

// reviewedToggle plans the toggle as the dialog does, then sends it with
// the plan's UID and token.
func reviewedToggle(t *testing.T, c *Client, req types.DSCToggleRequest) (*types.OperationResponse, error) {
	t.Helper()
	plan, err := PlanDSCChange(c, "toggle", req.Path, req.To, req.WithParent, false)
	if err != nil {
		t.Fatal(err)
	}
	req.UID, req.Token = plan.UID, plan.Token
	return SetDSCManagementState(c, req)
}

func TestDSCToggleTreeFromTheInstalledCRD(t *testing.T) {
	f, c := newResourceFake(t)
	f.put(components.DSCCRDPath, readFixture(t, dscCRDFixture))
	crd, err := components.ReadDSCCRD(c)
	if err != nil {
		t.Fatal(err)
	}
	if crd.Storage() != "v3" || crd.Version("v2") == nil || !crd.Version("v2").Deprecated {
		t.Fatalf("versions: %+v", crd.Versions)
	}
	tree := components.DSCToggleTree(crd.Version("v3").Spec)
	byKey := map[string]components.DSCToggle{}
	var order []string
	for _, n := range tree {
		byKey[n.Key()] = n
		order = append(order, n.Key())
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
	v2 := map[string]components.DSCToggle{}
	for _, n := range components.DSCToggleTree(crd.Version("v2").Spec) {
		v2[n.Key()] = n
	}
	if w := v2["kserve.wva"]; !reflect.DeepEqual(w.Allowed, []string{"Removed"}) {
		t.Fatalf("v2 kserve.wva = %+v", w)
	}
	if got := components.DSCProtectedFields(crd.Version("v3").Spec); !reflect.DeepEqual(got, map[string]bool{"workbenches.workbenchNamespace": true, "aiHub.instancesNamespace": true}) {
		t.Fatalf("protected fields = %v", got)
	}
}

func TestAllowedStatesFallsBackToThePatternOnlyWithoutEnum(t *testing.T) {
	if got := components.AllowedStates(map[string]any{"enum": []any{"Managed", "Removed"}, "pattern": "^(Managed|Unmanaged|Force|Removed)$"}); !reflect.DeepEqual(got, []string{"Managed", "Removed"}) {
		t.Fatalf("enum: %v", got)
	}
	if got := components.AllowedStates(map[string]any{"pattern": "^(Managed|Unmanaged|Force|Removed)$"}); !reflect.DeepEqual(got, []string{"Managed", "Unmanaged", "Force", "Removed"}) {
		t.Fatalf("pattern: %v", got)
	}
	if got := components.AllowedStates(map[string]any{"pattern": "^[a-z]+$"}); got != nil {
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
	_, c := managerWorld(t, func(dsc map[string]any) {
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

// The comparison says what it covered: on the live DSC the CRD, the release
// default and the DSC list the same 16 components.
func TestDSCManagerComparisonOnTheLiveDSC(t *testing.T) {
	_, c := managerWorld(t, nil)
	m, err := GetDSCManager(c)
	if err != nil {
		t.Fatal(err)
	}
	cmp := m.Comparison
	if !cmp.DefaultsRead || !cmp.SchemaRead || cmp.DefaultsCSV != "rhods-operator.3.6.0" {
		t.Fatalf("comparison = %+v", cmp)
	}
	want := []string{"aiHub", "aigateway", "aipipelines", "dashboard", "data", "databaseservice", "kserve", "kueue", "mcplifecycleoperator", "mlflowoperator", "ogx", "ray", "sparkoperator", "trainer", "trustyai", "workbenches"}
	for name, got := range map[string][]string{"crd": cmp.CRDComponents, "default": cmp.DefaultComponents, "dsc": cmp.DSCComponents} {
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s components = %v", name, got)
		}
	}
	for _, n := range m.Nodes {
		if n.Unknown {
			t.Fatalf("unknown node on the live DSC: %+v", n)
		}
	}
}

// Fields the installed CRD does not define are rows of the tree, under the
// nearest node above them, as well as mismatches; Fix removes them.
func TestDSCManagerUnknownFieldsAreRowsAndFixRemovesThem(t *testing.T) {
	f, c := managerWorld(t, func(dsc map[string]any) {
		setPath(t, dsc, "spec.components.codeflare", map[string]any{"managementState": "Removed"}) // a v2 component
		setPath(t, dsc, "spec.components.kserve.serving", map[string]any{"managementState": "Managed"})
		setPath(t, dsc, "spec.components.kserve.oauthProxy.legacy", true) // under an object that is not a row
		setPath(t, dsc, "spec.components.databaseservice", nil)
	})
	m, err := GetDSCManager(c)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, mm := range m.Mismatches {
		kinds[mm.Path] = mm.Kind
	}
	if kinds["codeflare"] != "unknown-field" || kinds["kserve.serving"] != "unknown-field" || kinds["kserve.oauthProxy.legacy"] != "unknown-field" || kinds["databaseservice"] != "unset" {
		t.Fatalf("mismatches = %+v", m.Mismatches)
	}
	var paths []string
	for _, n := range m.Nodes {
		paths = append(paths, n.Path)
	}
	index := func(p string) int {
		for i, q := range paths {
			if q == p {
				return i
			}
		}
		t.Fatalf("no row %s in %v", p, paths)
		return -1
	}
	cf := nodeOf(t, m, "codeflare")
	if !cf.Unknown || cf.Toggle || cf.Parent != "" || cf.Depth != 0 || cf.Name != "codeflare" || cf.SpecSet || len(cf.Allowed) != 0 {
		t.Fatalf("codeflare = %+v", cf)
	}
	if index("codeflare") != len(paths)-1 {
		t.Fatalf("a top-level unknown field is not after the CRD's components: %v", paths)
	}
	sv := nodeOf(t, m, "kserve.serving")
	if !sv.Unknown || sv.Parent != "kserve" || sv.Depth != 1 || sv.Name != "serving" {
		t.Fatalf("kserve.serving = %+v", sv)
	}
	op := nodeOf(t, m, "kserve.oauthProxy.legacy")
	if !op.Unknown || op.Parent != "kserve" || op.Depth != 1 || op.Name != "oauthProxy.legacy" {
		t.Fatalf("kserve.oauthProxy.legacy = %+v", op)
	}
	// After kserve's own sub-components, before the next component.
	if index("kserve.nim") > index("kserve.oauthProxy.legacy") || index("kserve.serving") > index("kueue") || index("kserve.oauthProxy.legacy") > index("kserve.serving") {
		t.Fatalf("unknown rows out of place: %v", paths)
	}
	if want := []string{"codeflare"}; !reflect.DeepEqual(diffList(m.Comparison.DSCComponents, m.Comparison.CRDComponents), want) {
		t.Fatalf("dsc components = %v", m.Comparison.DSCComponents)
	}
	if got := diffList(m.Comparison.DefaultComponents, m.Comparison.DSCComponents); !reflect.DeepEqual(got, []string{"databaseservice"}) {
		t.Fatalf("default components not in the DSC = %v", got)
	}

	plan, err := PlanDSCChange(c, "fix", "", "", false, false)
	if err != nil || plan.Refusal != "" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	res, err := FixDSC(c, types.DSCPlanRequest{Name: "default-dsc", UID: plan.UID, Token: plan.Token})
	if err != nil || !res.Success {
		t.Fatalf("fix = %+v, %v", res, err)
	}
	stored := comps(f.get(dscV3Path))
	if _, ok := stored["codeflare"]; ok {
		t.Fatal("codeflare is still in the DSC")
	}
	if _, ok := components.LookupPath(stored, []string{"kserve", "serving"}); ok {
		t.Fatal("kserve.serving is still in the DSC")
	}
	if _, ok := components.LookupPath(stored, []string{"kserve", "oauthProxy", "legacy"}); ok {
		t.Fatal("kserve.oauthProxy.legacy is still in the DSC")
	}
	after, _ := GetDSCManager(c)
	if len(after.Mismatches) != 0 {
		t.Fatalf("still mismatched: %+v", after.Mismatches)
	}
	for _, n := range after.Nodes {
		if n.Unknown {
			t.Fatalf("unknown row after the fix: %+v", n)
		}
	}
}

// diffList returns the entries of a that b does not have.
func diffList(a, b []string) []string {
	in := map[string]bool{}
	for _, s := range b {
		in[s] = true
	}
	var out []string
	for _, s := range a {
		if !in[s] {
			out = append(out, s)
		}
	}
	return out
}

// Without a release default nothing is compared with it, and the
// comparison says so rather than reporting nothing missing.
func TestDSCManagerComparisonWithoutAReleaseDefault(t *testing.T) {
	f, c := managerWorld(t, func(dsc map[string]any) {
		setPath(t, dsc, "spec.components.dashboard.standard", nil)
	})
	csv := rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", rhoai.SubNS, "rhods-operator.3.6.0")
	obj := f.get(csv)
	setPath(t, obj, "metadata.annotations.alm-examples", "[]")
	f.put(csv, obj)
	m, err := GetDSCManager(c)
	if err != nil {
		t.Fatal(err)
	}
	cmp := m.Comparison
	if cmp.DefaultsRead || !cmp.SchemaRead || m.DefaultsError == "" || cmp.DefaultsCSV != "" || len(cmp.DefaultComponents) != 0 || len(cmp.DSCComponents) != 16 || len(cmp.CRDComponents) != 16 {
		t.Fatalf("comparison = %+v (defaults error %q)", cmp, m.DefaultsError)
	}
	raw, _ := json.Marshal(cmp)
	if !strings.Contains(string(raw), `"defaultComponents":[]`) {
		t.Fatalf("defaultComponents must be an empty list: %s", raw)
	}
	for _, mm := range m.Mismatches {
		if mm.Kind == "unset" {
			t.Fatalf("an unset value was compared with no release default: %+v", mm)
		}
	}
}

// --- toggle --------------------------------------------------------------------

func TestToggleReplacesOnlyThatFieldAfterADryRun(t *testing.T) {
	f, c := managerWorld(t, nil)
	res, err := reviewedToggle(t, c, types.DSCToggleRequest{Name: "default-dsc", Path: "kserve.modelExpress", From: "Removed", To: "Managed"})
	if err != nil || !res.Success {
		t.Fatalf("result = %+v, %v", res, err)
	}
	dry, real := patchesTo(f)
	if len(dry) != 1 || len(real) != 1 {
		t.Fatalf("patches: dry %d, real %d", len(dry), len(real))
	}
	if i := strings.Join(f.requests, "\n"); strings.Index(i, "DRYRUN PATCH "+dscV3Path) > strings.Index(i, "\nPATCH "+dscV3Path) {
		t.Fatalf("the dry run did not come first: %v", f.requests)
	}
	for _, call := range append(dry, real...) {
		if call.ContentType != "application/json-patch+json" || call.Query.Get("fieldManager") != dsc.ComponentsManager || call.Query.Get("fieldValidation") != "Strict" {
			t.Fatalf("call = %+v", call)
		}
		want := `[{"op":"test","path":"/metadata/uid","value":"` + dscFixtureUID + `"},{"op":"test","path":"/metadata/resourceVersion","value":"` + dscFixtureRV + `"},` +
			`{"op":"test","path":"/spec/components/kserve/modelExpress/managementState","value":"Removed"},{"op":"replace","path":"/spec/components/kserve/modelExpress/managementState","value":"Managed"}]`
		if call.Body != want {
			t.Fatalf("body = %s", call.Body)
		}
	}
	if s, _ := components.LookupState(comps(f.get(dscV3Path)), []string{"kserve", "modelExpress"}); s != "Managed" {
		t.Fatalf("stored %q", s)
	}
}

func TestToggleAddsMissingObjectsWithATestOfTheirParent(t *testing.T) {
	f, c := managerWorld(t, func(dsc map[string]any) {
		setPath(t, dsc, "spec.components.databaseservice", nil)
		setPath(t, dsc, "spec.components.dashboard.maasPortal", nil)
	})
	before := comps(f.get(dscV3Path))
	res, err := reviewedToggle(t, c, types.DSCToggleRequest{Name: "default-dsc", Path: "databaseservice", From: "", To: "Managed"})
	if err != nil || !res.Success {
		t.Fatalf("result = %+v, %v", res, err)
	}
	_, real := patchesTo(f)
	ops := fieldOps(t, real[0].Body)
	if len(ops) != 2 || ops[0]["op"] != "test" || ops[0]["path"] != "/spec/components" || !kube.JSONEqual(ops[0]["value"], before) ||
		ops[1]["op"] != "add" || ops[1]["path"] != "/spec/components/databaseservice" || !kube.JSONEqual(ops[1]["value"], map[string]any{"managementState": "Managed"}) {
		t.Fatalf("ops = %v", ops)
	}

	dashboard, _ := comps(f.get(dscV3Path))["dashboard"].(map[string]any)
	res, err = reviewedToggle(t, c, types.DSCToggleRequest{Name: "default-dsc", Path: "dashboard.maasPortal", From: "", To: "Managed"})
	if err != nil || !res.Success {
		t.Fatalf("result = %+v, %v", res, err)
	}
	_, real = patchesTo(f)
	ops = fieldOps(t, real[1].Body)
	if len(ops) != 2 || ops[0]["path"] != "/spec/components/dashboard" || !kube.JSONEqual(ops[0]["value"], dashboard) ||
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
				f.bumpRV(ensureMap(f.objects[k], "metadata"))
				f.mu.Unlock()
			})
		}
	}
	res, err := reviewedToggle(t, c, types.DSCToggleRequest{Name: "default-dsc", Path: "ray", From: "Managed", To: "Removed"})
	if err != nil || res.Success || res.ErrorCode != "conflict" || !strings.Contains(res.Message, "changed since you looked") {
		t.Fatalf("result = %+v, %v", res, err)
	}
	// And a stale "from" is refused before anything is sent.
	f.calls = nil
	res, _ = reviewedToggle(t, c, types.DSCToggleRequest{Name: "default-dsc", Path: "trainer", From: "Removed", To: "Managed"})
	if res.Success || res.ErrorCode != "conflict" {
		t.Fatalf("stale from = %+v", res)
	}
	if dry, real := patchesTo(f); len(dry)+len(real) != 0 {
		t.Fatal("a stale from was sent")
	}
}

func TestToggleShowsAWebhookRejectionVerbatim(t *testing.T) {
	f, c := managerWorld(t, nil)
	f.failDryRun["PATCH "+dscV3Path] = http.StatusForbidden
	const denial = `admission webhook "datasciencecluster-v3-validator.opendatahub.io" denied the request: Kueue managementState Managed is no longer supported`
	f.failMessage["DRYRUN PATCH "+dscV3Path] = denial
	res, err := reviewedToggle(t, c, types.DSCToggleRequest{Name: "default-dsc", Path: "kueue", From: "Removed", To: "Managed"})
	if err != nil || res.Success || !strings.Contains(res.Message, denial) || !strings.Contains(res.Message, "dry run") {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if _, real := patchesTo(f); len(real) != 0 {
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
		res, err := reviewedToggle(t, c, types.DSCToggleRequest{Name: "default-dsc", Path: tc.path, From: tc.from, To: tc.to})
		if err != nil || res.Success || res.ErrorCode != tc.code {
			t.Errorf("%s to %s: %+v, %v", tc.path, tc.to, res, err)
		}
	}
	if dry, real := patchesTo(f); len(dry)+len(real) != 0 {
		t.Fatal("an invalid toggle was sent")
	}
	// Kueue accepts Unmanaged.
	res, _ := reviewedToggle(t, c, types.DSCToggleRequest{Name: "default-dsc", Path: "kueue", From: "Removed", To: "Unmanaged"})
	if !res.Success {
		t.Fatalf("kueue Unmanaged: %+v", res)
	}
}

func TestToggleToManagedUnderARemovedParent(t *testing.T) {
	f, c := managerWorld(t, func(dsc map[string]any) {
		setPath(t, dsc, "spec.components.aigateway.managementState", "Removed")
		setPath(t, dsc, "spec.components.aigateway.modelsAsAService.managementState", "Removed")
	})
	plan, err := PlanDSCChange(c, "toggle", "aigateway.batchGateway", "Managed", false, false)
	if err != nil || plan.Code != "parent_removed" || plan.ParentPath != "aigateway" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	res, _ := reviewedToggle(t, c, types.DSCToggleRequest{Name: "default-dsc", Path: "aigateway.batchGateway", From: "Removed", To: "Managed"})
	if res.Success || res.ErrorCode != "parent_removed" {
		t.Fatalf("without parent: %+v", res)
	}
	if dry, real := patchesTo(f); len(dry)+len(real) != 0 {
		t.Fatal("patched without the parent")
	}
	plan, _ = PlanDSCChange(c, "toggle", "aigateway.batchGateway", "Managed", true, false)
	effects := map[string]string{}
	for _, ch := range plan.Changes {
		effects[ch.Path] = ch.Effect
	}
	if plan.Refusal != "" || effects["aigateway"] != "deploys" || effects["aigateway.batchGateway"] != "deploys" {
		t.Fatalf("plan with parent = %+v", plan)
	}
	res, _ = reviewedToggle(t, c, types.DSCToggleRequest{Name: "default-dsc", Path: "aigateway.batchGateway", From: "Removed", To: "Managed", WithParent: true})
	if !res.Success {
		t.Fatalf("with parent: %+v", res)
	}
	_, real := patchesTo(f)
	if ops := fieldOps(t, real[0].Body); len(ops) != 4 || ops[0]["op"] != "test" || ops[1]["op"] != "test" || ops[2]["op"] != "replace" || ops[3]["op"] != "replace" {
		t.Fatalf("ops = %v", ops)
	}
	stored := comps(f.get(dscV3Path))
	if a, _ := components.LookupState(stored, []string{"aigateway"}); a != "Managed" {
		t.Fatalf("aigateway = %s", a)
	}
}

// Removing a component runs the removal preconditions and refuses when one
// is unmet or cannot be read.
func TestToggleToRemovedFailsClosed(t *testing.T) {
	f, c := managerWorld(t, nil)
	f.put(rhodsOperator, map[string]any{"status": map[string]any{"readyReplicas": 0}})
	plan, err := PlanDSCChange(c, "toggle", "ray", "Removed", false, false)
	if err != nil || len(plan.Blocked) != 1 || plan.Blocked[0].Component != "ray" || !strings.Contains(strings.Join(plan.Blocked[0].Reasons, " "), "RHOAI operator must be running to apply the change and clean up after it: Deployment redhat-ods-operator/rhods-operator has no ready pod") {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	res, _ := reviewedToggle(t, c, types.DSCToggleRequest{Name: "default-dsc", Path: "ray", From: "Managed", To: "Removed"})
	if res.Success || res.ErrorCode != "prerequisites" {
		t.Fatalf("result = %+v", res)
	}
	// The module API cannot be discovered: refused, not skipped.
	f.put(rhodsOperator, map[string]any{"status": map[string]any{"readyReplicas": 1}})
	f.fail["GET /apis/components.platform.opendatahub.io"] = http.StatusServiceUnavailable
	res, _ = reviewedToggle(t, c, types.DSCToggleRequest{Name: "default-dsc", Path: "ray", From: "Managed", To: "Removed"})
	if res.Success || !strings.Contains(res.Message, "could not read the ray module CR") {
		t.Fatalf("discovery error: %+v", res)
	}
	// The tool's standing refusals still apply to a toggle.
	res, _ = reviewedToggle(t, c, types.DSCToggleRequest{Name: "default-dsc", Path: "aipipelines", From: "Managed", To: "Removed"})
	if res.Success || !strings.Contains(res.Message, "DSPAs") {
		t.Fatalf("aipipelines: %+v", res)
	}
	if dry, real := patchesTo(f); len(dry)+len(real) != 0 {
		t.Fatal("patched a blocked removal")
	}
	delete(f.fail, "GET /apis/components.platform.opendatahub.io")
	res, _ = reviewedToggle(t, c, types.DSCToggleRequest{Name: "default-dsc", Path: "ray", From: "Managed", To: "Removed"})
	if !res.Success {
		t.Fatalf("preconditions met: %+v", res)
	}
}

func TestDSCChangesRefusedUnderArgoCDAndACM(t *testing.T) {
	for name, edit := range map[string]func(map[string]any){
		"argo": func(dsc map[string]any) {
			setPath(t, dsc, "metadata.annotations", map[string]any{argo.TrackingAnnotation: testArgoApp + ":datasciencecluster.opendatahub.io/DataScienceCluster:odh-gitops/default-dsc"})
		},
		"acm": func(dsc map[string]any) {
			setPath(t, dsc, "metadata.managedFields", []any{map[string]any{"manager": argo.ACMPolicyManager, "operation": "Update", "fieldsType": "FieldsV1", "fieldsV1": map[string]any{"f:spec": map[string]any{}}}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, c := managerWorld(t, edit)
			f.argo = argoManagingAll(argo.DSCObject("default-dsc"))
			m, _ := GetDSCManager(c)
			if m.ReadOnlyReason == "" {
				t.Fatal("no read-only reason")
			}
			res, _ := reviewedToggle(t, c, types.DSCToggleRequest{Name: "default-dsc", Path: "ray", From: "Managed", To: "Removed"})
			if res.Success || res.ErrorCode != kube.ErrorCodeGitOpsManaged {
				t.Fatalf("toggle = %+v", res)
			}
			for _, action := range []string{"fix", "soft-reset", "hard-reset"} {
				if p, _ := PlanDSCChange(c, action, "", "", false, false); p.Code != kube.ErrorCodeGitOpsManaged {
					t.Fatalf("%s plan = %+v", action, p)
				}
			}
			if len(f.mutations()) != 0 {
				t.Fatalf("writes: %v", f.mutations())
			}
		})
	}
}

// A tracking annotation Argo CD does not confirm: left over, the DSC is the
// tool's to change; Argo CD not reachable, it stays read-only and says why.
func TestDSCManagerConfirmsArgoCDTracking(t *testing.T) {
	tracked := func(dsc map[string]any) {
		setPath(t, dsc, "metadata.annotations", map[string]any{argo.TrackingAnnotation: testArgoApp + ":datasciencecluster.opendatahub.io/DataScienceCluster:odh-gitops/default-dsc"})
	}
	for name, argoCD := range map[string]*fakeArgoCD{
		"argo cd not installed": {},
		"application gone":      {served: true},
		"not among its resources": {served: true, apps: []map[string]any{
			argoApplicationListing("openshift-gitops", argo.SubscriptionObject)}},
	} {
		t.Run(name, func(t *testing.T) {
			f, c := managerWorld(t, tracked)
			f.argo = argoCD
			m, err := GetDSCManager(c)
			if err != nil || m.ReadOnlyReason != "" || m.ManagedBy != "" {
				t.Fatalf("manager = %+v, %v", m, err)
			}
			res, _ := reviewedToggle(t, c, types.DSCToggleRequest{Name: "default-dsc", Path: "ray", From: "Managed", To: "Removed"})
			if !res.Success {
				t.Fatalf("toggle = %+v", res)
			}
		})
	}
	t.Run("cannot ask Argo CD", func(t *testing.T) {
		f, c := managerWorld(t, tracked)
		f.argo = &fakeArgoCD{served: true, fail: map[string]int{argoDiscoveryPath + "/applications": http.StatusForbidden}}
		m, _ := GetDSCManager(c)
		if m.ManagedBy != testArgoApp || !strings.Contains(m.ReadOnlyReason, "could not confirm this with Argo CD") || !strings.Contains(m.ReadOnlyReason, "403") {
			t.Fatalf("manager = %+v", m)
		}
		res, _ := reviewedToggle(t, c, types.DSCToggleRequest{Name: "default-dsc", Path: "ray", From: "Managed", To: "Removed"})
		if res.Success || res.ErrorCode != kube.ErrorCodeGitOpsManaged || len(f.mutations()) != 0 {
			t.Fatalf("toggle = %+v, writes %v", res, f.mutations())
		}
	})
}

// --- fix -------------------------------------------------------------------

func TestFixAddsExactlyThePreviewedToggles(t *testing.T) {
	f, c := managerWorld(t, func(dsc map[string]any) {
		setPath(t, dsc, "spec.components.dashboard.standard", nil)
		setPath(t, dsc, "spec.components.databaseservice", nil)
	})
	plan, err := PlanDSCChange(c, "fix", "", "", false, false)
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
	if dry, real := patchesTo(f); len(dry)+len(real) != 0 {
		t.Fatal("patched a stale plan")
	}
	res, err = FixDSC(c, types.DSCPlanRequest{Name: "default-dsc", UID: plan.UID, Token: plan.Token})
	if err != nil || !res.Success {
		t.Fatalf("fix = %+v, %v", res, err)
	}
	stored := comps(f.get(dscV3Path))
	if s, _ := components.LookupState(stored, []string{"dashboard", "standard"}); s != "Managed" {
		t.Fatalf("dashboard.standard = %q", s)
	}
	if s, _ := components.LookupState(stored, []string{"databaseservice"}); s != "Removed" {
		t.Fatalf("databaseservice = %q", s)
	}
	if m, _ := GetDSCManager(c); len(m.Mismatches) != 0 {
		t.Fatalf("still mismatched: %+v", m.Mismatches)
	}
	// The DSC changed after the preview: refused.
	plan2, _ := PlanDSCChange(c, "fix", "", "", false, false)
	if plan2.Code != "nothing_to_do" {
		t.Fatalf("plan after fix = %+v", plan2)
	}
}

// --- soft reset --------------------------------------------------------------

func TestSoftResetChangesOnlyWhatDiffersAndKeepsProtectedFields(t *testing.T) {
	f, c := managerWorld(t, func(dsc map[string]any) {
		setPath(t, dsc, "spec.components.workbenches.workbenchNamespace", "my-notebooks")
		setPath(t, dsc, "spec.components.aiHub.instancesNamespace", "my-registries")
		setPath(t, dsc, "spec.components.kserve.rawDeploymentServiceConfig", "Headed")
	})
	plan, err := PlanDSCChange(c, "soft-reset", "", "", false, false)
	if err != nil || plan.Refusal != "" || len(plan.Blocked) != 0 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	var paths []string
	for _, ch := range plan.Changes {
		paths = append(paths, ch.Path+":"+ch.From+">"+ch.To+":"+ch.Effect)
	}
	want := []string{"aigateway:Managed>Removed:removes", "aigateway.modelsAsAService:Managed>Removed:removes", "kserve.rawDeploymentServiceConfig:Headed>Headless:"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("changes = %v, want %v", paths, want)
	}
	res, err := SoftResetDSC(c, types.DSCPlanRequest{Name: "default-dsc", UID: plan.UID, Token: plan.Token, KeepEnabled: new(false)})
	if err != nil || !res.Success {
		t.Fatalf("reset = %+v, %v", res, err)
	}
	_, real := patchesTo(f)
	body := real[0].Body
	for _, kept := range []string{"workbenchNamespace", "instancesNamespace", "defaultLocalQueueName", "airGapped", "mcpGuardrailsMode"} {
		if strings.Contains(body, kept) {
			t.Errorf("the reset touched %s: %s", kept, body)
		}
	}
	stored := comps(f.get(dscV3Path))
	if v, _ := components.LookupPath(stored, []string{"workbenches", "workbenchNamespace"}); v != "my-notebooks" {
		t.Fatalf("workbenchNamespace = %v", v)
	}
	if v, _ := components.LookupPath(stored, []string{"aiHub", "instancesNamespace"}); v != "my-registries" {
		t.Fatalf("instancesNamespace = %v", v)
	}
	if _, ok := components.LookupPath(stored, []string{"kserve", "rawDeploymentServiceConfig"}); ok {
		t.Fatal("rawDeploymentServiceConfig kept")
	}
	if a, _ := components.LookupState(stored, []string{"aigateway"}); a != "Removed" {
		t.Fatalf("aigateway = %s", a)
	}
}

func TestSoftResetRefusesWhileARemovalIsBlocked(t *testing.T) {
	f, c := managerWorld(t, nil)
	f.put(rhodsOperator, map[string]any{"status": map[string]any{"readyReplicas": 0}})
	plan, _ := PlanDSCChange(c, "soft-reset", "", "", false, false)
	if len(plan.Blocked) == 0 || plan.Blocked[0].Component != "aigateway" {
		t.Fatalf("plan = %+v", plan)
	}
	res, _ := SoftResetDSC(c, types.DSCPlanRequest{Name: "default-dsc", UID: plan.UID, Token: plan.Token, KeepEnabled: new(false)})
	if res.Success || res.ErrorCode != "prerequisites" {
		t.Fatalf("reset = %+v", res)
	}
	if dry, real := patchesTo(f); len(dry)+len(real) != 0 {
		t.Fatal("patched a blocked reset")
	}
}

// --- JSON patch builder -------------------------------------------------------

func TestBuildJSONPatchGroupsMissingObjects(t *testing.T) {
	obj := map[string]any{"spec": map[string]any{"components": map[string]any{"kserve": map[string]any{"managementState": "Managed"}}}}
	ops, err := components.BuildJSONPatch(obj, []components.DSCAssign{
		{Path: dsc.ComponentsPath("aigateway", "managementState"), Value: "Removed"},
		{Path: dsc.ComponentsPath("aigateway", "batchGateway", "managementState"), Value: "Removed"},
		{Path: dsc.ComponentsPath("kserve", "managementState"), Value: "Removed"},
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
	after, err := kube.ApplyJSONPatch(obj, ops)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := components.LookupState(comps(after), []string{"aigateway", "batchGateway"}); s != "Removed" {
		t.Fatalf("after = %v", after)
	}
	// The original is unchanged; a failed test applies nothing.
	if _, err := kube.ApplyJSONPatch(obj, []kube.JSONPatchOp{components.PatchOp("test", dsc.ComponentsPath("kserve", "managementState"), "Removed")}); err == nil {
		t.Fatal("test passed on another value")
	}
	if s, _ := components.LookupState(comps(obj), []string{"kserve"}); s != "Managed" {
		t.Fatal("applyJSONPatch changed its input")
	}
}

// --- RBAC ----------------------------------------------------------------------

// The hard reset deletes the DataScienceCluster with the signed-in user's
// own token, so neither the template nor the gitops profile grants the
// ServiceAccount delete on it (a leaked token cannot remove RHOAI's
// configuration).
func TestHardResetDeleteRule(t *testing.T) {
	has := func(path string) bool {
		for _, g := range templateGrantsFrom(t, path) {
			if slices.Contains(g.rule.APIGroups, rhoai.DSCGroup) && slices.Contains(g.rule.Resources, "datascienceclusters") && slices.Contains(g.rule.Verbs, "delete") {
				return true
			}
		}
		return false
	}
	if has(templateRel) {
		t.Fatal("deploy/template.yaml grants the ServiceAccount delete on datascienceclusters; the hard reset runs as the user")
	}
	if has(rbacGitOpsRel) {
		t.Fatal("deploy/rbac-gitops.yaml grants delete on datascienceclusters")
	}
}
