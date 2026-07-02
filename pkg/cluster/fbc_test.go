package cluster

import (
	"fmt"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// resetFBCCache clears the global cache for test isolation.
func resetFBCCache() {
	fbcContentCacheMu.Lock()
	fbcContentCache = make(map[string]*types.FBCContentResponse)
	fbcContentCacheMu.Unlock()
}

// fillFBCCache populates the global cache with n entries keyed "key-0" .. "key-(n-1)".
func fillFBCCache(n int) {
	fbcContentCacheMu.Lock()
	for i := 0; i < n; i++ {
		fbcContentCache[fmt.Sprintf("key-%d", i)] = &types.FBCContentResponse{
			Tag:   fmt.Sprintf("tag-%d", i),
			Image: fmt.Sprintf("image-%d", i),
		}
	}
	fbcContentCacheMu.Unlock()
}

func TestFBCCache_StoreAndRetrieve(t *testing.T) {
	resetFBCCache()

	entry := &types.FBCContentResponse{
		Tag:        "rhoai-3.5",
		Image:      "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5",
		BundleName: "rhods-operator.3.5.0",
	}

	fbcContentCacheMu.Lock()
	fbcContentCache["sha256:abc123"] = entry
	fbcContentCacheMu.Unlock()

	fbcContentCacheMu.RLock()
	got, ok := fbcContentCache["sha256:abc123"]
	fbcContentCacheMu.RUnlock()

	if !ok {
		t.Fatal("expected cache hit, got miss")
	}
	if got.Tag != "rhoai-3.5" {
		t.Errorf("tag = %q, want %q", got.Tag, "rhoai-3.5")
	}
	if got.BundleName != "rhods-operator.3.5.0" {
		t.Errorf("bundleName = %q, want %q", got.BundleName, "rhods-operator.3.5.0")
	}
	if got.Image != "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5" {
		t.Errorf("image = %q, want %q", got.Image, "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5")
	}
}

func TestFBCCache_EvictRemovesAbout20Percent(t *testing.T) {
	resetFBCCache()
	fillFBCCache(fbcCacheMaxEntries) // fill to exactly max (100)

	before := len(fbcContentCache)
	if before != fbcCacheMaxEntries {
		t.Fatalf("precondition: cache has %d entries, want %d", before, fbcCacheMaxEntries)
	}

	// evictFBCCache must be called with the lock held
	fbcContentCacheMu.Lock()
	evictFBCCache()
	after := len(fbcContentCache)
	fbcContentCacheMu.Unlock()

	removed := before - after
	// Target size is 75% of max = 75, so 25 entries should be removed.
	// That is 25% of 100, which is "about 20%".
	expectedRemoved := fbcCacheMaxEntries - (fbcCacheMaxEntries * 3 / 4) // 25
	if removed != expectedRemoved {
		t.Errorf("eviction removed %d entries, want %d (from %d down to %d)",
			removed, expectedRemoved, before, after)
	}

	// Verify final size matches target (75% of max)
	expectedSize := fbcCacheMaxEntries * 3 / 4
	if after != expectedSize {
		t.Errorf("cache size after eviction = %d, want %d", after, expectedSize)
	}
}

func TestFBCCache_EvictDoesNotRemoveAll(t *testing.T) {
	resetFBCCache()
	fillFBCCache(fbcCacheMaxEntries)

	fbcContentCacheMu.Lock()
	evictFBCCache()
	after := len(fbcContentCache)
	fbcContentCacheMu.Unlock()

	if after == 0 {
		t.Fatal("eviction removed ALL entries; expected some to survive")
	}

	// At least 75% of max should survive
	minSurvivors := fbcCacheMaxEntries * 3 / 4
	if after < minSurvivors {
		t.Errorf("only %d entries survived, want at least %d", after, minSurvivors)
	}
}

func TestFBCCache_EvictNoOpWhenBelowCapacity(t *testing.T) {
	resetFBCCache()
	fillFBCCache(10) // well under the 100 max

	fbcContentCacheMu.Lock()
	before := len(fbcContentCache)
	evictFBCCache()
	after := len(fbcContentCache)
	fbcContentCacheMu.Unlock()

	if before != after {
		t.Errorf("eviction changed cache size from %d to %d; should be no-op below capacity", before, after)
	}
}

func TestFBCCache_DoesNotGrowBeyondMax(t *testing.T) {
	resetFBCCache()

	// Fill beyond max by simulating the pattern used in ExtractFBCContent:
	// check capacity, evict if needed, then insert.
	for i := 0; i < fbcCacheMaxEntries+50; i++ {
		key := fmt.Sprintf("digest-%d", i)
		entry := &types.FBCContentResponse{
			Tag:   fmt.Sprintf("tag-%d", i),
			Image: fmt.Sprintf("image-%d", i),
		}

		fbcContentCacheMu.Lock()
		if len(fbcContentCache) >= fbcCacheMaxEntries {
			evictFBCCache()
		}
		fbcContentCache[key] = entry
		fbcContentCacheMu.Unlock()
	}

	fbcContentCacheMu.RLock()
	size := len(fbcContentCache)
	fbcContentCacheMu.RUnlock()

	// After eviction + one insert, the cache should never exceed max.
	// The maximum possible is targetSize + 1 = 76 (right after eviction + insert).
	// But since we loop 50 more times after hitting max the first time,
	// the cache must stay at or below fbcCacheMaxEntries.
	if size > fbcCacheMaxEntries {
		t.Errorf("cache size %d exceeds max %d", size, fbcCacheMaxEntries)
	}
}

func TestFBCCache_EvictSurvivorsAreValid(t *testing.T) {
	resetFBCCache()
	fillFBCCache(fbcCacheMaxEntries)

	fbcContentCacheMu.Lock()
	evictFBCCache()
	// Verify all surviving entries are non-nil and have expected fields
	for key, val := range fbcContentCache {
		if val == nil {
			t.Errorf("surviving entry %q is nil", key)
			continue
		}
		if val.Tag == "" {
			t.Errorf("surviving entry %q has empty tag", key)
		}
	}
	fbcContentCacheMu.Unlock()
}
