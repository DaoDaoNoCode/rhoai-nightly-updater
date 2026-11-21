package cluster

import (
	"net/http"
	"slices"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
)

// The infrastructure namespace made by someone else between the check and
// its creation (409) is not recorded as this tool's, and Set up goes on
// only when it was not made through a project request.
func TestCreateMaaSInfraNamespace_MadeMeanwhile(t *testing.T) {
	const infra = "redhat-ai-gateway-infra"
	for _, tc := range []struct {
		name        string
		annotations string
		wantCode    string
	}{
		{"made by RHOAI", `{}`, ""},
		{"made through a project request", `{"openshift.io/requester":"mallory"}`, "not_managed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFakeAPI(t)
			f.status("POST", "/api/v1/namespaces", http.StatusConflict, "AlreadyExists")
			f.json("GET", "/api/v1/namespaces/"+infra, http.StatusOK, `{"metadata":{"name":"`+infra+`","annotations":`+tc.annotations+`}}`)
			rec := serving.MaaSRecord{InfraNamespace: "set before"}
			r, err := (&serving.Run{Client: c}).CreateMaaSInfraNamespace(&rec, infra)
			if err != nil {
				t.Fatal(err)
			}
			if rec.InfraNamespace != "" {
				t.Fatalf("recorded %q as this tool's", rec.InfraNamespace)
			}
			switch {
			case tc.wantCode == "" && r != nil:
				t.Fatalf("stopped: %+v", r)
			case tc.wantCode != "" && (r == nil || r.Success || r.ErrorCode != tc.wantCode):
				t.Fatalf("result %+v, want %s", r, tc.wantCode)
			}
		})
	}
}

// A namespace this tool creates is recorded as its own.
func TestCreateMaaSInfraNamespace_Created(t *testing.T) {
	const infra = "redhat-ai-gateway-infra"
	f, c := newFakeAPI(t)
	f.json("POST", "/api/v1/namespaces", http.StatusCreated, `{"metadata":{"name":"`+infra+`"}}`)
	rec := serving.MaaSRecord{}
	m := &serving.Run{Client: c}
	if r, err := m.CreateMaaSInfraNamespace(&rec, infra); r != nil || err != nil {
		t.Fatalf("%+v %v", r, err)
	}
	if rec.InfraNamespace != infra || !slices.Contains(m.Logs, "OK: Namespace "+infra+" created") {
		t.Fatalf("record %q, logs %v", rec.InfraNamespace, m.Logs)
	}
}
