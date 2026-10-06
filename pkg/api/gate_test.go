package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func allowMutations(t *testing.T) {
	t.Helper()
	mutationPermission = func(context.Context, string) (bool, error) { return true, nil }
}

func postJSON(path, token, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("X-Forwarded-Access-Token", token)
	}
	return r
}

func decodeError(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %q", w.Body.String())
	}
	return body
}

// TestAuthGate covers both wrappers: no token, a token the API server
// rejects (expired session) and an API server that cannot be asked.
func TestAuthGate(t *testing.T) {
	for _, tc := range []struct {
		name, token string
		status      int
		errorCode   string
	}{
		{"no token", "", 401, "unauthorized"},
		{"expired session", expiredToken, 401, "session_expired"},
		{"api unreachable", apiDownToken, 503, "authorization_unavailable"},
		{"valid", "user:alice", 200, ""},
	} {
		for _, wrapper := range []string{"withAuth", "withMutationAuth"} {
			t.Run(tc.name+"/"+wrapper, func(t *testing.T) {
				setupDevMode(t)
				t.Setenv("DEV_TOKEN", "") // no dev fallback for the user token
				t.Setenv("DEV_MODE", "")
				cachedTokenMu.Lock()
				cachedTokenValue, cachedTokenAt = "sa-token", time.Now()
				cachedTokenMu.Unlock()
				t.Cleanup(func() {
					cachedTokenMu.Lock()
					cachedTokenValue = ""
					cachedTokenMu.Unlock()
				})
				allowMutations(t)
				var gotUser string
				fn := func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
					gotUser = resolveUsername(r)
					w.WriteHeader(http.StatusOK)
				}
				h := withAuth(fn)
				if wrapper == "withMutationAuth" {
					h = withMutationAuth(fn)
				}
				r := postJSON("/api/gate-"+strings.ReplaceAll(tc.name, " ", "-"), tc.token, "{}")
				// A spoofed header never overrides the verified identity.
				r.Header.Set("X-Forwarded-User", "mallory")
				w := httptest.NewRecorder()
				h(w, r)
				if w.Code != tc.status {
					t.Fatalf("status = %d, want %d (%s)", w.Code, tc.status, w.Body.String())
				}
				if tc.errorCode != "" && decodeError(t, w)["errorCode"] != tc.errorCode {
					t.Fatalf("errorCode = %v, want %s", decodeError(t, w)["errorCode"], tc.errorCode)
				}
				if tc.status == 200 && gotUser != "alice" {
					t.Fatalf("handler saw user %q, want the token owner alice", gotUser)
				}
			})
		}
	}
}

func TestMutationPermissionErrorsMapToSessionExpiredOr503(t *testing.T) {
	setupDevMode(t)
	for _, tc := range []struct {
		err       error
		status    int
		errorCode string
	}{
		{nil, 403, "forbidden"},
		{errors.New("review failed: " + cluster.ErrUnauthenticated.Error()), 503, "authorization_unavailable"},
		{errors.Join(errors.New("review failed"), cluster.ErrUnauthenticated), 401, "session_expired"},
		{errors.New("connection refused"), 503, "authorization_unavailable"},
	} {
		mutationPermission = func(context.Context, string) (bool, error) { return false, tc.err }
		for name, h := range map[string]http.HandlerFunc{
			"mutation":    withMutationAuth(func(*cluster.Client, http.ResponseWriter, *http.Request) { t.Error("handler called") }),
			"permissions": HandleUserPermissions,
		} {
			w := httptest.NewRecorder()
			h(w, postJSON("/api/perm-"+name, "user:bob", "{}"))
			want := tc.status
			if name == "permissions" && tc.err == nil {
				want = 200 // {"canMutate":false}
			}
			if w.Code != want {
				t.Errorf("%s with %v: status %d, want %d", name, tc.err, w.Code, want)
			}
			if want != 200 && decodeError(t, w)["errorCode"] != tc.errorCode {
				t.Errorf("%s with %v: %s", name, tc.err, w.Body.String())
			}
		}
	}
}

// TestMutationGateAsksForClusterAdmin pins the permission the gate asks
// the API server about: "*" on "*" in all namespaces, with the user's token.
// (pkg/cluster's authz tests pin the SubjectAccessReview body itself.)
func TestMutationGateAsksForClusterAdmin(t *testing.T) {
	setupDevMode(t)
	t.Setenv("DEV_MODE", "")
	var got []string
	checkPermission = func(_ context.Context, token, verb, resource, group, namespace string) (bool, error) {
		got = []string{token, verb, resource, group, namespace}
		return false, nil
	}
	allowed, err := mutationPermission(context.Background(), "the-user-token")
	if err != nil || allowed {
		t.Fatalf("allowed=%v err=%v", allowed, err)
	}
	if strings.Join(got, "|") != "the-user-token|*|*|*|" {
		t.Fatalf("permission asked: %q", got)
	}
}

func TestImageAllowlistOnEveryImageEndpoint(t *testing.T) {
	digest := strings.Repeat("a", 64)
	bad := []string{
		"docker.io/evil/fbc:1",
		"quay.io/evil/fbc:1",
		"quay.io.evil.example/rhoai/fbc:1",
		"quay.io/rhoaix/fbc:1",
		"registry.redhat.io/other/fbc:1",
	}
	good := "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + digest
	for _, image := range append(bad, good) {
		if got := isAllowedImage(image); got != (image == good) {
			t.Errorf("isAllowedImage(%q) = %v", image, got)
		}
	}
	for _, path := range []string{"/api/update", "/api/update/stream"} {
		for _, image := range bad {
			setupDevMode(t)
			allowMutations(t)
			called := false
			runUpdateStream = func(*cluster.Client, string, cluster.OperationOptions, func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
				called = true
				return &types.OperationResponse{Success: true}, nil
			}
			runUpdateDryRun = func(*cluster.Client, string) (*types.OperationResponse, error) {
				called = true
				return &types.OperationResponse{Success: true}, nil
			}
			h := HandleUpdate
			if path == "/api/update/stream" {
				h = HandleUpdateStream
			}
			w := httptest.NewRecorder()
			h(w, postJSON(path, "tok", `{"image":"`+image+`","dryRun":true}`))
			if w.Code != 400 || called {
				t.Errorf("%s %s: status %d called=%v", path, image, w.Code, called)
			}
		}
	}
	setupDevMode(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/build-explorer/content?image=docker.io/evil/fbc:1", nil)
	r.Header.Set("X-Forwarded-Access-Token", "tok")
	HandleBuildExplorerContent(w, r)
	if w.Code != 400 {
		t.Errorf("build explorer accepted a foreign registry: %d", w.Code)
	}
}

func stubOperations(t *testing.T) {
	t.Helper()
	origU, origD, origR, origF := runUpdateStream, runUpdateDryRun, runReinstallStream, runRefreshStream
	t.Cleanup(func() {
		runUpdateStream, runUpdateDryRun, runReinstallStream, runRefreshStream = origU, origD, origR, origF
	})
}

func TestUpdateEndpointOnlyRunsDryRuns(t *testing.T) {
	setupDevMode(t)
	stubOperations(t)
	allowMutations(t)
	image := "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6"
	calls := 0
	runUpdateDryRun = func(_ *cluster.Client, got string) (*types.OperationResponse, error) {
		calls++
		if got != image {
			t.Errorf("image %q", got)
		}
		return &types.OperationResponse{Success: true, Message: "Dry run complete"}, nil
	}
	w := httptest.NewRecorder()
	HandleUpdate(w, postJSON("/api/update", "tok", `{"image":"`+image+`","dryRun":false}`))
	if w.Code != 400 || calls != 0 {
		t.Fatalf("non-dry-run update: status %d calls %d", w.Code, calls)
	}
	for i := 0; i < 2; i++ { // dry runs do not use up the rate limit
		w = httptest.NewRecorder()
		HandleUpdate(w, postJSON("/api/update", "tok", `{"image":"`+image+`","dryRun":true}`))
		if w.Code != 200 {
			t.Fatalf("dry run %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	if calls != 2 {
		t.Fatalf("dry run calls = %d", calls)
	}
	if clusterMutationInProgress.Load() {
		t.Fatal("lock still held after the dry run")
	}
}

// TestBusyClusterReturnsRunningOperation runs a stream operation, checks
// that GET /api/operation and a competing mutation both report it, and that
// everything is released afterwards.
func TestBusyClusterReturnsRunningOperation(t *testing.T) {
	setupDevMode(t)
	stubOperations(t)
	allowMutations(t)
	t.Setenv("HOSTNAME", "updater-pod-1")
	var saved []*types.OperationMarker
	var completed []*types.CompletedOperation
	var mu sync.Mutex
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

	image := "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6"
	stepped, release := make(chan struct{}), make(chan struct{})
	runUpdateStream = func(_ *cluster.Client, _ string, _ cluster.OperationOptions, emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		emit(cluster.UpdateStepEvent{Step: "wait_catalog_ready", Status: "running", Message: "Waiting for the catalog"})
		close(stepped)
		<-release
		return &types.OperationResponse{Success: true, Message: "done"}, nil
	}
	done := make(chan *httptest.ResponseRecorder)
	go func() {
		w := httptest.NewRecorder()
		HandleUpdateStream(w, postJSON("/api/update/stream", "user:alice", `{"image":"`+image+`"}`))
		done <- w
	}()
	select {
	case <-stepped:
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not start")
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/operation", nil)
	r.Header.Set("X-Forwarded-Access-Token", "user:bob")
	HandleOperation(w, r)
	var status OperationStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || w.Code != 200 {
		t.Fatalf("GET /api/operation: %d %s", w.Code, w.Body.String())
	}
	op := status.Operation
	if !status.InProgress || op == nil || op.Type != "update" || op.User != "alice" || op.Target != image ||
		op.Step != "wait_catalog_ready" || op.StepStatus != "running" || op.Message != "Waiting for the catalog" || op.StartedAt.IsZero() {
		t.Fatalf("operation = %+v", status)
	}

	w = httptest.NewRecorder()
	HandleDashboardRevert(w, postJSON("/api/dashboard/revert", "user:bob", "{}"))
	body := decodeError(t, w)
	busyOp, _ := body["operation"].(map[string]interface{})
	if w.Code != 409 || body["errorCode"] != "cluster_busy" || busyOp["user"] != "alice" || busyOp["type"] != "update" {
		t.Fatalf("busy response: %d %s", w.Code, w.Body.String())
	}
	// The refused request does not use up bob's rate-limit slot.
	if mutationLimiter.isRateLimited("bob:/api/dashboard/revert") {
		t.Fatal("a 409 consumed the rate-limit slot")
	}

	close(release)
	select {
	case w = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not finish")
	}
	if !strings.Contains(w.Body.String(), `"step":"operation_complete","status":"success"`) {
		t.Fatalf("stream: %s", w.Body.String())
	}
	if clusterMutationInProgress.Load() || inflight.snapshot() != nil {
		t.Fatal("lock or operation not released")
	}
	mu.Lock()
	defer mu.Unlock()
	// One marker, written once the request was validated, naming the image
	// and this process; one completion, which clears it.
	if len(saved) != 1 || saved[0].User != "alice" || saved[0].Pod != "updater-pod-1" || saved[0].Target != image ||
		saved[0].BootID != bootID || saved[0].ID == "" {
		t.Fatalf("marker saved=%+v", saved)
	}
	if len(completed) != 1 || completed[0].ID != saved[0].ID || !completed[0].Success || completed[0].Message != "done" ||
		completed[0].Target != image || completed[0].Type != "update" || completed[0].FinishedAt == "" {
		t.Fatalf("completed=%+v", completed)
	}
}

func TestLockReleasedWhenOperationPanics(t *testing.T) {
	setupDevMode(t)
	stubOperations(t)
	allowMutations(t)
	runRefreshStream = func(*cluster.Client, cluster.OperationOptions, func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		panic("boom")
	}
	w := httptest.NewRecorder()
	HandleRefreshStream(w, postJSON("/api/refresh/stream", "tok", "{}"))
	if clusterMutationInProgress.Load() || inflight.snapshot() != nil {
		t.Fatal("lock held after a panic")
	}
}

func TestInterruptedOperationReportedAfterRestart(t *testing.T) {
	setupDevMode(t)
	t.Setenv("HOSTNAME", "same-pod")
	recent := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	marker := &types.OperationMarker{Type: "reinstall", Label: "Reinstall operator", User: "alice", StartedAt: recent, Pod: "old-pod", BootID: "old-boot"}
	for _, tc := range []struct {
		name   string
		marker *types.OperationMarker
		err    error
		want   bool
	}{
		{"left by a dead pod", marker, nil, true},
		// The kubelet restarted the container (OOM kill, liveness failure):
		// same pod name, new process.
		{"container restarted in the same pod", &types.OperationMarker{Pod: "same-pod", BootID: "old-boot", StartedAt: recent}, nil, true},
		{"written by an older version", &types.OperationMarker{Pod: "same-pod", StartedAt: recent}, nil, true},
		{"none", nil, nil, false},
		{"this process", &types.OperationMarker{Pod: "same-pod", BootID: bootID, StartedAt: recent}, nil, false},
		{"too old", &types.OperationMarker{Pod: "old-pod", BootID: "old-boot", StartedAt: time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)}, nil, false},
		{"unreadable", nil, errors.New("forbidden"), false},
	} {
		readOperationState = func(*cluster.Client) (*cluster.OperationRecord, error) {
			if tc.err != nil {
				return nil, tc.err
			}
			return &cluster.OperationRecord{Marker: tc.marker}, nil
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/operation", nil)
		r.Header.Set("X-Forwarded-Access-Token", "tok")
		HandleOperation(w, r)
		var status OperationStatus
		_ = json.Unmarshal(w.Body.Bytes(), &status)
		if w.Code != 200 || (status.Interrupted != nil) != tc.want || status.InProgress {
			t.Errorf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
	}
}

func TestConcurrentDuplicateMutationsRunOnce(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	var runs int
	var mu sync.Mutex
	release := make(chan struct{})
	h := withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		runs++
		mu.Unlock()
		<-release
		w.WriteHeader(http.StatusOK)
	})
	codes := make(chan int, 3)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			h(w, postJSON("/api/dup", "user:alice", "{}"))
			codes <- w.Code
		}()
	}
	// Two duplicates are refused while the first one still runs.
	deadline := time.After(5 * time.Second)
	refused := 0
	for refused < 2 {
		select {
		case c := <-codes:
			if c != http.StatusTooManyRequests {
				t.Fatalf("duplicate got %d", c)
			}
			refused++
		case <-deadline:
			t.Fatal("duplicates were not refused while the first request ran")
		}
	}
	close(release)
	wg.Wait()
	if c := <-codes; c != 200 || runs != 1 {
		t.Fatalf("first request %d, runs %d", c, runs)
	}
	// Another user is not affected.
	w := httptest.NewRecorder()
	release2 := make(chan struct{})
	close(release2)
	withMutationAuth(func(*cluster.Client, http.ResponseWriter, *http.Request) {})(w, postJSON("/api/dup", "user:bob", "{}"))
	if w.Code != 200 {
		t.Fatalf("other user: %d", w.Code)
	}
}

func TestRateLimitSlotReturnedOnFailure(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	h := withMutationAuth(func(_ *cluster.Client, w http.ResponseWriter, _ *http.Request) {
		writeError(w, "bad input", http.StatusBadRequest, "validation")
	})
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		h(w, postJSON("/api/fails", "tok", "{}"))
		if w.Code != 400 {
			t.Fatalf("attempt %d: %d (a failed request must not block a retry)", i, w.Code)
		}
	}
}

func TestRateLimiterWindow(t *testing.T) {
	rl := newRateLimiter(20 * time.Millisecond)
	slot, ok := rl.tryAcquire("a")
	if !ok {
		t.Fatal("first acquire refused")
	}
	if _, ok := rl.tryAcquire("a"); ok {
		t.Fatal("second acquire within the window allowed")
	}
	if _, ok := rl.tryAcquire("b"); !ok {
		t.Fatal("other key limited")
	}
	time.Sleep(30 * time.Millisecond)
	newSlot, ok := rl.tryAcquire("a")
	if !ok {
		t.Fatal("acquire after the window refused")
	}
	rl.release("a", slot) // a stale release must not free the newer slot
	if !rl.isRateLimited("a") {
		t.Fatal("stale release freed a newer slot")
	}
	rl.release("a", newSlot)
	if rl.isRateLimited("a") {
		t.Fatal("release did not free the slot")
	}
}

func TestFiveFormerlyUnlockedMutationsTakeTheClusterLock(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	if !acquireClusterMutationLock() {
		t.Fatal("lock unexpectedly held")
	}
	defer releaseClusterMutationLock()
	for path, h := range map[string]http.HandlerFunc{
		"/api/setup/pull-secret":                  HandleCreatePullSecret,
		"/api/resources/minio/setup":              HandleMinIOSetup,
		"/api/resources/minio/teardown":           HandleMinIOTeardown,
		"/api/resources/pipeline-server/setup":    HandlePipelineServerSetup,
		"/api/resources/pipeline-server/teardown": HandlePipelineServerTeardown,
	} {
		w := httptest.NewRecorder()
		h(w, postJSON(path, "tok", `{"auth":"dXNlcjpwYXNz","project":"my-project"}`))
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "cluster_busy") {
			t.Errorf("%s while another operation runs: %d %s", path, w.Code, w.Body.String())
		}
	}
}

func TestReadinessCachesTheAPICheck(t *testing.T) {
	setupDevMode(t)
	calls := 0
	fail := false
	checkAPIVersion = func(context.Context, string) error {
		calls++
		if fail {
			return errors.New("429 Too Many Requests")
		}
		return nil
	}
	probe := func() int {
		w := httptest.NewRecorder()
		HandleReady(w, httptest.NewRequest("GET", "/api/health/ready", nil))
		return w.Code
	}
	for i := 0; i < 5; i++ {
		if c := probe(); c != 200 {
			t.Fatalf("probe %d: %d", i, c)
		}
	}
	if calls != 1 {
		t.Fatalf("API called %d times for 5 probes", calls)
	}
	// A failed refresh within the grace period keeps the pod ready.
	fail = true
	apiReady.mu.Lock()
	apiReady.lastCheck = time.Now().Add(-apiCheckInterval)
	apiReady.mu.Unlock()
	if c := probe(); c != 200 || calls != 2 {
		t.Fatalf("throttled check within grace: %d calls=%d", c, calls)
	}
	// Once the last success is older than the grace period it is not ready.
	apiReady.mu.Lock()
	apiReady.lastCheck = time.Now().Add(-apiCheckInterval)
	apiReady.lastSuccess = time.Now().Add(-apiReadyGrace)
	apiReady.mu.Unlock()
	if c := probe(); c != 503 {
		t.Fatalf("API down beyond grace: %d", c)
	}
}

func TestMetricLabelsAreBounded(t *testing.T) {
	setupDevMode(t)
	for i := 0; i < 50; i++ {
		RecordPageView("probe_" + strings.Repeat("x", i))
	}
	RecordFeatureUsage("teardown-minio")
	RecordFeatureUsage("dry_run")
	pageViewsMu.RLock()
	for k := range pageViews {
		if !knownPages[k] && k != "other" {
			t.Errorf("unbounded page label %q", k)
		}
	}
	pageViewsMu.RUnlock()
	featureUsageMu.RLock()
	defer featureUsageMu.RUnlock()
	if featureUsage["teardown_minio"] == nil || featureUsage["dry_run"] == nil {
		t.Error("known features (also with hyphens) must be counted under their own label")
	}
}

func TestVersionEndpoint(t *testing.T) {
	t.Setenv("TEMPLATE_REVISION", ExpectedTemplateRevision)
	w := httptest.NewRecorder()
	HandleVersion(w, httptest.NewRequest("GET", "/api/version", nil))
	var v VersionInfo
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || v.Version == "" || v.Commit == "" || v.GoVersion == "" ||
		v.TemplateRevision != ExpectedTemplateRevision || v.TemplateOutdated {
		t.Fatalf("version: %s", w.Body.String())
	}
}
