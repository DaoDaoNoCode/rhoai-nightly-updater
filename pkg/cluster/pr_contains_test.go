package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	forkURL   = "https://github.com/red-hat-data-services/odh-dashboard"
	forkRepo  = "red-hat-data-services/odh-dashboard"
	mergeSHA  = "fa1bcdbf5d61a7efd4b83f3c36e9d61f591326b3"
	commitOld = "32a213123fbc434a602ba5b05e4a2022dd0d85d6"
	commitNew = "a3b6b581b465f97f8701f220efadcf8787879dde"
)

// fakeGitHub is an in-memory GitHub REST API: pull requests and compare
// answers, with injectable failures. It counts requests per path kind.
type fakeGitHub struct {
	mu       sync.Mutex
	prs      map[int]map[string]interface{}
	compare  map[string]string // "base...head" -> status; missing = 404
	failures map[string]func(w http.ResponseWriter) bool
	calls    map[string]int
	queries  []url.Values
	auth     []string
	inFlight atomic.Int32
	maxIn    atomic.Int32
	delay    time.Duration
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		prs:      map[int]map[string]interface{}{},
		compare:  map[string]string{},
		failures: map[string]func(w http.ResponseWriter) bool{},
		calls:    map[string]int{},
	}
}

func (g *fakeGitHub) count(kind string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls[kind]
}

func (g *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := g.inFlight.Add(1)
	defer g.inFlight.Add(-1)
	for {
		m := g.maxIn.Load()
		if n <= m || g.maxIn.CompareAndSwap(m, n) {
			break
		}
	}
	if g.delay > 0 {
		time.Sleep(g.delay)
	}
	kind := "other"
	switch {
	case strings.Contains(r.URL.Path, "/pulls/"):
		kind = "pull"
	case strings.Contains(r.URL.Path, "/compare/"):
		kind = "compare"
	}
	g.mu.Lock()
	g.calls[kind]++
	g.queries = append(g.queries, r.URL.Query())
	g.auth = append(g.auth, r.Header.Get("Authorization"))
	fail := g.failures[kind]
	g.mu.Unlock()
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if fail != nil && fail(w) {
		return
	}
	switch kind {
	case "pull":
		num, _ := strconv.Atoi(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		g.mu.Lock()
		pr, ok := g.prs[num]
		g.mu.Unlock()
		if !ok || !strings.HasPrefix(r.URL.Path, "/repos/opendatahub-io/odh-dashboard/pulls/") {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(pr)
	case "compare":
		if !strings.HasPrefix(r.URL.Path, "/repos/"+forkRepo+"/compare/") {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		spec := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		g.mu.Lock()
		status, ok := g.compare[spec]
		g.mu.Unlock()
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": status, "ahead_by": 1, "behind_by": 0, "commits": []string{}})
	default:
		http.NotFound(w, r)
	}
}

// githubTransport sends api.github.com requests to the fake server.
type githubTransport struct{ server *url.URL }

func (t githubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "api.github.com" {
		return nil, fmt.Errorf("unexpected host %s", r.URL.Host)
	}
	out := r.Clone(r.Context())
	out.URL.Scheme, out.URL.Host = t.server.Scheme, t.server.Host
	return http.DefaultTransport.RoundTrip(out)
}

func installFakeGitHub(t *testing.T, g *fakeGitHub) {
	t.Helper()
	server := httptest.NewServer(g)
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	resetRegistryCaches()
	original := quayHTTPClient
	quayHTTPClient = &http.Client{Transport: githubTransport{server: u}, Timeout: 10 * time.Second}
	t.Cleanup(func() {
		quayHTTPClient = original
		resetRegistryCaches()
	})
}

func mergedPR(sha string) map[string]interface{} {
	return map[string]interface{}{"state": "closed", "merged": true, "merged_at": "2026-10-05T21:16:13Z", "merge_commit_sha": sha,
		"title": "Fix the thing", "html_url": "https://github.com/opendatahub-io/odh-dashboard/pull/9938"}
}

// seedBuild puts a build's catalog and dashboard labels in the caches, so
// no registry call is needed. n makes the digests unique.
func seedBuild(t *testing.T, n int, tag, gitURL, commit string) string {
	t.Helper()
	fbc := fmt.Sprintf("quay.io/rhoai/rhoai-fbc-fragment:%s@%s", tag, digestOf(n))
	dash := fmt.Sprintf("quay.io/rhoai/odh-dashboard-rhel9@%s", digestOf(1000+n))
	key, ok := fbcCacheKey(fbc)
	if !ok {
		t.Fatal("bad fbc ref")
	}
	fbcContentCache.Add(key, &types.FBCContentResponse{Tag: tag, RelatedImages: []types.RelatedImage{
		{Name: "odh_kserve_image", Image: "quay.io/rhoai/odh-kserve@" + digestOf(2000+n)},
		{Name: "odh_dashboard_image", Image: dash},
	}}, 0)
	repo, digest, _ := extractRepoAndDigest(dash)
	labelCache.Add(repo+"@"+digest, labelCacheValue{labels: ImageLabels{GitCommit: commit, GitURL: gitURL}}, 0)
	return fbc
}

func staticBuilds(builds ...prBuild) func(context.Context) ([]prBuild, error) {
	return func(context.Context) ([]prBuild, error) { return builds, nil }
}

func resultsByTag(resp *types.PRContainsResponse) map[string]types.PRContainsBuild {
	out := map[string]types.PRContainsBuild{}
	for _, b := range resp.Builds {
		out[b.Tag] = b
	}
	return out
}

func TestFindBuildsContainingPR_Verdicts(t *testing.T) {
	g := newFakeGitHub()
	installFakeGitHub(t, g)
	g.prs[9938] = mergedPR(mergeSHA)
	g.compare[mergeSHA+"..."+commitNew] = "ahead"
	g.compare[mergeSHA+"..."+commitOld] = "diverged"
	g.compare[mergeSHA+"..."+mergeSHA] = "identical"
	behind := "1111111111111111111111111111111111111111"
	g.compare[mergeSHA+"..."+behind] = "behind"

	installed := seedBuild(t, 1, "rhoai-3.6", forkURL+".git", commitNew)
	builds := staticBuilds(
		prBuild{image: installed, installed: true},
		prBuild{image: seedBuild(t, 2, "rhoai-3.5", forkURL, commitOld)},
		prBuild{image: seedBuild(t, 3, "rhoai-3.7", forkURL, mergeSHA)},
		prBuild{image: seedBuild(t, 4, "rhoai-3.4", forkURL, behind)},
	)
	resp, err := findBuildsContainingPR(context.Background(), "", DefaultPRSearchRepo, 9938, builds)
	if err != nil {
		t.Fatal(err)
	}
	if resp.MergeCommit != mergeSHA || resp.Title != "Fix the thing" || resp.RateLimited {
		t.Fatalf("response %+v", resp)
	}
	got := resultsByTag(resp)
	want := map[string]string{"rhoai-3.6": "contains", "rhoai-3.5": "not_contained", "rhoai-3.7": "contains", "rhoai-3.4": "not_contained"}
	for tag, result := range want {
		if got[tag].Result != result {
			t.Errorf("%s: result %q, want %q (%+v)", tag, got[tag].Result, result, got[tag])
		}
	}
	b := got["rhoai-3.6"]
	if !b.Installed || b.Commit != commitNew || b.CommitRepo != forkRepo ||
		b.CompareURL != "https://github.com/"+forkRepo+"/compare/"+mergeSHA+"..."+commitNew {
		t.Errorf("installed build %+v", b)
	}
	if resp.Builds[0].Tag != "rhoai-3.6" {
		t.Errorf("order not kept: %+v", resp.Builds)
	}
	for _, q := range g.queries {
		if q.Get("per_page") != "" && (q.Get("per_page") != "1" || q.Get("page") != "2") {
			t.Errorf("compare must ask for page 2 of 1 commit (no file list), got %v", q)
		}
	}

	// Merge commit and compare answers are immutable: a second search makes no GitHub call.
	before := g.count("pull") + g.count("compare")
	if _, err := findBuildsContainingPR(context.Background(), "", DefaultPRSearchRepo, 9938, builds); err != nil {
		t.Fatal(err)
	}
	if after := g.count("pull") + g.count("compare"); after != before {
		t.Errorf("repeat search made %d GitHub calls", after-before)
	}
}

func TestFindBuildsContainingPR_RequestErrors(t *testing.T) {
	cases := []struct {
		name   string
		pr     map[string]interface{}
		fail   func(w http.ResponseWriter) bool
		status int
		code   string
	}{
		{name: "open PR (test-merge SHA is ignored)", pr: map[string]interface{}{"state": "open", "merged": false, "merged_at": nil, "merge_commit_sha": mergeSHA}, status: 422, code: "pr_not_merged"},
		{name: "closed without merge", pr: map[string]interface{}{"state": "closed", "merged": false, "merged_at": nil, "merge_commit_sha": nil}, status: 422, code: "pr_not_merged"},
		{name: "no such PR", status: 404, code: "pr_not_found"},
		{name: "GitHub 502", fail: func(w http.ResponseWriter) bool { w.WriteHeader(502); return true }, status: 502, code: "upstream_error"},
		{name: "merged without merge SHA", pr: map[string]interface{}{"state": "closed", "merged": true, "merged_at": "2026-10-01T00:00:00Z", "merge_commit_sha": ""}, status: 502, code: "upstream_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newFakeGitHub()
			installFakeGitHub(t, g)
			if tc.pr != nil {
				g.prs[42] = tc.pr
			}
			if tc.fail != nil {
				g.failures["pull"] = tc.fail
			}
			listed := false
			_, err := findBuildsContainingPR(context.Background(), "", DefaultPRSearchRepo, 42, func(context.Context) ([]prBuild, error) {
				listed = true
				return nil, nil
			})
			var searchErr *PRSearchError
			if !errors.As(err, &searchErr) || searchErr.Status != tc.status || searchErr.Code != tc.code {
				t.Fatalf("err %v, want %d %s", err, tc.status, tc.code)
			}
			if listed {
				t.Error("builds were read for a PR that cannot be in any build")
			}
		})
	}

	t.Run("repository not on the allowlist", func(t *testing.T) {
		g := newFakeGitHub()
		installFakeGitHub(t, g)
		_, err := findBuildsContainingPR(context.Background(), "", "evil/repo", 1, staticBuilds())
		var searchErr *PRSearchError
		if !errors.As(err, &searchErr) || searchErr.Code != "validation" || g.count("pull") != 0 {
			t.Fatalf("err %v, calls %d", err, g.count("pull"))
		}
	})
}

func TestFindBuildsContainingPR_RateLimits(t *testing.T) {
	t.Run("PR lookup rate limited: partial result, never an error", func(t *testing.T) {
		g := newFakeGitHub()
		installFakeGitHub(t, g)
		reset := time.Now().Add(10 * time.Minute).Unix()
		g.failures["pull"] = func(w http.ResponseWriter) bool {
			w.Header().Set("X-Ratelimit-Remaining", "0")
			w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(reset, 10))
			w.WriteHeader(http.StatusForbidden)
			return true
		}
		image := seedBuild(t, 1, "rhoai-3.6", forkURL, commitNew)
		resp, err := findBuildsContainingPR(context.Background(), "", DefaultPRSearchRepo, 9938, staticBuilds(prBuild{image: image}))
		if err != nil {
			t.Fatal(err)
		}
		if !resp.RateLimited || resp.RetryAfterSeconds < 500 || resp.RetryAfterSeconds > 600 {
			t.Fatalf("rateLimited %v retryAfter %d", resp.RateLimited, resp.RetryAfterSeconds)
		}
		b := resp.Builds[0]
		if b.Result != "unknown" || b.Reason != "rate_limited" || b.Commit != commitNew {
			t.Errorf("build %+v", b)
		}
		if g.count("compare") != 0 {
			t.Error("compared without a merge commit")
		}
		// The backoff holds: the next request does not call GitHub at all.
		before := g.count("pull")
		resp, err = findBuildsContainingPR(context.Background(), "", DefaultPRSearchRepo, 9938, staticBuilds(prBuild{image: image}))
		if err != nil || !resp.RateLimited || g.count("pull") != before {
			t.Errorf("during the backoff: err %v, rateLimited %v, calls %d", err, resp.RateLimited, g.count("pull")-before)
		}
	})

	t.Run("compare rate limited (429 Retry-After): the rest are still answered", func(t *testing.T) {
		g := newFakeGitHub()
		installFakeGitHub(t, g)
		g.prs[9938] = mergedPR(mergeSHA)
		g.compare[mergeSHA+"..."+commitNew] = "ahead"
		// Answered first and cached, so the second build is checked from the cache.
		if _, err := compareContains(context.Background(), forkRepo, mergeSHA, commitNew); err != nil {
			t.Fatal(err)
		}
		g.failures["compare"] = func(w http.ResponseWriter) bool {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			return true
		}
		resp, err := findBuildsContainingPR(context.Background(), "", DefaultPRSearchRepo, 9938, staticBuilds(
			prBuild{image: seedBuild(t, 1, "rhoai-3.6", forkURL, commitNew)},
			prBuild{image: seedBuild(t, 2, "rhoai-3.5", forkURL, commitOld)},
		))
		if err != nil {
			t.Fatal(err)
		}
		got := resultsByTag(resp)
		if got["rhoai-3.6"].Result != "contains" || got["rhoai-3.5"].Reason != "rate_limited" || got["rhoai-3.5"].Result != "unknown" {
			t.Errorf("results %+v", got)
		}
		if !resp.RateLimited || resp.RetryAfterSeconds < 110 || resp.RetryAfterSeconds > 120 {
			t.Errorf("rateLimited %v retryAfter %d", resp.RateLimited, resp.RetryAfterSeconds)
		}
	})
}

func TestFindBuildsContainingPR_UnknownBuilds(t *testing.T) {
	g := newFakeGitHub()
	installFakeGitHub(t, g)
	g.prs[9938] = mergedPR(mergeSHA)
	g.compare[mergeSHA+"..."+commitNew] = "ahead"
	errSHA := "2222222222222222222222222222222222222222"

	noDash := fmt.Sprintf("quay.io/rhoai/rhoai-fbc-fragment:rhoai-2.25@%s", digestOf(10))
	key, _ := fbcCacheKey(noDash)
	fbcContentCache.Add(key, &types.FBCContentResponse{RelatedImages: []types.RelatedImage{{Name: "odh_kserve_image", Image: "quay.io/rhoai/odh-kserve@" + digestOf(11)}}}, 0)

	noLabels := fmt.Sprintf("quay.io/rhoai/rhoai-fbc-fragment:rhoai-2.24@%s", digestOf(12))
	key, _ = fbcCacheKey(noLabels)
	dash := "quay.io/rhoai/odh-dashboard-rhel9@" + digestOf(13)
	fbcContentCache.Add(key, &types.FBCContentResponse{RelatedImages: []types.RelatedImage{{Name: "odh_dashboard_image", Image: dash}}}, 0)
	repo, digest, _ := extractRepoAndDigest(dash)
	labelCache.Add(repo+"@"+digest, labelCacheValue{noLabels: true}, 0)

	builds := staticBuilds(
		prBuild{image: noDash},
		prBuild{image: noLabels},
		prBuild{image: seedBuild(t, 3, "rhoai-2.23", "https://gitlab.com/x/odh-dashboard", commitNew)},
		prBuild{image: seedBuild(t, 4, "rhoai-2.22", "https://github.com/someone/odh-dashboard", commitNew)},
		prBuild{image: seedBuild(t, 5, "rhoai-2.21", forkURL, "3333333333333333333333333333333333333333")},
		prBuild{image: seedBuild(t, 6, "rhoai-2.20", forkURL, errSHA)},
		prBuild{image: seedBuild(t, 7, "rhoai-3.6", forkURL, commitNew)},
	)
	g.compare[mergeSHA+"..."+errSHA] = "weird"
	resp, err := findBuildsContainingPR(context.Background(), "", DefaultPRSearchRepo, 9938, builds)
	if err != nil {
		t.Fatal(err)
	}
	got := resultsByTag(resp)
	want := map[string]string{
		"rhoai-2.25": "no_component_image",
		"rhoai-2.24": "no_commit_label",
		"rhoai-2.23": "no_commit_label",
		"rhoai-2.22": "unexpected_repo",
		"rhoai-2.21": "commit_not_found",
		"rhoai-2.20": "github_error",
	}
	for tag, reason := range want {
		if got[tag].Result != "unknown" || got[tag].Reason != reason {
			t.Errorf("%s: %+v, want unknown/%s", tag, got[tag], reason)
		}
	}
	if got["rhoai-3.6"].Result != "contains" || resp.RateLimited {
		t.Errorf("good build %+v, rateLimited %v", got["rhoai-3.6"], resp.RateLimited)
	}
	if got["rhoai-2.22"].CompareURL != "" {
		t.Error("a repository outside the search was compared")
	}
}

func TestFindBuildsContainingPR_BoundedConcurrencyAndToken(t *testing.T) {
	g := newFakeGitHub()
	g.delay = 30 * time.Millisecond
	installFakeGitHub(t, g)
	t.Setenv("GITHUB_TOKEN", "ghp_test")
	g.prs[9938] = mergedPR(mergeSHA)
	var builds []prBuild
	for i := 0; i < 12; i++ {
		commit := fmt.Sprintf("%040d", i+1)
		g.compare[mergeSHA+"..."+commit] = "ahead"
		builds = append(builds, prBuild{image: seedBuild(t, 100+i, fmt.Sprintf("rhoai-3.%d", i), forkURL, commit)})
	}
	resp, err := findBuildsContainingPR(context.Background(), "", DefaultPRSearchRepo, 9938, staticBuilds(builds...))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range resp.Builds {
		if b.Result != "contains" {
			t.Errorf("%s: %+v", b.Tag, b)
		}
	}
	if m := g.maxIn.Load(); m > prConcurrency || m < 2 {
		t.Errorf("max concurrent GitHub calls %d, want 2..%d", m, prConcurrency)
	}
	for _, a := range g.auth {
		if a != "Bearer ghp_test" {
			t.Errorf("Authorization %q", a)
		}
	}
}

func TestFindBuildsContainingPR_Timeout(t *testing.T) {
	g := newFakeGitHub()
	installFakeGitHub(t, g)
	g.prs[9938] = mergedPR(mergeSHA)
	ctx, cancel := context.WithCancel(context.Background())
	image := seedBuild(t, 1, "rhoai-3.6", forkURL, commitNew)
	resp, err := findBuildsContainingPR(ctx, "", DefaultPRSearchRepo, 9938, func(context.Context) ([]prBuild, error) {
		cancel()
		return []prBuild{{image: image}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if b := resp.Builds[0]; b.Result != "unknown" || b.Reason != "build_unreadable" {
		t.Errorf("build after the deadline: %+v", b)
	}
}

func TestGitHubRepoFromURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/red-hat-data-services/odh-dashboard":      forkRepo,
		"https://github.com/red-hat-data-services/odh-dashboard.git":  forkRepo,
		"https://github.com/red-hat-data-services/odh-dashboard/":     forkRepo,
		"https://gitlab.com/red-hat-data-services/odh-dashboard":      "",
		"https://github.com/red-hat-data-services/odh-dashboard/tree": "",
		"": "",
	} {
		got, ok := githubRepoFromURL(in)
		if got != want || ok != (want != "") {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
}
