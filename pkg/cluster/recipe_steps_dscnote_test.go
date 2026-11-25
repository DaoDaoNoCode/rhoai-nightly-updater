package cluster

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/recipe"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The live 3.7.0-ea.1 Trainer CR on a cluster without the JobSet Operator.
const msgTrainerJobSetLive = "dependency not met: JobSet Operator is not installed. Please install the JobSet Operator via OLM (OperatorHub) before deploying Trainer."

// dscNoteAdvice follows the list of components that are not ready.
const dscNoteAdvice = " A component that is still starting becomes ready on its own; one that reports a missing prerequisite stays not ready until an administrator installs it (the Components page shows the cause, Diagnostics the commands)."

// serveComponents serves components.platform.opendatahub.io with the given
// kinds (plural to Kind) and their CRs.
func serveComponents(f *resourceFake, kinds map[string]string, crs map[string]string) {
	if f.groups == nil {
		f.groups = map[string]string{}
	}
	f.groups["/apis/components.platform.opendatahub.io"] = `{"kind":"APIGroup","name":"components.platform.opendatahub.io","preferredVersion":{"groupVersion":"components.platform.opendatahub.io/v1alpha1","version":"v1alpha1"}}`
	var res []string
	for _, plural := range slices.Sorted(maps.Keys(kinds)) {
		kind := kinds[plural]
		res = append(res, fmt.Sprintf(`{"name":%q,"kind":%q},{"name":"%s/status","kind":%q}`, plural, kind, plural, kind))
	}
	f.groups["/apis/components.platform.opendatahub.io/v1alpha1"] = `{"resources":[` + strings.Join(res, ",") + `]}`
	for path, body := range crs {
		f.putJSON(componentV1alpha1+"/"+path, body)
	}
}

func componentCR(generation, observed int, conds ...string) string {
	return fmt.Sprintf(`{"metadata":{"generation":%d},"status":{"observedGeneration":%d,"conditions":[%s]}}`, generation, observed, strings.Join(conds, ","))
}

func compCond(typ, status, reason, msg string) string {
	return fmt.Sprintf(`{"type":%q,"status":%q,"reason":%q,"message":%q}`, typ, status, reason, msg)
}

func runDSCStep(t *testing.T, c *Client) *types.OperationResponse {
	t.Helper()
	r := tRecipe(t, "create")
	res, err := recipeStep(t, r, recipe.StepDSC).Run(c, &recipe.State{Recipe: r}, func(types.UpdateStepEvent) {})
	if err != nil || res == nil {
		t.Fatalf("%+v %v", res, err)
	}
	return res
}

// After the create, the step waits for the components' own conditions and
// lists the components that are not ready, with their condition exactly as
// RHOAI reports it: those the operator could not provision first. It
// succeeds: the note is not a failure.
func TestRecipeDSCStep_ListsComponentsThatAreNotReady(t *testing.T) {
	defer recipe.SetDSCTimingForTest(time.Minute, time.Second, time.Minute, time.Second)()
	f, c := dscStepFake(t, 0, 1)
	serveComponents(f, map[string]string{"dashboards": "Dashboard", "trainers": "Trainer", "rays": "Ray"}, map[string]string{
		"dashboards/default-dashboard": componentCR(1, 1,
			compCond("Ready", "False", "DeploymentsNotReady", "0/1 deployments ready"), compCond("ProvisioningSucceeded", "True", "", "")),
		"trainers/default-trainer": componentCR(1, 1,
			compCond("Ready", "False", "Error", msgTrainerJobSetLive), compCond("ProvisioningSucceeded", "False", "Error", msgTrainerJobSetLive), compCond("Degraded", "True", "Error", msgTrainerJobSetLive)),
		"rays/default-ray": componentCR(2, 2, compCond("Ready", "True", "", "")),
	})
	res := runDSCStep(t, c)
	if !res.Success {
		t.Fatalf("%+v", res)
	}
	trainer := "Trainer default-trainer: ProvisioningSucceeded=False (Error): " + msgTrainerJobSetLive
	dashboard := "Dashboard default-dashboard: Ready=False (DeploymentsNotReady): 0/1 deployments ready"
	want := "DataScienceCluster created with default components. Not ready when checked, as RHOAI reports it: " + trainer + " " + dashboard + "." + dscNoteAdvice
	if res.Message != want {
		t.Fatalf("message\n got %q\nwant %q", res.Message, want)
	}
	if logs := strings.Join(res.Logs, "\n"); !strings.Contains(logs, trainer) || !strings.Contains(logs, dashboard) || strings.Contains(logs, "Ray default-ray") {
		t.Fatalf("logs %s", logs)
	}
	if n := len(f.mutations()); n != 1 {
		t.Fatalf("writes %v", f.mutations())
	}
}

// A component whose status is stale, or that has no Ready condition yet,
// has not reported: the step waits for it, within the same bound as the
// DataScienceCluster's conditions, and says so when it never does.
func TestRecipeDSCStep_ComponentsNotReported(t *testing.T) {
	defer recipe.SetDSCTimingForTest(time.Minute, time.Second, time.Minute, time.Second)()

	t.Run("reported later", func(t *testing.T) {
		f, c := dscStepFake(t, 0, 1)
		serveComponents(f, map[string]string{"rays": "Ray"}, map[string]string{"rays/default-ray": componentCR(2, 1)})
		reads := 0
		prev := f.onGet
		f.listOnGet["rays"] = true
		f.onGet = func(k fakeKey, obj map[string]any) {
			prev(k, obj)
			if k.plural != "rays" {
				return
			}
			if reads++; reads >= 3 {
				obj["status"] = map[string]any{"observedGeneration": 2, "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
			}
		}
		res := runDSCStep(t, c)
		if !res.Success || res.Message != "DataScienceCluster created with default components. All 1 components report Ready." || reads != 3 {
			t.Fatalf("%+v after %d reads", res, reads)
		}
	})

	t.Run("never", func(t *testing.T) {
		f, c := dscStepFake(t, 0, 1)
		serveComponents(f, map[string]string{"rays": "Ray", "trainers": "Trainer"}, map[string]string{
			"rays/default-ray":         componentCR(2, 1, compCond("Ready", "True", "", "")),
			"trainers/default-trainer": componentCR(1, 0, compCond("ProvisioningSucceeded", "True", "", "")),
		})
		res := runDSCStep(t, c)
		want := "DataScienceCluster created with default components. Not reported yet within 1m of the create: Ray default-ray, Trainer default-trainer."
		if !res.Success || res.Message != want {
			t.Fatalf("message\n got %q\nwant %q", res.Message, want)
		}
	})

	t.Run("no component CR", func(t *testing.T) {
		f, c := dscStepFake(t, 0, 1)
		serveComponents(f, map[string]string{"rays": "Ray"}, nil)
		res := runDSCStep(t, c)
		if !res.Success || !strings.HasSuffix(res.Message, "No component reported its own conditions within 1m of the create; the Components page shows each one as RHOAI reports it.") {
			t.Fatalf("%+v", res)
		}
	})

	t.Run("group not served", func(t *testing.T) {
		_, c := dscStepFake(t, 0, 1)
		res := runDSCStep(t, c)
		if !res.Success || !strings.Contains(res.Message, "serves no component API (components.platform.opendatahub.io)") {
			t.Fatalf("%+v", res)
		}
	})

	t.Run("read error", func(t *testing.T) {
		f, c := dscStepFake(t, 0, 1)
		serveComponents(f, map[string]string{"rays": "Ray"}, nil)
		f.fail["GET "+componentV1alpha1+"/rays"] = 403
		res := runDSCStep(t, c)
		if !res.Success || !strings.Contains(res.Message, "Could not read the components' own conditions: list rays:") {
			t.Fatalf("%+v", res)
		}
	})
}

// The run record keeps 1024 bytes of a step's message: past the note's
// budget, components are counted; every one is in the logs.
func TestRecipeDSCStep_LongListFitsTheRunRecord(t *testing.T) {
	defer recipe.SetDSCTimingForTest(time.Minute, time.Second, time.Minute, time.Second)()
	f, c := dscStepFake(t, 0, 1)
	kinds := map[string]string{}
	crs := map[string]string{}
	for i := range 20 {
		plural := fmt.Sprintf("parts%02d", i)
		kinds[plural] = fmt.Sprintf("Part%02d", i)
		crs[plural+"/default"] = componentCR(1, 1, compCond("Ready", "False", "DeploymentsNotReady", strings.Repeat("x", 80)))
	}
	crs["parts07/default"] = componentCR(1, 1, compCond("ProvisioningSucceeded", "False", "Error", msgTrainerJobSetLive))
	serveComponents(f, kinds, crs)
	res := runDSCStep(t, c)
	if !res.Success || len(res.Message) > 1024 {
		t.Fatalf("%d bytes: %s", len(res.Message), res.Message)
	}
	if !strings.Contains(res.Message, "as RHOAI reports it: Part07 default: ProvisioningSucceeded=False (Error): "+msgTrainerJobSetLive) ||
		!strings.Contains(res.Message, "more not ready.") || !strings.HasSuffix(res.Message, dscNoteAdvice) {
		t.Fatalf("%s", res.Message)
	}
	if n := strings.Count(strings.Join(res.Logs, "\n"), "=False"); n != 20 {
		t.Fatalf("%d conditions in the logs", n)
	}
}

func compCondGen(typ, status, reason, msg string, gen int) string {
	return fmt.Sprintf(`{"type":%q,"status":%q,"reason":%q,"message":%q,"observedGeneration":%d}`, typ, status, reason, msg, gen)
}

// Provisioning that is not True is not ready even when Ready is True;
// False is still listed first.
func TestRecipeDSCStep_ProvisioningUnknownIsNotReady(t *testing.T) {
	defer recipe.SetDSCTimingForTest(time.Minute, time.Second, time.Minute, time.Second)()
	f, c := dscStepFake(t, 0, 1)
	serveComponents(f, map[string]string{"rays": "Ray", "trainers": "Trainer", "dashboards": "Dashboard"}, map[string]string{
		"dashboards/default-dashboard": componentCR(1, 1, compCond("Ready", "True", "", ""), compCond("ProvisioningSucceeded", "True", "", "")),
		"rays/default-ray":             componentCR(1, 1, compCond("Ready", "True", "", ""), compCond("ProvisioningSucceeded", "Unknown", "Reconciling", "provisioning in progress")),
		"trainers/default-trainer":     componentCR(1, 1, compCond("Ready", "False", "Error", msgTrainerJobSetLive), compCond("ProvisioningSucceeded", "False", "Error", msgTrainerJobSetLive)),
	})
	res := runDSCStep(t, c)
	want := "DataScienceCluster created with default components. Not ready when checked, as RHOAI reports it: Trainer default-trainer: ProvisioningSucceeded=False (Error): " +
		msgTrainerJobSetLive + " Ray default-ray: ProvisioningSucceeded=Unknown (Reconciling): provisioning in progress." + dscNoteAdvice
	if !res.Success || res.Message != want {
		t.Fatalf("message\n got %q\nwant %q", res.Message, want)
	}
}

// A status observedGeneration of 0 (or none) proves nothing: the
// controllers' framework sets it only once the component is ready. The
// ProvisioningSucceeded condition's own observedGeneration does.
func TestRecipeDSCStep_ObservedGenerationMustProveTheReport(t *testing.T) {
	defer recipe.SetDSCTimingForTest(time.Minute, time.Second, time.Minute, time.Second)()
	f, c := dscStepFake(t, 0, 1)
	serveComponents(f, map[string]string{"rays": "Ray", "trainers": "Trainer", "dashboards": "Dashboard"}, map[string]string{
		// Ready with no generation anywhere: not proven.
		"rays/default-ray": `{"metadata":{"generation":1},"status":{"conditions":[` + compCond("Ready", "True", "", "") + `]}}`,
		// observedGeneration 0, as the framework leaves it until ready:
		// not proven either.
		"dashboards/default-dashboard": componentCR(1, 0, compCond("Ready", "False", "DeploymentsNotReady", "0/1 deployments ready")),
		// The live trainer: status observedGeneration 0, its
		// ProvisioningSucceeded condition set for generation 1.
		"trainers/default-trainer": componentCR(1, 0, compCond("Ready", "False", "Error", msgTrainerJobSetLive), compCondGen("ProvisioningSucceeded", "False", "Error", msgTrainerJobSetLive, 1)),
	})
	res := runDSCStep(t, c)
	want := "DataScienceCluster created with default components. Not ready when checked, as RHOAI reports it: Trainer default-trainer: ProvisioningSucceeded=False (Error): " +
		msgTrainerJobSetLive + dscNoteAdvice + " Not reported yet within 1m of the create: Dashboard default-dashboard, Ray default-ray."
	if !res.Success || res.Message != want {
		t.Fatalf("message\n got %q\nwant %q", res.Message, want)
	}
}

// One condition longer than the whole record: it is shortened so the
// count of the others, the advice and the components not reported yet
// still fit in 1024 bytes; the logs keep it in full.
func TestRecipeDSCStep_OversizedConditionIsShortened(t *testing.T) {
	defer recipe.SetDSCTimingForTest(time.Minute, time.Second, time.Minute, time.Second)()
	f, c := dscStepFake(t, 0, 1)
	huge := strings.Repeat("é", 3000) // two bytes each: the cut must not split one
	serveComponents(f, map[string]string{"trainers": "Trainer", "rays": "Ray", "dashboards": "Dashboard", "kserves": "Kserve", "workbenches": "Workbenches"}, map[string]string{
		"trainers/default-trainer":        componentCR(1, 1, compCond("ProvisioningSucceeded", "False", "Error", huge)),
		"rays/default-ray":                componentCR(1, 1, compCond("Ready", "False", "DeploymentsNotReady", "0/1 deployments ready")),
		"dashboards/default-dashboard":    componentCR(1, 1, compCond("Ready", "False", "DeploymentsNotReady", "0/2 deployments ready")),
		"kserves/default-kserve":          componentCR(2, 1, compCond("Ready", "True", "", "")),
		"workbenches/default-workbenches": componentCR(3, 1),
	})
	res := runDSCStep(t, c)
	if !res.Success || len(res.Message) > 1024 || !utf8.ValidString(res.Message) {
		t.Fatalf("%d bytes, valid UTF-8 %v: %s", len(res.Message), utf8.ValidString(res.Message), res.Message)
	}
	for _, want := range []string{
		"as RHOAI reports it: Trainer default-trainer: ProvisioningSucceeded=False (Error): éé",
		"é [shortened; the Components page shows it in full]; and 2 more not ready." + dscNoteAdvice,
		" Not reported yet within 1m of the create: Kserve default-kserve, Workbenches default-workbenches.",
	} {
		if !strings.Contains(res.Message, want) {
			t.Fatalf("lacks %q:\n%s", want, res.Message)
		}
	}
	if !strings.Contains(strings.Join(res.Logs, "\n"), "Trainer default-trainer: ProvisioningSucceeded=False (Error): "+huge) {
		t.Fatal("the logs lack the full condition")
	}
}
