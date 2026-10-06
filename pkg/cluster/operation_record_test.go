package cluster

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// fakeOperationConfigMap serves the operation ConfigMap like the API
// server: a merge patch that carries metadata.resourceVersion fails with
// 409 Conflict when the object changed, and a create of an existing object
// fails with 409 AlreadyExists.
type fakeOperationConfigMap struct {
	t        *testing.T
	mu       sync.Mutex
	data     map[string]string // nil: the ConfigMap does not exist
	rv       int
	requests []string
	// beforeWrite runs before each PATCH or POST is applied, so a test can
	// change the object in between (another writer).
	beforeWrite func(f *fakeOperationConfigMap)
}

const fakeOperationCMPath = "/api/v1/namespaces/test-ns/configmaps/rhoai-nightly-updater-operation"

func newFakeOperationConfigMap(t *testing.T) (*fakeOperationConfigMap, *Client) {
	t.Setenv("NAMESPACE", "test-ns")
	f := &fakeOperationConfigMap{t: t}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: t.Context()}
}

func (f *fakeOperationConfigMap) status(w http.ResponseWriter, code int, reason string) {
	w.WriteHeader(code)
	out, _ := json.Marshal(map[string]interface{}{"kind": "Status", "status": "Failure", "code": code, "reason": reason})
	w.Write(out)
}

func (f *fakeOperationConfigMap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method)
	var body struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == fakeOperationCMPath:
		if f.data == nil {
			f.status(w, 404, "NotFound")
			return
		}
		out, _ := json.Marshal(map[string]interface{}{
			"metadata": map[string]string{"resourceVersion": strconv.Itoa(f.rv)},
			"data":     f.data,
		})
		w.Write(out)
	case r.Method == http.MethodPatch && r.URL.Path == fakeOperationCMPath:
		if f.beforeWrite != nil {
			f.beforeWrite(f)
		}
		if f.data == nil {
			f.status(w, 404, "NotFound")
			return
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Metadata.ResourceVersion != "" && body.Metadata.ResourceVersion != strconv.Itoa(f.rv) {
			f.status(w, 409, "Conflict")
			return
		}
		for k, v := range body.Data {
			f.data[k] = v
		}
		f.rv++
		w.Write([]byte(`{}`))
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces/test-ns/configmaps":
		if f.beforeWrite != nil {
			f.beforeWrite(f)
		}
		if f.data != nil {
			f.status(w, 409, "AlreadyExists")
			return
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.data = body.Data
		f.rv++
		w.WriteHeader(201)
		w.Write([]byte(`{}`))
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(500)
	}
}

func (f *fakeOperationConfigMap) lease() *types.OperationLease {
	f.mu.Lock()
	defer f.mu.Unlock()
	return decodeRecordValue[types.OperationLease](f.data, leaseKey)
}

func (f *fakeOperationConfigMap) get(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data[key]
}

func (f *fakeOperationConfigMap) reqs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeOperationConfigMap) set(key string, v interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.data == nil {
		f.data = map[string]string{}
	}
	raw, _ := json.Marshal(v)
	f.data[key] = string(raw)
	f.rv++
}

func testLease(id, boot string, beat time.Time) *types.OperationLease {
	return &types.OperationLease{ID: id, BootID: boot, Pod: "pod-" + boot, User: "alice", Type: "update", Label: "Update to nightly",
		StartedAt: beat.Add(-time.Minute).UTC().Format(time.RFC3339), HeartbeatAt: beat.UTC().Format(time.RFC3339)}
}

// heldBy treats a lease of any boot ID in alive as held by a live process.
func heldBy(alive ...string) func(*types.OperationLease) bool {
	return func(l *types.OperationLease) bool {
		for _, b := range alive {
			if l.BootID == b {
				return true
			}
		}
		return false
	}
}

func TestWriteOperationLease(t *testing.T) {
	now := time.Now()

	t.Run("creates the ConfigMap when it is missing", func(t *testing.T) {
		f, c := newFakeOperationConfigMap(t)
		mine := testLease("op1", "boot-a", now)
		if holder, err := WriteOperationLease(c, mine, heldBy("boot-b")); holder != nil || err != nil {
			t.Fatalf("holder %+v err %v", holder, err)
		}
		if got := f.lease(); got == nil || *got != *mine {
			t.Fatalf("lease = %+v", got)
		}
	})

	t.Run("a live lease of another process is returned, not overwritten", func(t *testing.T) {
		f, c := newFakeOperationConfigMap(t)
		theirs := testLease("op0", "boot-b", now)
		f.set(leaseKey, theirs)
		holder, err := WriteOperationLease(c, testLease("op1", "boot-a", now), heldBy("boot-b"))
		if err != nil || holder == nil || *holder != *theirs {
			t.Fatalf("holder %+v err %v", holder, err)
		}
		if got := f.lease(); *got != *theirs {
			t.Fatalf("lease overwritten: %+v", got)
		}
	})

	t.Run("an expired or dead lease is taken over, and other keys are kept", func(t *testing.T) {
		f, c := newFakeOperationConfigMap(t)
		f.set(leaseKey, testLease("op0", "boot-dead", now.Add(-time.Hour)))
		f.set(lastCompletedKey, &types.CompletedOperation{ID: "op0"})
		mine := testLease("op1", "boot-a", now)
		if holder, err := WriteOperationLease(c, mine, heldBy()); holder != nil || err != nil {
			t.Fatalf("holder %+v err %v", holder, err)
		}
		if got := f.lease(); *got != *mine {
			t.Fatalf("lease = %+v", got)
		}
		if f.get(lastCompletedKey) == "" {
			t.Fatal("lastCompleted lost")
		}
	})

	t.Run("a conflicting write is retried from a fresh read", func(t *testing.T) {
		f, c := newFakeOperationConfigMap(t)
		f.set(operationKey, &types.OperationMarker{ID: "op0"})
		conflicts := 1
		f.beforeWrite = func(f *fakeOperationConfigMap) {
			if conflicts > 0 {
				conflicts--
				f.rv++ // another writer (a marker write) came first
			}
		}
		mine := testLease("op1", "boot-a", now)
		if holder, err := WriteOperationLease(c, mine, heldBy()); holder != nil || err != nil {
			t.Fatalf("holder %+v err %v", holder, err)
		}
		if got := f.lease(); got == nil || *got != *mine {
			t.Fatalf("lease = %+v", got)
		}
		if want := []string{"GET", "PATCH", "GET", "PATCH"}; len(f.reqs()) != len(want) {
			t.Fatalf("requests %v", f.reqs())
		}
	})

	t.Run("a process that wins the race between read and write is respected", func(t *testing.T) {
		f, c := newFakeOperationConfigMap(t)
		theirs := testLease("op0", "boot-b", now)
		f.beforeWrite = func(f *fakeOperationConfigMap) {
			if f.data == nil {
				raw, _ := json.Marshal(theirs)
				f.data = map[string]string{leaseKey: string(raw)}
				f.rv++
			}
		}
		holder, err := WriteOperationLease(c, testLease("op1", "boot-a", now), heldBy("boot-b"))
		if err != nil || holder == nil || holder.ID != "op0" {
			t.Fatalf("holder %+v err %v (requests %v)", holder, err, f.reqs())
		}
	})

	t.Run("conflicts on every attempt", func(t *testing.T) {
		f, c := newFakeOperationConfigMap(t)
		f.set(operationKey, &types.OperationMarker{ID: "op0"})
		f.beforeWrite = func(f *fakeOperationConfigMap) { f.rv++ }
		holder, err := WriteOperationLease(c, testLease("op1", "boot-a", now), heldBy())
		if holder != nil || !errors.Is(err, ErrOperationRecordContended) {
			t.Fatalf("holder %+v err %v", holder, err)
		}
	})

	t.Run("a read error is returned", func(t *testing.T) {
		t.Setenv("NAMESPACE", "test-ns")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
		defer srv.Close()
		c := &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: t.Context()}
		if holder, err := WriteOperationLease(c, testLease("op1", "boot-a", now), heldBy()); holder != nil || err == nil || errors.Is(err, ErrOperationRecordContended) {
			t.Fatalf("holder %+v err %v", holder, err)
		}
	})
}

func TestReleaseOperationLeaseOnlyClearsItsOwn(t *testing.T) {
	now := time.Now()
	f, c := newFakeOperationConfigMap(t)
	theirs := testLease("op0", "boot-b", now)
	f.set(leaseKey, theirs)
	if err := ReleaseOperationLease(c, LeaseOwner{BootID: "boot-a", ID: "op1"}); err != nil {
		t.Fatal(err)
	}
	if got := f.lease(); got == nil || *got != *theirs {
		t.Fatalf("another process's lease was cleared: %+v", got)
	}
	// Same process, another operation: not this one's either.
	if err := ReleaseOperationLease(c, LeaseOwner{BootID: "boot-b", ID: "op9"}); err != nil || f.lease() == nil {
		t.Fatalf("err %v lease %+v", err, f.lease())
	}
	if err := ReleaseOperationLease(c, LeaseOwner{BootID: "boot-b", ID: "op0"}); err != nil || f.lease() != nil {
		t.Fatalf("own lease not cleared: err %v lease %+v", err, f.lease())
	}
	// Nothing recorded: nothing to do.
	f2, c2 := newFakeOperationConfigMap(t)
	if err := ReleaseOperationLease(c2, LeaseOwner{BootID: "boot-a", ID: "op1"}); err != nil || len(f2.reqs()) != 1 {
		t.Fatalf("err %v requests %v", err, f2.reqs())
	}
}

// TestCompletionReleasesTheLeaseInTheSameWrite: the completion clears the
// marker and the operation's own lease in one write, and leaves what
// another process recorded alone.
func TestCompletionReleasesTheLeaseInTheSameWrite(t *testing.T) {
	now := time.Now()
	done := &types.CompletedOperation{ID: "op1", Type: "update", FinishedAt: now.UTC().Format(time.RFC3339), Success: true}
	owner := &LeaseOwner{BootID: "boot-a", ID: "op1"}

	f, c := newFakeOperationConfigMap(t)
	f.set(operationKey, &types.OperationMarker{ID: "op1", BootID: "boot-a"})
	f.set(leaseKey, testLease("op1", "boot-a", now))
	if err := SaveCompletedOperation(c, done, owner); err != nil {
		t.Fatal(err)
	}
	rec, err := ReadOperationRecord(c)
	if err != nil || rec.Marker != nil || rec.Lease != nil || rec.LastCompleted == nil || rec.LastCompleted.ID != "op1" {
		t.Fatalf("record %+v err %v", rec, err)
	}
	if r := f.reqs(); r[len(r)-2] != "PATCH" || r[len(r)-3] != "GET" {
		t.Fatalf("requests %v", f.reqs())
	}

	// Another process took the lock over after this one's heartbeat
	// stopped, and began its own operation.
	f, c = newFakeOperationConfigMap(t)
	theirMarker := &types.OperationMarker{ID: "op2", BootID: "boot-b"}
	theirLease := testLease("op2", "boot-b", now)
	f.set(operationKey, theirMarker)
	f.set(leaseKey, theirLease)
	if err := SaveCompletedOperation(c, done, owner); err != nil {
		t.Fatal(err)
	}
	rec, _ = ReadOperationRecord(c)
	if rec.Marker == nil || *rec.Marker != *theirMarker || rec.Lease == nil || *rec.Lease != *theirLease || rec.LastCompleted.ID != "op1" {
		t.Fatalf("record %+v", rec)
	}

	// The ConfigMap is gone: it is created with the completion.
	f, c = newFakeOperationConfigMap(t)
	if err := SaveCompletedOperation(c, done, owner); err != nil {
		t.Fatal(err)
	}
	if rec, _ := ReadOperationRecord(c); rec.LastCompleted == nil || rec.Marker != nil {
		t.Fatalf("record %+v", rec)
	}
	if r := f.reqs(); len(r) != 3 || r[1] != "POST" {
		t.Fatalf("requests %v", r)
	}
}

func TestLeaseLive(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		beat string
		want bool
	}{
		{"fresh", now.Add(-10 * time.Second).UTC().Format(time.RFC3339), true},
		{"expired", now.Add(-time.Minute).UTC().Format(time.RFC3339), false},
		{"ahead (clock skew)", now.Add(20 * time.Second).UTC().Format(time.RFC3339), true},
		{"unreadable", "yesterday", false},
	} {
		if got := LeaseLive(&types.OperationLease{HeartbeatAt: tc.beat}, now, 45*time.Second); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
	if LeaseLive(nil, now, time.Minute) {
		t.Error("nil lease is live")
	}
}

func TestReadOperationRecordIgnoresUnreadableValues(t *testing.T) {
	f, c := newFakeOperationConfigMap(t)
	f.set(lastCompletedKey, &types.CompletedOperation{ID: "op0"})
	f.mu.Lock()
	f.data[leaseKey] = "{not json"
	f.data[operationKey] = "[]"
	f.mu.Unlock()
	rec, err := ReadOperationRecord(c)
	if err != nil || rec.Lease != nil || rec.Marker != nil || rec.LastCompleted == nil {
		t.Fatalf("record %+v err %v", rec, err)
	}
}
