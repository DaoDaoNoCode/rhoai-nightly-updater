package cluster

import (
	"context"
	"encoding/base64"
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
	onGet func(k fakeKey, obj map[string]interface{})
	// groups holds API discovery documents by path ("/apis/kuadrant.io");
	// a group without one answers 404, as an unserved group.
	groups map[string]string
	// beforeServe runs before a request is handled, outside the lock, so it
	// can change the stored objects (for example to inject a foreign object
	// between the tool's check and its write).
	beforeServe func(method, path string)
	// failDryRun maps "METHOD /path" to an HTTP status to return for a
	// server-side dry run (?dryRun=All) of that request.
	failDryRun map[string]int
	// onMergePatch, when set, sees each merge patch and the object it
	// produces (under the lock), for example to record managedFields.
	onMergePatch func(r *http.Request, k fakeKey, patch, next map[string]interface{})
	requests     []string
}

type fakeKey struct {
	gv, ns, plural, name string
}

func newResourceFake(t *testing.T) (*resourceFake, *Client) {
	t.Helper()
	f := &resourceFake{t: t, objects: map[fakeKey]map[string]interface{}{}, fail: map[string]int{}, failDryRun: map[string]int{}}
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
		if !strings.HasPrefix(r, "GET ") && !strings.HasPrefix(r, "DRYRUN ") && !strings.Contains(r, "/configmaps/") && !strings.HasSuffix(r, "/configmaps") {
			out = append(out, r)
		}
	}
	return out
}

// foldStringData merges a Secret's stringData into data (base64), as the
// API server does on every write; stringData is never returned.
func foldStringData(obj map[string]interface{}) {
	sd, ok := obj["stringData"].(map[string]interface{})
	if !ok {
		return
	}
	data := ensureMap(obj, "data")
	for k, v := range sd {
		data[k] = base64.StdEncoding.EncodeToString([]byte(fmt.Sprint(v)))
	}
	delete(obj, "stringData")
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
	if f.beforeServe != nil {
		f.beforeServe(r.Method, r.URL.Path)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	dryRun := r.URL.Query().Get("dryRun") == "All"
	if dryRun {
		f.requests = append(f.requests, "DRYRUN "+r.Method+" "+r.URL.Path)
		if code, ok := f.failDryRun[r.Method+" "+r.URL.Path]; ok {
			f.status(w, code, "injected dry-run failure")
			return
		}
	} else {
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		if code, ok := f.fail[r.Method+" "+r.URL.Path]; ok {
			f.status(w, code, "injected failure")
			return
		}
	}
	// API discovery documents (/apis/<group>): the ones a test set in
	// groups, else 404, as for a group that is not served, so callers use
	// their fallback versions.
	if parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/"); parts[0] == "apis" && len(parts) < 3 {
		if doc, ok := f.groups[r.URL.Path]; ok {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(doc))
			return
		}
		f.status(w, 404, "not found")
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
		entry := map[string]interface{}{"manager": manager, "operation": "Update", "apiVersion": k.gv, "time": now, "fieldsType": "FieldsV1"}
		setOwnedPorts(entry, containerPorts(obj))
		meta["managedFields"] = []interface{}{entry}
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
			f.serveJSONPatch(w, k, obj, body, dryRun)
			return
		}
		var patch map[string]interface{}
		if err := json.Unmarshal(body, &patch); err != nil {
			f.status(w, 400, "bad body")
			return
		}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/apply-patch") {
			manager := r.URL.Query().Get("fieldManager")
			if obj == nil {
				f.nextID++
				obj = map[string]interface{}{}
				mergePatch(obj, patch)
				meta := ensureMap(obj, "metadata")
				meta["uid"] = fmt.Sprintf("uid-%d", f.nextID)
				meta["resourceVersion"] = "1"
				meta["creationTimestamp"] = time.Now().UTC().Format(time.RFC3339)
				f.addManagedField(meta, manager, "Apply")
				entries := meta["managedFields"].([]interface{})
				setOwnedPorts(entries[len(entries)-1].(map[string]interface{}), containerPorts(obj))
				foldStringData(obj)
				if !dryRun {
					f.objects[k] = obj
				}
				f.write(w, 201, obj)
				return
			}
			// A resourceVersion in the applied object is a precondition,
			// as on a real API server.
			if pm, ok := patch["metadata"].(map[string]interface{}); ok {
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
					mergePatch(ensureMap(next, key), v.(map[string]interface{}))
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
		if pm, ok := patch["metadata"].(map[string]interface{}); ok {
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
		if fin, _ := nextMeta["finalizers"].([]interface{}); nextMeta["deletionTimestamp"] != nil && len(fin) == 0 {
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
					UID string `json:"uid"`
				} `json:"preconditions"`
			}
			_ = json.Unmarshal(body, &opts)
			if opts.Preconditions.UID != "" && opts.Preconditions.UID != meta["uid"] {
				f.status(w, 409, "Precondition failed: UID in precondition does not match")
				return
			}
		}
		if dryRun {
			// The preconditions are checked; nothing is deleted.
			f.write(w, 200, map[string]interface{}{"kind": "Status", "status": "Success"})
			return
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

// serveJSONPatch implements the JSON patch (RFC 6902) "replace" operations
// on metadata.managedFields and metadata.resourceVersion, which is what a
// field-ownership upgrade sends. Replacing resourceVersion with another
// value than the stored one fails with 409, as the API server's update
// precondition does.
func (f *resourceFake) serveJSONPatch(w http.ResponseWriter, k fakeKey, obj map[string]interface{}, body []byte, dryRun bool) {
	if obj == nil {
		f.status(w, 404, "not found")
		return
	}
	var ops []struct {
		Op    string          `json:"op"`
		Path  string          `json:"path"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(body, &ops); err != nil {
		f.status(w, 400, "bad JSON patch")
		return
	}
	next := deepCopyObject(obj)
	meta := ensureMap(next, "metadata")
	for _, op := range ops {
		if op.Op != "replace" {
			f.status(w, 422, "unsupported op "+op.Op)
			return
		}
		switch op.Path {
		case "/metadata/resourceVersion":
			var rv string
			_ = json.Unmarshal(op.Value, &rv)
			if rv != meta["resourceVersion"] {
				f.status(w, 409, "the object has been modified")
				return
			}
		case "/metadata/managedFields":
			var entries []interface{}
			if err := json.Unmarshal(op.Value, &entries); err != nil {
				f.status(w, 400, "bad managedFields")
				return
			}
			meta["managedFields"] = entries
		default:
			f.status(w, 422, "unsupported path "+op.Path)
			return
		}
	}
	f.bumpRV(meta)
	if !dryRun {
		f.objects[k] = next
	}
	f.write(w, 200, next)
}

func deepCopyObject(obj map[string]interface{}) map[string]interface{} {
	raw, _ := json.Marshal(obj)
	var out map[string]interface{}
	_ = json.Unmarshal(raw, &out)
	return out
}

// podContainers returns the containers of a Deployment's pod template, or nil.
func podContainers(obj map[string]interface{}) []interface{} {
	spec, _ := obj["spec"].(map[string]interface{})
	tmpl, _ := spec["template"].(map[string]interface{})
	podSpec, _ := tmpl["spec"].(map[string]interface{})
	containers, _ := podSpec["containers"].([]interface{})
	return containers
}

// containerPorts returns each container's ports by container name.
func containerPorts(obj map[string]interface{}) map[string][]interface{} {
	out := map[string][]interface{}{}
	for _, c := range podContainers(obj) {
		cm, _ := c.(map[string]interface{})
		name, _ := cm["name"].(string)
		ports, _ := cm["ports"].([]interface{})
		if name != "" && len(ports) > 0 {
			out[name] = ports
		}
	}
	return out
}

// portKey is the fieldsV1 key of a container port: ports are a list map
// keyed by containerPort and protocol (k8s.io/api core/v1 Container.Ports,
// +listMapKey=containerPort, +listMapKey=protocol).
func portKey(p interface{}) string {
	pm, _ := p.(map[string]interface{})
	protocol, _ := pm["protocol"].(string)
	if protocol == "" {
		protocol = "TCP"
	}
	return fmt.Sprintf(`k:{"containerPort":%v,"protocol":%q}`, pm["containerPort"], protocol)
}

func containerKey(name string) string { return fmt.Sprintf(`k:{"name":%q}`, name) }

// portsNode returns the fieldsV1 node of a container's ports in a managedFields
// entry (created when create is true).
func portsNode(entry map[string]interface{}, container string, create bool) map[string]interface{} {
	node := entry
	for _, key := range []string{"fieldsV1", "f:spec", "f:template", "f:spec", "f:containers", containerKey(container), "f:ports"} {
		next, ok := node[key].(map[string]interface{})
		if !ok {
			if !create {
				return nil
			}
			next = map[string]interface{}{}
			node[key] = next
		}
		node = next
	}
	return node
}

// setOwnedPorts makes entry own exactly these ports of each container.
func setOwnedPorts(entry map[string]interface{}, ports map[string][]interface{}) {
	for container, list := range ports {
		node := portsNode(entry, container, true)
		for k := range node {
			delete(node, k)
		}
		node["."] = map[string]interface{}{}
		for _, p := range list {
			node[portKey(p)] = map[string]interface{}{".": map[string]interface{}{}, "f:containerPort": map[string]interface{}{}, "f:name": map[string]interface{}{}, "f:protocol": map[string]interface{}{}}
		}
	}
}

func ownsPort(entry map[string]interface{}, container, key string) bool {
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
func applySSAPorts(old, next map[string]interface{}, manager string) {
	meta := ensureMap(next, "metadata")
	entries, _ := meta["managedFields"].([]interface{})
	var applyEntry map[string]interface{}
	for _, e := range entries {
		em, _ := e.(map[string]interface{})
		if em["manager"] == manager && em["operation"] == "Apply" && em["subresource"] == nil {
			applyEntry = em
		}
	}
	if applyEntry == nil {
		applyEntry = map[string]interface{}{"manager": manager, "operation": "Apply", "time": time.Now().UTC().Format(time.RFC3339), "fieldsType": "FieldsV1"}
		entries = append(entries, applyEntry)
		meta["managedFields"] = entries
	}
	applied := containerPorts(next)
	stored := containerPorts(old)
	for _, c := range podContainers(next) {
		cm, _ := c.(map[string]interface{})
		name, _ := cm["name"].(string)
		appliedByKey := map[string]interface{}{}
		for _, p := range applied[name] {
			appliedByKey[portKey(p)] = p
		}
		var result []interface{}
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
				em, _ := e.(map[string]interface{})
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
func duplicatePortName(k fakeKey, obj map[string]interface{}) string {
	for ci, c := range podContainers(obj) {
		cm, _ := c.(map[string]interface{})
		ports, _ := cm["ports"].([]interface{})
		names := map[string]bool{}
		for pi, p := range ports {
			pm, _ := p.(map[string]interface{})
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
