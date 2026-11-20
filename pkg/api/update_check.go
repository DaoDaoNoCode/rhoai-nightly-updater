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

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
)

// The update check compares the running release with the highest vX.Y.Z
// tag of the updater's image repository (IMAGE_REPOSITORY, set by the
// template). It reads the registry's tag list anonymously (OCI distribution
// API), caches the answer for hours and ignores errors: an unreachable
// registry only means no "update available" banner. Pre-release tags
// (vX.Y.Z-rc.N) are never offered; an updater that runs one is offered its
// release once it exists (compareReleases).

const (
	updateCheckTTL      = 6 * time.Hour
	updateCheckErrorTTL = 30 * time.Minute
	updateCheckTimeout  = 20 * time.Second
	// Pages of the tag list read at most (1000 tags each).
	maxTagPages = 20
)

var releaseTagRe = regexp.MustCompile(`^v(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})$`)

// preReleaseRe is a pre-release as scripts/release.sh tags them:
// vX.Y.Z-alpha.N, -beta.N or -rc.N, N from 1 without leading zeros.
var preReleaseRe = regexp.MustCompile(`^(v[0-9.]+)-(?:alpha|beta|rc)\.[1-9]\d{0,8}$`)

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

// parsePreRelease parses a pre-release (v5.0.0-rc.1) into the release it
// leads to (5.0.0).
func parsePreRelease(s string) (releaseVersion, bool) {
	m := preReleaseRe.FindStringSubmatch(s)
	if m == nil {
		return releaseVersion{}, false
	}
	return parseRelease(m[1])
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
	// Current is the running release or pre-release; empty for main and
	// commit builds, which get no update check.
	Current string `json:"current,omitempty"`
	// Latest is the highest release in the image repository, when known;
	// never a pre-release.
	Latest          string `json:"latest,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable"`
	// MajorUpgrade: Latest has another MAJOR version, so it needs its own
	// deploy template (a full redeploy), not just a new image.
	MajorUpgrade    bool   `json:"majorUpgrade"`
	ReleaseNotesURL string `json:"releaseNotesURL,omitempty"`
	// InstallerURL is where Latest's install.sh is downloaded from, when
	// the installer's releases page (installerReleasesURL) is a GitHub or
	// GitLab releases page.
	InstallerURL string `json:"installerURL,omitempty"`
	// InstallerSHA256 is the SHA-256 of that install.sh as Latest's release
	// notes publish it (release.sh notes: "install.sh SHA-256: `<hex>`"),
	// read from the releases page's API; empty when it cannot be read.
	InstallerSHA256 string `json:"installerSHA256,omitempty"`
	// PinnedInGit: the gitops profile does not check the registry; the
	// chart pins the release in Git (HandleUpdateCheckGitOps).
	PinnedInGit bool `json:"pinnedInGit,omitempty"`
}

const (
	// canonicalReleasesURL is the template's default RELEASES_URL.
	canonicalReleasesURL = "https://gitlab.com/redhat/ai/rhoai-dashboard-team/rhoai-nightly-updater/-/releases"
	// mirrorReleasesURL is the public GitHub mirror of that project. Its
	// release job attaches the same install.sh: the installer is generated
	// from the tag alone (scripts/release.sh installer), so both CI systems
	// build identical bytes (v2.0.0: SHA-256 b08dd37d… on both).
	mirrorReleasesURL = "https://github.com/DaoDaoNoCode/rhoai-nightly-updater/releases"
)

// installerReleasesURL is the releases page to download install.sh from.
// INSTALLER_RELEASES_URL overrides it. The canonical GitLab project is not
// public: anonymous downloads of its release assets redirect to the sign-in
// page, so curl would save that page; its public mirror is used instead.
// Any other releases page (a fork's) serves its own installer.
func installerReleasesURL(releasesURL string) string {
	if v := strings.TrimSpace(os.Getenv("INSTALLER_RELEASES_URL")); v != "" {
		return v
	}
	if strings.TrimRight(releasesURL, "/") == canonicalReleasesURL {
		return mirrorReleasesURL
	}
	return releasesURL
}

// compareReleases builds the answer for the running version and the highest
// release found. The release notes and the installer both come from
// installerReleases, the public releases page (installerReleasesURL): the
// canonical GitLab project's release pages redirect anonymous readers to its
// sign-in page, and its public GitHub mirror publishes the same notes with
// each release. releasesURL is used for the notes only when installerReleases
// is not an https URL.
//
// A running pre-release of X.Y.Z is offered the latest release once that is
// X.Y.Z or higher. latest is never a pre-release (highestRelease skips
// them), so nobody is offered one. MajorUpgrade compares X with the latest
// release's MAJOR, as for a release.
func compareReleases(running, latest, releasesURL, installerReleases string) UpdateInfo {
	cur, ok := parseRelease(running)
	pre := false
	if !ok {
		if cur, ok = parsePreRelease(running); !ok {
			return UpdateInfo{}
		}
		pre = true
	}
	info := UpdateInfo{Current: running}
	lv, ok := parseRelease(latest)
	if !ok {
		return info
	}
	info.Latest = latest
	if cur.less(lv) || (pre && cur == lv) {
		info.UpdateAvailable = true
		info.MajorUpgrade = lv[0] != cur[0]
		notes := installerReleases
		if !strings.HasPrefix(notes, "https://") {
			notes = releasesURL
		}
		if strings.HasPrefix(notes, "https://") {
			info.ReleaseNotesURL = releaseNotesURL(strings.TrimRight(notes, "/"), latest)
		}
		if strings.HasPrefix(installerReleases, "https://") {
			info.InstallerURL = installerURL(strings.TrimRight(installerReleases, "/"), latest)
		}
	}
	return info
}

// safeInstallerURL is what an installer URL may look like. The URL ends up
// in a shell command that admins copy (the update notice), so it allows no
// shell syntax: https, a host with an optional port, and a path of
// unreserved characters and slashes only. No "$", backticks, quotes,
// spaces, ";", "&", "|", parentheses, "<", ">", backslashes or "%".
var safeInstallerURL = regexp.MustCompile(`^https://[A-Za-z0-9._~-]+(:[0-9]{1,5})?/[A-Za-z0-9._~/-]*$`)

// installerURL returns the download URL of tag's install.sh asset for a
// releases page (RELEASES_URL), as the release jobs publish it: GitHub
// release assets at <owner>/<repo>/releases/download/<tag>/<asset>
// (.github/workflows/release.yml), GitLab release links at
// <project>/-/releases/<tag>/downloads/<direct_asset_path>
// (scripts/release.sh gitlab-release). "" for any other page.
// releaseNotesURL is the page of one release on a releases page: GitHub's
// documented release link is /OWNER/REPO/releases/tag/TAG ("Linking to
// releases", GitHub docs); GitLab's is .../-/releases/TAG, and any other
// releases page gets /TAG as before.
func releaseNotesURL(releasesURL, tag string) string {
	if u, err := url.Parse(releasesURL); err == nil && u.Host == "github.com" {
		if segs := strings.Split(strings.Trim(u.Path, "/"), "/"); len(segs) == 3 && segs[2] == "releases" {
			return releasesURL + "/tag/" + tag
		}
	}
	return releasesURL + "/" + tag
}

func installerURL(releasesURL, tag string) string {
	u, err := url.Parse(releasesURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	var out string
	switch {
	case len(segs) >= 3 && segs[len(segs)-2] == "-" && segs[len(segs)-1] == "releases":
		out = releasesURL + "/" + tag + "/downloads/install.sh"
	case u.Host == "github.com" && len(segs) == 3 && segs[2] == "releases":
		out = releasesURL + "/download/" + tag + "/install.sh"
	}
	if !safeInstallerURL.MatchString(out) {
		return ""
	}
	return out
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

// --- The installer's checksum ------------------------------------------------

// releaseAPIURL is the API address of one release of a releases page, and
// the JSON field holding its notes: GitHub's "Get a release by tag name"
// (GET /repos/{owner}/{repo}/releases/tags/{tag}, field body) and GitLab's
// "Get a release by a tag name" (GET /projects/:id/releases/:tag_name, id
// the URL-encoded project path, field description). "" for any other page.
func releaseAPIURL(releasesURL, tag string) (apiURL, field string) {
	if _, ok := parseRelease(tag); !ok {
		return "", ""
	}
	u, err := url.Parse(strings.TrimRight(releasesURL, "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", ""
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch {
	case u.Host == "github.com" && len(segs) == 3 && segs[2] == "releases":
		return "https://api.github.com/repos/" + url.PathEscape(segs[0]) + "/" + url.PathEscape(segs[1]) + "/releases/tags/" + tag, "body"
	case len(segs) >= 3 && segs[len(segs)-2] == "-" && segs[len(segs)-1] == "releases":
		project := strings.Join(segs[:len(segs)-2], "/")
		return "https://" + u.Host + "/api/v4/projects/" + url.PathEscape(project) + "/releases/" + tag, "description"
	}
	return "", ""
}

// installerSumRe is the checksum line of the release notes (scripts/release.sh notes).
var installerSumRe = regexp.MustCompile("install\\.sh SHA-256: `([0-9a-f]{64})`")

// checksumCache keeps the installer checksum per release API URL.
type checksumCache struct {
	fetch func(ctx context.Context, apiURL, field string) (string, error)
	now   func() time.Time

	mu      sync.Mutex
	entries map[string]checksumEntry
}

type checksumEntry struct {
	sum     string
	expires time.Time
}

var defaultChecksums = &checksumCache{fetch: fetchReleaseNotes, now: time.Now}

// installerSHA256 returns the install.sh checksum the release notes of tag
// publish, or "" (no supported releases page, no such line, or the API could
// not be read: the notice then points at the release notes). Answers are
// cached like the release list; failures for a shorter time.
func (c *checksumCache) installerSHA256(releasesURL, tag string) string {
	apiURL, field := releaseAPIURL(releasesURL, tag)
	if apiURL == "" {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[apiURL]; ok && c.now().Before(e.expires) {
		return e.sum
	}
	ctx, cancel := context.WithTimeout(context.Background(), updateCheckTimeout)
	defer cancel()
	notes, err := c.fetch(ctx, apiURL, field)
	ttl := updateCheckTTL
	sum := ""
	if err != nil {
		slog.Debug("update check: cannot read the release notes", "url", apiURL, "error", err)
		ttl = updateCheckErrorTTL
	} else if m := installerSumRe.FindStringSubmatch(notes); m != nil {
		sum = m[1]
	}
	if c.entries == nil {
		c.entries = map[string]checksumEntry{}
	}
	c.entries[apiURL] = checksumEntry{sum: sum, expires: c.now().Add(ttl)}
	return sum
}

// fetchReleaseNotes reads one release's notes from its API, anonymously.
func fetchReleaseNotes(ctx context.Context, apiURL, field string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := withSameHostRedirects(registryHTTPClient).Do(req)
	if err != nil {
		return "", err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("release notes: HTTP %d", resp.StatusCode)
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("release notes: %w", err)
	}
	var notes string
	if raw, ok := body[field]; ok {
		if err := json.Unmarshal(raw, &notes); err != nil {
			return "", fmt.Errorf("release notes: %s: %w", field, err)
		}
	}
	return notes, nil
}

// HandleUpdateCheck reports whether a newer release of the updater exists,
// to a signed-in user (withAuth), as every other API read: the answer
// reads the registry, which an anonymous request must not trigger.
var HandleUpdateCheck = withAuth(func(_ *cluster.Client, w http.ResponseWriter, _ *http.Request) {
	info := compareReleases(Version, "", "", "")
	if repository := strings.TrimSpace(os.Getenv("IMAGE_REPOSITORY")); info.Current != "" && repository != "" {
		releases := os.Getenv("RELEASES_URL")
		installerReleases := installerReleasesURL(releases)
		info = compareReleases(Version, defaultUpdateChecker.latestRelease(repository), releases, installerReleases)
		if info.InstallerURL != "" {
			info.InstallerSHA256 = defaultChecksums.installerSHA256(strings.TrimRight(installerReleases, "/"), info.Latest)
		}
	}
	writeJSON(w, info, "update-check")
})

// HandleUpdateCheckGitOps is HandleUpdateCheck in the gitops profile: no
// registry is read. The chart pins the updater's release in Git, so a newer
// release arrives as a change there (a new targetRevision and image digest),
// not as a banner: the answer names the running release only, with
// pinnedInGit set.
func HandleUpdateCheckGitOps(w http.ResponseWriter, r *http.Request) {
	info := compareReleases(Version, "", "", "")
	info.PinnedInGit = true
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
	if next != "" {
		// The newest release may be on a page not read: no answer is
		// better than a wrong one (the caller does not cache errors).
		return nil, fmt.Errorf("tag list of %s: more than %d pages, incomplete", repository, maxTagPages)
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
