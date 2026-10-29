package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	upstreamRepo = "opendatahub-io/odh-dashboard"
	upstreamURL  = "https://github.com/" + upstreamRepo
	mainHead     = "aaaa000000000000000000000000000000000001"
	prHead       = "bbbb000000000000000000000000000000000002"
)

// splitTransport sends api.github.com requests to the fake GitHub and
// everything else (quay.io) to the dashboard tag mock.
type splitTransport struct {
	quay, github http.RoundTripper
}

func (t splitTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == "api.github.com" {
		return t.github.RoundTrip(r)
	}
	return t.quay.RoundTrip(r)
}

// installDashboardBuildsFakes publishes the given builds on a fake quay.io
// and routes GitHub to a fake whose main branch head is mainHead.
func installDashboardBuildsFakes(t *testing.T, published ...string) (*dashboardTagMock, *fakeGitHub) {
	t.Helper()
	m := mockDashboardTags(t, published...)
	g := newFakeGitHub()
	g.compareRepos[upstreamRepo] = true
	g.branches[upstreamRepo+":main"] = mainHead
	server := httptest.NewServer(g)
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	quayHTTPClient = &http.Client{Transport: splitTransport{quay: m, github: githubTransport{server: u}}, Timeout: 10 * time.Second}
	return m, g
}

// runningBuild is a container running the build published as "repo:tag".
func runningBuild(container, repo, tag, running string, pr int, flavor string) types.DashboardDevImage {
	return types.DashboardDevImage{
		Deployment: container, Container: container, Repository: "opendatahub/" + repo,
		CurrentImage: "quay.io/opendatahub/" + repo + ":" + tag + "@" + fixtureDigest(repo+":"+tag),
		Running:      running, RunningPR: pr, Flavor: flavor,
	}
}

func oneBuild(t *testing.T, resp *types.DashboardBuildsResponse) types.DashboardContainerBuild {
	t.Helper()
	if len(resp.Builds) != 1 {
		t.Fatalf("builds=%+v", resp.Builds)
	}
	return resp.Builds[0]
}

func TestDashboardBuildsMainBuild(t *testing.T) {
	t.Run("odh-build-up-to-date", func(t *testing.T) {
		// OpenShift CI labels: vcs-ref and io.openshift.build.source-location,
		// no git.url, and a revision label that is not the source commit.
		m, g := installDashboardBuildsFakes(t, "odh-dashboard:main")
		commit := fixtureCommit("odh-dashboard:main")
		m.setLabels(fixtureDigest("odh-dashboard:main"), map[string]string{
			"vcs-ref": commit, "io.openshift.build.commit.id": commit, "io.openshift.build.source-location": upstreamURL,
			"org.opencontainers.image.revision": "9999999999999999999999999999999999999999", "build-date": "2026-10-29T06:00:00Z",
		})
		g.compare[commit+"..."+mainHead] = "identical"
		g.compareCounts[commit+"..."+mainHead] = [2]int{0, 0}
		// The container runs the pinned commit tag; the floating tag has the same digest.
		image := runningBuild("rhods-dashboard", "odh-dashboard", "main", "main", 0, DashboardFlavorODH)
		image.CurrentImage = "quay.io/opendatahub/odh-dashboard:" + fixtureCommitTag("odh-dashboard:main") + "@" + fixtureDigest("odh-dashboard:main")

		resp := dashboardBuilds(context.Background(), []types.DashboardDevImage{image})
		b := oneBuild(t, resp)
		if b.Deployment != "rhods-dashboard" || b.Image != image.CurrentImage || b.Running != "main" || b.Flavor != DashboardFlavorODH || b.PR != 0 {
			t.Fatalf("build=%+v", b)
		}
		if b.Commit != commit || b.Repo != upstreamRepo || b.CommitURL != upstreamURL+"/commit/"+commit || b.BuildDate != "2026-10-29T06:00:00Z" || b.CommitDate != g.commitDate || b.Reason != "" {
			t.Fatalf("build=%+v", b)
		}
		d := b.Divergence
		if d == nil || d.Base != "main" || d.BaseHead != mainHead || d.Status != "identical" || d.Behind != 0 || d.Ahead != 0 || d.CheckedAt == "" ||
			d.CompareURL != upstreamURL+"/compare/"+commit+"...main" || d.Reason != "" {
			t.Fatalf("divergence=%+v", d)
		}
		if b.Newer != nil {
			t.Fatalf("newer=%+v for a build the floating tag still points to", b.Newer)
		}
		if resp.RateLimited {
			t.Fatal("rate limited")
		}
		if n := g.count("other"); n != 0 {
			t.Fatalf("%d unexpected GitHub calls", n)
		}
	})

	t.Run("rhoai-build-3-behind", func(t *testing.T) {
		// Konflux labels: git.commit and git.url. GitHub's compare of
		// <commit>...<main head> says main is "ahead" by 3: the build is 3 behind.
		m, g := installDashboardBuildsFakes(t, "odh-dashboard:odh-stable")
		commit := fixtureCommit("odh-dashboard:odh-stable")
		m.setLabels(fixtureDigest("odh-dashboard:odh-stable"), map[string]string{"git.commit": commit, "git.url": upstreamURL + ".git", "build-date": "2026-10-29T06:00:00Z"})
		g.compare[commit+"..."+mainHead] = "ahead"
		g.compareCounts[commit+"..."+mainHead] = [2]int{3, 0}

		resp := dashboardBuilds(context.Background(), []types.DashboardDevImage{runningBuild("rhods-dashboard", "odh-dashboard", "odh-stable", "main", 0, DashboardFlavorRHOAI)})
		b := oneBuild(t, resp)
		if b.Commit != commit || b.Repo != upstreamRepo || b.Flavor != DashboardFlavorRHOAI {
			t.Fatalf("build=%+v", b)
		}
		if d := b.Divergence; d == nil || d.Status != "behind" || d.Behind != 3 || d.Ahead != 0 || d.BaseHead != mainHead {
			t.Fatalf("divergence=%+v", d)
		}
	})

	t.Run("status-is-the-build-against-main", func(t *testing.T) {
		// GitHub's status describes the head (main) against the base (the
		// build); the response turns it around so "behind" means behind main.
		for githubStatus, want := range map[string]string{"behind": "ahead", "diverged": "diverged", "identical": "identical", "ahead": "behind"} {
			t.Run(githubStatus, func(t *testing.T) {
				_, g := installDashboardBuildsFakes(t, "odh-dashboard:odh-stable")
				commit := fixtureCommit("odh-dashboard:odh-stable")
				g.compare[commit+"..."+mainHead] = githubStatus
				g.compareCounts[commit+"..."+mainHead] = [2]int{2, 5}
				b := oneBuild(t, dashboardBuilds(context.Background(), []types.DashboardDevImage{runningBuild("rhods-dashboard", "odh-dashboard", "odh-stable", "main", 0, DashboardFlavorRHOAI)}))
				if d := b.Divergence; d == nil || d.Status != want || d.Behind != 2 || d.Ahead != 5 {
					t.Fatalf("divergence=%+v, want status %q", d, want)
				}
			})
		}
	})

	t.Run("containers-on-the-same-commit-share-one-compare", func(t *testing.T) {
		m, g := installDashboardBuildsFakes(t, "odh-dashboard:odh-stable", "odh-mod-arch-gen-ai:odh-stable")
		// Both modules carry the same source commit (one monorepo).
		commit := fixtureCommit("odh-dashboard:odh-stable")
		m.setLabels(fixtureDigest("odh-mod-arch-gen-ai:odh-stable"), map[string]string{"git.commit": commit, "git.url": upstreamURL})
		g.compare[commit+"..."+mainHead] = "identical"
		images := []types.DashboardDevImage{
			runningBuild("rhods-dashboard", "odh-dashboard", "odh-stable", "main", 0, DashboardFlavorRHOAI),
			runningBuild("gen-ai-ui", "odh-mod-arch-gen-ai", "odh-stable", "main", 0, DashboardFlavorRHOAI),
		}
		resp := dashboardBuilds(context.Background(), images)
		if len(resp.Builds) != 2 || resp.Builds[0].Container != "rhods-dashboard" || resp.Builds[1].Container != "gen-ai-ui" {
			t.Fatalf("builds=%+v", resp.Builds)
		}
		for _, b := range resp.Builds {
			if b.Commit != commit || b.Divergence == nil || b.Divergence.Status != "identical" || b.CommitDate != g.commitDate {
				t.Fatalf("build=%+v", b)
			}
		}
		if g.count("compare") != 1 || g.count("branch") != 1 || g.count("commit") != 1 {
			t.Fatalf("GitHub calls: compare=%d branch=%d commit=%d, want 1 each", g.count("compare"), g.count("branch"), g.count("commit"))
		}
		// The second request is answered from the caches: the branch head for
		// 5 minutes, the compare of two SHAs for good.
		dashboardBuilds(context.Background(), images)
		if g.count("compare") != 1 || g.count("branch") != 1 {
			t.Fatalf("repeat made GitHub calls: compare=%d branch=%d", g.count("compare"), g.count("branch"))
		}
	})
}

func TestDashboardBuildsPRBuild(t *testing.T) {
	openPR := func(head string) map[string]interface{} {
		return map[string]interface{}{"state": "open", "merged": false, "merged_at": nil, "title": "Show the commit", "html_url": upstreamURL + "/pull/5123", "head": map[string]interface{}{"sha": head}}
	}
	t.Run("built-from-the-pr-head", func(t *testing.T) {
		m, g := installDashboardBuildsFakes(t, "odh-dashboard:odh-pr-5123")
		m.setLabels(fixtureDigest("odh-dashboard:odh-pr-5123"), map[string]string{"git.commit": prHead, "git.url": upstreamURL, "build-date": "2026-10-29T07:00:00Z"})
		g.prs[5123] = openPR(prHead)
		g.compare[prHead+"..."+prHead] = "identical"
		g.compareCounts[prHead+"..."+prHead] = [2]int{0, 0}
		b := oneBuild(t, dashboardBuilds(context.Background(), []types.DashboardDevImage{runningBuild("rhods-dashboard", "odh-dashboard", "odh-pr-5123", "pr", 5123, DashboardFlavorRHOAI)}))
		if b.Running != "pr" || b.PR != 5123 || b.Commit != prHead {
			t.Fatalf("build=%+v", b)
		}
		d := b.Divergence
		if d == nil || d.Base != "pr-5123" || d.BaseHead != prHead || d.Status != "identical" || d.Behind != 0 || d.PRState != "open" || d.PRTitle != "Show the commit" ||
			d.CompareURL != upstreamURL+"/compare/"+prHead+"..."+prHead {
			t.Fatalf("divergence=%+v", d)
		}
		if g.count("branch") != 0 {
			t.Fatal("a PR build must not look up main")
		}
	})

	t.Run("pr-has-2-newer-commits", func(t *testing.T) {
		m, g := installDashboardBuildsFakes(t, "odh-dashboard:pr-5123")
		commit := fixtureCommit("odh-dashboard:pr-5123")
		m.setLabels(fixtureDigest("odh-dashboard:pr-5123"), map[string]string{"vcs-ref": commit, "io.openshift.build.source-location": upstreamURL})
		g.prs[5123] = openPR(prHead)
		g.compare[commit+"..."+prHead] = "ahead"
		g.compareCounts[commit+"..."+prHead] = [2]int{2, 0}
		b := oneBuild(t, dashboardBuilds(context.Background(), []types.DashboardDevImage{runningBuild("rhods-dashboard", "odh-dashboard", "pr-5123", "pr", 5123, DashboardFlavorODH)}))
		if d := b.Divergence; d == nil || d.Status != "behind" || d.Behind != 2 || d.BaseHead != prHead || d.PRState != "open" {
			t.Fatalf("divergence=%+v", d)
		}
	})

	t.Run("merged-pr", func(t *testing.T) {
		m, g := installDashboardBuildsFakes(t, "odh-dashboard:odh-pr-5123")
		m.setLabels(fixtureDigest("odh-dashboard:odh-pr-5123"), map[string]string{"git.commit": prHead, "git.url": upstreamURL})
		pr := openPR(prHead)
		pr["state"], pr["merged"], pr["merged_at"], pr["merge_commit_sha"] = "closed", true, "2026-10-29T08:00:00Z", mergeSHA
		g.prs[5123] = pr
		g.compare[prHead+"..."+prHead] = "identical"
		b := oneBuild(t, dashboardBuilds(context.Background(), []types.DashboardDevImage{runningBuild("rhods-dashboard", "odh-dashboard", "odh-pr-5123", "pr", 5123, DashboardFlavorRHOAI)}))
		if d := b.Divergence; d == nil || d.PRState != "merged" || d.Status != "identical" {
			t.Fatalf("divergence=%+v", d)
		}
	})

	t.Run("closed-pr-is-read-again-once-reopened", func(t *testing.T) {
		// A closed PR can be reopened and pushed to, so unlike a merged one it
		// is not remembered for good.
		m, g := installDashboardBuildsFakes(t, "odh-dashboard:odh-pr-5123")
		m.setLabels(fixtureDigest("odh-dashboard:odh-pr-5123"), map[string]string{"git.commit": prHead, "git.url": upstreamURL})
		closed := openPR(prHead)
		closed["state"] = "closed"
		g.prs[5123] = closed
		g.compare[prHead+"..."+prHead] = "identical"
		previous := dashboardPRHeadTTL
		dashboardPRHeadTTL = time.Nanosecond // 0 would mean "for good"
		t.Cleanup(func() { dashboardPRHeadTTL = previous })
		images := []types.DashboardDevImage{runningBuild("rhods-dashboard", "odh-dashboard", "odh-pr-5123", "pr", 5123, DashboardFlavorRHOAI)}
		if d := oneBuild(t, dashboardBuilds(context.Background(), images)).Divergence; d == nil || d.PRState != "closed" || d.Status != "identical" {
			t.Fatalf("divergence=%+v", d)
		}
		newHead := strings.Repeat("e", 40)
		time.Sleep(time.Millisecond)
		g.mu.Lock()
		g.prs[5123] = openPR(newHead)
		g.compare[prHead+"..."+newHead] = "ahead"
		g.compareCounts[prHead+"..."+newHead] = [2]int{1, 0}
		g.mu.Unlock()
		if d := oneBuild(t, dashboardBuilds(context.Background(), images)).Divergence; d == nil || d.PRState != "open" || d.Status != "behind" || d.BaseHead != newHead {
			t.Fatalf("after reopening, divergence=%+v", d)
		}
		if g.count("pull") != 2 {
			t.Fatalf("the closed PR must be read again, got %d reads", g.count("pull"))
		}
	})

	t.Run("pr-not-found", func(t *testing.T) {
		m, g := installDashboardBuildsFakes(t, "odh-dashboard:odh-pr-5123")
		m.setLabels(fixtureDigest("odh-dashboard:odh-pr-5123"), map[string]string{"git.commit": prHead, "git.url": upstreamURL})
		b := oneBuild(t, dashboardBuilds(context.Background(), []types.DashboardDevImage{runningBuild("rhods-dashboard", "odh-dashboard", "odh-pr-5123", "pr", 5123, DashboardFlavorRHOAI)}))
		if d := b.Divergence; d == nil || d.Status != "unknown" || d.Reason != "pr_not_found" || d.Base != "pr-5123" {
			t.Fatalf("divergence=%+v", d)
		}
		if g.count("compare") != 0 {
			t.Fatal("compared without a PR head")
		}
	})

	t.Run("tag-only-image-resolves-the-tag", func(t *testing.T) {
		// A container someone pointed at the bare tag: its labels come from
		// what the tag resolves to now, and there is no newer build to report.
		m, g := installDashboardBuildsFakes(t, "odh-dashboard:odh-pr-5123")
		m.setLabels(fixtureDigest("odh-dashboard:odh-pr-5123"), map[string]string{"git.commit": prHead, "git.url": upstreamURL})
		g.prs[5123] = openPR(prHead)
		g.compare[prHead+"..."+prHead] = "identical"
		image := runningBuild("rhods-dashboard", "odh-dashboard", "odh-pr-5123", "pr", 5123, DashboardFlavorRHOAI)
		image.CurrentImage = "quay.io/opendatahub/odh-dashboard:odh-pr-5123"
		b := oneBuild(t, dashboardBuilds(context.Background(), []types.DashboardDevImage{image}))
		if b.Commit != prHead || b.Newer != nil || b.Divergence == nil || b.Divergence.Status != "identical" {
			t.Fatalf("build=%+v", b)
		}
	})
}

func TestDashboardBuildsGitHubFailures(t *testing.T) {
	t.Run("rate-limited", func(t *testing.T) {
		_, g := installDashboardBuildsFakes(t, "odh-dashboard:odh-stable", "odh-mod-arch-gen-ai:odh-stable")
		g.rateLimited("branch", 30*time.Minute)
		resp := dashboardBuilds(context.Background(), []types.DashboardDevImage{
			runningBuild("rhods-dashboard", "odh-dashboard", "odh-stable", "main", 0, DashboardFlavorRHOAI),
			runningBuild("gen-ai-ui", "odh-mod-arch-gen-ai", "odh-stable", "main", 0, DashboardFlavorRHOAI),
		})
		if !resp.RateLimited || resp.RetryAfterSeconds <= 0 || resp.RetryAfterSeconds > 1800 {
			t.Fatalf("rateLimited=%v retryAfter=%d", resp.RateLimited, resp.RetryAfterSeconds)
		}
		for _, b := range resp.Builds {
			// The commit still comes from the registry; only GitHub is skipped.
			if b.Commit == "" || b.CommitDate != "" || b.Divergence == nil || b.Divergence.Status != "unknown" || b.Divergence.Reason != "rate_limited" || b.Divergence.Message == "" {
				t.Fatalf("build=%+v divergence=%+v", b, b.Divergence)
			}
		}
		// Once the backoff is on, no further GitHub call is made.
		if g.count("branch") != 1 || g.count("compare") != 0 || g.count("commit") != 0 {
			t.Fatalf("GitHub calls: branch=%d compare=%d commit=%d", g.count("branch"), g.count("compare"), g.count("commit"))
		}
	})

	t.Run("commit-not-found", func(t *testing.T) {
		_, g := installDashboardBuildsFakes(t, "odh-dashboard:odh-stable")
		g.failures["compare"] = func(w http.ResponseWriter) bool {
			http.Error(w, `{"message":"No commit found"}`, http.StatusUnprocessableEntity)
			return true
		}
		images := []types.DashboardDevImage{runningBuild("rhods-dashboard", "odh-dashboard", "odh-stable", "main", 0, DashboardFlavorRHOAI)}
		b := oneBuild(t, dashboardBuilds(context.Background(), images))
		if d := b.Divergence; d == nil || d.Status != "unknown" || d.Reason != "commit_not_found" || d.BaseHead != mainHead {
			t.Fatalf("divergence=%+v", d)
		}
		// Remembered for a while: the commit may be pushed later.
		dashboardBuilds(context.Background(), images)
		if g.count("compare") != 1 {
			t.Fatalf("compare called %d times", g.count("compare"))
		}
	})

	t.Run("github-error-is-not-cached", func(t *testing.T) {
		_, g := installDashboardBuildsFakes(t, "odh-dashboard:odh-stable")
		g.failures["compare"] = func(w http.ResponseWriter) bool { w.WriteHeader(http.StatusBadGateway); return true }
		images := []types.DashboardDevImage{runningBuild("rhods-dashboard", "odh-dashboard", "odh-stable", "main", 0, DashboardFlavorRHOAI)}
		b := oneBuild(t, dashboardBuilds(context.Background(), images))
		if d := b.Divergence; d == nil || d.Status != "unknown" || d.Reason != "github_error" || d.Message == "" {
			t.Fatalf("divergence=%+v", d)
		}
		dashboardBuilds(context.Background(), images)
		if g.count("compare") != 2 {
			t.Fatalf("compare called %d times, want a retry", g.count("compare"))
		}
	})
}

func TestDashboardBuildsLabelProblems(t *testing.T) {
	t.Run("no-labels", func(t *testing.T) {
		m, g := installDashboardBuildsFakes(t, "odh-dashboard:odh-stable")
		m.setLabels(fixtureDigest("odh-dashboard:odh-stable"), nil)
		b := oneBuild(t, dashboardBuilds(context.Background(), []types.DashboardDevImage{runningBuild("rhods-dashboard", "odh-dashboard", "odh-stable", "main", 0, DashboardFlavorRHOAI)}))
		if b.Reason != "no_commit_label" || b.Message == "" || b.Commit != "" || b.Divergence != nil {
			t.Fatalf("build=%+v", b)
		}
		if g.count("branch")+g.count("compare")+g.count("commit") != 0 {
			t.Fatal("GitHub called without a commit")
		}
	})
	t.Run("commit-label-is-not-a-sha", func(t *testing.T) {
		m, _ := installDashboardBuildsFakes(t, "odh-dashboard:odh-stable")
		m.setLabels(fixtureDigest("odh-dashboard:odh-stable"), map[string]string{"vcs-ref": "release-3.6", "git.url": upstreamURL})
		b := oneBuild(t, dashboardBuilds(context.Background(), []types.DashboardDevImage{runningBuild("rhods-dashboard", "odh-dashboard", "odh-stable", "main", 0, DashboardFlavorRHOAI)}))
		if b.Reason != "no_commit_label" || b.Commit != "" || b.Divergence != nil {
			t.Fatalf("build=%+v", b)
		}
	})
	t.Run("unknown-repo", func(t *testing.T) {
		m, g := installDashboardBuildsFakes(t, "odh-dashboard:odh-stable")
		commit := fixtureCommit("odh-dashboard:odh-stable")
		m.setLabels(fixtureDigest("odh-dashboard:odh-stable"), map[string]string{"git.commit": commit, "git.url": "https://gitlab.example.com/forks/odh-dashboard", "build-date": "2026-10-29T06:00:00Z"})
		b := oneBuild(t, dashboardBuilds(context.Background(), []types.DashboardDevImage{runningBuild("rhods-dashboard", "odh-dashboard", "odh-stable", "main", 0, DashboardFlavorRHOAI)}))
		if b.Reason != "unknown_repo" || b.Commit != commit || b.BuildDate == "" || b.Repo != "" || b.CommitURL != "" || b.Divergence != nil {
			t.Fatalf("build=%+v", b)
		}
		if g.count("branch")+g.count("compare")+g.count("commit") != 0 {
			t.Fatal("GitHub called for a repository that is not on GitHub")
		}
	})
	t.Run("registry-timeout", func(t *testing.T) {
		original := dashboardBuildContainerTimeout
		dashboardBuildContainerTimeout = 50 * time.Millisecond
		t.Cleanup(func() { dashboardBuildContainerTimeout = original })
		m, _ := installDashboardBuildsFakes(t, "odh-dashboard:odh-stable", "odh-mod-arch-gen-ai:odh-stable")
		m.delay("odh-mod-arch-gen-ai", time.Second)
		resp := dashboardBuilds(context.Background(), []types.DashboardDevImage{
			runningBuild("rhods-dashboard", "odh-dashboard", "odh-stable", "main", 0, DashboardFlavorRHOAI),
			runningBuild("gen-ai-ui", "odh-mod-arch-gen-ai", "odh-stable", "main", 0, DashboardFlavorRHOAI),
		})
		if len(resp.Builds) != 2 {
			t.Fatalf("builds=%+v", resp.Builds)
		}
		if host := resp.Builds[0]; host.Commit == "" || host.Reason != "" {
			t.Fatalf("the slow container held up the others: %+v", host)
		}
		if slow := resp.Builds[1]; slow.Reason != "registry_error" || slow.Message == "" || slow.Commit != "" {
			t.Fatalf("slow container=%+v", slow)
		}
	})
}

func TestDashboardBuildsNewerBuild(t *testing.T) {
	t.Run("floating-tag-moved", func(t *testing.T) {
		m, g := installDashboardBuildsFakes(t, "odh-dashboard:odh-stable")
		running := runningBuild("rhods-dashboard", "odh-dashboard", "odh-stable", "main", 0, DashboardFlavorRHOAI)
		commit := fixtureCommit("odh-dashboard:odh-stable")
		g.compare[commit+"..."+mainHead] = "ahead"
		g.compareCounts[commit+"..."+mainHead] = [2]int{1, 0}
		// A new push: odh-stable moves to a digest whose commit is main's head,
		// published under its commit tag too.
		newDigest := digestOf(42)
		m.republish("odh-dashboard:odh-stable", newDigest)
		m.republish("odh-dashboard:"+mainHead, newDigest)
		m.setLabels(newDigest, map[string]string{"git.commit": mainHead, "git.url": upstreamURL, "build-date": "2026-10-29T09:00:00Z"})

		b := oneBuild(t, dashboardBuilds(context.Background(), []types.DashboardDevImage{running}))
		if b.Commit != commit || b.Divergence == nil || b.Divergence.Behind != 1 {
			t.Fatalf("build=%+v", b)
		}
		n := b.Newer
		if n == nil || n.Tag != "odh-stable" || n.Image != "quay.io/opendatahub/odh-dashboard:"+mainHead+"@"+newDigest || n.Commit != mainHead ||
			n.CommitURL != upstreamURL+"/commit/"+mainHead || n.BuildDate != "2026-10-29T09:00:00Z" {
			t.Fatalf("newer=%+v", n)
		}
	})

	t.Run("odh-flavor-checks-main-and-pins-main-7hex", func(t *testing.T) {
		m, _ := installDashboardBuildsFakes(t, "odh-dashboard:main")
		running := runningBuild("rhods-dashboard", "odh-dashboard", "main", "main", 0, DashboardFlavorODH)
		newDigest := digestOf(43)
		m.republish("odh-dashboard:main", newDigest)
		m.republish("odh-dashboard:main-"+mainHead[:7], newDigest)
		m.setLabels(newDigest, map[string]string{"vcs-ref": mainHead, "io.openshift.build.source-location": upstreamURL})
		b := oneBuild(t, dashboardBuilds(context.Background(), []types.DashboardDevImage{running}))
		if n := b.Newer; n == nil || n.Tag != "main" || n.Image != "quay.io/opendatahub/odh-dashboard:main-"+mainHead[:7]+"@"+newDigest || n.Commit != mainHead {
			t.Fatalf("newer=%+v", n)
		}
		if m.headCount("odh-dashboard:odh-stable") != 0 {
			t.Fatal("an ODH build must not be compared with the Konflux tag")
		}
	})

	t.Run("pr-tag-moved", func(t *testing.T) {
		m, g := installDashboardBuildsFakes(t, "odh-dashboard:odh-pr-7")
		m.setLabels(fixtureDigest("odh-dashboard:odh-pr-7"), map[string]string{"git.commit": fixtureCommit("odh-dashboard:odh-pr-7"), "git.url": upstreamURL})
		g.prs[7] = map[string]interface{}{"state": "open", "merged": false, "title": "x", "head": map[string]interface{}{"sha": prHead}}
		newDigest := digestOf(44)
		m.republish("odh-dashboard:odh-pr-7", newDigest)
		m.setLabels(newDigest, map[string]string{"git.commit": prHead, "git.url": upstreamURL})
		b := oneBuild(t, dashboardBuilds(context.Background(), []types.DashboardDevImage{runningBuild("rhods-dashboard", "odh-dashboard", "odh-pr-7", "pr", 7, DashboardFlavorRHOAI)}))
		// PR builds are not pinned to a commit tag: the floating PR tag is kept.
		if n := b.Newer; n == nil || n.Tag != "odh-pr-7" || n.Image != "quay.io/opendatahub/odh-dashboard:odh-pr-7@"+newDigest || n.Commit != prHead {
			t.Fatalf("newer=%+v", n)
		}
	})

	t.Run("same-digest-and-cached-for-a-minute", func(t *testing.T) {
		m, _ := installDashboardBuildsFakes(t, "odh-dashboard:odh-stable")
		images := []types.DashboardDevImage{runningBuild("rhods-dashboard", "odh-dashboard", "odh-stable", "main", 0, DashboardFlavorRHOAI)}
		if b := oneBuild(t, dashboardBuilds(context.Background(), images)); b.Newer != nil {
			t.Fatalf("newer=%+v", b.Newer)
		}
		dashboardBuilds(context.Background(), images)
		if n := m.headCount("odh-dashboard:odh-stable"); n != 1 {
			t.Fatalf("the floating tag was resolved %d times in a minute, want 1", n)
		}
	})

	t.Run("tag-gone", func(t *testing.T) {
		m, _ := installDashboardBuildsFakes(t, "odh-dashboard:odh-pr-7")
		m.setLabels(fixtureDigest("odh-dashboard:odh-pr-7"), map[string]string{"git.commit": prHead, "git.url": upstreamURL})
		m.unpublish("odh-dashboard:odh-pr-7")
		b := oneBuild(t, dashboardBuilds(context.Background(), []types.DashboardDevImage{runningBuild("rhods-dashboard", "odh-dashboard", "odh-pr-7", "pr", 7, DashboardFlavorRHOAI)}))
		if b.Newer != nil || b.Commit != prHead {
			t.Fatalf("build=%+v", b)
		}
	})
}

// GetDashboardBuilds reads the same state as GET /api/dashboard/state and
// reports only the containers that run a PR or main build.
func TestGetDashboardBuildsCoversPRAndMainContainersOnly(t *testing.T) {
	f, c := newDashboardDevFixture(t)
	m, g := installDashboardBuildsFakes(t, "odh-dashboard:odh-stable", "odh-mod-arch-notebooks:odh-pr-9")
	m.setLabels(fixtureDigest("odh-mod-arch-notebooks:odh-pr-9"), map[string]string{"git.commit": prHead, "git.url": upstreamURL})
	g.prs[9] = map[string]interface{}{"state": "open", "merged": false, "title": "x", "head": map[string]interface{}{"sha": prHead}}
	g.compare[prHead+"..."+prHead] = "identical"
	commit := fixtureCommit("odh-dashboard:odh-stable")
	g.compare[commit+"..."+mainHead] = "identical"
	host := f.workloads[dashboardDeploymentName]
	host.Spec.Template.Spec.Containers[0].Image = runningBuild("", "odh-dashboard", "odh-stable", "", 0, "").CurrentImage
	f.workloads[dashboardDeploymentName] = host
	notebooks := f.workloads["notebooks-ui"]
	notebooks.Spec.Template.Spec.Containers[0].Image = runningBuild("", "odh-mod-arch-notebooks", "odh-pr-9", "", 0, "").CurrentImage
	f.workloads["notebooks-ui"] = notebooks
	// future-workload keeps its release image.

	resp, err := GetDashboardBuilds(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Builds) != 2 {
		t.Fatalf("builds=%+v", resp.Builds)
	}
	byContainer := map[string]types.DashboardContainerBuild{}
	for _, b := range resp.Builds {
		byContainer[b.Container] = b
	}
	if b := byContainer["notebooks-ui"]; b.Running != "pr" || b.PR != 9 || b.Flavor != DashboardFlavorRHOAI || b.Commit != prHead || b.Divergence == nil || b.Divergence.Base != "pr-9" {
		t.Fatalf("notebooks=%+v", b)
	}
	if b := byContainer[dashboardContainerName]; b.Running != "main" || b.Commit != commit || b.Divergence == nil || b.Divergence.Base != "main" {
		t.Fatalf("host=%+v", b)
	}
	if _, ok := byContainer["custom-name"]; ok {
		t.Fatal("a release container was reported")
	}
}

func TestGetDashboardBuildsWithoutBuildsIsAnEmptyList(t *testing.T) {
	_, c := newDashboardDevFixture(t)
	installDashboardBuildsFakes(t)
	resp, err := GetDashboardBuilds(c)
	if err != nil || resp.Builds == nil || len(resp.Builds) != 0 || resp.RateLimited {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	body, err := json.Marshal(resp)
	if err != nil || !strings.HasPrefix(string(body), `{"builds":[]`) {
		t.Fatalf("json=%s err=%v", body, err)
	}
}
