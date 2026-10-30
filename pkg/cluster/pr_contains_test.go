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

// fakeGitHub is an in-memory GitHub REST API: pull requests, compare
// answers, branch heads and commit dates, with injectable failures. It
// counts requests per path kind.
type fakeGitHub struct {
	mu  sync.Mutex
	prs map[int]map[string]interface{}
	// prRepos are the repositories prs are found in (any other answers 404).
	prRepos map[string]bool
	// commits are the commits GET /repos/<repo>/commits/<ref> resolves, by
	// "repo|full sha" -> message; a ref matches by prefix.
	commits map[string]string
	compare map[string]string // "base...head" -> status; missing = 404
	// compareCounts gives ahead_by and behind_by of a compare (default 1, 0).
	compareCounts map[string][2]int
	// compareRepos are the repositories compare answers for.
	compareRepos map[string]bool
	branches     map[string]string // "repo:branch" -> head SHA; missing = 404
	commitDate   string            // committer date of every git/commits/<sha>
	failures     map[string]func(w http.ResponseWriter) bool
	calls        map[string]int
	queries      []url.Values
	auth         []string
	inFlight     atomic.Int32
	maxIn        atomic.Int32
	delay        time.Duration
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		prs:           map[int]map[string]interface{}{},
		prRepos:       map[string]bool{DefaultPRSearchRepo: true},
		commits:       map[string]string{},
		compare:       map[string]string{},
		compareCounts: map[string][2]int{},
		compareRepos:  map[string]bool{forkRepo: true},
		branches:      map[string]string{},
		commitDate:    "2026-10-28T12:00:00Z",
		failures:      map[string]func(w http.ResponseWriter) bool{},
		calls:         map[string]int{},
	}
}

// rateLimited makes every request of kind answer 429 with a reset in resetIn.
func (g *fakeGitHub) rateLimited(kind string, resetIn time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failures[kind] = func(w http.ResponseWriter) bool {
		w.Header().Set("X-Ratelimit-Remaining", "0")
		w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(time.Now().Add(resetIn).Unix(), 10))
		w.WriteHeader(http.StatusTooManyRequests)
		return true
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
	case strings.Contains(r.URL.Path, "/branches/"):
		kind = "branch"
	case strings.Contains(r.URL.Path, "/git/commits/"):
		kind = "commit"
	case strings.Contains(r.URL.Path, "/commits/"):
		kind = "commits"
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
		repo := strings.TrimPrefix(r.URL.Path[:strings.Index(r.URL.Path, "/pulls/")], "/repos/")
		g.mu.Lock()
		pr, ok := g.prs[num]
		allowed := g.prRepos[repo]
		g.mu.Unlock()
		if !ok || !allowed {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(pr)
	case "commits":
		repo := strings.TrimPrefix(r.URL.Path[:strings.Index(r.URL.Path, "/commits/")], "/repos/")
		ref := strings.ToLower(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		g.mu.Lock()
		var sha, message string
		for key, msg := range g.commits {
			keyRepo, full, _ := strings.Cut(key, "|")
			if keyRepo == repo && strings.HasPrefix(full, ref) {
				sha, message = full, msg
			}
		}
		date := g.commitDate
		g.mu.Unlock()
		if sha == "" {
			http.Error(w, `{"message":"No commit found for SHA: `+ref+`"}`, http.StatusUnprocessableEntity)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"sha": sha, "html_url": "https://github.com/" + repo + "/commit/" + sha,
			"commit": map[string]interface{}{"message": message, "committer": map[string]interface{}{"date": date}},
		})
	case "compare":
		repo := strings.TrimPrefix(r.URL.Path[:strings.Index(r.URL.Path, "/compare/")], "/repos/")
		spec := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		g.mu.Lock()
		status, ok := g.compare[spec]
		counts, hasCounts := g.compareCounts[spec]
		allowed := g.compareRepos[repo]
		g.mu.Unlock()
		if !allowed {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		if !hasCounts {
			counts = [2]int{1, 0}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": status, "ahead_by": counts[0], "behind_by": counts[1], "commits": []string{}})
	case "branch":
		repo := strings.TrimPrefix(r.URL.Path[:strings.Index(r.URL.Path, "/branches/")], "/repos/")
		branch := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		g.mu.Lock()
		sha, ok := g.branches[repo+":"+branch]
		g.mu.Unlock()
		if !ok {
			http.Error(w, `{"message":"Branch not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"name": branch, "commit": map[string]interface{}{"sha": sha}})
	case "commit":
		g.mu.Lock()
		date := g.commitDate
		g.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"committer": map[string]interface{}{"date": date}})
	default:
		http.NotFound(w, r)
	}
}

// githubTransport sends api.github.com requests to the fake server. Requests
// to any other host (quay.io) fail, and are counted when a counter is given.
type githubTransport struct {
	server *url.URL
	other  *atomic.Int32
}

func (t githubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "api.github.com" {
		if t.other != nil {
			t.other.Add(1)
		}
		return nil, fmt.Errorf("unexpected host %s", r.URL.Host)
	}
	out := r.Clone(r.Context())
	out.URL.Scheme, out.URL.Host = t.server.Scheme, t.server.Host
	return http.DefaultTransport.RoundTrip(out)
}

// installFakeGitHub routes GitHub to g and returns a counter of the registry
// reads that were attempted (there is no registry: every build must come
// from the caches, see seedBuild).
func installFakeGitHub(t *testing.T, g *fakeGitHub) *atomic.Int32 {
	t.Helper()
	server := httptest.NewServer(g)
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	resetRegistryCaches()
	original := quayHTTPClient
	registryReads := &atomic.Int32{}
	quayHTTPClient = &http.Client{Transport: githubTransport{server: u, other: registryReads}, Timeout: 10 * time.Second}
	t.Cleanup(func() {
		quayHTTPClient = original
		resetRegistryCaches()
	})
	return registryReads
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

// seedBuildDate puts the FBC image's build-date label in the cache, as the
// tags listing does.
func seedBuildDate(t *testing.T, fbc, buildDate string) {
	t.Helper()
	repo, digest, ok := extractRepoAndDigest(fbc)
	if !ok {
		t.Fatal("bad fbc ref")
	}
	labelCache.Add(repo+"@"+digest, labelCacheValue{labels: ImageLabels{BuildDate: buildDate}}, 0)
}

// seedComponentBuild puts a build in the caches whose component images are
// not the dashboard: the given (name, image repository, git URL, commit)
// tuples, in catalog order, plus a base image without labels.
func seedComponentBuild(t *testing.T, n int, tag string, images ...[4]string) string {
	t.Helper()
	fbc := fmt.Sprintf("quay.io/rhoai/rhoai-fbc-fragment:%s@%s", tag, digestOf(n))
	key, ok := fbcCacheKey(fbc)
	if !ok {
		t.Fatal("bad fbc ref")
	}
	related := []types.RelatedImage{{Name: "odh_base_image", Image: "registry.redhat.io/ubi9/ubi-minimal@" + digestOf(3000+n)}}
	for i, img := range images {
		ref := fmt.Sprintf("quay.io/rhoai/%s@%s", img[1], digestOf(4000+10*n+i))
		related = append(related, types.RelatedImage{Name: img[0], Image: ref})
		repo, digest, _ := extractRepoAndDigest(ref)
		if img[3] == "" {
			labelCache.Add(repo+"@"+digest, labelCacheValue{noLabels: true}, 0)
		} else {
			labelCache.Add(repo+"@"+digest, labelCacheValue{labels: ImageLabels{GitCommit: img[3], GitURL: img[2]}}, 0)
		}
	}
	fbcContentCache.Add(key, &types.FBCContentResponse{Tag: tag, RelatedImages: related}, 0)
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

	t.Run("repository that is not owner/name", func(t *testing.T) {
		g := newFakeGitHub()
		installFakeGitHub(t, g)
		for _, repo := range []string{"evil", "a/b/c", "a/b c", ""} {
			_, err := findBuildsContainingPR(context.Background(), "", repo, 1, staticBuilds())
			var searchErr *PRSearchError
			if !errors.As(err, &searchErr) || searchErr.Code != "validation" || g.count("pull") != 0 {
				t.Fatalf("%q: err %v, calls %d", repo, err, g.count("pull"))
			}
		}
	})
}

func TestForkCandidates(t *testing.T) {
	for repo, want := range map[string][]string{
		"opendatahub-io/odh-dashboard":          {"red-hat-data-services/odh-dashboard", "opendatahub-io/odh-dashboard"},
		"opendatahub-io/data-science-pipelines": {"red-hat-data-services/data-science-pipelines", "opendatahub-io/data-science-pipelines"},
		"red-hat-data-services/odh-dashboard":   {"red-hat-data-services/odh-dashboard"},
		"Red-Hat-Data-Services/ODH-Dashboard":   {"Red-Hat-Data-Services/ODH-Dashboard"},
		"kserve/kserve":                         {"red-hat-data-services/kserve", "kserve/kserve"},
	} {
		got := forkCandidates(repo)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: %v, want %v", repo, got, want)
		}
	}
	if !isCandidate([]string{"red-hat-data-services/odh-dashboard"}, "Red-Hat-Data-Services/odh-dashboard") {
		t.Error("candidates must compare case-insensitively")
	}
}

func TestFindBuildsContainingPR_AnyRepository(t *testing.T) {
	// A data-science-pipelines PR: no image is found by name, so the labels
	// of the build's RHOAI images are read and the one built from the fork
	// (whatever its name) is compared.
	g := newFakeGitHub()
	installFakeGitHub(t, g)
	const dspRepo = "opendatahub-io/data-science-pipelines"
	const dspFork = "red-hat-data-services/data-science-pipelines"
	g.prRepos[dspRepo] = true
	g.compareRepos[dspFork] = true
	g.prs[777] = mergedPR(mergeSHA)
	g.compare[mergeSHA+"..."+commitNew] = "ahead"
	g.compare[mergeSHA+"..."+commitOld] = "diverged"
	kserveURL := "https://github.com/red-hat-data-services/kserve"
	dspURL := "https://github.com/" + dspFork + ".git"
	withDSP := seedComponentBuild(t, 1, "rhoai-3.6",
		[4]string{"odh_kserve_image", "odh-kserve", kserveURL, commitOld},
		[4]string{"odh_ml_pipelines_api_server_v2_image", "odh-ml-pipelines-api-server-v2-rhel9", dspURL, commitNew},
		[4]string{"odh_ml_pipelines_persistenceagent_v2_image", "odh-ml-pipelines-persistenceagent-v2-rhel9", dspURL, commitNew},
	)
	older := seedComponentBuild(t, 2, "rhoai-3.5",
		[4]string{"odh_ml_pipelines_api_server_v2_image", "odh-ml-pipelines-api-server-v2-rhel9", dspURL, commitOld},
	)
	// The first matching image's commit is malformed: the next one wins.
	badCommit := seedComponentBuild(t, 3, "rhoai-3.4",
		[4]string{"odh_ml_pipelines_api_server_v2_image", "odh-ml-pipelines-api-server-v2-rhel9", dspURL, "not-a-sha"},
		[4]string{"odh_ml_pipelines_persistenceagent_v2_image", "odh-ml-pipelines-persistenceagent-v2-rhel9", dspURL, commitNew},
	)
	withoutDSP := seedComponentBuild(t, 4, "rhoai-3.3",
		[4]string{"odh_kserve_image", "odh-kserve", kserveURL, commitOld},
		[4]string{"odh_unlabelled_image", "odh-unlabelled", "", ""},
	)
	builds := staticBuilds(prBuild{image: withDSP}, prBuild{image: older}, prBuild{image: badCommit}, prBuild{image: withoutDSP})
	resp, err := findBuildsContainingPR(context.Background(), "", dspRepo, 777, builds)
	if err != nil {
		t.Fatal(err)
	}
	got := resultsByTag(resp)
	if b := got["rhoai-3.6"]; b.Result != "contains" || b.Commit != commitNew || !strings.EqualFold(b.CommitRepo, dspFork) ||
		!strings.Contains(b.CompareURL, "/compare/"+mergeSHA+"..."+commitNew) {
		t.Errorf("rhoai-3.6: %+v", b)
	}
	if b := got["rhoai-3.5"]; b.Result != "not_contained" || b.Commit != commitOld {
		t.Errorf("rhoai-3.5: %+v", b)
	}
	if b := got["rhoai-3.4"]; b.Result != "contains" || b.Commit != commitNew {
		t.Errorf("rhoai-3.4 (first match malformed): %+v", b)
	}
	if b := got["rhoai-3.3"]; b.Result != "unknown" || b.Reason != "no_component_image" ||
		b.Message != "This build has no image built from "+dspFork+" or "+dspRepo+"." {
		t.Errorf("rhoai-3.3: %+v", b)
	}
	if resp.URL != "https://github.com/opendatahub-io/odh-dashboard/pull/9938" { // the fake's html_url
		t.Errorf("url %q", resp.URL)
	}
	// The component of each build is remembered: a second search reads no labels.
	if _, ok := componentImageCache.Get(digestOf(1) + "|" + dspRepo); !ok {
		t.Error("the component image was not cached")
	}
	if _, ok := componentImageCache.Get(digestOf(4) + "|" + dspRepo); !ok {
		t.Error("the complete scan without a match was not cached")
	}

	t.Run("the PR on the fork itself", func(t *testing.T) {
		g.prRepos[dspFork] = true
		resp, err := findBuildsContainingPR(context.Background(), "", dspFork, 777, staticBuilds(prBuild{image: withDSP}))
		if err != nil {
			t.Fatal(err)
		}
		if b := resp.Builds[0]; b.Result != "contains" {
			t.Errorf("%+v", b)
		}
	})
}

func TestFindBuildsContainingPR_BuiltBeforeMerge(t *testing.T) {
	g := newFakeGitHub()
	registryReads := installFakeGitHub(t, g)
	g.prs[9938] = mergedPR(mergeSHA) // merged 2026-10-05T21:16:13Z
	g.compare[mergeSHA+"..."+commitNew] = "ahead"

	// Only the build date is known about the old build: its catalog is not
	// cached, so reading it would hit the (absent) registry.
	old := fmt.Sprintf("quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.4@%s", digestOf(1))
	seedBuildDate(t, old, "2026-10-03T21:00:00Z")
	sameMinute := fmt.Sprintf("quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.3@%s", digestOf(2))
	seedBuildDate(t, sameMinute, "2026-10-05T21:16:13Z") // not before: must be checked
	// Within the hour before the merge: clocks may disagree, so it is checked.
	halfHour := fmt.Sprintf("quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.2@%s", digestOf(5))
	seedBuildDate(t, halfHour, "2026-10-05T20:46:13Z")
	twoHours := fmt.Sprintf("quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.1@%s", digestOf(6))
	seedBuildDate(t, twoHours, "2026-10-05T19:16:13Z")
	newer := seedBuild(t, 3, "rhoai-3.6", forkURL, commitNew)
	seedBuildDate(t, newer, "2026-10-06T02:30:00Z")
	undated := seedBuild(t, 4, "rhoai-3.5", forkURL, commitNew)

	resp, err := findBuildsContainingPR(context.Background(), "", DefaultPRSearchRepo, 9938, staticBuilds(
		prBuild{image: old}, prBuild{image: sameMinute}, prBuild{image: halfHour}, prBuild{image: twoHours}, prBuild{image: newer}, prBuild{image: undated}))
	if err != nil {
		t.Fatal(err)
	}
	got := resultsByTag(resp)
	if b := got["rhoai-3.4"]; b.Result != "not_contained" || b.Reason != "built_before_merge" || b.Message != "Built 2 days before the PR was merged." || b.Commit != "" || b.CompareURL != "" {
		t.Errorf("old build: %+v", b)
	}
	if b := got["rhoai-3.3"]; b.Result != "unknown" || b.Reason != "build_unreadable" {
		t.Errorf("build of the merge minute must be read: %+v", b)
	}
	if b := got["rhoai-3.2"]; b.Result != "unknown" || b.Reason != "build_unreadable" {
		t.Errorf("build 30 minutes before the merge must be read: %+v", b)
	}
	if b := got["rhoai-3.1"]; b.Result != "not_contained" || b.Reason != "built_before_merge" || b.Message != "Built 2 hours before the PR was merged." {
		t.Errorf("build 2 hours before the merge: %+v", b)
	}
	if b := got["rhoai-3.6"]; b.Result != "contains" {
		t.Errorf("newer build: %+v", b)
	}
	if b := got["rhoai-3.5"]; b.Result != "contains" {
		t.Errorf("undated build: %+v", b)
	}
	// One compare for each build that was read (the undated one and the
	// newer one share the cached answer); the old build cost nothing.
	if g.count("compare") != 1 {
		t.Errorf("%d compare calls, want 1", g.count("compare"))
	}
	// The registry was asked once, for the uncached catalog of rhoai-3.3
	// (and the undated build's missing date), never for the old build.
	if n := registryReads.Load(); n == 0 || n > 2 {
		t.Errorf("%d registry reads", n)
	}
	if _, ok := cachedFBCContent(old); ok {
		t.Error("the old build's catalog was read")
	}
}

func TestRoughDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		20 * time.Second:              "1 minute",
		time.Minute:                   "1 minute",
		25 * time.Minute:              "25 minutes",
		time.Hour:                     "1 hour",
		90 * time.Minute:              "2 hours",
		47 * time.Hour:                "47 hours",
		49 * time.Hour:                "2 days",
		12*24*time.Hour + 5*time.Hour: "12 days",
	} {
		if got := roughDuration(d); got != want {
			t.Errorf("%s: %q, want %q", d, got, want)
		}
	}
}

func TestFindBuildsContainingCommit(t *testing.T) {
	const upstreamSHA = "6c8ac6e0f1d2c3b4a5968778695a4b3c2d1e0f9a"
	t.Run("resolved upstream, compared in the fork, short SHA", func(t *testing.T) {
		g := newFakeGitHub()
		installFakeGitHub(t, g)
		g.commits[DefaultPRSearchRepo+"|"+upstreamSHA] = "Show the model deployment status\n\nLonger text."
		g.commitDate = "2026-10-05T10:00:00Z"
		g.compare[upstreamSHA+"..."+commitNew] = "ahead"
		g.compare[upstreamSHA+"..."+commitOld] = "diverged"
		contains := seedBuild(t, 1, "rhoai-3.6", forkURL, commitNew)
		seedBuildDate(t, contains, "2026-10-06T02:30:00Z")
		not := seedBuild(t, 2, "rhoai-3.5", forkURL, commitOld)
		before := fmt.Sprintf("quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.4@%s", digestOf(3))
		seedBuildDate(t, before, "2026-10-04T03:00:00Z")
		resp, err := findBuildsContainingCommit(context.Background(), "", DefaultPRSearchRepo, "6C8AC6E", staticBuilds(
			prBuild{image: contains, installed: true}, prBuild{image: not}, prBuild{image: before}))
		if err != nil {
			t.Fatal(err)
		}
		if resp.Commit != upstreamSHA || resp.CommitTitle != "Show the model deployment status" || resp.CommitDate != "2026-10-05T10:00:00Z" ||
			resp.CommitURL != "https://github.com/"+DefaultPRSearchRepo+"/commit/"+upstreamSHA || resp.PR != 0 || resp.MergeCommit != "" || resp.Repo != DefaultPRSearchRepo {
			t.Fatalf("response %+v", resp)
		}
		got := resultsByTag(resp)
		if b := got["rhoai-3.6"]; b.Result != "contains" || !b.Installed || b.CompareURL != "https://github.com/"+forkRepo+"/compare/"+upstreamSHA+"..."+commitNew {
			t.Errorf("rhoai-3.6: %+v", b)
		}
		if b := got["rhoai-3.5"]; b.Result != "not_contained" || b.Reason != "" {
			t.Errorf("rhoai-3.5: %+v", b)
		}
		if b := got["rhoai-3.4"]; b.Result != "not_contained" || b.Reason != "built_before_commit" || b.Message != "Built 31 hours before the commit." {
			t.Errorf("rhoai-3.4: %+v", b)
		}
		if g.count("commits") != 1 {
			t.Errorf("%d commit lookups, want 1 (upstream knows it)", g.count("commits"))
		}
		// Resolved commits and compare answers are kept: a repeat is free.
		before2 := g.count("commits") + g.count("compare")
		if _, err := findBuildsContainingCommit(context.Background(), "", DefaultPRSearchRepo, "6c8ac6e", staticBuilds(prBuild{image: contains}, prBuild{image: not})); err != nil {
			t.Fatal(err)
		}
		if after := g.count("commits") + g.count("compare"); after != before2 {
			t.Errorf("repeat search made %d GitHub calls", after-before2)
		}
	})

	t.Run("resolved on the fork when upstream does not know it", func(t *testing.T) {
		g := newFakeGitHub()
		installFakeGitHub(t, g)
		g.commits[forkRepo+"|"+commitOld] = "Fork-only change"
		g.compare[commitOld+"..."+commitNew] = "ahead"
		resp, err := findBuildsContainingCommit(context.Background(), "", DefaultPRSearchRepo, commitOld[:10], staticBuilds(prBuild{image: seedBuild(t, 1, "rhoai-3.6", forkURL, commitNew)}))
		if err != nil {
			t.Fatal(err)
		}
		if resp.Commit != commitOld || resp.CommitURL != "https://github.com/"+forkRepo+"/commit/"+commitOld || resp.Builds[0].Result != "contains" {
			t.Errorf("response %+v", resp)
		}
		if g.count("commits") != 2 {
			t.Errorf("%d commit lookups, want 2 (upstream, then the fork)", g.count("commits"))
		}
	})

	t.Run("unknown everywhere", func(t *testing.T) {
		g := newFakeGitHub()
		installFakeGitHub(t, g)
		listed := false
		_, err := findBuildsContainingCommit(context.Background(), "", DefaultPRSearchRepo, "abcdef0", func(context.Context) ([]prBuild, error) {
			listed = true
			return nil, nil
		})
		var searchErr *PRSearchError
		if !errors.As(err, &searchErr) || searchErr.Status != 404 || searchErr.Code != "commit_not_found" ||
			searchErr.Msg != "GitHub does not know commit abcdef0 in opendatahub-io/odh-dashboard or red-hat-data-services/odh-dashboard." {
			t.Fatalf("err %v", err)
		}
		if listed || g.count("commits") != 2 {
			t.Errorf("listed %v, lookups %d", listed, g.count("commits"))
		}
		// The miss is remembered for a while.
		if _, err := findBuildsContainingCommit(context.Background(), "", DefaultPRSearchRepo, "abcdef0", staticBuilds()); err == nil || g.count("commits") != 2 {
			t.Errorf("err %v, lookups %d", err, g.count("commits"))
		}
	})

	t.Run("rate limited while resolving: every build unknown, never an error", func(t *testing.T) {
		g := newFakeGitHub()
		installFakeGitHub(t, g)
		g.rateLimited("commits", 10*time.Minute)
		resp, err := findBuildsContainingCommit(context.Background(), "", DefaultPRSearchRepo, "abcdef0", staticBuilds(prBuild{image: seedBuild(t, 1, "rhoai-3.6", forkURL, commitNew)}))
		if err != nil {
			t.Fatal(err)
		}
		if !resp.RateLimited || resp.RetryAfterSeconds < 500 || resp.Commit != "abcdef0" {
			t.Fatalf("response %+v", resp)
		}
		if b := resp.Builds[0]; b.Result != "unknown" || b.Reason != "rate_limited" || b.Commit != commitNew {
			t.Errorf("build %+v", b)
		}
	})

	t.Run("invalid input", func(t *testing.T) {
		g := newFakeGitHub()
		installFakeGitHub(t, g)
		for repo, sha := range map[string]string{"evil": "abcdef0", DefaultPRSearchRepo: "abc", "a/b": strings.Repeat("a", 41)} {
			_, err := findBuildsContainingCommit(context.Background(), "", repo, sha, staticBuilds())
			var searchErr *PRSearchError
			if !errors.As(err, &searchErr) || searchErr.Code != "validation" {
				t.Errorf("%s %s: %v", repo, sha, err)
			}
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
	if got["rhoai-2.25"].Message != "This build has no image built from red-hat-data-services/odh-dashboard or opendatahub-io/odh-dashboard." {
		t.Errorf("no_component_image message %q", got["rhoai-2.25"].Message)
	}
	// A compare 404 means GitHub does not know the merge commit at all (forks
	// of one network resolve each other's SHAs), which is said, not "does
	// not contain".
	if got["rhoai-2.21"].Message != "GitHub does not know commit "+mergeSHA[:12]+" in "+DefaultPRSearchRepo+" or "+forkRepo+"." {
		t.Errorf("commit_not_found message %q", got["rhoai-2.21"].Message)
	}
}

func TestValidSearchRepo(t *testing.T) {
	for repo, want := range map[string]bool{
		"opendatahub-io/odh-dashboard":                    true,
		"OpenDataHub-io/odh-dashboard":                    true, // GitHub names are case-insensitive
		"red-hat-data-services/data-science-pipelines":    true,
		"kubeflow/kubeflow":                               true,
		"kserve/kserve":                                   true,
		"trustyai-explainability/trustyai-explainability": true,
		"llm-d/llm-d":                                     true,
		"feast-dev/feast":                                 true,
		"ray-project/kuberay":                             true,
		"opendatahub-io/odh_dashboard.v2":                 true,
		"evil/repo":                                       false, // an owner outside the allowlist
		"../x":                                            false, // would reach GitHub as /repos/../x
		"opendatahub-io/..":                               false,
		"opendatahub-io/.":                                false,
		"opendatahub-io/":                                 false,
		"opendatahub-io":                                  false,
		"opendatahub-io/odh dashboard":                    false,
		"opendatahub-io/odh-dashboard/x":                  false,
		"opendatahub.io/odh-dashboard":                    false, // owners have no dots
		"opendatahub-io/" + strings.Repeat("a", 101):      false,
	} {
		if got := ValidSearchRepo(repo); got != want {
			t.Errorf("ValidSearchRepo(%q) = %v, want %v", repo, got, want)
		}
	}
}

func TestFindBuildsContainingPR_RepositoryAllowlist(t *testing.T) {
	g := newFakeGitHub()
	installFakeGitHub(t, g)
	t.Run("an owner outside the allowlist is refused before GitHub is asked", func(t *testing.T) {
		for _, repo := range []string{"evil/repo", "../x", "opendatahub-io/.."} {
			_, err := findBuildsContainingPR(context.Background(), "", repo, 5, staticBuilds())
			var searchErr *PRSearchError
			if !errors.As(err, &searchErr) || searchErr.Status != 400 || searchErr.Code != "validation" || searchErr.Msg != SearchRepoMessage {
				t.Errorf("%s: %v", repo, err)
			}
			_, err = findBuildsContainingCommit(context.Background(), "", repo, "abcdef0", staticBuilds())
			if !errors.As(err, &searchErr) || searchErr.Status != 400 || searchErr.Code != "validation" || searchErr.Msg != SearchRepoMessage {
				t.Errorf("%s (commit): %v", repo, err)
			}
		}
		if g.count("pull")+g.count("commits") != 0 {
			t.Error("GitHub was asked about a refused repository")
		}
	})
	t.Run("the owner's case does not matter", func(t *testing.T) {
		g.prRepos["OpenDataHub-io/odh-dashboard"] = true
		g.prs[9938] = mergedPR(mergeSHA)
		g.compare[mergeSHA+"..."+commitNew] = "ahead"
		resp, err := findBuildsContainingPR(context.Background(), "", "OpenDataHub-io/odh-dashboard", 9938, staticBuilds(prBuild{image: seedBuild(t, 1, "rhoai-3.6", forkURL, commitNew)}))
		if err != nil {
			t.Fatal(err)
		}
		if resp.Builds[0].Result != "contains" {
			t.Errorf("%+v", resp.Builds[0])
		}
	})
	if SearchRepoMessage != "PR and commit search cover repositories of opendatahub-io, red-hat-data-services, kubeflow, kserve, trustyai-explainability, llm-d, feast-dev and ray-project." {
		t.Errorf("message %q", SearchRepoMessage)
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
