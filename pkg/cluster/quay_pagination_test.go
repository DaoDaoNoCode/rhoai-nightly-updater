package cluster

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeTagRepo answers HEAD manifests/<tag> like the registry: 200 with a
// digest for a tag it has, 404 otherwise. fail maps a tag to the statuses
// its requests get first (0 = a network error).
type fakeTagRepo struct {
	mu       sync.Mutex
	tags     map[string]bool
	fail     map[string][]int
	requests atomic.Int32
}

func newFakeTagRepo(tags ...string) *fakeTagRepo {
	f := &fakeTagRepo{tags: map[string]bool{}, fail: map[string][]int{}}
	for _, t := range tags {
		f.tags[t] = true
	}
	return f
}

func (f *fakeTagRepo) client() *http.Client {
	return &http.Client{Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		f.requests.Add(1)
		if r.Method != http.MethodHead || !strings.Contains(r.URL.Path, "/manifests/") {
			return tagListResponse(http.StatusMethodNotAllowed, "only HEAD manifests"), nil
		}
		tag := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		f.mu.Lock()
		queued, status := false, 0
		if q := f.fail[tag]; len(q) > 0 {
			queued, status, f.fail[tag] = true, q[0], q[1:]
		}
		has := f.tags[tag]
		f.mu.Unlock()
		if queued && status == 0 {
			return nil, io.ErrUnexpectedEOF
		}
		if queued {
			return tagListResponse(status, "error"), nil
		}
		if !has {
			return tagListResponse(404, "not found"), nil
		}
		resp := tagListResponse(200, "")
		resp.Header.Set("Docker-Content-Digest", "sha256:"+fmt.Sprintf("%064x", len(tag)))
		return resp, nil
	})}
}

func rawTags(tags []parsedTag) []string {
	var out []string
	for _, t := range tags {
		out = append(out, t.raw)
	}
	sort.Strings(out)
	return out
}

func TestFetchAndParseTags_ProbesEveryReleaseName(t *testing.T) {
	resetReleaseTagCache(t)
	want := []string{
		"rhoai-2.8", "rhoai-2.13", "rhoai-2.16", "rhoai-2.25",
		"rhoai-3.0", "rhoai-3.2", "rhoai-3.3", "rhoai-3.4", "rhoai-3.4-ea.1",
		"rhoai-3.5", "rhoai-3.5-ea.1", "rhoai-3.5-ea.2",
		"rhoai-3.6", "rhoai-3.6-ea", "rhoai-3.6-ea.1", "rhoai-3.6-ea.2", "rhoai-3.6.1", "rhoai-3.6.1-ea.1",
		"rhoai-3.7-ea.1", // the EA exists before the GA tag
		"rhoai-3.7-ea.3", // a gap of one is tolerated
	}
	// Per-commit and unrelated tags exist too, and are never asked about.
	f := newFakeTagRepo(append(want, "rhoai-3.6-"+strings.Repeat("a", 40), "rhoai-3.7-ea.1-"+strings.Repeat("b", 40), "latest", "rhoai.end")...)
	got, err := fetchAndParseTags(context.Background(), f.client(), "t")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(want)
	if gotRaw := rawTags(got); !reflect.DeepEqual(gotRaw, want) {
		t.Fatalf("got  %v\nwant %v", gotRaw, want)
	}
	// 2.x up to 2.25, 3.x up to 3.7, the first 24 minors of 4.x: a few
	// hundred probes, not the tens of thousands of names in the repository.
	if n := f.requests.Load(); n > 450 {
		t.Fatalf("%d probes for the scan", n)
	}
	if d, ok := tagDigestCache.Get("rhoai-3.7-ea.1"); !ok || !strings.HasPrefix(d, "sha256:") {
		t.Fatalf("a probe that found the tag should have cached its digest, got %q", d)
	}
}

func TestFetchAndParseTags_MissingMajorsAndMinorsEndTheWalk(t *testing.T) {
	resetReleaseTagCache(t)
	f := newFakeTagRepo("rhoai-3.6")
	got, err := fetchAndParseTags(context.Background(), f.client(), "t")
	if err != nil || !reflect.DeepEqual(rawTags(got), []string{"rhoai-3.6"}) {
		t.Fatalf("tags=%v err=%v", rawTags(got), err)
	}
	// Major 2: its first 24 minors, twice (GA and -ea.1), nothing. Major 3:
	// two batches, then the EAs and patches of 3.6. Major 4: 24 minors,
	// nothing, and the walk ends: majors 5 to 9 are never asked about.
	if n := f.requests.Load(); n > 160 {
		t.Fatalf("%d probes for one tag", n)
	}
}
