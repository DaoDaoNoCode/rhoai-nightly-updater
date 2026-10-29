package cluster

// GET /api/dashboard/builds: the commit of every PR or main build a dashboard
// container runs, how far it is from the branch it tracks, and whether the
// floating tag it came from points to a newer build.
//
// Both flavours are built from upstream opendatahub-io/odh-dashboard main
// (the RHOAI build is the same commit built by Konflux with
// BUILD_MODE=RHOAI), so a main build is compared with that repository's main
// branch and a PR build with the PR's head commit. The commit comes from the
// running image's labels, read anonymously (quay.io/opendatahub is public).
//
// This is not part of GET /api/dashboard/state: that endpoint is polled and
// must stay fast, while registry and GitHub reads take seconds and GitHub
// allows an anonymous client 60 requests per hour per egress IP, shared by
// everyone using the pod. Everything GitHub answers is cached (two SHAs
// compare the same way forever; a branch head for a few minutes) and a
// GitHub failure never fails the request: the container's divergence is
// "unknown" with a reason.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	// dashboardBuildsTimeout bounds one request.
	dashboardBuildsTimeout = 12 * time.Second
	// dashboardBranchHeadTTL is how long main's head SHA is reused: main
	// moves many times a day, and one call per repository per TTL is cheap.
	dashboardBranchHeadTTL = 5 * time.Minute
	// dashboardFloatingTagTTL is how long a floating tag's digest is reused,
	// so the page's polling does not hammer quay.io.
	dashboardFloatingTagTTL = time.Minute
)

// dashboardPRHeadTTL is how long an open or closed PR's head is reused;
// merged PRs are kept for good. (A var for tests.)
var dashboardPRHeadTTL = 5 * time.Minute

// dashboardBuildContainerTimeout bounds one container's registry and GitHub
// reads; a variable so tests can shorten it.
var dashboardBuildContainerTimeout = 10 * time.Second

// branchHead is a branch's head commit when it was read.
type branchHead struct {
	sha       string
	checkedAt time.Time
}

// divergenceResult is how a build commit relates to a base commit.
type divergenceResult struct {
	status        string // "identical", "behind", "ahead" or "diverged", of the build against the base
	behind, ahead int
	notFound      bool // GitHub does not know one of the commits
	checkedAt     time.Time
}

// dashboardTagResolution is what a floating tag points to.
type dashboardTagResolution struct {
	image string // quay.io/<repo>:<tag>@<digest>
	found bool
}

var (
	branchHeadCache    = newLRU[string, branchHead](64)
	branchHeadFlight   flightGroup[branchHead]
	divergenceCache    = newLRU[string, divergenceResult](2048)
	divergenceFlight   flightGroup[divergenceResult]
	dashboardPRCache   = newLRU[string, prInfo](256)
	dashboardPRFlight  flightGroup[prInfo]
	dashboardTagCache  = newLRU[string, dashboardTagResolution](256)
	dashboardTagFlight flightGroup[dashboardTagResolution]
)

// GetDashboardBuilds reads the dashboard state and describes the build of
// every container that runs a PR or main build. Errors are those of
// GetDashboardState (DashboardStateError maps them).
func GetDashboardBuilds(c *Client) (*types.DashboardBuildsResponse, error) {
	state, err := GetDashboardState(c)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(c.ctx, dashboardBuildsTimeout)
	defer cancel()
	return dashboardBuilds(ctx, state.DevImages), nil
}

// dashboardBuilds describes the PR and main builds among images, in their
// order. Containers are checked concurrently, each within
// dashboardBuildContainerTimeout, so one slow registry read does not hold
// up the others.
func dashboardBuilds(ctx context.Context, images []types.DashboardDevImage) *types.DashboardBuildsResponse {
	resp := &types.DashboardBuildsResponse{Builds: []types.DashboardContainerBuild{}}
	var selected []types.DashboardDevImage
	for _, image := range images {
		if image.Running == "pr" || image.Running == "main" {
			selected = append(selected, image)
		}
	}
	if len(selected) == 0 {
		return resp
	}
	resp.Builds = make([]types.DashboardContainerBuild, len(selected))
	var rateLimit sync.Mutex
	noteRateLimit := func(until time.Time) {
		rateLimit.Lock()
		defer rateLimit.Unlock()
		resp.RateLimited = true
		secs := int(time.Until(until).Round(time.Second) / time.Second)
		if secs < 1 {
			secs = 1
		}
		if secs > resp.RetryAfterSeconds {
			resp.RetryAfterSeconds = secs
		}
	}
	sem := make(chan struct{}, labelFetchConcurrency)
	var wg sync.WaitGroup
	for i, image := range selected {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				resp.Builds[i] = dashboardBuildOf(image)
				resp.Builds[i].Reason, resp.Builds[i].Message = "registry_error", "The request timed out before this container was checked."
				return
			}
			containerCtx, cancel := context.WithTimeout(ctx, dashboardBuildContainerTimeout)
			defer cancel()
			resp.Builds[i] = dashboardContainerBuild(containerCtx, image, noteRateLimit)
		}()
	}
	wg.Wait()
	return resp
}

func dashboardBuildOf(image types.DashboardDevImage) types.DashboardContainerBuild {
	return types.DashboardContainerBuild{Deployment: image.Deployment, Container: image.Container, Image: image.CurrentImage, Running: image.Running, PR: image.RunningPR, Flavor: image.Flavor}
}

// dashboardFloatingTag is the tag a build of this kind and flavour is deployed
// from: odh-stable or odh-pr-N for the RHOAI build, main or pr-N for the ODH build.
func dashboardFloatingTag(running string, pr int, flavor string) string {
	return dashboardBuildTags(running, pr, flavor)[0]
}

// dashboardContainerBuild describes one container's build: its commit from
// the image labels, its divergence from GitHub, and the build its floating
// tag points to now.
func dashboardContainerBuild(ctx context.Context, image types.DashboardDevImage, noteRateLimit func(time.Time)) types.DashboardContainerBuild {
	out := dashboardBuildOf(image)
	repo := image.Repository
	floatingTag := dashboardFloatingTag(image.Running, image.RunningPR, image.Flavor)
	registryError := func(what string, err error) types.DashboardContainerBuild {
		message := what + ": " + err.Error()
		if isContextError(err) {
			message = what + " timed out."
		}
		slog.Warn("dashboard builds: registry read failed", "image", truncateForLog(image.CurrentImage), "error", err)
		out.Reason, out.Message = "registry_error", message
		return out
	}

	_, digest, ok := extractRepoAndDigest(image.CurrentImage)
	if !ok {
		// A bare tag (set outside Deploy): the labels are those of what the
		// tag points to now, and there is no newer build to tell apart.
		resolved, err := resolveDashboardBuildCached(ctx, repo, extractTagFromRef(image.CurrentImage))
		if err != nil {
			return registryError("Resolving the image tag failed", err)
		}
		if !resolved.found {
			return registryError("Resolving the image tag failed", errors.New("the tag is not on quay.io"))
		}
		_, digest, _ = extractRepoAndDigest(resolved.image)
	}

	labelsCtx, cancel := context.WithTimeout(ctx, labelFetchTimeout)
	labels, err := imageLabelsWithAuth(labelsCtx, "", "quay.io/"+repo+"@"+digest, false)
	cancel()
	switch {
	case errors.Is(err, errNoImageLabels):
		out.Reason, out.Message = "no_commit_label", "The image has no labels, so its commit is unknown."
	case err != nil:
		return registryError("Reading the image labels failed", err)
	default:
		commit := strings.ToLower(labels.GitCommit)
		switch githubRepo, ok := githubRepoFromURL(labels.GitURL); {
		case !dashboardCommitSHAPattern.MatchString(commit):
			out.Reason, out.Message = "no_commit_label", "The image has no commit label."
			out.BuildDate = labels.BuildDate
		case !ok:
			out.Commit, out.BuildDate = commit, labels.BuildDate
			out.Reason, out.Message = "unknown_repo", "The image was not built from a GitHub repository, so it cannot be compared with main."
		default:
			out.Commit, out.BuildDate, out.Repo = commit, labels.BuildDate, githubRepo
			out.CommitURL = "https://github.com/" + githubRepo + "/commit/" + commit
			divergence := dashboardDivergence(ctx, githubRepo, commit, image.Running, image.RunningPR, noteRateLimit)
			out.Divergence = &divergence
			// One request per new SHA, cached for good; skipped while rate limited.
			out.CommitDate = fetchCommitDate(ctx, labels.GitURL, commit)
		}
	}

	resolved, err := resolveDashboardBuildCached(ctx, repo, floatingTag)
	if err != nil {
		slog.Warn("dashboard builds: cannot resolve the floating tag", "repo", repo, "tag", floatingTag, "error", err)
		return out
	}
	if !resolved.found {
		return out
	}
	if _, newDigest, ok := extractRepoAndDigest(resolved.image); ok && newDigest != digest {
		newer := &types.DashboardNewerBuild{Image: resolved.image, Tag: floatingTag}
		if image.Running == "main" {
			// As Deploy latest main would write it: under its commit tag.
			newer.Image = pinDashboardMainBuild(ctx, repo, floatingTag, resolved.image)
		}
		labelsCtx, cancel := context.WithTimeout(ctx, labelFetchTimeout)
		newLabels, err := imageLabelsWithAuth(labelsCtx, "", resolved.image, false)
		cancel()
		if err == nil {
			newer.BuildDate = newLabels.BuildDate
			if commit := strings.ToLower(newLabels.GitCommit); dashboardCommitSHAPattern.MatchString(commit) {
				newer.Commit = commit
				if githubRepo, ok := githubRepoFromURL(newLabels.GitURL); ok {
					newer.CommitURL = "https://github.com/" + githubRepo + "/commit/" + commit
				}
			}
		} else if !errors.Is(err, errNoImageLabels) {
			slog.Warn("dashboard builds: cannot read the newer build's labels", "image", truncateForLog(resolved.image), "error", err)
		}
		out.Newer = newer
	}
	return out
}

// resolveDashboardBuildCached is resolveDashboardBuild with the answer kept
// for dashboardFloatingTagTTL and concurrent lookups shared.
func resolveDashboardBuildCached(ctx context.Context, repo, tag string) (dashboardTagResolution, error) {
	key := repo + ":" + tag
	if v, ok := dashboardTagCache.Get(key); ok {
		return v, nil
	}
	v, err, _ := dashboardTagFlight.Do(ctx, key, func(ctx context.Context) (dashboardTagResolution, error) {
		if v, ok := dashboardTagCache.Get(key); ok {
			return v, nil
		}
		image, found, err := resolveDashboardBuild(ctx, repo, tag)
		if err != nil {
			return dashboardTagResolution{}, err
		}
		v := dashboardTagResolution{image: image, found: found}
		dashboardTagCache.Add(key, v, dashboardFloatingTagTTL)
		return v, nil
	})
	return v, err
}

// dashboardDivergence compares the build commit with the branch it tracks:
// main for a main build, the PR's head for a PR build.
func dashboardDivergence(ctx context.Context, repo, commit, running string, pr int, noteRateLimit func(time.Time)) types.DashboardBuildDivergence {
	d := types.DashboardBuildDivergence{Base: "main", Status: "unknown"}
	var baseHead string
	if running == "pr" {
		d.Base = fmt.Sprintf("pr-%d", pr)
		info, err := lookupPRHead(ctx, repo, pr)
		if dashboardGitHubFailed(err, noteRateLimit, &d) {
			return d
		}
		if info.notFound {
			d.Reason, d.Message = "pr_not_found", fmt.Sprintf("%s has no pull request #%d.", repo, pr)
			return d
		}
		d.PRState, d.PRTitle = info.state, info.title
		if info.merged {
			d.PRState = "merged"
		}
		if info.headSHA == "" {
			d.Reason, d.Message = "github_error", "GitHub did not report the pull request's head commit."
			return d
		}
		baseHead = info.headSHA
		d.CompareURL = fmt.Sprintf("https://github.com/%s/compare/%s...%s", repo, commit, baseHead)
	} else {
		head, err := lookupBranchHead(ctx, repo, "main")
		if dashboardGitHubFailed(err, noteRateLimit, &d) {
			return d
		}
		baseHead = head.sha
		d.CompareURL = fmt.Sprintf("https://github.com/%s/compare/%s...main", repo, commit)
	}
	d.BaseHead = baseHead
	result, err := compareDivergence(ctx, repo, commit, baseHead)
	if dashboardGitHubFailed(err, noteRateLimit, &d) {
		return d
	}
	d.CheckedAt = result.checkedAt.UTC().Format(time.RFC3339)
	if result.notFound {
		d.Reason, d.Message = "commit_not_found", fmt.Sprintf("GitHub does not know commit %s in %s.", shortSHA(commit), repo)
		return d
	}
	d.Status, d.Behind, d.Ahead = result.status, result.behind, result.ahead
	return d
}

// dashboardGitHubFailed records a GitHub failure in d and reports whether
// there was one. A rate limit is noted on the response; other errors are
// named, and not cached, so the next request retries.
func dashboardGitHubFailed(err error, noteRateLimit func(time.Time), d *types.DashboardBuildDivergence) bool {
	var limited *githubRateLimitError
	switch {
	case err == nil:
		return false
	case errors.As(err, &limited):
		noteRateLimit(limited.until)
		d.Reason = "rate_limited"
		d.Message = fmt.Sprintf("GitHub's API rate limit is reached; it resets at %s. Set GITHUB_TOKEN on the updater to raise it.", limited.until.UTC().Format(time.RFC3339))
	default:
		slog.Warn("dashboard builds: GitHub read failed", "base", d.Base, "error", err)
		d.Reason, d.Message = "github_error", err.Error()
	}
	return true
}

// lookupBranchHead reads a branch's head commit, reused for dashboardBranchHeadTTL.
func lookupBranchHead(ctx context.Context, repo, branch string) (branchHead, error) {
	key := repo + "|" + branch
	if v, ok := branchHeadCache.Get(key); ok {
		return v, nil
	}
	v, err, _ := branchHeadFlight.Do(ctx, key, func(ctx context.Context) (branchHead, error) {
		if v, ok := branchHeadCache.Get(key); ok {
			return v, nil
		}
		resp, err := githubGet(ctx, fmt.Sprintf("/repos/%s/branches/%s", repo, branch))
		if err != nil {
			return branchHead{}, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, githubBodyLimit))
		if err != nil {
			return branchHead{}, err
		}
		if resp.StatusCode != http.StatusOK {
			return branchHead{}, fmt.Errorf("GitHub returned %d for branch %s of %s", resp.StatusCode, branch, repo)
		}
		var b struct {
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if err := json.Unmarshal(body, &b); err != nil {
			return branchHead{}, fmt.Errorf("parse branch: %w", err)
		}
		sha := strings.ToLower(b.Commit.SHA)
		if !dashboardCommitSHAPattern.MatchString(sha) {
			return branchHead{}, fmt.Errorf("GitHub reported no usable head commit for branch %s of %s", branch, repo)
		}
		v := branchHead{sha: sha, checkedAt: time.Now()}
		branchHeadCache.Add(key, v, dashboardBranchHeadTTL)
		return v, nil
	})
	return v, err
}

// lookupPRHead reads a pull request for its head commit and state. Open and
// closed PRs are reused for dashboardPRHeadTTL (a closed PR can be reopened
// and pushed to); merged ones for good.
func lookupPRHead(ctx context.Context, repo string, number int) (prInfo, error) {
	key := fmt.Sprintf("%s#%d", repo, number)
	if info, ok := dashboardPRCache.Get(key); ok {
		return info, nil
	}
	info, err, _ := dashboardPRFlight.Do(ctx, key, func(ctx context.Context) (prInfo, error) {
		if info, ok := dashboardPRCache.Get(key); ok {
			return info, nil
		}
		info, err := fetchPullRequest(ctx, repo, number)
		if err != nil {
			return prInfo{}, err
		}
		ttl := dashboardPRHeadTTL
		if !info.notFound && info.merged {
			ttl = 0
		}
		dashboardPRCache.Add(key, info, ttl)
		return info, nil
	})
	return info, err
}

// compareDivergence asks GitHub how commit base relates to commit head and
// turns the answer around to describe the build (base) against the branch
// (head): GitHub's "ahead" (head has commits base lacks, ahead_by of them)
// is the build being behind. Two SHAs compare the same way forever; a
// commit GitHub does not know is remembered for compareNegativeTTL.
func compareDivergence(ctx context.Context, repo, base, head string) (divergenceResult, error) {
	key := repo + "|" + base + "|" + head
	if v, ok := divergenceCache.Get(key); ok {
		return v, nil
	}
	v, err, _ := divergenceFlight.Do(ctx, key, func(ctx context.Context) (divergenceResult, error) {
		if v, ok := divergenceCache.Get(key); ok {
			return v, nil
		}
		// Page 2 of one commit per page carries the status and the counts
		// without the commit list or the changed files (see compareContains).
		resp, err := githubGet(ctx, fmt.Sprintf("/repos/%s/compare/%s...%s?per_page=1&page=2", repo, base, head))
		if err != nil {
			return divergenceResult{}, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, githubBodyLimit))
		if err != nil {
			return divergenceResult{}, err
		}
		switch {
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnprocessableEntity:
			v := divergenceResult{notFound: true, checkedAt: time.Now()}
			divergenceCache.Add(key, v, compareNegativeTTL)
			return v, nil
		case resp.StatusCode != http.StatusOK:
			return divergenceResult{}, fmt.Errorf("GitHub compare returned %d", resp.StatusCode)
		}
		var cmp struct {
			Status   string `json:"status"`
			AheadBy  int    `json:"ahead_by"`
			BehindBy int    `json:"behind_by"`
		}
		if err := json.Unmarshal(body, &cmp); err != nil {
			return divergenceResult{}, fmt.Errorf("parse compare: %w", err)
		}
		v := divergenceResult{behind: cmp.AheadBy, ahead: cmp.BehindBy, checkedAt: time.Now()}
		switch cmp.Status {
		case "identical", "diverged":
			v.status = cmp.Status
		case "ahead":
			v.status = "behind"
		case "behind":
			v.status = "ahead"
		default:
			return divergenceResult{}, fmt.Errorf("unexpected compare status %q", cmp.Status)
		}
		divergenceCache.Add(key, v, 0)
		return v, nil
	})
	return v, err
}
