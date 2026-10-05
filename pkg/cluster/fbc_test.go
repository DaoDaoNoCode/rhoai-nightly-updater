package cluster

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func TestFBCCache_StoreAndRetrieve(t *testing.T) {
	fbcContentCache.Purge()
	t.Cleanup(fbcContentCache.Purge)

	ref := "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5@" + digestOf(1)
	key, ok := fbcCacheKey(ref)
	if !ok {
		t.Fatal("digest-pinned reference must be cacheable")
	}
	fbcContentCache.Add(key, &types.FBCContentResponse{Tag: "rhoai-3.5", BundleName: "rhods-operator.3.5.0"}, 0)

	got, ok := cachedFBCContent(ref)
	if !ok || got.Tag != "rhoai-3.5" || got.BundleName != "rhods-operator.3.5.0" {
		t.Fatalf("cache miss or wrong entry: %+v %v", got, ok)
	}
	// The tag selects the bundle, so the same digest under another tag is another entry.
	if _, ok := cachedFBCContent("quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5-ea.1@" + digestOf(1)); ok {
		t.Fatal("a different tag must not share the entry")
	}
	if _, ok := fbcCacheKey("quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5"); ok {
		t.Fatal("tag-only references move and must not be cached")
	}
}

func TestFBCCache_BoundedAndKeepsRecentlyUsed(t *testing.T) {
	fbcContentCache.Purge()
	t.Cleanup(fbcContentCache.Purge)
	for i := 0; i < fbcCacheMaxEntries; i++ {
		fbcContentCache.Add(fmt.Sprintf("key-%d", i), &types.FBCContentResponse{Tag: fmt.Sprint(i)}, 0)
	}
	fbcContentCache.Get("key-0") // most recently used now
	for i := 0; i < 50; i++ {
		fbcContentCache.Add(fmt.Sprintf("new-%d", i), &types.FBCContentResponse{}, 0)
	}
	if n := fbcContentCache.Len(); n != fbcCacheMaxEntries {
		t.Fatalf("cache size %d, want %d", n, fbcCacheMaxEntries)
	}
	if _, ok := fbcContentCache.Get("key-0"); !ok {
		t.Fatal("recently used entry was evicted")
	}
	if _, ok := fbcContentCache.Get("key-1"); ok {
		t.Fatal("least recently used entry should be evicted")
	}
	if _, ok := fbcContentCache.Get("key-51"); !ok {
		t.Fatal("only as many entries as were added beyond the bound may be evicted")
	}
}

func TestExtractFBCContent_ConcurrentRequestsDownloadOnce(t *testing.T) {
	f := newFakeRegistry()
	digest := digestOf(7)
	f.fbcLayers[digest] = gzipTar(t, map[string][]byte{
		"configs/rhods-operator/catalog.json": []byte(fbcPackage + "\n" + fbcBundleJSON("rhods-operator.3.6.0", "odh-dashboard-rhel9")),
	})
	installFakeRegistry(t, f)
	ref := "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@" + digest

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			content, err := extractFBCContentWithAuth(context.Background(), "auth", ref)
			if err != nil || content.BundleName != "rhods-operator.3.6.0" || len(content.RelatedImages) != 1 {
				t.Errorf("content=%+v err=%v", content, err)
			}
		}()
	}
	wg.Wait()
	if n := f.count("quay-layer"); n != 1 {
		t.Fatalf("expected one layer download for 5 concurrent requests, got %d", n)
	}
	if n := f.count("quay-token"); n != 1 {
		t.Fatalf("expected one token, got %d", n)
	}
	// Later requests are served from the cache.
	if _, err := extractFBCContentWithAuth(context.Background(), "auth", ref); err != nil {
		t.Fatal(err)
	}
	if n := f.count("quay-manifest"); n != 1 {
		t.Fatalf("cached content must not refetch the manifest, got %d", n)
	}
}
