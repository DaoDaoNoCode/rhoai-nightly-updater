package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
)

// rateLimiter provides simple per-user rate limiting for mutation endpoints:
// one accepted request per user and endpoint per window. This is protection
// against double-clicks and duplicate tabs, not a DDoS defense. A slot is
// taken atomically when the request is accepted (so concurrent duplicates
// are refused while the first one runs) and given back when the request is
// rejected or asks not to count.
type rateLimiter struct {
	mu     sync.Mutex
	users  map[string]time.Time // key -> time the slot was taken
	window time.Duration
	now    func() time.Time // tests replace it
}

func newRateLimiter(window time.Duration) *rateLimiter {
	return &rateLimiter{users: map[string]time.Time{}, window: window, now: time.Now}
}

// retryAfter is how long until key's slot frees, rounded up to whole
// seconds (at least 1) for the Retry-After header.
func (rl *rateLimiter) retryAfter(key string) int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	wait := rl.window - rl.now().Sub(rl.users[key])
	secs := int((wait + time.Second - 1) / time.Second)
	return max(1, min(secs, int(rl.window/time.Second)))
}

// mutationLimiter is the package-level rate limiter for mutation endpoints.
var mutationLimiter = newRateLimiter(30 * time.Second)

// tryAcquire takes the slot for key unless it was taken within the window.
// It returns the slot's timestamp for release.
func (rl *rateLimiter) tryAcquire(key string) (time.Time, bool) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := rl.now()
	if last, ok := rl.users[key]; ok && now.Sub(last) < rl.window {
		return time.Time{}, false
	}
	if len(rl.users) > 256 {
		for k, t := range rl.users {
			if now.Sub(t) >= rl.window {
				delete(rl.users, k)
			}
		}
	}
	rl.users[key] = now
	return now, true
}

// release gives back a slot taken at the given time (and not a newer one).
func (rl *rateLimiter) release(key string, takenAt time.Time) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if t, ok := rl.users[key]; ok && t.Equal(takenAt) {
		delete(rl.users, key)
	}
}

// withMutationAuth wraps a handler that performs a cluster mutation.
// It verifies the user's identity and cluster-admin permission with the
// user's own token, checks that the user's sign-in can carry the change
// (sessionProblem), applies a per-user rate limit, and gives accepted work
// a bounded lifetime independent of the browser. The handler's client
// acts with the user's own token (X-Forwarded-Access-Token, which only
// oauth-proxy sets; requestUser refuses a bearer token outside DEV_MODE),
// so the API server checks every change against the user's own RBAC and
// its audit log names them. The updater's own records use the
// ServiceAccount (see bookkeeper below).
func withMutationAuth(fn func(c *cluster.Client, w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return mutationAuth(true, fn)
}

// withGitOpsMutationAuth is withMutationAuth for the gitops profile's
// Update, whose client acts with the ServiceAccount: that profile's
// ValidatingAdmissionPolicy (deploy/rbac-gitops.yaml) bounds what the
// ServiceAccount may write to the build the Git-managed catalog serves, and
// it matches only the ServiceAccount's username, so a change made with the
// user's token would bypass it.
func withGitOpsMutationAuth(fn func(c *cluster.Client, w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return mutationAuth(false, fn)
}

func mutationAuth(asUser bool, fn func(c *cluster.Client, w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		RecordRequest()
		username, r, ok := requestUser(w, r)
		if !ok {
			return
		}
		clusterToken := getClusterToken()
		if clusterToken == "" {
			writeError(w, "cluster token not available", http.StatusInternalServerError)
			return
		}
		userToken := extractUserToken(r)
		allowed, permissionErr := mutationPermission(r.Context(), userToken)
		if permissionErr != nil {
			writeAuthError(w, permissionErr, "Cannot verify mutation permissions. No changes were made.")
			return
		}
		if !allowed {
			writeError(w, readOnlyMessage, http.StatusForbidden, "forbidden")
			return
		}
		if asUser {
			if msg, code, status := sessionProblem(r.Context(), userToken); msg != "" {
				writeError(w, msg+" Nothing was changed.", status, code)
				return
			}
		}

		rateLimitKey := username + ":" + r.URL.Path
		slot, acquired := mutationLimiter.tryAcquire(rateLimitKey)
		if !acquired {
			// The tool's own limit, not the OpenShift API's throttling
			// ("rate_limited"): its own errorCode and the real wait.
			wait := mutationLimiter.retryAfter(rateLimitKey)
			slog.Warn("rate limited", "user", username, "path", r.URL.Path, "retryAfter", wait)
			w.Header().Set("Retry-After", strconv.Itoa(wait))
			writeError(w, fmt.Sprintf("This tool allows one change per user every %d seconds on this action. Nothing was changed; try again in %d %s.", int(mutationLimiter.window/time.Second), wait, map[bool]string{true: "second", false: "seconds"}[wait == 1]), http.StatusTooManyRequests, "user_rate_limited")
			return
		}

		if !mutations.begin() {
			mutationLimiter.release(rateLimitKey, slot)
			w.Header().Set("Retry-After", "30")
			writeError(w, "The updater is restarting. No changes were made; retry once it is back.", http.StatusServiceUnavailable, "shutting_down")
			return
		}
		defer mutations.end()

		// Accepted operations can outlast the server's 180s write timeout,
		// including the non-streaming endpoints; their own deadline applies.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

		opContext, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Minute)
		defer cancel()
		// The lease heartbeat stops the operation with
		// cluster.ErrOperationLockLost when another pod takes the lock over.
		opContext, cancelOp := context.WithCancelCause(opContext)
		defer cancelOp(nil)
		// The updater's own records (the cross-pod lease, the operation
		// marker, the activity log and the snapshot) are always written
		// with the ServiceAccount: they outlive the request (lease
		// heartbeat, marker retries, the shutdown flush) and must not
		// depend on the token the operation acts with.
		bookkeeper := cluster.NewClientWithContext(opContext, clusterToken)
		bookkeeper.SetUsername(username)
		client := cluster.NewClientWithContext(opContext, clusterToken)
		if asUser {
			client = cluster.NewUserClient(opContext, userToken)
		}
		client.SetUsername(username)
		client.SetBookkeeping(bookkeeper)

		sw := &statusWriter{ResponseWriter: w, path: r.URL.Path, user: username, bookkeeper: bookkeeper, cancelOp: cancelOp}
		completed := false
		defer func() {
			// Rejected requests (4xx/5xx status, including 409 busy),
			// requests that opt out (dry runs) and handlers that panicked
			// do not use up the user's slot.
			if !completed || sw.status >= 400 || sw.Header().Get("X-Skip-Rate-Limit") == "true" {
				mutationLimiter.release(rateLimitKey, slot)
			}
		}()
		fn(client, sw, r)
		completed = true
	}
}

// resolveUsername returns the request's user: the identity verified by
// withAuth/withMutationAuth when present, otherwise the oauth-proxy header.
func resolveUsername(r *http.Request) string {
	if u, ok := r.Context().Value(identityKey{}).(string); ok && u != "" {
		return u
	}
	if u := r.Header.Get("X-Forwarded-User"); u != "" {
		return u
	}
	if os.Getenv("DEV_MODE") == "true" {
		if u := os.Getenv("DEV_USER"); u != "" {
			return u
		}
	}
	return "unknown"
}

// extractUserToken returns the user's OAuth token for identity purposes.
//
// In the cluster only X-Forwarded-Access-Token counts. oauth-proxy sets it
// from the user's own session, which it got through its OAuth flow, and
// replaces any client-sent copy (setRequestHeader in openshift/oauth-proxy
// oauthproxy.go). Without --openshift-delegate-urls the proxy does not
// accept client bearer tokens at all, and with --pass-basic-auth (default
// true) it overwrites the Authorization header with Basic credentials. So a
// Bearer token would come from somewhere else; requestUser rejects it. A
// bearer token is accepted only in DEV_MODE, where there is no proxy.
func extractUserToken(r *http.Request) string {
	if token := r.Header.Get("X-Forwarded-Access-Token"); token != "" {
		return token
	}
	if os.Getenv("DEV_MODE") == "true" {
		if token, ok := bearerToken(r); ok {
			return token
		}
		if token := os.Getenv("DEV_TOKEN"); token != "" {
			return token
		}
	}
	return ""
}

// bearerToken returns the request's "Authorization: Bearer" token.
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

// cachedToken holds the SA token and the time it was read, so we avoid
// reading the token file on every single HTTP request.
var (
	cachedTokenMu    sync.Mutex
	cachedTokenValue string
	cachedTokenAt    time.Time
)

const tokenCacheTTL = 5 * time.Minute

// getClusterToken returns the token to use for k8s API calls.
// In-cluster: uses the ServiceAccount token (scoped RBAC), cached for 5 minutes.
// Dev mode: uses the DEV_TOKEN (only when SA token is not available).
func getClusterToken() string {
	cachedTokenMu.Lock()
	defer cachedTokenMu.Unlock()

	if cachedTokenValue != "" && time.Since(cachedTokenAt) < tokenCacheTTL {
		return cachedTokenValue
	}

	// Always prefer the in-cluster SA token
	data, err := os.ReadFile(serviceAccountTokenPath)
	if err == nil && len(data) > 0 {
		cachedTokenValue = string(data)
		cachedTokenAt = time.Now()
		return cachedTokenValue
	}
	// Fall back to DEV_TOKEN only in dev mode (not in-cluster)
	if os.Getenv("DEV_MODE") == "true" {
		token := os.Getenv("DEV_TOKEN")
		cachedTokenValue = token
		cachedTokenAt = time.Now()
		return token
	}
	return ""
}

// withAuth wraps a handler that requires authentication.
// It verifies the user's token with the API server (the user's own token,
// forwarded by oauth-proxy) and creates a Client using the ServiceAccount
// token for k8s API calls. A request without a token, or whose token the API
// server rejects, never reaches the handler.
func withAuth(fn func(c *cluster.Client, w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		RecordRequest()
		username, r, ok := requestUser(w, r)
		if !ok {
			return
		}
		clusterToken := getClusterToken()
		if clusterToken == "" {
			writeError(w, "cluster token not available", http.StatusInternalServerError)
			return
		}
		client := cluster.NewClientWithContext(r.Context(), clusterToken)
		client.SetUsername(username)
		fn(client, w, r)
	}
}

// HandleUserPermissions reports whether the logged-in user may mutate, using
// the same permission review that guards every mutation endpoint. A 503
// means the answer is unknown: clients must treat it as read-only.
var HandleUserPermissions = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	username := resolveUsername(r)
	token := extractUserToken(r)
	allowed, err := mutationPermission(r.Context(), token)
	if err != nil {
		writeAuthError(w, err, "Cannot verify mutation permissions")
		return
	}
	resp := map[string]interface{}{"canMutate": allowed, "user": username}
	// A cluster-admin whose sign-in cannot carry a change (an old session,
	// or one about to expire) sees why before trying, as every change would
	// be refused with the same reason.
	if allowed && activeProfile != ProfileGitOps {
		if msg, code, _ := sessionProblem(r.Context(), token); msg != "" {
			resp["canMutate"], resp["reason"], resp["reasonCode"] = false, msg, code
		}
	}
	writeJSON(w, resp, "user-permissions")
})

// adminReadMessage explains a 403 from withAdminRead.
const adminReadMessage = "Only cluster-admins can read this through the updater."

// withAdminRead gates a read that returns a credential: only a user who
// passes the mutation gate (cluster-admin, see mutationRequirement) gets an
// answer. Such a user can read every Secret in the cluster with their own
// token, so the ServiceAccount reveals nothing they could not read
// themselves. Unlike withMutationAuth it takes no lock and starts no
// operation: nothing changes.
func withAdminRead(fn func(c *cluster.Client, w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
		allowed, err := mutationPermission(r.Context(), extractUserToken(r))
		if err != nil {
			writeAuthError(w, err, "Cannot verify your permissions. Nothing was read.")
			return
		}
		if !allowed {
			writeError(w, adminReadMessage, http.StatusForbidden, "forbidden")
			return
		}
		fn(c, w, r)
	})
}
