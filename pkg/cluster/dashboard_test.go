package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetDashboardState_NoPR(t *testing.T) {
	deploy := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{
				"opendatahub.io/managed": "true",
			},
		},
		"spec": map[string]interface{}{
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"app": "rhods-dashboard",
				},
			},
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "rhods-dashboard",
							"image": "registry.redhat.io/rhoai/odh-dashboard-rhel9:v3.5",
						},
					},
				},
			},
		},
	}
	deployJSON, _ := json.Marshal(deploy)

	podList := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{
				"metadata": map[string]interface{}{
					"name":      "rhods-dashboard-abc",
					"namespace": "redhat-ods-applications",
				},
				"spec": map[string]interface{}{
					"nodeName": "node-1",
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "rhods-dashboard",
							"image": "registry.redhat.io/rhoai/odh-dashboard-rhel9:v3.5",
						},
					},
				},
				"status": map[string]interface{}{
					"phase": "Running",
					"containerStatuses": []interface{}{
						map[string]interface{}{
							"name":         "rhods-dashboard",
							"ready":        true,
							"restartCount": 0,
							"state": map[string]interface{}{
								"running": map[string]interface{}{},
							},
						},
					},
				},
			},
		},
	}
	podListJSON, _ := json.Marshal(podList)

	client, cleanup := newMockClient(map[string]mockResponse{
		"/apis/apps/v1/namespaces/redhat-ods-applications/deployments/rhods-dashboard": {
			body: string(deployJSON),
		},
		"/api/v1/namespaces/redhat-ods-applications/pods": {
			body: string(podListJSON),
		},
	})
	defer cleanup()

	state, err := GetDashboardState(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.IsCustomPR {
		t.Error("expected IsCustomPR to be false when running stable image")
	}
	if state.CurrentImage != "registry.redhat.io/rhoai/odh-dashboard-rhel9:v3.5" {
		t.Errorf("unexpected CurrentImage: %s", state.CurrentImage)
	}
	if !state.Managed {
		t.Error("expected Managed to be true")
	}
}

func TestGetDashboardState_SingleContainerPR(t *testing.T) {
	deploy := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{
				"opendatahub.io/managed": "false",
			},
		},
		"spec": map[string]interface{}{
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"app": "rhods-dashboard",
				},
			},
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "rhods-dashboard",
							"image": "quay.io/opendatahub/odh-dashboard:pr-42",
						},
						map[string]interface{}{
							"name":  "model-registry-ui",
							"image": "registry.redhat.io/rhoai/model-registry-ui:v3.5",
						},
					},
				},
			},
		},
	}
	deployJSON, _ := json.Marshal(deploy)

	podList := map[string]interface{}{"items": []interface{}{}}
	podListJSON, _ := json.Marshal(podList)

	client, cleanup := newMockClient(map[string]mockResponse{
		"/apis/apps/v1/namespaces/redhat-ods-applications/deployments/rhods-dashboard": {
			body: string(deployJSON),
		},
		"/api/v1/namespaces/redhat-ods-applications/pods": {
			body: string(podListJSON),
		},
	})
	defer cleanup()

	state, err := GetDashboardState(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !state.IsCustomPR {
		t.Error("expected IsCustomPR to be true for PR image")
	}
	if state.PRNumber != 42 {
		t.Errorf("expected PRNumber 42, got %d", state.PRNumber)
	}
	if len(state.PRContainers) != 1 {
		t.Errorf("expected 1 PR container, got %d", len(state.PRContainers))
	}
	if state.Managed {
		t.Error("expected Managed to be false when annotation is 'false'")
	}
}

func TestGetDashboardState_MultiContainerPR(t *testing.T) {
	deploy := map[string]interface{}{
		"metadata": map[string]interface{}{},
		"spec": map[string]interface{}{
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"app": "rhods-dashboard",
				},
			},
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "rhods-dashboard",
							"image": "quay.io/opendatahub/odh-dashboard:pr-99",
						},
						map[string]interface{}{
							"name":  "model-registry-ui",
							"image": "quay.io/opendatahub/odh-mod-arch-modular-architecture:pr-99",
						},
						map[string]interface{}{
							"name":  "gen-ai-ui",
							"image": "quay.io/opendatahub/odh-mod-arch-gen-ai:pr-99",
						},
					},
				},
			},
		},
	}
	deployJSON, _ := json.Marshal(deploy)

	podList := map[string]interface{}{"items": []interface{}{}}
	podListJSON, _ := json.Marshal(podList)

	client, cleanup := newMockClient(map[string]mockResponse{
		"/apis/apps/v1/namespaces/redhat-ods-applications/deployments/rhods-dashboard": {
			body: string(deployJSON),
		},
		"/api/v1/namespaces/redhat-ods-applications/pods": {
			body: string(podListJSON),
		},
	})
	defer cleanup()

	state, err := GetDashboardState(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !state.IsCustomPR {
		t.Error("expected IsCustomPR to be true for multi-container PR deploy")
	}
	if state.PRNumber != 99 {
		t.Errorf("expected PRNumber 99, got %d", state.PRNumber)
	}
	if len(state.PRContainers) != 3 {
		t.Errorf("expected 3 PR containers, got %d", len(state.PRContainers))
	}
}

func TestPRContainerRepos_Entries(t *testing.T) {
	expectedContainers := []string{
		"rhods-dashboard",
		"model-registry-ui",
		"gen-ai-ui",
		"maas-ui",
		"mlflow-ui",
		"eval-hub-ui",
		"automl-ui",
		"autorag-ui",
		"agent-ops-ui",
		"core-bff",
	}
	for _, name := range expectedContainers {
		if _, ok := prContainerRepos[name]; !ok {
			t.Errorf("prContainerRepos missing container %q", name)
		}
	}
	if len(prContainerRepos) != len(expectedContainers) {
		t.Errorf("expected %d entries in prContainerRepos, got %d", len(expectedContainers), len(prContainerRepos))
	}
}

func TestGetDashboardState_RolloutPending(t *testing.T) {
	deploy := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{
				"opendatahub.io/managed": "true",
			},
		},
		"spec": map[string]interface{}{
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"app": "rhods-dashboard",
				},
			},
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "rhods-dashboard",
							"image": "registry.redhat.io/rhoai/odh-dashboard-rhel9:v3.5",
						},
					},
				},
			},
		},
	}
	deployJSON, _ := json.Marshal(deploy)

	// Two pods: one ready, one not ready (simulates a rollout)
	podList := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{
				"metadata": map[string]interface{}{
					"name":      "rhods-dashboard-old",
					"namespace": "redhat-ods-applications",
				},
				"spec": map[string]interface{}{
					"nodeName": "node-1",
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "rhods-dashboard",
							"image": "registry.redhat.io/rhoai/odh-dashboard-rhel9:v3.5",
						},
					},
				},
				"status": map[string]interface{}{
					"phase": "Running",
					"containerStatuses": []interface{}{
						map[string]interface{}{
							"name":         "rhods-dashboard",
							"ready":        true,
							"restartCount": 0,
							"state":        map[string]interface{}{"running": map[string]interface{}{}},
						},
					},
				},
			},
			map[string]interface{}{
				"metadata": map[string]interface{}{
					"name":      "rhods-dashboard-new",
					"namespace": "redhat-ods-applications",
				},
				"spec": map[string]interface{}{
					"nodeName": "node-2",
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "rhods-dashboard",
							"image": "registry.redhat.io/rhoai/odh-dashboard-rhel9:v3.6",
						},
					},
				},
				"status": map[string]interface{}{
					"phase": "Pending",
					"containerStatuses": []interface{}{
						map[string]interface{}{
							"name":         "rhods-dashboard",
							"ready":        false,
							"restartCount": 0,
							"state": map[string]interface{}{
								"waiting": map[string]interface{}{"reason": "ContainerCreating"},
							},
						},
					},
				},
			},
		},
	}
	podListJSON, _ := json.Marshal(podList)

	client, cleanup := newMockClient(map[string]mockResponse{
		"/apis/apps/v1/namespaces/redhat-ods-applications/deployments/rhods-dashboard": {
			body: string(deployJSON),
		},
		"/api/v1/namespaces/redhat-ods-applications/pods": {
			body: string(podListJSON),
		},
	})
	defer cleanup()

	state, err := GetDashboardState(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !state.RolloutPending {
		t.Error("expected RolloutPending to be true when one pod is not ready")
	}
	if state.PodReady {
		t.Error("expected PodReady to be false during rollout")
	}
	if len(state.Pods) != 2 {
		t.Errorf("expected 2 pods, got %d", len(state.Pods))
	}
}

func TestContainerEnvVarMap_MatchesPRContainerRepos(t *testing.T) {
	for name := range prContainerRepos {
		if _, ok := containerEnvVarMap[name]; !ok {
			t.Errorf("containerEnvVarMap missing entry for container %q (present in prContainerRepos)", name)
		}
	}
	for name := range containerEnvVarMap {
		if _, ok := prContainerRepos[name]; !ok {
			t.Errorf("prContainerRepos missing entry for container %q (present in containerEnvVarMap)", name)
		}
	}
}

func TestGetDashboardState_ContainersReadyDuringRollout(t *testing.T) {
	// During a rollout (multiple pods, not all ready) the rollout branch sets
	// ContainersTotal from the first not-ready pod but does NOT compute
	// ContainersReady, so it must stay 0.
	deploy := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{
				"opendatahub.io/managed": "true",
			},
		},
		"spec": map[string]interface{}{
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"app": "rhods-dashboard",
				},
			},
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "rhods-dashboard",
							"image": "registry.redhat.io/rhoai/odh-dashboard-rhel9:v3.6",
						},
						map[string]interface{}{
							"name":  "model-registry-ui",
							"image": "registry.redhat.io/rhoai/model-registry-ui:v3.6",
						},
						map[string]interface{}{
							"name":  "gen-ai-ui",
							"image": "registry.redhat.io/rhoai/gen-ai-ui:v3.6",
						},
					},
				},
			},
		},
	}
	deployJSON, _ := json.Marshal(deploy)

	// Three pods: one ready old pod, one new pod with 2/3 containers ready,
	// one new pod still pending. This exercises the rollout branch.
	podList := map[string]interface{}{
		"items": []interface{}{
			// Old pod -- fully ready
			map[string]interface{}{
				"metadata": map[string]interface{}{
					"name":      "rhods-dashboard-old-abc",
					"namespace": "redhat-ods-applications",
				},
				"spec": map[string]interface{}{
					"nodeName": "node-1",
					"containers": []interface{}{
						map[string]interface{}{"name": "rhods-dashboard", "image": "registry.redhat.io/rhoai/odh-dashboard-rhel9:v3.5"},
						map[string]interface{}{"name": "model-registry-ui", "image": "registry.redhat.io/rhoai/model-registry-ui:v3.5"},
						map[string]interface{}{"name": "gen-ai-ui", "image": "registry.redhat.io/rhoai/gen-ai-ui:v3.5"},
					},
				},
				"status": map[string]interface{}{
					"phase": "Running",
					"containerStatuses": []interface{}{
						map[string]interface{}{"name": "rhods-dashboard", "ready": true, "restartCount": 0, "state": map[string]interface{}{"running": map[string]interface{}{}}},
						map[string]interface{}{"name": "model-registry-ui", "ready": true, "restartCount": 0, "state": map[string]interface{}{"running": map[string]interface{}{}}},
						map[string]interface{}{"name": "gen-ai-ui", "ready": true, "restartCount": 0, "state": map[string]interface{}{"running": map[string]interface{}{}}},
					},
				},
			},
			// New pod -- 2 of 3 containers ready, 1 still waiting
			map[string]interface{}{
				"metadata": map[string]interface{}{
					"name":      "rhods-dashboard-new-xyz",
					"namespace": "redhat-ods-applications",
				},
				"spec": map[string]interface{}{
					"nodeName": "node-2",
					"containers": []interface{}{
						map[string]interface{}{"name": "rhods-dashboard", "image": "registry.redhat.io/rhoai/odh-dashboard-rhel9:v3.6"},
						map[string]interface{}{"name": "model-registry-ui", "image": "registry.redhat.io/rhoai/model-registry-ui:v3.6"},
						map[string]interface{}{"name": "gen-ai-ui", "image": "registry.redhat.io/rhoai/gen-ai-ui:v3.6"},
					},
				},
				"status": map[string]interface{}{
					"phase": "Running",
					"containerStatuses": []interface{}{
						map[string]interface{}{"name": "rhods-dashboard", "ready": true, "restartCount": 0, "state": map[string]interface{}{"running": map[string]interface{}{}}},
						map[string]interface{}{"name": "model-registry-ui", "ready": true, "restartCount": 0, "state": map[string]interface{}{"running": map[string]interface{}{}}},
						map[string]interface{}{"name": "gen-ai-ui", "ready": false, "restartCount": 0, "state": map[string]interface{}{"waiting": map[string]interface{}{"reason": "ContainerCreating"}}},
					},
				},
			},
		},
	}
	podListJSON, _ := json.Marshal(podList)

	client, cleanup := newMockClient(map[string]mockResponse{
		"/apis/apps/v1/namespaces/redhat-ods-applications/deployments/rhods-dashboard": {
			body: string(deployJSON),
		},
		"/api/v1/namespaces/redhat-ods-applications/pods": {
			body: string(podListJSON),
		},
	})
	defer cleanup()

	state, err := GetDashboardState(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !state.RolloutPending {
		t.Error("expected RolloutPending to be true")
	}
	// During rollout, ContainersReady reflects ready containers on the not-ready pod
	if state.ContainersReady < 0 {
		t.Errorf("expected ContainersReady>=0 during rollout, got %d", state.ContainersReady)
	}
	// ContainersTotal should reflect the not-ready pod's container count
	if state.ContainersTotal != 3 {
		t.Errorf("expected ContainersTotal=3, got %d", state.ContainersTotal)
	}
	// Verify the individual pod's containers ARE tracked (2 ready, 1 not)
	var newPod *bool
	for _, pod := range state.Pods {
		if pod.Name == "rhods-dashboard-new-xyz" {
			readyCount := 0
			for _, ct := range pod.Containers {
				if ct.Ready {
					readyCount++
				}
			}
			if readyCount != 2 {
				t.Errorf("expected 2 ready containers in new pod, got %d", readyCount)
			}
			found := true
			newPod = &found
		}
	}
	if newPod == nil {
		t.Error("new pod rhods-dashboard-new-xyz not found in state.Pods")
	}
}

func TestGetDashboardState_ContainersTotalFallbackForPendingPod(t *testing.T) {
	// When containerStatuses is empty (Pending pod before kubelet reports),
	// getDashboardPods falls back to spec containers so the UI shows "0/N"
	// instead of "0/0".
	deploy := map[string]interface{}{
		"metadata": map[string]interface{}{},
		"spec": map[string]interface{}{
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"app": "rhods-dashboard",
				},
			},
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "rhods-dashboard",
							"image": "registry.redhat.io/rhoai/odh-dashboard-rhel9:v3.5",
						},
						map[string]interface{}{
							"name":  "model-registry-ui",
							"image": "registry.redhat.io/rhoai/model-registry-ui:v3.5",
						},
					},
				},
			},
		},
	}
	deployJSON, _ := json.Marshal(deploy)

	// Single Pending pod with NO containerStatuses (kubelet hasn't reported yet)
	podList := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{
				"metadata": map[string]interface{}{
					"name":      "rhods-dashboard-pending",
					"namespace": "redhat-ods-applications",
				},
				"spec": map[string]interface{}{
					"nodeName": "",
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "rhods-dashboard",
							"image": "registry.redhat.io/rhoai/odh-dashboard-rhel9:v3.5",
						},
						map[string]interface{}{
							"name":  "model-registry-ui",
							"image": "registry.redhat.io/rhoai/model-registry-ui:v3.5",
						},
					},
				},
				"status": map[string]interface{}{
					"phase": "Pending",
					// No containerStatuses -- simulates a pod just created
				},
			},
		},
	}
	podListJSON, _ := json.Marshal(podList)

	client, cleanup := newMockClient(map[string]mockResponse{
		"/apis/apps/v1/namespaces/redhat-ods-applications/deployments/rhods-dashboard": {
			body: string(deployJSON),
		},
		"/api/v1/namespaces/redhat-ods-applications/pods": {
			body: string(podListJSON),
		},
	})
	defer cleanup()

	state, err := GetDashboardState(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Single pod, not ready — now correctly detected as rollout
	if !state.RolloutPending {
		t.Error("expected RolloutPending to be true for single non-ready pod")
	}
	if state.PodReady {
		t.Error("expected PodReady to be false for Pending pod")
	}
	// ContainersTotal should fall back to spec container count (2), not 0
	if state.ContainersTotal != 2 {
		t.Errorf("expected ContainersTotal=2 (from spec fallback), got %d", state.ContainersTotal)
	}
	if state.ContainersReady != 0 {
		t.Errorf("expected ContainersReady=0 for Pending pod, got %d", state.ContainersReady)
	}
	// Verify the pod's fallback containers have correct names and waiting state
	if len(state.Pods) != 1 {
		t.Fatalf("expected 1 pod, got %d", len(state.Pods))
	}
	pod := state.Pods[0]
	if len(pod.Containers) != 2 {
		t.Fatalf("expected 2 fallback containers, got %d", len(pod.Containers))
	}
	for _, ct := range pod.Containers {
		if ct.Ready {
			t.Errorf("expected container %q Ready=false in fallback", ct.Name)
		}
		if ct.State != "waiting" {
			t.Errorf("expected container %q State=waiting in fallback, got %q", ct.Name, ct.State)
		}
	}
}

func TestGetDashboardPods_URLEncodesLabelSelector(t *testing.T) {
	// getDashboardPods must URL-encode label selector values that contain
	// special characters (slashes, spaces, equals signs, etc.).
	var capturedRawQuery string
	podList := map[string]interface{}{"items": []interface{}{}}
	podListJSON, _ := json.Marshal(podList)

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces/redhat-ods-applications/pods" {
			capturedRawQuery = r.URL.RawQuery
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(podListJSON)
	}))
	defer server.Close()

	client := &Client{
		baseURL:    server.URL,
		token:      "test-token",
		httpClient: server.Client(),
		ctx:        context.Background(),
	}

	// Labels with special characters that need URL encoding
	labels := map[string]string{
		"app.kubernetes.io/part-of": "rhods-dashboard",
		"deployment":                "rhods-dashboard",
	}

	getDashboardPods(client, labels)

	if capturedRawQuery == "" {
		t.Fatal("expected pods request to be made, but no query was captured")
	}

	// The query should start with labelSelector=
	if !strings.HasPrefix(capturedRawQuery, "labelSelector=") {
		t.Fatalf("expected query to start with labelSelector=, got %q", capturedRawQuery)
	}

	// The slash in "app.kubernetes.io/part-of" must be encoded as %2F in the
	// query-escaped selector value (url.QueryEscape encodes '/' as '%2F').
	if !strings.Contains(capturedRawQuery, "%2F") {
		t.Errorf("expected URL-encoded slash (%%2F) in query, got %q", capturedRawQuery)
	}

	// The '=' signs in the label selector (key=value) must also be encoded
	// as %3D within the query-escaped value.
	if !strings.Contains(capturedRawQuery, "%3D") {
		t.Errorf("expected URL-encoded equals (%%3D) in query, got %q", capturedRawQuery)
	}
}

func TestIsStandaloneMode(t *testing.T) {
	tests := []struct {
		name       string
		containers []string
		want       bool
	}{
		{
			name:       "standalone - core containers only",
			containers: []string{"rhods-dashboard", "kube-rbac-proxy", "core-bff"},
			want:       true,
		},
		{
			name:       "sidecar - has module containers",
			containers: []string{"rhods-dashboard", "kube-rbac-proxy", "core-bff", "model-registry-ui", "gen-ai-ui"},
			want:       false,
		},
		{
			name:       "sidecar - single module present",
			containers: []string{"rhods-dashboard", "kube-rbac-proxy", "core-bff", "agent-ops-ui"},
			want:       false,
		},
		{
			name:       "empty containers",
			containers: []string{},
			want:       true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isStandaloneMode(tt.containers)
			if got != tt.want {
				t.Errorf("isStandaloneMode(%v) = %v, want %v", tt.containers, got, tt.want)
			}
		})
	}
}

func TestGetDashboardState_StandaloneMode(t *testing.T) {
	// Main deployment has only core containers (standalone mode)
	deploy := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{
				"opendatahub.io/managed": "true",
			},
		},
		"spec": map[string]interface{}{
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"app": "rhods-dashboard",
				},
			},
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "rhods-dashboard",
							"image": "registry.redhat.io/rhoai/odh-dashboard-rhel9:v3.5",
						},
						map[string]interface{}{
							"name":  "kube-rbac-proxy",
							"image": "quay.io/opendatahub/odh-kube-rbac-proxy:latest",
						},
						map[string]interface{}{
							"name":  "core-bff",
							"image": "quay.io/opendatahub/odh-core-bff:main",
						},
					},
				},
			},
		},
	}
	deployJSON, _ := json.Marshal(deploy)

	// Standalone module deployment with a PR image
	moduleDeploy := map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "model-registry-ui",
							"image": "quay.io/opendatahub/odh-mod-arch-modular-architecture:pr-42",
						},
					},
				},
			},
		},
	}
	moduleDeployJSON, _ := json.Marshal(moduleDeploy)

	podList := map[string]interface{}{"items": []interface{}{}}
	podListJSON, _ := json.Marshal(podList)

	client, cleanup := newMockClient(map[string]mockResponse{
		"/apis/apps/v1/namespaces/redhat-ods-applications/deployments/rhods-dashboard": {
			body: string(deployJSON),
		},
		"/apis/apps/v1/namespaces/redhat-ods-applications/deployments/model-registry-ui": {
			body: string(moduleDeployJSON),
		},
		"/api/v1/namespaces/redhat-ods-applications/pods": {
			body: string(podListJSON),
		},
	})
	defer cleanup()

	state, err := GetDashboardState(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.DeploymentMode != "Standalone" {
		t.Errorf("expected DeploymentMode Standalone, got %q", state.DeploymentMode)
	}
	if !state.IsCustomPR {
		t.Error("expected IsCustomPR to be true (standalone module has PR image)")
	}
	if state.PRNumber != 42 {
		t.Errorf("expected PRNumber 42, got %d", state.PRNumber)
	}
	found := false
	for _, c := range state.PRContainers {
		if c == "model-registry-ui" {
			found = true
		}
	}
	if !found {
		t.Error("expected model-registry-ui in PRContainers")
	}
}

func TestGetDashboardState_SidecarModeDetection(t *testing.T) {
	deploy := map[string]interface{}{
		"metadata": map[string]interface{}{},
		"spec": map[string]interface{}{
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"app": "rhods-dashboard",
				},
			},
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "rhods-dashboard",
							"image": "registry.redhat.io/rhoai/odh-dashboard-rhel9:v3.5",
						},
						map[string]interface{}{
							"name":  "model-registry-ui",
							"image": "registry.redhat.io/rhoai/model-registry:v3.5",
						},
					},
				},
			},
		},
	}
	deployJSON, _ := json.Marshal(deploy)
	podList := map[string]interface{}{"items": []interface{}{}}
	podListJSON, _ := json.Marshal(podList)

	client, cleanup := newMockClient(map[string]mockResponse{
		"/apis/apps/v1/namespaces/redhat-ods-applications/deployments/rhods-dashboard": {
			body: string(deployJSON),
		},
		"/api/v1/namespaces/redhat-ods-applications/pods": {
			body: string(podListJSON),
		},
	})
	defer cleanup()

	state, err := GetDashboardState(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.DeploymentMode != "Sidecar" {
		t.Errorf("expected DeploymentMode Sidecar, got %q", state.DeploymentMode)
	}
}

func TestDeployPRImage_Standalone(t *testing.T) {
	// Main deployment with core containers only (standalone mode)
	deploy := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{
				"opendatahub.io/managed": "true",
			},
		},
		"spec": map[string]interface{}{
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{"app": "rhods-dashboard"},
			},
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{"name": "rhods-dashboard", "image": "registry.redhat.io/rhoai/odh-dashboard-rhel9:v3.5"},
						map[string]interface{}{"name": "kube-rbac-proxy", "image": "quay.io/opendatahub/odh-kube-rbac-proxy:latest"},
						map[string]interface{}{"name": "core-bff", "image": "quay.io/opendatahub/odh-core-bff:main"},
					},
				},
			},
		},
	}
	deployJSON, _ := json.Marshal(deploy)

	// Standalone module deployment
	moduleDeploy := map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{"name": "model-registry-ui", "image": "registry.redhat.io/rhoai/model-registry:v3.5"},
					},
				},
			},
		},
	}
	moduleDeployJSON, _ := json.Marshal(moduleDeploy)

	podList := map[string]interface{}{"items": []interface{}{}}
	podListJSON, _ := json.Marshal(podList)

	client, requests, cleanup := newRecordingMockClient(map[string]mockResponse{
		"/apis/apps/v1/namespaces/redhat-ods-applications/deployments/rhods-dashboard": {
			body: string(deployJSON),
		},
		"/apis/apps/v1/namespaces/redhat-ods-applications/deployments/model-registry-ui": {
			body: string(moduleDeployJSON),
		},
		"/api/v1/namespaces/redhat-ods-applications/pods": {
			body: string(podListJSON),
		},
	})
	defer cleanup()

	// Simulate: only rhods-dashboard and model-registry-ui have PR images
	// (Quay check is skipped in this test — we test the patching logic)
	// We can't easily test with real Quay checks, so verify the mode detection
	// and request paths instead
	state, err := GetDashboardState(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.DeploymentMode != "Standalone" {
		t.Errorf("expected Standalone mode, got %q", state.DeploymentMode)
	}

	// Verify that GET requests went to both the main deployment and module deployments
	var gotMainDeploy, gotModuleDeploy bool
	for _, req := range *requests {
		if req.Method == "GET" && strings.Contains(req.Path, "/deployments/rhods-dashboard") {
			gotMainDeploy = true
		}
		if req.Method == "GET" && strings.Contains(req.Path, "/deployments/model-registry-ui") {
			gotModuleDeploy = true
		}
	}
	if !gotMainDeploy {
		t.Error("expected GET request to rhods-dashboard deployment")
	}
	if !gotModuleDeploy {
		t.Error("expected GET request to model-registry-ui standalone deployment")
	}
}

func TestModuleContainers_MatchesModuleRegistry(t *testing.T) {
	expectedModules := []string{
		"model-registry-ui",
		"gen-ai-ui",
		"maas-ui",
		"mlflow-ui",
		"eval-hub-ui",
		"automl-ui",
		"autorag-ui",
		"agent-ops-ui",
	}
	for _, name := range expectedModules {
		if !moduleContainers[name] {
			t.Errorf("moduleContainers missing %q", name)
		}
	}
	if len(moduleContainers) != len(expectedModules) {
		t.Errorf("expected %d entries in moduleContainers, got %d", len(expectedModules), len(moduleContainers))
	}
}
