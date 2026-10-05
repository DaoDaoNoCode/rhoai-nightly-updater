package cluster

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 5, 22, 0, 0, 0, time.UTC)

func ts(d time.Duration) string { return testNow.Add(-d).Format(time.RFC3339) }

type podOpt func(map[string]interface{})

// makePod builds a ReplicaSet-owned pod JSON object like the API returns.
func makePod(ns, deployment, hash, suffix string, age time.Duration, opts ...podOpt) map[string]interface{} {
	ctrl := true
	p := map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":              deployment + "-" + hash + "-" + suffix,
			"namespace":         ns,
			"uid":               "pod-" + suffix,
			"creationTimestamp": ts(age),
			"labels":            map[string]interface{}{"pod-template-hash": hash},
			"ownerReferences": []interface{}{map[string]interface{}{
				"kind": "ReplicaSet", "name": deployment + "-" + hash, "uid": "rs-" + hash, "controller": ctrl,
			}},
		},
		"status": map[string]interface{}{"phase": "Running", "conditions": []interface{}{
			map[string]interface{}{"type": "PodScheduled", "status": "True"},
		}},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func status(p map[string]interface{}) map[string]interface{} {
	return p["status"].(map[string]interface{})
}

func phase(ph string) podOpt { return func(p map[string]interface{}) { status(p)["phase"] = ph } }

func unschedulable(msg string) podOpt {
	return func(p map[string]interface{}) {
		status(p)["phase"] = "Pending"
		status(p)["conditions"] = []interface{}{map[string]interface{}{
			"type": "PodScheduled", "status": "False", "reason": "Unschedulable", "message": msg,
		}}
	}
}

func waiting(container, reason, msg string, restarts int) podOpt {
	return func(p map[string]interface{}) {
		status(p)["containerStatuses"] = []interface{}{map[string]interface{}{
			"name": container, "ready": false, "restartCount": restarts,
			"state": map[string]interface{}{"waiting": map[string]interface{}{"reason": reason, "message": msg}},
		}}
	}
}

func lastTerminated(reason string, code int, finishedAgo time.Duration) podOpt {
	return func(p map[string]interface{}) {
		css := status(p)["containerStatuses"].([]interface{})
		css[0].(map[string]interface{})["lastState"] = map[string]interface{}{
			"terminated": map[string]interface{}{"reason": reason, "exitCode": code, "finishedAt": ts(finishedAgo)},
		}
	}
}

func running(container string, restarts int) podOpt {
	return func(p map[string]interface{}) {
		status(p)["containerStatuses"] = []interface{}{map[string]interface{}{
			"name": container, "ready": true, "restartCount": restarts,
			"state": map[string]interface{}{"running": map[string]interface{}{"startedAt": ts(time.Minute)}},
		}}
	}
}

func toDiagPod(t *testing.T, raw map[string]interface{}) diagPod {
	t.Helper()
	b, _ := json.Marshal(raw)
	var p diagPod
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClassifyPod(t *testing.T) {
	const ns = "redhat-ods-applications"
	tests := []struct {
		name     string
		pod      map[string]interface{}
		wantKind podIssueKind // "" means healthy / not reported
		wantIn   string       // substring of Message
	}{
		{"running and ready", makePod(ns, "dash", "h1", "a", time.Hour, running("c", 0)), "", ""},
		{"scheduled ContainerCreating past grace",
			makePod(ns, "odh-observability", "7b6d65d9b7", "vxzxh", 2*time.Hour, phase("Pending"), waiting("manager", "ContainerCreating", "", 0)),
			issueStuckCreating, ""},
		{"scheduled ContainerCreating within grace",
			makePod(ns, "x", "h", "a", 10*time.Second, phase("Pending"), waiting("manager", "ContainerCreating", "", 0)), "", ""},
		{"unschedulable",
			makePod(ns, "x", "h", "a", 5*time.Minute, unschedulable("0/6 nodes are available: 3 Insufficient cpu.")),
			issueUnschedulable, "Insufficient cpu"},
		{"unschedulable within grace", makePod(ns, "x", "h", "a", 5*time.Second, unschedulable("0/6 nodes")), "", ""},
		{"crashloop",
			makePod(ns, "trustyai-operator-module-controller-manager", "768dc89f6c", "ttk76", 48*time.Hour,
				waiting("manager", "CrashLoopBackOff", "back-off 5m0s restarting failed container", 527),
				lastTerminated("Error", 1, time.Minute)),
			issueCrashLoop, "last exit: Error (exit code 1)"},
		{"image pull backoff reported immediately",
			makePod(ns, "minio", "h", "a", 5*time.Second, phase("Pending"),
				waiting("minio", "ImagePullBackOff", `Back-off pulling image "quay.io/minio/minio:latest": unauthorized`, 0)),
			issueImagePull, "unauthorized"},
		{"missing config secret",
			makePod(ns, "x", "h", "a", time.Minute, phase("Pending"), waiting("c", "CreateContainerConfigError", `secret "db-creds" not found`, 0)),
			issueContainerConfig, `secret "db-creds" not found`},
		{"running but restarting recently",
			makePod(ns, "x", "h", "a", time.Hour, running("c", 7), lastTerminated("OOMKilled", 137, 5*time.Minute)),
			issueRestarting, "OOMKilled"},
		{"old restarts are not reported",
			makePod(ns, "x", "h", "a", 72*time.Hour, running("c", 7), lastTerminated("Error", 1, 2*time.Hour)), "", ""},
		{"completed pod", makePod(ns, "maas-ui", "d5d4dc448", "8868x", 48*time.Hour, phase("Succeeded")), "", ""},
		{"terminating pod",
			makePod(ns, "x", "h", "a", time.Hour, waiting("c", "CrashLoopBackOff", "", 9), func(p map[string]interface{}) {
				p["metadata"].(map[string]interface{})["deletionTimestamp"] = ts(time.Second)
			}), "", ""},
		{"never scheduled, no condition", makePod(ns, "x", "h", "a", 5*time.Minute, phase("Pending"), func(p map[string]interface{}) {
			status(p)["conditions"] = []interface{}{}
		}), issueNotScheduled, "scheduler"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyPod(toDiagPod(t, tt.pod), testNow)
			if tt.wantKind == "" {
				if got != nil {
					t.Fatalf("got %+v, want healthy", got)
				}
				return
			}
			if got == nil || got.Kind != tt.wantKind || !strings.Contains(got.Message, tt.wantIn) {
				t.Fatalf("got %+v, want kind %s with %q", got, tt.wantKind, tt.wantIn)
			}
		})
	}
}

func TestClassifyPod_InitContainerCrashLoop(t *testing.T) {
	p := makePod("ns", "x", "h", "a", time.Hour, phase("Pending"), func(p map[string]interface{}) {
		status(p)["initContainerStatuses"] = []interface{}{map[string]interface{}{
			"name": "init", "restartCount": 4,
			"state": map[string]interface{}{"waiting": map[string]interface{}{"reason": "CrashLoopBackOff"}},
		}}
	})
	got := classifyPod(toDiagPod(t, p), testNow)
	if got == nil || got.Kind != issueCrashLoop || got.Container != "init" {
		t.Fatalf("got %+v", got)
	}
}

func TestPodOwner(t *testing.T) {
	p := toDiagPod(t, makePod("ns", "odh-observability", "7b6d65d9b7", "vxzxh", time.Hour))
	if name, kind := podOwner(p); name != "odh-observability" || kind != "Deployment" {
		t.Fatalf("owner = %s %s", name, kind)
	}
}

// observabilityCluster reproduces the live redhat-ods-applications state:
// two scheduled odh-observability pods that cannot mount a missing Secret,
// and a crash-looping trustyai module operator.
func observabilityCluster(t *testing.T, eventsForbidden bool) (*fakeAPI, *Client) {
	f, c := newFakeAPI(t)
	const ns = "redhat-ods-applications"
	pods := []interface{}{
		makePod(ns, "odh-observability", "7b6d65d9b7", "vxzxh", 65*time.Hour, phase("Pending"), waiting("manager", "ContainerCreating", "", 0)),
		makePod(ns, "odh-observability", "84f945d8bc", "g5kp6", 65*time.Hour, phase("Pending"), waiting("manager", "ContainerCreating", "", 0)),
		makePod(ns, "trustyai-operator-module-controller-manager", "768dc89f6c", "ttk76", 65*time.Hour,
			waiting("manager", "CrashLoopBackOff", "back-off 5m0s restarting failed container=manager", 527),
			lastTerminated("Error", 1, time.Minute)),
		makePod(ns, "maas-ui", "d5d4dc448", "8868x", 65*time.Hour, phase("Succeeded")),
	}
	// Creation times are relative to the real clock for scanPods.
	for _, p := range pods {
		meta := p.(map[string]interface{})["metadata"].(map[string]interface{})
		meta["creationTimestamp"] = time.Now().Add(-65 * time.Hour).UTC().Format(time.RFC3339)
	}
	for _, p := range pods[2:3] {
		css := status(p.(map[string]interface{}))["containerStatuses"].([]interface{})
		css[0].(map[string]interface{})["lastState"] = map[string]interface{}{"terminated": map[string]interface{}{
			"reason": "Error", "exitCode": 1, "finishedAt": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		}}
	}
	f.obj("GET", "/api/v1/namespaces/"+ns+"/pods", map[string]interface{}{"items": pods})
	if eventsForbidden {
		f.status("GET", "/api/v1/namespaces/"+ns+"/events", http.StatusForbidden, "Forbidden")
	} else {
		f.handle("GET", "/api/v1/namespaces/"+ns+"/events", func(r *http.Request, _ []byte) (int, string) {
			sel := r.URL.Query().Get("fieldSelector")
			if !strings.Contains(sel, "involvedObject.name=odh-observability-") {
				return http.StatusOK, `{"items":[]}`
			}
			return http.StatusOK, `{"items":[
				{"type":"Normal","reason":"Scheduled","message":"Successfully assigned","lastTimestamp":"2026-10-03T05:00:00Z"},
				{"type":"Warning","reason":"FailedMount","message":"MountVolume.SetUp failed for volume \"webhook-certs\" : secret \"odh-observability-webhook-cert\" not found","count":1926,"lastTimestamp":"2026-10-05T22:15:49Z"}]}`
		})
	}
	return f, c
}

func TestRHOAIPods_ScheduledPendingPodIsNotInsufficientResources(t *testing.T) {
	f, c := observabilityCluster(t, false)
	resp, err := RunDiagnostics(c)
	if err != nil {
		t.Fatal(err)
	}
	p := findProblem(resp, "pod-stuck-creating-redhat-ods-applications-odh-observability")
	if p == nil {
		t.Fatalf("missing stuck-creating problem; problems: %+v", resp.Problems)
	}
	if !strings.Contains(p.Title, `secret "odh-observability-webhook-cert" not found`) || !strings.Contains(p.Title, "2 pods") {
		t.Errorf("title = %q", p.Title)
	}
	if p.AutoFixable || p.AutoFixAction != "" {
		t.Errorf("a missing Secret must not offer an auto-fix: %+v", p)
	}
	for _, pr := range resp.Problems {
		if strings.Contains(strings.ToLower(pr.Title), "insufficient") || strings.HasPrefix(pr.AutoFixAction, "assist-rollout") {
			t.Errorf("unexpected capacity diagnosis: %+v", pr)
		}
	}
	if len(p.AffectedObjects) != 2 {
		t.Errorf("affected = %v", p.AffectedObjects)
	}
	assertWrites(t, f)
}

func TestRHOAIPods_ReportsCrashLoopInApplicationsNamespace(t *testing.T) {
	_, c := observabilityCluster(t, false)
	resp, _ := RunDiagnostics(c)
	p := findProblem(resp, "pod-crashloop-redhat-ods-applications-trustyai-operator-module-controller-manager")
	if p == nil || !strings.Contains(p.Title, "527 restarts") || p.Severity != "warning" {
		t.Fatalf("problem = %+v", p)
	}
	if !strings.Contains(p.TechnicalCmd, "--previous") {
		t.Errorf("cmd = %s", p.TechnicalCmd)
	}
	if ch := findCheck(resp, "RHOAI pods"); ch == nil || ch.Status != "fail" {
		t.Errorf("check = %+v", ch)
	}
}

func TestRHOAIPods_EventsForbiddenSaysSo(t *testing.T) {
	_, c := observabilityCluster(t, true)
	resp, _ := RunDiagnostics(c)
	p := findProblem(resp, "pod-stuck-creating-redhat-ods-applications-odh-observability")
	if p == nil || !strings.Contains(strings.Join(p.Evidence, "\n"), "cannot read events") {
		t.Fatalf("problem = %+v", p)
	}
}

func TestOperatorPods_CrashLoopIsCritical(t *testing.T) {
	f, c := newFakeAPI(t)
	pod := makePod(SubNS, "rhods-operator", "75bd6cd964", "abcde", time.Hour,
		waiting("manager", "CrashLoopBackOff", "", 12), lastTerminated("Error", 2, time.Minute))
	pod["metadata"].(map[string]interface{})["creationTimestamp"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	f.obj("GET", "/api/v1/namespaces/"+SubNS+"/pods", map[string]interface{}{"items": []interface{}{pod}})
	out := checkOperatorPods(c)
	if len(out.problems) != 1 || out.problems[0].Severity != "critical" || out.problems[0].AutoFixable {
		t.Fatalf("out = %+v", out)
	}
}
