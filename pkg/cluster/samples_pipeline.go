package cluster

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The sample pipeline: a two-step pipeline (write an artifact, read it
// back) in KFP's IR, which the Pipelines tab offers as a file with the
// commands that upload and run it with the user's own OpenShift token.
//
// The tool itself never calls a pipeline server's (KFP) API: that API is a
// Service in the user's project, and sending the app's ServiceAccount token
// there would hand it to whoever can run a pod behind that Service
// (SECURITY.md). The spec is data only; serving it makes no cluster request.
const (
	pipelineSampleName        = "rhoai-nightly-sample"
	pipelineSampleDescription = "Sample created by rhoai-nightly-updater: a two-step pipeline that writes an artifact and reads it back."
	// pipelineSampleDefaultImage is UBI 9 minimal, pinned by digest (the
	// multi-arch manifest list, amd64/arm64/s390x/ppc64le, read from
	// registry.access.redhat.com on 2026-11-17). Both steps only need sh,
	// mkdir, dirname, echo and cat. On a disconnected cluster mirror it or
	// set SAMPLE_PIPELINE_IMAGE.
	pipelineSampleDefaultImage = "registry.access.redhat.com/ubi9/ubi-minimal@sha256:baafce2d34783a28b4a472779f304e97e00f9d35c99553bc53b068b14344d9a9"
)

// pipelineSampleImage is the image both steps run: SAMPLE_PIPELINE_IMAGE,
// else the pinned default.
func pipelineSampleImage() string {
	if img := strings.TrimSpace(os.Getenv("SAMPLE_PIPELINE_IMAGE")); img != "" {
		return img
	}
	return pipelineSampleDefaultImage
}

// pipelineSampleSpec is the pipeline in KFP's IR (PipelineSpec, schema
// 2.1.0) with two shell-only container components. The launcher replaces
// the artifact path placeholders; "sh -c script path" passes the path as
// $0. Caching is off so every run executes.
//
// It is built as data and marshalled, never templated as text: a templated
// description with ": " in it was invalid YAML, and KFP's upload handler
// hung on it until the route answered 504 (seen live on 3.6).
func pipelineSampleSpec(image string) string {
	artifact := map[string]any{
		"artifactType": map[string]any{"schemaTitle": "system.Artifact", "schemaVersion": "0.0.1"},
	}
	container := func(script, path string) map[string]any {
		return map[string]any{"container": map[string]any{
			"image": image, "command": []string{"sh", "-c"}, "args": []string{script, path},
		}}
	}
	spec := map[string]any{
		"components": map[string]any{
			"comp-write-artifact": map[string]any{
				"executorLabel":     "exec-write-artifact",
				"outputDefinitions": map[string]any{"artifacts": map[string]any{"greeting": artifact}},
			},
			"comp-read-artifact": map[string]any{
				"executorLabel":    "exec-read-artifact",
				"inputDefinitions": map[string]any{"artifacts": map[string]any{"greeting": artifact}},
			},
		},
		"deploymentSpec": map[string]any{"executors": map[string]any{
			"exec-write-artifact": container(`mkdir -p "$(dirname "$0")" && echo "Hello from the rhoai-nightly-updater sample pipeline" > "$0" && echo "Wrote $0"`,
				"{{$.outputs.artifacts['greeting'].path}}"),
			"exec-read-artifact": container(`echo "Read $0:" && cat "$0"`, "{{$.inputs.artifacts['greeting'].path}}"),
		}},
		"pipelineInfo": map[string]any{"name": pipelineSampleName, "description": pipelineSampleDescription},
		"root": map[string]any{"dag": map[string]any{"tasks": map[string]any{
			"write-artifact": map[string]any{
				"cachingOptions": map[string]any{},
				"componentRef":   map[string]any{"name": "comp-write-artifact"},
				"taskInfo":       map[string]any{"name": "write-artifact"},
			},
			"read-artifact": map[string]any{
				"cachingOptions": map[string]any{},
				"componentRef":   map[string]any{"name": "comp-read-artifact"},
				"dependentTasks": []string{"write-artifact"},
				"inputs": map[string]any{"artifacts": map[string]any{"greeting": map[string]any{
					"taskOutputArtifact": map[string]any{"outputArtifactKey": "greeting", "producerTask": "write-artifact"},
				}}},
				"taskInfo": map[string]any{"name": "read-artifact"},
			},
		}}},
		"schemaVersion": "2.1.0",
		"sdkVersion":    "kfp-2.13.0",
	}
	out, err := yaml.Marshal(spec)
	if err != nil {
		// Marshalling plain maps of strings cannot fail; a panic here is a
		// programming error caught by the tests.
		panic(fmt.Sprintf("marshal the sample pipeline: %v", err))
	}
	return string(out)
}

// PipelineSampleSpec is GET /api/resources/samples/pipeline/spec: the
// sample pipeline's file and what the commands need.
func PipelineSampleSpec() types.PipelineSampleSpec {
	image := pipelineSampleImage()
	return types.PipelineSampleSpec{
		Name: pipelineSampleName, FileName: pipelineSampleName + ".yaml",
		Description: pipelineSampleDescription, Image: image, YAML: pipelineSampleSpec(image),
	}
}
