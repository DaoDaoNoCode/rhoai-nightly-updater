package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestComponentMutationsShareTheClusterLock(t *testing.T) {
	setupDevMode(t)
	original := mutationPermission
	mutationPermission = func(context.Context, string) (bool, error) { return true, nil }
	t.Cleanup(func() { mutationPermission = original })

	if !acquireClusterMutationLock() {
		t.Fatal("lock unexpectedly held")
	}
	defer releaseClusterMutationLock()

	handlers := map[string]http.HandlerFunc{
		"/api/dashboard/deploy-pr":        HandleDashboardDeployPR,
		"/api/dashboard/deploy-main":      HandleDashboardDeployMain,
		"/api/dashboard/revert":           HandleDashboardRevert,
		"/api/resources/mlflow/setup":     HandleMLflowSetup,
		"/api/resources/mlflow/teardown":  HandleMLflowTeardown,
		"/api/resources/mlflow/deploy-pr": HandleMLflowDeployPR,
		"/api/resources/mlflow/revert":    HandleMLflowRevert,
	}
	for path, h := range handlers {
		r := httptest.NewRequest("POST", path, strings.NewReader(`{"pr":1}`))
		r.Header.Set("X-Forwarded-Access-Token", "user-token")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "cluster_busy") {
			t.Errorf("%s while an operator operation runs: status=%d body=%s", path, w.Code, w.Body.String())
		}
	}
}
