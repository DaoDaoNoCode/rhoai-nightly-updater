package cluster

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// resourceFake is a small stateful Kubernetes API server for the quick-resource
// tests. It stores objects by path and implements GET (object and list, with
// equality label selectors), POST, server-side apply and merge PATCH (with
// resourceVersion checks), and DELETE (with UID preconditions, finalizers and
// namespace cascade). It records every request.
type resourceFake struct {
	t       *testing.T
	mu      sync.Mutex
	objects map[fakeKey]map[string]any
	nextID  int
	// fail maps "METHOD /path" to an HTTP status to return instead.
	fail map[string]int
	// failAs maps "<token> METHOD /path" to an HTTP status returned to that
	// token only (the ServiceAccount's is fakeSAToken, the signed-in user's
	// fakeUserToken), as RBAC that grants one identity and not the other.
	failAs map[string]int
	// finalizeOnGet removes finalizers of terminating objects on the next
	// GET, simulating a running operator.
	finalizeOnGet bool
	// namespacesLinger keeps deleted namespaces in Terminating.
	namespacesLinger bool
	// onGet may change an object before a GET returns it (for example to
	// simulate a controller updating status).
	onGet func(k fakeKey, obj map[string]any)
	// listOnGet: the items of a list of these resources (by plural) also
	// go through onGet (a controller's status shows in lists too).
	listOnGet map[string]bool
	// groups holds API discovery documents by path ("/apis/kuadrant.io");
	// a group without one answers 404, as an unserved group.
	groups map[string]string
	// beforeServe runs before a request is handled, outside the lock, so it
	// can change the stored objects (for example to inject a foreign object
	// between the tool's check and its write).
	beforeServe func(method, path string)
	// beforeRequest is beforeServe with the whole request (its query tells
	// a server-side dry run from the write; its body can be read again).
	beforeRequest func(r *http.Request)
	// failDryRun maps "METHOD /path" to an HTTP status to return for a
	// server-side dry run (?dryRun=All) of that request.
	failDryRun map[string]int
	// failEcho maps "METHOD /path" (a write) or "DRYRUN METHOD /path" (its
	// server-side dry run) to an HTTP status returned with a message that
	// quotes the request's URL and body, as an API server's validation
	// error can quote the object it refuses. Tests use it to check that no
	// secret sent in a request comes back in a message.
	failEcho map[string]int
	// echoed are the request bodies failEcho quoted.
	echoed []string
	// onMergePatch, when set, sees each merge patch and the object it
	// produces (under the lock), for example to record managedFields.
	onMergePatch func(r *http.Request, k fakeKey, patch, next map[string]any)
	requests     []string
	// failMessage is the message returned with fail or failDryRun for that
	// key ("METHOD /path" or "DRYRUN METHOD /path"), as an admission
	// webhook's or validation rule's own words.
	failMessage map[string]string
	// calls records every request with its query, content type and body.
	calls []fakeCall
	// argo answers Argo CD's (and ACM's) reads of argocd.go; nil = the
	// generic handling.
	argo *fakeArgoCD
	// intercept, when set, may answer a request itself (under the lock,
	// after it was recorded and its injected failures applied): it returns
	// true when it did.
	intercept func(w http.ResponseWriter, r *http.Request, body []byte) bool
	// sw is the SeaweedFS the tool's own S3 and IAM requests reach.
	sw *seaweedFake
	// projectLists counts the LISTs of projects projectFake answered; the
	// first projectListsHidden of them leave out every project (the
	// openshift-apiserver's project cache not synchronized yet).
	projectLists, projectListsHidden int
	// serverClockOffset, when not zero, is how far the API server's clock
	// (its responses' Date header) is from the wall clock.
	serverClockOffset time.Duration
	// noDate: responses carry no Date header (net/http leaves it out when
	// the handler sets it to nil).
	noDate bool
}

// fakeCall is one request to resourceFake.
type fakeCall struct {
	Method, Path, ContentType, Body string
	Query                           url.Values
	// Token is the request's bearer token: who made it.
	Token string
	// Accept is the request's Accept header.
	Accept string
}

// dryRun reports whether the API server runs the call as a server-side dry
// run (kube.IsDryRun: a DELETE with a body takes it from the body).
func (c fakeCall) dryRun() bool {
	return kube.IsDryRun(c.Method, &url.URL{RawQuery: c.Query.Encode()}, []byte(c.Body))
}

// callsTo returns the calls of method to path, in order.
func (f *resourceFake) callsTo(method, path string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.Method == method && c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

type fakeKey struct {
	gv, ns, plural, name string
}

// The tokens of newResourceFake's client (the ServiceAccount's) and of the
// signed-in user's client it carries.
const (
	fakeSAToken   = "test-token"
	fakeUserToken = "signed-in-user-token"
)

func newResourceFake(t *testing.T) (*resourceFake, *Client) {
	t.Helper()
	isolateClusterCaches(t)
	f := &resourceFake{t: t, objects: map[fakeKey]map[string]any{}, fail: map[string]int{}, failAs: map[string]int{}, failDryRun: map[string]int{}, failEcho: map[string]int{}, failMessage: map[string]string{}}
	srv := newFakeServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	c := newTestClient(testClientOptions{BaseURL: srv.URL, Token: fakeSAToken, HTTPClient: srv.Client(), Ctx: context.Background()})
	// The signed-in user's own client, as pkg/api withAdminRead attaches it
	// (the hard reset's namespace guard reads with it).
	c.SetSignedInUser(newTestClient(testClientOptions{BaseURL: srv.URL, Token: fakeUserToken, HTTPClient: srv.Client(), Ctx: context.Background(), AsUser: true}))
	// An installed RHOAI: its DSCInitialization names the applications
	// namespace these fixtures put RHOAI's objects in (delete it to test a
	// cluster without one).
	f.putJSON(dsciV2List+"/default-dsci", `{"metadata":{"name":"default-dsci"},"spec":{"applicationsNamespace":"redhat-ods-applications"}}`)
	f.sw = newSeaweedFake(t, f)
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
	case len(rest) == 0:
		// /apis/<group>/<version> without a document in groups (serve
		// answers 404).
		return k, true
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
func (f *resourceFake) put(path string, obj map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putLocked(path, obj)
}

// putLocked is put with f.mu held (from an intercept).
func (f *resourceFake) putLocked(path string, obj map[string]any) {
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
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		f.t.Fatalf("bad fixture for %s: %v", path, err)
	}
	f.put(path, obj)
}

func (f *resourceFake) get(path string) map[string]any {
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
		if !strings.HasPrefix(r, "GET ") && !strings.HasPrefix(r, "DRYRUN ") && !strings.Contains(r, "/configmaps/") && !strings.HasSuffix(r, "/configmaps") {
			out = append(out, r)
		}
	}
	return out
}

// foldStringData merges a Secret's stringData into data (base64), as the
// API server does on every write; stringData is never returned.
func foldStringData(obj map[string]any) {
	sd, ok := obj["stringData"].(map[string]any)
	if !ok {
		return
	}
	data := ensureMap(obj, "data")
	for k, v := range sd {
		data[k] = base64.StdEncoding.EncodeToString([]byte(fmt.Sprint(v)))
	}
	delete(obj, "stringData")
}

func ensureMap(obj map[string]any, key string) map[string]any {
	m, ok := obj[key].(map[string]any)
	if !ok {
		m = map[string]any{}
		obj[key] = m
	}
	return m
}

func mergePatch(dst, patch map[string]any) {
	for k, v := range patch {
		if v == nil {
			delete(dst, k)
			continue
		}
		if pm, ok := v.(map[string]any); ok {
			dm, ok := dst[k].(map[string]any)
			if !ok {
				dm = map[string]any{}
				dst[k] = dm
			}
			mergePatch(dm, pm)
			continue
		}
		dst[k] = v
	}
}

func (f *resourceFake) write(w http.ResponseWriter, code int, obj any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(obj)
}

func (f *resourceFake) status(w http.ResponseWriter, code int, msg string) {
	f.write(w, code, map[string]any{"kind": "Status", "status": "Failure", "message": msg, "code": code})
}

func matchesSelector(obj map[string]any, selector string) bool {
	if selector == "" {
		return true
	}
	labels, _ := ensureMap(obj, "metadata")["labels"].(map[string]any)
	for term := range strings.SplitSeq(selector, ",") {
		// "!key": the label is absent (as listClusterCSVs excludes OLM's
		// copies with !olm.copiedFrom).
		if key, ok := strings.CutPrefix(term, "!"); ok {
			if _, has := labels[key]; has {
				return false
			}
			continue
		}
		kv := strings.SplitN(term, "=", 2)
		if len(kv) == 1 {
			// "key": the label is present, with any value.
			if _, has := labels[term]; !has {
				return false
			}
			continue
		}
		if labels[kv[0]] != kv[1] {
			return false
		}
	}
	return true
}

func (f *resourceFake) bumpRV(meta map[string]any) {
	n, _ := strconv.Atoi(fmt.Sprint(meta["resourceVersion"]))
	meta["resourceVersion"] = strconv.Itoa(n + 1)
}

func (f *resourceFake) addManagedField(meta map[string]any, manager, op string) {
	entries, _ := meta["managedFields"].([]any)
	meta["managedFields"] = append(entries, map[string]any{"manager": manager, "operation": op, "time": time.Now().UTC().Format(time.RFC3339)})
}

func (f *resourceFake) serve(w http.ResponseWriter, r *http.Request) {
	if f.noDate {
		w.Header()["Date"] = nil
	} else if f.serverClockOffset != 0 {
		// net/http adds a Date header only when the handler sets none.
		w.Header().Set("Date", time.Now().Add(f.serverClockOffset).UTC().Format(http.TimeFormat))
	}
	body, _ := io.ReadAll(r.Body)
	if f.beforeServe != nil {
		f.beforeServe(r.Method, r.URL.Path)
	}
	if f.beforeRequest != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.beforeRequest(r)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// As the API server reads it: a DELETE with a body from its body.
	dryRun := kube.IsDryRun(r.Method, r.URL, body)
	echoKey := r.Method + " " + r.URL.Path
	if dryRun {
		echoKey = "DRYRUN " + echoKey
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.calls = append(f.calls, fakeCall{Method: r.Method, Path: r.URL.Path, ContentType: r.Header.Get("Content-Type"), Body: string(body), Query: r.URL.Query(), Token: token, Accept: r.Header.Get("Accept")})
	if code, ok := f.failAs[token+" "+r.Method+" "+r.URL.Path]; ok {
		f.status(w, code, kube.FirstNonEmpty(f.failMessage[token+" "+r.Method+" "+r.URL.Path], "injected failure for this token"))
		return
	}
	if dryRun {
		f.requests = append(f.requests, "DRYRUN "+r.Method+" "+r.URL.Path)
		if code, ok := f.failDryRun[r.Method+" "+r.URL.Path]; ok {
			f.status(w, code, kube.FirstNonEmpty(f.failMessage[echoKey], "injected dry-run failure"))
			return
		}
	} else {
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		if code, ok := f.fail[r.Method+" "+r.URL.Path]; ok {
			f.status(w, code, kube.FirstNonEmpty(f.failMessage[echoKey], "injected failure"))
			return
		}
	}
	if f.argo != nil && f.argo.serve(w, r) {
		return
	}
	if f.intercept != nil && f.intercept(w, r, body) {
		return
	}
	// A JSON patch (the field-ownership upgrade) carries no object to quote.
	if code, ok := f.failEcho[echoKey]; ok && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json-patch+json") {
		f.echoed = append(f.echoed, string(body))
		f.status(w, code, fmt.Sprintf("%s %s is invalid: the request was %s", r.Method, r.URL.String(), body))
		return
	}
	// API discovery documents (/apis/<group> and /apis/<group>/<version>):
	// the ones a test set in groups, else 404, as for a group or version
	// that is not served, so callers use their fallback versions (and Argo
	// CD's Application API reads as not installed: argocd.go).
	if doc, ok := f.groups[r.URL.Path]; ok && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(doc))
		return
	}
	if parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/"); parts[0] == "apis" && len(parts) <= 3 {
		if doc, ok := f.groups[r.URL.Path]; ok {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(doc))
			return
		}
		f.status(w, 404, "not found")
		return
	}
	k, isList := parseFakePath(r.URL.Path)
	if k.plural == "" {
		// A version's resource list no test serves: an error that is not
		// "not served" (the tests of unserved groups use the group path).
		f.status(w, 500, "no resource list served for this version")
		return
	}

	// A metadata-only read in the wrong form is refused as the API server
	// refuses it (k8s.io/apiserver pkg/endpoints/handlers/response.go
	// asPartialObjectMetadata/asPartialObjectMetadataList: 406; a custom
	// resource asked for as a list answered 500 "content is not a list",
	// live 2026-11-25).
	if accept := r.Header.Get("Accept"); r.Method == http.MethodGet {
		switch {
		case strings.Contains(accept, "as=PartialObjectMetadataList") && !isList:
			f.status(w, http.StatusInternalServerError, "content is not a list")
			return
		case strings.Contains(accept, "as=PartialObjectMetadata;") && isList:
			f.status(w, http.StatusNotAcceptable, "you requested PartialObjectMetadata, but the requested object is a list")
			return
		}
	}
	switch r.Method {
	case http.MethodGet:
		if isList {
			items := []any{}
			for key, obj := range f.objects {
				name, byName := strings.CutPrefix(r.URL.Query().Get("fieldSelector"), "metadata.name=")
				if key.gv == k.gv && key.plural == k.plural && (k.ns == "" || key.ns == k.ns) && matchesSelector(obj, r.URL.Query().Get("labelSelector")) && (!byName || key.name == name) {
					if f.onGet != nil && f.listOnGet[k.plural] {
						raw, _ := json.Marshal(obj)
						var cp map[string]any
						_ = json.Unmarshal(raw, &cp)
						f.onGet(key, cp)
						obj = cp
					}
					items = append(items, obj)
				}
			}
			f.write(w, 200, map[string]any{"items": items})
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
			var cp map[string]any
			_ = json.Unmarshal(raw, &cp)
			f.onGet(k, cp)
			obj = cp
		}
		f.write(w, 200, obj)

	case http.MethodPost:
		var obj map[string]any
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
		// As the API server names a write's manager: its fieldManager, else
		// its User-Agent up to the first "/" (Go's default without one).
		manager := kube.FirstNonEmpty(r.URL.Query().Get("fieldManager"), strings.SplitN(r.UserAgent(), "/", 2)[0], envkit.LegacyPostManager)
		entry := map[string]any{"manager": manager, "operation": "Update", "apiVersion": k.gv, "time": now, "fieldsType": "FieldsV1"}
		setOwnedPorts(entry, containerPorts(obj))
		meta["managedFields"] = []any{entry}
		if msg := duplicatePortName(k, obj); msg != "" {
			f.status(w, 422, msg)
			return
		}
		foldStringData(obj)
		if !dryRun {
			f.objects[k] = obj
		}
		f.write(w, 201, obj)

	case http.MethodPatch:
		obj := f.objects[k]
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json-patch+json") {
			f.serveJSONPatch(w, r, k, obj, body, dryRun)
			return
		}
		var patch map[string]any
		if err := json.Unmarshal(body, &patch); err != nil {
			f.status(w, 400, "bad body")
			return
		}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/apply-patch") {
			manager := r.URL.Query().Get("fieldManager")
			if obj == nil {
				f.nextID++
				obj = map[string]any{}
				mergePatch(obj, patch)
				meta := ensureMap(obj, "metadata")
				meta["uid"] = fmt.Sprintf("uid-%d", f.nextID)
				meta["resourceVersion"] = "1"
				meta["creationTimestamp"] = time.Now().UTC().Format(time.RFC3339)
				f.addManagedField(meta, manager, "Apply")
				entries := meta["managedFields"].([]any)
				setOwnedPorts(entries[len(entries)-1].(map[string]any), containerPorts(obj))
				foldStringData(obj)
				if !dryRun {
					f.objects[k] = obj
				}
				f.write(w, 201, obj)
				return
			}
			// A resourceVersion in the applied object is a precondition,
			// as on a real API server.
			if pm, ok := patch["metadata"].(map[string]any); ok {
				if rv, ok := pm["resourceVersion"]; ok {
					if rv != ensureMap(obj, "metadata")["resourceVersion"] {
						f.status(w, 409, "the object has been modified")
						return
					}
					delete(pm, "resourceVersion")
				}
			}
			// Server-side apply replaces the manager's fields; for these
			// tests replacing spec/data and merging metadata is enough,
			// except for the containers' ports: a list map keyed by
			// containerPort and protocol, merged by key with per-manager
			// ownership (applySSAPorts).
			next := deepCopyObject(obj)
			for key, v := range patch {
				// A ConfigMap's data is a map whose keys are owned one by
				// one: an apply keeps the keys other managers applied (the
				// tool's records share the snapshot ConfigMap).
				if key == "metadata" || (k.plural == "configmaps" && key == "data") {
					mergePatch(ensureMap(next, key), v.(map[string]any))
				} else {
					next[key] = v
				}
			}
			applySSAPorts(obj, next, manager)
			if _, ok := next["stringData"]; ok {
				delete(next, "data")
				foldStringData(next)
			}
			if msg := duplicatePortName(k, next); msg != "" {
				f.status(w, 422, msg)
				return
			}
			f.bumpRV(ensureMap(next, "metadata"))
			if !dryRun {
				f.objects[k] = next
			}
			f.write(w, 200, next)
			return
		}
		if obj == nil {
			f.status(w, 404, "not found")
			return
		}
		meta := ensureMap(obj, "metadata")
		if pm, ok := patch["metadata"].(map[string]any); ok {
			if rv, ok := pm["resourceVersion"]; ok {
				if rv != meta["resourceVersion"] {
					f.status(w, 409, "the object has been modified")
					return
				}
				delete(pm, "resourceVersion")
			}
		}
		// A merge patch: a dry run persists nothing, and a change to spec
		// bumps metadata.generation, as on a real API server.
		next := deepCopyObject(obj)
		mergePatch(next, patch)
		nextMeta := ensureMap(next, "metadata")
		f.bumpRV(nextMeta)
		if _, ok := patch["spec"]; ok {
			n, _ := strconv.Atoi(fmt.Sprint(nextMeta["generation"]))
			nextMeta["generation"] = n + 1
		}
		if f.onMergePatch != nil {
			f.onMergePatch(r, k, patch, next)
		}
		// A terminating object whose last finalizer is removed is deleted.
		if fin, _ := nextMeta["finalizers"].([]any); nextMeta["deletionTimestamp"] != nil && len(fin) == 0 {
			if !dryRun {
				delete(f.objects, k)
			}
			f.write(w, 200, next)
			return
		}
		if !dryRun {
			f.objects[k] = next
		}
		f.write(w, 200, next)

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
					UID             string `json:"uid"`
					ResourceVersion string `json:"resourceVersion"`
				} `json:"preconditions"`
			}
			_ = json.Unmarshal(body, &opts)
			if opts.Preconditions.UID != "" && opts.Preconditions.UID != meta["uid"] {
				f.status(w, 409, "Precondition failed: UID in precondition does not match")
				return
			}
			if opts.Preconditions.ResourceVersion != "" && opts.Preconditions.ResourceVersion != fmt.Sprint(meta["resourceVersion"]) {
				f.status(w, 409, "Precondition failed: ResourceVersion in precondition does not match")
				return
			}
		}
		if dryRun {
			// The preconditions are checked; nothing is deleted.
			f.write(w, 200, map[string]any{"kind": "Status", "status": "Success"})
			return
		}
		if fin, _ := meta["finalizers"].([]any); len(fin) > 0 || (k.plural == "namespaces" && f.namespacesLinger) {
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

// apiServerPatchTestFailed is the API server's whole answer to a JSON Patch
// whose "test" operation failed (live on OpenShift's kube-apiserver,
// 2026-11-20): k8s.io/apiserver pkg/endpoints/handlers/patch.go
// applyJSPatch turns the json-patch error into
// errors.NewGenericServerResponse(422, "", schema.GroupResource{}, "",
// err.Error(), 0, false), and with isUnexpectedResponse false that keeps
// only the generic message of reason Invalid: no cause, no path, no
// "testing value". It is the answer of any failed test, whatever its path.
const apiServerPatchTestFailed = `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"the server rejected our request due to an error in our request","reason":"Invalid","details":{},"code":422}`

// serveJSONPatch implements a JSON patch (RFC 6902: test, add, replace,
// remove) as the API server does: the whole patch or nothing, a failed
// "test" answering 422 with the API server's body
// (apiServerPatchTestFailed). Replacing
// metadata.resourceVersion with another value than the stored one fails
// with 409, as the API server's update precondition does (a field-ownership
// upgrade sends that). A change to spec bumps metadata.generation, and the
// fieldManager gets an Update entry in managedFields.
func (f *resourceFake) serveJSONPatch(w http.ResponseWriter, r *http.Request, k fakeKey, obj map[string]any, body []byte, dryRun bool) {
	if obj == nil {
		f.status(w, 404, "not found")
		return
	}
	var ops []kube.JSONPatchOp
	if err := json.Unmarshal(body, &ops); err != nil {
		f.status(w, 400, "bad JSON patch")
		return
	}
	meta := ensureMap(obj, "metadata")
	spec := false
	for _, op := range ops {
		if op.Path == "/metadata/resourceVersion" && op.Op == "replace" {
			var rv string
			_ = json.Unmarshal(op.Value, &rv)
			if rv != meta["resourceVersion"] {
				f.status(w, 409, "the object has been modified")
				return
			}
		}
		spec = spec || strings.HasPrefix(op.Path, "/spec") && op.Op != "test"
	}
	next, err := kube.ApplyJSONPatch(obj, ops)
	if err != nil {
		if _, ok := errors.AsType[*kube.JSONPatchTestError](err); ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, apiServerPatchTestFailed)
			return
		}
		f.status(w, 422, err.Error())
		return
	}
	nextMeta := ensureMap(next, "metadata")
	f.bumpRV(nextMeta)
	if spec {
		n, _ := strconv.Atoi(fmt.Sprint(nextMeta["generation"]))
		nextMeta["generation"] = n + 1
		if manager := r.URL.Query().Get("fieldManager"); manager != "" {
			f.addManagedField(nextMeta, manager, "Update")
		}
	}
	if !dryRun {
		f.objects[k] = next
	}
	f.write(w, 200, next)
}

func deepCopyObject(obj map[string]any) map[string]any {
	raw, _ := json.Marshal(obj)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

// podContainers returns the containers of a Deployment's pod template, or nil.
func podContainers(obj map[string]any) []any {
	spec, _ := obj["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tmpl["spec"].(map[string]any)
	containers, _ := podSpec["containers"].([]any)
	return containers
}

// containerPorts returns each container's ports by container name.
func containerPorts(obj map[string]any) map[string][]any {
	out := map[string][]any{}
	for _, c := range podContainers(obj) {
		cm, _ := c.(map[string]any)
		name, _ := cm["name"].(string)
		ports, _ := cm["ports"].([]any)
		if name != "" && len(ports) > 0 {
			out[name] = ports
		}
	}
	return out
}

// portKey is the fieldsV1 key of a container port: ports are a list map
// keyed by containerPort and protocol (k8s.io/api core/v1 Container.Ports,
// +listMapKey=containerPort, +listMapKey=protocol).
func portKey(p any) string {
	pm, _ := p.(map[string]any)
	protocol, _ := pm["protocol"].(string)
	if protocol == "" {
		protocol = "TCP"
	}
	return fmt.Sprintf(`k:{"containerPort":%v,"protocol":%q}`, pm["containerPort"], protocol)
}

func containerKey(name string) string { return fmt.Sprintf(`k:{"name":%q}`, name) }

// portsNode returns the fieldsV1 node of a container's ports in a managedFields
// entry (created when create is true).
func portsNode(entry map[string]any, container string, create bool) map[string]any {
	node := entry
	for _, key := range []string{"fieldsV1", "f:spec", "f:template", "f:spec", "f:containers", containerKey(container), "f:ports"} {
		next, ok := node[key].(map[string]any)
		if !ok {
			if !create {
				return nil
			}
			next = map[string]any{}
			node[key] = next
		}
		node = next
	}
	return node
}

// setOwnedPorts makes entry own exactly these ports of each container.
func setOwnedPorts(entry map[string]any, ports map[string][]any) {
	for container, list := range ports {
		node := portsNode(entry, container, true)
		for k := range node {
			delete(node, k)
		}
		node["."] = map[string]any{}
		for _, p := range list {
			node[portKey(p)] = map[string]any{".": map[string]any{}, "f:containerPort": map[string]any{}, "f:name": map[string]any{}, "f:protocol": map[string]any{}}
		}
	}
}

func ownsPort(entry map[string]any, container, key string) bool {
	node := portsNode(entry, container, false)
	_, ok := node[key]
	return ok
}

// applySSAPorts merges the containers' ports of a server-side apply by
// manager the way the API server does for a list map: an applied port
// replaces the stored one with its key; a stored port the manager's Apply
// entry owned but no longer applies is removed, unless another entry (another
// manager, or the same manager's Update entry) also owns it; any other stored
// port stays. The manager's Apply entry then owns exactly the applied ports.
// The entry is added when the manager had none.
func applySSAPorts(old, next map[string]any, manager string) {
	meta := ensureMap(next, "metadata")
	entries, _ := meta["managedFields"].([]any)
	var applyEntry map[string]any
	for _, e := range entries {
		em, _ := e.(map[string]any)
		if em["manager"] == manager && em["operation"] == "Apply" && em["subresource"] == nil {
			applyEntry = em
		}
	}
	if applyEntry == nil {
		applyEntry = map[string]any{"manager": manager, "operation": "Apply", "time": time.Now().UTC().Format(time.RFC3339), "fieldsType": "FieldsV1"}
		entries = append(entries, applyEntry)
		meta["managedFields"] = entries
	}
	applied := containerPorts(next)
	stored := containerPorts(old)
	for _, c := range podContainers(next) {
		cm, _ := c.(map[string]any)
		name, _ := cm["name"].(string)
		appliedByKey := map[string]any{}
		for _, p := range applied[name] {
			appliedByKey[portKey(p)] = p
		}
		var result []any
		seen := map[string]bool{}
		for _, p := range stored[name] {
			key := portKey(p)
			seen[key] = true
			if ap, ok := appliedByKey[key]; ok {
				result = append(result, ap)
				continue
			}
			ownedByOthers := false
			for _, e := range entries {
				em, _ := e.(map[string]any)
				if em["subresource"] == nil && (em["manager"] != manager || em["operation"] != "Apply") && ownsPort(em, name, key) {
					ownedByOthers = true
				}
			}
			if ownsPort(applyEntry, name, key) && !ownedByOthers {
				continue // the manager stopped applying it: removed
			}
			result = append(result, p)
		}
		for _, p := range applied[name] {
			if !seen[portKey(p)] {
				result = append(result, p)
			}
		}
		if len(result) > 0 {
			cm["ports"] = result
		}
	}
	setOwnedPorts(applyEntry, applied)
}

// duplicatePortName returns the API server's validation error for two ports
// of a Deployment container with the same name, or "".
func duplicatePortName(k fakeKey, obj map[string]any) string {
	for ci, c := range podContainers(obj) {
		cm, _ := c.(map[string]any)
		ports, _ := cm["ports"].([]any)
		names := map[string]bool{}
		for pi, p := range ports {
			pm, _ := p.(map[string]any)
			name, _ := pm["name"].(string)
			if name != "" && names[name] {
				return fmt.Sprintf(`Deployment.apps %q is invalid: spec.template.spec.containers[%d].ports[%d].name: Duplicate value: %q`, k.name, ci, pi, name)
			}
			names[name] = true
		}
	}
	return ""
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
