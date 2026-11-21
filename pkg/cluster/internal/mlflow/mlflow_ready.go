package mlflow

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Bounds of WaitReady. The timeout is the browser's wait for an MLflow it
// just set up (READY_TIMEOUT_MS in
// frontend/src/components/environment/useGoalRun.ts; it usually takes 1 to
// 3 minutes). Tests set them with SetReadyTimingForTest.
var (
	ReadyTimeout = 10 * time.Minute
	readyPoll    = 5 * time.Second
)

var (
	// ErrNotReady: MLflow did not become ready within ReadyTimeout; it may
	// still.
	ErrNotReady = errors.New("MLflow is not ready")
	// ErrCannotStart: MLflow will not become ready by waiting: its pod
	// reports a reason that does not clear by itself (image pull, crash
	// loop, configuration; envkit.InspectPods), or the CR is being deleted
	// or gone.
	ErrCannotStart = errors.New("MLflow cannot become ready")
)

// statusReadFailed starts Status's message when the CR cannot be read.
const statusReadFailed = "Cannot read MLflow status: "

// WaitReady polls Status until MLflow is ready, the way the Environment
// page's Set up waits for it in the browser today. It returns the last
// state, and:
//
//   - nil once it is ready;
//   - an error wrapping ErrCannotStart, with the reason, as soon as waiting
//     cannot help: a terminal pod reason (Status's TerminalError), the CR is
//     being deleted, or it is gone (deleted since it was set up);
//   - an error wrapping ErrNotReady, with the last message, after
//     ReadyTimeout; a CR that cannot be read (Status reports it as its
//     message) is read again until then;
//   - the client context's error when that ends first.
//
// Every request runs under one context bounded by the timeout, so a request
// that starts just before the deadline cannot overrun it.
func WaitReady(c *kube.Client) (types.ResourceState, error) {
	ctx, cancel := kube.Clock().WithTimeout(c.Context(), ReadyTimeout)
	defer cancel()
	cc := c.WithContext(ctx)
	last := types.ResourceState{Message: "not read yet"}
	for {
		state := Status(cc)
		if ctx.Err() != nil {
			if err := c.Context().Err(); err != nil {
				return last, err
			}
			return last, fmt.Errorf("%w after %s: %s", ErrNotReady, ReadyTimeout, last.Message)
		}
		last = state
		switch {
		case state.Ready:
			return state, nil
		case state.Deployed && state.Terminating:
			return state, fmt.Errorf("%w: the MLflow CR is being deleted", ErrCannotStart)
		case state.Deployed && state.TerminalError:
			return state, fmt.Errorf("%w: its pod cannot start (%s)", ErrCannotStart, state.Message)
		case !state.Deployed && !strings.HasPrefix(state.Message, statusReadFailed):
			return state, fmt.Errorf("%w: the MLflow CR %s does not exist (it was deleted)", ErrCannotStart, CRName)
		}
		if kube.Clock().Sleep(ctx, readyPoll) != nil {
			if err := c.Context().Err(); err != nil {
				return last, err
			}
			return last, fmt.Errorf("%w after %s: %s", ErrNotReady, ReadyTimeout, last.Message)
		}
	}
}
