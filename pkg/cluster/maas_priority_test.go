package cluster

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const tMaaSSubs = "/apis/maas.opendatahub.io/v1alpha1/namespaces/models-as-a-service/maassubscriptions/"

// subPriority is spec.priority of this tool's subscription as stored; it
// fails when the field is not written out.
func subPriority(t *testing.T, mc *modelsCluster) string {
	t.Helper()
	spec, _ := subSpec(t, mc)
	v, ok := spec["priority"]
	if !ok {
		t.Fatalf("spec.priority is not written: %s", asJSON(spec))
	}
	return fmt.Sprint(v)
}

// setSubPriority gives this tool's subscription the priority an earlier
// version wrote, keeping everything else of it.
func setSubPriority(t *testing.T, mc *modelsCluster, v int) {
	t.Helper()
	sub := mc.f.get(tMaaSSub)
	if sub == nil {
		t.Fatal("no subscription")
	}
	ensureMap(sub, "spec")["priority"] = v
	mc.f.put(tMaaSSub, sub)
}

// Every write of this tool's subscription sets spec.priority 0, written out
// so that it also lowers the 10 (or the value below the others') an earlier
// version left: Publish, Publish of another model, Change limit, Change who
// can use them, Unpublish of one of two models, and the background prune.
func TestMaaSPriority_EveryWriteSetsZero(t *testing.T) {
	t.Run("publish alone", func(t *testing.T) {
		mc := publishedCluster(t)
		if got := subPriority(t, mc); got != "0" {
			t.Fatalf("priority %s", got)
		}
	})
	t.Run("publish with others, whatever their priorities", func(t *testing.T) {
		mc := maasClusterWithModel(t)
		mc.f.putJSON(tMaaSSubs+"premium", `{"spec":{"priority":10}}`)
		mc.f.putJSON(tMaaSSubs+"free", `{"spec":{"priority":-3}}`)
		if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"}); !res.Success {
			t.Fatalf("publish: %+v", res)
		}
		if got := subPriority(t, mc); got != "0" {
			t.Fatalf("priority %s", got)
		}
	})
	t.Run("publish another model", func(t *testing.T) {
		mc := maasWithModels(t, "qwen25-05b-instruct", "smollm2-135m-instruct")
		if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj", Model: "qwen25-05b-instruct"}); !res.Success {
			t.Fatalf("publish: %+v", res)
		}
		setSubPriority(t, mc, 10)
		if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj", Model: "smollm2-135m-instruct"}); !res.Success {
			t.Fatalf("publish: %+v", res)
		}
		if got := subPriority(t, mc); got != "0" {
			t.Fatalf("priority %s", got)
		}
	})
	t.Run("change limit", func(t *testing.T) {
		mc := publishedCluster(t)
		setSubPriority(t, mc, 10)
		if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "change-limit", Project: "proj", Model: "qwen25-05b-instruct", Limit: &types.MaaSLimit{Preset: "premium"}}); !res.Success {
			t.Fatalf("change limit: %+v", res)
		}
		if got := subPriority(t, mc); got != "0" {
			t.Fatalf("priority %s", got)
		}
	})
	t.Run("change access", func(t *testing.T) {
		mc := publishedCluster(t)
		setSubPriority(t, mc, -4)
		if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "change-access", Access: &types.MaaSAudience{Users: []string{"alice"}}}); !res.Success {
			t.Fatalf("change access: %+v", res)
		}
		if got := subPriority(t, mc); got != "0" {
			t.Fatalf("priority %s", got)
		}
	})
	t.Run("unpublish one of two", func(t *testing.T) {
		mc := maasWithModels(t, "qwen25-05b-instruct", "smollm2-135m-instruct")
		for _, m := range []string{"qwen25-05b-instruct", "smollm2-135m-instruct"} {
			if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj", Model: m}); !res.Success {
				t.Fatalf("publish %s: %+v", m, res)
			}
		}
		setSubPriority(t, mc, 10)
		if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj", Model: "smollm2-135m-instruct"}); !res.Success {
			t.Fatalf("unpublish: %+v", res)
		}
		if got := subPriority(t, mc); got != "0" {
			t.Fatalf("priority %s", got)
		}
	})
	t.Run("prune", func(t *testing.T) {
		mc := maasWithModels(t, "qwen25-05b-instruct", "smollm2-135m-instruct")
		for _, m := range []string{"qwen25-05b-instruct", "smollm2-135m-instruct"} {
			if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj", Model: m}); !res.Success {
				t.Fatalf("publish %s: %+v", m, res)
			}
		}
		setSubPriority(t, mc, 10)
		// smollm2 is made again by someone else: its publication is gone.
		mc.f.mu.Lock()
		k, _ := parseFakePath(strings.Replace(testModelPath, "qwen25-05b-instruct", "smollm2-135m-instruct", 1))
		ensureMap(mc.f.objects[k], "metadata")["uid"] = "someone-elses-uid"
		mc.f.mu.Unlock()
		if changed, _, err := serving.PruneGoneMaaSPublications(mc.c); err != nil || !changed {
			t.Fatalf("prune: changed %t, %v", changed, err)
		}
		if got := subPriority(t, mc); got != "0" {
			t.Fatalf("priority %s", got)
		}
	})
}

// The MaaS tab reads the priority of this tool's subscription: the fix is
// offered while it is not 0 (above, or below, which maas-api reads as 0),
// and the other subscriptions maas-api ranks level with it are named
// (spec.priority 0, unset or negative; not one being deleted).
func TestMaaSStatus_SubscriptionPriority(t *testing.T) {
	mc := publishedCluster(t)
	st, err := GetMaaSStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	if p := st.SubscriptionPriority; p == nil || p.Value != 0 || p.Fix != nil || len(p.Ties) != 0 {
		t.Fatalf("priority %+v", p)
	}

	mc.f.putJSON(tMaaSSubs+"admin-unset", `{"spec":{}}`)
	mc.f.putJSON(tMaaSSubs+"admin-zero", `{"spec":{"priority":0}}`)
	mc.f.putJSON(tMaaSSubs+"admin-negative", `{"spec":{"priority":-2}}`)
	mc.f.putJSON(tMaaSSubs+"premium", `{"spec":{"priority":5}}`)
	mc.f.putJSON(tMaaSSubs+"leaving", `{"metadata":{"deletionTimestamp":"2026-11-25T00:00:00Z","finalizers":["x"]},"spec":{"priority":0}}`)
	setSubPriority(t, mc, 10)
	st, _ = GetMaaSStatus(mc.c)
	p := st.SubscriptionPriority
	if p == nil || p.Value != 10 || !slices.Equal(p.Ties, []types.MaaSPriorityTie{{Name: "admin-negative", Priority: -2}, {Name: "admin-unset"}, {Name: "admin-zero"}}) {
		t.Fatalf("priority %+v", p)
	}
	if p.Fix == nil || p.Fix.ID != "subscription-priority" || p.Fix.Label != "Set to the lowest priority (0)" || p.Fix.Danger ||
		len(p.Fix.Changes) != 1 || !strings.Contains(p.Fix.Changes[0], "rhoai-nightly-updater-subscription: spec.priority 10 to 0") ||
		!strings.Contains(p.Fix.Summary, "revoke them and make them again") {
		t.Fatalf("fix %+v", p.Fix)
	}

	setSubPriority(t, mc, -4)
	st, _ = GetMaaSStatus(mc.c)
	if p := st.SubscriptionPriority; p == nil || p.Value != -4 || p.Fix == nil || !strings.Contains(p.Fix.Changes[0], "spec.priority -4 to 0") {
		t.Fatalf("a negative priority: %+v", p)
	}

	// Ties that cannot be listed: the rest is still shown.
	mc.f.fail["GET /apis/maas.opendatahub.io/v1alpha1/namespaces/models-as-a-service/maassubscriptions"] = 403
	st, _ = GetMaaSStatus(mc.c)
	if p := st.SubscriptionPriority; p == nil || p.Fix == nil || p.Ties != nil {
		t.Fatalf("without the list: %+v", p)
	}
	delete(mc.f.fail, "GET /apis/maas.opendatahub.io/v1alpha1/namespaces/models-as-a-service/maassubscriptions")

	// A subscription of that name that is someone else's: nothing to offer.
	mc.f.putJSON(tMaaSSub, `{"spec":{"priority":10,"owner":{"groups":[{"name":"system:authenticated"}]}}}`)
	st, _ = GetMaaSStatus(mc.c)
	if st.SubscriptionPriority != nil {
		t.Fatalf("someone else's subscription: %+v", st.SubscriptionPriority)
	}
}

// Set to the lowest priority changes the priority alone: a publication
// that is gone (its project deleted) is not pruned by it, as every other
// MaaS change does first; the next one does.
func TestMaaSSetSubscriptionPriority_LeavesGonePublications(t *testing.T) {
	mc := publishedCluster(t)
	setSubPriority(t, mc, 10)
	// The project is deleted (its namespace object goes, the rest stays).
	mc.f.mu.Lock()
	delete(mc.f.objects, fakeKey{gv: "v1", plural: "namespaces", name: "proj"})
	mc.f.mu.Unlock()
	subBefore := mc.f.get(tMaaSSub)
	refsBefore := asJSON(ensureMap(subBefore, "spec")["modelRefs"])
	mc.f.requests = nil
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "subscription-priority"})
	if !res.Success || subPriority(t, mc) != "0" {
		t.Fatalf("res %+v", res)
	}
	if !gatewayAdmits(t, mc) {
		t.Fatal("the gone project left the gateway: the action pruned")
	}
	if got := asJSON(ensureMap(mc.f.get(tMaaSSub), "spec")["modelRefs"]); got != refsBefore {
		t.Fatalf("modelRefs changed: %s", got)
	}
	for _, r := range mc.f.requests {
		if !strings.HasPrefix(r, "GET ") && !strings.HasSuffix(r, " "+tMaaSSub) && !strings.HasSuffix(r, "/configmaps/rhoai-nightly-updater-activity") {
			t.Errorf("another write: %s", r)
		}
	}
}

// priorityPatches are the merge patches of this tool's subscription that
// were not dry runs.
func priorityPatches(mc *modelsCluster) []fakeCall {
	var out []fakeCall
	for _, c := range mc.f.callsTo(http.MethodPatch, tMaaSSub) {
		if c.Query.Get("dryRun") == "" && c.ContentType == "application/merge-patch+json" {
			out = append(out, c)
		}
	}
	return out
}

// Set to the lowest priority: after a dry run, one merge patch of this
// tool's subscription, with the read resourceVersion, sets spec.priority to
// 0 and nothing else (the models, their limits, the audience and the
// annotations stay); the access policy is not touched.
func TestMaaSSetSubscriptionPriority(t *testing.T) {
	mc := publishedCluster(t)
	setSubPriority(t, mc, 10)
	before := mc.f.get(tMaaSSub)
	beforeSpec, beforeDesc := asJSON(before["spec"]), asJSON(ensureMap(before, "metadata")["annotations"])
	beforeRV := fmt.Sprint(ensureMap(before, "metadata")["resourceVersion"])
	authBefore := asJSON(mc.f.get(tMaaSAuth))
	mc.f.requests = nil

	res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "subscription-priority"})
	if !res.Success || finalSteps(events) != "check=success priority=success" {
		t.Fatalf("res %+v, steps %s", res, finalSteps(events))
	}
	if got := subPriority(t, mc); got != "0" {
		t.Fatalf("priority %s", got)
	}
	after := mc.f.get(tMaaSSub)
	afterSpec := ensureMap(after, "spec")
	afterSpec["priority"] = 10
	if asJSON(afterSpec) != beforeSpec || asJSON(ensureMap(after, "metadata")["annotations"]) != beforeDesc {
		t.Fatalf("more than the priority changed:\n%s\n%s", beforeSpec, asJSON(afterSpec))
	}
	afterSpec["priority"] = 0
	if asJSON(mc.f.get(tMaaSAuth)) != authBefore {
		t.Fatal("the authorization policy changed")
	}
	dry, write := slices.Index(mc.f.requests, "DRYRUN PATCH "+tMaaSSub), slices.Index(mc.f.requests, "PATCH "+tMaaSSub)
	if dry < 0 || write < dry {
		t.Fatalf("no dry run before the write: %v", mc.f.requests)
	}
	patches := priorityPatches(mc)
	if len(patches) != 1 {
		t.Fatalf("patches %+v", patches)
	}
	if want := `{"metadata":{"resourceVersion":"` + beforeRV + `"},"spec":{"priority":0}}`; patches[0].Body != want || patches[0].Query.Get("fieldManager") != "rhoai-nightly-updater" {
		t.Fatalf("patch %s ?%v, want %s", patches[0].Body, patches[0].Query, want)
	}
	for _, r := range mc.f.requests {
		// The activity log is the tool's own bookkeeping.
		if !strings.HasPrefix(r, "GET ") && !strings.HasSuffix(r, " "+tMaaSSub) && !strings.HasSuffix(r, "/configmaps/rhoai-nightly-updater-activity") {
			t.Errorf("another write: %s", r)
		}
	}
	if !strings.Contains(strings.Join(res.Logs, "\n"), "revoke and make again") {
		t.Errorf("the steps do not say that keys made before keep their subscription: %v", res.Logs)
	}
	if st, _ := GetMaaSStatus(mc.c); st.SubscriptionPriority == nil || st.SubscriptionPriority.Fix != nil {
		t.Fatalf("still offered: %+v", st.SubscriptionPriority)
	}

	// At 0 already: nothing is written.
	res, _ = runMaaSOp(t, mc.c, types.MaaSRequest{Action: "subscription-priority"})
	if !res.Success || !strings.Contains(res.Message, "priority 0 already") || len(priorityPatches(mc)) != 1 {
		t.Fatalf("again: %+v", res)
	}
}

// Set to the lowest priority changes only this tool's subscription: none,
// someone else's, a dry-run refusal and a change between the read and the
// write each stop it with the priority as it was.
func TestMaaSSetSubscriptionPriority_Refusals(t *testing.T) {
	t.Run("no subscription", func(t *testing.T) {
		mc := maasClusterWithModel(t)
		res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "subscription-priority"})
		if res.Success || res.ErrorCode != "prerequisites" || len(mc.f.callsTo(http.MethodPatch, tMaaSSub)) != 0 {
			t.Fatalf("res %+v", res)
		}
	})
	t.Run("someone else's", func(t *testing.T) {
		mc := maasClusterWithModel(t)
		mc.f.putJSON(tMaaSSub, `{"spec":{"priority":10,"owner":{"groups":[{"name":"system:authenticated"}]}}}`)
		res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "subscription-priority"})
		if res.Success || res.ErrorCode != "not_managed" || !strings.Contains(res.Message, "was not created by this tool") || len(mc.f.callsTo(http.MethodPatch, tMaaSSub)) != 0 {
			t.Fatalf("res %+v", res)
		}
		if got := fmt.Sprint(ensureMap(mc.f.get(tMaaSSub), "spec")["priority"]); got != "10" {
			t.Fatalf("priority %s", got)
		}
	})
	t.Run("dry run refused", func(t *testing.T) {
		mc := publishedCluster(t)
		setSubPriority(t, mc, 10)
		mc.f.failDryRun["PATCH "+tMaaSSub] = 422
		res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "subscription-priority"})
		if res.Success || finalSteps(events) != "check=failed" || len(priorityPatches(mc)) != 0 || subPriority(t, mc) != "10" {
			t.Fatalf("res %+v", res)
		}
	})
	t.Run("changed since it was read", func(t *testing.T) {
		mc := publishedCluster(t)
		setSubPriority(t, mc, 10)
		mc.f.beforeRequest = func(r *http.Request) {
			if r.Method == http.MethodPatch && r.URL.Path == tMaaSSub && r.URL.Query().Get("dryRun") == "" {
				mc.f.mu.Lock()
				defer mc.f.mu.Unlock()
				k, _ := parseFakePath(tMaaSSub)
				mc.f.bumpRV(ensureMap(mc.f.objects[k], "metadata"))
			}
		}
		res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "subscription-priority"})
		if res.Success || res.ErrorCode != "conflict" || finalSteps(events) != "check=success priority=failed" || subPriority(t, mc) != "10" {
			t.Fatalf("res %+v, steps %s", res, finalSteps(events))
		}
	})
	t.Run("being deleted", func(t *testing.T) {
		mc := publishedCluster(t)
		sub := mc.f.get(tMaaSSub)
		meta := ensureMap(sub, "metadata")
		meta["deletionTimestamp"], meta["finalizers"] = "2026-11-25T00:00:00Z", []any{"maas.opendatahub.io/cleanup"}
		ensureMap(sub, "spec")["priority"] = 10
		mc.f.put(tMaaSSub, sub)
		res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "subscription-priority"})
		if res.Success || res.ErrorCode != "conflict" || !strings.Contains(res.Message, "is being deleted") || len(mc.f.callsTo(http.MethodPatch, tMaaSSub)) != 0 {
			t.Fatalf("res %+v", res)
		}
	})
	t.Run("not for a model", func(t *testing.T) {
		if got := ValidateMaaSRequest(types.MaaSRequest{Action: "subscription-priority", Project: "p"}); got != "the platform takes no project" {
			t.Fatalf("validation %q", got)
		}
		if got := ValidateMaaSRequest(types.MaaSRequest{Action: "subscription-priority", Limit: &types.MaaSLimit{Preset: "free"}}); got != "subscription-priority takes no limit" {
			t.Fatalf("validation %q", got)
		}
	})
}
