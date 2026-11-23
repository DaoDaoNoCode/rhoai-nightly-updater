package storage

import (
	"errors"
	"fmt"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Bounds of WaitPipelineServerReady. The timeout is the browser's wait for
// a pipeline server it just set up (READY_TIMEOUT_MS in
// frontend/src/components/environment/useGoalRun.ts): DSPO usually needs 1
// to 3 minutes, and a server whose API server is not ready DSPAStuckAfter
// after creation is reported stuck well before it. Tests set them with
// SetPipelineServerReadyTimingForTest.
var (
	PipelineServerReadyTimeout = 10 * time.Minute
	pipelineServerReadyPoll    = 5 * time.Second
)

var (
	// ErrPipelineServerNotReady: the pipeline server did not become ready
	// within PipelineServerReadyTimeout; it may still.
	ErrPipelineServerNotReady = errors.New("the pipeline server is not ready")
	// ErrPipelineServerFailed: the pipeline server will not become ready by
	// waiting: it is stuck (see PipelineServerState), being deleted, or gone.
	ErrPipelineServerFailed = errors.New("the pipeline server cannot become ready")
)

// WaitPipelineServerReady polls the tool's pipeline server in project
// (ListDSPAs, PipelineServerState) until it is ready, the way the
// Environment page's Set up waits for it in the browser today. It returns
// the server's last state, and:
//
//   - nil once it is ready;
//   - an error wrapping ErrPipelineServerFailed, with the reason, as soon as
//     waiting cannot help: the server is stuck (its API server is not ready
//     DSPAStuckAfter after creation, as the dashboard judges it), it is
//     being deleted, or the project has no pipeline server of the tool's
//     (deleted since it was set up);
//   - an error wrapping ErrPipelineServerNotReady, with its last message,
//     after PipelineServerReadyTimeout;
//   - the client context's error when that ends first, and any read error
//     but a network error (retried until the timeout), for example
//     ErrDSPACRDMissing or a 403.
//
// Every request runs under one context bounded by the timeout, so a request
// that starts just before the deadline cannot overrun it.
func WaitPipelineServerReady(c *kube.Client, project string) (types.ResourceState, error) {
	ctx, cancel := kube.Clock().WithTimeout(c.Context(), PipelineServerReadyTimeout)
	defer cancel()
	cc := c.WithContext(ctx)
	last := types.ResourceState{Namespace: project, Message: "not read yet"}
	expired := func() (types.ResourceState, error) {
		if err := c.Context().Err(); err != nil {
			return last, err
		}
		return last, fmt.Errorf("%w after %s: the pipeline server in %s: %s", ErrPipelineServerNotReady, PipelineServerReadyTimeout, project, last.Message)
	}
	for {
		dspas, err := ListDSPAs(cc, project)
		if ctx.Err() != nil {
			return expired()
		}
		switch {
		case kube.IsNetworkError(err):
			last.Message = err.Error()
		case err != nil:
			return last, err
		default:
			state, found := toolPipelineServer(dspas)
			if !found {
				return last, fmt.Errorf("%w: project %s has no pipeline server set up by this tool (it was deleted)", ErrPipelineServerFailed, project)
			}
			last = state
			switch {
			case state.Ready:
				return state, nil
			case state.Terminating:
				return state, fmt.Errorf("%w: the pipeline server %s in %s is being deleted", ErrPipelineServerFailed, state.Name, project)
			case state.TerminalError:
				return state, fmt.Errorf("%w: the pipeline server %s in %s is stuck: its API server is not ready %s after it was created (%s)", ErrPipelineServerFailed, state.Name, project, DSPAStuckAfter, state.Message)
			}
		}
		if kube.Clock().Sleep(ctx, pipelineServerReadyPoll) != nil {
			return expired()
		}
	}
}

// toolPipelineServer is the state of the tool's pipeline server among
// dspas, preferring the current name (DSPAName) to an earlier one.
func toolPipelineServer(dspas []DSPA) (types.ResourceState, bool) {
	var found *DSPA
	for i := range dspas {
		d := &dspas[i]
		if !d.ManagedByTool() {
			continue
		}
		if found == nil || d.Meta.Name == DSPAName {
			found = d
		}
	}
	if found == nil {
		return types.ResourceState{}, false
	}
	return PipelineServerState(*found, ""), true
}
