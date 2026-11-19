package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// sessionNow is the fixed time of these tests' tokens.
var sessionNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// fakeSession makes the user's token object read as tok (or fail with err).
func fakeSession(t *testing.T, tok *cluster.SessionToken, err error) *int {
	t.Helper()
	reads := 0
	sessionClock = func() time.Time { return sessionNow }
	readSessionToken = func(context.Context, string) (*cluster.SessionToken, error) {
		reads++
		return tok, err
	}
	return &reads
}

// fullToken is a user:full token created at created, valid for a day.
func fullToken(created time.Time) *cluster.SessionToken {
	return &cluster.SessionToken{Created: created, ExpiresIn: 86400, Scopes: []string{"user:full"}}
}

// A change acts with the user's own token; its lease, marker and records
// with the ServiceAccount's. The gitops profile's Update keeps the
// ServiceAccount (its admission policy matches only that username).
func TestMutationClientActsAsTheUser(t *testing.T) {
	inCluster(t)
	allowMutations(t)
	fakeSession(t, fullToken(sessionNow.Add(-time.Hour)), nil)
	f := installFakeRecord(t)
	var mu sync.Mutex
	var leaseClients []*cluster.Client
	orig := writeOperationLease
	writeOperationLease = func(c *cluster.Client, l *types.OperationLease, held func(*types.OperationLease) bool) (*types.OperationLease, error) {
		mu.Lock()
		leaseClients = append(leaseClients, c)
		mu.Unlock()
		return orig(c, l, held)
	}
	saveOperationMarker = func(c *cluster.Client, m *types.OperationMarker) error {
		mu.Lock()
		leaseClients = append(leaseClients, c)
		mu.Unlock()
		return nil
	}
	_ = f
	for _, tc := range []struct {
		name   string
		wrap   func(func(*cluster.Client, http.ResponseWriter, *http.Request)) http.HandlerFunc
		asUser bool
	}{
		{"full profile", withMutationAuth, true},
		{"gitops Update", withGitOpsMutationAuth, false},
	} {
		leaseClients = nil
		var opClient, bookkeeper *cluster.Client
		h := tc.wrap(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
			opClient = c
			if !lockCluster(w) {
				t.Fatal("lock refused")
			}
			defer releaseClusterMutationLock()
			bookkeeper = w.(*statusWriter).bookkeeper
			beginOperation(w, "t")
			writeJSON(w, types.OperationResponse{Success: true}, "t")
		})
		w := httptest.NewRecorder()
		h(w, postJSON("/api/refresh/stream", "sha256~alice-token", "{}"))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
		if opClient.ActsAsUser() != tc.asUser || bookkeeper.ActsAsUser() {
			t.Errorf("%s: operation client as user %v, bookkeeping client as user %v", tc.name, opClient.ActsAsUser(), bookkeeper.ActsAsUser())
		}
		mu.Lock()
		for _, c := range leaseClients {
			if c.ActsAsUser() {
				t.Errorf("%s: the lease or marker was written with the user's token", tc.name)
			}
		}
		if len(leaseClients) == 0 {
			t.Errorf("%s: no lease or marker write", tc.name)
		}
		mu.Unlock()
		mutationLimiter = newRateLimiter()
	}
}

// Before a change is accepted the user's sign-in must be able to carry it:
// a token that cannot act (a pre-4.0.0 session), an expired one, or one
// that expires before the operation's deadline is refused with a message
// to sign in again. Only a refusal that is known to be needed is made: an
// unreadable token object lets the change go ahead.
func TestMutationRefusedWhenTheSignInCannotCarryIt(t *testing.T) {
	needed := sessionNeeded()
	for _, tc := range []struct {
		name     string
		tok      *cluster.SessionToken
		err      error
		status   int
		code     string
		contains string
	}{
		{"a day left", fullToken(sessionNow.Add(-time.Hour)), nil, 200, "", ""},
		{"never expires", &cluster.SessionToken{Scopes: []string{"user:full"}}, nil, 200, "", ""},
		{"just enough", fullToken(sessionNow.Add(-24*time.Hour + needed)), nil, 200, "", ""},
		{"expires within the deadline", fullToken(sessionNow.Add(-24*time.Hour + needed - time.Minute)), nil, 401, signInAgainCode, fmt.Sprintf("expires in %d minutes", int((needed-time.Minute)/time.Minute))},
		{"already past", fullToken(sessionNow.Add(-25 * time.Hour)), nil, 401, signInAgainCode, "expires in 0 minutes"},
		{"scoped token", &cluster.SessionToken{Created: sessionNow, ExpiresIn: 86400, Scopes: []string{"user:info", "user:check-access"}}, nil, 401, signInAgainCode, "Log out and log in again"},
		{"its own object is forbidden (scoped)", nil, &cluster.K8sError{Status: 403, Reason: "Forbidden", User: true}, 401, signInAgainCode, "cannot make changes"},
		{"rejected", nil, fmt.Errorf("x: %w", cluster.ErrUnauthenticated), 401, "session_expired", "Log in again"},
		{"its own object is gone (revoked)", nil, fmt.Errorf("x: %w", cluster.ErrSessionTokenGone), 401, "session_expired", "revoked or has ended"},
		{"not found without details (type not served): go ahead", nil, &cluster.K8sError{Status: 404, Reason: "NotFound", User: true}, 200, "", ""},
		{"API unreachable: go ahead", nil, errors.New("dial tcp: connection refused"), 200, "", ""},
		{"not an OAuth token: go ahead", nil, cluster.ErrNotAnOAuthToken, 200, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inCluster(t)
			allowMutations(t)
			fakeSession(t, tc.tok, tc.err)
			ran := false
			runUpdateDryRun = func(*cluster.Client, string) (*types.OperationResponse, error) {
				ran = true
				return &types.OperationResponse{Success: true}, nil
			}
			w := httptest.NewRecorder()
			HandleUpdate(w, postJSON("/api/update", "sha256~alice-token", `{"image":"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6","dryRun":true}`))
			if w.Code != tc.status || ran != (tc.status == 200) {
				t.Fatalf("status %d ran %v: %s", w.Code, ran, w.Body.String())
			}
			if tc.status != 200 {
				body := decodeError(t, w)
				if body["errorCode"] != tc.code || !strings.Contains(body["error"].(string), tc.contains) || !strings.Contains(body["error"].(string), "Nothing was changed") {
					t.Fatalf("body %v", body)
				}
				if clusterMutationInProgress.Load() || mutationLimiter.isRateLimited("alice:/api/update") {
					t.Fatal("a refused change took the lock or the rate-limit slot")
				}
			}
		})
	}
}

// The read-only switch tells a cluster-admin why changes are off when their
// sign-in cannot carry one, before they try.
func TestUserPermissionsReportsTheSignIn(t *testing.T) {
	inCluster(t)
	allowMutations(t)
	fakeSession(t, &cluster.SessionToken{Scopes: []string{"user:info"}}, nil)
	w := httptest.NewRecorder()
	HandleUserPermissions(w, postJSONMethod(http.MethodGet, "/api/user/permissions", "sha256~alice-token", ""))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 200 || body["canMutate"] != false || body["reasonCode"] != signInAgainCode || !strings.Contains(fmt.Sprint(body["reason"]), "Log out and log in again") {
		t.Fatalf("%d %v", w.Code, body)
	}
	fakeSession(t, fullToken(sessionNow), nil)
	w = httptest.NewRecorder()
	HandleUserPermissions(w, postJSONMethod(http.MethodGet, "/api/user/permissions", "sha256~alice-token", ""))
	body = nil
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["canMutate"] != true || body["reason"] != nil {
		t.Fatalf("%v", body)
	}
	// A user who may not change the cluster is not asked about the sign-in.
	reads := fakeSession(t, nil, errors.New("must not be read"))
	mutationPermission = func(context.Context, string) (bool, error) { return false, nil }
	w = httptest.NewRecorder()
	HandleUserPermissions(w, postJSONMethod(http.MethodGet, "/api/user/permissions", "sha256~bob-token", ""))
	if *reads != 0 {
		t.Fatal("the sign-in of a read-only user was read")
	}
}

// The gitops profile signs users in through the ServiceAccount, as 3.x
// did (the chart's rnu.gitopsSignIn): their token has the scopes
// user:info user:check-access only. Nothing in that profile acts with it,
// so neither the read-only switch nor Update reads the sign-in or asks to
// sign in again, and Update's client is the ServiceAccount's.
func TestGitOpsProfileWorksWithAScopedSignIn(t *testing.T) {
	inCluster(t)
	allowMutations(t)
	reads := fakeSession(t, &cluster.SessionToken{Created: sessionNow, ExpiresIn: 86400, Scopes: []string{"user:info", "user:check-access"}}, nil)
	mux := gitOpsMux(t)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, postJSONMethod(http.MethodGet, "/api/user/permissions", "sha256~alice-token", ""))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusOK || body["canMutate"] != true || body["reason"] != nil || body["reasonCode"] != nil {
		t.Fatalf("permissions: %d %v", w.Code, body)
	}

	var asUser *bool
	runUpdateDryRunGitOps = func(c *cluster.Client, _ string) (*types.OperationResponse, error) {
		v := c.ActsAsUser()
		asUser = &v
		return &types.OperationResponse{Success: true, Message: "Dry run complete. No changes were made."}, nil
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON("/api/update", "sha256~alice-token", `{"image":"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6","dryRun":true}`))
	if w.Code != http.StatusOK || asUser == nil || *asUser {
		t.Fatalf("update dry run: %d, acted as the user: %v: %s", w.Code, asUser, w.Body.String())
	}
	if *reads != 0 {
		t.Fatalf("the gitops profile read the sign-in %d times", *reads)
	}

	// The same scoped sign-in in the full profile cannot change anything.
	SetProfile(ProfileFull)
	w = httptest.NewRecorder()
	HandleUserPermissions(w, postJSONMethod(http.MethodGet, "/api/user/permissions", "sha256~alice-token", ""))
	body = nil
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["canMutate"] != false || body["reasonCode"] != signInAgainCode {
		t.Fatalf("full profile: %v", body)
	}
}

// The user's token never appears in a log line, a streamed step, an
// error, the operation record, the lease or the activity, whatever the
// operation reports; it reaches only the seams that call the API server.
func TestUserTokenIsNeverLoggedOrEchoed(t *testing.T) {
	const secret = "sha256~TOPSECRET-user-token-1234567890"
	inCluster(t)
	allowMutations(t)
	f := installFakeRecord(t)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Each refusal path, then a change that fails with the user's token
	// rejected mid-operation.
	for _, err := range []error{
		fmt.Errorf("x: %w", cluster.ErrUnauthenticated),
		&cluster.K8sError{Status: 403, Reason: "Forbidden", User: true},
		errors.New("dial tcp: connection refused"),
	} {
		fakeSession(t, nil, err)
		w := httptest.NewRecorder()
		HandleRefreshStream(w, postJSON("/api/refresh/stream", secret, "{}"))
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("response shows the token: %s", w.Body.String())
		}
		mutationLimiter = newRateLimiter()
	}
	fakeSession(t, fullToken(sessionNow), nil)
	runRefreshStream = func(c *cluster.Client, _ cluster.OperationOptions, emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		err := &cluster.K8sError{Status: 401, Reason: "Unauthorized", User: c.ActsAsUser()}
		emit(cluster.UpdateStepEvent{Step: "verify", Status: "failed", Message: err.Error()})
		return &types.OperationResponse{Success: false, Message: err.Error()}, err
	}
	w := httptest.NewRecorder()
	HandleRefreshStream(w, postJSON("/api/refresh/stream", secret, "{}"))
	out := w.Body.String()
	if !strings.Contains(out, "Log out of the updater and log in again") {
		t.Errorf("a mid-operation 401 does not say to sign in again: %s", out)
	}
	f.mu.Lock()
	record, _ := json.Marshal(map[string]any{"marker": f.marker, "last": f.last, "lease": f.lease, "writes": f.writes})
	f.mu.Unlock()
	snap, _ := json.Marshal(inflight.snapshot())
	for what, s := range map[string]string{"stream": out, "logs": logs.String(), "operation record": string(record), "operation": string(snap)} {
		if strings.Contains(s, secret) || strings.Contains(s, "TOPSECRET") {
			t.Errorf("the %s shows the user's token", what)
		}
	}
}
