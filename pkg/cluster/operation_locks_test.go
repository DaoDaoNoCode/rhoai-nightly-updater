package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// invalidStatus is the whole answer of the API server to a JSON patch whose
// test operation failed, as kube-apiserver sends it (checked live on
// OpenShift 2026-11-20): a generic 422 that names neither the operation
// nor the path.
const invalidStatus = `{"kind":"Status","status":"Failure","message":"the server rejected our request due to an error in our request","reason":"Invalid","details":{},"code":422}`

// jsonPatch applies a JSON patch (RFC 6902) to the ConfigMap as the API
// server does: whole or not at all, without a resourceVersion check, and a
// failed test (or any other failed operation) answers invalidStatus.
func (f *fakeOperationConfigMap) jsonPatch(w http.ResponseWriter, r *http.Request) {
	if f.beforeWrite != nil {
		f.beforeWrite(f)
	}
	if f.data == nil {
		f.status(w, 404, "NotFound")
		return
	}
	var ops []kube.JSONPatchOp
	if err := json.NewDecoder(r.Body).Decode(&ops); err != nil {
		f.status(w, 400, "BadRequest")
		return
	}
	data := map[string]any{}
	for k, v := range f.data {
		data[k] = v
	}
	out, err := kube.ApplyJSONPatch(map[string]any{"data": data}, ops)
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		io.WriteString(w, invalidStatus)
		return
	}
	next := map[string]string{}
	for k, v := range out["data"].(map[string]any) {
		next[k] = v.(string)
	}
	f.data = next
	f.rv++
	w.Write([]byte(`{}`))
}

func leaseValue(t *testing.T, l *types.OperationLease) string {
	t.Helper()
	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func projectLease(id, boot, uid, user string, beat time.Time) *types.OperationLease {
	l := testLease(id, boot, beat)
	l.User, l.Type, l.Label = user, "workbench", "Workbench"
	l.Project, l.ProjectUID = "team-"+uid, uid
	return l
}

func noCheck(*OperationRecord) error { return nil }

// Two pods hold a lock each (the cluster lock and a project lock), and
// other keys are written in between: each renewal is one JSON patch that
// tests and replaces its own entry only, carries no resourceVersion, and so
// never conflicts with the other writes, however many came in between.
func TestRenewalsDoNotConflictAcrossKeys(t *testing.T) {
	now := kube.Clock().Now()
	f, c := newFakeOperationConfigMap(t)
	a := testLease("op-a", "boot-a", now)
	b := projectLease("op-b", "boot-b", "uid-1", "bob", now)
	if err := AcquireOperationLock(c, ClusterLockKey, leaseValue(t, a), noCheck); err != nil {
		t.Fatal(err)
	}
	if err := AcquireOperationLock(c, ProjectLockKey("uid-1"), leaseValue(t, b), noCheck); err != nil {
		t.Fatal(err)
	}
	oldA, oldB := leaseValue(t, a), leaseValue(t, b)
	for i := range 3 {
		beat := now.Add(time.Duration(i+1) * 10 * time.Second)
		a.HeartbeatAt, b.HeartbeatAt = beat.UTC().Format(time.RFC3339), beat.UTC().Format(time.RFC3339)
		// Another key changes between the renewals (a recipe run record).
		f.set(record.RecipeRunKey, map[string]any{"id": strconv.Itoa(i)})
		lost, err := RenewOperationLocks(c, []LockRenewal{{Key: ClusterLockKey, Old: oldA, New: leaseValue(t, a)}})
		if err != nil || lost != nil {
			t.Fatalf("pod a, renewal %d: lost %v err %v", i, lost, err)
		}
		f.set(record.LastCompletedKey, &types.CompletedOperation{ID: strconv.Itoa(i)})
		lost, err = RenewOperationLocks(c, []LockRenewal{{Key: ProjectLockKey("uid-1"), Old: oldB, New: leaseValue(t, b)}})
		if err != nil || lost != nil {
			t.Fatalf("pod b, renewal %d: lost %v err %v", i, lost, err)
		}
		oldA, oldB = leaseValue(t, a), leaseValue(t, b)
	}
	if f.conflicts != 0 {
		t.Fatalf("%d writes conflicted", f.conflicts)
	}
	rec, err := ReadOperationRecord(c)
	if err != nil || rec.Value(ClusterLockKey) != oldA || rec.Value(ProjectLockKey("uid-1")) != oldB {
		t.Fatalf("record %+v err %v", rec, err)
	}
	// One request per renewal, no read before it: the two acquires made
	// the first four, the final read is the last.
	if r := f.reqs(); fmt.Sprint(r[4:len(r)-1]) != "[PATCH PATCH PATCH PATCH PATCH PATCH]" {
		t.Fatalf("requests %v", r)
	}
}

// A renewal of two entries, one of which another pod took over: the API
// server answers the whole patch with its generic 422 (nothing applied).
// The holder reads the record again, reports that entry lost with the
// lease recorded there now, and renews the other one in a second patch.
func TestFailedTestOpReturns422AndRereads(t *testing.T) {
	now := kube.Clock().Now()
	f, c := newFakeOperationConfigMap(t)
	mine := testLease("op-a", "boot-a", now)
	proj := projectLease("op-p", "boot-a", "uid-1", "alice", now)
	f.set(ClusterLockKey, mine)
	theirs := projectLease("op-x", "boot-b", "uid-1", "bob", now)
	f.set(ProjectLockKey("uid-1"), theirs) // taken over
	f.mu.Lock()
	f.requests = nil
	f.mu.Unlock()

	renewed, renewedProj := *mine, *proj
	renewed.HeartbeatAt = now.Add(10 * time.Second).UTC().Format(time.RFC3339)
	renewedProj.HeartbeatAt = renewed.HeartbeatAt
	lost, err := RenewOperationLocks(c, []LockRenewal{
		{Key: ClusterLockKey, Old: leaseValue(t, mine), New: leaseValue(t, &renewed)},
		{Key: ProjectLockKey("uid-1"), Old: leaseValue(t, proj), New: leaseValue(t, &renewedProj)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lost) != 1 || lost[ProjectLockKey("uid-1")] == nil || lost[ProjectLockKey("uid-1")].ID != "op-x" {
		t.Fatalf("lost %+v", lost)
	}
	if got := f.get(ClusterLockKey); got != leaseValue(t, &renewed) {
		t.Fatalf("cluster lock not renewed: %s", got)
	}
	if got := f.get(ProjectLockKey("uid-1")); got != leaseValue(t, theirs) {
		t.Fatalf("the other pod's entry was touched: %s", got)
	}
	if want := []string{"PATCH", "GET", "PATCH"}; fmt.Sprint(f.reqs()) != fmt.Sprint(want) {
		t.Fatalf("requests %v, want %v", f.reqs(), want)
	}

	// The entry was removed (the ConfigMap edited by hand): lost, with no
	// holder.
	f.mu.Lock()
	delete(f.data, ClusterLockKey)
	f.mu.Unlock()
	lost, err = RenewOperationLocks(c, []LockRenewal{{Key: ClusterLockKey, Old: leaseValue(t, &renewed), New: leaseValue(t, mine)}})
	if err != nil || len(lost) != 1 || lost[ClusterLockKey] != nil {
		t.Fatalf("removed entry: lost %+v err %v", lost, err)
	}
}

// A 422 after which every entry still holds what the patch tested is not a
// failed test: it is the API server's own refusal (a webhook, a size
// limit). Nothing is lost; the renewal failed and says why.
func TestRenewalRefusedWithoutAFailedTestIsAnError(t *testing.T) {
	now := kube.Clock().Now()
	f, _ := newFakeOperationConfigMap(t)
	mine := testLease("op-a", "boot-a", now)
	f.set(ClusterLockKey, mine)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			w.WriteHeader(http.StatusUnprocessableEntity)
			io.WriteString(w, invalidStatus)
			return
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(testClientOptions{BaseURL: srv.URL, HTTPClient: srv.Client(), Ctx: t.Context()})
	renewed := *mine
	renewed.HeartbeatAt = now.Add(10 * time.Second).UTC().Format(time.RFC3339)
	lost, err := RenewOperationLocks(c, []LockRenewal{{Key: ClusterLockKey, Old: leaseValue(t, mine), New: leaseValue(t, &renewed)}})
	if lost != nil || err == nil || !kube.IsK8sError(err, http.StatusUnprocessableEntity) {
		t.Fatalf("lost %+v err %v", lost, err)
	}
	if got := f.get(ClusterLockKey); got != leaseValue(t, mine) {
		t.Fatalf("entry changed: %s", got)
	}
}

// Every conditional write removes the project locks that expired: those
// that never began at once, interrupted ones (expired after they began)
// after InterruptedLockMaxAge from their start. Live ones, the cluster
// lock (released to "" by earlier versions too) and the other keys stay.
// Writes that do not read the record first (the renewals, the hard reset
// record) remove nothing.
func TestExpiredLockKeysArePrunedOnWrite(t *testing.T) {
	now := kube.Clock().Now()
	f, c := newFakeOperationConfigMap(t)
	expired := now.Add(-2 * LockTTL)
	live := projectLease("op-live", "boot-b", "live", "bob", now)
	stale := projectLease("op-stale", "boot-b", "stale", "carol", expired)
	interrupted := projectLease("op-int", "boot-b", "int", "dave", expired)
	interrupted.Begun = true
	interrupted.StartedAt = now.Add(-time.Hour).UTC().Format(time.RFC3339)
	old := projectLease("op-old", "boot-b", "old", "erin", now.Add(-25*time.Hour))
	old.Begun = true
	old.StartedAt = now.Add(-26 * time.Hour).UTC().Format(time.RFC3339)
	f.set(ProjectLockKey("live"), live)
	f.set(ProjectLockKey("stale"), stale)
	f.set(ProjectLockKey("int"), interrupted)
	f.set(ProjectLockKey("old"), old)
	f.set(ClusterLockKey, testLease("op-c", "boot-c", expired))
	f.mu.Lock()
	f.data[ProjectLockKey("garbage")] = "{not json"
	f.mu.Unlock()

	// Blind writes and renewals leave them.
	if err := record.SaveDSCHardResetRecord(c, &types.DSCHardResetRecord{Name: "d", Object: "{}", APIVersion: "v1", OperatorCSV: "csv"}); err != nil {
		t.Fatal(err)
	}
	renewed := *live
	if _, err := RenewOperationLocks(c, []LockRenewal{{Key: ProjectLockKey("live"), Old: leaseValue(t, live), New: leaseValue(t, &renewed)}}); err != nil {
		t.Fatal(err)
	}
	if f.get(ProjectLockKey("stale")) == "" || f.get(ProjectLockKey("garbage")) == "" {
		t.Fatal("a write that did not read the record removed an entry")
	}

	// A conditional write of another key (the recipe run) prunes.
	if err := SaveRecipeRun(c, &types.RecipeRun{ID: "run", OperationID: "op", BootID: "boot-a"}, true); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	keys := map[string]bool{}
	for k := range f.data {
		keys[k] = true
	}
	f.mu.Unlock()
	for _, k := range []string{ProjectLockKey("live"), ProjectLockKey("int"), ClusterLockKey, record.RecipeRunKey, record.DSCHardResetKey} {
		if !keys[k] {
			t.Errorf("%s was removed", k)
		}
	}
	for _, k := range []string{ProjectLockKey("stale"), ProjectLockKey("old"), ProjectLockKey("garbage")} {
		if keys[k] {
			t.Errorf("%s was kept", k)
		}
	}
	rec, err := ReadOperationRecord(c)
	if err != nil || len(rec.ProjectLocks) != 2 || rec.ProjectLocks["int"] == nil || !rec.ProjectLocks["int"].Begun || rec.ProjectLocks["live"] == nil {
		t.Fatalf("project locks %+v err %v", rec.ProjectLocks, err)
	}
}

// A record an earlier version wrote: its cluster lock (no project fields)
// and its marker. Both read as before; a project lock is taken next to the
// live one (its check sees it), and the cluster lock is released to "",
// the form earlier versions read as free.
func TestOldFormatLockStillRead(t *testing.T) {
	now := kube.Clock().Now()
	f, c := newFakeOperationConfigMap(t)
	beat := now.UTC().Format(time.RFC3339)
	legacy := `{"id":"op-old","bootId":"boot-old","pod":"updater-old","user":"alice","type":"update","label":"Update to nightly","target":"quay.io/x:y","startedAt":"` + beat + `","heartbeatAt":"` + beat + `","step":"verify_installplan","stepStatus":"running","message":"Waiting"}`
	f.mu.Lock()
	f.data = map[string]string{ClusterLockKey: legacy, record.OperationKey: `{"type":"update","label":"Update to nightly","user":"alice","startedAt":"` + beat + `","id":"op-old","bootId":"boot-old"}`}
	f.rv = 7
	f.mu.Unlock()
	rec, err := ReadOperationRecord(c)
	if err != nil || rec.Lease == nil || rec.Lease.ID != "op-old" || rec.Lease.Step != "verify_installplan" || rec.Lease.ProjectUID != "" || len(rec.ProjectLocks) != 0 {
		t.Fatalf("record %+v err %v", rec, err)
	}
	if !LeaseLive(rec.Lease, now, LockTTL) {
		t.Fatal("the old lock is not live")
	}
	// The cluster lock's format is the old one: the same lease encodes to
	// the same bytes.
	if got := leaseValue(t, rec.Lease); got != legacy {
		t.Fatalf("the cluster lock's format changed:\n got %s\nwant %s", got, legacy)
	}
	// A check sees it (pkg/api refuses a project lock while it is live).
	saw := false
	err = AcquireOperationLock(c, ProjectLockKey("uid-1"), leaseValue(t, projectLease("op-p", "boot-a", "uid-1", "bob", now)), func(rec *OperationRecord) error {
		saw = rec.Lease != nil && rec.Lease.ID == "op-old"
		return nil
	})
	if err != nil || !saw || f.get(ClusterLockKey) != legacy {
		t.Fatalf("err %v saw %t lock %s", err, saw, f.get(ClusterLockKey))
	}
	// Released by its owner, the cluster lock is "" as earlier versions
	// write it.
	if err := ReleaseOperationLock(c, ClusterLockKey, LeaseOwner{BootID: "boot-old", ID: "op-old"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	v, ok := f.data[ClusterLockKey]
	f.mu.Unlock()
	if !ok || v != "" {
		t.Fatalf("cluster lock after release: %q (present %t)", v, ok)
	}
	// A project lock is removed when released.
	if err := ReleaseOperationLock(c, ProjectLockKey("uid-1"), LeaseOwner{BootID: "boot-a", ID: "op-p"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	_, ok = f.data[ProjectLockKey("uid-1")]
	f.mu.Unlock()
	if ok {
		t.Fatal("the released project lock is still recorded")
	}
}

// Writers of one process run one at a time (one writer per ConfigMap per
// pod): conditional writes of the operation record and activity entries
// that all start at the same instant never conflict with each other. The
// fake holds the first read until every writer has started, so writers
// that were not serialized would all read the same version, and all but
// one would conflict.
func TestOnePodNeverConflictsWithItself(t *testing.T) {
	t.Run("operation record", func(t *testing.T) {
		now := kube.Clock().Now()
		f, _ := newFakeOperationConfigMap(t)
		f.set(record.OperationKey, &types.OperationMarker{ID: "op0", BootID: "boot-a"})
		f.set(ClusterLockKey, testLease("op0", "boot-a", now))
		const writers = 6
		var started atomic.Int32
		allStarted := make(chan struct{})
		var gate sync.Once
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				gate.Do(func() { <-allStarted })
			}
			f.ServeHTTP(w, r)
		}))
		t.Cleanup(srv.Close)
		c := newTestClient(testClientOptions{BaseURL: srv.URL, HTTPClient: srv.Client(), Ctx: t.Context()})
		var wg sync.WaitGroup
		errs := make(chan error, writers)
		for i := range writers {
			wg.Go(func() {
				if started.Add(1) == writers {
					close(allStarted)
				}
				switch i {
				case 0:
					errs <- SaveCompletedOperation(c, &types.CompletedOperation{ID: "op0", FinishedAt: now.UTC().Format(time.RFC3339)}, &LeaseOwner{BootID: "boot-a", ID: "op0"})
				case 1:
					errs <- SaveRecipeRun(c, &types.RecipeRun{ID: "run"}, true)
				default:
					uid := "uid-" + strconv.Itoa(i)
					errs <- AcquireOperationLock(c, ProjectLockKey(uid), leaseValue(t, projectLease("op"+uid, "boot-a", uid, "user"+uid, now)), noCheck)
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if f.conflicts != 0 {
			t.Fatalf("%d writes of this process conflicted", f.conflicts)
		}
		rec, err := ReadOperationRecord(c)
		if err != nil || len(rec.ProjectLocks) != writers-2 || rec.Lease != nil || rec.Marker != nil || rec.Value(record.RecipeRunKey) == "" {
			t.Fatalf("record %+v err %v", rec, err)
		}
	})

	t.Run("activity", func(t *testing.T) {
		t.Setenv("NAMESPACE", "test-ns")
		var mu sync.Mutex
		var entries []types.ActivityEntry
		rv, conflicts := 0, 0
		const writers = 5
		var started atomic.Int32
		allStarted := make(chan struct{})
		var gate sync.Once
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				gate.Do(func() { <-allStarted })
			}
			mu.Lock()
			defer mu.Unlock()
			switch r.Method {
			case http.MethodGet:
				if rv == 0 {
					w.WriteHeader(404)
					io.WriteString(w, `{"kind":"Status","status":"Failure","reason":"NotFound","code":404}`)
					return
				}
				raw, _ := json.Marshal(entries)
				out, _ := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": strconv.Itoa(rv)}, "data": map[string]string{"entries": string(raw)}})
				w.Write(out)
			case http.MethodPost, http.MethodPut:
				var cm struct {
					Metadata struct {
						ResourceVersion string `json:"resourceVersion"`
					} `json:"metadata"`
					Data map[string]string `json:"data"`
				}
				json.NewDecoder(r.Body).Decode(&cm)
				if (r.Method == http.MethodPost && rv != 0) || (r.Method == http.MethodPut && cm.Metadata.ResourceVersion != strconv.Itoa(rv)) {
					conflicts++
					w.WriteHeader(409)
					io.WriteString(w, `{"kind":"Status","status":"Failure","reason":"Conflict","code":409}`)
					return
				}
				json.Unmarshal([]byte(cm.Data["entries"]), &entries)
				rv++
				w.Write([]byte(`{}`))
			}
		}))
		t.Cleanup(srv.Close)
		c := newTestClient(testClientOptions{BaseURL: srv.URL, HTTPClient: srv.Client(), Ctx: t.Context()})
		var wg sync.WaitGroup
		for i := range writers {
			wg.Go(func() {
				if started.Add(1) == writers {
					close(allStarted)
				}
				record.Activity(c, types.ActivityEntry{Timestamp: "2026-11-25T10:00:00Z", User: "u", Action: "update", Detail: strconv.Itoa(i), Success: true})
			})
		}
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		if conflicts != 0 || len(entries) != writers {
			t.Fatalf("%d conflicts, %d entries", conflicts, len(entries))
		}
	})
}

// Conflicts with another pod are retried from a fresh read after a
// jittered backoff, up to eight attempts in all.
func TestAcquireRetriesConflictsEightTimesWithBackoff(t *testing.T) {
	f, c := newFakeOperationConfigMap(t)
	f.set(record.OperationKey, &types.OperationMarker{ID: "op0"})
	var waits []int
	t.Cleanup(record.SetLockBackoffForTest(func(attempt int) time.Duration {
		waits = append(waits, attempt)
		return time.Millisecond
	}))
	f.beforeWrite = func(f *fakeOperationConfigMap) { f.rv++ } // another pod writes first, every time
	checks := 0
	err := AcquireOperationLock(c, ClusterLockKey, `{"id":"op1"}`, func(*OperationRecord) error { checks++; return nil })
	if !errors.Is(err, ErrOperationRecordContended) {
		t.Fatalf("err %v", err)
	}
	if checks != 8 || f.conflicts != 8 || fmt.Sprint(waits) != "[1 2 3 4 5 6 7]" {
		t.Fatalf("checks %d conflicts %d backoffs %v", checks, f.conflicts, waits)
	}
}

// Interrupted project actions are kept for a day, but only the
// MaxInterruptedLocks newest: many projects interrupted within a day
// cannot fill the ConfigMap.
func TestInterruptedLocksAreBounded(t *testing.T) {
	now := kube.Clock().Now()
	f, c := newFakeOperationConfigMap(t)
	n := record.MaxInterruptedLocks + 5
	for i := range n {
		uid := "uid-" + strconv.Itoa(i)
		l := projectLease("op-"+uid, "boot-b", uid, "user"+uid, now.Add(-2*LockTTL))
		l.Begun = true
		l.StartedAt = now.Add(-time.Duration(i+1) * time.Minute).UTC().Format(time.RFC3339) // uid-0 newest
		f.set(ProjectLockKey(uid), l)
	}
	if err := SaveRecipeRun(c, &types.RecipeRun{ID: "run"}, true); err != nil {
		t.Fatal(err)
	}
	rec, err := ReadOperationRecord(c)
	if err != nil || len(rec.ProjectLocks) != record.MaxInterruptedLocks {
		t.Fatalf("%d kept, err %v", len(rec.ProjectLocks), err)
	}
	for i := range n {
		_, kept := rec.ProjectLocks["uid-"+strconv.Itoa(i)]
		if kept != (i < record.MaxInterruptedLocks) {
			t.Errorf("uid-%d kept %t", i, kept)
		}
	}
}

// A renewal does not wait for this process's other writes of the record:
// while a recipe run write hangs, the lease is renewed.
func TestRenewalDoesNotWaitForTheWriter(t *testing.T) {
	now := kube.Clock().Now()
	f, _ := newFakeOperationConfigMap(t)
	mine := testLease("op-a", "boot-a", now)
	f.set(ClusterLockKey, mine)
	entered, release := make(chan struct{}), make(chan struct{})
	var once, released sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && r.Header.Get("Content-Type") != "application/json-patch+json" {
			once.Do(func() { close(entered); <-release })
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	// Registered after srv.Close, so it runs first: a test that ends early
	// (t.Fatal) lets the held write go, and the server closes.
	free := func() { released.Do(func() { close(release) }) }
	t.Cleanup(free)
	c := newTestClient(testClientOptions{BaseURL: srv.URL, HTTPClient: srv.Client(), Ctx: t.Context()})
	saved := make(chan error, 1)
	go func() { saved <- SaveRecipeRun(c, &types.RecipeRun{ID: "run"}, true) }()
	<-entered // the recipe run write holds this process's writer
	renewed := *mine
	renewed.HeartbeatAt = now.Add(10 * time.Second).UTC().Format(time.RFC3339)
	// A renewal that waited for the writer would wait for the held write:
	// its own bound ends it then (a failure, not a hang).
	rctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	rc := newTestClient(testClientOptions{BaseURL: srv.URL, HTTPClient: srv.Client(), Ctx: rctx})
	if lost, err := RenewOperationLocks(rc, []LockRenewal{{Key: ClusterLockKey, Old: leaseValue(t, mine), New: leaseValue(t, &renewed)}}); err != nil || lost != nil {
		t.Fatalf("lost %v err %v", lost, err)
	}
	free()
	// The recipe run write read the record before the renewal: it
	// conflicts once and is written from a fresh read.
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	if f.get(ClusterLockKey) != leaseValue(t, &renewed) || f.get(record.RecipeRunKey) == "" {
		t.Fatalf("lock %s run %s", f.get(ClusterLockKey), f.get(record.RecipeRunKey))
	}
}

// An activity entry's write budget starts once this process's previous
// entry is written, not while it waits for it: an entry queued behind a
// slow one is still written. The second entry starts once the first is
// writing; the first write then takes longer than a whole budget, after
// the second began to wait.
func TestActivityBudgetStartsAfterTheWait(t *testing.T) {
	t.Setenv("NAMESPACE", "test-ns")
	t.Cleanup(record.SetActivityWriteTimeoutForTest(time.Second))
	firstWriting := make(chan struct{})
	secondWaiting := make(chan struct{}, 1)
	t.Cleanup(record.SetBeforeActivityWaitForTest(func() {
		select {
		case <-firstWriting: // only the second entry: the first is writing
			secondWaiting <- struct{}{}
		default:
		}
	}))
	var mu sync.Mutex
	var details []string
	rv := 0
	var first sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			first.Do(func() {
				close(firstWriting)
				<-secondWaiting // the second entry waits for this one now
				// This write takes longer than an entry's whole budget.
				kube.Clock().Sleep(context.Background(), 2*time.Second)
			})
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			if rv == 0 {
				w.WriteHeader(404)
				io.WriteString(w, `{"kind":"Status","status":"Failure","reason":"NotFound","code":404}`)
				return
			}
			var entries []types.ActivityEntry
			for _, d := range details {
				entries = append(entries, types.ActivityEntry{Action: "update", Detail: d})
			}
			raw, _ := json.Marshal(entries)
			out, _ := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": strconv.Itoa(rv)}, "data": map[string]string{"entries": string(raw)}})
			w.Write(out)
		default:
			var cm struct {
				Data map[string]string `json:"data"`
			}
			json.NewDecoder(r.Body).Decode(&cm)
			var entries []types.ActivityEntry
			json.Unmarshal([]byte(cm.Data["entries"]), &entries)
			details = details[:0]
			for _, e := range entries {
				details = append(details, e.Detail)
			}
			rv++
			w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(testClientOptions{BaseURL: srv.URL, HTTPClient: srv.Client(), Ctx: context.Background()})
	done := make(chan struct{})
	go func() {
		defer close(done)
		record.Activity(c, types.ActivityEntry{Timestamp: "2026-11-25T10:00:00Z", User: "u", Action: "update", Detail: "first", Success: true})
	}()
	<-firstWriting
	record.Activity(c, types.ActivityEntry{Timestamp: "2026-11-25T10:00:01Z", User: "u", Action: "update", Detail: "second", Success: true})
	<-done
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, d := range details {
		found = found || d == "second"
	}
	if !found {
		t.Fatalf("the queued entry was not written: %v", details)
	}
}
