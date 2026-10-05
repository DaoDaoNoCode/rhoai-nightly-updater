package middleware

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeDist(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"index.html":                 "<html>app</html>",
		"bundle.435605b5.js":         strings.Repeat("console.log('x');", 200),
		"styles.0a1b2c3d.css":        strings.Repeat("body{color:red}", 200),
		"7d2c1e0f9a8b7c6d5e4f.woff2": "font",
		"312.9f8e7d6c.js":            "chunk",
		"favicon.ico":                "ico",
		"precompressed.11223344.js":  strings.Repeat("y", 500),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte("PRECOMPRESSED"))
	zw.Close()
	if err := os.WriteFile(filepath.Join(dir, "precompressed.11223344.js.gz"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	os.Chtimes(filepath.Join(dir, "precompressed.11223344.js.gz"), later, later)
	return dir
}

func serve(h http.Handler, method, path, acceptEncoding string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if acceptEncoding != "" {
		r.Header.Set("Accept-Encoding", acceptEncoding)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	out, _ := io.ReadAll(zr)
	return string(out)
}

func TestStaticFiles(t *testing.T) {
	dir := writeDist(t)
	// Security headers wrap the file server as in main.go; the file
	// server's caching policy must win over the API default.
	h := SecurityHeaders(StaticFiles(dir))

	for _, tc := range []struct {
		path, wantCache, wantType string
		wantStatus                int
	}{
		{"/bundle.435605b5.js", cacheImmutable, "javascript", 200},
		{"/styles.0a1b2c3d.css", cacheImmutable, "text/css", 200},
		{"/7d2c1e0f9a8b7c6d5e4f.woff2", cacheImmutable, "font/woff2", 200},
		{"/312.9f8e7d6c.js", cacheImmutable, "javascript", 200},
		{"/", cacheRevalidate, "text/html", 200},
		{"/components", cacheRevalidate, "text/html", 200},
		{"/dashboard-dev/some/route", cacheRevalidate, "text/html", 200},
		{"/favicon.ico", cacheRevalidate, "", 200},
	} {
		w := serve(h, "GET", tc.path, "")
		if w.Code != tc.wantStatus || w.Header().Get("Cache-Control") != tc.wantCache || !strings.Contains(w.Header().Get("Content-Type"), tc.wantType) {
			t.Errorf("%s: %d cache=%q type=%q", tc.path, w.Code, w.Header().Get("Cache-Control"), w.Header().Get("Content-Type"))
		}
		if w.Header().Get("Pragma") != "" {
			t.Errorf("%s: Pragma %q on a static file", tc.path, w.Header().Get("Pragma"))
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s: security headers missing", tc.path)
		}
	}

	// Missing files with an extension are 404, not the app shell.
	for _, p := range []string{"/bundle.deadbeef.js", "/styles.00000000.css", "/missing.png"} {
		if w := serve(h, "GET", p, ""); w.Code != 404 || strings.Contains(w.Body.String(), "<html>") {
			t.Errorf("%s: %d %q", p, w.Code, w.Body.String())
		}
	}
	// Unknown API paths get a JSON 404.
	for _, p := range []string{"/api/does-not-exist", "/api"} {
		w := serve(h, "GET", p, "")
		if w.Code != 404 || !strings.Contains(w.Header().Get("Content-Type"), "application/json") || !strings.Contains(w.Body.String(), `"errorCode":"not_found"`) {
			t.Errorf("%s: %d %s %q", p, w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
	}
	// Path traversal stays inside the directory.
	if w := serve(h, "GET", "/../../etc/passwd", ""); w.Code == 200 || strings.Contains(w.Body.String(), "root:") {
		t.Errorf("traversal: %d", w.Code)
	}
	if w := serve(h, "POST", "/bundle.435605b5.js", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST to a file: %d", w.Code)
	}
}

func TestStaticFilesGzip(t *testing.T) {
	dir := writeDist(t)
	h := StaticFiles(dir)
	orig, _ := os.ReadFile(filepath.Join(dir, "bundle.435605b5.js"))

	w := serve(h, "GET", "/bundle.435605b5.js", "gzip, deflate, br")
	if w.Header().Get("Content-Encoding") != "gzip" || !strings.Contains(w.Header().Get("Vary"), "Accept-Encoding") {
		t.Fatalf("no gzip: %v", w.Header())
	}
	if got := gunzip(t, w.Body.Bytes()); got != string(orig) {
		t.Fatal("gzip body differs from the file")
	}
	if w.Body.Len() >= len(orig) {
		t.Fatalf("gzip did not shrink: %d >= %d", w.Body.Len(), len(orig))
	}
	// Second request is served from the cache with the same bytes.
	if w2 := serve(h, "GET", "/bundle.435605b5.js", "gzip"); !bytes.Equal(w2.Body.Bytes(), w.Body.Bytes()) {
		t.Fatal("cached gzip differs")
	}
	// Precompressed sibling wins.
	if w := serve(h, "GET", "/precompressed.11223344.js", "gzip"); gunzip(t, w.Body.Bytes()) != "PRECOMPRESSED" {
		t.Fatal("precompressed .gz not used")
	}
	// The app shell is compressed too.
	if w := serve(h, "GET", "/components", "gzip"); w.Header().Get("Content-Encoding") != "gzip" || gunzip(t, w.Body.Bytes()) != "<html>app</html>" {
		t.Fatalf("index.html not gzipped: %v", w.Header())
	}
	// No gzip unless accepted; fonts are never recompressed.
	for _, tc := range []struct{ path, ae string }{
		{"/bundle.435605b5.js", ""},
		{"/bundle.435605b5.js", "gzip;q=0, br"},
		{"/bundle.435605b5.js", "identity"},
		{"/7d2c1e0f9a8b7c6d5e4f.woff2", "gzip"},
	} {
		w := serve(h, "GET", tc.path, tc.ae)
		if w.Header().Get("Content-Encoding") != "" {
			t.Errorf("%s with %q: unexpected gzip", tc.path, tc.ae)
		}
	}
}

func TestIsHashedAsset(t *testing.T) {
	for name, want := range map[string]bool{
		"bundle.435605b5.js":         true,
		"styles.0a1b2c3d.css":        true,
		"vendors.0a1b2c3d.chunk.js":  true,
		"7d2c1e0f9a8b7c6d5e4f.woff2": true,
		"index.html":                 false,
		"favicon.ico":                false,
		"bundle.js":                  false,
		"logo.short12.svg":           false,
		"bundle.DEADBEEF.js":         false,
	} {
		if got := IsHashedAsset(name); got != want {
			t.Errorf("IsHashedAsset(%q) = %v", name, got)
		}
	}
}

func TestAcceptsGzip(t *testing.T) {
	for h, want := range map[string]bool{
		"": false, "gzip": true, "GZIP": true, "br, gzip;q=0.5": true, "gzip;q=0": false, "*": true, "deflate": false,
	} {
		if acceptsGzip(h) != want {
			t.Errorf("acceptsGzip(%q) != %v", h, want)
		}
	}
}
