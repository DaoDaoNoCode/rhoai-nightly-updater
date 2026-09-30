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

const testDSCSample = `apiVersion: datasciencecluster.opendatahub.io/v2
kind: DataScienceCluster
metadata:
  name: default-dsc
spec:
  components:
    dashboard:
      managementState: Managed
    aipipelines:
      managementState: Managed
`

type dscSampleTransport func(*http.Request) (*http.Response, error)

func (f dscSampleTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mockDSCSamples(t *testing.T, fetch func(*http.Request) (int, string)) {
	t.Helper()
	t.Setenv("DSC_SAMPLE_REF", "")
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		status, body := fetch(r)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	dscDefaultsCache.Lock()
	dscDefaultsCache.entries = make(map[string]dscDefaultsCacheEntry)
	dscDefaultsCache.Unlock()
	t.Cleanup(func() {
		http.DefaultClient = old
		dscDefaultsCache.Lock()
		dscDefaultsCache.entries = make(map[string]dscDefaultsCacheEntry)
		dscDefaultsCache.Unlock()
	})
}

func csvListMock(version string) mockResponse {
	return mockResponse{body: `{"items":[{"metadata":{"name":"rhods-operator.test"},"spec":{"displayName":"Red Hat OpenShift AI","version":"` + version + `"}}]}`}
}

func TestDSCBranchForVersion(t *testing.T) {
	for version, want := range map[string]string{
		"3.6.0": "rhoai-3.6", "3.6.7": "rhoai-3.6", "3.3.0": "rhoai-3.3",
		"3.6.0-ea.2": "rhoai-3.6-ea.2", "3.6.0-EA2": "rhoai-3.6-ea.2",
		"rhoai-3.6-ea.2": "rhoai-3.6-ea.2", "3.6.0-ea.2-20260928": "rhoai-3.6-ea.2",
		"v3.6.0-rc.1": "rhoai-3.6-rc.1", "3.6.0+build.42": "rhoai-3.6",
		"3.7.0-ea": "rhoai-3.7-ea", "rhoai-3.7-ea": "rhoai-3.7-ea",
		"3.7.0-EA": "rhoai-3.7-ea", "3.7.0-ea+build.42": "rhoai-3.7-ea",
		"3.7.0-ea-20260930": "rhoai-3.7-ea", "3.7.0": "rhoai-3.7", "3.7.0-ga": "rhoai-3.7",
		"4.12.5-ea": "rhoai-4.12-ea", "4.12.5": "rhoai-4.12",
		"5.23.0-preview": "rhoai-5.23-preview", "5.23.0-preview.4": "rhoai-5.23-preview.4",
	} {
		t.Run(version, func(t *testing.T) {
			got, err := dscBranchForVersion(version)
			if err != nil || got != want {
				t.Fatalf("got %q, %v; want %q", got, err, want)
			}
		})
	}
	for _, version := range []string{"", "latest", "../../main", "3"} {
		if _, err := dscBranchForVersion(version); err == nil {
			t.Errorf("accepted %q", version)
		}
	}
}

func TestDefaultDSCFollowsUnnumberedEAAndRefOverride(t *testing.T) {
	var requestedPaths []string
	mockDSCSamples(t, func(r *http.Request) (int, string) {
		requestedPaths = append(requestedPaths, r.URL.EscapedPath())
		return 200, testDSCSample
	})
	c, cleanup := newMockClient(map[string]mockResponse{
		namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, ""): csvListMock("3.7.0-ea"),
	})
	defer cleanup()
	defaults, err := fetchDefaultDSCSpec(c)
	if err != nil || defaults.Branch != "rhoai-3.7-ea" || !strings.Contains(requestedPaths[0], "/rhoai-3.7-ea/") {
		t.Fatalf("defaults=%+v, error=%v, paths=%v", defaults, err, requestedPaths)
	}
	t.Setenv("DSC_SAMPLE_REF", "release/new-convention")
	defaults, err = fetchDefaultDSCSpec(c)
	if err != nil || defaults.Branch != "release/new-convention" || len(requestedPaths) != 2 || !strings.Contains(requestedPaths[1], "/release%2Fnew-convention/") {
		t.Fatalf("override defaults=%+v, error=%v, paths=%v", defaults, err, requestedPaths)
	}
}

func TestDefaultDSCUsesInstalledCSVAndBranchCache(t *testing.T) {
	var fetched []string
	mockDSCSamples(t, func(r *http.Request) (int, string) { fetched = append(fetched, r.URL.Path); return 200, testDSCSample })
	subPath := namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName)
	csvPath := namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "installed")
	responses := map[string]mockResponse{
		subPath: {body: `{"status":{"installedCSV":"installed"}}`},
		csvPath: {body: `{"spec":{"version":"3.6.0-ea.2"}}`},
	}
	c, cleanup := newMockClient(responses)
	defer cleanup()
	first, err := fetchDefaultDSCSpec(c)
	if err != nil || first.Branch != "rhoai-3.6-ea.2" {
		t.Fatalf("defaults: %+v, %v", first, err)
	}
	// Every caller gets a fresh parsed map; repairs must never mutate cached defaults.
	first.Spec["spec"].(map[string]interface{})["components"] = nil
	second, err := fetchDefaultDSCSpec(c)
	if err != nil || second.Spec["spec"].(map[string]interface{})["components"] == nil {
		t.Fatal("shared mutable defaults")
	}
	if len(fetched) != 1 {
		t.Fatalf("cache miss: %v", fetched)
	}
	older, olderCleanup := newMockClient(map[string]mockResponse{
		namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, ""): csvListMock("3.3.0"),
	})
	defer olderCleanup()
	third, err := fetchDefaultDSCSpec(older)
	if err != nil || third.Branch != "rhoai-3.3" || len(fetched) != 2 {
		t.Fatalf("upgrade reused wrong cache: %+v %v %v", third, err, fetched)
	}
	if !strings.Contains(fetched[0], "/rhoai-3.6-ea.2/") || !strings.Contains(fetched[1], "/rhoai-3.3/") {
		t.Fatal(fetched)
	}
}

func TestDefaultDSCVersionFallbackAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		sample  string
		wantErr bool
		v1      bool
	}{
		{"v1 fallback", 404, testDSCSample, false, true},
		{"upstream unavailable", 503, "unavailable", true, false},
		{"invalid YAML", 200, "{broken", true, false},
		{"invalid resource", 200, "kind: ConfigMap", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			mockDSCSamples(t, func(r *http.Request) (int, string) {
				calls++
				if tc.v1 && strings.Contains(r.URL.Path, "_v1_") {
					return 200, strings.ReplaceAll(testDSCSample, "/v2", "/v1")
				}
				return tc.status, tc.sample
			})
			c, cleanup := newMockClient(map[string]mockResponse{namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, ""): csvListMock("3.3.0")})
			defer cleanup()
			defaults, err := fetchDefaultDSCSpec(c)
			if (err != nil) != tc.wantErr {
				t.Fatalf("defaults=%+v error=%v", defaults, err)
			}
			if tc.v1 && (calls != 2 || defaults.Spec["apiVersion"] != "datasciencecluster.opendatahub.io/v1") {
				t.Fatalf("wrong fallback: %+v, calls %d", defaults, calls)
			}
		})
	}
}

func testDSCSchema() map[string]interface{} {
	state := map[string]interface{}{"type": "string"}
	component := map[string]interface{}{"type": "object", "properties": map[string]interface{}{
		"managementState":      state,
		"optionalValidSetting": state,
		"config":               map[string]interface{}{"type": "object", "additionalProperties": true},
	}}
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{
		"components": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"dashboard": component, "aipipelines": component}},
		"extensions": map[string]interface{}{"type": "object", "x-kubernetes-preserve-unknown-fields": true},
	}}
}

func testDSCCRD() string {
	body, _ := json.Marshal(map[string]interface{}{"spec": map[string]interface{}{"versions": []interface{}{map[string]interface{}{
		"name": "v2", "served": true, "schema": map[string]interface{}{"openAPIV3Schema": map[string]interface{}{"properties": map[string]interface{}{"spec": testDSCSchema()}}},
	}}}})
	return string(body)
}

func TestDSCCompatibilityChecksKeysAndPreservesOptionalFields(t *testing.T) {
	mockDSCSamples(t, func(*http.Request) (int, string) { return 200, testDSCSample })
	c, cleanup := newMockClient(map[string]mockResponse{
		"/apis/apiextensions.k8s.io/v1/customresourcedefinitions/datascienceclusters.datasciencecluster.opendatahub.io": {body: testDSCCRD()},
		namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, ""):                            csvListMock("3.6.0-ea.2"),
	})
	defer cleanup()
	var dsc map[string]interface{}
	json.Unmarshal([]byte(`{"apiVersion":"datasciencecluster.opendatahub.io/v2","spec":{"components":{"dashboard":{"managementState":"Removed","optionalValidSetting":"custom","oldField":"x","config":{"arbitrary":"value"}},"trainingoperator":{"managementState":"Removed"}},"extensions":{"custom":"keep"}}}`), &dsc)
	result := checkDSCCompatibility(c, dsc)
	if !reflect.DeepEqual(result.InvalidFields, []string{"spec.components.dashboard.oldField", "spec.components.trainingoperator"}) {
		t.Fatalf("invalid: %v", result.InvalidFields)
	}
	if !reflect.DeepEqual(result.MissingComponents, []string{"aipipelines"}) {
		t.Fatal(result.MissingComponents)
	}
	if result.ValidationError != "" || result.DefaultsError != "" {
		t.Fatalf("unexpected errors: %+v", result)
	}
}

func TestRepairDSCPrunesOrResetsWithExplicitDeletions(t *testing.T) {
	for _, mode := range []string{"remove-invalid", "reset-defaults"} {
		t.Run(mode, func(t *testing.T) {
			mockDSCSamples(t, func(*http.Request) (int, string) {
				if mode == "remove-invalid" {
					t.Error("removing invalid fields must work without fetching defaults")
				}
				return 200, testDSCSample
			})
			var patch map[string]interface{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "PATCH" && strings.Contains(r.URL.Path, "datascienceclusters/"):
					if r.Header.Get("Content-Type") != "application/merge-patch+json" {
						t.Error("wrong patch type")
					}
					json.NewDecoder(r.Body).Decode(&patch)
					io.WriteString(w, `{}`)
				case strings.Contains(r.URL.Path, "customresourcedefinitions/"):
					io.WriteString(w, testDSCCRD())
				case strings.Contains(r.URL.Path, "datascienceclusters/"):
					io.WriteString(w, `{"apiVersion":"datasciencecluster.opendatahub.io/v2","metadata":{"name":"my-dsc","resourceVersion":"42","labels":{"keep":"yes"}},"spec":{"components":{"dashboard":{"managementState":"Removed","optionalValidSetting":"custom","oldField":"x"},"trainingoperator":{"managementState":"Managed"}}}}`)
				case strings.HasSuffix(r.URL.Path, "/clusterserviceversions"):
					io.WriteString(w, csvListMock("3.6.0").body)
				default:
					io.WriteString(w, `{}`)
				}
			}))
			defer server.Close()
			c := &Client{baseURL: server.URL, httpClient: server.Client(), ctx: context.Background()}
			result, err := RepairDSC(c, "my-dsc", mode, "3.6.0")
			if err != nil || !result.Success {
				t.Fatalf("repair: %+v %v", result, err)
			}
			meta := patch["metadata"].(map[string]interface{})
			if !reflect.DeepEqual(meta, map[string]interface{}{"resourceVersion": "42"}) {
				t.Fatalf("metadata modified: %v", meta)
			}
			components := patch["spec"].(map[string]interface{})["components"].(map[string]interface{})
			if v, exists := components["trainingoperator"]; !exists || v != nil {
				t.Fatal("deprecated component was not explicitly deleted")
			}
			dashboard := components["dashboard"].(map[string]interface{})
			if v, exists := dashboard["oldField"]; !exists || v != nil {
				t.Fatal("deprecated nested field was not deleted")
			}
			if mode == "remove-invalid" {
				if dashboard["managementState"] != "Removed" || dashboard["optionalValidSetting"] != "custom" {
					t.Fatal("valid settings were changed")
				}
				if _, added := components["aipipelines"]; added {
					t.Fatal("prune added components")
				}
			} else {
				if dashboard["managementState"] != "Managed" || dashboard["optionalValidSetting"] != nil || components["aipipelines"] == nil {
					t.Fatal("defaults not applied")
				}
			}
		})
	}
}

func TestRepairDSCRefusesUnavailableSchemaOrChangedVersion(t *testing.T) {
	for _, tc := range []struct {
		name            string
		schemaStatus    int
		expectedVersion string
	}{
		{"schema unavailable", 503, "3.6.0"},
		{"operator changed since preview", 200, "3.5.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mockDSCSamples(t, func(*http.Request) (int, string) { return 200, testDSCSample })
			c, records, cleanup := newRecordingMockClient(map[string]mockResponse{
				"/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/my-dsc":                                         {body: `{"apiVersion":"datasciencecluster.opendatahub.io/v2","metadata":{"name":"my-dsc","resourceVersion":"42"},"spec":{"components":{"dashboard":{"managementState":"Removed"}}}}`},
				"/apis/apiextensions.k8s.io/v1/customresourcedefinitions/datascienceclusters.datasciencecluster.opendatahub.io": {body: testDSCCRD(), statusCode: tc.schemaStatus},
				namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, ""):                            csvListMock("3.6.0"),
			})
			defer cleanup()
			if _, err := RepairDSC(c, "my-dsc", "reset-defaults", tc.expectedVersion); err == nil {
				t.Fatal("unsafe reset accepted")
			}
			for _, rec := range *records {
				if rec.Method != "GET" {
					t.Fatalf("mutated despite failed validation: %+v", rec)
				}
			}
		})
	}
}

func TestCreateDSCUpstreamFailureDoesNotApplyBuiltinDefaults(t *testing.T) {
	mockDSCSamples(t, func(*http.Request) (int, string) { return 503, "offline" })
	c, records, cleanup := newRecordingMockClient(map[string]mockResponse{
		"/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters":                     {body: `{"items":[]}`},
		namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, ""): csvListMock("3.6.0"),
	})
	defer cleanup()
	result, err := CreateDefaultDSC(c)
	if err != nil || result.Success || !strings.Contains(result.Message, "503") {
		t.Fatalf("got %+v %v", result, err)
	}
	for _, request := range *records {
		if request.Method != "GET" {
			t.Fatalf("mutated cluster after failed fetch: %+v", request)
		}
	}
}
