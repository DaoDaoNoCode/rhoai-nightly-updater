package cluster

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/workbench"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// rhoaiImageStreams loads fixture ImageStreams taken from RHOAI's shipped
// workbench streams, trimmed from red-hat-data-services/notebooks
// manifests/rhoai/base/*-imagestream.yaml (jupyter-* and code-server-*) down to
// what image selection consults, with each spec tag mirrored in status (cluster-like):
//   - rhoai-3.6.json: branch rhoai-3.6 @483902b139b29f68c9eaa6cc824bffbb52338815
//   - rhoai-3.7-ea.1.json: tag rhoai-3.7-ea.1 @56d32504bf974517e3e7c5f79d5c25e54795ae7e
//     (all streams except jupyter-universal-notebook and code-server-minimal-notebook
//     bear opendatahub.io/deprecated; the deprecated s2i-minimal-notebook:3.7
//     still has default-image).
func rhoaiImageStreams(t *testing.T, release string) []workbench.ImageStream {
	t.Helper()
	raw, err := os.ReadFile("testdata/notebook-imagestreams/" + release + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var out []workbench.ImageStream
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// deprecateAll flags each stream deprecated, matching a cluster with no non-deprecated image.
func deprecateAll(streams []workbench.ImageStream) []workbench.ImageStream {
	for i := range streams {
		streams[i].Metadata.Annotations["opendatahub.io/deprecated"] = "true"
	}
	return streams
}

// byonStream models a bring-your-own image imported by a cluster admin via the
// dashboard (Settings, Workbench images); lacking a tier annotation, selection
// classifies it as "custom", above RHOAI's "Red Hat-provided".
func byonStream(t *testing.T) workbench.ImageStream {
	return stream(t, `{"metadata":{"name":"custom-team-image","namespace":"redhat-ods-applications",
		"labels":{"app.kubernetes.io/created-by":"byon","opendatahub.io/notebook-image":"true"},
		"annotations":{"opendatahub.io/notebook-image-name":"Team image","opendatahub.io/notebook-image-order":"1"}},
		"spec":{"tags":[{"name":"1.0"}]},
		"status":{"dockerImageRepository":"reg/team","tags":[{"tag":"1.0","items":[{}]}]}}`)
}

// ChooseImage against RHOAI-shipped ImageStreams: deprecated streams lose while any
// current option exists, including those tagged default-image; BYO never outranks RHOAI;
// a deprecated pick happens only when all candidates are deprecated, and that fact is
// surfaced in the result.
func TestWorkbenchChooseImageRHOAIManifests(t *testing.T) {
	type want struct {
		stream, tag, name string
		deprecated        bool
	}
	check := func(t *testing.T, streams []workbench.ImageStream, w want) {
		t.Helper()
		got, ok := workbench.ChooseImage(streams)
		if !ok || got.Stream != w.stream || got.Tag != w.tag || got.DisplayName != w.name || got.Deprecated != w.deprecated {
			t.Fatalf("got %+v (ok %v), want %+v", got, ok, w)
		}
	}
	t.Run("3.7: the current Jupyter image, not the deprecated default", func(t *testing.T) {
		check(t, rhoaiImageStreams(t, "rhoai-3.7-ea.1"), want{"jupyter-universal-notebook", "3.7", "Jupyter | CPU | Python 3.12", false})
	})
	t.Run("3.6: RHOAI's default image, as before", func(t *testing.T) {
		check(t, rhoaiImageStreams(t, "rhoai-3.6"), want{"s2i-minimal-notebook", "3.6", "Jupyter | Minimal | CPU | Python 3.12", false})
	})
	t.Run("3.7, every image deprecated: the default image, marked deprecated", func(t *testing.T) {
		check(t, deprecateAll(rhoaiImageStreams(t, "rhoai-3.7-ea.1")), want{"s2i-minimal-notebook", "3.7", "Jupyter | Minimal | CPU | Python 3.12", true})
	})
	t.Run("3.7, every image deprecated and none default: the first in the selector's order", func(t *testing.T) {
		streams := deprecateAll(rhoaiImageStreams(t, "rhoai-3.7-ea.1"))
		for i := range streams {
			for j := range streams[i].Spec.Tags {
				delete(streams[i].Spec.Tags[j].Annotations, "opendatahub.io/default-image")
			}
		}
		// Among visible streams, s2i-minimal-notebook sorts first (order 4).
		check(t, streams, want{"s2i-minimal-notebook", "3.7", "Jupyter | Minimal | CPU | Python 3.12", true})
	})
	t.Run("a custom image without a tier does not beat RHOAI's current one", func(t *testing.T) {
		check(t, append(rhoaiImageStreams(t, "rhoai-3.7-ea.1"), byonStream(t)), want{"jupyter-universal-notebook", "3.7", "Jupyter | CPU | Python 3.12", false})
		check(t, append([]workbench.ImageStream{byonStream(t)}, rhoaiImageStreams(t, "rhoai-3.6")...), want{"s2i-minimal-notebook", "3.6", "Jupyter | Minimal | CPU | Python 3.12", false})
	})
	t.Run("a current custom image beats deprecated RHOAI ones", func(t *testing.T) {
		check(t, append(deprecateAll(rhoaiImageStreams(t, "rhoai-3.7-ea.1")), byonStream(t)), want{"custom-team-image", "1.0", "Team image", false})
	})
}

// Setup on a cluster offering only a deprecated image still applies it; check, result,
// and plan all reflect deprecation; running setup again after a current stream appears
// is a no-op—the workbench retains its prior image.
func TestWorkbenchSetupDeprecatedImageAndKeepsExistingImage(t *testing.T) {
	f, c := newResourceFake(t)
	putWorkbenchCluster(t, f, true)
	f.putJSON(wbStreams+"s2i-minimal-notebook", `{"metadata":{"labels":{"opendatahub.io/notebook-image":"true"},"annotations":{"opendatahub.io/notebook-image-name":"Jupyter | Minimal | CPU | Python 3.12","opendatahub.io/deprecated":"true"}},
		"spec":{"tags":[{"name":"3.7","annotations":{"opendatahub.io/default-image":"true"}}]},"status":{"dockerImageRepository":"reg/minimal","tags":[{"tag":"3.7"}]}}`)
	u := userClientOf(c, fakeUserToken)
	u.SetUsername("kube:admin")
	var check string
	res, err := RunWorkbench(u, types.WorkbenchRequest{Action: "setup", Project: "proj"}, func(e types.UpdateStepEvent) {
		if e.Step == "check" && e.Status == "success" {
			check = e.Message
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success || !strings.HasSuffix(res.Message, workbench.DeprecatedImageNote) || !strings.HasSuffix(check, workbench.DeprecatedImageNote) {
		t.Fatalf("result %q, check step %q", res.Message, check)
	}
	image := func() string {
		nb := f.get(wbNotebook)
		containers := nb["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
		return containers[0].(map[string]any)["image"].(string)
	}
	if got := image(); got != "reg/minimal:3.7" {
		t.Fatalf("image %q", got)
	}
	st, err := GetWorkbenchStatus(c, u, "proj")
	if err != nil {
		t.Fatal(err)
	}
	if st.Plan == nil || !st.Plan.ImageDeprecated || st.Plan.ImageStream != "s2i-minimal-notebook" {
		t.Fatalf("plan %+v %q", st.Plan, st.PlanProblem)
	}

	// Once a non-deprecated stream exists (e.g. post-upgrade), new workbenches plan
	// to use it, yet setup on an already-provisioned workbench leaves state unchanged.
	f.putJSON(wbStreams+"jupyter-universal-notebook", `{"metadata":{"labels":{"opendatahub.io/notebook-image":"true"},"annotations":{"opendatahub.io/notebook-image-name":"Jupyter | CPU | Python 3.12","opendatahub.io/notebook-image-order":"24"}},
		"spec":{"tags":[{"name":"3.7","annotations":{"opendatahub.io/workbench-image-recommended":"true"}}]},"status":{"dockerImageRepository":"reg/universal","tags":[{"tag":"3.7"}]}}`)
	if st, err = GetWorkbenchStatus(c, u, "proj"); err != nil || st.Plan == nil || st.Plan.ImageStream != "jupyter-universal-notebook" || st.Plan.ImageDeprecated {
		t.Fatalf("plan after a current image appeared: %+v %v", st.Plan, err)
	}
	before := len(f.mutations())
	if res := wbSteps(t, u, types.WorkbenchRequest{Action: "setup", Project: "proj"}); !res.Success || !strings.Contains(res.Message, "already set up") || strings.Contains(res.Message, "Deprecated") {
		t.Fatalf("second setup: %+v", res)
	}
	if len(f.mutations()) != before {
		t.Fatalf("second setup wrote: %v", f.mutations()[before:])
	}
	if got := image(); got != "reg/minimal:3.7" {
		t.Fatalf("the existing workbench's image changed to %q", got)
	}
}