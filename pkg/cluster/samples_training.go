package cluster

import (
	"net/url"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The training sample: experiment trainingExperiment with three runs (step
// metrics loss and accuracy, params learning_rate and epochs), and
// registered model trainingModel with two versions and the alias
// "champion". It is metadata only: no artifacts, so the model's Download
// and Serve actions have nothing to fetch. Both names are in the
// ServiceAccount's resourceNames (deploy/template.yaml).
const (
	trainingExperiment = "nightly-sample-training"
	trainingModel      = "nightly-sample-classifier"
	trainingAlias      = "champion"
)

type mlflowRegisteredModel struct {
	Name    string      `json:"name"`
	Tags    []mlflowTag `json:"tags"`
	Aliases []struct {
		Alias   string `json:"alias"`
		Version string `json:"version"`
	} `json:"aliases"`
}

func (m *mlflowRegisteredModel) marked(sample string) bool {
	for _, t := range m.Tags {
		if t.Key == sampleMarkerKey && t.Value == sample {
			return true
		}
	}
	return false
}

func (m *mlflowRegisteredModel) hasAlias(alias string) bool {
	for _, a := range m.Aliases {
		if a.Alias == alias {
			return true
		}
	}
	return false
}

// registeredModel returns the model, or nil when there is none
// (GetRegisteredModel: registeredmodels get, named; rules_base.py:659-662).
func (t *mlflowTarget) registeredModel(name string) (*mlflowRegisteredModel, error) {
	var out struct {
		RegisteredModel mlflowRegisteredModel `json:"registered_model"`
	}
	err := t.api.sendJSON("GET", "/api/2.0/mlflow/registered-models/get", url.Values{"name": {name}}, nil, &out)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out.RegisteredModel, nil
}

func experimentMarked(e *mlflowExperiment, sample string) bool {
	v, ok := e.tag(sampleMarkerKey)
	return ok && v == sample
}

func trainingStatus(t *mlflowTarget) types.SampleStatus {
	s := types.SampleStatus{}
	exp, err := t.experimentByName(trainingExperiment)
	if err != nil {
		return statusFromError(s, err)
	}
	model, err := t.registeredModel(trainingModel)
	if err != nil {
		return statusFromError(s, err)
	}
	switch {
	case exp != nil && !experimentMarked(exp, SampleTraining):
		s.State, s.Message, s.Actions = "foreign", foreignMessage("Experiment '"+trainingExperiment+"'"), []string{}
		return s
	case model != nil && !model.marked(SampleTraining):
		s.State, s.Message, s.Actions = "foreign", foreignMessage("Registered model '"+trainingModel+"'"), []string{}
		return s
	}
	if exp != nil && exp.active() {
		s.Link = t.link("/experiments/" + exp.ExperimentID)
	}
	switch {
	case exp == nil && model == nil:
		s.State, s.Actions = "absent", []string{"add"}
	case exp != nil && !exp.active() && model == nil:
		s.State, s.Actions = "removed", []string{"add"}
		s.Message = "Removed: the experiment is in MLflow's deleted view. Add restores it."
	case exp != nil && exp.active() && model != nil && model.hasAlias(trainingAlias):
		s.State, s.Actions = "present", []string{"remove"}
	default:
		s.State, s.Actions = "partial", []string{"add", "remove"}
		if exp == nil || !exp.active() {
			s.Missing = append(s.Missing, "experiment")
		}
		if model == nil {
			s.Missing = append(s.Missing, "registered model")
		} else if !model.hasAlias(trainingAlias) {
			s.Missing = append(s.Missing, "alias "+trainingAlias)
		}
	}
	return s
}
