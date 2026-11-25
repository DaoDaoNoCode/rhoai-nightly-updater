package cluster

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
)

// Every request names the tool and its version, so the API server records
// writes without a fieldManager as rhoai-nightly-updater-client, not Go's
// default Go-http-client (the manager earlier versions left), and audit
// logs name the tool. The manager is not the server-side apply field
// manager: UpgradeToolFieldOwnership turns that one's Update entries into
// Apply ones.
func TestClient_SendsTheToolsUserAgent(t *testing.T) {
	t.Cleanup(func() { kube.SetUserAgentVersion("") })
	kube.SetUserAgentVersion("v9.8.7")
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.UserAgent())
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := newTestClient(testClientOptions{BaseURL: srv.URL, Token: "t", HTTPClient: srv.Client(), Ctx: t.Context()})
	_, _, _ = c.Get("/api/v1/namespaces/x")
	_, _, _ = c.Patch("/api/v1/namespaces/x", []byte(`{"metadata":{}}`))
	if want := []string{"GET rhoai-nightly-updater-client/v9.8.7", "PATCH rhoai-nightly-updater-client/v9.8.7"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("requests = %q, want %q", got, want)
	}
	manager := strings.SplitN(kube.UserAgent(), "/", 2)[0]
	if manager != envkit.ClientManager || manager == envkit.ToolFieldManager || manager == envkit.LegacyPostManager {
		t.Fatalf("the API server would name the manager %q", manager)
	}
	kube.SetUserAgentVersion("")
	if kube.UserAgent() != "rhoai-nightly-updater-client/dev" {
		t.Errorf("without a version: %q", kube.UserAgent())
	}
}

// Go-http-client counts as the tool only for objects of names that only
// earlier versions wrote: here the legacy pipeline server's Secret
// dashboard-dspa-secret is deleted with it, while a Secret of the current
// name that only Go-http-client wrote, without the tool's label, is kept.
func TestTeardownPipelineServer_LegacyManagerOnlyForTheLegacySecret(t *testing.T) {
	for _, tc := range []struct {
		name, host, secret string
		labels, manager    string
		deleted            bool
	}{
		{"legacy secret of an earlier version", storage.LegacyS3Host(), storage.LegacyDSPASecretName, `{}`, envkit.LegacyPostManager, true},
		{"current name, Go-http-client only", storage.S3Host(), storage.DSPASecretName, `{}`, envkit.LegacyPostManager, false},
		{"current name, the tool's client", storage.S3Host(), storage.DSPASecretName, `{}`, envkit.ClientManager, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fastPipelineTimings(t)
			f, c := newResourceFake(t)
			if tc.host == storage.LegacyS3Host() {
				putDSPA(f, "p1", storage.LegacyDSPAName, `{}`, envkit.LegacyPostManager, "Update", tc.host, tc.secret, "", "")
			} else {
				putDSPA(f, "p1", storage.DSPAName, toolLabelJSON, envkit.ToolFieldManager, "Apply", tc.host, tc.secret, "", "")
			}
			f.putJSON(secretPath("p1", tc.secret), `{"metadata":{"labels":`+tc.labels+`,"managedFields":`+managedFieldsJSON(tc.manager, "Update")+`}}`)
			resp, _ := TeardownPipelineServer(c, "p1", toolDSPAUID(c, "p1"), nil)
			if !resp.Success {
				t.Fatalf("teardown = %+v", resp)
			}
			if gone := !f.has(secretPath("p1", tc.secret)); gone != tc.deleted {
				t.Fatalf("secret deleted = %v, want %v; logs %v", gone, tc.deleted, resp.Logs)
			}
		})
	}
}
