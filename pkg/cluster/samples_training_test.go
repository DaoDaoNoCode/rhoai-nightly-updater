package cluster

import (
	"strings"
	"testing"
)

// writes returns the non-GET MLflow requests made since index from.
func (f *mlflowFake) writes(from int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests[from:] {
		if !strings.HasPrefix(r, "GET ") {
			out = append(out, r)
		}
	}
	return out
}

func TestTrainingSample_AddIsCompleteAndIdempotent(t *testing.T) {
	env := newSamplesEnv(t)
	ok, msg, steps := env.run(t, "add", SampleTraining)
	if !ok {
		t.Fatalf("%s\n%s", msg, stepMessages(steps))
	}
	f := env.mlflow
	exp := f.expByName(trainingExperiment)
	if exp == nil || exp.stage != "active" || exp.tags[sampleMarkerKey] != SampleTraining {
		t.Fatalf("experiment = %+v", exp)
	}
	lrs := map[string]bool{}
	for _, r := range f.runs {
		if r.exp != exp.id || r.status != "FINISHED" || r.metrics != 2*trainingEpochs || r.tags[sampleMarkerKey] != SampleTraining || r.params["epochs"] != "20" {
			t.Errorf("run %+v", r)
		}
		lrs[r.params["learning_rate"]] = true
	}
	if len(f.runs) != 3 || !lrs["0.1"] || !lrs["0.01"] || !lrs["0.001"] {
		t.Errorf("runs: %v", lrs)
	}
	m := f.models[trainingModel]
	if m == nil || len(m.versions) != 2 || m.aliases[trainingAlias] != "2" || m.tags[sampleMarkerKey] != SampleTraining {
		t.Fatalf("model = %+v", m)
	}
	if s := env.status(t)[SampleTraining]; s.State != "present" || s.Link == "" {
		t.Errorf("status %+v", s)
	}

	n := len(f.requests)
	if ok, msg, _ := env.run(t, "add", SampleTraining); !ok {
		t.Fatal(msg)
	}
	for _, w := range f.writes(n) {
		if w != "POST /api/2.0/mlflow/runs/search" {
			t.Errorf("second Add wrote %s", w)
		}
	}
}

func TestTrainingSample_RemoveThenAddRestores(t *testing.T) {
	env := newSamplesEnv(t)
	if ok, msg, _ := env.run(t, "add", SampleTraining); !ok {
		t.Fatal(msg)
	}
	// Someone else's objects in the same workspace are never touched.
	env.mlflow.experiments["99"] = &fakeExperiment{id: "99", name: "my-experiment", stage: "active", tags: map[string]string{}}
	env.mlflow.models["my-model"] = &fakeModel{tags: map[string]string{}}

	if ok, msg, steps := env.run(t, "remove", SampleTraining); !ok {
		t.Fatalf("%s\n%s", msg, stepMessages(steps))
	}
	if env.mlflow.models[trainingModel] != nil || env.mlflow.expByName(trainingExperiment).stage != "deleted" {
		t.Error("not removed")
	}
	if env.mlflow.models["my-model"] == nil || env.mlflow.experiments["99"].stage != "active" {
		t.Error("removal touched objects the tool did not create")
	}
	if s := env.status(t)[SampleTraining]; s.State != "removed" {
		t.Errorf("status after remove: %+v", s)
	}
	if ok, msg, _ := env.run(t, "remove", SampleTraining); !ok || !strings.Contains(msg, "removed") {
		t.Errorf("second remove: %s", msg)
	}

	if ok, msg, steps := env.run(t, "add", SampleTraining); !ok {
		t.Fatalf("%s\n%s", msg, stepMessages(steps))
	}
	if len(env.mlflow.runs) != 3 {
		t.Errorf("restore brings the runs back; Add must not log new ones: %d runs", len(env.mlflow.runs))
	}
	if s := env.status(t)[SampleTraining]; s.State != "present" {
		t.Errorf("status after re-add: %+v", s)
	}
}

func TestTrainingSample_ForeignIsLeftAlone(t *testing.T) {
	for _, foreign := range []string{"experiment", "model"} {
		t.Run(foreign, func(t *testing.T) {
			env := newSamplesEnv(t)
			if foreign == "experiment" {
				env.mlflow.experiments["5"] = &fakeExperiment{id: "5", name: trainingExperiment, stage: "active", tags: map[string]string{}}
			} else {
				env.mlflow.models[trainingModel] = &fakeModel{tags: map[string]string{"team": "x"}, aliases: map[string]string{}}
			}
			for _, action := range []string{"add", "remove"} {
				n := len(env.mlflow.requests)
				ok, msg, _ := env.run(t, action, SampleTraining)
				if ok || !strings.Contains(msg, "not_managed") {
					t.Errorf("%s: %s", action, msg)
				}
				if w := env.mlflow.writes(n); len(w) != 0 {
					t.Errorf("%s wrote %v", action, w)
				}
			}
		})
	}
}

func TestTrainingSample_InterruptedAddIsPartialAndCompletes(t *testing.T) {
	env := newSamplesEnv(t)
	env.mlflow.failNext["POST /api/2.0/mlflow/registered-models/alias"] = 500
	ok, msg, _ := env.run(t, "add", SampleTraining)
	if ok || !strings.Contains(msg, "sample_api") {
		t.Fatalf("got %s", msg)
	}
	s := env.status(t)[SampleTraining]
	if s.State != "partial" || len(s.Missing) != 1 || s.Missing[0] != "alias champion" {
		t.Errorf("status %+v", s)
	}
	if ok, msg, _ := env.run(t, "add", SampleTraining); !ok {
		t.Fatal(msg)
	}
	if m := env.mlflow.models[trainingModel]; m.aliases[trainingAlias] == "" || len(env.mlflow.runs) != 3 {
		t.Errorf("model %+v, %d runs", m, len(env.mlflow.runs))
	}
}

func TestTrainingSample_PermissionDenied(t *testing.T) {
	env := newSamplesEnv(t)
	env.mlflow.allow = func(resource, verb, ns, name string) bool {
		return resource != "registeredmodels" || verb != "create"
	}
	ok, msg, steps := env.run(t, "add", SampleTraining)
	if ok || !strings.Contains(msg, "forbidden") {
		t.Fatalf("got %s", msg)
	}
	last := steps[len(steps)-1]
	if last.Status != "failed" || last.ErrorCode != "forbidden" || !strings.Contains(last.Message, "re-apply the template") || !strings.Contains(last.Message, "5 minutes") {
		t.Errorf("step %+v", last)
	}
}
