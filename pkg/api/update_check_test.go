package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHighestRelease(t *testing.T) {
	tags := []string{"latest", "main", "v1", "2cdcb42d", "v1.0.0", "v1.9.0", "v1.10.0", "v2.0.0-rc.1", "v01.0.0", "1.2.3", "v1.2"}
	if got := highestRelease(tags); got != "v1.10.0" {
		t.Fatalf("highestRelease = %q, want v1.10.0 (numeric, releases only)", got)
	}
	if got := highestRelease([]string{"latest", "main"}); got != "" {
		t.Fatalf("no releases: got %q", got)
	}
}

func TestCompareReleases(t *testing.T) {
	const notes = "https://gitlab.example.com/group/app/-/releases/"
	cases := []struct {
		name, running, latest, url string
		want                       UpdateInfo
	}{
		{"commit build", "2cdcb42d", "v2.0.0", notes, UpdateInfo{}},
		{"dev build", "dev", "v2.0.0", notes, UpdateInfo{}},
		{"unknown latest", "v1.0.0", "", notes, UpdateInfo{Current: "v1.0.0"}},
		{"up to date", "v2.0.0", "v2.0.0", notes, UpdateInfo{Current: "v2.0.0", Latest: "v2.0.0"}},
		{"newer running", "v2.1.0", "v2.0.0", notes, UpdateInfo{Current: "v2.1.0", Latest: "v2.0.0"}},
		{"patch", "v2.0.0", "v2.0.1", notes, UpdateInfo{Current: "v2.0.0", Latest: "v2.0.1", UpdateAvailable: true, ReleaseNotesURL: "https://gitlab.example.com/group/app/-/releases/v2.0.1"}},
		{"major", "v1.0.0", "v2.0.0", notes, UpdateInfo{Current: "v1.0.0", Latest: "v2.0.0", UpdateAvailable: true, MajorUpgrade: true, ReleaseNotesURL: "https://gitlab.example.com/group/app/-/releases/v2.0.0"}},
		{"no https notes", "v1.0.0", "v1.1.0", "http://example.com/releases", UpdateInfo{Current: "v1.0.0", Latest: "v1.1.0", UpdateAvailable: true}},
	}
	for _, c := range cases {
		if got := compareReleases(c.running, c.latest, c.url); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestRegistryRepository(t *testing.T) {
	cases := map[string][2]string{
		"quay.io/juntao_wang/rhoai-nightly-updater": {"https://quay.io", "juntao_wang/rhoai-nightly-updater"},
		"registry.example.com:5000/a/b/c":           {"https://registry.example.com:5000", "a/b/c"},
		"localhost/app":                             {"https://localhost", "app"},
		"org/app":                                   {"https://registry-1.docker.io", "org/app"},
		"docker.io/busybox":                         {"https://registry-1.docker.io", "library/busybox"},
	}
	for in, want := range cases {
		base, path, err := registryRepository(in)
		if err != nil || base != want[0] || path != want[1] {
			t.Errorf("registryRepository(%q) = %q, %q, %v; want %q, %q", in, base, path, err, want[0], want[1])
		}
	}
	for _, bad := range []string{"", "quay.io/a/b:latest", "quay.io/a/b@sha256:00"} {
		if _, _, err := registryRepository(bad); err == nil {
			t.Errorf("registryRepository(%q): want an error", bad)
		}
	}
}

// fakeRegistry serves a two-page tag list behind an anonymous bearer token.
func fakeRegistry(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var tokenCalls atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenCalls.Add(1)
			if r.URL.Query().Get("scope") != "repository:team/app:pull" || r.URL.Query().Get("service") != "reg" {
				http.Error(w, "bad scope", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "anon"})
		case "/v2/team/app/tags/list":
			if r.Header.Get("Authorization") != "Bearer anon" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="reg"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.URL.Query().Get("last") == "" {
				w.Header().Set("Link", `</v2/team/app/tags/list?n=1000&last=v1.0.0>; rel="next"`)
				_ = json.NewEncoder(w).Encode(map[string][]string{"tags": {"latest", "v1.0.0"}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string][]string{"tags": {"v2.0.0", "v1.9.9", "main"}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &tokenCalls
}

func TestListRegistryTags(t *testing.T) {
	srv, tokenCalls := fakeRegistry(t)
	repo := strings.TrimPrefix(srv.URL, "https://") + "/team/app"
	tags, err := listRegistryTags(context.Background(), srv.Client(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(tags, ",") != "latest,v1.0.0,v2.0.0,v1.9.9,main" {
		t.Fatalf("tags = %v (both pages expected)", tags)
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("token requested %d times, want once", tokenCalls.Load())
	}
	if got := highestRelease(tags); got != "v2.0.0" {
		t.Fatalf("highest = %q", got)
	}
	if _, err := listRegistryTags(context.Background(), srv.Client(), strings.TrimPrefix(srv.URL, "https://")+"/team/missing"); err == nil {
		t.Fatal("a missing repository must be an error")
	}
}

func TestAnonymousTokenNeedsHTTPSRealm(t *testing.T) {
	if _, err := anonymousToken(context.Background(), http.DefaultClient, `Bearer realm="http://example.com/token"`, "a/b"); err == nil {
		t.Fatal("a plain-http token realm must be refused")
	}
	if _, err := anonymousToken(context.Background(), http.DefaultClient, `Basic realm="x"`, "a/b"); err == nil {
		t.Fatal("basic authentication is not anonymous")
	}
}

func TestNextPage(t *testing.T) {
	cur := "https://quay.io/v2/a/b/tags/list?n=1000"
	if got := nextPage(cur, `</v2/a/b/tags/list?n=1000&last=x>; rel="next"`); got != "https://quay.io/v2/a/b/tags/list?n=1000&last=x" {
		t.Fatalf("relative next = %q", got)
	}
	if got := nextPage(cur, `<https://evil.example.com/v2/x>; rel="next"`); got != "" {
		t.Fatalf("a next page on another host must be ignored, got %q", got)
	}
	if got := nextPage(cur, ""); got != "" {
		t.Fatalf("no link: %q", got)
	}
}

func TestUpdateCheckerCaches(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	var calls int
	var fail bool
	c := &updateChecker{
		now: func() time.Time { return now },
		fetch: func(context.Context, string) ([]string, error) {
			calls++
			if fail {
				return nil, errors.New("unreachable")
			}
			return []string{"v1.0.0", "v2.0.0"}, nil
		},
	}
	if got := c.latestRelease("quay.io/a/b"); got != "v2.0.0" || calls != 1 {
		t.Fatalf("first read: %q after %d calls", got, calls)
	}
	now = now.Add(5 * time.Hour)
	if got := c.latestRelease("quay.io/a/b"); got != "v2.0.0" || calls != 1 {
		t.Fatalf("cached read: %q after %d calls", got, calls)
	}
	now = now.Add(2 * time.Hour)
	fail = true
	if got := c.latestRelease("quay.io/a/b"); got != "v2.0.0" || calls != 2 {
		t.Fatalf("a failed refresh keeps the last answer: %q after %d calls", got, calls)
	}
	now = now.Add(10 * time.Minute)
	if c.latestRelease("quay.io/a/b"); calls != 2 {
		t.Fatalf("a failed read is not retried at once (%d calls)", calls)
	}
	now = now.Add(time.Hour)
	fail = false
	if got := c.latestRelease("quay.io/a/b"); got != "v2.0.0" || calls != 3 {
		t.Fatalf("retried after the error TTL: %q after %d calls", got, calls)
	}
	fail = true
	if got := c.latestRelease("quay.io/other/repo"); got != "" || calls != 4 {
		t.Fatalf("another repository does not reuse the answer: %q after %d calls", got, calls)
	}
}

func TestHandleUpdateCheck(t *testing.T) {
	oldVersion, oldChecker := Version, defaultUpdateChecker
	t.Cleanup(func() { Version, defaultUpdateChecker = oldVersion, oldChecker })
	var calls int
	defaultUpdateChecker = &updateChecker{
		now:   time.Now,
		fetch: func(context.Context, string) ([]string, error) { calls++; return []string{"v1.0.0", "v2.0.0"}, nil },
	}
	t.Setenv("IMAGE_REPOSITORY", "quay.io/example/app")
	t.Setenv("RELEASES_URL", "https://gitlab.example.com/group/app/-/releases")

	get := func() UpdateInfo {
		w := httptest.NewRecorder()
		HandleUpdateCheck(w, httptest.NewRequest("GET", "/api/update-check", nil))
		var info UpdateInfo
		if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
			t.Fatalf("decode %s: %v", w.Body.String(), err)
		}
		return info
	}

	Version = "v1.0.0"
	want := UpdateInfo{Current: "v1.0.0", Latest: "v2.0.0", UpdateAvailable: true, MajorUpgrade: true, ReleaseNotesURL: "https://gitlab.example.com/group/app/-/releases/v2.0.0"}
	if got := get(); got != want {
		t.Fatalf("v1.0.0: %+v", got)
	}

	Version = "2cdcb42d"
	if got := get(); got != (UpdateInfo{}) || calls != 1 {
		t.Fatalf("a commit build gets no check: %+v (%d registry reads)", got, calls)
	}

	Version = "v2.0.0"
	t.Setenv("IMAGE_REPOSITORY", "")
	if got := get(); got != (UpdateInfo{Current: "v2.0.0"}) {
		t.Fatalf("no repository configured: %+v", got)
	}
}
