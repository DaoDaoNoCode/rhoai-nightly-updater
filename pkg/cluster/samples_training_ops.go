package cluster

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// trainingRunSpecs are the three runs: one learning rate each, with a loss
// curve that decays at its own rate (floor + (start-floor)*e^(-decay*step)),
// so the charts and the compare view show three distinct curves.
var trainingRunSpecs = []struct {
	lr           string
	decay, floor float64
}{
	{"0.1", 0.35, 0.32},
	{"0.01", 0.18, 0.08},
	{"0.001", 0.05, 0.45},
}

const (
	trainingEpochs    = 20
	trainingStartLoss = 2.3
	// trainingV1LR and trainingChampionLR are the runs registered as
	// versions 1 and 2; version 2 (the lowest loss) gets the alias.
	trainingV1LR       = "0.1"
	trainingChampionLR = "0.01"
)

// trainingRun is one of the sample's runs: finished, or left RUNNING by an
// Add that stopped part way (logged: its params and metrics were written,
// which runs/log-batch does in one request).
type trainingRun struct {
	id       string
	finished bool
	logged   bool
}

// trainingRuns returns the sample's active runs by learning rate, a
// finished run over a running one. A run without params is known by its
// name, "lr-<rate>". SearchRuns is authorized as experiments list, else as
// a named get of each requested experiment id (rules_base.py:653-656;
// collection_filters.py 123-139, 161-174), so the named get on the sample
// experiment suffices.
func (t *mlflowTarget) trainingRuns(experimentID string) (map[string]trainingRun, error) {
	var out struct {
		Runs []struct {
			Info struct {
				RunID   string `json:"run_id"`
				RunName string `json:"run_name"`
				Status  string `json:"status"`
			} `json:"info"`
			Data struct {
				Params []mlflowTag `json:"params"`
			} `json:"data"`
		} `json:"runs"`
	}
	req := map[string]interface{}{
		"experiment_ids": []string{experimentID},
		"filter":         fmt.Sprintf("tags.`%s` = '%s'", sampleMarkerKey, SampleTraining),
		"run_view_type":  "ACTIVE_ONLY",
		"max_results":    100,
	}
	if err := t.api.sendJSON("POST", "/api/2.0/mlflow/runs/search", nil, req, &out); err != nil {
		return nil, err
	}
	runs := map[string]trainingRun{}
	for _, r := range out.Runs {
		if r.Info.Status != "FINISHED" && r.Info.Status != "RUNNING" {
			continue
		}
		lr, logged := "", false
		for _, p := range r.Data.Params {
			if p.Key == "learning_rate" {
				lr, logged = p.Value, true
			}
		}
		if lr == "" {
			lr, _ = strings.CutPrefix(r.Info.RunName, "lr-")
		}
		run := trainingRun{id: r.Info.RunID, finished: r.Info.Status == "FINISHED", logged: logged}
		if prev, ok := runs[lr]; lr == "" || (ok && prev.finished) {
			continue
		}
		runs[lr] = run
	}
	return runs, nil
}

// createTrainingRun logs one finished run: runs/create, one runs/log-batch
// with the params and 2x20 step metrics (well under MLflow's 1000 metrics
// per batch), runs/update FINISHED. All three are experiments update on the
// sample experiment (rules_base.py:403-431).
func (t *mlflowTarget) createTrainingRun(experimentID, lr string, decay, floor float64) (string, error) {
	var created struct {
		Run struct {
			Info struct {
				RunID string `json:"run_id"`
			} `json:"info"`
		} `json:"run"`
	}
	err := t.api.sendJSON("POST", "/api/2.0/mlflow/runs/create", nil, map[string]interface{}{
		"experiment_id": experimentID,
		"run_name":      "lr-" + lr,
		"start_time":    time.Now().UnixMilli(),
		"tags":          []mlflowTag{{Key: sampleMarkerKey, Value: SampleTraining}},
	}, &created)
	if err != nil {
		return "", err
	}
	runID := created.Run.Info.RunID
	return runID, t.finishTrainingRun(trainingRun{id: runID}, lr, decay, floor)
}

// finishTrainingRun logs the run's params and metrics unless they are
// logged already, then marks it FINISHED. It completes a run an earlier Add
// left RUNNING, so a retry adds no second run for the same rate.
func (t *mlflowTarget) finishTrainingRun(run trainingRun, lr string, decay, floor float64) error {
	start := time.Now().UnixMilli()
	runID := run.id
	if !run.logged {
		if err := t.logTrainingRun(runID, lr, decay, floor, start); err != nil {
			return err
		}
	}
	return t.api.sendJSON("POST", "/api/2.0/mlflow/runs/update", nil, map[string]interface{}{
		"run_id": runID, "status": "FINISHED", "end_time": start + trainingEpochs*1000,
	}, nil)
}

func (t *mlflowTarget) logTrainingRun(runID, lr string, decay, floor float64, start int64) error {
	var metrics []map[string]interface{}
	for step := 0; step < trainingEpochs; step++ {
		loss := floor + (trainingStartLoss-floor)*math.Exp(-decay*float64(step))
		accuracy := math.Max(0, 1-loss/trainingStartLoss)
		ts := start + int64(step)*1000
		metrics = append(metrics,
			map[string]interface{}{"key": "loss", "value": math.Round(loss*1e4) / 1e4, "timestamp": ts, "step": step},
			map[string]interface{}{"key": "accuracy", "value": math.Round(accuracy*1e4) / 1e4, "timestamp": ts, "step": step})
	}
	return t.api.sendJSON("POST", "/api/2.0/mlflow/runs/log-batch", nil, map[string]interface{}{
		"run_id":  runID,
		"params":  []mlflowTag{{Key: "learning_rate", Value: lr}, {Key: "epochs", Value: strconv.Itoa(trainingEpochs)}},
		"metrics": metrics,
	}, nil)
}

// modelVersionRuns maps the run ids of the sample model's versions to the
// version, read one by one with model-versions/get (registeredmodels get
// on the model's name, rules_base.py:738-741) until MLflow answers
// RESOURCE_DOES_NOT_EXIST. Versions are numbered from 1 per model; a gap
// left by a deleted version ends the scan early, which only costs a new
// version.
func (t *mlflowTarget) modelVersionRuns() (map[string]string, error) {
	out := map[string]string{}
	for v := 1; v <= maxSampleModelVersions; v++ {
		var mv struct {
			ModelVersion struct {
				Version string `json:"version"`
				RunID   string `json:"run_id"`
			} `json:"model_version"`
		}
		err := t.api.sendJSON("GET", "/api/2.0/mlflow/model-versions/get", url.Values{"name": {trainingModel}, "version": {strconv.Itoa(v)}}, nil, &mv)
		if isNotFound(err) {
			break
		}
		if err != nil {
			return nil, err
		}
		if _, seen := out[mv.ModelVersion.RunID]; !seen {
			out[mv.ModelVersion.RunID] = mv.ModelVersion.Version
		}
	}
	return out, nil
}

// maxSampleModelVersions bounds the scan: the sample makes two versions.
const maxSampleModelVersions = 20

// createModelVersion registers runID's (absent) model as a new version. The
// server only checks that the runs:/ URI names run_id (handlers.py:
// 3214-3221), so no artifact is needed.
func (t *mlflowTarget) createModelVersion(runID, lr string) (string, error) {
	var out struct {
		ModelVersion struct {
			Version string `json:"version"`
		} `json:"model_version"`
	}
	err := t.api.sendJSON("POST", "/api/2.0/mlflow/model-versions/create", nil, map[string]interface{}{
		"name": trainingModel, "source": "runs:/" + runID + "/model", "run_id": runID,
		"description": "Sample version from run lr-" + lr + " (metadata only: no model files).",
	}, &out)
	return out.ModelVersion.Version, err
}

func (t *mlflowTarget) trainingObjects() (*mlflowExperiment, *mlflowRegisteredModel, error) {
	exp, err := t.experimentByName(trainingExperiment)
	if err != nil {
		return nil, nil, err
	}
	model, err := t.registeredModel(trainingModel)
	if err != nil {
		return nil, nil, err
	}
	if exp != nil && !experimentMarked(exp, SampleTraining) {
		return nil, nil, &sampleRefusal{foreignMessage("Experiment '" + trainingExperiment + "'")}
	}
	if model != nil && !model.marked(SampleTraining) {
		return nil, nil, &sampleRefusal{foreignMessage("Registered model '" + trainingModel + "'")}
	}
	return exp, model, nil
}

// addTraining creates what is missing of the training sample. A soft-deleted
// sample experiment is restored (its name cannot be reused until mlflow gc;
// sqlalchemy_store.py:668-672), which also brings its runs back.
func addTraining(t *mlflowTarget, log func(string)) (string, error) {
	exp, model, err := t.trainingObjects()
	if err != nil {
		return "", err
	}
	var expID string
	switch {
	case exp == nil:
		var created struct {
			ExperimentID string `json:"experiment_id"`
		}
		if err := t.api.sendJSON("POST", "/api/2.0/mlflow/experiments/create", nil, map[string]interface{}{
			"name": trainingExperiment,
			"tags": []mlflowTag{{Key: sampleMarkerKey, Value: SampleTraining}, {Key: "mlflow.note.content", Value: "Sample training runs created by rhoai-nightly-updater. Metadata only: no artifacts."}},
		}, &created); err != nil {
			return "", err
		}
		expID = created.ExperimentID
		log("Created experiment '" + trainingExperiment + "'")
	case !exp.active():
		if err := t.api.sendJSON("POST", "/api/2.0/mlflow/experiments/restore", nil, map[string]interface{}{"experiment_id": exp.ExperimentID}, nil); err != nil {
			return "", err
		}
		expID = exp.ExperimentID
		log("Restored experiment '" + trainingExperiment + "'")
	default:
		expID = exp.ExperimentID
	}

	runs, err := t.trainingRuns(expID)
	if err != nil {
		return "", err
	}
	runIDs := map[string]string{}
	for _, spec := range trainingRunSpecs {
		run, ok := runs[spec.lr]
		switch {
		case ok && run.finished:
			runIDs[spec.lr] = run.id
			continue
		case ok:
			// Left RUNNING by an earlier Add that stopped: complete it rather
			// than log a second run for the same rate.
			if err := t.finishTrainingRun(run, spec.lr, spec.decay, spec.floor); err != nil {
				return "", fmt.Errorf("run lr-%s: %w", spec.lr, err)
			}
			runIDs[spec.lr] = run.id
			log("Completed run lr-" + spec.lr + ", left running by an earlier Add")
			continue
		}
		id, err := t.createTrainingRun(expID, spec.lr, spec.decay, spec.floor)
		if err != nil {
			return "", fmt.Errorf("run lr-%s: %w", spec.lr, err)
		}
		runIDs[spec.lr] = id
		log("Logged run lr-" + spec.lr + " (" + strconv.Itoa(trainingEpochs) + " steps of loss and accuracy)")
	}

	if model == nil {
		err := t.api.sendJSON("POST", "/api/2.0/mlflow/registered-models/create", nil, map[string]interface{}{
			"name":        trainingModel,
			"description": "Sample registered model created by rhoai-nightly-updater (metadata only).",
			"tags":        []mlflowTag{{Key: sampleMarkerKey, Value: SampleTraining}},
		}, nil)
		switch {
		case isAlreadyExists(err):
			// Created by someone in the meantime: use it only if it is the tool's.
			if model, err = t.registeredModel(trainingModel); err != nil {
				return "", err
			}
			if model == nil || !model.marked(SampleTraining) {
				return "", &sampleRefusal{foreignMessage("Registered model '" + trainingModel + "'")}
			}
		case err != nil:
			return "", err
		default:
			log("Created registered model '" + trainingModel + "'")
		}
	}
	if model == nil || !model.hasAlias(trainingAlias) {
		// A model that existed before this Add may hold the versions an
		// earlier Add registered before it stopped: reuse those whose run
		// matches, so a retry adds no extra versions.
		versions := map[string]string{}
		if model != nil {
			if versions, err = t.modelVersionRuns(); err != nil {
				return "", err
			}
		}
		version := func(lr string) (string, error) {
			if v, ok := versions[runIDs[lr]]; ok {
				return v, nil
			}
			return t.createModelVersion(runIDs[lr], lr)
		}
		if _, err := version(trainingV1LR); err != nil {
			return "", err
		}
		v, err := version(trainingChampionLR)
		if err != nil {
			return "", err
		}
		if err := t.api.sendJSON("POST", "/api/2.0/mlflow/registered-models/alias", nil, map[string]interface{}{
			"name": trainingModel, "alias": trainingAlias, "version": v,
		}, nil); err != nil {
			return "", err
		}
		log("Registered two versions; alias '" + trainingAlias + "' on version " + v)
	}
	return "added (3 runs, a registered model with 2 versions and the alias " + trainingAlias + ")", nil
}

// removeTraining deletes the registered model (a hard delete with its
// versions: model_registry sqlalchemy_store.py:504-517) and soft-deletes
// the experiment (its runs with it; Add restores both). Both are checked
// for the marker before either is deleted.
func removeTraining(t *mlflowTarget, log func(string)) (string, error) {
	exp, model, err := t.trainingObjects()
	if err != nil {
		return "", err
	}
	if model == nil && (exp == nil || !exp.active()) {
		return "nothing to remove", nil
	}
	if model != nil {
		if err := t.api.sendJSON("DELETE", "/api/2.0/mlflow/registered-models/delete", nil, map[string]interface{}{"name": trainingModel}, nil); err != nil && !isNotFound(err) {
			return "", err
		}
		log("Deleted registered model '" + trainingModel + "'")
	}
	if exp != nil && exp.active() {
		if err := t.api.sendJSON("POST", "/api/2.0/mlflow/experiments/delete", nil, map[string]interface{}{"experiment_id": exp.ExperimentID}, nil); err != nil {
			return "", err
		}
		log("Deleted experiment '" + trainingExperiment + "' (MLflow keeps it in its deleted view until garbage collection)")
	}
	return "removed", nil
}
