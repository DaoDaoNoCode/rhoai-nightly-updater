package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func getOperation(t *testing.T) (OperationStatus, map[string]json.RawMessage) {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/operation", nil)
	r.Header.Set("X-Forwarded-Access-Token", "user:bob")
	HandleOperation(w, r)
	if w.Code != 200 {
		t.Fatalf("GET /api/operation: %d %s", w.Code, w.Body.String())
	}
	var status OperationStatus
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	return status, raw
}

// recordMarkerWrites replaces the marker seams with recorders.
func recordMarkerWrites(t *testing.T) (*sync.Mutex, *[]*types.OperationMarker, *[]*types.CompletedOperation) {
	t.Helper()
	var mu sync.Mutex
	var saved []*types.OperationMarker
	var completed []*types.CompletedOperation
	saveOperationMarker = func(_ *cluster.Client, m *types.OperationMarker) error {
		mu.Lock()
		defer mu.Unlock()
		saved = append(saved, m)
		return nil
	}
	saveCompletedOperation = func(_ *cluster.Client, done *types.CompletedOperation, _ *cluster.LeaseOwner) error {
		mu.Lock()
		defer mu.Unlock()
		completed = append(completed, done)
		return nil
	}
	return &mu, &saved, &completed
}

func refreshResult(result *types.OperationResponse, err error) {
	runRefreshStream = func(*cluster.Client, cluster.OperationOptions, func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		return result, err
	}
}

// TestLastCompletedContract pins the lastCompleted object of
// GET /api/operation that the frontend reads.
func TestLastCompletedContract(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	mu, _, completed := recordMarkerWrites(t)

	if status, raw := getOperation(t); status.LastCompleted != nil || raw["lastCompleted"] != nil {
		t.Fatalf("lastCompleted before any operation: %+v", status.LastCompleted)
	}

	refreshResult(&types.OperationResponse{Success: false, Message: "The CSV did not become ready"}, nil)
	w := httptest.NewRecorder()
	HandleRefreshStream(w, postJSON("/api/refresh/stream", "user:alice", "{}"))
	if w.Code != 200 {
		t.Fatalf("refresh: %d", w.Code)
	}

	status, raw := getOperation(t)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw["lastCompleted"], &fields); err != nil {
		t.Fatalf("lastCompleted: %s", raw["lastCompleted"])
	}
	var keys []string
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got := strings.Join(keys, ","); got != "finishedAt,id,label,message,startedAt,success,target,type,user" {
		t.Fatalf("lastCompleted keys = %s", got)
	}
	if string(fields["success"]) != "false" {
		t.Fatalf("success = %s", fields["success"])
	}
	last := status.LastCompleted
	if last.Type != "refresh" || last.Label != "Refresh operator" || last.User != "alice" || last.Success ||
		last.Message != "The CSV did not become ready" || last.ID == "" || status.InProgress || status.Operation != nil {
		t.Fatalf("lastCompleted = %+v", last)
	}
	for _, ts := range []string{last.StartedAt, last.FinishedAt} {
		if _, err := time.Parse(time.RFC3339, ts); err != nil {
			t.Fatalf("time %q: %v", ts, err)
		}
	}
	mu.Lock()
	if len(*completed) != 1 || *(*completed)[0] != *last {
		t.Fatalf("persisted %+v, reported %+v", *completed, last)
	}
	mu.Unlock()

	// A later success replaces it.
	refreshResult(&types.OperationResponse{Success: true, Message: "Operator refreshed"}, nil)
	mutationLimiter = newRateLimiter(30 * time.Second)
	HandleRefreshStream(httptest.NewRecorder(), postJSON("/api/refresh/stream", "user:carol", "{}"))
	if status, _ := getOperation(t); status.LastCompleted == nil || !status.LastCompleted.Success ||
		status.LastCompleted.User != "carol" || status.LastCompleted.ID == last.ID {
		t.Fatalf("after a second operation: %+v", status.LastCompleted)
	}
}

// TestLastCompletedSurvivesRestart reads the persisted lastCompleted when
// this process has not finished an operation yet, and prefers its own.
func TestLastCompletedSurvivesRestart(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	recordMarkerWrites(t)
	persisted := &types.CompletedOperation{ID: "before-restart", Type: "update", Label: "Update to nightly", User: "alice",
		Target: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6", StartedAt: "2026-10-05T10:00:00Z", FinishedAt: "2026-10-05T10:05:00Z", Success: true, Message: "done"}
	reads := 0
	readOperationState = func(*cluster.Client) (*cluster.OperationRecord, error) {
		reads++
		return &cluster.OperationRecord{LastCompleted: persisted}, nil
	}
	if status, _ := getOperation(t); status.LastCompleted == nil || *status.LastCompleted != *persisted || status.Interrupted != nil {
		t.Fatalf("after restart: %+v", status)
	}

	// While an operation runs, the remembered one is reported without
	// reading the ConfigMap on every poll.
	release := make(chan struct{})
	started := make(chan struct{})
	runRefreshStream = func(*cluster.Client, cluster.OperationOptions, func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		close(started)
		<-release
		return &types.OperationResponse{Success: true, Message: "refreshed"}, nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		HandleRefreshStream(httptest.NewRecorder(), postJSON("/api/refresh/stream", "user:dave", "{}"))
	}()
	<-started
	before := reads
	status, _ := getOperation(t)
	if !status.InProgress || status.LastCompleted == nil || status.LastCompleted.ID != "before-restart" || reads != before {
		t.Fatalf("during an operation: %+v (reads %d -> %d)", status, before, reads)
	}
	close(release)
	<-done
	if status, _ := getOperation(t); status.LastCompleted == nil || status.LastCompleted.User != "dave" {
		t.Fatalf("own operation not preferred: %+v", status.LastCompleted)
	}
}

// TestRejectedAndDryRunRequestsWriteNoMarker: the marker is written only
// for validated requests that change the cluster.
func TestRejectedAndDryRunRequestsWriteNoMarker(t *testing.T) {
	setupDevMode(t)
	stubOperations(t)
	allowMutations(t)
	mu, saved, completed := recordMarkerWrites(t)
	runUpdateDryRun = func(*cluster.Client, string) (*types.OperationResponse, error) {
		return &types.OperationResponse{Success: true, Message: "Dry run complete"}, nil
	}
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
		path string
		body string
		code int
	}{
		{"invalid image", HandleUpdateStream, "/api/update/stream", `{"image":"docker.io/evil/fbc:1"}`, 400},
		{"malformed body", HandleReinstallStream, "/api/rollback/stream", `{`, 400},
		{"bad PR", HandleDashboardDeployPR, "/api/dashboard/deploy-pr", `{"pr":0}`, 400},
		{"bad project", HandlePipelineServerSetup, "/api/resources/pipeline-server/setup", `{"project":"Not_Valid"}`, 400},
		{"dry run", HandleUpdate, "/api/update", `{"image":"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6","dryRun":true}`, 200},
	} {
		w := httptest.NewRecorder()
		tc.h(w, postJSON(tc.path, "user:alice", tc.body))
		if w.Code != tc.code {
			t.Errorf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*saved) != 0 || len(*completed) != 0 {
		t.Fatalf("marker writes for rejected or dry-run requests: saved %d, completed %d", len(*saved), len(*completed))
	}
	if inflight.lastCompleted() != nil {
		t.Fatalf("lastCompleted = %+v", inflight.lastCompleted())
	}
}

// TestFailedMarkerClearIsRetried: a failed clear neither reports the
// operation as interrupted to this process nor stays failed.
func TestFailedMarkerClearIsRetried(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	markers.mu.Lock()
	markers.delays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	markers.mu.Unlock()

	var mu sync.Mutex
	var marker *types.OperationMarker
	attempts, failures := 0, 2
	saveOperationMarker = func(_ *cluster.Client, m *types.OperationMarker) error {
		mu.Lock()
		defer mu.Unlock()
		marker = m
		return nil
	}
	saveCompletedOperation = func(*cluster.Client, *types.CompletedOperation, *cluster.LeaseOwner) error {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts <= failures {
			return errors.New("etcdserver: request timed out")
		}
		marker = nil
		return nil
	}
	readOperationState = func(*cluster.Client) (*cluster.OperationRecord, error) {
		mu.Lock()
		defer mu.Unlock()
		return &cluster.OperationRecord{Marker: marker}, nil
	}

	refreshResult(&types.OperationResponse{Success: true, Message: "refreshed"}, nil)
	HandleRefreshStream(httptest.NewRecorder(), postJSON("/api/refresh/stream", "user:alice", "{}"))
	// The marker of this process is not an interruption, even while the
	// clear is still failing.
	if status, _ := getOperation(t); status.Interrupted != nil {
		t.Fatalf("own leftover marker reported as interrupted: %+v", status.Interrupted)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		cleared, n := marker == nil, attempts
		mu.Unlock()
		if cleared {
			if n != failures+1 {
				t.Fatalf("attempts = %d", n)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("marker not cleared after %d attempts", n)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Retries that run out are restarted by GET /api/operation, and the
	// shutdown flush makes a last attempt.
	mu.Lock()
	attempts, failures = 0, 100
	mu.Unlock()
	mutationLimiter = newRateLimiter(30 * time.Second)
	HandleRefreshStream(httptest.NewRecorder(), postJSON("/api/refresh/stream", "user:alice", "{}"))
	waitFor(t, func() bool {
		markers.mu.Lock()
		defer markers.mu.Unlock()
		return !markers.retrying
	})
	mu.Lock()
	n := attempts
	mu.Unlock()
	if n != 4 { // the first write and three retries
		t.Fatalf("attempts before giving up = %d", n)
	}
	getOperation(t)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return attempts > n })
	waitFor(t, func() bool {
		markers.mu.Lock()
		defer markers.mu.Unlock()
		return !markers.retrying
	})
	mu.Lock()
	failures = 0
	mu.Unlock()
	FlushOperationMarker(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if marker != nil {
		t.Fatal("flush did not clear the marker")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestNewOperationSupersedesPendingClear: a retry must never clear the
// marker of a newer operation.
func TestNewOperationSupersedesPendingClear(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	markers.mu.Lock()
	markers.delays = []time.Duration{20 * time.Millisecond}
	markers.mu.Unlock()
	var mu sync.Mutex
	var completedIDs []string
	fail := true
	saveCompletedOperation = func(_ *cluster.Client, done *types.CompletedOperation, _ *cluster.LeaseOwner) error {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return errors.New("unavailable")
		}
		completedIDs = append(completedIDs, done.ID)
		return nil
	}
	refreshResult(&types.OperationResponse{Success: true, Message: "first"}, nil)
	HandleRefreshStream(httptest.NewRecorder(), postJSON("/api/refresh/stream", "user:alice", "{}"))
	first := inflight.lastCompleted().ID

	release, started := make(chan struct{}), make(chan struct{})
	runRefreshStream = func(*cluster.Client, cluster.OperationOptions, func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		close(started)
		<-release
		return &types.OperationResponse{Success: true, Message: "second"}, nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		HandleRefreshStream(httptest.NewRecorder(), postJSON("/api/refresh/stream", "user:bob", "{}"))
	}()
	<-started
	mu.Lock()
	fail = false
	mu.Unlock()
	time.Sleep(60 * time.Millisecond) // the retry of the first operation would run now
	mu.Lock()
	if len(completedIDs) != 0 {
		t.Fatalf("a retry cleared the running operation's marker: %v (first %s)", completedIDs, first)
	}
	mu.Unlock()
	close(release)
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(completedIDs) != 1 || completedIDs[0] == first {
		t.Fatalf("completions = %v", completedIDs)
	}
}

func TestOperationOutcome(t *testing.T) {
	for _, tc := range []struct {
		name      string
		op        Operation
		status    int
		body      string
		streaming bool
		success   bool
		message   string
	}{
		{"stream success", Operation{Step: "operation_complete", StepStatus: "success", Message: "Updated"}, 200, "", true, true, "Updated"},
		{"stream failure", Operation{Step: "operation_complete", StepStatus: "failed", Message: "CSV failed"}, 200, "", true, false, "CSV failed"},
		{"stream without a result", Operation{Step: "wait_csv", StepStatus: "running", Message: "Waiting"}, 200, "", true, false, "The operation ended without reporting a result."},
		{"operation response", Operation{}, 200, `{"success":true,"message":"MinIO is ready","logs":[]}`, false, true, "MinIO is ready"},
		{"failed operation response", Operation{}, 422, `{"success":false,"message":"No DSC"}`, false, false, "No DSC"},
		{"200 with success false", Operation{}, 200, `{"success":false,"message":"Fix not applied"}`, false, false, "Fix not applied"},
		{"error", Operation{}, 500, `{"error":"S3 storage setup failed","errorCode":"internal"}`, false, false, "S3 storage setup failed"},
		{"no body", Operation{}, 204, "", false, true, "Completed."},
		{"no body, failed", Operation{}, 502, "", false, false, "Failed (HTTP 502)."},
		{"no response (panic)", Operation{}, 0, "", false, false, "The operation ended without reporting a result."},
	} {
		op := tc.op
		op.sw = &statusWriter{status: tc.status, body: []byte(tc.body), streaming: tc.streaming}
		if success, message := operationOutcome(&op); success != tc.success || message != tc.message {
			t.Errorf("%s: %v %q", tc.name, success, message)
		}
	}
}

func TestStatusWriterCapturesBoundedJSONBody(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec}
	writeOperationResult(sw, &types.OperationResponse{Success: true, Message: strings.Repeat("x", 2*maxCapturedResponse)}, "test")
	if sw.status != 200 || sw.streaming || len(sw.body) != maxCapturedResponse || rec.Body.Len() <= maxCapturedResponse {
		t.Fatalf("status %d streaming %v captured %d written %d", sw.status, sw.streaming, len(sw.body), rec.Body.Len())
	}

	rec = httptest.NewRecorder()
	sw = &statusWriter{ResponseWriter: rec}
	s, _ := NewSSEWriter(sw)
	_ = s.SendStep(UpdateStep{Step: "x", Status: "running"})
	if !sw.streaming || len(sw.body) != 0 {
		t.Fatalf("stream captured: streaming %v body %q", sw.streaming, sw.body)
	}
}

func TestTruncateTextKeepsCharactersWhole(t *testing.T) {
	if got := truncateText("aé", 2); got != "a" {
		t.Fatalf("got %q", got)
	}
	if got := truncateText("short", 10); got != "short" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("ü", 600)
	if got := truncateText(long, maxOperationMessageBytes); len(got) > maxOperationMessageBytes || !strings.HasPrefix(long, got) {
		t.Fatalf("len %d", len(got))
	}
}
