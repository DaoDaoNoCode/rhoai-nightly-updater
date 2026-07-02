package cluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	quayAuthURL = "https://quay.io/v2/auth?service=quay.io&scope=repository:rhoai/rhoai-fbc-fragment:pull"
	quayTagsURL = "https://quay.io/v2/rhoai/rhoai-fbc-fragment/tags/list"
	quayImage   = "quay.io/rhoai/rhoai-fbc-fragment"
)

// quayHTTPClient is a shared HTTP client for all Quay/GitHub API calls,
// avoiding per-request client allocation and enabling connection reuse.
var quayHTTPClient = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	},
}

// Matches clean version tags:
//   rhoai-3.4, rhoai-3.5.0, rhoai-3.5.0-ea.1, rhoai-3.5-ea.2
var cleanTagRegex = regexp.MustCompile(`^rhoai-\d+\.\d+(\.\d+)?(-ea\.\d+)?$`)
var tagParseRegex = regexp.MustCompile(`^rhoai-(\d+)\.(\d+)(?:\.(\d+))?(?:-ea\.(\d+))?$`)

type parsedTag struct {
	raw   string
	major int
	minor int
	patch int
	ea    int // -1 = GA (no suffix), 0+ = EA number
}

func parseTag(tag string) (parsedTag, bool) {
	m := tagParseRegex.FindStringSubmatch(tag)
	if m == nil {
		return parsedTag{}, false
	}
	major, minor, patch := 0, 0, 0
	ea := -1
	fmt.Sscanf(m[1], "%d", &major)
	fmt.Sscanf(m[2], "%d", &minor)
	if m[3] != "" {
		fmt.Sscanf(m[3], "%d", &patch)
	}
	if m[4] != "" {
		fmt.Sscanf(m[4], "%d", &ea)
	}
	return parsedTag{raw: tag, major: major, minor: minor, patch: patch, ea: ea}, true
}

// Sort order: 3.4.0-ea.1 < 3.4.0-ea.2 < 3.4.0 (GA) < 3.5.0-ea.1 < 3.5.0 (GA)
func compareTags(a, b parsedTag) int {
	if a.major != b.major {
		return a.major - b.major
	}
	if a.minor != b.minor {
		return a.minor - b.minor
	}
	if a.patch != b.patch {
		return a.patch - b.patch
	}
	// Same version: GA (ea=-1) is greater than any EA
	if a.ea == -1 && b.ea == -1 {
		return 0
	}
	if a.ea == -1 {
		return 1
	}
	if b.ea == -1 {
		return -1
	}
	return a.ea - b.ea
}

type quayTokenResponse struct {
	Token string `json:"token"`
}

type quayTagsResponse struct {
	Tags []string `json:"tags"`
}

// getQuayAuth extracts the quay.io/rhoai auth credential from the cluster pull secret.
func getQuayAuth(c *Client) string {
	path := namespacedPath("v1", "secrets", "kube-system", "additional-pull-secret")
	body, _, err := c.get(path)
	if err != nil {
		slog.Warn("quay auth: failed to read pull secret", "error", err)
		return ""
	}

	var secret map[string]interface{}
	if err := json.Unmarshal(body, &secret); err != nil {
		slog.Warn("quay auth: failed to parse pull secret JSON", "error", err)
		return ""
	}

	data, _ := secret["data"].(map[string]interface{})
	dockerCfgB64, _ := data[".dockerconfigjson"].(string)
	if dockerCfgB64 == "" {
		slog.Warn("quay auth: pull secret has no .dockerconfigjson data")
		return ""
	}

	decodedBytes, err := base64.StdEncoding.DecodeString(dockerCfgB64)
	if err != nil {
		slog.Warn("quay auth: failed to decode .dockerconfigjson base64", "error", err)
		return ""
	}

	var dockerCfg map[string]interface{}
	if err := json.Unmarshal(decodedBytes, &dockerCfg); err != nil {
		slog.Warn("quay auth: failed to parse docker config JSON", "error", err)
		return ""
	}

	auths, _ := dockerCfg["auths"].(map[string]interface{})
	for key, val := range auths {
		if key == "quay.io/rhoai" || key == "quay.io" {
			entry, _ := val.(map[string]interface{})
			auth, _ := entry["auth"].(string)
			if auth != "" {
				return auth
			}
		}
	}
	slog.Warn("quay auth: no quay.io/rhoai or quay.io entry found in pull secret auths")
	return ""
}

func getQuayBearerToken(ctx context.Context, httpClient *http.Client, basicAuth string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", quayAuthURL, nil)
	if err != nil {
		return "", fmt.Errorf("creating auth request: %w", err)
	}
	if basicAuth != "" {
		req.Header.Set("Authorization", "Basic "+basicAuth)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("quay auth request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return "", fmt.Errorf("quay auth returned %d: %s", resp.StatusCode, string(body))
	}

	var tokenData quayTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenData); err != nil {
		return "", fmt.Errorf("decoding auth response: %w", err)
	}
	return tokenData.Token, nil
}

func getTagDigest(ctx context.Context, httpClient *http.Client, bearerToken, tag string) (string, error) {
	url := fmt.Sprintf("https://quay.io/v2/rhoai/rhoai-fbc-fragment/manifests/%s", tag)
	req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.v2+json,application/vnd.docker.distribution.manifest.list.v2+json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("manifest HEAD returned %d", resp.StatusCode)
	}

	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		return "", fmt.Errorf("no digest in response headers")
	}
	return digest, nil
}

// FetchNightlyTags returns the top `limit` nightly version tags (sorted descending),
// each with its full image reference including digest.
func FetchNightlyTags(ctx context.Context, c *Client, limit int) (*types.NightlyTagsResponse, error) {
	if limit < 0 {
		limit = 5
	}

	quayAuth := getQuayAuth(c)
	bearerToken, err := getQuayBearerToken(ctx, quayHTTPClient, quayAuth)
	if err != nil {
		return nil, err
	}

	parsed, err := fetchAndParseTags(ctx, quayHTTPClient, bearerToken)
	if err != nil {
		return nil, err
	}

	// Take the top N tags (already sorted ascending, so take from the end)
	var topTags []parsedTag
	if limit == 0 {
		topTags = parsed
	} else {
		start := len(parsed) - limit
		if start < 0 {
			start = 0
		}
		topTags = parsed[start:]
	}

	// Build response in descending order (newest first)
	tags := make([]types.NightlyTag, 0, len(topTags))
	for i := len(topTags) - 1; i >= 0; i-- {
		tag := topTags[i].raw
		digest, err := getTagDigest(ctx, quayHTTPClient, bearerToken, tag)
		var image string
		if err != nil {
			image = fmt.Sprintf("%s:%s", quayImage, tag)
		} else {
			image = fmt.Sprintf("%s:%s@%s", quayImage, tag, digest)
		}
		tags = append(tags, types.NightlyTag{Tag: tag, Image: image})
	}

	return &types.NightlyTagsResponse{Tags: tags}, nil
}

// EnrichTagsWithBuildDates fetches the OCI image config "created" timestamp for each tag in parallel.
func EnrichTagsWithBuildDates(ctx context.Context, c *Client, tags []types.NightlyTag) {
	type result struct {
		index int
		date  string
	}
	results := make(chan result, len(tags))
	sem := make(chan struct{}, 10)

	for i, t := range tags {
		go func(idx int, imageRef string) {
			sem <- struct{}{}
			defer func() { <-sem }()

			imgCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()

			labels, err := GetImageLabels(imgCtx, c, imageRef)
			if err != nil || labels == nil {
				results <- result{idx, ""}
				return
			}
			results <- result{idx, labels.BuildDate}
		}(i, t.Image)
	}

	for range tags {
		r := <-results
		if r.date != "" {
			tags[r.index].BuildDate = r.date
		}
	}
}

// tagScanCache caches the full tag scan result (tags change at most once/day).
var (
	tagScanCache     []parsedTag
	tagScanCacheMu   sync.RWMutex
	tagScanCacheAt   time.Time
	tagScanRefreshMu sync.Mutex // serializes cache refresh to prevent stampede
)

const tagScanCacheTTL = 5 * time.Minute

// fetchAndParseTags scans the Quay v2 tags/list API to find all clean version tags.
// It runs concurrent paginations from multiple lexicographic start points to cover
// the full range without scanning every page sequentially. Results are cached for 5m.
// A refresh mutex prevents multiple goroutines from performing the expensive scan
// simultaneously (cache stampede).
func fetchAndParseTags(ctx context.Context, httpClient *http.Client, bearerToken string) ([]parsedTag, error) {
	tagScanCacheMu.RLock()
	if len(tagScanCache) > 0 && time.Since(tagScanCacheAt) < tagScanCacheTTL {
		result := make([]parsedTag, len(tagScanCache))
		copy(result, tagScanCache)
		tagScanCacheMu.RUnlock()
		return result, nil
	}
	tagScanCacheMu.RUnlock()

	// Serialize refreshes: only one goroutine does the actual fetch while others wait.
	tagScanRefreshMu.Lock()
	defer tagScanRefreshMu.Unlock()

	// Double-check: another goroutine may have refreshed while we waited for the lock.
	tagScanCacheMu.RLock()
	if len(tagScanCache) > 0 && time.Since(tagScanCacheAt) < tagScanCacheTTL {
		result := make([]parsedTag, len(tagScanCache))
		copy(result, tagScanCache)
		tagScanCacheMu.RUnlock()
		return result, nil
	}
	tagScanCacheMu.RUnlock()

	// Start concurrent paginations from evenly-spaced points across the
	// rhoai- lexicographic range. Each goroutine scans up to 30 pages (3000 tags).
	// This covers ~20k+ tags in parallel instead of sequentially.
	startPoints := []string{
		"rhoai-",     // catches everything from the start
		"rhoai-2.2",  // jumps past early 2.x noise
		"rhoai-2.9",  // jumps to the 2.9/3.0 boundary
		"rhoai-3.2",  // 3.2+ range
		"rhoai-3.4",  // 3.4+ range
		"rhoai-3.5",  // 3.5+ range (catches EA tags)
		"rhoai-3.9",  // future versions
	}

	type scanResult struct {
		tags []string
		err  error
	}
	results := make(chan scanResult, len(startPoints))

	for _, start := range startPoints {
		go func(startTag string) {
			var found []string
			lastTag := startTag
			for page := 0; page < 30; page++ {
				url := fmt.Sprintf("%s?n=100&last=%s", quayTagsURL, lastTag)
				req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
				if err != nil {
					break
				}
				req.Header.Set("Authorization", "Bearer "+bearerToken)
				req.Header.Set("Accept", "application/json")
				resp, err := httpClient.Do(req)
				if err != nil {
					break
				}
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				if resp.StatusCode != 200 {
					break
				}
				var data quayTagsResponse
				if json.Unmarshal(body, &data) != nil || len(data.Tags) == 0 {
					break
				}
				pastRange := false
				for _, tag := range data.Tags {
					if cleanTagRegex.MatchString(tag) {
						found = append(found, tag)
					}
					if tag > "rhoai." {
						pastRange = true
						break
					}
				}
				if pastRange {
					break
				}
				lastTag = data.Tags[len(data.Tags)-1]
			}
			results <- scanResult{tags: found}
		}(start)
	}

	seen := make(map[string]bool)
	var allCleanTags []string
	for range startPoints {
		r := <-results
		for _, t := range r.tags {
			if !seen[t] {
				seen[t] = true
				allCleanTags = append(allCleanTags, t)
			}
		}
	}

	if len(allCleanTags) == 0 {
		return nil, fmt.Errorf("no rhoai release tags found")
	}

	var parsed []parsedTag
	for _, tag := range allCleanTags {
		if p, ok := parseTag(tag); ok {
			parsed = append(parsed, p)
		}
	}
	if len(parsed) == 0 {
		return nil, fmt.Errorf("no parseable rhoai tags found")
	}

	sort.Slice(parsed, func(i, j int) bool {
		return compareTags(parsed[i], parsed[j]) < 0
	})

	// Update the cache so subsequent callers get the cached result.
	tagScanCacheMu.Lock()
	tagScanCache = make([]parsedTag, len(parsed))
	copy(tagScanCache, parsed)
	tagScanCacheAt = time.Now()
	tagScanCacheMu.Unlock()

	return parsed, nil
}

// FetchLatestNightly returns the highest-versioned nightly tag and its image reference.
func FetchLatestNightly(ctx context.Context, c *Client) (*types.LatestNightlyResponse, error) {
	quayAuth := getQuayAuth(c)
	bearerToken, err := getQuayBearerToken(ctx, quayHTTPClient, quayAuth)
	if err != nil {
		return nil, err
	}

	parsed, err := fetchAndParseTags(ctx, quayHTTPClient, bearerToken)
	if err != nil {
		return nil, err
	}

	latest := parsed[len(parsed)-1].raw

	// Fetch the digest for the tag by doing a HEAD on the manifest
	digest, err := getTagDigest(ctx, quayHTTPClient, bearerToken, latest)
	if err != nil {
		// Fall back to tag-only if digest fetch fails
		return &types.LatestNightlyResponse{
			Tag:   latest,
			Image: fmt.Sprintf("%s:%s", quayImage, latest),
		}, nil
	}

	return &types.LatestNightlyResponse{
		Tag:   latest,
		Image: fmt.Sprintf("%s:%s@%s", quayImage, latest, digest),
	}, nil
}
