package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
	"gopkg.in/yaml.v3"
)

// fbcContentCache holds parsed catalogs of digest-pinned FBC images. The
// content of a digest never changes, so entries leave only by LRU eviction.
var (
	FBCContentCache  = kube.NewLRU[string, *types.FBCContentResponse](FBCCacheMaxEntries)
	fbcContentFlight kube.FlightGroup[*types.FBCContentResponse]
)

const (
	maxLayerSize       = 50 * 1024 * 1024 // compressed layer bytes
	maxCatalogFileSize = 10 * 1024 * 1024 // per catalog file
	FBCCacheMaxEntries = 100
)

type fbcEntry struct {
	Schema        string            `json:"schema" yaml:"schema"`
	Name          string            `json:"name" yaml:"name"`
	Package       string            `json:"package" yaml:"package"`
	RelatedImages []FBCRelatedImage `json:"relatedImages" yaml:"relatedImages"`
}

type FBCRelatedImage struct {
	Name  string `json:"name" yaml:"name"`
	Image string `json:"image" yaml:"image"`
}

// FBCCacheKey returns the cache key of a digest-pinned reference. The tag is
// part of the key because it selects the bundle to show.
func FBCCacheKey(imageRef string) (string, bool) {
	_, digest, ok := RepoAndDigest(imageRef)
	return digest + "|" + TagFromRef(imageRef), ok
}

// CachedFBCContent returns a parsed catalog without any network call.
func CachedFBCContent(imageRef string) (*types.FBCContentResponse, bool) {
	key, ok := FBCCacheKey(imageRef)
	if !ok {
		return nil, false
	}
	return FBCContentCache.Get(key)
}

// ExtractFBCContent downloads an FBC catalog image from Quay, parses its
// layers to find olm.bundle entries, and returns the relatedImages list.
// The returned value is shared: callers must copy RelatedImages before changing it.
func ExtractFBCContent(ctx context.Context, c *kube.Client, imageRef string) (*types.FBCContentResponse, error) {
	// Only digest-pinned references are cached: a tag can move to a new build.
	if cached, ok := CachedFBCContent(imageRef); ok {
		return cached, nil
	}
	return ExtractFBCContentWithAuth(ctx, QuayAuth(c), imageRef)
}

func ExtractFBCContentWithAuth(ctx context.Context, basicAuth, imageRef string) (*types.FBCContentResponse, error) {
	key, cacheable := FBCCacheKey(imageRef)
	if !cacheable {
		return downloadFBCContent(ctx, basicAuth, imageRef)
	}
	if cached, ok := FBCContentCache.Get(key); ok {
		return cached, nil
	}
	// Two tabs opening the same build download its catalog once.
	result, err, _ := fbcContentFlight.Do(ctx, key, func(ctx context.Context) (*types.FBCContentResponse, error) {
		if cached, ok := FBCContentCache.Get(key); ok {
			return cached, nil
		}
		result, err := downloadFBCContent(ctx, basicAuth, imageRef)
		if err != nil {
			return nil, err
		}
		FBCContentCache.Add(key, result, 0)
		return result, nil
	})
	return result, err
}

func downloadFBCContent(ctx context.Context, basicAuth, imageRef string) (*types.FBCContentResponse, error) {
	if basicAuth == "" {
		slog.Debug("fbc: proceeding without quay auth (pull secret may be missing or invalid)")
	}
	bearerToken, err := CachedQuayToken(ctx, basicAuth, FBCRepo)
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

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Do not keep reusing a token the registry rejected.
		forgetQuayToken(basicAuth, FBCRepo)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("manifest fetch returned HTTP %d", resp.StatusCode)
	}

	manifestBody, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}

	// Determine if this is an image index (multi-arch) or a single manifest
	var manifestData map[string]any
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
	tag := TagFromRef(imageRef)

	// Download and parse layers in reverse order (catalog content is typically
	// in the last layer, added on top of a base image during build)
	var allRelatedImages []FBCRelatedImage
	var bundleName string
	var layerErr error

	for _, layerDigest := range slices.Backward(layerDigests) {
		images, bn, err := DownloadAndParseLayer(ctx, quayHTTPClient, bearerToken, layerDigest, tag)
		if err != nil {
			slog.Warn("failed to parse FBC layer", "digest", layerDigest, "error", err)
			layerErr = err
			continue
		}
		allRelatedImages = append(allRelatedImages, images...)
		if bn != "" {
			bundleName = bn
		}
		if bundleName != "" && len(allRelatedImages) > 0 {
			break
		}
	}

	if bundleName == "" && len(allRelatedImages) == 0 && layerErr != nil {
		// Report the failure instead of an apparently empty catalog.
		return nil, fmt.Errorf("reading FBC layers: %w", layerErr)
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

	return &types.FBCContentResponse{
		Tag:           tag,
		Image:         imageRef,
		BundleName:    bundleName,
		RelatedImages: deduped,
		Categories:    categories,
	}, nil
}

// ResolveRelatedImageLabels enriches related images with git commit info from Quay image labels.
// The pull secret is read once for the whole batch. It skips the GitHub API
// call for commit merge dates to avoid rate limit exhaustion (60/hr).
func ResolveRelatedImageLabels(ctx context.Context, c *kube.Client, images []types.RelatedImage) []types.RelatedImage {
	refs := make([]string, 0, len(images))
	for _, img := range images {
		if IsRHOAIImage(img.Image) {
			refs = append(refs, img.Image)
		}
	}
	if len(refs) == 0 {
		return images
	}
	labels := ResolveImageLabels(ctx, QuayAuth(c), refs, false)
	for i := range images {
		l := labels[images[i].Image]
		if l == nil {
			continue
		}
		images[i].GitCommit = l.GitCommit
		images[i].GitURL = l.GitURL
		// Skip CommitDate — it comes from GitHub API (60/hr rate limit)
		images[i].BuildDate = l.BuildDate
		images[i].Version = l.Version
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

func resolveAmd64FromIndex(indexData map[string]any) (string, error) {
	manifests, ok := indexData["manifests"].([]any)
	if !ok || len(manifests) == 0 {
		return "", fmt.Errorf("no manifests in image index")
	}

	for _, m := range manifests {
		mObj, _ := m.(map[string]any)
		platform, _ := mObj["platform"].(map[string]any)
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
	first, _ := manifests[0].(map[string]any)
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

	var manifest map[string]any
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, err
	}

	return extractLayerDigests(manifest), nil
}

func extractLayerDigests(manifest map[string]any) []string {
	layers, ok := manifest["layers"].([]any)
	if !ok {
		return nil
	}
	var digests []string
	for _, l := range layers {
		lObj, _ := l.(map[string]any)
		digest, _ := lObj["digest"].(string)
		if digest != "" {
			digests = append(digests, digest)
		}
	}
	return digests
}

func DownloadAndParseLayer(ctx context.Context, client *http.Client, token, layerDigest, targetTag string) ([]FBCRelatedImage, string, error) {
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

	// Fail rather than parse a silently truncated layer or catalog file.
	gzReader, err := gzip.NewReader(&SizeLimitedReader{R: resp.Body, Remaining: maxLayerSize, What: "layer"})
	if err != nil {
		return nil, "", fmt.Errorf("gzip decompress: %w", err)
	}
	defer gzReader.Close()

	tarReader := tar.NewReader(gzReader)

	var allImages []FBCRelatedImage
	var bundleName string

	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("read layer archive: %w", err)
		}

		if header.Typeflag != tar.TypeReg {
			continue
		}

		// Look for catalog JSON files
		name := strings.TrimPrefix(header.Name, "./")
		if !isCatalogFile(name) {
			continue
		}

		content, err := io.ReadAll(&SizeLimitedReader{R: tarReader, Remaining: maxCatalogFileSize, What: "catalog file " + name})
		if err != nil {
			return nil, "", err
		}

		images, bn := ParseFBCContent(content, targetTag)
		allImages = append(allImages, images...)
		if bn != "" {
			bundleName = bn
		}
	}

	return allImages, bundleName, nil
}

// SizeLimitedReader reads at most remaining bytes and returns an error, rather
// than a silent EOF, when the underlying stream is longer.
type SizeLimitedReader struct {
	R         io.Reader
	Remaining int64
	What      string
}

func (l *SizeLimitedReader) Read(p []byte) (int, error) {
	if l.Remaining <= 0 {
		var probe [1]byte
		n, err := l.R.Read(probe[:])
		if n > 0 {
			return 0, fmt.Errorf("%s exceeds the size limit", l.What)
		}
		return 0, err
	}
	if int64(len(p)) > l.Remaining {
		p = p[:l.Remaining]
	}
	n, err := l.R.Read(p)
	l.Remaining -= int64(n)
	return n, err
}

func isCatalogFile(path string) bool {
	if strings.HasPrefix(path, "configs/") || strings.HasPrefix(path, "catalog/") {
		return strings.HasSuffix(path, ".json") || strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")
	}
	return false
}

// ParseFBCContent returns the relatedImages of the bundle that matches
// targetTag in one catalog file. File-based catalogs are a stream of objects
// written as YAML documents or as JSON (one per line or pretty-printed and
// concatenated, the default output of opm render).
func ParseFBCContent(content []byte, targetTag string) ([]FBCRelatedImage, string) {
	bundles := decodeFBCBundles(content)
	if len(bundles) == 0 {
		return nil, ""
	}

	// Match bundle to the tag version. Tag "rhoai-3.5-ea.2" → match "rhods-operator.3.5*-ea.2"
	// Tag "rhoai-3.4" → match "rhods-operator.3.4.*" (highest patch)
	best := FindMatchingBundle(bundles, targetTag)
	if best == nil {
		best = highestBundle(bundles)
	}

	var images []FBCRelatedImage
	for _, img := range best.Images {
		if strings.HasSuffix(img.Name, "-annotation") {
			continue
		}
		images = append(images, img)
	}
	return images, best.Name
}

// decodeFBCBundles extracts olm.bundle entries that list relatedImages.
func decodeFBCBundles(content []byte) []FBCBundle {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 {
		return nil
	}
	decoders := []func([]byte) []fbcEntry{decodeYAMLEntries, decodeJSONEntries}
	if trimmed[0] == '{' {
		decoders = []func([]byte) []fbcEntry{decodeJSONEntries, decodeYAMLEntries}
	}
	for _, decode := range decoders {
		var bundles []FBCBundle
		for _, entry := range decode(trimmed) {
			if entry.Schema == "olm.bundle" && len(entry.RelatedImages) > 0 {
				bundles = append(bundles, FBCBundle{Name: entry.Name, Images: entry.RelatedImages})
			}
		}
		if len(bundles) > 0 {
			return bundles
		}
	}
	return nil
}

func decodeYAMLEntries(content []byte) []fbcEntry {
	var entries []fbcEntry
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	for {
		var entry fbcEntry
		if err := decoder.Decode(&entry); err != nil {
			break
		}
		entries = append(entries, entry)
	}
	return entries
}

// decodeJSONEntries reads a stream of JSON objects. If the stream is malformed
// it falls back to decoding each line separately, skipping bad lines.
func decodeJSONEntries(content []byte) []fbcEntry {
	var entries []fbcEntry
	decoder := json.NewDecoder(bytes.NewReader(content))
	for {
		var entry fbcEntry
		err := decoder.Decode(&entry)
		if err == io.EOF {
			return entries
		}
		if err != nil {
			break
		}
		entries = append(entries, entry)
	}
	var lineEntries []fbcEntry
	for line := range strings.SplitSeq(string(content), "\n") {
		var entry fbcEntry
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &entry) == nil {
			lineEntries = append(lineEntries, entry)
		}
	}
	if len(lineEntries) > len(entries) {
		return lineEntries
	}
	return entries
}

// highestBundle returns the bundle with the highest operator version, using
// the name as a tiebreaker and for names that do not parse as versions.
func highestBundle(bundles []FBCBundle) *FBCBundle {
	best := &bundles[0]
	bestVersion, bestOK := ParseCSVVersion(best.Name)
	for i := 1; i < len(bundles); i++ {
		version, ok := ParseCSVVersion(bundles[i].Name)
		var better bool
		switch {
		case ok != bestOK:
			better = ok // a parseable version beats an unparseable name
		case ok && CompareTags(version, bestVersion) != 0:
			better = CompareTags(version, bestVersion) > 0
		default:
			better = bundles[i].Name > best.Name
		}
		if better {
			best, bestVersion, bestOK = &bundles[i], version, ok
		}
	}
	return best
}

func FindMatchingBundle(bundles []FBCBundle, tag string) *FBCBundle {
	target, ok := ParseTag(tag)
	if !ok {
		return nil
	}
	var best *FBCBundle
	var bestVersion Tag
	for i := range bundles {
		version, ok := ParseCSVVersion(bundles[i].Name)
		if !ok || version.Major != target.Major || version.Minor != target.Minor || version.EA != target.EA {
			continue
		}
		if best == nil || CompareTags(version, bestVersion) > 0 {
			best = &bundles[i]
			bestVersion = version
		}
	}
	return best
}

type FBCBundle struct {
	Name   string
	Images []FBCRelatedImage
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

// TagFromRef gets the tag from a full image reference.
func TagFromRef(imageRef string) string {
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
