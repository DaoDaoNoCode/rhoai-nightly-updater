package cluster

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The summary carries the reviewed fields only: the operator's version,
// channel and phase, the DataScienceCluster's components, and no name of
// a pod, project, user or the pull secret's content, even when the
// cluster holds them.
func TestGetSummary(t *testing.T) {
	k8s, c := newResourceFake(t)
	k8s.putJSON("/apis/operators.coreos.com/v1alpha1/namespaces/redhat-ods-operator/subscriptions/rhods-operator",
		`{"metadata":{"name":"rhods-operator"},"spec":{"source":"redhat-operators","channel":"stable-3.6"},"status":{"state":"AtLatestKnown","installedCSV":"rhods-operator.3.6.0"}}`)
	k8s.putJSON("/apis/operators.coreos.com/v1alpha1/namespaces/redhat-ods-operator/clusterserviceversions/rhods-operator.3.6.0",
		`{"metadata":{"name":"rhods-operator.3.6.0"},"spec":{"version":"3.6.0"},"status":{"phase":"Succeeded"}}`)
	k8s.putJSON("/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/default-dsc", `{
		"metadata":{"name":"default-dsc"},
		"spec":{"components":{"dashboard":{"managementState":"Managed"},"ray":{"managementState":"Removed"}}},
		"status":{"phase":"Ready","conditions":[{"type":"Ready","status":"True"},{"type":"DashboardReady","status":"True"},
			{"type":"RayReady","status":"False","reason":"Removed","message":"set to Removed by alice in team-alpha"}]}}`)
	k8s.putJSON("/api/v1/namespaces/kube-system/secrets/additional-pull-secret", `{"data":{".dockerconfigjson":"bm90LWpzb24="}}`)
	k8s.putJSON("/api/v1/namespaces/team-alpha/pods/secret-pod-1", `{"metadata":{"name":"secret-pod-1"}}`)

	s, err := GetSummary(c)
	if err != nil {
		t.Fatal(err)
	}
	if !s.RHOAI.Installed || s.RHOAI.Version != "3.6.0" || s.RHOAI.Channel != "stable-3.6" || s.RHOAI.Phase != "Succeeded" {
		t.Errorf("rhoai: %+v", s.RHOAI)
	}
	if s.Nightly.InstalledBuild != nil || s.Nightly.LatestBuild != nil || s.Nightly.NewerAvailable {
		t.Errorf("not on the nightly catalog: %+v", s.Nightly)
	}
	if !s.DSC.Exists || !s.DSC.Ready || len(s.DSC.Components) != 2 {
		t.Fatalf("dsc: %+v", s.DSC)
	}
	if d := s.DSC.Components[0]; d.Name != "dashboard" || d.ManagementState != "Managed" || !d.Ready {
		t.Errorf("dashboard: %+v", d)
	}
	if r := s.DSC.Components[1]; r.Name != "ray" || r.ManagementState != "Removed" || r.Ready {
		t.Errorf("ray: %+v", r)
	}
	b, _ := json.Marshal(s)
	for _, leak := range []string{"alice", "team-alpha", "secret-pod", "dockerconfig", "redhat-ods-operator", "default-dsc"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("the summary shows %q: %s", leak, b)
		}
	}
	for _, call := range k8s.calls {
		if call.Method != "GET" {
			t.Errorf("the summary wrote: %s %s", call.Method, call.Path)
		}
	}
}

// The nightly part says when it does not know: an unreadable
// CatalogSource is not "not a nightly install", and a failed lookup is
// not "the newest build". Only fixed codes, never the read's text.
func TestSummaryNightly(t *testing.T) {
	yes, no := true, false
	stream := "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + strings.Repeat("a", 64)
	installed := &types.NightlyBuild{Image: stream, Tag: "rhoai-3.6", BuildDate: "2026-11-19"}
	latest := &types.NightlyBuild{Image: stream, Tag: "rhoai-3.6", BuildDate: "2026-11-20"}
	for _, tc := range []struct {
		name    string
		csErr   error
		n       *types.NightlyStatus
		problem string
		newer   bool
	}{
		{"catalog unreadable", &K8sError{Status: 403, K8sMessage: "SENTINEL"}, nil, types.NightlyProblemCatalogUnreadable, false},
		{"not a nightly install", nil, nil, "", false},
		{"newer", nil, &types.NightlyStatus{Installed: installed, Latest: latest, UpdateAvailable: &yes}, "", true},
		{"newest", nil, &types.NightlyStatus{Installed: installed, Latest: latest, UpdateAvailable: &no}, "", false},
		{"lookup failed", nil, &types.NightlyStatus{Installed: installed, Error: "SENTINEL quay said no"}, types.NightlyProblemLookupFailed, false},
		{"digest unknown", nil, &types.NightlyStatus{Installed: installed, Latest: latest}, types.NightlyProblemLookupFailed, false},
		{"custom image", nil, &types.NightlyStatus{Installed: &types.NightlyBuild{Image: "quay.io/rhoai/rhoai-fbc-fragment@sha256:" + strings.Repeat("b", 64)}}, types.NightlyProblemNoStream, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := summaryNightly(tc.csErr, func() *types.NightlyStatus { return tc.n })
			b, _ := json.Marshal(got)
			if got.Problem != tc.problem || got.NewerAvailable != tc.newer || strings.Contains(string(b), "SENTINEL") {
				t.Fatalf("got %s", b)
			}
		})
	}
}

// Without RHOAI and without a DataScienceCluster the summary says so; a
// read the API server refuses fails it rather than showing a guess.
func TestGetSummaryNotInstalledAndErrors(t *testing.T) {
	_, c := newResourceFake(t)
	s, err := GetSummary(c)
	if err != nil {
		t.Fatal(err)
	}
	if s.RHOAI.Installed || s.RHOAI.Version != "" || s.DSC.Exists || s.DSC.Components == nil || len(s.DSC.Components) != 0 {
		t.Errorf("empty cluster: %+v", s)
	}

	k8s, c := newResourceFake(t)
	k8s.fail["GET /apis/operators.coreos.com/v1alpha1/namespaces/redhat-ods-operator/subscriptions/rhods-operator"] = 403
	if _, err := GetSummary(c); err == nil || !IsK8sError(err, 403) {
		t.Fatalf("a refused Subscription read: %v", err)
	}
}
