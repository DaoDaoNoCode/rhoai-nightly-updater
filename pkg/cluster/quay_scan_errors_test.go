package cluster

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
)

func tagListResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestFetchAndParseTags_RetriesTransientProbeFailure(t *testing.T) {
	resetReleaseTagCache(t)
	f := newFakeTagRepo("rhoai-3.5", "rhoai-3.6")
	f.fail["rhoai-3.6"] = []int{503} // once, then fine
	tags, err := registry.FetchAndParseTags(context.Background(), f.client(), "t")
	if err != nil || len(tags) != 2 || tags[1].Raw != "rhoai-3.6" {
		t.Fatalf("tags=%v err=%v", rawTags(tags), err)
	}
	cached := registry.TagScanLenForTest()
	if cached != 2 {
		t.Fatalf("a scan that recovered after a retry should be cached, cache has %d", cached)
	}
}

func TestFetchAndParseTags_PartialScanIsNotCached(t *testing.T) {
	resetReleaseTagCache(t)
	f := newFakeTagRepo("rhoai-3.4", "rhoai-3.5")
	// The probe of 3.5 fails for good (the retry too), on both scans.
	f.fail["rhoai-3.5"] = []int{502, 502, 502, 502}
	for i := range 2 {
		tags, err := registry.FetchAndParseTags(context.Background(), f.client(), "t")
		if err != nil || len(tags) != 1 || tags[0].Raw != "rhoai-3.4" {
			t.Fatalf("scan %d: tags=%v err=%v", i, rawTags(tags), err)
		}
	}
	cached := registry.TagScanLenForTest()
	if cached != 0 {
		t.Fatalf("an incomplete scan must not be cached; cache has %d", cached)
	}
}

func TestFetchAndParseTags_EveryProbeFailingIsAnError(t *testing.T) {
	resetReleaseTagCache(t)
	client := &http.Client{Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})}
	if tags, err := registry.FetchAndParseTags(context.Background(), client, "t"); err == nil || !strings.Contains(err.Error(), "could not list") {
		t.Fatalf("tags=%v err=%v", rawTags(tags), err)
	}
}

func TestFetchAndParseTags_RejectedTokenIsAnError(t *testing.T) {
	resetReleaseTagCache(t)
	client := &http.Client{Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		return tagListResponse(401, "unauthorized"), nil
	})}
	if tags, err := registry.FetchAndParseTags(context.Background(), client, "t"); err == nil || len(tags) != 0 {
		t.Fatalf("tags=%v err=%v", rawTags(tags), err)
	}
}
