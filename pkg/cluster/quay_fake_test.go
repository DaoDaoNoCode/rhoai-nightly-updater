package cluster

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRegistry answers quay.io and api.github.com requests in memory and
// counts them by kind, so tests can assert how many external calls a code
// path makes.
type fakeRegistry struct {
	mu      sync.Mutex
	counts  map[string]int
	scopes  [][]string // scopes of each token request
	latency time.Duration

	tags       []string          // tag list (sorted on use)
	tagDigests map[string]string // tag -> digest for HEAD
	// labels per image digest; a digest that is missing here returns a config without labels
	labels map[string]map[string]string
	// fbcLayers per FBC manifest digest: gzip tar layer blob
	fbcLayers map[string][]byte
	// status overrides by kind ("quay-token", "quay-manifest", "quay-blob", "quay-head", "github")
	status map[string]int
	// headers added to github responses
	githubHeaders http.Header
	githubDate    string
	tokenExpiry   time.Duration
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{
		counts:      map[string]int{},
		tagDigests:  map[string]string{},
		labels:      map[string]map[string]string{},
		fbcLayers:   map[string][]byte{},
		status:      map[string]int{},
		githubDate:  "2026-10-01T00:00:00Z",
		tokenExpiry: time.Hour,
	}
}

func (f *fakeRegistry) count(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[kind]
}

func (f *fakeRegistry) setStatus(kind string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[kind] = status
}

func fakeJWT(exp time.Time) string {
	enc := base64.RawURLEncoding
	payload, _ := json.Marshal(map[string]int64{"exp": exp.Unix()})
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString(payload) + ".sig"
}

func (f *fakeRegistry) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.latency > 0 {
		select {
		case <-time.After(f.latency):
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	kind, status, body, header := f.answer(r)
	f.mu.Lock()
	f.counts[kind]++
	if s, ok := f.status[kind]; ok {
		status = s
	}
	f.mu.Unlock()
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
}

func (f *fakeRegistry) answer(r *http.Request) (kind string, status int, body []byte, header http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	switch {
	case r.URL.Host == "api.github.com":
		h := http.Header{}
		for k, v := range f.githubHeaders {
			h[k] = v
		}
		return "github", 200, []byte(fmt.Sprintf(`{"committer":{"date":%q}}`, f.githubDate)), h
	case path == "/v2/auth":
		f.scopes = append(f.scopes, r.URL.Query()["scope"])
		return "quay-token", 200, []byte(fmt.Sprintf(`{"token":%q}`, fakeJWT(time.Now().Add(f.tokenExpiry)))), nil
	case strings.HasSuffix(path, "/tags/list"):
		last := r.URL.Query().Get("last")
		tags := append([]string(nil), f.tags...)
		sort.Strings(tags)
		var page []string
		for _, t := range tags {
			if t > last && len(page) < 100 {
				page = append(page, t)
			}
		}
		b, _ := json.Marshal(map[string][]string{"tags": page})
		return "quay-tags", 200, b, nil
	case strings.Contains(path, "/manifests/") && r.Method == http.MethodHead:
		tag := path[strings.LastIndex(path, "/")+1:]
		digest, ok := f.tagDigests[tag]
		if !ok {
			return "quay-head", 404, nil, nil
		}
		return "quay-head", 200, nil, http.Header{"Docker-Content-Digest": {digest}}
	case strings.Contains(path, "/manifests/"):
		ref := path[strings.LastIndex(path, "/")+1:]
		h := http.Header{"Content-Type": {"application/vnd.oci.image.manifest.v1+json"}}
		if strings.HasPrefix(path, "/v2/rhoai/rhoai-fbc-fragment/") {
			if _, ok := f.fbcLayers[ref]; ok {
				b, _ := json.Marshal(map[string]interface{}{"config": map[string]string{"digest": "cfg-" + ref}, "layers": []map[string]string{{"digest": "layer-" + ref}}})
				return "quay-manifest", 200, b, h
			}
		}
		return "quay-manifest", 200, []byte(fmt.Sprintf(`{"config":{"digest":"cfg-%s"}}`, ref)), h
	case strings.Contains(path, "/blobs/layer-"):
		ref := strings.TrimPrefix(path[strings.LastIndex(path, "/")+1:], "layer-")
		return "quay-layer", 200, f.fbcLayers[ref], nil
	case strings.Contains(path, "/blobs/cfg-"):
		ref := strings.TrimPrefix(path[strings.LastIndex(path, "/")+1:], "cfg-")
		labels, ok := f.labels[ref]
		if !ok {
			return "quay-blob", 200, []byte(`{"config":{}}`), nil
		}
		b, _ := json.Marshal(map[string]interface{}{"config": map[string]interface{}{"Labels": labels}})
		return "quay-blob", 200, b, nil
	}
	return "other", 404, nil, nil
}

// installFakeRegistry routes quayHTTPClient through f and resets every
// registry/GitHub cache so tests do not see each other's entries.
func installFakeRegistry(t *testing.T, f *fakeRegistry) {
	t.Helper()
	resetRegistryCaches()
	original := quayHTTPClient
	quayHTTPClient = &http.Client{Transport: f, Timeout: 10 * time.Second}
	t.Cleanup(func() {
		nightlyBackground.Wait()
		quayHTTPClient = original
		resetRegistryCaches()
	})
}

func resetRegistryCaches() {
	quayTokenCache.Purge()
	labelCache.Purge()
	commitDateCache.Purge()
	tagDigestCache.Purge()
	fbcContentCache.Purge()
	nightlyStatusCache.Purge()
	dashboardWarmAttempts.Purge()
	prCache.Purge()
	compareCache.Purge()
	githubBackoff.Lock()
	githubBackoff.until = time.Time{}
	githubBackoff.Unlock()
	tagScanCacheMu.Lock()
	tagScanCache = nil
	tagScanCacheMu.Unlock()
}

func digestOf(n int) string { return fmt.Sprintf("sha256:%064d", n) }
