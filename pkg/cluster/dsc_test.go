package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestCreateDefaultDSC_AlreadyExists verifies that CreateDefaultDSC returns success
// with an "already exists" message when a DSC is already present.
func TestCreateDefaultDSC_AlreadyExists(t *testing.T) {
	// Mock DSC list response with one existing DSC
	dscList := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{
				"metadata": map[string]interface{}{
					"name": "default-dsc",
				},
				"spec": map[string]interface{}{
					"components": map[string]interface{}{
						"dashboard": map[string]interface{}{
							"managementState": "Managed",
						},
					},
				},
			},
		},
	}
	dscListJSON, _ := json.Marshal(dscList)

	dscListPathV2 := "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters"
	dscListPathV1 := "/apis/datasciencecluster.opendatahub.io/v1/datascienceclusters"

	client, records, cleanup := newRecordingMockClient(map[string]mockResponse{
		dscListPathV2: {body: string(dscListJSON)},
		dscListPathV1: {body: string(dscListJSON)},
	})
	defer cleanup()

	result, err := CreateDefaultDSC(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success, got failure: %s", result.Message)
	}
	if result.Message != "DataScienceCluster already exists" {
		t.Errorf("expected 'already exists' message, got: %s", result.Message)
	}

	// Verify no apply was attempted (no PATCH or POST requests)
	for _, rec := range *records {
		if rec.Method == "PATCH" || rec.Method == "POST" {
			t.Errorf("expected no apply attempt, but found %s request to %s", rec.Method, rec.Path)
		}
	}
}

// TestCreateDefaultDSC_CreatesSuccessfully verifies that CreateDefaultDSC creates
// a new DSC when none exists.
func TestCreateDefaultDSC_CreatesSuccessfully(t *testing.T) {
	mockDSCSamples(t, func(*http.Request) (int, string) { return 200, testDSCSample })
	// Mock empty DSC list (no existing DSC)
	emptyList := map[string]interface{}{
		"items": []interface{}{},
	}
	emptyListJSON, _ := json.Marshal(emptyList)

	// Mock successful apply response
	applyResponse := map[string]interface{}{
		"apiVersion": "datasciencecluster.opendatahub.io/v2",
		"kind":       "DataScienceCluster",
		"metadata": map[string]interface{}{
			"name": "default-dsc",
		},
	}
	applyResponseJSON, _ := json.Marshal(applyResponse)

	dscListPathV2 := "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters"
	dscListPathV1 := "/apis/datasciencecluster.opendatahub.io/v1/datascienceclusters"
	dscApplyPath := "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/default-dsc"

	// The actual implementation uses c.apply() which internally uses PATCH with SSA params
	client, records, cleanup := newRecordingMockClient(map[string]mockResponse{
		namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, ""): csvListMock("3.6.0"),
		dscListPathV2: {body: string(emptyListJSON)},
		dscListPathV1: {body: string(emptyListJSON)},
		dscApplyPath:  {body: string(applyResponseJSON)},
	})
	defer cleanup()

	result, err := CreateDefaultDSC(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success, got failure: %s\nlogs: %v", result.Message, result.Logs)
	}

	// Verify apply was called (c.apply uses PATCH internally but path may have query params)
	var applyCalled bool
	for _, rec := range *records {
		if rec.Method == "PATCH" && strings.HasPrefix(rec.Path, dscApplyPath) {
			applyCalled = true
			break
		}
	}
	if !applyCalled {
		t.Error("expected PATCH request to create DSC, but none was made")
	}
}

// TestCreateDefaultDSC_CRDNotInstalled verifies that CreateDefaultDSC returns
// a meaningful error when the DSC CRD doesn't exist (operator not installed).
func TestCreateDefaultDSC_CRDNotInstalled(t *testing.T) {
	// Mock 404 response (CRD not found)
	notFoundResponse := `{"kind":"Status","status":"Failure","message":"the server could not find the requested resource","reason":"NotFound","code":404}`

	dscListPathV2 := "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters"
	dscListPathV1 := "/apis/datasciencecluster.opendatahub.io/v1/datascienceclusters"

	client, cleanup := newMockClient(map[string]mockResponse{
		dscListPathV2: {body: notFoundResponse, statusCode: 404},
		dscListPathV1: {body: notFoundResponse, statusCode: 404},
	})
	defer cleanup()

	result, err := CreateDefaultDSC(client)
	if err != nil {
		t.Fatalf("CreateDefaultDSC should not return a Go error, got: %v", err)
	}
	if result.Success {
		t.Error("expected Success=false when CRD is not installed")
	}
	if result.ErrorCode != "prerequisites" {
		t.Errorf("expected errorCode 'prerequisites', got %q", result.ErrorCode)
	}
	if result.Message == "" {
		t.Error("expected non-empty error message")
	}
	// Message should mention that the operator needs to be installed
	if result.Message != "DataScienceCluster CRD not found. Install the RHOAI operator first." {
		t.Errorf("unexpected error message: %s", result.Message)
	}
}

// TestCreateDefaultDSC_ApplyFailure verifies that CreateDefaultDSC handles
// API errors during the apply operation gracefully.
func TestCreateDefaultDSC_ApplyFailure(t *testing.T) {
	mockDSCSamples(t, func(*http.Request) (int, string) { return 200, testDSCSample })
	// Mock empty DSC list (no existing DSC)
	emptyList := map[string]interface{}{
		"items": []interface{}{},
	}
	emptyListJSON, _ := json.Marshal(emptyList)

	// Mock 500 error on apply
	errorResponse := `{"kind":"Status","status":"Failure","message":"internal server error","reason":"InternalError","code":500}`

	dscListPathV2 := "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters"
	dscListPathV1 := "/apis/datasciencecluster.opendatahub.io/v1/datascienceclusters"
	dscApplyPath := "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/default-dsc"

	client, cleanup := newMockClient(map[string]mockResponse{
		namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, ""): csvListMock("3.6.0"),
		dscListPathV2: {body: string(emptyListJSON)},
		dscListPathV1: {body: string(emptyListJSON)},
		dscApplyPath:  {body: errorResponse, statusCode: 500},
	})
	defer cleanup()

	result, err := CreateDefaultDSC(client)
	if err != nil {
		t.Fatalf("CreateDefaultDSC should not return a Go error, got: %v", err)
	}
	if result.Success {
		t.Error("expected Success=false when apply fails")
	}
	// errorCodeFromK8sErr returns empty string for 500 errors (not specifically handled)
	// This is acceptable - just verify the operation failed with a message
	if result.Message == "" {
		t.Error("expected non-empty error message when apply fails")
	}
	if !strings.Contains(result.Message, "Failed to create DataScienceCluster") {
		t.Errorf("expected error message about creation failure, got: %s", result.Message)
	}
}

// TestGetStatus_IncludesDSCExists verifies that GetStatus correctly populates
// the DSCExists field.
func TestGetStatus_IncludesDSCExists(t *testing.T) {
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

	// DSC list with one item (DSC exists)
	dscList := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{
				"metadata": map[string]interface{}{
					"name": "default-dsc",
				},
			},
		},
	}
	dscListJSON, _ := json.Marshal(dscList)

	// InstallPlan list response
	ipListResponse := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{
				"metadata": map[string]interface{}{
					"name":              "install-plan-test",
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
	dscListPathV2 := "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters"
	dscListPathV1 := "/apis/datasciencecluster.opendatahub.io/v1/datascienceclusters"

	client, cleanup := newMockClient(map[string]mockResponse{
		subPath:         {body: string(subJSON)},
		csvListPath:     {body: string(csvJSON)},
		csPath:          {body: string(csJSON)},
		pullSecretPath:  {body: string(secretJSON)},
		idmsPath:        {body: string(idmsJSON)},
		ipListPath:      {body: string(ipListJSON)},
		catalogPodsPath: {body: string(podListJSON)},
		activityPath:    {body: string(activityJSON)},
		dscListPathV2:   {body: string(dscListJSON)},
		dscListPathV1:   {body: string(dscListJSON)},
	})
	defer cleanup()

	status, err := GetStatus(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify DSCExists is set to true
	if !status.DSCExists {
		t.Error("expected DSCExists to be true when DSC list has items")
	}
}

// TestGetStatus_DSCNotExists verifies that GetStatus correctly reports when no DSC exists.
func TestGetStatus_DSCNotExists(t *testing.T) {
	// Build minimal mock responses for GetStatus
	subResponse := map[string]interface{}{
		"apiVersion": "operators.coreos.com/v1alpha1", "kind": "Subscription",
		"metadata": map[string]interface{}{"name": SubName, "namespace": SubNS},
		"spec":     map[string]interface{}{"source": CatalogName, "channel": "fast"},
		"status": map[string]interface{}{
			"state": "AtLatestKnown",
		},
	}
	subJSON, _ := json.Marshal(subResponse)

	// Empty DSC list (no DSC exists)
	emptyDSCList := map[string]interface{}{
		"items": []interface{}{},
	}
	emptyDSCListJSON, _ := json.Marshal(emptyDSCList)

	ipListResponse := map[string]interface{}{"items": []interface{}{}}
	ipListJSON, _ := json.Marshal(ipListResponse)

	podList := map[string]interface{}{"items": []interface{}{}}
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
		"data": map[string]interface{}{".dockerconfigjson": "e30="},
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
	dscListPathV2 := "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters"
	dscListPathV1 := "/apis/datasciencecluster.opendatahub.io/v1/datascienceclusters"

	client, cleanup := newMockClient(map[string]mockResponse{
		subPath:         {body: string(subJSON)},
		csvListPath:     {body: string(csvJSON)},
		csPath:          {body: string(csJSON)},
		pullSecretPath:  {body: string(secretJSON)},
		idmsPath:        {body: string(idmsJSON)},
		ipListPath:      {body: string(ipListJSON)},
		catalogPodsPath: {body: string(podListJSON)},
		activityPath:    {body: string(activityJSON)},
		dscListPathV2:   {body: string(emptyDSCListJSON)},
		dscListPathV1:   {body: string(emptyDSCListJSON)},
	})
	defer cleanup()

	status, err := GetStatus(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify DSCExists is set to false
	if status.DSCExists {
		t.Error("expected DSCExists to be false when DSC list is empty")
	}
}

// TestGetStatus_DSCCRDNotInstalled verifies that GetStatus handles 404 errors
// gracefully when the DSC CRD is not installed.
func TestGetStatus_DSCCRDNotInstalled(t *testing.T) {
	// Build minimal mock responses for GetStatus
	subResponse := map[string]interface{}{
		"apiVersion": "operators.coreos.com/v1alpha1", "kind": "Subscription",
		"metadata": map[string]interface{}{"name": SubName, "namespace": SubNS},
		"spec":     map[string]interface{}{"source": CatalogName, "channel": "fast"},
		"status": map[string]interface{}{
			"state": "AtLatestKnown",
		},
	}
	subJSON, _ := json.Marshal(subResponse)

	// 404 for both v2 and v1 DSC endpoints (CRD not installed)
	notFoundResponse := `{"kind":"Status","status":"Failure","message":"the server could not find the requested resource","reason":"NotFound","code":404}`

	ipListResponse := map[string]interface{}{"items": []interface{}{}}
	ipListJSON, _ := json.Marshal(ipListResponse)

	podList := map[string]interface{}{"items": []interface{}{}}
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
		"data": map[string]interface{}{".dockerconfigjson": "e30="},
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
	dscListPathV2 := "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters"
	dscListPathV1 := "/apis/datasciencecluster.opendatahub.io/v1/datascienceclusters"

	client, cleanup := newMockClient(map[string]mockResponse{
		subPath:         {body: string(subJSON)},
		csvListPath:     {body: string(csvJSON)},
		csPath:          {body: string(csJSON)},
		pullSecretPath:  {body: string(secretJSON)},
		idmsPath:        {body: string(idmsJSON)},
		ipListPath:      {body: string(ipListJSON)},
		catalogPodsPath: {body: string(podListJSON)},
		activityPath:    {body: string(activityJSON)},
		dscListPathV2:   {body: notFoundResponse, statusCode: 404},
		dscListPathV1:   {body: notFoundResponse, statusCode: 404},
	})
	defer cleanup()

	status, err := GetStatus(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify DSCExists is set to false when CRD is not installed
	if status.DSCExists {
		t.Error("expected DSCExists to be false when CRD is not installed (404)")
	}
}
