package cluster

import (
	"encoding/json"
	"fmt"
	"testing"
)

// --- getClusterVersion tests ---

func TestGetClusterVersion_AllFieldsPresent(t *testing.T) {
	cv := map[string]interface{}{
		"status": map[string]interface{}{
			"desired": map[string]interface{}{
				"version": "4.16.3",
			},
		},
	}
	body, _ := json.Marshal(cv)

	client, cleanup := newMockClient(map[string]mockResponse{
		"/apis/config.openshift.io/v1/clusterversions/version": {
			body: string(body),
		},
	})
	defer cleanup()

	version, err := getClusterVersion(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if version != "4.16.3" {
		t.Errorf("expected version '4.16.3', got %q", version)
	}
}

func TestGetClusterVersion_NilStatus(t *testing.T) {
	// status field is missing entirely
	cv := map[string]interface{}{
		"apiVersion": "config.openshift.io/v1",
		"kind":       "ClusterVersion",
		"metadata":   map[string]interface{}{"name": "version"},
	}
	body, _ := json.Marshal(cv)

	client, cleanup := newMockClient(map[string]mockResponse{
		"/apis/config.openshift.io/v1/clusterversions/version": {
			body: string(body),
		},
	})
	defer cleanup()

	version, err := getClusterVersion(client)
	if err != nil {
		t.Fatalf("should not error on missing status, got: %v", err)
	}
	if version != "" {
		t.Errorf("expected empty version when status is nil, got %q", version)
	}
}

func TestGetClusterVersion_StatusPresentButDesiredNil(t *testing.T) {
	// status exists but desired is missing
	cv := map[string]interface{}{
		"status": map[string]interface{}{
			"conditions": []interface{}{},
		},
	}
	body, _ := json.Marshal(cv)

	client, cleanup := newMockClient(map[string]mockResponse{
		"/apis/config.openshift.io/v1/clusterversions/version": {
			body: string(body),
		},
	})
	defer cleanup()

	version, err := getClusterVersion(client)
	if err != nil {
		t.Fatalf("should not error on missing desired, got: %v", err)
	}
	if version != "" {
		t.Errorf("expected empty version when desired is nil, got %q", version)
	}
}

func TestGetClusterVersion_DesiredPresentButVersionEmpty(t *testing.T) {
	// desired exists but version key is absent
	cv := map[string]interface{}{
		"status": map[string]interface{}{
			"desired": map[string]interface{}{
				"image": "quay.io/openshift-release-dev/ocp-release:4.16.3",
			},
		},
	}
	body, _ := json.Marshal(cv)

	client, cleanup := newMockClient(map[string]mockResponse{
		"/apis/config.openshift.io/v1/clusterversions/version": {
			body: string(body),
		},
	})
	defer cleanup()

	version, err := getClusterVersion(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if version != "" {
		t.Errorf("expected empty version when version key is missing, got %q", version)
	}
}

func TestGetClusterVersion_StatusIsWrongType(t *testing.T) {
	// status is a string instead of a map -- should not panic
	cv := map[string]interface{}{
		"status": "not-a-map",
	}
	body, _ := json.Marshal(cv)

	client, cleanup := newMockClient(map[string]mockResponse{
		"/apis/config.openshift.io/v1/clusterversions/version": {
			body: string(body),
		},
	})
	defer cleanup()

	version, err := getClusterVersion(client)
	if err != nil {
		t.Fatalf("should not error on wrong-typed status, got: %v", err)
	}
	if version != "" {
		t.Errorf("expected empty version when status is wrong type, got %q", version)
	}
}

func TestGetClusterVersion_DesiredIsWrongType(t *testing.T) {
	// desired is a string instead of a map -- should not panic
	cv := map[string]interface{}{
		"status": map[string]interface{}{
			"desired": "not-a-map",
		},
	}
	body, _ := json.Marshal(cv)

	client, cleanup := newMockClient(map[string]mockResponse{
		"/apis/config.openshift.io/v1/clusterversions/version": {
			body: string(body),
		},
	})
	defer cleanup()

	version, err := getClusterVersion(client)
	if err != nil {
		t.Fatalf("should not error on wrong-typed desired, got: %v", err)
	}
	if version != "" {
		t.Errorf("expected empty version when desired is wrong type, got %q", version)
	}
}

func TestGetClusterVersion_APIError(t *testing.T) {
	// API returns 500 -- should propagate error
	client, cleanup := newMockClient(map[string]mockResponse{
		"/apis/config.openshift.io/v1/clusterversions/version": {
			body:       `{"kind":"Status","status":"Failure","message":"internal error","reason":"InternalError","code":500}`,
			statusCode: 500,
		},
	})
	defer cleanup()

	_, err := getClusterVersion(client)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestGetClusterVersion_InvalidJSON(t *testing.T) {
	client, cleanup := newMockClient(map[string]mockResponse{
		"/apis/config.openshift.io/v1/clusterversions/version": {
			body: `not valid json`,
		},
	})
	defer cleanup()

	_, err := getClusterVersion(client)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// --- getCatalogSource tests ---

func TestGetCatalogSource_404ReturnsNotExists(t *testing.T) {
	csPath := fmt.Sprintf(
		"/apis/operators.coreos.com/v1alpha1/namespaces/%s/catalogsources/%s",
		CatalogNS, CatalogName,
	)
	client, cleanup := newMockClient(map[string]mockResponse{
		csPath: {
			body:       `{"kind":"Status","status":"Failure","message":"not found","reason":"NotFound","code":404}`,
			statusCode: 404,
		},
	})
	defer cleanup()

	cs, err := getCatalogSource(client)
	if err != nil {
		t.Fatalf("404 should not produce an error, got: %v", err)
	}
	if cs.Exists {
		t.Error("expected Exists to be false for 404")
	}
	if cs.Name != CatalogName {
		t.Errorf("expected Name %q, got %q", CatalogName, cs.Name)
	}
}

func TestGetCatalogSource_NonNotFoundErrorPropagates(t *testing.T) {
	csPath := fmt.Sprintf(
		"/apis/operators.coreos.com/v1alpha1/namespaces/%s/catalogsources/%s",
		CatalogNS, CatalogName,
	)
	client, cleanup := newMockClient(map[string]mockResponse{
		csPath: {
			body:       `{"kind":"Status","status":"Failure","message":"forbidden","reason":"Forbidden","code":403}`,
			statusCode: 403,
		},
	})
	defer cleanup()

	cs, err := getCatalogSource(client)
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
	if cs.Exists {
		t.Error("expected Exists to be false on error")
	}
}

func TestGetCatalogSource_ExistsWithState(t *testing.T) {
	csBody := map[string]interface{}{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "CatalogSource",
		"metadata":   map[string]interface{}{"name": CatalogName},
		"spec": map[string]interface{}{
			"image": "quay.io/rhoai/rhoai-fbc-fragment:nightly-4.16-20240101",
		},
		"status": map[string]interface{}{
			"connectionState": map[string]interface{}{
				"lastObservedState": "READY",
			},
		},
	}
	body, _ := json.Marshal(csBody)

	csPath := fmt.Sprintf(
		"/apis/operators.coreos.com/v1alpha1/namespaces/%s/catalogsources/%s",
		CatalogNS, CatalogName,
	)
	client, cleanup := newMockClient(map[string]mockResponse{
		csPath: {
			body: string(body),
		},
	})
	defer cleanup()

	cs, err := getCatalogSource(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cs.Exists {
		t.Error("expected Exists to be true")
	}
	if cs.Image != "quay.io/rhoai/rhoai-fbc-fragment:nightly-4.16-20240101" {
		t.Errorf("unexpected Image: %s", cs.Image)
	}
	if cs.State != "READY" {
		t.Errorf("expected State 'READY', got %q", cs.State)
	}
}

func TestGetCatalogSource_ExistsMissingStatus(t *testing.T) {
	csBody := map[string]interface{}{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "CatalogSource",
		"metadata":   map[string]interface{}{"name": CatalogName},
		"spec": map[string]interface{}{
			"image": "quay.io/rhoai/rhoai-fbc-fragment:nightly-4.16-20240101",
		},
	}
	body, _ := json.Marshal(csBody)

	csPath := fmt.Sprintf(
		"/apis/operators.coreos.com/v1alpha1/namespaces/%s/catalogsources/%s",
		CatalogNS, CatalogName,
	)
	client, cleanup := newMockClient(map[string]mockResponse{
		csPath: {
			body: string(body),
		},
	})
	defer cleanup()

	cs, err := getCatalogSource(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cs.Exists {
		t.Error("expected Exists to be true")
	}
	if cs.State != "Unknown" {
		t.Errorf("expected State 'Unknown' when status is missing, got %q", cs.State)
	}
}

// --- getInstallPlan tests ---

func TestGetInstallPlan_ReturnsCorrectData(t *testing.T) {
	// InstallPlan list with one rhods-operator entry
	ipListResponse := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{
				"metadata": map[string]interface{}{
					"name":              "install-plan-xyz789",
					"namespace":         SubNS,
					"creationTimestamp": "2026-01-01T00:00:00Z",
				},
				"spec": map[string]interface{}{
					"approved":                   true,
					"clusterServiceVersionNames": []interface{}{"rhods-operator.3.5.0-ea.1"},
				},
				"status": map[string]interface{}{"phase": "Complete"},
			},
		},
	}
	ipListJSON, _ := json.Marshal(ipListResponse)

	ipListPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/installplans", SubNS)

	client, cleanup := newMockClient(map[string]mockResponse{
		ipListPath: {body: string(ipListJSON)},
	})
	defer cleanup()

	ip, err := getInstallPlan(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip == nil {
		t.Fatal("expected non-nil InstallPlanInfo")
	}
	if ip.Name != "install-plan-xyz789" {
		t.Errorf("expected Name 'install-plan-xyz789', got %q", ip.Name)
	}
	if ip.Phase != "Complete" {
		t.Errorf("expected Phase 'Complete', got %q", ip.Phase)
	}
	if !ip.Approved {
		t.Error("expected Approved to be true")
	}
}

func TestGetInstallPlan_NoRhodsPlans(t *testing.T) {
	// InstallPlan list with entries that don't match rhods-operator
	ipListResponse := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{
				"metadata": map[string]interface{}{
					"name":              "install-plan-other",
					"namespace":         SubNS,
					"creationTimestamp": "2026-01-01T00:00:00Z",
				},
				"spec": map[string]interface{}{
					"approved":                   true,
					"clusterServiceVersionNames": []interface{}{"some-other-operator.1.0.0"},
				},
				"status": map[string]interface{}{"phase": "Complete"},
			},
		},
	}
	ipListJSON, _ := json.Marshal(ipListResponse)

	ipListPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/installplans", SubNS)

	client, cleanup := newMockClient(map[string]mockResponse{
		ipListPath: {body: string(ipListJSON)},
	})
	defer cleanup()

	ip, err := getInstallPlan(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip != nil {
		t.Errorf("expected nil when no rhods-operator plans, got %+v", ip)
	}
}

func TestGetInstallPlan_ListEmpty(t *testing.T) {
	// InstallPlan list with no entries
	ipListResponse := map[string]interface{}{
		"items": []interface{}{},
	}
	ipListJSON, _ := json.Marshal(ipListResponse)

	ipListPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/installplans", SubNS)

	client, cleanup := newMockClient(map[string]mockResponse{
		ipListPath: {body: string(ipListJSON)},
	})
	defer cleanup()

	ip, err := getInstallPlan(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip != nil {
		t.Error("expected nil for empty installplan list")
	}
}

func TestGetInstallPlan_APIError(t *testing.T) {
	// API returns 500 on the installplans list path
	ipListPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/installplans", SubNS)

	client, cleanup := newMockClient(map[string]mockResponse{
		ipListPath: {
			body:       `{"kind":"Status","status":"Failure","message":"internal error","reason":"InternalError","code":500}`,
			statusCode: 500,
		},
	})
	defer cleanup()

	_, err := getInstallPlan(client)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

// --- getCatalogPod tests ---

func TestGetCatalogPod_ReturnsCorrectData(t *testing.T) {
	// Pod list with one catalog pod
	podList := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{
				"metadata": map[string]interface{}{
					"name":      "rhoai-catalog-dev-abc123",
					"namespace": CatalogNS,
				},
				"status": map[string]interface{}{
					"phase": "Running",
					"containerStatuses": []interface{}{
						map[string]interface{}{
							"name":         "registry-server",
							"ready":        true,
							"restartCount": float64(2),
						},
					},
				},
			},
		},
	}
	podListJSON, _ := json.Marshal(podList)

	podsPath := fmt.Sprintf("/api/v1/namespaces/%s/pods", CatalogNS)
	client, cleanup := newMockClient(map[string]mockResponse{
		podsPath: {body: string(podListJSON)},
	})
	defer cleanup()

	cp, err := getCatalogPod(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cp == nil {
		t.Fatal("expected non-nil CatalogPodInfo")
	}
	if cp.Name != "rhoai-catalog-dev-abc123" {
		t.Errorf("expected Name 'rhoai-catalog-dev-abc123', got %q", cp.Name)
	}
	if cp.Phase != "Running" {
		t.Errorf("expected Phase 'Running', got %q", cp.Phase)
	}
	if !cp.Ready {
		t.Error("expected Ready to be true")
	}
	if cp.RestartCount != 2 {
		t.Errorf("expected RestartCount 2, got %d", cp.RestartCount)
	}
}

func TestGetCatalogPod_NoPods(t *testing.T) {
	podList := map[string]interface{}{"items": []interface{}{}}
	podListJSON, _ := json.Marshal(podList)

	podsPath := fmt.Sprintf("/api/v1/namespaces/%s/pods", CatalogNS)
	client, cleanup := newMockClient(map[string]mockResponse{
		podsPath: {body: string(podListJSON)},
	})
	defer cleanup()

	cp, err := getCatalogPod(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cp != nil {
		t.Errorf("expected nil when no catalog pods, got %+v", cp)
	}
}

func TestGetCatalogPod_PodNotReady(t *testing.T) {
	podList := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{
				"metadata": map[string]interface{}{
					"name":      "rhoai-catalog-dev-pending",
					"namespace": CatalogNS,
				},
				"status": map[string]interface{}{
					"phase": "Pending",
					"containerStatuses": []interface{}{
						map[string]interface{}{
							"name":         "registry-server",
							"ready":        false,
							"restartCount": float64(0),
						},
					},
				},
			},
		},
	}
	podListJSON, _ := json.Marshal(podList)

	podsPath := fmt.Sprintf("/api/v1/namespaces/%s/pods", CatalogNS)
	client, cleanup := newMockClient(map[string]mockResponse{
		podsPath: {body: string(podListJSON)},
	})
	defer cleanup()

	cp, err := getCatalogPod(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cp == nil {
		t.Fatal("expected non-nil CatalogPodInfo")
	}
	if cp.Phase != "Pending" {
		t.Errorf("expected Phase 'Pending', got %q", cp.Phase)
	}
	if cp.Ready {
		t.Error("expected Ready to be false")
	}
}

// --- GetStatus tests with installPlan and catalogPod ---

func TestGetStatus_IncludesInstallPlanAndCatalogPod(t *testing.T) {
	t.Setenv("STABLE_SOURCE", "redhat-operators")
	t.Setenv("STABLE_CHANNEL", "")
	// Build all required mock responses for GetStatus
	subResponse := map[string]interface{}{
		"apiVersion": "operators.coreos.com/v1alpha1", "kind": "Subscription",
		"metadata": map[string]interface{}{"name": SubName, "namespace": SubNS},
		"spec":     map[string]interface{}{"source": CatalogName, "channel": "fast"},
		"status": map[string]interface{}{
			"state": "AtLatestKnown",
		},
	}
	subJSON, _ := json.Marshal(subResponse)

	// InstallPlan list response (getInstallPlan now lists all installplans)
	ipListResponse := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{
				"metadata": map[string]interface{}{
					"name":              "install-plan-status-test",
					"namespace":         SubNS,
					"creationTimestamp": "2026-01-01T00:00:00Z",
				},
				"spec": map[string]interface{}{
					"approved":                   true,
					"clusterServiceVersionNames": []interface{}{"rhods-operator.3.5.0"},
				},
				"status": map[string]interface{}{"phase": "Complete"},
			},
		},
	}
	ipListJSON, _ := json.Marshal(ipListResponse)

	podList := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{
				"metadata": map[string]interface{}{"name": "rhoai-catalog-dev-pod1", "namespace": CatalogNS},
				"status": map[string]interface{}{
					"phase": "Running",
					"containerStatuses": []interface{}{
						map[string]interface{}{"name": "server", "ready": true, "restartCount": float64(0)},
					},
				},
			},
		},
	}
	podListJSON, _ := json.Marshal(podList)

	csvResponse := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{
				"metadata": map[string]interface{}{"name": "rhods-operator.v3.5.0"},
				"spec":     map[string]interface{}{"displayName": "Red Hat OpenShift AI", "version": "3.5.0"},
				"status":   map[string]interface{}{"phase": "Succeeded"},
			},
		},
	}
	csvJSON, _ := json.Marshal(csvResponse)

	csBody := map[string]interface{}{
		"apiVersion": "operators.coreos.com/v1alpha1", "kind": "CatalogSource",
		"metadata": map[string]interface{}{"name": CatalogName},
		"spec":     map[string]interface{}{"image": "quay.io/rhoai/test:latest"},
		"status":   map[string]interface{}{"connectionState": map[string]interface{}{"lastObservedState": "READY"}},
	}
	csJSON, _ := json.Marshal(csBody)

	secretData := map[string]interface{}{
		"apiVersion": "v1", "kind": "Secret",
		"data": map[string]interface{}{".dockerconfigjson": "e30="}, // base64("{}")
	}
	secretJSON, _ := json.Marshal(secretData)

	idmsData := map[string]interface{}{"items": []interface{}{}}
	idmsJSON, _ := json.Marshal(idmsData)

	activityData := map[string]interface{}{"data": map[string]interface{}{}}
	activityJSON, _ := json.Marshal(activityData)

	subPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/subscriptions/%s", SubNS, SubName)
	csvListPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/clusterserviceversions", SubNS)
	csPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/catalogsources/%s", CatalogNS, CatalogName)
	pullSecretPath := "/api/v1/namespaces/kube-system/secrets/additional-pull-secret"
	idmsPath := "/apis/config.openshift.io/v1/imagedigestmirrorsets"
	ipListPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/installplans", SubNS)
	catalogPodsPath := fmt.Sprintf("/api/v1/namespaces/%s/pods", CatalogNS)
	activityPath := fmt.Sprintf("/api/v1/namespaces/%s/configmaps/rhoai-updater-activity", SubNS)

	client, cleanup := newMockClient(map[string]mockResponse{
		namespacedPath("packages.operators.coreos.com/v1", "packagemanifests", CatalogNS, ""): stableCatalogMock("redhat-operators", "stable-3.5", "3.5.0"),
		subPath:         {body: string(subJSON)},
		csvListPath:     {body: string(csvJSON)},
		csPath:          {body: string(csJSON)},
		pullSecretPath:  {body: string(secretJSON)},
		idmsPath:        {body: string(idmsJSON)},
		ipListPath:      {body: string(ipListJSON)},
		catalogPodsPath: {body: string(podListJSON)},
		activityPath:    {body: string(activityJSON)},
	})
	defer cleanup()

	status, err := GetStatus(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.StableChannel != "stable-3.5" || status.StableVersion != "3.5.0" || status.StableDiscoveryError != "" {
		t.Fatalf("stable channel=%q version=%q error=%q", status.StableChannel, status.StableVersion, status.StableDiscoveryError)
	}

	// Verify installPlan is present
	if status.InstallPlan == nil {
		t.Fatal("expected InstallPlan to be populated")
	}
	if status.InstallPlan.Name != "install-plan-status-test" {
		t.Errorf("expected InstallPlan Name 'install-plan-status-test', got %q", status.InstallPlan.Name)
	}
	if status.InstallPlan.Phase != "Complete" {
		t.Errorf("expected InstallPlan Phase 'Complete', got %q", status.InstallPlan.Phase)
	}
	if !status.InstallPlan.Approved {
		t.Error("expected InstallPlan Approved to be true")
	}

	// Verify catalogPod is present
	if status.CatalogPod == nil {
		t.Fatal("expected CatalogPod to be populated")
	}
	if status.CatalogPod.Name != "rhoai-catalog-dev-pod1" {
		t.Errorf("expected CatalogPod Name 'rhoai-catalog-dev-pod1', got %q", status.CatalogPod.Name)
	}
	if status.CatalogPod.Phase != "Running" {
		t.Errorf("expected CatalogPod Phase 'Running', got %q", status.CatalogPod.Phase)
	}
	if !status.CatalogPod.Ready {
		t.Error("expected CatalogPod Ready to be true")
	}
}
