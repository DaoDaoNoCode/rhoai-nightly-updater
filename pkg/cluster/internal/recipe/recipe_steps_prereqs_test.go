package recipe

import (
	"fmt"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// One step sets up what every feature of the recipe needs, right after
// the project: Models for the recipe's exposure, Kueue with its operators
// only (kueue-prereq turns its component on, late). Without an exposure,
// Models' set is what every way needs, and models-prereq stays for what
// the resolved one needs.
func TestPrerequisitesStep_OneSetAfterTheProject(t *testing.T) {
	items := func(r types.EnvironmentRecipe) string {
		var out []string
		for _, it := range prereqItems(r) {
			out = append(out, fmt.Sprintf("%s/%s/%v", it.Feature, it.Exposure, it.OperatorsOnly))
		}
		return strings.Join(out, " ")
	}
	ids := func(r types.EnvironmentRecipe) string {
		var out []string
		for _, s := range Steps(r, false) {
			out = append(out, s.ID)
		}
		return strings.Join(out, " ")
	}
	all := validRecipe(t, allFeatures)
	if got := items(all); got != "pipelines//false mlflow//false models//false playground//false model-registry//false" {
		t.Errorf("items %s", got)
	}
	if got := ids(all); !strings.HasPrefix(got, "rhoai dsc project prerequisites s3 ") || !strings.Contains(got, " maas models-prereq model ") || strings.Contains(got, "pipelines-prereq") {
		t.Errorf("steps %s", got)
	}
	s := stepOf(t, all, StepModelsPrereq)
	if s.Title != "What the model's access needs" {
		t.Errorf("title %q", s.Title)
	}
	gw := validRecipe(t, "  project: {mode: existing, name: proj}\n  features:\n    models: {enabled: true, exposure: default-gateway}\n    kueue: {enabled: true}\n")
	if got := items(gw); got != "models/default-gateway/false kueue//true" {
		t.Errorf("items %s", got)
	}
	if got := ids(gw); got != "rhoai dsc project prerequisites model kueue-prereq kueue" {
		t.Errorf("steps %s", got)
	}
	for _, st := range Steps(gw, false) {
		if st.ID == StepPrerequisites && (st.Title != "What the features need" || st.Typical.Minutes() != 5 || st.Max.Minutes() != 15) {
			t.Errorf("step %+v", st)
		}
	}
	if problem := features.ValidatePrerequisitesSet(prereqItems(all)); problem != "" {
		t.Error(problem)
	}
}

// Its check reads the set as its run plans it: every feature ready is
// done; one feature's refusal blocks, named; else it runs. Its run sets
// the set up in one call and claims what that turned on.
func TestPrerequisitesStep_CheckAndRun(t *testing.T) {
	r := validRecipe(t, "  project: {mode: existing, name: proj}\n  features:\n    pipelines: {enabled: true}\n    mlflow: {enabled: true}\n    kueue: {enabled: true}\n")
	var step Step
	for _, s := range Steps(r, false) {
		if s.ID == StepPrerequisites {
			step = s
		}
	}
	fc := &fakeCluster{prereqReady: map[string]bool{"pipelines": true, "kueue": true}}
	fc.install(t)
	s := &State{Recipe: r}
	if chk, err := step.Check(nil, s); err != nil || chk.Done || chk.Blocked != "" {
		t.Fatalf("mlflow missing: %+v %v", chk, err)
	}
	fc.prereqBlocked = map[string]string{"mlflow": "The DataScienceCluster is managed by Argo CD."}
	if chk, _ := step.Check(nil, s); chk.Blocked != "MLflow: The DataScienceCluster is managed by Argo CD." || chk.Fix == nil || chk.Fix.Page != pageEnvironment {
		t.Fatalf("blocked: %+v", chk)
	}
	fc.prereqBlocked = nil
	res, err := step.Run(testClient(), s, func(types.UpdateStepEvent) {})
	if err != nil || !res.Success || strings.Join(fc.ran, " ") != "prerequisites" {
		t.Fatalf("run %+v %v, ran %v", res, err, fc.ran)
	}
	last := fc.setItems[len(fc.setItems)-1]
	if len(last) != 3 || last[2] != (features.SetItem{Feature: "kueue", OperatorsOnly: true}) {
		t.Errorf("set %+v", last)
	}
	if chk, err := step.Check(nil, s); err != nil || !chk.Done || chk.Reason != "Everything the features need is there." {
		t.Errorf("after: %+v %v", chk, err)
	}
}
