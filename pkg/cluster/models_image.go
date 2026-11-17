package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

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
	vllmImageEnv         = "VLLM_CPU_IMAGE"
	redHatRegistry       = "registry.redhat.io/"
	catalogImagesTimeout = 20 * time.Second
)

// redHatCatalogURL is the base of Red Hat's container catalog API (tests
// replace it).
var redHatCatalogURL = "https://catalog.redhat.com/api/containers/v1"

// imagePlan is the image a model setup writes: Kind "preset" (Image
// empty: the preset's own), "released" or "override".
type imagePlan struct {
	Kind, Image, Version string
	// Reason says why, in words, when it is not simply the preset's.
	Reason string
}

func (p imagePlan) describe() string {
	switch p.Kind {
	case "released":
		v := ""
		if p.Version != "" {
			v = " " + p.Version
		}
		return fmt.Sprintf("the newest released Red Hat vLLM CPU image%s", v)
	case "override":
		return "the image " + vllmImageEnv + " names (" + p.Image + ")"
	}
	return "the cluster preset's Red Hat vLLM CPU image"
}

// imageRepository is an image reference without its tag or digest.
func imageRepository(ref string) string {
	if at := strings.Index(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	if colon := strings.LastIndex(ref, ":"); colon > strings.LastIndex(ref, "/") {
		ref = ref[:colon]
	}
	return ref
}

// mirrorCovers reports whether a mirror set's source covers repo: the
// same repository, a parent path, or a *.domain wildcard of its host
// (OpenShift's ImageDigestMirrorSet and ImageTagMirrorSet source rules).
func mirrorCovers(source, repo string) bool {
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
func imageMirrored(c *Client, repo string) (bool, error) {
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
		clusterPath("config.openshift.io/v1", "imagedigestmirrorsets", ""),
		clusterPath("config.openshift.io/v1", "imagetagmirrorsets", ""),
	} {
		body, _, err := c.get(path)
		if IsK8sError(err, 404) {
			continue
		}
		if err != nil {
			return false, err
		}
		var sets mirrorSets
		if err := json.Unmarshal(body, &sets); err != nil {
			return false, err
		}
		for _, s := range sets.Items {
			for _, m := range s.Spec.Digest {
				if len(m.Mirrors) > 0 && mirrorCovers(m.Source, repo) {
					return true, nil
				}
			}
			for _, m := range s.Spec.Tag {
				if len(m.Mirrors) > 0 && mirrorCovers(m.Source, repo) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// releasedImage is a released image: its digest reference and version.
type releasedImage struct {
	Ref, Version string
}

// lookupReleasedImage finds the newest released image of a
// registry.redhat.io repository ("rhaii/vllm-cpu-rhel9"); tests replace it.
var lookupReleasedImage = fetchReleasedImage

func fetchReleasedImage(ctx context.Context, repository string) (releasedImage, error) {
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
		return releasedImage{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := quayHTTPClient.Do(req)
	if err != nil {
		return releasedImage{}, fmt.Errorf("the Red Hat container catalog cannot be reached: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return releasedImage{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return releasedImage{}, fmt.Errorf("the Red Hat container catalog answered HTTP %d for %s", resp.StatusCode, repository)
	}
	return pickReleasedImage(body, repository)
}

var versionTag = regexp.MustCompile(`^\d+(\.\d+){1,3}$`)

func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return len(pa) - len(pb)
}

// pickReleasedImage chooses, from the catalog's images of repository,
// the published amd64 image with the highest version tag (a patch release
// of an older stream can be newer by date than the newest stream's), and
// returns its multi-arch digest as a registry.redhat.io reference.
func pickReleasedImage(body []byte, repository string) (releasedImage, error) {
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
		return releasedImage{}, fmt.Errorf("parse Red Hat's container catalog answer: %w", err)
	}
	var best releasedImage
	bestDate := ""
	for _, img := range page.Data {
		if img.Architecture != "" && img.Architecture != "amd64" {
			continue
		}
		for _, r := range img.Repositories {
			if !r.Published || r.Repository != repository {
				continue
			}
			digest := firstNonEmpty(r.ManifestListDigest, r.ManifestDigest)
			if !strings.HasPrefix(digest, "sha256:") {
				continue
			}
			version := ""
			for _, t := range r.Tags {
				if versionTag.MatchString(t.Name) && (version == "" || compareVersions(t.Name, version) > 0) {
					version = t.Name
				}
			}
			if version == "" {
				continue
			}
			cmp := 1
			if best.Ref != "" {
				cmp = compareVersions(version, best.Version)
			}
			if cmp > 0 || (cmp == 0 && img.CreationDate > bestDate) {
				best = releasedImage{Ref: redHatRegistry + repository + "@" + digest, Version: version}
				bestDate = img.CreationDate
			}
		}
	}
	if best.Ref == "" {
		return releasedImage{}, fmt.Errorf("the Red Hat container catalog lists no published amd64 release of %s", repository)
	}
	return best, nil
}

// clusterIsNightly reports whether RHOAI comes from this tool's nightly
// catalog (the Subscription's source), whose presets name images that are
// not published yet.
func clusterIsNightly(c *Client) (bool, error) {
	sub, err := readSubscription(c)
	if err != nil {
		return false, err
	}
	return sub.info.Source == CatalogName, nil
}

// planModelImage decides the model's image (see the top of this file).
// With resolve false (the status) the released image is not looked up:
// only the kind and the reason are returned.
func planModelImage(c *Client, presetImage string, wantReleased bool, existing *llmisvcObject, resolve bool) (imagePlan, error) {
	if img := strings.TrimSpace(os.Getenv(vllmImageEnv)); img != "" {
		if strings.ContainsAny(img, " \t\n") {
			return imagePlan{}, fmt.Errorf("%s is not an image reference: %q", vllmImageEnv, img)
		}
		return imagePlan{Kind: "override", Image: img, Reason: vllmImageEnv + " names the image."}, nil
	}
	// A model set up on a released image keeps it, so setting it up again
	// (a repair) does not move it back to an image the cluster cannot pull.
	if existing != nil && !wantReleased && existing.Metadata.Annotations[modelImageSourceAnnotation] == "released" && existing.mainImage() != "" {
		return imagePlan{Kind: "released", Image: existing.mainImage(), Version: existing.Metadata.Annotations[modelImageVersionAnnotation], Reason: "The model keeps the released image it runs."}, nil
	}
	repo := imageRepository(presetImage)
	released := func(reason string) (imagePlan, error) {
		plan := imagePlan{Kind: "released", Reason: reason}
		if !resolve {
			return plan, nil
		}
		if !strings.HasPrefix(repo, redHatRegistry) {
			return imagePlan{}, fmt.Errorf("the preset's image %q is not on %s, so no released image of it can be looked up; set %s to an image the cluster can pull", presetImage, strings.TrimSuffix(redHatRegistry, "/"), vllmImageEnv)
		}
		img, err := lookupReleasedImage(c.ctx, strings.TrimPrefix(repo, redHatRegistry))
		if err != nil {
			return imagePlan{}, fmt.Errorf("cannot find the newest released Red Hat image of %s: %v. Set %s to an image the cluster can pull, or retry", repo, err, vllmImageEnv)
		}
		plan.Image, plan.Version = img.Ref, img.Version
		return plan, nil
	}
	if wantReleased {
		return released("You chose the newest released Red Hat image.")
	}
	if presetImage == "" {
		return imagePlan{Kind: "preset"}, nil
	}
	nightly, err := clusterIsNightly(c)
	if err != nil || !nightly {
		return imagePlan{Kind: "preset"}, nil
	}
	mirrored, err := imageMirrored(c, repo)
	if err == nil && mirrored {
		return imagePlan{Kind: "preset", Reason: "A mirror set on this cluster serves the preset's repository " + repo + ", so its nightly image can be pulled."}, nil
	}
	plan, perr := released(fmt.Sprintf("This cluster runs a nightly RHOAI build, whose preset names an image that Red Hat has not published yet, and no mirror set serves %s; the newest released Red Hat image is used instead.", repo))
	if perr != nil {
		// Not found: keep the preset's image and say why; the status
		// offers the released image again if the pull fails.
		return imagePlan{Kind: "preset", Reason: fmt.Sprintf("The preset's image may not be published for this nightly build, but %v", perr)}, nil
	}
	return plan, nil
}

// --- capacity ----------------------------------------------------------------

// parseQuantity reads a Kubernetes quantity ("250m", "4", "12Gi", "64000Ki",
// "1e3") as a number of units (cores or bytes).
func parseQuantity(s string) (float64, bool) {
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
		if strings.HasSuffix(s, suf.s) {
			v, err := strconv.ParseFloat(strings.TrimSuffix(s, suf.s), 64)
			return v * suf.f, err == nil
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil
}

// capacityCache keeps the capacity check for a minute: it lists every
// pod of the cluster.
var capacityCache = newLRU[string, string](8)

const capacityCacheTTL = time.Minute

// modelCapacityWarning returns a warning when no schedulable amd64 node
// has the model's CPU and memory requests free, or "" (room found, or the
// check could not run: unknown is not a warning).
func modelCapacityWarning(c *Client, entry catalogModel) string {
	key := c.baseURL + "|" + entry.Name
	if w, ok := capacityCache.Get(key); ok {
		return w
	}
	cpu, _ := parseQuantity(entry.CPU)
	mem, _ := parseQuantity(entry.MemoryRequest)
	w, err := capacityWarning(c, cpu, mem, entry)
	if err != nil {
		return ""
	}
	capacityCache.Add(key, w, capacityCacheTTL)
	return w
}

func capacityWarning(c *Client, cpu, mem float64, entry catalogModel) (string, error) {
	body, _, err := c.get("/api/v1/nodes")
	if err != nil {
		return "", err
	}
	var nodes struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
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
		return "", err
	}
	type room struct{ cpu, mem float64 }
	free := map[string]*room{}
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
		cc, ok1 := parseQuantity(n.Status.Allocatable["cpu"])
		mm, ok2 := parseQuantity(n.Status.Allocatable["memory"])
		if ok1 && ok2 {
			free[n.Metadata.Name] = &room{cc, mm}
		}
	}
	if len(free) == 0 {
		return fmt.Sprintf("No schedulable amd64 worker was found; the model needs one with %s CPUs and %s of memory free.", entry.CPU, entry.MemoryRequest), nil
	}
	body, _, err = c.get("/api/v1/pods?fieldSelector=" + url.QueryEscape("status.phase!=Succeeded,status.phase!=Failed"))
	if err != nil {
		return "", err
	}
	var pods struct {
		Items []struct {
			Spec struct {
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
		return "", err
	}
	for _, p := range pods.Items {
		r := free[p.Spec.NodeName]
		if r == nil || p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed" {
			continue
		}
		for _, ctr := range p.Spec.Containers {
			if v, ok := parseQuantity(ctr.Resources.Requests["cpu"]); ok {
				r.cpu -= v
			}
			if v, ok := parseQuantity(ctr.Resources.Requests["memory"]); ok {
				r.mem -= v
			}
		}
	}
	names := make([]string, 0, len(free))
	for n := range free {
		names = append(names, n)
	}
	sort.Strings(names)
	best := ""
	for _, n := range names {
		r := free[n]
		if r.cpu >= cpu && r.mem >= mem {
			return "", nil
		}
		if best == "" || r.cpu > free[best].cpu {
			best = n
		}
	}
	return fmt.Sprintf("No worker seems to have room for the model: it needs %s CPUs and %s of memory free (up to %s), and the most CPU free is %.1f CPUs and %.1f GiB on %s. It may stay pending until a node is added or other workloads stop; the setup still runs.",
		entry.CPU, entry.MemoryRequest, entry.Memory, maxFloat(free[best].cpu, 0), maxFloat(free[best].mem, 0)/(1<<30), best), nil
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// --- status -----------------------------------------------------------------

// planCache keeps the status's image plan for a minute (it reads the
// Subscription and the mirror sets).
var planCache = newLRU[string, imagePlan](8)

// getModelsStatus reports what a model setup needs and the catalog's
// models in the visible projects. Unknowns stay empty, never a block.
func getModelsStatus(c *Client, dsc dscRead, dscErr error, models []llmisvcObject, modelsErr error, visible map[string]bool) types.ModelsStatus {
	st := types.ModelsStatus{}
	for _, m := range modelCatalog {
		st.Catalog = append(st.Catalog, m.CatalogModel)
	}
	if dscErr == nil {
		st.Blocked, st.Dependencies = modelsBlock(dsc)
	}
	if st.Blocked == nil && errors.Is(modelsErr, errLLMISvcAPIMissing) {
		st.Blocked = &types.ComponentBlock{Component: "kserve", Reason: "Model serving is not installed yet: this cluster has no LLMInferenceService API. If KServe was just turned on, wait for the operator; otherwise check the Components page."}
	}
	if modelsErr == nil {
		st.Items = modelItems(c, models, visible)
	}
	if st.Blocked != nil {
		return st
	}
	preset := discoverCPUPreset(c, appsNamespace(c))
	st.Preset, st.PresetProblem = preset.name, preset.problem
	if preset.problem == "" {
		key := c.baseURL + "|" + preset.image
		plan, ok := planCache.Get(key)
		if !ok {
			var err error
			if plan, err = planModelImage(c, preset.image, false, nil, false); err == nil {
				planCache.Add(key, plan, capacityCacheTTL)
			}
		}
		st.ImagePlan, st.ImagePlanReason = plan.Kind, plan.Reason
	}
	st.CapacityWarning = modelCapacityWarning(c, modelCatalog[0])
	return st
}
