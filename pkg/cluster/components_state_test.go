package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const almExamplesJSON = `[{"apiVersion":"datasciencecluster.opendatahub.io/v2","kind":"DataScienceCluster","metadata":{"labels":{"app.kubernetes.io/name":"datasciencecluster"},"name":"default-dsc"},"spec":{"components":{"dashboard":{"managementState":"Managed"},"mcplifecycleoperator":{"managementState":"Removed"}}}},{"apiVersion":"dscinitialization.opendatahub.io/v2","kind":"DSCInitialization","metadata":{"name":"default-dsci"},"spec":{}}]`

func installedCSVBody(almExamples string) string {
	ann := map[string]string{}
	if almExamples != "" {
		ann["alm-examples"] = almExamples
	}
	b, _ := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"name": "rhods-operator.3.6.0", "annotations": ann},
		"spec":     map[string]interface{}{"version": "3.6.0", "displayName": "Red Hat OpenShift AI"},
		"status":   map[string]interface{}{"phase": "Succeeded"},
	})
	return string(b)
}

// componentsAPI serves the reads of GetComponents with a fixed per-request latency.
func componentsAPI(t *testing.T, latency time.Duration, dscV2, dscV1 mockResponse, almExamples string) (*Client, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	routes := map[string]mockResponse{
		"/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters":                                                dscV2,
		"/apis/datasciencecluster.opendatahub.io/v1/datascienceclusters":                                                dscV1,
		namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName):                                {body: `{"spec":{"source":"rhoai-catalog-dev"},"status":{"installedCSV":"rhods-operator.3.6.0"}}`},
		namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "rhods-operator.3.6.0"):        {body: installedCSVBody(almExamples)},
		"/apis/apiextensions.k8s.io/v1/customresourcedefinitions/datascienceclusters.datasciencecluster.opendatahub.io": {body: testDSCCRD()},
		namespacedPath("apps/v1", "deployments", "redhat-ods-applications", ""):                                         {body: `{"items":[{"metadata":{"name":"rhods-dashboard"},"spec":{"replicas":2,"selector":{"matchLabels":{"app":"rhods-dashboard"}},"template":{"spec":{"containers":[{"image":"registry.redhat.io/rhoai/odh-dashboard-rhel9@sha256:` + strings.Repeat("1", 64) + `"}]}}},"status":{"readyReplicas":2,"availableReplicas":2,"updatedReplicas":2}}]}`},
		namespacedPath("apps/v1", "deployments", "redhat-ods-operator", ""):                                             {body: `{"items":[{"metadata":{"name":"rhods-operator"},"spec":{"selector":{"matchLabels":{"name":"rhods-operator"}},"template":{"spec":{"containers":[{"image":"registry.redhat.io/rhoai/odh-rhel9-operator@sha256:` + strings.Repeat("2", 64) + `"}]}}},"status":{"readyReplicas":0,"unavailableReplicas":1,"conditions":[{"type":"Progressing","status":"False","reason":"ProgressDeadlineExceeded","message":"timed out"}]}}]}`},
		namespacedPath("v1", "pods", "redhat-ods-applications", ""):                                                     {body: `{"items":[{"metadata":{"name":"rhods-dashboard-abc-1","labels":{"app":"rhods-dashboard","pod-template-hash":"abc"}},"status":{"phase":"Running","containerStatuses":[{"name":"c","ready":true}]}}]}`},
		"/apis/config.openshift.io/v1/consoles/cluster":                                                                 {body: `{"status":{"consoleURL":"https://console"}}`},
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		time.Sleep(latency)
		resp, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(404)
			_, _ = fmt.Fprint(w, `{"kind":"Status","status":"Failure","reason":"NotFound","code":404}`)
			return
		}
		if resp.statusCode != 0 {
			w.WriteHeader(resp.statusCode)
		}
		_, _ = fmt.Fprint(w, resp.body)
	}))
	t.Cleanup(srv.Close)
	consoleURLCache.Purge()
	t.Cleanup(consoleURLCache.Purge)
	return &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: context.Background()}, &requests
}

var notFoundResp = mockResponse{statusCode: 404, body: `{"kind":"Status","status":"Failure","reason":"NotFound","code":404}`}

func TestGetComponents_DSCStates(t *testing.T) {
	cases := []struct {
		name      string
		v2, v1    mockResponse
		wantState string
		wantErr   int // K8s status expected in the error
	}{
		{"operator installed, no DSC yet", mockResponse{body: `{"items":[]}`}, mockResponse{body: `{"items":[]}`}, DSCStateNoDSC, 0},
		{"no CRD", notFoundResp, notFoundResp, DSCStateNoCRD, 0},
		{"forbidden is an error, not a missing DSC", mockResponse{statusCode: 403, body: `{"kind":"Status","status":"Failure","reason":"Forbidden","code":403}`}, notFoundResp, "", 403},
		{"API throttled", mockResponse{statusCode: 429, body: `{}`}, mockResponse{statusCode: 429, body: `{}`}, "", 429},
		{"DSC present", mockResponse{body: `{"items":[{"apiVersion":"datasciencecluster.opendatahub.io/v2","metadata":{"name":"default-dsc"},"spec":{"components":{"dashboard":{"managementState":"Managed"},"aipipelines":{"managementState":"Removed"}}},"status":{"phase":"Ready","conditions":[{"type":"DashboardReady","status":"True"}]}}]}`}, notFoundResp, DSCStatePresent, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			installFakeRegistry(t, newFakeRegistry())
			c, _ := componentsAPI(t, 0, tc.v2, tc.v1, almExamplesJSON)
			resp, err := GetComponents(c, false)
			if tc.wantErr != 0 {
				if !IsK8sError(err, tc.wantErr) {
					t.Fatalf("want K8s %d error, got resp=%+v err=%v", tc.wantErr, resp, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if resp.DSCState != tc.wantState || resp.DSCExists != (tc.wantState == DSCStatePresent) {
				t.Fatalf("state %q exists %v", resp.DSCState, resp.DSCExists)
			}
			if resp.OperatorVersion != "3.6.0" || resp.OperatorPhase != "Succeeded" {
				t.Fatalf("operator %q %q", resp.OperatorVersion, resp.OperatorPhase)
			}
			if len(resp.Deployments) != 2 || resp.Components == nil {
				t.Fatalf("deployments %d components %v", len(resp.Deployments), resp.Components)
			}
			if tc.wantState == DSCStatePresent {
				if resp.DSCName != "default-dsc" || resp.DSCPhase != "Ready" || len(resp.Components) != 2 ||
					resp.Components[0].Name != "aipipelines" || resp.Components[1].Status != "Available" {
					t.Fatalf("components %+v", resp)
				}
				if resp.DSCCompatibility == nil || resp.DSCCompatibility.DefaultsSource != "csv" || resp.DSCCompatibility.OperatorVersion != "3.6.0" {
					t.Fatalf("compatibility %+v", resp.DSCCompatibility)
				}
			} else if resp.DSCCompatibility != nil || len(resp.Components) != 0 {
				t.Fatalf("no DSC must not report components: %+v", resp)
			}
		})
	}
}

func TestGetComponents_DeploymentsAndPodsTyped(t *testing.T) {
	installFakeRegistry(t, newFakeRegistry())
	c, _ := componentsAPI(t, 0, mockResponse{body: `{"items":[]}`}, notFoundResp, "")
	resp, err := GetComponents(c, false)
	if err != nil {
		t.Fatal(err)
	}
	dash, op := resp.Deployments[0], resp.Deployments[1]
	if dash.Name != "rhods-dashboard" || dash.Desired != 2 || dash.Ready != 2 || len(dash.Pods) != 1 || dash.MatchLabels["app"] != "rhods-dashboard" {
		t.Fatalf("dashboard %+v", dash)
	}
	if op.Desired != 1 || !op.RolloutStuck || op.RolloutMessage != "timed out" || op.UnavailableReplicas != 1 || op.Namespace != "redhat-ods-operator" {
		t.Fatalf("operator %+v", op)
	}
	if resp.ConsoleURL != "https://console" {
		t.Fatalf("console %q", resp.ConsoleURL)
	}
}

func TestGetComponents_ReadsRunInParallel(t *testing.T) {
	installFakeRegistry(t, newFakeRegistry())
	const latency = 60 * time.Millisecond
	dsc := mockResponse{body: `{"items":[{"apiVersion":"datasciencecluster.opendatahub.io/v2","metadata":{"name":"default-dsc"},"spec":{"components":{"dashboard":{"managementState":"Managed"}}}}]}`}
	c, requests := componentsAPI(t, latency, dsc, notFoundResp, almExamplesJSON)
	start := time.Now()
	if _, err := GetComponents(c, false); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	// ~10 requests; the longest chain is DSC (1) || Subscription -> CSV (2), then the CRD (1).
	if n := requests.Load(); n < 9 {
		t.Fatalf("only %d requests", n)
	}
	if elapsed > 6*latency {
		t.Fatalf("GetComponents took %v (> %v): reads are not parallel", elapsed, 6*latency)
	}
}

func TestGetComponents_UsesCachedLabelsWithoutQuay(t *testing.T) {
	f := newFakeRegistry()
	installFakeRegistry(t, f)
	labelCache.Add("rhoai/odh-dashboard-rhel9@sha256:"+strings.Repeat("1", 64), labelCacheValue{labels: ImageLabels{GitCommit: "abc", BuildDate: "2026-10-01"}}, 0)
	c, _ := componentsAPI(t, 0, mockResponse{body: `{"items":[]}`}, notFoundResp, "")
	resp, err := GetComponents(c, false)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Deployments[0].GitCommit != "abc" || resp.Deployments[1].GitCommit != "" {
		t.Fatalf("labels %+v", resp.Deployments)
	}
	total := 0
	for _, n := range f.counts {
		total += n
	}
	if total != 0 {
		t.Fatalf("labels=false must not call Quay: %v", f.counts)
	}
}

func TestDefaultDSCFromALMExamples(t *testing.T) {
	cases := []struct {
		name       string
		alm        string
		override   string
		wantSource string
	}{
		{"installed CSV example", almExamplesJSON, "", "csv"},
		{"no annotation falls back to GitHub", "", "", "github"},
		{"malformed annotation falls back", "{not json", "", "github"},
		{"no DSC example falls back", `[{"kind":"DSCInitialization","apiVersion":"dscinitialization.opendatahub.io/v2","metadata":{"name":"x"}}]`, "", "github"},
		{"DSC example without components falls back", `[{"kind":"DataScienceCluster","apiVersion":"datasciencecluster.opendatahub.io/v2","metadata":{"name":"default-dsc"},"spec":{}}]`, "", "github"},
		{"explicit DSC_SAMPLE_REF wins", almExamplesJSON, "rhoai-3.6", "github"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var github int
			mockDSCSamples(t, func(*http.Request) (int, string) { github++; return 200, testDSCSample })
			t.Setenv("DSC_SAMPLE_REF", tc.override)
			defaults, err := defaultDSCSpecFor(context.Background(), &installedOperator{Name: "rhods-operator.3.6.0", Version: "3.6.0", ALMExamples: tc.alm})
			if err != nil {
				t.Fatal(err)
			}
			if defaults.Source != tc.wantSource || defaults.Version != "3.6.0" || defaults.Branch == "" {
				t.Fatalf("defaults %+v", defaults)
			}
			if (github > 0) != (tc.wantSource == "github") {
				t.Fatalf("GitHub calls %d for source %s", github, tc.wantSource)
			}
			if tc.wantSource == "csv" {
				comps := defaults.Spec["spec"].(map[string]interface{})["components"].(map[string]interface{})
				mcp := comps["mcplifecycleoperator"].(map[string]interface{})
				if mcp["managementState"] != "Removed" || defaults.Spec["kind"] != "DataScienceCluster" || defaults.SourceURL != "" {
					t.Fatalf("spec %+v", defaults.Spec)
				}
				if !strings.Contains(defaults.YAML, "mcplifecycleoperator:") || strings.Contains(defaults.YAML, "status") {
					t.Fatalf("yaml %s", defaults.YAML)
				}
				// Callers get their own copy.
				comps["dashboard"] = nil
				again, _ := defaultDSCSpecFor(context.Background(), &installedOperator{Version: "3.6.0", ALMExamples: tc.alm})
				if again.Spec["spec"].(map[string]interface{})["components"].(map[string]interface{})["dashboard"] == nil {
					t.Fatal("shared mutable defaults")
				}
			}
		})
	}
}

func TestCreateDefaultDSC_UsesInstalledCSVExampleAndToleratesRace(t *testing.T) {
	for _, tc := range []struct {
		name        string
		postStatus  int
		wantSuccess bool
		wantMessage string
	}{
		{"created", 201, true, "DataScienceCluster created with default components"},
		{"operator created it concurrently", 409, true, "DataScienceCluster already exists"},
		{"webhook rejects", 403, false, "Failed to create DataScienceCluster"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mockDSCSamples(t, func(*http.Request) (int, string) {
				t.Error("GitHub must not be used when the CSV ships alm-examples")
				return 500, ""
			})
			list := "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters"
			c, records, cleanup := newRecordingMockClient(map[string]mockResponse{
				"GET " + list: {body: `{"items":[]}`},
				"POST " + list: {statusCode: tc.postStatus, body: map[int]string{
					201: `{}`,
					409: `{"kind":"Status","status":"Failure","reason":"AlreadyExists","code":409}`,
					403: `{"kind":"Status","status":"Failure","reason":"Forbidden","message":"admission webhook denied the request","code":403}`,
				}[tc.postStatus]},
				namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName):                         {body: `{"status":{"installedCSV":"rhods-operator.3.6.0"}}`},
				namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "rhods-operator.3.6.0"): {body: installedCSVBody(almExamplesJSON)},
			})
			defer cleanup()
			result, err := CreateDefaultDSC(c)
			if err != nil || result.Success != tc.wantSuccess || !strings.Contains(result.Message, tc.wantMessage) {
				t.Fatalf("result %+v err %v", result, err)
			}
			if !strings.Contains(strings.Join(result.Logs, "\n"), "alm-examples of the installed CSV rhods-operator.3.6.0") {
				t.Fatalf("logs %v", result.Logs)
			}
			for _, rec := range *records {
				if strings.Contains(rec.Path, "datascienceclusters") && rec.Method != "GET" && rec.Method != "POST" {
					t.Fatalf("unexpected %s %s", rec.Method, rec.Path)
				}
			}
		})
	}
}
