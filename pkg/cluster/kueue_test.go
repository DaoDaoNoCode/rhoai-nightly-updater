package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
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

// putKueueCluster assembles a fake cluster ready for Kueue exercises: DSC
// kueue is Unmanaged with KueueReady True, ray Managed, kueue.x-k8s.io/v1beta2
// is served, the cluster Kueue CR is Available, proj is a data science project,
// and dashboard config has no disableKueue. The caller receives a user-scoped client.
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

// Manifests follow the documented self-managed layout: LocalQueue cannot be
// "default" (Kueue would otherwise admit every new pod workload), ClusterQueue
// limits scope to the project, turns off preemption, and sets nominal quotas for
// both Ray samples and one opt-in-profile workload; the Ray samples reuse one
// cluster spec and both carry the queue name label.
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
			"resourceGroups":[{"coveredResources":["cpu","memory","ephemeral-storage"],"flavors":[{"name":"nightly-kueue-cpu","resources":[{"name":"cpu","nominalQuota":"14"},{"name":"memory","nominalQuota":"44Gi"},{"name":"ephemeral-storage","nominalQuota":"12Gi"}]}]}]}}`)
	assertJSONEqual(t, kueue.FlavorManifest(), `{"apiVersion":"kueue.x-k8s.io/v1beta2","kind":"ResourceFlavor","metadata":{"name":"nightly-kueue-cpu","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}},"spec":{}}`)
	// Sample requests plus the profile default must remain within the quota ceiling.
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
	// Opt-in-profile workbench uses 1 CPU and 2Gi, with kube-rbac-proxy at
	// 100m CPU and 64Mi; the largest catalog model (Qwen3: 4 CPUs, 12Gi
	// requested, 6.2G of ephemeral storage) on the same profile, twice: its
	// Deployment's rolling update adds a second pod, which Kueue admits on
	// its own.
	wbCPU, wbMi := 1.0+0.1, 2048.0+64
	var modelCPU, modelMi, modelEph float64
	for _, e := range serving.ModelCatalog {
		cpu, _ := serving.ParseQuantity(e.CPU)
		mem, _ := serving.ParseQuantity(e.MemoryRequest)
		eph, ok := serving.ParseQuantity(e.EphemeralStorage)
		if !ok {
			t.Fatalf("%s: ephemeral storage %q", e.Name, e.EphemeralStorage)
		}
		modelCPU, modelMi, modelEph = max(modelCPU, cpu), max(modelMi, mem/(1<<20)), max(modelEph, eph)
	}
	if modelCPU != 4 || modelMi != 12*1024 || modelEph != 6.2e9 {
		t.Fatalf("largest model %v CPUs, %v Mi, %v bytes", modelCPU, modelMi, modelEph)
	}
	if c1+c2+wbCPU+2*modelCPU > 14 || c1+c2+wbCPU+2*modelCPU <= 13 || m1+m2+wbMi+2*modelMi > 44*1024 || m1+m2+wbMi+2*modelMi <= 43*1024 || 2*modelEph > 12*(1<<30) || 2*modelEph <= 11*(1<<30) || c1 != 2 || c2 != 2.5 || m1 != 8*1024 || m2 != 9*1024 {
		t.Fatalf("quota 14 CPUs / 44Gi / 12Gi does not hold the samples, the workbench and the model twice: %v+%v+%v+2*%v CPU, %v+%v+%v+2*%v Mi", c1, c2, wbCPU, modelCPU, m1, m2, wbMi, modelMi)
	}
	// The Queue profile's ranges hold every catalog model, as the
	// dashboard deploys one in a Kueue project only with such a profile.
	var prof serving.HardwareProfileObject
	raw, _ := json.Marshal(kueue.ProfileManifest("apps", "now"))
	if err := json.Unmarshal(raw, &prof); err != nil {
		t.Fatal(err)
	}
	for _, e := range serving.ModelCatalog {
		if why := prof.Fits(e); why != "" {
			t.Errorf("the Queue profile does not hold %s: %s", e.Name, why)
		}
	}
	// Head and worker match CodeFlare SDK defaults; requests stay under limits
	// (a 2Gi head OOMKilled in live runs).
	spec := kueue.RayClusterSpec(kueue.DefaultRayImage)
	head := spec["headGroupSpec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["resources"]
	worker := spec["workerGroupSpecs"].([]any)[0].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["resources"]
	assertJSONEqual(t, head, `{"requests":{"cpu":"1","memory":"5Gi"},"limits":{"cpu":"2","memory":"8Gi"}}`)
	assertJSONEqual(t, worker, `{"requests":{"cpu":"1","memory":"3Gi"},"limits":{"cpu":"1","memory":"6Gi"}}`)
	if cpu, memory, eph := kueue.Quota(); cpu != "14" || memory != "44Gi" || eph != "12Gi" {
		t.Fatalf("Quota() = %s, %s, %s; want 14, 44Gi, 12Gi", cpu, memory, eph)
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

// WorkloadState uses the same condition-to-state ordering as the dashboard.
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

// Setup server-side dry-runs every create, then provisions queues, patches the
// project label, and creates samples in that order, all as the user, storing UIDs
// and whether the label was applied.
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
	// A second setup must not trigger any more creates.
	before := len(f.mutations())
	if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj", RayCluster: true}); !res.Success || len(f.mutations()) != before {
		t.Fatalf("second setup: %+v %v", res, f.mutations()[before:])
	}
	// GetKueueStatus with the ServiceAccount client must error.
	if _, err := GetKueueStatus(u, u.Bookkeeping(), "proj"); !errors.Is(err, authz.ErrNotUserClient) {
		t.Fatalf("status with the ServiceAccount: %v", err)
	}
}

// Setup bails out without mutations when prerequisites would obscure the dashboard
// flow or conflict with queue state already present on the project.
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
		{"DSC autoCreateQueues", "kueue.autoCreateQueues: true", "unsupported", func(f *resourceFake) {
			f.putJSON(wbDSCv2+"/default-dsc", `{"metadata":{"name":"default-dsc"},"spec":{"components":{"kueue":{"managementState":"Unmanaged","autoCreateQueues":true},"ray":{"managementState":"Managed"}}},
				"status":{"conditions":[{"type":"KueueReady","status":"True"}]}}`)
		}},
		{"DSC autoCreateQueues named like the tool's LocalQueue", "LocalQueue named nightly-kueue", "unsupported", func(f *resourceFake) {
			f.putJSON(wbDSCv2+"/default-dsc", `{"metadata":{"name":"default-dsc"},"spec":{"components":{"kueue":{"managementState":"Unmanaged","autoCreateQueues":true,"defaultLocalQueueName":"nightly-kueue"},"ray":{"managementState":"Managed"}}},
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

// When the project already has the managed label, setup does not record it and
// teardown must leave it in place.
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

// Teardown deletes samples, LocalQueue, ClusterQueue, and ResourceFlavor in that
// sequence by stored UID, removes only the label this tool added, and wipes the
// bookkeeping record.
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

// Teardown honors ownership: queues recreated under a new UID remain, ClusterQueues
// still referenced elsewhere remain, and lingering Workloads can require keeping
// the label.
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

// Opt-in sets disableKueue false and creates the Queue hardware profile, both
// recorded; opt-out clears disableKueue and deletes the profile unless someone
// else modified either object.
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

	// Once edited externally, teardown retains the profile and disableKueue as found.
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

// GetKueueStatus fetches each object as the user and derives sample states from
// corresponding Workload conditions.
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
	// Quota reflects the live ClusterQueue; artifacts from this version are not outdated.
	if st.Quota != "14 CPUs, 44Gi of memory and 12Gi of ephemeral storage" || st.Objects[1].Outdated != "" || st.Samples[0].Outdated != "" {
		t.Fatalf("quota %q, queue %+v, sample %+v", st.Quota, st.Objects[1], st.Samples[0])
	}
	// Equivalent quantities in other representations (numbers, 1000m, bytes as
	// written on custom resources) are not outdated; queues and samples from an
	// older release (11Gi queue, 2Gi head with requests equal to limits) are.
	cqPath, rcPath := kqAPI+"/clusterqueues/"+kueue.ClusterQueueName, kqRayCs+"/"+kueue.RayClusterName
	setSizes := func(cpuQuota any, memoryQuota string, headRes map[string]any) {
		t.Helper()
		cq := f.get(cqPath)
		res := cq["spec"].(map[string]any)["resourceGroups"].([]any)[0].(map[string]any)["flavors"].([]any)[0].(map[string]any)["resources"].([]any)
		res[0].(map[string]any)["nominalQuota"], res[1].(map[string]any)["nominalQuota"] = cpuQuota, memoryQuota
		if cpuQuota == "6" {
			// An earlier version's ClusterQueue: CPU and memory only.
			cq["spec"].(map[string]any)["resourceGroups"].([]any)[0].(map[string]any)["flavors"].([]any)[0].(map[string]any)["resources"] = res[:2]
		}
		rc := f.get(rcPath)
		rc["spec"].(map[string]any)["headGroupSpec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["resources"] = headRes
		for path, obj := range map[string]map[string]any{cqPath: cq, rcPath: rc} {
			raw, _ := json.Marshal(obj)
			f.putJSON(path, string(raw))
		}
	}
	setSizes(14, "45056Mi", map[string]any{"requests": map[string]any{"cpu": 1, "memory": "5368709120"}, "limits": map[string]any{"cpu": "2000m", "memory": "8Gi"}})
	st, err = GetKueueStatus(u, u, "proj")
	if err != nil || st.Quota != "14 CPUs, 45056Mi of memory and 12Gi of ephemeral storage" || st.Objects[1].Outdated != "" || st.Samples[0].Outdated != "" || !st.Samples[0].Present {
		t.Fatalf("equivalent forms: %v quota %q, queue %+v, sample %+v", err, st.Quota, st.Objects[1], st.Samples[0])
	}
	// Unreadable values are not labeled outdated.
	setSizes("lots", "44Gi", map[string]any{"requests": map[string]any{"cpu": "one", "memory": "5Gi"}, "limits": map[string]any{"cpu": "2", "memory": "8Gi"}})
	if st, _ = GetKueueStatus(u, u, "proj"); st.Objects[1].Outdated != "" || st.Samples[0].Outdated != "" {
		t.Fatalf("unreadable: queue %+v, sample %+v", st.Objects[1], st.Samples[0])
	}
	// 10 CPUs, 32Gi and 6Gi, before the model's rolling update was
	// counted, is an earlier version's too.
	setSizes(10, "32Gi", map[string]any{"requests": map[string]any{"cpu": "1", "memory": "5Gi"}, "limits": map[string]any{"cpu": "2", "memory": "8Gi"}})
	cq := f.get(cqPath)
	cq["spec"].(map[string]any)["resourceGroups"].([]any)[0].(map[string]any)["flavors"].([]any)[0].(map[string]any)["resources"].([]any)[2].(map[string]any)["nominalQuota"] = "6Gi"
	raw, _ := json.Marshal(cq)
	f.putJSON(cqPath, string(raw))
	if st, _ = GetKueueStatus(u, u, "proj"); st.Objects[1].Outdated != "its quota is 10 CPUs, 32Gi of memory and 6Gi of ephemeral storage; this version sets 14 CPUs, 44Gi of memory and 12Gi of ephemeral storage" || st.Samples[0].Outdated != "" {
		t.Fatalf("10 CPUs: queue %+v, sample %+v", st.Objects[1], st.Samples[0])
	}
	// 6 CPUs and 19Gi or 20Gi from 5.0.0 pre-releases (no model, no
	// ephemeral storage) also signal an older version.
	setSizes("6", "19Gi", map[string]any{"requests": map[string]any{"cpu": "1", "memory": "5Gi"}, "limits": map[string]any{"cpu": "2", "memory": "8Gi"}})
	if st, _ = GetKueueStatus(u, u, "proj"); st.Objects[1].Outdated != "its quota is 6 CPUs, 19Gi of memory and no ephemeral storage; this version sets 14 CPUs, 44Gi of memory and 12Gi of ephemeral storage" {
		t.Fatalf("19Gi: queue %+v", st.Objects[1])
	}
	setSizes("6", "11Gi", map[string]any{"requests": map[string]any{"cpu": "1", "memory": "2Gi"}, "limits": map[string]any{"cpu": "1", "memory": "2Gi"}})
	st, _ = GetKueueStatus(u, u, "proj")
	if st.Quota != "6 CPUs, 11Gi of memory and no ephemeral storage" || st.Objects[1].Outdated != "its quota is 6 CPUs, 11Gi of memory and no ephemeral storage; this version sets 14 CPUs, 44Gi of memory and 12Gi of ephemeral storage" ||
		st.Samples[0].Outdated != "its head and worker have other CPU and memory than this version sets (head 1 CPU and 5Gi requested, up to 2 CPUs and 8Gi; worker 1 CPU and 3Gi requested, up to 1 CPU and 6Gi)" {
		t.Fatalf("outdated: quota %q, queue %+v, sample %+v", st.Quota, st.Objects[1], st.Samples[0])
	}
	// RayJob exposes a single Ray state: the job's, rendered as text.
	f.putJSON(kqRayJobs+"/"+kueue.RayJobName, `{"metadata":{"name":"nightly-rayjob","namespace":"proj"},"status":{"jobDeploymentStatus":"Running","jobStatus":"RUNNING"}}`)
	if st, _ = GetKueueStatus(u, u, "proj"); st.Samples[1].RayState != "Running" {
		t.Fatalf("RayJob state %q", st.Samples[1].RayState)
	}
	// Unsupported API versions also appear as refusal codes in status.
	f.groups[kqGroup] = `{"versions":[{"version":"v1beta1"}]}`
	if st, _ = GetKueueStatus(u, u, "proj"); st.RefusalCode != "unsupported" {
		t.Fatalf("v1beta1: %+v", st)
	}
}

// Status enumerates all ClusterQueues and LocalQueues on the cluster (so Clean up
// knows when it may revert the DataScienceCluster's kueue), regardless of owner;
// a failed list marks queues unknown (the tab then blocks irreversible actions).
func TestKueueStatusQueuesOnTheCluster(t *testing.T) {
	f, u := putKueueCluster(t)
	if st, err := GetKueueStatus(u, u, "proj"); err != nil || len(st.Queues) != 0 || st.QueuesUnknown {
		t.Fatalf("no queues: %+v %v", st, err)
	}
	f.putJSON(kqAPI+"/clusterqueues/team", `{"metadata":{"name":"team"}}`)
	f.putJSON(kqAPI+"/namespaces/team-a/localqueues/q", `{"metadata":{"name":"q","namespace":"team-a"}}`)
	st, err := GetKueueStatus(u, u, "proj")
	if err != nil || !reflect.DeepEqual(st.Queues, []string{"ClusterQueue team", "LocalQueue team-a/q"}) || st.QueuesUnknown {
		t.Fatalf("someone's queues: %+v %v", st.Queues, err)
	}
	f.fail["GET "+kqAPI+"/clusterqueues"] = 403
	if st, err = GetKueueStatus(u, u, "proj"); err != nil || !st.QueuesUnknown || len(st.Queues) != 0 || !strings.Contains(st.ItemsProblem, "ClusterQueues and LocalQueues") {
		t.Fatalf("unreadable: %+v %v", st, err)
	}
}

// delete-sample deletes one Ray object, keyed by the UID stored in the record.
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

// Kueue prerequisites install cert-manager and the Red Hat Kueue build, patch
// DataScienceCluster kueue to Unmanaged (never Managed), and wait until RHOAI's
// Kueue CR is Available.
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

// When Kueue APIs are already served by a non-OLM controller, prerequisite setup
// is refused to prevent duplicate controllers and RHOAI state drift.
func TestKueuePrerequisitesRefuseKueueOutsideOLM(t *testing.T) {
	const crdList = "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/clusterqueues.kueue.x-k8s.io"
	for _, tc := range []struct {
		name, crd, deployment string
		refused               bool
	}{
		// Upstream manifests lack OLM labels on the CRD; the controller has a new name.
		{"renamed controller", `{"metadata":{"name":"clusterqueues.kueue.x-k8s.io"}}`, "my-kueue", true},
		{"usual controller", `{"metadata":{"name":"clusterqueues.kueue.x-k8s.io"}}`, "kueue-controller-manager", true},
		// API is served without Kueue's CRD name: confirmation impossible, so refuse.
		{"no CRD", "", "", true},
		// OLM-installed kueue-operator CRDs left after uninstall: a new install can adopt them.
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

// Restoring the kueue component changes DataScienceCluster only once queues are
// removed (Clean up components); until then kueue stays Unmanaged.
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

// Models setup selects hardware profiles per the dashboard Kueue rule: Queue-type
// profiles bound to a LocalQueue only in managed projects, never Queue
// profiles outside Kueue projects. In a Kueue project without a fitting
// Queue profile, or one Kueue is off for in the dashboard, the dashboard
// deploys no model: setup refuses and creates nothing (fail closed).
func TestModelsHardwareProfileKueueRule(t *testing.T) {
	queue := func(name, lq, memMax string) string {
		p := hwProfile(name, "8", memMax, "")
		return p[:len(p)-2] + `,"scheduling":{"type":"Queue","kueue":{"localQueueName":"` + lq + `"}}}}`
	}
	for _, tc := range []struct {
		name, label, disable, queueMem, want, refused string
	}{
		{"not a Kueue project", "", "false", "32Gi", "a-node", ""},
		{"Kueue project", `,"kueue.openshift.io/managed":"true"`, "false", "32Gi", "b-queue", ""},
		{"Kueue project, the Queue profile too small", `,"kueue.openshift.io/managed":"true"`, "false", "4Gi", "", "b-queue: memory 6Gi is outside 2Gi-4Gi"},
		{"Kueue off in the dashboard", `,"kueue.openshift.io/managed":"true"`, "true", "32Gi", "", "while Kueue is off in the dashboard"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mc := newModelsCluster(t)
			mc.f.putJSON("/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/default-dsc", `{
				"spec":{"components":{"kserve":{"managementState":"Managed"},"kueue":{"managementState":"Unmanaged"}}},
				"status":{"conditions":[{"type":"KserveLLMInferenceServiceDependencies","status":"True"}]}}`)
			mc.f.putJSON("/api/v1/namespaces/proj", `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"`+tc.label+`}}}`)
			mc.f.putJSON(kqConfig, `{"spec":{"dashboardConfig":{"disableKueue":`+tc.disable+`}}}`)
			mc.f.putJSON(kqLQ, `{"metadata":{"name":"nightly-kueue","namespace":"proj"}}`)
			putProfiles(mc, hwProfile("a-node", "8", "32Gi", ""), queue("b-queue", "nightly-kueue", tc.queueMem), queue("0-other-queue", "missing", "32Gi"))
			u := userClientOf(mc.c, fakeUserToken)
			res, _ := runModels(t, u, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
			if tc.refused != "" {
				if res.Success || !strings.Contains(res.Message, tc.refused) || !strings.Contains(res.Message, "Nothing was changed.") {
					t.Fatalf("not refused: %+v", res)
				}
				if mc.f.get(testModelPath) != nil {
					t.Fatal("the model was created")
				}
				return
			}
			if !res.Success {
				t.Fatalf("setup failed: %+v", res)
			}
			ann := mc.f.get(testModelPath)["metadata"].(map[string]any)["annotations"].(map[string]any)
			if got, _ := ann[serving.HardwareProfileNameAnnotation].(string); got != tc.want {
				t.Fatalf("hardware profile %q, want %q", got, tc.want)
			}
		})
	}
}

// The dashboard's namespace cannot be found: a Kueue project's model is
// refused (fail closed), a project that is not Kueue's still gets set up
// without a profile, as before.
func TestModelsHardwareProfileAppsNamespaceUnknown(t *testing.T) {
	for _, labelled := range []bool{true, false} {
		mc := newModelsCluster(t)
		label := ""
		if labelled {
			label = `,"kueue.openshift.io/managed":"true"`
		}
		mc.f.putJSON("/api/v1/namespaces/proj", `{"metadata":{"labels":{"opendatahub.io/dashboard":"true"`+label+`}}}`)
		mc.f.fail["GET /apis/dscinitialization.opendatahub.io/v1/dscinitializations"] = 500
		mc.f.fail["GET /apis/dscinitialization.opendatahub.io/v2/dscinitializations"] = 500
		res, _ := runModels(t, userClientOf(mc.c, fakeUserToken), types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"})
		// The model's setup reads the same namespace earlier and refuses
		// first; pickHardwareProfile refuses too (defence in depth).
		if labelled && (res.Success || !strings.Contains(res.Message, "Nothing was changed") || mc.f.get(testModelPath) != nil) {
			t.Fatalf("Kueue project: %+v", res)
		}
		if !labelled && strings.Contains(res.Message, "uses Kueue") {
			t.Fatalf("not a Kueue project: %+v", res)
		}
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
	parts := cleanupParts(c, types.CleanupRequest{Kueue: true, Workbenches: []string{"proj"}, Models: []string{"proj"}, RecipeProjects: []string{"proj"}, Components: []string{"kueue"}}, nil)
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

// kqLeftover is the tool's object at path as setup made it (its UID and
// label), to put back after the project is made again, as a namespace an
// admin force-finalized leaves its objects in etcd for the next namespace
// of that name.
func kqLeftover(t *testing.T, f *resourceFake, path string) map[string]any {
	t.Helper()
	obj := f.get(path)
	if obj == nil {
		t.Fatalf("%s is not there", path)
	}
	raw, _ := json.Marshal(obj)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

// kqDeleteProject deletes project proj with its LocalQueue and samples, as
// deleting the namespace does.
func kqDeleteProject(f *resourceFake) {
	for _, p := range []string{kqLQ, kqRayCs + "/" + kueue.RayClusterName, kqRayJobs + "/" + kueue.RayJobName, wbProjectNS} {
		f.deleteObject(p)
	}
}

// kqRemakeProject deletes project proj and makes it again with UID new-uid
// and the dashboard label plus labels (a JSON fragment starting with a
// comma).
func kqRemakeProject(f *resourceFake, labels string) {
	kqDeleteProject(f)
	f.putJSON(wbProjectNS, `{"metadata":{"name":"proj","uid":"new-uid","labels":{"opendatahub.io/dashboard":"true"`+labels+`}},"status":{"phase":"Active"}}`)
}

// kqSetUpBoth sets up the queues and both samples in proj and returns the
// record.
func kqSetUpBoth(t *testing.T) (*resourceFake, *Client, kueue.Record) {
	t.Helper()
	f, u := putKueueCluster(t)
	if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj", RayCluster: true, RayJob: true}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	return f, u, kueueRecord(t, u)
}

// A foreign LocalQueue named like the tool's in the new project: a new UID
// (UIDs are never reused) and the tool's label, which anyone can copy.
const kqForeignLQ = `{"metadata":{"name":"nightly-kueue","namespace":"proj","uid":"foreign-uid","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}},"spec":{"clusterQueue":"%s"}}`

// A foreign RayJob named like the tool's sample in the new project.
const kqForeignJob = `{"metadata":{"name":"nightly-rayjob","namespace":"proj","uid":"their-job","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`

// The project deleted and made again (a recipe's project-only Clean up, then
// a new run): deleting it deleted the recorded LocalQueue, samples and label,
// so setup sets them up again in the new project, keeps the cluster-scoped
// queues, and claims only what it adds there now.
func TestKueueSetupAfterProjectMadeAgain(t *testing.T) {
	t.Run("set up again in the new project", func(t *testing.T) {
		f, u, old := kqSetUpBoth(t)
		kqRemakeProject(f, "")
		before := len(f.mutations())
		res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj", RayCluster: true, RayJob: true})
		if !res.Success {
			t.Fatalf("setup again: %+v", res)
		}
		want := []string{"POST " + kqLQs, "PATCH " + wbProjectNS, "POST " + kqRayCs, "POST " + kqRayJobs}
		if got := f.mutations()[before:]; strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("writes:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		rec := kueueRecord(t, u)
		if rec.ProjectUID != "new-uid" || !rec.LabelAdded || rec.FlavorUID != old.FlavorUID || rec.ClusterQueueUID != old.ClusterQueueUID ||
			rec.LocalQueueUID == "" || rec.LocalQueueUID == old.LocalQueueUID || rec.RayClusterUID == "" || rec.RayClusterUID == old.RayClusterUID ||
			rec.RayJobUID == "" || rec.RayJobUID == old.RayJobUID {
			t.Fatalf("record %+v (was %+v)", rec, old)
		}
		for path, uid := range map[string]string{kqLQ: rec.LocalQueueUID, kqRayCs + "/" + kueue.RayClusterName: rec.RayClusterUID, kqRayJobs + "/" + kueue.RayJobName: rec.RayJobUID} {
			if got := ensureMap(f.get(path), "metadata")["uid"]; got != uid {
				t.Fatalf("%s has UID %v, the record %s", path, got, uid)
			}
		}
	})
	t.Run("label set by someone else is not claimed", func(t *testing.T) {
		f, u, _ := kqSetUpBoth(t)
		kqRemakeProject(f, `,"kueue.openshift.io/managed":"true"`)
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"}); !res.Success {
			t.Fatalf("setup again: %+v", res)
		}
		if rec := kueueRecord(t, u); rec.LabelAdded || rec.LabelPrior != "" || rec.ProjectUID != "new-uid" {
			t.Fatalf("label claimed: %+v", rec)
		}
	})
	// A same-named LocalQueue in the new project is refused, whether it has
	// a new UID (someone else's) or the old one with the tool's label (left
	// behind by a namespace an admin force-finalized): nothing is deleted
	// and the record stays as it was.
	for _, tc := range []struct {
		name string
		lq   func(f *resourceFake) map[string]any
	}{
		{"someone else's queue with a new UID", func(*resourceFake) map[string]any {
			var obj map[string]any
			_ = json.Unmarshal([]byte(fmt.Sprintf(kqForeignLQ, kueue.ClusterQueueName)), &obj)
			return obj
		}},
		{"the tool's queue left behind with its old UID", func(f *resourceFake) map[string]any { return kqLeftover(t, f, kqLQ) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, u, old := kqSetUpBoth(t)
			lq := tc.lq(f)
			kqRemakeProject(f, "")
			f.put(kqLQ, lq)
			before := len(f.mutations())
			res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"})
			if res.Success || res.ErrorCode != "not_managed" || !strings.Contains(res.Message, "LocalQueue nightly-kueue exists and was not created by this tool") || len(f.mutations()) != before {
				t.Fatalf("got %+v, writes %v", res, f.mutations()[before:])
			}
			if !f.has(kqLQ) {
				t.Fatal("the LocalQueue was deleted")
			}
			if rec := kueueRecord(t, u); !reflect.DeepEqual(rec, old) {
				t.Fatalf("a refused check changed the record: %+v (was %+v)", rec, old)
			}
		})
	}
}

// Clean up after the project was deleted (and made again): the project's
// LocalQueue, samples and label went with it, so their entries are cleared
// first and Clean up goes on to the cluster-wide queues; what has the tool's
// names in the new project is not deleted, and a ClusterQueue a LocalQueue
// there uses stays.
func TestKueueTeardownAfterProjectMadeAgain(t *testing.T) {
	t.Run("someone else's queue and sample of the names", func(t *testing.T) {
		f, u, _ := kqSetUpBoth(t)
		kqRemakeProject(f, "")
		f.putJSON(kqLQ, fmt.Sprintf(kqForeignLQ, "team-cq"))
		f.putJSON(kqRayJobs+"/"+kueue.RayJobName, kqForeignJob)
		before := len(f.mutations())
		res, _ := runKueueOp(t, u, types.KueueRequest{Action: "teardown"})
		if !res.Success {
			t.Fatalf("teardown: %+v", res)
		}
		want := []string{"DELETE " + kqCQ, "DELETE " + kqFlavor}
		if got := f.mutations()[before:]; strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("writes:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if !f.has(kqLQ) || !f.has(kqRayJobs+"/"+kueue.RayJobName) || f.has(kqCQ) || f.has(kqFlavor) {
			t.Fatal("wrong objects deleted")
		}
		if rec := kueueRecord(t, u); !rec.Empty() || rec.Project != "" {
			t.Fatalf("record %+v", rec)
		}
	})
	// A LocalQueue in the new project that uses the tool's ClusterQueue
	// keeps it: someone else's (a new UID) or the tool's own left behind by
	// a force-finalized namespace (its old UID), which only a person deletes.
	for _, tc := range []struct {
		name string
		lq   func(f *resourceFake) map[string]any
	}{
		{"someone else's queue uses the ClusterQueue", func(*resourceFake) map[string]any {
			var obj map[string]any
			_ = json.Unmarshal([]byte(fmt.Sprintf(kqForeignLQ, kueue.ClusterQueueName)), &obj)
			return obj
		}},
		{"the tool's queue left behind with its old UID stays", func(f *resourceFake) map[string]any { return kqLeftover(t, f, kqLQ) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, u, old := kqSetUpBoth(t)
			lq := tc.lq(f)
			kqRemakeProject(f, "")
			f.put(kqLQ, lq)
			before := len(f.mutations())
			res, _ := runKueueOp(t, u, types.KueueRequest{Action: "teardown"})
			if res.Success || res.ErrorCode != "in_use" || !strings.Contains(res.Message, "LocalQueue proj/nightly-kueue") {
				t.Fatalf("got %+v", res)
			}
			if got := f.mutations()[before:]; len(got) != 0 || !f.has(kqCQ) || !f.has(kqFlavor) || !f.has(kqLQ) {
				t.Fatalf("writes %v", got)
			}
			if rec := kueueRecord(t, u); rec.LocalQueueUID != "" || rec.ProjectUID != "" || rec.LabelAdded || rec.RayClusterUID != "" || rec.RayJobUID != "" ||
				rec.ClusterQueueUID != old.ClusterQueueUID || rec.FlavorUID != old.FlavorUID || rec.Project != "proj" {
				t.Fatalf("record %+v", rec)
			}
		})
	}
	t.Run("a label someone else set stays", func(t *testing.T) {
		f, u, _ := kqSetUpBoth(t)
		kqRemakeProject(f, `,"kueue.openshift.io/managed":"true"`)
		patches := len(f.callsTo("PATCH", wbProjectNS))
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "teardown"}); !res.Success {
			t.Fatalf("teardown: %+v", res)
		}
		if nsLabels(f)[kueue.ProjectLabel] != "true" || len(f.callsTo("PATCH", wbProjectNS)) != patches {
			t.Fatal("removed a label someone else set")
		}
		if f.has(kqCQ) || f.has(kqFlavor) {
			t.Fatal("left a queue of the tool's")
		}
	})
	t.Run("project gone", func(t *testing.T) {
		f, u, _ := kqSetUpBoth(t)
		kqDeleteProject(f)
		before := len(f.mutations())
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "teardown"}); !res.Success {
			t.Fatalf("teardown: %+v", res)
		}
		want := []string{"DELETE " + kqCQ, "DELETE " + kqFlavor}
		if got := f.mutations()[before:]; strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("writes:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if rec := kueueRecord(t, u); !rec.Empty() || rec.Project != "" {
			t.Fatalf("record %+v", rec)
		}
	})
	t.Run("project unreadable", func(t *testing.T) {
		f, u, old := kqSetUpBoth(t)
		f.fail["GET "+wbProjectNS] = 500
		before := len(f.mutations())
		res, _ := runKueueOp(t, u, types.KueueRequest{Action: "teardown"})
		if res.Success || !strings.Contains(res.Message, "Cannot read project proj") || !strings.HasSuffix(res.Message, "Nothing was deleted.") || len(f.mutations()) != before {
			t.Fatalf("got %+v, writes %v", res, f.mutations()[before:])
		}
		if rec := kueueRecord(t, u); !reflect.DeepEqual(rec, old) {
			t.Fatalf("record %+v (was %+v)", rec, old)
		}
	})
	t.Run("project being deleted is still the recorded one", func(t *testing.T) {
		f, u, _ := kqSetUpBoth(t)
		ns := f.get(wbProjectNS)
		ensureMap(ns, "metadata")["deletionTimestamp"] = "2026-11-25T10:00:00Z"
		ensureMap(ns, "status")["phase"] = "Terminating"
		f.put(wbProjectNS, ns)
		patches := len(f.callsTo("PATCH", wbProjectNS))
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "teardown"}); !res.Success {
			t.Fatalf("teardown: %+v", res)
		}
		if len(f.callsTo("DELETE", kqLQ)) != 1 || len(f.callsTo("DELETE", kqRayCs+"/"+kueue.RayClusterName)) != 1 || len(f.callsTo("PATCH", wbProjectNS)) != patches+1 {
			t.Fatalf("writes %v", f.mutations())
		}
	})
}

// delete-sample after the project was deleted and made again: the sample
// went with it; a same-named one of someone else's stays.
func TestKueueDeleteSampleAfterProjectMadeAgain(t *testing.T) {
	f, u, _ := kqSetUpBoth(t)
	kqRemakeProject(f, "")
	f.putJSON(kqRayJobs+"/"+kueue.RayJobName, kqForeignJob)
	before := len(f.mutations())
	res, _ := runKueueOp(t, u, types.KueueRequest{Action: "delete-sample", Sample: "rayjob"})
	if !res.Success || res.Message != "Project proj was deleted or made again since this tool's setup, so its RayJob nightly-rayjob is not this tool's to delete. Nothing was deleted." || len(f.mutations()) != before || !f.has(kqRayJobs+"/"+kueue.RayJobName) {
		t.Fatalf("got %+v, writes %v", res, f.mutations()[before:])
	}
	if rec := kueueRecord(t, u); rec.RayJobUID != "" || rec.ProjectUID != "" || rec.LabelAdded || rec.LocalQueueUID != "" || rec.ClusterQueueUID == "" {
		t.Fatalf("record %+v", rec)
	}
}

// The status of a project deleted and made again shows the tool's
// LocalQueue left behind with its old UID as someone else's, and the label
// as not the tool's.
func TestKueueStatusAfterProjectMadeAgain(t *testing.T) {
	f, u, _ := kqSetUpBoth(t)
	lq := kqLeftover(t, f, kqLQ)
	kqRemakeProject(f, `,"kueue.openshift.io/managed":"true"`)
	f.put(kqLQ, lq)
	st, err := GetKueueStatus(u, u, "proj")
	if err != nil || st.Label == nil || !st.Label.Present || st.Label.AddedByTool {
		t.Fatalf("status %+v %v", st, err)
	}
	if o := st.Objects[2]; o.Kind != "LocalQueue" || !o.Present || o.ManagedByTool || !o.Foreign {
		t.Fatalf("LocalQueue %+v", o)
	}
	if o := st.Objects[1]; o.Kind != "ClusterQueue" || !o.ManagedByTool {
		t.Fatalf("ClusterQueue %+v", o)
	}
	if st.ProjectGone != "Project proj was deleted and made again since this tool's setup; set up again." || !st.ProjectRemade {
		t.Fatalf("project gone %q", st.ProjectGone)
	}
	// Deleted and not made again: the status says the project is gone.
	f.deleteObject(wbProjectNS)
	if st, err = GetKueueStatus(u, u, "proj"); err != nil || st.ProjectGone != "Project proj is gone; clean up, or make it again and set up." || st.ProjectRemade {
		t.Fatalf("gone: %q %v", st.ProjectGone, err)
	}
}

// A project that cannot be read: its LocalQueue and samples are shown
// neither as the tool's nor as someone else's (Unknown), and the reason is
// in ItemsProblem; the cluster-wide queues stay the tool's.
func TestKueueStatusProjectUnreadable(t *testing.T) {
	f, u, _ := kqSetUpBoth(t)
	f.fail["GET "+wbProjectNS] = 500
	st, err := GetKueueStatus(u, u, "proj")
	if err != nil || !strings.Contains(st.ItemsProblem, "Cannot read project proj") || st.ProjectGone != "" {
		t.Fatalf("status %+v %v", st, err)
	}
	if o := st.Objects[2]; !o.Present || o.ManagedByTool || o.Foreign || !o.Unknown {
		t.Fatalf("LocalQueue %+v", o)
	}
	for _, s := range st.Samples {
		if s.ManagedByTool || s.Foreign || !s.Unknown {
			t.Fatalf("sample %+v", s)
		}
	}
	if o := st.Objects[1]; !o.ManagedByTool || o.Unknown {
		t.Fatalf("ClusterQueue %+v", o)
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

// A Workload of pods being deleted is leaving, not a user of the LocalQueue
// (Kueue's pod integration makes the Pod its owner, and the garbage
// collector deletes it once the Pod is gone): the teardown waits for it,
// then deletes the queues. Any user that is not leaving refuses the
// teardown at once, as before; a cancel or the operation's end stops the
// wait and keeps the queue.
func TestKueueTeardownWaitsForLeavingWorkloads(t *testing.T) {
	const kqPods = "/api/v1/namespaces/proj/pods"
	podWorkload := func(name, podUID string) string {
		return `{"metadata":{"name":"` + name + `","namespace":"proj","ownerReferences":[{"kind":"Pod","name":"p","uid":"` + podUID + `"}]},"spec":{"queueName":"nightly-kueue"}}`
	}
	// goneAfter removes the paths (the Workload and its Pod, as the
	// garbage collector does) when the teardown lists the Workloads for
	// the nth time.
	goneAfter := func(f *resourceFake, n int, paths ...string) *int {
		lists := 0
		f.beforeServe = func(method, path string) {
			if method != "GET" || path != kqWorkloads {
				return
			}
			lists++
			if lists == n {
				f.mu.Lock()
				defer f.mu.Unlock()
				for _, p := range paths {
					k, _ := parseFakePath(p)
					delete(f.objects, k)
				}
			}
		}
		return &lists
	}
	waited := func(events []UpdateStepEvent, want string) bool {
		for _, e := range events {
			if e.Step == "queues" && e.Status == "running" && e.Message == want {
				return true
			}
		}
		return false
	}

	for _, tc := range []struct {
		name  string
		setup func(f *resourceFake)
	}{
		{"its pod is terminating", func(f *resourceFake) {
			f.putJSON(kqPods+"/p", `{"metadata":{"name":"p","uid":"pod-uid","deletionTimestamp":"2026-11-25T15:34:16Z"}}`)
			f.putJSON(kqWorkloads+"/w1", podWorkload("w1", "pod-uid"))
		}},
		{"its pod is gone", func(f *resourceFake) {
			f.putJSON(kqWorkloads+"/w1", podWorkload("w1", "pod-uid"))
		}},
		{"its pod was made again", func(f *resourceFake) {
			f.putJSON(kqPods+"/p", `{"metadata":{"name":"p","uid":"new-pod-uid"}}`)
			f.putJSON(kqWorkloads+"/w1", podWorkload("w1", "pod-uid"))
		}},
		{"it is terminating", func(f *resourceFake) {
			f.putJSON(kqWorkloads+"/w1", `{"metadata":{"name":"w1","namespace":"proj","deletionTimestamp":"2026-11-25T15:34:16Z","finalizers":["kueue.x-k8s.io/resource-in-use"]},"spec":{"queueName":"nightly-kueue"}}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, u := putKueueCluster(t)
			if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"}); !res.Success {
				t.Fatalf("setup: %+v", res)
			}
			tc.setup(f)
			lists := goneAfter(f, 3, kqWorkloads+"/w1", kqPods+"/p")
			res, events := runKueueOp(t, u, types.KueueRequest{Action: "teardown"})
			if !res.Success || f.has(kqLQ) || f.has(kqCQ) || f.has(kqFlavor) {
				t.Fatalf("got %+v", res)
			}
			if !waited(events, "Waiting for the workload of pods being deleted to finish") || *lists < 3 {
				t.Fatalf("no wait reported (%d lists): %+v", *lists, events)
			}
		})
	}

	t.Run("one user is not leaving", func(t *testing.T) {
		f, u := putKueueCluster(t)
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"}); !res.Success {
			t.Fatalf("setup: %+v", res)
		}
		f.putJSON(kqPods+"/p", `{"metadata":{"name":"p","uid":"pod-uid","deletionTimestamp":"2026-11-25T15:34:16Z"}}`)
		f.putJSON(kqPods+"/q", `{"metadata":{"name":"q","uid":"running-uid"}}`)
		f.putJSON(kqWorkloads+"/w1", podWorkload("w1", "pod-uid"))
		f.putJSON(kqWorkloads+"/w2", podWorkload("w2", "running-uid"))
		f.putJSON(kqWorkloads+"/w3", `{"metadata":{"name":"w3","namespace":"proj","ownerReferences":[{"kind":"Job","name":"j","uid":"job-uid"}]},"spec":{"queueName":"nightly-kueue"}}`)
		lists := goneAfter(f, 0)
		res, events := runKueueOp(t, u, types.KueueRequest{Action: "teardown"})
		// The list's order is the API server's: either user may come first.
		if res.Success || res.ErrorCode != "in_use" || !f.has(kqLQ) || !strings.Contains(res.Message, "Workload w2") || !strings.Contains(res.Message, "Workload w3") ||
			strings.Contains(res.Message, "Workload w1") || !strings.HasSuffix(res.Message, ", so it stays; remove them first.") {
			t.Fatalf("got %+v", res)
		}
		if *lists != 1 || waited(events, "Waiting for the workload of pods being deleted to finish") {
			t.Fatalf("waited (%d lists): %+v", *lists, events)
		}
	})

	t.Run("no pod read without a workload only pods own", func(t *testing.T) {
		f, u := putKueueCluster(t)
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"}); !res.Success {
			t.Fatalf("setup: %+v", res)
		}
		f.putJSON(kqWorkloads+"/w3", `{"metadata":{"name":"w3","namespace":"proj","ownerReferences":[{"kind":"Pod","name":"p","uid":"pod-uid"},{"kind":"Job","name":"j","uid":"job-uid"}]},"spec":{"queueName":"nightly-kueue"}}`)
		res, _ := runKueueOp(t, u, types.KueueRequest{Action: "teardown"})
		if res.Success || res.ErrorCode != "in_use" || len(f.callsTo("GET", kqPods)) != 0 {
			t.Fatalf("got %+v, pod reads %d", res, len(f.callsTo("GET", kqPods)))
		}
	})

	t.Run("unreadable pods", func(t *testing.T) {
		f, u := putKueueCluster(t)
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"}); !res.Success {
			t.Fatalf("setup: %+v", res)
		}
		f.putJSON(kqWorkloads+"/w1", podWorkload("w1", "pod-uid"))
		f.fail["GET "+kqPods] = 403
		res, _ := runKueueOp(t, u, types.KueueRequest{Action: "teardown"})
		if res.Success || !f.has(kqLQ) || !strings.HasPrefix(res.Message, "Cannot tell what uses LocalQueue nightly-kueue, so it stays") {
			t.Fatalf("got %+v", res)
		}
	})

	t.Run("cancel while waiting", func(t *testing.T) {
		f, u := putKueueCluster(t)
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"}); !res.Success {
			t.Fatalf("setup: %+v", res)
		}
		f.putJSON(kqWorkloads+"/w1", podWorkload("w1", "pod-uid"))
		var sw cancelSwitch
		lists := 0
		f.beforeServe = func(method, path string) {
			if method == "GET" && path == kqWorkloads {
				if lists++; lists == 2 {
					sw.on.Store(true)
				}
			}
		}
		res, _ := runKueueOp(t, withCancel(u, &sw), types.KueueRequest{Action: "teardown"})
		if res.Success || res.ErrorCode != "cancelled" || !f.has(kqLQ) || lists != 2 ||
			res.Message != "Cancelled by the user who started it while it waited for workloads of pods being deleted; LocalQueue nightly-kueue stays." {
			t.Fatalf("got %+v after %d lists", res, lists)
		}
	})

	// On one poll, the last Workload goes and the change arrives (a cancel,
	// the queue edited): the teardown deletes nothing.
	lastPoll := func(f *resourceFake, change func(lq map[string]any)) *int {
		lists := 0
		f.beforeServe = func(method, path string) {
			if method != "GET" || path != kqWorkloads {
				return
			}
			if lists++; lists == 2 {
				f.mu.Lock()
				defer f.mu.Unlock()
				k, _ := parseFakePath(kqWorkloads + "/w1")
				delete(f.objects, k)
				lk, _ := parseFakePath(kqLQ)
				change(f.objects[lk])
			}
		}
		return &lists
	}

	t.Run("cancel as the last workload goes", func(t *testing.T) {
		f, u := putKueueCluster(t)
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"}); !res.Success {
			t.Fatalf("setup: %+v", res)
		}
		f.putJSON(kqWorkloads+"/w1", podWorkload("w1", "pod-uid"))
		var sw cancelSwitch
		lists := lastPoll(f, func(map[string]any) { sw.on.Store(true) })
		res, _ := runKueueOp(t, withCancel(u, &sw), types.KueueRequest{Action: "teardown"})
		if res.Success || res.ErrorCode != "cancelled" || !f.has(kqLQ) || !f.has(kqCQ) || *lists != 2 || len(f.callsTo("DELETE", kqLQ)) != 0 {
			t.Fatalf("got %+v after %d lists", res, *lists)
		}
	})

	t.Run("the queue loses the tool's label during the wait", func(t *testing.T) {
		f, u := putKueueCluster(t)
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"}); !res.Success {
			t.Fatalf("setup: %+v", res)
		}
		f.putJSON(kqWorkloads+"/w1", podWorkload("w1", "pod-uid"))
		lastPoll(f, func(lq map[string]any) {
			meta := ensureMap(lq, "metadata")
			delete(ensureMap(meta, "labels"), "app.kubernetes.io/managed-by")
			meta["resourceVersion"] = "99"
		})
		res, _ := runKueueOp(t, u, types.KueueRequest{Action: "teardown"})
		if res.Success || res.ErrorCode != "not_managed" || !f.has(kqLQ) || len(f.callsTo("DELETE", kqLQ)) != 0 ||
			res.Message != "LocalQueue nightly-kueue changed while this tool waited for the workloads of pods being deleted: it is no longer the one this tool created (another UID, or without the label app.kubernetes.io/managed-by=rhoai-nightly-updater), so this tool does not delete it." {
			t.Fatalf("got %+v", res)
		}
	})

	t.Run("Kueue updates the queue between the re-read and the delete", func(t *testing.T) {
		f, u := putKueueCluster(t)
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"}); !res.Success {
			t.Fatalf("setup: %+v", res)
		}
		f.putJSON(kqWorkloads+"/w1", podWorkload("w1", "pod-uid"))
		goneAfter(f, 2, kqWorkloads+"/w1")
		bumped := false
		f.beforeRequest = func(r *http.Request) {
			if r.Method == "DELETE" && r.URL.Path == kqLQ && !bumped {
				bumped = true
				f.mu.Lock()
				defer f.mu.Unlock()
				k, _ := parseFakePath(kqLQ)
				ensureMap(f.objects[k], "metadata")["resourceVersion"] = "100"
			}
		}
		res, _ := runKueueOp(t, u, types.KueueRequest{Action: "teardown"})
		deletes := f.callsTo("DELETE", kqLQ)
		if !res.Success || f.has(kqLQ) || len(deletes) != 2 || !strings.Contains(deletes[1].Body, `"resourceVersion":"100"`) {
			t.Fatalf("got %+v, deletes %+v", res, deletes)
		}
	})

	t.Run("the operation ends while waiting", func(t *testing.T) {
		f, u := putKueueCluster(t)
		if res, _ := runKueueOp(t, u, types.KueueRequest{Action: "setup", Project: "proj"}); !res.Success {
			t.Fatalf("setup: %+v", res)
		}
		f.putJSON(kqWorkloads+"/w1", podWorkload("w1", "pod-uid"))
		ctx, cancel := context.WithCancel(u.Context())
		defer cancel()
		lists := 0
		f.beforeServe = func(method, path string) {
			if method == "GET" && path == kqWorkloads {
				if lists++; lists == 2 {
					cancel()
				}
			}
		}
		res, _ := runKueueOp(t, u.WithContext(ctx), types.KueueRequest{Action: "teardown"})
		if res.Success || res.ErrorCode != "cancelled" || !f.has(kqLQ) || !strings.HasPrefix(res.Message, "Stopped waiting for workloads of pods being deleted") {
			t.Fatalf("got %+v after %d lists", res, lists)
		}
	})
}
