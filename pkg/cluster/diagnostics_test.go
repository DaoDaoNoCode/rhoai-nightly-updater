package cluster

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func buildHealthyCatalogSource() string {
	cs := map[string]interface{}{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "CatalogSource",
		"metadata":   map[string]interface{}{"name": CatalogName, "namespace": CatalogNS},
		"spec":       map[string]interface{}{"image": "quay.io/rhoai/rhoai-fbc-fragment:nightly-20260101"},
		"status": map[string]interface{}{
			"connectionState": map[string]interface{}{"lastObservedState": "READY"},
		},
	}
	b, _ := json.Marshal(cs)
	return string(b)
}

func buildHealthyCSVList() string {
	csvList := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{
				"metadata": map[string]interface{}{"name": "rhods-operator.v3.5.0"},
				"spec":     map[string]interface{}{"displayName": "Red Hat OpenShift AI", "version": "3.5.0"},
				"status":   map[string]interface{}{"phase": "Succeeded"},
			},
		},
	}
	b, _ := json.Marshal(csvList)
	return string(b)
}

func buildHealthySubscription() string {
	sub := map[string]interface{}{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "Subscription",
		"metadata":   map[string]interface{}{"name": SubName, "namespace": SubNS},
		"spec":       map[string]interface{}{"source": CatalogName, "channel": "fast"},
		"status":     map[string]interface{}{"state": "AtLatestKnown"},
	}
	b, _ := json.Marshal(sub)
	return string(b)
}

func buildValidPullSecret() string {
	dockerConfig := map[string]interface{}{
		"auths": map[string]interface{}{
			"quay.io/rhoai": map[string]interface{}{
				"auth": exampleAuth("test:test"),
			},
		},
	}
	dockerConfigJSON, _ := json.Marshal(dockerConfig)
	dockerConfigB64 := base64.StdEncoding.EncodeToString(dockerConfigJSON)

	secret := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"data": map[string]interface{}{
			".dockerconfigjson": dockerConfigB64,
		},
	}
	b, _ := json.Marshal(secret)
	return string(b)
}

func buildIDMSList() string {
	idms := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{
				"metadata": map[string]interface{}{"name": "rhoai-mirror"},
				"spec": map[string]interface{}{
					"imageDigestMirrors": []interface{}{
						map[string]interface{}{"source": IDMSSource},
					},
				},
			},
		},
	}
	b, _ := json.Marshal(idms)
	return string(b)
}

func buildInstallPlanList() string {
	list := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{
				"metadata": map[string]interface{}{
					"name":              "install-abc",
					"creationTimestamp": "2026-01-01T00:00:00Z",
				},
				"spec":   map[string]interface{}{"approved": true},
				"status": map[string]interface{}{"phase": "Complete"},
			},
		},
	}
	b, _ := json.Marshal(list)
	return string(b)
}

func buildEmptyWebhookList() string {
	list := map[string]interface{}{"items": []interface{}{}}
	b, _ := json.Marshal(list)
	return string(b)
}

func buildEmptyList() string {
	list := map[string]interface{}{"items": []interface{}{}}
	b, _ := json.Marshal(list)
	return string(b)
}

func buildHealthyPodList() string {
	list := map[string]interface{}{"items": []interface{}{
		map[string]interface{}{
			"metadata": map[string]interface{}{"name": "rhods-operator-abc123", "namespace": SubNS, "creationTimestamp": "2026-07-01T00:00:00Z"},
			"status": map[string]interface{}{
				"phase": "Running",
				"containerStatuses": []interface{}{
					map[string]interface{}{"name": "manager", "ready": true, "restartCount": 0, "state": map[string]interface{}{"running": map[string]interface{}{}}},
				},
			},
			"spec": map[string]interface{}{"nodeName": "node-1"},
		},
	}}
	b, _ := json.Marshal(list)
	return string(b)
}

func buildNodeList(count int) string {
	items := make([]interface{}, count)
	for i := 0; i < count; i++ {
		items[i] = map[string]interface{}{
			"metadata": map[string]interface{}{
				"name": fmt.Sprintf("node-%d", i),
			},
			"status": map[string]interface{}{
				"conditions": []interface{}{
					map[string]interface{}{
						"type":   "Ready",
						"status": "True",
					},
				},
				"allocatable": map[string]interface{}{
					"cpu":    "4",
					"memory": "16Gi",
				},
			},
		}
	}
	list := map[string]interface{}{"items": items}
	b, _ := json.Marshal(list)
	return string(b)
}

func diagnosticPaths() map[string]string {
	return map[string]string{
		"catalogSource": fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/catalogsources/%s", CatalogNS, CatalogName),
		"csvList":       fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/clusterserviceversions", SubNS),
		"subscription":  fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/subscriptions/%s", SubNS, SubName),
		"pullSecret":    "/api/v1/namespaces/kube-system/secrets/additional-pull-secret",
		"idms":          "/apis/config.openshift.io/v1/imagedigestmirrorsets",
		"vwh":           "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations",
		"mwh":           "/apis/admissionregistration.k8s.io/v1/mutatingwebhookconfigurations",
		"installPlans":  fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/installplans", SubNS),
		"nodes":         "/api/v1/nodes",
		"opPods":        "/api/v1/namespaces/redhat-ods-operator/pods",
		"appPods":       "/api/v1/namespaces/redhat-ods-applications/pods",
	}
}

func buildAllHealthyResponses() map[string]mockResponse {
	paths := diagnosticPaths()
	return map[string]mockResponse{
		paths["catalogSource"]: {body: buildHealthyCatalogSource()},
		paths["csvList"]:       {body: buildHealthyCSVList()},
		paths["subscription"]:  {body: buildHealthySubscription()},
		paths["pullSecret"]:    {body: buildValidPullSecret()},
		paths["idms"]:          {body: buildIDMSList()},
		paths["vwh"]:           {body: buildEmptyWebhookList()},
		paths["mwh"]:           {body: buildEmptyWebhookList()},
		paths["installPlans"]:  {body: buildInstallPlanList()},
		paths["nodes"]:         {body: buildNodeList(3)},
		paths["opPods"]:        {body: buildHealthyPodList()},
		paths["appPods"]:       {body: buildEmptyList()},
		"/api/v1/namespaces":   {body: buildEmptyList()},
	}
}

// --- Test cases ---

func TestDiagnostics_AllHealthy(t *testing.T) {
	client, cleanup := newMockClient(buildAllHealthyResponses())
	defer cleanup()

	resp, err := RunDiagnostics(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(resp.Problems) != 0 {
		t.Errorf("expected 0 problems, got %d:", len(resp.Problems))
		for _, p := range resp.Problems {
			t.Errorf("  - [%s] %s: %s", p.Severity, p.ID, p.Title)
		}
	}

	if len(resp.Checks) != len(diagnosticChecks) {
		t.Errorf("expected %d checks, got %d", len(diagnosticChecks), len(resp.Checks))
		for _, ch := range resp.Checks {
			t.Logf("  check: %s = %s (%s)", ch.Name, ch.Status, ch.Detail)
		}
	}

	for _, ch := range resp.Checks {
		if ch.Status != "pass" {
			t.Errorf("check %q should be 'pass', got %q (%s)", ch.Name, ch.Status, ch.Detail)
		}
	}
}

func TestDiagnostics_CatalogSourceFailing(t *testing.T) {
	responses := buildAllHealthyResponses()
	paths := diagnosticPaths()

	failingCS := map[string]interface{}{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "CatalogSource",
		"metadata":   map[string]interface{}{"name": CatalogName, "namespace": CatalogNS},
		"spec":       map[string]interface{}{"image": "quay.io/rhoai/rhoai-fbc-fragment:bad"},
		"status": map[string]interface{}{
			"connectionState": map[string]interface{}{"lastObservedState": "TRANSIENT_FAILURE"},
		},
	}
	failingCSJSON, _ := json.Marshal(failingCS)
	responses[paths["catalogSource"]] = mockResponse{body: string(failingCSJSON)}

	client, cleanup := newMockClient(responses)
	defer cleanup()

	resp, err := RunDiagnostics(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, p := range resp.Problems {
		if p.ID == "catalog-transient-failure" {
			found = true
			if p.Severity != "warning" {
				t.Errorf("expected severity 'warning', got %q", p.Severity)
			}
			if p.Title == "" {
				t.Error("expected non-empty title")
			}
			if p.Fix == "" {
				t.Error("expected non-empty fix guidance")
			}
			if p.LearnMore == "" {
				t.Error("expected non-empty learnMore")
			}
			break
		}
	}
	if !found {
		t.Error("expected to find problem with ID 'catalog-transient-failure'")
		for _, p := range resp.Problems {
			t.Logf("  found problem: %s", p.ID)
		}
	}

	for _, ch := range resp.Checks {
		if ch.Name == "Catalog health" {
			if ch.Status != "fail" {
				t.Errorf("expected catalog check status 'fail', got %q", ch.Status)
			}
			break
		}
	}
}

func TestDiagnostics_NoCSV(t *testing.T) {
	responses := buildAllHealthyResponses()
	paths := diagnosticPaths()

	emptyCSV := map[string]interface{}{"items": []interface{}{}}
	emptyCSVJSON, _ := json.Marshal(emptyCSV)
	responses[paths["csvList"]] = mockResponse{body: string(emptyCSVJSON)}

	client, cleanup := newMockClient(responses)
	defer cleanup()

	resp, err := RunDiagnostics(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, p := range resp.Problems {
		if p.ID == "csv-not-found" {
			found = true
			if p.Severity != "critical" {
				t.Errorf("expected severity 'critical', got %q", p.Severity)
			}
			break
		}
	}
	if !found {
		t.Error("expected to find problem with ID 'csv-not-found'")
		for _, p := range resp.Problems {
			t.Logf("  found problem: %s", p.ID)
		}
	}

	for _, ch := range resp.Checks {
		if ch.Name == "Operator installed" {
			if ch.Status != "fail" {
				t.Errorf("expected operator check status 'fail', got %q", ch.Status)
			}
			break
		}
	}
}

func TestDiagnostics_StaleWebhooks(t *testing.T) {
	responses := buildAllHealthyResponses()
	paths := diagnosticPaths()

	emptyCSV := map[string]interface{}{"items": []interface{}{}}
	emptyCSVJSON, _ := json.Marshal(emptyCSV)
	responses[paths["csvList"]] = mockResponse{body: string(emptyCSVJSON)}

	vwhList := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{
				"metadata": map[string]interface{}{
					"name":   "opendatahub-operator-validating-webhook",
					"labels": map[string]interface{}{"olm.owner": "rhods-operator"},
				},
				// The backing Service does not exist (the mock answers 404).
				"webhooks": []interface{}{
					map[string]interface{}{
						"clientConfig": map[string]interface{}{
							"service": map[string]interface{}{"name": "rhods-operator-service", "namespace": "redhat-ods-operator"},
						},
					},
				},
			},
		},
	}
	vwhJSON, _ := json.Marshal(vwhList)
	responses[paths["vwh"]] = mockResponse{body: string(vwhJSON)}

	client, cleanup := newMockClient(responses)
	defer cleanup()

	resp, err := RunDiagnostics(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, p := range resp.Problems {
		if p.ID == "stale-webhooks" {
			found = true
			// failurePolicy defaults to Fail, so the webhook blocks requests.
			if p.Severity != "critical" {
				t.Errorf("expected severity 'critical', got %q", p.Severity)
			}
			if !p.AutoFixable {
				t.Error("expected autoFixable=true for stale webhooks")
			}
			if len(p.Evidence) == 0 {
				t.Error("expected evidence to list webhook names")
			}
			break
		}
	}
	if !found {
		t.Error("expected to find problem with ID 'stale-webhooks'")
		for _, p := range resp.Problems {
			t.Logf("  found problem: %s", p.ID)
		}
	}

	for _, ch := range resp.Checks {
		if ch.Name == "Stale webhooks" {
			if ch.Status != "fail" {
				t.Errorf("expected stale webhooks check status 'fail', got %q", ch.Status)
			}
			break
		}
	}
}

func TestDiagnostics_PullSecretMissing(t *testing.T) {
	responses := buildAllHealthyResponses()
	paths := diagnosticPaths()

	// Override: 404 for pull secret
	responses[paths["pullSecret"]] = mockResponse{
		body:       `{"kind":"Status","status":"Failure","message":"not found","reason":"NotFound","code":404}`,
		statusCode: 404,
	}

	client, cleanup := newMockClient(responses)
	defer cleanup()

	resp, err := RunDiagnostics(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, p := range resp.Problems {
		if p.ID == "pull-secret-missing" {
			found = true
			if p.Severity != "critical" {
				t.Errorf("expected severity 'critical', got %q", p.Severity)
			}
			break
		}
	}
	if !found {
		t.Error("expected to find problem with ID 'pull-secret-missing'")
		for _, p := range resp.Problems {
			t.Logf("  found problem: %s", p.ID)
		}
	}

	for _, ch := range resp.Checks {
		if ch.Name == "Pull secret" {
			if ch.Status != "fail" {
				t.Errorf("expected pull secret check status 'fail', got %q", ch.Status)
			}
			break
		}
	}
}

func TestDiagnostics_InstallPlanFailed(t *testing.T) {
	responses := buildAllHealthyResponses()
	paths := diagnosticPaths()

	failedIP := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{
				"metadata": map[string]interface{}{
					"name":              "install-xyz",
					"creationTimestamp": "2026-06-01T00:00:00Z",
				},
				"spec":   map[string]interface{}{"approved": true},
				"status": map[string]interface{}{"phase": "Failed"},
			},
		},
	}
	failedIPJSON, _ := json.Marshal(failedIP)
	responses[paths["installPlans"]] = mockResponse{body: string(failedIPJSON)}

	client, cleanup := newMockClient(responses)
	defer cleanup()

	resp, err := RunDiagnostics(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, p := range resp.Problems {
		if p.ID == "installplan-failed" {
			found = true
			if p.Severity != "critical" {
				t.Errorf("expected severity 'critical', got %q", p.Severity)
			}
			if !p.AutoFixable {
				t.Error("expected autoFixable=true")
			}
			if p.AutoFixAction != "delete-stale-installplans" {
				t.Errorf("expected autoFixAction 'delete-stale-installplans', got %q", p.AutoFixAction)
			}
			break
		}
	}
	if !found {
		t.Error("expected to find problem with ID 'installplan-failed'")
		for _, p := range resp.Problems {
			t.Logf("  found problem: %s", p.ID)
		}
	}
}

func TestDiagnostics_ApplyFix_DeleteStaleWebhooks(t *testing.T) {
	paths := diagnosticPaths()

	vwhList := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{
				"metadata": map[string]interface{}{
					"name":   "opendatahub-operator-validating-webhook",
					"labels": map[string]interface{}{"olm.owner": "rhods-operator"},
				},
				"webhooks": []interface{}{
					map[string]interface{}{
						"clientConfig": map[string]interface{}{
							"service": map[string]interface{}{
								"name":      "opendatahub-operator-webhook-service",
								"namespace": "redhat-ods-operator",
							},
						},
					},
				},
			},
		},
	}
	vwhJSON, _ := json.Marshal(vwhList)

	mwhList := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{
				"metadata": map[string]interface{}{
					"name":   "rhods-mutating-webhook",
					"labels": map[string]interface{}{"olm.owner": "rhods-operator"},
				},
				"webhooks": []interface{}{
					map[string]interface{}{
						"clientConfig": map[string]interface{}{
							"service": map[string]interface{}{
								"name":      "rhods-webhook-service",
								"namespace": "redhat-ods-operator",
							},
						},
					},
				},
			},
		},
	}
	mwhJSON, _ := json.Marshal(mwhList)

	svc1Path := "/api/v1/namespaces/redhat-ods-operator/services/opendatahub-operator-webhook-service"
	svc2Path := "/api/v1/namespaces/redhat-ods-operator/services/rhods-webhook-service"
	activityCM := "/api/v1/namespaces/rhoai-nightly-updater/configmaps/rhoai-nightly-updater-activity"

	responses := map[string]mockResponse{
		paths["vwh"]: {body: string(vwhJSON)},
		paths["mwh"]: {body: string(mwhJSON)},
		svc1Path: {
			body:       `{"kind":"Status","status":"Failure","message":"not found","reason":"NotFound","code":404}`,
			statusCode: 404,
		},
		svc2Path: {
			body:       `{"kind":"Status","status":"Failure","message":"not found","reason":"NotFound","code":404}`,
			statusCode: 404,
		},
		activityCM: {
			body:       `{"kind":"Status","status":"Failure","message":"not found","reason":"NotFound","code":404}`,
			statusCode: 404,
		},
		// The owning CSV is gone, so OLM will not recreate the webhooks.
		"/apis/operators.coreos.com/v1alpha1/namespaces/redhat-ods-operator/clusterserviceversions/rhods-operator": {
			body:       `{"kind":"Status","status":"Failure","message":"not found","reason":"NotFound","code":404}`,
			statusCode: 404,
		},
		"DELETE /apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations/opendatahub-operator-validating-webhook": {
			body: `{"kind":"Status","status":"Success"}`,
		},
		"DELETE /apis/admissionregistration.k8s.io/v1/mutatingwebhookconfigurations/rhods-mutating-webhook": {
			body: `{"kind":"Status","status":"Success"}`,
		},
	}

	client, records, cleanup := newRecordingMockClient(responses)
	defer cleanup()

	result, err := ApplyFix(client, "delete-stale-webhooks")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success, got failure: %s", result.Message)
	}

	var deletedPaths []string
	for _, rec := range *records {
		if rec.Method == "DELETE" {
			deletedPaths = append(deletedPaths, rec.Path)
		}
	}
	sort.Strings(deletedPaths)
	wantDeleted := []string{
		"/apis/admissionregistration.k8s.io/v1/mutatingwebhookconfigurations/rhods-mutating-webhook",
		"/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations/opendatahub-operator-validating-webhook",
	}
	if strings.Join(deletedPaths, ",") != strings.Join(wantDeleted, ",") {
		t.Errorf("deleted %v, want %v", deletedPaths, wantDeleted)
	}

	if !strings.Contains(result.Message, "Deleted 2") {
		t.Errorf("expected message to report 2 deletions, got: %s", result.Message)
	}
}

func TestDiagnostics_ApplyFix_InvalidProblemID(t *testing.T) {
	client, cleanup := newMockClient(map[string]mockResponse{})
	defer cleanup()

	result, err := ApplyFix(client, "nonexistent-problem-id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Error("expected Success=false for unknown problem ID")
	}
	if !strings.Contains(result.Message, "Unknown fix action") {
		t.Errorf("expected message to mention 'Unknown fix action', got: %s", result.Message)
	}
}
