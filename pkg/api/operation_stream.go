package api

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// operationPanicMessage is the outcome of an operation that panicked.
const operationPanicMessage = "The operation stopped on an internal error in the updater, so how far it got is not known. " +
	"Check what it changed before you retry; the updater's log (oc logs deploy/rhoai-nightly-updater -c app) has the details."

// errOperationPanicked is streamOperation's error when the operation
// panicked.
var errOperationPanicked = errors.New("the operation panicked")

// streamOperation runs an accepted change as a Server-Sent Events stream:
// the steps run emits, a heartbeat every 15 s, then run's outcome as the
// final operation_complete event. A panic in run is recovered here
// (reportOperationPanic): the stream still ends with a failed
// operation_complete, and streamOperation returns errOperationPanicked.
func streamOperation(w http.ResponseWriter, label string, run func(emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error)) (result *types.OperationResponse, err error) {
	sseWriter, err := NewSSEWriter(w)
	if err != nil {
		writeError(w, "streaming not supported", http.StatusInternalServerError)
		return nil, err
	}
	defer sseWriter.Close()
	done := make(chan struct{})
	defer close(done)
	go sseHeartbeat(sseWriter, done)
	// Deferred last, so it runs before the writer is closed.
	defer func() {
		if rec := recover(); rec != nil {
			reportOperationPanic(w, label, rec, sseWriter)
			result, err = nil, errOperationPanicked
		}
	}()
	result, err = run(forwardSteps(sseWriter))
	sendOperationResult(sseWriter, result, err)
	return result, err
}

// recoverOperation is deferred by a change handler that does not stream:
// a panic is reported (reportOperationPanic) with a 500 "internal" answer
// when nothing was written yet. It must be deferred directly.
func recoverOperation(w http.ResponseWriter, label string) {
	if rec := recover(); rec != nil {
		reportOperationPanic(w, label, rec, nil)
	}
}

// reportOperationPanic handles a recovered panic of an accepted change:
// it logs the panic with its stack (which the recover would otherwise
// lose), tells the client (the stream's final event, or a 500 "internal"
// answer when nothing was written), and marks the request so mutationAuth
// gives the user's rate-limit slot back. The cluster lock is released by
// the handler's deferred releaseClusterMutationLock as on any return.
func reportOperationPanic(w http.ResponseWriter, label string, rec any, sse *SSEWriter) {
	slog.Error("panic in operation handler", "op", label, "panic", rec, "stack", string(debug.Stack()))
	sw, _ := w.(*statusWriter)
	if sw != nil {
		sw.panicked = true
	}
	switch {
	case sse != nil:
		sendOperationResult(sse, &types.OperationResponse{Success: false, Message: operationPanicMessage, ErrorCode: "internal"}, nil)
	case sw == nil || sw.status == 0:
		writeError(w, operationPanicMessage, http.StatusInternalServerError, "internal")
	}
}

// recoverRun is deferred directly by a goroutine that runs an operation
// with no request (a recipe run): a panic is logged with its stack and
// onPanic ends the operation (its outcome, the lock).
func recoverRun(label string, onPanic func()) {
	if rec := recover(); rec != nil {
		slog.Error("panic in operation", "op", label, "panic", rec, "stack", string(debug.Stack()))
		onPanic()
	}
}
