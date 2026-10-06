package cluster

// R6-8 / R7-L4: the automatic restore deletes only what the failed attempt
// created, by UID, and never an object someone replaced in the meantime.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// csvDeletes returns the UID preconditions of the CSV DELETEs, by name.
func csvDeletes(f *fakeOLM) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for _, r := range f.requests {
		if r.Method == http.MethodDelete && strings.Contains(r.Path, "/clusterserviceversions/") {
			pre, _ := r.Body["preconditions"].(map[string]interface{})
			uid, _ := pre["uid"].(string)
			out[r.Path[strings.LastIndex(r.Path, "/")+1:]] = uid
		}
	}
	return out
}

func csvUID(f *fakeOLM, name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	uid, _ := f.csvs[name]["metadata"].(map[string]interface{})["uid"].(string)
	return uid
}

// A second CSV that existed before the operation (here one left in
// Replacing) is kept even though the operation deleted the installed CSV.
func TestRestore_OperationRemovedCSV_DeletesOnlyTheAttemptsCSV(t *testing.T) {
	f := newFakeOLM(t).installed("rhods-operator.3.6.0", map[string]interface{}{"channel": "fast"})
	f.addCSV("rhods-operator.3.5.0", "Replacing")
	c := f.client(context.Background())
	r, err := captureOperatorRecovery(c)
	if err != nil {
		t.Fatal(err)
	}
	keptUID := csvUID(f, "rhods-operator.3.5.0")
	// The operation deletes the installed CSV, then the attempt installs a
	// new one that fails.
	r.subscriptionChanged = true
	r.noteCSVRemoved("rhods-operator.3.6.0")
	f.mu.Lock()
	delete(f.csvs, "rhods-operator.3.6.0")
	f.addCSV("rhods-operator.3.6.1", "Failed")
	f.mu.Unlock()
	newUID := csvUID(f, "rhods-operator.3.6.1")

	emit, events := eventsRecorder()
	r.restore(c, &types.OperationResponse{Message: "failed"}, emit)
	if lastStatus(events(), restoreStepName) != "success" {
		t.Fatalf("restore: %v", events())
	}
	dels := csvDeletes(f)
	if len(dels) != 1 || dels["rhods-operator.3.6.1"] != newUID {
		t.Fatalf("CSV deletes (name -> UID precondition) = %v, want only the attempt's CSV with uid %s", dels, newUID)
	}
	if csvUID(f, "rhods-operator.3.5.0") != keptUID {
		t.Fatal("the pre-existing CSV was deleted")
	}
}

// The CSV the operation deleted, still finishing behind its finalizer, is
// waited for (OLM cannot reinstall that name until it is gone), with its own
// UID as the precondition; the other pre-existing CSV is kept.
func TestRestore_OperationRemovedCSVStillTerminatingIsWaitedFor(t *testing.T) {
	oldDel := CSVDeletionTimeout
	CSVDeletionTimeout = 50 * time.Millisecond
	t.Cleanup(func() { CSVDeletionTimeout = oldDel })
	f := newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
	f.addCSV("rhods-operator.3.5.0", "Replacing")
	c := f.client(context.Background())
	r, _ := captureOperatorRecovery(c)
	removedUID := csvUID(f, "rhods-operator.3.6.0")
	r.subscriptionChanged = true
	r.noteCSVRemoved("rhods-operator.3.6.0")
	f.mu.Lock()
	f.stuckCSVs["rhods-operator.3.6.0"] = true
	f.mu.Unlock()

	result := &types.OperationResponse{}
	emit, _ := eventsRecorder()
	r.restore(c, result, emit)
	dels := csvDeletes(f)
	if len(dels) != 1 || dels["rhods-operator.3.6.0"] != removedUID {
		t.Fatalf("CSV deletes = %v, want only rhods-operator.3.6.0 with uid %s", dels, removedUID)
	}
	if !strings.Contains(result.Message, "rhods-operator.3.6.0 from the failed attempt is still being deleted") {
		t.Fatalf("message = %q", result.Message)
	}
}

// A CSV or Subscription replaced between the restore's read and its delete
// is reported and left alone; a replaced Subscription is not overwritten.
func TestRestore_ObjectsReplacedMeanwhileAreNotDeleted(t *testing.T) {
	t.Run("CSV", func(t *testing.T) {
		f := newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
		c := f.client(context.Background())
		r, _ := captureOperatorRecovery(c)
		r.subscriptionChanged = true
		f.mu.Lock()
		f.addCSV("rhods-operator.3.6.1", "Failed")
		f.intercept = func(f *fakeOLM, _ http.ResponseWriter, req *http.Request) bool {
			if req.Method == http.MethodDelete && strings.HasSuffix(req.URL.Path, "/rhods-operator.3.6.1") {
				// An administrator recreated it just before the delete.
				f.csvs["rhods-operator.3.6.1"]["metadata"].(map[string]interface{})["uid"] = "admin-uid"
			}
			return false
		}
		f.mu.Unlock()
		result := &types.OperationResponse{}
		emit, events := eventsRecorder()
		r.restore(c, result, emit)
		if csvUID(f, "rhods-operator.3.6.1") != "admin-uid" {
			t.Fatal("the replaced CSV was deleted")
		}
		if lastStatus(events(), restoreStepName) != "failed" || !strings.Contains(result.Message, "was replaced by another one") {
			t.Fatalf("events=%v message=%q", events(), result.Message)
		}
	})
	t.Run("Subscription", func(t *testing.T) {
		f := newFakeOLM(t).installed("rhods-operator.3.6.0", map[string]interface{}{"channel": "fast"})
		c := f.client(context.Background())
		r, _ := captureOperatorRecovery(c)
		r.subscriptionChanged = true
		f.mu.Lock()
		f.intercept = func(f *fakeOLM, _ http.ResponseWriter, req *http.Request) bool {
			if req.Method == http.MethodDelete && strings.HasSuffix(req.URL.Path, "/subscriptions/"+SubName) {
				f.sub["metadata"].(map[string]interface{})["uid"] = "admin-sub"
				f.sub["spec"].(map[string]interface{})["channel"] = "admin-channel"
			}
			return false
		}
		f.mu.Unlock()
		result := &types.OperationResponse{}
		emit, events := eventsRecorder()
		r.restore(c, result, emit)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.sub == nil || f.sub["metadata"].(map[string]interface{})["uid"] != "admin-sub" || f.sub["spec"].(map[string]interface{})["channel"] != "admin-channel" {
			t.Fatalf("the replaced Subscription was deleted or overwritten: %v", f.sub)
		}
		if lastStatus(events(), restoreStepName) != "failed" || !strings.Contains(result.Message, "Subscription was replaced by someone else") {
			t.Fatalf("events=%v message=%q", events(), result.Message)
		}
	})
}

// When the CSV list fails, the CSVs the attempt was seen installing are read
// one by one, so their deletes still carry UIDs.
func TestRestore_ListFailureFallsBackToUIDGuardedDeletes(t *testing.T) {
	f := newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
	c := f.client(context.Background())
	r, _ := captureOperatorRecovery(c)
	r.subscriptionChanged = true
	r.noteAttemptCSV("rhods-operator.3.6.1")
	r.noteAttemptCSV("rhods-operator.3.6.0") // existed before: kept
	f.mu.Lock()
	f.addCSV("rhods-operator.3.6.1", "Failed")
	f.intercept = func(_ *fakeOLM, w http.ResponseWriter, req *http.Request) bool {
		if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/clusterserviceversions") {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"kind":"Status","code":503}`)
			return true
		}
		return false
	}
	f.mu.Unlock()
	newUID := csvUID(f, "rhods-operator.3.6.1")
	result := &types.OperationResponse{}
	emit, _ := eventsRecorder()
	r.restore(c, result, emit)
	dels := csvDeletes(f)
	if len(dels) != 1 || dels["rhods-operator.3.6.1"] != newUID {
		t.Fatalf("CSV deletes = %v, want only rhods-operator.3.6.1 with uid %s", dels, newUID)
	}
	if !strings.Contains(result.Message, "list CSVs") {
		t.Fatalf("the list failure is not reported: %q", result.Message)
	}
}
