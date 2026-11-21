package serving

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// ModelSpecExpectation is what the spec of the tool's model must hold for
// publish to offer it to every signed-in user: the catalog entry's
// manifest (ModelManifest), with the preset and the images it may run
// decided now. Anyone who may edit the project can change the model before
// a cluster-admin publishes it (another image, model URI, preset, command
// or environment), so publish compares the content that decides what runs
// and refuses a difference. Who wrote a field does not matter: KServe's
// own defaulting (field manager "manager") owns parts of every model's
// template, so an ownership check would refuse them all (live, RHOAI 3.6).
type ModelSpecExpectation struct {
	entry  CatalogModel
	preset string
	// PresetRepo is the preset image's repository: a model on the released
	// image runs a digest of it (PlanModelImage).
	PresetRepo string
	// Override is VLLM_CPU_IMAGE, when set.
	Override string
}

// planModelSpecExpectation reads what the expectation needs: the cluster's
// CPU preset. It returns a refusal, or "".
func planModelSpecExpectation(c *kube.Client, name string) (ModelSpecExpectation, string) {
	entry, ok := CatalogEntry(name)
	if !ok {
		return ModelSpecExpectation{}, fmt.Sprintf("%s is not a model of this tool's catalog.", name)
	}
	preset := discoverCPUPreset(c)
	if preset.problem != "" {
		return ModelSpecExpectation{}, "Cannot tell what the model should run, so it is not published: " + preset.problem
	}
	return ModelSpecExpectation{entry: entry, preset: preset.name, PresetRepo: ImageRepository(preset.image), Override: strings.TrimSpace(os.Getenv(VLLMImageEnv))}, ""
}

// ImageAllowed: the main container may run the preset's image (none set),
// VLLM_CPU_IMAGE's, or a Red Hat release of the preset's repository pinned
// by digest (what "Use the newest released Red Hat image" sets).
func (e ModelSpecExpectation) ImageAllowed(img string) bool {
	switch {
	case img == "":
		return true
	case e.Override != "" && img == e.Override:
		return true
	case strings.HasPrefix(e.PresetRepo, redHatRegistry) && strings.HasPrefix(img, e.PresetRepo+"@sha256:"):
		return true
	}
	return false
}

// modelSpecTopKeys are the spec fields the tool writes; any other one (a
// prefill or worker template, for example) is a difference.
var modelSpecTopKeys = map[string]bool{"baseRefs": true, "model": true, "replicas": true, "router": true, "template": true}

// differences lists the spec fields that differ from the expectation, by
// path, sorted. Resources are not compared (KServe defaults some of them,
// and they change nothing about what runs).
func (e ModelSpecExpectation) differences(spec map[string]any) []string {
	var out []string
	add := func(path string) { out = append(out, path) }
	for k := range spec {
		if !modelSpecTopKeys[k] {
			add("spec." + k)
		}
	}
	model, _ := spec["model"].(map[string]any)
	if s, _ := model["uri"].(string); s != e.entry.URI {
		add("spec.model.uri")
	}
	if s, _ := model["name"].(string); s != e.entry.Name {
		add("spec.model.name")
	}
	if !reflect.DeepEqual(spec["baseRefs"], []any{map[string]any{"name": e.preset}}) {
		add("spec.baseRefs")
	}
	tmpl, _ := spec["template"].(map[string]any)
	for k := range tmpl {
		if k != "containers" && k != "initContainers" {
			add("spec.template." + k)
		}
	}
	mainEnv := []envVar{{"OMP_NUM_THREADS", e.entry.Threads}, {"VLLM_CPU_KVCACHE_SPACE", e.entry.KVCacheGiB}, {"VLLM_ADDITIONAL_ARGS", e.entry.Args}}
	initEnv := []envVar{{"STORAGE_IGNORE_PATTERNS", storageIgnorePatterns}}
	out = append(out, containerDifferences("spec.template.containers", tmpl["containers"], "main", mainEnv, e.ImageAllowed)...)
	out = append(out, containerDifferences("spec.template.initContainers", tmpl["initContainers"], "storage-initializer", initEnv, func(img string) bool { return img == "" })...)
	sort.Strings(out)
	return out
}

type envVar struct{ name, value string }

// containerDifferences compares a container list that must hold exactly
// one container, want, with no command, args, envFrom or volumeMounts, the
// env listed (plain values) and an allowed image. Other fields (resources,
// probes KServe may default) are not compared.
func containerDifferences(path string, raw any, want string, env []envVar, imageOK func(string) bool) []string {
	var out []string
	list, _ := raw.([]any)
	found := false
	for _, item := range list {
		ctr, _ := item.(map[string]any)
		name, _ := ctr["name"].(string)
		at := fmt.Sprintf("%s[%s]", path, name)
		if name != want || found {
			out = append(out, at)
			continue
		}
		found = true
		if img, _ := ctr["image"].(string); !imageOK(img) {
			out = append(out, at+".image")
		}
		for _, k := range []string{"command", "args", "envFrom", "volumeMounts"} {
			if v, ok := ctr[k]; ok && !emptyValue(v) {
				out = append(out, at+"."+k)
			}
		}
		if !envEquals(ctr["env"], env) {
			out = append(out, at+".env")
		}
	}
	if !found {
		out = append(out, fmt.Sprintf("%s[%s] (missing)", path, want))
	}
	return out
}

func emptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case []any:
		return len(t) == 0
	}
	return false
}

// envEquals: the env holds exactly want (any order), each a plain value.
func envEquals(raw any, want []envVar) bool {
	list, _ := raw.([]any)
	if len(list) != len(want) {
		return false
	}
	got := map[string]string{}
	for _, item := range list {
		e, _ := item.(map[string]any)
		name, _ := e["name"].(string)
		value, isString := e["value"].(string)
		if _, from := e["valueFrom"]; from || !isString {
			return false
		}
		got[name] = value
	}
	for _, w := range want {
		if v, ok := got[w.name]; !ok || v != w.value {
			return false
		}
	}
	return true
}

// specChangedRefusal says why publish stops on a model whose spec differs.
func specChangedRefusal(name, project string, fields []string, after string) string {
	return fmt.Sprintf("The spec of %s in %s was changed since this tool set it up: %s. Set it up again on the Models tab, then publish; if a difference stays (a field someone added is kept), delete the model there and set it up again. %s", name, project, strings.Join(fields, ", "), after)
}
