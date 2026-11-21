package cluster

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dashdev"
)

// dashboardStateWith reads the dashboard state of a rhods-dashboard
// Deployment with spec.replicas (nil: unset) and the given pods (ready,
// image).
func dashboardStateWith(t *testing.T, replicas *int, pods []struct {
	ready bool
	image string
}) (podReady, rolloutPending bool) {
	t.Helper()
	const image = "registry.redhat.io/rhoai/odh-dashboard-rhel9:v3.5"
	spec := map[string]any{
		"selector": map[string]any{"matchLabels": map[string]any{"app": "rhods-dashboard"}},
		"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "rhods-dashboard", "image": image}}}},
	}
	if replicas != nil {
		spec["replicas"] = *replicas
	}
	deployJSON, _ := json.Marshal(map[string]any{"metadata": map[string]any{}, "spec": spec})
	var items []any
	for i, p := range pods {
		img := p.image
		if img == "" {
			img = image
		}
		items = append(items, map[string]any{
			"metadata": map[string]any{"name": fmt.Sprintf("rhods-dashboard-%d", i), "namespace": "redhat-ods-applications"},
			"spec":     map[string]any{"nodeName": "node-1", "containers": []any{map[string]any{"name": "rhods-dashboard", "image": img}}},
			"status": map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{
				"name": "rhods-dashboard", "ready": p.ready, "state": map[string]any{"running": map[string]any{}},
			}}},
		})
	}
	podListJSON, _ := json.Marshal(ownedByReplicaSets(map[string]any{"items": append([]any{}, items...)}))
	client, cleanup := newMockClient(map[string]mockResponse{
		dsciV2List: {body: defaultDSCIList},
		"/apis/apps/v1/namespaces/redhat-ods-applications/deployments/rhods-dashboard": {body: string(deployJSON)},
		"/api/v1/namespaces/redhat-ods-applications/pods":                              {body: string(podListJSON)},
	})
	defer cleanup()
	state, err := GetDashboardState(client)
	if err != nil {
		t.Fatal(err)
	}
	return state.PodReady, state.RolloutPending
}

// The other Deployment checks answer other questions on purpose, and keep
// their answers (K3 pins): deploymentReady (the operator that removes a
// finalizer is up: one ready pod is enough), dashboardDeploymentReady (a
// Dashboard Dev workload fully rolled out) and the S3 server's rolledOut
// (the current template has a ready pod).
func TestDeploymentChecksKeepTheirQuestions(t *testing.T) {
	deployment := func(replicas, ready int) string {
		return fmt.Sprintf(`{"spec":{"replicas":%d},"status":{"replicas":%d,"readyReplicas":%d}}`, replicas, replicas, ready)
	}
	for _, tc := range []struct {
		body string
		want bool
	}{
		{deployment(2, 1), true},
		{deployment(2, 2), true},
		{deployment(1, 0), false},
	} {
		c, cleanup := newMockClient(map[string]mockResponse{"/apis/apps/v1/namespaces/ns/deployments/op": {body: tc.body}})
		if ok, why := deploymentReady(c, "ns", "op"); ok != tc.want {
			t.Errorf("deploymentReady(%s) = %v %q", tc.body, ok, why)
		}
		cleanup()
	}

	two := 2
	var d dashdev.Deployment
	d.Spec.Replicas = &two
	d.Metadata.Generation, d.Status.ObservedGeneration = 3, 3
	d.Status.Replicas, d.Status.UpdatedReplicas, d.Status.ReadyReplicas = 2, 2, 2
	if !dashdev.DeploymentReady(d) {
		t.Error("2 of 2 updated and ready is not ready")
	}
	d.Status.ReadyReplicas = 1
	if dashdev.DeploymentReady(d) {
		t.Error("1 of 2 ready is ready")
	}
	d.Status.ReadyReplicas, d.Status.ObservedGeneration = 2, 2
	if dashdev.DeploymentReady(d) {
		t.Error("a template the controller has not seen is ready")
	}

	var s s3DeploymentSpec
	s.Metadata.Generation, s.Status.ObservedGeneration = 2, 2
	s.Status.UpdatedReplicas, s.Status.ReadyReplicas = 1, 1
	if !s.rolledOut() {
		t.Error("an updated, ready S3 server is not rolled out")
	}
	s.Status.ObservedGeneration = 1
	if s.rolledOut() {
		t.Error("an unobserved template is rolled out")
	}
}

// The dashboard is "ready" only when it runs as many current, ready pods
// as the Deployment asks for: kubectl rollout status waits while
// status.updatedReplicas < spec.replicas (k8s.io/kubectl
// pkg/polymorphichelpers/rollout_status.go), so one pod of two is still
// rolling out. The cases that were already right are pinned.
func TestDashboardStateCountsTheReplicas(t *testing.T) {
	type pod = struct {
		ready bool
		image string
	}
	one, two := 1, 2
	for _, tc := range []struct {
		name              string
		replicas          *int
		pods              []pod
		podReady, pending bool
	}{
		// Pins: answers that were right before.
		{"one of one ready", &one, []pod{{ready: true}}, true, false},
		{"replicas unset, one ready", nil, []pod{{ready: true}}, true, false},
		{"two of two ready", &two, []pod{{ready: true}, {ready: true}}, true, false},
		{"one of one not ready", &one, []pod{{ready: false}}, false, true},
		{"surge: an old pod still runs", &one, []pod{{ready: true}, {ready: true, image: "registry.redhat.io/rhoai/odh-dashboard-rhel9:v3.4"}}, false, true},
		{"two of two, one not ready", &two, []pod{{ready: true}, {ready: false}}, false, true},
		// K3: one pod of two (the second was never created).
		{"one of two ready", &two, []pod{{ready: true}}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			podReady, pending := dashboardStateWith(t, tc.replicas, tc.pods)
			if podReady != tc.podReady || pending != tc.pending {
				t.Fatalf("podReady=%v rolloutPending=%v; want %v %v", podReady, pending, tc.podReady, tc.pending)
			}
		})
	}
}
