package cluster

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The live scenario (RHOAI 3.6, 2026-11-19): this tool set MaaS up
// (aigateway and modelsAsAService Managed, as the fixture DSC has them)
// and published a model, so a MaaSModelRef, the tool's MaaSSubscription
// and MaaSAuthPolicy exist and the record lists the publication.
const (
	maasGroupDocV1alpha1 = `{"kind":"APIGroup","name":"maas.opendatahub.io","versions":[{"groupVersion":"maas.opendatahub.io/v1alpha1","version":"v1alpha1"}],"preferredVersion":{"groupVersion":"maas.opendatahub.io/v1alpha1","version":"v1alpha1"}}`
	guardModelRef        = "/apis/maas.opendatahub.io/v1alpha1/namespaces/proj/maasmodelrefs/qwen25-05b-instruct"
	guardModelRefList    = "/apis/maas.opendatahub.io/v1alpha1/maasmodelrefs"
	guardSubscription    = "/apis/maas.opendatahub.io/v1alpha1/namespaces/models-as-a-service/maassubscriptions/rhoai-nightly-updater-subscription"
	guardAuthPolicy      = "/apis/maas.opendatahub.io/v1alpha1/namespaces/models-as-a-service/maasauthpolicies/rhoai-nightly-updater-access"
)

func snapshotPath() string {
	return namespacedPath("v1", "configmaps", getActivityNamespace(), snapshotConfigMapName)
}

// maasServed makes the fake serve the MaaS API (no objects yet).
func maasServed(f *resourceFake) {
	f.groups["/apis/maas.opendatahub.io"] = maasGroupDocV1alpha1
}

// putMaaSRecord stores this tool's MaaS record.
func putMaaSRecord(t *testing.T, f *resourceFake, rec maasRecord) {
	t.Helper()
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	f.put(snapshotPath(), map[string]interface{}{"metadata": map[string]interface{}{"name": snapshotConfigMapName, "namespace": getActivityNamespace()},
		"data": map[string]interface{}{maasRecordKey: string(raw)}})
}

// maasInUseWorld is managerWorld with MaaS in use as on the live cluster.
func maasInUseWorld(t *testing.T, edit func(dsc map[string]interface{})) (*resourceFake, *Client) {
	t.Helper()
	f, c := managerWorld(t, edit)
	maasServed(f)
	f.put(guardModelRef, map[string]interface{}{"metadata": map[string]interface{}{"name": "qwen25-05b-instruct", "namespace": "proj", "labels": toolLabels()},
		"spec": map[string]interface{}{"modelRef": map[string]interface{}{"kind": "LLMInferenceService", "name": "qwen25-05b-instruct"}}})
	f.put(guardSubscription, map[string]interface{}{"metadata": map[string]interface{}{"name": maasSubscriptionName, "namespace": maasTenantNamespace, "labels": toolLabels()}})
	f.put(guardAuthPolicy, map[string]interface{}{"metadata": map[string]interface{}{"name": maasAuthPolicyName, "namespace": maasTenantNamespace, "labels": toolLabels()}})
	putMaaSRecord(t, f, maasRecord{DSCChanged: true, PreviousModelsAsAService: "Removed", AIGatewayChanged: true, PreviousAIGateway: "Removed",
		Published: []string{"proj/qwen25-05b-instruct"}, DBNamespace: "redhat-ai-gateway-infra", Database: "cnpg"})
	return f, c
}

// maasBlock returns the block of component in plan, or nil.
func maasBlock(plan *types.DSCPlan, component string) *types.DSCRemovalBlock {
	for i := range plan.Blocked {
		if plan.Blocked[i].Component == component {
			return &plan.Blocked[i]
		}
	}
	return nil
}

func mustPlan(t *testing.T, c *Client, action, path, to string, keep bool) *types.DSCPlan {
	t.Helper()
	plan, err := PlanDSCChange(c, action, path, to, false, keep)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// assertMaaSBlock checks that plan blocks component with the in-use message.
func assertMaaSBlock(t *testing.T, plan *types.DSCPlan, component string) {
	t.Helper()
	b := maasBlock(plan, component)
	if b == nil {
		t.Fatalf("%s not blocked: %+v", component, plan)
	}
	why := strings.Join(b.Reasons, " ")
	for _, want := range []string{"MaaS is in use", "published model proj/qwen25-05b-instruct", "subscription models-as-a-service/rhoai-nightly-updater-subscription",
		"access policy models-as-a-service/rhoai-nightly-updater-access", "this tool set MaaS up", "every API key stops working",
		"Remove MaaS on the MaaS tab first (Clean up MaaS unpublishes and removes it in order)", "or keep " + component + " Managed"} {
		if !strings.Contains(why, want) {
			t.Errorf("block of %s lacks %q: %s", component, want, why)
		}
	}
}

func assertNoDSCPatch(t *testing.T, f *resourceFake) {
	t.Helper()
	if dry, real := patchesTo(f, dscV3Path); len(dry)+len(real) != 0 {
		t.Fatalf("patched the DSC: %d dry runs, %d patches", len(dry), len(real))
	}
}

func TestMaaSGuardBlocksTogglingAIGatewayOff(t *testing.T) {
	f, c := maasInUseWorld(t, nil)
	plan := mustPlan(t, c, "toggle", "aigateway", "Removed", false)
	assertMaaSBlock(t, plan, "aigateway")
	res, err := reviewedToggle(t, c, types.DSCToggleRequest{Name: "default-dsc", Path: "aigateway", From: "Managed", To: "Removed"})
	if err != nil || res.Success || res.ErrorCode != "prerequisites" || !strings.Contains(res.Message, "aigateway must not be removed now") || !strings.Contains(res.Message, "MaaS is in use") {
		t.Fatalf("toggle = %+v, %v", res, err)
	}
	assertNoDSCPatch(t, f)
}

func TestMaaSGuardBlocksTurningMaaSOff(t *testing.T) {
	f, c := maasInUseWorld(t, nil)
	plan := mustPlan(t, c, "toggle", "aigateway.modelsAsAService", "Removed", false)
	assertMaaSBlock(t, plan, "aigateway.modelsAsAService")
	if maasBlock(plan, "aigateway") != nil {
		t.Fatalf("aigateway stays, yet blocked: %+v", plan.Blocked)
	}
	res, _ := reviewedToggle(t, c, types.DSCToggleRequest{Name: "default-dsc", Path: "aigateway.modelsAsAService", From: "Managed", To: "Removed"})
	if res.Success {
		t.Fatalf("toggle = %+v", res)
	}
	assertNoDSCPatch(t, f)
}

// Diagnostics' and the Components page's disable-component fix runs the
// toggle's checks: the guard applies there too.
func TestMaaSGuardBlocksDisableComponent(t *testing.T) {
	f, c := maasInUseWorld(t, nil)
	res, err := applyFixDisableComponent(c, "aigateway")
	if err != nil || res.Success || !strings.Contains(res.Message, "MaaS is in use") {
		t.Fatalf("disable = %+v, %v", res, err)
	}
	assertNoDSCPatch(t, f)
}

// The version-matched repair (reset-defaults) checks the MaaS guard
// before it patches the DSC.
func TestMaaSGuardBlocksTheRepair(t *testing.T) {
	f, c := maasInUseWorld(t, nil)
	compat := checkDSCCompatibility(c, f.get(dscV3Path))
	res, err := RepairDSC(c, "default-dsc", "reset-defaults", "3.6.0", "datasciencecluster.opendatahub.io/v3", nil, compat.ResetRemovals)
	if err != nil || res.Success || res.ErrorCode != "prerequisites" || !strings.Contains(res.Message, "MaaS is in use") {
		t.Fatalf("repair = %+v, %v (removals %v)", res, err, compat.ResetRemovals)
	}
	if real := f.callsTo(http.MethodPatch, dscV3Path); len(real) != 0 {
		t.Fatalf("patched the DSC: %+v", real)
	}
}

func TestSoftResetWithoutKeepIsBlockedWhileMaaSIsInUse(t *testing.T) {
	f, c := maasInUseWorld(t, nil)
	plan := mustPlan(t, c, "soft-reset", "", "", false)
	assertMaaSBlock(t, plan, "aigateway")
	if plan.KeepEnabled || len(plan.Kept) != 0 || !reflect.DeepEqual(plan.TurnedOn, []string{"aigateway", "aigateway.modelsAsAService"}) {
		t.Fatalf("keep = %v, kept = %v, turned on = %v", plan.KeepEnabled, plan.Kept, plan.TurnedOn)
	}
	res, _ := SoftResetDSC(c, types.DSCPlanRequest{Name: "default-dsc", UID: plan.UID, Token: plan.Token, KeepEnabled: boolPtr(false)})
	if res.Success || res.ErrorCode != "prerequisites" {
		t.Fatalf("reset = %+v", res)
	}
	assertNoDSCPatch(t, f)
}

func TestSoftResetKeepsWhatTheUserTurnedOn(t *testing.T) {
	f, c := maasInUseWorld(t, func(dsc map[string]interface{}) {
		setPath(t, dsc, "spec.components.kserve.rawDeploymentServiceConfig", "Headed")
	})
	plan := mustPlan(t, c, "soft-reset", "", "", true)
	if plan.Refusal != "" || len(plan.Blocked) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
	if !plan.KeepEnabled || !reflect.DeepEqual(plan.Kept, []string{"aigateway", "aigateway.modelsAsAService"}) || !reflect.DeepEqual(plan.TurnedOn, plan.Kept) {
		t.Fatalf("kept = %v, turned on = %v", plan.Kept, plan.TurnedOn)
	}
	for _, ch := range plan.Changes {
		if strings.HasPrefix(ch.Path, "aigateway") {
			t.Fatalf("the reset changes %s: %+v", ch.Path, plan.Changes)
		}
	}
	// The token binds the option: the plan without keep is another change.
	other := mustPlan(t, c, "soft-reset", "", "", false)
	if other.Token == plan.Token {
		t.Fatal("keepEnabled is not part of the token")
	}
	res, _ := SoftResetDSC(c, types.DSCPlanRequest{Name: "default-dsc", UID: plan.UID, Token: plan.Token, KeepEnabled: boolPtr(false)})
	if res.Success {
		t.Fatalf("the reviewed keep plan ran without keep: %+v", res)
	}
	res, _ = SoftResetDSC(c, types.DSCPlanRequest{Name: "default-dsc", UID: plan.UID, Token: plan.Token})
	if res.Success || res.ErrorCode != "validation" {
		t.Fatalf("reset without keepEnabled = %+v", res)
	}
	assertNoDSCPatch(t, f)
	res, err := SoftResetDSC(c, types.DSCPlanRequest{Name: "default-dsc", UID: plan.UID, Token: plan.Token, KeepEnabled: boolPtr(true)})
	if err != nil || !res.Success || !strings.Contains(res.Message, "Kept: aigateway, aigateway.modelsAsAService (you turned these on).") {
		t.Fatalf("reset = %+v, %v", res, err)
	}
	stored := comps(f.get(dscV3Path))
	for _, p := range []string{"aigateway", "aigateway.modelsAsAService"} {
		if s, _ := lookupState(stored, strings.Split(p, ".")); s != "Managed" {
			t.Fatalf("%s = %s after a reset that keeps it", p, s)
		}
	}
	if _, ok := lookupPath(stored, []string{"kserve", "rawDeploymentServiceConfig"}); ok {
		t.Fatal("the reset kept rawDeploymentServiceConfig")
	}
}

// With nothing else to change, a reset that keeps everything turned on
// has nothing to do, and says what it keeps.
func TestSoftResetKeepingEverythingHasNothingToDo(t *testing.T) {
	_, c := maasInUseWorld(t, nil)
	plan := mustPlan(t, c, "soft-reset", "", "", true)
	if plan.Code != "nothing_to_do" || !strings.Contains(plan.Refusal, "which the reset keeps (aigateway, aigateway.modelsAsAService)") || len(plan.TurnedOn) != 2 {
		t.Fatalf("plan = %+v", plan)
	}
}

// What the user turned on: an Unmanaged component the default leaves
// Removed, and a sub-component under a parent the default enables (kept
// alone); an orphan (Managed under a Removed parent) deploys nothing and is
// not kept.
func TestTurnedOnListsDeployedTogglesTheDefaultLeavesOff(t *testing.T) {
	_, c := managerWorld(t, func(dsc map[string]interface{}) {
		setPath(t, dsc, "spec.components.kueue.managementState", "Unmanaged")
		setPath(t, dsc, "spec.components.kserve.modelExpress.managementState", "Managed")
		setPath(t, dsc, "spec.components.aigateway.managementState", "Removed")
		setPath(t, dsc, "spec.components.aigateway.batchGateway.managementState", "Managed")
	})
	v, err := readDSCView(c)
	if err != nil {
		t.Fatal(err)
	}
	plan := &types.DSCPlan{}
	target := v.resetTarget(plan, true)
	if want := []string{"kserve.modelExpress", "kueue"}; !reflect.DeepEqual(plan.Kept, want) {
		t.Fatalf("kept = %v, want %v", plan.Kept, want)
	}
	if s, _ := lookupState(target, []string{"kueue"}); s != "Unmanaged" {
		t.Fatalf("kueue = %s", s)
	}
	if s, _ := lookupState(target, []string{"aigateway", "modelsAsAService"}); s != "Removed" {
		t.Fatalf("modelsAsAService = %s (an orphan was kept)", s)
	}
	plan = &types.DSCPlan{}
	if target := v.resetTarget(plan, false); !jsonEqual(target, v.defaultComponents()) || len(plan.Kept) != 0 || len(plan.TurnedOn) != 2 {
		t.Fatalf("without keep: kept %v, turned on %v, target %v", plan.Kept, plan.TurnedOn, target)
	}
}

// A sub-component kept needs its parent: both are kept, parent first.
func TestKeptSubComponentKeepsItsParent(t *testing.T) {
	_, c := managerWorld(t, func(dsc map[string]interface{}) {
		setPath(t, dsc, "spec.components.aigateway.modelsAsAService.managementState", "Removed")
		setPath(t, dsc, "spec.components.aigateway.batchGateway.managementState", "Managed")
	})
	v, _ := readDSCView(c)
	plan := &types.DSCPlan{}
	target := v.resetTarget(plan, true)
	if want := []string{"aigateway", "aigateway.batchGateway"}; !reflect.DeepEqual(plan.Kept, want) {
		t.Fatalf("kept = %v, want %v", plan.Kept, want)
	}
	if s, _ := lookupState(target, []string{"aigateway"}); s != "Managed" {
		t.Fatalf("aigateway = %s", s)
	}
}

func TestHardResetWithoutKeepIsBlockedWhileMaaSIsInUse(t *testing.T) {
	f, c := maasInUseWorld(t, nil)
	plan := mustPlan(t, c, "hard-reset", "", "", false)
	assertMaaSBlock(t, plan, "aigateway")
	res, _ := runHardReset(t, c, nil)
	if res.Success || res.ErrorCode != "prerequisites" || !strings.Contains(res.Message, "MaaS is in use") {
		t.Fatalf("hard reset = %+v", res)
	}
	if deletes := f.callsTo(http.MethodDelete, dscV3Path); len(deletes) != 0 {
		t.Fatalf("deleted the DSC: %+v", deletes)
	}
}

func TestHardResetKeepsWhatTheUserTurnedOn(t *testing.T) {
	f, c := maasInUseWorld(t, nil)
	operatorRemovesDSCAfter(f, 3)
	plan := mustPlan(t, c, "hard-reset", "", "", true)
	if plan.Refusal != "" || len(plan.Blocked) != 0 || len(plan.StayRemoved) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
	if !reflect.DeepEqual(plan.Kept, []string{"aigateway", "aigateway.modelsAsAService"}) {
		t.Fatalf("kept = %v", plan.Kept)
	}
	// MaaS is down while the components are: a warning, not a block.
	warned := false
	for _, w := range plan.Warnings {
		warned = warned || (strings.Contains(w, "While aigateway is down, MaaS does not serve") && strings.Contains(w, "proj/qwen25-05b-instruct"))
	}
	if !warned {
		t.Fatalf("warnings = %v", plan.Warnings)
	}
	if other := mustPlan(t, c, "hard-reset", "", "", false); other.Token == plan.Token {
		t.Fatal("keepEnabled is not part of the hard reset's token")
	}
	res, steps := runHardReset(t, c, func(r *types.DSCPlanRequest) { r.Token, r.KeepEnabled = plan.Token, boolPtr(true) })
	if !res.Success {
		t.Fatalf("hard reset = %+v", res)
	}
	if !strings.Contains(stepNames(steps), "create:success") || !strings.Contains(strings.Join(res.Logs, "\n"), "keeping aigateway, aigateway.modelsAsAService") {
		t.Fatalf("steps = %s, logs = %v", stepNames(steps), res.Logs)
	}
	created := comps(f.get(dscV3Path))
	for _, p := range []string{"aigateway", "aigateway.modelsAsAService"} {
		if s, _ := lookupState(created, strings.Split(p, ".")); s != "Managed" {
			t.Fatalf("%s = %s in the new DSC", p, s)
		}
	}
	// The recorded object (for Finish) was the kept one; cleared once created.
	if rec, _ := readDSCHardResetRecord(c); rec != nil {
		t.Fatalf("record left: %+v", rec)
	}
}

func TestHardResetNeedsKeepEnabled(t *testing.T) {
	f, c := maasInUseWorld(t, nil)
	res, _ := runHardReset(t, c, func(r *types.DSCPlanRequest) { r.KeepEnabled = nil })
	if res.Success || res.ErrorCode != "validation" {
		t.Fatalf("hard reset = %+v", res)
	}
	if deletes := f.callsTo(http.MethodDelete, dscV3Path); len(deletes) != 0 {
		t.Fatalf("deleted the DSC: %+v", deletes)
	}
}

// Any MaaS read that fails blocks: the tool cannot tell whether MaaS is
// in use.
func TestMaaSGuardFailsClosed(t *testing.T) {
	for name, key := range map[string]string{
		"record":         "GET " + snapshotPath(),
		"discovery":      "GET /apis/maas.opendatahub.io",
		"model refs":     "GET " + guardModelRefList,
		"subscriptions":  "GET /apis/maas.opendatahub.io/v1alpha1/namespaces/models-as-a-service/maassubscriptions",
		"access polices": "GET /apis/maas.opendatahub.io/v1alpha1/namespaces/models-as-a-service/maasauthpolicies",
	} {
		t.Run(name, func(t *testing.T) {
			f, c := managerWorld(t, nil)
			maasServed(f)
			f.fail[key] = http.StatusForbidden
			for _, tc := range []struct {
				action, path, component string
				keep                    bool
			}{
				{"toggle", "aigateway", "aigateway", false},
				{"toggle", "aigateway.modelsAsAService", "aigateway.modelsAsAService", false},
				{"soft-reset", "", "aigateway", false},
				{"hard-reset", "", "aigateway", false},
				{"hard-reset", "", "aigateway", true},
			} {
				plan := mustPlan(t, c, tc.action, tc.path, "Removed", tc.keep)
				b := maasBlock(plan, tc.component)
				if b == nil || !strings.Contains(strings.Join(b.Reasons, " "), "cannot tell whether MaaS is in use") {
					t.Errorf("%s %s keep=%v: %+v", tc.action, tc.path, tc.keep, plan)
				}
			}
			// A soft reset that keeps aigateway does not take MaaS away:
			// nothing to read, nothing blocked.
			if plan := mustPlan(t, c, "soft-reset", "", "", true); len(plan.Blocked) != 0 {
				t.Errorf("soft reset with keep: %+v", plan.Blocked)
			}
			assertNoDSCPatch(t, f)
		})
	}
}

// Without MaaS in use (the API served, nothing in it, no record) the guard
// stays out of the way.
func TestMaaSGuardAllowsWhenMaaSIsNotInUse(t *testing.T) {
	f, c := managerWorld(t, nil)
	maasServed(f)
	putMaaSRecord(t, f, maasRecord{Shared: map[string]string{"rhcl": "installed"}}) // shared pieces only: kept by Clean up MaaS
	for _, path := range []string{"aigateway", "aigateway.modelsAsAService"} {
		if plan := mustPlan(t, c, "toggle", path, "Removed", false); len(plan.Blocked) != 0 {
			t.Fatalf("%s: %+v", path, plan.Blocked)
		}
	}
	if plan := mustPlan(t, c, "soft-reset", "", "", false); len(plan.Blocked) != 0 {
		t.Fatalf("soft reset: %+v", plan.Blocked)
	}
	plan := mustPlan(t, c, "hard-reset", "", "", true)
	if len(plan.Blocked) != 0 {
		t.Fatalf("hard reset: %+v", plan.Blocked)
	}
	for _, w := range plan.Warnings {
		if strings.Contains(w, "MaaS") {
			t.Fatalf("warned about MaaS not in use: %s", w)
		}
	}
	res, err := reviewedToggle(t, c, types.DSCToggleRequest{Name: "default-dsc", Path: "aigateway", From: "Managed", To: "Removed"})
	if err != nil || !res.Success {
		t.Fatalf("toggle = %+v, %v", res, err)
	}
}

// Each signal alone is in use: a MaaSModelRef someone else made (being
// deleted: maas-controller must still remove its finalizer), and the
// record alone (a setup with nothing published).
func TestMaaSGuardSignalsAlone(t *testing.T) {
	t.Run("model ref being deleted", func(t *testing.T) {
		f, c := managerWorld(t, nil)
		maasServed(f)
		f.put("/apis/maas.opendatahub.io/v1alpha1/namespaces/team/maasmodelrefs/their-model", map[string]interface{}{
			"metadata": map[string]interface{}{"name": "their-model", "namespace": "team", "deletionTimestamp": "2026-11-19T10:00:00Z", "finalizers": []interface{}{maasModelFinalizer}}})
		plan := mustPlan(t, c, "toggle", "aigateway", "Removed", false)
		b := maasBlock(plan, "aigateway")
		if b == nil || !strings.Contains(b.Reasons[len(b.Reasons)-1], "published model team/their-model (being deleted)") {
			t.Fatalf("plan = %+v", plan)
		}
	})
	t.Run("record only", func(t *testing.T) {
		f, c := managerWorld(t, nil)
		putMaaSRecord(t, f, maasRecord{DSCChanged: true, PreviousModelsAsAService: "Removed"})
		plan := mustPlan(t, c, "toggle", "aigateway.modelsAsAService", "Removed", false)
		b := maasBlock(plan, "aigateway.modelsAsAService")
		if b == nil || !strings.Contains(b.Reasons[0], "MaaS is in use (this tool set MaaS up (MaaS tab))") {
			t.Fatalf("plan = %+v", plan)
		}
	})
}

// The guard is about aigateway and modelsAsAService only: with MaaS off
// but the tool's setup recorded, removing aigateway is still blocked
// (Clean up MaaS removes the rest in order), while other parts of
// aigateway and other components are not.
func TestMaaSGuardPaths(t *testing.T) {
	f, c := managerWorld(t, func(dsc map[string]interface{}) {
		setPath(t, dsc, "spec.components.aigateway.modelsAsAService.managementState", "Removed")
		setPath(t, dsc, "spec.components.aigateway.batchGateway.managementState", "Managed")
	})
	putMaaSRecord(t, f, maasRecord{DBNamespace: "redhat-ai-gateway-infra", Database: "cnpg"})
	if b := maasBlock(mustPlan(t, c, "toggle", "aigateway", "Removed", false), "aigateway"); b == nil || !strings.Contains(b.Reasons[len(b.Reasons)-1], "MaaS is in use") {
		t.Fatalf("aigateway: %+v", b)
	}
	for _, path := range []string{"aigateway.batchGateway", "ray"} {
		plan := mustPlan(t, c, "toggle", path, "Removed", false)
		for _, b := range plan.Blocked {
			if strings.Contains(strings.Join(b.Reasons, " "), "MaaS") {
				t.Fatalf("%s got the MaaS guard: %+v", path, b)
			}
		}
	}
}
