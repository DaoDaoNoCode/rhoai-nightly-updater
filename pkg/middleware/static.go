package middleware

import (
	"bytes"
	"compress/gzip"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Cache-Control values for the single-page app's files.
const (
	// Content-hashed files never change under the same name.
	cacheImmutable = "public, max-age=31536000, immutable"
	// index.html and other unhashed files must be revalidated so a new
	// deploy is picked up on the next load (no-cache still allows 304s).
	cacheRevalidate = "no-cache"
)

// hashPart matches a file-name segment that is a content hash, as emitted
// by webpack for [contenthash] (bundle.435605b5.js, styles.<hash>.css,
// 123.<hash>.js) and for asset modules ([hash][ext], e.g. fonts).
var hashPart = regexp.MustCompile(`^[0-9a-f]{8,}$`)

// IsHashedAsset reports whether the base name carries a content hash in a
// dot-separated segment other than the extension.
func IsHashedAsset(name string) bool {
	parts := strings.Split(path.Base(name), ".")
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts[:len(parts)-1] {
		if hashPart.MatchString(p) {
			return true
		}
	}
	return false
}

// compressible lists the extensions worth gzipping (fonts like woff2 are
// already compressed).
var compressible = map[string]bool{
	".html": true, ".js": true, ".css": true, ".svg": true, ".json": true,
	".map": true, ".txt": true, ".ttf": true, ".eot": true, ".ico": true,
}

const maxGzipSource = 16 << 20

type gzipEntry struct {
	modTime time.Time
	size    int64
	data    []byte
}

// StaticFiles serves the built frontend from dir:
//   - existing files, with long-lived caching for content-hashed names and
//     revalidation for everything else (index.html);
//   - gzip when the client accepts it, from a precompressed "<file>.gz"
//     next to the file or compressed once in memory;
//   - index.html for extension-less paths (client-side routes);
//   - 404 for missing files with an extension (for example a bundle from
//     an older build) and a JSON 404 for unknown /api/ paths, instead of
//     answering them with index.html.
func StaticFiles(dir string) http.Handler {
	root, err := filepath.Abs(dir)
	if err != nil {
		root = dir
	}
	var gzCache sync.Map // full path -> *gzipEntry
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		urlPath := path.Clean("/" + r.URL.Path)
		if urlPath == "/api" || strings.HasPrefix(urlPath, "/api/") {
			APINotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		full := filepath.Join(root, filepath.FromSlash(urlPath))
		if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		info, statErr := os.Stat(full)
		if statErr != nil || info.IsDir() {
			if path.Ext(urlPath) != "" {
				http.NotFound(w, r)
				return
			}
			full = filepath.Join(root, "index.html")
			info, statErr = os.Stat(full)
			if statErr != nil {
				http.NotFound(w, r)
				return
			}
		}

		name := filepath.Base(full)
		if IsHashedAsset(name) {
			w.Header().Set("Cache-Control", cacheImmutable)
		} else {
			w.Header().Set("Cache-Control", cacheRevalidate)
		}
		w.Header().Del("Pragma")
		ext := strings.ToLower(filepath.Ext(name))
		if ctype := mime.TypeByExtension(ext); ctype != "" {
			w.Header().Set("Content-Type", ctype)
		}

		if compressible[ext] {
			w.Header().Add("Vary", "Accept-Encoding")
			if acceptsGzip(r.Header.Get("Accept-Encoding")) {
				if data, modTime, ok := gzipped(&gzCache, full, info); ok {
					w.Header().Set("Content-Encoding", "gzip")
					http.ServeContent(w, r, name, modTime, bytes.NewReader(data))
					return
				}
			}
		}
		http.ServeFile(w, r, full)
	})
}

// gzipped returns the gzip encoding of a file: a precompressed sibling when
// it is at least as new, or the file compressed once and cached in memory.
func gzipped(cache *sync.Map, full string, info os.FileInfo) ([]byte, time.Time, bool) {
	if gzInfo, err := os.Stat(full + ".gz"); err == nil && !gzInfo.IsDir() && !gzInfo.ModTime().Before(info.ModTime()) {
		if data, err := os.ReadFile(full + ".gz"); err == nil {
			return data, info.ModTime(), true
		}
	}
	if v, ok := cache.Load(full); ok {
		e := v.(*gzipEntry)
		if e.modTime.Equal(info.ModTime()) && e.size == info.Size() {
			return e.data, e.modTime, true
		}
	}
	if info.Size() > maxGzipSource {
		return nil, time.Time{}, false
	}
	src, err := os.ReadFile(full)
	if err != nil {
		return nil, time.Time{}, false
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := zw.Write(src); err != nil {
		return nil, time.Time{}, false
	}
	if err := zw.Close(); err != nil {
		return nil, time.Time{}, false
	}
	cache.Store(full, &gzipEntry{modTime: info.ModTime(), size: info.Size(), data: buf.Bytes()})
	return buf.Bytes(), info.ModTime(), true
}

// acceptsGzip parses Accept-Encoding for gzip (or *) with a non-zero q.
func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		coding := strings.ToLower(strings.TrimSpace(fields[0]))
		if coding != "gzip" && coding != "*" {
			continue
		}
		q := 1.0
		for _, f := range fields[1:] {
			f = strings.TrimSpace(f)
			if strings.HasPrefix(f, "q=") {
				if v, err := strconv.ParseFloat(strings.TrimPrefix(f, "q="), 64); err == nil {
					q = v
				}
			}
		}
		if q > 0 {
			return true
		}
	}
	return false
}

// APINotFound answers an unknown /api/ path with a JSON 404, so clients
// see "not found" rather than the app's HTML.
func APINotFound(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"unknown API endpoint","errorCode":"not_found"}`))
}
