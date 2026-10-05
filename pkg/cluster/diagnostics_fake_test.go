package cluster

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeAPI is a strict Kubernetes API fake for diagnostics tests. Routes are
// keyed by "METHOD /path". Unknown GETs return 404 (the object does not
// exist); unknown mutations return 501 so that an unexpected write fails the
// operation visibly, and the test can assert the exact writes made.
type fakeAPI struct {
	mu     sync.Mutex
	routes map[string]func(r *http.Request, body []byte) (int, string)
	reqs   []fakeReq
}

type fakeReq struct {
	Method string
	Path   string
	Query  string
	Body   string
}

const k8sNotFound = `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404,"message":"not found"}`

func newFakeAPI(t *testing.T) (*fakeAPI, *Client) {
	t.Helper()
	f := &fakeAPI{routes: map[string]func(*http.Request, []byte) (int, string){}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, fakeReq{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(body)})
		h, ok := f.routes[r.Method+" "+r.URL.Path]
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case ok:
			status, resp := h(r, body)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, resp)
		case strings.Contains(r.URL.Path, "/configmaps"):
			// Activity log side effects.
			if r.Method == http.MethodGet {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, k8sNotFound)
				return
			}
			_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"ConfigMap","data":{}}`)
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, k8sNotFound)
		default:
			w.WriteHeader(http.StatusNotImplemented)
			_, _ = io.WriteString(w, `{"kind":"Status","status":"Failure","reason":"NotImplemented","code":501,"message":"unexpected write in test"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return f, &Client{baseURL: srv.URL, token: "test", httpClient: srv.Client(), ctx: context.Background()}
}

// json registers a fixed response.
func (f *fakeAPI) json(method, path string, status int, body string) {
	f.handle(method, path, func(*http.Request, []byte) (int, string) { return status, body })
}

func (f *fakeAPI) obj(method, path string, v interface{}) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	f.json(method, path, http.StatusOK, string(b))
}

func (f *fakeAPI) handle(method, path string, h func(*http.Request, []byte) (int, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[method+" "+path] = h
}

func (f *fakeAPI) status(method, path string, code int, reason string) {
	f.json(method, path, code, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"`+reason+`","code":`+itoa(code)+`}`)
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// writes returns "METHOD path" of every non-GET request, excluding the
// activity log ConfigMap, sorted.
func (f *fakeAPI) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.reqs {
		if r.Method == http.MethodGet || strings.Contains(r.Path, "/configmaps") {
			continue
		}
		out = append(out, r.Method+" "+r.Path)
	}
	sort.Strings(out)
	return out
}

func (f *fakeAPI) requests(method, path string) []fakeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeReq
	for _, r := range f.reqs {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeAPI) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func assertWrites(t *testing.T, f *fakeAPI, want ...string) {
	t.Helper()
	got := f.writes()
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("writes mismatch\n got: %q\nwant: %q", got, want)
	}
}

func findProblem(resp *DiagnosticsResponse, id string) *Problem {
	for i := range resp.Problems {
		if resp.Problems[i].ID == id {
			return &resp.Problems[i]
		}
	}
	return nil
}

func findCheck(resp *DiagnosticsResponse, name string) *CheckResult {
	for i := range resp.Checks {
		if resp.Checks[i].Name == name {
			return &resp.Checks[i]
		}
	}
	return nil
}

// serveComponentGroup serves the API group discovery document of
// components.platform.opendatahub.io (preferred version v1alpha1).
func serveComponentGroup(f *fakeAPI) {
	f.json("GET", "/apis/components.platform.opendatahub.io", 200,
		`{"kind":"APIGroup","name":"components.platform.opendatahub.io","preferredVersion":{"groupVersion":"components.platform.opendatahub.io/v1alpha1","version":"v1alpha1"}}`)
}
