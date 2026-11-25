package cluster

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/authz"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kueue"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	kqGroup     = "/apis/kueue.x-k8s.io"
	kqAPI       = "/apis/kueue.x-k8s.io/v1beta2"
	kqFlavor    = kqAPI + "/resourceflavors/" + kueue.FlavorName
	kqCQ        = kqAPI + "/clusterqueues/" + kueue.ClusterQueueName
	kqLQs       = kqAPI + "/namespaces/proj/localqueues"
	kqLQ        = kqLQs + "/" + kueue.LocalQueueName
	kqInstance  = "/apis/kueue.openshift.io/v1/kueues/cluster"
	kqRayCs     = "/apis/ray.io/v1/namespaces/proj/rayclusters"
	kqRayJobs   = "/apis/ray.io/v1/namespaces/proj/rayjobs"
	kqWorkloads = kqAPI + "/namespaces/proj/workloads"
	kqConfig    = "/apis/opendatahub.io/v1alpha/namespaces/redhat-ods-applications/odhdashboardconfigs/odh-dashboard-config"
	kqProfile   = "/apis/infrastructure.opendatahub.io/v1/namespaces/redhat-ods-applications/hardwareprofiles/" + kueue.ProfileName
	kqV1beta2   = `{"name":"kueue.x-k8s.io","versions":[{"version":"v1beta2"},{"version":"v1beta1"}],"preferredVersion":{"version":"v1beta2"}}`
)

// putKueueCluster builds a fake cluster with Kueue ready to exercise: DSC
// has kueue Unmanaged and KueueReady True, ray Managed, kueue.x-k8s.io/v1beta2
// on the API, the Kueue CR cluster Available, namespace proj marked as a data
// science project, and dashboard config without disableKueue. The caller gets
// a user-scoped client.
func putKueueCluster(t *testing.T) (*resourceFake, *Client) {
	t.Helper()
	f, c := newResourceFake(t)
	f.putJSON(wbDSCv2+"/default-dsc", `{"metadata":{"name":"default-dsc"},"spec":{"components":{"kueue":{"managementState":"Unmanaged"},"ray":{"managementState":"Managed"}}},
		"status":{"components":{"kueue":{"managementState":"Unmanaged"},"ray":{"managementState":"Managed"}},"conditions":[{"type":"KueueReady","status":"True"}]}}`)
	f.groups = map[string]string{kqGroup: kqV1beta2}
	f.putJSON(kqInstance, `{"metadata":{"name":"cluster"},"status":{"conditions":[{"type":"Available","status":"True"}]}}`)
	f.putJSON(wbProjectNS, `{"metadata":{"name":"proj","uid":"ns-uid","labels":{"opendatahub.io/dashboard":"true"}},"status":{"phase":"Active"}}`)
	f.putJSON(kqConfig, `{"metadata":{"name":"odh-dashboard-config","resourceVersion":"5"},"spec":{"dashboardConfig":{}}}`)
	f.onMergePatch = func(r *http.Request, k fakeKey, patch, next map[string]any) {
		updateFieldOwnership(r.URL.Query().Get("fieldManager"), f.objects[k], patch, next)
	}
	u := userClientOf(c, fakeUserToken)
	u.SetUsername("kube:admin")
	return f, u
}

func runKueueOp(t *testing.T, c *Client, req types.KueueRequest) (*types.OperationResponse, []UpdateStepEvent) {
	t.Helper()
	var events []UpdateStepEvent
	res, err := RunKueue(c, req, func(e UpdateStepEvent) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	return res, events
}

func kueueRecord(t *testing.T, c *Client) kueue.Record {
	t.Helper()
	rec, err := kueue.ReadRecord(c)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func nsLabels(f *resourceFake) map[string]any {
	return ensureMap(ensureMap(f.get(wbProjectNS), "metadata"), "labels")
}

// Queue manifests match the self-managed layout from the docs: LocalQueue must
// not be "default" (otherwise Kueue would admit every new pod workload), the
// ClusterQueue scopes to the project only, disables preemption, and sets
// nominal quotas large enough for both Ray samples plus one opt-in-profile
// workload; the Ray samples share a single cluster spec and both bear the
// queue name label.
func TestKueueManifests(t *testing.T) {
	if kueue.LocalQueueName == "default" || kueue.ClusterQueueName == "default" {
		t.Fatal("a queue is named default")
	}
	lq := kueue.LocalQueueManifest("proj")
	if lq["metadata"].(map[string]any)["name"] == "default" || lq["spec"].(map[string]any)["clusterQueue"] != kueue.ClusterQueueName {
		t.Fatalf("LocalQueue: %v", lq)
	}
	assertJSONEqual(t, kueue.ClusterQueueManifest("proj"), `{"apiVersion":"kueue.x-k8s.io/v1beta2","kind":"ClusterQueue",
		"metadata":{"name":"nightly-kueue","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}},
		"spec":{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"proj"}},
			"resourceGroups":[{"coveredResources":["cpu","memory"],"flavors":[{"name":"nightly-kueue-cpu","resources":[{"name":"cpu","nominalQuota":"6"},{"name":"memory","nominalQuota":"19Gi"}]}]}]}}`)
	assertJSONEqual(t, kueue.FlavorManifest(), `{"apiVersion":"kueue.x-k8s.io/v1beta2","kind":"ResourceFlavor","metadata":{"name":"nightly-kueue-cpu","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}},"spec":{}}`)
	// Combined sample requests plus the profile default must fit under the quota.
	sum := func(obj map[string]any) (cpu, memMi float64) {
		raw, _ := json.Marshal(obj)
		var walk func(v any)
		walk = func(v any) {
			switch t := v.(type) {
			case map[string]any:
				if req, ok := t["requests"].(map[string]any); ok {
					c, _ := serving.ParseQuantity(req["cpu"].(string))
					m, _ := serving.ParseQuantity(req["memory"].(string))
					n := 1.0
					cpu, memMi = cpu+c*n, memMi+m/(1<<20)*n
				}
				for _, child := range t {
					walk(child)
				}
			case []any:
				for _, child := range t {
					walk(child)
				}
			}
		}
		var generic any
		_ = json.Unmarshal(raw, &generic)
		walk(generic)
		return
	}
	c1, m1 := sum(kueue.RayClusterManifest("proj", kueue.DefaultRayImage))
	c2, m2 := sum(kueue.RayJobManifest("proj", kueue.DefaultRayImage))
	if c1+c2+1 > 6 || m1+m2+2048 > 19*1024 || c1 != 2 || c2 != 2.5 || m1 != 8*1024 || m2 != 9*1024 {
		t.Fatalf("quota 6 CPUs / 19Gi does not hold the samples: %v+%v CPU, %v+%v Mi", c1, c2, m1, m2)
	}
	// Head and worker sized as the CodeFlare SDK's defaults, requests below
	// limits (a 2Gi head was OOMKilled live).
	spec := kueue.RayClusterSpec(kueue.DefaultRayImage)
	head := spec["headGroupSpec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["resources"]
	worker := spec["workerGroupSpecs"].([]any)[0].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["resources"]
	assertJSONEqual(t, head, `{"requests":{"cpu":"1","memory":"5Gi"},"limits":{"cpu":"2","memory":"8Gi"}}`)
	assertJSONEqual(t, worker, `{"requests":{"cpu":"1","memory":"3Gi"},"limits":{"cpu":"1","memory":"6Gi"}}`)
	if cpu, memory := kueue.Quota(); cpu != "6" || memory != "19Gi" {
		t.Fatalf("Quota() = %s, %s; want 6, 19Gi", cpu, memory)
	}
	rc, rj := kueue.RayClusterManifest("proj", "img:2.47.1-x"), kueue.RayJobManifest("proj", "img:2.47.1-x")
	a, _ := json.Marshal(rc["spec"])
	b, _ := json.Marshal(rj["spec"].(map[string]any)["rayClusterSpec"])
	if string(a) != string(b) || !strings.Contains(string(a), `"rayVersion":"2.47.1"`) {
		t.Fatalf("the samples do not share one cluster spec:\n%s\n%s", a, b)
	}
	for _, obj := range []map[string]any{rc, rj} {
		if obj["metadata"].(map[string]any)["labels"].(map[string]any)[kueue.QueueNameLabel] != kueue.LocalQueueName {
			t.Fatalf("queue label: %v", obj["metadata"])
		}
	}
	if kueue.DefaultRayImage != "quay.io/modh/ray:2.47.1-py311-cu121" || strings.Contains(kueue.DefaultRayImage, "-cpu") {
		t.Fatalf("default image %s", kueue.DefaultRayImage)
	}
	p := kueue.ProfileManifest("apps", "now")
	sched := p["spec"].(map[string]any)["scheduling"].(map[string]any)
	if sched["type"] != "Queue" || sched["kueue"].(map[string]any)["localQueueName"] != kueue.LocalQueueName {
		t.Fatalf("profile scheduling %v", sched)
	}
	if kueue.Operator.Pkg != "kueue-operator" || kueue.Operator.Channel != nil || kueue.Operator.OwnNamespace {
		t.Fatalf("operator spec %+v", kueue.Operator)
	}
}

func TestKueueValidate(t *testing.T) {
	for _, tc := range []struct {
		req  types.KueueRequest
		want string
	}{
		{types.KueueRequest{Action: "setup", Project: "proj", RayCluster: true}, ""},
		{types.KueueRequest{Action: "teardown"}, ""},
		{types.KueueRequest{Action: "optin"}, ""},
		{types.KueueRequest{Action: "delete-sample", Sample: "rayjob"}, ""},
		{types.KueueRequest{Action: "nuke"}, "action must be"},
		{types.KueueRequest{Action: "setup"}, "a valid project"},
		{types.KueueRequest{Action: "setup", Project: "Bad_Name"}, "a valid project"},
		{types.KueueRequest{Action: "delete-sample", Sample: "pod"}, "sample must be"},
		{types.KueueRequest{Action: "teardown", RayJob: true}, "go with setup"},
		{types.KueueRequest{Action: "setup", Project: "proj", RayImage: "img; rm -rf /"}, "rayImage must be"},
	} {
		if got := kueue.Validate(tc.req); (tc.want == "" && got != "") || !strings.Contains(got, tc.want) {
			t.Errorf("%+v: %q, want %q", tc.req, got, tc.want)
		}
	}
}

// WorkloadState mirrors dashboard ordering when it maps conditions to a state.
func TestKueueWorkloadState(t *testing.T) {
	c := func(typ, status, reason, msg string) kueue.Condition {
		return kueue.Condition{Type: typ, Status: status, Reason: reason, Message: msg}
	}
	for _, tc := range []struct {
		conds []kueue.Condition
		want  string
	}{
		{nil, kueue.StateQueued},
		{[]kueue.Condition{c("QuotaReserved", "False", "Pending", "waiting")}, kueue.StateQueued},
		{[]kueue.Condition{c("QuotaReserved", "False", "Inadmissible", "no quota")}, kueue.StateInadmissible},
		{[]kueue.Condition{c("QuotaReserved", "True", "", ""), c("Admitted", "True", "", "")}, kueue.StateAdmitted},
		{[]kueue.Condition{c("Admitted", "True", "", ""), c("PodsReady", "True", "", "")}, kueue.StateRunning},
		{[]kueue.Condition{c("PodsReady", "True", "", ""), c("Finished", "True", "Succeeded", "Job finished successfully")}, kueue.StateComplete},
		{[]kueue.Condition{c("Finished", "True", "Failed", "Job failed")}, kueue.StateFailed},
		{[]kueue.Condition{c("Evicted", "True", "Preempted", "")}, kueue.StatePreempted},
		{[]kueue.Condition{c("Evicted", "True", "PodsReadyTimeout", ""), c("Admitted", "True", "", "")}, kueue.StateEvicted},
	} {
		if got, _ := kueue.WorkloadState(tc.conds); got != tc.want {
			t.Errorf("%+v: %s, want %s", tc.conds, got, tc.want)
		}
	}
}

// Setup dry-runs each create server-side, then provisions queues, patches the
// project label, and creates samples—in that sequence—all authenticated as
// the user, persisting UIDs and whether the label was applied.
func TestKueueSetup(t *testing.T) {
	f, u := putKueueCluster(t)
	res, events := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj", RayCluster: true, RayJob: true})
	if !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	var steps []string
	for _, e := range events {
		if e.Status == "success" {
			steps = append(steps, e.Step)
		}
	}
	if strings.Join(steps, " ") != "check queues project samples" {
		t.Fatalf("steps %v", steps)
	}
	order := []string{"POST " + kqAPI + "/resourceflavors", "POST " + kqAPI + "/clusterqueues", "POST " + kqLQs, "PATCH " + wbProjectNS, "POST " + kqRayCs, "POST " + kqRayJobs}
	got := strings.Join(f.mutations(), "\n")
	if got != strings.Join(order, "\n") {
		t.Fatalf("writes:\n%s\nwant\n%s", got, strings.Join(order, "\n"))
	}
	for _, p := range []string{kqAPI + "/resourceflavors", kqAPI + "/clusterqueues", kqLQs, kqRayCs, kqRayJobs} {
		calls := f.callsTo("POST", p)
		if len(calls) != 2 || calls[0].Query.Get("dryRun") != "All" || calls[1].Query.Get("dryRun") != "" {
			t.Fatalf("%s: want a dry run then the create, got %d calls", p, len(calls))
		}
		for _, call := range calls {
			if call.Token != fakeUserToken {
				t.Fatalf("%s with the ServiceAccount's token", p)
			}
		}
	}
	if nsLabels(f)[kueue.ProjectLabel] != "true" {
		t.Fatalf("labels %v", nsLabels(f))
	}
	rec := kueueRecord(t, u)
	if rec.Project != "proj" || rec.ProjectUID != "ns-uid" || !rec.LabelAdded || rec.FlavorUID == "" || rec.ClusterQueueUID == "" || rec.LocalQueueUID == "" || rec.RayClusterUID == "" || rec.RayJobUID == "" {
		t.Fatalf("record %+v", rec)
	}
	// A repeat setup must not issue further creates.
	before := len(f.mutations())
	if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj", RayCluster: true}); !res.Success || len(f.mutations()) != before {
		t.Fatalf("second setup: %+v %v", res, f.mutations()[before:])
	}
	// Status calls with the ServiceAccount client must fail.
	if _, err := GetKueueStatus(u, u.Bookkeeping(), "proj"); !errors.Is(err, authz.ErrNotUserClient) {
		t.Fatalf("status with the ServiceAccount: %v", err)
	}
}

// Setup aborts with no mutations when prerequisites would hide the flow in the
// dashboard or would collide with existing project queue state.
func TestKueueSetupRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, want, code string
		change           func(f *resourceFake)
	}{
		{"v1beta1 only", "does not serve kueue.x-k8s.io/v1beta2", "unsupported", func(f *resourceFake) {
			f.groups[kqGroup] = `{"versions":[{"version":"v1beta1"}],"preferredVersion":{"version":"v1beta1"}}`
		}},
		{"api not served", "is not served yet", "prerequisites", func(f *resourceFake) { delete(f.groups, kqGroup) }},
		{"labelPolicy None", "labelPolicy None", "unsupported", func(f *resourceFake) {
			f.putJSON(kqInstance, `{"spec":{"config":{"workloadManagement":{"labelPolicy":"None"}}},"status":{"conditions":[{"type":"Available","status":"True"}]}}`)
		}},
		{"no Kueue CR", "has not created the Kueue CR", "not_ready", func(f *resourceFake) { f.deleteObject(kqInstance) }},
		{"Kueue CR not Available", "is not Available", "not_ready", func(f *resourceFake) {
			f.putJSON(kqInstance, `{"status":{"conditions":[{"type":"Available","status":"False","message":"cert-manager missing"}]}}`)
		}},
		{"kueue Removed", "Set up its prerequisites first (above): Kueue is turned off on this cluster.", "prerequisites", func(f *resourceFake) {
			f.putJSON(wbDSCv2+"/default-dsc", `{"metadata":{"name":"default-dsc"},"spec":{"components":{"kueue":{"managementState":"Removed"}}}}`)
		}},
		{"default LocalQueue", "LocalQueue named default", "validation", func(f *resourceFake) {
			f.putJSON(kqLQs+"/default", `{"metadata":{"name":"default","namespace":"proj"},"spec":{"clusterQueue":"x"}}`)
		}},
		{"foreign ClusterQueue", "was not created by this tool", "not_managed", func(f *resourceFake) {
			f.putJSON(kqCQ, `{"metadata":{"name":"nightly-kueue","uid":"theirs","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`)
		}},
		{"ray off", "component ray", "prerequisites", func(f *resourceFake) {
			f.putJSON(wbDSCv2+"/default-dsc", `{"metadata":{"name":"default-dsc"},"spec":{"components":{"kueue":{"managementState":"Unmanaged"},"ray":{"managementState":"Removed"}}},
				"status":{"conditions":[{"type":"KueueReady","status":"True"}]}}`)
		}},
		{"not a ds project", "not a data science project", "validation", func(f *resourceFake) {
			f.putJSON(wbProjectNS, `{"metadata":{"name":"proj","uid":"ns-uid"}}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, u := putKueueCluster(t)
			tc.change(f)
			res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj", RayCluster: true})
			if res.Success || !strings.Contains(res.Message, tc.want) || res.ErrorCode != tc.code || !strings.Contains(res.Message, "Nothing was changed") {
				t.Fatalf("got %+v", res)
			}
			if m := f.mutations(); len(m) != 0 {
				t.Fatalf("wrote %v", m)
			}
		})
	}
}

// If the project already carries the managed label, setup skips recording it
// and teardown must not strip it.
func TestKueueLabelOnlyTheTools(t *testing.T) {
	f, u := putKueueCluster(t)
	f.putJSON(wbProjectNS, `{"metadata":{"name":"proj","uid":"ns-uid","labels":{"opendatahub.io/dashboard":"true","kueue.openshift.io/managed":"true"}}}`)
	if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	if len(f.callsTo("PATCH", wbProjectNS)) != 0 || kueueRecord(t, u).LabelAdded {
		t.Fatal("labelled a labelled project")
	}
	if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "teardown"}); !res.Success {
		t.Fatalf("teardown: %+v", res)
	}
	if nsLabels(f)[kueue.ProjectLabel] != "true" || len(f.callsTo("PATCH", wbProjectNS)) != 0 {
		t.Fatal("removed a label the tool did not add")
	}
}

// Teardown removes samples, LocalQueue, ClusterQueue, and ResourceFlavor in
// that order using stored UIDs, drops only the label this tool added, and
// clears the bookkeeping record.
func TestKueueTeardownOrder(t *testing.T) {
	f, u := putKueueCluster(t)
	if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj", RayCluster: true, RayJob: true}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	before := len(f.mutations())
	res, events := runKueueOp(t, u, types.KueueRequest{Action: "teardown"})
	if !res.Success {
		t.Fatalf("teardown: %+v", res)
	}
	want := []string{"DELETE " + kqRayCs + "/" + kueue.RayClusterName, "DELETE " + kqRayJobs + "/" + kueue.RayJobName, "DELETE " + kqLQ, "DELETE " + kqCQ, "DELETE " + kqFlavor, "PATCH " + wbProjectNS}
	if got := strings.Join(f.mutations()[before:], "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("writes:\n%s", got)
	}
	for _, p := range []string{kqLQ, kqCQ, kqFlavor} {
		for _, call := range f.callsTo("DELETE", p) {
			if !strings.Contains(call.Body, `"uid":"uid-`) {
				t.Fatalf("%s deleted without its UID: %s", p, call.Body)
			}
		}
	}
	if _, ok := nsLabels(f)[kueue.ProjectLabel]; ok {
		t.Fatal("label kept")
	}
	if rec := kueueRecord(t, u); !rec.Empty() || rec.Project != "" {
		t.Fatalf("record %+v", rec)
	}
	var steps []string
	for _, e := range events {
		if e.Status == "success" {
			steps = append(steps, e.Step)
		}
	}
	if strings.Join(steps, " ") != "samples queues project opt-in" {
		t.Fatalf("steps %v", steps)
	}
}

// Teardown respects ownership: recreated queues with a new UID stay, ClusterQueues
// referenced elsewhere stay, and remaining Workloads can force keeping the label.
func TestKueueTeardownOwnership(t *testing.T) {
	t.Run("recreated LocalQueue", func(t *testing.T) {
		f, u := putKueueCluster(t)
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"}); !res.Success {
			t.Fatalf("setup: %+v", res)
		}
		f.putJSON(kqLQ, `{"metadata":{"name":"nightly-kueue","namespace":"proj","uid":"other","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}},"spec":{"clusterQueue":"nightly-kueue"}}`)
		res, _ := runKueueOp(t, u, types.KueueRequest{Action: "teardown"})
		if res.Success || res.ErrorCode != "not_managed" || f.has(kqLQ) == false || !f.has(kqCQ) {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("ClusterQueue in use", func(t *testing.T) {
		f, u := putKueueCluster(t)
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"}); !res.Success {
			t.Fatalf("setup: %+v", res)
		}
		f.putJSON(kqAPI+"/namespaces/team/localqueues/q", `{"metadata":{"name":"q","namespace":"team"},"spec":{"clusterQueue":"nightly-kueue"}}`)
		res, _ := runKueueOp(t, u, types.KueueRequest{Action: "teardown"})
		if res.Success || res.ErrorCode != "in_use" || !strings.Contains(res.Message, "LocalQueue team/q") || !f.has(kqCQ) || f.has(kqLQ) {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("Workload left", func(t *testing.T) {
		f, u := putKueueCluster(t)
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"}); !res.Success {
			t.Fatalf("setup: %+v", res)
		}
		f.putJSON(kqWorkloads+"/w1", `{"metadata":{"name":"w1","namespace":"proj"},"spec":{"queueName":"team-queue"}}`)
		res, _ := runKueueOp(t, u, types.KueueRequest{Action: "teardown"})
		if !res.Success || !strings.Contains(res.Message, "Kept the label") || nsLabels(f)[kueue.ProjectLabel] != "true" || !kueueRecord(t, u).LabelAdded {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("a Workload on the LocalQueue", func(t *testing.T) {
		f, u := putKueueCluster(t)
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"}); !res.Success {
			t.Fatalf("setup: %+v", res)
		}
		f.putJSON(kqWorkloads+"/w1", `{"metadata":{"name":"w1","namespace":"proj"},"spec":{"queueName":"nightly-kueue"}}`)
		res, _ := runKueueOp(t, u, types.KueueRequest{Action: "teardown"})
		if res.Success || res.ErrorCode != "in_use" || !f.has(kqLQ) {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("unreadable users", func(t *testing.T) {
		f, u := putKueueCluster(t)
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"}); !res.Success {
			t.Fatalf("setup: %+v", res)
		}
		f.fail["GET "+kqAPI+"/localqueues"] = 500
		res, _ := runKueueOp(t, u, types.KueueRequest{Action: "teardown"})
		if res.Success || !f.has(kqCQ) || !strings.Contains(res.Message, "stays") {
			t.Fatalf("got %+v", res)
		}
	})
}

// Opt-in flips disableKueue to false and provisions the Queue hardware profile,
// with both captured in the record; opt-out reverts disableKueue to unset and
// removes the profile unless an outsider changed either object.
func TestKueueOptIn(t *testing.T) {
	f, u := putKueueCluster(t)
	if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "optin"}); res.Success || !strings.Contains(res.Message, "Set up the queues first") {
		t.Fatalf("opt-in before setup: %+v", res)
	}
	if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	res, _ := runKueueOp(t, u, types.KueueRequest{Action: "optin"})
	if !res.Success {
		t.Fatalf("opt-in: %+v", res)
	}
	cfg := ensureMap(ensureMap(f.get(kqConfig), "spec"), "dashboardConfig")
	if cfg["disableKueue"] != false || !f.has(kqProfile) {
		t.Fatalf("config %v, profile %v", cfg, f.has(kqProfile))
	}
	rec := kueueRecord(t, u)
	if rec.OptIn == nil || !rec.OptIn.DisableKueueSet || rec.OptIn.Previous != "unset" || rec.OptIn.ProfileUID == "" {
		t.Fatalf("record %+v", rec.OptIn)
	}
	st, err := GetKueueStatus(u, u, "proj")
	if err != nil || !st.OptIn.On || !st.OptIn.SetByTool || !st.OptIn.Profile.ManagedByTool {
		t.Fatalf("status %+v %v", st.OptIn, err)
	}
	res, _ = runKueueOp(t, u, types.KueueRequest{Action: "optout"})
	if !res.Success {
		t.Fatalf("opt-out: %+v", res)
	}
	cfg = ensureMap(ensureMap(f.get(kqConfig), "spec"), "dashboardConfig")
	if _, set := cfg["disableKueue"]; set || f.has(kqProfile) || kueueRecord(t, u).OptIn != nil {
		t.Fatalf("config %v, profile %v", cfg, f.has(kqProfile))
	}

	// After external edits, teardown keeps the profile and disableKueue as left.
	if res, _ = runKueueOp(t, u, types.KueueRequest{Action: "optin"}); !res.Success {
		t.Fatalf("opt-in: %+v", res)
	}
	p := f.get(kqProfile)
	f.bumpRV(ensureMap(p, "metadata"))
	f.put(kqProfile, p)
	other := userClientOf(u, fakeUserToken)
	if _, _, err := other.Do(http.MethodPatch, kqConfig, "application/merge-patch+json", []byte(`{"spec":{"dashboardConfig":{"disableKueue":true}}}`), map[string][]string{"fieldManager": {"someone"}}); err != nil {
		t.Fatal(err)
	}
	res, _ = runKueueOp(t, u, types.KueueRequest{Action: "teardown"})
	if !res.Success || !strings.Contains(res.Message, "Kept") || !f.has(kqProfile) || ensureMap(ensureMap(f.get(kqConfig), "spec"), "dashboardConfig")["disableKueue"] != true {
		t.Fatalf("got %+v", res)
	}
}

// GetKueueStatus loads each object as the user and infers sample states from
// matching Workload conditions.
func TestKueueStatus(t *testing.T) {
	f, u := putKueueCluster(t)
	st, err := GetKueueStatus(u, u, "proj")
	if err != nil || st.Refusal != "" || !st.Instance.Ready || !st.Instance.V1beta2 || len(st.Objects) != 3 || st.Objects[0].Present || len(st.Samples) != 2 {
		t.Fatalf("before setup: %+v %v", st, err)
	}
	if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj", RayCluster: true}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	uid := kueueRecord(t, u).RayClusterUID
	f.putJSON(kqWorkloads+"/raycluster-x", `{"metadata":{"name":"raycluster-x","namespace":"proj","ownerReferences":[{"uid":"`+uid+`"}]},
		"spec":{"queueName":"nightly-kueue"},"status":{"conditions":[{"type":"QuotaReserved","status":"False","reason":"Inadmissible","message":"insufficient quota"}]}}`)
	st, err = GetKueueStatus(u, u, "other")
	if err != nil || st.Project != "proj" || st.SetUpIn != "proj" || st.Label == nil || !st.Label.AddedByTool {
		t.Fatalf("status %+v %v", st, err)
	}
	for _, o := range st.Objects {
		if !o.Present || !o.ManagedByTool {
			t.Fatalf("object %+v", o)
		}
	}
	if s := st.Samples[0]; !s.Present || !s.ManagedByTool || s.State != kueue.StateInadmissible || s.Message != "insufficient quota" || st.Samples[1].Present {
		t.Fatalf("samples %+v", st.Samples)
	}
	// The quota is the live ClusterQueue's; what this version made is not outdated.
	if st.Quota != "6 CPUs and 19Gi of memory" || st.Objects[1].Outdated != "" || st.Samples[0].Outdated != "" {
		t.Fatalf("quota %q, queue %+v, sample %+v", st.Quota, st.Objects[1], st.Samples[0])
	}
	// The same amounts in other forms (custom resources keep what was
	// written: numbers, 1000m, bytes) are not outdated; an earlier
	// version's queue (11Gi) and sample (2Gi head, requests = limits) are.
	cqPath, rcPath := kqAPI+"/clusterqueues/"+kueue.ClusterQueueName, kqRayCs+"/"+kueue.RayClusterName
	setSizes := func(cpuQuota any, memoryQuota string, headRes map[string]any) {
		t.Helper()
		cq := f.get(cqPath)
		res := cq["spec"].(map[string]any)["resourceGroups"].([]any)[0].(map[string]any)["flavors"].([]any)[0].(map[string]any)["resources"].([]any)
		res[0].(map[string]any)["nominalQuota"], res[1].(map[string]any)["nominalQuota"] = cpuQuota, memoryQuota
		rc := f.get(rcPath)
		rc["spec"].(map[string]any)["headGroupSpec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["resources"] = headRes
		for path, obj := range map[string]map[string]any{cqPath: cq, rcPath: rc} {
			raw, _ := json.Marshal(obj)
			f.putJSON(path, string(raw))
		}
	}
	setSizes(6, "19456Mi", map[string]any{"requests": map[string]any{"cpu": 1, "memory": "5368709120"}, "limits": map[string]any{"cpu": "2000m", "memory": "8Gi"}})
	st, err = GetKueueStatus(u, u, "proj")
	if err != nil || st.Quota != "6 CPUs and 19456Mi of memory" || st.Objects[1].Outdated != "" || st.Samples[0].Outdated != "" || !st.Samples[0].Present {
		t.Fatalf("equivalent forms: %v quota %q, queue %+v, sample %+v", err, st.Quota, st.Objects[1], st.Samples[0])
	}
	// A value that cannot be read is not called outdated.
	setSizes("lots", "19Gi", map[string]any{"requests": map[string]any{"cpu": "one", "memory": "5Gi"}, "limits": map[string]any{"cpu": "2", "memory": "8Gi"}})
	if st, _ = GetKueueStatus(u, u, "proj"); st.Objects[1].Outdated != "" || st.Samples[0].Outdated != "" {
		t.Fatalf("unreadable: queue %+v, sample %+v", st.Objects[1], st.Samples[0])
	}
	setSizes("6", "11Gi", map[string]any{"requests": map[string]any{"cpu": "1", "memory": "2Gi"}, "limits": map[string]any{"cpu": "1", "memory": "2Gi"}})
	st, _ = GetKueueStatus(u, u, "proj")
	if st.Quota != "6 CPUs and 11Gi of memory" || st.Objects[1].Outdated != "its quota is 6 CPUs and 11Gi of memory; this version sets 6 CPUs and 19Gi of memory" ||
		st.Samples[0].Outdated != "its head and worker have other CPU and memory than this version sets (head 1 CPU and 5Gi requested, up to 2 CPUs and 8Gi; worker 1 CPU and 3Gi requested, up to 1 CPU and 6Gi)" {
		t.Fatalf("outdated: quota %q, queue %+v, sample %+v", st.Quota, st.Objects[1], st.Samples[0])
	}
	// One Ray state for the RayJob: its job's, in words.
	f.putJSON(kqRayJobs+"/"+kueue.RayJobName, `{"metadata":{"name":"nightly-rayjob","namespace":"proj"},"status":{"jobDeploymentStatus":"Running","jobStatus":"RUNNING"}}`)
	if st, _ = GetKueueStatus(u, u, "proj"); st.Samples[1].RayState != "Running" {
		t.Fatalf("RayJob state %q", st.Samples[1].RayState)
	}
	// Unsupported API versions surface as refusal codes on status as well.
	f.groups[kqGroup] = `{"versions":[{"version":"v1beta1"}]}`
	if st, _ = GetKueueStatus(u, u, "proj"); st.RefusalCode != "unsupported" {
		t.Fatalf("v1beta1: %+v", st)
	}
}

// delete-sample removes a single Ray object identified by the stored UID.
func TestKueueDeleteSample(t *testing.T) {
	f, u := putKueueCluster(t)
	if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj", RayCluster: true, RayJob: true}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "delete-sample", Sample: "rayjob"}); !res.Success {
		t.Fatalf("delete: %+v", res)
	}
	rec := kueueRecord(t, u)
	if f.has(kqRayJobs+"/"+kueue.RayJobName) || !f.has(kqRayCs+"/"+kueue.RayClusterName) || rec.RayJobUID != "" || rec.RayClusterUID == "" {
		t.Fatalf("record %+v", rec)
	}
}

// The kueue prerequisite path installs cert-manager and the Red Hat Kueue
// build, patches DataScienceCluster kueue to Unmanaged (never Managed), and
// blocks until RHOAI's Kueue CR reports Available.
func TestKueuePrerequisites(t *testing.T) {
	pc := newPrereqCluster(t)
	f := pc.f
	f.putJSON(tDSC, `{"metadata":{"name":"default-dsc"},"spec":{"components":{"kueue":{"managementState":"Removed"}}}}`)
	pc.sim.packages["kueue-operator"] = fakePackage{source: "redhat-operators", defaultChannel: "stable-v1.5", order: []string{"stable-v1.4", "stable-v1.5"},
		installModes: []string{"AllNamespaces"},
		channels:     map[string]fakeChannel{"stable-v1.4": {head: "1.4.4", entries: []string{"1.4.4"}}, "stable-v1.5": {head: "1.5.1", entries: []string{"1.5.1"}}}}
	pc.putPackage("kueue-operator")
	installed := false
	prev := pc.sim.onInstalled
	pc.sim.onInstalled = func(pkg, ns, from, to string) {
		prev(pkg, ns, from, to)
		if pkg == "kueue-operator" {
			installed = true
		}
	}
	prevServe := f.beforeServe
	instance := false
	f.beforeServe = func(method, path string) {
		prevServe(method, path)
		if installed && !instance {
			instance = true
			f.mu.Lock()
			f.groups = map[string]string{kqGroup: kqV1beta2}
			f.mu.Unlock()
			f.putJSON(kqInstance, `{"metadata":{"name":"cluster"},"status":{"conditions":[{"type":"Available","status":"True"}]}}`)
		}
	}
	prevPatch := f.onMergePatch
	f.onMergePatch = func(r *http.Request, k fakeKey, patch, next map[string]any) {
		prevPatch(r, k, patch, next)
		if k.plural == "datascienceclusters" && r.URL.Query().Get("dryRun") == "" {
			if m, _ := ensureMap(ensureMap(next, "spec"), "components")["kueue"].(map[string]any); m["managementState"] == "Unmanaged" {
				status := ensureMap(next, "status")
				conds, _ := status["conditions"].([]any)
				status["conditions"] = setFakeCondition(conds, "KueueReady", "True", "")
			}
		}
	}
	st := prereqStatus(t, pc.c, "kueue")
	plan := strings.Join(st.SetupPlan, "\n")
	for _, want := range []string{"set kueue to Unmanaged (it is Removed", "Install Red Hat build of Kueue 1.5.1", "channel stable-v1.5", "Subscription openshift-kueue-operator/kueue-operator", "Install cert-manager"} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan lacks %q:\n%s", want, plan)
		}
	}
	var dscPart *types.MaaSPart
	for i, pt := range st.Parts {
		if pt.ID == "dsc-kueue" {
			dscPart = &st.Parts[i]
		}
	}
	if dscPart == nil || dscPart.Title != "Kueue component" || !strings.Contains(dscPart.Message, "uses the Kueue operator you install instead of its own") {
		t.Fatalf("kueue part %+v", dscPart)
	}
	u := userClientOf(pc.c, fakeUserToken)
	res, _ := runPrereqs(t, u, "kueue")
	if !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	if componentState(f, "kueue") != "Unmanaged" {
		t.Fatalf("kueue %s", componentState(f, "kueue"))
	}
	subPost, dscPatch := -1, -1
	for i, m := range f.mutations() {
		if strings.HasPrefix(m, "POST ") && strings.Contains(m, "openshift-kueue-operator/subscriptions") && subPost < 0 {
			subPost = i
		}
		if m == "PATCH "+tDSC && dscPatch < 0 {
			dscPatch = i
		}
		if strings.Contains(m, "Managed") {
			t.Fatal(m)
		}
	}
	if subPost < 0 || dscPatch < subPost {
		t.Fatalf("operator not installed before the DataScienceCluster changed: %v", f.mutations())
	}
	for _, call := range f.callsTo("PATCH", tDSC) {
		if strings.Contains(call.Body, `"Managed"`) {
			t.Fatalf("Managed written: %s", call.Body)
		}
	}
}

// If Kueue APIs are already served by a non-OLM controller, prerequisite setup
// is blocked to avoid duplicate controllers and unmatched RHOAI state.
func TestKueuePrerequisitesRefuseKueueOutsideOLM(t *testing.T) {
	const crdList = "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/clusterqueues.kueue.x-k8s.io"
	for _, tc := range []struct {
		name, crd, deployment string
		refused               bool
	}{
		// Upstream manifests: no OLM labels on the CRD; the controller renamed.
		{"renamed controller", `{"metadata":{"name":"clusterqueues.kueue.x-k8s.io"}}`, "my-kueue", true},
		{"usual controller", `{"metadata":{"name":"clusterqueues.kueue.x-k8s.io"}}`, "kueue-controller-manager", true},
		// No CRD named as Kueue's, but the API is served: cannot confirm, so refused.
		{"no CRD", "", "", true},
		// CRDs OLM installed for kueue-operator and left after an uninstall: installing adopts them.
		{"OLM leftovers", `{"metadata":{"name":"clusterqueues.kueue.x-k8s.io","labels":{"olm.managed":"true","operators.coreos.com/kueue-operator.openshift-kueue-operator":""}}}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pc := newPrereqCluster(t)
			pc.f.putJSON(tDSC, `{"metadata":{"name":"default-dsc"},"spec":{"components":{"kueue":{"managementState":"Removed"}}}}`)
			pc.sim.packages["kueue-operator"] = fakePackage{source: "redhat-operators", defaultChannel: "stable-v1.5", order: []string{"stable-v1.5"},
				installModes: []string{"AllNamespaces"}, channels: map[string]fakeChannel{"stable-v1.5": {head: "1.5.1", entries: []string{"1.5.1"}}}}
			pc.putPackage("kueue-operator")
			if pc.f.groups == nil {
				pc.f.groups = map[string]string{}
			}
			pc.f.groups[kqGroup] = kqV1beta2
			if tc.crd != "" {
				pc.f.putJSON(crdList, tc.crd)
			}
			if tc.deployment != "" {
				pc.f.putJSON("/apis/apps/v1/namespaces/kueue-system/deployments/"+tc.deployment, `{"metadata":{"name":"`+tc.deployment+`","namespace":"kueue-system"}}`)
			}
			st := prereqStatus(t, pc.c, "kueue")
			if got := strings.Contains(st.SetupBlockedReason, "outside OperatorHub"); got != tc.refused {
				t.Fatalf("refused %v, want %v: %+v", got, tc.refused, st)
			}
		})
	}
}

// Restoring the kueue component reverts DataScienceCluster only after queues are
// gone (Clean up components); until then management stays Unmanaged.
func TestKueueComponentRestore(t *testing.T) {
	pc := newPrereqCluster(t)
	f := pc.f
	f.putJSON(tDSC, `{"metadata":{"name":"default-dsc","resourceVersion":"1"},"spec":{"components":{"kueue":{"managementState":"Removed"}}}}`)
	u := userClientOf(pc.c, fakeUserToken)
	kueueTurnedOnByTool(t, u)
	if f.groups == nil {
		f.groups = map[string]string{}
	}
	f.groups[kqGroup] = kqV1beta2
	f.putJSON(kqCQ, `{"metadata":{"name":"nightly-kueue"}}`)
	res, err := features.RestoreComponents(u, []string{"kueue"})
	if err != nil || !res.Success || !strings.Contains(res.Message, "in use: ClusterQueue nightly-kueue") || componentState(f, "kueue") != "Unmanaged" {
		t.Fatalf("in use: %+v %v", res, err)
	}
	f.deleteObject(kqCQ)
	res, err = features.RestoreComponents(u, []string{"kueue"})
	if err != nil || !res.Success || componentState(f, "kueue") != "Removed" {
		t.Fatalf("restore: %+v %v", res, err)
	}
}

// Models setup picks hardware profiles using the dashboard Kueue rule: Queue-type
// profiles tied to a LocalQueue only inside managed projects, none when
// disableKueue is true, and never Queue profiles outside Kueue projects.
func TestModelsHardwareProfileKueueRule(t *testing.T) {
	queue := func(name, lq string) string {
		p := hwProfile(name, "8", "32Gi", "")
		return p[:len(p)-2] + `,"scheduling":{"type":"Queue","kueue":{"localQueueName":"` + lq + `"}}}}`
	}
	for _, tc := range []struct {
		name, label, disable, want string
	}{
		{"not a Kueue project", "", "false", "a-node"},
		{"Kueue project", `,"kueue.openshift.io/managed":"true"`, "false", "b-queue"},
		{"Kueue off in the dashboard", `,"kueue.openshift.io/managed":"true"`, "true", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mc := newModelsCluster(t)
			mc.f.putJSON("/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/default-dsc", `{
				"spec":{"components":{"kserve":{"managementState":"Managed"},"kueue":{"managementState":"Unmanaged"}}},
				"status":{"conditions":[{"type":"KserveLLMInferenceServiceDependencies","status":"True"}]}}`)
			mc.f.putJSON("/api/v1/namespaces/proj", `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"`+tc.label+`}}}`)
			mc.f.putJSON(kqConfig, `{"spec":{"dashboardConfig":{"disableKueue":`+tc.disable+`}}}`)
			mc.f.putJSON(kqLQ, `{"metadata":{"name":"nightly-kueue","namespace":"proj"}}`)
			putProfiles(mc, hwProfile("a-node", "8", "32Gi", ""), queue("b-queue", "nightly-kueue"), queue("0-other-queue", "missing"))
			u := userClientOf(mc.c, fakeUserToken)
			if res, _ := runModels(t, u, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
				t.Fatalf("setup failed: %+v", res)
			}
			ann := mc.f.get(testModelPath)["metadata"].(map[string]any)["annotations"].(map[string]any)
			if got, _ := ann[serving.HardwareProfileNameAnnotation].(string); got != tc.want {
				t.Fatalf("hardware profile %q, want %q", got, tc.want)
			}
		})
	}
}

// kueueTurnedOnByTool leaves the DataScienceCluster's kueue as the
// Distributed workloads prerequisites (and a recipe's kueue-prereq step)
// leave it, without the operator installs: Unmanaged, written by the
// component's own field manager, and named in the prerequisites record.
// The fake's merge patches must record field ownership.
func kueueTurnedOnByTool(t *testing.T, u *Client) {
	t.Helper()
	kueueTurnedOnByToolAs(t, u, "")
}

// kueueTurnedOnByToolAs is kueueTurnedOnByTool with the record's
// enablement ID ("" a record from before IDs).
func kueueTurnedOnByToolAs(t *testing.T, u *Client, id string) {
	t.Helper()
	if _, _, err := u.Do(http.MethodPatch, tDSC, "application/merge-patch+json", []byte(`{"spec":{"components":{"kueue":{"managementState":"Unmanaged"}}}}`), map[string][]string{"fieldManager": {"rhoai-nightly-updater-dsc-kueue"}}); err != nil {
		t.Fatal(err)
	}
	if err := writeTestPrereqRecord(u, `{"components":{"kueue":{"previous":"Removed","feature":"kueue","id":"`+id+`"}}}`); err != nil {
		t.Fatal(err)
	}
}

// writeTestPrereqRecord seeds the prerequisites snapshot key the way setup persists it.
func writeTestPrereqRecord(c *Client, raw string) error {
	return record.WriteSnapshotKey(c, "prerequisites", "rhoai-nightly-updater-prerequisites-record", raw)
}

// Clean up deletes the Kueue setup after the workbenches and models that
// may use its queue, and before the components (kueue goes back last).
func TestKueueCleanupOrder(t *testing.T) {
	_, c := newResourceFake(t)
	parts := cleanupParts(c, types.CleanupRequest{Kueue: true, Workbenches: []string{"proj"}, Models: []string{"proj"}, RecipeProjects: []string{"proj"}, Components: []string{"kueue"}})
	var ids []string
	for _, p := range parts {
		ids = append(ids, p.id)
	}
	if got := strings.Join(ids, " "); got != "workbench:proj model:proj kueue recipe-project:proj components" {
		t.Fatalf("order %s", got)
	}
}

// H1: a discovery read that fails is not "Kueue is absent": setup and the
// component restore refuse (403, 500, a document that does not parse).
func TestKueueDiscoveryErrorsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(f *resourceFake)
	}{
		{"403", func(f *resourceFake) { f.fail["GET "+kqGroup] = 403 }},
		{"500", func(f *resourceFake) { f.fail["GET "+kqGroup] = 500 }},
		{"malformed", func(f *resourceFake) { f.groups[kqGroup] = "not json" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, u := putKueueCluster(t)
			tc.change(f)
			res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"})
			if res.Success || !strings.Contains(res.Message, "Cannot read Kueue's API versions") || len(f.mutations()) != 0 {
				t.Fatalf("setup: %+v", res)
			}
			if users, err := kueue.Users(u); err == nil {
				t.Fatalf("Users: %v, no error", users)
			}
		})
	}
}

// M1: KueueReady must read True: absent, Unknown and False refuse setup and
// are not ready.
func TestKueueReadyMustBeTrue(t *testing.T) {
	for _, cond := range []string{``, `,"conditions":[{"type":"KueueReady","status":"Unknown"}]`, `,"conditions":[{"type":"KueueReady","status":"False","message":"broken"}]`} {
		f, u := putKueueCluster(t)
		f.putJSON(wbDSCv2+"/default-dsc", `{"metadata":{"name":"default-dsc"},"spec":{"components":{"kueue":{"managementState":"Unmanaged"},"ray":{"managementState":"Managed"}}},
			"status":{"components":{"kueue":{"managementState":"Unmanaged"}}`+cond+`}}`)
		res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"})
		if res.Success || !strings.Contains(res.Message, "KueueReady") || len(f.mutations()) != 0 {
			t.Fatalf("%q: %+v", cond, res)
		}
		st, _ := GetKueueStatus(u, u, "proj")
		if st.Instance.Ready || st.Refusal == "" {
			t.Fatalf("%q: status %+v", cond, st.Instance)
		}
	}
	// v1beta2 not served: not ready either.
	f, u := putKueueCluster(t)
	f.groups[kqGroup] = `{"versions":[{"version":"v1beta1"}]}`
	if st, _ := GetKueueStatus(u, u, "proj"); st.Instance.Ready {
		t.Fatalf("ready without v1beta2: %+v", st.Instance)
	}
}

// M2: a LocalQueue named default refuses every setup, the project labelled
// already and a sample added later too.
func TestKueueDefaultQueueRefusedOnEverySetup(t *testing.T) {
	f, u := putKueueCluster(t)
	f.putJSON(wbProjectNS, `{"metadata":{"name":"proj","uid":"ns-uid","labels":{"opendatahub.io/dashboard":"true","kueue.openshift.io/managed":"true"}}}`)
	f.putJSON(kqLQs+"/default", `{"metadata":{"name":"default","namespace":"proj"},"spec":{"clusterQueue":"x"}}`)
	if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"}); res.Success || !strings.Contains(res.Message, "LocalQueue named default") {
		t.Fatalf("labelled project: %+v", res)
	}
	f2, u2 := putKueueCluster(t)
	if res, _ := runKueueOp(t, u2, types.KueueRequest{Action: "setup", Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	f2.putJSON(kqLQs+"/default", `{"metadata":{"name":"default","namespace":"proj"},"spec":{"clusterQueue":"x"}}`)
	before := len(f2.mutations())
	if res, _ := runKueueOp(t, u2, types.KueueRequest{Action: "setup", Project: "proj", RayJob: true}); res.Success || !strings.Contains(res.Message, "LocalQueue named default") || len(f2.mutations()) != before {
		t.Fatalf("sample added: %+v", res)
	}
}

// M3: the opt-in checks the setup live before it changes the dashboard: a
// queue made again, the label gone, the project made again or Kueue not
// ready refuse with no change to the dashboard's settings.
func TestKueueOptInRevalidates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(f *resourceFake)
	}{
		{"LocalQueue replaced", func(f *resourceFake) {
			f.putJSON(kqLQ, `{"metadata":{"name":"nightly-kueue","namespace":"proj","uid":"other","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}},"spec":{"clusterQueue":"nightly-kueue"}}`)
		}},
		{"ClusterQueue gone", func(f *resourceFake) { f.deleteObject(kqCQ) }},
		{"label gone", func(f *resourceFake) {
			f.putJSON(wbProjectNS, `{"metadata":{"name":"proj","uid":"ns-uid","labels":{"opendatahub.io/dashboard":"true"}}}`)
		}},
		{"project made again", func(f *resourceFake) {
			f.putJSON(wbProjectNS, `{"metadata":{"name":"proj","uid":"new-uid","labels":{"opendatahub.io/dashboard":"true","kueue.openshift.io/managed":"true"}}}`)
		}},
		{"Kueue not ready", func(f *resourceFake) {
			f.putJSON(kqInstance, `{"status":{"conditions":[{"type":"Available","status":"False"}]}}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, u := putKueueCluster(t)
			if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"}); !res.Success {
				t.Fatalf("setup: %+v", res)
			}
			tc.change(f)
			res, _ := runKueueOp(t, u, types.KueueRequest{Action: "optin"})
			if res.Success || len(f.callsTo("PATCH", kqConfig)) != 0 || len(f.callsTo("POST", "/apis/infrastructure.opendatahub.io/v1/namespaces/redhat-ods-applications/hardwareprofiles")) != 0 {
				t.Fatalf("got %+v", res)
			}
		})
	}
}

// M4: the label: a value other than true is never overwritten (refused); a
// label another actor adds between the read and the write is not claimed;
// Clean up removes only the label the tool added while it still reads true.
func TestKueueLabelCompareAndSet(t *testing.T) {
	t.Run("other value", func(t *testing.T) {
		f, u := putKueueCluster(t)
		f.putJSON(wbProjectNS, `{"metadata":{"name":"proj","uid":"ns-uid","labels":{"opendatahub.io/dashboard":"true","kueue.openshift.io/managed":"false"}}}`)
		res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"})
		if res.Success || !strings.Contains(res.Message, "kueue.openshift.io/managed=false") || len(f.mutations()) != 0 {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("added meanwhile by another actor", func(t *testing.T) {
		f, u := putKueueCluster(t)
		done := false
		f.beforeRequest = func(r *http.Request) {
			if r.Method == http.MethodPatch && r.URL.Path == wbProjectNS && r.URL.Query().Get("dryRun") == "" && !done {
				done = true
				f.putJSON(wbProjectNS, `{"metadata":{"name":"proj","uid":"ns-uid","resourceVersion":"99","labels":{"opendatahub.io/dashboard":"true","kueue.openshift.io/managed":"true"}}}`)
			}
		}
		res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"})
		if !res.Success || kueueRecord(t, u).LabelAdded {
			t.Fatalf("claimed another actor's label: %+v %+v", res, kueueRecord(t, u))
		}
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "teardown"}); !res.Success || nsLabels(f)[kueue.ProjectLabel] != "true" {
			t.Fatalf("teardown removed it: %+v", res)
		}
	})
	t.Run("changed after setup", func(t *testing.T) {
		f, u := putKueueCluster(t)
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"}); !res.Success {
			t.Fatalf("setup: %+v", res)
		}
		ns := f.get(wbProjectNS)
		ensureMap(ensureMap(ns, "metadata"), "labels")[kueue.ProjectLabel] = "custom"
		f.put(wbProjectNS, ns)
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "teardown"}); !res.Success || nsLabels(f)[kueue.ProjectLabel] != "custom" {
			t.Fatalf("got %+v", res)
		}
	})
}

// M5: objects being deleted still count until they are gone: a terminating
// LocalQueue keeps the ClusterQueue and the DataScienceCluster's kueue, a
// terminating Workload keeps the label.
func TestKueueTerminatingDependencies(t *testing.T) {
	t.Run("ClusterQueue", func(t *testing.T) {
		f, u := putKueueCluster(t)
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"}); !res.Success {
			t.Fatalf("setup: %+v", res)
		}
		f.putJSON(kqAPI+"/namespaces/team/localqueues/q", `{"metadata":{"name":"q","namespace":"team","deletionTimestamp":"2026-11-24T10:00:00Z","finalizers":["x"]},"spec":{"clusterQueue":"nightly-kueue"}}`)
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "teardown"}); res.Success || res.ErrorCode != "in_use" || !f.has(kqCQ) {
			t.Fatalf("got %+v", res)
		}
		users, err := kueue.Users(u)
		if err != nil || !strings.Contains(strings.Join(users, ","), "LocalQueue team/q") {
			t.Fatalf("users %v %v", users, err)
		}
	})
	t.Run("label", func(t *testing.T) {
		f, u := putKueueCluster(t)
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"}); !res.Success {
			t.Fatalf("setup: %+v", res)
		}
		f.putJSON(kqWorkloads+"/w1", `{"metadata":{"name":"w1","namespace":"proj","deletionTimestamp":"2026-11-24T10:00:00Z","finalizers":["kueue.x-k8s.io/resource-in-use"]},"spec":{"queueName":"team-queue"}}`)
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "teardown"}); !res.Success || nsLabels(f)[kueue.ProjectLabel] != "true" {
			t.Fatalf("got %+v", res)
		}
	})
}
