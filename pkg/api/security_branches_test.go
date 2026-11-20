package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The gitops profile's dry run (POST /api/update, HandleUpdateGitOps)
// refuses an image outside the allowlist, an empty one and a malformed
// one before anything runs, like the full profile's endpoints
// (TestImageAllowlistOnEveryImageEndpoint), and runs an allowed one.
func TestGitOpsDryRunImageAllowlist(t *testing.T) {
	for _, tc := range []struct {
		image  string
		status int
	}{
		{"docker.io/evil/fbc:1", http.StatusBadRequest},
		{"quay.io/evil/fbc:1", http.StatusBadRequest},
		{"quay.io.evil.example/rhoai/fbc:1", http.StatusBadRequest},
		{"", http.StatusBadRequest},
		{"quay.io/rhoai/fbc fragment", http.StatusBadRequest},
		{"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6-nightly", http.StatusOK},
	} {
		t.Run(tc.image, func(t *testing.T) {
			setupDevMode(t)
			allowMutations(t)
			called := false
			runUpdateDryRunGitOps = func(*cluster.Client, string) (*types.OperationResponse, error) {
				called = true
				return &types.OperationResponse{Success: true, Message: "ok"}, nil
			}
			mux := gitOpsMux(t)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, postJSON("/api/update", "user:admin", `{"image":"`+tc.image+`","dryRun":true}`))
			if w.Code != tc.status || called != (tc.status == http.StatusOK) {
				t.Fatalf("status %d (called %v): %s", w.Code, called, w.Body.String())
			}
			if tc.status == http.StatusBadRequest {
				if body := decodeError(t, w); body["errorCode"] != "validation" {
					t.Fatalf("body %s", w.Body.String())
				}
			}
		})
	}
}

// The full profile's stream refuses an empty and a malformed image
// (decodeUpdateStreamRequest) before anything runs.
func TestUpdateStreamRefusesEmptyAndMalformedImages(t *testing.T) {
	for _, body := range []string{`{"image":""}`, `{"image":"quay.io/rhoai/fbc fragment"}`, `{"image":"-quay.io/rhoai/x"}`} {
		setupDevMode(t)
		allowMutations(t)
		called := false
		runUpdateStream = func(*cluster.Client, string, cluster.OperationOptions, func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
			called = true
			return &types.OperationResponse{Success: true}, nil
		}
		w := httptest.NewRecorder()
		HandleUpdateStream(w, postJSON("/api/update/stream", "user:admin", body))
		if w.Code != http.StatusBadRequest || called || decodeError(t, w)["errorCode"] != "validation" {
			t.Errorf("%s: status %d called=%v %s", body, w.Code, called, w.Body.String())
		}
	}
}

// withAdminRead's own gate refuses on its own, without the route tier in
// front of it: not a cluster-admin (403 admin_only), an expired session
// (401 session_expired) and a permission check that cannot be made (503
// authorization_unavailable, fail closed). The handler never runs.
func TestWithAdminReadRefusesOnItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name      string
		allowed   bool
		err       error
		status    int
		errorCode string
	}{
		{"not admin", false, nil, http.StatusForbidden, adminOnlyCode},
		{"session expired", false, fmt.Errorf("check: %w", cluster.ErrUnauthenticated), http.StatusUnauthorized, "session_expired"},
		{"check unavailable", false, errors.New("connection refused"), http.StatusServiceUnavailable, "authorization_unavailable"},
		{"check unavailable, allowed true", true, errors.New("connection refused"), http.StatusServiceUnavailable, "authorization_unavailable"},
		{"admin", true, nil, http.StatusOK, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupDevMode(t)
			mutationPermission = func(context.Context, string) (bool, error) { return tc.allowed, tc.err }
			called := false
			h := withAdminRead(func(_ *cluster.Client, w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			})
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/api/resources/s3/admin-password", nil)
			r.Header.Set("X-Forwarded-Access-Token", "user:alice")
			h(w, r)
			if w.Code != tc.status || called != (tc.status == http.StatusOK) {
				t.Fatalf("status %d called %v: %s", w.Code, called, w.Body.String())
			}
			if tc.errorCode != "" && decodeError(t, w)["errorCode"] != tc.errorCode {
				t.Fatalf("body %s", w.Body.String())
			}
		})
	}
}

// Without the ServiceAccount token (no file, not DEV_MODE) both wrappers
// answer 500 and never run the handler or check the gate.
func TestWrappersRefuseWithoutTheServiceAccountToken(t *testing.T) {
	for _, name := range []string{"withAuth", "withMutationAuth"} {
		t.Run(name, func(t *testing.T) {
			setupDevMode(t)
			setDev(t, DevConfig{}) // in the cluster, but no token file in tests
			cachedTokenMu.Lock()
			cachedTokenValue, cachedTokenAt = "", time.Time{}
			cachedTokenMu.Unlock()
			mutationPermission = func(context.Context, string) (bool, error) {
				t.Error("the gate ran without a ServiceAccount token")
				return true, nil
			}
			called := false
			fn := func(*cluster.Client, http.ResponseWriter, *http.Request) { called = true }
			h := withAuth(fn)
			if name == "withMutationAuth" {
				h = withMutationAuth(fn)
			}
			w := httptest.NewRecorder()
			h(w, postJSON("/api/x", "user:alice", `{}`))
			if w.Code != http.StatusInternalServerError || called || decodeError(t, w)["errorCode"] != "internal" {
				t.Fatalf("status %d called %v: %s", w.Code, called, w.Body.String())
			}
			if mutationLimiter.isRateLimited("alice:/api/x") {
				t.Fatal("a refused request took the slot")
			}
		})
	}
}

// The rate limiter forgets slots whose window ended once it tracks more
// than 256 keys, and keeps the ones still within it.
func TestRateLimiterPrunesExpiredSlots(t *testing.T) {
	resetPackageState(t)
	rl := newRateLimiter()
	for i := range 200 {
		if _, ok := rl.tryAcquire(fmt.Sprintf("old-%d", i)); !ok {
			t.Fatal("fresh key refused")
		}
	}
	testClock.Advance(mutationWindow)
	for i := range 57 {
		rl.tryAcquire(fmt.Sprintf("new-%d", i))
	}
	// 257 keys: the next acquire prunes the 200 expired ones.
	if _, ok := rl.tryAcquire("trigger"); !ok {
		t.Fatal("trigger refused")
	}
	rl.mu.Lock()
	n := len(rl.users)
	rl.mu.Unlock()
	if n != 58 {
		t.Fatalf("%d keys after pruning, want 58", n)
	}
	if !rl.isRateLimited("new-0") || rl.isRateLimited("old-0") {
		t.Fatal("pruned the wrong keys")
	}
	// A slot still in its window is never given out twice.
	if _, ok := rl.tryAcquire("new-1"); ok {
		t.Fatal("a slot within its window was taken again")
	}
}

// The identity cache stays at most identityCacheMax entries: expired
// entries go first, and a cache full of live entries is dropped (the next
// requests are looked up again, never trusted without a lookup).
func TestIdentityCacheIsBounded(t *testing.T) {
	c := &identityCache{entries: map[[32]byte]identityEntry{}}
	now := time.Unix(1_800_000_000, 0)
	key := func(i int) [32]byte { var k [32]byte; copy(k[:], fmt.Sprint(i)); return k }
	for i := range identityCacheMax {
		c.put(key(i), "u", now)
	}
	later := now.Add(identityCacheTTL + time.Second)
	c.put(key(-1), "fresh", later) // all others expired: pruned
	if len(c.entries) != 1 {
		t.Fatalf("%d entries after pruning expired ones", len(c.entries))
	}
	for i := range identityCacheMax - 1 {
		c.put(key(i), "u", later)
	}
	if len(c.entries) != identityCacheMax {
		t.Fatalf("%d entries", len(c.entries))
	}
	c.put(key(identityCacheMax+1), "next", later) // full of live entries: dropped
	if len(c.entries) != 1 {
		t.Fatalf("%d entries after a full cache", len(c.entries))
	}
	if u, ok := c.get(key(identityCacheMax+1), later); !ok || u != "next" {
		t.Fatal("the new entry is missing")
	}
	if _, ok := c.get(key(0), later); ok {
		t.Fatal("a dropped entry is still served")
	}
}

// POST /api/pageview needs a signed-in user, takes at most 256 bytes and
// records only bounded labels.
func TestHandlePageView(t *testing.T) {
	setupDevMode(t)
	setDev(t, DevConfig{}) // no dev token fallback: the request's own token counts
	count := func(name string) int64 {
		pageViewsMu.RLock()
		defer pageViewsMu.RUnlock()
		if c := pageViews[name]; c != nil {
			return c.Load()
		}
		return 0
	}
	t.Run("no token", func(t *testing.T) {
		w := httptest.NewRecorder()
		HandlePageView(w, postJSON("/api/pageview", "", `{"page":"status"}`))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status %d", w.Code)
		}
	})
	t.Run("expired session", func(t *testing.T) {
		w := httptest.NewRecorder()
		HandlePageView(w, postJSON("/api/pageview", expiredToken, `{"page":"status"}`))
		if w.Code != http.StatusUnauthorized || decodeError(t, w)["errorCode"] != "session_expired" {
			t.Fatalf("status %d %s", w.Code, w.Body.String())
		}
	})
	t.Run("too large", func(t *testing.T) {
		before := count("other")
		w := httptest.NewRecorder()
		HandlePageView(w, postJSON("/api/pageview", "user:alice", `{"page":"`+strings.Repeat("x", 300)+`"}`))
		if w.Code != http.StatusBadRequest || count("other") != before {
			t.Fatalf("status %d", w.Code)
		}
	})
	t.Run("counted", func(t *testing.T) {
		known := ""
		for name := range knownPages {
			known = name
			break
		}
		before, otherBefore := count(known), count("other")
		featureMade := func() int64 {
			featureUsageMu.RLock()
			defer featureUsageMu.RUnlock()
			if c := featureUsage["other"]; c != nil {
				return c.Load()
			}
			return 0
		}
		featureBefore := featureMade()
		w := httptest.NewRecorder()
		HandlePageView(w, postJSON("/api/pageview", "user:alice", `{"action":"made-up-action"}`))
		if w.Code != http.StatusNoContent || featureMade() != featureBefore+1 {
			t.Fatalf("action: status %d, other %d -> %d", w.Code, featureBefore, featureMade())
		}
		for _, body := range []string{`{"page":"` + known + `"}`, `{"page":"made-up-page"}`} {
			w := httptest.NewRecorder()
			HandlePageView(w, postJSON("/api/pageview", "user:alice", body))
			if w.Code != http.StatusNoContent {
				t.Fatalf("%s: status %d", body, w.Code)
			}
		}
		if count(known) != before+1 || count("other") != otherBefore+1 {
			t.Fatalf("%s %d -> %d, other %d -> %d", known, before, count(known), otherBefore, count("other"))
		}
	})
}
