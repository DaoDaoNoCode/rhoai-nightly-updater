package cluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
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
		// Honor HTTP(S)_PROXY/NO_PROXY like http.DefaultTransport does, so
		// registry calls work on clusters that require an egress proxy.
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	},
}

// Matches clean version tags:
//
//	rhoai-3.4, rhoai-3.5.0, rhoai-3.5.0-ea.1, rhoai-3.5-ea.2
var cleanTagRegex = regexp.MustCompile(`^rhoai-\d+\.\d+(\.\d+)?(-ea(\.\d+)?)?$`)
var tagParseRegex = regexp.MustCompile(`^rhoai-(\d+)\.(\d+)(?:\.(\d+))?(-ea(?:\.(\d+))?)?$`)

type parsedTag struct {
	raw   string
	major int
	minor int
	patch int
	ea    int // -1 = GA, 0 = unnumbered EA, positive values = numbered EA
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
		ea = 0
		if m[5] != "" {
			fmt.Sscanf(m[5], "%d", &ea)
		}
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

// quayRHOAIRegistry is the registry path whose credentials the updater needs.
const quayRHOAIRegistry = "quay.io/rhoai"

// decodeDockerConfigSecret extracts the docker config from a
// kubernetes.io/dockerconfigjson Secret API response. On failure it returns a
// user-facing reason instead of a config.
func decodeDockerConfigSecret(body []byte) (map[string]interface{}, string) {
	var secret struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &secret); err != nil {
		return nil, "Failed to parse secret response"
	}
	if secret.Data == nil {
		return nil, "Secret has no data"
	}
	dockerConfigB64 := secret.Data[".dockerconfigjson"]
	if dockerConfigB64 == "" {
		return nil, "Secret is missing .dockerconfigjson key"
	}
	dockerConfigBytes, err := base64.StdEncoding.DecodeString(dockerConfigB64)
	if err != nil {
		return nil, "Failed to decode .dockerconfigjson"
	}
	var dockerConfig map[string]interface{}
	if err := json.Unmarshal(dockerConfigBytes, &dockerConfig); err != nil || dockerConfig == nil {
		return nil, "Invalid JSON in .dockerconfigjson"
	}
	return dockerConfig, ""
}

// normalizeRegistryKey strips the optional scheme and trailing slash that
// docker config auth keys may carry ("https://quay.io/" -> "quay.io").
func normalizeRegistryKey(key string) string {
	key = strings.TrimPrefix(strings.TrimPrefix(key, "https://"), "http://")
	return strings.TrimSuffix(key, "/")
}

// authFromEntry returns the base64 "user:password" credential of a docker
// config auth entry, accepting either the "auth" field or username/password.
func authFromEntry(val interface{}) string {
	entry, _ := val.(map[string]interface{})
	if auth, _ := entry["auth"].(string); auth != "" {
		return auth
	}
	user, _ := entry["username"].(string)
	pass, _ := entry["password"].(string)
	if user != "" && pass != "" {
		return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	}
	return ""
}

// selectQuayAuth picks the credential that container runtimes use for
// quay.io/rhoai images: the most specific matching registry key wins, so an
// exact quay.io/rhoai entry is preferred over repository-scoped entries, which
// are preferred over a generic quay.io login. The choice is deterministic.
// It returns the credential and the auths key it came from.
func selectQuayAuth(auths map[string]interface{}) (string, string) {
	keys := make([]string, 0, len(auths))
	for key := range auths {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	rank := func(key string) int {
		switch k := normalizeRegistryKey(key); {
		case k == quayRHOAIRegistry:
			return 0
		case strings.HasPrefix(k, quayRHOAIRegistry+"/"):
			return 1
		case k == "quay.io":
			return 2
		default:
			return -1
		}
	}
	bestAuth, bestKey, bestRank := "", "", 3
	for _, key := range keys {
		r := rank(key)
		if r < 0 || r >= bestRank {
			continue
		}
		if auth := authFromEntry(auths[key]); auth != "" {
			bestAuth, bestKey, bestRank = auth, key, r
		}
	}
	return bestAuth, bestKey
}

// getQuayAuth extracts the quay.io/rhoai auth credential from the cluster pull secret.
func getQuayAuth(c *Client) string {
	path := namespacedPath("v1", "secrets", "kube-system", "additional-pull-secret")
	body, _, err := c.get(path)
	if err != nil {
		slog.Warn("quay auth: failed to read pull secret", "error", err)
		return ""
	}
	dockerCfg, problem := decodeDockerConfigSecret(body)
	if problem != "" {
		slog.Warn("quay auth: unusable pull secret", "reason", problem)
		return ""
	}
	auths, _ := dockerCfg["auths"].(map[string]interface{})
	auth, _ := selectQuayAuth(auths)
	if auth == "" {
		slog.Warn("quay auth: no quay.io/rhoai or quay.io entry found in pull secret auths")
	}
	return auth
}

// quayAuthError is returned when the Quay token endpoint answers with a
// non-200 status, letting callers tell rejected credentials from outages.
type quayAuthError struct {
	StatusCode int
	Body       string
}

func (e *quayAuthError) Error() string {
	return fmt.Sprintf("quay auth returned %d: %s", e.StatusCode, e.Body)
}

// isQuayCredentialRejection reports whether err means Quay refused the credentials.
func isQuayCredentialRejection(err error) bool {
	var authErr *quayAuthError
	return errors.As(err, &authErr) && (authErr.StatusCode == http.StatusUnauthorized || authErr.StatusCode == http.StatusForbidden)
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
		return "", &quayAuthError{StatusCode: resp.StatusCode, Body: string(body)}
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

	// Build response in descending order (newest first), resolve digests in parallel
	tags := make([]types.NightlyTag, len(topTags))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)
	for idx := 0; idx < len(topTags); idx++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			t := topTags[len(topTags)-1-i]
			digest, err := getTagDigest(ctx, quayHTTPClient, bearerToken, t.raw)
			var image string
			if err != nil {
				image = fmt.Sprintf("%s:%s", quayImage, t.raw)
			} else {
				image = fmt.Sprintf("%s:%s@%s", quayImage, t.raw, digest)
			}
			tags[i] = types.NightlyTag{Tag: t.raw, Image: image}
		}(idx)
	}
	wg.Wait()

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
		"rhoai-",    // catches everything from the start
		"rhoai-2.2", // jumps past early 2.x noise
		"rhoai-2.9", // jumps to the 2.9/3.0 boundary
		"rhoai-3.2", // 3.2+ range
		"rhoai-3.4", // 3.4+ range
		"rhoai-3.5", // 3.5+ range (catches EA tags)
		"rhoai-3.9", // future versions
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
