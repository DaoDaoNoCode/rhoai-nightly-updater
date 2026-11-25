package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/clock/clocktest"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// stubMaaSStatusWithGone patches getMaaSStatus so it either lists publications
// under removal or leaves Removing empty.
func stubMaaSStatusWithGone(t *testing.T, gone bool) {
	t.Helper()
	orig := getMaaSStatus
	t.Cleanup(func() { getMaaSStatus = orig })
	getMaaSStatus = func(*cluster.Client) (*types.MaaSStatus, error) {
		st := &types.MaaSStatus{}
		if gone {
			st.Removing = []types.MaaSRemoval{{Namespace: "old", Name: "qwen3-1-7b", Reason: "project old no longer exists"}}
		}
		return st, nil
	}
}

func useFakePruneClock(t *testing.T) *clocktest.Fake {
	t.Helper()
	fake := clocktest.NewFake()
	orig := maasPruneClock
	t.Cleanup(func() { maasPruneClock = orig; resetMaaSPruneKick() })
	maasPruneClock = fake
	resetMaaSPruneKick()
	return fake
}

func readMaaSStatus(t *testing.T) {
	t.Helper()
	w := httptest.NewRecorder()
	HandleMaaSStatus(w, getAs(t, "/api/resources/maas", "user:admin"))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

// Spotting gone publications on a status request should kick off exactly one
// background prune guarded by the cluster lock and lease, without blocking the handler.
func TestMaaSStatus_GonePublicationsStartOneBackgroundCheck(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	stubMaaSStatusWithGone(t, true)
	useFakePruneClock(t)
	orig := pruneGoneMaaS
	t.Cleanup(func() { pruneGoneMaaS = orig })
	locked, release := make(chan bool, 4), make(chan struct{})
	pruneGoneMaaS = func(*cluster.Client) {
		op := inflight.snapshot()
		locked <- locks.clusterHeld() && op != nil && op.Type == "prune-maas-publications"
		<-release
	}
	// The status response must return before the prune goroutine completes.
	readMaaSStatus(t)
	if !<-locked {
		t.Fatal("the check ran without the lock or the lease")
	}
	// Overlapping status polls must not enqueue a duplicate prune.
	readMaaSStatus(t)
	close(release)
	maasPruneKicks.Wait()
	select {
	case <-locked:
		t.Fatal("a second check ran")
	default:
	}
}

// Status polls inside the debounce window must not fire prune again; once the
// window passes, the following poll should.
func TestMaaSStatus_BackgroundCheckIsDebounced(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	stubMaaSStatusWithGone(t, true)
	fake := useFakePruneClock(t)
	runs := stubMaaSPrune(t)
	readMaaSStatus(t)
	<-runs
	maasPruneKicks.Wait()
	readMaaSStatus(t)
	maasPruneKicks.Wait()
	select {
	case <-runs:
		t.Fatal("ran again within the debounce interval")
	default:
	}
	fake.Advance(maasPruneDebounce)
	readMaaSStatus(t)
	<-runs
	maasPruneKicks.Wait()
}

// An empty Removing list and the gitops profile must both suppress background prune.
func TestMaaSStatus_NoBackgroundCheckWithoutGoneOrInGitOps(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	useFakePruneClock(t)
	runs := stubMaaSPrune(t)
	stubMaaSStatusWithGone(t, false)
	readMaaSStatus(t)
	maasPruneKicks.Wait()
	select {
	case <-runs:
		t.Fatal("a read with nothing gone started a check")
	default:
	}
	activeProfile = ProfileGitOps
	if kickMaaSPrune() {
		t.Fatal("started in the gitops profile")
	}
}
