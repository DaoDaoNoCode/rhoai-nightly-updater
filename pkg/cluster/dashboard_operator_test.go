package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Model operator pause and workload patches on a stateful Kubernetes API. This
// exercises the production functions, including restart recovery, without
// changing an actual cluster.
type dashboardDevFixture struct {
	mu               sync.Mutex
	operator         dashboardDeployment
	workloads        map[string]dashboardDeployment
	writes           []string
	operatorPods     bool
	failWorkload     string
	failResume       bool
	conflicts        map[string]int
	onConflict       func(*dashboardDeployment)
	containerPatches map[string][][]string
	// pods are returned for namespace pod lists without the operator selector.
	pods []map[string]interface{}
	// failOperatorPatch makes every dashboard-operator patch fail with this status.
	failOperatorPatch int
	// operatorConflicts makes the next N dashboard-operator patches conflict.
	operatorConflicts int
	podListCalls      int
	// dashboardDeleting gives the Dashboard CR a deletionTimestamp.
	dashboardDeleting bool
	// readyOnResume reports the operator Ready as soon as it is scaled up.
	readyOnResume bool
}

func dashboardFixtureDeployment(name, container, image, owner string) dashboardDeployment {
	var d dashboardDeployment
	data := fmt.Sprintf(`{"metadata":{"name":%q,"uid":%q,"resourceVersion":"1","generation":1,"ownerReferences":[{"kind":"Dashboard","apiVersion":"components.platform.opendatahub.io/v1alpha1","controller":true,"uid":%q}]},"spec":{"replicas":1,"template":{"spec":{"containers":[{"name":%q,"image":%q}]}}},"status":{"observedGeneration":1,"replicas":1,"updatedReplicas":1,"readyReplicas":1}}`, name, "uid-"+name, owner, container, image)
	json.Unmarshal([]byte(data), &d)
	return d
}

func newDashboardDevFixture(t *testing.T) (*dashboardDevFixture, *Client) {
	t.Helper()
	f := &dashboardDevFixture{workloads: map[string]dashboardDeployment{}}
	json.Unmarshal([]byte(`{"metadata":{"name":"dashboard-operator","uid":"operator-uid","resourceVersion":"1","annotations":{}},"spec":{"replicas":2,"selector":{"matchLabels":{"app":"dashboard-operator"}},"template":{"spec":{"containers":[{"name":"manager","env":[{"name":"RELATED_IMAGE_ODH_DASHBOARD_IMAGE","value":"release-host"},{"name":"RELATED_IMAGE_ODH_MOD_ARCH_NOTEBOOKS_IMAGE","value":"release-notebooks"},{"name":"RELATED_IMAGE_ODH_MOD_ARCH_FUTURE_IMAGE","value":"release-future"},{"name":"RELATED_IMAGE_ODH_KUBE_RBAC_PROXY_IMAGE","value":"release-proxy"}]}]}}}}`), &f.operator)
	f.workloads[dashboardDeploymentName] = dashboardFixtureDeployment(dashboardDeploymentName, dashboardContainerName, "release-host", "dashboard-owner")
	f.workloads["notebooks-ui"] = dashboardFixtureDeployment("notebooks-ui", "notebooks-ui", "release-notebooks", "dashboard-owner")
	// No naming convention is required when the original image matches the env.
	f.workloads["future-workload"] = dashboardFixtureDeployment("future-workload", "custom-name", "release-future", "dashboard-owner")
	f.workloads["unrelated"] = dashboardFixtureDeployment("unrelated", dashboardContainerName, "release-host", "different-owner")
	f.workloads["proxy"] = dashboardFixtureDeployment("proxy", "kube-rbac-proxy", "release-proxy", "dashboard-owner")
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: context.Background()}
}

func (f *dashboardDevFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if r.Method == "GET" {
		switch name {
		case dashboardOperatorName:
			json.NewEncoder(w).Encode(f.operator)
		case "dashboards":
			deletion := ""
			if f.dashboardDeleting {
				deletion = `,"deletionTimestamp":"2026-10-05T10:00:00Z"`
			}
			io.WriteString(w, `{"items":[{"metadata":{"name":"default-dashboard"`+deletion+`}}]}`)
		case "deployments", "":
			items := []dashboardDeployment{}
			for _, d := range f.workloads {
				items = append(items, d)
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"items": items})
		case "pods":
			if !strings.Contains(r.URL.RawQuery, "labelSelector") {
				f.podListCalls++
				items := f.pods
				if items == nil {
					items = []map[string]interface{}{}
				}
				json.NewEncoder(w).Encode(map[string]interface{}{"items": items})
			} else if f.operatorPods {
				io.WriteString(w, `{"items":[{"metadata":{"name":"still-running"}}]}`)
			} else {
				io.WriteString(w, `{"items":[]}`)
			}
		default:
			if d, ok := f.workloads[name]; ok {
				json.NewEncoder(w).Encode(d)
			} else {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `{"kind":"Status","status":"Failure","reason":"NotFound","code":404}`)
			}
		}
		return
	}
	if r.Method != "PATCH" {
		io.WriteString(w, `{}`)
		return
	}
	var patch map[string]interface{}
	json.NewDecoder(r.Body).Decode(&patch)
	f.writes = append(f.writes, name)
	if name == dashboardOperatorName {
		spec := patch["spec"].(map[string]interface{})
		replicas := int(spec["replicas"].(float64))
		if replicas > 0 && f.failResume {
			w.WriteHeader(403)
			io.WriteString(w, `{"kind":"Status","message":"forbidden"}`)
			return
		}
		if f.failOperatorPatch != 0 {
			w.WriteHeader(f.failOperatorPatch)
			io.WriteString(w, `{"kind":"Status","status":"Failure","message":"injected"}`)
			return
		}
		metadata := patch["metadata"].(map[string]interface{})
		if f.operatorConflicts > 0 {
			f.operatorConflicts--
			f.operator.Metadata.ResourceVersion += "-next"
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"kind":"Status","status":"Failure","reason":"Conflict","message":"object has been modified"}`)
			return
		}
		if version, ok := metadata["resourceVersion"].(string); ok && version != f.operator.Metadata.ResourceVersion {
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"kind":"Status","status":"Failure","reason":"Conflict","message":"stale resourceVersion"}`)
			return
		}
		f.operator.Spec.Replicas = &replicas
		if f.operator.Metadata.Annotations == nil {
			f.operator.Metadata.Annotations = map[string]string{}
		}
		for key, value := range metadata["annotations"].(map[string]interface{}) {
			if value == nil {
				delete(f.operator.Metadata.Annotations, key)
			} else {
				f.operator.Metadata.Annotations[key] = value.(string)
			}
		}
		f.operator.Metadata.ResourceVersion += "+"
		if f.readyOnResume && replicas > 0 {
			f.operator.Status.Replicas, f.operator.Status.UpdatedReplicas, f.operator.Status.ReadyReplicas = replicas, replicas, replicas
		}
		json.NewEncoder(w).Encode(f.operator)
		return
	}
	if name == f.failWorkload {
		w.WriteHeader(403)
		io.WriteString(w, `{"kind":"Status","message":"forbidden"}`)
		return
	}
	d, ok := f.workloads[name]
	if !ok {
		w.WriteHeader(404)
		return
	}
	if remaining := f.conflicts[name]; remaining > 0 {
		f.conflicts[name] = remaining - 1
		d.Metadata.ResourceVersion += "-next"
		if f.onConflict != nil {
			f.onConflict(&d)
		}
		f.workloads[name] = d
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"kind":"Status","reason":"Conflict","message":"object has been modified"}`)
		return
	}
	if metadata, ok := patch["metadata"].(map[string]interface{}); ok {
		if version, ok := metadata["resourceVersion"].(string); ok && version != d.Metadata.ResourceVersion {
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"kind":"Status","reason":"Conflict","message":"stale resourceVersion"}`)
			return
		}
		if uid, ok := metadata["uid"].(string); ok && uid != d.Metadata.UID {
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"kind":"Status","reason":"Conflict","message":"uid precondition failed"}`)
			return
		}
		if annotations, ok := metadata["annotations"].(map[string]interface{}); ok {
			merged := map[string]string{}
			for k, v := range d.Metadata.Annotations {
				merged[k] = v
			}
			for k, v := range annotations {
				if v == nil {
					delete(merged, k)
				} else {
					merged[k] = v.(string)
				}
			}
			d.Metadata.Annotations = merged
		}
	}
	if spec, ok := patch["spec"].(map[string]interface{}); ok {
		if *f.operator.Spec.Replicas != 0 {
			panic("image patch while operator running")
		}
		containers := spec["template"].(map[string]interface{})["spec"].(map[string]interface{})["containers"].([]interface{})
		if f.containerPatches == nil {
			f.containerPatches = map[string][][]string{}
		}
		var names []string
		for _, raw := range containers {
			ct := raw.(map[string]interface{})
			names = append(names, ct["name"].(string))
			for i := range d.Spec.Template.Spec.Containers {
				if d.Spec.Template.Spec.Containers[i].Name == ct["name"] {
					d.Spec.Template.Spec.Containers[i].Image = ct["image"].(string)
				}
			}
		}
		f.containerPatches[name] = append(f.containerPatches[name], names)
	}
	f.workloads[name] = d
	json.NewEncoder(w).Encode(d)
}

func TestDashboardAtomicHostPatchRetriesStatusConflicts(t *testing.T) {
	mockDashboardRegistry(t, nil)
	f, c := newDashboardDevFixture(t)
	d := f.workloads[dashboardDeploymentName]
	core := d.Spec.Template.Spec.Containers[0]
	core.Name = "core-bff"
	core.Image = "release-core"
	d.Spec.Template.Spec.Containers = append(d.Spec.Template.Spec.Containers, core)
	f.workloads[dashboardDeploymentName] = d
	env := f.operator.Spec.Template.Spec.Containers[0].Env[0]
	env.Name = "RELATED_IMAGE_ODH_CORE_BFF_IMAGE"
	env.Value = "release-core"
	f.operator.Spec.Template.Spec.Containers[0].Env = append(f.operator.Spec.Template.Spec.Containers[0].Env, env)
	f.conflicts = map[string]int{dashboardDeploymentName: 2}
	result, err := DeployDashboardMain(c)
	if err != nil || !result.Success {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	patches := f.containerPatches[dashboardDeploymentName]
	if len(patches) != 1 || len(patches[0]) != 2 {
		t.Fatalf("expected one atomic host/core patch after retries, got %v", patches)
	}
	var state types.DashboardState
	populateDashboardDevState(c, &state)
	if !state.DevImagesMatchTarget {
		t.Fatalf("target images did not apply: %+v", state.DevImages)
	}
}

func TestDashboardConflictRetriesRejectConcurrentChanges(t *testing.T) {
	for _, change := range []string{"owner", "uid", "image", "container", "operator-resumed"} {
		t.Run(change, func(t *testing.T) {
			f, c := newDashboardDevFixture(t)
			images, err := discoverDashboardImages(c, &f.operator)
			if err != nil {
				t.Fatal(err)
			}
			var image types.DashboardDevImage
			for _, candidate := range images {
				if candidate.Deployment == dashboardDeploymentName {
					image = candidate
				}
			}
			zero := 0
			f.operator.Spec.Replicas = &zero
			f.conflicts = map[string]int{dashboardDeploymentName: 1}
			f.onConflict = func(d *dashboardDeployment) {
				switch change {
				case "owner":
					d.Metadata.OwnerReferences[0].UID = "another-dashboard"
				case "uid":
					d.Metadata.UID = "replacement"
				case "image":
					d.Spec.Template.Spec.Containers[0].Image = "someone-elses-build"
				case "container":
					d.Spec.Template.Spec.Containers = nil
				case "operator-resumed":
					one := 1
					f.operator.Spec.Replicas = &one
				}
			}
			if err := patchControlledDashboardImage(c, &f.operator, image, "selected-build"); err == nil {
				t.Fatal("overwrote a concurrent change")
			}
			if len(f.containerPatches[dashboardDeploymentName]) != 0 {
				t.Fatal("unsafe retry patched a changed workload")
			}
		})
	}
}

func TestDashboardConflictRetriesAreBounded(t *testing.T) {
	f, c := newDashboardDevFixture(t)
	images, err := discoverDashboardImages(c, &f.operator)
	if err != nil {
		t.Fatal(err)
	}
	var image types.DashboardDevImage
	for _, candidate := range images {
		if candidate.Deployment == dashboardDeploymentName {
			image = candidate
		}
	}
	zero := 0
	f.operator.Spec.Replicas = &zero
	f.conflicts = map[string]int{dashboardDeploymentName: 10}
	err = patchControlledDashboardImage(c, &f.operator, image, "selected-build")
	if !IsK8sError(err, http.StatusConflict) || len(f.writes) != 5 {
		t.Fatalf("err=%v attempts=%v", err, f.writes)
	}
}

func TestDashboardPartialStateDoesNotClaimMainIsApplied(t *testing.T) {
	mockDashboardRegistry(t, nil)
	f, c := newDashboardDevFixture(t)
	f.failWorkload = dashboardDeploymentName
	result, err := DeployDashboardMain(c)
	if err != nil || result.Success {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, oldSession := range []bool{false, true} {
		if oldSession {
			session, _ := dashboardSession(&f.operator)
			session.TargetsRecorded = false
			for i := range session.Bindings {
				session.Bindings[i].TargetImage = ""
			}
			raw, _ := json.Marshal(session)
			f.operator.Metadata.Annotations[dashboardDevAnnotation] = string(raw)
		}
		var state types.DashboardState
		populateDashboardDevState(c, &state)
		if !state.AllDevImagesReady || state.DevImagesMatchTarget {
			t.Fatalf("oldSession=%v state=%+v", oldSession, state)
		}
		for _, image := range state.DevImages {
			if image.Deployment == dashboardDeploymentName && (image.MatchesTarget || image.TargetImage == "") {
				t.Fatalf("failed host incorrectly marked applied: %+v", image)
			}
		}
	}
}

func mockDashboardRegistry(t *testing.T, statuses map[string]int) {
	t.Helper()
	original := quayHTTPClient
	quayHTTPClient = &http.Client{Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		status := http.StatusOK
		for key, s := range statuses {
			if strings.Contains(r.URL.Path, key) {
				status = s
			}
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Docker-Content-Digest": []string{"sha256:" + strings.Repeat("a", 64)}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	t.Cleanup(func() { quayHTTPClient = original })
}

func TestDashboardMainDiscoversOwnedComponentsAndRecoversAfterRestart(t *testing.T) {
	mockDashboardRegistry(t, nil)
	f, c := newDashboardDevFixture(t)
	result, err := DeployDashboardMain(c)
	if err != nil || !result.Success {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if *f.operator.Spec.Replicas != 0 || f.writes[0] != dashboardOperatorName {
		t.Fatalf("operator not paused first: %v", f.writes)
	}
	if f.workloads["unrelated"].Spec.Template.Spec.Containers[0].Image != "release-host" || f.workloads["proxy"].Spec.Template.Spec.Containers[0].Image != "release-proxy" {
		t.Fatal("changed uncontrolled or infrastructure images")
	}
	// The default RHOAI flavor deploys the Konflux odh-stable build of main.
	if got := f.workloads["future-workload"].Spec.Template.Spec.Containers[0].Image; !strings.Contains(got, "odh-mod-arch-future:odh-stable@sha256:") {
		t.Fatalf("future module not discovered: %s", got)
	}
	// Re-reading persisted session data must retain even nonstandard container names.
	var state types.DashboardState
	populateDashboardDevState(c, &state)
	if !state.IsDevMode || state.DevMode != "main" || len(state.DevImages) != 3 {
		t.Fatalf("state=%+v", state)
	}
	fresh := *c
	result, err = RevertDashboardImage(&fresh)
	if err != nil || !result.Success || *f.operator.Spec.Replicas != 2 {
		t.Fatalf("revert=%+v err=%v replicas=%d", result, err, *f.operator.Spec.Replicas)
	}
	if f.operator.Metadata.Annotations[dashboardDevAnnotation] != "" {
		t.Fatal("session retained after successful revert")
	}
	// Images remain until the controller reconciles; the tool only resumes it.
	if !strings.Contains(f.workloads[dashboardDeploymentName].Spec.Template.Spec.Containers[0].Image, ":odh-stable@") {
		t.Fatal("revert unexpectedly patched image directly")
	}
}

func TestDashboardPRSkipsUnbuiltComponents(t *testing.T) {
	mockDashboardRegistry(t, map[string]int{"odh-mod-arch-notebooks": 404, "odh-mod-arch-future": 404})
	f, c := newDashboardDevFixture(t)
	result, err := DeployPRImage(c, 123)
	if err != nil || !result.Success {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if f.workloads["notebooks-ui"].Spec.Template.Spec.Containers[0].Image != "release-notebooks" {
		t.Fatal("unbuilt component patched")
	}
	if !strings.Contains(f.workloads[dashboardDeploymentName].Spec.Template.Spec.Containers[0].Image, ":odh-pr-123@") {
		t.Fatal("host PR not patched")
	}
	var state types.DashboardState
	populateDashboardDevState(c, &state)
	if !state.DevImagesMatchTarget {
		t.Fatalf("unbuilt PR components incorrectly marked incomplete: %+v", state)
	}
}

func TestDashboardTargetStatusDetectsMissingOrReplacedWorkloads(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(fmt.Sprintf("removed=%v", removed), func(t *testing.T) {
			mockDashboardRegistry(t, nil)
			f, c := newDashboardDevFixture(t)
			result, err := DeployDashboardMain(c)
			if err != nil || !result.Success {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if removed {
				delete(f.workloads, "notebooks-ui")
			} else {
				d := f.workloads["notebooks-ui"]
				d.Metadata.UID = "replacement-uid"
				f.workloads["notebooks-ui"] = d
			}
			var state types.DashboardState
			populateDashboardDevState(c, &state)
			if state.DevImagesMatchTarget {
				t.Fatalf("missing or replaced target incorrectly marked applied: %+v", state)
			}
		})
	}
}

func TestDashboardLegacyTargetStatusPreservesPRReadiness(t *testing.T) {
	state := types.DashboardState{IsCustomPR: true, PodReady: true}
	populateDashboardDevStateWithOperator(nil, &state, nil, nil)
	if !state.AllDevImagesReady || !state.DevImagesMatchTarget || !state.IsDevMode {
		t.Fatalf("legacy PR readiness lost: %+v", state)
	}
}

func TestDashboardPreflightFailuresDoNotPauseOrPatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		mode   string
	}{{"missing-main", 404, "main"}, {"registry-error", 503, "pr"}, {"unauthorized", 401, "pr"}} {
		t.Run(tc.name, func(t *testing.T) {
			mockDashboardRegistry(t, map[string]int{"odh-mod-arch-notebooks": tc.status})
			f, c := newDashboardDevFixture(t)
			result, err := deployDashboardBuild(c, tc.mode, 123, "")
			if err != nil || result.Success || len(f.writes) > 0 {
				t.Fatalf("result=%+v err=%v writes=%v", result, err, f.writes)
			}
		})
	}
}

func TestDashboardPauseFailurePreservesRecoveryAndPreventsImagePatches(t *testing.T) {
	mockDashboardRegistry(t, nil)
	f, c := newDashboardDevFixture(t)
	f.operatorPods = true
	ctx, cancel := context.WithCancel(context.Background())
	c.ctx = ctx
	// Cancel as soon as the first pod check proves the controller still runs.
	original := c.httpClient.Transport
	c.httpClient.Transport = dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		resp, err := original.RoundTrip(r)
		if strings.HasSuffix(r.URL.Path, "/pods") {
			cancel()
		}
		return resp, err
	})
	result, err := DeployDashboardMain(c)
	if err != nil || result.Success || len(f.writes) != 1 || f.operator.Metadata.Annotations[dashboardDevAnnotation] == "" {
		t.Fatalf("result=%+v err=%v writes=%v", result, err, f.writes)
	}
	// Recovery uses a fresh request after the failed/cancelled operation.
	c.ctx = context.Background()
	f.operatorPods = false
	result, err = RevertDashboardImage(c)
	if err != nil || !result.Success || *f.operator.Spec.Replicas != 2 {
		t.Fatalf("revert=%+v err=%v", result, err)
	}
}

func TestDashboardPartialFailureAndRetryKeepOriginalReplicas(t *testing.T) {
	mockDashboardRegistry(t, nil)
	f, c := newDashboardDevFixture(t)
	f.failWorkload = "notebooks-ui"
	result, err := DeployDashboardMain(c)
	if err != nil || result.Success || result.ErrorCode != "partial_failure" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	f.failWorkload = ""
	result, err = DeployPRImage(c, 123)
	if err != nil || !result.Success {
		t.Fatalf("retry=%+v err=%v", result, err)
	}
	f.failResume = true
	result, err = RevertDashboardImage(c)
	if err != nil || result.Success || f.operator.Metadata.Annotations[dashboardDevAnnotation] == "" {
		t.Fatalf("failed resume=%+v err=%v", result, err)
	}
	f.failResume = false
	result, err = RevertDashboardImage(c)
	if err != nil || !result.Success || *f.operator.Spec.Replicas != 2 {
		t.Fatalf("recovery=%+v err=%v", result, err)
	}
}

func TestDashboardRefusesExternallyPausedOperatorAndInvalidOwnership(t *testing.T) {
	for _, tc := range []string{"external-pause", "unowned-host", "invalid-session"} {
		t.Run(tc, func(t *testing.T) {
			mockDashboardRegistry(t, nil)
			f, c := newDashboardDevFixture(t)
			switch tc {
			case "external-pause":
				zero := 0
				f.operator.Spec.Replicas = &zero
			case "unowned-host":
				d := f.workloads[dashboardDeploymentName]
				d.Metadata.OwnerReferences = nil
				f.workloads[dashboardDeploymentName] = d
			case "invalid-session":
				f.operator.Metadata.Annotations[dashboardDevAnnotation] = `{"operatorUID":"old-operator","replicas":1}`
			}
			result, err := DeployDashboardMain(c)
			if err != nil || result.Success || len(f.writes) > 0 {
				t.Fatalf("result=%+v err=%v writes=%v", result, err, f.writes)
			}
		})
	}
}

func TestDashboardImagePatchRefusesReplacedWorkloads(t *testing.T) {
	for _, change := range []string{"uid", "owner", "container"} {
		t.Run(change, func(t *testing.T) {
			f, c := newDashboardDevFixture(t)
			images, err := discoverDashboardImages(c, &f.operator)
			if err != nil {
				t.Fatal(err)
			}
			var target types.DashboardDevImage
			for _, image := range images {
				if image.Deployment == dashboardDeploymentName {
					target = image
				}
			}
			zero := 0
			f.operator.Spec.Replicas = &zero
			d := f.workloads[dashboardDeploymentName]
			switch change {
			case "uid":
				d.Metadata.UID = "replacement"
			case "owner":
				d.Metadata.OwnerReferences[0].UID = "another-dashboard"
			case "container":
				d.Spec.Template.Spec.Containers = nil
			}
			f.workloads[dashboardDeploymentName] = d
			if err := patchControlledDashboardImage(c, &f.operator, target, "test-image"); err == nil || len(f.writes) > 0 {
				t.Fatalf("err=%v writes=%v", err, f.writes)
			}
		})
	}
}
