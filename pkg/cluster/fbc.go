package cluster

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
	"gopkg.in/yaml.v3"
)

var (
	fbcContentCache   = make(map[string]*types.FBCContentResponse)
	fbcContentCacheMu sync.RWMutex
)

const (
	maxLayerSize          = 50 * 1024 * 1024 // 50MB
	fbcCacheMaxEntries    = 100
)

type fbcEntry struct {
	Schema        string            `json:"schema" yaml:"schema"`
	Name          string            `json:"name" yaml:"name"`
	Package       string            `json:"package" yaml:"package"`
	RelatedImages []fbcRelatedImage `json:"relatedImages" yaml:"relatedImages"`
}

type fbcRelatedImage struct {
	Name  string `json:"name" yaml:"name"`
	Image string `json:"image" yaml:"image"`
}

// ExtractFBCContent downloads an FBC catalog image from Quay, parses its
// layers to find olm.bundle entries, and returns the relatedImages list.
func ExtractFBCContent(ctx context.Context, c *Client, imageRef string) (*types.FBCContentResponse, error) {
	_, digest, ok := extractRepoAndDigest(imageRef)
	if !ok {
		// Try parsing as tag-only reference (quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5-ea.2)
		digest = imageRef
	}

	fbcContentCacheMu.RLock()
	if cached, ok := fbcContentCache[digest]; ok {
		fbcContentCacheMu.RUnlock()
		return cached, nil
	}
	fbcContentCacheMu.RUnlock()

	basicAuth := getQuayAuth(c)
	if basicAuth == "" {
		slog.Debug("fbc: proceeding without quay auth (pull secret may be missing or invalid)")
	}
	bearerToken, err := getQuayBearerToken(ctx, quayHTTPClient, basicAuth)
	if err != nil {
		return nil, fmt.Errorf("quay auth: %w", err)
	}

	// Parse the image reference to get the manifest reference (tag or digest)
	manifestRef := extractManifestRef(imageRef)

	// Fetch the manifest
	manifestURL := fmt.Sprintf("https://quay.io/v2/rhoai/rhoai-fbc-fragment/manifests/%s", manifestRef)
	req, err := http.NewRequestWithContext(ctx, "GET", manifestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating manifest request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))

	resp, err := quayHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("manifest fetch returned HTTP %d", resp.StatusCode)
	}

	manifestBody, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}

	// Determine if this is an image index (multi-arch) or a single manifest
	var manifestData map[string]interface{}
	if err := json.Unmarshal(manifestBody, &manifestData); err != nil {
		return nil, fmt.Errorf("parsing manifest JSON: %w", err)
	}

	mediaType, _ := manifestData["mediaType"].(string)
	contentType := resp.Header.Get("Content-Type")

	var layerDigests []string

	if isImageIndex(mediaType, contentType) {
		// Resolve to amd64 platform manifest
		amd64Digest, err := resolveAmd64FromIndex(manifestData)
		if err != nil {
			return nil, fmt.Errorf("resolving amd64 manifest: %w", err)
		}

		layerDigests, err = fetchLayerDigests(ctx, quayHTTPClient, bearerToken, amd64Digest)
		if err != nil {
			return nil, fmt.Errorf("fetching amd64 layers: %w", err)
		}
	} else {
		layerDigests = extractLayerDigests(manifestData)
	}

	if len(layerDigests) == 0 {
		return nil, fmt.Errorf("no layers found in FBC image")
	}

	// Extract tag from image reference for bundle matching
	tag := extractTagFromRef(imageRef)

	// Download and parse each layer looking for FBC catalog content
	var allRelatedImages []fbcRelatedImage
	var bundleName string

	for _, layerDigest := range layerDigests {
		images, bn, err := downloadAndParseLayer(ctx, quayHTTPClient, bearerToken, layerDigest, tag)
		if err != nil {
			slog.Warn("failed to parse FBC layer", "digest", layerDigest[:20], "error", err)
			continue
		}
		allRelatedImages = append(allRelatedImages, images...)
		if bn != "" {
			bundleName = bn
		}
	}

	// Deduplicate by image reference and categorize
	seen := make(map[string]bool)
	var deduped []types.RelatedImage
	for _, ri := range allRelatedImages {
		if seen[ri.Image] {
			continue
		}
		seen[ri.Image] = true
		deduped = append(deduped, types.RelatedImage{
			Name:     ri.Name,
			Image:    ri.Image,
			Category: categorizeImage(ri.Name, ri.Image),
		})
	}

	// Sort: core first, then by name
	sort.Slice(deduped, func(i, j int) bool {
		ci, cj := categoryOrder(deduped[i].Category), categoryOrder(deduped[j].Category)
		if ci != cj {
			return ci < cj
		}
		return deduped[i].Name < deduped[j].Name
	})

	// Count images per category
	categories := make(map[string]int)
	for _, ri := range deduped {
		categories[ri.Category]++
	}

	result := &types.FBCContentResponse{
		Tag:           tag,
		Image:         imageRef,
		BundleName:    bundleName,
		RelatedImages: deduped,
		Categories:    categories,
	}

	fbcContentCacheMu.Lock()
	if len(fbcContentCache) >= fbcCacheMaxEntries {
		evictFBCCache()
	}
	fbcContentCache[digest] = result
	fbcContentCacheMu.Unlock()

	return result, nil
}

// evictFBCCache removes random entries to bring the cache down to 75% capacity.
// Must be called while fbcContentCacheMu is held for writing.
func evictFBCCache() {
	targetSize := fbcCacheMaxEntries * 3 / 4 // 75% of max
	toRemove := len(fbcContentCache) - targetSize
	if toRemove <= 0 {
		return
	}

	// Collect keys and shuffle to pick random victims
	keys := make([]string, 0, len(fbcContentCache))
	for k := range fbcContentCache {
		keys = append(keys, k)
	}

	// Fisher-Yates partial shuffle: only need toRemove random picks
	for i := 0; i < toRemove && i < len(keys); i++ {
		j := i + rand.Intn(len(keys)-i)
		keys[i], keys[j] = keys[j], keys[i]
		delete(fbcContentCache, keys[i])
	}
}

// ResolveRelatedImageLabels enriches related images with git commit info from Quay image labels.
// It skips the GitHub API call for commit merge dates to avoid rate limit exhaustion (60/hr).
func ResolveRelatedImageLabels(ctx context.Context, c *Client, images []types.RelatedImage) []types.RelatedImage {
	type labelResult struct {
		index  int
		labels *ImageLabels
	}

	results := make(chan labelResult, len(images))
	sem := make(chan struct{}, 5)

	for i, img := range images {
		if !isRHOAIImage(img.Image) {
			results <- labelResult{index: i, labels: nil}
			continue
		}
		go func(idx int, ref string) {
			sem <- struct{}{}
			defer func() { <-sem }()

			imageCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()

			labels, err := GetImageLabels(imageCtx, c, ref)
			if err != nil {
				slog.Warn("failed to fetch image labels", "image", truncateForLog(ref), "error", err)
				results <- labelResult{index: idx, labels: nil}
				return
			}
			results <- labelResult{index: idx, labels: labels}
		}(i, img.Image)
	}

	for range images {
		res := <-results
		if res.labels == nil {
			continue
		}
		images[res.index].GitCommit = res.labels.GitCommit
		images[res.index].GitURL = res.labels.GitURL
		// Skip CommitDate — it comes from GitHub API (60/hr rate limit)
		images[res.index].BuildDate = res.labels.BuildDate
		images[res.index].Version = res.labels.Version
	}

	return images
}

func isImageIndex(mediaType, contentType string) bool {
	for _, s := range []string{mediaType, contentType} {
		if strings.Contains(s, "image.index") || strings.Contains(s, "manifest.list") {
			return true
		}
	}
	return false
}

func resolveAmd64FromIndex(indexData map[string]interface{}) (string, error) {
	manifests, ok := indexData["manifests"].([]interface{})
	if !ok || len(manifests) == 0 {
		return "", fmt.Errorf("no manifests in image index")
	}

	for _, m := range manifests {
		mObj, _ := m.(map[string]interface{})
		platform, _ := mObj["platform"].(map[string]interface{})
		arch, _ := platform["architecture"].(string)
		os, _ := platform["os"].(string)
		if arch == "amd64" && (os == "" || os == "linux") {
			digest, _ := mObj["digest"].(string)
			if digest != "" {
				return digest, nil
			}
		}
	}

	// Fallback: use first manifest
	first, _ := manifests[0].(map[string]interface{})
	digest, _ := first["digest"].(string)
	if digest != "" {
		return digest, nil
	}
	return "", fmt.Errorf("no amd64 manifest found in index")
}

func fetchLayerDigests(ctx context.Context, client *http.Client, token, manifestDigest string) ([]string, error) {
	url := fmt.Sprintf("https://quay.io/v2/rhoai/rhoai-fbc-fragment/manifests/%s", manifestDigest)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	if err != nil {
		return nil, err
	}

	var manifest map[string]interface{}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, err
	}

	return extractLayerDigests(manifest), nil
}

func extractLayerDigests(manifest map[string]interface{}) []string {
	layers, ok := manifest["layers"].([]interface{})
	if !ok {
		return nil
	}
	var digests []string
	for _, l := range layers {
		lObj, _ := l.(map[string]interface{})
		digest, _ := lObj["digest"].(string)
		if digest != "" {
			digests = append(digests, digest)
		}
	}
	return digests
}

func downloadAndParseLayer(ctx context.Context, client *http.Client, token, layerDigest, targetTag string) ([]fbcRelatedImage, string, error) {
	url := fmt.Sprintf("https://quay.io/v2/rhoai/rhoai-fbc-fragment/blobs/%s", layerDigest)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("layer download returned HTTP %d", resp.StatusCode)
	}

	limitedReader := io.LimitReader(resp.Body, maxLayerSize)

	// Try gzip first, fall back to raw tar
	gzReader, err := gzip.NewReader(limitedReader)
	if err != nil {
		return nil, "", fmt.Errorf("gzip decompress: %w", err)
	}
	defer gzReader.Close()

	tarReader := tar.NewReader(gzReader)

	var allImages []fbcRelatedImage
	var bundleName string

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}

		if header.Typeflag != tar.TypeReg {
			continue
		}

		// Look for catalog JSON files
		name := strings.TrimPrefix(header.Name, "./")
		if !isCatalogFile(name) {
			continue
		}

		content, err := io.ReadAll(io.LimitReader(tarReader, 10*1024*1024))
		if err != nil {
			continue
		}

		images, bn := parseFBCContent(content, targetTag)
		allImages = append(allImages, images...)
		if bn != "" {
			bundleName = bn
		}
	}

	return allImages, bundleName, nil
}

func isCatalogFile(path string) bool {
	if strings.HasPrefix(path, "configs/") || strings.HasPrefix(path, "catalog/") {
		return strings.HasSuffix(path, ".json") || strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")
	}
	return false
}

func parseFBCContent(content []byte, targetTag string) ([]fbcRelatedImage, string) {
	var allImages []fbcRelatedImage
	var bundleName string

	// Try YAML multi-document first (split on "---")
	if parseYAMLFBC(content, targetTag, &allImages, &bundleName) {
		return allImages, bundleName
	}

	// Fallback: try newline-delimited JSON
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry fbcEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry.Schema == "olm.bundle" && len(entry.RelatedImages) > 0 {
			allImages = append(allImages, entry.RelatedImages...)
			if entry.Name != "" {
				bundleName = entry.Name
			}
		}
	}

	return allImages, bundleName
}

func parseYAMLFBC(content []byte, targetTag string, allImages *[]fbcRelatedImage, bundleName *string) bool {
	var bundles []fbcBundleInfo

	decoder := yaml.NewDecoder(strings.NewReader(string(content)))
	for {
		var entry fbcEntry
		err := decoder.Decode(&entry)
		if err != nil {
			break
		}
		if entry.Schema == "olm.bundle" && len(entry.RelatedImages) > 0 {
			bundles = append(bundles, fbcBundleInfo{name: entry.Name, images: entry.RelatedImages})
		}
	}

	if len(bundles) == 0 {
		return false
	}

	// Match bundle to the tag version. Tag "rhoai-3.5-ea.2" → match "rhods-operator.3.5*-ea.2"
	// Tag "rhoai-3.4" → match "rhods-operator.3.4.*" (highest patch)
	best := findMatchingBundle(bundles, targetTag)
	if best == nil {
		// Fallback: use the bundle with the highest version
		best = &bundles[0]
		for i := range bundles {
			if bundles[i].name > best.name {
				best = &bundles[i]
			}
		}
	}

	*bundleName = best.name
	for _, img := range best.images {
		if strings.HasSuffix(img.Name, "-annotation") {
			continue
		}
		*allImages = append(*allImages, img)
	}
	return true
}

func findMatchingBundle(bundles []fbcBundleInfo, tag string) *fbcBundleInfo {
	// Extract major.minor and optional EA from tag
	// "rhoai-3.5-ea.2" → major=3, minor=5, ea=2
	// "rhoai-3.4" → major=3, minor=4, ea=-1
	m := regexp.MustCompile(`^rhoai-(\d+)\.(\d+)(?:\.\d+)?(?:-ea\.(\d+))?$`).FindStringSubmatch(tag)
	if m == nil {
		return nil
	}
	major, minor := m[1], m[2]
	eaSuffix := ""
	if m[3] != "" {
		eaSuffix = "-ea." + m[3]
	}

	// Find all bundles matching this major.minor
	prefix := fmt.Sprintf("rhods-operator.%s.%s", major, minor)
	var candidates []*fbcBundleInfo
	for i := range bundles {
		name := bundles[i].name
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		// Check EA match: if tag is EA, bundle must have same EA suffix
		// If tag is GA, bundle must NOT have -ea.
		if eaSuffix != "" {
			if strings.Contains(name, eaSuffix) {
				candidates = append(candidates, &bundles[i])
			}
		} else {
			if !strings.Contains(name, "-ea.") {
				candidates = append(candidates, &bundles[i])
			}
		}
	}

	if len(candidates) == 0 {
		return nil
	}

	// Pick the highest version among candidates
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c.name > best.name {
			best = c
		}
	}
	return best
}

type fbcBundleInfo struct {
	name   string
	images []fbcRelatedImage
}

// extractManifestRef gets the tag or digest portion from a full image reference.
// e.g., "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5-ea.2@sha256:abc..." → "sha256:abc..."
// e.g., "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5-ea.2" → "rhoai-3.5-ea.2"
func extractManifestRef(imageRef string) string {
	if atIdx := strings.LastIndex(imageRef, "@"); atIdx >= 0 {
		return imageRef[atIdx+1:]
	}
	if colonIdx := strings.LastIndex(imageRef, ":"); colonIdx >= 0 {
		return imageRef[colonIdx+1:]
	}
	return imageRef
}

// extractTagFromRef gets the tag from a full image reference.
func extractTagFromRef(imageRef string) string {
	ref := imageRef
	if atIdx := strings.Index(ref, "@"); atIdx >= 0 {
		ref = ref[:atIdx]
	}
	if colonIdx := strings.LastIndex(ref, ":"); colonIdx >= 0 {
		return ref[colonIdx+1:]
	}
	return ""
}

// coreComponentSuffixes lists the image name suffixes that correspond to
// actual deployed controllers, operators, and services. Everything else
// is a runtime image (workbench, pipeline, training) or infra dependency.
var coreComponentSuffixes = []string{
	"dashboard",
	"operator",
	"controller",
	"scheduler",
	"manager",
	"server",
	"service",
	"gateway",
	"agent",
	"api",
}

func categorizeImage(name, image string) string {
	n := strings.ToLower(name)

	if !strings.Contains(image, "rhoai/") && !strings.Contains(image, "rhaii/") {
		return "infra"
	}

	// Bundle image itself
	if name == "" || strings.Contains(image, "operator-bundle") {
		return "other"
	}

	// Module architecture definitions
	if strings.HasPrefix(n, "odh_mod_arch_") {
		return "other"
	}

	// Workbench/notebook images
	if strings.Contains(n, "workbench") || strings.Contains(n, "th06_") || strings.Contains(n, "notebook") {
		return "workbench"
	}

	// Pipeline runtime images
	if strings.HasPrefix(n, "odh_pipeline_runtime_") {
		return "pipeline"
	}

	// Training runtime images (cuda/rocm variants)
	if strings.HasPrefix(n, "odh_training_cuda") || strings.HasPrefix(n, "odh_training_rocm") {
		return "training"
	}

	// Serving runtime images
	if strings.Contains(n, "vllm") || strings.Contains(n, "mlserver") || strings.Contains(n, "openvino") {
		return "runtime"
	}

	// Core: only images whose name contains a controller/operator/service suffix
	for _, suffix := range coreComponentSuffixes {
		if strings.Contains(n, suffix) {
			return "core"
		}
	}

	return "other"
}

func categoryOrder(cat string) int {
	switch cat {
	case "core":
		return 0
	case "runtime":
		return 1
	case "workbench":
		return 2
	case "pipeline":
		return 3
	case "training":
		return 4
	case "infra":
		return 5
	default:
		return 6
	}
}
