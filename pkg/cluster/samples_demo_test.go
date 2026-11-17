package cluster

import (
	"strings"
	"testing"
)

func demoTagWrites(f *mlflowFake, from int) []string {
	var out []string
	for _, w := range f.writes(from) {
		if strings.HasSuffix(w, "/set-experiment-tag") || strings.HasSuffix(w, "/restore") {
			out = append(out, w)
		}
	}
	return out
}

func allDemoFeaturesHealthy(t *testing.T, f *mlflowFake) {
	t.Helper()
	for name := range f.demo.versions {
		if !f.demo.data[name] || f.demo.broken[name] {
			t.Errorf("feature %s: data=%v broken=%v", name, f.demo.data[name], f.demo.broken[name])
		}
	}
}

func TestGenAIDemo_AddAndIdempotence(t *testing.T) {
	env := newSamplesEnv(t)
	ok, msg, steps := env.run(t, "add", SampleGenAIDemo)
	if !ok || !strings.Contains(stepMessages(steps), "generated custom_view, evaluation, issues") {
		t.Fatalf("%s\n%s", msg, stepMessages(steps))
	}
	allDemoFeaturesHealthy(t, env.mlflow)
	if s := env.status(t)[SampleGenAIDemo]; s.State != "present" || s.Actions[0] != "regenerate" {
		t.Errorf("status %+v", s)
	}
	n := len(env.mlflow.requests)
	ok, _, steps = env.run(t, "add", SampleGenAIDemo)
	if !ok || !strings.Contains(stepMessages(steps), "already complete") || len(demoTagWrites(env.mlflow, n)) != 0 {
		t.Errorf("second add: %s %v", stepMessages(steps), env.mlflow.writes(n))
	}
}

// The live failure: after MLflow's own demo/delete, a plain generate
// restores the experiment but skips evaluation and issues. Add first
// restores and resets the version tags, so every feature is generated anew.
func TestGenAIDemo_AddAfterMLflowRemovalIsComplete(t *testing.T) {
	t.Run("plain generate stays incomplete (the bug)", func(t *testing.T) {
		env := newSamplesEnv(t)
		if ok, msg, _ := env.run(t, "add", SampleGenAIDemo); !ok {
			t.Fatal(msg)
		}
		env.mlflow.mlflowDeleteDemo()
		target, _, _ := mlflowSampleTarget(env.c, fakeProject)
		if _, err := target.generateDemo(func(string) {}); err != nil {
			t.Fatal(err)
		}
		if !env.mlflow.demo.broken["evaluation"] || !env.mlflow.demo.broken["issues"] {
			t.Error("the fake no longer reproduces the incomplete regeneration")
		}
	})
	t.Run("Add repairs it", func(t *testing.T) {
		env := newSamplesEnv(t)
		if ok, msg, _ := env.run(t, "add", SampleGenAIDemo); !ok {
			t.Fatal(msg)
		}
		env.mlflow.mlflowDeleteDemo()
		if s := env.status(t)[SampleGenAIDemo]; s.State != "removed" {
			t.Errorf("status after MLflow's removal: %+v", s)
		}
		n := len(env.mlflow.requests)
		ok, msg, steps := env.run(t, "add", SampleGenAIDemo)
		if !ok {
			t.Fatalf("%s\n%s", msg, stepMessages(steps))
		}
		writes := demoTagWrites(env.mlflow, n)
		if len(writes) != 1+len(env.mlflow.demo.versions) || !strings.HasSuffix(writes[0], "/restore") {
			t.Errorf("writes: %v", writes)
		}
		allDemoFeaturesHealthy(t, env.mlflow)
		if e := env.mlflow.expByName(demoExperiment); e.tags[demoVersionTagPrefix+"evaluation"] != "2" || e.tags[demoVersionTagPrefix+"issues"] != "4" {
			t.Errorf("tags %v", e.tags)
		}
	})
}

func TestGenAIDemo_RegenerateRefreshesEveryFeature(t *testing.T) {
	env := newSamplesEnv(t)
	if ok, msg, _ := env.run(t, "add", SampleGenAIDemo); !ok {
		t.Fatal(msg)
	}
	// A demo damaged in a way the status cannot see (no reset tags).
	env.mlflow.demo.broken["evaluation"] = true
	ok, msg, steps := env.run(t, "regenerate", SampleGenAIDemo)
	if !ok || !strings.Contains(stepMessages(steps), "generated custom_view, evaluation, issues, judges, prompts, review_queues, saved_views, traces") {
		t.Fatalf("%s\n%s", msg, stepMessages(steps))
	}
	allDemoFeaturesHealthy(t, env.mlflow)
}

func TestGenAIDemo_InterruptedGenerateIsPartialAndRegenerateCompletes(t *testing.T) {
	env := newSamplesEnv(t)
	if ok, msg, _ := env.run(t, "add", SampleGenAIDemo); !ok {
		t.Fatal(msg)
	}
	env.mlflow.failNext["POST /ajax-api/3.0/mlflow/demo/generate"] = 504
	if ok, msg, _ := env.run(t, "regenerate", SampleGenAIDemo); ok {
		t.Fatalf("got %s", msg)
	}
	s := env.status(t)[SampleGenAIDemo]
	if s.State != "partial" || len(s.Missing) != len(env.mlflow.demo.versions) {
		t.Errorf("status %+v", s)
	}
	if ok, msg, _ := env.run(t, "add", SampleGenAIDemo); !ok {
		t.Fatal(msg)
	}
	if s := env.status(t)[SampleGenAIDemo]; s.State != "present" {
		t.Errorf("after add: %+v", s)
	}
}

func TestGenAIDemo_PermissionDenied(t *testing.T) {
	env := newSamplesEnv(t)
	env.mlflow.allow = func(resource, verb, ns, name string) bool { return resource != "datasets" }
	ok, msg, steps := env.run(t, "add", SampleGenAIDemo)
	if ok || !strings.Contains(msg, "forbidden") || !strings.Contains(stepMessages(steps), "403 PERMISSION_DENIED") {
		t.Errorf("%s\n%s", msg, stepMessages(steps))
	}
	if env.mlflow.generates != 0 {
		t.Error("generate ran although denied")
	}
}
