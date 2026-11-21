package cluster

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// changeList words a plan's changes as "path:from>to", "-" for unset.
func changeList(plan *types.DSCPlan) []string {
	var out []string
	for _, ch := range plan.Changes {
		from, to := kube.NonEmpty(ch.From, "-"), kube.NonEmpty(ch.To, "-")
		if !ch.FromSet {
			from = "-"
		}
		if !ch.ToSet {
			to = "-"
		}
		out = append(out, ch.Path+":"+from+">"+to)
	}
	return out
}

// The live 3.6 DSC (testdata/dsc/dsc-3.6.json) holds six values the API
// server defaulted from the CRD (no field manager owns them):
// kserve.nim.airGapped, kserve.rawDeploymentServiceConfig,
// kueue.autoCreateQueues, kueue.defaultClusterQueueName,
// kueue.defaultLocalQueueName and trustyai.mcpGuardrailsMode. The release
// default leaves them out, and a recreated DSC gets them again: no change.
var apiServerDefaulted = []string{"kserve.nim.airGapped", "kserve.rawDeploymentServiceConfig", "kueue.autoCreateQueues",
	"kueue.defaultClusterQueueName", "kueue.defaultLocalQueueName", "trustyai.mcpGuardrailsMode"}

func TestHardResetListsNoChangeForValuesTheAPIServerDefaults(t *testing.T) {
	_, c := managerWorld(t, nil)
	// Keeping what the user turned on (aigateway, MaaS), nothing changes.
	plan := mustPlan(t, c, "hard-reset", "", "", true)
	if got := changeList(plan); len(got) != 0 {
		t.Fatalf("changes = %v, want none", got)
	}
	// Without it, only the toggles the release default turns off.
	plan = mustPlan(t, c, "hard-reset", "", "", false)
	if got, want := changeList(plan), []string{"aigateway:Managed>Removed", "aigateway.modelsAsAService:Managed>Removed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	// The object it creates is shown as the API server stores it.
	for _, line := range []string{"airGapped: false", "rawDeploymentServiceConfig: Headless", "autoCreateQueues: false",
		"defaultClusterQueueName: default", "defaultLocalQueueName: default", "mcpGuardrailsMode: false"} {
		if !strings.Contains(plan.NewObject, line) {
			t.Errorf("new object lacks %q:\n%s", line, plan.NewObject)
		}
	}
	// A CRD default that holds no object is not made up (trustyai.eval has
	// none, so its lmeval defaults do not apply).
	if strings.Contains(plan.NewObject, "permitCodeExecution") {
		t.Errorf("new object has defaults below an object the CRD does not default:\n%s", plan.NewObject)
	}
}

func TestHardResetListsValuesThatDifferFromTheCRDDefault(t *testing.T) {
	_, c := managerWorld(t, func(dsc map[string]any) {
		setPath(t, dsc, "spec.components.kueue.defaultLocalQueueName", "team")
		setPath(t, dsc, "spec.components.kserve.rawDeploymentServiceConfig", "Headed")
	})
	plan := mustPlan(t, c, "hard-reset", "", "", true)
	if got, want := changeList(plan), []string{"kserve.rawDeploymentServiceConfig:Headed>Headless", "kueue.defaultLocalQueueName:team>default"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
}

// The soft reset neither sends nor lists the defaulted values: removing
// them would change nothing, as the API server puts them back.
func TestSoftResetLeavesValuesTheAPIServerDefaults(t *testing.T) {
	f, c := managerWorld(t, nil)
	plan := mustPlan(t, c, "soft-reset", "", "", true)
	if plan.Code != "nothing_to_do" {
		t.Fatalf("plan = %+v (changes %v)", plan, changeList(plan))
	}
	plan = mustPlan(t, c, "soft-reset", "", "", false)
	for _, ch := range plan.Changes {
		if slices.Contains(apiServerDefaulted, ch.Path) {
			t.Fatalf("listed %s: %v", ch.Path, changeList(plan))
		}
	}
	res, err := SoftResetDSC(c, types.DSCPlanRequest{Name: "default-dsc", UID: plan.UID, Token: plan.Token, KeepEnabled: new(false)})
	if err != nil || !res.Success {
		t.Fatalf("reset = %+v, %v", res, err)
	}
	for _, p := range append(patchesDry(f), patchesReal(f)...) {
		for _, field := range []string{"airGapped", "rawDeploymentServiceConfig", "autoCreateQueues", "QueueName", "mcpGuardrailsMode", `"/spec/components/kserve/nim"`} {
			if strings.Contains(p.Body, field) {
				t.Fatalf("sent %s: %s", field, p.Body)
			}
		}
	}
}

func patchesDry(f *resourceFake) []fakeCall  { d, _ := patchesTo(f); return d }
func patchesReal(f *resourceFake) []fakeCall { _, r := patchesTo(f); return r }

// A removal is skipped only when the API server puts the same value back:
// a field (or object) with a schema default; an object without one is
// not put back, so removing it is a change.
func TestDefaultedBack(t *testing.T) {
	nim := map[string]any{"default": map[string]any{}, "properties": map[string]any{
		"managementState": map[string]any{"type": "string", "default": "Managed"},
		"airGapped":       map[string]any{"type": "boolean", "default": false},
	}}
	eval := map[string]any{"properties": map[string]any{"permit": map[string]any{"type": "string", "default": "deny"}}}
	for _, tc := range []struct {
		name   string
		schema map[string]any
		value  any
		want   bool
	}{
		{"leaf equal to its default", map[string]any{"default": "Headless"}, "Headless", true},
		{"leaf that differs", map[string]any{"default": "Headless"}, "Headed", false},
		{"leaf without a default", map[string]any{"type": "string"}, "default", false},
		{"object default, filled in below", nim, map[string]any{"managementState": "Managed", "airGapped": false}, true},
		{"object default, a value differs", nim, map[string]any{"managementState": "Removed", "airGapped": false}, false},
		{"object without a default", eval, map[string]any{"permit": "deny"}, false},
	} {
		if got := defaultedBack(tc.schema, tc.value); got != tc.want {
			t.Errorf("%s: defaultedBack = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestApplySchemaDefaults(t *testing.T) {
	schema := map[string]any{"properties": map[string]any{
		"present": map[string]any{"properties": map[string]any{"a": map[string]any{"default": "x"}}},
		"absent":  map[string]any{"properties": map[string]any{"a": map[string]any{"default": "x"}}},
		"made":    map[string]any{"default": map[string]any{}, "properties": map[string]any{"a": map[string]any{"default": "x"}}},
		"null":    map[string]any{"default": "d"},
		"nullOK":  map[string]any{"default": "d", "nullable": true},
		"list": map[string]any{"items": map[string]any{"properties": map[string]any{
			"b": map[string]any{"default": 1},
		}}},
		"byName": map[string]any{"additionalProperties": map[string]any{"properties": map[string]any{
			"c": map[string]any{"default": true},
		}}},
	}}
	obj := map[string]any{"present": map[string]any{}, "null": nil, "nullOK": nil,
		"list": []any{map[string]any{}, map[string]any{"b": 2}}, "byName": map[string]any{"one": map[string]any{}}}
	applySchemaDefaults(obj, schema)
	want := map[string]any{"present": map[string]any{"a": "x"}, "made": map[string]any{"a": "x"}, "null": "d", "nullOK": nil,
		"list": []any{map[string]any{"b": float64(1)}, map[string]any{"b": 2}}, "byName": map[string]any{"one": map[string]any{"c": true}}}
	if !kube.JSONEqual(obj, want) {
		t.Fatalf("defaulted = %v\nwant      %v", obj, want)
	}
	if _, ok := obj["absent"]; ok {
		t.Fatal("an object without a default was made")
	}
}

// A13: a null additionalProperties value whose schema is not nullable gets
// that schema's default, then the defaults below it (kubernetes v1.34
// apiextensions-apiserver pkg/apiserver/schema/defaulting/algorithm.go:53-55);
// a nullable one, or one without a default, stays null.
func TestApplySchemaDefaultsNullAdditionalProperties(t *testing.T) {
	entry := func(extra map[string]any) map[string]any {
		s := map[string]any{"default": map[string]any{"a": 1}, "properties": map[string]any{"b": map[string]any{"default": "x"}}}
		for k, v := range extra {
			s[k] = v
		}
		return s
	}
	for _, tc := range []struct {
		name   string
		schema map[string]any
		want   any
	}{
		{"not nullable", entry(nil), map[string]any{"a": float64(1), "b": "x"}},
		{"nullable", entry(map[string]any{"nullable": true}), nil},
		{"no default", map[string]any{"properties": map[string]any{"b": map[string]any{"default": "x"}}}, nil},
	} {
		obj := map[string]any{"byName": map[string]any{"one": nil}}
		applySchemaDefaults(obj, map[string]any{"properties": map[string]any{"byName": map[string]any{"additionalProperties": tc.schema}}})
		if got := obj["byName"].(map[string]any)["one"]; !kube.JSONEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The plan carries what the dialog says: the wait before the reset stops
// without creating anything, and the fields the reset keeps.
func TestResetPlansCarryTheTimeoutAndTheProtectedFields(t *testing.T) {
	_, c := managerWorld(t, func(dsc map[string]any) {
		setPath(t, dsc, "spec.components.workbenches.workbenchNamespace", "my-notebooks")
	})
	hard := mustPlan(t, c, "hard-reset", "", "", true)
	if hard.DeleteTimeoutSeconds != int(HardResetDeleteTimeout.Seconds()) || hard.DeleteTimeoutSeconds == 0 {
		t.Fatalf("deleteTimeoutSeconds = %d", hard.DeleteTimeoutSeconds)
	}
	want := []string{"aiHub.instancesNamespace", "workbenches.workbenchNamespace"}
	soft := mustPlan(t, c, "soft-reset", "", "", false)
	if !reflect.DeepEqual(hard.ProtectedFields, want) || !reflect.DeepEqual(soft.ProtectedFields, want) {
		t.Fatalf("protected: hard %v, soft %v", hard.ProtectedFields, soft.ProtectedFields)
	}
}

// What stops working while the components are down is listed once per
// resource, with every component it needs: a playground server needs ogx
// and the dashboard (its gen-ai API).
func TestHardResetListsWhatStopsWorkingOncePerResource(t *testing.T) {
	f, c := managerWorld(t, nil)
	f.putJSON("/apis/ogx.io/v1beta1/namespaces/team/ogxservers/theirs", `{"metadata":{"name":"theirs","namespace":"team"}}`)
	plan := mustPlan(t, c, "hard-reset", "", "", true)
	want := []types.DSCStopsWorking{{Resource: "playground server team/theirs", Components: []string{"dashboard", "ogx"}}}
	if len(plan.Blocked) != 0 || !reflect.DeepEqual(plan.StopsWorking, want) {
		t.Fatalf("blocked = %+v, stops working = %+v", plan.Blocked, plan.StopsWorking)
	}
	if len(plan.Warnings) != 0 {
		t.Fatalf("warnings = %v", plan.Warnings)
	}
	if calls := f.callsTo(http.MethodPatch, dscV3Path); len(calls) != 0 {
		t.Fatalf("a plan patched the DSC: %+v", calls)
	}
}

// The shared blocker for a module CR whose operator the tool does not
// know: short, and says what to do.
func TestUnknownOperatorBlockerIsPlain(t *testing.T) {
	got := unknownOperatorBlocker()
	if got != "can't confirm its operator is running to clean up, so the tool won't remove it. Check in the OpenShift console, then change it there" {
		t.Fatalf("blocker = %q", got)
	}
}
