package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The update check compares the running release with the highest vX.Y.Z
// tag of the updater's image repository (IMAGE_REPOSITORY, set by the
// template). It reads the registry's tag list anonymously (OCI distribution
// API), caches the answer for hours and ignores errors: an unreachable
// registry only means no "update available" banner.

const (
	updateCheckTTL      = 6 * time.Hour
	updateCheckErrorTTL = 30 * time.Minute
	updateCheckTimeout  = 20 * time.Second
	// Pages of the tag list read at most (1000 tags each).
	maxTagPages = 20
)

var releaseTagRe = regexp.MustCompile(`^v(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})$`)

type releaseVersion [3]int

// parseRelease parses vMAJOR.MINOR.PATCH; anything else (commit builds,
// "dev", pre-releases) is not a release.
func parseRelease(s string) (releaseVersion, bool) {
	m := releaseTagRe.FindStringSubmatch(s)
	if m == nil {
		return releaseVersion{}, false
	}
	var v releaseVersion
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, true
}

func (a releaseVersion) less(b releaseVersion) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// highestRelease returns the highest release tag, or "".
func highestRelease(tags []string) string {
	best, bestTag := releaseVersion{}, ""
	for _, t := range tags {
		v, ok := parseRelease(t)
		if ok && (bestTag == "" || best.less(v)) {
			best, bestTag = v, t
		}
	}
	return bestTag
}

// UpdateInfo is the answer of GET /api/update-check.
type UpdateInfo struct {
	// Current is the running release; empty for main and commit builds,
	// which get no update check.
	Current string `json:"current,omitempty"`
	// Latest is the highest release in the image repository, when known.
	Latest          string `json:"latest,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable"`
	// MajorUpgrade: Latest has another MAJOR version, so it needs its own
	// deploy template (a full redeploy), not just a new image.
	MajorUpgrade    bool   `json:"majorUpgrade"`
	ReleaseNotesURL string `json:"releaseNotesURL,omitempty"`
}

// compareReleases builds the answer for the running version and the highest
// release found.
func compareReleases(running, latest, releasesURL string) UpdateInfo {
	cur, ok := parseRelease(running)
	if !ok {
		return UpdateInfo{}
	}
	info := UpdateInfo{Current: running}
	lv, ok := parseRelease(latest)
	if !ok {
		return info
	}
	info.Latest = latest
	if cur.less(lv) {
		info.UpdateAvailable = true
		info.MajorUpgrade = lv[0] != cur[0]
		if strings.HasPrefix(releasesURL, "https://") {
			info.ReleaseNotesURL = strings.TrimRight(releasesURL, "/") + "/" + latest
		}
	}
	return info
}

// updateChecker caches the highest release of one repository.
type updateChecker struct {
	fetch func(ctx context.Context, repository string) ([]string, error)
	now   func() time.Time

	mu         sync.Mutex
	repository string
	latest     string
	checked    time.Time
	ttl        time.Duration
}

var defaultUpdateChecker = &updateChecker{
	fetch: func(ctx context.Context, repository string) ([]string, error) {
		return listRegistryTags(ctx, registryHTTPClient, repository)
	},
	now: time.Now,
}

// latestRelease returns the cached highest release of repository, reading
// the registry when the cache is empty or old. Concurrent callers wait for
// one read. A failed read keeps the previous answer for a shorter time.
func (c *updateChecker) latestRelease(repository string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if repository == c.repository && !c.checked.IsZero() && c.now().Sub(c.checked) < c.ttl {
		return c.latest
	}
	if repository != c.repository {
		c.latest = ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), updateCheckTimeout)
	defer cancel()
	tags, err := c.fetch(ctx, repository)
	c.repository, c.checked = repository, c.now()
	if err != nil {
		slog.Debug("update check: cannot read the release tags", "repository", repository, "error", err)
		c.ttl = updateCheckErrorTTL
		return c.latest
	}
	c.latest, c.ttl = highestRelease(tags), updateCheckTTL
	return c.latest
}

// HandleUpdateCheck reports whether a newer release of the updater exists.
func HandleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	info := compareReleases(Version, "", "")
	if repository := strings.TrimSpace(os.Getenv("IMAGE_REPOSITORY")); info.Current != "" && repository != "" {
		info = compareReleases(Version, defaultUpdateChecker.latestRelease(repository), os.Getenv("RELEASES_URL"))
	}
	writeJSON(w, info, "update-check")
}

// --- Registry tag list ------------------------------------------------------

var registryHTTPClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, IdleConnTimeout: 30 * time.Second},
}

// registryRepository splits "host/path" into the registry's base URL and the
// repository path, the way container tools name images: a first component
// without "." or ":" (and not localhost) is a Docker Hub repository.
func registryRepository(repository string) (base, path string, err error) {
	if repository == "" || strings.ContainsAny(repository, "@ ") {
		return "", "", fmt.Errorf("invalid image repository %q", repository)
	}
	host, rest, found := strings.Cut(repository, "/")
	if !found || (!strings.ContainsAny(host, ".:") && host != "localhost") {
		host, rest = "docker.io", repository
	}
	if strings.Contains(rest, ":") {
		return "", "", fmt.Errorf("image repository %q has a tag", repository)
	}
	if host == "docker.io" {
		host = "registry-1.docker.io"
		if !strings.Contains(rest, "/") {
			rest = "library/" + rest
		}
	}
	return "https://" + host, rest, nil
}

// listRegistryTags reads every tag of repository with the OCI distribution
// API (GET /v2/<name>/tags/list), following Link pagination. When the
// registry asks for a bearer token, an anonymous pull token is requested
// from the realm it names.
func listRegistryTags(ctx context.Context, client *http.Client, repository string) ([]string, error) {
	base, path, err := registryRepository(repository)
	if err != nil {
		return nil, err
	}
	client = withSameHostRedirects(client)
	next := base + "/v2/" + path + "/tags/list?n=1000"
	registryHost := strings.TrimPrefix(base, "https://")
	token := ""
	var tags []string
	for page := 0; next != "" && page < maxTagPages; page++ {
		resp, err := registryGet(ctx, client, next, token)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && token == "" {
			challenge := resp.Header.Get("WWW-Authenticate")
			drain(resp)
			if token, err = anonymousToken(ctx, client, challenge, registryHost, path); err != nil {
				return nil, err
			}
			page--
			continue
		}
		if resp.StatusCode != http.StatusOK {
			drain(resp)
			return nil, fmt.Errorf("tag list of %s: HTTP %d", repository, resp.StatusCode)
		}
		var body struct {
			Tags []string `json:"tags"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body)
		link := resp.Header.Get("Link")
		drain(resp)
		if err != nil {
			return nil, fmt.Errorf("tag list of %s: %w", repository, err)
		}
		tags = append(tags, body.Tags...)
		next = nextPage(next, link)
	}
	return tags, nil
}

func registryGet(ctx context.Context, client *http.Client, u, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return client.Do(req)
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
}

// withSameHostRedirects returns a copy of client that follows at most three
// redirects, each to https on the host of the original request.
func withSameHostRedirects(client *http.Client) *http.Client {
	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("registry: too many redirects")
		}
		if req.URL.Scheme != "https" || req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("registry: refusing a redirect to %s://%s", req.URL.Scheme, req.URL.Host)
		}
		return nil
	}
	return &c
}

// tokenHosts are the token services of registries whose realm is not the
// registry host itself.
var tokenHosts = map[string]string{
	"registry-1.docker.io": "auth.docker.io",
}

var challengeParamRe = regexp.MustCompile(`(\w+)="([^"]*)"`)

// anonymousToken asks the token service named in a Bearer challenge for a
// pull token (https://distribution.github.io/distribution/spec/auth/token/).
// The realm must be https on the registry host or its known token host, so
// a registry answer cannot point the backend at another server.
func anonymousToken(ctx context.Context, client *http.Client, challenge, registryHost, path string) (string, error) {
	scheme, params, _ := strings.Cut(challenge, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", fmt.Errorf("registry asks for %q authentication", scheme)
	}
	values := map[string]string{}
	for _, m := range challengeParamRe.FindAllStringSubmatch(params, -1) {
		values[strings.ToLower(m[1])] = m[2]
	}
	realm, err := url.Parse(values["realm"])
	if err != nil || realm.Scheme != "https" || realm.Host == "" {
		return "", fmt.Errorf("registry token realm %q is not an https URL", values["realm"])
	}
	if realm.Host != registryHost && realm.Host != tokenHosts[registryHost] {
		return "", fmt.Errorf("registry token realm %q is not on the registry host %s", values["realm"], registryHost)
	}
	q := realm.Query()
	if s := values["service"]; s != "" {
		q.Set("service", s)
	}
	scope := values["scope"]
	if scope == "" {
		scope = "repository:" + path + ":pull"
	}
	q.Set("scope", scope)
	realm.RawQuery = q.Encode()
	resp, err := registryGet(ctx, client, realm.String(), "")
	if err != nil {
		return "", err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry token: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("registry token: %w", err)
	}
	if body.Token != "" {
		return body.Token, nil
	}
	if body.AccessToken != "" {
		return body.AccessToken, nil
	}
	return "", fmt.Errorf("registry token: empty answer")
}

var nextLinkRe = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="?next"?`)

// nextPage resolves a Link: <...>; rel="next" header against the current
// URL; "" when there is no next page or it leaves the registry.
func nextPage(current, link string) string {
	m := nextLinkRe.FindStringSubmatch(link)
	if m == nil {
		return ""
	}
	cur, err := url.Parse(current)
	if err != nil {
		return ""
	}
	ref, err := url.Parse(m[1])
	if err != nil {
		return ""
	}
	next := cur.ResolveReference(ref)
	if next.Host != cur.Host || next.Scheme != cur.Scheme {
		return ""
	}
	return next.String()
}
