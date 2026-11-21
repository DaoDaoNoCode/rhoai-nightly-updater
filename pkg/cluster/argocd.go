package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Whether Argo CD manages an object, asked of Argo CD itself.
//
// Argo CD marks the objects it syncs in one of two ways, by its resource
// tracking method (Argo CD util/argo/resource_tracking.go, SetAppInstance):
// the annotation argocd.argoproj.io/tracking-id ("annotation" and
// "annotation+label"), or only the instance label, app.kubernetes.io/instance
// unless application.instanceLabelKey names another key ("label"). Upstream
// Argo CD defaults to the annotation since 3.0; OpenShift GitOps defaulted to
// the label up to 1.18 and switched to the annotation in 1.19
// (redhat-developer/gitops-operator docs/upgrading.md, "Resource tracking
// method default changed from label to annotation"). Objects synced by
// OpenShift GitOps 1.12 to 1.18 therefore carry no tracking annotation.
//
// Neither mark proves that Argo CD manages the object now: a mark outlives
// the Application that wrote it (an Application deleted without cascade,
// OpenShift GitOps uninstalled, a cluster set up from Git once and handed
// over, an object copied from another cluster), Helm and many operators set
// app.kubernetes.io/instance themselves, and an object Git holds is Argo
// CD's before Argo CD first marks it. So the decision is Argo CD's own list:
// an Application's status.resources, which holds every object the
// application syncs or would prune (controller/state.go CompareAppState in
// Argo CD: one ResourceStatus per target and per tracked live object; a
// cluster-scoped object with an empty namespace, DeduplicateTargetObjects).
// The tool lists every Application and treats the object as managed when one
// lists it, whatever its marks say. The marks only name the application in
// messages and choose which Applications' doubts count:
//
//   - Argo CD's Application API (argoproj.io/v1alpha1 applications) is not
//     served here: not managed by an Argo CD on this cluster. A marked
//     object is then managed (not confirmed) when an Argo CD on another
//     cluster may manage this one (remoteArgoCD), else its tracking
//     annotation is left over; an instance label alone is not reported (it
//     need not be Argo CD's).
//   - An Application lists the object: managed (argoApp.manages).
//   - None lists it, and an Application the marks name has not been compared
//     with Git or failed to compare (argoApp.notCompared): not confirmed.
//   - None lists it, and the object carries no mark: not managed.
//   - Otherwise: not confirmed when an Argo CD on another cluster may manage
//     this one, else left over (the named Application lists other objects,
//     or no Application of that name exists; a label naming no Application
//     is not reported).
//
// A read error or a 403 leaves the object managed (not confirmed); without
// any mark to name the application, messages say argoUnknownApplication.
// The tool must never fight Argo CD on a guess.
//
// A mark's application is "<name>" for an Application in Argo CD's own
// namespace and "<namespace>_<name>" for one elsewhere (pkg/apis/application/
// v1alpha1 InstanceName; "_" cannot occur in a Kubernetes name). The first
// form does not say which namespace Argo CD runs in, so an Application of
// that name in any namespace matches it. Listing every Application needs
// `list` on applications cluster-wide, and nothing else (SECURITY.md,
// "Reading Argo CD Applications").

// argoAPI is the path of Argo CD's Application API.
const argoAPI = "/apis/argoproj.io/v1alpha1"

// argoInstanceLabel is the label Argo CD's label tracking writes, under its
// default application.instanceLabelKey (common.LabelKeyAppInstance in Argo
// CD).
const argoInstanceLabel = "app.kubernetes.io/instance"

// argoUnknownApplication names the application when Argo CD could not be
// asked about an object that carries no mark of one. Parentheses cannot
// occur in an Application's name.
const argoUnknownApplication = "(unknown)"

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

// argoTracking is the verdict on one object.
type argoTracking struct {
	// app is the application as the object's mark names it, or the name of
	// the Application that lists an unmarked object; "" when neither.
	app   string
	state string // types.ArgoCD*; "" when not managed and nothing to report
	// staleReason: types.ArgoCDNotInstalled, ArgoCDApplicationGone or
	// ArgoCDNotAmongResources.
	staleReason string
	reason      string // why Argo CD could not confirm (unconfirmed)
	namespace   string // the namespace of the Application found
	sync        string // its status.sync.status (managed)
}

// managedBy is the application the tool treats as managing the object, ""
// when none does. Every refusal because of Argo CD reads this.
func (t argoTracking) managedBy() string {
	if t.state == types.ArgoCDManaged || t.state == types.ArgoCDUnconfirmed {
		return nonEmpty(t.app, argoUnknownApplication)
	}
	return ""
}

// info is the verdict for the status; nil when there is nothing to report.
func (t argoTracking) info() *types.ArgoCDTracking {
	if t.state == "" {
		return nil
	}
	return &types.ArgoCDTracking{Application: nonEmpty(t.app, argoUnknownApplication), State: t.state, Namespace: t.namespace,
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

// argoCheck asks Argo CD about objects. It reads each answer once
// (discovery, the Applications, each remote sign), so one status poll or one
// operation check asks once for all its objects; status reads also share
// the list of Applications across polls (readArgoApplications). Not safe
// for concurrent use.
type argoCheck struct {
	c        *Client
	served   *argoRead[bool]
	apps     *argoRead[[]argoApp]
	remote   *argoRead[string]
	bindings map[string]argoRead[string] // argocd-manager RoleBinding, by namespace
	appsNS   *argoRead[string]           // RHOAI's applications namespace
}

// argoRead is a memoised read: its value, or the reason it failed.
type argoRead[T any] struct {
	value T
	err   string
}

func newArgoCheck(c *Client) *argoCheck {
	return &argoCheck{c: c, bindings: map[string]argoRead[string]{}}
}

// argoTrackingOf asks Argo CD about one decoded object (a Subscription, the
// DataScienceCluster) of this group and kind. Its metadata gives the name,
// the namespace ("" for a cluster-scoped object) and the marks.
func argoTrackingOf(c *Client, obj map[string]any, group, kind string) argoTracking {
	return newArgoCheck(c).objectOf(obj, group, kind)
}

func (a *argoCheck) objectOf(obj map[string]any, group, kind string) argoTracking {
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	ns, _ := meta["namespace"].(string)
	o := argoObject{group: group, kind: kind, namespace: ns, name: name, clusterScoped: ns == ""}
	return a.object(stringMap(meta["annotations"]), stringMap(meta["labels"]), o)
}

// stringMap is the string entries of a decoded map.
func stringMap(v any) map[string]string {
	raw, _ := v.(map[string]any)
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// argoMarks are what Argo CD may have written on an object.
type argoMarks struct {
	// annotation is the application of a tracking annotation that names the
	// object itself; "" when there is none.
	annotation string
	// copied: the tracking annotation names another object, so Argo CD
	// neither syncs nor prunes this one by it (isSelfReferencedObj).
	copied bool
	// label is the instance label's value.
	label string
}

func argoMarksOf(annotations, labels map[string]string, o argoObject) argoMarks {
	idNS := o.namespace
	if o.clusterScoped {
		idNS = ""
	}
	m := argoMarks{annotation: argoApplication(annotations, o.group, o.kind, idNS, o.name), label: labels[argoInstanceLabel]}
	m.copied = m.annotation == "" && annotations[argoTrackingAnnotation] != ""
	return m
}

// name is the application the marks name: the annotation's, else the
// label's.
func (m argoMarks) name() string {
	return nonEmpty(m.annotation, m.label)
}

// object asks Argo CD about object o, which carries these annotations and
// labels.
func (a *argoCheck) object(annotations, labels map[string]string, o argoObject) argoTracking {
	marks := argoMarksOf(annotations, labels, o)
	t := argoTracking{app: marks.name()}
	unconfirmed := func(reason string) argoTracking {
		t.state, t.reason = types.ArgoCDUnconfirmed, reason
		return t
	}
	stale := func(why string) argoTracking {
		t.state, t.staleReason = types.ArgoCDStale, why
		return t
	}
	// remote is the verdict when no Application here manages a marked
	// object: not confirmed when an Argo CD elsewhere may manage it.
	remote := func(here string) (argoTracking, bool) {
		if r := a.remoteArgoCD(o); r.err != "" || r.value != "" {
			return unconfirmed(here + ", but " + nonEmpty(r.value, r.err)), true
		}
		return t, false
	}

	served := a.applicationsServed()
	if served.err != "" {
		return unconfirmed(served.err)
	}
	if !served.value {
		if marks.name() == "" {
			return argoTracking{}
		}
		if v, ok := remote("Argo CD is not installed on this cluster"); ok {
			return v
		}
		if marks.annotation == "" {
			return argoTracking{}
		}
		return stale(types.ArgoCDNotInstalled)
	}

	apps := a.applications()
	if apps.err != "" {
		return unconfirmed(apps.err)
	}
	var listing, named []argoApp
	for _, ap := range apps.value {
		if ap.manages(o, marks) {
			listing = append(listing, ap)
		}
		if ap.isNamed(marks.annotation) || ap.isNamed(marks.label) {
			named = append(named, ap)
		}
	}
	if len(listing) > 0 {
		found := listing[0]
		for _, ap := range listing {
			if ap.isNamed(t.app) {
				found = ap
				break
			}
		}
		if !found.isNamed(t.app) {
			t.app = found.Metadata.Name
		}
		t.state, t.namespace, t.sync = types.ArgoCDManaged, found.Metadata.Namespace, found.Status.Sync.Status
		return t
	}
	for _, ap := range named {
		if why := ap.notCompared(); why != "" {
			t.namespace = ap.Metadata.Namespace
			return unconfirmed(why)
		}
	}
	if marks.name() == "" {
		return argoTracking{}
	}
	if len(named) > 0 {
		t.namespace = named[0].Metadata.Namespace
		if v, ok := remote(fmt.Sprintf("application %s/%s does not list it", named[0].Metadata.Namespace, named[0].Metadata.Name)); ok {
			return v
		}
		return stale(types.ArgoCDNotAmongResources)
	}
	if v, ok := remote(fmt.Sprintf("no application %q is on this cluster", t.app)); ok {
		return v
	}
	if marks.annotation == "" {
		return argoTracking{}
	}
	return stale(types.ArgoCDApplicationGone)
}

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
	case isK8sNotFound(err):
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
			Status    string `json:"status"`
			Hook      bool   `json:"hook"`
		} `json:"resources"`
		Conditions []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

// manages reports whether the Application's status.resources lists o. Argo
// CD clears the namespace of a cluster-scoped object there. One exception,
// as Argo CD decides it (controller/state.go CompareAppState and
// isSelfReferencedObj): an object whose tracking annotation names another
// object (copied from it) is listed without a sync status, and Argo CD
// neither syncs nor prunes it; an object Git holds always gets one.
func (ap argoApp) manages(o argoObject, marks argoMarks) bool {
	for _, r := range ap.Status.Resources {
		if r.Group != o.group || r.Kind != o.kind || r.Name != o.name || (!o.clusterScoped && r.Namespace != o.namespace) {
			continue
		}
		if marks.copied && r.Status == "" && !r.Hook {
			continue
		}
		return true
	}
	return false
}

// isNamed reports whether app, as a mark names an application ("<name>" or
// "<namespace>_<name>"), is this Application. Label tracking cuts the
// value to the label's 63 characters (util/argo/resource_tracking.go,
// LabelMaxLength).
func (ap argoApp) isNamed(app string) bool {
	if app == "" {
		return false
	}
	for _, instance := range []string{ap.Metadata.Name, ap.Metadata.Namespace + "_" + ap.Metadata.Name} {
		if app == instance || (len(app) == argoLabelMaxLength && strings.HasPrefix(instance, app)) {
			return true
		}
	}
	return false
}

// argoLabelMaxLength is the most a label value holds.
const argoLabelMaxLength = 63

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

// applications lists every Argo CD Application on the cluster.
func (a *argoCheck) applications() argoRead[[]argoApp] {
	if a.apps != nil {
		return *a.apps
	}
	var r argoRead[[]argoApp]
	apps, err := readArgoApplications(a.c)
	if err != nil {
		r.err = fmt.Sprintf("listing Argo CD applications failed: %v", err)
	} else {
		r.value = apps
	}
	a.apps = &r
	return r
}

// argoApplicationsTTL bounds how long a status read reuses the list of
// every Application: the pages poll every few seconds, each open page
// asks, and the list holds every Application's status.resources. Only
// clients of read requests use it (Client.statusRead); a change's checks
// list again every time. Only successful lists are cached.
const argoApplicationsTTL = 30 * time.Second

var (
	argoApplicationsCache  = newLRU[string, []argoApp](16)
	argoApplicationsFlight flightGroup[[]argoApp]
)

// readArgoApplications lists every Application: for a status read the list
// of the last argoApplicationsTTL, with concurrent status reads sharing one
// request; otherwise always from the API server. The cached slice is shared
// and must not be changed.
func readArgoApplications(c *Client) ([]argoApp, error) {
	if !c.statusRead {
		return listArgoApplications(c)
	}
	key := c.baseURL
	if apps, ok := argoApplicationsCache.Get(key); ok {
		return apps, nil
	}
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	apps, err, _ := argoApplicationsFlight.Do(ctx, key, func(context.Context) ([]argoApp, error) {
		if apps, ok := argoApplicationsCache.Get(key); ok {
			return apps, nil
		}
		apps, err := listArgoApplications(c)
		if err == nil {
			argoApplicationsCache.Add(key, apps, argoApplicationsTTL)
		}
		return apps, err
	})
	return apps, err
}

// listArgoApplications reads the list of every Application.
func listArgoApplications(c *Client) ([]argoApp, error) {
	body, _, err := c.get(argoAPI + "/applications")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []argoApp `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
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
//     The CRD stays after the cluster is detached (ocm pkg/operator/
//     operators/klusterlet/controllers/klusterletcontroller/
//     klusterlet_crd_reconcile.go: CRDs shared by klusterlets are no longer
//     removed), so a detached cluster still shows the sign.
//   - `argocd cluster add` creates ServiceAccount argocd-manager in
//     kube-system and binds it (util/clusterauth/clusterauth.go in Argo CD,
//     InstallClusterManagerRBAC): ClusterRoleBinding
//     argocd-manager-role-binding, or with --namespace a RoleBinding of that
//     name in each namespace given. The RoleBinding is looked for in the
//     operator's namespace (SubNS), the catalog's (CatalogNS), RHOAI's
//     applications namespace and the object's own.
//
// What leaves no mark the tool can read (RUNBOOK section 13): `argocd
// cluster add --service-account` (an existing account, no binding made),
// --aws-cluster-name or --exec-command (no account at all;
// cmd/argocd/commands/cluster.go NewClusterAddCommand), and cluster Secrets
// made by hand.
const (
	acmWorkAPI             = "/apis/work.open-cluster-management.io/v1"
	argoCDManagerBinding   = "argocd-manager-role-binding"
	argoCDManagerFieldPath = "metadata.name=" + argoCDManagerBinding
)

// remoteArgoCD says why an Argo CD on another cluster may manage o (value),
// or why that could not be read (err); both "" when there is no sign of one.
func (a *argoCheck) remoteArgoCD(o argoObject) argoRead[string] {
	if r := a.remoteCluster(); r.err != "" || r.value != "" {
		return r
	}
	namespaces := []string{SubNS, CatalogNS}
	appsNS := a.applicationsNamespace()
	if appsNS.err != "" {
		return argoRead[string]{err: "whether an Argo CD on another cluster manages RHOAI's applications namespace could not be read: " + appsNS.err}
	}
	if appsNS.value != "" {
		namespaces = append(namespaces, appsNS.value)
	}
	if !o.clusterScoped && o.namespace != "" {
		namespaces = append(namespaces, o.namespace)
	}
	for _, ns := range namespaces {
		if r := a.managerRoleBinding(ns); r.err != "" || r.value != "" {
			return r
		}
	}
	return argoRead[string]{}
}

// remoteCluster reads the cluster-wide signs: ACM's AppliedManifestWork
// API, and ClusterRoleBinding argocd-manager-role-binding.
func (a *argoCheck) remoteCluster() argoRead[string] {
	if a.remote != nil {
		return *a.remote
	}
	var r argoRead[string]
	defer func() { a.remote = &r }()
	body, _, err := a.c.get(acmWorkAPI)
	switch {
	case isK8sNotFound(err):
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

// managerRoleBinding reads RoleBinding argocd-manager-role-binding in ns,
// which `argocd cluster add --namespace` makes.
func (a *argoCheck) managerRoleBinding(ns string) argoRead[string] {
	if r, ok := a.bindings[ns]; ok {
		return r
	}
	var r argoRead[string]
	_, _, err := a.c.get(namespacedPath("rbac.authorization.k8s.io/v1", "rolebindings", ns, argoCDManagerBinding))
	switch {
	case isK8sNotFound(err):
	case err != nil:
		r.err = fmt.Sprintf("whether an Argo CD on another cluster manages namespace %s could not be read: %v", ns, err)
	default:
		r.value = fmt.Sprintf("RoleBinding %s in namespace %s (made by `argocd cluster add --namespace`) lets an Argo CD on another cluster manage that namespace", argoCDManagerBinding, ns)
	}
	a.bindings[ns] = r
	return r
}

// applicationsNamespace is RHOAI's applications namespace, "" when RHOAI
// has none (no DSCInitialization), or why it could not be read.
func (a *argoCheck) applicationsNamespace() argoRead[string] {
	if a.appsNS != nil {
		return *a.appsNS
	}
	var r argoRead[string]
	ns, err := appsNamespace(a.c)
	switch {
	case err == nil:
		r.value = ns
	case !errors.Is(err, errNoDSCInitialization):
		r.err = err.Error()
	}
	a.appsNS = &r
	return r
}
