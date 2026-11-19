package cluster

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const csvBase = "/apis/operators.coreos.com/v1alpha1/namespaces/redhat-ods-operator/clusterserviceversions"

func TestStaleWebhooks_OLMOwnedWhileCSVExistsIsGuidanceOnly(t *testing.T) {
	f, c := newFakeAPI(t)
	setupWebhookFixtures(f)
	f.json("GET", csvBase+"/rhods-operator.3.6.0", http.StatusOK, `{"kind":"ClusterServiceVersion"}`)

	out := checkStaleWebhooks(c)
	var guidance *Problem
	for i := range out.problems {
		if out.problems[i].ID == "stale-webhooks" {
			t.Fatalf("offered deletion of OLM-owned webhooks while their CSV exists: %+v", out.problems[i])
		}
		if out.problems[i].ID == "webhook-service-missing" {
			guidance = &out.problems[i]
		}
	}
	if guidance == nil || guidance.AutoFixable || !strings.Contains(strings.Join(guidance.Evidence, "\n"), "still exists") {
		t.Fatalf("guidance = %+v", guidance)
	}
	res, _ := ApplyFix(c, "delete-stale-webhooks")
	assertWrites(t, f)
	if res.ErrorCode != "nothing_to_do" {
		t.Fatalf("result = %+v", res)
	}
}

func TestStaleWebhooks_NothingDeletedDuringOperatorUpgrade(t *testing.T) {
	for _, phase := range []string{"Pending", "InstallReady", "Installing", "Replacing"} {
		t.Run(phase, func(t *testing.T) {
			f, c := newFakeAPI(t)
			setupWebhookFixtures(f)
			f.obj(csvBase, map[string]any{"items": []any{map[string]any{
				"metadata": map[string]any{"name": "rhods-operator.3.7.0"}, "status": map[string]any{"phase": phase},
			}}})
			res, _ := ApplyFix(c, "delete-stale-webhooks")
			assertWrites(t, f)
			if res.ErrorCode != "nothing_to_do" {
				t.Fatalf("result = %+v", res)
			}
		})
	}
}

const vwcRuntime = "inferenceservice.serving.kserve.io"

func runtimeWebhookFixture(f *fakeAPI) {
	f.obj(vwcPath, map[string]any{"items": []any{
		map[string]any{
			"metadata": map[string]any{"name": vwcRuntime, "uid": "w1", "resourceVersion": "7", "labels": map[string]string{"platform.opendatahub.io/part-of": "kserve"}},
			"webhooks": []any{map[string]any{"name": "x", "clientConfig": map[string]any{"service": map[string]any{"namespace": "redhat-ods-applications", "name": "kserve-webhook-server-service"}}}},
		},
	}})
	f.json("GET", mwcPath, http.StatusOK, `{"items":[]}`)
	serveComponentGroup(f)
	f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1", http.StatusOK,
		`{"resources":[{"name":"kserves","kind":"Kserve"},{"name":"kserves/status","kind":"Kserve"}]}`)
}

func TestStaleWebhooks_RuntimeConfigDependsOnModuleCR(t *testing.T) {
	t.Run("module installed: guidance only", func(t *testing.T) {
		f, c := newFakeAPI(t)
		runtimeWebhookFixture(f)
		f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1/kserves", http.StatusOK, `{"items":[{"metadata":{"name":"default-kserve"}}]}`)
		res, _ := ApplyFix(c, "delete-stale-webhooks")
		assertWrites(t, f)
		if res.ErrorCode != "nothing_to_do" {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("module removed: deleted with preconditions", func(t *testing.T) {
		f, c := newFakeAPI(t)
		runtimeWebhookFixture(f)
		f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1/kserves", http.StatusOK, `{"items":[]}`)
		f.json("DELETE", vwcPath+"/"+vwcRuntime, http.StatusOK, `{}`)
		res, _ := ApplyFix(c, "delete-stale-webhooks")
		assertWrites(t, f, "DELETE "+vwcPath+"/"+vwcRuntime)
		if !res.Success {
			t.Fatalf("result = %+v", res)
		}
		body := f.requests("DELETE", vwcPath+"/"+vwcRuntime)[0].Body
		if !strings.Contains(body, `"uid":"w1"`) || !strings.Contains(body, `"resourceVersion":"7"`) {
			t.Fatalf("delete without preconditions: %s", body)
		}
	})
	t.Run("changed since checked: kept", func(t *testing.T) {
		f, c := newFakeAPI(t)
		runtimeWebhookFixture(f)
		f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1/kserves", http.StatusOK, `{"items":[]}`)
		f.status("DELETE", vwcPath+"/"+vwcRuntime, http.StatusConflict, "Conflict")
		res, _ := ApplyFix(c, "delete-stale-webhooks")
		if res.Success || res.ErrorCode != "conflict" {
			t.Fatalf("result = %+v", res)
		}
	})
}

func TestStaleWebhooks_PartlyServingConfigIsNotStale(t *testing.T) {
	f, c := newFakeAPI(t)
	f.obj(vwcPath, map[string]any{"items": []any{map[string]any{
		"metadata": map[string]any{"name": "mixed.opendatahub.io", "labels": map[string]string{"olm.owner": "rhods-operator.3.5.0"}},
		"webhooks": []any{
			map[string]any{"name": "a", "clientConfig": map[string]any{"service": map[string]any{"namespace": "redhat-ods-operator", "name": "gone"}}},
			map[string]any{"name": "b", "clientConfig": map[string]any{"service": map[string]any{"namespace": "redhat-ods-operator", "name": "alive"}}},
		},
	}}})
	f.json("GET", svcPath("redhat-ods-operator", "alive"), http.StatusOK, `{}`)
	out := checkStaleWebhooks(c)
	if len(out.problems) != 0 {
		t.Fatalf("problems = %+v", out.problems)
	}
}

func TestStaleCRDConversion_IsReportedAsGuidance(t *testing.T) {
	f, c := newFakeAPI(t)
	serveDSCI(f, "redhat-ods-applications")
	crds := map[string]any{"items": []any{
		map[string]any{
			"metadata": map[string]any{"name": "mcpservers.mcp.x-k8s.io"},
			"spec": map[string]any{"conversion": map[string]any{"strategy": "Webhook", "webhook": map[string]any{
				"clientConfig": map[string]any{"service": map[string]any{"namespace": "redhat-ods-applications", "name": "mcp-lifecycle-operator-webhook-service"}},
			}}},
			"status": map[string]any{"storedVersions": []string{"v1alpha1", "v1beta1"}},
		},
		map[string]any{
			"metadata": map[string]any{"name": "modelregistries.modelregistry.opendatahub.io"},
			"spec": map[string]any{"conversion": map[string]any{"strategy": "Webhook", "webhook": map[string]any{
				"clientConfig": map[string]any{"service": map[string]any{"namespace": "redhat-ods-applications", "name": "model-registry-operator-webhook-service"}},
			}}},
		},
		map[string]any{"metadata": map[string]any{"name": "plain.example.com"}, "spec": map[string]any{"conversion": map[string]any{"strategy": "None"}}},
	}}
	f.handle("GET", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", func(r *http.Request, _ []byte) (int, string) {
		if r.URL.Query().Get("labelSelector") == "" {
			t.Errorf("CRDs listed without a label selector")
		}
		// Both selectors return the same objects; duplicates must be merged.
		b, _ := json.Marshal(crds)
		return http.StatusOK, string(b)
	})
	f.json("GET", svcPath("redhat-ods-applications", "model-registry-operator-webhook-service"), http.StatusOK, `{}`)
	f.json("GET", dscV2List, http.StatusOK, `{"items":[{"metadata":{"name":"default-dsc"}}]}`)
	f.json("GET", dscV2List+"/default-dsc", http.StatusOK, `{"spec":{"components":{"mcplifecycleoperator":{"managementState":"Removed"},"kserve":{"managementState":"Managed"}}}}`)

	out := checkStaleWebhooks(c)
	if len(out.problems) != 1 {
		t.Fatalf("problems = %+v", out.problems)
	}
	p := out.problems[0]
	if p.ID != "stale-crd-conversion" || p.AutoFixable || p.Severity != "critical" {
		t.Fatalf("problem = %+v", p)
	}
	ev := strings.Join(p.Evidence, "\n")
	if !strings.Contains(ev, "mcpservers.mcp.x-k8s.io") || !strings.Contains(ev, "mcplifecycleoperator, which is Removed") || strings.Contains(ev, "modelregistries") {
		t.Fatalf("evidence = %s", ev)
	}
	if !strings.Contains(p.Fix, "mcplifecycleoperator") {
		t.Fatalf("fix = %s", p.Fix)
	}
	if out.check.Status != "fail" {
		t.Fatalf("check = %+v", out.check)
	}
}

// sparkCRDs are the Spark operator's CRDs as live on 2026-11-19 after
// sparkoperator was set to Removed: the CRDs stay, their conversion
// webhook Service is gone.
func sparkCRDs(names ...string) string {
	var items []string
	for _, n := range names {
		plural, group, _ := strings.Cut(n, ".")
		items = append(items, `{"metadata":{"name":"`+n+`"},"spec":{"group":"`+group+`","names":{"plural":"`+plural+`"},`+
			`"versions":[{"name":"v1beta2","served":true,"storage":true},{"name":"v1alpha1","served":true,"storage":false}],`+
			`"conversion":{"strategy":"Webhook","webhook":{"clientConfig":{"service":{"namespace":"redhat-ods-applications","name":"spark-operator-webhook-svc"}}}}},`+
			`"status":{"storedVersions":["v1beta2"]}}`)
	}
	return `{"items":[` + strings.Join(items, ",") + `]}`
}

// Live 2026-11-19: Spark's 3 CRDs, 0 objects, sparkoperator Removed. The
// advice used to list the owner once per CRD and say to set the component
// to Managed and back to Removed, which leaves the same CRDs behind. Empty
// CRDs can be deleted; the operator applies them again with the component.
func TestStaleCRDConversion_EmptyCRDsCanBeDeleted(t *testing.T) {
	f, c := newFakeAPI(t)
	serveDSCI(f, "redhat-ods-applications")
	names := []string{"scheduledsparkapplications.sparkoperator.k8s.io", "sparkapplications.sparkoperator.k8s.io", "sparkconnects.sparkoperator.k8s.io"}
	f.json("GET", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", http.StatusOK, sparkCRDs(names...))
	for _, n := range names {
		plural, _, _ := strings.Cut(n, ".")
		f.handle("GET", "/apis/sparkoperator.k8s.io/v1beta2/"+plural, func(r *http.Request, _ []byte) (int, string) {
			if r.URL.Query().Get("limit") != "1" {
				t.Errorf("objects of %s listed without limit=1", n)
			}
			return http.StatusOK, `{"items":[]}`
		})
	}
	f.json("GET", dscV2List, http.StatusOK, `{"items":[{"metadata":{"name":"default-dsc"}}]}`)
	f.json("GET", dscV2List+"/default-dsc", http.StatusOK, `{"spec":{"components":{"sparkoperator":{"managementState":"Removed"}}}}`)

	out := checkStaleWebhooks(c)
	if len(out.problems) != 1 || out.problems[0].ID != "stale-crd-conversion" {
		t.Fatalf("problems = %+v", out.problems)
	}
	p := out.problems[0]
	cmd := "oc delete crd " + strings.Join(names, " ")
	if !strings.Contains(p.Fix, "hold no objects, so they can be deleted: `"+cmd+"`") || strings.Count(p.Fix, "sparkoperator)") != 1 ||
		strings.Contains(p.Fix, "sparkoperator, sparkoperator") || strings.Contains(p.Fix, "back to Removed") || p.TechnicalCmd != cmd {
		t.Fatalf("fix = %q, cmd = %q", p.Fix, p.TechnicalCmd)
	}
	if ev := strings.Join(p.Evidence, "\n"); strings.Count(ev, "it holds no objects") != 3 {
		t.Fatalf("evidence = %s", ev)
	}
	assertWrites(t, f)
}

// A CRD with objects keeps the bring-the-Service-back path (objects, then
// CRDs, then Removed); one this app may not list gets the oc check.
func TestStaleCRDConversion_ObjectsOrUnknown(t *testing.T) {
	f, c := newFakeAPI(t)
	serveDSCI(f, "redhat-ods-applications")
	f.json("GET", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", http.StatusOK,
		sparkCRDs("scheduledsparkapplications.sparkoperator.k8s.io", "sparkapplications.sparkoperator.k8s.io", "sparkconnects.sparkoperator.k8s.io"))
	f.json("GET", "/apis/sparkoperator.k8s.io/v1beta2/sparkapplications", http.StatusOK, `{"items":[{"metadata":{"name":"pi","namespace":"team"}}]}`)
	// An object stored at another version fails the list on the webhook.
	f.json("GET", "/apis/sparkoperator.k8s.io/v1beta2/scheduledsparkapplications", http.StatusInternalServerError,
		`{"kind":"Status","status":"Failure","reason":"InternalError","code":500,"message":"conversion webhook for sparkoperator.k8s.io/v1alpha1, Kind=ScheduledSparkApplication failed: service \"spark-operator-webhook-svc\" not found"}`)
	f.status("GET", "/apis/sparkoperator.k8s.io/v1beta2/sparkconnects", http.StatusForbidden, "Forbidden")
	f.json("GET", dscV2List, http.StatusOK, `{"items":[{"metadata":{"name":"default-dsc"}}]}`)
	f.json("GET", dscV2List+"/default-dsc", http.StatusOK, `{"spec":{"components":{"sparkoperator":{"managementState":"Removed"}}}}`)

	p := checkStaleWebhooks(c).problems[0]
	for _, want := range []string{
		"For CRDs scheduledsparkapplications.sparkoperator.k8s.io, sparkapplications.sparkoperator.k8s.io: bring back the Service",
		"to Managed again (here: sparkoperator). Then, if you do not need the objects, delete them, then the CRDs, before you set the component back to Removed",
		"could not check whether CRD sparkconnects.sparkoperator.k8s.io holds objects. Check with `oc get sparkconnects.sparkoperator.k8s.io -A`: if it lists none, delete it (`oc delete crd sparkconnects.sparkoperator.k8s.io`)",
	} {
		if !strings.Contains(p.Fix, want) {
			t.Errorf("fix lacks %q:\n%s", want, p.Fix)
		}
	}
	if strings.Contains(p.Fix, "sparkoperator, sparkoperator") || strings.Contains(p.Fix, "can be deleted") {
		t.Errorf("fix = %s", p.Fix)
	}
	ev := strings.Join(p.Evidence, "\n")
	if strings.Count(ev, "Removed; it holds objects") != 2 || !strings.Contains(ev, "could not check whether it holds objects: this app may not list them") {
		t.Fatalf("evidence = %s", ev)
	}
}

func TestStaleCRDConversion_ForbiddenListIsAWarning(t *testing.T) {
	f, c := newFakeAPI(t)
	serveDSCI(f, "redhat-ods-applications")
	f.status("GET", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", http.StatusForbidden, "Forbidden")
	out := checkStaleWebhooks(c)
	if out.check.Status != "warn" || !strings.Contains(out.check.Detail, "cannot list CRDs") {
		t.Fatalf("check = %+v", out.check)
	}
}

// R1-6: a conversion Service that exists but whose endpoints cannot be read
// is not proven healthy, so the check warns instead of passing.
func TestStaleCRDConversion_UnreadableEndpointsWarn(t *testing.T) {
	f, c := newFakeAPI(t)
	serveDSCI(f, "redhat-ods-applications")
	crds := `{"items":[{"metadata":{"name":"modelregistries.modelregistry.opendatahub.io"},"spec":{"conversion":{"strategy":"Webhook","webhook":{"clientConfig":{"service":{"namespace":"redhat-ods-applications","name":"model-registry-operator-webhook-service"}}}}}}]}`
	f.json("GET", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", http.StatusOK, crds)
	f.json("GET", svcPath("redhat-ods-applications", "model-registry-operator-webhook-service"), http.StatusOK, `{}`)
	f.status("GET", "/apis/discovery.k8s.io/v1/namespaces/redhat-ods-applications/endpointslices", http.StatusForbidden, "Forbidden")
	f.json("GET", vwcPath, http.StatusOK, `{"items":[]}`)
	f.json("GET", mwcPath, http.StatusOK, `{"items":[]}`)
	out := checkStaleWebhooks(c)
	if out.check.Status != "warn" || !strings.Contains(out.check.Detail, "conversion Service of CRD modelregistries.modelregistry.opendatahub.io: list endpoints") {
		t.Fatalf("check = %+v", out.check)
	}
	if len(out.problems) != 0 {
		t.Fatalf("problems = %+v", out.problems)
	}
}
