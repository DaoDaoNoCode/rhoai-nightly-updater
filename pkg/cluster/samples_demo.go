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
// The tool creates the experiment itself, with its marker tag, before it
// calls demo/generate; the generators then fill it (mlflow.set_experiment
// takes the existing experiment, traces.py:325-327). So the tool's demo is
// recognized by the marker like the other samples, and an experiment of
// that name someone else made is never handed to the generator.
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
const demoRemoveHint = "To remove the demo, delete it in the MLflow UI. This tool does not remove it, and Clean up leaves it in place."

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

// demoForeignMessage explains why the tool leaves an unmarked "MLflow Demo"
// alone. The generator works on whatever is in that experiment: on a
// version mismatch, or with no version tag at all, the traces generator's
// delete_demo() deletes up to 200 traces of the experiment, whoever logged
// them (generators/traces.py:421-441; base.py:97-112, called for every
// feature by demo/generate, handlers.py:7987-7989). So Add, Regenerate and
// restore run only on the experiment the tool created and marked.
func demoForeignMessage(e *mlflowExperiment) string {
	if len(demoVersionTags(e)) > 0 {
		return "Experiment '" + demoExperiment + "' holds MLflow's demo, made in the MLflow UI or by someone else, not by this tool. Regenerating it deletes traces and evaluations in that experiment, whoever logged them, so this tool leaves it alone: regenerate or delete it in the MLflow UI."
	}
	return foreignMessage("Experiment '"+demoExperiment+"'") + " MLflow's demo generator would delete traces in it."
}

// demoStatus reads the demo with one named get. Which features a server's
// generator has is not exposed over REST, so "partial" means what the tool
// can see: features left at the reset version "0" by an interrupted Add or
// Regenerate, or the tool's experiment with no version tags at all. A demo
// damaged by MLflow's own removal (above) still looks complete; Regenerate
// repairs any demo, so it is always offered on the tool's demo. An
// experiment of the demo's name without the tool's marker is "foreign"
// (demoForeignMessage).
func demoStatus(t *mlflowTarget) types.SampleStatus {
	s := demoStatusOfExperiment(t)
	if s.State == "unknown" || s.State == "foreign" || !t.cr.oldToolResources() {
		return s
	}
	if !t.cr.managedByTool() {
		s.State, s.Actions, s.Message, s.Missing = "unsupported", []string{}, demoLowMemoryMessage(false), nil
		return s
	}
	s.Message = demoLowMemoryMessage(true)
	s.Repair = demoMemoryRepair
	return s
}

// demoMemoryRepair is what Add or Regenerate does first on such a CR.
const demoMemoryRepair = "MLflow CR " + mlflowCRName + ": spec.resources (requests 500m/1Gi, limits 2/2Gi, set by an earlier version of this tool) is removed, so the operator's defaults (limits 4 CPU / 3Gi) apply. MLflow restarts, and the demo is not generated in this run: add it again once MLflow is ready."

// demoLowMemoryMessage explains the 2Gi limit of an MLflow CR created by an
// earlier version of this tool (mlflowCR.oldToolResources).
func demoLowMemoryMessage(managed bool) string {
	if !managed {
		return "MLflow's memory limit is 2Gi (spec.resources of MLflow CR " + mlflowCRName + "), and generating the demo runs MLflow out of memory. This tool did not create that MLflow, so it does not change the limit: raise it, or remove spec.resources so the operator's defaults (3Gi) apply."
	}
	return "MLflow still has the 2Gi memory limit an earlier version of this tool set, and generating the demo runs MLflow out of memory. Add or Regenerate first removes that limit (spec.resources of MLflow CR " + mlflowCRName + ", so the operator's defaults of 4 CPU / 3Gi apply) and MLflow restarts; then add the demo again once MLflow is ready."
}

func demoStatusOfExperiment(t *mlflowTarget) types.SampleStatus {
	s := types.SampleStatus{}
	exp, err := t.experimentByName(demoExperiment)
	if err != nil {
		return statusFromError(s, err)
	}
	switch {
	case exp == nil:
		s.State, s.Actions = "absent", []string{"add"}
		return s
	case !experimentMarked(exp, SampleGenAIDemo):
		s.State, s.Actions, s.Message = "foreign", []string{}, demoForeignMessage(exp)
		return s
	case !exp.active():
		s.State, s.Actions = "removed", []string{"add"}
		s.Message = "Removed in MLflow: Add restores the experiment and generates every feature again."
		return s
	}
	s.Link = t.experimentLink(exp.ExperimentID)
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
