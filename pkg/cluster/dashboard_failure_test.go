package cluster

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashboardPartialDeployAndRevertReportActualFailures(t *testing.T) {
	original := quayHTTPClient
	quayHTTPClient = &http.Client{Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		status := 404
		if strings.Contains(r.URL.Path, "/odh-dashboard/") || strings.Contains(r.URL.Path, "/odh-mod-arch-modular-architecture/") {
			status = 200
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	defer func() { quayHTTPClient = original }()
	for _, action := range []string{"deploy", "revert"} {
		t.Run(action, func(t *testing.T) {
			var mainPatched, moduleAttempted bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/deployments/dashboard-operator") {
					w.WriteHeader(404)
					io.WriteString(w, `{"kind":"Status","reason":"NotFound"}`)
					return
				}
				if r.Method == "PATCH" && strings.HasSuffix(r.URL.Path, "/deployments/model-registry-ui") {
					moduleAttempted = true
					w.WriteHeader(403)
					io.WriteString(w, `{"kind":"Status","status":"Failure","message":"forbidden"}`)
					return
				}
				if r.Method == "PATCH" && strings.HasSuffix(r.URL.Path, "/deployments/rhods-dashboard") {
					mainPatched = true
				}
				if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/deployments/rhods-dashboard") {
					io.WriteString(w, `{"spec":{"template":{"spec":{"containers":[{"name":"rhods-dashboard","image":"old"}]}}}}`)
					return
				}
				if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/deployments/rhods-operator") {
					io.WriteString(w, `{"spec":{"template":{"spec":{"containers":[{"env":[{"name":"RELATED_IMAGE_ODH_DASHBOARD_IMAGE","value":"original-dashboard"},{"name":"RELATED_IMAGE_ODH_MOD_ARCH_MODEL_REGISTRY_IMAGE","value":"original-module"}]}]}}}}`)
					return
				}
				io.WriteString(w, `{}`)
			}))
			defer srv.Close()
			c := &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: context.Background()}
			result, err := DeployPRImage(c, 123)
			if action == "revert" {
				mainPatched = false
				moduleAttempted = false
				result, err = RevertDashboardImage(c)
			}
			if err != nil || result.Success || result.ErrorCode != "partial_failure" || !mainPatched || !moduleAttempted || !strings.Contains(result.Message, "model-registry-ui") {
				t.Fatalf("result=%+v err=%v main=%v module=%v", result, err, mainPatched, moduleAttempted)
			}
		})
	}
}
