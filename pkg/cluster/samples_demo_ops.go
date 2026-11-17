package cluster

import (
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

func (t *mlflowTarget) generateDemo(log func(string)) (string, error) {
	log("Generating the demo (this takes about 30 seconds)...")
	var out demoGenerateResult
	if err := t.api.sendJSON("POST", "/ajax-api/3.0/mlflow/demo/generate", nil, map[string]interface{}{}, &out); err != nil {
		return "", err
	}
	if out.Status == "exists" || len(out.FeaturesGenerated) == 0 {
		return "already complete", nil
	}
	return "generated " + strings.Join(out.FeaturesGenerated, ", "), nil
}

func addGenAIDemo(t *mlflowTarget, log func(string)) (string, error) {
	return demoAddOrRegenerate(t, log, false)
}

func regenerateGenAIDemo(t *mlflowTarget, log func(string)) (string, error) {
	return demoAddOrRegenerate(t, log, true)
}

func demoAddOrRegenerate(t *mlflowTarget, log func(string), always bool) (string, error) {
	exp, err := t.experimentByName(demoExperiment)
	if err != nil {
		return "", err
	}
	if exp != nil {
		reset := always
		if !exp.active() {
			if err := t.restoreExperiment(exp, log); err != nil {
				return "", err
			}
			reset = true
		}
		tags := demoVersionTags(exp)
		if len(demoResetFeatures(tags)) > 0 {
			reset = true
		}
		if reset {
			if err := t.resetDemoVersions(exp, log); err != nil {
				return "", err
			}
		}
	}
	return t.generateDemo(log)
}
