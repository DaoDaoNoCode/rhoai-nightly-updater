package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLocalOnlyBlocksDNSRebinding(t *testing.T) {
	h := LocalOnly([]string{"8080", "9000"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		host, origin string
		want         int
	}{
		{"127.0.0.1:8080", "", 204},
		{"localhost:8080", "", 204},
		{"LOCALHOST:9000", "http://localhost:9000", 204},
		{"[::1]:8080", "", 204},
		{"127.0.0.1:9000", "http://127.0.0.1:9000", 204},
		{"rebind.attacker.example:8080", "", 403},
		{"127.0.0.1", "", 403},
		{"127.0.0.1:8081", "", 403},
		{"127.0.0.1:8080", "http://rebind.attacker.example:8080", 403},
		{"127.0.0.1:8080", "http://127.0.0.1:1234", 403},
		{"127.0.0.1:8080", "null", 403},
	} {
		r := httptest.NewRequest("POST", "/api/update/stream", nil)
		r.Host = tc.host
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("Host %q Origin %q: %d, want %d", tc.host, tc.origin, w.Code, tc.want)
		}
	}
}

func TestRequestIDRejectsMalformedClientIDs(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = RequestIDFrom(r.Context()) }))
	for _, id := range []string{strings.Repeat("a", 5000), "bad id", "x\ny", "<script>"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-Request-Id", id)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		got := w.Header().Get("X-Request-Id")
		if got == id || len(got) != 16 || seen != got {
			t.Errorf("client ID %q: response %q context %q", id[:min(len(id), 20)], got, seen)
		}
	}
}

func TestAccessLogIncludesRequestID(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	h := RequestID(AccessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})))
	r := httptest.NewRequest("GET", "/api/status", nil)
	r.Header.Set("X-Request-Id", "req-123")
	h.ServeHTTP(httptest.NewRecorder(), r)
	line := buf.String()
	for _, want := range []string{`"requestId":"req-123"`, `"path":"/api/status"`, `"status":418`} {
		if !strings.Contains(line, want) {
			t.Errorf("access log %q missing %s", line, want)
		}
	}
}
