package cluster

import (
	"net/http"
	"strings"
	"testing"
)

const dsciV2List = "/apis/dscinitialization.opendatahub.io/v2/dscinitializations"

func TestDataScienceClusterCheck(t *testing.T) {
	notReady := `{"items":[{"metadata":{"name":"default-dsc"},"status":{"conditions":[
		{"type":"Ready","status":"False","reason":"Error","message":"failure deploying resource /default-trustyai"},
		{"type":"TrainerReady","status":"False","reason":"Error","message":"JobSet Operator is not installed"},
		{"type":"KueueReady","status":"False","reason":"Removed","severity":"Info","message":"Component ManagementState is set to Removed"},
		{"type":"KserveReady","status":"True"}]}}]}`
	tests := []struct {
		name       string
		dsc        string
		dscStatus  int
		dsci       string
		wantStatus string
		wantIDs    []string
		wantIn     string
		wantNotIn  string
	}{
		{"no API (fresh cluster)", "", http.StatusNotFound, "", "pass", nil, "operator not installed", ""},
		{"no DSC", `{"items":[]}`, 0, "", "warn", []string{"dsc-missing"}, "", ""},
		{"ready", `{"items":[{"metadata":{"name":"default-dsc"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`, 0,
			`{"items":[{"metadata":{"name":"default-dsci"},"status":{"phase":"Ready"}}]}`, "pass", nil, "default-dsci phase Ready", ""},
		{"not ready ignores Info", notReady, 0, "", "fail", []string{"dsc-not-ready"}, "TrainerReady=False", "KueueReady"},
		{"dsci error", `{"items":[{"metadata":{"name":"default-dsc"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`, 0,
			`{"items":[{"metadata":{"name":"default-dsci"},"status":{"phase":"Error","conditions":[{"type":"Degraded","status":"True","reason":"ReconcileFailed","message":"boom"}]}}]}`,
			"fail", []string{"dsci-error"}, "boom", ""},
		{"read forbidden", "", http.StatusForbidden, "", "warn", nil, "Could not read", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c := newFakeAPI(t)
			switch {
			case tt.dscStatus != 0:
				f.status("GET", dscV2List, tt.dscStatus, http.StatusText(tt.dscStatus))
				f.status("GET", "/apis/datasciencecluster.opendatahub.io/v1/datascienceclusters", tt.dscStatus, http.StatusText(tt.dscStatus))
			default:
				f.json("GET", dscV2List, http.StatusOK, tt.dsc)
			}
			if tt.dsci != "" {
				f.json("GET", dsciV2List, http.StatusOK, tt.dsci)
			}
			out := checkDataScienceCluster(c)
			if out.check.Status != tt.wantStatus {
				t.Fatalf("check = %+v", out.check)
			}
			var ids []string
			var text []string
			for _, p := range out.problems {
				ids = append(ids, p.ID)
				text = append(text, p.Evidence...)
			}
			if strings.Join(ids, ",") != strings.Join(tt.wantIDs, ",") {
				t.Fatalf("problems = %v", ids)
			}
			all := out.check.Detail + "\n" + strings.Join(text, "\n")
			if tt.wantIn != "" && !strings.Contains(all, tt.wantIn) {
				t.Fatalf("missing %q in %s", tt.wantIn, all)
			}
			if tt.wantNotIn != "" && strings.Contains(all, tt.wantNotIn) {
				t.Fatalf("unexpected %q in %s", tt.wantNotIn, all)
			}
		})
	}
}
