package cluster

import (
	"strings"
	"testing"
)

func TestDisableComponent_PatchesTheExistingDSC(t *testing.T) {
	f, client := newFakeAPI(t)
	serveToggleDSC(f, "v2", `{"apiVersion":"datasciencecluster.opendatahub.io/v2","metadata":{"name":"my-dsc","uid":"u1"},"spec":{"components":{"llamastackoperator":{"managementState":"Managed"}}}}`, "", "llamastackoperator")
	f.json("GET", "/apis/apps/v1/namespaces/redhat-ods-operator/deployments/rhods-operator", 200, `{"status":{"readyReplicas":3}}`)
	result, err := ApplyFix(client, "disable-component:llamastackoperator")
	if err != nil || !result.Success {
		t.Fatalf("ApplyFix = %+v, %v", result, err)
	}
	const path = "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/my-dsc"
	if got := f.realWrites(); len(got) != 1 || got[0] != "PATCH "+path {
		t.Fatalf("writes = %v", got)
	}
	patches := f.requests("PATCH", path)
	if len(patches) != 2 || !strings.Contains(patches[0].Query, "dryRun=All") {
		t.Fatalf("no dry run first: %+v", patches)
	}
	want := `[{"op":"test","path":"/metadata/uid","value":"u1"},{"op":"test","path":"/metadata/resourceVersion","value":"1"},` +
		`{"op":"test","path":"/spec/components/llamastackoperator/managementState","value":"Managed"},{"op":"replace","path":"/spec/components/llamastackoperator/managementState","value":"Removed"}]`
	if patches[1].Body != want || !strings.Contains(patches[1].Query, "fieldManager="+dscComponentsManager) || !strings.Contains(patches[1].Query, "fieldValidation=Strict") {
		t.Fatalf("patch = %+v", patches[1])
	}
}

func TestDisableComponent_FallsBackToV1API(t *testing.T) {
	f, client := newFakeAPI(t)
	serveToggleDSC(f, "v1", `{"apiVersion":"datasciencecluster.opendatahub.io/v1","metadata":{"name":"rhods","uid":"u1"},"spec":{"components":{"ray":{"managementState":"Managed"}}}}`, "", "ray")
	f.json("GET", "/apis/apps/v1/namespaces/redhat-ods-operator/deployments/rhods-operator", 200, `{"status":{"readyReplicas":3}}`)
	result, err := ApplyFix(client, "disable-component:ray")
	if err != nil || !result.Success {
		t.Fatalf("ApplyFix = %+v, %v", result, err)
	}
	if got := f.realWrites(); len(got) != 1 || got[0] != "PATCH /apis/datasciencecluster.opendatahub.io/v1/datascienceclusters/rhods" {
		t.Fatalf("writes = %v", got)
	}
}

// The module CR check must not be skipped when discovery fails: a hidden
// finalizer could leave the component stuck in deletion.
func TestDisableComponent_DiscoveryErrorBlocks(t *testing.T) {
	f, client := newFakeAPI(t)
	serveToggleDSC(f, "v2", `{"metadata":{"name":"my-dsc","uid":"u1"},"spec":{"components":{"ray":{"managementState":"Managed"}}}}`, "", "ray")
	f.json("GET", "/apis/apps/v1/namespaces/redhat-ods-operator/deployments/rhods-operator", 200, `{"status":{"readyReplicas":3}}`)
	f.json("GET", "/apis/components.platform.opendatahub.io", 503, `{"kind":"Status","status":"Failure","code":503}`)
	result, err := ApplyFix(client, "disable-component:ray")
	if err != nil || result.Success || result.ErrorCode != "prerequisites" || !strings.Contains(result.Message, "could not read the ray module CR") {
		t.Fatalf("ApplyFix = %+v, %v", result, err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("patched although the module CR could not be checked: %v", w)
	}
}

// Without the CRD the change cannot be checked against its schema: refused.
func TestDisableComponent_NoCRDSchemaRefuses(t *testing.T) {
	f, client := newFakeAPI(t)
	serveToggleDSC(f, "v2", `{"metadata":{"name":"my-dsc","uid":"u1"},"spec":{"components":{"ray":{"managementState":"Managed"}}}}`, "", "ray")
	f.status("GET", dscCRDPath, 403, "Forbidden")
	result, err := ApplyFix(client, "disable-component:ray")
	if err != nil || result.Success || !strings.Contains(result.Message, "CRD") {
		t.Fatalf("ApplyFix = %+v, %v", result, err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes: %v", w)
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
