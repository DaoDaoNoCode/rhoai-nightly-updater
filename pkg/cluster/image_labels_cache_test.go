package cluster

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// pullSecretCountingClient serves the pull secret and counts how often it is read.
func pullSecretCountingClient(t *testing.T) (*Client, *atomic.Int32) {
	t.Helper()
	cfg := `{"auths":{"quay.io/rhoai":{"auth":"` + exampleAuth("user:pass") + `"}}}`
	secret := fmt.Sprintf(`{"data":{".dockerconfigjson":%q}}`, base64.StdEncoding.EncodeToString([]byte(cfg)))
	var reads atomic.Int32
	c, cleanup := newMockClient(map[string]mockResponse{
		namespacedPath("v1", "secrets", "kube-system", "additional-pull-secret"): {body: secret},
	})
	t.Cleanup(cleanup)
	inner := c.httpClient.Transport
	c.httpClient.Transport = dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/secrets/additional-pull-secret") {
			reads.Add(1)
		}
		return inner.RoundTrip(r)
	})
	return c, &reads
}

func relatedImages(n int, prefix string) []types.RelatedImage {
	var images []types.RelatedImage
	for i := 0; i < n; i++ {
		images = append(images, types.RelatedImage{Name: fmt.Sprint(i), Image: fmt.Sprintf("registry.redhat.io/rhoai/%s%d@%s", prefix, i, digestOf(i))})
	}
	return images
}

func labelFixture(f *fakeRegistry, n int) {
	for i := 0; i < n; i++ {
		f.labels[digestOf(i)] = map[string]string{"vcs-ref": fmt.Sprintf("%040d", i), "git.url": "https://github.com/o/r", "build-date": "2026-10-01"}
	}
}

func TestResolveRelatedImageLabels_OneSecretReadAndBatchedTokens(t *testing.T) {
	f := newFakeRegistry()
	labelFixture(f, 20)
	installFakeRegistry(t, f)
	c, secretReads := pullSecretCountingClient(t)

	images := ResolveRelatedImageLabels(context.Background(), c, relatedImages(20, "img"))
	for _, img := range images {
		if img.GitCommit == "" || img.BuildDate != "2026-10-01" || img.CommitDate != "" {
			t.Fatalf("labels not applied (or commit date fetched): %+v", img)
		}
	}
	if n := secretReads.Load(); n != 1 {
		t.Fatalf("pull secret read %d times, want 1 per batch", n)
	}
	if n := f.count("quay-token"); n != 2 {
		t.Fatalf("token requests %d, want 2 multi-scope tokens for 20 repositories", n)
	}
	for _, scopes := range f.scopes {
		if len(scopes) > quayTokenBatch {
			t.Fatalf("token with %d scopes exceeds the header-size bound %d", len(scopes), quayTokenBatch)
		}
	}
	if f.count("github") != 0 {
		t.Fatal("FBC label resolution must not call GitHub")
	}

	// Second request: labels are immutable per digest, so nothing is fetched.
	ResolveRelatedImageLabels(context.Background(), c, relatedImages(20, "img"))
	if f.count("quay-blob") != 20 || f.count("quay-manifest") != 20 || f.count("quay-token") != 2 {
		t.Fatalf("warm request hit the registry: %v", f.counts)
	}
}

func TestResolveRelatedImageLabels_ConcurrentIdenticalRequestsShareFetches(t *testing.T) {
	f := newFakeRegistry()
	f.latency = 20 * time.Millisecond
	labelFixture(f, 10)
	installFakeRegistry(t, f)
	c, _ := pullSecretCountingClient(t)

	var wg sync.WaitGroup
	for k := 0; k < 3; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ResolveRelatedImageLabels(context.Background(), c, relatedImages(10, "dup"))
		}()
	}
	wg.Wait()
	if n := f.count("quay-blob"); n != 10 {
		t.Fatalf("blob fetches %d, want 10 for 3 concurrent identical requests", n)
	}
}

func TestImageLabels_NoLabelsIsCachedBrieflyErrorsAreNot(t *testing.T) {
	f := newFakeRegistry()
	installFakeRegistry(t, f)
	ref := "quay.io/rhoai/nolabels@" + digestOf(1)
	for i := 0; i < 2; i++ {
		if _, err := imageLabelsWithAuth(context.Background(), "a", ref, false); err != errNoImageLabels {
			t.Fatalf("want errNoImageLabels, got %v", err)
		}
	}
	if f.count("quay-blob") != 1 {
		t.Fatalf("an image without labels should be remembered, blob fetches %d", f.count("quay-blob"))
	}

	f.setStatus("quay-blob", 500)
	ref2 := "quay.io/rhoai/flaky@" + digestOf(2)
	for i := 0; i < 2; i++ {
		if _, err := imageLabelsWithAuth(context.Background(), "a", ref2, false); err == nil {
			t.Fatal("expected error")
		}
	}
	if f.count("quay-blob") != 3 {
		t.Fatalf("transient errors must not be cached, blob fetches %d", f.count("quay-blob"))
	}
}

func TestImageLabels_RejectedTokenIsReplacedOnce(t *testing.T) {
	f := newFakeRegistry()
	labelFixture(f, 1)
	installFakeRegistry(t, f)
	ref := "quay.io/rhoai/img0@" + digestOf(0)
	// Seed a cached token, then have the registry reject the first manifest read.
	if _, err := cachedQuayToken(context.Background(), "a", "rhoai/img0"); err != nil {
		t.Fatal(err)
	}
	var rejected atomic.Bool
	quayHTTPClient.Transport = dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/manifests/") && !rejected.Swap(true) {
			return &http.Response{StatusCode: 401, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
		}
		return f.RoundTrip(r)
	})
	labels, err := imageLabelsWithAuth(context.Background(), "a", ref, false)
	if err != nil || labels.GitCommit == "" {
		t.Fatalf("labels=%+v err=%v", labels, err)
	}
	if f.count("quay-token") != 2 {
		t.Fatalf("expected the rejected token to be replaced once, token requests %d", f.count("quay-token"))
	}
}

func TestQuayTokenTTL(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cases := []struct {
		name  string
		token string
		want  time.Duration
	}{
		{"one hour token", fakeJWT(now.Add(time.Hour)), 55 * time.Minute},
		{"short token", fakeJWT(now.Add(4 * time.Minute)), 2 * time.Minute},
		{"expired", fakeJWT(now.Add(-time.Minute)), 0},
		{"very long token is capped", fakeJWT(now.Add(48 * time.Hour)), time.Hour},
		{"opaque token", "opaque", 30 * time.Second},
		{"bad payload", "a.!!!.c", 30 * time.Second},
	}
	for _, tc := range cases {
		if got := quayTokenTTL(tc.token, now); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestCachedQuayToken_ReusedPerCredentialAndRepo(t *testing.T) {
	f := newFakeRegistry()
	installFakeRegistry(t, f)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := cachedQuayToken(ctx, "cred-a", quayFBCRepo); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cachedQuayToken(ctx, "cred-b", quayFBCRepo); err != nil {
		t.Fatal(err)
	}
	if n := f.count("quay-token"); n != 2 {
		t.Fatalf("token requests %d, want 2 (one per credential)", n)
	}
	f.setStatus("quay-token", 401)
	if _, err := cachedQuayToken(ctx, "cred-c", quayFBCRepo); !isQuayCredentialRejection(err) {
		t.Fatalf("rejection must surface, got %v", err)
	}
}

func TestEnrichTagsWithBuildDates_NoGitHubAndCached(t *testing.T) {
	f := newFakeRegistry()
	installFakeRegistry(t, f)
	var tags []types.NightlyTag
	for i := 0; i < 24; i++ {
		f.labels[digestOf(1000+i)] = map[string]string{"vcs-ref": "74dafc3d", "git.url": "https://github.com/red-hat-data-services/RHOAI-Build-Config", "build-date": fmt.Sprintf("2026-10-%02d", i%28+1)}
		tags = append(tags, types.NightlyTag{Tag: fmt.Sprint(i), Image: fmt.Sprintf("quay.io/rhoai/rhoai-fbc-fragment:t%d@%s", i, digestOf(1000+i))})
	}
	tags = append(tags, types.NightlyTag{Tag: "unpinned", Image: "quay.io/rhoai/rhoai-fbc-fragment:unpinned"})
	enrichTagsWithBuildDates(context.Background(), "a", tags)
	for _, tg := range tags[:24] {
		if tg.BuildDate == "" {
			t.Fatalf("missing build date: %+v", tg)
		}
	}
	if f.count("github") != 0 {
		t.Fatalf("build dates must not call GitHub, got %d calls", f.count("github"))
	}
	enrichTagsWithBuildDates(context.Background(), "a", tags)
	if f.count("quay-blob") != 24 {
		t.Fatalf("second call should be served from cache, blob fetches %d", f.count("quay-blob"))
	}
}

func TestFetchCommitDate_CachesAndBacksOffOnRateLimit(t *testing.T) {
	f := newFakeRegistry()
	installFakeRegistry(t, f)
	ctx := context.Background()
	sha := strings.Repeat("a", 40)
	if d := fetchCommitDate(ctx, "https://github.com/o/r", sha); d != "2026-10-01T00:00:00Z" {
		t.Fatalf("date %q", d)
	}
	fetchCommitDate(ctx, "https://github.com/o/r/", sha)
	if f.count("github") != 1 {
		t.Fatalf("commit dates are immutable per SHA; GitHub calls %d", f.count("github"))
	}

	// Rate limited: the date is skipped (no error) and further calls stop until the reset.
	f.setStatus("github", 403)
	f.githubHeaders = http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(time.Now().Add(30*time.Minute).Unix(), 10)}}
	if d := fetchCommitDate(ctx, "https://github.com/o/r", strings.Repeat("b", 40)); d != "" {
		t.Fatalf("rate-limited date %q", d)
	}
	for i := 0; i < 5; i++ {
		fetchCommitDate(ctx, "https://github.com/o/r", strings.Repeat("c", 40))
	}
	if f.count("github") != 2 {
		t.Fatalf("calls during back-off: %d, want 2 total", f.count("github"))
	}
	// Cached dates are still served during the back-off.
	if d := fetchCommitDate(ctx, "https://github.com/o/r", sha); d == "" {
		t.Fatal("cached date lost")
	}
}

func TestFetchCommitDate_UsesGitHubTokenAndRejectsUnsafeInput(t *testing.T) {
	f := newFakeRegistry()
	installFakeRegistry(t, f)
	t.Setenv("GITHUB_TOKEN", "ghp_test")
	var auth string
	quayHTTPClient.Transport = dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.github.com" {
			auth = r.Header.Get("Authorization")
		}
		return f.RoundTrip(r)
	})
	fetchCommitDate(context.Background(), "https://github.com/o/r.git", strings.Repeat("d", 40))
	if auth != "Bearer ghp_test" {
		t.Fatalf("Authorization %q", auth)
	}
	for _, tc := range []struct{ url, sha string }{
		{"https://gitlab.com/o/r", strings.Repeat("a", 40)},
		{"https://github.com/o/r", "../../etc"},
		{"https://github.com/o/r/extra", strings.Repeat("a", 40)},
		{"https://github.com/o", strings.Repeat("a", 40)},
	} {
		if d := fetchCommitDate(context.Background(), tc.url, tc.sha); d != "" {
			t.Fatalf("%v: %q", tc, d)
		}
	}
	if f.count("github") != 1 {
		t.Fatalf("unsafe inputs reached GitHub: %d calls", f.count("github"))
	}
}

func TestGitHubRateLimitReset(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	h := func(k, v string) *http.Response { return &http.Response{Header: http.Header{k: {v}}} }
	if got := githubRateLimitReset(h("Retry-After", "60"), now); !got.Equal(now.Add(time.Minute)) {
		t.Fatalf("retry-after: %v", got)
	}
	if got := githubRateLimitReset(h("X-Ratelimit-Reset", strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10)), now); !got.Equal(now.Add(10 * time.Minute)) {
		t.Fatalf("reset: %v", got)
	}
	if got := githubRateLimitReset(h("X-Ratelimit-Reset", "garbage"), now); !got.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("default: %v", got)
	}
}

func TestImageLabels_OversizedTokenAndUnknownManifest(t *testing.T) {
	f := newFakeRegistry()
	labelFixture(f, 1)
	installFakeRegistry(t, f)
	// Quay answers 400 when the bearer header is too large; the image must
	// then be read with a single-scope token instead of failing.
	quayHTTPClient.Transport = dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/manifests/") && strings.HasSuffix(r.Header.Get("Authorization"), "BIG") {
			return &http.Response{StatusCode: 400, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
		}
		return f.RoundTrip(r)
	})
	quayTokenCache.Add(quayTokenKey("a", "rhoai/img0"), "BIG", time.Hour)
	if labels, err := imageLabelsWithAuth(context.Background(), "a", "quay.io/rhoai/img0@"+digestOf(0), false); err != nil || labels.GitCommit == "" {
		t.Fatalf("labels=%+v err=%v", labels, err)
	}

	// A garbage-collected digest (404) is remembered instead of re-read on every page load.
	f.setStatus("quay-manifest", 404)
	gone := "quay.io/rhoai/rhoai-fbc-fragment:rhoai-2.17@" + digestOf(5)
	for i := 0; i < 3; i++ {
		if _, err := imageLabelsWithAuth(context.Background(), "a", gone, false); err == nil {
			t.Fatal("expected an error for an unknown manifest")
		}
	}
	if n := f.count("quay-manifest"); n != 2 {
		t.Fatalf("manifest reads %d, want 2 (1 for img0, 1 for the unknown digest)", n)
	}
}
