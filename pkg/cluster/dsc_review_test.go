package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// conversionCRD stores a RHOAI CRD whose conversion webhook is served by
// Service ns/svc, labelled as the RHOAI operator labels its CRDs.
func putConversionCRD(f *resourceFake, name, svc string) {
	ns := "redhat-ods-applications"
	f.put("/apis/apiextensions.k8s.io/v1/customresourcedefinitions/"+name, map[string]interface{}{
		"metadata": map[string]interface{}{"name": name, "labels": map[string]interface{}{"platform.opendatahub.io/part-of": "platform"}},
		"spec": map[string]interface{}{"conversion": map[string]interface{}{"strategy": "Webhook", "webhook": map[string]interface{}{
			"clientConfig": map[string]interface{}{"service": map[string]interface{}{"namespace": ns, "name": svc}}}}},
	})
}

func blockReasons(p *types.DSCPlan, component string) string {
	for _, b := range p.Blocked {
		if b.Component == component {
			return strings.Join(b.Reasons, " ")
		}
	}
	return ""
}

// The live 3.6 bug: the plan showed no blocker for sparkoperator while the
// change refused it. Plan and change run planToggle; the dialog gets the
// refusal with the commands that check for objects.
func TestToggleOfAConversionWebhookComponentIsRefusedInThePlanAsInTheChange(t *testing.T) {
	f, c := managerWorld(t, func(dsc map[string]interface{}) {
		setPath(t, dsc, "spec.components.sparkoperator.managementState", "Managed")
	})
	putConversionCRD(f, "sparkapplications.sparkoperator.k8s.io", "spark-operator-webhook-service")
	putConversionCRD(f, "scheduledsparkapplications.sparkoperator.k8s.io", "spark-operator-webhook-service")
	plan, err := PlanDSCChange(c, "toggle", "sparkoperator", "Removed", false, false)
	if err != nil {
		t.Fatal(err)
	}
	why := blockReasons(plan, "sparkoperator")
	for _, want := range []string{"oc get scheduledsparkapplications.sparkoperator.k8s.io -A", "oc get sparkapplications.sparkoperator.k8s.io -A", "If none exist, set sparkoperator to Removed yourself in the OpenShift console"} {
		if !strings.Contains(why, want) {
			t.Fatalf("plan blocker %q lacks %q", why, want)
		}
	}
	res, _ := SetDSCManagementState(c, types.DSCToggleRequest{Name: "default-dsc", Path: "sparkoperator", From: "Managed", To: "Removed", UID: plan.UID, Token: plan.Token})
	if res.Success || res.ErrorCode != "prerequisites" || !reflect.DeepEqual(res.Logs, []string{"sparkoperator: " + why}) {
		t.Fatalf("change = %+v, plan said %q", res, why)
	}
	if dry, real := patchesTo(f); len(dry)+len(real) != 0 {
		t.Fatal("patched a blocked change")
	}
}

// A toggle runs only with the token of the plan the user reviewed.
func TestToggleNeedsTheReviewedPlan(t *testing.T) {
	f, c := managerWorld(t, nil)
	plan, _ := PlanDSCChange(c, "toggle", "ray", "Removed", false, false)
	for _, req := range []types.DSCToggleRequest{
		{Name: "default-dsc", Path: "ray", From: "Managed", To: "Removed", UID: plan.UID},
		{Name: "default-dsc", Path: "ray", From: "Managed", To: "Removed", UID: plan.UID, Token: strings.Repeat("0", 24)},
		{Name: "default-dsc", Path: "ray", From: "Managed", To: "Removed", UID: "other", Token: plan.Token},
	} {
		if res, _ := SetDSCManagementState(c, req); res.Success || res.ErrorCode != "conflict" {
			t.Fatalf("%+v: %+v", req, res)
		}
	}
	if dry, real := patchesTo(f); len(dry)+len(real) != 0 {
		t.Fatal("patched without the reviewed plan")
	}
}

// An unset managementState with a schema default is that default:
// aipipelines.argoWorkflowsControllers starts Managed, so setting it to
// Removed removes it, and the removal checks run.
func TestUnsetStateIsTheCRDDefault(t *testing.T) {
	f, c := managerWorld(t, nil)
	m, err := GetDSCManager(c)
	if err != nil {
		t.Fatal(err)
	}
	if n := nodeOf(t, m, "aipipelines.argoWorkflowsControllers"); n.SpecSet || n.SchemaDefault != "Managed" {
		t.Fatalf("node = %+v", n)
	}
	f.put(rhodsOperator, map[string]interface{}{"status": map[string]interface{}{"readyReplicas": 0}})
	plan, _ := PlanDSCChange(c, "toggle", "aipipelines.argoWorkflowsControllers", "Removed", false, false)
	if len(plan.Changes) != 1 || plan.Changes[0].Effect != "removes" || plan.Changes[0].FromSet {
		t.Fatalf("changes = %+v", plan.Changes)
	}
	if why := blockReasons(plan, "aipipelines.argoWorkflowsControllers"); !strings.Contains(why, "RHOAI operator must be running") {
		t.Fatalf("blockers = %+v", plan.Blocked)
	}
	// Unset with no schema default stays Removed: setting it Removed
	// changes the field, not what is deployed.
	_, c = managerWorld(t, func(dsc map[string]interface{}) { setPath(t, dsc, "spec.components.databaseservice", nil) })
	plan, _ = PlanDSCChange(c, "toggle", "databaseservice", "Removed", false, false)
	if len(plan.Changes) != 1 || plan.Changes[0].Effect != "" || len(plan.Blocked) != 0 {
		t.Fatalf("databaseservice plan = %+v", plan)
	}
}

// A DSC Helm installed is not changed here.
func TestHelmManagedDSCIsReadOnly(t *testing.T) {
	_, c := managerWorld(t, func(dsc map[string]interface{}) {
		setPath(t, dsc, "metadata.labels", map[string]interface{}{"app.kubernetes.io/managed-by": "Helm"})
		setPath(t, dsc, "metadata.annotations", map[string]interface{}{"meta.helm.sh/release-name": "rhoai", "meta.helm.sh/release-namespace": "rhoai-config"})
	})
	m, _ := GetDSCManager(c)
	if !strings.Contains(m.ReadOnlyReason, "managed by Helm (release rhoai-config/rhoai)") {
		t.Fatalf("read-only reason = %q", m.ReadOnlyReason)
	}
	for _, action := range []string{"toggle", "fix", "soft-reset", "hard-reset"} {
		if plan, _ := PlanDSCChange(c, action, "ray", "Removed", false, false); plan.Code != ErrorCodeGitOpsManaged {
			t.Fatalf("%s plan = %+v", action, plan)
		}
	}
}

func TestJSONPointerEscapesTildeAndSlash(t *testing.T) {
	keys := []string{"spec", "a/b", "c~d", "~1"}
	if p := pointerOf(keys); p != "/spec/a~1b/c~0d/~01" {
		t.Fatalf("pointer = %s", p)
	}
	if got := pointerKeys(pointerOf(keys)); !reflect.DeepEqual(got, keys) {
		t.Fatalf("keys = %v", got)
	}
	obj := map[string]interface{}{"spec": map[string]interface{}{"a/b": "old", "c~d": map[string]interface{}{}}}
	ops, err := buildJSONPatch(obj, []dscAssign{{Path: []string{"spec", "a/b"}, Value: "new"}, {Path: []string{"spec", "c~d", "~1"}, Value: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	after, err := applyJSONPatch(obj, ops)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := lookupPath(after, []string{"spec", "a/b"}); v != "new" {
		t.Fatalf("a/b = %v (ops %+v)", v, ops)
	}
	if v, _ := lookupPath(after, []string{"spec", "c~d", "~1"}); v != true {
		t.Fatalf("c~d/~1 = %v", v)
	}
}

// --- hard reset ---------------------------------------------------------------

// operatorRemovesDSCAt is operatorRemovesDSCAfter for the DSC at path.
func operatorRemovesDSCAt(f *resourceFake, path string, n int, then func()) {
	reads := 0
	f.beforeServe = func(method, p string) {
		if method != http.MethodGet || p != path {
			return
		}
		f.mu.Lock()
		k, _ := parseFakePath(path)
		obj := f.objects[k]
		if obj == nil || ensureMap(obj, "metadata")["deletionTimestamp"] == nil {
			f.mu.Unlock()
			return
		}
		reads++
		if reads >= n {
			delete(f.objects, k)
		}
		f.mu.Unlock()
		if reads >= n && then != nil {
			then()
		}
	}
}

func hardResetRecordOf(t *testing.T, f *resourceFake) string {
	t.Helper()
	cm := f.get(namespacedPath("v1", "configmaps", getActivityNamespace(), operationConfigMapName))
	if cm == nil {
		return ""
	}
	v, _ := ensureMap(cm, "data")[dscHardResetKey].(string)
	return v
}

func TestHardResetDeletesInTheForegroundAndClearsItsRecord(t *testing.T) {
	f, c := managerWorld(t, nil)
	operatorRemovesDSCAfter(f, 2)
	res, _ := runHardReset(t, c, nil)
	if !res.Success {
		t.Fatalf("result = %+v", res)
	}
	for _, d := range f.callsTo(http.MethodDelete, dscV3Path) {
		if !strings.Contains(d.Body, `"propagationPolicy":"Foreground"`) {
			t.Fatalf("delete not in the foreground: %s", d.Body)
		}
	}
	// The new spec was validated against the live DSC before the delete.
	dry, real := patchesTo(f)
	if len(real) != 0 || len(dry) != 1 || dry[0].Query.Get("fieldValidation") != "Strict" {
		t.Fatalf("patches = %+v %+v", dry, real)
	}
	if ops := decodeOps(t, dry[0].Body); len(ops) != 2 || ops[0]["path"] != "/metadata/uid" || ops[1]["op"] != "replace" || ops[1]["path"] != "/spec" {
		t.Fatalf("validation = %v", ops)
	}
	if rec := hardResetRecordOf(t, f); rec != "" {
		t.Fatalf("record left after success: %s", rec)
	}
}

// The API server refuses the new spec: nothing is deleted or recorded.
func TestHardResetValidatesTheNewSpecBeforeTheDelete(t *testing.T) {
	f, c := managerWorld(t, nil)
	f.failDryRun["PATCH "+dscV3Path] = http.StatusUnprocessableEntity
	f.failMessage["DRYRUN PATCH "+dscV3Path] = `admission webhook "vdatasciencecluster.kb.io" denied the request: Managed is no longer supported`
	res, _ := runHardReset(t, c, nil)
	if res.Success || !strings.Contains(res.Message, "denied the request") || !strings.Contains(res.Message, "Nothing was changed") {
		t.Fatalf("result = %+v", res)
	}
	for _, d := range f.callsTo(http.MethodDelete, dscV3Path) {
		if d.Query.Get("dryRun") == "" {
			t.Fatal("deleted the DSC")
		}
	}
	if hardResetRecordOf(t, f) != "" {
		t.Fatal("recorded a reset that did not start")
	}
}

func TestHardResetWithoutReleaseDefaultIsRefusedBeforeTheDelete(t *testing.T) {
	f, c := managerWorld(t, nil)
	csv := f.get(namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "rhods-operator.3.6.0"))
	ensureMap(csv, "metadata")["annotations"] = map[string]interface{}{"alm-examples": "[]"}
	res, _ := runHardReset(t, c, nil)
	if res.Success || res.ErrorCode != "prerequisites" || !strings.Contains(res.Message, "release default") {
		t.Fatalf("result = %+v", res)
	}
	if d := f.callsTo(http.MethodDelete, dscV3Path); len(d) != 0 {
		t.Fatalf("deletes = %+v", d)
	}
}

// Reads that fail while the DSC is deleted are retried until it is gone.
func TestHardResetWaitsThroughReadErrors(t *testing.T) {
	f, c := managerWorld(t, nil)
	reads := 0
	f.beforeServe = func(method, p string) {
		if method != http.MethodGet || p != dscV3Path {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		k, _ := parseFakePath(dscV3Path)
		if obj := f.objects[k]; obj == nil || ensureMap(obj, "metadata")["deletionTimestamp"] == nil {
			return
		}
		reads++
		switch reads {
		case 1, 2:
			f.fail["GET "+dscV3Path] = http.StatusServiceUnavailable
		default:
			delete(f.fail, "GET "+dscV3Path)
			delete(f.objects, k)
		}
	}
	res, steps := runHardReset(t, c, nil)
	if !res.Success {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(stepsText(steps), "Cannot read the DataScienceCluster now") {
		t.Fatalf("steps = %v", steps)
	}
}

func stepsText(steps []UpdateStepEvent) string {
	var b strings.Builder
	for _, s := range steps {
		b.WriteString(s.Step + ":" + s.Status + ":" + s.Message + "\n")
	}
	return b.String()
}

// The operation stops while the DSC is deleted (the lock is lost or the
// pod stops): nothing is created, the message carries the object, and the
// record lets the user finish.
func TestHardResetStoppedWhileWaitingCanBeFinished(t *testing.T) {
	f, c := managerWorld(t, func(dsc map[string]interface{}) {
		setPath(t, dsc, "spec.components.workbenches.workbenchNamespace", "my-notebooks")
	})
	plan, _ := PlanDSCChange(c, "hard-reset", "", "", false, false)
	ctx, cancel := context.WithCancelCause(context.Background())
	stopping := &Client{baseURL: c.baseURL, token: c.token, httpClient: c.httpClient, ctx: ctx}
	f.beforeServe = func(method, p string) {
		if method == http.MethodGet && p == dscV3Path {
			cancel(ErrOperationLockLost)
		}
	}
	res, err := HardResetDSC(stopping, types.DSCPlanRequest{Name: plan.Name, UID: plan.UID, Token: plan.Token, Confirm: plan.Name, KeepEnabled: boolPtr(false)}, func(UpdateStepEvent) {})
	if err != nil || res.Success || res.ErrorCode != "interrupted" || !strings.Contains(res.Message, ErrOperationLockLost.Error()) ||
		!strings.Contains(res.Message, "Finish the hard reset") || !strings.Contains(res.Message, "workbenchNamespace: my-notebooks") {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if posts := f.callsTo(http.MethodPost, dscV3List); len(posts) != 0 {
		t.Fatalf("created: %+v", posts)
	}
	f.beforeServe = nil

	// While the old DSC still exists, nothing is offered.
	if m, _ := GetDSCManager(c); m.PendingHardReset != nil {
		t.Fatalf("offered while the DSC exists: %+v", m.PendingHardReset)
	}
	if res, _ := FinishDSCHardReset(c, types.DSCFinishRequest{Name: "default-dsc", Token: strings.Repeat("0", 24)}); res.Success || res.ErrorCode != "conflict" {
		t.Fatalf("stale token = %+v", res)
	}
	// The operator removes it later.
	f.mu.Lock()
	k, _ := parseFakePath(dscV3Path)
	delete(f.objects, k)
	f.mu.Unlock()
	m, err := GetDSCManager(c)
	if err != nil || m.Exists || m.PendingHardReset == nil || m.PendingHardReset.Name != "default-dsc" || m.PendingHardReset.NewObject != plan.NewObject {
		t.Fatalf("manager = %+v, %v", m, err)
	}
	// Without a DSC the platform comes from the DSCInitialization; an
	// unknown platform is refused (fail closed).
	if res, _ := FinishDSCHardReset(c, types.DSCFinishRequest{Name: "default-dsc", Token: m.PendingHardReset.Token}); res.Success || res.ErrorCode != "managed_platform" {
		t.Fatalf("unknown platform = %+v", res)
	}
	f.put("/apis/dscinitialization.opendatahub.io/v2/dscinitializations/default-dsci", map[string]interface{}{
		"metadata": map[string]interface{}{"name": "default-dsci"}, "status": map[string]interface{}{"release": map[string]interface{}{"name": selfManagedPlatform}}})
	res, _ = FinishDSCHardReset(c, types.DSCFinishRequest{Name: "default-dsc", Token: m.PendingHardReset.Token})
	if !res.Success {
		t.Fatalf("finish = %+v", res)
	}
	created := f.get(dscV3Path)
	if v, _ := lookupPath(comps(created), []string{"workbenches", "workbenchNamespace"}); v != "my-notebooks" {
		t.Fatalf("created = %v", created)
	}
	posts := f.callsTo(http.MethodPost, dscV3List)
	if len(posts) != 2 || posts[0].Query.Get("dryRun") != "All" || posts[1].Query.Get("fieldValidation") != "Strict" {
		t.Fatalf("creates = %+v", posts)
	}
	if hardResetRecordOf(t, f) != "" {
		t.Fatal("record left after finishing")
	}
	// Finished: nothing is offered and a second finish is refused.
	if res, _ := FinishDSCHardReset(c, types.DSCFinishRequest{Name: "default-dsc", Token: m.PendingHardReset.Token}); res.Success {
		t.Fatalf("second finish = %+v", res)
	}
}

// Finish refuses when a DSC exists or the operator changed since.
func TestFinishHardResetGuards(t *testing.T) {
	f, c := managerWorld(t, nil)
	rec := &types.DSCHardResetRecord{Name: "default-dsc", OldUID: "u", APIVersion: "datasciencecluster.opendatahub.io/v3",
		Object: `{"apiVersion":"datasciencecluster.opendatahub.io/v3","kind":"DataScienceCluster","metadata":{"name":"default-dsc"},"spec":{}}`,
		YAML:   "kind: DataScienceCluster\n", OperatorCSV: "rhods-operator.3.5.0", StartedAt: "2026-11-19T00:00:00Z"}
	if err := SaveDSCHardResetRecord(c, rec); err != nil {
		t.Fatal(err)
	}
	token := pendingHardResetToken(rec)
	if res, _ := FinishDSCHardReset(c, types.DSCFinishRequest{Name: "default-dsc", Token: token}); res.Success || !strings.Contains(res.Message, "default-dsc now exists") {
		t.Fatalf("with a DSC = %+v", res)
	}
	f.mu.Lock()
	k, _ := parseFakePath(dscV3Path)
	delete(f.objects, k)
	f.mu.Unlock()
	if res, _ := FinishDSCHardReset(c, types.DSCFinishRequest{Name: "default-dsc", Token: token}); res.Success || !strings.Contains(res.Message, "not rhods-operator.3.5.0") {
		t.Fatalf("operator changed = %+v", res)
	}
	if posts := f.callsTo(http.MethodPost, dscV3List); len(posts) != 0 {
		t.Fatalf("created: %+v", posts)
	}
}

// A new DSC (a new UID under the same name) appears while the old one is
// deleted: the reset creates nothing.
func TestHardResetStopsWhenANewDSCAppearsDuringTheWait(t *testing.T) {
	f, c := managerWorld(t, nil)
	f.beforeServe = func(method, p string) {
		if method != http.MethodGet || p != dscV3Path {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		k, _ := parseFakePath(dscV3Path)
		if obj := f.objects[k]; obj != nil && ensureMap(obj, "metadata")["deletionTimestamp"] != nil {
			f.objects[k] = map[string]interface{}{"apiVersion": "datasciencecluster.opendatahub.io/v3", "kind": "DataScienceCluster", "metadata": map[string]interface{}{"name": "default-dsc", "uid": "someone-elses"}}
		}
	}
	res, _ := runHardReset(t, c, nil)
	if res.Success || res.ErrorCode != "conflict" || !strings.Contains(res.Message, "UID someone-elses") || !strings.Contains(res.Message, "kind: DataScienceCluster") {
		t.Fatalf("result = %+v", res)
	}
	if posts := f.callsTo(http.MethodPost, dscV3List); len(posts) != 0 {
		t.Fatalf("created: %+v", posts)
	}
}

// Another DataScienceCluster (another name) exists once the old one is
// gone: nothing is created.
func TestHardResetListsDSCsBeforeTheCreate(t *testing.T) {
	f, c := managerWorld(t, nil)
	operatorRemovesDSCAt(f, dscV3Path, 1, func() {
		f.put(dscV3List+"/someone-else", map[string]interface{}{"apiVersion": "datasciencecluster.opendatahub.io/v3", "kind": "DataScienceCluster", "metadata": map[string]interface{}{"name": "someone-else"}})
	})
	res, _ := runHardReset(t, c, nil)
	if res.Success || res.ErrorCode != "conflict" || !strings.Contains(res.Message, "DataScienceCluster someone-else now exists") {
		t.Fatalf("result = %+v", res)
	}
	if posts := f.callsTo(http.MethodPost, dscV3List); len(posts) != 0 {
		t.Fatalf("created: %+v", posts)
	}
}

// A DSC that is not named default-dsc keeps its name through the reset.
func TestHardResetOfANonDefaultName(t *testing.T) {
	f, c := managerWorld(t, func(dsc map[string]interface{}) { setPath(t, dsc, "metadata.name", "my-dsc") })
	f.mu.Lock()
	k, _ := parseFakePath(dscV3Path)
	obj := f.objects[k]
	delete(f.objects, k)
	f.mu.Unlock()
	path := dscV3List + "/my-dsc"
	f.put(path, obj)
	operatorRemovesDSCAt(f, path, 2, nil)
	res, _ := runHardReset(t, c, nil)
	if !res.Success {
		t.Fatalf("result = %+v", res)
	}
	if d := f.callsTo(http.MethodDelete, path); len(d) != 2 {
		t.Fatalf("deletes = %+v", d)
	}
	created := f.get(path)
	if created == nil || ensureMap(created, "metadata")["name"] != "my-dsc" {
		t.Fatalf("created = %v", created)
	}
	var body map[string]interface{}
	posts := f.callsTo(http.MethodPost, dscV3List)
	_ = json.Unmarshal([]byte(posts[len(posts)-1].Body), &body)
	if ensureMap(body, "metadata")["name"] != "my-dsc" {
		t.Fatalf("create body = %s", posts[len(posts)-1].Body)
	}
}

// A module the tool cannot check blocks its removal (fail closed): no kind
// is named after it, or its CR exists and the tool does not know its
// operator. The operator Deployment comes from the installed CSV.
func TestRemovalChecksFailClosedOnModulesTheToolCannotCheck(t *testing.T) {
	f, c := newFakeAPI(t)
	serveInstalledOperator(f)
	serveComponentGroup(f)
	f.json("GET", "/apis/apps/v1/namespaces/redhat-ods-operator/deployments/rhods-operator", http.StatusOK, `{"status":{"readyReplicas":1}}`)
	f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1", http.StatusOK, `{"resources":[{"name":"sparkoperators","kind":"SparkOperator"},{"name":"rays","kind":"Ray"}]}`)
	f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1/sparkoperators", http.StatusOK, `{"items":[{"metadata":{"name":"default-sparkoperator"}}]}`)
	f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1/rays", http.StatusOK, `{"items":[]}`)
	rc := newRemovalChecker(c)
	if got := strings.Join(rc.partBlockers("data"), " "); !strings.Contains(got, "no components.platform.opendatahub.io kind is named after data") {
		t.Fatalf("unknown kind: %q", got)
	}
	if got := strings.Join(rc.moduleBlockers("sparkoperator"), " "); !strings.Contains(got, "does not know which operator") {
		t.Fatalf("unknown operator: %q", got)
	}
	if b, _ := rc.teardownBlockers("sparkoperator"); len(b) == 0 {
		t.Fatal("teardown of an unknown operator's module passed")
	}
	if got := rc.moduleBlockers("ray"); len(got) != 0 {
		t.Fatalf("ray (no CR, operator known) = %v", got)
	}
	// The CSV names no Deployment: blocked, not assumed.
	f2, c2 := newFakeAPI(t)
	f2.json("GET", namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName), 200, `{"status":{"installedCSV":"rhods-operator.3.6.0"}}`)
	f2.json("GET", namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "rhods-operator.3.6.0"), 200, `{"metadata":{"name":"rhods-operator.3.6.0"},"spec":{"version":"3.6.0"}}`)
	f2.json("GET", "/apis/apps/v1/namespaces/redhat-ods-operator/deployments/rhods-operator", http.StatusOK, `{"status":{"readyReplicas":1}}`)
	if why := operatorDeploymentsBlocker(c2); !strings.Contains(why, "lists no Deployment") {
		t.Fatalf("no deployment = %q", why)
	}
}

// A module the reset leaves Removed gets the rules of removing it (a
// conversion webhook blocks); one it brings back only warns.
func TestHardResetChecksModulesThatStayRemovedAsRemovals(t *testing.T) {
	f, c := managerWorld(t, nil)
	putConversionCRD(f, "gateways.aigateway.opendatahub.io", "ai-gateway-webhook-service")
	putConversionCRD(f, "guardrails.trustyai.opendatahub.io", "trustyai-webhook-service")
	plan, _ := PlanDSCChange(c, "hard-reset", "", "", false, false)
	if why := blockReasons(plan, "aigateway"); !strings.Contains(why, "oc get gateways.aigateway.opendatahub.io -A") {
		t.Fatalf("aigateway stays Removed and serves a conversion webhook: blocked = %+v", plan.Blocked)
	}
	if blockReasons(plan, "trustyai") != "" || !strings.Contains(strings.Join(plan.Warnings, " "), "While trustyai is down") {
		t.Fatalf("trustyai comes back: blocked = %+v, warnings = %v", plan.Blocked, plan.Warnings)
	}
}
