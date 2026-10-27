package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	testArgoApp = "rhai-on-openshift"
	// The Subscription Git sets and the build it installs: an EA build on
	// beta, while stable-3.x has a newer GA build that Update would pick on
	// a cluster without Argo CD.
	gitOpsChannel = "beta"
	gitOpsCSV     = "rhods-operator.3.6.0-ea.1"
	// The catalog image Git sets (fakeOLM.installed), and the build it
	// serves: a newer build of the installed CSV's version.
	gitOpsImage         = "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:old"
	gitOpsOperatorImage = "registry.redhat.io/rhoai/odh-rhel9-operator@sha256:new"
	gitOpsBuilt         = "2026-10-08T04:00:00Z"
)

func trackingID(kind, ns, name string) map[string]interface{} {
	return map[string]interface{}{argoTrackingAnnotation: fmt.Sprintf("%s:operators.coreos.com/%s:%s/%s", testArgoApp, kind, ns, name)}
}

// gitOpsFake is a cluster where Argo CD manages the nightly catalog and the
// Subscription, as the odh-gitops chart renders them.
func gitOpsFake(t *testing.T) *fakeOLM {
	f := newFakeOLM(t).installed(gitOpsCSV, map[string]interface{}{
		"channel": gitOpsChannel,
		"config":  map[string]interface{}{"env": []interface{}{map[string]interface{}{"name": "DISABLE_DSC_CONFIG", "value": "true"}}},
	})
	subAnn := trackingID("Subscription", SubNS, SubName)
	subAnn[lastAppliedAnnotation] = `{"kind":"Subscription","spec":{"channel":"beta"}}`
	f.sub["metadata"].(map[string]interface{})["annotations"] = subAnn
	f.sub["metadata"].(map[string]interface{})["labels"] = map[string]interface{}{"helm.sh/chart": "rhai-on-openshift-chart-3.4.0", "app.kubernetes.io/managed-by": "Helm"}
	f.catalog["metadata"] = map[string]interface{}{"annotations": trackingID("CatalogSource", CatalogNS, CatalogName)}
	f.catalog["spec"].(map[string]interface{})["displayName"] = "Red Hat OpenShift AI (nightly)"
	f.gitOpsPods = true
	f.channels = `[{"name":"beta","currentCSV":"` + gitOpsCSV + `","currentCSVDesc":{"annotations":{"containerImage":"` + gitOpsOperatorImage + `","createdAt":"` + gitOpsBuilt + `"}}},` +
		`{"name":"stable-3.x","currentCSV":"rhods-operator.3.6.0"}]`
	// What OLM installs from this catalog carries the same build identity.
	f.csvAnnotations = map[string]interface{}{"containerImage": gitOpsOperatorImage, "createdAt": gitOpsBuilt}
	f.onSubscribe = olmInstalls(gitOpsCSV)
	return f
}

func indexOf(list []string, want string) int {
	for i, s := range list {
		if s == want {
			return i
		}
	}
	return -1
}

func TestArgoApplication(t *testing.T) {
	sub := func(id string) string {
		return argoApplication(map[string]string{argoTrackingAnnotation: id}, "Subscription", SubNS, SubName)
	}
	for id, want := range map[string]string{
		"rhai-on-openshift:operators.coreos.com/Subscription:redhat-ods-operator/rhods-operator": "rhai-on-openshift",
		"team-a_rhoai:operators.coreos.com/Subscription:redhat-ods-operator/rhods-operator":      "team-a_rhoai",
		// Copied from another object (Argo CD ignores such annotations too).
		"rhai-on-openshift:operators.coreos.com/CatalogSource:openshift-marketplace/rhoai-catalog-dev": "",
		"rhai-on-openshift:operators.coreos.com/Subscription:other-ns/rhods-operator":                  "",
		"rhai-on-openshift:operators.coreos.com/Subscription:redhat-ods-operator/other":                "",
		"":         "",
		"no-colon": "",
		"app:only": "",
	} {
		if got := sub(id); got != want {
			t.Errorf("argoApplication(%q) = %q, want %q", id, got, want)
		}
	}
	if got := argoApplication(map[string]string{"app.kubernetes.io/instance": "rhai-on-openshift"}, "Subscription", SubNS, SubName); got != "" {
		t.Errorf("the instance label alone must not count as Argo CD tracking, got %q", got)
	}
	// A cluster-scoped object carries the application's destination
	// namespace in the id; only kind and name are checked.
	if got := argoApplication(map[string]string{argoTrackingAnnotation: "rhai-on-openshift:datasciencecluster.opendatahub.io/DataScienceCluster:odh-gitops/default-dsc"}, "DataScienceCluster", "", "default-dsc"); got != testArgoApp {
		t.Errorf("cluster-scoped DSC: got %q", got)
	}
}

// The recreated Subscription keeps every Argo CD annotation (tracking id,
// installation id, sync options), the last-applied annotation and the
// labels; other annotations are not copied.
func TestKeepArgoTracking(t *testing.T) {
	prev := map[string]interface{}{"metadata": map[string]interface{}{
		"annotations": map[string]interface{}{
			argoTrackingAnnotation:                "app:operators.coreos.com/Subscription:ns/name",
			"argocd.argoproj.io/installation-id":  "inst-1",
			"argocd.argoproj.io/sync-options":     "Prune=false",
			lastAppliedAnnotation:                 "{}",
			"olm.operatorframework.io/managed-by": "x",
		},
		"labels": map[string]interface{}{"helm.sh/chart": "c"},
	}}
	obj := map[string]interface{}{"metadata": map[string]interface{}{"name": "name"}}
	keepArgoTracking(prev, obj)
	ann := obj["metadata"].(map[string]interface{})["annotations"].(map[string]interface{})
	for _, k := range []string{argoTrackingAnnotation, "argocd.argoproj.io/installation-id", "argocd.argoproj.io/sync-options", lastAppliedAnnotation} {
		if ann[k] == nil {
			t.Errorf("annotation %s not kept: %v", k, ann)
		}
	}
	if ann["olm.operatorframework.io/managed-by"] != "x" {
		t.Errorf("an annotation Git may set was not copied: %v", ann)
	}
	kept := map[string]interface{}{"metadata": map[string]interface{}{"annotations": map[string]interface{}{lastAppliedAnnotation: "mine"}}}
	keepArgoTracking(prev, kept)
	if kept["metadata"].(map[string]interface{})["annotations"].(map[string]interface{})[lastAppliedAnnotation] != "mine" {
		t.Errorf("an annotation of the new object was overwritten: %v", kept)
	}
	if labels := obj["metadata"].(map[string]interface{})["labels"].(map[string]interface{}); labels["helm.sh/chart"] != "c" {
		t.Errorf("labels = %v", labels)
	}
	untracked := map[string]interface{}{"metadata": map[string]interface{}{"name": "name"}}
	keepArgoTracking(map[string]interface{}{"metadata": map[string]interface{}{"labels": map[string]interface{}{"a": "b"}}}, untracked)
	if untracked["metadata"].(map[string]interface{})["labels"] != nil {
		t.Error("labels copied from an untracked object")
	}
}

// Update on a cluster where Argo CD manages the catalog and Subscription
// installs the build the catalog serves: the catalog is not written, the
// channel Git sets is kept although stable-3.x has a newer build, and the
// Subscription is deleted only right before it is created again, with Argo
// CD's tracking.
func TestUpdateUnderGitOps_InstallsWhatTheCatalogServes(t *testing.T) {
	f := gitOpsFake(t)
	emit, events := eventsRecorder()
	result, err := UpdateStream(f.client(context.Background()), gitOpsImage, emit)
	if err != nil || !result.Success {
		t.Fatalf("result=%+v err=%v", result, err)
	}

	csPath := namespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, CatalogName)
	subPath := subscriptionPath()
	var writes []string
	for _, w := range f.writes() {
		if !strings.Contains(w, "-verify-") { // the verification catalog
			writes = append(writes, w)
		}
	}
	for _, w := range writes {
		if strings.HasSuffix(w, " "+csPath) {
			t.Fatalf("the Argo CD managed catalog was written: %v", writes)
		}
	}
	order := []string{"DELETE " + namespacedPath("operators.coreos.com/v1alpha1", "installplans", SubNS, "install-old"),
		"DELETE " + csvPath(gitOpsCSV), "DELETE " + subPath, "PATCH " + subPath}
	for i := 1; i < len(order); i++ {
		a, b := indexOf(writes, order[i-1]), indexOf(writes, order[i])
		if a < 0 || b < 0 || a > b {
			t.Fatalf("expected %q before %q; writes: %v", order[i-1], order[i], writes)
		}
	}
	// No other write between deleting and recreating the Subscription.
	if d, p := indexOf(writes, "DELETE "+subPath), indexOf(writes, "PATCH "+subPath); p != d+1 {
		t.Fatalf("the Subscription stayed deleted across other writes: %v", writes)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	spec := f.catalog["spec"].(map[string]interface{})
	if spec["image"] != gitOpsImage || spec["displayName"] != "Red Hat OpenShift AI (nightly)" {
		t.Fatalf("catalog spec = %v, want it unchanged", spec)
	}
	subSpec := f.sub["spec"].(map[string]interface{})
	if subSpec["channel"] != gitOpsChannel || subSpec["config"] == nil {
		t.Fatalf("Subscription spec = %v, want channel %s and the config Git sets", subSpec, gitOpsChannel)
	}
	if ann, _ := f.sub["metadata"].(map[string]interface{})["annotations"].(map[string]interface{}); ann[argoTrackingAnnotation] == nil || ann[lastAppliedAnnotation] == nil {
		t.Fatalf("the recreated Subscription lacks Argo CD's tracking or last-applied annotation: %v", ann)
	}
	// The chart's labels too: without them Argo CD syncs the Subscription
	// again right after Update recreated it (seen on a live cluster).
	if labels, _ := f.sub["metadata"].(map[string]interface{})["labels"].(map[string]interface{}); labels["helm.sh/chart"] != "rhai-on-openshift-chart-3.4.0" {
		t.Fatalf("the recreated Subscription lost Git's labels: %v", labels)
	}
	if lastStatus(events(), "detect_channel") != "success" {
		t.Fatal("detect_channel step not reported")
	}
}

// Git picks the build: Update refuses any image but the catalog's, before
// any change, and says where to change it.
func TestUpdateUnderGitOps_RefusesAnotherImage(t *testing.T) {
	f := gitOpsFake(t)
	result, err := UpdateStream(f.client(context.Background()), testNightlyImage, func(UpdateStepEvent) {})
	if err != nil || result.Success || result.ErrorCode != ErrorCodeGitOpsManaged ||
		!strings.Contains(result.Message, "change the image in Git") || !strings.Contains(result.Message, gitOpsImage) || !strings.Contains(result.Message, testArgoApp) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

// Argo CD syncs another image from Git while Update waits for the catalog:
// Update stops before it touches the operator.
func TestUpdateUnderGitOps_ImageChangesMeanwhile(t *testing.T) {
	f := gitOpsFake(t)
	stale := strings.Replace(f.channels, "sha256:new", "sha256:older", 1)
	f.mainChannels = &stale // the catalog pod has not caught up yet
	csPath := namespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, CatalogName)
	reads := 0
	f.intercept = func(f *fakeOLM, w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && r.URL.Path == csPath {
			reads++
			if reads == 3 {
				f.catalog["spec"].(map[string]interface{})["image"] = "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:newer"
			}
		}
		return false
	}
	result, err := UpdateStream(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
	if err != nil || result.Success || result.ErrorCode != ErrorCodeGitOpsManaged ||
		!strings.Contains(result.Message, "while Update waited") || !strings.Contains(result.Message, testArgoApp) || !strings.Contains(result.Message, "sha256:newer") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, w := range f.writes() {
		if strings.Contains(w, "/clusterserviceversions/") || strings.Contains(w, "/subscriptions/") || strings.Contains(w, "/installplans/") || strings.HasSuffix(w, " "+csPath) {
			t.Fatalf("the operator or the catalog was changed: %v", f.writes())
		}
	}
}

// A failed install restores the previous Subscription with Argo CD's
// tracking annotation; the catalog was never touched.
func TestUpdateUnderGitOps_RestoreKeepsTracking(t *testing.T) {
	f := gitOpsFake(t)
	f.onSubscribe = func(*fakeOLM, map[string]interface{}) {} // OLM never acts
	emit, events := eventsRecorder()
	result, err := UpdateStream(f.client(context.Background()), gitOpsImage, emit)
	if err != nil || result.Success || result.ErrorCode != "install_timeout" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if lastStatus(events(), restoreStepName) != "success" {
		t.Fatalf("restore did not succeed: %v", events())
	}
	if !strings.Contains(result.Message, "OLM installs the build the catalog serves again") || strings.Contains(result.Message, "previous operator version") {
		t.Fatalf("restore message under GitOps: %q", result.Message)
	}
	csPath := namespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, CatalogName)
	for _, w := range f.writes() {
		if strings.HasSuffix(w, " "+csPath) {
			t.Fatalf("the catalog was written: %v", f.writes())
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if image := f.catalog["spec"].(map[string]interface{})["image"]; image != gitOpsImage {
		t.Fatalf("catalog image = %v, want Git's", image)
	}
	if ann, _ := f.sub["metadata"].(map[string]interface{})["annotations"].(map[string]interface{}); ann[argoTrackingAnnotation] == nil {
		t.Fatal("the restored Subscription has no Argo CD tracking annotation")
	}
	if f.sub["spec"].(map[string]interface{})["channel"] != gitOpsChannel {
		t.Fatalf("restored Subscription = %v", f.sub["spec"])
	}
}

// A catalog whose image Argo CD changed in place reports READY and its old
// channels until OLM has replaced the pod: Update waits for the verified
// build, and does not touch the operator when it never shows up.
func TestUpdateUnderGitOps_WaitsForTheNewBuild(t *testing.T) {
	f := gitOpsFake(t)
	stale := strings.Replace(f.channels, "sha256:new", "sha256:older", 1)
	f.mainChannels = &stale
	result, err := UpdateStream(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
	if err != nil || result.Success || result.ErrorCode != "timeout" || !strings.Contains(result.Message, "did not serve") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, w := range f.writes() {
		if strings.Contains(w, "/clusterserviceversions/") || strings.Contains(w, "/subscriptions/") {
			t.Fatalf("the operator was changed before the catalog served the build: %v", f.writes())
		}
	}
}

func TestCatalogServing(t *testing.T) {
	target := catalogTarget{Channel: "beta", HeadCSV: gitOpsCSV, Fingerprint: "op@sha256:new t1"}
	ok := catalogServing{image: "img:new", state: "READY", podReady: true, head: gitOpsCSV, fingerprint: "op@sha256:new t1"}
	if !ok.servesTarget("img:new", target) {
		t.Fatal("a ready catalog serving the build was not accepted")
	}
	for name, s := range map[string]catalogServing{
		"old pod still serving": {image: "img:new", state: "READY", podReady: true, stalePods: 1, head: gitOpsCSV, fingerprint: ok.fingerprint},
		"new pod not ready":     {image: "img:new", state: "READY", head: gitOpsCSV, fingerprint: ok.fingerprint},
		"old build listed":      {image: "img:new", state: "READY", podReady: true, head: gitOpsCSV, fingerprint: "op@sha256:old t0"},
		"not READY":             {image: "img:new", state: "CONNECTING", podReady: true, head: gitOpsCSV, fingerprint: ok.fingerprint},
		"other head":            {image: "img:new", state: "READY", podReady: true, head: "rhods-operator.3.5.0", fingerprint: ok.fingerprint},
	} {
		if s.servesTarget("img:new", target) {
			t.Errorf("%s: accepted", name)
		}
	}
	noFingerprint := target
	noFingerprint.Fingerprint = ""
	if !(catalogServing{image: "img:new", state: "READY", podReady: true, head: gitOpsCSV}).servesTarget("img:new", noFingerprint) {
		t.Fatal("a catalog without fingerprint annotations is judged by its head")
	}
}

// Update refuses, before any change, what Argo CD would undo.
func TestUpdateUnderGitOps_Refusals(t *testing.T) {
	t.Run("source not the nightly catalog", func(t *testing.T) {
		f := gitOpsFake(t)
		f.sub["spec"].(map[string]interface{})["source"] = "redhat-operators"
		result, err := UpdateStream(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
		if err != nil || result.Success || result.ErrorCode != ErrorCodeGitOpsManaged || !strings.Contains(result.Message, "redhat-operators") {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if w := f.writes(); len(w) != 0 {
			t.Fatalf("writes = %v", w)
		}
	})
	t.Run("no catalog to install from", func(t *testing.T) {
		f := gitOpsFake(t)
		f.catalog = nil
		result, err := UpdateStream(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
		if err != nil || result.Success || result.ErrorCode != ErrorCodeGitOpsManaged || !strings.Contains(result.Message, "does not exist") || !strings.Contains(result.Message, "in Git") {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if w := f.writes(); len(w) != 0 {
			t.Fatalf("writes = %v", w)
		}
	})
	t.Run("first install still running", func(t *testing.T) {
		f := gitOpsFake(t)
		// Argo CD created the Subscription; OLM is installing its InstallPlan.
		delete(f.csvs, gitOpsCSV)
		f.sub["status"] = map[string]interface{}{"state": "UpgradePending", "installPlanRef": map[string]interface{}{"name": "install-first"}}
		f.installPlans["install-first"] = map[string]interface{}{"spec": map[string]interface{}{"clusterServiceVersionNames": []string{gitOpsCSV}}, "status": map[string]interface{}{"phase": "Installing"}}
		result, err := UpdateStream(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
		if err != nil || result.Success || result.ErrorCode != ErrorCodeGitOpsManaged || !strings.Contains(result.Message, "install-first is Installing") {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if w := f.writes(); len(w) != 0 {
			t.Fatalf("writes = %v", w)
		}
		// Once that plan is done (Failed here), Update proceeds.
		f.installPlans["install-first"]["status"].(map[string]interface{})["phase"] = "Failed"
		if result, err := UpdateStream(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {}); err != nil || !result.Success {
			t.Fatalf("after the plan ended: result=%+v err=%v", result, err)
		}
	})
	t.Run("startingCSV pinned to another build", func(t *testing.T) {
		f := gitOpsFake(t)
		f.sub["spec"].(map[string]interface{})["startingCSV"] = "rhods-operator.3.5.0"
		result, err := UpdateStream(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
		if err != nil || result.Success || result.ErrorCode != ErrorCodeGitOpsManaged || !strings.Contains(result.Message, "pins startingCSV rhods-operator.3.5.0") {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		for _, w := range f.writes() {
			if strings.Contains(w, "/clusterserviceversions/") || strings.Contains(w, "/subscriptions/") {
				t.Fatalf("the operator was changed: %v", f.writes())
			}
		}
	})
	t.Run("build without Git's channel", func(t *testing.T) {
		f := gitOpsFake(t)
		f.channels = `[{"name":"stable-3.x","currentCSV":"rhods-operator.3.6.0"}]`
		result, err := UpdateStream(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
		if err != nil || result.Success || result.ErrorCode != "validation" || !strings.Contains(result.Message, `channel "beta" is not in the catalog`) ||
			!strings.Contains(result.Message, "change it in Git") {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		for _, w := range f.writes() {
			if !strings.Contains(w, "-verify-") {
				t.Fatalf("writes = %v", f.writes())
			}
		}
	})
}

// Git pins startingCSV to the build the catalog serves: the recreated
// Subscription keeps the pin, so it matches Git.
func TestUpdateUnderGitOps_KeepsStartingCSV(t *testing.T) {
	f := gitOpsFake(t)
	f.sub["spec"].(map[string]interface{})["startingCSV"] = gitOpsCSV
	result, err := UpdateStream(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
	if err != nil || !result.Success {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sub["spec"].(map[string]interface{})["startingCSV"] != gitOpsCSV {
		t.Fatalf("recreated Subscription spec = %v", f.sub["spec"])
	}
}

// Argo CD syncs a catalog change between the catalog check and the
// deletions: Update stops with the operator untouched.
func TestUpdateUnderGitOps_ImageChangesBeforeTheDeletions(t *testing.T) {
	f := gitOpsFake(t)
	f.intercept = func(f *fakeOLM, w http.ResponseWriter, r *http.Request) bool {
		// The catalog's PackageManifest is read last in each wait poll: the
		// image changes right after the poll that accepted the catalog.
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/packagemanifests") && strings.Contains(r.URL.Query().Get("labelSelector"), CatalogName) {
			f.catalog["spec"].(map[string]interface{})["image"] = "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:newer"
		}
		return false
	}
	result, err := UpdateStream(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
	if err != nil || result.Success || result.ErrorCode != ErrorCodeGitOpsManaged || !strings.Contains(result.Message, "sha256:newer") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, w := range f.writes() {
		if strings.Contains(w, "/clusterserviceversions/") || strings.Contains(w, "/subscriptions/") || strings.Contains(w, "/installplans/") {
			t.Fatalf("the operator was changed: %v", f.writes())
		}
	}
}

// Argo CD syncs a new Subscription spec from Git while the old CSV is being
// deleted: the Subscription is left as Git set it, and the previous operator
// is restored.
func TestUpdateUnderGitOps_SubscriptionChangesMeanwhile(t *testing.T) {
	f := gitOpsFake(t)
	f.intercept = func(f *fakeOLM, w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/clusterserviceversions/") {
			f.sub["spec"].(map[string]interface{})["channel"] = "fast"
		}
		return false
	}
	emit, events := eventsRecorder()
	result, err := UpdateStream(f.client(context.Background()), gitOpsImage, emit)
	if err != nil || result.Success || result.ErrorCode != "conflict" || !strings.Contains(result.Message, "channel fast") || !strings.Contains(result.Message, "Git's current settings") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	// Only the restore writes the Subscription: it recreates it from Git's
	// current spec (the old CSV is gone), with Argo CD's tracking.
	if lastStatus(events(), restoreStepName) != "success" {
		t.Fatalf("restore did not run: %v", events())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sub["spec"].(map[string]interface{})["channel"] != "fast" {
		t.Fatalf("the restore put the old channel back over Git's: %v", f.sub["spec"])
	}
	if ann, _ := f.sub["metadata"].(map[string]interface{})["annotations"].(map[string]interface{}); ann[argoTrackingAnnotation] == nil {
		t.Fatalf("the restored Subscription lost Argo CD's tracking: %v", f.sub["metadata"])
	}
}

// Git adds a startingCSV pin while Update runs: one for the catalog's head
// is kept on the recreated Subscription; one for another build stops the
// install, and the restore keeps Git's pin.
func TestUpdateUnderGitOps_StartingCSVChangesMeanwhile(t *testing.T) {
	for name, pin := range map[string]string{"head": gitOpsCSV, "other": "rhods-operator.3.5.0"} {
		t.Run(name, func(t *testing.T) {
			f := gitOpsFake(t)
			f.intercept = func(f *fakeOLM, w http.ResponseWriter, r *http.Request) bool {
				if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/clusterserviceversions/") {
					f.sub["spec"].(map[string]interface{})["startingCSV"] = pin
				}
				return false
			}
			result, err := UpdateStream(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
			if err != nil {
				t.Fatal(err)
			}
			if pin == gitOpsCSV && !result.Success {
				t.Fatalf("result=%+v", result)
			}
			if pin != gitOpsCSV && (result.Success || result.ErrorCode != "conflict" || !strings.Contains(result.Message, "now pins startingCSV "+pin)) {
				t.Fatalf("result=%+v", result)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.sub["spec"].(map[string]interface{})["startingCSV"] != pin {
				t.Fatalf("Subscription spec = %v", f.sub["spec"])
			}
		})
	}
}

// Only the catalog is Argo CD's: the Subscription is the tool's to rewrite,
// so its channel is the one the build publishes, as in any Update, and a
// different channel on it is no conflict.
func TestUpdateUnderGitOps_CatalogOnlyManaged(t *testing.T) {
	f := gitOpsFake(t)
	delete(f.sub["metadata"].(map[string]interface{}), "annotations")
	f.sub["spec"].(map[string]interface{})["channel"] = "fast"
	f.sub["spec"].(map[string]interface{})["startingCSV"] = "rhods-operator.3.5.0"
	result, err := UpdateStream(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
	if err != nil || !result.Success {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	spec := f.sub["spec"].(map[string]interface{})
	// Git's old pin is dropped; the new one is the verified head.
	if spec["channel"] == "fast" || spec["startingCSV"] != "rhods-operator.3.6.0" {
		t.Fatalf("recreated Subscription spec = %v", spec)
	}
}

// The installed CSV has no build annotations at all: that is not the
// verified build either.
func TestUpdateUnderGitOps_BuildWithoutAnnotations(t *testing.T) {
	f := gitOpsFake(t)
	f.onSubscribe = func(f *fakeOLM, spec map[string]interface{}) {
		f.csvAnnotations = nil
		olmInstalls(gitOpsCSV)(f, spec)
	}
	result, err := UpdateStream(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
	if err != nil || result.Success || result.ErrorCode != "install_mismatch" || !strings.Contains(result.Message, "carries no build annotations") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

// The Subscription Git synced meanwhile (same source and channel, new
// config) is the base of the recreated one, not the snapshot from the start.
func TestUpdateUnderGitOps_RecreatesFromTheLiveSubscription(t *testing.T) {
	f := gitOpsFake(t)
	f.intercept = func(f *fakeOLM, w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/clusterserviceversions/") {
			f.sub["spec"].(map[string]interface{})["config"] = map[string]interface{}{"env": []interface{}{map[string]interface{}{"name": "NEW", "value": "1"}}}
		}
		return false
	}
	result, err := UpdateStream(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
	if err != nil || !result.Success {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cfg, _ := json.Marshal(f.sub["spec"].(map[string]interface{})["config"])
	if !strings.Contains(string(cfg), `"NEW"`) {
		t.Fatalf("recreated Subscription has the old config: %s", cfg)
	}
}

// OLM installs a CSV of the right name but another build (the catalog moved
// on): Update reports it instead of success, and keeps the running operator.
func TestUpdateUnderGitOps_AnotherBuildInstalled(t *testing.T) {
	f := gitOpsFake(t)
	f.onSubscribe = func(f *fakeOLM, spec map[string]interface{}) {
		f.csvAnnotations = map[string]interface{}{"containerImage": "registry.redhat.io/rhoai/odh-rhel9-operator@sha256:other", "createdAt": "2026-10-09T04:00:00Z"}
		olmInstalls(gitOpsCSV)(f, spec)
	}
	emit, events := eventsRecorder()
	result, err := UpdateStream(f.client(context.Background()), gitOpsImage, emit)
	if err != nil || result.Success || result.ErrorCode != "install_mismatch" || !strings.Contains(result.Message, "sha256:other") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if lastStatus(events(), restoreStepName) != "" {
		t.Fatalf("a Succeeded operator was torn down: %v", events())
	}
	if _, ok := f.csvs[gitOpsCSV]; !ok {
		t.Fatal("the installed CSV was deleted")
	}
}

// Creating a DataScienceCluster by hand is refused while Argo CD manages the
// operator: Git creates it, and would overwrite a hand-made one.
func TestCreateDSCRefusedUnderGitOps(t *testing.T) {
	f := gitOpsFake(t)
	r, err := CreateDefaultDSC(f.client(context.Background()))
	if err != nil || r.Success || r.ErrorCode != ErrorCodeGitOpsManaged || !strings.Contains(r.Message, testArgoApp) {
		t.Fatalf("result=%+v err=%v", r, err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

// Reinstall and Re-deploy delete the Subscription for minutes; Argo CD would
// recreate it from Git meanwhile, so they refuse before any change.
func TestOperatorOperationsRefusedUnderGitOps(t *testing.T) {
	for name, run := range map[string]func(*Client) (string, string){
		"reinstall": func(c *Client) (string, string) {
			r, _ := ReinstallStreamWithOptions(c, "nightly", testNightlyImage, "", OperationOptions{}, func(UpdateStepEvent) {})
			return r.ErrorCode, r.Message
		},
		"re-deploy": func(c *Client) (string, string) {
			r, _ := RefreshOperatorStreamWithOptions(c, OperationOptions{}, func(UpdateStepEvent) {})
			return r.ErrorCode, r.Message
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := gitOpsFake(t)
			code, msg := run(f.client(context.Background()))
			if code != ErrorCodeGitOpsManaged || !strings.Contains(msg, testArgoApp) || !strings.Contains(msg, "Nothing was changed") {
				t.Fatalf("code=%q message=%q", code, msg)
			}
			if w := f.writes(); len(w) != 0 {
				t.Fatalf("writes = %v", w)
			}
		})
	}
	t.Run("only the catalog managed", func(t *testing.T) {
		f := gitOpsFake(t)
		delete(f.sub["metadata"].(map[string]interface{}), "annotations")
		r, _ := ReinstallStreamWithOptions(f.client(context.Background()), "stable", "", "", OperationOptions{}, func(UpdateStepEvent) {})
		if r.ErrorCode != ErrorCodeGitOpsManaged || !strings.Contains(r.Message, "CatalogSource "+CatalogName) {
			t.Fatalf("result=%+v", r)
		}
	})
}

func TestUpdateDryRunUnderGitOps(t *testing.T) {
	oldLookup := lookupTargetBundle
	t.Cleanup(func() { lookupTargetBundle = oldLookup })
	lookupTargetBundle = func(*Client, string) (string, error) { return gitOpsCSV, nil }
	f := gitOpsFake(t)
	result, err := Update(f.client(context.Background()), gitOpsImage, true)
	if err != nil || !result.Success {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	logs := strings.Join(result.Logs, "\n")
	for _, want := range []string{"as Git sets it", "channel beta (kept from Git)", "Argo CD tracking"} {
		if !strings.Contains(logs, want) {
			t.Errorf("dry run logs lack %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "Would delete and recreate Subscription") {
		t.Errorf("dry run describes the plain flow:\n%s", logs)
	}
	if result, err := Update(f.client(context.Background()), testNightlyImage, true); err != nil || result.Success || result.ErrorCode != ErrorCodeGitOpsManaged {
		t.Fatalf("dry run of another image: result=%+v err=%v", result, err)
	}
}

// Status says what the catalog serves on Git's channel and whether the
// installed operator is that build, by CSV name and build fingerprint.
func TestStatusReportsWhatTheCatalogServes(t *testing.T) {
	f := gitOpsFake(t)
	c := f.client(context.Background())
	sub := types.SubscriptionInfo{Source: CatalogName, Channel: gitOpsChannel}
	cs := types.CatalogSourceInfo{Exists: true, Image: gitOpsImage}
	csv := types.CSVInfo{Name: gitOpsCSV, Phase: "Succeeded"}
	annotate := func(image, created string) {
		f.csvs[gitOpsCSV]["metadata"].(map[string]interface{})["annotations"] = map[string]interface{}{"containerImage": image, "createdAt": created}
	}

	delete(f.csvs[gitOpsCSV]["metadata"].(map[string]interface{}), "annotations")
	s := readCatalogServes(c, sub, cs, csv)
	if s == nil || s.Channel != gitOpsChannel || s.CSV != gitOpsCSV || s.OperatorImage != gitOpsOperatorImage || s.Built != gitOpsBuilt || s.Installed != nil || s.Updating {
		t.Fatalf("without CSV annotations: %+v", s)
	}
	annotate(gitOpsOperatorImage, gitOpsBuilt)
	if s := readCatalogServes(c, sub, cs, csv); s == nil || s.Installed == nil || !*s.Installed {
		t.Fatalf("same build: %+v", s)
	}
	annotate("registry.redhat.io/rhoai/odh-rhel9-operator@sha256:older", "2026-09-18T04:00:00Z")
	if s := readCatalogServes(c, sub, cs, csv); s == nil || s.Installed == nil || *s.Installed {
		t.Fatalf("older build of the same version: %+v", s)
	}
	if s := readCatalogServes(c, sub, cs, types.CSVInfo{Name: "rhods-operator.3.5.0", Phase: "Succeeded"}); s == nil || s.Installed == nil || *s.Installed {
		t.Fatalf("another version: %+v", s)
	}
	if s := readCatalogServes(c, sub, cs, types.CSVInfo{Phase: "Not Found"}); s == nil || s.Installed != nil {
		t.Fatalf("nothing installed: %+v", s)
	}
	if s := readCatalogServes(c, types.SubscriptionInfo{Source: CatalogName, Channel: "fast"}, cs, csv); s != nil {
		t.Fatalf("channel not in the catalog: %+v", s)
	}
	if s := readCatalogServes(c, types.SubscriptionInfo{Source: "redhat-operators", Channel: "stable-3.x"}, cs, csv); s != nil {
		t.Fatalf("another source: %+v", s)
	}
	// Argo CD just changed the image: the registry pod still runs the old
	// one, so the PackageManifest is the old build's and says nothing yet.
	moved := types.CatalogSourceInfo{Exists: true, Image: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:newer"}
	f.gitOpsPods = false
	f.catalogPods = `{"items":[{"metadata":{"name":"old"},"spec":{"containers":[{"image":"` + gitOpsImage + `"}]},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`
	if s := readCatalogServes(c, sub, moved, csv); s == nil || !s.Updating || s.Installed != nil {
		t.Fatalf("catalog pod being replaced: %+v", s)
	}
	f.channels = `[{"name":"beta"}]`
	if s := readCatalogServes(c, sub, cs, csv); s != nil {
		t.Fatalf("channel without a head: %+v", s)
	}
}

// The DSC repairs and Diagnostics' disable-component refuse a DSC that Argo
// CD manages: it would put the old spec back.
func TestDSCChangesRefusedUnderGitOps(t *testing.T) {
	tracked := `"annotations":{"` + argoTrackingAnnotation + `":"` + testArgoApp + `:datasciencecluster.opendatahub.io/DataScienceCluster:odh-gitops/default-dsc"}`
	t.Run("repair", func(t *testing.T) {
		mockDSCSamples(t, func(*http.Request) (int, string) { return 200, testDSCSample })
		var mu sync.Mutex
		patched := false
		c := dscRepairD3Server(t, `{"apiVersion":"datasciencecluster.opendatahub.io/v2","metadata":{"name":"default-dsc","resourceVersion":"42",`+tracked+`},"spec":{"components":{"dashboard":{"managementState":"Managed"}}}}`, &patched, &mu)
		r, err := RepairDSC(c, "default-dsc", "reset-defaults", "3.6.0", "datasciencecluster.opendatahub.io/v2", nil, []string{})
		if err != nil || r.Success || r.ErrorCode != ErrorCodeGitOpsManaged || !strings.Contains(r.Message, testArgoApp) {
			t.Fatalf("result = %+v, %v", r, err)
		}
		mu.Lock()
		defer mu.Unlock()
		if patched {
			t.Fatal("the DSC was patched")
		}
	})
	t.Run("disable-component", func(t *testing.T) {
		f, c := newFakeAPI(t)
		f.json("GET", dscV2List, http.StatusOK, `{"items":[{"metadata":{"name":"default-dsc"}}]}`)
		f.json("GET", dscV2List+"/default-dsc", http.StatusOK, `{"metadata":{"name":"default-dsc","uid":"u1","resourceVersion":"7",`+tracked+`},"spec":{"components":{"ray":{"managementState":"Managed"}}}}`)
		res, err := ApplyFix(c, "disable-component:ray")
		if err != nil || res.Success || res.ErrorCode != ErrorCodeGitOpsManaged || !strings.Contains(res.Message, "Set ray to Removed in Git") {
			t.Fatalf("result = %+v, %v", res, err)
		}
		assertWrites(t, f)
	})
}

// Status names the Argo CD application of each managed object.
func TestStatusReportsArgoCDApplications(t *testing.T) {
	f := gitOpsFake(t)
	c := f.client(context.Background())
	sub, err := readSubscription(c)
	if err != nil || sub.info.ManagedBy != testArgoApp {
		t.Fatalf("subscription managedBy = %q, %v", sub.info.ManagedBy, err)
	}
	cs, err := getCatalogSource(c)
	if err != nil || cs.ManagedBy != testArgoApp {
		t.Fatalf("catalog managedBy = %q, %v", cs.ManagedBy, err)
	}
	delete(f.catalog, "metadata")
	if cs, _ := getCatalogSource(c); cs.ManagedBy != "" {
		t.Fatalf("untracked catalog managedBy = %q", cs.ManagedBy)
	}
}
