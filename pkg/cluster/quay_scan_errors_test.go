package cluster

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func tagListResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestFetchAndParseTags_RetriesTransientPageFailure(t *testing.T) {
	resetReleaseTagCache(t)
	var failures atomic.Int32
	client := &http.Client{Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("last") == "rhoai-3.5" {
			if failures.Add(1) == 1 {
				return tagListResponse(503, "unavailable"), nil
			}
			return tagListResponse(200, `{"tags":["rhoai-3.5","rhoai-3.6"]}`), nil
		}
		return tagListResponse(200, `{"tags":[]}`), nil
	})}
	tags, err := fetchAndParseTags(context.Background(), client, "t")
	if err != nil || len(tags) != 2 || tags[1].raw != "rhoai-3.6" {
		t.Fatalf("tags=%v err=%v", tags, err)
	}
	tagScanCacheMu.RLock()
	cached := len(tagScanCache)
	tagScanCacheMu.RUnlock()
	if cached != 2 {
		t.Fatalf("a scan that recovered after a retry should be cached, cache has %d", cached)
	}
}

func TestFetchAndParseTags_PartialScanIsNotCached(t *testing.T) {
	resetReleaseTagCache(t)
	var scans atomic.Int32
	client := &http.Client{Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Query().Get("last") {
		case "rhoai-":
			scans.Add(1)
			return tagListResponse(200, `{"tags":["rhoai-3.4"]}`), nil
		case "rhoai-3.4":
			return tagListResponse(200, `{"tags":[]}`), nil
		case "rhoai-3.5":
			return tagListResponse(502, "bad gateway"), nil
		}
		return tagListResponse(200, `{"tags":[]}`), nil
	})}
	for i := 0; i < 2; i++ {
		tags, err := fetchAndParseTags(context.Background(), client, "t")
		if err != nil || len(tags) != 1 {
			t.Fatalf("scan %d: tags=%v err=%v", i, tags, err)
		}
	}
	if scans.Load() != 2 {
		t.Fatalf("an incomplete scan must not be served from cache; scans=%d", scans.Load())
	}
}

func TestFetchAndParseTags_AllPagesFailingIsAnError(t *testing.T) {
	resetReleaseTagCache(t)
	client := &http.Client{Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})}
	if tags, err := fetchAndParseTags(context.Background(), client, "t"); err == nil || !strings.Contains(err.Error(), "could not list") {
		t.Fatalf("tags=%v err=%v", tags, err)
	}
}

func TestFetchAndParseTags_ClientErrorEndsOnlyThatStartPoint(t *testing.T) {
	resetReleaseTagCache(t)
	client := &http.Client{Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Query().Get("last") {
		case "rhoai-3.9":
			return tagListResponse(400, "bad cursor"), nil
		case "rhoai-3.5":
			return tagListResponse(200, `{"tags":["rhoai-3.5","rhoai.end"]}`), nil
		}
		return tagListResponse(200, `{"tags":[]}`), nil
	})}
	tags, err := fetchAndParseTags(context.Background(), client, "t")
	if err != nil || len(tags) != 1 {
		t.Fatalf("tags=%v err=%v", tags, err)
	}
	tagScanCacheMu.RLock()
	cached := len(tagScanCache)
	tagScanCacheMu.RUnlock()
	if cached != 1 {
		t.Fatal("a complete scan should be cached")
	}
}
