package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	rolloutNS  = "redhat-ods-operator"
	depPath    = "/apis/apps/v1/namespaces/redhat-ods-operator/deployments/rhods-operator"
	rsListPath = "/apis/apps/v1/namespaces/redhat-ods-operator/replicasets"
	podsPath   = "/api/v1/namespaces/redhat-ods-operator/pods"
)

type rolloutFixture struct {
	replicas       int
	strategy       map[string]interface{}
	managedFields  []interface{}
	newPodSchedule map[string]interface{} // PodScheduled condition of the new pod
	oldReady       int
}

func defaultRollout() rolloutFixture {
	return rolloutFixture{
		replicas: 3,
		strategy: map[string]interface{}{"type": "RollingUpdate", "rollingUpdate": map[string]interface{}{"maxSurge": "25%", "maxUnavailable": "25%"}},
		managedFields: []interface{}{
			map[string]interface{}{"manager": "olm", "operation": "Update", "fieldsV1": map[string]interface{}{
				"f:spec": map[string]interface{}{"f:strategy": map[string]interface{}{"f:rollingUpdate": map[string]interface{}{"f:maxUnavailable": map[string]interface{}{}}}}}},
		},
		newPodSchedule: map[string]interface{}{"type": "PodScheduled", "status": "False", "reason": "Unschedulable", "message": "0/3 nodes are available: 3 Insufficient cpu."},
		oldReady:       3,
	}
}

// install mirrors the live rhods-operator: selector name=rhods-operator (no
// app= label), and an unrelated ReplicaSet that matches the selector but is
// owned by another Deployment.
func (fx rolloutFixture) install(f *fakeAPI) {
	ctrl := true
	f.obj("GET", depPath, map[string]interface{}{
		"metadata": map[string]interface{}{"name": "rhods-operator", "uid": "dep-uid", "resourceVersion": "100", "generation": 5, "managedFields": fx.managedFields},
		"spec": map[string]interface{}{
			"replicas": fx.replicas,
			"selector": map[string]interface{}{"matchLabels": map[string]string{"name": "rhods-operator"}},
			"strategy": fx.strategy,
		},
	})
	f.handle("GET", rsListPath, func(r *http.Request, _ []byte) (int, string) {
		if r.URL.Query().Get("labelSelector") != "name=rhods-operator" {
			return http.StatusOK, `{"items":[]}`
		}
		b, _ := json.Marshal(map[string]interface{}{"items": []interface{}{
			map[string]interface{}{
				"metadata": map[string]interface{}{"name": "rhods-operator-new", "uid": "rs-new", "labels": map[string]string{"pod-template-hash": "new"},
					"annotations":     map[string]string{"deployment.kubernetes.io/revision": "10"},
					"ownerReferences": []interface{}{map[string]interface{}{"uid": "dep-uid", "controller": ctrl}}},
				"spec": map[string]interface{}{"replicas": 1}, "status": map[string]interface{}{"readyReplicas": 0},
			},
			map[string]interface{}{
				"metadata": map[string]interface{}{"name": "rhods-operator-old", "uid": "rs-old", "labels": map[string]string{"pod-template-hash": "old"},
					"annotations":     map[string]string{"deployment.kubernetes.io/revision": "9"},
					"ownerReferences": []interface{}{map[string]interface{}{"uid": "dep-uid", "controller": ctrl}}},
				"spec": map[string]interface{}{"replicas": fx.oldReady}, "status": map[string]interface{}{"readyReplicas": fx.oldReady},
			},
			map[string]interface{}{
				"metadata": map[string]interface{}{"name": "someone-else", "uid": "rs-x", "labels": map[string]string{"pod-template-hash": "x"},
					"annotations":     map[string]string{"deployment.kubernetes.io/revision": "99"},
					"ownerReferences": []interface{}{map[string]interface{}{"uid": "other-dep", "controller": ctrl}}},
				"spec": map[string]interface{}{"replicas": 1}, "status": map[string]interface{}{"readyReplicas": 0},
			},
		}})
		return http.StatusOK, string(b)
	})
	f.handle("GET", podsPath, func(r *http.Request, _ []byte) (int, string) {
		if r.URL.Query().Get("labelSelector") != "pod-template-hash=new" {
			return http.StatusOK, `{"items":[]}`
		}
		b, _ := json.Marshal(map[string]interface{}{"items": []interface{}{map[string]interface{}{
			"metadata": map[string]interface{}{"name": "rhods-operator-new-abc", "ownerReferences": []interface{}{map[string]interface{}{"kind": "ReplicaSet", "uid": "rs-new", "controller": ctrl}}},
			"status":   map[string]interface{}{"phase": "Pending", "conditions": []interface{}{fx.newPodSchedule}},
		}}})
		return http.StatusOK, string(b)
	})
}

func TestAssistRolloutFor_PatchesWithPreconditionAndRecordsOriginal(t *testing.T) {
	f, c := newFakeAPI(t)
	defaultRollout().install(f)
	f.json("PATCH", depPath, http.StatusOK, `{"spec":{"strategy":{"rollingUpdate":{"maxUnavailable":1}}}}`)

	res, err := ApplyFix(c, "assist-rollout:"+rolloutNS+"/rhods-operator")
	if err != nil || !res.Success {
		t.Fatalf("result = %+v, %v", res, err)
	}
	assertWrites(t, f, "PATCH "+depPath)
	var patch struct {
		Metadata struct {
			ResourceVersion string            `json:"resourceVersion"`
			Annotations     map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	body := f.requests("PATCH", depPath)[0].Body
	if err := json.Unmarshal([]byte(body), &patch); err != nil || patch.Metadata.ResourceVersion != "100" {
		t.Fatalf("patch = %s", body)
	}
	if !strings.Contains(patch.Metadata.Annotations[assistRolloutAnnotation], `"maxUnavailable":"25%"`) {
		t.Fatalf("original not recorded: %s", body)
	}
	if strings.Contains(body, "DELETE") || len(f.requests("DELETE", podsPath+"/rhods-operator-new-abc")) != 0 {
		t.Fatal("assist-rollout must not delete pods")
	}
}

func TestAssistRollout_NotApplicable(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*rolloutFixture)
		wantIn string
	}{
		{"pod scheduled but stuck creating", func(fx *rolloutFixture) {
			fx.newPodSchedule = map[string]interface{}{"type": "PodScheduled", "status": "True"}
		}, "not Unschedulable"},
		{"strategy already allows one unavailable", func(fx *rolloutFixture) {
			fx.replicas = 4
		}, "already allows 1"},
		// R1-4 / R5-F2: only a pure resource shortage is unblocked.
		{"untolerated taint", func(fx *rolloutFixture) {
			fx.newPodSchedule["message"] = "0/6 nodes are available: 3 Insufficient cpu, 3 node(s) had untolerated taint {node-role.kubernetes.io/master: }. preemption: 0/6 nodes are available: 3 No preemption victims found for incoming pod, 3 Preemption is not helpful for scheduling."
		}, "untolerated taint"},
		{"node selector or affinity", func(fx *rolloutFixture) {
			fx.newPodSchedule["message"] = "0/3 nodes are available: 3 node(s) didn't match Pod's node affinity/selector. preemption: 0/3 nodes are available: 3 Preemption is not helpful for scheduling."
		}, "node affinity/selector"},
		{"pod anti-affinity", func(fx *rolloutFixture) {
			fx.newPodSchedule["message"] = "0/3 nodes are available: 1 Insufficient memory, 2 node(s) didn't match pod anti-affinity rules."
		}, "anti-affinity"},
		{"volume zone", func(fx *rolloutFixture) {
			fx.newPodSchedule["message"] = "0/3 nodes are available: 3 node(s) had volume node affinity conflict."
		}, "volume node affinity conflict"},
		{"prefilter message", func(fx *rolloutFixture) {
			fx.newPodSchedule["message"] = "0/3 nodes are available: pod has unbound immediate PersistentVolumeClaims. preemption: 0/3 nodes are available: 3 Preemption is not helpful for scheduling."
		}, "unbound immediate PersistentVolumeClaims"},
		{"unparseable message", func(fx *rolloutFixture) {
			fx.newPodSchedule["message"] = "no nodes available to schedule pods"
		}, "no nodes available"},
		{"no message", func(fx *rolloutFixture) {
			delete(fx.newPodSchedule, "message")
		}, "the scheduler gave no reason"},
		{"GPU shortage", func(fx *rolloutFixture) {
			fx.newPodSchedule["message"] = "0/3 nodes are available: 3 Insufficient nvidia.com/gpu."
		}, "Insufficient nvidia.com/gpu"},
		// D4: a 1-replica module operator (often a failurePolicy Fail
		// webhook backend) would drop to 0 ready pods.
		{"single replica", func(fx *rolloutFixture) {
			fx.replicas = 1
			fx.oldReady = 1
		}, "only 1 pod(s) are ready (1 replica(s))"},
		{"one ready pod of three", func(fx *rolloutFixture) {
			fx.oldReady = 1
		}, "only 1 pod(s) are ready (3 replica(s))"},
		{"recreate strategy", func(fx *rolloutFixture) {
			fx.strategy = map[string]interface{}{"type": "Recreate"}
		}, "Recreate"},
		{"no old pods hold resources", func(fx *rolloutFixture) { fx.oldReady = 0 }, "no old pods"},
		{"an applier owns the field", func(fx *rolloutFixture) {
			fx.managedFields = []interface{}{map[string]interface{}{"manager": "workbenches-operator", "operation": "Apply", "fieldsV1": map[string]interface{}{
				"f:spec": map[string]interface{}{"f:strategy": map[string]interface{}{"f:rollingUpdate": map[string]interface{}{"f:maxUnavailable": map[string]interface{}{}}}}}}}
		}, "workbenches-operator manages its rollout strategy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c := newFakeAPI(t)
			fx := defaultRollout()
			tt.mutate(&fx)
			fx.install(f)
			res, err := AssistRolloutFor(c, rolloutNS, "rhods-operator")
			if err != nil || res.Success || res.ErrorCode != "nothing_to_do" || !strings.Contains(res.Message, tt.wantIn) {
				t.Fatalf("result = %+v, %v", res, err)
			}
			assertWrites(t, f)
		})
	}
}

func TestAssistRollout_ConflictAndValidation(t *testing.T) {
	f, c := newFakeAPI(t)
	defaultRollout().install(f)
	f.status("PATCH", depPath, http.StatusConflict, "Conflict")
	res, _ := AssistRolloutFor(c, rolloutNS, "rhods-operator")
	if res.Success || !strings.Contains(res.Message, "changed while it was being checked") {
		t.Fatalf("result = %+v", res)
	}
	for _, target := range [][2]string{{"kube-system", "coredns"}, {rolloutNS, "../x"}} {
		res, _ := AssistRolloutFor(c, target[0], target[1])
		if res.ErrorCode != "validation" {
			t.Fatalf("%v: %+v", target, res)
		}
	}
}

func TestAssistRollout_ProblemOffersFixOnlyWhenApplicable(t *testing.T) {
	f, c := newFakeAPI(t)
	defaultRollout().install(f)
	p := Problem{}
	attachRolloutAssist(c, &p, rolloutNS, "rhods-operator")
	if !p.AutoFixable || p.AutoFixAction != "assist-rollout:redhat-ods-operator/rhods-operator" ||
		!strings.Contains(p.ConfirmMessage, "rhods-operator-new-abc") || !strings.Contains(p.ConfirmMessage, "25% -> 1") {
		t.Fatalf("problem = %+v", p)
	}

	f2, c2 := newFakeAPI(t)
	fx := defaultRollout()
	fx.replicas = 4
	fx.install(f2)
	p2 := Problem{}
	attachRolloutAssist(c2, &p2, rolloutNS, "rhods-operator")
	if p2.AutoFixable {
		t.Fatalf("problem = %+v", p2)
	}
}

func restoreDeployment(maxUnavailable interface{}, note string, complete bool) map[string]interface{} {
	ready := 1
	if !complete {
		ready = 0
	}
	ann := map[string]string{}
	if note != "" {
		ann[assistRolloutAnnotation] = note
	}
	ru := map[string]interface{}{}
	if maxUnavailable != nil {
		ru["maxUnavailable"] = maxUnavailable
	}
	return map[string]interface{}{
		"metadata": map[string]interface{}{"name": "rhods-operator", "resourceVersion": "200", "generation": 6, "annotations": ann},
		"spec":     map[string]interface{}{"replicas": 1, "strategy": map[string]interface{}{"rollingUpdate": ru}},
		"status":   map[string]interface{}{"observedGeneration": 6, "replicas": 1, "updatedReplicas": 1, "readyReplicas": ready},
	}
}

func TestRestoreRolloutStrategy(t *testing.T) {
	note := `{"maxUnavailable":"25%","patchedAt":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
	tests := []struct {
		name      string
		dep       map[string]interface{}
		wantPatch string // substring of the patch body; "" = no patch
		wantOK    bool
		wantCode  string
	}{
		{"restores the original", restoreDeployment(1, note, true), `"maxUnavailable":"25%"`, true, ""},
		{"rollout still running", restoreDeployment(1, note, false), "", false, "prerequisites"},
		{"owner changed it: only the note is removed", restoreDeployment("10%", note, true), `"annotations":{"` + assistRolloutAnnotation + `":null}`, true, ""},
		{"no note", restoreDeployment(1, "", true), "", false, "nothing_to_do"},
		{"original unset is removed again", restoreDeployment(1, `{"maxUnavailable":null}`, true), `"maxUnavailable":null`, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c := newFakeAPI(t)
			f.obj("GET", depPath, tt.dep)
			f.json("PATCH", depPath, http.StatusOK, `{}`)
			res, err := ApplyFix(c, "restore-rollout-strategy:"+rolloutNS+"/rhods-operator")
			if err != nil || res.Success != tt.wantOK || res.ErrorCode != tt.wantCode {
				t.Fatalf("result = %+v, %v", res, err)
			}
			patches := f.requests("PATCH", depPath)
			if tt.wantPatch == "" {
				if len(patches) != 0 {
					t.Fatalf("unexpected patch %s", patches[0].Body)
				}
				return
			}
			if len(patches) != 1 || !strings.Contains(patches[0].Body, tt.wantPatch) || !strings.Contains(patches[0].Body, `"resourceVersion":"200"`) {
				t.Fatalf("patches = %+v", patches)
			}
			if strings.Contains(tt.name, "owner changed") && strings.Contains(patches[0].Body, "strategy") {
				t.Fatalf("overwrote the owner's value: %s", patches[0].Body)
			}
		})
	}
}

func TestNonResourceSchedulingReasons(t *testing.T) {
	for msg, want := range map[string]string{
		"0/3 nodes are available: 3 Insufficient cpu.": "[]",
		"0/6 nodes are available: 2 Insufficient memory, 3 Insufficient cpu, 6 Too many pods. preemption: 0/6 nodes are available: 6 No preemption victims found for incoming pod.": "[]",
		"0/6 nodes are available: 3 Insufficient cpu, 3 node(s) had untolerated taint {node-role.kubernetes.io/master: }.":                                                          "[3 node(s) had untolerated taint {node-role.kubernetes.io/master: }]",
		"0/3 nodes are available: 3 Insufficient ephemeral-storage.":                                                                                                                "[3 Insufficient ephemeral-storage]",
	} {
		if got := fmt.Sprint(nonResourceSchedulingReasons(msg)); got != want {
			t.Errorf("%q: got %s, want %s", msg, got, want)
		}
	}
}
