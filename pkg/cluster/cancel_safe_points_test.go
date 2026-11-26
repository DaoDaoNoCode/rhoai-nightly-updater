package cluster

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/clock/clocktest"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
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

// flipDuring makes the cancel request while the request matching (method,
// path, not a dry run) is in flight: before the fake answers it. The
// request still finishes (nothing ends its context).
type flipDuring struct {
	base         http.RoundTripper
	method, path string
	flip         func()
}

func (d *flipDuring) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == d.method && r.URL.Path == d.path && r.URL.Query().Get("dryRun") == "" {
		d.flip()
	}
	return d.base.RoundTrip(r)
}

// A workbench setup cancelled before its first change stops before the
// Notebook's dry run (whose webhook changes the project) and changes
// nothing; one cancelled while the storage's create is in flight lets
// that request finish, keeps the storage recorded (as when the
// workbench's create fails) and creates no workbench; one cancelled while
// the workbench's create is in flight (its last safe point behind it)
// finishes and succeeds.
func TestWorkbenchSetupCancelAtEverySafePoint(t *testing.T) {
	t.Setenv("NAMESPACE", "rhoai-nightly-updater")
	for _, tc := range []struct {
		name       string
		during     string // the create in flight when the request comes; "" from the start
		pvc, nb    bool
		success    bool
		wantPrefix string
	}{
		{"before anything", "", false, false, false, "Cancelled by the user who started it before this step. Nothing was changed."},
		{"while the storage is created", wbPVCs, true, false, false, "Cancelled by the user who started it before this step. No workbench was created; its storage nightly-workbench-storage stays"},
		{"while the workbench is created", wbNotebooks, true, true, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newResourceFake(t)
			putWorkbenchCluster(t, f, true)
			sw := &cancelSwitch{}
			sw.on.Store(tc.during == "")
			editTestClient(c, func(o *testClientOptions) {
				o.HTTPClient = &http.Client{Transport: &flipDuring{base: o.HTTPClient.Transport, method: http.MethodPost, path: tc.during, flip: func() { sw.on.Store(true) }}}
			})
			u := withCancel(userClientOf(c, fakeUserToken), sw)
			u.SetUsername("alice")
			u.SetProjectUser("uid-proj")
			res, err := RunWorkbench(u, types.WorkbenchRequest{Action: "setup", Project: "proj"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.Success != tc.success || !strings.HasPrefix(res.Message, tc.wantPrefix) || !tc.success && res.ErrorCode != "cancelled" {
				t.Fatalf("result %+v", res)
			}
			if f.has(wbPVC) != tc.pvc || f.has(wbNotebook) != tc.nb {
				t.Fatalf("storage %v, workbench %v", f.has(wbPVC), f.has(wbNotebook))
			}
			if tc.during == "" {
				for _, call := range f.callsTo(http.MethodPost, wbNotebooks) {
					t.Fatalf("the Notebook's dry run was sent: %+v", call.Query)
				}
			}
			rec, err := workbench.ReadRecord(c)
			if err != nil {
				t.Fatal(err)
			}
			e, _ := rec.Get("proj")
			if tc.pvc && e.StorageUID != f.get(wbPVC)["metadata"].(map[string]any)["uid"] {
				t.Fatalf("the storage is not recorded: %+v", e)
			}
			if u.Context().Err() != nil {
				t.Fatal("the operation's context ended")
			}
			log := storedActivity(t, f)
			if len(log) != 1 || log[0].Success != tc.success || !tc.success && log[0].Reason != "Cancelled by the user who started it" {
				t.Fatalf("activity %+v", log)
			}
		})
	}
}

// A playground setup cancelled before the dashboard is asked sets nothing
// up; one cancelled while the install call is in flight (from inside it)
// lets it finish and stops at the ready wait's first poll, the playground
// kept and recorded, deletable from the tab; one cancelled during the
// ready wait stops at the next poll, kept the same way. A cluster-admin's
// cancel says so.
func TestPlaygroundSetupCancelAtEverySafePoint(t *testing.T) {
	for _, tc := range []struct {
		name      string
		at        string // "install": from inside the install call; "ready": with the wait's step; "": from the start
		byAdmin   bool
		installed bool
		wantStep  string
		want      string
	}{
		{"before the install", "", false, false, "install", "Cancelled by the user who started it before this step. Nothing was changed."},
		{"while the install runs", "install", false, true, "ready", "Cancelled by the user who started it before this step. The dashboard was already asked to set up the playground in proj, so it stays, recorded as this tool's"},
		{"during the ready wait", "ready", true, true, "ready", "Cancelled by a cluster-admin before this step. The dashboard was already asked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPlaygroundEnv(t)
			var on atomic.Bool
			on.Store(tc.at == "")
			if tc.at == "install" {
				onInstall := e.g.onInstall
				e.g.onInstall = func(ns string) { on.Store(true); onInstall(ns) }
			}
			e.c = e.c.WithContext(WithCancelCheck(context.Background(), func() *ActionCancelledError {
				if on.Load() {
					return &ActionCancelledError{ByAdmin: tc.byAdmin}
				}
				return nil
			}))
			var steps []UpdateStepEvent
			res, err := RunPlayground(e.c, setup(), func(s UpdateStepEvent) {
				steps = append(steps, s)
				if tc.at == "ready" && s.Step == "ready" && s.Status == "running" {
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

// A cancel that comes while the setup waits to try the install again (the
// dashboard answered 409: the previous playground's database is still
// being removed) is seen right before the next attempt: no second POST,
// and the request's record goes.
func TestPlaygroundSetupCancelDuringTheInstallRetryWait(t *testing.T) {
	e := newPlaygroundEnv(t)
	clk := clocktest.NewFake()
	t.Cleanup(kube.SetClockForTest(clk))
	e.g.conflicts = 1
	var on atomic.Bool
	e.c = e.c.WithContext(WithCancelCheck(context.Background(), func() *ActionCancelledError {
		if on.Load() {
			return &ActionCancelledError{}
		}
		return nil
	}))
	type result struct {
		res *types.OperationResponse
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := RunPlayground(e.c, setup(), nil)
		done <- result{res, err}
	}()
	clk.WaitForSleepers(1) // the wait before the second attempt
	on.Store(true)
	clk.Advance(time.Hour)
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.res.Success || r.res.ErrorCode != "cancelled" || !strings.HasSuffix(r.res.Message, "Nothing was set up.") {
		t.Fatalf("result %+v", r.res)
	}
	e.g.mu.Lock()
	installs := len(e.g.installs)
	e.g.mu.Unlock()
	if installs != 1 {
		t.Fatalf("install POSTs: %d", installs)
	}
	if _, recorded := e.records(t)["proj"]; recorded {
		t.Fatal("the request's record stayed")
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
	if res.Success || res.ErrorCode != "cancelled" || !strings.Contains(res.Message, "Added before the cancel: MCP server") {
		t.Fatalf("result %+v\n%s", res, stepMessages(steps))
	}
	st := env.status(t)
	if st[SampleMCPServer].State != "present" || st[mlflow.SampleSkill].State != "absent" {
		t.Fatalf("after the cancel: %+v", st)
	}
	log := storedActivity(t, env.k8s)
	if len(log) != 1 || log[0].Success || log[0].Reason != "Cancelled by the user who started it" {
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
	if res.Success || res.ErrorCode != "cancelled" || !strings.HasPrefix(res.Message, "Cancelled by a cluster-admin before this step. Removed before the cancel:") {
		t.Fatalf("result %+v", res)
	}
	st := env.status(t)
	for _, id := range []string{SampleMCPServer, mlflow.SampleSkill} {
		if want := map[bool]string{true: "absent", false: "present"}[id == first]; st[id].State != want {
			t.Errorf("%s: %s, want %s (first removed: %s)", id, st[id].State, want, first)
		}
	}
}

// A sample that failed before the cancel is reported with it (it may have
// changed something), in the result and in the activity entry's detail;
// "Nothing was changed" only when nothing ran.
func TestMLflowSamplesCancelAfterAFailedSample(t *testing.T) {
	env := newSamplesEnv(t)
	env.mlflow.noDemo = true
	var on atomic.Bool
	user := env.user.WithContext(WithCancelCheck(env.user.Context(), func() *ActionCancelledError {
		if on.Load() {
			return &ActionCancelledError{}
		}
		return nil
	}))
	res, err := RunMLflowSamples(user, mlflowRequest("add", SampleGenAIDemo, SampleTraining), func(s UpdateStepEvent) {
		if s.Step == SampleGenAIDemo && s.Status == "failed" {
			on.Store(true)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Success || res.ErrorCode != "cancelled" || !strings.Contains(res.Message, "Failed before the cancel: ") || strings.Contains(res.Message, "Nothing was changed") {
		t.Fatalf("result %+v", res)
	}
	if st := env.status(t); st[SampleTraining].State == "present" {
		t.Fatal("the next sample ran after the cancel")
	}
	log := storedActivity(t, env.k8s)
	if len(log) != 1 || log[0].Success || log[0].CancelledBy != "starter" || !strings.Contains(log[0].Detail, "(failed: ") {
		t.Fatalf("activity %+v", log)
	}
}
