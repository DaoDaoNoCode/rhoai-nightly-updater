package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// The "Pull secret" check, once additional-pull-secret is valid, by control
// plane topology and by whether a nightly catalog shows that the nodes pull.
func TestDiagnostics_PullSecretNodesPerTopology(t *testing.T) {
	notReady := `{"metadata":{"name":"` + rhoai.CatalogName + `"},"spec":{"image":"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:abc"},"status":{"connectionState":{"lastObservedState":"TRANSIENT_FAILURE"}}}`
	for _, tc := range []struct {
		name      string
		infra     *mockResponse // nil = absent (404)
		catalog   string        // "" = the healthy READY catalog; "-" = none
		want      string        // check status
		wantTitle string
		evidence  string
	}{
		{"hosted, no catalog", &mockResponse{body: `{"status":{"controlPlaneTopology":"External"}}`}, "-", "pass", "", ""},
		{"self-managed, READY catalog", &mockResponse{body: `{"status":{"controlPlaneTopology":"HighlyAvailable"}}`}, "", "pass", "", ""},
		{"self-managed, no catalog", &mockResponse{body: `{"status":{"controlPlaneTopology":"HighlyAvailable"}}`}, "-", "warn", "The nodes may not have the quay.io/rhoai credentials", "no nightly catalog runs yet"},
		{"single node, failing catalog", &mockResponse{body: `{"status":{"controlPlaneTopology":"SingleReplica"}}`}, notReady, "warn", "The nodes may not have the quay.io/rhoai credentials", "is TRANSIENT_FAILURE on quay.io/rhoai/"},
		{"unreadable, no catalog", nil, "-", "warn", "(if this cluster is self-managed)", "could not be read"},
		{"forbidden, READY catalog", &mockResponse{statusCode: 403, body: `{"kind":"Status","code":403}`}, "", "pass", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			responses := buildAllHealthyResponses()
			paths := diagnosticPaths()
			if tc.infra != nil {
				responses[infrastructurePath] = *tc.infra
			}
			switch tc.catalog {
			case "-":
				delete(responses, paths["catalogSource"])
			case "":
			default:
				responses[paths["catalogSource"]] = mockResponse{body: tc.catalog}
			}
			c, cleanup := newMockClient(responses)
			defer cleanup()
			out := checkPullSecretHealth(c)
			if out.check.Status != tc.want {
				t.Fatalf("check %+v, want %s", out.check, tc.want)
			}
			if tc.want == "pass" {
				if len(out.problems) != 0 {
					t.Fatalf("problems %+v", out.problems)
				}
				return
			}
			if len(out.problems) != 1 {
				t.Fatalf("problems %+v", out.problems)
			}
			p := out.problems[0]
			if p.ID != "node-pull-secret-unchecked" || p.Severity != "warning" || !strings.Contains(p.Title, tc.wantTitle) ||
				p.TechnicalCmd != rhoai.NodePullSecretCommand || !strings.Contains(p.Description, "openshift-config/pull-secret") ||
				!strings.Contains(p.Fix, "Check that the nodes can pull") || !strings.Contains(strings.Join(p.Evidence, "\n"), tc.evidence) {
				t.Fatalf("problem %+v", p)
			}
		})
	}
}
