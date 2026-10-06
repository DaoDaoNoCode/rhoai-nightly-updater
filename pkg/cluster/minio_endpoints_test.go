package cluster

// Regression tests for the second review round: MinIO's Route hosts and
// service address must be readable before anything that a pipeline server
// may depend on is deleted.

import (
	"encoding/json"
	"strings"
	"testing"
)

// deployedToolMinIO puts a managed namespace, Deployment, PVC, console Route
// and Service, all created by the tool.
func deployedToolMinIO(f *resourceFake) {
	managedMinIONamespace(f)
	putS3Deployment(f, 1)
	f.putJSON(s3PVCPath, `{"metadata":{"labels":`+toolLabelJSON+`}}`)
	f.putJSON(minioUIRoute, `{"metadata":{"labels":`+toolLabelJSON+`},"spec":{"host":"minio-ui-minio.apps.example.com"}}`)
	f.putJSON(minioSvcPath, `{"metadata":{"labels":`+toolLabelJSON+`},"spec":{"clusterIP":"172.30.10.20"}}`)
}

func TestTeardownMinIO_FailsClosedWhenEndpointsUnreadable(t *testing.T) {
	cases := []struct {
		name     string
		failPath string
		status   int
		wantCode string
	}{
		{"console Route read forbidden", minioUIRoute, 403, "forbidden"},
		{"API Route read fails", minioAPIRoute, 500, ""},
		{"Service read fails", minioSvcPath, 503, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastMinIOTimings(t)
			f, c := newResourceFake(t)
			deployedToolMinIO(f)
			// The pipeline server uses the console Route host, which only
			// the unreadable object would reveal.
			putDSPA(f, "team", "dspa", `{}`, "Mozilla", "Update", "https://minio-ui-minio.apps.example.com", "s", "", "")
			f.fail["GET "+tc.failPath] = tc.status
			resp, err := TeardownMinIO(c)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Success || !strings.Contains(resp.Message, "Cannot verify which pipeline servers use the S3 storage") {
				t.Fatalf("teardown = %+v, want a refusal", resp)
			}
			if tc.wantCode != "" && resp.ErrorCode != tc.wantCode {
				t.Errorf("errorCode = %q, want %q", resp.ErrorCode, tc.wantCode)
			}
			if m := f.mutations(); len(m) != 0 {
				t.Errorf("teardown changed objects although it refused: %v", m)
			}
			if !f.has(s3PVCPath) {
				t.Error("the data PVC was deleted")
			}
		})
	}
}

func TestTeardownMinIO_UnreadableEndpointsWithoutPipelineServersProceeds(t *testing.T) {
	fastMinIOTimings(t)
	f, c := newResourceFake(t)
	deployedToolMinIO(f)
	f.fail["GET "+minioAPIRoute] = 500
	resp, _ := TeardownMinIO(c)
	// With no pipeline server nothing can depend on MinIO, so the guard
	// passes; the unreadable Route itself is then reported as not removed.
	if f.has(s3PVCPath) || resp.ErrorCode != "partial_failure" || !strings.Contains(resp.Message, "Route minio-api: cannot read it") {
		t.Fatalf("teardown = %+v, pvc kept = %v", resp, f.has(s3PVCPath))
	}
}

func TestReadMinIOEndpoints_MissingObjectsAreNotErrors(t *testing.T) {
	f, c := newResourceFake(t)
	managedMinIONamespace(f)
	ep, err := readMinIOEndpoints(c)
	if err != nil || ep != (minioEndpoints{}) {
		t.Fatalf("endpoints = %+v, err = %v; want empty and no error when nothing exists", ep, err)
	}
	deployedToolMinIO(f)
	ep, err = readMinIOEndpoints(c)
	if err != nil || ep.uiHost != "minio-ui-minio.apps.example.com" || ep.clusterIP != "172.30.10.20" || ep.apiHost != "" {
		t.Fatalf("endpoints = %+v, err = %v", ep, err)
	}
}

func TestResourcesStatus_TeardownBlockedWhenEndpointsUnreadable(t *testing.T) {
	f, c := newResourceFake(t)
	deployedToolMinIO(f)
	putDSPA(f, "team", "dspa", `{}`, "Mozilla", "Update", "https://s3.example.com", "s", "", "")
	f.fail["GET "+minioUIRoute] = 500
	st, err := GetResourcesStatus(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.MinIO.TeardownBlockedReason, "Cannot verify which pipeline servers use the S3 storage") {
		t.Fatalf("teardownBlockedReason = %q", st.MinIO.TeardownBlockedReason)
	}
}

const (
	minioNPPath = "/apis/networking.k8s.io/v1/namespaces/minio/networkpolicies/minio-ingress"
	s3NPPath    = "/apis/networking.k8s.io/v1/namespaces/minio/networkpolicies/seaweedfs-ingress"
)

// R6-1: setup fences in SeaweedFS's ports with a tool-owned NetworkPolicy:
// S3 from pods in any namespace, the admin UI only from the router, and
// nothing else (master, volume, filer and worker ports stay unreachable).
func TestSetupMinIO_CreatesIngressPolicy(t *testing.T) {
	fastMinIOTimings(t)
	f, c := newResourceFake(t)
	readyAfterApply(f)
	resp, _ := SetupMinIO(c)
	if !resp.Success {
		t.Fatalf("setup = %+v", resp)
	}
	np := f.get(s3NPPath)
	if np == nil {
		t.Fatal("NetworkPolicy seaweedfs-ingress was not created")
	}
	if f.has(minioNPPath) {
		t.Error("a fresh setup must not create the MinIO policy")
	}
	labels, _ := np["metadata"].(map[string]interface{})["labels"].(map[string]interface{})
	if labels[managedByLabelKey] != managedByLabelValue {
		t.Errorf("NetworkPolicy is not labelled as the tool's: %v", labels)
	}
	raw, _ := json.Marshal(np["spec"])
	for _, want := range []string{
		`"podSelector":{"matchLabels":{"app":"seaweedfs"}}`,
		`"policyTypes":["Ingress"]`,
		`{"from":[{"namespaceSelector":{}}],"ports":[{"port":8333,"protocol":"TCP"}]}`,
		`{"namespaceSelector":{"matchLabels":{"policy-group.network.openshift.io/ingress":""}}}`,
		`{"namespaceSelector":{"matchLabels":{"policy-group.network.openshift.io/host-network":""}}}],"ports":[{"port":23646,"protocol":"TCP"}]`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("spec lacks %s: %s", want, raw)
		}
	}
	var spec struct {
		Ingress []struct {
			Ports []struct {
				Port int `json:"port"`
			} `json:"ports"`
		} `json:"ingress"`
	}
	_ = json.Unmarshal(raw, &spec)
	var ports []int
	for _, rule := range spec.Ingress {
		for _, p := range rule.Ports {
			ports = append(ports, p.Port)
		}
	}
	if len(ports) != 2 || ports[0] != 8333 || ports[1] != 23646 {
		t.Errorf("only the S3 and admin UI ports may be reachable: %v", ports)
	}
}

func TestSetupMinIO_RefusesForeignIngressPolicy(t *testing.T) {
	fastMinIOTimings(t)
	f, c := newResourceFake(t)
	readyAfterApply(f)
	managedMinIONamespace(f)
	f.putJSON(s3NPPath, foreignJSON(`,"spec":{"podSelector":{}}`))
	resp, _ := SetupMinIO(c)
	if resp.Success || resp.ErrorCode != "not_managed" || !strings.Contains(resp.Message, "NetworkPolicy seaweedfs-ingress") {
		t.Fatalf("setup = %+v", resp)
	}
	assertForeignUntouched(t, f, s3NPPath, "spec", `"podSelector":{}`)
}

func TestTeardownMinIO_RemovesIngressPolicy(t *testing.T) {
	fastMinIOTimings(t)
	f, c := newResourceFake(t)
	deployedToolMinIO(f)
	f.putJSON(s3NPPath, `{"metadata":{"uid":"np-uid","labels":`+toolLabelJSON+`}}`)
	f.putJSON(minioNPPath, `{"metadata":{"uid":"old-np-uid","labels":`+toolLabelJSON+`}}`)
	resp, _ := TeardownMinIO(c)
	if !resp.Success || f.has(s3NPPath) || f.has(minioNPPath) {
		t.Fatalf("teardown = %+v, policies kept = %v %v", resp, f.has(s3NPPath), f.has(minioNPPath))
	}
}

func TestSetupMinIO_KeepsAPIRouteWhilePipelineServersUseIt(t *testing.T) {
	const apiRoute = `{"metadata":{"uid":"api-uid","labels":` + toolLabelJSON + `},"spec":{"host":"minio-api-minio.apps.example.com"}}`
	cases := []struct {
		name      string
		dspaHost  string // "" means no pipeline server
		failDSPA  bool
		failRoute bool
		wantOK    bool
		wantKept  bool
		wantInMsg string
	}{
		{name: "no pipeline server", wantOK: true},
		{name: "pipeline server on the service", dspaHost: minioS3Host(), wantOK: true},
		{name: "pipeline server on the Route host", dspaHost: "https://minio-api-minio.apps.example.com", wantOK: true, wantKept: true, wantInMsg: "team/dspa"},
		{name: "Route host in another case with port", dspaHost: "MINIO-API-minio.apps.example.com.:443", wantOK: true, wantKept: true, wantInMsg: "team/dspa"},
		{name: "pipeline servers unreadable", failDSPA: true, wantKept: true, wantInMsg: "cannot check whether a pipeline server uses it"},
		{name: "Route host unreadable", dspaHost: minioS3Host(), failRoute: true, wantKept: true, wantInMsg: "cannot check whether a pipeline server uses it"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastMinIOTimings(t)
			f, c := newResourceFake(t)
			readyAfterApply(f)
			managedMinIONamespace(f)
			f.putJSON(minioAPIRoute, apiRoute)
			if tc.dspaHost != "" {
				putDSPA(f, "team", "dspa", `{}`, "Mozilla", "Update", tc.dspaHost, "s", "", "")
			}
			if tc.failDSPA {
				f.fail["GET /apis/"+dspaAPIGroup+"/datasciencepipelinesapplications"] = 500
			}
			if tc.failRoute {
				// The first read (the ownership check) succeeds; the later
				// host read fails.
				reads := 0
				f.beforeServe = func(method, path string) {
					if method == "GET" && path == minioAPIRoute {
						if reads++; reads > 1 {
							f.mu.Lock()
							f.fail["GET "+minioAPIRoute] = 500
							f.mu.Unlock()
						}
					}
				}
			}
			resp, _ := SetupMinIO(c)
			if resp.Success != tc.wantOK {
				t.Fatalf("setup = %+v", resp)
			}
			if f.has(minioAPIRoute) != tc.wantKept {
				t.Errorf("minio-api kept = %v, want %v", f.has(minioAPIRoute), tc.wantKept)
			}
			if tc.wantInMsg != "" && !strings.Contains(resp.Message, tc.wantInMsg) {
				t.Errorf("message = %q, want it to mention %q", resp.Message, tc.wantInMsg)
			}
		})
	}
}
