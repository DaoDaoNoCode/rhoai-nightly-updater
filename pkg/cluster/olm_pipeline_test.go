package cluster

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const testNightlyImage = "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:new"

func cond(typ, reason, msg string) map[string]interface{} {
	return map[string]interface{}{"type": typ, "status": "True", "reason": reason, "message": msg}
}

// A01-2: success is reported only once OLM installed the CSV; every OLM
// failure is a failure and restores the previous operator.
func TestUpdateStream_WaitsForOLMOutcome(t *testing.T) {
	const newCSV = "rhods-operator.3.6.1"
	for _, tc := range []struct {
		name        string
		onSubscribe func(*fakeOLM, map[string]interface{})
		wantSuccess bool
		wantCode    string
		wantRestore bool
		wantInMsg   string
	}{
		{name: "installed", onSubscribe: olmInstalls(newCSV), wantSuccess: true, wantInMsg: newCSV + " is installed"},
		{name: "resolution failed", wantCode: "resolution_failed", wantRestore: true, wantInMsg: "constraints not satisfiable",
			onSubscribe: func(f *fakeOLM, _ map[string]interface{}) {
				f.sub["status"] = map[string]interface{}{"conditions": []interface{}{cond("ResolutionFailed", "ConstraintsNotSatisfiable", "constraints not satisfiable")}}
			}},
		{name: "bundle unpack failed", wantCode: "bundle_unpack_failed", wantRestore: true, wantInMsg: "bundle-unpack Job",
			onSubscribe: func(f *fakeOLM, _ map[string]interface{}) {
				f.sub["status"] = map[string]interface{}{"conditions": []interface{}{cond("BundleUnpackFailed", "BundleUnpackFailed", "DeadlineExceeded")}}
			}},
		{name: "install plan failed", wantCode: "installplan_failed", wantRestore: true, wantInMsg: "InstallComponentFailed",
			onSubscribe: func(f *fakeOLM, _ map[string]interface{}) {
				f.installPlans["install-new"] = map[string]interface{}{"spec": map[string]interface{}{}, "status": map[string]interface{}{"phase": "Failed",
					"conditions": []interface{}{map[string]interface{}{"type": "Installed", "status": "False", "reason": "InstallComponentFailed", "message": "CRD step failed"}}}}
				f.sub["status"] = map[string]interface{}{"installPlanRef": map[string]interface{}{"name": "install-new"}}
			}},
		{name: "csv failed", wantCode: "csv_failed", wantRestore: true, wantInMsg: "InstallCheckFailed",
			onSubscribe: func(f *fakeOLM, _ map[string]interface{}) {
				f.installPlans["install-new"] = map[string]interface{}{"spec": map[string]interface{}{}, "status": map[string]interface{}{"phase": "Complete"}}
				f.addCSV(newCSV, "Failed")
				f.csvs[newCSV]["status"] = map[string]interface{}{"phase": "Failed", "reason": "InstallCheckFailed", "message": "install timeout"}
				f.sub["status"] = map[string]interface{}{"currentCSV": newCSV, "installedCSV": newCSV, "installPlanRef": map[string]interface{}{"name": "install-new"}}
			}},
		{name: "OLM never acts", wantCode: "install_timeout", wantRestore: true, wantInMsg: "did not finish installing",
			onSubscribe: func(*fakeOLM, map[string]interface{}) {}},
		{name: "still installing at timeout", wantCode: "install_timeout", wantRestore: false, wantInMsg: "kept because OLM is still installing",
			onSubscribe: func(f *fakeOLM, _ map[string]interface{}) {
				f.installPlans["install-new"] = map[string]interface{}{"spec": map[string]interface{}{}, "status": map[string]interface{}{"phase": "Installing"}}
				f.addCSV(newCSV, "Installing")
				f.sub["status"] = map[string]interface{}{"currentCSV": newCSV, "installPlanRef": map[string]interface{}{"name": "install-new"}}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeOLM(t).installed("rhods-operator.3.6.0", map[string]interface{}{"channel": "fast"})
			f.channels = `[{"name":"stable-3.x","currentCSV":"` + newCSV + `"}]`
			f.onSubscribe = func(f *fakeOLM, spec map[string]interface{}) {
				if spec["channel"] != "fast" { // the restored Subscription is not the attempt
					tc.onSubscribe(f, spec)
				}
			}
			emit, events := eventsRecorder()
			result, err := UpdateStream(f.client(context.Background()), testNightlyImage, emit)
			if err != nil || result == nil {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if result.Success != tc.wantSuccess || result.ErrorCode != tc.wantCode || !strings.Contains(result.Message, tc.wantInMsg) {
				t.Fatalf("success=%v code=%q message=%q", result.Success, result.ErrorCode, result.Message)
			}
			ev := events()
			wantVerify := "failed"
			if tc.wantSuccess {
				wantVerify = "success"
			}
			if got := lastStatus(ev, "verify_installplan"); got != wantVerify {
				t.Fatalf("verify_installplan ended %q, want %q", got, wantVerify)
			}
			restored := lastStatus(ev, restoreStepName) == "success"
			if restored != tc.wantRestore {
				t.Fatalf("restore ran=%v, want %v (events %v)", restored, tc.wantRestore, ev)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			spec, _ := f.sub["spec"].(map[string]interface{})
			if tc.wantRestore {
				image, _ := f.catalog["spec"].(map[string]interface{})["image"].(string)
				if spec["channel"] != "fast" || !strings.HasSuffix(image, "@sha256:old") {
					t.Fatalf("previous state not restored: sub=%v catalog=%v", spec, image)
				}
				if _, ok := f.csvs[newCSV]; ok {
					t.Fatalf("CSV %s from the failed attempt was not removed", newCSV)
				}
			} else if spec["channel"] != "stable-3.x" {
				t.Fatalf("new Subscription not kept: %v", spec)
			}
		})
	}
}

// A01-14: the Subscription is removed before the catalog is swapped and
// created once, after the old CSV is gone.
func TestUpdateStream_OrderAndSingleSubscriptionApply(t *testing.T) {
	f := newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
	f.onSubscribe = olmInstalls("rhods-operator.3.6.0")
	result, err := UpdateStream(f.client(context.Background()), testNightlyImage, func(UpdateStepEvent) {})
	if err != nil || !result.Success {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	subPath := subscriptionPath()
	csPath := namespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, CatalogName)
	idx := map[string]int{}
	for i, w := range f.writes() {
		if _, seen := idx[w]; !seen {
			idx[w] = i + 1
		}
	}
	order := []string{"DELETE " + subPath, "DELETE " + csPath, "PATCH " + csPath, "DELETE " + csvPath("rhods-operator.3.6.0"), "PATCH " + subPath}
	for i := 1; i < len(order); i++ {
		if idx[order[i-1]] == 0 || idx[order[i]] == 0 || idx[order[i-1]] > idx[order[i]] {
			t.Fatalf("expected %q before %q; writes: %v", order[i-1], order[i], f.writes())
		}
	}
	if n := len(f.subscriptionApplies()); n != 1 {
		t.Fatalf("Subscription applied %d times, want 1", n)
	}
}

// A01-3: spec.config and Manual approval survive Update, Refresh and
// Reinstall; the generated InstallPlan for RHOAI is approved once.
func TestOperationsPreserveSubscriptionSettings(t *testing.T) {
	config := map[string]interface{}{"env": []interface{}{map[string]interface{}{"name": "DISABLE_DSC_CONFIG", "value": "true"}}, "nodeSelector": map[string]interface{}{"node-role.kubernetes.io/infra": ""}}
	for _, op := range []string{"update", "refresh", "reinstall"} {
		t.Run(op, func(t *testing.T) {
			f := newFakeOLM(t).installed("rhods-operator.3.6.0", map[string]interface{}{"installPlanApproval": "Manual", "config": config, "startingCSV": "rhods-operator.3.5.0"})
			f.onSubscribe = func(f *fakeOLM, _ map[string]interface{}) {
				f.installPlans["install-new"] = map[string]interface{}{"spec": map[string]interface{}{"approved": false, "clusterServiceVersionNames": []interface{}{"rhods-operator.3.6.0"}}, "status": map[string]interface{}{"phase": "RequiresApproval"}}
				f.sub["status"] = map[string]interface{}{"currentCSV": "rhods-operator.3.6.0", "installPlanRef": map[string]interface{}{"name": "install-new"}}
			}
			f.onApprove = func(f *fakeOLM, ip string) {
				olmInstalls("rhods-operator.3.6.0")(f, nil)
			}
			c := f.client(context.Background())
			var ok bool
			var msg string
			switch op {
			case "update":
				r, err := UpdateStream(c, testNightlyImage, func(UpdateStepEvent) {})
				ok, msg = err == nil && r.Success, fmt.Sprint(r, err)
			case "refresh":
				r, err := RefreshOperator(c)
				ok, msg = err == nil && r.Success, fmt.Sprint(r, err)
				if !strings.Contains(r.Message, "same version") {
					t.Errorf("refresh message should say it re-deploys the same version: %s", r.Message)
				}
			case "reinstall":
				r, err := Reinstall(c, "nightly", testNightlyImage, "")
				ok, msg = err == nil && r.Success, fmt.Sprint(r, err)
			}
			if !ok {
				t.Fatalf("%s failed: %s", op, msg)
			}
			applies := f.subscriptionApplies()
			if len(applies) == 0 {
				t.Fatal("no Subscription applied")
			}
			spec := applies[len(applies)-1]
			if spec["installPlanApproval"] != "Manual" || fmt.Sprint(spec["config"]) != fmt.Sprint(config) || spec["startingCSV"] != nil {
				t.Fatalf("settings not preserved: %v", spec)
			}
			approved := false
			for _, w := range f.writes() {
				approved = approved || w == "PATCH "+namespacedPath("operators.coreos.com/v1alpha1", "installplans", SubNS, "install-new")
			}
			if !approved {
				t.Fatalf("InstallPlan was not approved: %v", f.writes())
			}
		})
	}
}

func TestManualApprovalNotGivenForOtherOperators(t *testing.T) {
	f := newFakeOLM(t).installed("rhods-operator.3.6.0", map[string]interface{}{"installPlanApproval": "Manual"})
	f.onSubscribe = func(f *fakeOLM, _ map[string]interface{}) {
		f.installPlans["install-new"] = map[string]interface{}{"spec": map[string]interface{}{"clusterServiceVersionNames": []interface{}{"rhods-operator.3.6.0", "other-operator.v1.0.0"}}, "status": map[string]interface{}{"phase": "RequiresApproval"}}
		f.sub["status"] = map[string]interface{}{"installPlanRef": map[string]interface{}{"name": "install-new"}}
	}
	result, err := UpdateStream(f.client(context.Background()), testNightlyImage, func(UpdateStepEvent) {})
	if err != nil || result.Success || result.ErrorCode != "approval_required" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, w := range f.writes() {
		if strings.Contains(w, "/installplans/install-new") {
			t.Fatalf("approved a plan that installs another operator: %v", w)
		}
	}
}

// A01-1 / A10-5: the downgrade guard compares the bundle the target catalog
// installs (its channel head), not the minor-stream tag.
func TestUpdateStream_DowngradeUsesTargetBundle(t *testing.T) {
	for _, tc := range []struct {
		installed, head string
		blocked         bool
	}{
		{"rhods-operator.3.5.1", "rhods-operator.3.5.2", false}, // GA 3.5.1 -> 3.5 nightly shipping 3.5.2
		{"rhods-operator.3.5.2", "rhods-operator.3.5.2", false}, // same z-stream build again
		{"rhods-operator.3.6.0", "rhods-operator.3.5.2", true},  // real downgrade
	} {
		t.Run(tc.installed+"->"+tc.head, func(t *testing.T) {
			f := newFakeOLM(t).installed(tc.installed, nil)
			f.channels = `[{"name":"stable-3.x","currentCSV":"` + tc.head + `"}]`
			f.onSubscribe = olmInstalls(tc.head)
			result, err := UpdateStream(f.client(context.Background()), "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5@sha256:x", func(UpdateStepEvent) {})
			if err != nil {
				t.Fatal(err)
			}
			if tc.blocked {
				if result.Success || result.ErrorCode != "validation" || !strings.Contains(result.Message, "Downgrade detected") {
					t.Fatalf("expected a blocked downgrade, got %+v", result)
				}
				for _, w := range f.writes() {
					if !strings.Contains(w, "-verify-") {
						t.Fatalf("blocked downgrade changed the cluster: %v", f.writes())
					}
				}
				return
			}
			if !result.Success {
				t.Fatalf("valid z-stream update refused: %s", result.Message)
			}
		})
	}
}

func TestDowngradeCheck(t *testing.T) {
	for _, tc := range []struct {
		installed, tag, bundle string
		blocked                bool
	}{
		{"rhods-operator.3.5.2", "rhoai-3.5", "", false},
		{"rhods-operator.3.5.1", "rhoai-3.5", "", false},
		{"rhods-operator.3.6.0", "rhoai-3.5", "", true},
		{"rhods-operator.3.5.0", "rhoai-3.5-ea.1", "", true},
		{"rhods-operator.3.5.0-ea.1", "rhoai-3.5", "", false},
		{"rhods-operator.3.5.2", "rhoai-3.5.1", "", true},
		{"rhods-operator.3.6.0", "rhoai-3.5", "rhods-operator.3.6.1", false}, // bundle wins over tag
		{"rhods-operator.3.5.2", "rhoai-3.5", "rhods-operator.3.5.1", true},
		{"rhods-operator.3.6.0", "nightly-latest", "", false}, // unknown: warn, do not block
		{"", "rhoai-3.5", "", false},
	} {
		blocked, _ := downgradeCheck(types.CSVInfo{Name: tc.installed}, tc.tag, tc.bundle)
		if (blocked != "") != tc.blocked {
			t.Errorf("%s -> %s (bundle %q): blocked=%q, want %v", tc.installed, tc.tag, tc.bundle, blocked, tc.blocked)
		}
	}
}

// A10-5: the dry run asks the API server (dryRun=All) and fails when the
// server rejects the CatalogSource; the downgrade rule matches the real run.
func TestUpdateDryRun(t *testing.T) {
	oldLookup, oldQuay := lookupTargetBundle, quayHTTPClient
	t.Cleanup(func() { lookupTargetBundle, quayHTTPClient = oldLookup, oldQuay })
	quayHTTPClient = &http.Client{Transport: dscSampleTransport(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("offline")
	})}
	for _, tc := range []struct {
		name, installed, bundle string
		bundleErr               bool
		reject                  bool
		wantSuccess             bool
		wantCode                string
	}{
		{name: "valid", installed: "rhods-operator.3.5.1", bundle: "rhods-operator.3.5.2", wantSuccess: true},
		{name: "server rejects", installed: "rhods-operator.3.5.1", bundle: "rhods-operator.3.5.2", reject: true, wantCode: "validation"},
		{name: "downgrade", installed: "rhods-operator.3.6.0", bundle: "rhods-operator.3.5.2", wantCode: "validation"},
		{name: "bundle unknown, same minor", installed: "rhods-operator.3.5.2", bundleErr: true, wantSuccess: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookupTargetBundle = func(*Client, string) (string, error) {
				if tc.bundleErr {
					return "", fmt.Errorf("registry unavailable")
				}
				return tc.bundle, nil
			}
			f := newFakeOLM(t).installed(tc.installed, nil)
			f.rejectDryRun = tc.reject
			result, err := Update(f.client(context.Background()), "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5@sha256:x", true)
			if err != nil || result.Success != tc.wantSuccess || result.ErrorCode != tc.wantCode {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			dryRunSeen := false
			f.mu.Lock()
			for _, r := range f.requests {
				if r.Method != http.MethodGet && r.Query.Get("dryRun") == "" && !strings.Contains(r.Path, "/configmaps") {
					t.Errorf("dry run changed the cluster: %s %s", r.Method, r.Path)
				}
				dryRunSeen = dryRunSeen || (r.Method == http.MethodPatch && r.Query.Get("dryRun") == "All")
			}
			f.mu.Unlock()
			if tc.name != "downgrade" && !dryRunSeen {
				t.Fatal("no server-side dry run was sent")
			}
		})
	}
}

// A01-7: a catalog image that cannot be pulled fails fast with the kubelet
// reason instead of waiting for the READY timeout.
func TestUpdateStream_CatalogImagePullFailsFast(t *testing.T) {
	f := newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
	f.verifyState = "TRANSIENT_FAILURE"
	f.catalogPods = `{"items":[{"status":{"containerStatuses":[{"state":{"waiting":{"reason":"ImagePullBackOff","message":"Back-off pulling image: unauthorized: access to the requested resource is not authorized"}}}]}}]}`
	old := CatalogReadyTimeout
	CatalogReadyTimeout = 10 * time.Second
	t.Cleanup(func() { CatalogReadyTimeout = old })
	start := time.Now()
	result, err := UpdateStream(f.client(context.Background()), testNightlyImage, func(UpdateStepEvent) {})
	if err != nil || result.Success || result.ErrorCode != "catalog_image_pull" || !strings.Contains(result.Message, "unauthorized") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("pull failure was not detected early (%s)", time.Since(start))
	}
	for _, w := range f.writes() {
		if !strings.Contains(w, "-verify-") {
			t.Fatalf("operator changed after a failed preflight: %v", f.writes())
		}
	}
}

// A01-13: verification catalogs carry labels so leftovers can be removed.
func TestPreflightLabelsVerificationCatalog(t *testing.T) {
	f := newFakeOLM(t)
	if _, err := preflightReinstallCatalog(f.client(context.Background()), testNightlyImage, ""); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if r.Method == http.MethodPatch && strings.Contains(r.Path, "-verify-") {
			labels, _ := r.Body["metadata"].(map[string]interface{})["labels"].(map[string]interface{})
			if labels[catalogPurposeLabel] != verificationCatalogRole || labels[managedByLabel] != managedByValue {
				t.Fatalf("labels = %v", labels)
			}
			return
		}
	}
	t.Fatal("verification catalog not created")
}

// A01-12: an unusable existing OperatorGroup stops the operation before any
// change; a missing one is created.
func TestEnsureOperatorNamespaceAndGroup(t *testing.T) {
	for _, tc := range []struct {
		name    string
		groups  []interface{}
		problem string
		creates bool
	}{
		{"global group", []interface{}{map[string]interface{}{"metadata": map[string]interface{}{"name": "og"}, "spec": map[string]interface{}{}}}, "", false},
		{"two groups", []interface{}{map[string]interface{}{"metadata": map[string]interface{}{"name": "a"}}, map[string]interface{}{"metadata": map[string]interface{}{"name": "b"}}}, "TooManyOperatorGroups", false},
		{"own namespace group", []interface{}{map[string]interface{}{"metadata": map[string]interface{}{"name": "og"}, "spec": map[string]interface{}{"targetNamespaces": []interface{}{SubNS}}}}, "AllNamespaces", false},
		{"missing", nil, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeOLM(t)
			f.operatorGrps = tc.groups
			_, problem, _ := ensureOperatorNamespaceAndGroup(f.client(context.Background()), false)
			if (tc.problem == "") != (problem == "") || !strings.Contains(problem, tc.problem) {
				t.Fatalf("problem = %q, want %q", problem, tc.problem)
			}
			created := len(f.writes()) > 0
			if created != tc.creates {
				t.Fatalf("writes = %v", f.writes())
			}
		})
	}
}

// Reinstall must not run with an unusable OperatorGroup (A01-12).
func TestReinstallRefusesBadOperatorGroup(t *testing.T) {
	f := newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
	f.operatorGrps = append(f.operatorGrps, map[string]interface{}{"metadata": map[string]interface{}{"name": "second"}})
	result, err := Reinstall(f.client(context.Background()), "nightly", testNightlyImage, "")
	if err != nil || result.Success || result.ErrorCode != "prerequisites" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, w := range f.writes() {
		if !strings.Contains(w, "-verify-") {
			t.Fatalf("changed the cluster: %v", f.writes())
		}
	}
}

// A01-6 and §8 #8: stable reinstall handles an unhealthy install on the same
// channel and refuses an unconfirmed downgrade.
func TestReinstallStableTargets(t *testing.T) {
	t.Setenv("STABLE_SOURCE", "redhat-operators")
	t.Setenv("STABLE_CHANNEL", "")
	stable := `[{"name":"stable-3.x","currentCSV":"rhods-operator.3.5.1","currentCSVDesc":{"version":"3.5.1"}}]`
	for _, tc := range []struct {
		name, installed, phase, source string
		allow                          bool
		wantSuccess                    bool
		wantCode, wantMsg              string
		wantWrites                     bool
	}{
		{name: "healthy stable", installed: "rhods-operator.3.5.1", phase: "Succeeded", source: "redhat-operators", wantSuccess: true, wantMsg: "Already on stable"},
		{name: "failed stable is reinstalled", installed: "rhods-operator.3.5.1", phase: "Failed", source: "redhat-operators", wantSuccess: true, wantMsg: "is installed", wantWrites: true},
		{name: "nightly to older GA refused", installed: "rhods-operator.3.6.0", phase: "Succeeded", source: CatalogName, wantCode: errorCodeDowngrade, wantMsg: "older than the installed"},
		{name: "nightly to older GA confirmed", installed: "rhods-operator.3.6.0", phase: "Succeeded", source: CatalogName, allow: true, wantSuccess: true, wantMsg: "is installed", wantWrites: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeOLM(t).installed(tc.installed, map[string]interface{}{"source": tc.source})
			f.csvs[tc.installed]["status"] = map[string]interface{}{"phase": tc.phase}
			f.stableChans = stable
			f.onSubscribe = olmInstalls("rhods-operator.3.5.1")
			result, err := ReinstallWithOptions(f.client(context.Background()), "stable", "", "", OperationOptions{AllowDowngrade: tc.allow})
			if err != nil || result.Success != tc.wantSuccess || result.ErrorCode != tc.wantCode || !strings.Contains(result.Message, tc.wantMsg) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if wrote := len(f.writes()) > 0; wrote != tc.wantWrites {
				t.Fatalf("writes=%v", f.writes())
			}
		})
	}
}

// §8 #1/#2 (D1): no OLM operation runs while a Dashboard Dev session has
// paused dashboard-operator, unless the caller asks to end it first.
func TestOperationsRefuseActiveDashboardDev(t *testing.T) {
	for _, op := range []string{"update", "reinstall", "refresh"} {
		t.Run(op, func(t *testing.T) {
			f := newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
			f.dashboardOp = `{"metadata":{"uid":"u1","annotations":{"` + dashboardDevAnnotation + `":"{}"}},"spec":{"replicas":0}}`
			c := f.client(context.Background())
			var code string
			switch op {
			case "update":
				r, _ := UpdateStream(c, testNightlyImage, func(UpdateStepEvent) {})
				code = r.ErrorCode
			case "reinstall":
				r, _ := Reinstall(c, "nightly", testNightlyImage, "")
				code = r.ErrorCode
			case "refresh":
				r, _ := RefreshOperator(c)
				code = r.ErrorCode
			}
			if code != errorCodeDashboardDevActive {
				t.Fatalf("errorCode = %q", code)
			}
			if w := f.writes(); len(w) > 0 {
				t.Fatalf("changed the cluster: %v", w)
			}
		})
	}
}

// A01-10: every failure marks the running step failed, keeps the logs and
// restores; a cancelled operation returns a result, not a bare error.
func TestFailuresMarkStepAndKeepLogs(t *testing.T) {
	t.Run("delete fails mid-way", func(t *testing.T) {
		f := newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
		f.failDelete[csvPath("rhods-operator.3.6.0")] = 1
		emit, events := eventsRecorder()
		result, err := UpdateStream(f.client(context.Background()), testNightlyImage, emit)
		if err != nil || result.Success || !strings.Contains(result.Message, "Cannot delete current CSV") {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		ev := events()
		if lastStatus(ev, "delete_csv") != "failed" || lastStatus(ev, restoreStepName) != "success" {
			t.Fatalf("events = %v", ev)
		}
		if !strings.Contains(result.Message, "installed operator was not changed") {
			t.Fatalf("restore message = %q", result.Message)
		}
	})
	t.Run("cancelled while waiting for OLM", func(t *testing.T) {
		f := newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
		ctx, cancel := context.WithCancel(context.Background())
		f.onSubscribe = func(*fakeOLM, map[string]interface{}) { go func() { time.Sleep(30 * time.Millisecond); cancel() }() }
		emit, events := eventsRecorder()
		result, err := UpdateStream(f.client(ctx), testNightlyImage, emit)
		if err != nil || result == nil || result.Success || result.ErrorCode != "cancelled" || len(result.Logs) == 0 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if lastStatus(events(), "verify_installplan") != "failed" || lastStatus(events(), restoreStepName) != "success" {
			t.Fatalf("events = %v", events())
		}
	})
}

// A01-9: when the swapped catalog has no matching channel, the catalog and
// Subscription are restored and the message says the operator was kept.
func TestUpdateStream_ChannelDetectionFailureRestores(t *testing.T) {
	f := newFakeOLM(t).installed("rhods-operator.3.6.0", map[string]interface{}{"channel": "fast"})
	empty := `[]`
	f.mainChannels = &empty
	result, err := UpdateStream(f.client(context.Background()), testNightlyImage, func(UpdateStepEvent) {})
	if err != nil || result.Success || result.ErrorCode != "channel_detection" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !strings.Contains(result.Message, "installed operator was not changed") {
		t.Fatalf("message = %q", result.Message)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sub == nil || f.sub["spec"].(map[string]interface{})["channel"] != "fast" || f.csvs["rhods-operator.3.6.0"] == nil {
		t.Fatalf("sub=%v csvs=%v", f.sub, f.csvs)
	}
}

func TestAdminAckReported(t *testing.T) {
	f := newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
	f.onSubscribe = olmInstalls("rhods-operator.3.6.0")
	f.platform = `{"status":{"conditions":[{"type":"ProvisioningProgress","status":"False","reason":"AdminAckRequired","message":"gate kserve-3.7 not acknowledged"}]}}`
	result, err := UpdateStream(f.client(context.Background()), testNightlyImage, func(UpdateStepEvent) {})
	if err != nil || !result.Success || !strings.Contains(result.Message, "AdminAckRequired") || !strings.Contains(result.Message, "odh-upgrade-acks") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

// A same-name CSV that is still being deleted is not the new install.
func TestWaitIgnoresDeletingCSV(t *testing.T) {
	f := newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
	f.csvs["rhods-operator.3.6.0"]["metadata"] = map[string]interface{}{"name": "rhods-operator.3.6.0", "deletionTimestamp": "2026-10-05T00:00:00Z"}
	var logs []string
	outcome := waitForOperatorInstall(f.client(context.Background()), "verify_installplan", func(UpdateStepEvent) {}, &logs, &operatorRecovery{})
	if outcome.succeeded || outcome.errorCode != "install_timeout" {
		t.Fatalf("outcome = %+v", outcome)
	}
}
