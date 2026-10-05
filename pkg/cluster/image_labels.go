package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ImageLabels holds the build/commit metadata extracted from a container image's config labels.
type ImageLabels struct {
	GitCommit  string
	GitURL     string
	CommitDate string // when the commit was merged (from GitHub API)
	BuildDate  string
	Version    string
}

const (
	// Labels are read from a digest-addressed config blob, so they never
	// change: entries leave the cache only through LRU eviction.
	labelCacheMax = 2000
	// An image without labels is remembered briefly; transient errors are not cached.
	labelNegativeTTL = 10 * time.Minute
	// Commit dates are immutable per SHA.
	commitDateCacheMax = 2000
	// A missing commit (404/422) is retried after this long.
	commitDateNegativeTTL = 10 * time.Minute
	// Parallel registry reads per batch. quayHTTPClient keeps 16 idle
	// connections per host and negotiates HTTP/2 with quay.io.
	labelFetchConcurrency = 16
	// labelFetchTimeout bounds each image of a batch.
	labelFetchTimeout = 8 * time.Second
)

var errNoImageLabels = errors.New("no labels in config")

type labelCacheValue struct {
	labels   ImageLabels // CommitDate is never stored here
	noLabels bool
}

var (
	labelCache      = newLRU[string, labelCacheValue](labelCacheMax)
	labelFlight     flightGroup[labelCacheValue]
	commitDateCache = newLRU[string, string](commitDateCacheMax)
	commitFlight    flightGroup[string]
)

// isRHOAIImage checks whether an image reference is an RHOAI component image.
func isRHOAIImage(imageRef string) bool {
	return strings.Contains(imageRef, "rhoai/")
}

// extractRepoAndDigest parses an imageID like
//
//	"registry.redhat.io/rhoai/odh-dashboard-rhel9@sha256:abc123..."
//
// and returns ("rhoai/odh-dashboard-rhel9", "sha256:abc123...").
// It also handles the quay.io prefix directly.
func extractRepoAndDigest(imageID string) (repo, digest string, ok bool) {
	// imageID typically looks like:
	//   registry.redhat.io/rhoai/odh-dashboard-rhel9@sha256:...
	//   or docker-pullable://registry.redhat.io/rhoai/odh-dashboard-rhel9@sha256:...
	ref := imageID
	// Strip docker-pullable:// prefix if present
	ref = strings.TrimPrefix(ref, "docker-pullable://")

	atIdx := strings.LastIndex(ref, "@")
	if atIdx < 0 {
		return "", "", false
	}
	digest = ref[atIdx+1:]
	if !strings.HasPrefix(digest, "sha256:") {
		return "", "", false
	}

	imagePath := ref[:atIdx]
	// Strip tag if present (e.g., ":v3.5.0-ea.2" before @sha256:...)
	if colonIdx := strings.LastIndex(imagePath, ":"); colonIdx > strings.LastIndex(imagePath, "/") {
		imagePath = imagePath[:colonIdx]
	}

	// Extract the repo path after the registry host.
	// "registry.redhat.io/rhoai/odh-dashboard-rhel9" -> "rhoai/odh-dashboard-rhel9"
	slashIdx := strings.Index(imagePath, "/")
	if slashIdx < 0 {
		return "", "", false
	}
	repo = imagePath[slashIdx+1:]
	return repo, digest, true
}

// GetImageLabels fetches the OCI image config labels from Quay for a given
// imageID, including the commit date. Batches should resolve the credential
// once and call resolveImageLabels instead.
func GetImageLabels(ctx context.Context, c *Client, imageID string) (*ImageLabels, error) {
	return imageLabelsWithAuth(ctx, getQuayAuth(c), imageID, true)
}

// imageLabelsWithAuth returns the labels of a digest-pinned image. Images are
// read from quay.io (IDMS mirrors registry.redhat.io/rhoai to quay.io/rhoai).
// Identical concurrent lookups share one registry read.
func imageLabelsWithAuth(ctx context.Context, basicAuth, imageID string, withCommitDate bool) (*ImageLabels, error) {
	repo, digest, ok := extractRepoAndDigest(imageID)
	if !ok {
		return nil, fmt.Errorf("cannot parse image reference: %s", imageID)
	}
	key := repo + "@" + digest
	v, cached := labelCache.Get(key)
	if !cached {
		var err error
		v, err, _ = labelFlight.Do(ctx, key, func(ctx context.Context) (labelCacheValue, error) {
			if v, ok := labelCache.Get(key); ok {
				return v, nil
			}
			labels, err := fetchImageLabels(ctx, basicAuth, repo, digest)
			if errors.Is(err, errNoImageLabels) || errors.Is(err, errManifestUnknown) {
				v := labelCacheValue{noLabels: true}
				labelCache.Add(key, v, labelNegativeTTL)
				return v, nil
			}
			if err != nil {
				return labelCacheValue{}, err
			}
			v := labelCacheValue{labels: *labels}
			labelCache.Add(key, v, 0)
			return v, nil
		})
		if err != nil {
			return nil, err
		}
	}
	if v.noLabels {
		return nil, errNoImageLabels
	}
	out := v.labels
	if withCommitDate && out.GitURL != "" && out.GitCommit != "" {
		out.CommitDate = fetchCommitDate(ctx, out.GitURL, out.GitCommit)
	}
	return &out, nil
}

// cachedImageLabels returns labels already in the cache without any network
// call. The commit date is included when it is cached too.
func cachedImageLabels(imageID string) (*ImageLabels, bool) {
	repo, digest, ok := extractRepoAndDigest(imageID)
	if !ok {
		return nil, false
	}
	v, ok := labelCache.Get(repo + "@" + digest)
	if !ok || v.noLabels {
		return nil, false
	}
	out := v.labels
	if key, ok := commitDateKey(out.GitURL, out.GitCommit); ok {
		out.CommitDate, _ = commitDateCache.Get(key)
	}
	return &out, true
}

// resolveImageLabels fetches labels for a batch of image references with one
// Quay credential, bounded concurrency and a per-image timeout. Images whose
// labels cannot be read are left out of the result.
func resolveImageLabels(ctx context.Context, basicAuth string, refs []string, withCommitDate bool) map[string]*ImageLabels {
	unique := make([]string, 0, len(refs))
	seen := make(map[string]bool, len(refs))
	var repos []string
	for _, ref := range refs {
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		unique = append(unique, ref)
		if repo, digest, ok := extractRepoAndDigest(ref); ok {
			if _, cached := labelCache.Get(repo + "@" + digest); !cached {
				repos = append(repos, repo)
			}
		}
	}
	// One multi-scope token covers many repositories (Quay honours repeated
	// scope parameters), instead of one token request per image.
	prefetchQuayTokens(ctx, basicAuth, repos)

	out := make(map[string]*ImageLabels, len(unique))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, labelFetchConcurrency)
	for _, ref := range unique {
		wg.Add(1)
		go func(ref string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			imageCtx, cancel := context.WithTimeout(ctx, labelFetchTimeout)
			defer cancel()
			labels, err := imageLabelsWithAuth(imageCtx, basicAuth, ref, withCommitDate)
			if err != nil {
				if !errors.Is(err, errNoImageLabels) {
					slog.Warn("failed to fetch image labels", "image", truncateForLog(ref), "error", err)
				}
				return
			}
			mu.Lock()
			out[ref] = labels
			mu.Unlock()
		}(ref)
	}
	wg.Wait()
	return out
}

// fetchImageLabels reads the manifest and config blob of one image. A
// rejected cached token is dropped and the read retried once with a token
// for this repository alone.
func fetchImageLabels(ctx context.Context, basicAuth, repo, digest string) (*ImageLabels, error) {
	for attempt := 0; ; attempt++ {
		bearerToken, err := cachedQuayToken(ctx, basicAuth, repo)
		if err != nil {
			return nil, err
		}
		labels, err := fetchImageLabelsWithToken(ctx, bearerToken, repo, digest)
		var authErr *registryAuthError
		if attempt == 0 && errors.As(err, &authErr) {
			forgetQuayToken(basicAuth, repo)
			continue
		}
		return labels, err
	}
}

// tokenRejected reports a registry status that can mean the bearer token is
// unusable: 401/403 (expired or under-scoped), or 400 (Quay's nginx answers
// "Request Header Or Cookie Too Large" to an oversized multi-scope token).
func tokenRejected(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusBadRequest
}

// errManifestUnknown marks a digest the registry does not have (404). Old
// FBC builds are garbage-collected, so this is remembered like "no labels".
var errManifestUnknown = errors.New("manifest unknown")

// registryAuthError marks a registry read rejected for its token (see tokenRejected).
type registryAuthError struct {
	what   string
	status int
}

func (e *registryAuthError) Error() string {
	return fmt.Sprintf("%s returned %d", e.what, e.status)
}

func fetchImageLabelsWithToken(ctx context.Context, bearerToken, repo, digest string) (*ImageLabels, error) {
	// Fetch the manifest (may be an image index or a single manifest)
	manifestURL := fmt.Sprintf("https://quay.io/v2/%s/manifests/%s", repo, digest)
	configDigest, err := getConfigDigestFromManifest(ctx, quayHTTPClient, bearerToken, manifestURL, repo)
	if err != nil {
		return nil, fmt.Errorf("fetching manifest: %w", err)
	}

	// Fetch the config blob and extract labels
	blobURL := fmt.Sprintf("https://quay.io/v2/%s/blobs/%s", repo, configDigest)
	labels, err := fetchConfigLabels(ctx, quayHTTPClient, bearerToken, blobURL)
	if err != nil {
		return nil, fmt.Errorf("fetching config blob: %w", err)
	}
	return labels, nil
}

// getConfigDigestFromManifest fetches the manifest and returns the config blob digest.
// If the manifest is an OCI image index (multi-arch), it picks the amd64 manifest first.
func getConfigDigestFromManifest(ctx context.Context, httpClient *http.Client, bearerToken, manifestURL, repo string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", manifestURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
	}, ", "))

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if tokenRejected(resp.StatusCode) {
		return "", &registryAuthError{what: "manifest GET", status: resp.StatusCode}
	}
	if resp.StatusCode == http.StatusNotFound {
		return "", errManifestUnknown
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("manifest GET returned %d: %s", resp.StatusCode, string(body))
	}

	contentType := resp.Header.Get("Content-Type")

	// Check if this is an image index / manifest list
	if strings.Contains(contentType, "image.index") || strings.Contains(contentType, "manifest.list") {
		return resolveAmd64Manifest(ctx, httpClient, bearerToken, body, repo)
	}

	// It's a single manifest — extract config digest directly
	return extractConfigDigest(body)
}

// resolveAmd64Manifest picks the amd64/linux manifest from an image index and fetches it.
func resolveAmd64Manifest(ctx context.Context, httpClient *http.Client, bearerToken string, indexBody []byte, repo string) (string, error) {
	var index struct {
		Manifests []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			Platform  struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(indexBody, &index); err != nil {
		return "", fmt.Errorf("parsing image index: %w", err)
	}

	var amd64Digest string
	for _, m := range index.Manifests {
		if m.Platform.Architecture == "amd64" && m.Platform.OS == "linux" {
			amd64Digest = m.Digest
			break
		}
	}
	if amd64Digest == "" {
		// Fallback: just pick the first manifest
		if len(index.Manifests) > 0 {
			amd64Digest = index.Manifests[0].Digest
		} else {
			return "", fmt.Errorf("no manifests in image index")
		}
	}

	// Fetch the platform-specific manifest
	manifestURL := fmt.Sprintf("https://quay.io/v2/%s/manifests/%s", repo, amd64Digest)
	req, err := http.NewRequestWithContext(ctx, "GET", manifestURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("amd64 manifest GET returned %d: %s", resp.StatusCode, string(body))
	}

	return extractConfigDigest(body)
}

// extractConfigDigest pulls the config.digest from a single image manifest.
func extractConfigDigest(manifestBody []byte) (string, error) {
	var manifest struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(manifestBody, &manifest); err != nil {
		return "", fmt.Errorf("parsing manifest: %w", err)
	}
	if manifest.Config.Digest == "" {
		return "", fmt.Errorf("no config digest in manifest")
	}
	return manifest.Config.Digest, nil
}

// fetchConfigLabels fetches the config blob and extracts image labels.
func fetchConfigLabels(ctx context.Context, httpClient *http.Client, bearerToken, blobURL string) (*ImageLabels, error) {
	// Use a client that strips the Authorization header on redirect.
	// Quay returns a 302 to an S3 pre-signed URL which rejects extra auth headers.
	blobClient := &http.Client{
		Timeout:   httpClient.Timeout,
		Transport: httpClient.Transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			req.Header.Del("Authorization")
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, "GET", blobURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearerToken)

	resp, err := blobClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if tokenRejected(resp.StatusCode) {
		return nil, &registryAuthError{what: "config blob GET", status: resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("config blob GET returned %d", resp.StatusCode)
	}

	var config struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &config); err != nil {
		return nil, fmt.Errorf("parsing config blob: %w", err)
	}

	labels := config.Config.Labels
	if labels == nil {
		return nil, errNoImageLabels
	}

	result := &ImageLabels{}

	// Try vcs-ref first, then git.commit
	if v := labels["vcs-ref"]; v != "" {
		result.GitCommit = v
	} else if v := labels["git.commit"]; v != "" {
		result.GitCommit = v
	}

	if v := labels["git.url"]; v != "" {
		result.GitURL = v
	}

	if v := labels["build-date"]; v != "" {
		result.BuildDate = v
	}

	if v := labels["version"]; v != "" {
		result.Version = v
	}

	return result, nil
}

var (
	githubNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	githubSHAPattern  = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
)

// commitDateKey returns the cache key for a commit, or false when the URL is
// not a GitHub repository or the SHA is malformed (label values come from
// image metadata and are used to build an API path).
func commitDateKey(gitURL, sha string) (string, bool) {
	u := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(gitURL), "/"), ".git")
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	parts := strings.Split(u, "/")
	if len(parts) != 3 || parts[0] != "github.com" || !githubNamePattern.MatchString(parts[1]) ||
		!githubNamePattern.MatchString(parts[2]) || !githubSHAPattern.MatchString(sha) {
		return "", false
	}
	return parts[1] + "/" + parts[2] + "/" + sha, true
}

// githubAPIBase is the GitHub REST API root; tests replace the transport, not this.
const githubAPIBase = "https://api.github.com"

// githubBackoff stops GitHub calls until the rate limit resets. Without a
// token the limit is 60 requests per hour per egress IP, shared by every user
// of an in-cluster deployment.
var githubBackoff struct {
	sync.Mutex
	until time.Time
}

func githubBackoffActive() bool {
	githubBackoff.Lock()
	defer githubBackoff.Unlock()
	return time.Now().Before(githubBackoff.until)
}

func setGitHubBackoff(until time.Time) {
	githubBackoff.Lock()
	defer githubBackoff.Unlock()
	if until.After(githubBackoff.until) {
		githubBackoff.until = until
	}
}

// githubRateLimitReset reads when a rate-limited request may be retried
// (Retry-After for secondary limits, x-ratelimit-reset for the primary one).
func githubRateLimitReset(resp *http.Response, now time.Time) time.Time {
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		return now.Add(time.Duration(s) * time.Second)
	}
	if epoch, err := strconv.ParseInt(resp.Header.Get("X-Ratelimit-Reset"), 10, 64); err == nil {
		if reset := time.Unix(epoch, 0); reset.After(now) && reset.Sub(now) <= 2*time.Hour {
			return reset
		}
	}
	return now.Add(5 * time.Minute)
}

// fetchCommitDate gets the commit timestamp from the GitHub API. It returns
// an empty string on any error: the date is optional, and a rate limit must
// never fail the page. Set GITHUB_TOKEN to raise the limit from 60 to 5000
// requests per hour.
func fetchCommitDate(ctx context.Context, gitURL, sha string) string {
	key, ok := commitDateKey(gitURL, sha)
	if !ok {
		return ""
	}
	if date, ok := commitDateCache.Get(key); ok {
		return date
	}
	if githubBackoffActive() {
		return ""
	}
	date, _, _ := commitFlight.Do(ctx, key, func(ctx context.Context) (string, error) {
		if date, ok := commitDateCache.Get(key); ok {
			return date, nil
		}
		return requestCommitDate(ctx, key)
	})
	return date
}

func requestCommitDate(ctx context.Context, key string) (string, error) {
	parts := strings.SplitN(key, "/", 3)
	// git/commits returns ~1KB, the commits endpoint ~200KB.
	apiURL := fmt.Sprintf("%s/repos/%s/%s/git/commits/%s", githubAPIBase, parts[0], parts[1], parts[2])
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "rhoai-nightly-updater")
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := quayHTTPClient.Do(req)
	if err != nil {
		slog.Debug("fetchCommitDate: request failed", "url", apiURL, "error", err)
		return "", err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && (resp.Header.Get("X-Ratelimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "")):
		until := githubRateLimitReset(resp, time.Now())
		setGitHubBackoff(until)
		slog.Warn("GitHub API rate limit reached; commit dates are skipped until it resets (set GITHUB_TOKEN to raise the limit)",
			"status", resp.StatusCode, "until", until.UTC().Format(time.RFC3339))
		return "", fmt.Errorf("github rate limited")
	case resp.StatusCode >= 500:
		return "", fmt.Errorf("github returned %d", resp.StatusCode)
	default:
		// 401/403/404/422: unknown commit or private repository. Retry later.
		slog.Debug("fetchCommitDate: non-200 response", "status", resp.StatusCode, "commit", key)
		commitDateCache.Add(key, "", commitDateNegativeTTL)
		return "", nil
	}

	var commitData struct {
		Committer struct {
			Date string `json:"date"`
		} `json:"committer"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16)) // 64KB limit
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(body, &commitData); err != nil {
		return "", fmt.Errorf("parse commit: %w", err)
	}
	date := commitData.Committer.Date
	if date == "" {
		commitDateCache.Add(key, "", commitDateNegativeTTL)
		return "", nil
	}
	commitDateCache.Add(key, date, 0)
	return date, nil
}

// truncateForLog shortens an image reference for log output.
func truncateForLog(s string) string {
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}
