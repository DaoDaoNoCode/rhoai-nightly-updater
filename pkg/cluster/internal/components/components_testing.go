package components

import (
	"strings"
	"testing"
	"time"
)

// ResetDSCDefaultsCacheForTest empties the cache of fetched DSC samples. It
// panics outside a test binary.
func ResetDSCDefaultsCacheForTest() {
	mustBeTest("ResetDSCDefaultsCacheForTest")
	dscDefaultsCache.Lock()
	dscDefaultsCache.entries = make(map[string]dscDefaultsCacheEntry)
	dscDefaultsCache.Unlock()
}

func mustBeTest(what string) {
	if !testing.Testing() {
		panic(what + " is only for tests")
	}
}

// SetWebhookNowForTest makes now the webhook scan's clock and returns the
// function that puts the previous one back. It panics outside a test
// binary.
func SetWebhookNowForTest(now func() time.Time) (restore func()) {
	mustBeTest("SetWebhookNowForTest")
	old := webhookNow
	webhookNow = now
	return func() { webhookNow = old }
}

// AgeStaleObservationsForTest moves every remembered stale observation back
// by d, standing in for the time between two scans. It panics outside a
// test binary.
func AgeStaleObservationsForTest(d time.Duration) {
	mustBeTest("AgeStaleObservationsForTest")
	staleSeen.Lock()
	defer staleSeen.Unlock()
	for k, v := range staleSeen.m {
		v.first = v.first.Add(-d)
		staleSeen.m[k] = v
	}
}

// StaleObservationsForTest counts the remembered stale observations whose
// key starts with prefix. It panics outside a test binary.
func StaleObservationsForTest(prefix string) int {
	mustBeTest("StaleObservationsForTest")
	staleSeen.Lock()
	n := 0
	for k := range staleSeen.m {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	staleSeen.Unlock()
	return n
}

// UseFreshReplicaSetOwnerCacheForTest empties the ReplicaSet owner cache and
// returns the function that empties it again, so a test neither reads nor
// leaves entries. It empties the declared cache rather than making one: a
// new LRU would register its purge function for good (NewLRU). It panics
// outside a test binary.
func UseFreshReplicaSetOwnerCacheForTest() (restore func()) {
	mustBeTest("UseFreshReplicaSetOwnerCacheForTest")
	replicaSetOwnerCache.Purge()
	return replicaSetOwnerCache.Purge
}

// SeedPackageIndexForTest caches an empty package index for the API server
// at url. It panics outside a test binary.
func SeedPackageIndexForTest(url string) {
	mustBeTest("SeedPackageIndexForTest")
	packageIndexMu.Lock()
	packageIndexCache[url] = packageIndex{}
	packageIndexMu.Unlock()
}

// PackageIndexLenForTest returns how many API servers' package indexes are
// cached. It panics outside a test binary.
func PackageIndexLenForTest() int {
	mustBeTest("PackageIndexLenForTest")
	packageIndexMu.Lock()
	n := len(packageIndexCache)
	packageIndexMu.Unlock()
	return n
}
