package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The v3 example the RHOAI 3.6 bundle ships in alm-examples (abridged), and
// the live DataScienceCluster, readable only as v2.
const (
	almExamplesV3   = `[{"apiVersion":"datasciencecluster.opendatahub.io/v3","kind":"DataScienceCluster","metadata":{"labels":{"app.kubernetes.io/name":"datasciencecluster"},"name":"default-dsc"},"spec":{"components":{"aiHub":{"managementState":"Managed"},"dashboard":{"maasPortal":{"managementState":"Removed"},"standard":{"managementState":"Managed"}},"data":{"featureStore":{"managementState":"Managed"}}}}},{"apiVersion":"dscinitialization.opendatahub.io/v2","kind":"DSCInitialization","metadata":{"name":"default-dsci"},"spec":{}}]`
	liveDSCv2       = `{"apiVersion":"datasciencecluster.opendatahub.io/v2","items":[{"apiVersion":"datasciencecluster.opendatahub.io/v2","kind":"DataScienceCluster","metadata":{"name":"default-dsc"},"spec":{"components":{"dashboard":{"managementState":"Managed"},"feastoperator":{"managementState":"Managed"},"modelregistry":{"managementState":"Managed"}}},"status":{"phase":"Ready","conditions":[{"type":"DashboardReady","status":"True"}]}}]}`
	testDSCSampleV3 = `apiVersion: datasciencecluster.opendatahub.io/v3
kind: DataScienceCluster
metadata:
  name: default-dsc
spec:
  components:
    aiHub:
      managementState: Managed
`
)

var discoveredV3V2 = apiVersions{Versions: []string{"v3", "v2"}, Discovered: true}

func TestDSCDefaults_AcceptsV3ALMExample(t *testing.T) {
	mockDSCSamples(t, func(*http.Request) (int, string) {
		t.Error("GitHub must not be used when alm-examples has the version")
		return 500, ""
	})
	op := &installedOperator{Name: "rhods-operator.3.6.0", Version: "3.6.0", ALMExamples: almExamplesV3}
	for _, versions := range []apiVersions{discoveredV3V2, {Versions: dscFallbackVersions}} {
		d, err := defaultDSCSpecFor(context.Background(), op, versions, "")
		if err != nil || d.Source != "csv" || d.APIVersion != "v3" || d.Spec["apiVersion"] != "datasciencecluster.opendatahub.io/v3" {
			t.Fatalf("versions %+v: %+v %v", versions, d, err)
		}
		if !strings.Contains(d.YAML, "aiHub:") || !strings.Contains(d.YAML, "maasPortal:") {
			t.Fatalf("yaml %s", d.YAML)
		}
	}
	d, err := defaultDSCSpecFor(context.Background(), op, discoveredV3V2, "v3")
	if err != nil || d.APIVersion != "v3" {
		t.Fatalf("want v3: %+v %v", d, err)
	}
}

func TestDSCDefaults_GitHubTriesServedVersionsInOrder(t *testing.T) {
	var fetched []string
	mockDSCSamples(t, func(r *http.Request) (int, string) {
		fetched = append(fetched, r.URL.Path)
		if strings.Contains(r.URL.Path, "_v3_") {
			return 200, testDSCSampleV3
		}
		return 200, testDSCSample
	})
	op := &installedOperator{Name: "rhods-operator.3.6.0", Version: "3.6.0"}
	d, err := defaultDSCSpecFor(context.Background(), op, discoveredV3V2, "")
	if err != nil || d.APIVersion != "v3" || d.Source != "github" || len(fetched) != 1 ||
		!strings.HasSuffix(fetched[0], "/rhoai-3.6/config/rhoai/samples/datasciencecluster_v3_datasciencecluster.yaml") {
		t.Fatalf("%+v %v %v", d, err, fetched)
	}

	// The 3.5 branch has only the v2 sample: v3 is a 404, v2 is used.
	fetched = nil
	mockDSCSamples(t, func(r *http.Request) (int, string) {
		fetched = append(fetched, r.URL.Path)
		if strings.Contains(r.URL.Path, "_v3_") {
			return 404, "404: Not Found"
		}
		return 200, testDSCSample
	})
	d, err = defaultDSCSpecFor(context.Background(), &installedOperator{Version: "3.5.0"}, discoveredV3V2, "")
	if err != nil || d.APIVersion != "v2" || len(fetched) != 2 || !strings.Contains(fetched[0], "_v3_") {
		t.Fatalf("%+v %v %v", d, err, fetched)
	}

	// DSC_SAMPLE_REF keeps working, for the version asked for.
	fetched = nil
	t.Setenv("DSC_SAMPLE_REF", "main")
	d, err = defaultDSCSpecFor(context.Background(), &installedOperator{Version: "3.6.0", ALMExamples: almExamplesV3}, discoveredV3V2, "v2")
	if err != nil || d.Source != "github" || d.APIVersion != "v2" || len(fetched) != 1 || !strings.Contains(fetched[0], "/main/config/rhoai/samples/datasciencecluster_v2_") {
		t.Fatalf("%+v %v %v", d, err, fetched)
	}
}

func TestDSCDefaults_NeverAnotherVersion(t *testing.T) {
	mockDSCSamples(t, func(r *http.Request) (int, string) {
		if strings.Contains(r.URL.Path, "_v3_") {
			return 200, testDSCSampleV3
		}
		return 404, "404: Not Found"
	})
	op := &installedOperator{Name: "rhods-operator.3.6.0", Version: "3.6.0", ALMExamples: almExamplesV3}
	_, err := defaultDSCSpecFor(context.Background(), op, discoveredV3V2, "v2")
	var missing *missingDefaultsError
	if !errors.As(err, &missing) || !reflect.DeepEqual(missing.Available, []string{"v3"}) || missing.Want != "v2" {
		t.Fatalf("err %v", err)
	}
	if got := err.Error(); got != "operator 3.6.0 has no DataScienceCluster v2 defaults (none in the alm-examples of rhods-operator.3.6.0 or branch rhoai-3.6 of rhods-operator); it ships them as v3 only" {
		t.Fatalf("message %q", got)
	}
}

// dscV3BrokenAPI is GetComponents' view of the live cluster: v3 preferred,
// its reads failing in the conversion webhook, v2 working, and a bundle
// whose alm-examples have only the v3 example.
func dscV3BrokenAPI(t *testing.T) (*fakeAPI, *Client) {
	t.Helper()
	apiVersionsCache.Purge()
	t.Cleanup(apiVersionsCache.Purge)
	consoleURLCache.Purge()
	t.Cleanup(consoleURLCache.Purge)
	f, c := newFakeAPI(t)
	f.json("GET", "/apis/datasciencecluster.opendatahub.io", 200, dscGroupDoc)
	f.json("GET", "/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters", 429, liveDSCv3Conversion429)
	f.json("GET", "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters", 200, liveDSCv2)
	f.json("GET", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/datascienceclusters.datasciencecluster.opendatahub.io", 200, testDSCCRD())
	f.json("GET", namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName), 200, `{"status":{"installedCSV":"rhods-operator.3.6.0"}}`)
	f.json("GET", namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "rhods-operator.3.6.0"), 200, installedCSVBody(almExamplesV3))
	return f, c
}

func TestGetComponents_V3UnreadableComparesNothingAndSaysWhy(t *testing.T) {
	installFakeRegistry(t, newFakeRegistry())
	var fetched []string
	mockDSCSamples(t, func(r *http.Request) (int, string) {
		fetched = append(fetched, r.URL.Path)
		return 404, "404: Not Found"
	})
	f, c := dscV3BrokenAPI(t)
	resp, err := GetComponents(c, false)
	if err != nil {
		t.Fatal(err)
	}
	assertWrites(t, f)
	if resp.DSCState != DSCStatePresent || resp.DSCAPIVersion != "datasciencecluster.opendatahub.io/v2" || len(resp.Components) != 3 {
		t.Fatalf("resp %+v", resp)
	}
	fb := resp.DSCVersionFallback
	if fb == nil || fb.Version != "v3" || fb.Used != "v2" || fb.Message != liveDSCv3Message {
		t.Fatalf("fallback %+v", fb)
	}
	compat := resp.DSCCompatibility
	if len(compat.MissingComponents) != 0 || len(compat.ExtraComponents) != 0 || compat.DefaultsAPIVersion != "" {
		t.Fatalf("compared across versions: %+v", compat)
	}
	want := "This DataScienceCluster can only be read as v2: reading it as v3 fails because the operator's conversion webhook cannot convert it. " +
		"Operator 3.6.0 has no DataScienceCluster v2 defaults (none in the alm-examples of rhods-operator.3.6.0 or branch rhoai-3.6 of rhods-operator); it ships them as v3 only. " +
		"Component names differ between API versions, so the DataScienceCluster is not compared with defaults of another version. " +
		"Diagnostics explains the conversion failure and how to fix it."
	if compat.DefaultsError != want {
		t.Fatalf("defaults error:\n got %q\nwant %q", compat.DefaultsError, want)
	}
	// Only the v2 sample was looked for, never the v3 one.
	if len(fetched) != 1 || !strings.Contains(fetched[0], "datasciencecluster_v2_") {
		t.Fatalf("fetched %v", fetched)
	}
}

func TestGetDefaultDSCYAML_UsesTheLiveDSCVersion(t *testing.T) {
	mockDSCSamples(t, func(*http.Request) (int, string) { return 404, "" })
	_, c := dscV3BrokenAPI(t)
	if _, err := GetDefaultDSCYAML(c); err == nil || !strings.Contains(err.Error(), "no DataScienceCluster v2 defaults") {
		t.Fatalf("err %v", err)
	}
}

func TestCreateDefaultDSC_PostsToTheDefaultsVersion(t *testing.T) {
	mockDSCSamples(t, func(*http.Request) (int, string) { return 500, "" })
	f, c := dscV3BrokenAPI(t)
	// No DSC yet: nothing to convert, so v3 lists fine.
	f.json("GET", "/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters", 200, `{"items":[]}`)
	f.json("POST", "/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters", 201, `{}`)
	result, err := CreateDefaultDSC(c)
	if err != nil || !result.Success {
		t.Fatalf("%+v %v", result, err)
	}
	assertWrites(t, f, "POST /apis/datasciencecluster.opendatahub.io/v3/datascienceclusters")
	if body := f.requests("POST", "/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters")[0].Body; !strings.Contains(body, `"apiVersion":"datasciencecluster.opendatahub.io/v3"`) {
		t.Fatalf("body %s", body)
	}
}

func TestAddDSCComponents_GroupedV3Components(t *testing.T) {
	var dsc map[string]interface{}
	if err := json.Unmarshal([]byte(`{"spec":{"components":{
		"dashboard":{"standard":{"managementState":"Managed"},"maasPortal":{"managementState":"Removed"}},
		"aigateway":{"modelsAsAService":{"managementState":"Removed"},"batchGateway":{"managementState":"Removed"}},
		"data":{"featureStore":{"managementState":"Managed"}},
		"trainer":{"managementState":"Removed"},
		"newthing":{"someSetting":"x"}}},
		"status":{"components":{"data":{"managementState":"Managed"}},"conditions":[{"type":"DashboardReady","status":"True"}]}}`), &dsc); err != nil {
		t.Fatal(err)
	}
	resp := &types.ComponentsResponse{}
	addDSCComponents(resp, dsc)
	got := map[string]string{}
	for _, comp := range resp.Components {
		got[comp.Name] = comp.ManagementState + "/" + comp.Status
	}
	want := map[string]string{
		"dashboard": "Managed/Available", "aigateway": "Removed/Removed", "data": "Managed/Unknown",
		"trainer": "Removed/Removed", "newthing": "Unknown/Unknown",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestClientReturns429WithoutWaitingForRetryAfter(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(liveDSCv3Conversion429))
	}))
	defer srv.Close()
	c := &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: context.Background()}
	start := time.Now()
	_, status, err := c.get("/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters")
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("waited %s", elapsed)
	}
	if status != 429 || calls != 1 || !isConversionWebhookError(err) {
		t.Fatalf("status %d calls %d err %v", status, calls, err)
	}
}
