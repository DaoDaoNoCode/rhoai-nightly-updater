package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

const cacheTTL = 1 * time.Hour
const cacheMaxEntries = 500

type labelCacheEntry struct {
	labels *ImageLabels
	at     time.Time
}

type commitDateCacheEntry struct {
	date string
	at   time.Time
}

// In-memory caches with TTL-based lazy eviction.
var (
	labelCache        = make(map[string]labelCacheEntry)
	labelCacheMu      sync.RWMutex
	commitDateCache   = make(map[string]commitDateCacheEntry) // key: "owner/repo/sha" → date
	commitDateCacheMu sync.RWMutex
)

type skipCommitDateKey struct{}

// SkipCommitDate returns a context that signals GetImageLabels to skip the
// GitHub API call for commit dates (saves rate limit and latency).
func SkipCommitDate(ctx context.Context) context.Context {
	return context.WithValue(ctx, skipCommitDateKey{}, true)
}

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

// GetImageLabels fetches the OCI image config labels from Quay for a given imageID.
// The imageID is the fully-qualified image reference with digest from the pod status.
// Images are accessed via quay.io (IDMS mirrors registry.redhat.io -> quay.io).
func GetImageLabels(ctx context.Context, c *Client, imageID string) (*ImageLabels, error) {
	// Check cache first (skip stale entries)
	labelCacheMu.RLock()
	if entry, ok := labelCache[imageID]; ok && time.Since(entry.at) < cacheTTL {
		labelCacheMu.RUnlock()
		return entry.labels, nil
	}
	labelCacheMu.RUnlock()

	repo, digest, ok := extractRepoAndDigest(imageID)
	if !ok {
		return nil, fmt.Errorf("cannot parse image reference: %s", imageID)
	}

	// Get quay auth from the cluster pull secret
	quayAuth := getQuayAuth(c)
	if quayAuth == "" {
		slog.Debug("image labels: proceeding without quay auth (pull secret may be missing or invalid)")
	}

	// Get bearer token scoped to this repo
	authURL := fmt.Sprintf("https://quay.io/v2/auth?service=quay.io&scope=repository:%s:pull", repo)
	tokenReq, err := http.NewRequestWithContext(ctx, "GET", authURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating auth request: %w", err)
	}
	if quayAuth != "" {
		tokenReq.Header.Set("Authorization", "Basic "+quayAuth)
	}

	tokenResp, err := quayHTTPClient.Do(tokenReq)
	if err != nil {
		return nil, fmt.Errorf("quay auth failed: %w", err)
	}
	defer tokenResp.Body.Close()

	if tokenResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(tokenResp.Body, 1<<16))
		return nil, fmt.Errorf("quay auth returned %d: %s", tokenResp.StatusCode, string(body))
	}

	var tokenData quayTokenResponse
	if err := json.NewDecoder(tokenResp.Body).Decode(&tokenData); err != nil {
		return nil, fmt.Errorf("decoding auth token: %w", err)
	}
	bearerToken := tokenData.Token

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

	// Cache the result with timestamp for TTL-based eviction
	labelCacheMu.Lock()
	if len(labelCache) >= cacheMaxEntries {
		evictStaleEntries(labelCache)
	}
	labelCache[imageID] = labelCacheEntry{labels: labels, at: time.Now()}
	labelCacheMu.Unlock()

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
		return nil, fmt.Errorf("no labels in config")
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

	// Fetch commit date from GitHub API if we have both gitURL and gitCommit
	// Skip when caller sets skipCommitDate in context (e.g., FBC label resolution)
	if result.GitURL != "" && result.GitCommit != "" && ctx.Value(skipCommitDateKey{}) == nil {
		result.CommitDate = fetchCommitDate(ctx, result.GitURL, result.GitCommit)
	}

	return result, nil
}

// fetchCommitDate gets the commit timestamp from the GitHub API.
// Returns empty string on any error (non-critical).
func fetchCommitDate(ctx context.Context, gitURL, sha string) string {
	// Parse owner/repo from gitURL like "https://github.com/red-hat-data-services/odh-dashboard"
	parts := strings.Split(strings.TrimSuffix(gitURL, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	owner := parts[len(parts)-2]
	repo := parts[len(parts)-1]

	// Check cache (skip stale entries)
	cacheKey := owner + "/" + repo + "/" + sha
	commitDateCacheMu.RLock()
	if entry, ok := commitDateCache[cacheKey]; ok && time.Since(entry.at) < cacheTTL {
		commitDateCacheMu.RUnlock()
		return entry.date
	}
	commitDateCacheMu.RUnlock()

	// Use the SHA-specific endpoint with minimal data by reading only what we need
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/commits/%s", owner, repo, sha)
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		slog.Debug("fetchCommitDate: failed to create request", "url", apiURL, "error", err)
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := quayHTTPClient.Do(req)
	if err != nil {
		slog.Debug("fetchCommitDate: request failed", "url", apiURL, "error", err)
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		slog.Debug("fetchCommitDate: non-200 response", "status", resp.StatusCode, "repo", owner+"/"+repo)
		return ""
	}

	// git/commits endpoint returns a small response (~1KB) vs commits endpoint (~200KB)
	var commitData struct {
		Committer struct {
			Date string `json:"date"`
		} `json:"committer"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16)) // 64KB limit
	if err != nil {
		slog.Debug("fetchCommitDate: failed to read response body", "error", err)
		return ""
	}
	if err := json.Unmarshal(body, &commitData); err != nil {
		slog.Debug("fetchCommitDate: failed to parse response JSON", "error", err)
		return ""
	}
	date := commitData.Committer.Date
	if date != "" {
		commitDateCacheMu.Lock()
		if len(commitDateCache) >= cacheMaxEntries {
			evictStaleCommitEntries(commitDateCache)
		}
		commitDateCache[cacheKey] = commitDateCacheEntry{date: date, at: time.Now()}
		commitDateCacheMu.Unlock()
	}
	return date
}

// truncateForLog shortens an image reference for log output.
func truncateForLog(s string) string {
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}

// evictStaleEntries removes entries older than cacheTTL from the label cache.
// Must be called with labelCacheMu held.
func evictStaleEntries(cache map[string]labelCacheEntry) {
	now := time.Now()
	for k, v := range cache {
		if now.Sub(v.at) > cacheTTL {
			delete(cache, k)
		}
	}
}

// evictStaleCommitEntries removes entries older than cacheTTL from the commit date cache.
// Must be called with commitDateCacheMu held.
func evictStaleCommitEntries(cache map[string]commitDateCacheEntry) {
	now := time.Now()
	for k, v := range cache {
		if now.Sub(v.at) > cacheTTL {
			delete(cache, k)
		}
	}
}
