package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
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
	isolateClusterCaches(t)
	f := &fakeAPI{routes: map[string]func(*http.Request, []byte) (int, string){}}
	srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	// A new server may reuse an earlier test's address, which keys the cache.
	controlPlaneCache.Purge()
	return f, &Client{baseURL: srv.URL, token: "test", httpClient: srv.Client(), ctx: context.Background()}
}

// json registers a fixed response.
func (f *fakeAPI) json(method, path string, status int, body string) {
	f.handle(method, path, func(*http.Request, []byte) (int, string) { return status, body })
}

func (f *fakeAPI) obj(path string, v any) {
	method := "GET"
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
// minimalDSCCRD is a DataScienceCluster CRD whose only (stored) version
// defines these top-level components with managementState Managed or Removed.
func minimalDSCCRD(version string, components ...string) string {
	props := map[string]any{}
	for _, c := range components {
		props[c] = map[string]any{"type": "object", "properties": map[string]any{
			"managementState": map[string]any{"type": "string", "enum": []any{"Managed", "Removed"}}}}
	}
	crd := map[string]any{"spec": map[string]any{"versions": []any{map[string]any{
		"name": version, "served": true, "storage": true, "schema": map[string]any{"openAPIV3Schema": map[string]any{
			"type": "object", "properties": map[string]any{"spec": map[string]any{"type": "object", "properties": map[string]any{
				"components": map[string]any{"type": "object", "properties": props}}}}}}}}}}
	raw, _ := json.Marshal(crd)
	return string(raw)
}

// serveToggleDSC serves dsc as the cluster's only DataScienceCluster at
// version (in its list, as an API server lists objects, and by name), a CRD
// that defines components, and JSON patches of it: a dry run changes
// nothing, a write is kept. patchBody, when set, is answered instead (an
// API server that dropped the change).
func serveToggleDSC(f *fakeAPI, version, dsc, patchBody string, components ...string) {
	list := "/apis/datasciencecluster.opendatahub.io/" + version + "/datascienceclusters"
	var obj map[string]any
	if err := json.Unmarshal([]byte(dsc), &obj); err != nil {
		panic(err)
	}
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	// The API server sets a uid and a resourceVersion on every object, and
	// moves the resourceVersion on every write.
	if meta["uid"] == nil {
		meta["uid"] = "uid-" + name
	}
	if meta["resourceVersion"] == nil {
		meta["resourceVersion"] = "1"
	}
	var mu sync.Mutex
	current := func() string {
		raw, _ := json.Marshal(obj)
		return string(raw)
	}
	f.handle("GET", list, func(*http.Request, []byte) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		return 200, `{"items":[` + current() + `]}`
	})
	f.handle("GET", list+"/"+name, func(*http.Request, []byte) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		return 200, current()
	})
	f.json("GET", dscCRDPath, 200, minimalDSCCRD(version, components...))
	serveInstalledOperator(f)
	f.handle("PATCH", list+"/"+name, func(r *http.Request, body []byte) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		if patchBody != "" {
			return 200, patchBody
		}
		var ops []jsonPatchOp
		if err := json.Unmarshal(body, &ops); err != nil {
			return 400, `{"kind":"Status","status":"Failure","code":400}`
		}
		next, err := applyJSONPatch(obj, ops)
		if err != nil {
			return 422, `{"kind":"Status","status":"Failure","code":422,"message":"` + err.Error() + `"}`
		}
		if r.URL.Query().Get("dryRun") != "All" {
			m, _ := next["metadata"].(map[string]any)
			n, _ := strconv.Atoi(fmt.Sprint(m["resourceVersion"]))
			m["resourceVersion"] = strconv.Itoa(n + 1)
			obj = next
		}
		raw, _ := json.Marshal(next)
		return 200, string(raw)
	})
}

// realWrites are the writes that are not server-side dry runs.
func (f *fakeAPI) realWrites() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.reqs {
		if r.Method == http.MethodGet || strings.Contains(r.Path, "/configmaps") || strings.Contains(r.Query, "dryRun=All") {
			continue
		}
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

// serveInstalledOperator serves the Subscription and the installed CSV,
// which names the operator's Deployment (rhods-operator).
func serveInstalledOperator(f *fakeAPI) {
	f.json("GET", namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName), 200, `{"status":{"installedCSV":"rhods-operator.3.6.0"}}`)
	f.json("GET", namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "rhods-operator.3.6.0"), 200,
		`{"metadata":{"name":"rhods-operator.3.6.0"},"spec":{"version":"3.6.0","install":{"spec":{"deployments":[{"name":"rhods-operator"}]}}},"status":{"phase":"Succeeded"}}`)
}

func serveComponentGroup(f *fakeAPI) {
	f.json("GET", "/apis/components.platform.opendatahub.io", 200,
		`{"kind":"APIGroup","name":"components.platform.opendatahub.io","preferredVersion":{"groupVersion":"components.platform.opendatahub.io/v1alpha1","version":"v1alpha1"}}`)
}
