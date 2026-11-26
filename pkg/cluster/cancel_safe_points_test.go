package cluster

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/mlflow"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/workbench"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The cooperative cancel's safe points (the non-admin design, B4): a cancel
// is a request an action checks between its own steps and at each poll of
// a wait (record.CancelRequested), then ends through its failure path at
// that point. Here the request arrives at a chosen step, as pkg/api's
// requestCancel sets it (pkg/api TestCancelIsCheckedThroughTheGate drives
// the real request through the gate).

// cancelSwitch is a cancel request a test makes at a chosen moment.
type cancelSwitch struct{ on atomic.Bool }

func (s *cancelSwitch) check() *ActionCancelledError {
	if s.on.Load() {
		return &ActionCancelledError{}
	}
	return nil
}

// withCancel gives c's context the switch's check.
func withCancel(c *Client, s *cancelSwitch) *Client {
	return c.WithContext(WithCancelCheck(c.Context(), s.check))
}

// abortWatch fails the test when a request ends without its answer (its
// context ended, or it was cut off): a cancel never interrupts a request.
type abortWatch struct {
	t    *testing.T
	base http.RoundTripper
	mu   sync.Mutex
	n    int
}

func (a *abortWatch) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := a.base.RoundTrip(r)
	a.mu.Lock()
	a.n++
	a.mu.Unlock()
	if err != nil || r.Context().Err() != nil {
		a.t.Errorf("%s %s ended early: %v (context %v)", r.Method, r.URL.Path, err, r.Context().Err())
	}
	return resp, err
}

// A workbench setup cancelled before its first change creates nothing; one
// cancelled while it creates the storage lets that request finish, keeps
// the storage recorded (as when the workbench's create fails) and creates
// no workbench; one cancelled once its last safe point is behind it (the
// workbench's create) finishes and succeeds.
func TestWorkbenchSetupCancelAtEverySafePoint(t *testing.T) {
	t.Setenv("NAMESPACE", "rhoai-nightly-updater")
	for _, tc := range []struct {
		name       string
		at         string // the step whose "running" event the request arrives with; "" from the start
		pvc, nb    bool
		success    bool
		wantPrefix string
	}{
		{"before anything", "", false, false, false, "Cancelled by you before this step. Nothing was changed."},
		{"while the storage is created", "storage", true, false, false, "Cancelled by you before this step. No workbench was created; its storage nightly-workbench-storage stays"},
		{"after the last safe point", "workbench", true, true, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newResourceFake(t)
			putWorkbenchCluster(t, f, true)
			watch := &abortWatch{t: t}
			editTestClient(c, func(o *testClientOptions) {
				watch.base = o.HTTPClient.Transport
				o.HTTPClient = &http.Client{Transport: watch}
			})
			sw := &cancelSwitch{}
			sw.on.Store(tc.at == "")
			u := withCancel(userClientOf(c, fakeUserToken), sw)
			u.SetUsername("alice")
			u.SetProjectUser("uid-proj")
			res, err := RunWorkbench(u, types.WorkbenchRequest{Action: "setup", Project: "proj"}, func(e UpdateStepEvent) {
				if e.Step == tc.at && e.Status == "running" {
					sw.on.Store(true)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.Success != tc.success || !strings.HasPrefix(res.Message, tc.wantPrefix) || !tc.success && res.ErrorCode != "cancelled" {
				t.Fatalf("result %+v", res)
			}
			if f.has(wbPVC) != tc.pvc || f.has(wbNotebook) != tc.nb {
				t.Fatalf("storage %v, workbench %v", f.has(wbPVC), f.has(wbNotebook))
			}
			rec, err := workbench.ReadRecord(c)
			if err != nil {
				t.Fatal(err)
			}
			e, _ := rec.Get("proj")
			if tc.pvc && e.StorageUID != f.get(wbPVC)["metadata"].(map[string]any)["uid"] {
				t.Fatalf("the storage is not recorded: %+v", e)
			}
			log := storedActivity(t, f)
			if len(log) != 1 || log[0].Success != tc.success || !tc.success && log[0].Reason != "Cancelled by you" {
				t.Fatalf("activity %+v", log)
			}
		})
	}
}

// A playground setup cancelled before the dashboard is asked sets nothing
// up; one cancelled while the install call runs lets it finish and stops
// after it, the playground kept and recorded (as when its ready wait
// fails), deletable from the tab; one cancelled during the ready wait stops
// at the next poll, kept the same way. A cluster-admin's cancel says so.
func TestPlaygroundSetupCancelAtEverySafePoint(t *testing.T) {
	for _, tc := range []struct {
		name      string
		at        string
		byAdmin   bool
		installed bool
		wantStep  string
		want      string
	}{
		{"before the install", "", false, false, "install", "Cancelled by you before this step. Nothing was changed."},
		{"while the install runs", "install", false, true, "ready", "Cancelled by you before this step. The dashboard was already asked to set up the playground in proj, so it stays, recorded as this tool's"},
		{"during the ready wait", "ready", true, true, "ready", "Cancelled by a cluster-admin before this step. The dashboard was already asked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPlaygroundEnv(t)
			var on atomic.Bool
			on.Store(tc.at == "")
			e.c = e.c.WithContext(WithCancelCheck(context.Background(), func() *ActionCancelledError {
				if on.Load() {
					return &ActionCancelledError{ByAdmin: tc.byAdmin}
				}
				return nil
			}))
			var steps []UpdateStepEvent
			res, err := RunPlayground(e.c, setup(), func(s UpdateStepEvent) {
				steps = append(steps, s)
				if s.Step == tc.at && s.Status == "running" {
					on.Store(true)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			last := steps[len(steps)-1]
			if res.Success || res.ErrorCode != "cancelled" || last.Step != tc.wantStep || !strings.HasPrefix(res.Message, tc.want) {
				t.Fatalf("result %+v, last step %+v", res, last)
			}
			if got := len(e.g.installs); got != map[bool]int{true: 1, false: 0}[tc.installed] {
				t.Fatalf("installs: %d", got)
			}
			if _, recorded := e.records(t)["proj"]; recorded != tc.installed {
				t.Fatalf("recorded %v", recorded)
			}
			if e.c.Context().Err() != nil {
				t.Fatal("the operation's context ended")
			}
		})
	}
}

// MLflow samples stop between samples only: a sample whose add runs when
// the request comes (its creates and tags) completes and stays; the next
// is not started.
func TestMLflowSamplesCancelBetweenSamples(t *testing.T) {
	env := newSamplesEnv(t)
	var on atomic.Bool
	user := env.user.WithContext(WithCancelCheck(env.user.Context(), func() *ActionCancelledError {
		if on.Load() {
			return &ActionCancelledError{}
		}
		return nil
	}))
	var steps []UpdateStepEvent
	res, err := RunMLflowSamples(user, mlflowRequest("add", SampleMCPServer, mlflow.SampleSkill), func(s UpdateStepEvent) {
		steps = append(steps, s)
		if s.Step == SampleMCPServer && s.Status == "running" {
			on.Store(true)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Success || res.ErrorCode != "cancelled" || !strings.Contains(res.Message, "Done before the cancel, and kept: MCP server") {
		t.Fatalf("result %+v\n%s", res, stepMessages(steps))
	}
	st := env.status(t)
	if st[SampleMCPServer].State != "present" || st[mlflow.SampleSkill].State != "absent" {
		t.Fatalf("after the cancel: %+v", st)
	}
	log := storedActivity(t, env.k8s)
	if len(log) != 1 || log[0].Success || log[0].Reason != "Cancelled by you" {
		t.Fatalf("activity %+v", log)
	}
}

// A remove stops between samples the same way: the sample being removed
// when the request comes is removed, the next stays.
func TestMLflowSamplesRemoveCancelBetweenSamples(t *testing.T) {
	env := newSamplesEnv(t)
	if ok, msg, _ := env.run(t, "add", SampleMCPServer, mlflow.SampleSkill); !ok {
		t.Fatal(msg)
	}
	var on atomic.Bool
	user := env.user.WithContext(WithCancelCheck(env.user.Context(), func() *ActionCancelledError {
		if on.Load() {
			return &ActionCancelledError{ByAdmin: true}
		}
		return nil
	}))
	first := ""
	res, err := RunMLflowSamples(user, mlflowRequest("remove", SampleMCPServer, mlflow.SampleSkill), func(s UpdateStepEvent) {
		if first == "" && s.Status == "running" && s.Step != "check" {
			first = s.Step
			on.Store(true)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Success || res.ErrorCode != "cancelled" || !strings.HasPrefix(res.Message, "Cancelled by a cluster-admin before this step. Done before the cancel, and kept:") {
		t.Fatalf("result %+v", res)
	}
	st := env.status(t)
	for _, id := range []string{SampleMCPServer, mlflow.SampleSkill} {
		if want := map[bool]string{true: "absent", false: "present"}[id == first]; st[id].State != want {
			t.Errorf("%s: %s, want %s (first removed: %s)", id, st[id].State, want, first)
		}
	}
}
