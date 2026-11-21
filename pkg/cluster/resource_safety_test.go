package cluster

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/mlflow"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func TestS3TeardownDependencyFailure(t *testing.T) {
	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/api/v1/namespaces" {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"message":"namespace lookup forbidden","code":403}`)
			return
		}
		if r.Method == "DELETE" {
			deleted = true
		}
		io.WriteString(w, `{}`)
	}))
	defer server.Close()
	c := newTestClient(testClientOptions{BaseURL: server.URL, HTTPClient: server.Client(), Ctx: context.Background()})
	result, err := TeardownS3Storage(c, types.S3TeardownRequest{})
	if err != nil || result.Success || deleted {
		t.Fatalf("dependency lookup failed but result success=%v deleted=%v error=%v", result.Success, deleted, err)
	}
}

func TestMLflowQuay503DoesNotDeploy(t *testing.T) {
	defer registry.SetHTTPClientForTest(&http.Client{Transport: dscSampleTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("unavailable")), Header: http.Header{}}, nil
	})})()
	patched := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" && strings.HasSuffix(r.URL.Path, "/mlflows/"+mlflow.CRName) {
			patched = true
		}
		io.WriteString(w, `{}`)
	}))
	defer server.Close()
	c := newTestClient(testClientOptions{BaseURL: server.URL, HTTPClient: server.Client(), Ctx: context.Background()})
	result, err := DeployMLflowPR(c, 123)
	if err != nil || result.Success || patched {
		t.Fatalf("Quay returned 503 but result success=%v CRPatched=%v error=%v", result.Success, patched, err)
	}
}
