package cluster

import (
	"encoding/json"
	"strings"
	"testing"
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
