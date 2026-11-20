package cluster

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// argoListPath is the list of every Application, as argocd.go asks it.
const argoListPath = argoDiscoveryPath + "/applications"

// argoLists counts the Application lists f answered.
func argoLists(f *fakeArgoCD) int {
	n := 0
	for _, p := range f.requested() {
		if p == argoListPath {
			n++
		}
	}
	return n
}

// setArgoApps replaces the Applications f serves.
func setArgoApps(f *fakeArgoCD, apps ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.apps = apps
}

// setArgoFail makes f answer the Application list with code (0: answer).
func setArgoFail(f *fakeArgoCD, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = map[string]int{}
	if code != 0 {
		f.fail[argoListPath] = code
	}
}

// statusReadClient is argoClient as pkg/api's read handlers make it.
func statusReadClient(t *testing.T, f *fakeArgoCD) *Client {
	t.Helper()
	c := argoClient(t, f)
	c.SetStatusRead()
	return c
}

// dscVerdict asks about the DataScienceCluster default-dsc (unmarked).
func dscVerdict(c *Client) argoTracking {
	return newArgoCheck(c).object(nil, nil, argoDSCObject("default-dsc"))
}

// Status reads within the lifetime share one list of the Applications;
// after it they list again.
func TestArgoApplicationsStatusReadsShareOneList(t *testing.T) {
	isolateClusterCaches(t)
	clock := fakeCacheClock(t, argoApplicationsCache)
	f := argoManagingAll(argoDSCObject("default-dsc"))
	c := statusReadClient(t, f)

	if got := dscVerdict(c); got.state != types.ArgoCDManaged {
		t.Fatalf("first read: %+v", got)
	}
	// Argo CD stops listing it; a status read within the lifetime still
	// uses the list it read.
	setArgoApps(f, argoApplicationListing("openshift-gitops"))
	clock.Advance(argoApplicationsTTL - time.Second)
	if got := dscVerdict(c); got.state != types.ArgoCDManaged {
		t.Fatalf("second read within the lifetime: %+v", got)
	}
	if n := argoLists(f); n != 1 {
		t.Fatalf("two status reads within %s listed the Applications %d times, want 1", argoApplicationsTTL, n)
	}

	clock.Advance(time.Second)
	if got := dscVerdict(c); got.state != "" {
		t.Fatalf("read after the lifetime: %+v, want not managed", got)
	}
	if n := argoLists(f); n != 2 {
		t.Fatalf("a status read after %s listed %d times in all, want 2", argoApplicationsTTL, n)
	}
}

// A check that a change makes (a client pkg/api's read handlers did not
// make: the user's, or the gitops profile's Update's) lists the
// Applications itself, even right after a status read cached them, and
// does not fill the cache.
func TestArgoApplicationsChangeChecksReadFresh(t *testing.T) {
	isolateClusterCaches(t)
	fakeCacheClock(t, argoApplicationsCache)
	f := &fakeArgoCD{served: true, apps: []map[string]any{argoApplicationListing("openshift-gitops")}}
	status := statusReadClient(t, f)
	change := &Client{baseURL: status.baseURL, httpClient: status.httpClient, ctx: status.ctx}
	user := &Client{baseURL: status.baseURL, httpClient: status.httpClient, ctx: status.ctx, asUser: true}

	if got := dscVerdict(status); got.state != "" {
		t.Fatalf("status read: %+v", got)
	}
	// Argo CD now manages the DataScienceCluster: every change check sees
	// it at once.
	setArgoApps(f, argoApplicationListing("openshift-gitops", argoDSCObject("default-dsc")))
	for _, c := range []*Client{change, user} {
		if got := dscVerdict(c); got.state != types.ArgoCDManaged || got.managedBy() != testArgoApp {
			t.Fatalf("change check (as user %v) used the cached list: %+v", c.asUser, got)
		}
	}
	if n := argoLists(f); n != 3 {
		t.Fatalf("a status read and two change checks listed %d times, want 3", n)
	}
	// The change checks did not refresh the status cache either.
	if got := dscVerdict(status); got.state != "" {
		t.Fatalf("status read within the lifetime: %+v, want the list it cached", got)
	}
	if n := argoLists(f); n != 3 {
		t.Fatalf("listed %d times, want 3", n)
	}
}

// A failed list is not kept: the next status read asks again, and the
// failure still keeps the object managed (not confirmed) meanwhile.
func TestArgoApplicationsErrorsAreNotCached(t *testing.T) {
	isolateClusterCaches(t)
	fakeCacheClock(t, argoApplicationsCache)
	f := argoManagingAll()
	setArgoFail(f, http.StatusForbidden)
	c := statusReadClient(t, f)

	got := dscVerdict(c)
	if got.state != types.ArgoCDUnconfirmed || !strings.Contains(got.reason, "listing Argo CD applications failed") {
		t.Fatalf("failed list: %+v, want unconfirmed with the reason", got)
	}
	if _, cached := argoApplicationsCache.Get(c.baseURL); cached {
		t.Fatal("the failed list was cached")
	}

	setArgoFail(f, 0)
	setArgoApps(f, argoApplicationListing("openshift-gitops", argoDSCObject("default-dsc")))
	if got := dscVerdict(c); got.state != types.ArgoCDManaged {
		t.Fatalf("read after the failure: %+v, want managed", got)
	}
	if n := argoLists(f); n != 2 {
		t.Fatalf("listed %d times, want 2 (the failure was not reused)", n)
	}
}

// One status check still lists once for all its objects, and the cache
// sits under it: a second check within the lifetime lists nothing.
func TestArgoApplicationsCacheAcrossChecks(t *testing.T) {
	isolateClusterCaches(t)
	fakeCacheClock(t, argoApplicationsCache)
	f := argoManagingAll(argoGitOpsObjects...)
	c := statusReadClient(t, f)
	for range 2 {
		argo := newArgoCheck(c)
		for _, o := range argoGitOpsObjects {
			if got := argo.object(nil, nil, o); got.state != types.ArgoCDManaged {
				t.Fatalf("%s: %+v", o.kind, got)
			}
		}
	}
	if n := argoLists(f); n != 1 {
		t.Fatalf("two status checks of three objects listed %d times, want 1", n)
	}
}
