package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const requestIDHeader = "X-Request-Id"

// validRequestID bounds client-supplied IDs, which end up in logs and in the
// response header.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type requestIDKey struct{}

// RequestIDFrom returns the request ID stored by RequestID, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// RequestID wraps an http.Handler and assigns a request ID to every
// request. A well-formed incoming X-Request-Id (e.g., from a load balancer)
// is preserved; otherwise a new random hex ID is generated. The ID is set
// on the response header and in the request context.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validRequestID.MatchString(id) {
			id = generateID()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

func generateID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// accessRecorder captures the response status for the access log.
type accessRecorder struct {
	http.ResponseWriter
	status int
}

func (a *accessRecorder) WriteHeader(code int) {
	if a.status == 0 {
		a.status = code
	}
	a.ResponseWriter.WriteHeader(code)
}

func (a *accessRecorder) Write(b []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	return a.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach Flush and deadlines (SSE).
func (a *accessRecorder) Unwrap() http.ResponseWriter { return a.ResponseWriter }

// AccessLog logs one line per request with the request ID. API requests
// are logged at Info; probes, metrics and static files at Debug.
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &accessRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		level := slog.LevelDebug
		if strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/api/health") {
			level = slog.LevelInfo
		}
		if rec.status >= 500 {
			level = slog.LevelWarn
		}
		slog.Log(r.Context(), level, "request",
			"requestId", RequestIDFrom(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"durationMs", time.Since(start).Milliseconds())
	})
}
