package cluster

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/clock/clocktest"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/workbench"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// deadlineAfter reaches the operation's deadline (it moves the injected
// clock past it) once the create of path has its whole answer.
type deadlineAfter struct {
	base http.RoundTripper
	path string
	clk  *clocktest.Fake
	by   time.Duration
}

func (d *deadlineAfter) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := d.base.RoundTrip(r)
	if err != nil || r.Method != http.MethodPost || r.URL.Path != d.path || r.URL.Query().Get("dryRun") != "" {
		return resp, err
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	d.clk.Advance(d.by)
	return resp, nil
}

// The operation's deadline right after a create of the workbench setup:
// the object is still recorded as this tool's (or deleted again), on the
// post-operation context, never left unrecorded. Before, the record write
// ran on the operation's own context, which had ended, and so did the
// delete meant to undo the create.
func TestWorkbenchSetupRecordsWhatItMadeAtItsDeadline(t *testing.T) {
	t.Setenv("NAMESPACE", "rhoai-nightly-updater")
	for _, tc := range []struct{ name, path, object string }{
		{"after the storage's create", wbPVCs, wbPVC},
		{"after the workbench's create", wbNotebooks, wbNotebook},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := clocktest.NewFake()
			t.Cleanup(kube.SetClockForTest(clk))
			f, c := newResourceFake(t)
			putWorkbenchCluster(t, f, true)
			ctx, cancel := clk.WithDeadline(context.Background(), clk.Now().Add(15*time.Minute))
			defer cancel()
			editTestClient(c, func(o *testClientOptions) {
				o.HTTPClient = &http.Client{Transport: &deadlineAfter{base: o.HTTPClient.Transport, path: tc.path, clk: clk, by: 16 * time.Minute}}
				o.Ctx = ctx
			})
			u := userClientOf(c, fakeUserToken)
			u.SetUsername("alice")
			if _, err := RunWorkbench(u, types.WorkbenchRequest{Action: "setup", Project: "proj"}, nil); err != nil {
				t.Fatal(err)
			}
			if ctx.Err() == nil {
				t.Fatal("the deadline was not reached")
			}
			obj := f.get(tc.object)
			if obj == nil {
				return // deleted again
			}
			rec, err := workbench.ReadRecord(c.WithContext(context.Background()))
			if err != nil {
				t.Fatal(err)
			}
			e, _ := rec.Get("proj")
			uid := obj["metadata"].(map[string]any)["uid"]
			if uid != e.StorageUID && uid != e.UID {
				t.Fatalf("%s (uid %v) is left unrecorded: %+v", tc.object, uid, e)
			}
		})
	}
}

// The record write after the deadline fails: the create is undone, its
// UID-guarded delete reaching the API on a budget of its own; and a
// project user's setup at its deadline records its storage through the
// users' records admission (record.AdmitUserEntry) as theirs.
func TestWorkbenchSetupUndoesWhatItCannotRecordAtItsDeadline(t *testing.T) {
	t.Setenv("NAMESPACE", "rhoai-nightly-updater")
	const snapshot = "/api/v1/namespaces/rhoai-nightly-updater/configmaps/rhoai-nightly-updater-snapshot"
	for _, tc := range []struct {
		name        string
		failRecord  bool
		projectUser bool
	}{
		{"the record write fails", true, false},
		{"a project user's record", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := clocktest.NewFake()
			t.Cleanup(kube.SetClockForTest(clk))
			f, c := newResourceFake(t)
			putWorkbenchCluster(t, f, true)
			if tc.failRecord {
				f.fail["PATCH "+snapshot] = http.StatusInternalServerError
			}
			ctx, cancel := clk.WithDeadline(context.Background(), clk.Now().Add(15*time.Minute))
			defer cancel()
			editTestClient(c, func(o *testClientOptions) {
				o.HTTPClient = &http.Client{Transport: &deadlineAfter{base: o.HTTPClient.Transport, path: wbPVCs, clk: clk, by: 16 * time.Minute}}
				o.Ctx = ctx
			})
			u := userClientOf(c, fakeUserToken)
			u.SetUsername("alice")
			if tc.projectUser {
				u.SetProjectUser(f.get(wbProjectNS)["metadata"].(map[string]any)["uid"].(string))
			}
			res, err := RunWorkbench(u, types.WorkbenchRequest{Action: "setup", Project: "proj"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if ctx.Err() == nil {
				t.Fatal("the deadline was not reached")
			}
			if tc.failRecord {
				deletes := f.callsTo(http.MethodDelete, wbPVC)
				if len(deletes) != 1 || !strings.Contains(deletes[0].Body, `"uid"`) || f.has(wbPVC) || res.Success {
					t.Fatalf("undo: deletes %+v, storage left %v, result %+v", deletes, f.has(wbPVC), res)
				}
				return
			}
			rec, err := workbench.ReadRecord(c.WithContext(context.Background()))
			if err != nil {
				t.Fatal(err)
			}
			e, _ := rec.Get("proj")
			if e.StorageUID == "" || e.StorageUID != f.get(wbPVC)["metadata"].(map[string]any)["uid"] || e.ActorRole != record.ActorRoleUser {
				t.Fatalf("the storage is not recorded as the user's: %+v", e)
			}
		})
	}
}

// lockLostAfter ends the operation's context with the lock-lost cause
// once the create of path has its whole answer.
type lockLostAfter struct {
	base http.RoundTripper
	path string
	lose context.CancelCauseFunc
}

func (d *lockLostAfter) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := d.base.RoundTrip(r)
	if err != nil || r.Method != http.MethodPost || r.URL.Path != d.path || r.URL.Query().Get("dryRun") != "" {
		return resp, err
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	d.lose(record.ErrOperationLockLost)
	return resp, nil
}

// After the cross-pod lock was lost right after a create, the record is
// still attempted (the tool's own bookkeeping), but when it fails the
// object is not deleted: another pod may be changing the same objects.
func TestWorkbenchSetupDeletesNothingAfterItsLockWasLost(t *testing.T) {
	t.Setenv("NAMESPACE", "rhoai-nightly-updater")
	f, c := newResourceFake(t)
	putWorkbenchCluster(t, f, true)
	f.fail["PATCH /api/v1/namespaces/rhoai-nightly-updater/configmaps/rhoai-nightly-updater-snapshot"] = http.StatusInternalServerError
	ctx, lose := context.WithCancelCause(context.Background())
	defer lose(nil)
	editTestClient(c, func(o *testClientOptions) {
		o.HTTPClient = &http.Client{Transport: &lockLostAfter{base: o.HTTPClient.Transport, path: wbPVCs, lose: lose}}
		o.Ctx = ctx
	})
	u := userClientOf(c, fakeUserToken)
	u.SetUsername("alice")
	res, err := RunWorkbench(u, types.WorkbenchRequest{Action: "setup", Project: "proj"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.callsTo(http.MethodPatch, "/api/v1/namespaces/rhoai-nightly-updater/configmaps/rhoai-nightly-updater-snapshot")); n == 0 {
		t.Fatal("the record was not attempted")
	}
	if d := f.callsTo(http.MethodDelete, wbPVC); len(d) != 0 || res.Success {
		t.Fatalf("deleted after the lock was lost: %+v, %+v", d, res)
	}
}
