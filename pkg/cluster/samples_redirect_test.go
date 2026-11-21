package cluster

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/mlflow"
)

// The samples' and the playground's client sends the app's ServiceAccount
// token. A redirect is never followed: the token goes only to the address
// the tool called (a same-host redirect would carry the Authorization
// header, also to plain http). A 3xx is an error that says so.
func TestSamplesHTTPClient_DoesNotFollowRedirects(t *testing.T) {
	if mlflow.SamplesHTTPClient.CheckRedirect == nil || features.GenAIHTTPClientForTest().CheckRedirect == nil {
		t.Fatal("the samples and gen-ai clients must refuse redirects")
	}
	var followed atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/next") {
			followed.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	client := &http.Client{Transport: srv.Client().Transport, CheckRedirect: mlflow.SamplesHTTPClient.CheckRedirect}
	restoreGenAI := features.SetGenAIHTTPClientForTest(client)
	t.Cleanup(mlflow.SetSamplesHTTPClientForTest(client))
	t.Cleanup(restoreGenAI)

	const token = "sa-token-secret"
	a := &mlflow.SampleAPI{Ctx: context.Background(), Base: srv.URL, Token: token, Server: "MLflow"}
	err := a.Send(http.MethodGet, "/api/2.0/mlflow/experiments/search", nil, "", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "redirect") || !strings.Contains(err.Error(), "does not follow") || strings.Contains(err.Error(), token) {
		t.Fatalf("samples: %v", err)
	}
	g := features.GenAIAPI{Ctx: context.Background(), Base: srv.URL, Token: token}
	err = g.Call(http.MethodGet, "/lsd/status", "proj", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "redirect") || !strings.Contains(err.Error(), "does not follow") || strings.Contains(err.Error(), token) {
		t.Fatalf("gen-ai: %v", err)
	}
	if n := followed.Load(); n != 0 {
		t.Fatalf("followed %d redirects", n)
	}
}
