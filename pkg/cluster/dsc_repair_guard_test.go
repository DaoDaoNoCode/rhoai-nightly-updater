package cluster

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// repairGuardMock answers the removal preconditions with a healthy
// cluster: rhods-operator ready, no component API (no module CRs) and no
// conversion CRDs. It returns false for other requests.
func repairGuardMock(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case strings.HasSuffix(r.URL.Path, "/deployments/rhods-operator"):
		_, _ = io.WriteString(w, `{"status":{"readyReplicas":1}}`)
	case r.URL.Path == "/apis/components.platform.opendatahub.io":
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"kind":"Status","code":404}`)
	case r.URL.Path == "/apis/apiextensions.k8s.io/v1/customresourcedefinitions":
		_, _ = io.WriteString(w, `{"items":[]}`)
	default:
		return false
	}
	return true
}

// dscCRDWith returns the test DSC CRD with extra component keys in its
// schema.
func dscCRDWith(t *testing.T, names ...string) string {
	t.Helper()
	var crd map[string]interface{}
	if err := json.Unmarshal([]byte(testDSCCRD()), &crd); err != nil {
		t.Fatal(err)
	}
	version := crd["spec"].(map[string]interface{})["versions"].([]interface{})[0].(map[string]interface{})
	root := version["schema"].(map[string]interface{})["openAPIV3Schema"].(map[string]interface{})
	spec := root["properties"].(map[string]interface{})["spec"].(map[string]interface{})
	components := spec["properties"].(map[string]interface{})["components"].(map[string]interface{})["properties"].(map[string]interface{})
	for _, n := range names {
		components[n] = components["dashboard"]
	}
	body, _ := json.Marshal(crd)
	return string(body)
}

// The live D3 shape: mcpservers.mcp.x-k8s.io converts through
// mcp-lifecycle-operator-webhook-service. A DSC that has
// mcplifecycleoperator enabled (the fix for the live D3) must not lose it to
// a reset or an extra-component removal: the Service would go away again,
// the CRD would not (R5-F1).
func dscRepairD3Server(t *testing.T, dsc string, patched *bool, mu *sync.Mutex) *Client {
	crd := dscCRDWith(t, "mcplifecycleoperator", "trainingoperator")
	conv := `{"items":[{"metadata":{"name":"mcpservers.mcp.x-k8s.io"},"spec":{"conversion":{"strategy":"Webhook","webhook":{"clientConfig":{"service":{"namespace":"redhat-ods-applications","name":"mcp-lifecycle-operator-webhook-service"}}}}},"status":{"storedVersions":["v1alpha1","v1beta1"]}}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch:
			mu.Lock()
			*patched = true
			mu.Unlock()
			_, _ = io.WriteString(w, `{}`)
		case r.URL.Path == "/apis/apiextensions.k8s.io/v1/customresourcedefinitions":
			_, _ = io.WriteString(w, conv)
		case strings.Contains(r.URL.Path, "customresourcedefinitions/"):
			_, _ = io.WriteString(w, crd)
		case strings.Contains(r.URL.Path, "datascienceclusters/"):
			_, _ = io.WriteString(w, dsc)
		case strings.HasSuffix(r.URL.Path, "/clusterserviceversions"):
			_, _ = io.WriteString(w, csvListMock("3.6.0").body)
		case strings.Contains(r.URL.Path, "/services/"):
			w.WriteHeader(http.StatusNotFound) // already gone, as live
			_, _ = io.WriteString(w, `{"kind":"Status","code":404}`)
		case repairGuardMock(w, r):
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(server.Close)
	return &Client{baseURL: server.URL, httpClient: server.Client(), ctx: context.Background()}
}

func TestRepairDSC_RefusesRemovalsThatFailPreconditions(t *testing.T) {
	dsc := `{"apiVersion":"datasciencecluster.opendatahub.io/v2","metadata":{"name":"my-dsc","resourceVersion":"42"},"spec":{"components":{"dashboard":{"managementState":"Managed"},"aipipelines":{"managementState":"Managed"},"mcplifecycleoperator":{"managementState":"Managed"}}}}`
	for _, tc := range []struct {
		mode     string
		expected []string
	}{
		{"reset-defaults", nil},
		{"remove-extra-components", []string{"mcplifecycleoperator"}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			mockDSCSamples(t, func(*http.Request) (int, string) { return 200, testDSCSample })
			var mu sync.Mutex
			patched := false
			c := dscRepairD3Server(t, dsc, &patched, &mu)
			r, err := RepairDSC(c, "my-dsc", tc.mode, "3.6.0", "datasciencecluster.opendatahub.io/v2", tc.expected, []string{"mcplifecycleoperator"})
			if err != nil || r.Success || r.ErrorCode != "prerequisites" ||
				!strings.Contains(r.Message, "set mcplifecycleoperator to Removed") || !strings.Contains(r.Message, "mcpservers.mcp.x-k8s.io") {
				t.Fatalf("result = %+v, %v", r, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if patched {
				t.Fatal("the DSC was patched")
			}
		})
	}
}

// The preview (GET /api/components dscCompatibility) lists the same block,
// so the confirmation can say why the repair would be refused.
func TestDSCCompatibility_PreviewFlagsBlockedRemovals(t *testing.T) {
	mockDSCSamples(t, func(*http.Request) (int, string) { return 200, testDSCSample })
	var mu sync.Mutex
	patched := false
	raw := `{"apiVersion":"datasciencecluster.opendatahub.io/v2","metadata":{"name":"my-dsc","resourceVersion":"42"},"spec":{"components":{"dashboard":{"managementState":"Managed"},"aipipelines":{"managementState":"Managed"},"mcplifecycleoperator":{"managementState":"Managed"},"trainingoperator":{"managementState":"Removed"}}}}`
	c := dscRepairD3Server(t, raw, &patched, &mu)
	var dsc map[string]interface{}
	_ = json.Unmarshal([]byte(raw), &dsc)
	result := checkDSCCompatibility(c, dsc)
	if strings.Join(result.ResetRemovals, ",") != "mcplifecycleoperator" {
		t.Fatalf("resetRemovals = %v", result.ResetRemovals)
	}
	if len(result.RemovalBlocks) != 1 || result.RemovalBlocks[0].Component != "mcplifecycleoperator" ||
		!strings.Contains(strings.Join(result.RemovalBlocks[0].Reasons, " "), "mcpservers.mcp.x-k8s.io") {
		t.Fatalf("removalBlocks = %+v", result.RemovalBlocks)
	}
	if patched {
		t.Fatal("the preview changed the cluster")
	}
}

// A refused component (dashboard, mlflowoperator, aipipelines) is never
// removed by a repair either.
func TestRepairDSC_RefusedComponents(t *testing.T) {
	mockDSCSamples(t, func(*http.Request) (int, string) {
		return 200, "apiVersion: datasciencecluster.opendatahub.io/v2\nkind: DataScienceCluster\nmetadata:\n  name: default-dsc\nspec:\n  components:\n    dashboard:\n      managementState: Removed\n    aipipelines:\n      managementState: Managed\n"
	})
	var mu sync.Mutex
	patched := false
	c := dscRepairD3Server(t, `{"apiVersion":"datasciencecluster.opendatahub.io/v2","metadata":{"name":"my-dsc","resourceVersion":"42"},"spec":{"components":{"dashboard":{"managementState":"Managed"},"aipipelines":{"managementState":"Managed"}}}}`, &patched, &mu)
	r, err := RepairDSC(c, "my-dsc", "reset-defaults", "3.6.0", "datasciencecluster.opendatahub.io/v2", nil, []string{"dashboard"})
	if err != nil || r.Success || !strings.Contains(r.Message, "dashboard") || !strings.Contains(r.Message, "dashboard-operator") {
		t.Fatalf("result = %+v, %v", r, err)
	}
	if patched {
		t.Fatal("the DSC was patched")
	}
}

// R5-F7: disable-component requires the module operator to be running even
// when the module CR has no finalizer (its operands' finalizers need it),
// and refuses a component whose Service serves a CRD conversion webhook.
func TestDisableComponent_OperatorAndConversionPreconditions(t *testing.T) {
	for _, tc := range []struct {
		name, component, want string
		operatorReady         bool
		conv                  string
	}{
		{"operator down, CR without finalizers", "ray", "ray-module-operator-controller-manager", false, `{"items":[]}`},
		{"conversion webhook served by the component", "trustyai", "trustyaiservices.trustyai.opendatahub.io", true,
			`{"items":[{"metadata":{"name":"trustyaiservices.trustyai.opendatahub.io"},"spec":{"conversion":{"strategy":"Webhook","webhook":{"clientConfig":{"service":{"namespace":"redhat-ods-applications","name":"trustyai-service-operator-webhook-service"}}}}}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFakeAPI(t)
			f.json("GET", dscV2List, http.StatusOK, `{"items":[{"metadata":{"name":"default-dsc"}}]}`)
			f.json("GET", dscV2List+"/default-dsc", http.StatusOK, `{"spec":{"components":{"`+tc.component+`":{"managementState":"Managed"}}}}`)
			f.json("GET", "/apis/apps/v1/namespaces/redhat-ods-operator/deployments/rhods-operator", http.StatusOK, `{"status":{"readyReplicas":1}}`)
			serveComponentGroup(f)
			kind := map[string]string{"ray": "Ray", "trustyai": "TrustyAI"}[tc.component]
			f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1", http.StatusOK, `{"resources":[{"name":"`+strings.ToLower(kind)+`s","kind":"`+kind+`"}]}`)
			f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1/"+strings.ToLower(kind)+"s", http.StatusOK, `{"items":[{"metadata":{"name":"default"}}]}`)
			op := moduleOperators[tc.component]
			ready := 0
			if tc.operatorReady {
				ready = 1
			}
			f.json("GET", "/apis/apps/v1/namespaces/"+op.namespace+"/deployments/"+op.name, http.StatusOK, `{"status":{"readyReplicas":`+map[int]string{0: "0", 1: "1"}[ready]+`}}`)
			f.json("GET", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", http.StatusOK, tc.conv)
			f.json("GET", svcPath("redhat-ods-applications", "trustyai-service-operator-webhook-service"), http.StatusOK,
				`{"metadata":{"ownerReferences":[{"apiVersion":"components.platform.opendatahub.io/v1alpha1","kind":"TrustyAI","name":"default-trustyai"}]}}`)
			res, err := ApplyFix(c, "disable-component:"+tc.component)
			if err != nil || res.Success || res.ErrorCode != "prerequisites" || !strings.Contains(res.Message, tc.want) {
				t.Fatalf("result = %+v, %v", res, err)
			}
			assertWrites(t, f)
		})
	}
}
