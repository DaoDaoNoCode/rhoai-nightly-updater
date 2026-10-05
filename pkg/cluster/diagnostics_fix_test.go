package cluster

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const (
	vwcPath = "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations"
	mwcPath = "/apis/admissionregistration.k8s.io/v1/mutatingwebhookconfigurations"
)

func webhookConfig(name string, labels map[string]string, svcNS, svcName string) map[string]interface{} {
	meta := map[string]interface{}{"name": name}
	if labels != nil {
		meta["labels"] = labels
	}
	return map[string]interface{}{
		"metadata": meta,
		"webhooks": []interface{}{map[string]interface{}{
			"name":         name,
			"clientConfig": map[string]interface{}{"service": map[string]interface{}{"namespace": svcNS, "name": svcName}},
		}},
	}
}

func svcPath(ns, name string) string { return "/api/v1/namespaces/" + ns + "/services/" + name }

// setupWebhookFixtures mirrors the live cluster: OLM-owned operator
// webhooks, a KServe component webhook whose Service exists, and webhooks
// whose Service was deleted.
func setupWebhookFixtures(f *fakeAPI) {
	f.obj("GET", vwcPath, map[string]interface{}{"items": []interface{}{
		// Stale: operator Service is gone.
		webhookConfig("datasciencecluster-v2-validator.opendatahub.io-nfzvz", map[string]string{"olm.owner": "rhods-operator.3.6.0"}, "redhat-ods-operator", "rhods-operator-service"),
		// Serving: Service exists (the webhook the old fix deleted).
		webhookConfig("validating.odh-model-controller.opendatahub.io", map[string]string{"platform.opendatahub.io/part-of": "kserve"}, "redhat-ods-applications", "odh-model-controller-webhook-service"),
		// Not RHOAI: never touched even though its Service is missing.
		webhookConfig("some-other-operator.example.com", nil, "other", "gone-service"),
	}})
	f.obj("GET", mwcPath, map[string]interface{}{"items": []interface{}{
		webhookConfig("servicemonitor-injector.opendatahub.io-7vqqf", map[string]string{"olm.owner": "rhods-operator.3.6.0"}, "redhat-ods-operator", "rhods-operator-service"),
	}})
	f.json("GET", svcPath("redhat-ods-applications", "odh-model-controller-webhook-service"), http.StatusOK, `{"kind":"Service"}`)
	// rhods-operator-service and gone-service: unknown GET -> 404.
}

func TestDeleteStaleWebhooks_DeletesOnlyWebhooksWhoseServiceIsMissing(t *testing.T) {
	f, c := newFakeAPI(t)
	setupWebhookFixtures(f)
	f.json("DELETE", vwcPath+"/datasciencecluster-v2-validator.opendatahub.io-nfzvz", http.StatusOK, `{"kind":"Status","status":"Success"}`)
	f.json("DELETE", mwcPath+"/servicemonitor-injector.opendatahub.io-7vqqf", http.StatusOK, `{"kind":"Status","status":"Success"}`)

	res, err := ApplyFix(c, "delete-stale-webhooks")
	if err != nil {
		t.Fatal(err)
	}
	assertWrites(t, f,
		"DELETE "+vwcPath+"/datasciencecluster-v2-validator.opendatahub.io-nfzvz",
		"DELETE "+mwcPath+"/servicemonitor-injector.opendatahub.io-7vqqf",
	)
	if !res.Success || !strings.Contains(res.Message, "Deleted 2") {
		t.Fatalf("result = %+v", res)
	}
}

func TestDeleteStaleWebhooks_NothingToDoWhenEveryServiceExists(t *testing.T) {
	f, c := newFakeAPI(t)
	setupWebhookFixtures(f)
	// Diagnosis sees the operator Service missing...
	resp, err := RunDiagnostics(c)
	if err != nil {
		t.Fatal(err)
	}
	p := findProblem(resp, "stale-webhooks")
	if p == nil || !p.AutoFixable || p.AutoFixAction != "delete-stale-webhooks" {
		t.Fatalf("stale-webhooks problem = %+v", p)
	}
	// ...then the operator comes back before the user clicks Fix.
	f.json("GET", svcPath("redhat-ods-operator", "rhods-operator-service"), http.StatusOK, `{"kind":"Service"}`)

	res, err := ApplyFix(c, "delete-stale-webhooks")
	if err != nil {
		t.Fatal(err)
	}
	assertWrites(t, f)
	if res.Success || res.ErrorCode != "nothing_to_do" {
		t.Fatalf("result = %+v, want nothing_to_do", res)
	}
	// Running it again stays a no-op.
	res, _ = ApplyFix(c, "delete-stale-webhooks")
	assertWrites(t, f)
	if res.ErrorCode != "nothing_to_do" {
		t.Fatalf("second run = %+v", res)
	}
}

func TestDeleteStaleWebhooks_ServiceLookupForbiddenIsNotStale(t *testing.T) {
	f, c := newFakeAPI(t)
	setupWebhookFixtures(f)
	f.status("GET", svcPath("redhat-ods-operator", "rhods-operator-service"), http.StatusForbidden, "Forbidden")
	res, _ := ApplyFix(c, "delete-stale-webhooks")
	assertWrites(t, f)
	if res.Success {
		t.Fatalf("result = %+v", res)
	}
}

func TestStaleWebhookCheck_ListsExactlyTheObjectsTheFixDeletes(t *testing.T) {
	f, c := newFakeAPI(t)
	setupWebhookFixtures(f)
	out := checkStaleWebhooks(c)
	if len(out.problems) != 1 {
		t.Fatalf("problems = %+v", out.problems)
	}
	p := out.problems[0]
	want := []string{
		"ValidatingWebhookConfiguration datasciencecluster-v2-validator.opendatahub.io-nfzvz",
		"MutatingWebhookConfiguration servicemonitor-injector.opendatahub.io-7vqqf",
	}
	if strings.Join(p.AffectedObjects, "|") != strings.Join(want, "|") {
		t.Fatalf("affected = %q, want %q", p.AffectedObjects, want)
	}
	for _, w := range want {
		name := strings.Fields(w)[1]
		if !strings.Contains(p.ConfirmMessage, name) {
			t.Errorf("confirm message does not name %s:\n%s", name, p.ConfirmMessage)
		}
	}
	if strings.Contains(p.ConfirmMessage, "odh-model-controller") || p.Severity != "critical" {
		t.Errorf("problem = %+v", p)
	}
}

func TestStaleWebhookCheck_ServingWebhooksDuringUpgradeAreNotStale(t *testing.T) {
	// The old check flagged every RHOAI webhook whenever the CSV was not
	// Succeeded (e.g. during an update), although the Services still served.
	f, c := newFakeAPI(t)
	setupWebhookFixtures(f)
	f.json("GET", svcPath("redhat-ods-operator", "rhods-operator-service"), http.StatusOK, `{"kind":"Service"}`)
	f.obj("GET", "/apis/operators.coreos.com/v1alpha1/namespaces/"+SubNS+"/clusterserviceversions", map[string]interface{}{
		"items": []interface{}{map[string]interface{}{"metadata": map[string]interface{}{"name": "rhods-operator.3.6.0"}, "spec": map[string]interface{}{"version": "3.6.0"}, "status": map[string]interface{}{"phase": "Installing"}}},
	})
	out := checkStaleWebhooks(c)
	if len(out.problems) != 0 || out.check.Status != "pass" {
		t.Fatalf("out = %+v", out)
	}
}

func installPlan(name, phase, uid, rv, created string) map[string]interface{} {
	return map[string]interface{}{
		"metadata": map[string]interface{}{"name": name, "uid": uid, "resourceVersion": rv, "creationTimestamp": created},
		"spec":     map[string]interface{}{"approved": true, "clusterServiceVersionNames": []string{"rhods-operator.3.6.0"}},
		"status":   map[string]interface{}{"phase": phase},
	}
}

const ipListPath = "/apis/operators.coreos.com/v1alpha1/namespaces/redhat-ods-operator/installplans"

func TestDeleteStaleInstallPlans_DeletesOnlyFailedPlansWithPreconditions(t *testing.T) {
	f, c := newFakeAPI(t)
	f.obj("GET", ipListPath, map[string]interface{}{"items": []interface{}{
		installPlan("install-complete", "Complete", "u1", "11", "2026-10-01T00:00:00Z"),
		installPlan("install-new", "", "u2", "12", "2026-10-05T00:00:00Z"),
		installPlan("install-running", "Installing", "u3", "13", "2026-10-04T00:00:00Z"),
		installPlan("install-approval", "RequiresApproval", "u4", "14", "2026-10-03T00:00:00Z"),
		installPlan("install-failed-a", "Failed", "u5", "15", "2026-10-02T00:00:00Z"),
		installPlan("install-failed-b", "Failed", "u6", "16", "2026-10-02T01:00:00Z"),
	}})
	f.json("DELETE", ipListPath+"/install-failed-a", http.StatusOK, `{}`)
	f.json("DELETE", ipListPath+"/install-failed-b", http.StatusOK, `{}`)

	res, err := ApplyFix(c, "delete-stale-installplans")
	if err != nil {
		t.Fatal(err)
	}
	assertWrites(t, f, "DELETE "+ipListPath+"/install-failed-a", "DELETE "+ipListPath+"/install-failed-b")
	if !res.Success || !strings.Contains(res.Message, "install-failed-a") || !strings.Contains(res.Message, "install-failed-b") {
		t.Fatalf("result = %+v", res)
	}
	del := f.requests("DELETE", ipListPath+"/install-failed-a")[0]
	var opts struct {
		Preconditions struct {
			UID             string `json:"uid"`
			ResourceVersion string `json:"resourceVersion"`
		} `json:"preconditions"`
	}
	if err := json.Unmarshal([]byte(del.Body), &opts); err != nil || opts.Preconditions.UID != "u5" || opts.Preconditions.ResourceVersion != "15" {
		t.Fatalf("delete body = %s", del.Body)
	}
}

func TestDeleteStaleInstallPlans_Outcomes(t *testing.T) {
	failedOnly := map[string]interface{}{"items": []interface{}{installPlan("install-failed", "Failed", "u1", "1", "2026-10-02T00:00:00Z")}}
	tests := []struct {
		name        string
		list        interface{}
		listStatus  int
		deleteCode  int
		wantSuccess bool
		wantCode    string
		wantDeletes int
	}{
		{"no failed plans", map[string]interface{}{"items": []interface{}{installPlan("install-new", "", "u", "1", "2026-10-05T00:00:00Z")}}, 0, 0, false, "nothing_to_do", 0},
		{"no plans at all", map[string]interface{}{"items": []interface{}{}}, 0, 0, false, "nothing_to_do", 0},
		{"namespace missing", nil, http.StatusNotFound, 0, false, "nothing_to_do", 0},
		{"list forbidden", nil, http.StatusForbidden, 0, false, "forbidden", 0},
		{"already deleted (second click)", failedOnly, 0, http.StatusNotFound, false, "nothing_to_do", 1},
		{"changed since checked", failedOnly, 0, http.StatusConflict, false, "conflict", 1},
		{"delete fails", failedOnly, 0, http.StatusInternalServerError, false, "", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c := newFakeAPI(t)
			if tt.listStatus != 0 {
				f.status("GET", ipListPath, tt.listStatus, http.StatusText(tt.listStatus))
			} else {
				f.obj("GET", ipListPath, tt.list)
			}
			if tt.deleteCode != 0 {
				f.status("DELETE", ipListPath+"/install-failed", tt.deleteCode, http.StatusText(tt.deleteCode))
			}
			res, err := ApplyFix(c, "delete-stale-installplans")
			if err != nil {
				t.Fatal(err)
			}
			if res.Success != tt.wantSuccess || res.ErrorCode != tt.wantCode {
				t.Fatalf("result = %+v", res)
			}
			if got := len(f.writes()); got != tt.wantDeletes {
				t.Fatalf("writes = %v", f.writes())
			}
		})
	}
}

func TestInstallPlanCheck_FailedPlanConfirmNamesThePlans(t *testing.T) {
	f, c := newFakeAPI(t)
	f.obj("GET", ipListPath, map[string]interface{}{"items": []interface{}{
		installPlan("install-old", "Complete", "u1", "1", "2026-10-01T00:00:00Z"),
		installPlan("install-bad", "Failed", "u2", "2", "2026-10-02T00:00:00Z"),
	}})
	out := checkInstallPlanHealth(c)
	if len(out.problems) != 1 || out.problems[0].AutoFixAction != "delete-stale-installplans" {
		t.Fatalf("problems = %+v", out.problems)
	}
	p := out.problems[0]
	if !strings.Contains(p.ConfirmMessage, "install-bad") || strings.Contains(p.ConfirmMessage, "install-old") {
		t.Errorf("confirm = %s", p.ConfirmMessage)
	}
	if len(p.AffectedObjects) != 1 || p.AffectedObjects[0] != "InstallPlan redhat-ods-operator/install-bad" {
		t.Errorf("affected = %v", p.AffectedObjects)
	}
}

func TestInstallPlanCheck_NoSubscriptionAndNoPlansPasses(t *testing.T) {
	// The old code treated a 404 Subscription as "exists" on this path.
	_, c := newFakeAPI(t)
	out := checkInstallPlanHealth(c)
	if out.check.Status != "pass" || len(out.problems) != 0 {
		t.Fatalf("out = %+v", out)
	}
}

func TestRemovedFixesMakeNoRequests(t *testing.T) {
	for _, id := range []string{"recreate-subscription", "fix-maas-gateway-annotation", "restart-operator"} {
		t.Run(id, func(t *testing.T) {
			f, c := newFakeAPI(t)
			res, err := ApplyFix(c, id)
			if err != nil || res.Success || res.ErrorCode != "validation" {
				t.Fatalf("ApplyFix = %+v, %v", res, err)
			}
			if n := f.requestCount(); n != 0 {
				t.Fatalf("removed fix made %d requests", n)
			}
		})
	}
}

const dscV2List = "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters"

func TestDisableComponent_PreconditionAndReadBack(t *testing.T) {
	dsc := func(state string) string {
		return `{"metadata":{"name":"default-dsc"},"spec":{"components":{"llamastackoperator":{"managementState":"` + state + `"}}}}`
	}
	tests := []struct {
		name      string
		current   string
		patchResp string
		wantCode  string
		wantOK    bool
		wantPatch bool
		component string
	}{
		{"managed component is removed", dsc("Managed"), dsc("Removed"), "", true, true, "llamastackoperator"},
		{"already removed", dsc("Removed"), "", "nothing_to_do", false, false, "llamastackoperator"},
		{"component absent", `{"metadata":{"name":"default-dsc"},"spec":{"components":{}}}`, "", "nothing_to_do", false, false, "llamastackoperator"},
		{"patch silently pruned", dsc("Managed"), dsc("Managed"), "", false, true, "llamastackoperator"},
		{"not a top-level component", dsc("Managed"), "", "validation", false, false, "modelsasservice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c := newFakeAPI(t)
			f.json("GET", dscV2List, http.StatusOK, `{"items":[{"metadata":{"name":"default-dsc"}}]}`)
			f.json("GET", dscV2List+"/default-dsc", http.StatusOK, tt.current)
			if tt.patchResp != "" {
				f.json("PATCH", dscV2List+"/default-dsc", http.StatusOK, tt.patchResp)
			}
			res, err := ApplyFix(c, "disable-component:"+tt.component)
			if err != nil || res.Success != tt.wantOK || res.ErrorCode != tt.wantCode {
				t.Fatalf("ApplyFix = %+v, %v", res, err)
			}
			if got := len(f.writes()) == 1; got != tt.wantPatch {
				t.Fatalf("writes = %v", f.writes())
			}
		})
	}
}
