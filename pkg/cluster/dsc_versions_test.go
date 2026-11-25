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

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
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

var discoveredV3V2 = rhoai.APIVersions{Versions: []string{"v3", "v2"}, Discovered: true}

func TestDSCDefaults_AcceptsV3ALMExample(t *testing.T) {
	mockDSCSamples(t, func(*http.Request) (int, string) {
		t.Error("GitHub must not be used when alm-examples has the version")
		return 500, ""
	})
	op := &components.InstalledOperator{Name: "rhods-operator.3.6.0", Version: "3.6.0", ALMExamples: almExamplesV3}
	for _, versions := range []rhoai.APIVersions{discoveredV3V2, {Versions: rhoai.DSCFallbackVersions}} {
		d, err := components.DefaultDSCSpecFor(context.Background(), op, versions, "")
		if err != nil || d.Source != "csv" || d.APIVersion != "v3" || d.Spec["apiVersion"] != "datasciencecluster.opendatahub.io/v3" {
			t.Fatalf("versions %+v: %+v %v", versions, d, err)
		}
		if !strings.Contains(d.YAML, "aiHub:") || !strings.Contains(d.YAML, "maasPortal:") {
			t.Fatalf("yaml %s", d.YAML)
		}
	}
	d, err := components.DefaultDSCSpecFor(context.Background(), op, discoveredV3V2, "v3")
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
	op := &components.InstalledOperator{Name: "rhods-operator.3.6.0", Version: "3.6.0"}
	d, err := components.DefaultDSCSpecFor(context.Background(), op, discoveredV3V2, "")
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
	d, err = components.DefaultDSCSpecFor(context.Background(), &components.InstalledOperator{Version: "3.5.0"}, discoveredV3V2, "")
	if err != nil || d.APIVersion != "v2" || len(fetched) != 2 || !strings.Contains(fetched[0], "_v3_") {
		t.Fatalf("%+v %v %v", d, err, fetched)
	}

	// DSC_SAMPLE_REF keeps working, for the version asked for.
	fetched = nil
	t.Setenv("DSC_SAMPLE_REF", "main")
	d, err = components.DefaultDSCSpecFor(context.Background(), &components.InstalledOperator{Version: "3.6.0", ALMExamples: almExamplesV3}, discoveredV3V2, "v2")
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
	op := &components.InstalledOperator{Name: "rhods-operator.3.6.0", Version: "3.6.0", ALMExamples: almExamplesV3}
	_, err := components.DefaultDSCSpecFor(context.Background(), op, discoveredV3V2, "v2")
	var missing *components.MissingDefaultsError
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
	rhoai.APIVersionsCache.Purge()
	t.Cleanup(rhoai.APIVersionsCache.Purge)
	consoleURLCache.Purge()
	t.Cleanup(consoleURLCache.Purge)
	f, c := newFakeAPI(t)
	f.json("GET", "/apis/datasciencecluster.opendatahub.io", 200, dscGroupDoc)
	f.json("GET", "/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters", 429, liveDSCv3Conversion429)
	f.json("GET", "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters", 200, liveDSCv2)
	f.json("GET", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/datascienceclusters.datasciencecluster.opendatahub.io", 200, testDSCCRD())
	f.json("GET", rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "subscriptions", rhoai.SubNS, rhoai.SubName), 200, `{"status":{"installedCSV":"rhods-operator.3.6.0"}}`)
	f.json("GET", rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", rhoai.SubNS, "rhods-operator.3.6.0"), 200, installedCSVBody(almExamplesV3))
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
	if resp.DSCState != rhoai.DSCStatePresent || resp.DSCAPIVersion != "datasciencecluster.opendatahub.io/v2" || len(resp.Components) != 3 {
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
	var dsc map[string]any
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
	components.AddDSCComponents(resp, dsc)
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

// The live answer of a DSC whose v3 objects cannot be converted: 429
// "storage is (re)initializing: ... conversion webhook ... failed" with
// Retry-After 30, which does not clear by itself. A versioned read takes it
// at once and reads v2 (rhoai.readServed's NoRetryWhen); a plain read
// retries it as client-go would, within the wait budget (one 30 s wait).
// While the cache only initializes (no conversion error) the versioned
// read waits and reads v3 (F9).
func TestListServedTakesTheConversion429AtOnce(t *testing.T) {
	const v3, v2 = "/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters", "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters"
	serve := func(t *testing.T, v3Answer string) (calls map[string]int, c *Client) {
		calls = map[string]int{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls[r.URL.Path]++
			switch {
			case r.URL.Path == "/apis/datasciencecluster.opendatahub.io":
				_, _ = w.Write([]byte(dscGroupDoc))
			case r.URL.Path == v3 && calls[v3] == 1:
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(v3Answer))
			case r.URL.Path == v3 && strings.Contains(v3Answer, "conversion webhook"):
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(v3Answer))
			case r.URL.Path == v3:
				_, _ = w.Write([]byte(`{"items":[]}`))
			case r.URL.Path == v2:
				_, _ = w.Write([]byte(liveDSCv2))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)
		rhoai.APIVersionsCache.Purge()
		t.Cleanup(rhoai.APIVersionsCache.Purge)
		return calls, newTestClient(testClientOptions{BaseURL: srv.URL, HTTPClient: srv.Client(), Ctx: context.Background()})
	}

	t.Run("conversion failure: v2 at once", func(t *testing.T) {
		calls, c := serve(t, liveDSCv3Conversion429)
		start := kube.Clock().Now()
		r, err := rhoai.ListServed(c, rhoai.DSCGroup, rhoai.DSCListFmt, nil, rhoai.DSCFallbackVersions)
		if elapsed := kube.Clock().Since(start); elapsed != 0 {
			t.Fatalf("waited %s", elapsed)
		}
		if err != nil || r.Version != "v2" || len(r.Skipped) != 1 || r.Skipped[0].Version != "v3" || calls[v3] != 1 {
			t.Fatalf("got %+v %v after %d v3 reads", r, err, calls[v3])
		}
	})
	t.Run("conversion failure, plain read: one wait, then the answer", func(t *testing.T) {
		calls, c := serve(t, liveDSCv3Conversion429)
		start := kube.Clock().Now()
		_, status, err := c.Get(v3)
		if elapsed := kube.Clock().Since(start); elapsed != 30*time.Second {
			t.Fatalf("waited %s", elapsed)
		}
		if status != 429 || calls[v3] != 2 || !rhoai.IsConversionWebhookError(err) {
			t.Fatalf("status %d calls %d err %v", status, calls[v3], err)
		}
	})
	t.Run("cache initializing: v3 after the wait", func(t *testing.T) {
		calls, c := serve(t, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"storage is (re)initializing","reason":"TooManyRequests","details":{"retryAfterSeconds":30},"code":429}`)
		start := kube.Clock().Now()
		r, err := rhoai.ListServed(c, rhoai.DSCGroup, rhoai.DSCListFmt, nil, rhoai.DSCFallbackVersions)
		if elapsed := kube.Clock().Since(start); elapsed != 30*time.Second {
			t.Fatalf("waited %s", elapsed)
		}
		if err != nil || r.Version != "v3" || len(r.Skipped) != 0 || calls[v3] != 2 {
			t.Fatalf("got %+v %v after %d v3 reads", r, err, calls[v3])
		}
	})
}
