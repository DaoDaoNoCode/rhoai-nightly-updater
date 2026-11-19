package middleware

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecurityHeaders_SetsXContentTypeOptions(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := SecurityHeaders(inner)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rr, req)

	got := rr.Header().Get("X-Content-Type-Options")
	if got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
	}
}

func TestSecurityHeaders_SetsXFrameOptions(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := SecurityHeaders(inner)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rr, req)

	got := rr.Header().Get("X-Frame-Options")
	if got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want %q", got, "DENY")
	}
}

func TestSecurityHeaders_SetsCacheControl(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := SecurityHeaders(inner)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rr, req)

	want := "no-store, no-cache, must-revalidate"
	got := rr.Header().Get("Cache-Control")
	if got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
}

func TestSecurityHeaders_SetsPragma(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := SecurityHeaders(inner)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rr, req)

	got := rr.Header().Get("Pragma")
	if got != "no-cache" {
		t.Errorf("Pragma = %q, want %q", got, "no-cache")
	}
}

func TestSecurityHeaders_PassesThroughToNextHandler(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte("hello"))
	})
	handler := SecurityHeaders(inner)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test-path", nil)
	handler.ServeHTTP(rr, req)

	if !called {
		t.Error("inner handler was not called")
	}
	if rr.Code != http.StatusTeapot {
		t.Errorf("status code = %d, want %d", rr.Code, http.StatusTeapot)
	}
	if body := rr.Body.String(); body != "hello" {
		t.Errorf("body = %q, want %q", body, "hello")
	}
}

// default-src does not cover base-uri, form-action, frame-ancestors (or,
// in older browsers, object-src): the policy names each.
func TestSecurityHeaders_CSPDirectives(t *testing.T) {
	handler := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	directives := map[string]string{}
	for _, d := range strings.Split(rr.Header().Get("Content-Security-Policy"), ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(d), " ")
		directives[name] = value
	}
	for name, want := range map[string]string{
		"default-src":     "'self'",
		"base-uri":        "'none'",
		"form-action":     "'self'",
		"frame-ancestors": "'none'",
		"object-src":      "'none'",
		"img-src":         "'self' data:",
	} {
		if got := directives[name]; got != want {
			t.Errorf("CSP %s = %q, want %q (policy %q)", name, got, want, rr.Header().Get("Content-Security-Policy"))
		}
	}
	if _, ok := directives["script-src"]; ok {
		t.Error("script-src is set; default-src 'self' already allows only same-origin scripts")
	}
}

// The static files replace Cache-Control (hashed assets are immutable,
// index.html is revalidated); the security headers stay.
func TestSecurityHeaders_StaticCacheControl(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bundle.435605b5.js"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := SecurityHeaders(StaticFiles(dir))
	for path, want := range map[string]string{"/": "no-cache", "/bundle.435605b5.js": "public, max-age=31536000, immutable"} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rr.Header().Get("Cache-Control"); got != want {
			t.Errorf("%s: Cache-Control = %q, want %q", path, got, want)
		}
		if rr.Header().Get("Content-Security-Policy") != ContentSecurityPolicy {
			t.Errorf("%s: no CSP", path)
		}
	}
}
