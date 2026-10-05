package cluster

import (
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
			f.obj("GET", csvBase, map[string]interface{}{"items": []interface{}{map[string]interface{}{
				"metadata": map[string]interface{}{"name": "rhods-operator.3.7.0"}, "status": map[string]interface{}{"phase": phase},
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
	f.obj("GET", vwcPath, map[string]interface{}{"items": []interface{}{
		map[string]interface{}{
			"metadata": map[string]interface{}{"name": vwcRuntime, "uid": "w1", "resourceVersion": "7", "labels": map[string]string{"platform.opendatahub.io/part-of": "kserve"}},
			"webhooks": []interface{}{map[string]interface{}{"name": "x", "clientConfig": map[string]interface{}{"service": map[string]interface{}{"namespace": "redhat-ods-applications", "name": "kserve-webhook-server-service"}}}},
		},
	}})
	f.json("GET", mwcPath, http.StatusOK, `{"items":[]}`)
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
	f.obj("GET", vwcPath, map[string]interface{}{"items": []interface{}{map[string]interface{}{
		"metadata": map[string]interface{}{"name": "mixed.opendatahub.io", "labels": map[string]string{"olm.owner": "rhods-operator.3.5.0"}},
		"webhooks": []interface{}{
			map[string]interface{}{"name": "a", "clientConfig": map[string]interface{}{"service": map[string]interface{}{"namespace": "redhat-ods-operator", "name": "gone"}}},
			map[string]interface{}{"name": "b", "clientConfig": map[string]interface{}{"service": map[string]interface{}{"namespace": "redhat-ods-operator", "name": "alive"}}},
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
	f.obj("GET", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", map[string]interface{}{"items": []interface{}{
		map[string]interface{}{
			"metadata": map[string]interface{}{"name": "mcpservers.mcp.x-k8s.io"},
			"spec": map[string]interface{}{"conversion": map[string]interface{}{"strategy": "Webhook", "webhook": map[string]interface{}{
				"clientConfig": map[string]interface{}{"service": map[string]interface{}{"namespace": "redhat-ods-applications", "name": "mcp-lifecycle-operator-webhook-service"}},
			}}},
			"status": map[string]interface{}{"storedVersions": []string{"v1alpha1", "v1beta1"}},
		},
		map[string]interface{}{
			"metadata": map[string]interface{}{"name": "modelregistries.modelregistry.opendatahub.io"},
			"spec": map[string]interface{}{"conversion": map[string]interface{}{"strategy": "Webhook", "webhook": map[string]interface{}{
				"clientConfig": map[string]interface{}{"service": map[string]interface{}{"namespace": "redhat-ods-applications", "name": "model-registry-operator-webhook-service"}},
			}}},
		},
		map[string]interface{}{"metadata": map[string]interface{}{"name": "plain.example.com"}, "spec": map[string]interface{}{"conversion": map[string]interface{}{"strategy": "None"}}},
	}})
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

func TestStaleCRDConversion_ForbiddenListIsAWarning(t *testing.T) {
	f, c := newFakeAPI(t)
	f.status("GET", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", http.StatusForbidden, "Forbidden")
	out := checkStaleWebhooks(c)
	if out.check.Status != "warn" || !strings.Contains(out.check.Detail, "cannot list CRDs") {
		t.Fatalf("check = %+v", out.check)
	}
}
