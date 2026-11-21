package cluster

import (
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/mlflow"
	"gopkg.in/yaml.v3"
	"strings"
	"testing"
)

// The spec carries the image SAMPLE_PIPELINE_IMAGE names, else the pinned
// default, and is offered under the pipeline's own name.
func TestPipelineSampleSpec_Image(t *testing.T) {
	if got := PipelineSampleSpec(); got.Image != mlflow.PipelineSampleDefaultImage || got.YAML != mlflow.PipelineIR(mlflow.PipelineSampleDefaultImage) ||
		got.Name != mlflow.PipelineSampleName || got.FileName != mlflow.PipelineSampleName+".yaml" || got.Description != mlflow.PipelineSampleDescription {
		t.Errorf("default spec = %+v", got)
	}
	t.Setenv("SAMPLE_PIPELINE_IMAGE", " mirror.example/ubi9/ubi-minimal@sha256:abc ")
	if got := PipelineSampleSpec(); got.Image != "mirror.example/ubi9/ubi-minimal@sha256:abc" || !strings.Contains(got.YAML, "image: mirror.example/ubi9/ubi-minimal@sha256:abc") {
		t.Errorf("override spec = %+v", got)
	}
}

// The sample spec is valid YAML whatever its strings hold, and carries the
// values the launcher and the dashboard read.
func TestPipelineSampleSpec_IsValidIR(t *testing.T) {
	image := "registry.example.com/x@sha256:" + strings.Repeat("a", 64)
	var spec struct {
		PipelineInfo   struct{ Name, Description string } `yaml:"pipelineInfo"`
		SchemaVersion  string                             `yaml:"schemaVersion"`
		DeploymentSpec struct {
			Executors map[string]struct {
				Container struct {
					Image   string   `yaml:"image"`
					Command []string `yaml:"command"`
					Args    []string `yaml:"args"`
				} `yaml:"container"`
			} `yaml:"executors"`
		} `yaml:"deploymentSpec"`
		Root struct {
			Dag struct {
				Tasks map[string]struct {
					DependentTasks []string `yaml:"dependentTasks"`
				} `yaml:"tasks"`
			} `yaml:"dag"`
		} `yaml:"root"`
	}
	if err := yaml.Unmarshal([]byte(mlflow.PipelineIR(image)), &spec); err != nil {
		t.Fatalf("the sample spec is not valid YAML: %v", err)
	}
	if spec.PipelineInfo.Name != mlflow.PipelineSampleName || spec.PipelineInfo.Description != mlflow.PipelineSampleDescription || !strings.Contains(spec.PipelineInfo.Description, ": ") {
		t.Errorf("pipelineInfo = %+v (the description must survive its \": \")", spec.PipelineInfo)
	}
	if spec.SchemaVersion != "2.1.0" {
		t.Errorf("schemaVersion = %q", spec.SchemaVersion)
	}
	for _, name := range []string{"exec-write-artifact", "exec-read-artifact"} {
		c := spec.DeploymentSpec.Executors[name].Container
		if c.Image != image || len(c.Command) != 2 || len(c.Args) != 2 || !strings.HasPrefix(c.Args[1], "{{$.") {
			t.Errorf("%s = %+v", name, c)
		}
	}
	if got := spec.Root.Dag.Tasks["read-artifact"].DependentTasks; len(got) != 1 || got[0] != "write-artifact" {
		t.Errorf("read-artifact depends on %v", got)
	}
}
