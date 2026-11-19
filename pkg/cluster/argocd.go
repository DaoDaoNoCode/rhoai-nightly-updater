package cluster

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Confirming Argo CD's tracking annotation with Argo CD.
//
// argocd.argoproj.io/tracking-id (gitops.go, argoApplication) outlives the
// application that wrote it: an Application deleted without cascade, Argo
// CD or OpenShift GitOps uninstalled, a cluster provisioned by GitOps once
// and handed over, an object copied from another cluster. Trusting the
// annotation alone then switches features off for a cluster nobody manages
// from Git. So the tool asks Argo CD whether the application still manages
// the object, and treats the annotation as left over only when Argo CD's
// answer (or its absence) shows it:
//
//   - Argo CD's Application API (argoproj.io/v1alpha1 applications) is not
//     served here, or no Application of that name exists: left over, unless
//     an Argo CD on another cluster may manage this one (remoteArgoCD).
//   - The Application exists, Argo CD has compared it with Git, and its
//     status.resources does not list the object: left over. Argo CD lists
//     every object of the application's (in Git, or carrying its tracking
//     id and to be pruned: controller/state.go, CompareAppState, the
//     ResourceStatus of each managed object), so an Application that manages
//     the object lists it.
//   - It lists it: managed.
//
// Anything else (a read error, a 403, an Application Argo CD has not
// compared yet or could not compare) leaves the object managed: the tool
// must never fight Argo CD on a guess.
//
// The application part of the id is "<name>" for an Application in Argo
// CD's own namespace and "<namespace>_<name>" for one elsewhere
// (util/argo/argo.go AppInstanceName in Argo CD; "_" cannot occur in a
// Kubernetes name). The first form does not say which namespace Argo CD
// runs in (openshift-gitops for OpenShift GitOps' default instance, any
// namespace for another instance or an upstream install), so the tool lists
// Applications of that name in every namespace: only Argo CD's own
// namespace can hold the one that wrote the id, and an Application of the
// same name elsewhere can only add a "managed" answer, never remove one.
// Listing by name needs `list` on applications cluster-wide, and nothing
// else (SECURITY.md, "Reading Argo CD Applications").

// argoAPI is the path of Argo CD's Application API.
const argoAPI = "/apis/argoproj.io/v1alpha1"

// argoObject is an object Argo CD may manage, as status.resources lists it.
type argoObject struct {
	group, kind, namespace, name string
	clusterScoped                bool
}

var (
	argoSubscriptionObject = argoObject{group: "operators.coreos.com", kind: "Subscription", namespace: SubNS, name: SubName}
	argoCatalogObject      = argoObject{group: "operators.coreos.com", kind: "CatalogSource", namespace: CatalogNS, name: CatalogName}
)

func argoDSCObject(name string) argoObject {
	return argoObject{group: dscGroup, kind: "DataScienceCluster", name: name, clusterScoped: true}
}

// argoTracking is the verdict on one object's tracking annotation.
type argoTracking struct {
	app   string // the application part of the tracking id; "" when untracked
	state string // types.ArgoCD*; "" when untracked
	// staleReason: types.ArgoCDNotInstalled, ArgoCDApplicationGone or
	// ArgoCDNotAmongResources.
	staleReason string
	reason      string // why Argo CD could not confirm (unconfirmed)
	namespace   string // the namespace of the Application found
	sync        string // its status.sync.status (managed)
}

// managedBy is the application the tool treats as managing the object: the
// tracked application unless the annotation is left over. Every refusal
// because of Argo CD reads this.
func (t argoTracking) managedBy() string {
	if t.state == types.ArgoCDManaged || t.state == types.ArgoCDUnconfirmed {
		return t.app
	}
	return ""
}

// info is the verdict for the status; nil when the object is untracked.
func (t argoTracking) info() *types.ArgoCDTracking {
	if t.app == "" {
		return nil
	}
	return &types.ArgoCDTracking{Application: t.app, State: t.state, Namespace: t.namespace,
		StaleReason: t.staleReason, Reason: t.reason, SyncStatus: t.sync}
}

// unconfirmedNote is appended to a refusal when Argo CD could not confirm
// the tracking; "" otherwise.
func (t argoTracking) unconfirmedNote() string {
	if t.state != types.ArgoCDUnconfirmed {
		return ""
	}
	return fmt.Sprintf(" (The tool could not confirm this with Argo CD: %s. It treats the object as managed until it can.)", t.reason)
}

// argoCheck confirms tracking annotations with Argo CD. It reads each
// answer once (discovery, each application, the remote check), so one
// status poll or one operation check asks once for all its objects. Not
// safe for concurrent use.
type argoCheck struct {
	c      *Client
	served *argoRead[bool]
	apps   map[string]argoRead[[]argoApp]
	remote *argoRead[string]
}

// argoRead is a memoised read: its value, or the reason it failed.
type argoRead[T any] struct {
	value T
	err   string
}

func newArgoCheck(c *Client) *argoCheck {
	return &argoCheck{c: c, apps: map[string]argoRead[[]argoApp]{}}
}

// argoTrackingOf confirms the tracking annotation of one decoded object (a
// Subscription, the DataScienceCluster) of this group and kind. Its
// metadata gives the name, and the namespace ("" for a cluster-scoped
// object).
func argoTrackingOf(c *Client, obj map[string]any, group, kind string) argoTracking {
	return newArgoCheck(c).objectOf(obj, group, kind)
}

func (a *argoCheck) objectOf(obj map[string]any, group, kind string) argoTracking {
	meta, _ := obj["metadata"].(map[string]any)
	raw, _ := meta["annotations"].(map[string]any)
	id, _ := raw[argoTrackingAnnotation].(string)
	name, _ := meta["name"].(string)
	ns, _ := meta["namespace"].(string)
	o := argoObject{group: group, kind: kind, namespace: ns, name: name, clusterScoped: ns == ""}
	return a.object(map[string]string{argoTrackingAnnotation: id}, o)
}

// object confirms the tracking annotation among annotations of object o.
func (a *argoCheck) object(annotations map[string]string, o argoObject) argoTracking {
	idNS := o.namespace
	if o.clusterScoped {
		idNS = ""
	}
	app := argoApplication(annotations, o.kind, idNS, o.name)
	if app == "" {
		return argoTracking{}
	}
	t := argoTracking{app: app}
	unconfirmed := func(reason string) argoTracking {
		t.state, t.reason = types.ArgoCDUnconfirmed, reason
		return t
	}
	stale := func(why string) argoTracking {
		t.state, t.staleReason = types.ArgoCDStale, why
		return t
	}
	ns, name, qualified := strings.Cut(app, "_")
	if !qualified {
		ns, name = "", app
	}
	if !argoNameRE.MatchString(name) || (qualified && !argoNameRE.MatchString(ns)) {
		return unconfirmed(fmt.Sprintf("the annotation names %q, which is not an application name", app))
	}

	served := a.applicationsServed()
	if served.err != "" {
		return unconfirmed(served.err)
	}
	if !served.value {
		if remote := a.remoteArgoCD(); remote.err != "" || remote.value != "" {
			return unconfirmed("Argo CD is not installed on this cluster, but " + nonEmpty(remote.value, remote.err))
		}
		return stale(types.ArgoCDNotInstalled)
	}

	found := a.applications(app, ns, name)
	if found.err != "" {
		return unconfirmed(found.err)
	}
	if len(found.value) == 0 {
		if remote := a.remoteArgoCD(); remote.err != "" || remote.value != "" {
			return unconfirmed(fmt.Sprintf("no application %q is on this cluster, but %s", app, nonEmpty(remote.value, remote.err)))
		}
		return stale(types.ArgoCDApplicationGone)
	}
	for _, ap := range found.value {
		if ap.lists(o) {
			t.state, t.namespace, t.sync = types.ArgoCDManaged, ap.Metadata.Namespace, ap.Status.Sync.Status
			return t
		}
	}
	for _, ap := range found.value {
		if why := ap.notCompared(); why != "" {
			t.namespace = ap.Metadata.Namespace
			return unconfirmed(why)
		}
	}
	t.namespace = found.value[0].Metadata.Namespace
	return stale(types.ArgoCDNotAmongResources)
}

// argoNameRE: an application's name and namespace, as Kubernetes allows
// them (DNS-1123 subdomain). Anything else in the id is not Argo CD's, and
// is not put into a request path.
var argoNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

// applicationsServed reads Argo CD's API discovery document (readable by
// every authenticated user, system:discovery): 404 when the group version
// is not served. argoproj.io is also Argo Rollouts' and Argo Workflows'
// group, so the applications resource must be in it.
func (a *argoCheck) applicationsServed() argoRead[bool] {
	if a.served != nil {
		return *a.served
	}
	var r argoRead[bool]
	body, _, err := a.c.get(argoAPI)
	switch {
	case IsK8sError(err, 404):
	case err != nil:
		r.err = fmt.Sprintf("reading the API server's list of Argo CD APIs failed: %v", err)
	default:
		r.value, r.err = discoveryServes(body, "applications")
		if r.err != "" {
			r.err = "reading the API server's list of Argo CD APIs failed: " + r.err
		}
	}
	a.served = &r
	return r
}

// discoveryServes reports whether an APIResourceList lists resource.
func discoveryServes(body []byte, resource string) (bool, string) {
	var list struct {
		Resources []struct {
			Name string `json:"name"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return false, err.Error()
	}
	for _, r := range list.Resources {
		if r.Name == resource {
			return true, ""
		}
	}
	return false, ""
}

// argoApp is the part of an Application the check reads.
type argoApp struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Status struct {
		ReconciledAt string `json:"reconciledAt"`
		Sync         struct {
			Status string `json:"status"`
		} `json:"sync"`
		Resources []struct {
			Group     string `json:"group"`
			Kind      string `json:"kind"`
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
		} `json:"resources"`
		Conditions []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

// lists reports whether the Application's status.resources lists o. Argo
// CD clears the namespace of a cluster-scoped object there.
func (ap argoApp) lists(o argoObject) bool {
	for _, r := range ap.Status.Resources {
		if r.Group == o.group && r.Kind == o.kind && r.Name == o.name && (o.clusterScoped || r.Namespace == o.namespace) {
			return true
		}
	}
	return false
}

// notCompared says why the Application's status.resources may not be
// complete: Argo CD has not compared it with Git yet (no reconciledAt, sync
// status Unknown), or the comparison failed (the error conditions of
// pkg/apis/application/v1alpha1 types.go, ApplicationCondition*Error). ""
// when the list is Argo CD's full answer.
func (ap argoApp) notCompared() string {
	qualified := ap.Metadata.Namespace + "/" + ap.Metadata.Name
	for _, cond := range ap.Status.Conditions {
		switch cond.Type {
		case "ComparisonError", "InvalidSpecError", "UnknownError":
			return fmt.Sprintf("application %s reports %s: %s", qualified, cond.Type, truncateText(cond.Message, 200))
		}
	}
	if ap.Status.ReconciledAt == "" || ap.Status.Sync.Status == "" || ap.Status.Sync.Status == "Unknown" {
		return fmt.Sprintf("Argo CD has not compared application %s with Git yet", qualified)
	}
	return ""
}

// applications lists the Applications named name in namespace ns, or in
// every namespace when ns is "". app is the tracking id's application, for
// messages.
func (a *argoCheck) applications(app, ns, name string) argoRead[[]argoApp] {
	key := ns + "/" + name
	if r, ok := a.apps[key]; ok {
		return r
	}
	path := argoAPI + "/applications"
	if ns != "" {
		path = argoAPI + "/namespaces/" + ns + "/applications"
	}
	var r argoRead[[]argoApp]
	body, _, err := a.c.get(path + "?fieldSelector=" + url.QueryEscape("metadata.name="+name))
	if err != nil {
		r.err = fmt.Sprintf("reading Argo CD application %q failed: %v", app, err)
	} else {
		var list struct {
			Items []argoApp `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			r.err = fmt.Sprintf("reading Argo CD application %q failed: %v", app, err)
		} else {
			r.value = list.Items
		}
	}
	a.apps[key] = r
	return r
}

// The signs that an Argo CD on another cluster may manage this one. Argo
// CD reaches another cluster with credentials made there, and two ways of
// making them leave a mark on the managed cluster:
//
//   - An ACM hub's GitOpsCluster adds every managed cluster it places to the
//     hub's Argo CD (the push model; docs/GITOPS.md section 9's
//     ApplicationSets). Every ACM (Open Cluster Management) managed cluster
//     serves the AppliedManifestWork API: the klusterlet applies its CRD to
//     the managed cluster, in the default and the hosted mode
//     (open-cluster-management-io/ocm manifests/klusterlet/managed/
//     0000_01_work.open-cluster-management.io_appliedmanifestworks.crd.yaml).
//   - `argocd cluster add` creates ClusterRoleBinding
//     argocd-manager-role-binding for ServiceAccount kube-system/argocd-manager
//     (util/clusterauth/clusterauth.go in Argo CD).
//
// Cluster credentials made by hand (any token in an Argo CD cluster Secret)
// leave no mark the tool can read: docs/GITOPS.md section 8 names this
// limit.
const (
	acmWorkAPI             = "/apis/work.open-cluster-management.io/v1"
	argoCDManagerBinding   = "argocd-manager-role-binding"
	argoCDManagerFieldPath = "metadata.name=" + argoCDManagerBinding
)

// remoteArgoCD says why an Argo CD on another cluster may manage this one
// (value), or why that could not be read (err); both "" when there is no
// sign of one.
func (a *argoCheck) remoteArgoCD() argoRead[string] {
	if a.remote != nil {
		return *a.remote
	}
	var r argoRead[string]
	defer func() { a.remote = &r }()
	body, _, err := a.c.get(acmWorkAPI)
	switch {
	case IsK8sError(err, 404):
	case err != nil:
		r.err = fmt.Sprintf("whether an ACM hub manages this cluster could not be read: %v", err)
		return r
	default:
		served, perr := discoveryServes(body, "appliedmanifestworks")
		if perr != "" {
			r.err = "whether an ACM hub manages this cluster could not be read: " + perr
			return r
		}
		if served {
			r.value = "an ACM hub manages this cluster (it serves ACM's AppliedManifestWork API), and an Argo CD on the hub may manage these objects"
			return r
		}
	}
	body, _, err = a.c.get(clusterPath("rbac.authorization.k8s.io/v1", "clusterrolebindings", "") + "?fieldSelector=" + url.QueryEscape(argoCDManagerFieldPath))
	if err != nil {
		r.err = fmt.Sprintf("whether an Argo CD on another cluster manages this one could not be read: %v", err)
		return r
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		r.err = fmt.Sprintf("whether an Argo CD on another cluster manages this one could not be read: %v", err)
		return r
	}
	if len(list.Items) > 0 {
		r.value = "ClusterRoleBinding " + argoCDManagerBinding + " (made by `argocd cluster add`) lets an Argo CD on another cluster manage this one"
	}
	return r
}
