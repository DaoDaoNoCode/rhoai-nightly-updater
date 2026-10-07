package cluster

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// Model an upgrade whose CRD still accepts component names absent from its sample.
func dscCRDWithLegacyComponents(t *testing.T) string {
	t.Helper()
	var crd map[string]interface{}
	if err := json.Unmarshal([]byte(testDSCCRD()), &crd); err != nil {
		t.Fatal(err)
	}
	version := crd["spec"].(map[string]interface{})["versions"].([]interface{})[0].(map[string]interface{})
	root := version["schema"].(map[string]interface{})["openAPIV3Schema"].(map[string]interface{})
	spec := root["properties"].(map[string]interface{})["spec"].(map[string]interface{})
	components := spec["properties"].(map[string]interface{})["components"].(map[string]interface{})["properties"].(map[string]interface{})
	components["llamastackoperator"] = components["dashboard"]
	components["trainingoperator"] = components["dashboard"]
	body, err := json.Marshal(crd)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestDSCCompatibilityReportsCRDAcceptedExtraComponents(t *testing.T) {
	mockDSCSamples(t, func(*http.Request) (int, string) { return 200, testDSCSample })
	c, cleanup := newMockClient(map[string]mockResponse{
		"/apis/apiextensions.k8s.io/v1/customresourcedefinitions/datascienceclusters.datasciencecluster.opendatahub.io": {body: dscCRDWithLegacyComponents(t)},
		namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, ""):                            csvListMock("3.6.0"),
	})
	defer cleanup()
	var dsc map[string]interface{}
	if err := json.Unmarshal([]byte(`{"apiVersion":"datasciencecluster.opendatahub.io/v2","spec":{"components":{"dashboard":{"managementState":"Removed","optionalValidSetting":"custom"},"trainingoperator":{"managementState":"Removed"},"llamastackoperator":{"managementState":"Managed"}}}}`), &dsc); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(dsc)
	result := checkDSCCompatibility(c, dsc)
	if result.ValidationError != "" || result.DefaultsError != "" || len(result.InvalidFields) != 0 {
		t.Fatalf("schema-valid extras were classified as invalid: %+v", result)
	}
	if !reflect.DeepEqual(result.ExtraComponents, []string{"llamastackoperator", "trainingoperator"}) || !reflect.DeepEqual(result.MissingComponents, []string{"aipipelines"}) {
		t.Fatalf("component comparison is not bidirectional: %+v", result)
	}
	after, _ := json.Marshal(dsc)
	if string(before) != string(after) {
		t.Fatal("compatibility inspection modified the DSC")
	}
}

func TestRepairDSCRemoveExtraComponentsPreservesRemainingSettings(t *testing.T) {
	for _, tc := range []struct {
		name      string
		version   string
		expected  []string
		defaults  int
		schema    int
		rv        string
		clean     bool
		wantError bool
		wantPatch bool
	}{
		{name: "remove reviewed extras", version: "3.6.0", expected: []string{"trainingoperator", "llamastackoperator"}, defaults: 200, schema: 200, rv: "42", wantPatch: true},
		{name: "operator changed", version: "3.5.0", expected: []string{"trainingoperator", "llamastackoperator"}, defaults: 200, schema: 200, rv: "42", wantError: true},
		{name: "extra list changed", version: "3.6.0", expected: []string{"trainingoperator"}, defaults: 200, schema: 200, rv: "42", wantError: true},
		{name: "extra names changed", version: "3.6.0", expected: []string{"trainingoperator", "differentoperator"}, defaults: 200, schema: 200, rv: "42", wantError: true},
		{name: "unreviewed extras", version: "3.6.0", defaults: 200, schema: 200, rv: "42", wantError: true},
		{name: "defaults unavailable", version: "3.6.0", expected: []string{"trainingoperator", "llamastackoperator"}, defaults: 503, schema: 200, rv: "42", wantError: true},
		{name: "schema unavailable", version: "3.6.0", expected: []string{"trainingoperator", "llamastackoperator"}, defaults: 200, schema: 503, rv: "42", wantError: true},
		{name: "resource version missing", version: "3.6.0", expected: []string{"trainingoperator", "llamastackoperator"}, defaults: 200, schema: 200, wantError: true},
		{name: "already clean", version: "3.6.0", defaults: 200, schema: 200, rv: "42", clean: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mockDSCSamples(t, func(*http.Request) (int, string) { return tc.defaults, testDSCSample })
			var dsc map[string]interface{}
			json.Unmarshal([]byte(`{"apiVersion":"datasciencecluster.opendatahub.io/v2","metadata":{"name":"my-dsc","labels":{"keep":"yes"}},"spec":{"components":{"dashboard":{"managementState":"Removed","optionalValidSetting":"custom","oldField":"preserve","config":{"nested":{"keep":true}}},"trainingoperator":{"managementState":"Managed"},"llamastackoperator":{"managementState":"Removed"}},"extensions":{"custom":"keep"}}}`), &dsc)
			dsc["metadata"].(map[string]interface{})["resourceVersion"] = tc.rv
			if tc.clean {
				components := dsc["spec"].(map[string]interface{})["components"].(map[string]interface{})
				delete(components, "trainingoperator")
				delete(components, "llamastackoperator")
			}
			body, _ := json.Marshal(dsc)
			crd := dscCRDWithLegacyComponents(t)
			var patch map[string]interface{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "PATCH" && strings.Contains(r.URL.Path, "datascienceclusters/"):
					if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
						t.Error(err)
					}
					io.WriteString(w, `{}`)
				case strings.Contains(r.URL.Path, "datascienceclusters/"):
					w.Write(body)
				case strings.Contains(r.URL.Path, "customresourcedefinitions/"):
					w.WriteHeader(tc.schema)
					io.WriteString(w, crd)
				case strings.HasSuffix(r.URL.Path, "/clusterserviceversions"):
					io.WriteString(w, csvListMock("3.6.0").body)
				case repairGuardMock(w, r):
				default:
					io.WriteString(w, `{}`)
				}
			}))
			defer server.Close()
			c := &Client{baseURL: server.URL, httpClient: server.Client(), ctx: context.Background()}
			result, err := RepairDSC(c, "my-dsc", "remove-extra-components", tc.version, "datasciencecluster.opendatahub.io/v2", tc.expected)
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected result: %+v, %v", result, err)
			}
			if !tc.wantError && (result == nil || !result.Success) {
				t.Fatalf("repair failed: %+v", result)
			}
			if (patch != nil) != tc.wantPatch {
				t.Fatalf("unexpected mutation: %v", patch)
			}
			if !tc.wantPatch {
				return
			}
			if !reflect.DeepEqual(patch["metadata"], map[string]interface{}{"resourceVersion": "42"}) {
				t.Fatalf("metadata changed or concurrency guard missing: %v", patch["metadata"])
			}
			spec := patch["spec"].(map[string]interface{})
			components := spec["components"].(map[string]interface{})
			for _, name := range []string{"llamastackoperator", "trainingoperator"} {
				if value, exists := components[name]; !exists || value != nil {
					t.Fatalf("extra component %s was not explicitly deleted", name)
				}
			}
			original := dsc["spec"].(map[string]interface{})
			if !reflect.DeepEqual(components["dashboard"], original["components"].(map[string]interface{})["dashboard"]) || !reflect.DeepEqual(spec["extensions"], original["extensions"]) {
				t.Fatal("targeted removal changed remaining settings")
			}
			if _, added := components["aipipelines"]; added {
				t.Fatal("targeted removal added a missing component")
			}
		})
	}
}
