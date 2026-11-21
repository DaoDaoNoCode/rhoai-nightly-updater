package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resetClusterCaches empties the package's caches keyed by an API server
// URL. In production one URL is one cluster, so sharing them is right. In
// tests every fake API server is https://127.0.0.1:<port> and the OS reuses
// ports, so a later test's fake cluster could read what an earlier one
// cached: a stale 15-minute catalog index made
// TestDSCCheck_OperandMessage_OperatorNotInstalled fail on a CI runner. The
// shared fake constructors call it when a fake cluster starts and ends.
func resetClusterCaches() {
	apiVersionsCache.Purge()
	controlPlaneCache.Purge()
	replicaSetOwnerCache.Purge()
	capacityCache.Purge()
	planCache.Purge()
	stableTargetCache.Purge()
	consoleURLCache.Purge()
	dashboardURLCache.Purge()
	rhoaiNamespacesCache.Purge()
	dashboardTargetCache.Purge()
	argoApplicationsCache.Purge()
	packageIndexMu.Lock()
	packageIndexCache = map[string]packageIndex{}
	packageIndexMu.Unlock()
	staleSeen.Lock()
	staleSeen.m = map[string]staleObservation{}
	staleSeen.Unlock()
	callChecks.Lock()
	for k := range callChecks.m {
		delete(callChecks.m, k)
	}
	callChecks.Unlock()
}

// isolateClusterCaches gives a test's fake cluster empty URL-keyed caches.
func isolateClusterCaches(t *testing.T) {
	t.Helper()
	resetClusterCaches()
	t.Cleanup(resetClusterCaches)
}

// Every place the package keys state by the API server URL, per file. A new
// one must be emptied by resetClusterCaches too; then update this list.
func TestResetClusterCachesCoversEveryURLKeyedCache(t *testing.T) {
	want := map[string]int{
		"api_versions.go":                 1, // apiVersionsCache
		"argocd.go":                       1, // argoApplicationsCache
		"components.go":                   1, // replicaSetOwnerCache
		"control_plane.go":                2, // controlPlaneCache
		"dashboard_operator_discovery.go": 2, // dashboardTargetCache
		"diagnostics_dsc.go":              1, // staleSeen
		"maas.go":                         1, // apiVersionsCache (read)
		"models_call.go":                  1, // callChecks
		"models_image.go":                 2, // capacityCache, planCache
		"prerequisites.go":                2, // packageIndexCache
		"rhoai_namespaces.go":             1, // rhoaiNamespacesCache
		"status.go":                       5, // stableTargetCache, consoleURLCache, dashboardURLCache
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || requestSourceFiles[f] {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for line := range strings.SplitSeq(string(b), "\n") {
			if strings.Contains(line, "c.baseURL") && !strings.Contains(line, "return user, c.baseURL") {
				got[f]++
			}
		}
	}
	for f, n := range got {
		if want[f] != n {
			t.Errorf("%s keys state by c.baseURL %d times, the list says %d: add any new cache to resetClusterCaches, then update this list", f, n, want[f])
		}
	}
	for f, n := range want {
		if got[f] == 0 {
			t.Errorf("%s no longer uses c.baseURL (the list says %d): update resetClusterCaches and this list", f, n)
		}
	}
}

// A fake cluster starts with nothing an earlier fake cluster cached, even
// under the same URL (a reused port).
func TestFakeClusterStartsWithEmptyURLKeyedCaches(t *testing.T) {
	const earlier = "https://127.0.0.1:1"
	packageIndexMu.Lock()
	packageIndexCache[earlier] = packageIndex{}
	packageIndexMu.Unlock()
	apiVersionsCache.Add(earlier+"|kuadrant.io", []string{"v1"}, 0)
	newFakeAPI(t)
	packageIndexMu.Lock()
	n := len(packageIndexCache)
	packageIndexMu.Unlock()
	if _, ok := apiVersionsCache.Get(earlier + "|kuadrant.io"); ok || n != 0 {
		t.Fatalf("a new fake cluster still sees an earlier one's caches: package index %d entries, api versions cached %v", n, ok)
	}
}
