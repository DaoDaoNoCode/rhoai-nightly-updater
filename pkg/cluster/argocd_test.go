package cluster

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func argoClient(t *testing.T, f *fakeArgoCD) *Client {
	t.Helper()
	srv := newFakePlainServer(f.handler())
	t.Cleanup(srv.Close)
	return &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: context.Background()}
}

// subTracked is the Subscription's annotations with a tracking id naming app.
func subTracked(app string) map[string]string {
	return map[string]string{argoTrackingAnnotation: app + ":operators.coreos.com/Subscription:" + SubNS + "/" + SubName}
}

// subLabelled is the Subscription's labels under label tracking (OpenShift
// GitOps up to 1.18): the instance label naming app, no annotation.
func subLabelled(app string) map[string]string {
	return map[string]string{argoInstanceLabel: app}
}

// Each answer of Argo CD (or its absence) and the verdict on the
// Subscription with a tracking annotation.
func TestArgoTrackingVerdicts(t *testing.T) {
	compared := func(ns string, objs ...argoObject) map[string]any {
		return argoApplicationListing(ns, objs...)
	}
	notCompared := func(edit func(status map[string]any)) map[string]any {
		app := compared("openshift-gitops")
		edit(app["status"].(map[string]any))
		return app
	}
	for name, tc := range map[string]struct {
		argo        *fakeArgoCD
		state       string
		staleReason string
		reason      string // in the unconfirmed reason
		namespace   string
		sync        string
	}{
		"managed": {
			argo:  argoManagingAll(argoSubscriptionObject),
			state: types.ArgoCDManaged, namespace: "openshift-gitops", sync: "Synced",
		},
		"managed by a second instance's application of the same name": {
			argo: &fakeArgoCD{served: true, apps: []map[string]any{
				compared("openshift-gitops", argoCatalogObject),
				compared("team-gitops", argoSubscriptionObject),
			}},
			state: types.ArgoCDManaged, namespace: "team-gitops", sync: "Synced",
		},
		"argo cd not installed": {
			argo:  &fakeArgoCD{},
			state: types.ArgoCDStale, staleReason: types.ArgoCDNotInstalled,
		},
		"only another argoproj.io API (Argo Rollouts)": {
			argo:  &fakeArgoCD{rolloutsOnly: true},
			state: types.ArgoCDStale, staleReason: types.ArgoCDNotInstalled,
		},
		"argo cd not installed, an ACM hub manages the cluster": {
			argo:  &fakeArgoCD{acm: true},
			state: types.ArgoCDUnconfirmed, reason: "ACM hub",
		},
		"argo cd not installed, argocd cluster add registered the cluster": {
			argo:  &fakeArgoCD{argocdManager: true},
			state: types.ArgoCDUnconfirmed, reason: "ClusterRoleBinding argocd-manager-role-binding",
		},
		"argo cd not installed, argocd cluster add --namespace registered the operator's namespace": {
			argo:  &fakeArgoCD{managerRoleBindings: []string{SubNS}},
			state: types.ArgoCDUnconfirmed, reason: "RoleBinding argocd-manager-role-binding in namespace " + SubNS,
		},
		"argo cd not installed, argocd cluster add --namespace registered the catalog's namespace": {
			argo:  &fakeArgoCD{managerRoleBindings: []string{CatalogNS}},
			state: types.ArgoCDUnconfirmed, reason: "RoleBinding argocd-manager-role-binding in namespace " + CatalogNS,
		},
		"argo cd not installed, argocd cluster add --namespace registered another namespace": {
			argo:  &fakeArgoCD{managerRoleBindings: []string{"team-x"}},
			state: types.ArgoCDStale, staleReason: types.ArgoCDNotInstalled,
		},
		"argo cd not installed, ACM discovery fails": {
			argo:  &fakeArgoCD{fail: map[string]int{acmWorkAPI: http.StatusServiceUnavailable}},
			state: types.ArgoCDUnconfirmed, reason: "whether an ACM hub manages this cluster could not be read",
		},
		"argo cd not installed, the binding list is forbidden": {
			argo:  &fakeArgoCD{fail: map[string]int{"/apis/rbac.authorization.k8s.io/v1/clusterrolebindings": http.StatusForbidden}},
			state: types.ArgoCDUnconfirmed, reason: "403",
		},
		"argo cd not installed, the RoleBinding read is forbidden": {
			argo:  &fakeArgoCD{fail: map[string]int{managerRoleBindingPath(SubNS): http.StatusForbidden}},
			state: types.ArgoCDUnconfirmed, reason: "manages namespace " + SubNS + " could not be read",
		},
		"application gone": {
			argo:  &fakeArgoCD{served: true},
			state: types.ArgoCDStale, staleReason: types.ArgoCDApplicationGone,
		},
		"application gone, an ACM hub manages the cluster": {
			argo:  &fakeArgoCD{served: true, acm: true},
			state: types.ArgoCDUnconfirmed, reason: `no application "` + testArgoApp + `" is on this cluster, but an ACM hub`,
		},
		"application does not list it": {
			argo:  &fakeArgoCD{served: true, apps: []map[string]any{compared("openshift-gitops", argoCatalogObject)}},
			state: types.ArgoCDStale, staleReason: types.ArgoCDNotAmongResources, namespace: "openshift-gitops",
		},
		"application does not list it, an ACM hub manages the cluster": {
			argo:  &fakeArgoCD{served: true, acm: true, apps: []map[string]any{compared("openshift-gitops", argoCatalogObject)}},
			state: types.ArgoCDUnconfirmed, reason: "application openshift-gitops/" + testArgoApp + " does not list it, but an ACM hub", namespace: "openshift-gitops",
		},
		"application does not list it, argocd cluster add registered the cluster": {
			argo:  &fakeArgoCD{served: true, argocdManager: true, apps: []map[string]any{compared("openshift-gitops", argoCatalogObject)}},
			state: types.ArgoCDUnconfirmed, reason: "ClusterRoleBinding argocd-manager-role-binding", namespace: "openshift-gitops",
		},
		"application lists it in another namespace": {
			argo: &fakeArgoCD{served: true, apps: []map[string]any{compared("openshift-gitops",
				argoObject{group: "operators.coreos.com", kind: "Subscription", namespace: "other", name: SubName})}},
			state: types.ArgoCDStale, staleReason: types.ArgoCDNotAmongResources, namespace: "openshift-gitops",
		},
		"application lists another group's object of that kind and name": {
			argo: &fakeArgoCD{served: true, apps: []map[string]any{compared("openshift-gitops",
				argoObject{group: "example.com", kind: "Subscription", namespace: SubNS, name: SubName})}},
			state: types.ArgoCDStale, staleReason: types.ArgoCDNotAmongResources, namespace: "openshift-gitops",
		},
		"application not compared yet": {
			argo: &fakeArgoCD{served: true, apps: []map[string]any{notCompared(func(s map[string]any) {
				delete(s, "reconciledAt")
				s["sync"] = map[string]any{}
			})}},
			state: types.ArgoCDUnconfirmed, reason: "has not compared application openshift-gitops/" + testArgoApp + " with Git yet", namespace: "openshift-gitops",
		},
		"application sync status Unknown": {
			argo: &fakeArgoCD{served: true, apps: []map[string]any{notCompared(func(s map[string]any) {
				s["sync"] = map[string]any{"status": "Unknown"}
			})}},
			state: types.ArgoCDUnconfirmed, reason: "has not compared", namespace: "openshift-gitops",
		},
		"application comparison failed": {
			argo: &fakeArgoCD{served: true, apps: []map[string]any{notCompared(func(s map[string]any) {
				s["conditions"] = []any{map[string]any{"type": "ComparisonError", "message": "rpc error: repository not accessible"}}
			})}},
			state: types.ArgoCDUnconfirmed, reason: "reports ComparisonError: rpc error: repository not accessible", namespace: "openshift-gitops",
		},
		"discovery fails": {
			argo:  &fakeArgoCD{fail: map[string]int{argoDiscoveryPath: http.StatusInternalServerError}},
			state: types.ArgoCDUnconfirmed, reason: "list of Argo CD APIs failed",
		},
		"applications forbidden": {
			argo:  &fakeArgoCD{served: true, fail: map[string]int{argoDiscoveryPath + "/applications": http.StatusForbidden}},
			state: types.ArgoCDUnconfirmed, reason: "listing Argo CD applications failed: k8s API error 403",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := newArgoCheck(argoClient(t, tc.argo)).object(subTracked(testArgoApp), nil, argoSubscriptionObject)
			if got.app != testArgoApp || got.state != tc.state || got.staleReason != tc.staleReason || got.namespace != tc.namespace || got.sync != tc.sync {
				t.Fatalf("verdict = %+v", got)
			}
			if !strings.Contains(got.reason, tc.reason) || (tc.reason == "") != (got.reason == "") {
				t.Fatalf("reason = %q, want it to contain %q", got.reason, tc.reason)
			}
			// Every refusal reads managedBy: only a left-over annotation
			// lets the tool change the object.
			if want := testArgoApp; tc.state == types.ArgoCDStale {
				if got.managedBy() != "" {
					t.Fatalf("stale tracking still managed by %q", got.managedBy())
				}
			} else if got.managedBy() != want {
				t.Fatalf("managedBy = %q, want %q", got.managedBy(), want)
			}
			if note := got.unconfirmedNote(); (tc.state == types.ArgoCDUnconfirmed) != (note != "") || !strings.Contains(note, tc.reason) {
				t.Fatalf("note = %q", note)
			}
			info := got.info()
			if info == nil || info.Application != testArgoApp || info.State != tc.state || info.StaleReason != tc.staleReason || info.Reason != got.reason {
				t.Fatalf("info = %+v", info)
			}
		})
	}
}

// Label tracking, OpenShift GitOps' default up to 1.18: the object carries
// only the instance label. Argo CD's status.resources decides; the label
// only names the application.
func TestArgoTrackingLabelTracking(t *testing.T) {
	for name, tc := range map[string]struct {
		argo      *fakeArgoCD
		state     string
		app       string // managedBy; "" when not managed
		reason    string
		namespace string
		reported  bool // info() is set
	}{
		"listed in an Application's status.resources, no annotation: managed": {
			argo:  argoManagingAll(argoSubscriptionObject),
			state: types.ArgoCDManaged, app: testArgoApp, namespace: "openshift-gitops", reported: true,
		},
		"Application in another namespace, label <namespace>_<name>": {
			argo:  &fakeArgoCD{served: true, apps: []map[string]any{argoApplicationListing("team-a", argoSubscriptionObject)}},
			state: types.ArgoCDManaged, app: testArgoApp, namespace: "team-a", reported: true,
		},
		"the named Application does not list it: left over": {
			argo:  &fakeArgoCD{served: true, apps: []map[string]any{argoApplicationListing("openshift-gitops", argoCatalogObject)}},
			state: types.ArgoCDStale, namespace: "openshift-gitops", reported: true,
		},
		"the named Application has not compared yet: not confirmed": {
			argo: &fakeArgoCD{served: true, apps: []map[string]any{{
				"metadata": map[string]any{"name": testArgoApp, "namespace": "openshift-gitops"},
				"status":   map[string]any{"sync": map[string]any{"status": "Unknown"}},
			}}},
			state: types.ArgoCDUnconfirmed, app: testArgoApp, reason: "has not compared", namespace: "openshift-gitops", reported: true,
		},
		"no Application of that name, nothing lists it: not Argo CD's to report": {
			argo: &fakeArgoCD{served: true},
		},
		"no Application of that name, an ACM hub manages the cluster: not confirmed": {
			argo:  &fakeArgoCD{served: true, acm: true},
			state: types.ArgoCDUnconfirmed, app: testArgoApp, reason: "ACM hub", reported: true,
		},
		"Argo CD not installed, a hub's Argo CD may manage the cluster: not confirmed": {
			argo:  &fakeArgoCD{acm: true},
			state: types.ArgoCDUnconfirmed, app: testArgoApp, reason: "Argo CD is not installed on this cluster, but an ACM hub", reported: true,
		},
		"Argo CD not installed, argocd cluster add --namespace on the operator's namespace: not confirmed": {
			argo:  &fakeArgoCD{managerRoleBindings: []string{SubNS}},
			state: types.ArgoCDUnconfirmed, app: testArgoApp, reason: "RoleBinding argocd-manager-role-binding in namespace " + SubNS, reported: true,
		},
		"Argo CD not installed, no remote sign: not reported (Helm sets the label too)": {
			argo: &fakeArgoCD{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			label := testArgoApp
			if strings.Contains(name, "<namespace>_<name>") {
				label = "team-a_" + testArgoApp
			}
			got := newArgoCheck(argoClient(t, tc.argo)).object(nil, subLabelled(label), argoSubscriptionObject)
			wantApp := tc.app
			if wantApp != "" {
				wantApp = label
			}
			if got.state != tc.state || got.managedBy() != wantApp || got.namespace != tc.namespace || !strings.Contains(got.reason, tc.reason) {
				t.Fatalf("verdict = %+v, managedBy %q", got, got.managedBy())
			}
			if (got.info() != nil) != tc.reported {
				t.Fatalf("info = %+v", got.info())
			}
		})
	}
}

// An object without any mark is still Argo CD's when an Application lists
// it (Git holds it, Argo CD has not marked it yet, or application.
// instanceLabelKey names another label); Argo CD unknown or unreadable
// keeps it managed.
func TestArgoTrackingUnmarkedObject(t *testing.T) {
	t.Run("listed: managed, named by the Application", func(t *testing.T) {
		got := newArgoCheck(argoClient(t, argoManagingAll(argoSubscriptionObject))).object(nil, nil, argoSubscriptionObject)
		if got.state != types.ArgoCDManaged || got.managedBy() != testArgoApp || got.namespace != "openshift-gitops" {
			t.Fatalf("verdict = %+v", got)
		}
	})
	t.Run("not listed: not managed, nothing reported", func(t *testing.T) {
		f := &fakeArgoCD{served: true, acm: true, apps: []map[string]any{argoApplicationListing("openshift-gitops", argoCatalogObject)}}
		got := newArgoCheck(argoClient(t, f)).object(nil, nil, argoSubscriptionObject)
		if got.managedBy() != "" || got.info() != nil {
			t.Fatalf("verdict = %+v", got)
		}
		// Unmarked objects do not look for remote signs.
		for _, p := range f.requested() {
			if strings.Contains(p, "rbac") || strings.Contains(p, "open-cluster-management") {
				t.Fatalf("asked %s", p)
			}
		}
	})
	t.Run("Argo CD not installed: only discovery is asked", func(t *testing.T) {
		f := &fakeArgoCD{acm: true}
		got := newArgoCheck(argoClient(t, f)).object(nil, nil, argoSubscriptionObject)
		if got.managedBy() != "" || got.info() != nil {
			t.Fatalf("verdict = %+v", got)
		}
		if reqs := f.requested(); len(reqs) != 1 || reqs[0] != argoDiscoveryPath {
			t.Fatalf("requests = %v", reqs)
		}
	})
	for name, f := range map[string]*fakeArgoCD{
		"the list is forbidden": {served: true, fail: map[string]int{argoDiscoveryPath + "/applications": http.StatusForbidden}},
		"discovery fails":       {fail: map[string]int{argoDiscoveryPath: http.StatusInternalServerError}},
	} {
		t.Run(name+": not confirmed, application unknown", func(t *testing.T) {
			got := newArgoCheck(argoClient(t, f)).object(nil, nil, argoSubscriptionObject)
			if got.state != types.ArgoCDUnconfirmed || got.managedBy() != argoUnknownApplication || got.reason == "" {
				t.Fatalf("verdict = %+v", got)
			}
			if info := got.info(); info == nil || info.Application != argoUnknownApplication {
				t.Fatalf("info = %+v", info)
			}
		})
	}
}

// An Application that lists the object decides, even when the annotation
// names another application (an earlier one, or a copy).
func TestArgoTrackingListedByAnotherApplication(t *testing.T) {
	other := argoApplicationListing("openshift-gitops", argoSubscriptionObject)
	other["metadata"].(map[string]any)["name"] = "rhoai-operator"
	f := &fakeArgoCD{served: true, apps: []map[string]any{argoApplicationListing("openshift-gitops", argoCatalogObject), other}}
	got := newArgoCheck(argoClient(t, f)).object(subTracked(testArgoApp), nil, argoSubscriptionObject)
	if got.state != types.ArgoCDManaged || got.managedBy() != "rhoai-operator" || got.namespace != "openshift-gitops" {
		t.Fatalf("verdict = %+v", got)
	}
}

// A tracking annotation copied from another object (it names another
// object, or the same kind and name in another group): Argo CD lists the
// object without a sync status and neither syncs nor prunes it
// (controller/state.go, isSelfReferencedObj), unless Git holds it.
func TestArgoTrackingCopiedAnnotation(t *testing.T) {
	listing := func(status string) *fakeArgoCD {
		app := argoApplicationListing("openshift-gitops", argoSubscriptionObject)
		res := app["status"].(map[string]any)["resources"].([]any)[0].(map[string]any)
		if status == "" {
			delete(res, "status")
		} else {
			res["status"] = status
		}
		return &fakeArgoCD{served: true, apps: []map[string]any{app}}
	}
	for _, copied := range []string{
		testArgoApp + ":operators.coreos.com/CatalogSource:" + CatalogNS + "/" + CatalogName,
		testArgoApp + ":example.com/Subscription:" + SubNS + "/" + SubName,
	} {
		ann := map[string]string{argoTrackingAnnotation: copied}
		if got := newArgoCheck(argoClient(t, listing(""))).object(ann, nil, argoSubscriptionObject); got.managedBy() != "" || got.info() != nil {
			t.Errorf("%s, listed without status: %+v", copied, got)
		}
		if got := newArgoCheck(argoClient(t, listing("OutOfSync"))).object(ann, nil, argoSubscriptionObject); got.state != types.ArgoCDManaged {
			t.Errorf("%s, in Git (OutOfSync): %+v", copied, got)
		}
	}
	// Without a copied annotation, an entry without a status still counts.
	if got := newArgoCheck(argoClient(t, listing(""))).object(subTracked(testArgoApp), nil, argoSubscriptionObject); got.state != types.ArgoCDManaged {
		t.Errorf("own annotation, listed without status: %+v", got)
	}
}

// An application outside Argo CD's own namespace: the mark names
// "<namespace>_<name>", and only that namespace's Application matches it.
func TestArgoTrackingNamespacedApplication(t *testing.T) {
	f := &fakeArgoCD{served: true, apps: []map[string]any{
		argoApplicationListing("team-a", argoSubscriptionObject),
		// Same name in Argo CD's own namespace, not listing it.
		argoApplicationListing("openshift-gitops"),
	}}
	got := newArgoCheck(argoClient(t, f)).object(subTracked("team-a_"+testArgoApp), nil, argoSubscriptionObject)
	if got.state != types.ArgoCDManaged || got.namespace != "team-a" || got.managedBy() != "team-a_"+testArgoApp {
		t.Fatalf("verdict = %+v", got)
	}
	if reqs := f.requested(); len(reqs) != 2 || reqs[1] != argoDiscoveryPath+"/applications" {
		t.Fatalf("requests = %v, want discovery then the list of every Application", reqs)
	}

	// Gone from its namespace (another namespace's same-named application
	// does not count).
	f.setApps(argoApplicationListing("openshift-gitops"))
	got = newArgoCheck(argoClient(t, f)).object(subTracked("team-a_"+testArgoApp), nil, argoSubscriptionObject)
	if got.state != types.ArgoCDStale || got.staleReason != types.ArgoCDApplicationGone {
		t.Fatalf("gone: %+v", got)
	}
}

// Label tracking cuts the instance label to 63 characters
// (util/argo/resource_tracking.go); the cut value still names the
// Application.
func TestArgoTrackingTruncatedLabel(t *testing.T) {
	long := strings.Repeat("a", 70)
	app := argoApplicationListing("openshift-gitops")
	app["metadata"].(map[string]any)["name"] = long
	app["status"].(map[string]any)["sync"] = map[string]any{"status": "Unknown"}
	got := newArgoCheck(argoClient(t, &fakeArgoCD{served: true, apps: []map[string]any{app}})).object(nil, subLabelled(long[:63]), argoSubscriptionObject)
	if got.state != types.ArgoCDUnconfirmed || got.namespace != "openshift-gitops" {
		t.Fatalf("verdict = %+v", got)
	}
}

// A mark that is not an application name never goes into a request; Argo
// CD's list decides as for any other.
func TestArgoTrackingInvalidApplicationName(t *testing.T) {
	for _, app := range []string{"Upper", "a/b", "a%2Fb", "ns_Bad", "x_"} {
		f := argoManagingAll(argoSubscriptionObject)
		got := newArgoCheck(argoClient(t, f)).object(subTracked(app), nil, argoSubscriptionObject)
		if got.state != types.ArgoCDManaged || got.managedBy() != testArgoApp {
			t.Errorf("%q, listed: verdict = %+v", app, got)
		}
		for _, p := range f.requested() {
			if strings.Contains(p, app) {
				t.Errorf("%q: put into request %s", app, p)
			}
		}
		f = &fakeArgoCD{served: true}
		if got := newArgoCheck(argoClient(t, f)).object(subTracked(app), nil, argoSubscriptionObject); got.state != types.ArgoCDStale || got.staleReason != types.ArgoCDApplicationGone {
			t.Errorf("%q, not listed: verdict = %+v", app, got)
		}
	}
}

// The DataScienceCluster is cluster-scoped: Argo CD writes its destination
// namespace in the tracking id and none in status.resources.
func TestArgoTrackingClusterScopedDSC(t *testing.T) {
	dsc := map[string]any{"metadata": map[string]any{"name": "default-dsc", "annotations": map[string]any{
		argoTrackingAnnotation: testArgoApp + ":datasciencecluster.opendatahub.io/DataScienceCluster:redhat-ods-applications/default-dsc"}}}
	f := argoManagingAll(argoDSCObject("default-dsc"))
	if got := argoTrackingOf(argoClient(t, f), dsc, dscGroup, "DataScienceCluster"); got.state != types.ArgoCDManaged {
		t.Fatalf("verdict = %+v", got)
	}
	f = argoManagingAll(argoDSCObject("other-dsc"))
	if got := argoTrackingOf(argoClient(t, f), dsc, dscGroup, "DataScienceCluster"); got.state != types.ArgoCDStale || got.staleReason != types.ArgoCDNotAmongResources {
		t.Fatalf("another DSC listed: %+v", got)
	}
	untracked := map[string]any{"metadata": map[string]any{"name": "default-dsc"}}
	if got := argoTrackingOf(argoClient(t, f), untracked, dscGroup, "DataScienceCluster"); got.app != "" || got.managedBy() != "" || got.info() != nil {
		t.Fatalf("untracked, not listed: %+v", got)
	}
	labelled := map[string]any{"metadata": map[string]any{"name": "default-dsc", "labels": map[string]any{argoInstanceLabel: testArgoApp}}}
	if got := argoTrackingOf(argoClient(t, argoManagingAll(argoDSCObject("default-dsc"))), labelled, dscGroup, "DataScienceCluster"); got.state != types.ArgoCDManaged || got.managedBy() != testArgoApp {
		t.Fatalf("label tracking: %+v", got)
	}
}

// One check reads each answer once: three objects, and the remote check,
// cost one request each.
func TestArgoTrackingReadsOncePerCheck(t *testing.T) {
	f := &fakeArgoCD{}
	argo := newArgoCheck(argoClient(t, f))
	dsc := map[string]string{argoTrackingAnnotation: testArgoApp + ":datasciencecluster.opendatahub.io/DataScienceCluster:x/default-dsc"}
	cat := map[string]string{argoTrackingAnnotation: testArgoApp + ":operators.coreos.com/CatalogSource:" + CatalogNS + "/" + CatalogName}
	for _, got := range []argoTracking{
		argo.object(subTracked(testArgoApp), nil, argoSubscriptionObject),
		argo.object(cat, nil, argoCatalogObject),
		argo.object(dsc, nil, argoDSCObject("default-dsc")),
	} {
		if got.state != types.ArgoCDStale || got.staleReason != types.ArgoCDNotInstalled {
			t.Fatalf("verdict = %+v", got)
		}
	}
	want := []string{argoDiscoveryPath, acmWorkAPI, "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings?fieldSelector=metadata.name%3D" + argoCDManagerBinding,
		managerRoleBindingPath(SubNS), managerRoleBindingPath(CatalogNS)}
	if reqs := f.requested(); strings.Join(reqs, " ") != strings.Join(want, " ") {
		t.Fatalf("requests = %v, want %v", reqs, want)
	}

	// Served: discovery and one list of every Application for all three.
	f = argoManagingAll(argoGitOpsObjects...)
	argo = newArgoCheck(argoClient(t, f))
	argo.object(subTracked(testArgoApp), nil, argoSubscriptionObject)
	argo.object(nil, nil, argoCatalogObject)
	argo.object(dsc, nil, argoDSCObject("default-dsc"))
	if reqs := f.requested(); len(reqs) != 2 {
		t.Fatalf("requests = %v, want discovery and one list", reqs)
	}
}

// The RoleBinding of `argocd cluster add --namespace` in the object's own
// namespace (another operator's Subscription) is a remote sign too.
func TestArgoTrackingRemoteSignInTheObjectsNamespace(t *testing.T) {
	o := argoObject{group: "operators.coreos.com", kind: "Subscription", namespace: "kuadrant-system", name: "rhcl-operator"}
	ann := map[string]string{argoTrackingAnnotation: testArgoApp + ":operators.coreos.com/Subscription:kuadrant-system/rhcl-operator"}
	f := &fakeArgoCD{managerRoleBindings: []string{"kuadrant-system"}}
	got := newArgoCheck(argoClient(t, f)).object(ann, nil, o)
	if got.state != types.ArgoCDUnconfirmed || !strings.Contains(got.reason, "namespace kuadrant-system") {
		t.Fatalf("verdict = %+v", got)
	}
}

// Status: a left-over annotation is reported (no manager, the verdict for
// the note); one Argo CD cannot confirm keeps the manager, with the reason.
func TestStatusReportsLeftOverArgoCDTracking(t *testing.T) {
	f := gitOpsFake(t)
	f.argo = &fakeArgoCD{served: true}
	s := statusManagersOf(t, f.client(context.Background()))
	if s.Subscription.ManagedBy != "" || s.CatalogSource.ManagedBy != "" || s.CatalogSource.ManagedByACM {
		t.Fatalf("stale tracking counted as managed: %+v %+v", s.Subscription, s.CatalogSource)
	}
	want := types.ArgoCDTracking{Application: testArgoApp, State: types.ArgoCDStale, StaleReason: types.ArgoCDApplicationGone}
	if s.Subscription.ArgoCD == nil || *s.Subscription.ArgoCD != want {
		t.Fatalf("argoCD = %+v", s.Subscription.ArgoCD)
	}

	f.argo = &fakeArgoCD{served: true, fail: map[string]int{argoDiscoveryPath + "/applications": http.StatusForbidden}}
	s = statusManagersOf(t, f.client(context.Background()))
	if s.Subscription.ManagedBy != testArgoApp || s.Subscription.ArgoCD == nil || s.Subscription.ArgoCD.State != types.ArgoCDUnconfirmed || !strings.Contains(s.Subscription.ArgoCD.Reason, "403") {
		t.Fatalf("unconfirmed: %+v", s.Subscription)
	}
}

// Re-deploy and Install a different build run when the annotation is left
// over, and stay refused, saying why, when Argo CD cannot be asked.
func TestOperatorOperationsWithLeftOverArgoCDTracking(t *testing.T) {
	redeploy := func(c *Client) *types.OperationResponse {
		r, _ := RefreshOperatorStreamWithOptions(c, OperationOptions{}, func(UpdateStepEvent) {})
		return r
	}
	f := gitOpsFake(t)
	f.argo = &fakeArgoCD{served: true, apps: []map[string]any{argoApplicationListing("openshift-gitops")}}
	if r := redeploy(f.client(context.Background())); r.ErrorCode == ErrorCodeGitOpsManaged {
		t.Fatalf("refused for a left-over annotation: %+v", r)
	}
	g, err := readOperatorGitOps(f.client(context.Background()))
	if err != nil || g.active() || g.catalogArgo.staleReason != types.ArgoCDNotAmongResources {
		t.Fatalf("gitOps = %+v, %v", g, err)
	}

	f = gitOpsFake(t)
	f.argo = &fakeArgoCD{fail: map[string]int{argoDiscoveryPath: http.StatusServiceUnavailable}}
	r := redeploy(f.client(context.Background()))
	if r.ErrorCode != ErrorCodeGitOpsManaged || !strings.Contains(r.Message, "could not confirm this with Argo CD") || !strings.Contains(r.Message, "503") {
		t.Fatalf("unconfirmed: %+v", r)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

// Label tracking (OpenShift GitOps up to 1.18): an operator Subscription
// and catalog without the tracking annotation, listed by the Application,
// refuse Re-deploy as under annotation tracking.
func TestOperatorOperationsUnderLabelTracking(t *testing.T) {
	f := gitOpsFake(t)
	for _, obj := range []map[string]any{f.sub, f.catalog} {
		meta := obj["metadata"].(map[string]any)
		delete(meta["annotations"].(map[string]any), argoTrackingAnnotation)
		labels, _ := meta["labels"].(map[string]any)
		if labels == nil {
			labels = map[string]any{}
			meta["labels"] = labels
		}
		labels[argoInstanceLabel] = testArgoApp
	}
	r, _ := RefreshOperatorStreamWithOptions(f.client(context.Background()), OperationOptions{}, func(UpdateStepEvent) {})
	if r.ErrorCode != ErrorCodeGitOpsManaged || !strings.Contains(r.Message, `Argo CD application "`+testArgoApp+`"`) {
		t.Fatalf("result = %+v", r)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

// Update with a left-over annotation is the plain Update: it may install
// any build (it writes the catalog), not only the one Git's catalog serves.
func TestUpdateWithLeftOverArgoCDTracking(t *testing.T) {
	oldLookup := lookupTargetBundle
	t.Cleanup(func() { lookupTargetBundle = oldLookup })
	lookupTargetBundle = func(*Client, string) (string, error) { return gitOpsCSV, nil }
	f := gitOpsFake(t)
	f.argo = &fakeArgoCD{}
	result, err := Update(f.client(context.Background()), testNightlyImage, true)
	if err != nil || result.ErrorCode == ErrorCodeGitOpsManaged {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if logs := strings.Join(result.Logs, "\n"); !result.Success || strings.Contains(logs, "as Git sets it") || !strings.Contains(logs, "Would delete and recreate Subscription") {
		t.Fatalf("dry run describes the Argo CD flow:\n%s", logs)
	}
}
