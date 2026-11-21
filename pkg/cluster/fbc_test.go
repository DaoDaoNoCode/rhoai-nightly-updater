package cluster

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func TestFBCCache_StoreAndRetrieve(t *testing.T) {
	registry.FBCContentCache.Purge()
	t.Cleanup(registry.FBCContentCache.Purge)

	ref := "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5@" + digestOf(1)
	key, ok := registry.FBCCacheKey(ref)
	if !ok {
		t.Fatal("digest-pinned reference must be cacheable")
	}
	registry.FBCContentCache.Add(key, &types.FBCContentResponse{Tag: "rhoai-3.5", BundleName: "rhods-operator.3.5.0"}, 0)

	got, ok := registry.CachedFBCContent(ref)
	if !ok || got.Tag != "rhoai-3.5" || got.BundleName != "rhods-operator.3.5.0" {
		t.Fatalf("cache miss or wrong entry: %+v %v", got, ok)
	}
	// The tag selects the bundle, so the same digest under another tag is another entry.
	if _, ok := registry.CachedFBCContent("quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5-ea.1@" + digestOf(1)); ok {
		t.Fatal("a different tag must not share the entry")
	}
	if _, ok := registry.FBCCacheKey("quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5"); ok {
		t.Fatal("tag-only references move and must not be cached")
	}
}

func TestFBCCache_BoundedAndKeepsRecentlyUsed(t *testing.T) {
	registry.FBCContentCache.Purge()
	t.Cleanup(registry.FBCContentCache.Purge)
	for i := range registry.FBCCacheMaxEntries {
		registry.FBCContentCache.Add(fmt.Sprintf("key-%d", i), &types.FBCContentResponse{Tag: fmt.Sprint(i)}, 0)
	}
	registry.FBCContentCache.Get("key-0") // most recently used now
	for i := range 50 {
		registry.FBCContentCache.Add(fmt.Sprintf("new-%d", i), &types.FBCContentResponse{}, 0)
	}
	if n := registry.FBCContentCache.Len(); n != registry.FBCCacheMaxEntries {
		t.Fatalf("cache size %d, want %d", n, registry.FBCCacheMaxEntries)
	}
	if _, ok := registry.FBCContentCache.Get("key-0"); !ok {
		t.Fatal("recently used entry was evicted")
	}
	if _, ok := registry.FBCContentCache.Get("key-1"); ok {
		t.Fatal("least recently used entry should be evicted")
	}
	if _, ok := registry.FBCContentCache.Get("key-51"); !ok {
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
	for range 5 {
		wg.Go(func() {
			content, err := registry.ExtractFBCContentWithAuth(context.Background(), "auth", ref)
			if err != nil || content.BundleName != "rhods-operator.3.6.0" || len(content.RelatedImages) != 1 {
				t.Errorf("content=%+v err=%v", content, err)
			}
		})
	}
	wg.Wait()
	if n := f.count("quay-layer"); n != 1 {
		t.Fatalf("expected one layer download for 5 concurrent requests, got %d", n)
	}
	if n := f.count("quay-token"); n != 1 {
		t.Fatalf("expected one token, got %d", n)
	}
	// Later requests are served from the cache.
	if _, err := registry.ExtractFBCContentWithAuth(context.Background(), "auth", ref); err != nil {
		t.Fatal(err)
	}
	if n := f.count("quay-manifest"); n != 1 {
		t.Fatalf("cached content must not refetch the manifest, got %d", n)
	}
}
