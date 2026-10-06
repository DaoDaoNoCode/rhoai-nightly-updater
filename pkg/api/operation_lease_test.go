package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// fakeRecord is the operation ConfigMap shared by "pods": it backs the
// record and lease seams the way pkg/cluster writes the ConfigMap (a write
// of the lease never replaces a live lease of another process).
type fakeRecord struct {
	mu        sync.Mutex
	marker    *types.OperationMarker
	last      *types.CompletedOperation
	lease     *types.OperationLease
	writes    []types.OperationLease
	releases  []cluster.LeaseOwner
	completed []*cluster.LeaseOwner
	writeErr  error
	failNext  int // the next lease writes that fail
}

func installFakeRecord(t *testing.T) *fakeRecord {
	t.Helper()
	f := &fakeRecord{}
	readOperationState = func(*cluster.Client) (*cluster.OperationRecord, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return &cluster.OperationRecord{Marker: f.marker, LastCompleted: f.last, Lease: f.lease}, nil
	}
	writeOperationLease = func(_ *cluster.Client, l *types.OperationLease, heldElsewhere func(*types.OperationLease) bool) (*types.OperationLease, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.writeErr != nil {
			return nil, f.writeErr
		}
		if f.failNext > 0 {
			f.failNext--
			return nil, errors.New("etcdserver: request timed out")
		}
		if f.lease != nil && heldElsewhere(f.lease) {
			cp := *f.lease
			return &cp, nil
		}
		cp := *l
		f.lease = &cp
		f.writes = append(f.writes, cp)
		return nil, nil
	}
	releaseOperationLease = func(_ *cluster.Client, owner cluster.LeaseOwner) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.releases = append(f.releases, owner)
		if f.lease != nil && f.lease.BootID == owner.BootID && f.lease.ID == owner.ID {
			f.lease = nil
		}
		return nil
	}
	saveOperationMarker = func(_ *cluster.Client, m *types.OperationMarker) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.marker = m
		return nil
	}
	saveCompletedOperation = func(_ *cluster.Client, done *types.CompletedOperation, owner *cluster.LeaseOwner) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.completed = append(f.completed, owner)
		f.marker, f.last = nil, done
		if owner != nil && f.lease != nil && f.lease.BootID == owner.BootID && f.lease.ID == owner.ID {
			f.lease = nil
		}
		return nil
	}
	return f
}

func (f *fakeRecord) setLease(l *types.OperationLease) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lease = l
}

func (f *fakeRecord) currentLease() *types.OperationLease {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lease == nil {
		return nil
	}
	cp := *f.lease
	return &cp
}

// otherPodLease is a lease of another updater process (another pod).
func otherPodLease(beat time.Time) *types.OperationLease {
	return &types.OperationLease{ID: "remote-op", BootID: "other-boot", Pod: "updater-old-pod", User: "alice", Type: "update",
		Label: "Update to nightly", Target: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6", StartedAt: beat.Add(-2 * time.Minute).UTC().Format(time.RFC3339),
		HeartbeatAt: beat.UTC().Format(time.RFC3339), Step: "verify_installplan", StepStatus: "running", Message: "Waiting for OLM"}
}

// blockingRefresh makes Refresh report a step and wait for release.
func blockingRefresh() (started, release chan struct{}) {
	started, release = make(chan struct{}), make(chan struct{})
	runRefreshStream = func(_ *cluster.Client, _ cluster.OperationOptions, emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		emit(cluster.UpdateStepEvent{Step: "verify_installplan", Status: "running", Message: "Waiting for OLM to install the operator..."})
		close(started)
		<-release
		return &types.OperationResponse{Success: true, Message: "refreshed"}, nil
	}
	return started, release
}

// E1: a replacement pod must not start an operation while the old pod
// still drains one, and must name that operation.
func TestLiveForeignLeaseRefusesWithTheRemoteOperation(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	f := installFakeRecord(t)
	f.setLease(otherPodLease(time.Now().Add(-5 * time.Second)))
	ran := false
	runUpdateDryRun = func(*cluster.Client, string) (*types.OperationResponse, error) {
		ran = true
		return &types.OperationResponse{Success: true}, nil
	}

	w := httptest.NewRecorder()
	HandleUpdate(w, postJSON("/api/update", "user:bob", `{"image":"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6","dryRun":true}`))
	if w.Code != 409 || ran {
		t.Fatalf("status %d ran=%v body %s", w.Code, ran, w.Body.String())
	}
	var body struct {
		Error     string    `json:"error"`
		ErrorCode string    `json:"errorCode"`
		Operation Operation `json:"operation"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ErrorCode != "cluster_busy" || !body.Operation.Remote || body.Operation.Pod != "updater-old-pod" || body.Operation.ID != "remote-op" ||
		body.Operation.Step != "verify_installplan" || !strings.Contains(body.Error, "on updater pod updater-old-pod") {
		t.Fatalf("body %s", w.Body.String())
	}
	if clusterMutationInProgress.Load() || inflight.snapshot() != nil {
		t.Fatal("the local lock is still held after the refusal")
	}
	if f.currentLease().ID != "remote-op" || len(f.releases) != 0 {
		t.Fatalf("the remote lease was touched: %+v releases %v", f.currentLease(), f.releases)
	}
	// The refusal does not use up the user's rate-limit slot.
	f.setLease(nil)
	w = httptest.NewRecorder()
	HandleUpdate(w, postJSON("/api/update", "user:bob", `{"image":"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6","dryRun":true}`))
	if w.Code != 200 || !ran {
		t.Fatalf("after the remote operation: %d %s", w.Code, w.Body.String())
	}
}

func TestLeaseHeldElsewhere(t *testing.T) {
	t.Setenv("HOSTNAME", "updater-new-pod")
	t.Setenv("DEV_MODE", "")
	now := time.Now()
	live := otherPodLease(now.Add(-10 * time.Second))
	expired := otherPodLease(now.Add(-time.Minute))
	ownProcess := otherPodLease(now)
	ownProcess.BootID = bootID
	samePodEarlierContainer := otherPodLease(now)
	samePodEarlierContainer.Pod = "updater-new-pod"
	legacy := otherPodLease(now)
	legacy.BootID = ""
	for _, tc := range []struct {
		name  string
		lease *types.OperationLease
		want  bool
	}{
		{"live lease of another pod", live, true},
		{"expired lease (taken over)", expired, false},
		{"this process", ownProcess, false},
		{"earlier container of this pod", samePodEarlierContainer, false},
		{"no boot ID", legacy, false},
		{"none", nil, false},
	} {
		if got := leaseHeldElsewhere(tc.lease); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
	// DEV_MODE processes share a host name: no shortcut there.
	t.Setenv("DEV_MODE", "true")
	if !leaseHeldElsewhere(samePodEarlierContainer) {
		t.Error("DEV_MODE: a live lease with the same host name was taken as dead")
	}
}

// TestExpiredLeaseIsTakenOver: the operation of a pod that died without
// releasing its lease does not block a new one after the TTL.
func TestExpiredLeaseIsTakenOver(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	f := installFakeRecord(t)
	f.setLease(otherPodLease(time.Now().Add(-2 * leaseTTL)))
	refreshResult(&types.OperationResponse{Success: true, Message: "refreshed"}, nil)
	w := httptest.NewRecorder()
	HandleRefreshStream(w, postJSON("/api/refresh/stream", "user:bob", "{}"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "operation_complete") {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writes) != 1 || f.writes[0].BootID != bootID || f.writes[0].User != "bob" || f.writes[0].Type != "refresh" {
		t.Fatalf("lease writes %+v", f.writes)
	}
	if len(f.completed) != 1 || f.completed[0] == nil || f.completed[0].ID != f.writes[0].ID || f.lease != nil {
		t.Fatalf("completion %+v lease %+v", f.completed, f.lease)
	}
}

// TestLeaseRenewalCarriesTheStep: while the operation runs, the heartbeat
// renews heartbeatAt with the latest step, so another pod can show it.
func TestLeaseRenewalCarriesTheStep(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	f := installFakeRecord(t)
	leaseHeartbeatInterval = 5 * time.Millisecond
	started, release := blockingRefresh()
	done := make(chan struct{})
	go func() {
		defer close(done)
		HandleRefreshStream(httptest.NewRecorder(), postJSON("/api/refresh/stream", "user:alice", "{}"))
	}()
	<-started
	first := f.currentLease()
	if first == nil || first.Step != "" {
		t.Fatalf("initial lease %+v", first)
	}
	waitFor(t, func() bool {
		l := f.currentLease()
		return l != nil && l.Step == "verify_installplan" && l.StepStatus == "running" && strings.HasPrefix(l.Message, "Waiting for OLM")
	})
	renewed := f.currentLease()
	if renewed.ID != first.ID || renewed.StartedAt != first.StartedAt || renewed.BootID != bootID {
		t.Fatalf("renewal changed the lease identity: %+v -> %+v", first, renewed)
	}
	if _, err := time.Parse(time.RFC3339, renewed.HeartbeatAt); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	if l := f.currentLease(); l != nil {
		t.Fatalf("lease kept after the operation: %+v", l)
	}
	// No renewal after the release.
	f.mu.Lock()
	n := len(f.writes)
	f.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writes) != n || f.lease != nil {
		t.Fatalf("heartbeat after release: %d -> %d writes, lease %+v", n, len(f.writes), f.lease)
	}
}

// TestDrainingProcessKeepsRenewing: a pod that was asked to stop drains its
// operation and keeps its lease alive meanwhile, so the replacement pod
// keeps refusing.
func TestDrainingProcessKeepsRenewing(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	f := installFakeRecord(t)
	leaseHeartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		mutations.mu.Lock()
		mutations.draining, mutations.idle = false, nil
		mutations.mu.Unlock()
	})
	started, release := blockingRefresh()
	go HandleRefreshStream(httptest.NewRecorder(), postJSON("/api/refresh/stream", "user:alice", "{}"))
	<-started

	drained := make(chan int)
	go func() { drained <- DrainMutations(context.Background()) }()
	waitFor(t, mutations.isDraining)
	f.mu.Lock()
	before := len(f.writes)
	f.mu.Unlock()
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.writes) >= before+3 })
	close(release)
	if running := <-drained; running != 0 {
		t.Fatalf("drain left %d running", running)
	}
	if l := f.currentLease(); l != nil {
		t.Fatalf("lease kept after the drained operation: %+v", l)
	}
}

func TestDryRunReleasesItsLease(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	f := installFakeRecord(t)
	runUpdateDryRun = func(*cluster.Client, string) (*types.OperationResponse, error) {
		if l := f.currentLease(); l == nil || l.Type != "update-dry-run" {
			t.Errorf("no lease while the dry run runs: %+v", l)
		}
		return &types.OperationResponse{Success: true, Message: "ok"}, nil
	}
	w := httptest.NewRecorder()
	HandleUpdate(w, postJSON("/api/update", "user:bob", `{"image":"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6","dryRun":true}`))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lease != nil || len(f.releases) != 1 || f.releases[0].BootID != bootID || len(f.completed) != 0 {
		t.Fatalf("lease %+v releases %+v completed %v", f.lease, f.releases, f.completed)
	}
}

// TestLeaseErrorPolicy: fail closed. A lease that cannot be read or
// written (an API error, or conflicts through every retry) refuses with 503
// lock_unavailable and Retry-After, runs nothing and frees the local lock.
func TestLeaseErrorPolicy(t *testing.T) {
	for _, leaseErr := range []error{errors.New("etcdserver: request timed out"), cluster.ErrOperationRecordContended} {
		setupDevMode(t)
		allowMutations(t)
		f := installFakeRecord(t)
		f.writeErr = leaseErr
		ran := false
		runRefreshStream = func(*cluster.Client, cluster.OperationOptions, func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
			ran = true
			return &types.OperationResponse{Success: true}, nil
		}
		w := httptest.NewRecorder()
		HandleRefreshStream(w, postJSON("/api/refresh/stream", "user:alice", "{}"))
		var body struct{ Error, ErrorCode string }
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != 503 || ran || body.ErrorCode != "lock_unavailable" || w.Header().Get("Retry-After") == "" ||
			!strings.Contains(body.Error, "Cannot verify that no other updater pod is running an operation") {
			t.Fatalf("%v: %d ran=%v %s", leaseErr, w.Code, ran, w.Body.String())
		}
		if clusterMutationInProgress.Load() || inflight.snapshot() != nil || inflight.lastCompleted() != nil {
			t.Fatalf("%v: local lock or operation kept after the refusal", leaseErr)
		}
		// The refusal does not use up the rate-limit slot: a retry runs.
		f.mu.Lock()
		f.writeErr = nil
		f.mu.Unlock()
		w = httptest.NewRecorder()
		HandleRefreshStream(w, postJSON("/api/refresh/stream", "user:alice", "{}"))
		if w.Code != 200 || !ran {
			t.Fatalf("%v: retry %d %s", leaseErr, w.Code, w.Body.String())
		}
	}
}

// TestOperationClassification: GET /api/operation tells a running remote
// operation from an interrupted one.
func TestOperationClassification(t *testing.T) {
	setupDevMode(t)
	t.Setenv("HOSTNAME", "updater-new-pod")
	now := time.Now()
	recent := now.Add(-time.Minute).UTC().Format(time.RFC3339)
	live := otherPodLease(now.Add(-5 * time.Second))
	expired := otherPodLease(now.Add(-2 * leaseTTL))
	markerOf := func(boot string) *types.OperationMarker {
		return &types.OperationMarker{ID: "remote-op", Type: "update", Label: "Update to nightly", User: "alice", StartedAt: recent, Pod: "updater-old-pod", BootID: boot}
	}
	for _, tc := range []struct {
		name        string
		marker      *types.OperationMarker
		lease       *types.OperationLease
		remote      bool
		interrupted bool
	}{
		{"running in another pod (begun)", markerOf("other-boot"), live, true, false},
		{"running in another pod (not begun yet)", nil, live, true, false},
		{"the other pod died: lease expired", markerOf("other-boot"), expired, false, true},
		{"the other pod died: no lease", markerOf("other-boot"), nil, false, true},
		{"another process holds the lease", markerOf("dead-boot"), live, true, true},
		{"legacy marker without a boot ID", &types.OperationMarker{Type: "update", StartedAt: recent}, live, true, true},
		{"legacy marker, no lease", &types.OperationMarker{Type: "update", StartedAt: recent}, nil, false, true},
		{"nothing", nil, nil, false, false},
	} {
		f := installFakeRecord(t)
		f.marker, f.lease = tc.marker, tc.lease
		status, _ := getOperation(t)
		remote := status.InProgress && status.Operation != nil && status.Operation.Remote
		if remote != tc.remote || (status.Interrupted != nil) != tc.interrupted {
			t.Errorf("%s: inProgress %v operation %+v interrupted %+v", tc.name, status.InProgress, status.Operation, status.Interrupted)
			continue
		}
		if remote && (status.Operation.Pod != "updater-old-pod" || status.Operation.Step != "verify_installplan" || status.Operation.User != "alice") {
			t.Errorf("%s: operation %+v", tc.name, status.Operation)
		}
	}
}

// TestLastCompletedFreshness: a pod that remembers an older lastCompleted
// reports the newer persisted one (finished in another pod), and keeps its
// own when that is newer.
func TestLastCompletedFreshness(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	f := installFakeRecord(t)
	older := &types.CompletedOperation{ID: "older", FinishedAt: "2020-01-01T10:00:00Z", Success: true}
	newer := &types.CompletedOperation{ID: "newer", FinishedAt: "2020-01-01T10:05:00Z", Success: true}
	tie := &types.CompletedOperation{ID: "tie", FinishedAt: "2020-01-01T10:05:00Z", Success: false}

	f.last = older
	if status, _ := getOperation(t); status.LastCompleted == nil || status.LastCompleted.ID != "older" {
		t.Fatalf("first read: %+v", status.LastCompleted)
	}
	f.mu.Lock()
	f.last = newer
	f.mu.Unlock()
	if status, _ := getOperation(t); status.LastCompleted == nil || status.LastCompleted.ID != "newer" {
		t.Fatalf("cached value not refreshed: %+v", status.LastCompleted)
	}
	f.mu.Lock()
	f.last = tie
	f.mu.Unlock()
	if status, _ := getOperation(t); status.LastCompleted == nil || status.LastCompleted.ID != "tie" {
		t.Fatalf("a tie goes to the persisted value: %+v", status.LastCompleted)
	}
	// This process finishes one now, but its write is lost: it keeps its own.
	saveCompletedOperation = func(*cluster.Client, *types.CompletedOperation, *cluster.LeaseOwner) error {
		return errors.New("unavailable")
	}
	refreshResult(&types.OperationResponse{Success: true, Message: "refreshed"}, nil)
	HandleRefreshStream(httptest.NewRecorder(), postJSON("/api/refresh/stream", "user:carol", "{}"))
	if status, _ := getOperation(t); status.LastCompleted == nil || status.LastCompleted.User != "carol" {
		t.Fatalf("own newer operation not preferred: %+v", status.LastCompleted)
	}
	f.mu.Lock()
	f.last = nil
	f.mu.Unlock()
	if status, _ := getOperation(t); status.LastCompleted == nil || status.LastCompleted.User != "carol" {
		t.Fatalf("nothing persisted: %+v", status.LastCompleted)
	}
}

func TestFinishedBefore(t *testing.T) {
	a := &types.CompletedOperation{FinishedAt: "2026-10-06T10:00:00Z"}
	b := &types.CompletedOperation{FinishedAt: "2026-10-06T10:00:01Z"}
	bad := &types.CompletedOperation{FinishedAt: "?"}
	if !finishedBefore(a, b) || finishedBefore(b, a) || finishedBefore(a, a) || !finishedBefore(bad, a) || finishedBefore(a, bad) || finishedBefore(bad, bad) {
		t.Fatal("finishedBefore")
	}
}

// The release of an operation that never began runs where the marker
// clear would, inside the shutdown budget.
func TestLeaseTimingFitsTheBudget(t *testing.T) {
	if leaseWriteTimeout > cluster.MarkerClearTimeout {
		t.Fatalf("lease write %s > marker clear %s", leaseWriteTimeout, cluster.MarkerClearTimeout)
	}
	if leaseTTL < 3*leaseHeartbeatInterval+leaseWriteTimeout {
		t.Fatalf("TTL %s does not survive three missed renewals every %s", leaseTTL, leaseHeartbeatInterval)
	}
}
