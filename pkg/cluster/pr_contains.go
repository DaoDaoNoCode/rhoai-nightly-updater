package cluster

// "Which nightly contains PR #N?"
//
// Pull requests are opened and merged in opendatahub-io/odh-dashboard, while
// RHOAI images are built from red-hat-data-services/odh-dashboard (the git.url
// label of the dashboard image). The fork's release branches are updated by
// merging upstream main ("Merge remote-tracking branch 'upstream/main' into
// rhoai-3.6"), so the upstream merge commit keeps its SHA in the fork. Checked
// on 2026-10-05: compare/<merge of #9986>...<build commit 32a2131> in the fork
// is "ahead" (contained), and compare/<merge of #10063>...32a2131 is
// "diverged" (merged after that build), while the later build a3b6b58 is
// "ahead" of it. A PR whose change reached a build only as a cherry-pick (a
// different SHA) is reported as not contained.
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
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// DefaultPRSearchRepo is the repository searched when none is given.
const DefaultPRSearchRepo = "opendatahub-io/odh-dashboard"

// prSearchRepo describes a repository whose PRs can be searched: the
// repositories its build commits may come from, and how to find its image in
// a catalog.
type prSearchRepo struct {
	buildRepos map[string]bool
	image      func(*types.FBCContentResponse) string
}

// prSearchRepos is the allowlist: only repositories the nightly builds carry.
var prSearchRepos = map[string]prSearchRepo{
	DefaultPRSearchRepo: {
		buildRepos: map[string]bool{
			"red-hat-data-services/odh-dashboard": true,
			"opendatahub-io/odh-dashboard":        true,
		},
		image: dashboardRelatedImage,
	},
}

// IsPRSearchRepo reports whether repo's pull requests can be searched.
func IsPRSearchRepo(repo string) bool {
	_, ok := prSearchRepos[repo]
	return ok
}

// PRSearchRepos lists the repositories whose PRs can be searched.
func PRSearchRepos() []string {
	out := make([]string, 0, len(prSearchRepos))
	for repo := range prSearchRepos {
		out = append(out, repo)
	}
	return out
}

const (
	// prMaxBuilds bounds the builds one request checks.
	prMaxBuilds = 40
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
	notFound    bool
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
			info := prInfo{notFound: true}
			prCache.Add(key, info, prPendingTTL)
			return info, nil
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
		}
		if err := json.Unmarshal(body, &pr); err != nil {
			return prInfo{}, fmt.Errorf("parse pull request: %w", err)
		}
		// An open PR also has a merge_commit_sha (GitHub's test merge), so
		// only "merged" counts.
		info := prInfo{merged: pr.Merged && pr.MergedAt != "", state: pr.State, title: pr.Title, url: pr.HTMLURL, mergedAt: pr.MergedAt}
		if info.merged {
			if !githubSHAPattern.MatchString(pr.MergeCommitSHA) {
				return prInfo{}, fmt.Errorf("pull request %s has no usable merge commit", key)
			}
			info.mergeCommit = pr.MergeCommitSHA
			prCache.Add(key, info, 0)
		} else {
			prCache.Add(key, info, prPendingTTL)
		}
		return info, nil
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

// FindBuildsContainingPR reports which nightly builds contain merged PR
// number of repo. images are digest-pinned FBC references; when empty, the
// installed nightly build and the newest build of every tag are checked.
func FindBuildsContainingPR(ctx context.Context, c *Client, repo string, number int, images []string) (*types.PRContainsResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, prRequestTimeout)
	defer cancel()
	basicAuth := getQuayAuth(c)
	list := func(ctx context.Context) ([]prBuild, error) {
		if len(images) > 0 {
			builds := make([]prBuild, 0, len(images))
			for _, image := range images {
				builds = append(builds, prBuild{image: image})
			}
			return builds, nil
		}
		return defaultPRBuilds(ctx, c, basicAuth)
	}
	return findBuildsContainingPR(ctx, basicAuth, repo, number, list)
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

func findBuildsContainingPR(ctx context.Context, basicAuth, repo string, number int, list func(context.Context) ([]prBuild, error)) (*types.PRContainsResponse, error) {
	spec, ok := prSearchRepos[repo]
	if !ok {
		return nil, &PRSearchError{Status: http.StatusBadRequest, Code: "validation", Msg: fmt.Sprintf("PR search is not available for %s", repo)}
	}
	resp := &types.PRContainsResponse{Repo: repo, PR: number, URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, number), Builds: []types.PRContainsBuild{}}
	var rateLimit sync.Mutex
	noteRateLimit := func(until time.Time) {
		rateLimit.Lock()
		defer rateLimit.Unlock()
		resp.RateLimited = true
		if secs := int(time.Until(until).Round(time.Second) / time.Second); secs > resp.RetryAfterSeconds {
			resp.RetryAfterSeconds = secs
		}
	}

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
	}

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
			resp.Builds[i] = checkBuildForPR(ctx, basicAuth, spec, resp.MergeCommit, b, noteRateLimit)
		}()
	}
	wg.Wait()
	return resp, nil
}

func checkBuildForPR(ctx context.Context, basicAuth string, spec prSearchRepo, mergeCommit string, b prBuild, noteRateLimit func(time.Time)) types.PRContainsBuild {
	out := types.PRContainsBuild{Image: b.image, Tag: extractTagFromRef(b.image), Installed: b.installed, Result: "unknown"}
	if err := ctx.Err(); err != nil {
		out.Reason, out.Message = "build_unreadable", "The search timed out before this build was checked."
		return out
	}
	content, err := extractFBCContentWithAuth(ctx, basicAuth, b.image)
	if err != nil {
		out.Reason, out.Message = "build_unreadable", fmt.Sprintf("Could not read the build's catalog: %v", err)
		return out
	}
	componentImage := spec.image(content)
	if componentImage == "" {
		out.Reason, out.Message = "no_component_image", "This build has no image of this repository."
		return out
	}
	labels, err := imageLabelsWithAuth(ctx, basicAuth, componentImage, false)
	switch {
	case errors.Is(err, errNoImageLabels):
		out.Reason, out.Message = "no_commit_label", "The component image has no commit label."
		return out
	case err != nil:
		out.Reason, out.Message = "build_unreadable", fmt.Sprintf("Could not read the component image labels: %v", err)
		return out
	}
	commitRepo, ok := githubRepoFromURL(labels.GitURL)
	if !ok || !githubSHAPattern.MatchString(labels.GitCommit) {
		out.Reason, out.Message = "no_commit_label", "The component image has no GitHub commit label."
		return out
	}
	out.Commit, out.CommitRepo = labels.GitCommit, commitRepo
	if !spec.buildRepos[commitRepo] {
		out.Reason, out.Message = "unexpected_repo", fmt.Sprintf("The component was built from %s, which this search does not cover.", commitRepo)
		return out
	}
	if mergeCommit == "" {
		// The PR could not be read (rate limit): the build commit is still shown.
		out.Reason = "rate_limited"
		return out
	}
	out.CompareURL = fmt.Sprintf("https://github.com/%s/compare/%s...%s", commitRepo, mergeCommit, labels.GitCommit)
	verdict, err := compareContains(ctx, commitRepo, mergeCommit, labels.GitCommit)
	var limited *githubRateLimitError
	switch {
	case errors.As(err, &limited):
		noteRateLimit(limited.until)
		out.Reason = "rate_limited"
	case err != nil:
		out.Reason, out.Message = "github_error", err.Error()
	case verdict.notFound:
		out.Reason, out.Message = "commit_not_found", fmt.Sprintf("GitHub does not know commit %s in %s.", shortSHA(labels.GitCommit), commitRepo)
	case verdict.contains:
		out.Result = "contains"
	default:
		out.Result = "not_contained"
	}
	return out
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
