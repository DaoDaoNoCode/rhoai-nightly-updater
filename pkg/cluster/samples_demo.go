package cluster

import (
	"sort"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The GenAI demo is MLflow's own demo generator (mlflow/demo, byte-identical
// on red-hat-data-services/mlflow rhoai-3.6 5b0385e4 and rhoai-3.7
// 6ef5fbb4). It fills one experiment, demoExperiment, with prompts, traces,
// evaluation datasets and runs, judges, issues, review queues and saved
// views (the dashboard's Agent traces and Prompt management pages).
//
// Facts this file relies on, read at rhoai-3.6:
//   - The experiment name is a module constant, DEMO_EXPERIMENT_NAME =
//     "MLflow Demo" (demo/base.py:10), so it is not configurable.
//   - Each feature stores its generator version as experiment tag
//     "mlflow.demo.version.<feature>" after generating (base.py:127-137,
//     demo/__init__.py:44-45).
//   - is_generated() returns true only when the feature's data exists and
//     the stored version equals the generator's; on a mismatch it calls the
//     generator's own delete_demo() and returns false, so the feature is
//     generated again (base.py:97-112; demo/__init__.py:37-46). A stored
//     "0" parses as 0 (base.py:121-122), and generator versions start at 1
//     (base.py:76; evaluation.py:158 is 2, issues.py:40 is 4).
//   - POST /ajax-api/3.0/mlflow/demo/generate runs is_generated() for every
//     feature first and answers "exists" when all are current
//     (handlers.py:7965-8012).
//   - POST .../demo/delete soft-deletes the experiment (handlers.py:
//     8015-8041). Its evaluation runs and issues stay behind (soft-deleted
//     runs, REJECTED issues: evaluation.py:239-267, issues.py:204-230), and
//     restoring the experiment makes every run active again
//     (sqlalchemy_store.py:1050-1058). Their version tags survive, so the
//     next generate skips evaluation and issues although their datasets
//     and pending issues are gone: the incomplete demo seen live.
//
// The tool may not delete the demo: demo/delete checks experiments,
// datasets and registeredmodels "delete" without names
// (mlflow-kubernetes-plugins v2.1.1 auth/rules_base.py:1183-1187), and the
// ServiceAccount holds no unnamed delete on MLflow data. So the demo has
// Add and Regenerate, no Remove.
const (
	demoExperiment       = "MLflow Demo"
	demoVersionTagPrefix = "mlflow.demo.version."
)

// demoRemoveHint tells the user how to remove the demo without the tool.
const demoRemoveHint = "The updater cannot remove the demo (it holds no right to delete MLflow data it did not name); remove it from the MLflow UI. Clean up leaves it in place."

// demoVersionTags returns the demo's feature version tags: feature -> value.
func demoVersionTags(e *mlflowExperiment) map[string]string {
	out := map[string]string{}
	for _, t := range e.Tags {
		if f, ok := strings.CutPrefix(t.Key, demoVersionTagPrefix); ok && f != "" {
			out[f] = t.Value
		}
	}
	return out
}

// demoResetFeatures are the features whose version tag is "0": a reset
// the tool made before generating, which the generate did not finish.
func demoResetFeatures(tags map[string]string) []string {
	var out []string
	for f, v := range tags {
		if v == "0" {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// demoStatus reads the demo with one named get. Which features a server's
// generator has is not exposed over REST, so "partial" means what the tool
// can see: features left at the reset version "0" by an interrupted Add or
// Regenerate, or an experiment with no version tags at all. A demo damaged
// by MLflow's own removal (above) still looks complete; Regenerate repairs
// any demo, so it is always offered on one that exists.
func demoStatus(t *mlflowTarget) types.SampleStatus {
	s := types.SampleStatus{}
	exp, err := t.experimentByName(demoExperiment)
	if err != nil {
		return statusFromError(s, err)
	}
	switch {
	case exp == nil:
		s.State, s.Actions = "absent", []string{"add"}
		return s
	case !exp.active():
		s.State, s.Actions = "removed", []string{"add"}
		s.Message = "Removed in MLflow: Add restores the experiment and generates every feature again."
		return s
	}
	s.Link = t.link("/experiments/" + exp.ExperimentID)
	s.Actions = []string{"regenerate"}
	tags := demoVersionTags(exp)
	reset := demoResetFeatures(tags)
	switch {
	case len(tags) == 0:
		s.State = "partial"
		s.Message = "The demo experiment has no feature versions; Regenerate generates every feature."
	case len(reset) > 0:
		s.State, s.Missing = "partial", reset
		s.Message = "An earlier Add or Regenerate did not finish; Regenerate completes it."
	default:
		s.State = "present"
		s.Message = demoRemoveHint
	}
	return s
}
