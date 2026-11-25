package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/authz"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
)

// ssarFake answers SelfSubjectAccessReviews: allow decides each, fail
// makes the API server answer 500 for one.
type ssarFake struct {
	mu     sync.Mutex
	asked  []AccessRequest
	tokens map[string]bool
	allow  func(AccessRequest) bool
	fail   func(AccessRequest) bool
}

func newSSARClient(t *testing.T, f *ssarFake) *Client {
	t.Helper()
	f.tokens = map[string]bool{}
	srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var review struct {
			Kind string `json:"kind"`
			Spec struct {
				ResourceAttributes AccessRequest `json:"resourceAttributes"`
			} `json:"spec"`
		}
		if err := json.NewDecoder(r.Body).Decode(&review); err != nil || review.Kind != "SelfSubjectAccessReview" {
			t.Errorf("bad review %v %+v", err, review)
		}
		req := review.Spec.ResourceAttributes
		f.mu.Lock()
		f.asked = append(f.asked, req)
		f.tokens[r.Header.Get("Authorization")] = true
		f.mu.Unlock()
		if f.fail != nil && f.fail(req) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"kind":"Status","status":"Failure","code":500}`))
			return
		}
		fmt.Fprintf(w, `{"status":{"allowed":%v}}`, f.allow(req))
	}))
	t.Cleanup(srv.Close)
	return newTestClient(testClientOptions{BaseURL: srv.URL, Token: "user-token", HTTPClient: srv.Client(), Ctx: context.Background(), AsUser: true})
}

// Every request of the actions is reviewed once with the user's token,
// in the project or the applications namespace; a denied one is listed
// for each action that sends it, with who can grant it.
func TestReviewProjectActionsDedupesAndListsMissing(t *testing.T) {
	f := &ssarFake{allow: func(r AccessRequest) bool { return r.Verb != "delete" }}
	u := newSSARClient(t, f)
	ids := []string{"workbench.setup", "workbench.teardown"}
	missing, err := ReviewProjectActions(u, ids, "team-a", "apps-ns")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[AccessRequest]int{}
	for _, r := range f.asked {
		seen[r]++
		inCluster := r.Resource == "storageclasses" && r.Namespace == ""
		if r.Namespace != "team-a" && r.Namespace != "apps-ns" && !inCluster {
			t.Errorf("reviewed outside the project: %+v", r)
		}
	}
	for r, n := range seen {
		if n != 1 {
			t.Errorf("%+v reviewed %d times", r, n)
		}
	}
	if len(f.tokens) != 1 || !f.tokens["Bearer user-token"] {
		t.Errorf("reviews sent with %v", f.tokens)
	}
	for _, id := range ids {
		if len(missing[id]) != 2 {
			t.Errorf("%s misses %v", id, missing[id])
		}
		for _, m := range missing[id] {
			if m.Request.Verb != "delete" || !strings.Contains(m.Text, "ask an admin of project team-a to give you the edit role") {
				t.Errorf("%s: %+v", id, m)
			}
		}
	}
	// The project's own namespace is read by name in the project.
	if seen[AccessRequest{Verb: "get", Resource: "namespaces", Namespace: "team-a", Name: "team-a"}] != 1 {
		t.Errorf("the project's namespace was not reviewed by name: %v", f.asked)
	}
}

// Fail closed: one review that fails, an action without the applications
// namespace it needs, an action users may not run, a client that is not
// the user's: an error, nothing allowed.
func TestReviewProjectActionsFailsClosed(t *testing.T) {
	f := &ssarFake{allow: func(AccessRequest) bool { return true }, fail: func(r AccessRequest) bool { return r.Resource == "pods" }}
	u := newSSARClient(t, f)
	if m, err := ReviewProjectActions(u, []string{"workbench.teardown"}, "team-a", "apps-ns"); err == nil || m != nil {
		t.Fatalf("a failed review allowed: %v %v", m, err)
	}
	if _, err := ReviewProjectActions(u, []string{"workbench.setup"}, "team-a", ""); !errors.Is(err, ErrAppsNamespaceUnknown) {
		t.Fatalf("no applications namespace: %v", err)
	}
	if _, err := ReviewProjectActions(u, []string{"mlflow-samples.regenerate"}, "team-a", "apps"); err == nil {
		t.Fatal("a cluster-admin's action was reviewed for a user")
	}
	sa := newTestClient(testClientOptions{BaseURL: u.BaseURL(), Token: "sa", Ctx: context.Background()})
	if _, err := ReviewProjectActions(sa, []string{"workbench.stop"}, "team-a", ""); !errors.Is(err, authz.ErrNotUserClient) {
		t.Fatalf("the ServiceAccount's client reviewed: %v", err)
	}
}

// The records caps: a user's 11th project and the 101st project with
// users' records are refused; a project already counted, a project that is
// gone and a cluster-admin's records do not count.
func TestRecordsCapsPerUserAndTotal(t *testing.T) {
	exists := func(string) (bool, error) { return true, nil }
	var mine []record.RecordOwner
	for i := range record.MaxUserRecordProjectsPerUser {
		mine = append(mine, record.RecordOwner{Project: fmt.Sprintf("a-%d", i), By: "alice", ActorRole: record.ActorRoleUser})
	}
	if msg, _ := record.UserRecordCapRefusal(mine, "new", "alice", exists); !strings.Contains(msg, "in 10 projects") {
		t.Fatalf("11th project: %q", msg)
	}
	if msg, _ := record.UserRecordCapRefusal(mine, "a-3", "alice", exists); msg != "" {
		t.Fatalf("a project already counted: %q", msg)
	}
	if msg, _ := record.UserRecordCapRefusal(mine, "new", "bob", exists); msg != "" {
		t.Fatalf("another user: %q", msg)
	}
	gone := func(p string) (bool, error) { return p != "a-0", nil }
	if msg, _ := record.UserRecordCapRefusal(mine, "new", "alice", gone); msg != "" {
		t.Fatalf("a deleted project still counts: %q", msg)
	}
	if _, err := record.UserRecordCapRefusal(mine, "new", "alice", func(string) (bool, error) { return false, errors.New("HTTP 500") }); err == nil {
		t.Fatal("an unreadable project count allowed (fail closed)")
	}
	var all []record.RecordOwner
	for i := range record.MaxUserRecordProjects {
		all = append(all, record.RecordOwner{Project: fmt.Sprintf("p-%d", i), By: fmt.Sprintf("u-%d", i), ActorRole: record.ActorRoleUser})
	}
	if msg, _ := record.UserRecordCapRefusal(all, "new", "carol", exists); !strings.Contains(msg, "are full (100 projects)") || !strings.Contains(msg, "Clean up") {
		t.Fatalf("101st project: %q", msg)
	}
	if msg, _ := record.UserRecordCapRefusal(all, "p-5", "carol", exists); msg != "" {
		t.Fatalf("a project already counted in the total: %q", msg)
	}
}

// A cluster-admin's records are outside both caps.
func TestAdminRecordsOutsideCaps(t *testing.T) {
	var admins []record.RecordOwner
	for i := range 2 * record.MaxUserRecordProjects {
		role := record.ActorRoleAdmin
		if i%2 == 0 {
			role = "" // written before actorRole existed: an admin's
		}
		admins = append(admins, record.RecordOwner{Project: fmt.Sprintf("p-%d", i), By: "alice", ActorRole: role})
	}
	if msg, err := record.UserRecordCapRefusal(admins, "new", "alice", func(string) (bool, error) { return true, nil }); msg != "" || err != nil {
		t.Fatalf("admins' records counted: %q %v", msg, err)
	}
	c := newTestClient(testClientOptions{Token: "t", Ctx: context.Background(), AsUser: true})
	if record.ActorRole(c) != record.ActorRoleAdmin {
		t.Fatal("an unmarked client writes as a user")
	}
	c.SetProjectUser()
	if record.ActorRole(c) != record.ActorRoleUser {
		t.Fatal("a project user's client writes as an admin")
	}
}
