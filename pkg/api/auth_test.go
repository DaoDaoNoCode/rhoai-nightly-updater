package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/clock/clocktest"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Tokens understood by the fake identity lookup installed by setupDevMode.
const (
	expiredToken = "expired-token" // the API server answers 401
	apiDownToken = "api-down"      // the API server cannot be reached
)

func setupDevMode(t *testing.T) {
	t.Helper()
	setDev(t, DevConfig{Mode: true, Token: "test-cluster-token", User: "test-user"})
	// Clear the cached token so DEV_TOKEN takes effect
	cachedTokenMu.Lock()
	cachedTokenValue = ""
	cachedTokenAt = time.Time{}
	cachedTokenMu.Unlock()
	resetPackageState(t)
	// "user:<name>" tokens belong to <name>; any other token to test-user.
	lookupUser = func(_ context.Context, token string) (string, error) {
		switch {
		case token == expiredToken:
			return "", fmt.Errorf("identify user: %w", cluster.ErrUnauthenticated)
		case token == apiDownToken:
			return "", errors.New("identify user: connection refused")
		case strings.HasPrefix(token, "user:"):
			return strings.TrimPrefix(token, "user:"), nil
		}
		return "test-user", nil
	}
}

// testClock is the fake clock resetPackageState installs as clk.
var testClock *clocktest.Fake

// resetPackageState gives each test fresh package-level state (rate
// limiter, lock, in-flight operation, caches) and restores the seams, so
// tests are repeatable with -count=N and independent of their order.
func resetPackageState(t *testing.T) {
	t.Helper()
	origLimiter, origLookup, origPermission := mutationLimiter, lookupUser, mutationPermission
	origSave, origComplete, origRead := saveOperationMarker, saveCompletedOperation, readOperationState
	origAcquireLock, origRenewLocks, origReleaseLock := acquireOperationLock, renewOperationLocks, releaseOperationLock
	origInterval, origTTL := leaseHeartbeatInterval, leaseTTL
	origCheck, origPerm := checkAPIVersion, checkPermission
	origU, origD, origR, origF := runUpdateStream, runUpdateDryRun, runReinstallStream, runRefreshStream
	origUG, origUDG, origProfile := runUpdateStreamGitOps, runUpdateDryRunGitOps, activeProfile
	origPolicyCheck := checkGitOpsPolicy
	origClock := clk
	// Every test starts on its own fake clock: nothing waits in real time,
	// and a test moves the time with testClock.Advance.
	testClock = clocktest.NewFake()
	clk = testClock
	origSession, origSessionClock := readSessionToken, sessionClock
	origIsUserProject, origProjectUID, origReview, origBlock, origApps := isUserProject, readProjectUID, reviewProjectActions, projectFeatureBlock, projectAppsNamespace
	// The project actions' gate: no project is the user's, every project
	// has the UID "uid-<name>", no prerequisite blocks, every review allows.
	isUserProject = func(*cluster.Client, string) (bool, error) { return false, nil }
	readProjectUID = func(_ *cluster.Client, project string) (string, error) { return "uid-" + project, nil }
	reviewProjectActions = func(*cluster.Client, []string, string, string) (map[string][]cluster.MissingPermission, error) {
		return map[string][]cluster.MissingPermission{}, nil
	}
	projectFeatureBlock = func(*cluster.Client, string) (string, string) { return "", "" }
	projectAppsNamespace = func(*cluster.Client) (string, error) { return "redhat-ods-applications", nil }
	checkGitOpsPolicy = func(*cluster.Client) error { return nil }
	setGitOpsPolicyState("")
	mutationLimiter = newRateLimiter()
	identities.reset()
	capabilities.reset()
	apiReady.reset()
	inflight.reset()
	markers.reset()
	leases.stopAll()
	locks.reset()
	saveOperationMarker = func(*cluster.Client, *types.OperationMarker) error { return nil }
	saveCompletedOperation = func(*cluster.Client, *types.CompletedOperation, *cluster.LeaseOwner) error { return nil }
	readOperationState = func(*cluster.Client) (*cluster.OperationRecord, error) { return &cluster.OperationRecord{}, nil }
	acquireOperationLock = func(*cluster.Client, string, string, func(*cluster.OperationRecord) error) error { return nil }
	renewOperationLocks = func(*cluster.Client, []cluster.LockRenewal) (map[string]*types.OperationLease, error) {
		return nil, nil
	}
	releaseOperationLock = func(*cluster.Client, string, cluster.LeaseOwner) error { return nil }
	t.Cleanup(func() {
		// Stop marker retries and the lease heartbeat before the seams they
		// call are restored.
		markers.reset()
		leases.stopAll()
		mutationLimiter, lookupUser, mutationPermission = origLimiter, origLookup, origPermission
		saveOperationMarker, saveCompletedOperation, readOperationState = origSave, origComplete, origRead
		acquireOperationLock, renewOperationLocks, releaseOperationLock = origAcquireLock, origRenewLocks, origReleaseLock
		leaseHeartbeatInterval, leaseTTL = origInterval, origTTL
		checkAPIVersion, checkPermission = origCheck, origPerm
		runUpdateStream, runUpdateDryRun, runReinstallStream, runRefreshStream = origU, origD, origR, origF
		runUpdateStreamGitOps, runUpdateDryRunGitOps, activeProfile = origUG, origUDG, origProfile
		checkGitOpsPolicy = origPolicyCheck
		clk = origClock
		readSessionToken, sessionClock = origSession, origSessionClock
		isUserProject, readProjectUID, reviewProjectActions, projectFeatureBlock, projectAppsNamespace = origIsUserProject, origProjectUID, origReview, origBlock, origApps
		setGitOpsPolicyState("")
		identities.reset()
		apiReady.reset()
		inflight.reset()
		locks.reset()
	})
}

func TestWithAuth_MissingToken_Returns401(t *testing.T) {
	handler := withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequestWithContext(t.Context(), "GET", "/api/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	var body map[string]string
	json.NewDecoder(rec.Body).Decode(&body)
	if body["errorCode"] != "unauthorized" {
		t.Errorf("expected errorCode 'unauthorized', got %q", body["errorCode"])
	}
}

func TestWithAuth_ValidToken_CallsHandler(t *testing.T) {
	setupDevMode(t)

	called := false
	handler := withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequestWithContext(t.Context(), "GET", "/api/status", nil)
	req.Header.Set("X-Forwarded-Access-Token", "user-oauth-token")
	req.Header.Set("X-Forwarded-User", "alice")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Fatal("expected handler to be called")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

// Read requests get a client that may reuse a recent status answer (the
// Argo CD Applications list); withAdminRead is a read too. Changes never do
// (TestMutationClientActsAsTheUser).
func TestWithAuth_ClientIsAStatusRead(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	for name, wrap := range map[string]func(func(*cluster.Client, http.ResponseWriter, *http.Request)) http.HandlerFunc{
		"withAuth": withAuth, "withAdminRead": withAdminRead,
	} {
		var got *cluster.Client
		handler := wrap(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
			got = c
			w.WriteHeader(http.StatusOK)
		})
		req := httptest.NewRequestWithContext(t.Context(), "GET", "/api/status", nil)
		req.Header.Set("X-Forwarded-Access-Token", "user-oauth-token")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if got == nil {
			t.Fatalf("%s: handler not called (code %d)", name, rec.Code)
		}
		if !got.StatusRead() || got.ActsAsUser() {
			t.Fatalf("%s: status read %v, as user %v; want the ServiceAccount's client, marked as a status read", name, got.StatusRead(), got.ActsAsUser())
		}
	}
}

func TestWithAuth_AuthorizationBearer_Works(t *testing.T) {
	setupDevMode(t)

	called := false
	handler := withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequestWithContext(t.Context(), "GET", "/api/status", nil)
	req.Header.Set("Authorization", "Bearer my-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Fatal("expected handler to be called with Bearer token")
	}
}

func TestWithMutationAuth_MissingToken_Returns401(t *testing.T) {
	handler := withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequestWithContext(t.Context(), "POST", "/api/update", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestWithMutationAuth_RateLimiting(t *testing.T) {
	setupDevMode(t)

	handler := withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// First call succeeds
	req1 := httptest.NewRequestWithContext(t.Context(), "POST", "/api/test-endpoint", nil)
	req1.Header.Set("X-Forwarded-Access-Token", "token")
	req1.Header.Set("X-Forwarded-User", "rate-test-user")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("first call: expected 200, got %d", rec1.Code)
	}

	// Second call is rate limited
	req2 := httptest.NewRequestWithContext(t.Context(), "POST", "/api/test-endpoint", nil)
	req2.Header.Set("X-Forwarded-Access-Token", "token")
	req2.Header.Set("X-Forwarded-User", "rate-test-user")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second call: expected 429, got %d", rec2.Code)
	}

	retryAfter := rec2.Header().Get("Retry-After")
	if retryAfter != "30" {
		t.Errorf("expected Retry-After: 30, got %q", retryAfter)
	}
}

func TestWithMutationAuth_SkipRateLimit_Header(t *testing.T) {
	setupDevMode(t)

	handler := withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Skip-Rate-Limit", "true")
		w.WriteHeader(http.StatusOK)
	})

	// First call with skip header
	req1 := httptest.NewRequestWithContext(t.Context(), "POST", "/api/skip-test", nil)
	req1.Header.Set("X-Forwarded-Access-Token", "token")
	req1.Header.Set("X-Forwarded-User", "skip-test-user")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("first call: expected 200, got %d", rec1.Code)
	}

	// Second call should NOT be rate limited because first had X-Skip-Rate-Limit
	req2 := httptest.NewRequestWithContext(t.Context(), "POST", "/api/skip-test", nil)
	req2.Header.Set("X-Forwarded-Access-Token", "token")
	req2.Header.Set("X-Forwarded-User", "skip-test-user")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("second call: expected 200 (rate limit skipped), got %d", rec2.Code)
	}
}

func TestWithMutationAuth_DifferentEndpoints_IndependentRateLimiting(t *testing.T) {
	setupDevMode(t)

	handler := withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Call endpoint A
	req1 := httptest.NewRequestWithContext(t.Context(), "POST", "/api/endpoint-a", nil)
	req1.Header.Set("X-Forwarded-Access-Token", "token")
	req1.Header.Set("X-Forwarded-User", "multi-endpoint-user")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("endpoint A first call: expected 200, got %d", rec1.Code)
	}

	// Call endpoint B — should NOT be rate limited (different path)
	req2 := httptest.NewRequestWithContext(t.Context(), "POST", "/api/endpoint-b", nil)
	req2.Header.Set("X-Forwarded-Access-Token", "token")
	req2.Header.Set("X-Forwarded-User", "multi-endpoint-user")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("endpoint B first call: expected 200 (different endpoint), got %d", rec2.Code)
	}
}

func TestStatusWriter_CapturesStatusCode(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec}

	sw.WriteHeader(http.StatusUnprocessableEntity)

	if sw.status != http.StatusUnprocessableEntity {
		t.Errorf("expected status %d, got %d", http.StatusUnprocessableEntity, sw.status)
	}
}
