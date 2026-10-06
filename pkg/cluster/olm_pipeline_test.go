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
			// Refresh pins the installed CSV (R5-F3); the others drop the
			// old channel's startingCSV.
			wantStart := interface{}(nil)
			if op == "refresh" {
				wantStart = "rhods-operator.3.6.0"
			}
			if spec["installPlanApproval"] != "Manual" || fmt.Sprint(spec["config"]) != fmt.Sprint(config) || spec["startingCSV"] != wantStart {
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
		{"rhods-operator.3.6.0", "nightly-latest", "", true},                 // target unknown: fail closed
		{"rhods-operator.custom", "rhoai-3.6", "rhods-operator.3.6.0", true}, // installed unknown: fail closed
		{"", "rhoai-3.5", "", false},                                         // nothing installed
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

// Review fix 2: an operation whose cross-pod lock was lost stops, and the
// automatic restore does not run (another pod may be changing the same
// Subscription and catalog).
func TestLostLockSkipsTheRestore(t *testing.T) {
	f := newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	writesAtLoss := -1
	f.onSubscribe = func(f *fakeOLM, _ map[string]interface{}) {
		// The heartbeat saw another holder right after the Subscription
		// was created; OLM never installs.
		for _, r := range f.requests {
			if r.Method != http.MethodGet && !strings.Contains(r.Path, "/configmaps") {
				writesAtLoss++
			}
		}
		writesAtLoss++ // this Subscription write
		cancel(ErrOperationLockLost)
	}
	var events []UpdateStepEvent
	result, err := UpdateStream(f.client(ctx), testNightlyImage, func(e UpdateStepEvent) { events = append(events, e) })
	if err != nil || result.Success || result.ErrorCode != "lock_lost" || result.Message != LockLostMessage {
		t.Fatalf("result %+v err %v", result, err)
	}
	if writesAtLoss < 0 {
		t.Fatal("the Subscription was never created")
	}
	if after := f.writes()[writesAtLoss:]; len(after) != 0 {
		t.Fatalf("cluster writes after the lock was lost: %v", after)
	}
	skipped := false
	for _, e := range events {
		skipped = skipped || (e.Step == restoreStepName && e.Status == "skipped" && e.ErrorCode == "lock_lost")
	}
	if !skipped || !strings.Contains(strings.Join(result.Logs, "\n"), "Automatic operator recovery skipped") {
		t.Fatalf("restore not reported as skipped: events %+v", events)
	}
	// Bookkeeping of the updater itself still happens; cluster cleanup does not.
	if c := f.client(ctx); !operationLockLost(c) {
		t.Fatal("cause not seen")
	}
	pctx, pcancel := postOperationContext(f.client(ctx), time.Minute)
	defer pcancel()
	if pctx.Err() == nil {
		t.Fatal("postOperationContext is usable after the lock was lost")
	}
	bctx, bcancel := bookkeepingContext(f.client(ctx), time.Minute)
	defer bcancel()
	if bctx.Err() != nil {
		t.Fatal("bookkeeping context cancelled")
	}
}

// E3: an operator that stops at upgrade gates reports them on the DSC (a
// 3.5 operator does not update the Platform). The note reads any DSC
// condition, waits a bounded time for the new operator to report, and stays
// quiet when nothing is gated.
func TestUpgradeGateNoteFromTheDSC(t *testing.T) {
	oldWait, oldPoll := upgradeGateWait, upgradeGatePoll
	upgradeGateWait, upgradeGatePoll = 300*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { upgradeGateWait, upgradeGatePoll = oldWait, oldPoll })
	now := time.Now().UTC().Format(time.RFC3339)
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	dsc := func(release string, conds ...string) string {
		return `{"metadata":{"name":"default-dsc"},"status":{"release":{"version":"` + release + `"},"conditions":[` + strings.Join(conds, ",") + `]}}`
	}
	cond := func(typ, status, reason, msg, at string) string {
		return fmt.Sprintf(`{"type":%q,"status":%q,"reason":%q,"message":%q,"lastTransitionTime":%q}`, typ, status, reason, msg, at)
	}
	gated := []string{
		cond("Ready", "False", "Error", "failed to resolve upgrade gate version: unable to determine target release for upgrade gates", now),
		cond("ModulesReady", "False", "AdminAckRequired", "Waiting for upgrade gates to be acknowledged", now),
	}
	for _, tc := range []struct {
		name    string
		dsc     string
		want    []string
		quiet   bool
		maxTime time.Duration
	}{
		{"gated by the new operator", dsc("3.5.1", gated...), []string{"ModulesReady=False (AdminAckRequired)", "failed to resolve upgrade gate version", "odh-upgrade-acks"}, false, time.Second},
		{"a gate from before the install", dsc("3.4.0", cond("ModulesReady", "False", "AdminAckRequired", "Waiting for upgrade gates to be acknowledged", old)),
			[]string{"reported before this install finished"}, false, 5 * time.Second},
		{"reported, nothing gated", dsc("3.6.0", cond("Ready", "True", "Ready", "", now)), nil, true, 250 * time.Millisecond},
		{"no DSC", "", nil, true, 250 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
			f.onSubscribe = olmInstalls("rhods-operator.3.6.0")
			f.dsc = tc.dsc
			start := time.Now()
			note := adminAckNote(f.client(context.Background()), "rhods-operator.3.6.0", start)
			if tc.quiet != (note == "") || time.Since(start) > tc.maxTime {
				t.Fatalf("note %q after %s", note, time.Since(start))
			}
			for _, w := range tc.want {
				if !strings.Contains(note, w) {
					t.Errorf("note lacks %q: %s", w, note)
				}
			}
		})
	}
}

// Review fix 5: a Platform gate is the new operator's only with the same
// evidence as a DSC gate; one that only an earlier operator could have set
// is reported as possibly stale.
func TestUpgradeGateNotePlatformFreshness(t *testing.T) {
	oldWait, oldPoll := upgradeGateWait, upgradeGatePoll
	upgradeGateWait, upgradeGatePoll = 200*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { upgradeGateWait, upgradeGatePoll = oldWait, oldPoll })
	now := time.Now().UTC().Format(time.RFC3339)
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	platform := func(at string) string {
		return `{"metadata":{"name":"default"},"status":{"conditions":[{"type":"ProvisioningProgress","status":"False","reason":"AdminAckRequired","message":"gate kserve-3.7 not acknowledged","lastTransitionTime":"` + at + `"}]}}`
	}
	dsc := func(release string) string {
		return `{"metadata":{"name":"default-dsc"},"status":{"release":{"version":"` + release + `"},"conditions":[]}}`
	}
	const hedge = "reported before this install finished"
	for _, tc := range []struct {
		name, platform, dsc string
		hedged              bool
	}{
		{"set after the install", platform(now), dsc("3.5.1"), false},
		{"set long before, DSC not on the new version", platform(old), dsc("3.5.1"), true},
		{"set long before, the new operator reconciled the DSC", platform(old), dsc("3.6.0"), false},
		{"set long before, no DSC", platform(old), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeOLM(t)
			f.platform, f.dsc = tc.platform, tc.dsc
			note := adminAckNote(f.client(context.Background()), "rhods-operator.3.6.0", time.Now())
			if !strings.Contains(note, "AdminAckRequired") || strings.Contains(note, hedge) != tc.hedged {
				t.Fatalf("note %q", note)
			}
		})
	}
}

// E3: a confirmed downgrade that installed says what to expect and how to
// go back; the confirmation says what OLM does to the CRDs.
func TestConfirmedDowngradeNote(t *testing.T) {
	f := newFakeOLM(t)
	installedCSVFixture{csvName: "rhods-operator.3.7.0", version: "3.7.0"}.apply(f)
	f.onSubscribe = olmInstalls("rhods-operator.3.6.0")
	c := f.client(context.Background())
	refused, _ := Reinstall(c, "nightly", testNightlyImage, "")
	if refused.Success || !strings.Contains(refused.Message, "OLM replaces the CRDs that ship in the older bundle") || strings.Contains(refused.Message, "keep the newer schema") {
		t.Fatalf("confirmation text: %s", refused.Message)
	}
	result, err := ReinstallWithOptions(c, "nightly", testNightlyImage, "", OperationOptions{AllowDowngrade: true})
	if err != nil || !result.Success || !strings.Contains(result.Message, "use Update") || !strings.Contains(result.Message, "upgrade gates") {
		t.Fatalf("result %+v err %v", result, err)
	}
	// A same-version reinstall has no such note.
	f2 := newFakeOLM(t)
	installedCSVFixture{csvName: "rhods-operator.3.6.0", version: "3.6.0"}.apply(f2)
	f2.onSubscribe = olmInstalls("rhods-operator.3.6.0")
	if same, _ := Reinstall(f2.client(context.Background()), "nightly", testNightlyImage, ""); !same.Success || strings.Contains(same.Message, "older operator") {
		t.Fatalf("same version: %+v", same)
	}
}

// A same-name CSV that is still being deleted is not the new install.
func TestWaitIgnoresDeletingCSV(t *testing.T) {
	f := newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
	f.csvs["rhods-operator.3.6.0"]["metadata"] = map[string]interface{}{"name": "rhods-operator.3.6.0", "deletionTimestamp": "2026-10-05T00:00:00Z"}
	var logs []string
	outcome := waitForOperatorInstall(f.client(context.Background()), "verify_installplan", func(UpdateStepEvent) {}, &logs, &operatorRecovery{}, "")
	if outcome.succeeded || outcome.errorCode != "install_timeout" {
		t.Fatalf("outcome = %+v", outcome)
	}
}

// R1-2: on a fresh cluster (no CSV, no Subscription) a failed first install
// leaves no CSV behind: the CSV did not exist before the operation, so the
// restore removes it even though the operation deleted no CSV.
func TestUpdateStream_FailedFirstInstallRemovesTheAttemptCSV(t *testing.T) {
	const newCSV = "rhods-operator.3.6.1"
	f := newFakeOLM(t)
	f.channels = `[{"name":"stable-3.x","currentCSV":"` + newCSV + `"}]`
	f.onSubscribe = func(f *fakeOLM, _ map[string]interface{}) {
		f.installPlans["install-new"] = map[string]interface{}{"spec": map[string]interface{}{}, "status": map[string]interface{}{"phase": "Complete"}}
		f.addCSV(newCSV, "Failed")
		f.csvs[newCSV]["status"] = map[string]interface{}{"phase": "Failed", "reason": "InstallCheckFailed", "message": "install timeout"}
		f.sub["status"] = map[string]interface{}{"currentCSV": newCSV, "installedCSV": newCSV, "installPlanRef": map[string]interface{}{"name": "install-new"}}
	}
	emit, events := eventsRecorder()
	result, err := UpdateStream(f.client(context.Background()), testNightlyImage, emit)
	if err != nil || result.Success || result.ErrorCode != "csv_failed" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if lastStatus(events(), restoreStepName) != "success" || !strings.Contains(result.Message, "There was no previous Subscription") {
		t.Fatalf("restore: %v / %q", events(), result.Message)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.csvs) != 0 || f.sub != nil || f.catalog != nil {
		t.Fatalf("leftovers: csvs=%v sub=%v catalog=%v", f.csvs, f.sub, f.catalog)
	}
}

// A CSV that existed before the operation and was not deleted by it is
// never removed by the restore: the restored Subscription adopts it. Here
// the attempt fails before the CSV step (channel detection).
func TestRestore_KeepsAPreExistingCSV(t *testing.T) {
	f := newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
	c := f.client(context.Background())
	r, err := captureOperatorRecovery(c)
	if err != nil {
		t.Fatal(err)
	}
	r.subscriptionChanged = true
	f.mu.Lock()
	f.sub = nil
	f.mu.Unlock()
	emit, events := eventsRecorder()
	result := &types.OperationResponse{Message: "failed"}
	r.restore(c, result, emit)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.csvs["rhods-operator.3.6.0"]; !ok {
		t.Fatalf("the pre-existing CSV was deleted; writes=%v", f.requests)
	}
	if lastStatus(events(), restoreStepName) != "success" || f.sub == nil {
		t.Fatalf("restore: %v sub=%v", events(), f.sub)
	}
	// A CSV with the same name but a new UID was created by the attempt.
	f.csvs["rhods-operator.3.6.0"]["metadata"].(map[string]interface{})["uid"] = "recreated"
	f.mu.Unlock()
	csvs, err := r.attemptCSVsToRemove(c)
	f.mu.Lock()
	if err != nil || fmt.Sprint(csvs) != "[{rhods-operator.3.6.0 recreated}]" {
		t.Fatalf("attempt CSVs = %v, %v", csvs, err)
	}
}

// R1-5: a CSV of the failed attempt that is still deleting when the
// restore budget ends makes the recovery incomplete (failed), with
// guidance; the previous Subscription is still re-applied.
func TestRestore_CSVDeletionTimeoutIsIncomplete(t *testing.T) {
	f := newFakeOLM(t).installed("rhods-operator.3.6.0", map[string]interface{}{"channel": "fast"})
	c := f.client(context.Background())
	r, err := captureOperatorRecovery(c)
	if err != nil {
		t.Fatal(err)
	}
	r.subscriptionChanged, r.csvRemoved = true, true
	f.mu.Lock()
	delete(f.csvs, "rhods-operator.3.6.0")
	f.addCSV("rhods-operator.3.6.1", "Failed")
	f.stuckCSVs["rhods-operator.3.6.1"] = true
	f.mu.Unlock()
	emit, events := eventsRecorder()
	result := &types.OperationResponse{Message: "CSV failed."}
	r.restore(c, result, emit)
	if lastStatus(events(), restoreStepName) != "failed" || !strings.Contains(result.Message, "recovery is incomplete") ||
		!strings.Contains(result.Message, "rhods-operator.3.6.1 from the failed attempt is still being deleted") {
		t.Fatalf("events=%v message=%q", events(), result.Message)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if spec, _ := f.sub["spec"].(map[string]interface{}); spec["channel"] != "fast" {
		t.Fatalf("previous Subscription not re-applied: %v", f.sub)
	}
}

// R5-F4: slow CSV deletions use only their own budget; the catalog and
// Subscription re-apply always runs with a fresh context.
func TestRestore_ReapplyRunsAfterTheDeletionBudget(t *testing.T) {
	oldBudget := restoreCSVBudget
	restoreCSVBudget = 30 * time.Millisecond
	oldDel := CSVDeletionTimeout
	CSVDeletionTimeout = time.Second
	t.Cleanup(func() { restoreCSVBudget, CSVDeletionTimeout = oldBudget, oldDel })

	f := newFakeOLM(t).installed("rhods-operator.3.6.0", map[string]interface{}{"channel": "fast"})
	c := f.client(context.Background())
	r, _ := captureOperatorRecovery(c)
	r.subscriptionChanged, r.csvRemoved, r.catalogChanged = true, true, true
	f.mu.Lock()
	delete(f.csvs, "rhods-operator.3.6.0")
	f.addCSV("rhods-operator.3.6.1", "Failed")
	f.addCSV("rhods-operator.3.6.2", "Failed")
	f.stuckCSVs["rhods-operator.3.6.1"], f.stuckCSVs["rhods-operator.3.6.2"] = true, true
	f.catalog = nil
	f.mu.Unlock()
	result := &types.OperationResponse{}
	emit, _ := eventsRecorder()
	start := time.Now()
	r.restore(c, result, emit)
	if time.Since(start) > 900*time.Millisecond {
		t.Fatalf("restore took %s: the CSV waits did not respect the budget", time.Since(start))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if spec, _ := f.sub["spec"].(map[string]interface{}); spec["channel"] != "fast" || f.catalog == nil {
		t.Fatalf("re-apply skipped: sub=%v catalog=%v", f.sub, f.catalog)
	}
	if !strings.Contains(result.Message, "incomplete") {
		t.Fatalf("message = %q", result.Message)
	}
}

// R5-F3: with Manual approval the tool approves only an InstallPlan for the
// exact CSV the user confirmed. Anything else OLM proposes is reported and
// left for an administrator.
func TestManualApproval_OnlyTheConfirmedCSV(t *testing.T) {
	proposeNewer := func(f *fakeOLM, _ map[string]interface{}) {
		f.installPlans["install-new"] = map[string]interface{}{"spec": map[string]interface{}{"approved": false, "clusterServiceVersionNames": []interface{}{"rhods-operator.3.7.0"}}, "status": map[string]interface{}{"phase": "RequiresApproval"}}
		f.sub["status"] = map[string]interface{}{"currentCSV": "rhods-operator.3.7.0", "installPlanRef": map[string]interface{}{"name": "install-new"}}
	}
	for _, op := range []string{"update", "refresh"} {
		t.Run(op, func(t *testing.T) {
			f := newFakeOLM(t).installed("rhods-operator.3.6.0", map[string]interface{}{"installPlanApproval": "Manual", "channel": "old"})
			f.onSubscribe = func(f *fakeOLM, spec map[string]interface{}) {
				if spec["channel"] != "old" || op == "refresh" && spec["startingCSV"] != nil {
					proposeNewer(f, spec)
				}
			}
			f.channels = `[{"name":"stable-3.x","currentCSV":"rhods-operator.3.6.0"},{"name":"old","currentCSV":"rhods-operator.3.7.0"}]`
			c := f.client(context.Background())
			var r *types.OperationResponse
			if op == "update" {
				r, _ = UpdateStream(c, testNightlyImage, func(UpdateStepEvent) {})
			} else {
				r, _ = RefreshOperator(c)
			}
			if r == nil || r.Success || r.ErrorCode != "approval_required" || !strings.Contains(r.Message, "rhods-operator.3.7.0") {
				t.Fatalf("result = %+v", r)
			}
			for _, w := range f.writes() {
				if w == "PATCH "+namespacedPath("operators.coreos.com/v1alpha1", "installplans", SubNS, "install-new") {
					t.Fatalf("approved an InstallPlan for a CSV that was not confirmed: %v", f.writes())
				}
			}
		})
	}
}

// R5-F3: with Automatic approval, Refresh would install the channel head;
// when that is not the installed CSV, it refuses before changing anything.
func TestRefresh_RefusesWhenTheChannelHeadMoved(t *testing.T) {
	f := newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
	f.channels = `[{"name":"stable-3.x","currentCSV":"rhods-operator.3.6.1"}]`
	r, err := RefreshOperator(f.client(context.Background()))
	if err != nil || r.Success || r.ErrorCode != "validation" || !strings.Contains(r.Message, "now installs rhods-operator.3.6.1") {
		t.Fatalf("result = %+v, %v", r, err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("Refresh changed the cluster: %v", w)
	}
}

// R5-F5: the Subscription is recorded before an operation, so after a crash
// that left no Subscription and no CSV, Refresh re-installs the recorded
// version with the recorded settings instead of dead-ending.
func TestRefresh_AfterACrashUsesTheRecordedSubscription(t *testing.T) {
	config := map[string]interface{}{"nodeSelector": map[string]interface{}{"node-role.kubernetes.io/infra": ""}}
	f := newFakeOLM(t).installed("rhods-operator.3.6.0", map[string]interface{}{"config": config})
	c := f.client(context.Background())
	if _, err := captureOperatorRecovery(c); err != nil { // an operation starts and records it
		t.Fatal(err)
	}
	f.mu.Lock()
	recorded := f.recordedSub
	f.sub, f.csvs = nil, map[string]map[string]interface{}{} // the pod died mid-operation
	f.onSubscribe = olmInstalls("rhods-operator.3.6.0")
	f.mu.Unlock()
	if !strings.Contains(recorded, `"installedCSV":"rhods-operator.3.6.0"`) {
		t.Fatalf("recorded = %s", recorded)
	}
	r, err := RefreshOperator(c)
	if err != nil || !r.Success || !strings.Contains(r.Message, "rhods-operator.3.6.0 is installed again") {
		t.Fatalf("result = %+v, %v", r, err)
	}
	applies := f.subscriptionApplies()
	if spec := applies[0]; spec["source"] != CatalogName || spec["channel"] != "stable-3.x" || fmt.Sprint(spec["config"]) != fmt.Sprint(config) {
		t.Fatalf("recreated Subscription = %v", spec)
	}
}

// R5-F5: without a recorded Subscription, Refresh explains instead of
// claiming success.
func TestRefresh_NothingInstalledNothingRecorded(t *testing.T) {
	f := newFakeOLM(t)
	r, _ := RefreshOperator(f.client(context.Background()))
	if r.Success || r.ErrorCode != "prerequisites" || !strings.Contains(r.Message, "nothing to refresh") {
		t.Fatalf("result = %+v", r)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

// R5-F5: "Reinstall to stable" works with no Subscription and no CSV, and
// carries over the recorded settings.
func TestReinstallStable_WithoutASubscription(t *testing.T) {
	t.Setenv("STABLE_SOURCE", "redhat-operators")
	t.Setenv("STABLE_CHANNEL", "")
	f := newFakeOLM(t)
	f.stableChans = `[{"name":"stable-3.x","currentCSV":"rhods-operator.3.5.1","currentCSVDesc":{"version":"3.5.1"}}]`
	f.recordedSub = `{"recordedAt":"2026-10-01T00:00:00Z","installedCSV":"rhods-operator.3.6.0","spec":{"source":"rhoai-catalog-dev","channel":"stable-3.x","name":"rhods-operator","installPlanApproval":"Automatic","config":{"env":[{"name":"X","value":"1"}]}}}`
	f.onSubscribe = olmInstalls("rhods-operator.3.5.1")
	r, err := Reinstall(f.client(context.Background()), "stable", "", "")
	if err != nil || !r.Success {
		t.Fatalf("result = %+v, %v", r, err)
	}
	spec := f.subscriptionApplies()[0]
	if spec["source"] != "redhat-operators" || !strings.Contains(fmt.Sprint(spec["config"]), "X") {
		t.Fatalf("Subscription = %v", spec)
	}
}

// R7-L1: with "end the Dashboard Dev session first" confirmed, a refusal
// (here a foreign Subscription) still comes before the session is ended,
// so the message "Nothing was changed" is true.
func TestOperationsRefuseBeforeEndingDashboardDev(t *testing.T) {
	for _, op := range []string{"update", "reinstall", "refresh"} {
		t.Run(op, func(t *testing.T) {
			f := newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
			f.dashboardOp = `{"metadata":{"uid":"u1","annotations":{"` + dashboardDevAnnotation + `":"{}"}},"spec":{"replicas":0}}`
			f.installPlans["install-gitops"] = map[string]interface{}{
				"metadata": map[string]interface{}{"ownerReferences": []interface{}{map[string]interface{}{"kind": "Subscription", "name": "rhoai-gitops"}}},
				"spec":     map[string]interface{}{"clusterServiceVersionNames": []string{"rhods-operator.3.6.0"}},
			}
			c := f.client(context.Background())
			opts := OperationOptions{RevertDashboardDev: true}
			var r *types.OperationResponse
			switch op {
			case "update":
				r, _ = UpdateStreamWithOptions(c, testNightlyImage, opts, func(UpdateStepEvent) {})
			case "reinstall":
				r, _ = ReinstallStreamWithOptions(c, "nightly", testNightlyImage, "", opts, func(UpdateStepEvent) {})
			case "refresh":
				r, _ = RefreshOperatorStreamWithOptions(c, opts, func(UpdateStepEvent) {})
			}
			if r == nil || r.Success || !strings.Contains(r.Message, "Another Subscription (rhoai-gitops)") {
				t.Fatalf("result = %+v", r)
			}
			for _, w := range f.writes() {
				if !strings.Contains(w, "-verify-") {
					t.Fatalf("changed the cluster (the Dashboard Dev session must stay): %v", f.writes())
				}
			}
		})
	}
}

// R5-F9: another Subscription for the package (seen as the owner of an
// InstallPlan) stops every operation before it changes anything.
func TestOperationsRefuseAForeignSubscription(t *testing.T) {
	for _, op := range []string{"update", "reinstall", "refresh"} {
		t.Run(op, func(t *testing.T) {
			f := newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
			f.installPlans["install-gitops"] = map[string]interface{}{
				"metadata": map[string]interface{}{"ownerReferences": []interface{}{map[string]interface{}{"kind": "Subscription", "name": "rhoai-gitops"}}},
				"spec":     map[string]interface{}{"clusterServiceVersionNames": []string{"rhods-operator.3.6.0"}},
			}
			c := f.client(context.Background())
			var r *types.OperationResponse
			switch op {
			case "update":
				r, _ = UpdateStream(c, testNightlyImage, func(UpdateStepEvent) {})
			case "reinstall":
				r, _ = Reinstall(c, "nightly", testNightlyImage, "")
			case "refresh":
				r, _ = RefreshOperator(c)
			}
			if r.Success || !strings.Contains(r.Message, "Another Subscription (rhoai-gitops)") {
				t.Fatalf("result = %+v", r)
			}
			for _, w := range f.writes() {
				if !strings.Contains(w, "-verify-") {
					t.Fatalf("changed the cluster: %v", f.writes())
				}
			}
		})
	}
}

// R5-F10 / R6-9: before a downgrade, the live CRDs' storedVersions are
// compared with the versions the target CSV describes
// (customresourcedefinitions.owned[].version). That list is not the bundled
// CRD's full spec.versions (a CRD serving v1 and v2 may be described only as
// v1), so a mismatch or an unreadable CRD list is a warning in the
// confirmation and the result, never a refusal.
func TestReinstallDowngrade_StoredVersionsCheck(t *testing.T) {
	t.Setenv("STABLE_SOURCE", "redhat-operators")
	t.Setenv("STABLE_CHANNEL", "")
	const dsc = "datascienceclusters.datasciencecluster.opendatahub.io"
	liveCRDs := `{"items":[{"metadata":{"name":"` + dsc + `"},"status":{"storedVersions":["v2"]}}]}`
	for _, tc := range []struct {
		name, owned, crds string
		confirmed         bool
		wantSuccess       bool
		wantCode          string
		wantInMsg         string
		wantInLogs        string
	}{
		{name: "CSV describes only v1 of a CRD storing v2: warning, installed", owned: `[{"name":"` + dsc + `","version":"v1"}]`, crds: liveCRDs, confirmed: true,
			wantSuccess: true, wantInMsg: "is installed", wantInLogs: "may not serve every version the live CRDs store"},
		{name: "unconfirmed: the confirmation carries the warning", owned: `[{"name":"` + dsc + `","version":"v1"}]`, crds: liveCRDs,
			wantCode: errorCodeDowngrade, wantInMsg: "stores objects as v2"},
		{name: "CSV describes the stored version", owned: `[{"name":"` + dsc + `","version":"v1"},{"name":"` + dsc + `","version":"v2"}]`, crds: liveCRDs, confirmed: true,
			wantSuccess: true, wantInMsg: "is installed", wantInLogs: "describes every stored version"},
		{name: "CRD list unreadable: warning, installed", owned: `[{"name":"` + dsc + `","version":"v1"}]`, crds: `not json`, confirmed: true,
			wantSuccess: true, wantInMsg: "is installed", wantInLogs: "could not be compared"},
		{name: "catalog does not say", crds: liveCRDs, confirmed: true,
			wantSuccess: true, wantInMsg: "is installed", wantInLogs: "stored versions of the live CRDs were not checked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeOLM(t).installed("rhods-operator.3.6.0", map[string]interface{}{"source": CatalogName})
			desc := `"version":"3.5.1"`
			if tc.owned != "" {
				desc += `,"customresourcedefinitions":{"owned":` + tc.owned + `}`
			}
			f.stableChans = `[{"name":"stable-3.x","currentCSV":"rhods-operator.3.5.1","currentCSVDesc":{` + desc + `}}]`
			f.crds = tc.crds
			f.onSubscribe = olmInstalls("rhods-operator.3.5.1")
			r, err := ReinstallWithOptions(f.client(context.Background()), "stable", "", "", OperationOptions{AllowDowngrade: tc.confirmed})
			if err != nil || r.Success != tc.wantSuccess || r.ErrorCode != tc.wantCode || !strings.Contains(r.Message, tc.wantInMsg) {
				t.Fatalf("result = %+v, %v", r, err)
			}
			if !tc.wantSuccess {
				for _, w := range f.writes() {
					if !strings.Contains(w, "-verify-") {
						t.Fatalf("changed the cluster: %v", f.writes())
					}
				}
			}
			if tc.wantInLogs != "" && !strings.Contains(strings.Join(r.Logs, "\n"), tc.wantInLogs) {
				t.Fatalf("logs lack %q: %v", tc.wantInLogs, r.Logs)
			}
		})
	}
}
