package registry

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const FBCImage = "quay.io/rhoai/rhoai-fbc-fragment"

// quayHTTPClient is a shared HTTP client for all Quay/GitHub API calls,
// avoiding per-request client allocation and enabling connection reuse.
var quayHTTPClient = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		// Honor HTTP(S)_PROXY/NO_PROXY like http.DefaultTransport does, so
		// registry calls work on clusters that require an egress proxy.
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	},
}

// HTTPClient is the client of every Quay and GitHub call.
func HTTPClient() *http.Client { return quayHTTPClient }

// Matches clean version tags:
//
//	rhoai-3.4, rhoai-3.5.0, rhoai-3.5.0-ea.1, rhoai-3.5-ea.2
var CleanTagRegex = regexp.MustCompile(`^rhoai-\d+\.\d+(\.\d+)?(-ea(\.\d+)?)?$`)
var TagParseRegex = regexp.MustCompile(`^rhoai-(\d+)\.(\d+)(?:\.(\d+))?(-ea(?:\.(\d+))?)?$`)

type Tag struct {
	Raw   string
	Major int
	Minor int
	Patch int
	EA    int // -1 = GA, 0 = unnumbered EA, positive values = numbered EA
}

func ParseTag(tag string) (Tag, bool) {
	m := TagParseRegex.FindStringSubmatch(tag)
	if m == nil {
		return Tag{}, false
	}
	// The regex only captures digits, so Atoi fails only on overflow.
	num := func(s string) (int, bool) {
		if s == "" {
			return 0, true
		}
		n, err := strconv.Atoi(s)
		return n, err == nil
	}
	major, ok1 := num(m[1])
	minor, ok2 := num(m[2])
	patch, ok3 := num(m[3])
	ea, ok4 := -1, true
	if m[4] != "" {
		ea, ok4 = num(m[5])
	}
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return Tag{}, false
	}
	return Tag{Raw: tag, Major: major, Minor: minor, Patch: patch, EA: ea}, true
}

// Sort order: 3.4.0-ea.1 < 3.4.0-ea.2 < 3.4.0 (GA) < 3.5.0-ea.1 < 3.5.0 (GA)
func CompareTags(a, b Tag) int {
	if a.Major != b.Major {
		return a.Major - b.Major
	}
	if a.Minor != b.Minor {
		return a.Minor - b.Minor
	}
	if a.Patch != b.Patch {
		return a.Patch - b.Patch
	}
	// Same version: GA (ea=-1) is greater than any EA
	if a.EA == -1 && b.EA == -1 {
		return 0
	}
	if a.EA == -1 {
		return 1
	}
	if b.EA == -1 {
		return -1
	}
	return a.EA - b.EA
}

type quayTokenResponse struct {
	Token string `json:"token"`
}

// QuayRHOAI is the registry path whose credentials the updater needs.
const QuayRHOAI = "quay.io/rhoai"

// DecodeDockerConfigSecret extracts the docker config from a
// kubernetes.io/dockerconfigjson Secret API response. On failure it returns a
// user-facing reason instead of a config.
func DecodeDockerConfigSecret(body []byte) (map[string]any, string) {
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
	var dockerConfig map[string]any
	if err := json.Unmarshal(dockerConfigBytes, &dockerConfig); err != nil || dockerConfig == nil {
		return nil, "Invalid JSON in .dockerconfigjson"
	}
	return dockerConfig, ""
}

// NormalizeKey strips the optional scheme and trailing slash that
// docker config auth keys may carry ("https://quay.io/" -> "quay.io").
func NormalizeKey(key string) string {
	key = strings.TrimPrefix(strings.TrimPrefix(key, "https://"), "http://")
	return strings.TrimSuffix(key, "/")
}

// AuthFromEntry returns the base64 "user:password" credential of a docker
// config auth entry, accepting either the "auth" field or username/password.
func AuthFromEntry(val any) string {
	entry, _ := val.(map[string]any)
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

// SelectQuayAuth picks the credential that container runtimes use for
// quay.io/rhoai images: the most specific matching registry key wins, so an
// exact quay.io/rhoai entry is preferred over repository-scoped entries, which
// are preferred over a generic quay.io login. The choice is deterministic.
func SelectQuayAuth(auths map[string]any) string {
	keys := make([]string, 0, len(auths))
	for key := range auths {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	rank := func(key string) int {
		switch k := NormalizeKey(key); {
		case k == QuayRHOAI:
			return 0
		case strings.HasPrefix(k, QuayRHOAI+"/"):
			return 1
		case k == "quay.io":
			return 2
		default:
			return -1
		}
	}
	bestAuth, bestRank := "", 3
	for _, key := range keys {
		r := rank(key)
		if r < 0 || r >= bestRank {
			continue
		}
		if auth := AuthFromEntry(auths[key]); auth != "" {
			bestAuth, bestRank = auth, r
		}
	}
	return bestAuth
}

// QuayAuth extracts the quay.io/rhoai auth credential from the cluster pull secret.
func QuayAuth(c *kube.Client) string {
	path := rhoai.NamespacedPath("v1", "secrets", "kube-system", "additional-pull-secret")
	body, _, err := c.Get(path)
	if err != nil {
		slog.Warn("quay auth: failed to read pull secret", "error", err)
		return ""
	}
	dockerCfg, problem := DecodeDockerConfigSecret(body)
	if problem != "" {
		slog.Warn("quay auth: unusable pull secret", "reason", problem)
		return ""
	}
	auths, _ := dockerCfg["auths"].(map[string]any)
	auth := SelectQuayAuth(auths)
	if auth == "" {
		slog.Warn("quay auth: no quay.io/rhoai or quay.io entry found in pull secret auths")
	}
	return auth
}

// QuayAuthError is returned when the Quay token endpoint answers with a
// non-200 status, letting callers tell rejected credentials from outages.
type QuayAuthError struct {
	StatusCode int
	Body       string
}

func (e *QuayAuthError) Error() string {
	return fmt.Sprintf("quay auth returned %d: %s", e.StatusCode, e.Body)
}

// CredentialsRejected reports whether Quay refused the credentials (401 or
// 403), not answered with an outage (HTTPStatusForError).
func (e *QuayAuthError) CredentialsRejected() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// IsCredentialRejection reports whether err means Quay refused the credentials.
func IsCredentialRejection(err error) bool {
	var authErr *QuayAuthError
	return errors.As(err, &authErr) && authErr.CredentialsRejected()
}

func QuayBearerToken(ctx context.Context, httpClient *http.Client, basicAuth string) (string, error) {
	return FetchQuayToken(ctx, httpClient, basicAuth, []string{FBCRepo})
}

// FBCRepo is the repository of the nightly FBC catalog images.
const FBCRepo = "rhoai/rhoai-fbc-fragment"

// FetchQuayToken requests one pull token for the given repositories. Quay
// honours repeated scope parameters (the Docker token protocol allows them),
// so a single token can cover a whole batch of images.
func FetchQuayToken(ctx context.Context, httpClient *http.Client, basicAuth string, repos []string) (string, error) {
	var authURL strings.Builder
	authURL.WriteString("https://quay.io/v2/auth?service=quay.io")
	for _, repo := range repos {
		authURL.WriteString("&scope=" + url.QueryEscape("repository:"+repo+":pull"))
	}
	req, err := http.NewRequestWithContext(ctx, "GET", authURL.String(), nil)
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
		return "", &QuayAuthError{StatusCode: resp.StatusCode, Body: string(body)}
	}

	// A token is about 2.6 KB (quayTokenBatch); the bound keeps a response
	// that does not end from being buffered whole.
	var tokenData quayTokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tokenData); err != nil {
		return "", fmt.Errorf("decoding auth response: %w", err)
	}
	if tokenData.Token == "" {
		return "", fmt.Errorf("quay auth response has no token")
	}
	return tokenData.Token, nil
}

var (
	QuayTokenCache  = kube.NewLRU[string, string](1024)
	quayTokenFlight kube.FlightGroup[string]
)

// QuayTokenBatch bounds the scopes of one token. The token lists every
// scope, growing ~180 bytes per repository (measured live: 10 scopes 2.6 KB,
// 40 scopes 8 KB), and Quay's nginx rejects a 40-scope bearer header with
// "400 Request Header Or Cookie Too Large". 10 keeps the header near 2.6 KB.
const QuayTokenBatch = 10

func QuayTokenKey(basicAuth, repo string) string {
	sum := sha256.Sum256([]byte(basicAuth))
	return hex.EncodeToString(sum[:]) + "|" + repo
}

// QuayTokenTTL returns how long a bearer token may be reused: until five
// minutes before the "exp" claim of the JWT Quay issues (3600s lifetime,
// verified live). The token is only decoded to read its expiry; checking its
// signature is Quay's job. Without a readable expiry the token is kept for
// 30s, half of the Docker token protocol's 60s default lifetime.
func QuayTokenTTL(token string, now time.Time) time.Duration {
	const fallback = 30 * time.Second
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return fallback
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return fallback
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return fallback
	}
	remaining := time.Unix(claims.Exp, 0).Sub(now)
	switch {
	case remaining <= 0:
		return 0
	case remaining > 10*time.Minute:
		remaining -= 5 * time.Minute
	default:
		remaining /= 2
	}
	if remaining > time.Hour {
		remaining = time.Hour
	}
	return remaining
}

func storeQuayToken(basicAuth, repo, token string) {
	if ttl := QuayTokenTTL(token, time.Now()); ttl > 0 {
		QuayTokenCache.Add(QuayTokenKey(basicAuth, repo), token, ttl)
	}
}

// CachedQuayToken returns a pull token for repo, reusing it until shortly
// before it expires. Concurrent requests for the same token share one call.
func CachedQuayToken(ctx context.Context, basicAuth, repo string) (string, error) {
	key := QuayTokenKey(basicAuth, repo)
	if token, ok := QuayTokenCache.Get(key); ok {
		return token, nil
	}
	token, err, _ := quayTokenFlight.Do(ctx, key, func(ctx context.Context) (string, error) {
		if token, ok := QuayTokenCache.Get(key); ok {
			return token, nil
		}
		token, err := FetchQuayToken(ctx, quayHTTPClient, basicAuth, []string{repo})
		if err != nil {
			return "", err
		}
		storeQuayToken(basicAuth, repo, token)
		return token, nil
	})
	return token, err
}

// prefetchQuayTokens fills the token cache for repos that have no token yet,
// using one multi-scope token per batch. Failures are ignored: each image
// then requests its own token.
func prefetchQuayTokens(ctx context.Context, basicAuth string, repos []string) {
	var missing []string
	seen := map[string]bool{}
	for _, repo := range repos {
		if seen[repo] {
			continue
		}
		seen[repo] = true
		if _, ok := QuayTokenCache.Get(QuayTokenKey(basicAuth, repo)); !ok {
			missing = append(missing, repo)
		}
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for start := 0; start < len(missing); start += QuayTokenBatch {
		batch := missing[start:min(start+QuayTokenBatch, len(missing))]
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			token, err := FetchQuayToken(ctx, quayHTTPClient, basicAuth, batch)
			if err != nil {
				slog.Debug("quay token prefetch failed", "repos", len(batch), "error", err)
				return
			}
			for _, repo := range batch {
				storeQuayToken(basicAuth, repo, token)
			}
		})
	}
	wg.Wait()
}

// forgetQuayToken drops a cached token that a registry rejected.
func forgetQuayToken(basicAuth, repo string) {
	QuayTokenCache.Remove(QuayTokenKey(basicAuth, repo))
}

func TagDigest(ctx context.Context, httpClient *http.Client, bearerToken, tag string) (string, error) {
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

	if tokenRejected(resp.StatusCode) {
		return "", &registryAuthError{what: "manifest HEAD", status: resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("manifest HEAD returned %d", resp.StatusCode)
	}

	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		return "", fmt.Errorf("no digest in response headers")
	}
	return digest, nil
}

// Nightly tags such as rhoai-3.6 are re-pushed with every build, so a tag's
// digest is reused for at most the caller's maxAge.
var (
	TagDigestCache  = kube.NewLRU[string, string](512)
	tagDigestFlight kube.FlightGroup[string]
)

// cachedTagDigest resolves tag to its current digest, reusing a lookup no
// older than maxAge.
func cachedTagDigest(ctx context.Context, basicAuth, tag string, maxAge time.Duration) (string, error) {
	fresh := func() (string, bool) {
		digest, stored, ok := TagDigestCache.GetWithAge(tag)
		return digest, ok && time.Since(stored) < maxAge
	}
	if digest, ok := fresh(); ok {
		return digest, nil
	}
	digest, err, _ := tagDigestFlight.Do(ctx, tag, func(ctx context.Context) (string, error) {
		if digest, ok := fresh(); ok {
			return digest, nil
		}
		for attempt := 0; ; attempt++ {
			token, err := CachedQuayToken(ctx, basicAuth, FBCRepo)
			if err != nil {
				return "", err
			}
			digest, err := TagDigest(ctx, quayHTTPClient, token, tag)
			var authErr *registryAuthError
			if attempt == 0 && errors.As(err, &authErr) {
				forgetQuayToken(basicAuth, FBCRepo)
				continue
			}
			if err != nil {
				return "", err
			}
			TagDigestCache.Add(tag, digest, tagScanCacheTTL)
			return digest, nil
		}
	})
	return digest, err
}

// FetchNightlyTags returns the top `limit` nightly version tags (sorted descending),
// each with its full image reference including digest.
func FetchNightlyTags(ctx context.Context, c *kube.Client, limit int) (*types.NightlyTagsResponse, error) {
	return fetchNightlyTags(ctx, QuayAuth(c), limit, false)
}

// FetchNightlyTagsWithBuildDates is FetchNightlyTags plus each build's date,
// read from the (cached) FBC image labels. It never calls GitHub.
func FetchNightlyTagsWithBuildDates(ctx context.Context, c *kube.Client, limit int) (*types.NightlyTagsResponse, error) {
	return fetchNightlyTags(ctx, QuayAuth(c), limit, true)
}

func fetchNightlyTags(ctx context.Context, basicAuth string, limit int, buildDates bool) (*types.NightlyTagsResponse, error) {
	if limit < 0 {
		limit = 5
	}

	parsed, err := scanNightlyTags(ctx, basicAuth)
	if err != nil {
		return nil, err
	}

	// Take the top N tags (already sorted ascending, so take from the end)
	var topTags []Tag
	if limit == 0 {
		topTags = parsed
	} else {
		start := max(len(parsed)-limit, 0)
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
			digest, err := cachedTagDigest(ctx, basicAuth, t.Raw, tagScanCacheTTL)
			var image string
			if err != nil {
				image = fmt.Sprintf("%s:%s", FBCImage, t.Raw)
			} else {
				image = fmt.Sprintf("%s:%s@%s", FBCImage, t.Raw, digest)
			}
			tags[i] = types.NightlyTag{Tag: t.Raw, Image: image}
		}(idx)
	}
	wg.Wait()

	if buildDates {
		EnrichTagsWithBuildDates(ctx, basicAuth, tags)
	}
	return &types.NightlyTagsResponse{Tags: tags}, nil
}

// scanNightlyTags returns the parsed release tags. A failed scan drops the
// cached token so a rejected token is not reused until it expires.
func scanNightlyTags(ctx context.Context, basicAuth string) ([]Tag, error) {
	bearerToken, err := CachedQuayToken(ctx, basicAuth, FBCRepo)
	if err != nil {
		return nil, err
	}
	parsed, err := FetchAndParseTags(ctx, quayHTTPClient, bearerToken)
	if err != nil {
		forgetQuayToken(basicAuth, FBCRepo)
		return nil, err
	}
	return parsed, nil
}

// EnrichTagsWithBuildDates reads the build-date label of each digest-pinned
// tag image. Labels are cached by digest. The GitHub commit date is not
// fetched: the response only carries the build date.
func EnrichTagsWithBuildDates(ctx context.Context, basicAuth string, tags []types.NightlyTag) {
	refs := make([]string, 0, len(tags))
	for _, t := range tags {
		if strings.Contains(t.Image, "@sha256:") {
			refs = append(refs, t.Image)
		}
	}
	labels := ResolveImageLabels(ctx, basicAuth, refs, false)
	for i := range tags {
		if l := labels[tags[i].Image]; l != nil && l.BuildDate != "" {
			tags[i].BuildDate = l.BuildDate
		}
	}
}

// tagScanCache caches the full tag scan result (tags change at most once/day).
var (
	tagScanCache     []Tag
	tagScanCacheMu   sync.RWMutex
	tagScanCacheAt   time.Time
	tagScanRefreshMu sync.Mutex // serializes cache refresh to prevent stampede
)

const tagScanCacheTTL = 5 * time.Minute

// The release tags are found by asking the registry whether each name
// exists (HEAD manifests/<tag>), not by paging the tag list. The list holds
// tens of thousands of per-commit tags (rhoai-3.6-<sha>, rhoai-3.6-ea.1-<sha>)
// in a locale order that ignores punctuation, so no cursor can jump past a
// version's block reliably, and a page cap leaves gaps: on 2026-11-06 3,600
// tags lay between rhoai-3.5 and rhoai-3.7-ea.1, more than the 30 pages one
// start point read, so the 3.7 builds were never listed. Names are
// predictable (rhoai-M.m, rhoai-M.m.p, with -ea or -ea.N), so the probes
// walk the version numbers and stop after a run of misses.
const (
	tagProbeFirstMajor = 2
	tagProbeLastMajor  = 9
	// tagProbeMinorGap is the run of missing minors that ends a major once
	// one was found; a major with no minor in its first tagProbeMinorStart
	// numbers is not there yet (2.x starts at 2.8), and ends the majors.
	tagProbeMinorGap    = 8
	tagProbeMinorStart  = 24
	tagProbeSeriesGap   = 2 // consecutive missing patches or EAs that end a series
	tagProbeConcurrency = 16
)

// probeTag reports whether the FBC repository has a tag of that name. A
// 404 is "no"; a rejected token, throttling, a server error or a network
// error (after one retry) is an error, so the scan is not cached.
func probeTag(ctx context.Context, httpClient *http.Client, bearerToken, tag string) (bool, error) {
	var lastErr error
	for attempt := range 2 {
		if attempt > 0 {
			if err := kube.Clock().Sleep(ctx, 500*time.Millisecond); err != nil {
				return false, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, fmt.Sprintf("https://quay.io/v2/%s/manifests/%s", FBCRepo, tag), nil)
		if err != nil {
			return false, err
		}
		req.Header.Set("Authorization", "Bearer "+bearerToken)
		req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.v2+json,application/vnd.docker.distribution.manifest.list.v2+json")
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK:
			// The digest comes for free; the tag list resolves it next.
			if d := resp.Header.Get("Docker-Content-Digest"); d != "" {
				TagDigestCache.Add(tag, d, 0)
			}
			return true, nil
		case resp.StatusCode == http.StatusNotFound:
			return false, nil
		case tokenRejected(resp.StatusCode):
			return false, &registryAuthError{what: "manifest HEAD", status: resp.StatusCode}
		default:
			lastErr = fmt.Errorf("manifest HEAD returned %d", resp.StatusCode)
		}
	}
	return false, lastErr
}

// tagProber runs probes a few at a time and collects the tags that exist.
type tagProber struct {
	ctx        context.Context
	httpClient *http.Client
	token      string
	sem        chan struct{}
	background sync.WaitGroup // the EA and patch probes of the minors found
	mu         sync.Mutex
	found      map[string]bool
	failed     bool
}

// exists probes one tag and records it when it is there. An error marks
// the scan incomplete and counts as a miss, so a series still ends.
func (p *tagProber) exists(tag string) bool {
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-p.ctx.Done():
		p.mu.Lock()
		p.failed = true
		p.mu.Unlock()
		return false
	}
	ok, err := probeTag(p.ctx, p.httpClient, p.token, tag)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		slog.Warn("quay tag scan: probe failed", "tag", tag, "error", err)
		p.failed = true
		return false
	}
	if ok {
		p.found[tag] = true
	}
	return ok
}

// series probes base+suffix(1), base+suffix(2), ... in parallel batches and
// stops after tagProbeSeriesGap consecutive misses; the tags found are
// handed to each.
func (p *tagProber) series(suffix func(int) string, each func(tag string)) {
	for n := 1; ; n += tagProbeSeriesGap {
		hits := make([]bool, tagProbeSeriesGap)
		var wg sync.WaitGroup
		for i := range hits {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				hits[i] = p.exists(suffix(n + i))
			}(i)
		}
		wg.Wait()
		any := false
		for i, hit := range hits {
			if hit {
				any = true
				each(suffix(n + i))
			}
		}
		if !any {
			return
		}
	}
}

// version probes the EAs and patches of one version (rhoai-3.6 or
// rhoai-3.6.1): rhoai-3.6-ea, rhoai-3.6-ea.1, ...; and, for a minor,
// rhoai-3.6.1, ... each with its own EAs.
func (p *tagProber) version(base string, patches bool) {
	var wg sync.WaitGroup
	wg.Go(func() {
		p.exists(base + "-ea")
		p.series(func(n int) string { return fmt.Sprintf("%s-ea.%d", base, n) }, func(string) {})
	})
	if patches {
		wg.Go(func() {
			var inner sync.WaitGroup
			p.series(func(n int) string { return fmt.Sprintf("%s.%d", base, n) }, func(tag string) {
				inner.Go(func() { ; p.version(tag, false) })
			})
			inner.Wait()
		})
	}
	wg.Wait()
}

// major probes the minors of rhoai-M in batches: a minor is there when its
// GA tag or its first EA exists (3.7 had only rhoai-3.7-ea.1 for weeks).
// It stops after tagProbeMinorGap consecutive misses, or after
// tagProbeMinorStart numbers without any. Every minor found gets its EAs
// and patches probed in the background. Returns whether the major exists.
func (p *tagProber) major(m int) bool {
	found := false
	for minor := 0; ; minor += tagProbeMinorGap {
		hits := make([]bool, tagProbeMinorGap)
		var batch sync.WaitGroup
		for i := range hits {
			batch.Add(1)
			go func(i int) {
				defer batch.Done()
				tag := fmt.Sprintf("rhoai-%d.%d", m, minor+i)
				var ea sync.WaitGroup
				ga, firstEA := false, false
				ea.Add(2)
				go func() { defer ea.Done(); ga = p.exists(tag) }()
				go func() { defer ea.Done(); firstEA = p.exists(tag + "-ea.1") }()
				ea.Wait()
				hits[i] = ga || firstEA
				if hits[i] {
					p.background.Go(func() { ; p.version(tag, true) })
				}
			}(i)
		}
		batch.Wait()
		any := false
		for _, hit := range hits {
			any = any || hit
		}
		found = found || any
		if !any && (found || minor+tagProbeMinorGap >= tagProbeMinorStart) {
			break
		}
	}
	return found
}

// FetchAndParseTags returns the release tags of the FBC repository, from
// the cache when it is fresh. A scan that could not probe every name is
// returned but not cached, so the next request scans again.
func FetchAndParseTags(ctx context.Context, httpClient *http.Client, bearerToken string) ([]Tag, error) {
	tagScanCacheMu.RLock()
	if len(tagScanCache) > 0 && time.Since(tagScanCacheAt) < tagScanCacheTTL {
		result := make([]Tag, len(tagScanCache))
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
		result := make([]Tag, len(tagScanCache))
		copy(result, tagScanCache)
		tagScanCacheMu.RUnlock()
		return result, nil
	}
	tagScanCacheMu.RUnlock()

	p := &tagProber{ctx: ctx, httpClient: httpClient, token: bearerToken, sem: make(chan struct{}, tagProbeConcurrency), found: map[string]bool{}}
	// Majors in order: once one exists, the first one with no minor ends
	// the walk (there is no rhoai-5 before rhoai-4). Their EA and patch
	// probes run meanwhile.
	for m, any := tagProbeFirstMajor, false; m <= tagProbeLastMajor; m++ {
		found := p.major(m)
		if any && !found {
			break
		}
		any = any || found
	}
	p.background.Wait()
	incomplete := p.failed

	if len(p.found) == 0 {
		if incomplete {
			return nil, fmt.Errorf("could not list rhoai release tags from Quay; retry shortly")
		}
		return nil, fmt.Errorf("no rhoai release tags found")
	}

	var parsed []Tag
	for tag := range p.found {
		if q, ok := ParseTag(tag); ok {
			parsed = append(parsed, q)
		}
	}
	if len(parsed) == 0 {
		return nil, fmt.Errorf("no parseable rhoai tags found")
	}

	sort.Slice(parsed, func(i, j int) bool {
		return CompareTags(parsed[i], parsed[j]) < 0
	})

	// A partial scan can miss the newest releases, so it is returned for this
	// request but not cached; the next request scans again.
	if incomplete {
		slog.Warn("quay tag scan incomplete; result not cached", "tags", len(parsed))
		return parsed, nil
	}

	// Update the cache so subsequent callers get the cached result.
	tagScanCacheMu.Lock()
	tagScanCache = make([]Tag, len(parsed))
	copy(tagScanCache, parsed)
	tagScanCacheAt = time.Now()
	tagScanCacheMu.Unlock()

	return parsed, nil
}

// FetchLatestNightly returns the highest-versioned nightly tag, its digest
// and, when its labels can be read, its build date. It shares the cached tag
// scan, token and tag digest with the tag list.
func FetchLatestNightly(ctx context.Context, c *kube.Client) (*types.LatestNightlyResponse, error) {
	basicAuth := QuayAuth(c)
	parsed, err := scanNightlyTags(ctx, basicAuth)
	if err != nil {
		return nil, err
	}

	latest := parsed[len(parsed)-1].Raw
	digest, err := cachedTagDigest(ctx, basicAuth, latest, tagScanCacheTTL)
	if err != nil {
		// Fall back to tag-only if digest fetch fails
		return &types.LatestNightlyResponse{
			Tag:   latest,
			Image: fmt.Sprintf("%s:%s", FBCImage, latest),
		}, nil
	}

	resp := &types.LatestNightlyResponse{
		Tag:    latest,
		Image:  fmt.Sprintf("%s:%s@%s", FBCImage, latest, digest),
		Digest: digest,
	}
	labelCtx, cancel := context.WithTimeout(ctx, LabelFetchTimeout)
	defer cancel()
	if labels, err := ImageLabelsWithAuth(labelCtx, basicAuth, resp.Image, false); err == nil {
		resp.BuildDate = labels.BuildDate
	}
	return resp, nil
}

// ResolveNightlyTag returns the newest build of one nightly line (the tag,
// such as rhoai-3.6) pinned by its digest, from the same cached Quay token
// and tag digest as FetchLatestNightly. A tag Quay does not have is an
// error.
func ResolveNightlyTag(ctx context.Context, c *kube.Client, tag string) (*types.LatestNightlyResponse, error) {
	digest, err := cachedTagDigest(ctx, QuayAuth(c), tag, tagScanCacheTTL)
	if err != nil {
		return nil, fmt.Errorf("resolve %s:%s: %w", FBCImage, tag, err)
	}
	return &types.LatestNightlyResponse{Tag: tag, Image: fmt.Sprintf("%s:%s@%s", FBCImage, tag, digest), Digest: digest}, nil
}

// ParseCSVVersion extracts a parsedTag from a CSV name like "rhods-operator.3.5.0-ea.1".
// It handles three cases:
//  1. Full EA suffix:       "rhods-operator.3.5.0-ea.1" -> ea=1
//  2. GA (no suffix):       "rhods-operator.3.5.0"      -> ea=-1 (ranks above EA)
//  3. Non-EA pre-release:   "rhods-operator.3.5.0-rc.1" -> ea=-1000 (ranks below GA and EA)
//
// Returns the parsed version and true on success, or zero value and false if unparseable.
func ParseCSVVersion(csv string) (Tag, bool) {
	// Normalize: strip optional "v" prefix from version portion.
	// CSV names may use "rhods-operator.v3.5.0" or "rhods-operator.3.5.0".
	normalized := csv
	dotIdx := strings.Index(csv, ".")
	if dotIdx >= 0 {
		after := csv[dotIdx+1:]
		if strings.HasPrefix(after, "v") {
			normalized = csv[:dotIdx+1] + after[1:]
		}
	}

	var major, minor, patch, eaNum int
	// Try full EA parse: "rhods-operator.M.m.p-ea.N"
	n, _ := fmt.Sscanf(normalized, "rhods-operator.%d.%d.%d-ea.%d", &major, &minor, &patch, &eaNum)
	if n == 4 {
		return Tag{Raw: csv, Major: major, Minor: minor, Patch: patch, EA: eaNum}, true
	}
	// Recognize unnumbered EA and build metadata using the same parser as release tags.
	version := strings.TrimPrefix(normalized, "rhods-operator.")
	if strings.HasPrefix(normalized, "rhods-operator.") {
		if parsed, ok := ParseTag("rhoai-" + strings.SplitN(version, "+", 2)[0]); ok {
			parsed.Raw = csv
			return parsed, true
		}
	}

	// Try GA parse (no suffix): "rhods-operator.M.m.p"
	n, _ = fmt.Sscanf(normalized, "rhods-operator.%d.%d.%d", &major, &minor, &patch)
	if n == 3 {
		// Check if the normalized CSV has any suffix after the patch number.
		// Build the canonical GA prefix and see if the normalized string is longer.
		gaPrefix := fmt.Sprintf("rhods-operator.%d.%d.%d", major, minor, patch)
		remainder := strings.TrimPrefix(normalized, gaPrefix)
		if strings.HasPrefix(remainder, "-") {
			// Has a non-EA suffix (e.g., "-rc.1", "-beta.2") — treat as pre-release,
			// ranking below both GA (ea=-1) and any EA build (ea>=0).
			slog.Debug("CSV has non-EA pre-release suffix, ranking below GA and EA",
				"csv", csv, "suffix", remainder)
			return Tag{Raw: csv, Major: major, Minor: minor, Patch: patch, EA: -1000}, true
		}
		// Genuine GA — no suffix at all.
		return Tag{Raw: csv, Major: major, Minor: minor, Patch: patch, EA: -1}, true
	}

	slog.Debug("failed to parse CSV version", "csv", csv)
	return Tag{}, false
}
