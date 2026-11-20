package cluster

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
)

// fakeArgoCD answers what argocd.go reads: Argo CD's discovery document and
// Application lists (all, or by fieldSelector metadata.name, in a namespace
// or all), ACM's work API discovery, the argocd-manager ClusterRoleBinding
// list and RoleBinding argocd-manager-role-binding by namespace. The zero
// value is a cluster without Argo CD, without ACM and without
// argocd-manager. Plug it into a fake with serve (it answers only these
// paths) or register.
type fakeArgoCD struct {
	mu sync.Mutex
	// served: Argo CD's Application API is served.
	served bool
	// rolloutsOnly: argoproj.io/v1alpha1 is served, by Argo Rollouts only.
	rolloutsOnly bool
	// apps are the Applications on the cluster.
	apps []map[string]any
	// acm: the cluster serves ACM's AppliedManifestWork API.
	acm bool
	// argocdManager: ClusterRoleBinding argocd-manager-role-binding exists.
	argocdManager bool
	// managerRoleBindings: the namespaces with RoleBinding
	// argocd-manager-role-binding (`argocd cluster add --namespace`).
	managerRoleBindings []string
	// fail answers a path (without query) with this status code.
	fail map[string]int
	// requests: the paths (with query) asked, in order.
	requests []string
}

// argoManagingAll is an Argo CD whose Application openshift-gitops/testArgoApp
// (Synced) lists objs: the cluster a gitops test means by "Argo CD manages
// it".
func argoManagingAll(objs ...argoObject) *fakeArgoCD {
	return &fakeArgoCD{served: true, apps: []map[string]any{argoApplicationListing("openshift-gitops", objs...)}}
}

// argoManagingAllBut is argoManagingAll of the gitops fakes' objects
// (argoGitOpsObjects) except these: a test that takes Argo CD's marks off an
// object takes it out of the Application too, since Argo CD's list decides.
func argoManagingAllBut(except ...argoObject) *fakeArgoCD {
	var objs []argoObject
	for _, o := range argoGitOpsObjects {
		if !slices.Contains(except, o) {
			objs = append(objs, o)
		}
	}
	return argoManagingAll(objs...)
}

// argoApplicationListing is Application ns/testArgoApp, compared with Git
// (Synced), whose status.resources lists objs as Argo CD does: a
// cluster-scoped object without a namespace.
func argoApplicationListing(ns string, objs ...argoObject) map[string]any {
	resources := []any{}
	for _, o := range objs {
		r := map[string]any{"group": o.group, "version": "v1", "kind": o.kind, "name": o.name, "status": "Synced"}
		if !o.clusterScoped {
			r["namespace"] = o.namespace
		}
		resources = append(resources, r)
	}
	return map[string]any{
		"metadata": map[string]any{"name": testArgoApp, "namespace": ns},
		"status": map[string]any{
			"reconciledAt": "2026-11-19T10:00:00Z",
			"sync":         map[string]any{"status": "Synced"},
			"resources":    resources,
		},
	}
}

// the operator objects and the DSC the gitops fakes track.
var argoGitOpsObjects = []argoObject{argoSubscriptionObject, argoCatalogObject, argoDSCObject("default-dsc")}

const argoDiscoveryPath = "/apis/argoproj.io/v1alpha1"

// serve answers r when it is one of the fake's paths; false otherwise.
func (f *fakeArgoCD) serve(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	isArgo := strings.HasPrefix(p, argoDiscoveryPath)
	isACM := p == acmWorkAPI
	isBinding := p == "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings" && strings.Contains(r.URL.Query().Get("fieldSelector"), argoCDManagerBinding)
	roleBindingNS, isRoleBinding := managerRoleBindingNamespace(p)
	if r.Method != http.MethodGet || (!isArgo && !isACM && !isBinding && !isRoleBinding) {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.URL.RequestURI())
	w.Header().Set("Content-Type", "application/json")
	if code := f.fail[p]; code != 0 {
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, `{"kind":"Status","status":"Failure","reason":%q,"message":"fake failure","code":%d}`, http.StatusText(code), code)
		return true
	}
	switch {
	case p == argoDiscoveryPath && f.rolloutsOnly:
		_, _ = io.WriteString(w, `{"kind":"APIResourceList","groupVersion":"argoproj.io/v1alpha1","resources":[{"name":"rollouts","namespaced":true,"kind":"Rollout"}]}`)
	case p == argoDiscoveryPath:
		if !f.served {
			notFound(w)
			return true
		}
		_, _ = io.WriteString(w, `{"kind":"APIResourceList","groupVersion":"argoproj.io/v1alpha1","resources":[{"name":"applications","namespaced":true,"kind":"Application"},{"name":"appprojects","namespaced":true,"kind":"AppProject"}]}`)
	case isArgo:
		if !f.served {
			notFound(w)
			return true
		}
		ns := ""
		if rest, ok := strings.CutPrefix(p, argoDiscoveryPath+"/namespaces/"); ok {
			ns, _, _ = strings.Cut(rest, "/")
		}
		name := strings.TrimPrefix(r.URL.Query().Get("fieldSelector"), "metadata.name=")
		items := []any{}
		for _, app := range f.apps {
			meta := app["metadata"].(map[string]any)
			if (name == "" || meta["name"] == name) && (ns == "" || meta["namespace"] == ns) {
				items = append(items, app)
			}
		}
		writeJSON(w, map[string]any{"kind": "ApplicationList", "items": items})
	case isACM:
		if !f.acm {
			notFound(w)
			return true
		}
		_, _ = io.WriteString(w, `{"kind":"APIResourceList","groupVersion":"work.open-cluster-management.io/v1","resources":[{"name":"appliedmanifestworks","namespaced":false,"kind":"AppliedManifestWork"}]}`)
	case isBinding:
		items := []any{}
		if f.argocdManager {
			items = append(items, map[string]any{"metadata": map[string]any{"name": argoCDManagerBinding}})
		}
		writeJSON(w, map[string]any{"items": items})
	case isRoleBinding:
		if !slices.Contains(f.managerRoleBindings, roleBindingNS) {
			notFound(w)
			return true
		}
		writeJSON(w, map[string]any{"kind": "RoleBinding", "metadata": map[string]any{"name": argoCDManagerBinding, "namespace": roleBindingNS}})
	}
	return true
}

// managerRoleBindingNamespace is the namespace of a path to RoleBinding
// argocd-manager-role-binding; false for any other path.
func managerRoleBindingNamespace(p string) (string, bool) {
	rest, ok := strings.CutPrefix(p, "/apis/rbac.authorization.k8s.io/v1/namespaces/")
	if !ok {
		return "", false
	}
	ns, name, _ := strings.Cut(rest, "/")
	return ns, name == "rolebindings/"+argoCDManagerBinding
}

// managerRoleBindingPath is the path of RoleBinding argocd-manager-role-binding
// in ns.
func managerRoleBindingPath(ns string) string {
	return "/apis/rbac.authorization.k8s.io/v1/namespaces/" + ns + "/rolebindings/" + argoCDManagerBinding
}

// handler is an http.Handler that answers only the fake's paths (404
// otherwise), for tests of argocd.go alone.
func (f *fakeArgoCD) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !f.serve(w, r) {
			notFound(w)
		}
	})
}

// register adds the fake's paths to a fakeAPI (which matches paths without
// their query).
func (f *fakeArgoCD) register(api *fakeAPI) {
	paths := []string{argoDiscoveryPath, argoDiscoveryPath + "/applications", acmWorkAPI, "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings"}
	for _, ns := range append([]string{SubNS, CatalogNS, "redhat-ods-applications"}, f.managerRoleBindings...) {
		paths = append(paths, managerRoleBindingPath(ns))
	}
	for _, p := range paths {
		api.handle(http.MethodGet, p, func(r *http.Request, _ []byte) (int, string) {
			rec := httptest.NewRecorder()
			if !f.serve(rec, r) {
				return http.StatusNotFound, k8sNotFound
			}
			return rec.Code, rec.Body.String()
		})
	}
}

// setApps replaces the Applications on the cluster.
func (f *fakeArgoCD) setApps(apps ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.apps = apps
}

// requested returns the paths asked so far.
func (f *fakeArgoCD) requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}
