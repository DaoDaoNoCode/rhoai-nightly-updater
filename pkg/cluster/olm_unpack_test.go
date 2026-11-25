package cluster

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/catalog"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/operator"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

const testBundleImage = "registry.redhat.io/rhoai/odh-operator-bundle@sha256:0123abcd"

// unpackingSubscription is the status OLM 4.22 gives a Subscription while
// its bundle unpack Job runs: BundleUnpacking True with no message
// (operator-framework-olm release-4.22, catalog/operator.go:1423-1440), and
// no InstallPlan or CSV yet.
func unpackingSubscription(f *fakeOLM) {
	f.sub["status"] = map[string]any{"conditions": []any{
		map[string]any{"type": "CatalogSourcesUnhealthy", "status": "False", "reason": "AllCatalogSourcesHealthy", "message": "all available catalogsources are healthy"},
		map[string]any{"type": "BundleUnpacking", "status": "True", "reason": "UnpackingInProgress"},
	}}
}

// unpackPodsPullFailing is OLM's unpack pods as the field case had them:
// one Pending pod whose "pull" init container (the bundle image) cannot
// be pulled, a second one with the same failure, one whose containers are
// only starting, and an older pod that already finished.
const unpackPodsPullFailing = `{"items":[
 {"metadata":{"name":"a1"},"status":{"phase":"Pending","initContainerStatuses":[
   {"name":"util","image":"registry.redhat.io/openshift4/ose-operator-framework-tools-rhel9@sha256:1","ready":true,"state":{"terminated":{"reason":"Completed"}}},
   {"name":"pull","image":"` + testBundleImage + `","ready":false,"state":{"waiting":{"reason":"ImagePullBackOff","message":"Back-off pulling image \"` + testBundleImage + `\": manifest unknown"}}}]}},
 {"metadata":{"name":"a2"},"status":{"phase":"Pending","initContainerStatuses":[
   {"name":"pull","image":"` + testBundleImage + `","ready":false,"state":{"waiting":{"reason":"ImagePullBackOff","message":"Back-off pulling image \"` + testBundleImage + `\": manifest unknown"}}}]}},
 {"metadata":{"name":"b1"},"status":{"phase":"Pending","initContainerStatuses":[
   {"name":"pull","image":"registry.redhat.io/rhoai/other-bundle@sha256:9","ready":false,"state":{"waiting":{"reason":"PodInitializing"}}}]}},
 {"metadata":{"name":"c1"},"status":{"phase":"Failed","initContainerStatuses":[
   {"name":"pull","image":"registry.redhat.io/rhoai/old-bundle@sha256:8","ready":false,"state":{"waiting":{"reason":"ErrImagePull","message":"old"}}}]}}
]}`

// unpackPodLists counts the reads of OLM's bundle-unpack pods.
func unpackPodLists(f *fakeOLM) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.Method == http.MethodGet && strings.HasSuffix(r.Path, "/namespaces/"+rhoai.CatalogNS+"/pods") && r.Query.Get("labelSelector") == catalog.BundleUnpackRefLabel {
			n++
		}
	}
	return n
}

// The field case on OLM 4.22: no InstallPlan, no CSV, no failure condition,
// while the unpack pod cannot pull the bundle. The wait names the
// Subscription's conditions and the pull error, and its give-up message says
// that OLM was still unpacking and what to check.
func TestUpdateStream_ReportsTheBundleUnpackPullFailure(t *testing.T) {
	const newCSV = "rhods-operator.3.6.1"
	f := newFakeOLM(t).installed("rhods-operator.3.6.0", map[string]any{"channel": "fast"})
	f.channels = `[{"name":"stable-3.x","currentCSV":"` + newCSV + `"}]`
	f.unpackPods = unpackPodsPullFailing
	f.onSubscribe = func(f *fakeOLM, spec map[string]any) {
		if spec["channel"] != "fast" { // the restored Subscription is not the attempt
			unpackingSubscription(f)
		}
	}
	emit, events := eventsRecorder()
	result, err := operator.UpdateStream(f.client(context.Background()), testNightlyImage, emit)
	if err != nil || result == nil || result.Success || result.ErrorCode != "install_timeout" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	wantState := "OLM is unpacking the operator bundle; Subscription conditions: CatalogSourcesUnhealthy=False (AllCatalogSourcesHealthy), BundleUnpacking=True (UnpackingInProgress). " +
		"OLM could not pull the operator bundle " + testBundleImage + `: ImagePullBackOff (Back-off pulling image "` + testBundleImage + `": manifest unknown)`
	for _, want := range []string{
		"last state: " + wantState,
		"OLM was still unpacking the operator bundle",
		"10m0s unless the OperatorGroup in " + rhoai.SubNS + " sets operatorframework.io/bundle-unpack-timeout",
		"oc get pods -n " + rhoai.CatalogNS + " -l " + catalog.BundleUnpackRefLabel,
	} {
		if !strings.Contains(result.Message, want) {
			t.Fatalf("message lacks %q:\n%s", want, result.Message)
		}
	}
	if n := strings.Count(result.Message, "OLM could not pull"); n != 1 {
		t.Fatalf("%d pull failures reported, want the one image once:\n%s", n, result.Message)
	}
	for _, unwanted := range []string{"PodInitializing", "old-bundle", "other-bundle", "kept because OLM is still installing"} {
		if strings.Contains(result.Message, unwanted) {
			t.Fatalf("message reports %q:\n%s", unwanted, result.Message)
		}
	}
	shown := false
	for _, e := range events() {
		shown = shown || e.Step == "verify_installplan" && e.Status == "running" && e.Message == wantState
	}
	if !shown {
		t.Fatalf("the wait did not show the state while it ran: %v", events())
	}
	if lastStatus(events(), operator.RestoreStepName) != "success" {
		t.Fatalf("no InstallPlan yet: the previous operator must be restored: %v", events())
	}
}

// Under Argo CD, Update waits with the same WaitForInstall: the same
// evidence reaches its result.
func TestUpdateUnderGitOps_ReportsTheBundleUnpackPullFailure(t *testing.T) {
	f := gitOpsFake(t)
	f.unpackPods = unpackPodsPullFailing
	f.onSubscribe = func(f *fakeOLM, _ map[string]any) { unpackingSubscription(f) }
	result, err := operator.UpdateStream(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
	if err != nil || result.Success || result.ErrorCode != "install_timeout" ||
		!strings.Contains(result.Message, "OLM could not pull the operator bundle "+testBundleImage+": ImagePullBackOff") ||
		!strings.Contains(result.Message, "OLM was still unpacking the operator bundle") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

// Before OLM unpacks anything the wait reports the conditions it set and
// does not read the unpack pods.
func TestWaitForInstall_ReportsConditionsWhileResolving(t *testing.T) {
	f := newFakeOLM(t)
	f.unpackPods = unpackPodsPullFailing
	f.sub = map[string]any{"metadata": map[string]any{"name": rhoai.SubName}, "spec": map[string]any{}, "status": map[string]any{"conditions": []any{
		cond("CatalogSourcesUnhealthy", "UnhealthyCatalogSourceFound", "targeted catalogsource openshift-marketplace/rhoai-catalog-dev unhealthy"),
	}}}
	var logs []string
	out := operator.WaitForInstall(f.client(context.Background()), func(UpdateStepEvent) {}, &logs, &operator.Recovery{}, "")
	want := "last state: Waiting for OLM to resolve the Subscription; Subscription conditions: CatalogSourcesUnhealthy=True (UnhealthyCatalogSourceFound: targeted catalogsource openshift-marketplace/rhoai-catalog-dev unhealthy))."
	if out.ErrorCode != "install_timeout" || !strings.Contains(out.Message, want) || strings.Contains(out.Message, "unpacking") {
		t.Fatalf("outcome = %+v", out)
	}
	if n := unpackPodLists(f); n != 0 {
		t.Fatalf("the unpack pods were read %d times before OLM unpacked anything", n)
	}
}

// Unreadable unpack pods are said, not hidden.
func TestWaitForInstall_SaysWhenUnpackPodsCannotBeRead(t *testing.T) {
	f := newFakeOLM(t)
	f.sub = map[string]any{"metadata": map[string]any{"name": rhoai.SubName}, "spec": map[string]any{}, "status": map[string]any{}}
	unpackingSubscription(f)
	f.intercept = func(_ *fakeOLM, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("labelSelector") != catalog.BundleUnpackRefLabel {
			return false
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","status":"Failure","reason":"Forbidden","message":"pods is forbidden","code":403}`))
		return true
	}
	var logs []string
	out := operator.WaitForInstall(f.client(context.Background()), func(UpdateStepEvent) {}, &logs, &operator.Recovery{}, "")
	if out.ErrorCode != "install_timeout" || !strings.Contains(out.Message, "OLM's bundle unpack pods in "+rhoai.CatalogNS+" could not be read") ||
		!strings.Contains(out.Message, "OLM was still unpacking the operator bundle") {
		t.Fatalf("outcome = %+v", out)
	}
	if unpackPodLists(f) == 0 {
		t.Fatal("the unpack pods were not read")
	}
}
