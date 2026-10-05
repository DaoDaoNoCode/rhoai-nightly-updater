package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
)

const serviceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// lookupUser verifies a user token with the API server and returns its
// owner. Tests replace it.
var lookupUser = cluster.LookupUserWithToken

// Verified identities are cached briefly so that polling pages do not add
// an API call per request. A revoked or expired token is noticed within the
// TTL. Only successes are cached; failures are retried on the next request.
const (
	identityCacheTTL = time.Minute
	identityCacheMax = 1024
)

type identityEntry struct {
	user    string
	expires time.Time
}

type identityCache struct {
	mu      sync.Mutex
	entries map[[32]byte]identityEntry
}

var identities = &identityCache{entries: map[[32]byte]identityEntry{}}

func (c *identityCache) get(key [32]byte, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || now.After(e.expires) {
		return "", false
	}
	return e.user, true
}

func (c *identityCache) put(key [32]byte, user string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= identityCacheMax {
		for k, e := range c.entries {
			if now.After(e.expires) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= identityCacheMax {
			c.entries = map[[32]byte]identityEntry{}
		}
	}
	c.entries[key] = identityEntry{user: user, expires: now.Add(identityCacheTTL)}
}

func (c *identityCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[[32]byte]identityEntry{}
}

// authenticate verifies the request's user token and returns the user it
// belongs to. The identity comes from the API server, not from the
// X-Forwarded-User header, so activity entries cannot be attributed to
// someone else and a request that bypasses oauth-proxy with a made-up token
// is refused.
func authenticate(ctx context.Context, token string) (string, error) {
	key := sha256.Sum256([]byte(token))
	now := time.Now()
	if user, ok := identities.get(key, now); ok {
		return user, nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	user, err := lookupUser(lookupCtx, token)
	if err != nil {
		return "", err
	}
	identities.put(key, user, now)
	return user, nil
}

type identityKey struct{}

func withIdentity(r *http.Request, user string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), identityKey{}, user))
}

// writeAuthError answers a failed identity or permission check: 401
// "session_expired" when the API server rejected the user's token (log in
// again), 503 "authorization_unavailable" when it could not be asked.
func writeAuthError(w http.ResponseWriter, err error, unavailableMsg string) {
	if errors.Is(err, cluster.ErrUnauthenticated) {
		writeError(w, "Your OpenShift session has expired. Log in again.", http.StatusUnauthorized, "session_expired")
		return
	}
	slog.Warn("cannot verify the user with the API server", "error", err)
	writeError(w, unavailableMsg, http.StatusServiceUnavailable, "authorization_unavailable")
}

// requestUser authenticates the request's token. It writes the error
// response and returns false when the request must not proceed.
func requestUser(w http.ResponseWriter, r *http.Request) (string, *http.Request, bool) {
	userToken := extractUserToken(r)
	if userToken == "" {
		writeError(w, "no auth token", http.StatusUnauthorized, "unauthorized")
		return "", r, false
	}
	user, err := authenticate(r.Context(), userToken)
	if err != nil {
		writeAuthError(w, err, "Cannot verify your identity with the OpenShift API. Try again shortly.")
		return "", r, false
	}
	if header := r.Header.Get("X-Forwarded-User"); header != "" && header != user {
		slog.Debug("X-Forwarded-User differs from the token owner; using the token owner", "header", header, "user", user)
	}
	return user, withIdentity(r, user), true
}

// devModeWithoutServiceAccount is the local `dev.sh` setup: DEV_MODE with a
// DEV_TOKEN and no mounted ServiceAccount token.
func devModeWithoutServiceAccount() bool {
	if os.Getenv("DEV_MODE") != "true" || os.Getenv("DEV_TOKEN") == "" {
		return false
	}
	_, err := os.Stat(serviceAccountTokenPath)
	return os.IsNotExist(err)
}
