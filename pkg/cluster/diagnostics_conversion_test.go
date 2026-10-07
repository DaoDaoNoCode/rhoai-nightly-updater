package cluster

import (
	"net/http"
	"strings"
	"testing"
)

// conversionWorld is the API of the live RHOAI 3.6 nightly of 2026-10-07:
// DataScienceCluster serves v3 (preferred) and v2, Platform v1alpha2
// (preferred) and v1alpha1, and the operator's conversion webhook knows
// neither v3 nor v1alpha2. dsc3 is the answer to the v3 list (status and
// body). HardwareProfiles serve two versions the app may not list (403);
// dashboard.opendatahub.io serves one version and is not probed.
func conversionWorld(t *testing.T, dsc3Status int, dsc3Body string) (*fakeAPI, *Client) {
	t.Helper()
	apiVersionsCache.Purge()
	t.Cleanup(apiVersionsCache.Purge)
	f, c := newFakeAPI(t)
	f.json("GET", "/apis", 200, `{"kind":"APIGroupList","apiVersion":"v1","groups":[
		{"name":"dashboard.opendatahub.io","versions":[{"groupVersion":"dashboard.opendatahub.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"dashboard.opendatahub.io/v1","version":"v1"}},
		{"name":"datasciencecluster.opendatahub.io","versions":[{"groupVersion":"datasciencecluster.opendatahub.io/v3","version":"v3"},{"groupVersion":"datasciencecluster.opendatahub.io/v2","version":"v2"}],"preferredVersion":{"groupVersion":"datasciencecluster.opendatahub.io/v3","version":"v3"}},
		{"name":"config.opendatahub.io","versions":[{"groupVersion":"config.opendatahub.io/v1alpha2","version":"v1alpha2"},{"groupVersion":"config.opendatahub.io/v1alpha1","version":"v1alpha1"}],"preferredVersion":{"groupVersion":"config.opendatahub.io/v1alpha2","version":"v1alpha2"}},
		{"name":"infrastructure.opendatahub.io","versions":[{"groupVersion":"infrastructure.opendatahub.io/v1","version":"v1"},{"groupVersion":"infrastructure.opendatahub.io/v1alpha1","version":"v1alpha1"}],"preferredVersion":{"groupVersion":"infrastructure.opendatahub.io/v1","version":"v1"}},
		{"name":"apps","versions":[{"groupVersion":"apps/v1","version":"v1"}],"preferredVersion":{"groupVersion":"apps/v1","version":"v1"}}]}`)
	resources := func(gv, name, kind string) string {
		return `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"` + gv + `","resources":[
			{"name":"` + name + `","singularName":"","namespaced":false,"kind":"` + kind + `","verbs":["delete","deletecollection","get","list","patch","create","update","watch"]},
			{"name":"` + name + `/status","singularName":"","namespaced":false,"kind":"` + kind + `","verbs":["get","patch","update"]}]}`
	}
	for _, v := range []string{"v3", "v2"} {
		f.json("GET", "/apis/datasciencecluster.opendatahub.io/"+v, 200, resources("datasciencecluster.opendatahub.io/"+v, "datascienceclusters", "DataScienceCluster"))
	}
	for _, v := range []string{"v1alpha2", "v1alpha1"} {
		f.json("GET", "/apis/config.opendatahub.io/"+v, 200, resources("config.opendatahub.io/"+v, "platforms", "Platform"))
	}
	for _, v := range []string{"v1", "v1alpha1"} {
		f.json("GET", "/apis/infrastructure.opendatahub.io/"+v, 200, resources("infrastructure.opendatahub.io/"+v, "hardwareprofiles", "HardwareProfile"))
		f.status("GET", "/apis/infrastructure.opendatahub.io/"+v+"/hardwareprofiles", 403, "Forbidden")
	}
	f.json("GET", "/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters", dsc3Status, dsc3Body)
	f.json("GET", "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters", 200, `{"apiVersion":"datasciencecluster.opendatahub.io/v2","items":[{"metadata":{"name":"default-dsc"}}]}`)
	f.json("GET", "/apis/config.opendatahub.io/v1alpha2/platforms", 500, livePlatformConversion)
	f.json("GET", "/apis/config.opendatahub.io/v1alpha1/platforms", 200, `{"items":[{"metadata":{"name":"default"}}]}`)
	f.json("GET", namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName), 200, `{"status":{"installedCSV":"rhods-operator.3.6.0"}}`)
	f.json("GET", namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "rhods-operator.3.6.0"), 200,
		`{"metadata":{"name":"rhods-operator.3.6.0"},"spec":{"version":"3.6.0","install":{"spec":{"deployments":[{"name":"rhods-operator"}]}}},"status":{"phase":"Succeeded"}}`)
	return f, c
}

func TestConversionCheck_ReportsOneProblemPerKind(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"v3 list 500 (with limit)", 500, liveDSCv3Conversion500},
		{"v3 list 429 storage reinitializing", 429, liveDSCv3Conversion429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := conversionWorld(t, tc.status, tc.body)
			out := checkConversionFailures(c)
			assertWrites(t, f)
			if out.check.Status != "fail" || len(out.problems) != 2 {
				t.Fatalf("check %+v problems %d", out.check, len(out.problems))
			}
			resp := &DiagnosticsResponse{Problems: out.problems}
			dsc := findProblem(resp, "conversion-failed-datascienceclusters.datasciencecluster.opendatahub.io")
			if dsc == nil {
				t.Fatalf("problems %+v", out.problems)
			}
			if dsc.Severity != "critical" ||
				dsc.Title != "The operator cannot convert DataScienceCluster to v3" {
				t.Fatalf("dsc problem %+v", dsc)
			}
			evidence := strings.Join(dsc.Evidence, "\n")
			for _, want := range []string{
				`GET /apis/datasciencecluster.opendatahub.io/v3/datascienceclusters?limit=1: HTTP ` + itoa(tc.status) + `: `,
				`is registered for version "datasciencecluster.opendatahub.io/v3"`,
				"GET /apis/datasciencecluster.opendatahub.io/v2/datascienceclusters?limit=1: OK",
				"Installed operator: rhods-operator.3.6.0, Deployment redhat-ods-operator/rhods-operator",
			} {
				if !strings.Contains(evidence, want) {
					t.Errorf("evidence misses %q:\n%s", want, evidence)
				}
			}
			for _, want := range []string{"Reading them as v2 works", "the version the API server prefers", "an inference", "older than the CRDs its bundle installed"} {
				if !strings.Contains(dsc.Description, want) {
					t.Errorf("description misses %q: %s", want, dsc.Description)
				}
			}
			wantCmd := "oc get --raw '/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters?limit=1' --request-timeout=20s\n" +
				"oc logs -n redhat-ods-operator deploy/rhods-operator | grep conversion-webhook"
			if dsc.TechnicalCmd != wantCmd || !strings.Contains(dsc.Fix, "newer nightly") {
				t.Fatalf("cmd %q fix %q", dsc.TechnicalCmd, dsc.Fix)
			}
			if p := findProblem(resp, "conversion-failed-platforms.config.opendatahub.io"); p == nil || p.Title != "The operator cannot convert Platform to v1alpha2" || !strings.Contains(strings.Join(p.Evidence, "\n"), "v1alpha1/platforms?limit=1: OK") {
				t.Fatalf("platform problem %+v", p)
			}
			if !strings.Contains(out.check.Detail, "Platform v1alpha2, DataScienceCluster v3") || !strings.Contains(out.check.Detail, "2 reads not checked") {
				t.Fatalf("detail %q", out.check.Detail)
			}
			// Discovery and ?limit=1 lists only; single-version groups are not probed.
			for _, r := range f.reqs {
				if strings.HasSuffix(r.Path, "s") && strings.Count(r.Path, "/") == 4 && r.Query != "limit=1" && !strings.Contains(r.Path, "operators.coreos.com") {
					t.Errorf("list without limit=1: %s?%s", r.Path, r.Query)
				}
				if strings.HasPrefix(r.Path, "/apis/dashboard.opendatahub.io") || strings.HasPrefix(r.Path, "/apis/apps") {
					t.Errorf("probed a single-version or non-RHOAI group: %s", r.Path)
				}
			}
		})
	}
}

func TestConversionCheck_PassesWhenEveryVersionReads(t *testing.T) {
	f, c := conversionWorld(t, 200, `{"items":[]}`)
	f.json("GET", "/apis/config.opendatahub.io/v1alpha2/platforms", 200, `{"items":[]}`)
	out := checkConversionFailures(c)
	if out.check.Status != "pass" || len(out.problems) != 0 {
		t.Fatalf("%+v %+v", out.check, out.problems)
	}
	if !strings.Contains(out.check.Detail, "Every served version of 3 resources in 3 multi-version RHOAI API groups can be read") {
		t.Fatalf("detail %q", out.check.Detail)
	}
}

func TestConversionCheck_OtherErrorsWarn(t *testing.T) {
	_, c := conversionWorld(t, 503, `{"kind":"Status","status":"Failure","message":"etcd unavailable","code":503}`)
	out := checkConversionFailures(c)
	if out.check.Status != "fail" || len(out.problems) != 1 || !strings.Contains(out.check.Detail, "could not verify") {
		t.Fatalf("%+v %d", out.check, len(out.problems))
	}

	f, c := newFakeAPI(t)
	f.status("GET", "/apis", http.StatusServiceUnavailable, "ServiceUnavailable")
	if out := checkConversionFailures(c); out.check.Status != "warn" || len(out.problems) != 0 {
		t.Fatalf("%+v", out)
	}
}

// The whole report on the live state: the new check reports the failure,
// and the DataScienceCluster check still reads the DSC (at v2).
func TestConversionCheck_InFullReport(t *testing.T) {
	_, c := conversionWorld(t, 429, liveDSCv3Conversion429)
	resp, err := DiagnoseCluster(c)
	if err != nil {
		t.Fatal(err)
	}
	if findProblem(resp, "conversion-failed-datascienceclusters.datasciencecluster.opendatahub.io") == nil {
		t.Fatalf("problems %+v", resp.Problems)
	}
	if ch := findCheck(resp, "DataScienceCluster"); ch == nil || strings.Contains(ch.Detail, "could not verify") {
		t.Fatalf("DSC check %+v", ch)
	}
}
