package cluster

// "Which nightly contains PR #N?" and "which nightly contains commit X?"
//
// Pull requests are opened and merged upstream (opendatahub-io/odh-dashboard,
// opendatahub-io/data-science-pipelines, ...), while RHOAI images are built
// from the red-hat-data-services forks: every component image of a nightly
// carries a git.url label https://github.com/red-hat-data-services/<name> and
// a commit label (checked 2026-10-29: 165 related images in the rhoai-3.6
// build, 14 of them from red-hat-data-services/odh-dashboard; about 24 base
// images have no git.url). The dashboard fork's release branches are updated
// by merging upstream main ("Merge remote-tracking branch 'upstream/main'
// into rhoai-3.6"), so upstream SHAs keep their identity in the fork:
// GET /repos/red-hat-data-services/odh-dashboard/commits/<7-char upstream
// sha> resolves, compare/<old upstream sha>...<rhoai-3.6 head> is "ahead"
// (contained) and compare/<today's upstream main head>...<rhoai-3.6 head> is
// "diverged" (not merged yet). Forks of one GitHub network resolve each
// other's SHAs (checked 2026-10-30: GET /repos/red-hat-data-services/
// data-science-pipelines/commits/<upstream merge sha> is 200 and the compare
// with a fork build commit is "diverged" although the fork never merged it),
// so a fork that cherry-picks or rebases upstream changes answers "not
// contained" for a PR it took that way: the cherry-pick has another SHA.
// A compare 404 only happens for a SHA outside the network, which is
// reported as unknown ("commit not on GitHub").
//
// A build whose image was pushed more than an hour before the PR's merge
// date (or the commit's date) cannot contain it: its answer comes from the
// FBC image's build-date label alone, with no catalog or GitHub read.
//
// GitHub compare semantics (GET /repos/{owner}/{repo}/compare/{base}...{head}):
// "ahead" or "identical" means head contains base; "behind" or "diverged"
// means it does not.

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
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// DefaultPRSearchRepo is the repository searched when none is given.
const DefaultPRSearchRepo = "opendatahub-io/odh-dashboard"

// forkOwner owns the forks the RHOAI images are built from.
const forkOwner = "red-hat-data-services"

// searchRepoOwners are the GitHub owners a search may name: the upstreams the
// RHOAI component forks come from, and the forks' owner. A search makes the
// server call GitHub with its GITHUB_TOKEN for whatever repository is named,
// so the repository is not free text. Compared case-insensitively, as GitHub
// names are.
var searchRepoOwners = []string{"opendatahub-io", "red-hat-data-services", "kubeflow", "kserve", "trustyai-explainability", "llm-d", "feast-dev", "ray-project"}

// SearchRepoMessage is the validation error for a repository the search does
// not cover.
const SearchRepoMessage = "PR and commit search cover repositories of opendatahub-io, red-hat-data-services, kubeflow, kserve, trustyai-explainability, llm-d, feast-dev and ray-project."

var (
	// searchRepoOwnerPattern: GitHub owners have no dots, so "../x" is out.
	searchRepoOwnerPattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	// searchRepoNamePattern: a repository name (it ends up in GitHub API paths).
	searchRepoNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
)

// ValidSearchRepo reports whether repo is a well-formed "owner/name" whose
// owner is one of searchRepoOwners.
func ValidSearchRepo(repo string) bool {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !searchRepoOwnerPattern.MatchString(owner) || !searchRepoNamePattern.MatchString(name) || strings.Trim(name, ".") == "" {
		return false
	}
	for _, o := range searchRepoOwners {
		if strings.EqualFold(o, owner) {
			return true
		}
	}
	return false
}

// invalidSearchRepo is the error for a repository ValidSearchRepo refuses.
func invalidSearchRepo() error {
	return &PRSearchError{Status: http.StatusBadRequest, Code: "validation", Msg: SearchRepoMessage}
}

// forkCandidates lists the repositories a build's component image may have
// been built from for a search of repo: the red-hat-data-services fork of the
// same name, and repo itself (the PR or commit may be on the fork already).
func forkCandidates(repo string) []string {
	_, name, _ := strings.Cut(repo, "/")
	fork := forkOwner + "/" + name
	if strings.EqualFold(fork, repo) {
		return []string{repo}
	}
	return []string{fork, repo}
}

// isCandidate reports whether repo is one of candidates (case-insensitive,
// as GitHub names are).
func isCandidate(candidates []string, repo string) bool {
	for _, c := range candidates {
		if strings.EqualFold(c, repo) {
			return true
		}
	}
	return false
}

const (
	// prMaxBuilds bounds the builds one request checks.
	prMaxBuilds = 40
	// builtBeforeMargin: a build is answered from its date alone only when
	// it was built more than this long before the merge or the commit. A
	// committer date comes from the developer's clock and the build date from
	// the registry's, so they may disagree by minutes. (A committer date
	// forged far into the future would make every build "older" than the
	// commit and skip them all; that is a known limit.)
	builtBeforeMargin = time.Hour
	// prConcurrency bounds the builds checked at once (each needs a catalog,
	// a label read and one GitHub call).
	prConcurrency = 4
	// prRequestTimeout bounds one request.
	prRequestTimeout = 60 * time.Second
	// prPendingTTL keeps the answer for a PR that is not merged, or not
	// found, for a short time: it may be merged any minute.
	prPendingTTL = time.Minute
	// compareNegativeTTL keeps "commit not found" for a while; it may be
	// pushed later.
	compareNegativeTTL = 10 * time.Minute
	// githubBodyLimit bounds a GitHub response body (a compare page with one
	// commit and no files is about 16 KB, a pull request about 30 KB).
	githubBodyLimit = 2 << 20
	// componentImageCacheMax bounds the remembered "which image of this build
	// is the component" answers (one per build and searched repository).
	componentImageCacheMax = 1024
)

// PRSearchError is a request-level failure with an API errorCode.
type PRSearchError struct {
	Status int
	Code   string
	Msg    string
}

func (e *PRSearchError) Error() string { return e.Msg }

// githubRateLimitError means GitHub refused the call because of a rate limit.
type githubRateLimitError struct{ until time.Time }

func (e *githubRateLimitError) Error() string { return "GitHub API rate limit reached" }

type prInfo struct {
	merged      bool
	state       string
	title       string
	url         string
	mergeCommit string
	mergedAt    string
	headSHA     string // the PR branch's current commit
	notFound    bool
}

// commitInfo is a commit GitHub resolved (possibly from a short SHA).
type commitInfo struct {
	sha      string // full, lower case
	date     string // committer date
	title    string // first line of the message
	url      string
	repo     string // where it was found
	notFound bool
}

type compareVerdict struct {
	contains bool
	notFound bool // a commit is unknown to GitHub
}

var (
	prCache       = newLRU[string, prInfo](512)
	prFlight      flightGroup[prInfo]
	compareCache  = newLRU[string, compareVerdict](4096)
	compareFlight flightGroup[compareVerdict]
	// commitCache remembers resolved commits for good (a SHA never changes
	// meaning) and misses for compareNegativeTTL.
	commitCache  = newLRU[string, commitInfo](1024)
	commitLookup flightGroup[commitInfo]
	// componentImageCache remembers which related image of a build is the
	// searched component, so a repeated search does not rescan the labels.
	componentImageCache = newLRU[string, componentLookup](componentImageCacheMax)
)

// githubBackoffUntil returns when GitHub calls may resume (zero when they may now).
func githubBackoffUntil() time.Time {
	githubBackoff.Lock()
	defer githubBackoff.Unlock()
	if time.Now().Before(githubBackoff.until) {
		return githubBackoff.until
	}
	return time.Time{}
}

// githubGet calls the GitHub REST API. A rate-limited answer starts the shared
// backoff and returns *githubRateLimitError. The caller closes the body.
func githubGet(ctx context.Context, path string) (*http.Response, error) {
	if until := githubBackoffUntil(); !until.IsZero() {
		return nil, &githubRateLimitError{until: until}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPIBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "rhoai-nightly-updater")
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := quayHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && (resp.Header.Get("X-Ratelimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "")) {
		resp.Body.Close()
		until := githubRateLimitReset(resp, time.Now())
		setGitHubBackoff(until)
		slog.Warn("GitHub API rate limit reached; PR search and commit dates pause until it resets (set GITHUB_TOKEN to raise the limit)",
			"status", resp.StatusCode, "until", until.UTC().Format(time.RFC3339))
		return nil, &githubRateLimitError{until: until}
	}
	return resp, nil
}

// fetchPullRequest reads a pull request from GitHub. A 404 is notFound, not
// an error. The merge commit and head SHA are kept only when well-formed
// (they end up in API paths).
func fetchPullRequest(ctx context.Context, repo string, number int) (prInfo, error) {
	key := fmt.Sprintf("%s#%d", repo, number)
	resp, err := githubGet(ctx, fmt.Sprintf("/repos/%s/pulls/%d", repo, number))
	if err != nil {
		return prInfo{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, githubBodyLimit))
	if err != nil {
		return prInfo{}, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return prInfo{notFound: true}, nil
	case resp.StatusCode != http.StatusOK:
		return prInfo{}, fmt.Errorf("GitHub returned %d for pull request %s", resp.StatusCode, key)
	}
	var pr struct {
		State          string `json:"state"`
		Title          string `json:"title"`
		HTMLURL        string `json:"html_url"`
		Merged         bool   `json:"merged"`
		MergedAt       string `json:"merged_at"`
		MergeCommitSHA string `json:"merge_commit_sha"`
		Head           struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return prInfo{}, fmt.Errorf("parse pull request: %w", err)
	}
	// An open PR also has a merge_commit_sha (GitHub's test merge), so
	// only "merged" counts.
	info := prInfo{merged: pr.Merged && pr.MergedAt != "", state: pr.State, title: pr.Title, url: pr.HTMLURL, mergedAt: pr.MergedAt}
	if info.merged && githubSHAPattern.MatchString(pr.MergeCommitSHA) {
		info.mergeCommit = pr.MergeCommitSHA
	}
	if githubSHAPattern.MatchString(pr.Head.SHA) {
		info.headSHA = strings.ToLower(pr.Head.SHA)
	}
	return info, nil
}

// lookupPR reads a pull request. Merged PRs are cached for good: the merge
// commit never changes.
func lookupPR(ctx context.Context, repo string, number int) (prInfo, error) {
	key := fmt.Sprintf("%s#%d", repo, number)
	if info, ok := prCache.Get(key); ok {
		return info, nil
	}
	info, err, _ := prFlight.Do(ctx, key, func(ctx context.Context) (prInfo, error) {
		if info, ok := prCache.Get(key); ok {
			return info, nil
		}
		info, err := fetchPullRequest(ctx, repo, number)
		switch {
		case err != nil:
			return prInfo{}, err
		case info.notFound:
			prCache.Add(key, info, prPendingTTL)
		case info.merged && info.mergeCommit == "":
			return prInfo{}, fmt.Errorf("pull request %s has no usable merge commit", key)
		case info.merged:
			prCache.Add(key, info, 0)
		default:
			prCache.Add(key, info, prPendingTTL)
		}
		return info, nil
	})
	return info, err
}

// fetchCommit resolves sha (7 to 40 hex) in repo. A 404 or 422 is notFound,
// not an error.
func fetchCommit(ctx context.Context, repo, sha string) (commitInfo, error) {
	resp, err := githubGet(ctx, fmt.Sprintf("/repos/%s/commits/%s", repo, sha))
	if err != nil {
		return commitInfo{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, githubBodyLimit))
	if err != nil {
		return commitInfo{}, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnprocessableEntity:
		return commitInfo{notFound: true}, nil
	case resp.StatusCode != http.StatusOK:
		return commitInfo{}, fmt.Errorf("GitHub returned %d for commit %s of %s", resp.StatusCode, sha, repo)
	}
	var c struct {
		SHA     string `json:"sha"`
		HTMLURL string `json:"html_url"`
		Commit  struct {
			Message   string `json:"message"`
			Committer struct {
				Date string `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(body, &c); err != nil {
		return commitInfo{}, fmt.Errorf("parse commit: %w", err)
	}
	full := strings.ToLower(c.SHA)
	if !dashboardCommitSHAPattern.MatchString(full) {
		return commitInfo{}, fmt.Errorf("GitHub reported no usable SHA for commit %s of %s", sha, repo)
	}
	title, _, _ := strings.Cut(c.Commit.Message, "\n")
	return commitInfo{sha: full, date: c.Commit.Committer.Date, title: strings.TrimSpace(title), url: c.HTMLURL, repo: repo}, nil
}

// lookupCommit resolves sha in repo, then in each fork candidate, and returns
// the first that knows it. Hits are cached for good, misses for a while.
func lookupCommit(ctx context.Context, repo, sha string) (commitInfo, error) {
	sha = strings.ToLower(sha)
	repos := []string{repo}
	for _, c := range forkCandidates(repo) {
		if !isCandidate(repos, c) {
			repos = append(repos, c)
		}
	}
	key := strings.ToLower(strings.Join(repos, ",")) + "|" + sha
	if info, ok := commitCache.Get(key); ok {
		return info, nil
	}
	info, err, _ := commitLookup.Do(ctx, key, func(ctx context.Context) (commitInfo, error) {
		if info, ok := commitCache.Get(key); ok {
			return info, nil
		}
		for _, r := range repos {
			info, err := fetchCommit(ctx, r, sha)
			if err != nil {
				return commitInfo{}, err
			}
			if !info.notFound {
				commitCache.Add(key, info, 0)
				return info, nil
			}
		}
		miss := commitInfo{notFound: true}
		commitCache.Add(key, miss, compareNegativeTTL)
		return miss, nil
	})
	return info, err
}

// compareContains asks GitHub whether head contains base, in repo. SHAs are
// immutable, so answers are cached for good (a missing commit for a while).
func compareContains(ctx context.Context, repo, base, head string) (compareVerdict, error) {
	key := repo + "|" + base + "|" + head
	if v, ok := compareCache.Get(key); ok {
		return v, nil
	}
	v, err, _ := compareFlight.Do(ctx, key, func(ctx context.Context) (compareVerdict, error) {
		if v, ok := compareCache.Get(key); ok {
			return v, nil
		}
		// Page 2 with one commit per page carries the status but neither the
		// commit list nor the changed files (only page 1 lists files): about
		// 16 KB instead of 1-2 MB for a release branch.
		resp, err := githubGet(ctx, fmt.Sprintf("/repos/%s/compare/%s...%s?per_page=1&page=2", repo, base, head))
		if err != nil {
			return compareVerdict{}, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, githubBodyLimit))
		if err != nil {
			return compareVerdict{}, err
		}
		switch {
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnprocessableEntity:
			v := compareVerdict{notFound: true}
			compareCache.Add(key, v, compareNegativeTTL)
			return v, nil
		case resp.StatusCode != http.StatusOK:
			return compareVerdict{}, fmt.Errorf("GitHub compare returned %d", resp.StatusCode)
		}
		var cmp struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(body, &cmp); err != nil {
			return compareVerdict{}, fmt.Errorf("parse compare: %w", err)
		}
		var v compareVerdict
		switch cmp.Status {
		case "ahead", "identical":
			v.contains = true
		case "behind", "diverged":
		default:
			return compareVerdict{}, fmt.Errorf("unexpected compare status %q", cmp.Status)
		}
		compareCache.Add(key, v, 0)
		return v, nil
	})
	return v, err
}

// githubRepoFromURL turns a git.url label into "owner/name".
func githubRepoFromURL(gitURL string) (string, bool) {
	key, ok := commitDateKey(gitURL, "0000000")
	if !ok {
		return "", false
	}
	return strings.TrimSuffix(key, "/0000000"), true
}

// prBuild is one build to check.
type prBuild struct {
	image     string
	installed bool
}

// containsSearch is what the builds are checked against: a PR's merge commit
// or a commit, in a repository and its fork candidates.
type containsSearch struct {
	repo       string
	candidates []string
	// base is the commit a build must contain; empty when GitHub could not
	// be asked (rate limit): builds are then "unknown" with their commit.
	base string
	// baseDate is when base was merged or committed; builds older than it
	// are answered without any read. Zero when unknown.
	baseDate time.Time
	// commit is true for a commit search, false for a PR search (wording).
	commit bool
}

// FindBuildsContainingPR reports which nightly builds contain merged PR
// number of repo. images are digest-pinned FBC references; when empty, the
// installed nightly build and the newest build of every tag are checked.
func FindBuildsContainingPR(ctx context.Context, c *Client, repo string, number int, images []string) (*types.PRContainsResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, prRequestTimeout)
	defer cancel()
	basicAuth := getQuayAuth(c)
	return findBuildsContainingPR(ctx, basicAuth, repo, number, buildLister(c, basicAuth, images))
}

// FindBuildsContainingCommit reports which nightly builds contain commit sha
// (7 to 40 hex) of repo, resolved on repo and then on its fork candidates.
func FindBuildsContainingCommit(ctx context.Context, c *Client, repo, sha string, images []string) (*types.PRContainsResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, prRequestTimeout)
	defer cancel()
	basicAuth := getQuayAuth(c)
	return findBuildsContainingCommit(ctx, basicAuth, repo, sha, buildLister(c, basicAuth, images))
}

// buildLister lists the given images, or the default builds when none are given.
func buildLister(c *Client, basicAuth string, images []string) func(context.Context) ([]prBuild, error) {
	return func(ctx context.Context) ([]prBuild, error) {
		if len(images) > 0 {
			builds := make([]prBuild, 0, len(images))
			for _, image := range images {
				builds = append(builds, prBuild{image: image})
			}
			return builds, nil
		}
		return defaultPRBuilds(ctx, c, basicAuth)
	}
}

// defaultPRBuilds lists the installed nightly build (when the Subscription
// uses the nightly CatalogSource) and the newest build of every tag.
func defaultPRBuilds(ctx context.Context, c *Client, basicAuth string) ([]prBuild, error) {
	var builds []prBuild
	installedDigest := ""
	if sub, err := getSubscription(c); err == nil && sub.Source == CatalogName {
		if cs, err := getCatalogSource(c); err == nil && cs.Exists && isNightlyStreamImage(cs.Image) {
			if _, digest, ok := extractRepoAndDigest(cs.Image); ok {
				installedDigest = digest
				builds = append(builds, prBuild{image: cs.Image, installed: true})
			}
		}
	}
	tags, err := fetchNightlyTags(ctx, basicAuth, 0, false)
	if err != nil {
		return nil, err
	}
	for _, t := range tags.Tags {
		_, digest, ok := extractRepoAndDigest(t.Image)
		if !ok || digest == installedDigest {
			continue
		}
		builds = append(builds, prBuild{image: t.Image})
	}
	if len(builds) > prMaxBuilds {
		builds = builds[:prMaxBuilds]
	}
	return builds, nil
}

// rateLimitNoter records a GitHub rate limit on the response.
func rateLimitNoter(resp *types.PRContainsResponse) func(time.Time) {
	var mu sync.Mutex
	return func(until time.Time) {
		mu.Lock()
		defer mu.Unlock()
		resp.RateLimited = true
		if secs := int(time.Until(until).Round(time.Second) / time.Second); secs > resp.RetryAfterSeconds {
			resp.RetryAfterSeconds = secs
		}
	}
}

func findBuildsContainingPR(ctx context.Context, basicAuth, repo string, number int, list func(context.Context) ([]prBuild, error)) (*types.PRContainsResponse, error) {
	if !ValidSearchRepo(repo) {
		return nil, invalidSearchRepo()
	}
	resp := &types.PRContainsResponse{Repo: repo, PR: number, URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, number), Builds: []types.PRContainsBuild{}}
	noteRateLimit := rateLimitNoter(resp)
	search := containsSearch{repo: repo, candidates: forkCandidates(repo)}

	info, err := lookupPR(ctx, repo, number)
	var limited *githubRateLimitError
	switch {
	case errors.As(err, &limited):
		noteRateLimit(limited.until)
	case err != nil:
		if isContextError(err) {
			return nil, err
		}
		slog.Warn("PR search: reading the pull request failed", "repo", repo, "pr", number, "error", err)
		return nil, &PRSearchError{Status: http.StatusBadGateway, Code: "upstream_error", Msg: fmt.Sprintf("Could not read PR #%d from GitHub: %v", number, err)}
	case info.notFound:
		return nil, &PRSearchError{Status: http.StatusNotFound, Code: "pr_not_found", Msg: fmt.Sprintf("%s has no pull request #%d", repo, number)}
	case !info.merged:
		state := "open"
		if info.state == "closed" {
			state = "closed without being merged"
		}
		return nil, &PRSearchError{Status: http.StatusUnprocessableEntity, Code: "pr_not_merged",
			Msg: fmt.Sprintf("PR #%d is %s, so no build contains it", number, state)}
	default:
		resp.Title, resp.MergeCommit, resp.MergedAt = info.title, info.mergeCommit, info.mergedAt
		if info.url != "" {
			resp.URL = info.url
		}
		search.base = info.mergeCommit
		if t, err := time.Parse(time.RFC3339, info.mergedAt); err == nil {
			search.baseDate = t
		}
	}
	return runContainsSearch(ctx, basicAuth, search, list, resp, noteRateLimit)
}

func findBuildsContainingCommit(ctx context.Context, basicAuth, repo, sha string, list func(context.Context) ([]prBuild, error)) (*types.PRContainsResponse, error) {
	if !ValidSearchRepo(repo) {
		return nil, invalidSearchRepo()
	}
	if !githubSHAPattern.MatchString(sha) || len(sha) > 40 {
		return nil, &PRSearchError{Status: http.StatusBadRequest, Code: "validation", Msg: "'commit' must be 7 to 40 hex characters"}
	}
	sha = strings.ToLower(sha)
	resp := &types.PRContainsResponse{Repo: repo, Commit: sha, Builds: []types.PRContainsBuild{}}
	noteRateLimit := rateLimitNoter(resp)
	search := containsSearch{repo: repo, candidates: forkCandidates(repo), commit: true}

	info, err := lookupCommit(ctx, repo, sha)
	var limited *githubRateLimitError
	switch {
	case errors.As(err, &limited):
		noteRateLimit(limited.until)
	case err != nil:
		if isContextError(err) {
			return nil, err
		}
		slog.Warn("commit search: resolving the commit failed", "repo", repo, "commit", sha, "error", err)
		return nil, &PRSearchError{Status: http.StatusBadGateway, Code: "upstream_error", Msg: fmt.Sprintf("Could not read commit %s from GitHub: %v", shortSHA(sha), err)}
	case info.notFound:
		return nil, &PRSearchError{Status: http.StatusNotFound, Code: "commit_not_found",
			Msg: fmt.Sprintf("GitHub does not know commit %s in %s.", shortSHA(sha), strings.Join(reposFor(repo), " or "))}
	default:
		resp.Commit, resp.CommitDate, resp.CommitTitle, resp.CommitURL = info.sha, info.date, info.title, info.url
		search.base = info.sha
		if t, err := time.Parse(time.RFC3339, info.date); err == nil {
			search.baseDate = t
		}
	}
	return runContainsSearch(ctx, basicAuth, search, list, resp, noteRateLimit)
}

// reposFor is repo followed by its fork candidates, without repeats.
func reposFor(repo string) []string {
	out := []string{repo}
	for _, c := range forkCandidates(repo) {
		if !isCandidate(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// runContainsSearch checks every listed build against search, a few at a time.
func runContainsSearch(ctx context.Context, basicAuth string, search containsSearch, list func(context.Context) ([]prBuild, error), resp *types.PRContainsResponse, noteRateLimit func(time.Time)) (*types.PRContainsResponse, error) {
	builds, err := list(ctx)
	if err != nil {
		return nil, err
	}
	resp.Builds = make([]types.PRContainsBuild, len(builds))
	sem := make(chan struct{}, prConcurrency)
	var wg sync.WaitGroup
	for i, b := range builds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
			}
			resp.Builds[i] = checkBuildContains(ctx, basicAuth, search, b, noteRateLimit)
		}()
	}
	wg.Wait()
	if search.base != "" && strings.EqualFold(search.repo, DefaultPRSearchRepo) && !anyContains(resp.Builds) {
		resp.Stable = stableBranchInfo(ctx, search.base, noteRateLimit)
	}
	return resp, nil
}

func anyContains(builds []types.PRContainsBuild) bool {
	for _, b := range builds {
		if b.Result == "contains" {
			return true
		}
	}
	return false
}

// stableBranchInfo says whether upstream opendatahub-io/odh-dashboard's
// stable branch has commit base yet, and when stable was last updated. A
// dashboard change reaches the nightlies through that branch (checked
// 2026-10-30): main → stable (the sync-stable workflow, in batches; it was
// 12 commits and 7 hours behind main) → red-hat-data-services/odh-dashboard
// main ("Merge remote-tracking branch 'upstream/stable'") → the release
// branch (rhoai-3.6) → the nightly. So a commit can be a few commits behind
// main and in no nightly yet, and the page says so instead of leaving the
// reader to guess. Nil when GitHub could not be asked.
func stableBranchInfo(ctx context.Context, base string, noteRateLimit func(time.Time)) *types.StableBranch {
	const branch = "stable"
	head, err := lookupBranchHead(ctx, DefaultPRSearchRepo, branch)
	if err != nil {
		var limited *githubRateLimitError
		if errors.As(err, &limited) {
			noteRateLimit(limited.until)
		} else if !isContextError(err) {
			slog.Warn("contains search: reading the stable branch failed", "error", err)
		}
		return nil
	}
	verdict, err := compareContains(ctx, DefaultPRSearchRepo, base, head.sha)
	if err != nil || verdict.notFound {
		var limited *githubRateLimitError
		if errors.As(err, &limited) {
			noteRateLimit(limited.until)
		}
		return nil
	}
	out := &types.StableBranch{Branch: branch, Head: head.sha, Contains: verdict.contains}
	// The date is a nicety: without it the page still says which way it went.
	if info, err := lookupCommit(ctx, DefaultPRSearchRepo, head.sha); err == nil && !info.notFound {
		out.UpdatedAt = info.date
	}
	return out
}

// builtBefore reports whether the build's FBC image was pushed more than
// builtBeforeMargin before t, from its (cached) build-date label, and by how
// much. False when the date is unknown.
func builtBefore(ctx context.Context, basicAuth, image string, t time.Time) (time.Duration, bool) {
	if t.IsZero() {
		return 0, false
	}
	labels, err := imageLabelsWithAuth(ctx, basicAuth, image, false)
	if err != nil || labels.BuildDate == "" {
		return 0, false
	}
	built, err := time.Parse(time.RFC3339, labels.BuildDate)
	if err != nil || !built.Before(t.Add(-builtBeforeMargin)) {
		return 0, false
	}
	return t.Sub(built), true
}

// roughDuration words a duration for a sentence: "20 minutes", "3 hours", "12 days".
func roughDuration(d time.Duration) string {
	plural := func(n int, unit string) string {
		if n == 1 {
			return fmt.Sprintf("1 %s", unit)
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	switch {
	case d < time.Hour:
		n := int(d.Round(time.Minute) / time.Minute)
		if n < 1 {
			n = 1
		}
		return plural(n, "minute")
	case d < 48*time.Hour:
		return plural(int(d.Round(time.Hour)/time.Hour), "hour")
	default:
		return plural(int(d.Round(24*time.Hour)/(24*time.Hour)), "day")
	}
}

func checkBuildContains(ctx context.Context, basicAuth string, search containsSearch, b prBuild, noteRateLimit func(time.Time)) types.PRContainsBuild {
	out := types.PRContainsBuild{Image: b.image, Tag: extractTagFromRef(b.image), Installed: b.installed, Result: "unknown"}
	if err := ctx.Err(); err != nil {
		out.Reason, out.Message = "build_unreadable", "The search timed out before this build was checked."
		return out
	}
	// The cheapest answer first: a build pushed before the merge (or the
	// commit) cannot contain it, and its date is already cached by the tags
	// listing.
	if age, ok := builtBefore(ctx, basicAuth, b.image, search.baseDate); ok {
		out.Result = "not_contained"
		if search.commit {
			out.Reason, out.Message = "built_before_commit", fmt.Sprintf("Built %s before the commit.", roughDuration(age))
		} else {
			out.Reason, out.Message = "built_before_merge", fmt.Sprintf("Built %s before the PR was merged.", roughDuration(age))
		}
		return out
	}
	content, err := extractFBCContentWithAuth(ctx, basicAuth, b.image)
	if err != nil {
		out.Reason, out.Message = "build_unreadable", fmt.Sprintf("Could not read the build's catalog: %v", err)
		return out
	}
	component, err := findComponentImage(ctx, basicAuth, b.image, content, search.repo, search.candidates)
	if err != nil {
		out.Reason, out.Message = "build_unreadable", fmt.Sprintf("Could not read the component image labels: %v", err)
		return out
	}
	if component.image == "" {
		out.Reason, out.Message = component.reason, component.message
		return out
	}
	out.Commit, out.CommitRepo = component.commit, component.repo
	if search.base == "" {
		// The PR or commit could not be read (rate limit): the build commit is still shown.
		out.Reason = "rate_limited"
		return out
	}
	out.CompareURL = fmt.Sprintf("https://github.com/%s/compare/%s...%s", component.repo, search.base, component.commit)
	verdict, err := compareContains(ctx, component.repo, search.base, component.commit)
	var limited *githubRateLimitError
	switch {
	case errors.As(err, &limited):
		noteRateLimit(limited.until)
		out.Reason = "rate_limited"
	case err != nil:
		out.Reason, out.Message = "github_error", err.Error()
	case verdict.notFound:
		// Forks of one network resolve each other's SHAs, so this is a SHA
		// GitHub does not know at all, not a fork that cherry-picks.
		repos := []string{search.repo}
		if !isCandidate(repos, component.repo) {
			repos = append(repos, component.repo)
		}
		out.Reason = "commit_not_found"
		out.Message = fmt.Sprintf("GitHub does not know commit %s in %s.", shortSHA(search.base), strings.Join(repos, " or "))
	case verdict.contains:
		out.Result = "contains"
	default:
		out.Result = "not_contained"
	}
	return out
}

// componentLookup is which related image of a build was built from the
// searched repository (image empty: none, with the reason).
type componentLookup struct {
	image   string
	commit  string
	repo    string // "owner/name" from the image's git.url label
	reason  string
	message string
}

// findComponentImage finds the related image of a build built from one of
// candidates. The dashboard image is found by name first (the common search)
// and its git.url checked; otherwise the labels of the build's RHOAI images
// are read (a few at a time, cached) and the first in catalog order with a
// matching git.url and a well-formed commit wins: images of one repository
// are built from the same commit. The answer is remembered per build and
// repository, unless a label read failed, so the next search can retry.
func findComponentImage(ctx context.Context, basicAuth, fbcImage string, content *types.FBCContentResponse, repo string, candidates []string) (componentLookup, error) {
	_, digest, _ := extractRepoAndDigest(fbcImage)
	key := digest + "|" + strings.ToLower(repo)
	if v, ok := componentImageCache.Get(key); ok {
		return v, nil
	}
	miss := componentLookup{reason: "no_component_image", message: fmt.Sprintf("This build has no image built from %s.", strings.Join(candidates, " or "))}
	matches := func(labels *ImageLabels) (componentLookup, bool) {
		labelRepo, ok := githubRepoFromURL(labels.GitURL)
		if !ok || !isCandidate(candidates, labelRepo) || !githubSHAPattern.MatchString(labels.GitCommit) {
			return componentLookup{}, false
		}
		return componentLookup{commit: strings.ToLower(labels.GitCommit), repo: labelRepo}, true
	}

	// Fast path: the dashboard image by name.
	checked := ""
	if _, name, _ := strings.Cut(repo, "/"); strings.EqualFold(name, "odh-dashboard") {
		if image := dashboardRelatedImage(content); image != "" {
			checked = image
			labels, err := imageLabelsWithAuth(ctx, basicAuth, image, false)
			switch {
			case errors.Is(err, errNoImageLabels):
				miss = componentLookup{reason: "no_commit_label", message: "The dashboard image has no commit label."}
			case err != nil:
				return componentLookup{}, err
			default:
				if m, ok := matches(labels); ok {
					m.image = image
					componentImageCache.Add(key, m, 0)
					return m, nil
				}
				if labelRepo, ok := githubRepoFromURL(labels.GitURL); !ok || !githubSHAPattern.MatchString(labels.GitCommit) {
					miss = componentLookup{reason: "no_commit_label", message: "The dashboard image has no GitHub commit label."}
				} else {
					miss = componentLookup{reason: "unexpected_repo", message: fmt.Sprintf("The dashboard image was built from %s, which this search does not cover.", labelRepo)}
				}
			}
		}
	}

	// Scan: every RHOAI image of the build, in catalog order.
	var refs []string
	for _, ri := range content.RelatedImages {
		if ri.Image != checked && isRHOAIImage(ri.Image) {
			refs = append(refs, ri.Image)
		}
	}
	labels := resolveImageLabels(ctx, basicAuth, refs, false)
	if err := ctx.Err(); err != nil {
		return componentLookup{}, err
	}
	complete := true
	for _, ref := range refs {
		l, ok := labels[ref]
		if !ok {
			// Left out: no labels (remembered) or a failed read (not remembered).
			if _, negative := cachedNoLabels(ref); !negative {
				complete = false
			}
			continue
		}
		if m, ok := matches(l); ok {
			m.image = ref
			componentImageCache.Add(key, m, 0)
			return m, nil
		}
	}
	if complete {
		componentImageCache.Add(key, miss, 0)
	}
	return miss, nil
}

// cachedNoLabels reports whether the label cache remembers image as having
// no labels (so a scan that skipped it is still complete).
func cachedNoLabels(image string) (labelCacheValue, bool) {
	repo, digest, ok := extractRepoAndDigest(image)
	if !ok {
		return labelCacheValue{}, false
	}
	v, ok := labelCache.Get(repo + "@" + digest)
	return v, ok && v.noLabels
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
