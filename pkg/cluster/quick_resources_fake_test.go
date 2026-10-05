package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// resourceFake is a small stateful Kubernetes API server for the quick-resource
// tests. It stores objects by path and implements GET (object and list, with
// equality label selectors), POST, server-side apply and merge PATCH (with
// resourceVersion checks), and DELETE (with UID preconditions, finalizers and
// namespace cascade). It records every request.
type resourceFake struct {
	t       *testing.T
	mu      sync.Mutex
	objects map[fakeKey]map[string]interface{}
	nextID  int
	// fail maps "METHOD /path" to an HTTP status to return instead.
	fail map[string]int
	// finalizeOnGet removes finalizers of terminating objects on the next
	// GET, simulating a running operator.
	finalizeOnGet bool
	// namespacesLinger keeps deleted namespaces in Terminating.
	namespacesLinger bool
	// onGet may change an object before a GET returns it (for example to
	// simulate a controller updating status).
	onGet    func(k fakeKey, obj map[string]interface{})
	requests []string
}

type fakeKey struct {
	gv, ns, plural, name string
}

func newResourceFake(t *testing.T) (*resourceFake, *Client) {
	t.Helper()
	f := &resourceFake{t: t, objects: map[fakeKey]map[string]interface{}{}, fail: map[string]int{}}
	srv := httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	c := &Client{baseURL: srv.URL, token: "test-token", httpClient: srv.Client(), ctx: context.Background()}
	return f, c
}

// parseFakePath splits an API path into its key. isList is true for
// collection paths; a collection without namespace lists all namespaces.
func parseFakePath(path string) (k fakeKey, isList bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	var rest []string
	if parts[0] == "api" {
		k.gv, rest = parts[1], parts[2:]
	} else {
		k.gv, rest = parts[1]+"/"+parts[2], parts[3:]
	}
	switch {
	case len(rest) == 1:
		k.plural = rest[0]
		return k, true
	case len(rest) == 2 && rest[0] == "namespaces":
		k.plural, k.name = "namespaces", rest[1]
		return k, false
	case len(rest) == 2:
		k.plural, k.name = rest[0], rest[1]
		return k, false
	case len(rest) == 3:
		k.ns, k.plural = rest[1], rest[2]
		return k, true
	default:
		k.ns, k.plural, k.name = rest[1], rest[2], rest[3]
		return k, false
	}
}

func fakePath(k fakeKey) string {
	prefix := "/apis/" + k.gv
	if k.gv == "v1" {
		prefix = "/api/v1"
	}
	if k.ns == "" {
		return prefix + "/" + k.plural + "/" + k.name
	}
	return prefix + "/namespaces/" + k.ns + "/" + k.plural + "/" + k.name
}

// put stores an object at path, filling in defaults for metadata.
func (f *resourceFake) put(path string, obj map[string]interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, _ := parseFakePath(path)
	meta := ensureMap(obj, "metadata")
	meta["name"] = k.name
	if k.ns != "" {
		meta["namespace"] = k.ns
	}
	if meta["uid"] == nil {
		f.nextID++
		meta["uid"] = fmt.Sprintf("uid-%d", f.nextID)
	}
	if meta["resourceVersion"] == nil {
		meta["resourceVersion"] = "1"
	}
	if meta["creationTimestamp"] == nil {
		meta["creationTimestamp"] = "2026-08-27T21:30:55Z"
	}
	f.objects[k] = obj
}

// putJSON stores an object given as JSON.
func (f *resourceFake) putJSON(path, body string) {
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		f.t.Fatalf("bad fixture for %s: %v", path, err)
	}
	f.put(path, obj)
}

func (f *resourceFake) get(path string) map[string]interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, _ := parseFakePath(path)
	return f.objects[k]
}

func (f *resourceFake) has(path string) bool { return f.get(path) != nil }

func (f *resourceFake) mutations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		if !strings.HasPrefix(r, "GET ") && !strings.Contains(r, "/configmaps/") && !strings.HasSuffix(r, "/configmaps") {
			out = append(out, r)
		}
	}
	return out
}

func ensureMap(obj map[string]interface{}, key string) map[string]interface{} {
	m, ok := obj[key].(map[string]interface{})
	if !ok {
		m = map[string]interface{}{}
		obj[key] = m
	}
	return m
}

func mergePatch(dst, patch map[string]interface{}) {
	for k, v := range patch {
		if v == nil {
			delete(dst, k)
			continue
		}
		if pm, ok := v.(map[string]interface{}); ok {
			dm, ok := dst[k].(map[string]interface{})
			if !ok {
				dm = map[string]interface{}{}
				dst[k] = dm
			}
			mergePatch(dm, pm)
			continue
		}
		dst[k] = v
	}
}

func (f *resourceFake) write(w http.ResponseWriter, code int, obj interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(obj)
}

func (f *resourceFake) status(w http.ResponseWriter, code int, msg string) {
	f.write(w, code, map[string]interface{}{"kind": "Status", "status": "Failure", "message": msg, "code": code})
}

func matchesSelector(obj map[string]interface{}, selector string) bool {
	if selector == "" {
		return true
	}
	labels, _ := ensureMap(obj, "metadata")["labels"].(map[string]interface{})
	for _, term := range strings.Split(selector, ",") {
		kv := strings.SplitN(term, "=", 2)
		if len(kv) != 2 || labels[kv[0]] != kv[1] {
			return false
		}
	}
	return true
}

func (f *resourceFake) bumpRV(meta map[string]interface{}) {
	n, _ := strconv.Atoi(fmt.Sprint(meta["resourceVersion"]))
	meta["resourceVersion"] = strconv.Itoa(n + 1)
}

func (f *resourceFake) addManagedField(meta map[string]interface{}, manager, op string) {
	entries, _ := meta["managedFields"].([]interface{})
	meta["managedFields"] = append(entries, map[string]interface{}{"manager": manager, "operation": op, "time": time.Now().UTC().Format(time.RFC3339)})
}

func (f *resourceFake) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if code, ok := f.fail[r.Method+" "+r.URL.Path]; ok {
		f.status(w, code, "injected failure")
		return
	}
	k, isList := parseFakePath(r.URL.Path)

	switch r.Method {
	case http.MethodGet:
		if isList {
			items := []interface{}{}
			for key, obj := range f.objects {
				if key.gv == k.gv && key.plural == k.plural && (k.ns == "" || key.ns == k.ns) && matchesSelector(obj, r.URL.Query().Get("labelSelector")) {
					items = append(items, obj)
				}
			}
			f.write(w, 200, map[string]interface{}{"items": items})
			return
		}
		obj := f.objects[k]
		if obj == nil {
			f.status(w, 404, "not found")
			return
		}
		meta := ensureMap(obj, "metadata")
		if f.finalizeOnGet && meta["deletionTimestamp"] != nil {
			delete(f.objects, k)
			f.status(w, 404, "not found")
			return
		}
		if f.onGet != nil {
			// onGet sees a copy, so it changes only this response.
			raw, _ := json.Marshal(obj)
			var cp map[string]interface{}
			_ = json.Unmarshal(raw, &cp)
			f.onGet(k, cp)
			obj = cp
		}
		f.write(w, 200, obj)

	case http.MethodPost:
		var obj map[string]interface{}
		if err := json.Unmarshal(body, &obj); err != nil {
			f.status(w, 400, "bad body")
			return
		}
		meta := ensureMap(obj, "metadata")
		k.name, _ = meta["name"].(string)
		if f.objects[k] != nil {
			f.status(w, 409, "already exists")
			return
		}
		f.nextID++
		meta["uid"] = fmt.Sprintf("uid-%d", f.nextID)
		meta["resourceVersion"] = "1"
		now := time.Now().UTC().Format(time.RFC3339)
		meta["creationTimestamp"] = now
		manager := firstNonEmpty(r.URL.Query().Get("fieldManager"), legacyPostManager)
		meta["managedFields"] = []interface{}{map[string]interface{}{"manager": manager, "operation": "Update", "time": now}}
		f.objects[k] = obj
		f.write(w, 201, obj)

	case http.MethodPatch:
		var patch map[string]interface{}
		if err := json.Unmarshal(body, &patch); err != nil {
			f.status(w, 400, "bad body")
			return
		}
		obj := f.objects[k]
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/apply-patch") {
			if obj == nil {
				f.nextID++
				obj = map[string]interface{}{}
				mergePatch(obj, patch)
				meta := ensureMap(obj, "metadata")
				meta["uid"] = fmt.Sprintf("uid-%d", f.nextID)
				meta["resourceVersion"] = "1"
				meta["creationTimestamp"] = time.Now().UTC().Format(time.RFC3339)
				f.addManagedField(meta, r.URL.Query().Get("fieldManager"), "Apply")
				f.objects[k] = obj
				f.write(w, 201, obj)
				return
			}
			// Server-side apply replaces the manager's fields; for these
			// tests replacing spec/data and merging metadata is enough.
			for key, v := range patch {
				if key == "metadata" {
					mergePatch(ensureMap(obj, "metadata"), v.(map[string]interface{}))
				} else {
					obj[key] = v
				}
			}
			f.bumpRV(ensureMap(obj, "metadata"))
			f.write(w, 200, obj)
			return
		}
		if obj == nil {
			f.status(w, 404, "not found")
			return
		}
		meta := ensureMap(obj, "metadata")
		if pm, ok := patch["metadata"].(map[string]interface{}); ok {
			if rv, ok := pm["resourceVersion"]; ok {
				if rv != meta["resourceVersion"] {
					f.status(w, 409, "the object has been modified")
					return
				}
				delete(pm, "resourceVersion")
			}
		}
		mergePatch(obj, patch)
		f.bumpRV(meta)
		f.write(w, 200, obj)

	case http.MethodDelete:
		obj := f.objects[k]
		if obj == nil {
			f.status(w, 404, "not found")
			return
		}
		meta := ensureMap(obj, "metadata")
		if len(body) > 0 {
			var opts struct {
				Preconditions struct {
					UID string `json:"uid"`
				} `json:"preconditions"`
			}
			_ = json.Unmarshal(body, &opts)
			if opts.Preconditions.UID != "" && opts.Preconditions.UID != meta["uid"] {
				f.status(w, 409, "Precondition failed: UID in precondition does not match")
				return
			}
		}
		if fin, _ := meta["finalizers"].([]interface{}); len(fin) > 0 || (k.plural == "namespaces" && f.namespacesLinger) {
			meta["deletionTimestamp"] = time.Now().UTC().Format(time.RFC3339)
			if k.plural == "namespaces" {
				ensureMap(obj, "status")["phase"] = "Terminating"
			}
			f.write(w, 200, obj)
			return
		}
		delete(f.objects, k)
		if k.plural == "namespaces" {
			for key := range f.objects {
				if key.ns == k.name {
					delete(f.objects, key)
				}
			}
		}
		f.write(w, 200, obj)

	default:
		f.status(w, 405, "method not allowed")
	}
}

func TestFakeAPIPathParsing(t *testing.T) {
	cases := map[string]fakeKey{
		"/api/v1/namespaces/minio":                               {gv: "v1", plural: "namespaces", name: "minio"},
		"/api/v1/namespaces/minio/secrets/s":                     {gv: "v1", ns: "minio", plural: "secrets", name: "s"},
		"/apis/apps/v1/namespaces/minio/deployments/minio":       {gv: "apps/v1", ns: "minio", plural: "deployments", name: "minio"},
		"/apis/mlflow.opendatahub.io/v1/mlflows/mlflow":          {gv: "mlflow.opendatahub.io/v1", plural: "mlflows", name: "mlflow"},
		"/apis/route.openshift.io/v1/namespaces/minio/routes/ui": {gv: "route.openshift.io/v1", ns: "minio", plural: "routes", name: "ui"},
	}
	for path, want := range cases {
		got, isList := parseFakePath(path)
		if got != want || isList || fakePath(got) != path {
			t.Errorf("%s: got %+v list=%v path=%s", path, got, isList, fakePath(got))
		}
	}
}
