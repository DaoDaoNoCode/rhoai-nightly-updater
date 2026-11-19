package cluster

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func argoClient(t *testing.T, f *fakeArgoCD) *Client {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: context.Background()}
}

// subTracked is the Subscription's annotations with a tracking id naming app.
func subTracked(app string) map[string]string {
	return map[string]string{argoTrackingAnnotation: app + ":operators.coreos.com/Subscription:" + SubNS + "/" + SubName}
}

// Each answer of Argo CD (or its absence) and the verdict on the
// Subscription's tracking annotation.
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
			state: types.ArgoCDUnconfirmed, reason: "argocd-manager-role-binding",
		},
		"argo cd not installed, ACM discovery fails": {
			argo:  &fakeArgoCD{fail: map[string]int{acmWorkAPI: http.StatusServiceUnavailable}},
			state: types.ArgoCDUnconfirmed, reason: "whether an ACM hub manages this cluster could not be read",
		},
		"argo cd not installed, the binding list is forbidden": {
			argo:  &fakeArgoCD{fail: map[string]int{"/apis/rbac.authorization.k8s.io/v1/clusterrolebindings": http.StatusForbidden}},
			state: types.ArgoCDUnconfirmed, reason: "403",
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
		"application lists it in another namespace": {
			argo: &fakeArgoCD{served: true, apps: []map[string]any{compared("openshift-gitops",
				argoObject{group: "operators.coreos.com", kind: "Subscription", namespace: "other", name: SubName})}},
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
			state: types.ArgoCDUnconfirmed, reason: `reading Argo CD application "` + testArgoApp + `" failed: k8s API error 403`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := newArgoCheck(argoClient(t, tc.argo)).object(subTracked(testArgoApp), argoSubscriptionObject)
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

// An application outside Argo CD's own namespace: the tracking id names
// "<namespace>_<name>", and only that namespace is asked.
func TestArgoTrackingNamespacedApplication(t *testing.T) {
	f := &fakeArgoCD{served: true, apps: []map[string]any{
		argoApplicationListing("team-a", argoSubscriptionObject),
		// Same name in Argo CD's own namespace, not listing it: not asked.
		argoApplicationListing("openshift-gitops"),
	}}
	got := newArgoCheck(argoClient(t, f)).object(subTracked("team-a_"+testArgoApp), argoSubscriptionObject)
	if got.state != types.ArgoCDManaged || got.namespace != "team-a" || got.managedBy() != "team-a_"+testArgoApp {
		t.Fatalf("verdict = %+v", got)
	}
	want := argoDiscoveryPath + "/namespaces/team-a/applications?fieldSelector=metadata.name%3D" + testArgoApp
	if reqs := f.requested(); len(reqs) != 2 || reqs[1] != want {
		t.Fatalf("requests = %v, want discovery then %s", reqs, want)
	}

	// Gone from its namespace (another namespace's same-named application
	// does not count).
	f.apps = f.apps[1:]
	got = newArgoCheck(argoClient(t, f)).object(subTracked("team-a_"+testArgoApp), argoSubscriptionObject)
	if got.state != types.ArgoCDStale || got.staleReason != types.ArgoCDApplicationGone {
		t.Fatalf("gone: %+v", got)
	}
}

// A name the API server would not accept is not Argo CD's, and is not put
// into a request path: the object stays managed.
func TestArgoTrackingInvalidApplicationName(t *testing.T) {
	for _, app := range []string{"Upper", "a/b", "a%2Fb", "ns_Bad", "x_"} {
		f := argoManagingAll(argoSubscriptionObject)
		got := newArgoCheck(argoClient(t, f)).object(subTracked(app), argoSubscriptionObject)
		if got.app == "" {
			continue // argoApplication refused the id itself
		}
		if got.state != types.ArgoCDUnconfirmed || got.managedBy() == "" {
			t.Errorf("%q: verdict = %+v", app, got)
		}
		if reqs := f.requested(); len(reqs) != 0 {
			t.Errorf("%q: requests = %v", app, reqs)
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
		t.Fatalf("untracked: %+v", got)
	}
	if reqs := f.requested(); len(reqs) != 2 {
		t.Fatalf("an untracked object asked Argo CD: %v", reqs)
	}
}

// One check reads each answer once: three objects of one application, and
// the remote check, cost one request each.
func TestArgoTrackingReadsOncePerCheck(t *testing.T) {
	f := &fakeArgoCD{}
	argo := newArgoCheck(argoClient(t, f))
	dsc := map[string]string{argoTrackingAnnotation: testArgoApp + ":datasciencecluster.opendatahub.io/DataScienceCluster:x/default-dsc"}
	cat := map[string]string{argoTrackingAnnotation: testArgoApp + ":operators.coreos.com/CatalogSource:" + CatalogNS + "/" + CatalogName}
	for _, got := range []argoTracking{
		argo.object(subTracked(testArgoApp), argoSubscriptionObject),
		argo.object(cat, argoCatalogObject),
		argo.object(dsc, argoDSCObject("default-dsc")),
	} {
		if got.state != types.ArgoCDStale || got.staleReason != types.ArgoCDNotInstalled {
			t.Fatalf("verdict = %+v", got)
		}
	}
	if reqs := f.requested(); len(reqs) != 3 {
		t.Fatalf("requests = %v, want discovery, ACM discovery, the binding list", reqs)
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
