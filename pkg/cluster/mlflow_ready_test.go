package cluster

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/mlflow"
)

const mlflowAvailable = `{"conditions":[{"type":"Available","status":"True"}]}`

func fastMLflowReadyWait(t *testing.T) {
	t.Helper()
	t.Cleanup(mlflow.SetReadyTimingForTest(time.Minute, time.Second))
}

func putToolMLflow(f *resourceFake, extraMeta, status string) {
	putMLflow(f, toolLabelJSON, envkit.ToolFieldManager, "Apply", extraMeta, `{}`, status)
}

// Provisioning, then not available, then available: the wait polls until
// MLflow is ready.
func TestWaitMLflowReady_PollsUntilReady(t *testing.T) {
	fastMLflowReadyWait(t)
	f, c := newResourceFake(t)
	putToolMLflow(f, "", `{}`)
	reads := 0
	f.beforeServe = func(method, path string) {
		if method != http.MethodGet || path != mlflowPath {
			return
		}
		reads++
		switch reads {
		case 2:
			putToolMLflow(f, "", notAvailable)
		case 3:
			putToolMLflow(f, "", mlflowAvailable)
		}
	}
	state, err := mlflow.WaitReady(c)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Ready || !state.ManagedByTool || reads != 3 {
		t.Fatalf("state %+v after %d reads", state, reads)
	}
}

// Waiting cannot help: a pod that cannot start, a CR being deleted, or no
// CR. Each ends at the first read with ErrCannotStart and says why.
func TestWaitMLflowReady_StopsAtATerminalState(t *testing.T) {
	cases := []struct {
		name string
		put  func(f *resourceFake)
		want string
	}{
		{"pod cannot start", func(f *resourceFake) {
			putToolMLflow(f, "", notAvailable)
			f.putJSON("/api/v1/namespaces/redhat-ods-applications/pods/mlflow-1", `{"metadata":{"labels":{"app":"mlflow"}},"spec":{"containers":[{"image":"x"}]},
				"status":{"containerStatuses":[{"state":{"waiting":{"reason":"ImagePullBackOff","message":"Back-off pulling image \"x\""}}}]}}`)
		}, "its pod cannot start (ImagePullBackOff: Back-off pulling image"},
		{"being deleted", func(f *resourceFake) {
			putToolMLflow(f, `,"deletionTimestamp":"2026-11-21T10:00:00Z"`, notAvailable)
		}, "the MLflow CR is being deleted"},
		{"gone", func(*resourceFake) {}, "the MLflow CR mlflow does not exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastMLflowReadyWait(t)
			f, c := newResourceFake(t)
			tc.put(f)
			_, err := mlflow.WaitReady(c)
			if !errors.Is(err, mlflow.ErrCannotStart) || errors.Is(err, mlflow.ErrNotReady) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want ErrCannotStart with %q", err, tc.want)
			}
			if n := len(f.callsTo(http.MethodGet, mlflowPath)); n != 1 {
				t.Fatalf("%d reads, want 1", n)
			}
		})
	}
}

// Never ready, or the CR cannot be read (read again each poll): the wait
// ends after its timeout on the fake clock, with the last message.
func TestWaitMLflowReady_TimesOut(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *resourceFake)
		want  string
	}{
		{"not available", func(f *resourceFake) { putToolMLflow(f, "", notAvailable) }, "after 30s: MLflow deployment is not ready"},
		{"unreadable", func(f *resourceFake) {
			putToolMLflow(f, "", notAvailable)
			f.fail["GET "+mlflowPath] = http.StatusForbidden
		}, "after 30s: Cannot read MLflow status: "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(mlflow.SetReadyTimingForTest(30*time.Second, 10*time.Second))
			f, c := newResourceFake(t)
			tc.setup(f)
			start := kube.Clock().Now()
			state, err := mlflow.WaitReady(c)
			if !errors.Is(err, mlflow.ErrNotReady) || !strings.Contains(err.Error(), tc.want) || state.Ready {
				t.Fatalf("err %v, state %+v", err, state)
			}
			if waited := kube.Clock().Since(start); waited < 30*time.Second {
				t.Fatalf("gave up after %s", waited)
			}
			if n := len(f.callsTo(http.MethodGet, mlflowPath)); n < 3 || n > 4 {
				t.Fatalf("%d reads in 30s at a 10s poll", n)
			}
		})
	}
}

// An ended client context ends the wait with its error.
func TestWaitMLflowReady_ContextEnded(t *testing.T) {
	fastMLflowReadyWait(t)
	f, c := newResourceFake(t)
	putToolMLflow(f, "", notAvailable)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := mlflow.WaitReady(c.WithContext(ctx)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}
