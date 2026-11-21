package serving

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The model's runtime image. All Red Hat; nothing is pinned in the source:
//  1. VLLM_CPU_IMAGE, when set (disconnected clusters, a mirror).
//  2. The preset's own image when the cluster can pull it: a released
//     RHOAI (the preset names a published image), or a nightly cluster
//     whose ImageDigestMirrorSet or ImageTagMirrorSet mirrors the preset's
//     repository (registry.redhat.io/rhaii to registry.stage.redhat.io).
//  3. Otherwise (a nightly build, whose preset names an image that is not
//     published yet): the newest released image of the preset's
//     repository, found in Red Hat's public container catalog and pinned
//     by digest, with its version recorded on the model.
//
// A model that still fails to pull the preset's image with "manifest
// unknown" is offered step 3 by name (ModelState.PresetImageMissing).
const (
	VLLMImageEnv         = "VLLM_CPU_IMAGE"
	redHatRegistry       = "registry.redhat.io/"
	catalogImagesTimeout = 20 * time.Second
)

// redHatCatalogURL is the base of Red Hat's container catalog API (tests
// replace it).
var redHatCatalogURL = "https://catalog.redhat.com/api/containers/v1"

// ImagePlan is the image a model setup writes: Kind "preset" (Image
// empty: the preset's own), "released" or "override".
type ImagePlan struct {
	Kind, Image, Version string
	// Reason says why, in words, when it is not simply the preset's.
	Reason string
}

func (p ImagePlan) describe() string {
	switch p.Kind {
	case "released":
		v := ""
		if p.Version != "" {
			v = " " + p.Version
		}
		return fmt.Sprintf("the newest released Red Hat vLLM CPU image%s", v)
	case "override":
		return "the image " + VLLMImageEnv + " names (" + p.Image + ")"
	}
	return "the cluster preset's Red Hat vLLM CPU image"
}

// ImageRepository is an image reference without its tag or digest.
func ImageRepository(ref string) string {
	if at := strings.Index(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	if colon := strings.LastIndex(ref, ":"); colon > strings.LastIndex(ref, "/") {
		ref = ref[:colon]
	}
	return ref
}

// MirrorCovers reports whether a mirror set's source covers repo: the
// same repository, a parent path, or a *.domain wildcard of its host
// (OpenShift's ImageDigestMirrorSet and ImageTagMirrorSet source rules).
func MirrorCovers(source, repo string) bool {
	source = strings.TrimSuffix(source, "/")
	if source == "" {
		return false
	}
	if source == repo || strings.HasPrefix(repo, source+"/") {
		return true
	}
	if strings.HasPrefix(source, "*.") {
		host := repo
		if i := strings.Index(host, "/"); i >= 0 {
			host = host[:i]
		}
		return strings.HasSuffix(host, source[1:])
	}
	return false
}

// imageMirrored reports whether an ImageDigestMirrorSet or ImageTagMirrorSet
// mirrors repo somewhere.
func imageMirrored(c *kube.Client, repo string) (bool, error) {
	type mirrorSets struct {
		Items []struct {
			Spec struct {
				Digest []struct {
					Source  string   `json:"source"`
					Mirrors []string `json:"mirrors"`
				} `json:"imageDigestMirrors"`
				Tag []struct {
					Source  string   `json:"source"`
					Mirrors []string `json:"mirrors"`
				} `json:"imageTagMirrors"`
			} `json:"spec"`
		} `json:"items"`
	}
	for _, path := range []string{
		rhoai.ClusterPath("config.openshift.io/v1", "imagedigestmirrorsets", ""),
		rhoai.ClusterPath("config.openshift.io/v1", "imagetagmirrorsets", ""),
	} {
		var sets mirrorSets
		found, err := c.GetIntoOptional(path, &sets)
		if err != nil {
			return false, err
		}
		if !found {
			continue
		}
		for _, s := range sets.Items {
			for _, m := range s.Spec.Digest {
				if len(m.Mirrors) > 0 && MirrorCovers(m.Source, repo) {
					return true, nil
				}
			}
			for _, m := range s.Spec.Tag {
				if len(m.Mirrors) > 0 && MirrorCovers(m.Source, repo) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// ReleasedImage is a released image: its digest reference and version.
type ReleasedImage struct {
	Ref, Version string
}

// lookupReleasedImage finds the newest released image of a
// registry.redhat.io repository ("rhaii/vllm-cpu-rhel9"); tests replace it.
var lookupReleasedImage = FetchReleasedImage

func FetchReleasedImage(ctx context.Context, repository string) (ReleasedImage, error) {
	ctx, cancel := context.WithTimeout(ctx, catalogImagesTimeout)
	defer cancel()
	q := url.Values{
		"filter":    {"architecture==amd64"},
		"sort_by":   {"creation_date[desc]"},
		"page_size": {"100"},
	}
	u := fmt.Sprintf("%s/repositories/registry/registry.access.redhat.com/repository/%s/images?%s", strings.TrimRight(redHatCatalogURL, "/"), repository, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ReleasedImage{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := registry.HTTPClient().Do(req)
	if err != nil {
		return ReleasedImage{}, fmt.Errorf("the Red Hat container catalog cannot be reached: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return ReleasedImage{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return ReleasedImage{}, fmt.Errorf("the Red Hat container catalog answered HTTP %d for %s", resp.StatusCode, repository)
	}
	return PickReleasedImage(body, repository)
}

var versionTag = regexp.MustCompile(`^\d+(\.\d+){1,3}$`)

// PickReleasedImage chooses, from the catalog's images of repository,
// the published amd64 image with the highest version tag (a patch release
// of an older stream can be newer by date than the newest stream's), and
// returns its multi-arch digest as a registry.redhat.io reference.
func PickReleasedImage(body []byte, repository string) (ReleasedImage, error) {
	var page struct {
		Data []struct {
			Architecture string `json:"architecture"`
			CreationDate string `json:"creation_date"`
			Repositories []struct {
				Registry           string `json:"registry"`
				Repository         string `json:"repository"`
				Published          bool   `json:"published"`
				ManifestListDigest string `json:"manifest_list_digest"`
				ManifestDigest     string `json:"manifest_schema2_digest"`
				Tags               []struct {
					Name string `json:"name"`
				} `json:"tags"`
			} `json:"repositories"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return ReleasedImage{}, fmt.Errorf("parse Red Hat's container catalog answer: %w", err)
	}
	var best ReleasedImage
	bestDate := ""
	for _, img := range page.Data {
		if img.Architecture != "" && img.Architecture != "amd64" {
			continue
		}
		for _, r := range img.Repositories {
			if !r.Published || r.Repository != repository {
				continue
			}
			digest := kube.FirstNonEmpty(r.ManifestListDigest, r.ManifestDigest)
			if !strings.HasPrefix(digest, "sha256:") {
				continue
			}
			version := ""
			for _, t := range r.Tags {
				if versionTag.MatchString(t.Name) && (version == "" || kube.CompareVersions(t.Name, version) > 0) {
					version = t.Name
				}
			}
			if version == "" {
				continue
			}
			cmp := 1
			if best.Ref != "" {
				cmp = kube.CompareVersions(version, best.Version)
			}
			if cmp > 0 || (cmp == 0 && img.CreationDate > bestDate) {
				best = ReleasedImage{Ref: redHatRegistry + repository + "@" + digest, Version: version}
				bestDate = img.CreationDate
			}
		}
	}
	if best.Ref == "" {
		return ReleasedImage{}, fmt.Errorf("the Red Hat container catalog lists no published amd64 release of %s", repository)
	}
	return best, nil
}

// clusterIsNightly reports whether RHOAI comes from this tool's nightly
// catalog (the Subscription's source), whose presets name images that are
// not published yet.
func clusterIsNightly(c *kube.Client) (bool, error) {
	sub, err := rhoai.ReadSubscription(c)
	if err != nil {
		return false, err
	}
	return sub.Info.Source == rhoai.CatalogName, nil
}

// PlanModelImage decides the model's image (see the top of this file).
// With resolve false (the status) the released image is not looked up:
// only the kind and the reason are returned.
func PlanModelImage(c *kube.Client, presetImage string, wantReleased bool, existing *LLMISvcObject, resolve bool) (ImagePlan, error) {
	if img := strings.TrimSpace(os.Getenv(VLLMImageEnv)); img != "" {
		if strings.ContainsAny(img, " \t\n") {
			return ImagePlan{}, fmt.Errorf("%s is not an image reference: %q", VLLMImageEnv, img)
		}
		return ImagePlan{Kind: "override", Image: img, Reason: VLLMImageEnv + " names the image."}, nil
	}
	repo := ImageRepository(presetImage)
	// A model set up on a released image keeps it, so setting it up again
	// (a repair) does not move it back to an image the cluster cannot pull.
	// Only a Red Hat release of the preset's repository pinned by digest is
	// kept: anyone who may edit the project can change the model's image
	// and its annotations, and setting it up again must restore it.
	if existing != nil && !wantReleased && existing.Metadata.Annotations[ModelImageSourceAnnotation] == "released" &&
		strings.HasPrefix(repo, redHatRegistry) && strings.HasPrefix(existing.mainImage(), repo+"@sha256:") {
		return ImagePlan{Kind: "released", Image: existing.mainImage(), Version: existing.Metadata.Annotations[ModelImageVersionAnnotation], Reason: "The model keeps the released image it runs."}, nil
	}
	released := func(reason string) (ImagePlan, error) {
		plan := ImagePlan{Kind: "released", Reason: reason}
		if !resolve {
			return plan, nil
		}
		if !strings.HasPrefix(repo, redHatRegistry) {
			return ImagePlan{}, fmt.Errorf("the preset's image %q is not on %s, so no released image of it can be looked up; set %s to an image the cluster can pull", presetImage, strings.TrimSuffix(redHatRegistry, "/"), VLLMImageEnv)
		}
		img, err := lookupReleasedImage(c.Context(), strings.TrimPrefix(repo, redHatRegistry))
		if err != nil {
			return ImagePlan{}, fmt.Errorf("cannot find the newest released Red Hat image of %s: %w. Set %s to an image the cluster can pull, or retry", repo, err, VLLMImageEnv)
		}
		plan.Image, plan.Version = img.Ref, img.Version
		return plan, nil
	}
	if wantReleased {
		return released("You chose the newest released Red Hat image.")
	}
	if presetImage == "" {
		return ImagePlan{Kind: "preset"}, nil
	}
	nightly, err := clusterIsNightly(c)
	if err != nil || !nightly {
		return ImagePlan{Kind: "preset"}, nil
	}
	mirrored, err := imageMirrored(c, repo)
	if err == nil && mirrored {
		return ImagePlan{Kind: "preset", Reason: "A mirror set on this cluster serves the preset's repository " + repo + ", so its nightly image can be pulled."}, nil
	}
	// The status (and the confirmation built from it) names the released
	// image, so a failed lookup refuses with the catalog's error rather
	// than quietly using another image than the one confirmed.
	return released(fmt.Sprintf("This cluster runs a nightly RHOAI build, whose preset names an image that Red Hat has not published yet, and no mirror set serves %s; the newest released Red Hat image is used instead.", repo))
}

// --- capacity ----------------------------------------------------------------

// ParseQuantity reads a Kubernetes quantity ("250m", "4", "12Gi", "64000Ki",
// "1e3") as a number of units (cores or bytes).
func ParseQuantity(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	suffixes := []struct {
		s string
		f float64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"Pi", 1 << 50}, {"Ei", 1 << 60},
		{"m", 1e-3}, {"k", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"P", 1e15}, {"E", 1e18}}
	for _, suf := range suffixes {
		if before, ok := strings.CutSuffix(s, suf.s); ok {
			v, err := strconv.ParseFloat(before, 64)
			return v * suf.f, err == nil
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil
}

// CapacityCache keeps the capacity check for a minute: it lists every
// pod of the cluster.
var CapacityCache = kube.NewLRU[string, string](16)

const capacityCacheTTL = time.Minute

// capacityKey is a model's cache entry: the cluster, the model, and the
// project whose copy of it is not counted.
func capacityKey(c *kube.Client, name, project string) string {
	return c.BaseURL() + "|" + name + "|" + project
}

// ModelCapacityWarning returns a warning when no schedulable amd64 node
// has the model's CPU and memory requests free, or "" (room found, or the
// check could not run: unknown is not a warning). The model's own pods in
// project ("" for none) are not counted: setting it up again there
// replaces them, so their room is the new pod's.
func ModelCapacityWarning(c *kube.Client, entry CatalogModel, project string) string {
	key := capacityKey(c, entry.Name, project)
	if w, ok := CapacityCache.Get(key); ok {
		return w
	}
	free, err := freeRoom(c, project, entry.Name)
	if err != nil {
		return ""
	}
	w := RoomWarning(entry, free)
	CapacityCache.Add(key, w, capacityCacheTTL)
	return w
}

// catalogCapacityWarnings is ModelCapacityWarning (with no project) for
// every catalog model, from one read of the nodes and pods.
func catalogCapacityWarnings(c *kube.Client) map[string]string {
	out := map[string]string{}
	missing := false
	for _, m := range ModelCatalog {
		w, ok := CapacityCache.Get(capacityKey(c, m.Name, ""))
		out[m.Name], missing = w, missing || !ok
	}
	if !missing {
		return out
	}
	free, err := freeRoom(c, "", "")
	if err != nil {
		return map[string]string{}
	}
	for _, m := range ModelCatalog {
		out[m.Name] = RoomWarning(m, free)
		CapacityCache.Add(capacityKey(c, m.Name, ""), out[m.Name], capacityCacheTTL)
	}
	return out
}

// NodeRoom is the CPU (cores) and memory (bytes) a node has unrequested.
// eph is its unrequested ephemeral storage (bytes), when the node reports
// it (EphKnown).
type NodeRoom struct {
	CPU, Mem, Eph float64
	EphKnown      bool
}

// freeRoom is what every schedulable, untainted amd64 node has left after
// the requests of its pods; the pods of model in project are not counted
// (both "" for none).
func freeRoom(c *kube.Client, project, model string) (map[string]*NodeRoom, error) {
	body, _, err := c.Get("/api/v1/nodes")
	if err != nil {
		return nil, err
	}
	var nodes struct {
		Items []struct {
			Metadata kube.ObjectMeta `json:"metadata"`
			Spec     struct {
				Unschedulable bool `json:"unschedulable"`
				Taints        []struct {
					Effect string `json:"effect"`
				} `json:"taints"`
			} `json:"spec"`
			Status struct {
				Allocatable map[string]string `json:"allocatable"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &nodes); err != nil {
		return nil, err
	}
	free := map[string]*NodeRoom{}
	for _, n := range nodes.Items {
		if n.Spec.Unschedulable || n.Metadata.Labels["kubernetes.io/arch"] != "amd64" {
			continue
		}
		tainted := false
		for _, t := range n.Spec.Taints {
			tainted = tainted || t.Effect == "NoSchedule" || t.Effect == "NoExecute"
		}
		if tainted {
			continue
		}
		cc, ok1 := ParseQuantity(n.Status.Allocatable["cpu"])
		mm, ok2 := ParseQuantity(n.Status.Allocatable["memory"])
		if ok1 && ok2 {
			ee, ok3 := ParseQuantity(n.Status.Allocatable["ephemeral-storage"])
			free[n.Metadata.Name] = &NodeRoom{CPU: cc, Mem: mm, Eph: ee, EphKnown: ok3}
		}
	}
	if len(free) == 0 {
		return free, nil
	}
	body, _, err = c.Get("/api/v1/pods?fieldSelector=" + url.QueryEscape("status.phase!=Succeeded,status.phase!=Failed"))
	if err != nil {
		return nil, err
	}
	var pods struct {
		Items []struct {
			Metadata kube.ObjectMeta `json:"metadata"`
			Spec     struct {
				NodeName   string `json:"nodeName"`
				Containers []struct {
					Resources struct {
						Requests map[string]string `json:"requests"`
					} `json:"resources"`
				} `json:"containers"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &pods); err != nil {
		return nil, err
	}
	for _, p := range pods.Items {
		r := free[p.Spec.NodeName]
		if r == nil || p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed" {
			continue
		}
		if project != "" && p.Metadata.Namespace == project && p.Metadata.Labels["app.kubernetes.io/name"] == model && p.Metadata.Labels["app.kubernetes.io/component"] == modelPodComponent {
			continue
		}
		for _, ctr := range p.Spec.Containers {
			if v, ok := ParseQuantity(ctr.Resources.Requests["cpu"]); ok {
				r.CPU -= v
			}
			if v, ok := ParseQuantity(ctr.Resources.Requests["memory"]); ok {
				r.Mem -= v
			}
			if v, ok := ParseQuantity(ctr.Resources.Requests["ephemeral-storage"]); ok {
				r.Eph -= v
			}
		}
	}
	return free, nil
}

// RoomWarning says that no node in free has the model's requests free, or
// is "".
func RoomWarning(entry CatalogModel, free map[string]*NodeRoom) string {
	if len(free) == 0 {
		return fmt.Sprintf("No schedulable amd64 worker was found; the model needs one with %s CPUs and %s of memory free.", entry.CPU, memoryWords(entry.MemoryRequest))
	}
	cpu, _ := ParseQuantity(entry.CPU)
	mem, _ := ParseQuantity(entry.MemoryRequest)
	names := make([]string, 0, len(free))
	for n := range free {
		names = append(names, n)
	}
	sort.Strings(names)
	// Ephemeral storage counts where the node reports it (the model's
	// download, models_storage.go).
	eph, _ := ParseQuantity(entry.EphemeralStorage)
	best, diskOnly := "", ""
	for _, n := range names {
		r := free[n]
		if r.CPU >= cpu && r.Mem >= mem {
			if !r.EphKnown || r.Eph >= eph {
				return ""
			}
			diskOnly = kube.FirstNonEmpty(diskOnly, n)
		}
		if best == "" || r.CPU > free[best].CPU {
			best = n
		}
	}
	if diskOnly != "" {
		return fmt.Sprintf("No worker seems to have room for the model's download: it needs %s of ephemeral storage free for it (the model is downloaded to the node's disk), and %s has %.1f GB.",
			entry.EphemeralStorage, diskOnly, max(free[diskOnly].Eph, 0)/1e9)
	}
	return fmt.Sprintf("No worker seems to have room for the model: it needs %s CPUs and %s of memory free (up to %s); the worker with the most free CPU, %s, has %.1f CPUs and %.1f GiB free.",
		entry.CPU, memoryWords(entry.MemoryRequest), memoryWords(entry.Memory), best, max(free[best].CPU, 0), max(free[best].Mem, 0)/(1<<30))
}

// memoryWords writes a Kubernetes memory quantity in words ("4Gi": "4
// GiB"); any other form is kept.
func memoryWords(q string) string {
	for _, unit := range []string{"Ki", "Mi", "Gi", "Ti"} {
		if n, ok := strings.CutSuffix(q, unit); ok && n != "" {
			return n + " " + unit[:1] + "iB"
		}
	}
	return q
}

// --- status -----------------------------------------------------------------

// PlanCache keeps the status's image plan for a minute (it reads the
// Subscription and the mirror sets).
var PlanCache = kube.NewLRU[string, ImagePlan](8)

// ModelsStatus reports what a model setup needs and the catalog's
// models in the visible projects. Unknowns stay empty, never a block.
func ModelsStatus(c *kube.Client, dsc rhoai.DSCRead, dscErr error, models []LLMISvcObject, modelsErr error, visible map[string]bool) types.ModelsStatus {
	st := types.ModelsStatus{}
	for _, m := range ModelCatalog {
		st.Catalog = append(st.Catalog, m.CatalogModel)
	}
	if dscErr == nil {
		st.Blocked, st.Dependencies = ModelsBlock(dsc)
	}
	if st.Blocked == nil && errors.Is(modelsErr, ErrLLMISvcAPIMissing) {
		st.Blocked = &types.ComponentBlock{Component: "kserve", Reason: "Model serving is not installed yet: this cluster has no LLMInferenceService API. If KServe was just turned on, wait for the operator; otherwise check the Components page."}
	}
	if modelsErr == nil {
		rec, err := ReadModelsRecord(c)
		if err != nil {
			slog.Warn("cannot read the models record", "error", err)
		}
		st.Items = ModelItems(c, models, visible, rec)
	}
	if st.Blocked != nil {
		return st
	}
	preset := discoverCPUPreset(c)
	st.Preset, st.PresetProblem = preset.name, preset.problem
	if preset.problem == "" {
		key := c.BaseURL() + "|" + preset.image
		plan, ok := PlanCache.Get(key)
		if !ok {
			var err error
			if plan, err = PlanModelImage(c, preset.image, false, nil, false); err == nil {
				PlanCache.Add(key, plan, capacityCacheTTL)
			}
		}
		st.ImagePlan, st.ImagePlanReason = plan.Kind, plan.Reason
	}
	warnings := catalogCapacityWarnings(c)
	for i := range st.Catalog {
		st.Catalog[i].CapacityWarning = warnings[st.Catalog[i].Name]
	}
	return st
}
