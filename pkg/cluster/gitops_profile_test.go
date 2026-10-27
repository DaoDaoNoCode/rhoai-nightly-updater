package cluster

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The writes the gitops profile's Update may make: the old InstallPlan and
// CSV, the Subscription (deleted and recreated back to back). Never a
// CatalogSource. writes() leaves out the updater's own ConfigMaps.
func gitOpsProfileWriteAllowed(w string) bool {
	method, path, _ := strings.Cut(w, " ")
	switch {
	case strings.HasPrefix(path, namespacedPath("operators.coreos.com/v1alpha1", "installplans", SubNS, "")) && method == http.MethodDelete:
		return true
	case strings.HasPrefix(path, namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "")) && method == http.MethodDelete:
		return true
	case path == subscriptionPath() && (method == http.MethodPatch || method == http.MethodDelete):
		return true
	}
	return false
}

// In the gitops profile Update installs the build the Argo CD managed
// catalog serves, exactly as Update's Argo CD path does, and writes
// nothing else.
func TestUpdateGitOpsProfile_InstallsWhatTheCatalogServes(t *testing.T) {
	f := gitOpsFake(t)
	emit, events := eventsRecorder()
	result, err := UpdateStreamGitOpsProfile(f.client(context.Background()), gitOpsImage, emit)
	if err != nil || !result.Success {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	writes := f.writes()
	for _, w := range writes {
		if !gitOpsProfileWriteAllowed(w) {
			t.Errorf("the gitops profile wrote %s; writes: %v", w, writes)
		}
	}
	subPath := subscriptionPath()
	if d, p := indexOf(writes, "DELETE "+subPath), indexOf(writes, "PATCH "+subPath); d < 0 || p != d+1 {
		t.Fatalf("the Subscription was not deleted and recreated back to back: %v", writes)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if spec := f.catalog["spec"].(map[string]interface{}); spec["image"] != gitOpsImage {
		t.Fatalf("catalog spec = %v", spec)
	}
	if ch := f.sub["spec"].(map[string]interface{})["channel"]; ch != gitOpsChannel {
		t.Fatalf("channel = %v, want Git's %s", ch, gitOpsChannel)
	}
	if lastStatus(events(), "verify_installplan") != "success" {
		t.Fatal("the install was not verified")
	}
}

// Without Argo CD on the catalog, Update would replace the catalog itself:
// the profile refuses before any change, also when Argo CD manages only the
// Subscription.
func TestUpdateGitOpsProfile_RefusesAnUnmanagedCatalog(t *testing.T) {
	for name, setup := range map[string]func() *fakeOLM{
		"nothing managed": func() *fakeOLM { return newFakeOLM(t).installed("rhods-operator.3.6.0", nil) },
		"subscription only": func() *fakeOLM {
			f := gitOpsFake(t)
			f.catalog["metadata"] = map[string]interface{}{}
			return f
		},
		"no catalog": func() *fakeOLM {
			f := gitOpsFake(t)
			f.catalog = nil
			return f
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup()
			result, err := UpdateStreamGitOpsProfile(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
			if err != nil || result.Success || result.ErrorCode != ErrorCodeProfileGitOps || !strings.Contains(result.Message, "Nothing was changed") {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if w := f.writes(); len(w) != 0 {
				t.Fatalf("writes = %v", w)
			}
		})
	}
}

// Git creates the operator namespace and OperatorGroup; the profile never
// does, and refuses an unusable one.
func TestUpdateGitOpsProfile_RefusesAMissingOperatorGroup(t *testing.T) {
	for name, setup := range map[string]func(f *fakeOLM){
		"no OperatorGroup": func(f *fakeOLM) { f.operatorGrps = nil },
		"two OperatorGroups": func(f *fakeOLM) {
			f.operatorGrps = append(f.operatorGrps, map[string]interface{}{"metadata": map[string]interface{}{"name": "other"}, "spec": map[string]interface{}{}})
		},
		"not global": func(f *fakeOLM) {
			f.operatorGrps = []interface{}{map[string]interface{}{"metadata": map[string]interface{}{"name": "rhods-operator"}, "spec": map[string]interface{}{"targetNamespaces": []interface{}{SubNS}}}}
		},
		"no namespace": func(f *fakeOLM) {
			f.intercept = func(_ *fakeOLM, w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/api/v1/namespaces/"+SubNS {
					notFound(w)
					return true
				}
				return false
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := gitOpsFake(t)
			setup(f)
			result, err := UpdateStreamGitOpsProfile(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
			if err != nil || result.Success || result.ErrorCode != "prerequisites" || !strings.Contains(result.Message, "Nothing was changed") {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if w := f.writes(); len(w) != 0 {
				t.Fatalf("writes = %v", w)
			}
		})
	}
}

// A paused dashboard-operator is a refusal: ending the session patches
// Deployments, which the profile cannot do.
func TestUpdateGitOpsProfile_RefusesADashboardDevSession(t *testing.T) {
	f := gitOpsFake(t)
	f.dashboardOp = `{"metadata":{"uid":"u1","annotations":{"` + dashboardDevAnnotation + `":"{}"}},"spec":{"replicas":0}}`
	result, err := UpdateStreamGitOpsProfile(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
	if err != nil || result.Success || result.ErrorCode != errorCodeDashboardDevActive || !strings.Contains(result.Message, "full profile") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

// Git picks the build, as in Update's Argo CD path.
func TestUpdateGitOpsProfile_RefusesAnotherImage(t *testing.T) {
	f := gitOpsFake(t)
	result, err := UpdateStreamGitOpsProfile(f.client(context.Background()), testNightlyImage, func(UpdateStepEvent) {})
	if err != nil || result.Success || result.ErrorCode != ErrorCodeGitOpsManaged || !strings.Contains(result.Message, gitOpsImage) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

// The gitops profile's dry run makes no change and refuses what Update would.
func TestUpdateDryRunGitOpsProfile(t *testing.T) {
	orig := lookupTargetBundle
	t.Cleanup(func() { lookupTargetBundle = orig })
	lookupTargetBundle = func(*Client, string) (string, error) { return gitOpsCSV, nil }

	f := gitOpsFake(t)
	result, err := UpdateDryRunGitOpsProfile(f.client(context.Background()), gitOpsImage)
	if err != nil || !result.Success || !strings.Contains(strings.Join(result.Logs, "\n"), "left as Git sets it") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}

	f = newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
	if result, _ := UpdateDryRunGitOpsProfile(f.client(context.Background()), gitOpsImage); result.Success || result.ErrorCode != ErrorCodeProfileGitOps {
		t.Fatalf("unmanaged catalog: %+v", result)
	}
	f = gitOpsFake(t)
	f.operatorGrps = nil
	if result, _ := UpdateDryRunGitOpsProfile(f.client(context.Background()), gitOpsImage); result.Success || result.ErrorCode != "prerequisites" {
		t.Fatalf("no OperatorGroup: %+v", result)
	}
	f = gitOpsFake(t)
	if result, _ := UpdateDryRunGitOpsProfile(f.client(context.Background()), testNightlyImage); result.Success || result.ErrorCode != ErrorCodeGitOpsManaged {
		t.Fatalf("another image: %+v", result)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

// A failed install restores the previous Subscription, as Update does;
// the restore writes only what the profile may write.
func TestUpdateGitOpsProfile_RestoreStaysInsideTheProfile(t *testing.T) {
	f := gitOpsFake(t)
	f.onSubscribe = func(f *fakeOLM, _ map[string]interface{}) {
		f.installPlans["install-new"] = map[string]interface{}{"spec": map[string]interface{}{"clusterServiceVersionNames": []string{gitOpsCSV}}, "status": map[string]interface{}{"phase": "Failed"}}
		f.sub["status"] = map[string]interface{}{"installPlanRef": map[string]interface{}{"name": "install-new"}, "state": "UpgradePending"}
	}
	result, err := UpdateStreamGitOpsProfile(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
	if err != nil || result.Success {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, w := range f.writes() {
		if !gitOpsProfileWriteAllowed(w) {
			t.Errorf("the restore wrote %s; writes: %v", w, f.writes())
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sub == nil {
		t.Fatal("the Subscription was not restored")
	}
	if ann, _ := f.sub["metadata"].(map[string]interface{})["annotations"].(map[string]interface{}); ann[argoTrackingAnnotation] == nil {
		t.Fatalf("the restored Subscription lost Argo CD's tracking: %v", f.sub["metadata"])
	}
}

// gitOpsSubscriptionConfig sets the config of the fake's Subscription.
func gitOpsSubscriptionConfig(f *fakeOLM, config map[string]interface{}) {
	f.sub["spec"].(map[string]interface{})["config"] = config
}

func relatedImageConfig() map[string]interface{} {
	return map[string]interface{}{"env": []interface{}{
		map[string]interface{}{"name": "DISABLE_DSC_CONFIG", "value": "true"},
		map[string]interface{}{"name": "RELATED_IMAGE_ODH_DASHBOARD_IMAGE", "value": "quay.io/evil/dashboard:1"},
	}}
}

// Update never writes a Subscription the admission policy would refuse,
// and never deletes one it could not write back: it refuses before any
// change when Git's Subscription (the new one is built from it, and the
// restore re-applies it) breaks the policy.
func TestUpdateGitOpsProfile_RefusesWhatThePolicyWouldRefuse(t *testing.T) {
	for name, setup := range map[string]func(f *fakeOLM){
		"RELATED_IMAGE in Git's config": func(f *fakeOLM) { gitOpsSubscriptionConfig(f, relatedImageConfig()) },
		"volumes in Git's config": func(f *fakeOLM) {
			gitOpsSubscriptionConfig(f, map[string]interface{}{"volumes": []interface{}{map[string]interface{}{"name": "v"}}})
		},
		// Argo CD manages only the catalog; the Subscription installs from
		// redhat-operators, so the restore could not re-apply it.
		"catalog only, Subscription on redhat-operators": func(f *fakeOLM) {
			f.sub["metadata"].(map[string]interface{})["annotations"] = map[string]interface{}{}
			f.sub["spec"].(map[string]interface{})["source"] = "redhat-operators"
		},
		"another sourceNamespace": func(f *fakeOLM) {
			f.sub["metadata"].(map[string]interface{})["annotations"] = map[string]interface{}{}
			f.sub["spec"].(map[string]interface{})["sourceNamespace"] = "other-catalogs"
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := gitOpsFake(t)
			setup(f)
			result, err := UpdateStreamGitOpsProfile(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
			if err != nil || result.Success || result.ErrorCode != ErrorCodeProfileGitOps || !strings.Contains(result.Message, "Nothing was changed") {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if w := f.writes(); len(w) != 0 {
				t.Fatalf("writes = %v", w)
			}
		})
	}
}

// Catalog-only management with a Subscription the policy allows works:
// the new Subscription takes the build's channel from the catalog Git
// manages.
func TestUpdateGitOpsProfile_CatalogOnlyManaged(t *testing.T) {
	f := gitOpsFake(t)
	f.sub["metadata"].(map[string]interface{})["annotations"] = map[string]interface{}{}
	result, err := UpdateStreamGitOpsProfile(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
	if err != nil || !result.Success {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, w := range f.writes() {
		if !gitOpsProfileWriteAllowed(w) {
			t.Errorf("wrote %s", w)
		}
	}
}

// Git changes the Subscription to a config the policy refuses after the
// first check, before anything is deleted: the check on the live
// Subscription right before the first deletion stops Update.
func TestUpdateGitOpsProfile_SubscriptionChangesBeforeTheDeletions(t *testing.T) {
	f := gitOpsFake(t)
	changed := false
	f.intercept = func(f *fakeOLM, _ http.ResponseWriter, r *http.Request) bool {
		// foreignSubscriptionRefusal lists the InstallPlans after the
		// recovery state was captured and checked.
		if !changed && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/installplans") {
			changed = true
			gitOpsSubscriptionConfig(f, relatedImageConfig())
		}
		return false
	}
	result, err := UpdateStreamGitOpsProfile(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
	if err != nil || result.Success || result.ErrorCode != ErrorCodeProfileGitOps || !strings.Contains(result.Message, "Nothing was changed") || !changed {
		t.Fatalf("result=%+v err=%v changed=%v", result, err, changed)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

// The same change after the old CSV was deleted: the Subscription is left
// as Git sets it (deleting it could not be undone) and the restore does not
// run, since it would delete and re-apply that Subscription.
func TestUpdateGitOpsProfile_SubscriptionChangesAfterTheCSVDeletion(t *testing.T) {
	f := gitOpsFake(t)
	f.intercept = func(f *fakeOLM, _ http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/clusterserviceversions/") {
			gitOpsSubscriptionConfig(f, relatedImageConfig())
		}
		return false
	}
	result, err := UpdateStreamGitOpsProfile(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
	if err != nil || result.Success || result.ErrorCode != ErrorCodeProfileGitOps || !strings.Contains(result.Message, "left as Git sets it") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, w := range f.writes() {
		if strings.Contains(w, "/subscriptions/") {
			t.Fatalf("the Subscription was written: %v", f.writes())
		}
	}
}
