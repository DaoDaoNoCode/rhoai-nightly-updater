package cluster

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// verificationCatalogWrites returns the writes CheckNodePull made, and
// fails the test when it wrote anything but its own verification catalog
// (one apply, one delete).
func verificationCatalogWrites(t *testing.T, f *fakeOLM) {
	t.Helper()
	var applied, deleted string
	for _, w := range f.writes() {
		method, path, _ := strings.Cut(w, " ")
		if !strings.Contains(path, "/namespaces/"+CatalogNS+"/catalogsources/"+verificationCatalogPrefix()) {
			t.Fatalf("the check wrote something else than its verification catalog: %v", f.writes())
		}
		switch method {
		case http.MethodPatch:
			applied = path
		case http.MethodDelete:
			deleted = path
		}
	}
	if applied == "" || deleted != applied {
		t.Fatalf("the verification catalog was not created and removed: %v", f.writes())
	}
}

func TestCheckNodePull_ImagePulled(t *testing.T) {
	f := newFakeOLM(t)
	f.topology = "HighlyAvailable"
	result, err := CheckNodePull(f.client(context.Background()), testNightlyImage)
	if err != nil || !result.Success {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !strings.Contains(result.Message, "The nodes can pull from quay.io/rhoai") || !strings.Contains(result.Message, testNightlyImage) || !strings.Contains(result.Message, "oc get mcp") {
		t.Fatalf("message %q", result.Message)
	}
	verificationCatalogWrites(t, f)
	// The catalog it created is a verification catalog, labelled as one.
	for _, r := range f.requests {
		if r.Method == http.MethodPatch {
			meta, _ := r.Body["metadata"].(map[string]interface{})
			labels, _ := meta["labels"].(map[string]interface{})
			if labels[catalogPurposeLabel] != verificationCatalogRole {
				t.Fatalf("labels %v", labels)
			}
		}
	}
}

func TestCheckNodePull_PullFailsPerTopology(t *testing.T) {
	for _, tc := range []struct {
		name, topology, want, notWant string
	}{
		{"self-managed", "HighlyAvailable", "This cluster is self-managed: its nodes pull with the cluster pull secret, openshift-config/pull-secret", "Setup step 1"},
		{"unreadable", "", "If it is self-managed", "This cluster is self-managed"},
		{"hosted control plane", "External", "(Setup step 1)", "openshift-config/pull-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeOLM(t)
			f.topology = tc.topology
			f.verifyState = "TRANSIENT_FAILURE"
			f.catalogPods = `{"items":[{"status":{"containerStatuses":[{"state":{"waiting":{"reason":"ErrImagePull","message":"unauthorized: access to the requested resource is not authorized"}}}]}}]}`
			result, err := CheckNodePull(f.client(context.Background()), testNightlyImage)
			if err != nil || result.Success || result.ErrorCode != "catalog_image_pull" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if !strings.HasPrefix(result.Message, "The nodes cannot pull from quay.io/rhoai: the catalog image "+testNightlyImage+" cannot be pulled (ErrImagePull: unauthorized") ||
				!strings.Contains(result.Message, tc.want) || strings.Contains(result.Message, tc.notWant) {
				t.Fatalf("message %q", result.Message)
			}
			verificationCatalogWrites(t, f)
		})
	}
}

// A pull that neither works nor fails within CatalogReadyTimeout (the fake
// clock moves when the code sleeps) ends the check without a verdict.
func TestCheckNodePull_NoAnswer(t *testing.T) {
	f := newFakeOLM(t)
	f.verifyState = "CONNECTING"
	result, err := CheckNodePull(f.client(context.Background()), testNightlyImage)
	if err != nil || result.Success || result.ErrorCode != "timeout" || !strings.Contains(result.Message, "catalog state is CONNECTING") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	verificationCatalogWrites(t, f)
}

// A stopped request removes the catalog all the same.
func TestCheckNodePull_StoppedRemovesTheCatalog(t *testing.T) {
	f := newFakeOLM(t)
	f.verifyState = "CONNECTING"
	ctx, cancel := context.WithCancel(context.Background())
	c := f.client(ctx)
	f.intercept = func(f *fakeOLM, w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/catalogsources/"+verificationCatalogPrefix()) {
			cancel()
		}
		return false
	}
	result, err := CheckNodePull(c, testNightlyImage)
	if err == nil || result.Success {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	verificationCatalogWrites(t, f)
}

// The catalog cannot be created: nothing else is attempted.
func TestCheckNodePull_CreateRefused(t *testing.T) {
	f := newFakeOLM(t)
	f.intercept = func(f *fakeOLM, w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/catalogsources/") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"kind":"Status","status":"Failure","message":"denied","code":403}`))
			return true
		}
		return false
	}
	result, err := CheckNodePull(f.client(context.Background()), testNightlyImage)
	if err != nil || result.Success || result.ErrorCode != "forbidden" || !strings.Contains(result.Message, "Nothing was changed") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
