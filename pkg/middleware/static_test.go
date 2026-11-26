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

func serve(t testing.TB, h http.Handler, method, path, acceptEncoding string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(t.Context(), method, path, nil)
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
		{"/favicon.ico", cacheRevalidate, "image/x-icon", 200},
		{"/index.html", cacheRevalidate, "text/html", 200},
	} {
		w := serve(t, h, "GET", tc.path, "")
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
		if w := serve(t, h, "GET", p, ""); w.Code != 404 || strings.Contains(w.Body.String(), "<html>") {
			t.Errorf("%s: %d %q", p, w.Code, w.Body.String())
		}
	}
	// Unknown API paths get a JSON 404.
	for _, p := range []string{"/api/does-not-exist", "/api"} {
		w := serve(t, h, "GET", p, "")
		if w.Code != 404 || !strings.Contains(w.Header().Get("Content-Type"), "application/json") || !strings.Contains(w.Body.String(), `"errorCode":"not_found"`) {
			t.Errorf("%s: %d %s %q", p, w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
	}
	// Path traversal stays inside the directory.
	if w := serve(t, h, "GET", "/../../etc/passwd", ""); strings.Contains(w.Body.String(), "root:") || (w.Code == 200 && w.Body.String() != "<html>app</html>") {
		t.Errorf("traversal: %d", w.Code)
	}
	if w := serve(t, h, "POST", "/bundle.435605b5.js", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST to a file: %d", w.Code)
	}
}

func TestStaticFilesGzip(t *testing.T) {
	dir := writeDist(t)
	h := StaticFiles(dir)
	orig, _ := os.ReadFile(filepath.Join(dir, "bundle.435605b5.js"))

	w := serve(t, h, "GET", "/bundle.435605b5.js", "gzip, deflate, br")
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
	if w2 := serve(t, h, "GET", "/bundle.435605b5.js", "gzip"); !bytes.Equal(w2.Body.Bytes(), w.Body.Bytes()) {
		t.Fatal("cached gzip differs")
	}
	// Precompressed sibling wins.
	if w := serve(t, h, "GET", "/precompressed.11223344.js", "gzip"); gunzip(t, w.Body.Bytes()) != "PRECOMPRESSED" {
		t.Fatal("precompressed .gz not used")
	}
	// The app shell is compressed too.
	if w := serve(t, h, "GET", "/components", "gzip"); w.Header().Get("Content-Encoding") != "gzip" || gunzip(t, w.Body.Bytes()) != "<html>app</html>" {
		t.Fatalf("index.html not gzipped: %v", w.Header())
	}
	// No gzip unless accepted; fonts are never recompressed.
	for _, tc := range []struct{ path, ae string }{
		{"/bundle.435605b5.js", ""},
		{"/bundle.435605b5.js", "gzip;q=0, br"},
		{"/bundle.435605b5.js", "identity"},
		{"/7d2c1e0f9a8b7c6d5e4f.woff2", "gzip"},
	} {
		w := serve(t, h, "GET", tc.path, tc.ae)
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

// Unhashed files (index.html) are revalidated by a strong ETag of the bytes
// served, with no Last-Modified: after a rollback to an older build, the
// browser's newer copy, sent back with a newer If-Modified-Since, gets the
// older index.html (200) rather than a 304 that keeps it loading chunks the
// older build lacks. Hashed assets keep their dates and stay immutable.
func TestStaticFilesRevalidateByContent(t *testing.T) {
	dir := writeDist(t)
	h := StaticFiles(dir)
	get := func(path, acceptEncoding string, header map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), "GET", path, nil)
		if acceptEncoding != "" {
			r.Header.Set("Accept-Encoding", acceptEncoding)
		}
		for k, v := range header {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	first := get("/", "", nil)
	etag := first.Header().Get("ETag")
	if first.Code != 200 || !strings.HasPrefix(etag, `"`) || strings.HasPrefix(etag, "W/") || first.Header().Get("Last-Modified") != "" {
		t.Fatalf("index.html: %d ETag %q Last-Modified %q", first.Code, etag, first.Header().Get("Last-Modified"))
	}
	if w := get("/components", "", map[string]string{"If-None-Match": etag}); w.Code != http.StatusNotModified {
		t.Fatalf("matching If-None-Match: %d", w.Code)
	}

	// Rollback: the older build's index.html is on disk, with an older
	// modification time than the copy the browser holds.
	older := time.Now().Add(-24 * time.Hour)
	index := filepath.Join(dir, "index.html")
	if err := os.WriteFile(index, []byte("<html>older build</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(index, older, older); err != nil {
		t.Fatal(err)
	}
	newer := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	w := get("/", "", map[string]string{"If-Modified-Since": newer, "If-None-Match": etag})
	if w.Code != 200 || w.Body.String() != "<html>older build</html>" || w.Header().Get("ETag") == etag {
		t.Fatalf("after a rollback: %d %q ETag %q", w.Code, w.Body.String(), w.Header().Get("ETag"))
	}
	if w := get("/", "", map[string]string{"If-Modified-Since": newer}); w.Code != 200 {
		t.Fatalf("If-Modified-Since alone: %d", w.Code)
	}

	// The gzip encoding has an ETag of its own bytes.
	gz := get("/", "gzip", nil)
	gzTag := gz.Header().Get("ETag")
	if gz.Header().Get("Content-Encoding") != "gzip" || gunzip(t, gz.Body.Bytes()) != "<html>older build</html>" || gzTag == "" || gzTag == w.Header().Get("ETag") || gz.Header().Get("Last-Modified") != "" {
		t.Fatalf("gzip: %v", gz.Header())
	}
	if w := get("/", "gzip", map[string]string{"If-None-Match": gzTag}); w.Code != http.StatusNotModified {
		t.Fatalf("gzip If-None-Match: %d", w.Code)
	}
	if w := get("/", "gzip", map[string]string{"If-None-Match": w.Header().Get("ETag")}); w.Code != 200 {
		t.Fatalf("identity ETag on a gzip request: %d", w.Code)
	}
	if w := get("/", "gzip", map[string]string{"If-Modified-Since": newer}); w.Code != 200 {
		t.Fatalf("gzip If-Modified-Since: %d", w.Code)
	}

	// Hashed assets: immutable, dated, no ETag.
	for _, ae := range []string{"", "gzip"} {
		a := get("/bundle.435605b5.js", ae, nil)
		if a.Header().Get("Cache-Control") != cacheImmutable || a.Header().Get("Last-Modified") == "" || a.Header().Get("ETag") != "" {
			t.Fatalf("hashed asset (%q): %v", ae, a.Header())
		}
	}
}

// The ETag is of the bytes served now: a replacement of the same size and
// modification time gets a new tag, for both encodings. HEAD answers the
// chosen encoding's tag with no body; a strong If-Range with that tag
// gives the range (206), the other encoding's tag the whole file (200).
func TestStaticFilesETagFollowsTheBytesServed(t *testing.T) {
	dir := writeDist(t)
	h := StaticFiles(dir)
	req := func(method, path, acceptEncoding string, header map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), method, path, nil)
		if acceptEncoding != "" {
			r.Header.Set("Accept-Encoding", acceptEncoding)
		}
		for k, v := range header {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	index := filepath.Join(dir, "index.html")
	stamp := time.Now().Add(-time.Hour).Truncate(time.Second)
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(index, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(index, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	write("<html>build A</html>")
	idA, gzA := req("GET", "/", "", nil).Header().Get("ETag"), req("GET", "/", "gzip", nil).Header().Get("ETag")
	write("<html>build B</html>") // same size, same modification time
	idB, gzB := req("GET", "/", "", nil), req("GET", "/", "gzip", nil)
	if idB.Body.String() != "<html>build B</html>" || gunzip(t, gzB.Body.Bytes()) != "<html>build B</html>" {
		t.Fatalf("stale content: %q", idB.Body.String())
	}
	if idB.Header().Get("ETag") == idA || gzB.Header().Get("ETag") == gzA || idB.Header().Get("ETag") == gzB.Header().Get("ETag") {
		t.Fatalf("tags: identity %s -> %s, gzip %s -> %s", idA, idB.Header().Get("ETag"), gzA, gzB.Header().Get("ETag"))
	}
	if w := req("GET", "/", "", map[string]string{"If-None-Match": idA}); w.Code != 200 {
		t.Fatalf("the replaced file's tag: %d", w.Code)
	}

	idTag, gzTag := idB.Header().Get("ETag"), gzB.Header().Get("ETag")
	for _, tc := range []struct{ ae, tag string }{{"", idTag}, {"gzip", gzTag}} {
		w := req("HEAD", "/", tc.ae, nil)
		if w.Code != 200 || w.Header().Get("ETag") != tc.tag || w.Body.Len() != 0 {
			t.Fatalf("HEAD (%q): %d ETag %q body %d bytes", tc.ae, w.Code, w.Header().Get("ETag"), w.Body.Len())
		}
	}
	if w := req("GET", "/", "", map[string]string{"Range": "bytes=0-5", "If-Range": idTag}); w.Code != http.StatusPartialContent || w.Body.String() != "<html>" {
		t.Fatalf("If-Range with the tag: %d %q", w.Code, w.Body.String())
	}
	if w := req("GET", "/", "", map[string]string{"Range": "bytes=0-5", "If-Range": gzTag}); w.Code != 200 || w.Body.String() != "<html>build B</html>" {
		t.Fatalf("If-Range with the gzip tag on identity: %d %q", w.Code, w.Body.String())
	}
	if w := req("GET", "/", "gzip", map[string]string{"Range": "bytes=0-5", "If-Range": idTag}); w.Code != 200 || gunzip(t, w.Body.Bytes()) != "<html>build B</html>" {
		t.Fatalf("If-Range with the identity tag on gzip: %d", w.Code)
	}
}
