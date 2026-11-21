package kube

// RegisteredCachesForTest returns how many purge functions the cache
// registry holds (RegisterCachePurge, NewLRU). It panics outside a test
// binary.
func RegisteredCachesForTest() int {
	mustBeTest("RegisteredCachesForTest")
	cacheRegistry.Lock()
	defer cacheRegistry.Unlock()
	return len(cacheRegistry.purges)
}
