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
	case strings.HasSuffix(r.URL.Path, "/clusterserviceversions/rhods-operator.test"):
		// The installed CSV names the operator's Deployment.
		_, _ = io.WriteString(w, `{"metadata":{"name":"rhods-operator.test"},"spec":{"version":"3.6.0","install":{"spec":{"deployments":[{"name":"rhods-operator"}]}}}}`)
	case r.URL.Path == "/apis/"+componentAPIGroup || r.URL.Path == "/apis/"+servicesAPIGroup:
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
	var crd map[string]any
	if err := json.Unmarshal([]byte(testDSCCRD()), &crd); err != nil {
		t.Fatal(err)
	}
	version := crd["spec"].(map[string]any)["versions"].([]any)[0].(map[string]any)
	root := version["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
	spec := root["properties"].(map[string]any)["spec"].(map[string]any)
	components := spec["properties"].(map[string]any)["components"].(map[string]any)["properties"].(map[string]any)
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
func dscRepairD3Server(t *testing.T, dsc string, patched *bool, mu *sync.Mutex, argo ...*fakeArgoCD) *Client {
	crd := dscCRDWith(t, "mcplifecycleoperator", "trainingoperator")
	conv := `{"items":[{"metadata":{"name":"mcpservers.mcp.x-k8s.io"},"spec":{"conversion":{"strategy":"Webhook","webhook":{"clientConfig":{"service":{"namespace":"redhat-ods-applications","name":"mcp-lifecycle-operator-webhook-service"}}}}},"status":{"storedVersions":["v1alpha1","v1beta1"]}}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case len(argo) > 0 && argo[0].serve(w, r):
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
	var dsc map[string]any
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

// dscRepairMCPServer is dscRepairD3Server with a listable MCPServer CRD
// (mcpCRDJSON: v1beta1 the storage version) whose objects are items (JSON
// objects). It returns the ServiceAccount's client carrying the signed-in
// user's (as pkg/api attaches it to GET /api/components), and the
// Authorization headers of the MCPServer lists.
func dscRepairMCPServer(t *testing.T, dsc, items string, patched *bool, mu *sync.Mutex) (*Client, func() []string) {
	crd := dscCRDWith(t, "mcplifecycleoperator", "trainingoperator")
	conv := `{"items":[` + mcpCRDJSON("Namespaced", mcpVersions) + `]}`
	var lists []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == mcpStorage:
			mu.Lock()
			lists = append(lists, r.Header.Get("Authorization")+" "+r.URL.RawQuery)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"metadata":{},"items":[`+items+`]}`)
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
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"kind":"Status","code":404}`)
		case repairGuardMock(w, r):
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(server.Close)
	c := &Client{baseURL: server.URL, token: "sa-token", httpClient: server.Client(), ctx: context.Background()}
	c.SetSignedInUser(&Client{baseURL: server.URL, token: "admin-token", httpClient: server.Client(), ctx: context.Background(), asUser: true})
	return c, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string{}, lists...)
	}
}

// The repair preview (GET /api/components, whose client carries the
// signed-in admin's) lists the MCPServers with the admin's token: none, and
// a reset that removes mcplifecycleoperator is not blocked (and the repair
// itself goes through); some, and the block names them.
func TestRepairPreviewListsConversionObjectsWithTheUsersToken(t *testing.T) {
	raw := `{"apiVersion":"datasciencecluster.opendatahub.io/v2","metadata":{"name":"my-dsc","resourceVersion":"42"},"spec":{"components":{"dashboard":{"managementState":"Managed"},"aipipelines":{"managementState":"Managed"},"mcplifecycleoperator":{"managementState":"Managed"},"trainingoperator":{"managementState":"Removed"}}}}`
	var dsc map[string]any
	_ = json.Unmarshal([]byte(raw), &dsc)

	t.Run("no objects", func(t *testing.T) {
		mockDSCSamples(t, func(*http.Request) (int, string) { return 200, testDSCSample })
		var mu sync.Mutex
		patched := false
		c, lists := dscRepairMCPServer(t, raw, "", &patched, &mu)
		result := checkDSCCompatibility(c, dsc)
		if strings.Join(result.ResetRemovals, ",") != "mcplifecycleoperator" || len(result.RemovalBlocks) != 0 {
			t.Fatalf("resetRemovals = %v, removalBlocks = %+v", result.ResetRemovals, result.RemovalBlocks)
		}
		if got := lists(); len(got) == 0 || got[0] != "Bearer admin-token limit=20" {
			t.Fatalf("MCPServer lists = %q", got)
		}
		if patched {
			t.Fatal("the preview changed the cluster")
		}
		// The repair runs with the admin's own client and goes through.
		user := c.SignedInUser()
		user.signedInUser = nil
		r, err := RepairDSC(user, "my-dsc", "reset-defaults", "3.6.0", "datasciencecluster.opendatahub.io/v2", nil, []string{"mcplifecycleoperator"})
		mu.Lock()
		defer mu.Unlock()
		if err != nil || !r.Success || !patched {
			t.Fatalf("repair = %+v, %v (patched %v)", r, err, patched)
		}
	})

	t.Run("objects", func(t *testing.T) {
		mockDSCSamples(t, func(*http.Request) (int, string) { return 200, testDSCSample })
		var mu sync.Mutex
		patched := false
		c, lists := dscRepairMCPServer(t, raw, mcpItems("team-a/github", "team-b/fetch"), &patched, &mu)
		result := checkDSCCompatibility(c, dsc)
		if len(result.RemovalBlocks) != 1 || result.RemovalBlocks[0].Component != "mcplifecycleoperator" ||
			!strings.Contains(strings.Join(result.RemovalBlocks[0].Reasons, " "), "2 objects of "+mcpCRDName+" exist: team-a/github, team-b/fetch.") {
			t.Fatalf("removalBlocks = %+v", result.RemovalBlocks)
		}
		for _, got := range lists() {
			if !strings.HasPrefix(got, "Bearer admin-token ") {
				t.Fatalf("listed with %q", got)
			}
		}
		if patched {
			t.Fatal("the preview changed the cluster")
		}
	})
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
	if err != nil || r.Success || !strings.Contains(r.Message, "dashboard") || !strings.Contains(r.Message, "the dashboard module's operator") {
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
			serveLive36ModuleOperators(f)
			serveToggleDSC(f, "v2", `{"metadata":{"name":"default-dsc","uid":"u1"},"spec":{"components":{"`+tc.component+`":{"managementState":"Managed"}}}}`, "", tc.component)
			f.json("GET", "/apis/apps/v1/namespaces/redhat-ods-operator/deployments/rhods-operator", http.StatusOK, `{"status":{"readyReplicas":1}}`)
			serveComponentGroup(f)
			kind := map[string]string{"ray": "Ray", "trustyai": "TrustyAI"}[tc.component]
			f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1", http.StatusOK, `{"resources":[{"name":"`+strings.ToLower(kind)+`s","kind":"`+kind+`"}]}`)
			f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1/"+strings.ToLower(kind)+"s", http.StatusOK, `{"items":[{"metadata":{"name":"default"}}]}`)
			op := liveOperator(tc.component)
			ready := 0
			if tc.operatorReady {
				ready = 1
			}
			f.json("GET", "/apis/apps/v1/namespaces/"+liveAppsNS+"/deployments/"+op.Deployment, http.StatusOK, `{"status":{"readyReplicas":`+map[int]string{0: "0", 1: "1"}[ready]+`}}`)
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
