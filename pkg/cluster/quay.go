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
var releaseBuildPrefixRegex = regexp.MustCompile(`^(rhoai-\d+\.\d+(?:\.\d+)?(?:-ea(?:\.\d+)?)?)-`)

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

// Cache release names, not digests: moving nightly tags still resolve to the
// current manifest on every request.
var (
	tagScanCache       []parsedTag
	tagScanCacheMu     sync.RWMutex
	tagScanCacheAt     time.Time
	tagScanRefreshDone chan struct{}
	tagScanRefreshErr  error
	tagScanRetryAt     time.Time
)

const (
	tagScanCacheTTL      = 5 * time.Minute
	tagScanCacheMaxStale = 30 * time.Minute
	tagScanTimeout       = 90 * time.Second
	tagScanConcurrency   = 8
)

// Refresh an expired release list in the background while callers use the last
// complete list. Cold callers share one refresh and can cancel their own wait.
func fetchAndParseTags(ctx context.Context, httpClient *http.Client, bearerToken string) ([]parsedTag, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tagScanCacheMu.Lock()
	age := time.Since(tagScanCacheAt)
	if len(tagScanCache) > 0 && age < tagScanCacheTTL {
		result := append([]parsedTag(nil), tagScanCache...)
		tagScanCacheMu.Unlock()
		return result, nil
	}
	if tagScanRefreshDone == nil && !time.Now().Before(tagScanRetryAt) {
		done := make(chan struct{})
		tagScanRefreshDone = done
		go func() {
			refreshCtx, cancel := context.WithTimeout(context.Background(), tagScanTimeout)
			defer cancel()
			tags, err := scanReleaseTags(refreshCtx, httpClient, bearerToken)
			tagScanCacheMu.Lock()
			if err == nil {
				tagScanCache = tags
				tagScanCacheAt = time.Now()
				tagScanRetryAt = time.Time{}
			} else {
				tagScanRetryAt = time.Now().Add(15 * time.Second)
				slog.Warn("release tag refresh failed", "error", err)
			}
			tagScanRefreshErr = err
			tagScanRefreshDone = nil
			close(done)
			tagScanCacheMu.Unlock()
		}()
	}
	if len(tagScanCache) > 0 && age < tagScanCacheMaxStale {
		result := append([]parsedTag(nil), tagScanCache...)
		tagScanCacheMu.Unlock()
		return result, nil
	}
	done, refreshErr := tagScanRefreshDone, tagScanRefreshErr
	tagScanCacheMu.Unlock()
	if done == nil {
		return nil, refreshErr
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-done:
	}
	tagScanCacheMu.RLock()
	defer tagScanCacheMu.RUnlock()
	if tagScanRefreshErr != nil {
		return nil, tagScanRefreshErr
	}
	return append([]parsedTag(nil), tagScanCache...), nil
}

type releaseTagRange struct {
	prefix string
	last   string
}

type releaseTagPage struct {
	tags []parsedTag
	next *releaseTagRange
	ea   *releaseTagRange
	err  error
}

// Registry pagination is lexical. A release's dated/hash build tags lie in
// "<release>-...". Jump past that block to "<release>.", preserving numeric
// patch tags, and scan "<release>-ea..." separately so no EA aliases are lost.
// All ranges come from observed tags, not a fixed list of versions or pages.
func scanReleaseTagPage(ctx context.Context, httpClient *http.Client, bearerToken string, scan releaseTagRange) releaseTagPage {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s?n=1000&last=%s", quayTagsURL, scan.last), nil)
	if err != nil {
		return releaseTagPage{err: err}
	}
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return releaseTagPage{err: fmt.Errorf("fetching Quay tags: %w", err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return releaseTagPage{err: fmt.Errorf("Quay tag listing returned HTTP %d", resp.StatusCode)}
	}
	var data quayTagsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data); err != nil {
		return releaseTagPage{err: fmt.Errorf("parsing Quay tags: %w", err)}
	}
	page := releaseTagPage{}
	if len(data.Tags) == 0 {
		return page
	}
	for _, tag := range data.Tags {
		if !strings.HasPrefix(tag, scan.prefix) {
			return page
		}
		if parsed, ok := parseTag(tag); ok {
			page.tags = append(page.tags, parsed)
		}
	}
	last := data.Tags[len(data.Tags)-1]
	if last <= scan.last {
		page.err = fmt.Errorf("Quay tag pagination did not advance past %q", scan.last)
		return page
	}
	if !cleanTagRegex.MatchString(last) {
		if match := releaseBuildPrefixRegex.FindStringSubmatch(last); match != nil {
			prefix := match[1]
			jump := prefix + "."
			if jump > last && strings.HasPrefix(jump, scan.prefix) {
				if release, ok := parseTag(prefix); ok && release.ea == -1 {
					page.ea = &releaseTagRange{prefix: prefix + "-ea", last: prefix + "-e"}
				}
				last = jump
			}
		}
	}
	page.next = &releaseTagRange{prefix: scan.prefix, last: last}
	return page
}

func scanReleaseTags(ctx context.Context, httpClient *http.Client, bearerToken string) ([]parsedTag, error) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	pending := []releaseTagRange{{prefix: "rhoai-", last: "rhoai-"}}
	ranges := map[string]bool{"rhoai-": true}
	seen := make(map[string]parsedTag)
	results := make(chan releaseTagPage, tagScanConcurrency)
	active := 0
	for len(pending) > 0 || active > 0 {
		for len(pending) > 0 && active < tagScanConcurrency {
			scan := pending[0]
			pending = pending[1:]
			active++
			wg.Add(1)
			go func() {
				defer wg.Done()
				page := scanReleaseTagPage(ctx, httpClient, bearerToken, scan)
				select {
				case results <- page:
				case <-ctx.Done():
				}
			}()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case page := <-results:
			active--
			if page.err != nil {
				return nil, page.err
			}
			for _, tag := range page.tags {
				seen[tag.raw] = tag
			}
			if page.next != nil {
				pending = append(pending, *page.next)
			}
			if page.ea != nil && !ranges[page.ea.prefix] {
				ranges[page.ea.prefix] = true
				pending = append(pending, *page.ea)
			}
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("no rhoai release tags found")
	}
	parsed := make([]parsedTag, 0, len(seen))
	for _, tag := range seen {
		parsed = append(parsed, tag)
	}
	sort.Slice(parsed, func(i, j int) bool {
		if order := compareTags(parsed[i], parsed[j]); order != 0 {
			return order < 0
		}
		return parsed[i].raw < parsed[j].raw
	})
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
