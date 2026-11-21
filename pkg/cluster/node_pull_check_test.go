package cluster

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// nodeCheckImage is a nightly catalog image by tag, as the check takes it
// (NodePullCheckImageProblem).
const nodeCheckImage = "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6"

// verificationCatalogWrites returns the writes CheckNodePull made, and
// fails the test when it wrote anything but its own verification catalog
// (one apply, one delete).
func verificationCatalogWrites(t *testing.T, f *fakeOLM) {
	t.Helper()
	var applied, deleted string
	for _, w := range f.writes() {
		method, path, _ := strings.Cut(w, " ")
		if !strings.Contains(path, "/namespaces/"+rhoai.CatalogNS+"/catalogsources/"+verificationCatalogPrefix()) {
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
	result, err := CheckNodePull(f.client(context.Background()), nodeCheckImage)
	if err != nil || !result.Success {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !strings.HasPrefix(result.Message, "A temporary catalog pod pulled "+nodeCheckImage+" with the nodes' pull secret") || !strings.Contains(result.Message, "oc get mcp") {
		t.Fatalf("message %q", result.Message)
	}
	verificationCatalogWrites(t, f)
	// The catalog it created is a verification catalog, labelled as one.
	for _, r := range f.requests {
		if r.Method == http.MethodPatch {
			meta, _ := r.Body["metadata"].(map[string]any)
			labels, _ := meta["labels"].(map[string]any)
			if labels[catalogPurposeLabel] != verificationCatalogRole {
				t.Fatalf("labels %v", labels)
			}
		}
	}
}

// A digest reference is refused before anything is created: OLM's registry
// pod would not pull a cached digest again (InferImagePullPolicy), so the
// check could pass without the nodes' credentials.
func TestCheckNodePull_RefusesADigest(t *testing.T) {
	f := newFakeOLM(t)
	f.topology = "HighlyAvailable"
	image := "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + strings.Repeat("a", 64)
	result, err := CheckNodePull(f.client(context.Background()), image)
	if err != nil || result.Success || result.ErrorCode != "validation" || !strings.Contains(result.Message, "by tag, not by digest") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(f.writes()) != 0 {
		t.Fatalf("writes %v", f.writes())
	}
	if NodePullCheckImageProblem("quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6") != "" {
		t.Fatal("a tag reference must be accepted")
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
			result, err := CheckNodePull(f.client(context.Background()), nodeCheckImage)
			if err != nil || result.Success || result.ErrorCode != "catalog_image_pull" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if !strings.HasPrefix(result.Message, "The catalog image "+nodeCheckImage+" cannot be pulled (ErrImagePull: unauthorized") ||
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
	result, err := CheckNodePull(f.client(context.Background()), nodeCheckImage)
	if err != nil || result.Success || result.ErrorCode != "timeout" || !strings.Contains(result.Message, "catalog state is CONNECTING") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	verificationCatalogWrites(t, f)
}

// A stopped request removes the catalog all the same, whether it is stopped
// while the check reads the catalog's state (the read fails) or right after
// a read answered (the next step finds the request stopped). requestHooks
// put the stop at that point, so each case takes its own path in every run.
func TestCheckNodePull_StoppedRemovesTheCatalog(t *testing.T) {
	isStateRead := func(r *http.Request) bool {
		return r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/catalogsources/"+verificationCatalogPrefix())
	}
	stateReads := func(f *fakeOLM) int {
		f.mu.Lock()
		defer f.mu.Unlock()
		n := 0
		for _, r := range f.requests {
			if r.Method == http.MethodGet && strings.Contains(r.Path, "/catalogsources/"+verificationCatalogPrefix()) {
				n++
			}
		}
		return n
	}
	for _, tc := range []struct {
		name string
		// afterRead: stopped once the state read answered, else just before
		// it is sent.
		afterRead bool
		// State reads the fake answered.
		wantReads int
	}{
		{name: "while the state is read", wantReads: 0},
		{name: "after the state was read", afterRead: true, wantReads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeOLM(t)
			f.verifyState = "CONNECTING"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stop := func(r *http.Request) {
				if isStateRead(r) {
					cancel()
				}
			}
			hooks := requestHooks{before: stop}
			if tc.afterRead {
				hooks = requestHooks{answered: stop}
			}
			result, err := CheckNodePull(withRequestHooks(f.client(ctx), hooks), nodeCheckImage)
			if !errors.Is(err, context.Canceled) || result.Success || result.Message != "The check was stopped" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if n := stateReads(f); n != tc.wantReads {
				t.Fatalf("%d state reads reached the API server, want %d", n, tc.wantReads)
			}
			verificationCatalogWrites(t, f)
		})
	}
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
	result, err := CheckNodePull(f.client(context.Background()), nodeCheckImage)
	if err != nil || result.Success || result.ErrorCode != "forbidden" || !strings.Contains(result.Message, "Nothing was changed") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
