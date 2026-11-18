package cluster

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Add and Regenerate of the GenAI demo (see samples_demo.go for the
// generator facts). Both use only MLflow's public calls and named rights:
//
//  1. get-by-name "MLflow Demo" (deleted experiments included);
//  2. a deleted experiment is restored (experiments/restore: experiments
//     update, named; rules_base.py:331-334);
//  3. every "mlflow.demo.version.<feature>" tag is set to "0"
//     (set-experiment-tag: experiments update, named; rules_base.py:
//     342-345). The keys are read from the experiment, never listed in the
//     code, so a release with other features needs no change. The
//     experiment must be active for this (sqlalchemy_store.py:2152-2168),
//     which is why it is restored first;
//  4. demo/generate. Every feature now fails is_generated() on the version
//     (base.py:107-110): the generator's own delete_demo() removes what is
//     left of it, and the feature is generated again (demo/__init__.py:
//     37-46). That repairs what MLflow's own removal leaves behind
//     (evaluation runs without datasets, REJECTED issues).
//
// Add resets only when the demo needs it (restored, or partial); on a
// complete demo it only calls generate, which answers "exists" and changes
// nothing (handlers.py:7986-7996). Regenerate always resets: a full
// refresh. Leftovers MLflow cannot purge over REST stay hidden: soft-deleted
// runs and REJECTED issues (only `mlflow gc` removes them).

// demoGenerateResult is the answer of demo/generate (handlers.py:7990-8012).
type demoGenerateResult struct {
	Status            string   `json:"status"`
	ExperimentID      string   `json:"experiment_id"`
	FeaturesGenerated []string `json:"features_generated"`
}

func (t *mlflowTarget) restoreExperiment(e *mlflowExperiment, log func(string)) error {
	if err := t.api.sendJSON("POST", "/api/2.0/mlflow/experiments/restore", nil, map[string]interface{}{"experiment_id": e.ExperimentID}, nil); err != nil {
		return err
	}
	e.LifecycleStage = "active"
	log("Restored experiment '" + e.Name + "'")
	return nil
}

// resetDemoVersions sets every feature version tag of the (active) demo
// experiment to "0".
func (t *mlflowTarget) resetDemoVersions(e *mlflowExperiment, log func(string)) error {
	tags := demoVersionTags(e)
	features := make([]string, 0, len(tags))
	for f := range tags {
		features = append(features, f)
	}
	sort.Strings(features)
	for _, f := range features {
		if err := t.api.sendJSON("POST", "/api/2.0/mlflow/experiments/set-experiment-tag", nil, map[string]interface{}{
			"experiment_id": e.ExperimentID, "key": demoVersionTagPrefix + f, "value": "0",
		}, nil); err != nil {
			return err
		}
	}
	if len(features) > 0 {
		log("Marked " + strings.Join(features, ", ") + " for regeneration")
	}
	return nil
}

// errNoDemoGenerator: this MLflow has no demo/generate route.
var errNoDemoGenerator = errors.New("this MLflow has no demo generator (demo/generate is not available)")

func (t *mlflowTarget) generateDemo(log func(string)) (demoGenerateResult, string, error) {
	log("Generating the demo (this takes about 30 seconds)...")
	var out demoGenerateResult
	if err := t.api.sendJSON("POST", "/ajax-api/3.0/mlflow/demo/generate", nil, map[string]interface{}{}, &out); err != nil {
		if isEndpointMissing(err) {
			return out, "", errNoDemoGenerator
		}
		return out, "", err
	}
	if out.Status == "exists" || len(out.FeaturesGenerated) == 0 {
		return out, "already complete", nil
	}
	return out, "generated " + strings.Join(out.FeaturesGenerated, ", "), nil
}

// createDemoExperiment creates the demo's experiment with the tool's
// marker, before the generator runs. When someone created one of that name
// in the meantime, it is used only if it carries the marker.
func (t *mlflowTarget) createDemoExperiment(log func(string)) (*mlflowExperiment, error) {
	tags := []mlflowTag{{Key: sampleMarkerKey, Value: SampleGenAIDemo}}
	var created struct {
		ExperimentID string `json:"experiment_id"`
	}
	err := t.api.sendJSON("POST", "/api/2.0/mlflow/experiments/create", nil, map[string]interface{}{"name": demoExperiment, "tags": tags}, &created)
	if isAlreadyExists(err) {
		exp, err := t.experimentByName(demoExperiment)
		if err != nil {
			return nil, err
		}
		if exp == nil || !experimentMarked(exp, SampleGenAIDemo) {
			return nil, &sampleRefusal{"Experiment '" + demoExperiment + "' was created by someone else in the meantime; MLflow's demo generator would delete traces in it, so it is left unchanged."}
		}
		return exp, nil
	}
	if err != nil {
		return nil, err
	}
	log("Created experiment '" + demoExperiment + "' for the demo")
	return &mlflowExperiment{ExperimentID: created.ExperimentID, Name: demoExperiment, LifecycleStage: "active", Tags: tags}, nil
}

// dropStaleDemoVersions deletes the version tags still at "0" after a
// successful generate: every current generator either was current already
// (status "exists") or ran and stored its version (features_generated,
// demo/__init__.py:44-46), so a "0" left belongs to a feature this
// server's generator does not have. Without this the demo would show as
// partial forever.
func (t *mlflowTarget) dropStaleDemoVersions(e *mlflowExperiment, zero []string, res demoGenerateResult, log func(string)) error {
	var stale []string
	for _, f := range zero {
		if res.Status == "exists" || !containsString(res.FeaturesGenerated, f) {
			stale = append(stale, f)
		}
	}
	for _, f := range stale {
		if err := t.api.sendJSON("POST", "/api/2.0/mlflow/experiments/delete-experiment-tag", nil, map[string]interface{}{
			"experiment_id": e.ExperimentID, "key": demoVersionTagPrefix + f,
		}, nil); err != nil {
			return err
		}
	}
	if len(stale) > 0 {
		log("Dropped the version tags of " + strings.Join(stale, ", ") + ": this MLflow's generator has no such feature")
	}
	return nil
}

func addGenAIDemo(t *mlflowTarget, log func(string)) (string, error) {
	return demoAddOrRegenerate(t, log, false)
}

func regenerateGenAIDemo(t *mlflowTarget, log func(string)) (string, error) {
	return demoAddOrRegenerate(t, log, true)
}

// errMLflowRestarting: the demo was not generated because MLflow restarts
// with new resources first.
var errMLflowRestarting = errors.New("MLflow restarts with the operator's default resources (limits 4 CPU / 3Gi) now; add the demo again once MLflow is ready (1 to 3 minutes). The demo was not generated: with the 2Gi memory limit an earlier version of this tool set, generating it runs MLflow out of memory")

// raiseOldMemoryLimit removes spec.resources from a CR this tool created
// with the old limit (oldToolResources), guarded by resourceVersion, so the
// operator's defaults apply. The server restarts, so the demo waits for the
// next Add.
func (t *mlflowTarget) raiseOldMemoryLimit(log func(string)) error {
	if err := patchMLflowCR(t.c, t.cr, nil, map[string]interface{}{"resources": nil}); err != nil {
		if IsK8sError(err, 409) {
			return fmt.Errorf("MLflow changed while its memory limit was being removed; nothing was changed, refresh and retry: %w", err)
		}
		return fmt.Errorf("remove the 2Gi memory limit from MLflow CR %s: %w", mlflowCRName, err)
	}
	log("Removed the 2Gi memory limit (spec.resources) an earlier version of this tool set on MLflow CR " + mlflowCRName)
	return errMLflowRestarting
}

func demoAddOrRegenerate(t *mlflowTarget, log func(string), always bool) (string, error) {
	if t.cr.oldToolResources() {
		if !t.cr.managedByTool() {
			return "", &sampleRefusal{demoLowMemoryMessage(false)}
		}
		return "", t.raiseOldMemoryLimit(log)
	}
	exp, err := t.experimentByName(demoExperiment)
	if err != nil {
		return "", err
	}
	if exp == nil {
		if exp, err = t.createDemoExperiment(log); err != nil {
			return "", err
		}
	}
	if !experimentMarked(exp, SampleGenAIDemo) {
		return "", &sampleRefusal{demoForeignMessage(exp)}
	}
	reset := always
	if !exp.active() {
		if err := t.restoreExperiment(exp, log); err != nil {
			return "", err
		}
		reset = true
	}
	tags := demoVersionTags(exp)
	zero := demoResetFeatures(tags)
	if len(zero) > 0 {
		reset = true
	}
	if reset {
		if err := t.resetDemoVersions(exp, log); err != nil {
			return "", err
		}
		zero = zero[:0]
		for f := range tags {
			zero = append(zero, f)
		}
		sort.Strings(zero)
	}
	res, msg, err := t.generateDemo(log)
	if err != nil {
		return "", err
	}
	if err := t.dropStaleDemoVersions(exp, zero, res, log); err != nil {
		return "", err
	}
	return msg, nil
}
