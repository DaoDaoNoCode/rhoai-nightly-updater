package cluster

import (
	"context"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const replacementCSV = "rhods-operator.3.6.9"

// replacedAfterApply: right after the operation's apply, someone replaces
// the Subscription (another UID), OLM installs a CSV for the replacement,
// and the operation's install fails.
func replacedAfterApply(f *fakeOLM) {
	f.onSubscribe = func(f *fakeOLM, _ map[string]interface{}) {
		f.sub["metadata"].(map[string]interface{})["uid"] = "uid-replacement"
		f.sub["spec"].(map[string]interface{})["channel"] = "replacement"
		f.addCSV(replacementCSV, "Installing")
		f.installPlans["install-new"] = map[string]interface{}{"spec": map[string]interface{}{}, "status": map[string]interface{}{"phase": "Failed",
			"conditions": []interface{}{map[string]interface{}{"type": "Installed", "status": "False", "reason": "InstallComponentFailed", "message": "CRD step failed"}}}}
		f.sub["status"] = map[string]interface{}{"installPlanRef": map[string]interface{}{"name": "install-new"}}
	}
}

// assertReplacementKept: the restore left the replacement and its CSV alone
// and said so.
func assertReplacementKept(t *testing.T, f *fakeOLM, result *types.OperationResponse, events []UpdateStepEvent) {
	t.Helper()
	if result.Success || lastStatus(events, restoreStepName) != "failed" || !strings.Contains(result.Message, "another Subscription replaced the one this operation created") {
		t.Fatalf("restore %q, message %q", lastStatus(events, restoreStepName), result.Message)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sub == nil || f.sub["metadata"].(map[string]interface{})["uid"] != "uid-replacement" || f.sub["spec"].(map[string]interface{})["channel"] != "replacement" {
		t.Fatalf("the replacement Subscription was deleted or overwritten: %v", f.sub)
	}
	if _, ok := f.csvs[replacementCSV]; !ok {
		t.Fatalf("the CSV installed for the replacement was deleted: %v", f.csvs)
	}
}

func TestRestoreLeavesASubscriptionThatReplacedTheAttempts(t *testing.T) {
	t.Run("full profile", func(t *testing.T) {
		f := newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
		f.channels = `[{"name":"stable-3.x","currentCSV":"rhods-operator.3.6.1"}]`
		replacedAfterApply(f)
		emit, events := eventsRecorder()
		result, err := UpdateStream(f.client(context.Background()), testNightlyImage, emit)
		if err != nil {
			t.Fatal(err)
		}
		assertReplacementKept(t, f, result, events())
	})
	t.Run("full profile under Argo CD", func(t *testing.T) {
		f := gitOpsFake(t)
		replacedAfterApply(f)
		emit, events := eventsRecorder()
		result, err := UpdateStream(f.client(context.Background()), gitOpsImage, emit)
		if err != nil {
			t.Fatal(err)
		}
		assertReplacementKept(t, f, result, events())
	})
	t.Run("gitops profile", func(t *testing.T) {
		f := servesGitOpsPolicy(t, gitOpsFake(t))
		replacedAfterApply(f)
		emit, events := eventsRecorder()
		result, err := UpdateStreamGitOpsProfile(f.client(context.Background()), gitOpsImage, emit)
		if err != nil {
			t.Fatal(err)
		}
		assertReplacementKept(t, f, result, events())
	})
}

// Every Subscription apply of an operation (Update, Reinstall, Re-deploy
// all use applySubscriptionWithRetry) records the applied UID from the
// apply's answer, and the restore deletes only that one.
func TestRestoreDeletesOnlyTheAttemptsSubscription(t *testing.T) {
	f := newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
	c := f.client(context.Background())
	r, err := captureOperatorRecovery(c)
	if err != nil {
		t.Fatal(err)
	}
	r.subscriptionChanged = true
	f.mu.Lock()
	f.sub = nil // the operation deleted the previous one
	f.mu.Unlock()
	if applyErr, _ := applySubscriptionWithRetry(c, subscriptionPath(), buildSubscription(r.subscriptionBase(), CatalogName, "stable-3.x"), r, nil); applyErr != nil {
		t.Fatal(applyErr)
	}
	f.mu.Lock()
	own, _ := f.sub["metadata"].(map[string]interface{})["uid"].(string)
	f.mu.Unlock()
	if r.attemptSubscriptionUID == "" || r.attemptSubscriptionUID != own {
		t.Fatalf("recorded %q, applied %q", r.attemptSubscriptionUID, own)
	}
	// Its own one is deleted and the previous one put back.
	result := &types.OperationResponse{}
	emit, events := eventsRecorder()
	r.restore(c, result, emit)
	f.mu.Lock()
	defer f.mu.Unlock()
	if lastStatus(events(), restoreStepName) != "success" || f.sub == nil || f.sub["metadata"].(map[string]interface{})["uid"] == own {
		t.Fatalf("restore %v, sub %v", events(), f.sub)
	}
}
