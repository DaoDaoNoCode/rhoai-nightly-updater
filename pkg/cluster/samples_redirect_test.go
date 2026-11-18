package cluster

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The samples' and the playground's client sends the app's ServiceAccount
// token. A redirect is never followed: the token goes only to the address
// the tool called (a same-host redirect would carry the Authorization
// header, also to plain http). A 3xx is an error that says so.
func TestSamplesHTTPClient_DoesNotFollowRedirects(t *testing.T) {
	if samplesHTTPClient.CheckRedirect == nil || genAIHTTPClient.CheckRedirect == nil {
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
	client := &http.Client{Transport: srv.Client().Transport, CheckRedirect: samplesHTTPClient.CheckRedirect}
	origSamples, origGenAI := samplesHTTPClient, genAIHTTPClient
	samplesHTTPClient, genAIHTTPClient = client, client
	t.Cleanup(func() { samplesHTTPClient, genAIHTTPClient = origSamples, origGenAI })

	const token = "sa-token-secret"
	a := &sampleAPI{ctx: context.Background(), base: srv.URL, token: token, server: "MLflow"}
	err := a.send(http.MethodGet, "/api/2.0/mlflow/experiments/search", nil, "", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "redirect") || !strings.Contains(err.Error(), "does not follow") || strings.Contains(err.Error(), token) {
		t.Fatalf("samples: %v", err)
	}
	g := genAIAPI{ctx: context.Background(), base: srv.URL, token: token}
	err = g.call(http.MethodGet, "/lsd/status", "proj", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "redirect") || !strings.Contains(err.Error(), "does not follow") || strings.Contains(err.Error(), token) {
		t.Fatalf("gen-ai: %v", err)
	}
	if n := followed.Load(); n != 0 {
		t.Fatalf("followed %d redirects", n)
	}
}
