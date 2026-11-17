package cluster

import (
	"fmt"
	"math"
	"strconv"
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

// trainingRuns returns the sample's finished runs by learning rate.
// SearchRuns is authorized as experiments list, else as a named get of each
// requested experiment id (rules_base.py:653-656; collection_filters.py
// 123-139, 161-174), so the named get on the sample experiment suffices.
func (t *mlflowTarget) trainingRuns(experimentID string) (map[string]string, error) {
	var out struct {
		Runs []struct {
			Info struct {
				RunID  string `json:"run_id"`
				Status string `json:"status"`
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
	runs := map[string]string{}
	for _, r := range out.Runs {
		if r.Info.Status != "FINISHED" {
			continue
		}
		for _, p := range r.Data.Params {
			if p.Key == "learning_rate" {
				runs[p.Value] = r.Info.RunID
			}
		}
	}
	return runs, nil
}

// createTrainingRun logs one finished run: runs/create, one runs/log-batch
// with the params and 2x20 step metrics (well under MLflow's 1000 metrics
// per batch), runs/update FINISHED. All three are experiments update on the
// sample experiment (rules_base.py:403-431).
func (t *mlflowTarget) createTrainingRun(experimentID, lr string, decay, floor float64) (string, error) {
	start := time.Now().UnixMilli()
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
		"start_time":    start,
		"tags":          []mlflowTag{{Key: sampleMarkerKey, Value: SampleTraining}},
	}, &created)
	if err != nil {
		return "", err
	}
	runID := created.Run.Info.RunID
	var metrics []map[string]interface{}
	for step := 0; step < trainingEpochs; step++ {
		loss := floor + (trainingStartLoss-floor)*math.Exp(-decay*float64(step))
		accuracy := math.Max(0, 1-loss/trainingStartLoss)
		ts := start + int64(step)*1000
		metrics = append(metrics,
			map[string]interface{}{"key": "loss", "value": math.Round(loss*1e4) / 1e4, "timestamp": ts, "step": step},
			map[string]interface{}{"key": "accuracy", "value": math.Round(accuracy*1e4) / 1e4, "timestamp": ts, "step": step})
	}
	err = t.api.sendJSON("POST", "/api/2.0/mlflow/runs/log-batch", nil, map[string]interface{}{
		"run_id":  runID,
		"params":  []mlflowTag{{Key: "learning_rate", Value: lr}, {Key: "epochs", Value: strconv.Itoa(trainingEpochs)}},
		"metrics": metrics,
	}, nil)
	if err != nil {
		return "", err
	}
	err = t.api.sendJSON("POST", "/api/2.0/mlflow/runs/update", nil, map[string]interface{}{
		"run_id": runID, "status": "FINISHED", "end_time": start + trainingEpochs*1000,
	}, nil)
	return runID, err
}

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
	for _, spec := range trainingRunSpecs {
		if _, ok := runs[spec.lr]; ok {
			continue
		}
		id, err := t.createTrainingRun(expID, spec.lr, spec.decay, spec.floor)
		if err != nil {
			return "", fmt.Errorf("run lr-%s: %w", spec.lr, err)
		}
		runs[spec.lr] = id
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
		if _, err := t.createModelVersion(runs[trainingV1LR], trainingV1LR); err != nil {
			return "", err
		}
		v, err := t.createModelVersion(runs[trainingChampionLR], trainingChampionLR)
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
