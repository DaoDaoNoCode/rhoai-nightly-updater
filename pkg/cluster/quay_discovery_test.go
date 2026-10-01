package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"
	"time"
)

func releaseRegistryClient(tags []string, pageSize int) (*http.Client, *atomic.Int32) {
	sort.Strings(tags)
	calls := &atomic.Int32{}
	return &http.Client{Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		last := r.URL.Query().Get("last")
		start := sort.Search(len(tags), func(i int) bool { return tags[i] > last })
		end := start + pageSize
		if end > len(tags) {
			end = len(tags)
		}
		body, _ := json.Marshal(quayTagsResponse{Tags: tags[start:end]})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}, calls
}

func releaseNames(tags []parsedTag) []string {
	var names []string
	for _, tag := range tags {
		names = append(names, tag.raw)
	}
	return names
}

func TestReleaseDiscoverySkipsBuildHistoryWithoutLosingVersions(t *testing.T) {
	releases := []string{"rhoai-3.7", "rhoai-3.7-ea", "rhoai-3.7-ea.2", "rhoai-13.47.2", "rhoai-13.47.2-ea.10"}
	registry := append([]string(nil), releases...)
	for _, release := range releases {
		for build := 0; build < 4000; build++ {
			registry = append(registry, fmt.Sprintf("%s-%08d-build", release, build))
		}
	}
	registry = append(registry, "unrelated-tag")
	client, calls := releaseRegistryClient(registry, 100)
	tags, err := scanReleaseTags(context.Background(), client, "test")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"rhoai-3.7-ea", "rhoai-3.7-ea.2", "rhoai-3.7", "rhoai-13.47.2-ea.10", "rhoai-13.47.2"}
	if !reflect.DeepEqual(releaseNames(tags), want) {
		t.Fatalf("releases=%v, want=%v", releaseNames(tags), want)
	}
	if calls.Load() > 20 {
		t.Fatalf("scanned historical builds: %d requests", calls.Load())
	}
	t.Logf("discovered %d releases among %d tags in %d requests (sequential scan needs %d)", len(tags), len(registry), calls.Load(), (len(registry)+99)/100)
}

func TestReleaseDiscoveryMatchesCompleteScanAtDifferentPageSizes(t *testing.T) {
	var registry, want []string
	for _, version := range []string{"3.7", "3.7.0", "3.7.1", "3.70", "8.12.5", "13.47"} {
		for _, suffix := range []string{"", "-ea", "-ea.1", "-ea.2", "-ea.10"} {
			release := "rhoai-" + version + suffix
			want = append(want, release)
			registry = append(registry, release)
			for build := 0; build < 40; build++ {
				registry = append(registry, fmt.Sprintf("%s-%03d-build", release, build))
			}
		}
	}
	registry = append(registry, "rhoai-nonversioned-build", "unrelated-tag")
	sort.Strings(want)
	for _, pageSize := range []int{1, 2, 7, 100, 1000} {
		t.Run(fmt.Sprint(pageSize), func(t *testing.T) {
			client, _ := releaseRegistryClient(registry, pageSize)
			tags, err := scanReleaseTags(context.Background(), client, "test")
			if err != nil {
				t.Fatal(err)
			}
			got := releaseNames(tags)
			sort.Strings(got)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got=%v, want=%v", got, want)
			}
		})
	}
}

func TestReleaseDiscoveryStaleCacheReturnsWhileRefreshing(t *testing.T) {
	resetReleaseTagCache(t)
	old, _ := parseTag("rhoai-3.7-ea")
	tagScanCacheMu.Lock()
	tagScanCache = []parsedTag{old}
	tagScanCacheAt = time.Now().Add(-tagScanCacheTTL - time.Minute)
	tagScanCacheMu.Unlock()
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	client := &http.Client{Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(`{"tags":["rhoai-3.7","unrelated"]}`))}, nil
	})}
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	tags, err := fetchAndParseTags(ctx, client, "test")
	if err != nil || !reflect.DeepEqual(releaseNames(tags), []string{old.raw}) {
		t.Fatalf("cached tags=%v, err=%v", tags, err)
	}
	<-started
	for i := 0; i < 5; i++ {
		if _, err := fetchAndParseTags(ctx, client, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("duplicate refreshes: %d", calls.Load())
	}
}

func TestReleaseDiscoveryColdWaitCanBeCanceled(t *testing.T) {
	resetReleaseTagCache(t)
	started, release := make(chan struct{}), make(chan struct{})
	client := &http.Client{Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(`{"tags":["rhoai-8.12","unrelated"]}`))}, nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := fetchAndParseTags(ctx, client, "test"); result <- err }()
	<-started
	cancel()
	if err := <-result; err != context.Canceled {
		t.Fatalf("err=%v, want context canceled", err)
	}
	tagScanCacheMu.RLock()
	done := tagScanRefreshDone
	tagScanCacheMu.RUnlock()
	close(release)
	<-done
	tags, err := fetchAndParseTags(context.Background(), client, "test")
	if err != nil || !reflect.DeepEqual(releaseNames(tags), []string{"rhoai-8.12"}) {
		t.Fatalf("shared refresh interrupted: tags=%v err=%v", tags, err)
	}
}

func TestReleaseDiscoveryDoesNotPublishPartialScan(t *testing.T) {
	resetReleaseTagCache(t)
	client := &http.Client{Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("last") == "rhoai-" {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(`{"tags":["rhoai-3.7"]}`))}, nil
		}
		return &http.Response{StatusCode: 503, Body: io.NopCloser(bytes.NewBufferString(`{}`))}, nil
	})}
	if tags, err := fetchAndParseTags(context.Background(), client, "test"); err == nil || tags != nil {
		t.Fatalf("partial scan accepted: tags=%v err=%v", tags, err)
	}
	tagScanCacheMu.RLock()
	defer tagScanCacheMu.RUnlock()
	if len(tagScanCache) != 0 {
		t.Fatalf("partial scan cached: %v", tagScanCache)
	}
}
