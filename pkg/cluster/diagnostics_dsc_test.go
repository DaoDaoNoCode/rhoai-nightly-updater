package cluster

import (
	"strings"
	"testing"
)

func TestDisableComponent_PatchesTheExistingDSC(t *testing.T) {
	client, records, cleanup := newRecordingMockClient(map[string]mockResponse{
		"GET /apis/datasciencecluster.opendatahub.io/v2/datascienceclusters": {
			body: `{"items":[{"metadata":{"name":"my-dsc"}}]}`,
		},
		"GET /apis/apps/v1/namespaces/redhat-ods-operator/deployments/rhods-operator": {body: `{"status":{"readyReplicas":3}}`},
		// No component API served, so no module CR (and no finalizer) exists.
		"GET /apis/components.platform.opendatahub.io": {statusCode: 404, body: `{"kind":"Status","status":"Failure","reason":"NotFound","code":404}`},
		"GET /apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/my-dsc": {
			body: `{"spec":{"components":{"llamastackoperator":{"managementState":"Managed"}}}}`,
		},
		"PATCH /apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/my-dsc": {
			body: `{"spec":{"components":{"llamastackoperator":{"managementState":"Removed"}}}}`,
		},
	})
	defer cleanup()
	result, err := ApplyFix(client, "disable-component:llamastackoperator")
	if err != nil || !result.Success {
		t.Fatalf("ApplyFix = %+v, %v", result, err)
	}
	patched := false
	for _, r := range *records {
		if r.Method == "PATCH" {
			if r.Path != "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/my-dsc" {
				t.Fatalf("patched %s", r.Path)
			}
			patched = true
		}
	}
	if !patched {
		t.Fatal("DSC was not patched")
	}
}

func TestDisableComponent_FallsBackToV1API(t *testing.T) {
	client, records, cleanup := newRecordingMockClient(map[string]mockResponse{
		"GET /apis/datasciencecluster.opendatahub.io/v2/datascienceclusters": {
			statusCode: 404, body: `{"kind":"Status","status":"Failure","reason":"NotFound","code":404}`,
		},
		"GET /apis/datasciencecluster.opendatahub.io/v1/datascienceclusters": {
			body: `{"items":[{"metadata":{"name":"rhods"}}]}`,
		},
		"GET /apis/apps/v1/namespaces/redhat-ods-operator/deployments/rhods-operator": {body: `{"status":{"readyReplicas":3}}`},
		// No component API served, so no module CR (and no finalizer) exists.
		"GET /apis/components.platform.opendatahub.io": {statusCode: 404, body: `{"kind":"Status","status":"Failure","reason":"NotFound","code":404}`},
		"GET /apis/datasciencecluster.opendatahub.io/v1/datascienceclusters/rhods": {
			body: `{"spec":{"components":{"ray":{"managementState":"Managed"}}}}`,
		},
		"PATCH /apis/datasciencecluster.opendatahub.io/v1/datascienceclusters/rhods": {
			body: `{"spec":{"components":{"ray":{"managementState":"Removed"}}}}`,
		},
	})
	defer cleanup()
	result, err := ApplyFix(client, "disable-component:ray")
	if err != nil || !result.Success {
		t.Fatalf("ApplyFix = %+v, %v", result, err)
	}
	for _, r := range *records {
		if r.Method == "PATCH" && r.Path != "/apis/datasciencecluster.opendatahub.io/v1/datascienceclusters/rhods" {
			t.Fatalf("patched %s", r.Path)
		}
	}
}

// The module CR check must not be skipped when discovery fails: a hidden
// finalizer could leave the component stuck in deletion.
func TestDisableComponent_DiscoveryErrorBlocks(t *testing.T) {
	client, records, cleanup := newRecordingMockClient(map[string]mockResponse{
		"GET /apis/datasciencecluster.opendatahub.io/v2/datascienceclusters": {
			body: `{"items":[{"metadata":{"name":"my-dsc"}}]}`,
		},
		"GET /apis/apps/v1/namespaces/redhat-ods-operator/deployments/rhods-operator": {body: `{"status":{"readyReplicas":3}}`},
		"GET /apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/my-dsc": {
			body: `{"spec":{"components":{"ray":{"managementState":"Managed"}}}}`,
		},
		"GET /apis/components.platform.opendatahub.io": {statusCode: 503, body: `{"kind":"Status","status":"Failure","code":503}`},
	})
	defer cleanup()
	result, err := ApplyFix(client, "disable-component:ray")
	if err != nil || result.Success || result.ErrorCode != "prerequisites" || !strings.Contains(result.Message, "could not read the ray module CR") {
		t.Fatalf("ApplyFix = %+v, %v", result, err)
	}
	for _, r := range *records {
		if r.Method == "PATCH" {
			t.Fatalf("patched %s although the module CR could not be checked", r.Path)
		}
	}
}

func TestDisableComponent_NoDSC(t *testing.T) {
	client, records, cleanup := newRecordingMockClient(map[string]mockResponse{
		"GET /apis/datasciencecluster.opendatahub.io/v2/datascienceclusters": {body: `{"items":[]}`},
	})
	defer cleanup()
	result, err := ApplyFix(client, "disable-component:ray")
	if err != nil || result.Success || !strings.Contains(result.Message, "no DataScienceCluster") {
		t.Fatalf("ApplyFix = %+v, %v", result, err)
	}
	for _, r := range *records {
		if r.Method == "PATCH" {
			t.Fatalf("unexpected patch %s", r.Path)
		}
	}
}

func TestForceDeleteComponentIsNotAFixAction(t *testing.T) {
	client, records, cleanup := newRecordingMockClient(map[string]mockResponse{})
	defer cleanup()
	result, err := ApplyFix(client, "force-delete-component:modelsasservice")
	if err != nil || result.Success || result.ErrorCode != "validation" {
		t.Fatalf("ApplyFix = %+v, %v", result, err)
	}
	if len(*records) != 0 {
		t.Fatalf("unknown fix made requests: %v", *records)
	}
}
