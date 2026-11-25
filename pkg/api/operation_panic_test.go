package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const updateStreamBody = `{"image":"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.4-nightly"}`

// sseSteps parses the data frames of an SSE body.
func sseSteps(t *testing.T, body string) []UpdateStep {
	t.Helper()
	var steps []UpdateStep
	for _, frame := range strings.Split(body, "\n\n") {
		data, ok := strings.CutPrefix(frame, "data: ")
		if !ok {
			continue
		}
		var s UpdateStep
		if err := json.Unmarshal([]byte(data), &s); err != nil {
			t.Fatalf("bad frame %q: %v", frame, err)
		}
		steps = append(steps, s)
	}
	return steps
}

// captureLog sends slog's default logger to a buffer for one test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// serveUpdateStream runs POST /api/update/stream with run as the update.
func serveUpdateStream(t *testing.T, run func(emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error)) *httptest.ResponseRecorder {
	t.Helper()
	setupDevMode(t)
	allowMutations(t)
	runUpdateStream = func(_ *cluster.Client, _ string, _ cluster.OperationOptions, emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		return run(emit)
	}
	mux := http.NewServeMux()
	Register(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON(t, "/api/update/stream", "user:admin", updateStreamBody))
	return w
}

// What a stream does today and must keep doing: its steps, then exactly
// one operation_complete with the outcome; the user's slot is used (the
// change was accepted, whatever its outcome) and the cluster lock is free.
func TestStreamOutcomesArePinned(t *testing.T) {
	for _, tc := range []struct {
		name    string
		result  *types.OperationResponse
		err     error
		status  string
		message string
		code    string
	}{
		{"success", &types.OperationResponse{Success: true, Message: "Updated"}, nil, "success", "Updated", ""},
		{"refused", &types.OperationResponse{Success: false, Message: "Blocked", ErrorCode: "prerequisites"}, nil, "failed", "Blocked", "prerequisites"},
		{"error", nil, errors.New("context deadline exceeded"), "failed", "context deadline exceeded", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := serveUpdateStream(t, func(emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
				emit(cluster.UpdateStepEvent{Step: "validate", Status: "success", Message: "ok"})
				return tc.result, tc.err
			})
			steps := sseSteps(t, w.Body.String())
			if w.Code != http.StatusOK || len(steps) != 2 || steps[0].Step != "validate" {
				t.Fatalf("status %d, steps %+v", w.Code, steps)
			}
			last := steps[1]
			if last.Step != "operation_complete" || last.Status != tc.status || last.Message != tc.message || last.ErrorCode != tc.code {
				t.Fatalf("final event %+v", last)
			}
			if !mutationLimiter.isRateLimited("admin:/api/update/stream") {
				t.Fatal("an accepted change did not use the slot")
			}
			if locks.clusterHeld() {
				t.Fatal("the cluster lock is still held")
			}
		})
	}
}

// A panic in a stream's operation is logged with its stack, ends the
// stream with a failed operation_complete (errorCode internal), frees the
// cluster lock and gives the user's slot back (mutationAuth: "handlers
// that panicked do not use up the user's slot").
func TestStreamPanicEndsTheStreamAndFreesTheSlot(t *testing.T) {
	logs := captureLog(t)
	w := serveUpdateStream(t, func(emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		emit(cluster.UpdateStepEvent{Step: "validate", Status: "success", Message: "ok"})
		panic(errors.New("models: index out of range [3] with length 3"))
	})
	steps := sseSteps(t, w.Body.String())
	if len(steps) == 0 || steps[len(steps)-1].Step != "operation_complete" {
		t.Errorf("no final event; the stream was: %+v", steps)
	} else if last := steps[len(steps)-1]; last.Status != "failed" || last.ErrorCode != "internal" || !strings.Contains(last.Message, "internal error") {
		t.Errorf("final event %+v", last)
	}
	if mutationLimiter.isRateLimited("admin:/api/update/stream") {
		t.Error("the user's slot is still taken after a panic")
	}
	if locks.clusterHeld() {
		t.Error("the cluster lock is still held")
	}
	if !strings.Contains(logs.String(), "index out of range [3]") || !strings.Contains(logs.String(), "goroutine") {
		t.Errorf("the log has no panic stack: %s", logs.String())
	}
}

// Every stream of a change goes through streamOperation, and no handler
// recovers a panic on its own: the stack, the final event and the slot
// are handled in one place (operation_stream.go).
func TestStreamsUseStreamOperation(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "operation_stream.go" || f == "sse.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"recover()", "NewSSEWriter("} {
			if strings.Contains(string(src), banned) {
				t.Errorf("%s calls %s: use streamOperation or recoverOperation (operation_stream.go)", f, banned)
			}
		}
	}
}

// The update dry run (not a stream) answers a panic with 500 internal JSON
// instead of an empty 200, logs the stack and gives the slot back.
func TestUpdateDryRunPanicAnswers500(t *testing.T) {
	logs := captureLog(t)
	setupDevMode(t)
	allowMutations(t)
	runUpdateDryRun = func(*cluster.Client, string) (*types.OperationResponse, error) {
		panic("dry run bug")
	}
	mux := http.NewServeMux()
	Register(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON(t, "/api/update", "user:admin", `{"image":"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.4-nightly","dryRun":true}`))
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), `"errorCode":"internal"`) {
		t.Errorf("status %d body %q", w.Code, w.Body.String())
	}
	if mutationLimiter.isRateLimited("admin:/api/update") {
		t.Error("the slot is taken")
	}
	if !strings.Contains(logs.String(), "dry run bug") || !strings.Contains(logs.String(), "goroutine") {
		t.Errorf("the log has no panic stack: %s", logs.String())
	}
}
