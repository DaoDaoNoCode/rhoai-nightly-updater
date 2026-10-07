package cluster

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func specOf(t *testing.T, components string) map[string]interface{} {
	t.Helper()
	var spec map[string]interface{}
	if err := json.Unmarshal([]byte(`{"components":`+components+`}`), &spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

// The shapes of rhods-operator rhoai-3.6
// config/rhoai/samples/datasciencecluster_v3_datasciencecluster.yaml: pure
// groups (dashboard, data), components with their own state and parts
// (kserve, aigateway, workbenches), and non-state fields (aiHub).
const v3Components = `{
	"aiHub":{"managementState":"Managed","instancesNamespace":"rhoai-model-registries"},
	"aigateway":{"managementState":"Managed","modelsAsAService":{"managementState":"Managed"},"batchGateway":{"managementState":"Removed"}},
	"dashboard":{"standard":{"managementState":"Managed"},"maasPortal":{"managementState":"Removed"}},
	"data":{"featureStore":{"managementState":"Managed"}},
	"kserve":{"managementState":"Managed","nim":{"managementState":"Managed"},"wva":{"managementState":"Removed"}},
	"trainer":{"managementState":"Removed"},
	"workbenches":{"managementState":"Managed","workbenchesV2":{"managementState":"Removed"}}}`

func TestRemovedComponents_V3ManagementPaths(t *testing.T) {
	old := specOf(t, v3Components)
	for _, tc := range []struct {
		name string
		next string
		want []string
	}{
		{"unchanged", v3Components, nil},
		{"a part switched off: dashboard.standard", strings.Replace(v3Components, `"standard":{"managementState":"Managed"}`, `"standard":{"managementState":"Removed"}`, 1), []string{"dashboard.standard"}},
		{"a component switched off removes its enabled parts", strings.Replace(v3Components, `"kserve":{"managementState":"Managed"`, `"kserve":{"managementState":"Removed"`, 1), []string{"kserve", "kserve.nim"}},
		{"a group dropped", strings.Replace(v3Components, `"data":{"featureStore":{"managementState":"Managed"}},`, ``, 1), []string{"data.featureStore"}},
		{"a part dropped", strings.Replace(v3Components, `,"modelsAsAService":{"managementState":"Managed"}`, ``, 1), []string{"aigateway.modelsAsAService"}},
		{"a part switched on is no removal", strings.Replace(v3Components, `"wva":{"managementState":"Removed"}`, `"wva":{"managementState":"Managed"}`, 1), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := removedComponents(old, specOf(t, tc.next), true); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	// A part of a component that is already Removed is not enabled.
	off := specOf(t, `{"kserve":{"managementState":"Removed","nim":{"managementState":"Managed"}}}`)
	if got := removedComponents(off, specOf(t, `{}`), true); got != nil {
		t.Fatalf("removed component's part counted: %v", got)
	}
}

func TestRemovedComponents_V2Unchanged(t *testing.T) {
	old := specOf(t, `{"kserve":{"managementState":"Managed","nim":{"managementState":"Managed"}},"ray":{"managementState":"Managed"},"kueue":{"managementState":"Removed"},"modelregistry":{}}`)
	for _, tc := range []struct {
		next string
		want []string
	}{
		{`{"kserve":{"managementState":"Managed","nim":{"managementState":"Removed"}},"ray":{"managementState":"Managed"},"kueue":{"managementState":"Removed"},"modelregistry":{}}`, nil},
		{`{"kserve":{"managementState":"Removed","nim":{"managementState":"Managed"}},"ray":{"managementState":"Managed"},"modelregistry":{}}`, []string{"kserve"}},
		{`{"kserve":{"managementState":"Managed"}}`, []string{"modelregistry", "ray"}},
	} {
		if got := removedComponents(old, specOf(t, tc.next), dscNestsComponents("datasciencecluster.opendatahub.io/v2")); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("next %s: got %v, want %v", tc.next, got, tc.want)
		}
	}
	for apiVersion, want := range map[string]bool{
		"datasciencecluster.opendatahub.io/v1": false, "datasciencecluster.opendatahub.io/v2": false,
		"datasciencecluster.opendatahub.io/v3": true, "datasciencecluster.opendatahub.io/v4": true, "other.io/v3": false, "": false,
	} {
		if got := dscNestsComponents(apiVersion); got != want {
			t.Errorf("dscNestsComponents(%q) = %v", apiVersion, got)
		}
	}
}

func TestGuardModule(t *testing.T) {
	for path, want := range map[string]string{
		"dashboard.standard": "dashboard", "dashboard.maasPortal": "dashboard", "kserve.nim": "kserve", "kserve": "kserve",
		"data.featureStore": "feastoperator", "aiHub": "aihub", "workbenches.workbenchesV2": "workbenches", "trainer": "trainer",
	} {
		if got := guardModule(path); got != want {
			t.Errorf("guardModule(%q) = %q, want %q", path, got, want)
		}
	}
}

// v3RepairWorld serves a v3 DataScienceCluster (v3Components) named
// my-dsc, its schema, a CSV whose alm-examples hold the given v3 defaults,
// and the removal preconditions: rhods-operator ready, the Kserve module
// CR with a finalizer whose operator has no Deployment.
func v3RepairWorld(t *testing.T, defaults string) (*fakeAPI, *Client) {
	t.Helper()
	apiVersionsCache.Purge()
	t.Cleanup(apiVersionsCache.Purge)
	f, c := newFakeAPI(t)
	f.json("GET", "/apis/datasciencecluster.opendatahub.io", 200, dscGroupDoc)
	f.json("GET", "/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters/my-dsc", 200,
		`{"apiVersion":"datasciencecluster.opendatahub.io/v3","kind":"DataScienceCluster","metadata":{"name":"my-dsc","resourceVersion":"42"},"spec":{"components":`+v3Components+`}}`)
	open := map[string]interface{}{"type": "object", "x-kubernetes-preserve-unknown-fields": true}
	props := map[string]interface{}{}
	for _, k := range []string{"aiHub", "aigateway", "dashboard", "data", "kserve", "trainer", "workbenches"} {
		props[k] = open
	}
	f.obj("GET", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/datascienceclusters.datasciencecluster.opendatahub.io", map[string]interface{}{
		"spec": map[string]interface{}{"versions": []interface{}{map[string]interface{}{"name": "v3", "served": true, "schema": map[string]interface{}{"openAPIV3Schema": map[string]interface{}{
			"properties": map[string]interface{}{"spec": map[string]interface{}{"type": "object", "properties": map[string]interface{}{
				"components": map[string]interface{}{"type": "object", "properties": props}}}}}}}}},
	})
	alm, _ := json.Marshal([]interface{}{map[string]interface{}{
		"apiVersion": "datasciencecluster.opendatahub.io/v3", "kind": "DataScienceCluster", "metadata": map[string]interface{}{"name": "default-dsc"},
		"spec": json.RawMessage(`{"components":` + defaults + `}`),
	}})
	f.json("GET", namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName), 200, `{"status":{"installedCSV":"rhods-operator.3.6.0"}}`)
	f.json("GET", namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "rhods-operator.3.6.0"), 200, installedCSVBody(string(alm)))
	f.json("GET", namespacedPath("apps/v1", "deployments", SubNS, "rhods-operator"), 200, `{"status":{"readyReplicas":1}}`)
	f.json("GET", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", 200, `{"items":[]}`)
	serveComponentGroup(f)
	f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1", 200,
		`{"resources":[{"name":"kserves","kind":"Kserve","verbs":["list"]},{"name":"dashboards","kind":"Dashboard","verbs":["list"]}]}`)
	f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1/kserves", 200,
		`{"items":[{"metadata":{"name":"default-kserve","finalizers":["components.platform.opendatahub.io/cleanup"]}}]}`)
	f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1/dashboards", 200, `{"items":[]}`)
	return f, c
}

func TestRepairDSC_V3PartRemovalsArePreviewedAndGuarded(t *testing.T) {
	mockDSCSamples(t, func(*http.Request) (int, string) {
		t.Error("GitHub must not be used: alm-examples has v3")
		return 500, ""
	})
	for _, tc := range []struct {
		name, defaults, wantPath, wantReason string
	}{
		{"dashboard.standard Managed to Removed",
			strings.Replace(v3Components, `"standard":{"managementState":"Managed"}`, `"standard":{"managementState":"Removed"}`, 1),
			"dashboard.standard", "Removing the dashboard deletes the Dashboard CR"},
		{"kserve off with kserve.nim enabled",
			strings.Replace(v3Components, `"kserve":{"managementState":"Managed"`, `"kserve":{"managementState":"Removed"`, 1),
			"kserve.nim", "kserve-module-controller-manager"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := v3RepairWorld(t, tc.defaults)

			// The Components page preview lists the path and why it is blocked.
			var dsc map[string]interface{}
			_ = json.Unmarshal([]byte(`{"apiVersion":"datasciencecluster.opendatahub.io/v3","spec":{"components":`+v3Components+`}}`), &dsc)
			compat := checkDSCCompatibility(c, dsc)
			if !containsString(compat.ResetRemovals, tc.wantPath) {
				t.Fatalf("resetRemovals %v, compat %+v", compat.ResetRemovals, compat)
			}
			blocked := false
			for _, b := range compat.RemovalBlocks {
				if b.Component == tc.wantPath && strings.Contains(strings.Join(b.Reasons, " "), tc.wantReason) {
					blocked = true
				}
			}
			if !blocked {
				t.Fatalf("removalBlocks %+v", compat.RemovalBlocks)
			}

			// The repair itself refuses, changing nothing.
			r, err := RepairDSC(c, "my-dsc", "reset-defaults", "3.6.0", "datasciencecluster.opendatahub.io/v3", nil, compat.ResetRemovals)
			if err != nil || r.Success || r.ErrorCode != "prerequisites" || !strings.Contains(r.Message, tc.wantPath) || !strings.Contains(r.Message, tc.wantReason) {
				t.Fatalf("repair %+v %v", r, err)
			}
			assertWrites(t, f)
		})
	}
}

func TestRepairDSC_BoundToThePreviewedAPIVersion(t *testing.T) {
	mockDSCSamples(t, func(*http.Request) (int, string) {
		t.Error("GitHub must not be used: alm-examples has v3")
		return 500, ""
	})
	t.Run("same version: the repair runs", func(t *testing.T) {
		f, c := v3RepairWorld(t, v3Components)
		r, err := RepairDSC(c, "my-dsc", "remove-invalid", "", "datasciencecluster.opendatahub.io/v3", nil, nil)
		if err != nil || !r.Success || r.Message != "No invalid DSC fields to remove" {
			t.Fatalf("%+v %v", r, err)
		}
		assertWrites(t, f)
	})
	t.Run("previewed v2, v3 reads again now (operator updated)", func(t *testing.T) {
		f, c := v3RepairWorld(t, v3Components)
		_, err := RepairDSC(c, "my-dsc", "reset-defaults", "3.6.0", "datasciencecluster.opendatahub.io/v2", nil, []string{})
		if err == nil || err.Error() != "the DataScienceCluster's API version changed since the preview (previewed v2, now v3); refresh and review the changes again. Nothing was changed" {
			t.Fatalf("err %v", err)
		}
		assertWrites(t, f)
	})
	t.Run("previewed v3, v3 stops converting: no fallback to v2", func(t *testing.T) {
		f, c := v3RepairWorld(t, v3Components)
		f.json("GET", "/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters/my-dsc", 500, liveDSCv3Conversion500)
		f.json("GET", "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/my-dsc", 200,
			`{"apiVersion":"datasciencecluster.opendatahub.io/v2","metadata":{"name":"my-dsc","resourceVersion":"42"},"spec":{"components":{}}}`)
		_, err := RepairDSC(c, "my-dsc", "reset-defaults", "3.6.0", "datasciencecluster.opendatahub.io/v3", nil, []string{})
		if err == nil || !strings.Contains(err.Error(), "(previewed v3, now v2)") {
			t.Fatalf("err %v", err)
		}
		assertWrites(t, f)
	})
	t.Run("no version converts", func(t *testing.T) {
		f, c := v3RepairWorld(t, v3Components)
		f.json("GET", "/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters/my-dsc", 500, liveDSCv3Conversion500)
		f.json("GET", "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/my-dsc", 500, liveDSCv3Conversion500)
		_, err := RepairDSC(c, "my-dsc", "remove-invalid", "", "datasciencecluster.opendatahub.io/v3", nil, nil)
		if err == nil || !strings.Contains(err.Error(), "now not readable") {
			t.Fatalf("err %v", err)
		}
		assertWrites(t, f)
	})
	t.Run("an old page that does not send the version is asked to refresh", func(t *testing.T) {
		f, c := v3RepairWorld(t, v3Components)
		for _, v := range []string{"", "v3", "other.io/v3"} {
			_, err := RepairDSC(c, "my-dsc", "remove-invalid", "", v, nil, nil)
			if err == nil || !strings.Contains(err.Error(), "refresh and review the changes again") {
				t.Fatalf("%q: err %v", v, err)
			}
		}
		if f.requestCount() != 0 {
			t.Fatalf("read the cluster: %d requests", f.requestCount())
		}
	})
}

const myDSCv3Path = "/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters/my-dsc"

// A reset carries the removals its preview listed and runs only when it
// would remove exactly those.
func TestRepairDSC_ResetBoundToThePreviewedRemovals(t *testing.T) {
	mockDSCSamples(t, func(*http.Request) (int, string) { t.Error("GitHub must not be used"); return 500, "" })
	// The defaults switch trainer on and nothing off.
	enableTrainer := strings.Replace(v3Components, `"trainer":{"managementState":"Removed"}`, `"trainer":{"managementState":"Managed"}`, 1)
	t.Run("the same removals: the reset runs", func(t *testing.T) {
		f, c := v3RepairWorld(t, enableTrainer)
		f.json("PATCH", myDSCv3Path, 200, `{}`)
		r, err := RepairDSC(c, "my-dsc", "reset-defaults", "3.6.0", "datasciencecluster.opendatahub.io/v3", nil, []string{})
		if err != nil || !r.Success {
			t.Fatalf("%+v %v", r, err)
		}
		assertWrites(t, f, "PATCH "+myDSCv3Path)
	})
	t.Run("the preview listed other removals: refused, nothing changed", func(t *testing.T) {
		f, c := v3RepairWorld(t, enableTrainer)
		_, err := RepairDSC(c, "my-dsc", "reset-defaults", "3.6.0", "datasciencecluster.opendatahub.io/v3", nil, []string{"ray"})
		if err == nil || err.Error() != "the components a reset would switch to Removed changed since the preview (previewed: ray; now: none); refresh and review the changes again. Nothing was changed" {
			t.Fatalf("err %v", err)
		}
		assertWrites(t, f)
	})
	t.Run("the reset now removes a part the preview did not list", func(t *testing.T) {
		f, c := v3RepairWorld(t, strings.Replace(v3Components, `"standard":{"managementState":"Managed"}`, `"standard":{"managementState":"Removed"}`, 1))
		_, err := RepairDSC(c, "my-dsc", "reset-defaults", "3.6.0", "datasciencecluster.opendatahub.io/v3", nil, []string{})
		if err == nil || !strings.Contains(err.Error(), "(previewed: none; now: dashboard.standard)") {
			t.Fatalf("err %v", err)
		}
		assertWrites(t, f)
	})
	t.Run("order does not matter", func(t *testing.T) {
		f, c := v3RepairWorld(t, strings.Replace(v3Components, `"kserve":{"managementState":"Managed"`, `"kserve":{"managementState":"Removed"`, 1))
		r, err := RepairDSC(c, "my-dsc", "reset-defaults", "3.6.0", "datasciencecluster.opendatahub.io/v3", nil, []string{"kserve.nim", "kserve"})
		// The removals match; the deletion guard refuses (the Kserve operator is down).
		if err != nil || r.ErrorCode != "prerequisites" {
			t.Fatalf("%+v %v", r, err)
		}
		assertWrites(t, f)
	})
	t.Run("an old page that does not send the removals is asked to refresh", func(t *testing.T) {
		f, c := v3RepairWorld(t, enableTrainer)
		_, err := RepairDSC(c, "my-dsc", "reset-defaults", "3.6.0", "datasciencecluster.opendatahub.io/v3", nil, nil)
		if err == nil || !strings.Contains(err.Error(), "did not say which components the reset would switch to Removed") || f.requestCount() != 0 {
			t.Fatalf("err %v, %d requests", err, f.requestCount())
		}
	})
}

// strictDashboardSchema serves a schema in which dashboard defines only
// its parts standard and maasPortal (not free-form), and a DSC whose
// dashboard also has a part the schema does not know.
func strictDashboardSchema(f *fakeAPI) {
	state := map[string]interface{}{"type": "object", "properties": map[string]interface{}{"managementState": map[string]interface{}{"type": "string"}}}
	open := map[string]interface{}{"type": "object", "x-kubernetes-preserve-unknown-fields": true}
	props := map[string]interface{}{
		"dashboard": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"standard": state, "maasPortal": state}},
	}
	for _, k := range []string{"aiHub", "aigateway", "data", "kserve", "trainer", "workbenches"} {
		props[k] = open
	}
	f.obj("GET", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/datascienceclusters.datasciencecluster.opendatahub.io", map[string]interface{}{
		"spec": map[string]interface{}{"versions": []interface{}{map[string]interface{}{"name": "v3", "served": true, "schema": map[string]interface{}{"openAPIV3Schema": map[string]interface{}{
			"properties": map[string]interface{}{"spec": map[string]interface{}{"type": "object", "properties": map[string]interface{}{
				"components": map[string]interface{}{"type": "object", "properties": props}}}}}}}}},
	})
	withLegacy := strings.Replace(v3Components, `"dashboard":{`, `"dashboard":{"legacyPart":{"managementState":"Managed"},`, 1)
	f.json("GET", myDSCv3Path, 200,
		`{"apiVersion":"datasciencecluster.opendatahub.io/v3","kind":"DataScienceCluster","metadata":{"name":"my-dsc","resourceVersion":"42"},"spec":{"components":`+withLegacy+`}}`)
}

func TestSchemaComponents_ChecksEveryPathSegment(t *testing.T) {
	state := map[string]interface{}{"type": "object"}
	schema := map[string]interface{}{"properties": map[string]interface{}{"components": map[string]interface{}{"properties": map[string]interface{}{
		"dashboard": map[string]interface{}{"properties": map[string]interface{}{"standard": state}},
		"kserve":    map[string]interface{}{"x-kubernetes-preserve-unknown-fields": true},
		"aigateway": map[string]interface{}{"additionalProperties": map[string]interface{}{"type": "object"}},
		"data":      map[string]interface{}{"additionalProperties": false},
		"trainer":   state,
	}}}}
	got := schemaComponents(schema, []string{"aigateway.anything", "dashboard", "dashboard.legacyPart", "dashboard.standard", "data.featureStore", "kserve.nim", "trainer", "unknown", "unknown.part"})
	want := []string{"aigateway.anything", "dashboard", "dashboard.standard", "kserve.nim", "trainer"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestRepairDSC_UnknownPartIsPrunedWithoutAGuard(t *testing.T) {
	mockDSCSamples(t, func(*http.Request) (int, string) { t.Error("GitHub must not be used"); return 500, "" })
	t.Run("remove-invalid drops dashboard.legacyPart without the dashboard guard", func(t *testing.T) {
		f, c := v3RepairWorld(t, v3Components)
		strictDashboardSchema(f)
		f.json("PATCH", myDSCv3Path, 200, `{}`)
		r, err := RepairDSC(c, "my-dsc", "remove-invalid", "", "datasciencecluster.opendatahub.io/v3", nil, nil)
		if err != nil || !r.Success || !strings.Contains(r.Message, "spec.components.dashboard.legacyPart") || strings.Contains(r.Message, "set to Removed") {
			t.Fatalf("%+v %v", r, err)
		}
		assertWrites(t, f, "PATCH "+myDSCv3Path)
	})
	t.Run("a known part is still guarded", func(t *testing.T) {
		f, c := v3RepairWorld(t, strings.Replace(v3Components, `"standard":{"managementState":"Managed"}`, `"standard":{"managementState":"Removed"}`, 1))
		strictDashboardSchema(f)
		r, err := RepairDSC(c, "my-dsc", "reset-defaults", "3.6.0", "datasciencecluster.opendatahub.io/v3", nil, []string{"dashboard.legacyPart", "dashboard.standard"})
		if err == nil {
			t.Fatalf("legacyPart counted as a removal: %+v", r)
		}
		r, err = RepairDSC(c, "my-dsc", "reset-defaults", "3.6.0", "datasciencecluster.opendatahub.io/v3", nil, []string{"dashboard.standard"})
		if err != nil || r.ErrorCode != "prerequisites" || !strings.Contains(r.Message, "dashboard.standard") || strings.Contains(r.Message, "legacyPart") {
			t.Fatalf("%+v %v", r, err)
		}
		assertWrites(t, f)
	})
}
