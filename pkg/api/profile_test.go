package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func TestParseProfile(t *testing.T) {
	for in, want := range map[string]Profile{"": ProfileFull, "full": ProfileFull, "gitops": ProfileGitOps} {
		if got, err := ParseProfile(in); err != nil || got != want {
			t.Errorf("ParseProfile(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"GitOps", "gitops ", "readonly", "Full"} {
		if _, err := ParseProfile(in); err == nil || !strings.Contains(err.Error(), "docs/GITOPS.md") {
			t.Errorf("ParseProfile(%q) = %v, want an error naming the profiles", in, err)
		}
	}
}

// Every route is either on the gitops profile's allowlist or refused there
// with a named feature: a new route fails here until someone decides. Both
// tables name registered routes only.
func TestEveryRouteHasAGitOpsProfileDecision(t *testing.T) {
	registered := map[string]bool{}
	for _, rt := range routes {
		registered[rt.Pattern] = true
		_, allowed := gitOpsRoutes[rt.Pattern]
		_, refused := gitOpsRefused[rt.Pattern]
		switch {
		case allowed && refused:
			t.Errorf("%s is in both gitOpsRoutes and gitOpsRefused", rt.Pattern)
		case !allowed && !refused:
			t.Errorf("%s has no gitops profile decision: add it to gitOpsRefused (with the feature the 403 names), or to gitOpsRoutes and deploy/rbac-gitops.yaml", rt.Pattern)
		}
	}
	for p := range gitOpsRoutes {
		if !registered[p] {
			t.Errorf("gitOpsRoutes lists %s, which is not a route", p)
		}
	}
	for p, feature := range gitOpsRefused {
		if !registered[p] {
			t.Errorf("gitOpsRefused lists %s, which is not a route", p)
		}
		if feature == "" {
			t.Errorf("gitOpsRefused[%s] names no feature", p)
		}
	}
}

// The allowlist stays what docs/GITOPS.md lists: a change here is a change
// of the profile's permissions (deploy/rbac-gitops.yaml) and of the docs.
func TestGitOpsProfileAllowlist(t *testing.T) {
	want := []string{
		"GET /api/activity", "GET /api/components", "GET /api/diagnostics", "GET /api/operation", "GET /api/status",
		"GET /api/update-check", "GET /api/user/permissions", "POST /api/pageview", "POST /api/update", "POST /api/update/stream",
	}
	if len(gitOpsRoutes) != len(want) {
		t.Fatalf("gitOpsRoutes has %d routes, want %v", len(gitOpsRoutes), want)
	}
	for _, p := range want {
		if _, ok := gitOpsRoutes[p]; !ok {
			t.Errorf("gitOpsRoutes lacks %s", p)
		}
	}
}

// countingAPIServer points the ServiceAccount client at a listener that
// counts connections, so a test can prove that no API call was made.
func countingAPIServer(t *testing.T) *atomic.Int32 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var n atomic.Int32
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = l.Close() })
	host, port, _ := net.SplitHostPort(l.Addr().String())
	t.Setenv("KUBERNETES_SERVICE_HOST", host)
	t.Setenv("KUBERNETES_SERVICE_PORT", port)
	return &n
}

func gitOpsMux(t *testing.T) *http.ServeMux {
	t.Helper()
	SetProfile(ProfileGitOps)
	mux := http.NewServeMux()
	Register(mux)
	return mux
}

// Every refused route answers 403 profile_gitops before anything else:
// without a token, for a cluster-admin, and without any API call (identity,
// permission or ServiceAccount).
func TestGitOpsProfileRefusesEveryOtherRoute(t *testing.T) {
	setupDevMode(t)
	calls := countingAPIServer(t)
	lookupUser = func(context.Context, string) (string, error) {
		t.Error("a refused route looked up the user")
		return "", nil
	}
	mutationPermission = func(context.Context, string) (bool, error) {
		t.Error("a refused route checked permissions")
		return true, nil
	}
	mux := gitOpsMux(t)
	for pattern, feature := range gitOpsRefused {
		method, path, _ := strings.Cut(pattern, " ")
		for _, token := range []string{"", "user:admin"} {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, postJSONMethod(method, path, token, "{}"))
			body := decodeError(t, w)
			if w.Code != http.StatusForbidden || body["errorCode"] != cluster.ErrorCodeProfileGitOps ||
				!strings.HasPrefix(body["error"].(string), feature+" is off") || !strings.Contains(body["error"].(string), "gitops profile") {
				t.Errorf("%s (token %q): %d %s", pattern, token, w.Code, w.Body.String())
			}
		}
		if clusterMutationInProgress.Load() {
			t.Fatalf("%s took the lock", pattern)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("refused routes made %d connections to the API server", n)
	}
}

// In the gitops profile every allowed read needs cluster-admin, checked with
// the user's token before the handler reaches the ServiceAccount. The
// permission probe itself stays open: it makes no ServiceAccount call.
func TestGitOpsProfileReadsNeedClusterAdmin(t *testing.T) {
	setupDevMode(t)
	calls := countingAPIServer(t)
	mutationPermission = func(context.Context, string) (bool, error) { return false, nil }
	mux := gitOpsMux(t)
	checked := 0
	for pattern := range gitOpsRoutes {
		method, path, _ := strings.Cut(pattern, " ")
		if method != http.MethodGet || path == "/api/user/permissions" {
			continue
		}
		checked++
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, postJSONMethod(method, path, "user:reader", ""))
		if body := decodeError(t, w); w.Code != http.StatusForbidden || body["errorCode"] != "forbidden" || body["error"] != gitOpsAdminMessage {
			t.Errorf("%s as a non-admin: %d %s", pattern, w.Code, w.Body.String())
		}
	}
	if checked < 6 {
		t.Fatalf("only %d reads checked", checked)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("reads refused to a non-admin made %d connections to the API server", n)
	}

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, postJSONMethod(http.MethodGet, "/api/user/permissions", "user:reader", ""))
	var perms map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &perms)
	if w.Code != http.StatusOK || perms["canMutate"] != false || perms["user"] != "reader" {
		t.Fatalf("permissions for a non-admin: %d %s", w.Code, w.Body.String())
	}

	// A permission check that cannot be answered fails closed (503), as for mutations.
	mutationPermission = func(context.Context, string) (bool, error) { return false, context.DeadlineExceeded }
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, postJSONMethod(http.MethodGet, "/api/status", "user:admin", ""))
	if body := decodeError(t, w); w.Code != http.StatusServiceUnavailable || body["errorCode"] != "authorization_unavailable" {
		t.Fatalf("status with an unanswered permission check: %d %s", w.Code, w.Body.String())
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("%d connections to the API server", n)
	}
}

// A cluster-admin's read reaches the handler.
func TestGitOpsProfileReadForClusterAdmin(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	mux := gitOpsMux(t)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, postJSONMethod(http.MethodGet, "/api/operation", "user:admin", ""))
	var op OperationStatus
	if err := json.Unmarshal(w.Body.Bytes(), &op); w.Code != http.StatusOK || err != nil {
		t.Fatalf("operation for an admin: %d %s", w.Code, w.Body.String())
	}
}

// Update in the gitops profile goes through the mutation gate and the lock
// like every mutation, and runs the profile's Update, never the full one.
func TestGitOpsProfileUpdateStream(t *testing.T) {
	setupDevMode(t)
	mux := gitOpsMux(t)

	mutationPermission = func(context.Context, string) (bool, error) { return false, nil }
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON("/api/update/stream", "user:reader", `{"image":"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6"}`))
	if w.Code != http.StatusForbidden || decodeError(t, w)["errorCode"] != "forbidden" {
		t.Fatalf("without cluster-admin: %d %s", w.Code, w.Body.String())
	}

	allowMutations(t)
	if !acquireClusterMutationLock() {
		t.Fatal("lock unexpectedly held")
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON("/api/update/stream", "user:admin", `{"image":"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6"}`))
	releaseClusterMutationLock()
	if w.Code != http.StatusConflict || decodeError(t, w)["errorCode"] != "cluster_busy" {
		t.Fatalf("while another operation runs: %d %s", w.Code, w.Body.String())
	}

	runUpdateStream = func(*cluster.Client, string, cluster.OperationOptions, func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		t.Error("the gitops profile ran the full profile's Update")
		return &types.OperationResponse{}, nil
	}
	var image string
	runUpdateStreamGitOps = func(_ *cluster.Client, img string, emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		image = img
		return &types.OperationResponse{Success: true, Message: "installed"}, nil
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON("/api/update/stream", "user:admin", `{"image":"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6","revertDashboardDev":true}`))
	if w.Code != http.StatusOK || image != "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6" || !strings.Contains(w.Body.String(), `"status":"success"`) {
		t.Fatalf("update: %d image=%q %s", w.Code, image, w.Body.String())
	}

	// The same validation as the full profile's Update.
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON("/api/update/stream", "user:admin2", `{"image":"docker.io/evil/catalog:1"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an image outside the allowed registries: %d %s", w.Code, w.Body.String())
	}
}

// The dry run in the gitops profile runs the profile's dry run, never the
// full one, behind the same gate.
func TestGitOpsProfileDryRun(t *testing.T) {
	setupDevMode(t)
	mux := gitOpsMux(t)
	mutationPermission = func(context.Context, string) (bool, error) { return false, nil }
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON("/api/update", "user:reader", `{"image":"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6","dryRun":true}`))
	if w.Code != http.StatusForbidden {
		t.Fatalf("without cluster-admin: %d %s", w.Code, w.Body.String())
	}
	allowMutations(t)
	runUpdateDryRun = func(*cluster.Client, string) (*types.OperationResponse, error) {
		t.Error("the gitops profile ran the full profile's dry run")
		return &types.OperationResponse{}, nil
	}
	runUpdateDryRunGitOps = func(*cluster.Client, string) (*types.OperationResponse, error) {
		return &types.OperationResponse{Success: true, Message: "Dry run complete. No changes were made."}, nil
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON("/api/update", "user:admin", `{"image":"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6","dryRun":true}`))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "No changes were made") {
		t.Fatalf("dry run: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON("/api/update", "user:admin", `{"image":"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("not a dry run: %d %s", w.Code, w.Body.String())
	}
}

// The full profile registers every route's own handler, as before.
func TestFullProfileRegistersEveryHandler(t *testing.T) {
	for _, rt := range routes {
		if h := handlerFor(ProfileFull, rt); h == nil {
			t.Fatalf("%s has no handler", rt.Pattern)
		}
	}
	setupDevMode(t)
	SetProfile(ProfileFull)
	mux := http.NewServeMux()
	Register(mux)
	mutationPermission = func(context.Context, string) (bool, error) { return false, nil }
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON("/api/rollback/stream", "user:reader", "{}"))
	if w.Code != http.StatusForbidden || decodeError(t, w)["errorCode"] != "forbidden" {
		t.Fatalf("full profile, Reinstall without cluster-admin: %d %s", w.Code, w.Body.String())
	}
}

func TestVersionReportsTheProfile(t *testing.T) {
	setupDevMode(t)
	for _, p := range []Profile{ProfileFull, ProfileGitOps} {
		SetProfile(p)
		w := httptest.NewRecorder()
		HandleVersion(w, httptest.NewRequest(http.MethodGet, "/api/version", nil))
		var v map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		if v["profile"] != string(p) {
			t.Errorf("profile %s: /api/version = %s", p, w.Body.String())
		}
	}
}

// Update runs only while the gitops profile's admission policy is in
// place, checked before the lock; /api/version reports the last check.
func TestGitOpsProfileUpdateNeedsTheAdmissionPolicy(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	mux := gitOpsMux(t)
	ran := false
	runUpdateStreamGitOps = func(*cluster.Client, string, func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		ran = true
		return &types.OperationResponse{Success: true}, nil
	}
	checkGitOpsPolicy = func(*cluster.Client) error {
		return errors.New("read ValidatingAdmissionPolicy x: not found")
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON("/api/update/stream", "user:admin", `{"image":"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6"}`))
	if body := decodeError(t, w); w.Code != http.StatusServiceUnavailable || body["errorCode"] != "admission_policy_missing" || ran {
		t.Fatalf("without the policy: %d %s ran=%v", w.Code, w.Body.String(), ran)
	}
	if clusterMutationInProgress.Load() {
		t.Fatal("the refused Update took the lock")
	}
	v := httptest.NewRecorder()
	HandleVersion(v, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	if !strings.Contains(v.Body.String(), "admission policy is not in place") {
		t.Fatalf("/api/version = %s", v.Body.String())
	}

	checkGitOpsPolicy = func(*cluster.Client) error { return nil }
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON("/api/update/stream", "user:admin2", `{"image":"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6"}`))
	if w.Code != http.StatusOK || !ran {
		t.Fatalf("with the policy: %d %s", w.Code, w.Body.String())
	}
	if gitOpsPolicyStatus() != "enforced" {
		t.Fatalf("status = %q", gitOpsPolicyStatus())
	}
}

// The watch does nothing in the full profile.
func TestGitOpsPolicyWatchIsGitOpsOnly(t *testing.T) {
	setupDevMode(t)
	SetProfile(ProfileFull)
	checkGitOpsPolicy = func(*cluster.Client) error {
		t.Error("the full profile checked the admission policy")
		return nil
	}
	StartGitOpsPolicyWatch(context.Background())
	if gitOpsPolicyStatus() != "" {
		t.Fatalf("status = %q", gitOpsPolicyStatus())
	}
}
