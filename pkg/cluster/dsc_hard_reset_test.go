package cluster

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// hardResetRun plans the hard reset and runs it with the plan's token,
// collecting the steps.
func runHardReset(t *testing.T, c *Client, mutate func(*types.DSCPlanRequest)) (*types.OperationResponse, []UpdateStepEvent) {
	t.Helper()
	plan, err := PlanDSCChange(c, "hard-reset", "", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	req := types.DSCPlanRequest{Name: plan.Name, UID: plan.UID, Token: plan.Token, Confirm: plan.Name, KeepEnabled: new(false)}
	if mutate != nil {
		mutate(&req)
	}
	var mu sync.Mutex
	var steps []UpdateStepEvent
	res, err := HardResetDSC(c, req, func(e UpdateStepEvent) {
		mu.Lock()
		defer mu.Unlock()
		steps = append(steps, e)
	})
	if err != nil {
		t.Fatal(err)
	}
	return res, steps
}

// operatorRemovesDSCAfter makes the fake delete the terminating DSC after
// n reads of it, as the operator does once its modules are cleaned up, and
// report the new DSC Ready.
func operatorRemovesDSCAfter(f *resourceFake, n int) {
	reads := 0
	f.beforeServe = func(method, path string) {
		if method != http.MethodGet || path != dscV3Path {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		k, _ := parseFakePath(dscV3Path)
		obj := f.objects[k]
		if obj == nil || ensureMap(obj, "metadata")["deletionTimestamp"] == nil {
			return
		}
		if reads++; reads >= n {
			delete(f.objects, k)
		}
	}
	f.onGet = func(k fakeKey, obj map[string]any) {
		if k.plural == "datascienceclusters" && ensureMap(obj, "metadata")["uid"] != "ba54ddbf-ad4a-4257-9ab4-977eb541dc1c" {
			obj["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
		}
	}
}

func stepNames(steps []UpdateStepEvent) string {
	var out []string
	for _, s := range steps {
		if len(out) == 0 || !strings.HasPrefix(out[len(out)-1], s.Step+":") || s.Status != "running" {
			out = append(out, s.Step+":"+s.Status)
		}
	}
	return strings.Join(out, " ")
}

func TestHardResetDeletesWaitsAndCreatesTheReleaseDefault(t *testing.T) {
	f, c := managerWorld(t, func(dsc map[string]any) {
		setPath(t, dsc, "spec.components.workbenches.workbenchNamespace", "my-notebooks")
		setPath(t, dsc, "spec.components.aiHub.instancesNamespace", "my-registries")
	})
	operatorRemovesDSCAfter(f, 3)
	plan, _ := PlanDSCChange(c, "hard-reset", "", "", false, false)
	if plan.Refusal != "" || len(plan.Blocked) != 0 || !strings.Contains(plan.NewObject, "my-notebooks") {
		t.Fatalf("plan = %+v", plan)
	}
	if strings.Join(plan.StayRemoved, ",") != "aigateway,aigateway.modelsAsAService" {
		t.Fatalf("stay removed = %v", plan.StayRemoved)
	}
	res, steps := runHardReset(t, c, nil)
	if !res.Success || !strings.Contains(res.Message, "is Ready") {
		t.Fatalf("result = %+v", res)
	}
	deletes := f.callsTo(http.MethodDelete, dscV3Path)
	if len(deletes) != 2 || deletes[0].Query.Get("dryRun") != "All" || deletes[1].Query.Get("dryRun") != "" {
		t.Fatalf("deletes = %+v", deletes)
	}
	for _, d := range deletes {
		if !strings.Contains(d.Body, `"preconditions":{"uid":"ba54ddbf-ad4a-4257-9ab4-977eb541dc1c"}`) {
			t.Fatalf("delete without the UID precondition: %s", d.Body)
		}
	}
	posts := f.callsTo(http.MethodPost, dscV3List)
	if len(posts) != 2 || posts[0].Query.Get("dryRun") != "All" || posts[1].Query.Get("fieldValidation") != "Strict" || posts[1].Query.Get("fieldManager") != dscComponentsManager {
		t.Fatalf("creates = %+v", posts)
	}
	created := f.get(dscV3Path)
	if created == nil || ensureMap(created, "metadata")["name"] != "default-dsc" {
		t.Fatalf("created = %v", created)
	}
	cs := comps(created)
	if v, _ := lookupPath(cs, []string{"workbenches", "workbenchNamespace"}); v != "my-notebooks" {
		t.Fatalf("workbenchNamespace = %v", v)
	}
	if v, _ := lookupPath(cs, []string{"aiHub", "instancesNamespace"}); v != "my-registries" {
		t.Fatalf("instancesNamespace = %v", v)
	}
	if s, _ := lookupState(cs, []string{"aigateway"}); s != "Removed" {
		t.Fatalf("aigateway = %s (not the release default)", s)
	}
	if got := stepNames(steps); got != "check:running check:success delete:running delete:success wait_deleted:running wait_deleted:success create:running create:success wait_ready:running wait_ready:success" {
		t.Fatalf("steps = %s", got)
	}
}

// The DSC is not gone in time: the reset stops without creating anything
// and never touches the finalizer.
func TestHardResetTimesOutWithoutCreating(t *testing.T) {
	f, c := managerWorld(t, nil)
	res, steps := runHardReset(t, c, nil)
	if res.Success || res.ErrorCode != "timeout" || !strings.Contains(res.Message, "platform.opendatahub.io/finalizer") || !strings.Contains(res.Message, "Do not remove the finalizer by hand") {
		t.Fatalf("result = %+v", res)
	}
	if posts := f.callsTo(http.MethodPost, dscV3List); len(posts) != 0 {
		t.Fatalf("created after a timeout: %+v", posts)
	}
	// Only the pre-delete validation, a dry run; never a finalizer.
	if _, real := patchesTo(f); len(real) != 0 {
		t.Fatalf("patched the DSC (finalizer?): %+v", real)
	}
	if !strings.Contains(res.Message, "Finish the hard reset") || !strings.Contains(res.Message, "kind: DataScienceCluster") {
		t.Fatalf("no way to finish in %q", res.Message)
	}
	if obj := f.get(dscV3Path); obj == nil || ensureMap(obj, "metadata")["deletionTimestamp"] == nil {
		t.Fatal("the DSC should still be terminating")
	}
	if last := steps[len(steps)-1]; last.Step != "wait_deleted" || last.Status != "failed" {
		t.Fatalf("last step = %+v", last)
	}
}

// The DSC is deleted and created again between the check and the dry run
// of the new spec, whose JSON Patch tests the uid: the API server's bare
// 422 is told apart from a refused spec by reading the DSC again, and
// nothing is deleted.
func TestHardResetStopsWhenTheDSCWasRecreated(t *testing.T) {
	f, c := managerWorld(t, nil)
	f.beforeRequest = func(r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != dscV3Path {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		k, _ := parseFakePath(dscV3Path)
		meta := ensureMap(f.objects[k], "metadata")
		meta["uid"], meta["resourceVersion"] = "recreated-uid", "200000000"
	}
	res, _ := runHardReset(t, c, nil)
	if res.Success || res.ErrorCode != "conflict" || !strings.Contains(res.Message, "deleted or created again") || !strings.Contains(res.Message, "Nothing was changed") {
		t.Fatalf("result = %+v", res)
	}
	for _, call := range f.callsTo(http.MethodDelete, dscV3Path) {
		if call.Query.Get("dryRun") != "All" {
			t.Fatalf("deleted the DSC: %+v", call)
		}
	}
	if ensureMap(f.get(dscV3Path), "metadata")["uid"] != "recreated-uid" {
		t.Fatal("the recreated DSC is gone")
	}
}

func TestHardResetExplainsHowToRecreateWhenCreateFails(t *testing.T) {
	f, c := managerWorld(t, nil)
	operatorRemovesDSCAfter(f, 1)
	f.fail["POST "+dscV3List] = http.StatusInternalServerError
	res, _ := runHardReset(t, c, nil)
	if res.Success || !strings.Contains(res.Message, "Finish the hard reset") || !strings.Contains(res.Message, "oc create -f -") || !strings.Contains(res.Message, "kind: DataScienceCluster") {
		t.Fatalf("result = %+v", res)
	}
}

func TestHardResetRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(t *testing.T, f *resourceFake, dsc map[string]any)
		mutate func(*types.DSCPlanRequest)
		code   string
	}{
		{"managed RHOAI", func(t *testing.T, _ *resourceFake, dsc map[string]any) {
			if dsc == nil {
				return
			}
			setPath(t, dsc, "status.release.name", "OpenShift AI Cloud Service")
		}, nil, "managed_platform"},
		{"platform unknown", func(t *testing.T, _ *resourceFake, dsc map[string]any) {
			if dsc == nil {
				return
			}
			setPath(t, dsc, "status.release", nil)
		}, nil, "managed_platform"},
		{"already deleting", func(t *testing.T, _ *resourceFake, dsc map[string]any) {
			if dsc == nil {
				return
			}
			setPath(t, dsc, "metadata.deletionTimestamp", "2026-11-18T00:00:00Z")
		}, nil, "conflict"},
		{"argo cd", func(t *testing.T, f *resourceFake, dsc map[string]any) {
			if f != nil {
				// Argo CD answers that its Application lists the DSC.
				f.argo = argoManagingAll(argoDSCObject("default-dsc"))
			}
			if dsc == nil {
				return
			}
			setPath(t, dsc, "metadata.annotations", map[string]any{argoTrackingAnnotation: testArgoApp + ":datasciencecluster.opendatahub.io/DataScienceCluster:x/default-dsc"})
		}, nil, kube.ErrorCodeGitOpsManaged},
		{"operator down", func(t *testing.T, f *resourceFake, dsc map[string]any) {
			if f == nil {
				return
			}
			f.put(rhodsOperator, map[string]any{"status": map[string]any{"readyReplicas": 0}})
		}, nil, "prerequisites"},
		{"two DSCs", func(t *testing.T, f *resourceFake, dsc map[string]any) {
			if f == nil {
				return
			}
			f.put(dscV3List+"/other", map[string]any{"apiVersion": "datasciencecluster.opendatahub.io/v3", "kind": "DataScienceCluster"})
		}, nil, "prerequisites"},
		{"MLflow in use", func(t *testing.T, f *resourceFake, dsc map[string]any) {
			if f == nil {
				return
			}
			f.put(rhoai.ClusterPath(mlflowAPIGroup, "mlflows", mlflowCRName), map[string]any{})
		}, nil, "prerequisites"},
		{"name not typed", nil, func(r *types.DSCPlanRequest) { r.Confirm = "default" }, "validation"},
		{"stale plan", nil, func(r *types.DSCPlanRequest) { r.Token = strings.Repeat("a", 24) }, "conflict"},
		{"another uid", nil, func(r *types.DSCPlanRequest) { r.UID = "other" }, "conflict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// An edit gets the DSC before it is stored, then the fake.
			f, c := managerWorld(t, func(dsc map[string]any) {
				if tc.edit != nil {
					tc.edit(t, nil, dsc)
				}
			})
			if tc.edit != nil {
				tc.edit(t, f, nil)
			}
			res, _ := runHardReset(t, c, tc.mutate)
			if res.Success || res.ErrorCode != tc.code {
				t.Fatalf("result = %+v", res)
			}
			for _, d := range f.callsTo(http.MethodDelete, dscV3Path) {
				if d.Query.Get("dryRun") == "" {
					t.Fatal("deleted the DSC")
				}
			}
		})
	}
}

// newDefaultObject keeps the release default's own values where the old
// DSC has no protected value, and is valid JSON for the create.
func TestNewDefaultObjectIsTheReleaseDefault(t *testing.T) {
	_, c := managerWorld(t, func(dsc map[string]any) {
		setPath(t, dsc, "spec.components.workbenches.workbenchNamespace", nil)
	})
	v, err := readDSCView(c)
	if err != nil {
		t.Fatal(err)
	}
	obj := v.newDefaultObject(v.resetTarget(&types.DSCPlan{}, false))
	raw, _ := json.Marshal(obj)
	if !strings.Contains(string(raw), `"name":"default-dsc"`) || strings.Contains(string(raw), "workbenchNamespace") {
		t.Fatalf("object = %s", raw)
	}
	if !kube.JSONEqual(comps(obj), v.defaultComponents()) {
		t.Fatalf("not the release default: %s", raw)
	}
}
