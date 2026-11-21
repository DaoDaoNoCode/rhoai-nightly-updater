package components

import "testing"

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
