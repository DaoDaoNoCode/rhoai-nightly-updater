package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/authz"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/workbench"
)

// ssarFake answers SelfSubjectAccessReviews: allow decides each, fail
// makes the API server answer 500 for one.
type ssarFake struct {
	mu     sync.Mutex
	asked  []AccessRequest
	tokens map[string]bool
	allow  func(AccessRequest) bool
	fail   func(AccessRequest) bool
	// body, when set, is the review's answer as sent.
	body func(AccessRequest) string
}

func newSSARClient(t *testing.T, f *ssarFake) *Client {
	t.Helper()
	f.tokens = map[string]bool{}
	srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The project's namespace (not Kueue-managed): the workbench
		// setup's conditional LocalQueues row is not reviewed.
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/") {
			fmt.Fprintf(w, `{"metadata":{"name":%q,"labels":{"opendatahub.io/dashboard":"true"}},"status":{"phase":"Active"}}`, strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/"))
			return
		}
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
		if f.body != nil {
			w.Write([]byte(f.body(req)))
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
		if len(missing[id].Missing) != 2 {
			t.Errorf("%s misses %v", id, missing[id])
		}
		for _, m := range missing[id].Missing {
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
	// Without the applications namespace only the action that reads there
	// cannot be told; the others are reviewed.
	f.fail = nil
	if r, err := ReviewProjectActions(u, []string{"workbench.setup", "workbench.stop"}, "team-a", ""); err != nil || !errors.Is(r["workbench.setup"].Err, ErrAppsNamespaceUnknown) || r["workbench.stop"].Err != nil {
		t.Fatalf("no applications namespace: %+v %v", r, err)
	}
	if _, err := ReviewProjectActions(u, []string{"mlflow-samples.regenerate"}, "team-a", "apps"); err == nil {
		t.Fatal("a cluster-admin's action was reviewed for a user")
	}
	sa := newTestClient(testClientOptions{BaseURL: u.BaseURL(), Token: "sa", Ctx: context.Background()})
	if _, err := ReviewProjectActions(sa, []string{"workbench.stop"}, "team-a", ""); !errors.Is(err, authz.ErrNotUserClient) {
		t.Fatalf("the ServiceAccount's client reviewed: %v", err)
	}
}

// entry is a user's new entry of project.
func entry(project, user string) record.RecordOwner {
	return record.RecordOwner{Project: project, ProjectUID: "uid-" + project, By: user, ActorRole: record.ActorRoleUser}
}

// The records caps: a user's 11th project and the 101st project with
// users' records are refused; a project already counted, a project that is
// gone and a cluster-admin's records do not count.
func TestRecordsCapsPerUserAndTotal(t *testing.T) {
	exists := func(record.RecordOwner) (bool, error) { return true, nil }
	var mine []record.RecordOwner
	for i := range record.MaxUserRecordProjectsPerUser {
		mine = append(mine, entry(fmt.Sprintf("a-%d", i), "alice"))
	}
	if msg, _ := record.UserRecordCapRefusal(mine, entry("new", "alice"), exists); !strings.Contains(msg, "in 10 projects") {
		t.Fatalf("11th project: %q", msg)
	}
	if msg, _ := record.UserRecordCapRefusal(mine, entry("a-3", "alice"), exists); msg != "" {
		t.Fatalf("a project already counted: %q", msg)
	}
	if msg, _ := record.UserRecordCapRefusal(mine, entry("new", "bob"), exists); msg != "" {
		t.Fatalf("another user: %q", msg)
	}
	gone := func(o record.RecordOwner) (bool, error) { return o.Project != "a-0", nil }
	if msg, _ := record.UserRecordCapRefusal(mine, entry("new", "alice"), gone); msg != "" {
		t.Fatalf("a deleted project still counts: %q", msg)
	}
	if _, err := record.UserRecordCapRefusal(mine, entry("new", "alice"), func(record.RecordOwner) (bool, error) { return false, errors.New("HTTP 500") }); err == nil {
		t.Fatal("an unreadable project count allowed (fail closed)")
	}
	var all []record.RecordOwner
	for i := range record.MaxUserRecordProjects {
		all = append(all, entry(fmt.Sprintf("p-%d", i), fmt.Sprintf("u-%d", i)))
	}
	if msg, _ := record.UserRecordCapRefusal(all, entry("new", "carol"), exists); !strings.Contains(msg, "are full (100 projects)") || !strings.Contains(msg, "Clean up") {
		t.Fatalf("101st project: %q", msg)
	}
	if msg, _ := record.UserRecordCapRefusal(all, entry("p-5", "carol"), exists); msg != "" {
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
	if msg, err := record.UserRecordCapRefusal(admins, entry("new", "alice"), func(record.RecordOwner) (bool, error) { return true, nil }); msg != "" || err != nil {
		t.Fatalf("admins' records counted: %q %v", msg, err)
	}
	c := newTestClient(testClientOptions{Token: "t", Ctx: context.Background(), AsUser: true})
	if record.ActorRole(c) != record.ActorRoleAdmin {
		t.Fatal("an unmarked client writes as a user")
	}
	c.SetProjectUser("uid-proj")
	if record.ActorRole(c) != record.ActorRoleUser {
		t.Fatal("a project user's client writes as an admin")
	}
}

// The caps count projects by namespace UID: a project deleted and made
// again under the same name is another project, and an entry whose
// project now has another UID is left over (not counted).
func TestRecordsCapsCountByNamespaceUID(t *testing.T) {
	var mine []record.RecordOwner
	for i := range record.MaxUserRecordProjectsPerUser {
		mine = append(mine, entry(fmt.Sprintf("a-%d", i), "alice"))
	}
	remade := record.RecordOwner{Project: "a-3", ProjectUID: "uid-a-3-new", By: "alice", ActorRole: record.ActorRoleUser}
	exists := func(record.RecordOwner) (bool, error) { return true, nil }
	if msg, _ := record.UserRecordCapRefusal(mine, remade, exists); msg == "" {
		t.Fatal("a project made again under the same name counted as the old one")
	}
	// The old a-3 is gone (its name now has another UID): not counted.
	f, c := newResourceFake(t)
	f.putJSON("/api/v1/namespaces/a-3", `{"metadata":{"name":"a-3","uid":"uid-a-3-new"}}`)
	f.putJSON("/api/v1/namespaces/a-4", `{"metadata":{"name":"a-4","uid":"uid-a-4"}}`)
	if ok, err := record.ProjectExists(c, mine[3]); ok || err != nil {
		t.Fatalf("an entry of the old a-3 exists: %v %v", ok, err)
	}
	if ok, err := record.ProjectExists(c, mine[4]); !ok || err != nil {
		t.Fatalf("a-4 does not exist: %v %v", ok, err)
	}
}

// putOwnerRecords writes workbench entries for the caps' admission.
func putOwnerRecords(t *testing.T, f *resourceFake, entries map[string]workbench.Recorded) {
	t.Helper()
	data, _ := json.Marshal(entries)
	cm, _ := json.Marshal(map[string]any{"metadata": map[string]any{"name": record.SnapshotConfigMapName}, "data": map[string]string{workbench.RecordKey: string(data)}})
	f.putJSON("/api/v1/namespaces/"+record.GetActivityNamespace()+"/configmaps/"+record.SnapshotConfigMapName, string(cm))
}

// Admission of a user's entry: a new one and one taken over (a
// cluster-admin's or another user's) count against the caps; the same
// user's entry in the same project does not; an entry too large is
// refused; a cluster-admin's entry is always admitted.
func TestAdmitUserEntry(t *testing.T) {
	f, c := newResourceFake(t)
	full := map[string]workbench.Recorded{"proj/" + workbench.Name: {StorageUID: "s", By: "kube:admin"}}
	for i := range record.MaxUserRecordProjectsPerUser {
		p := fmt.Sprintf("alice-%d", i)
		full[p+"/"+workbench.Name] = workbench.Recorded{UID: "u", By: "alice", ActorRole: record.ActorRoleUser, ProjectUID: "uid-" + p}
		f.putJSON("/api/v1/namespaces/"+p, `{"metadata":{"name":"`+p+`","uid":"uid-`+p+`"}}`)
	}
	putOwnerRecords(t, f, full)
	u := userClientOf(c, fakeUserToken)
	u.SetUsername("alice")
	u.SetProjectUser("uid-proj")
	admin := &record.RecordOwner{Project: "proj", By: "kube:admin"}
	var full1 *record.RecordsFullError
	if err := record.AdmitUserEntry(u, "proj", admin, []byte(`{}`)); !errors.As(err, &full1) {
		t.Fatalf("taking over an admin's entry at the cap: %v", err)
	}
	other := &record.RecordOwner{Project: "proj", ProjectUID: "uid-proj", By: "bob", ActorRole: record.ActorRoleUser}
	if err := record.AdmitUserEntry(u, "proj", other, []byte(`{}`)); !errors.As(err, &full1) {
		t.Fatalf("taking over another user's entry at the cap: %v", err)
	}
	same := &record.RecordOwner{Project: "alice-1", ProjectUID: "uid-alice-1", By: "alice", ActorRole: record.ActorRoleUser}
	u1 := userClientOf(c, fakeUserToken)
	u1.SetUsername("alice")
	u1.SetProjectUser("uid-alice-1")
	if err := record.AdmitUserEntry(u1, "alice-1", same, []byte(`{}`)); err != nil {
		t.Fatalf("the same user's entry: %v", err)
	}
	if err := record.AdmitUserEntry(u1, "alice-1", same, []byte(strings.Repeat("x", record.MaxUserRecordEntryBytes+1))); !errors.As(err, &full1) {
		t.Fatalf("an entry too large: %v", err)
	}
	a := userClientOf(c, fakeUserToken)
	a.SetUsername("alice")
	if err := record.AdmitUserEntry(a, "proj", nil, []byte(strings.Repeat("x", 10*record.MaxUserRecordEntryBytes))); err != nil {
		t.Fatalf("a cluster-admin's entry: %v", err)
	}
}

// A workbench setup's preflight reviews "list localqueues" only where the
// setup reads them: in a Kueue-managed project while Kueue is available to
// the dashboard. A user whose role lacks it (Kueue absent, or an RHBoK
// whose roles do not aggregate into edit) is refused only there.
func TestWorkbenchPreflightReviewsLocalQueuesOnlyWhereRead(t *testing.T) {
	for _, tc := range []struct {
		name        string
		kueue       bool
		wantMissing bool
	}{
		{"not Kueue-managed", false, false},
		{"Kueue-managed with Kueue available", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newResourceFake(t)
			putWorkbenchCluster(t, f, true)
			if tc.kueue {
				f.putJSON(wbProjectNS, `{"metadata":{"name":"proj","labels":{"opendatahub.io/dashboard":"true","kueue.openshift.io/managed":"true"}}}`)
				f.putJSON(wbDSCv2+"/default-dsc", `{"metadata":{"name":"default-dsc"},"spec":{"components":{"workbenches":{"managementState":"Managed"},"kueue":{"managementState":"Unmanaged"}}}}`)
				f.putJSON("/apis/opendatahub.io/v1alpha/namespaces/"+wbApps+"/odhdashboardconfigs/odh-dashboard-config", `{"spec":{"dashboardConfig":{"disableKueue":false}}}`)
			}
			var asked []AccessRequest
			inner := f.intercept
			f.intercept = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
				if r.URL.Path != "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews" {
					return inner != nil && inner(w, r, body)
				}
				var review struct {
					Spec struct {
						ResourceAttributes AccessRequest `json:"resourceAttributes"`
					} `json:"spec"`
				}
				_ = json.Unmarshal(body, &review)
				asked = append(asked, review.Spec.ResourceAttributes)
				// An editor whose role does not include Kueue's LocalQueues.
				fmt.Fprintf(w, `{"status":{"allowed":%v}}`, review.Spec.ResourceAttributes.Resource != "localqueues")
				return true
			}
			u := userClientOf(c, fakeUserToken)
			missing, err := ReviewProjectActions(u, []string{"workbench.setup"}, "proj", wbApps)
			if err != nil {
				t.Fatal(err)
			}
			m := missing["workbench.setup"].Missing
			got := len(m) == 1 && m[0].Request.Resource == "localqueues"
			if got != tc.wantMissing || len(m) > 1 {
				t.Fatalf("missing %+v", missing)
			}
			reviewed := false
			for _, r := range asked {
				reviewed = reviewed || r.Resource == "localqueues"
			}
			if reviewed != tc.kueue {
				t.Fatalf("localqueues reviewed: %v", reviewed)
			}
		})
	}
}

// The review's decision is status.allowed, whatever evaluationError says
// beside it; a review without a decision or one the API server refuses
// fails the call with a fixed text: never the authorizer's or the API
// server's words.
func TestAccessReviewDecisionAndErrorText(t *testing.T) {
	const secret = "role reader-of-team-b not found in namespace team-b"
	f := &ssarFake{allow: func(AccessRequest) bool { return true }, body: func(r AccessRequest) string {
		allowed := r.Verb != "delete"
		return fmt.Sprintf(`{"status":{"allowed":%v,"evaluationError":%q}}`, allowed, secret)
	}}
	u := newSSARClient(t, f)
	denied, err := authz.ReviewUserAccess(u, []AccessRequest{{Verb: "get", Resource: "pods", Namespace: "p"}, {Verb: "delete", Resource: "pods", Namespace: "p"}})
	if err != nil || len(denied) != 1 || denied[0].Verb != "delete" {
		t.Fatalf("denied %v err %v", denied, err)
	}
	for name, body := range map[string]string{"no decision": `{"status":{"evaluationError":"` + secret + `"}}`, "not JSON": secret} {
		f.body = func(AccessRequest) string { return body }
		_, err := authz.ReviewUserAccess(u, []AccessRequest{{Verb: "get", Resource: "pods", Namespace: "p"}})
		if !errors.Is(err, authz.ErrAccessReviewFailed) || strings.Contains(err.Error(), "team-b") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	f.body = nil
	f.fail = func(AccessRequest) bool { return true }
	_, err = authz.ReviewUserAccess(u, []AccessRequest{{Verb: "get", Resource: "pods", Namespace: "p"}})
	if !errors.Is(err, authz.ErrAccessReviewFailed) || !strings.Contains(err.Error(), "HTTP 500") || strings.Contains(err.Error(), "Failure") {
		t.Fatalf("a refused review: %v", err)
	}
}

// The hard ceiling on users' persisted entries counts stale ones too (of
// projects that are gone, which the project caps skip): an entry that
// would exceed it is refused with the Clean up wording; an entry the same
// user already has in the project is not a new one.
func TestUserRecordEntriesHardCeiling(t *testing.T) {
	if record.MaxUserRecordBytes > 512<<10 {
		t.Fatalf("users' entries may take %d bytes of the 1 MiB ConfigMap", record.MaxUserRecordBytes)
	}
	if want := record.MaxUserRecordProjects * len(serving.ModelCatalog); record.MaxUserModelEntries < want {
		t.Fatalf("live model entries (%d: every catalog model in each project) reach their ceiling of %d", want, record.MaxUserModelEntries)
	}
	f, c := newResourceFake(t)
	stale := map[string]workbench.Recorded{}
	for i := range record.MaxUserRecordEntries {
		// Projects that are gone: no namespace in the fake.
		stale[fmt.Sprintf("gone-%d/%s", i, workbench.Name)] = workbench.Recorded{UID: "u", By: fmt.Sprintf("u-%d", i), ActorRole: record.ActorRoleUser, ProjectUID: fmt.Sprintf("uid-gone-%d", i)}
	}
	putOwnerRecords(t, f, stale)
	u := userClientOf(c, fakeUserToken)
	u.SetUsername("alice")
	u.SetProjectUser("uid-proj")
	var full *record.RecordsFullError
	if err := record.AdmitUserEntry(u, "proj", nil, []byte(`{}`)); !errors.As(err, &full) || !strings.Contains(full.Msg, "full (200 entries)") || !strings.Contains(full.Msg, "background check, which runs every 5 minutes") {
		t.Fatalf("a new entry past the ceiling: %v", err)
	}
	u0 := userClientOf(c, fakeUserToken)
	u0.SetUsername("u-0")
	u0.SetProjectUser("uid-gone-0")
	prior := &record.RecordOwner{Project: "gone-0", ProjectUID: "uid-gone-0", By: "u-0", ActorRole: record.ActorRoleUser}
	if err := record.AdmitUserEntry(u0, "gone-0", prior, []byte(`{}`)); err != nil {
		t.Fatalf("the same user's entry: %v", err)
	}
}

// The background check drops the workbench entries of projects that are
// gone, by namespace UID where recorded (a project made again under its
// name is gone too) and by name for older entries; live ones, and ones
// whose project cannot be read, stay.
func TestSweepDropsWorkbenchEntriesOfGoneProjects(t *testing.T) {
	f, c := newResourceFake(t)
	f.putJSON("/api/v1/namespaces/live", `{"metadata":{"uid":"uid-live"}}`)
	f.putJSON("/api/v1/namespaces/remade", `{"metadata":{"uid":"uid-remade-new"}}`)
	// Namespaces whose UID changed while what the entry records still
	// lives there (replaced between a setup's gate and its creates).
	f.putJSON("/api/v1/namespaces/remade-pvc", `{"metadata":{"uid":"uid-remade-pvc-new"}}`)
	f.putJSON("/api/v1/namespaces/remade-pvc/persistentvolumeclaims/"+workbench.StorageName, `{"metadata":{"uid":"pvc-live"}}`)
	f.putJSON("/api/v1/namespaces/remade-nb", `{"metadata":{"uid":"uid-remade-nb-new"}}`)
	f.putJSON("/api/v1/namespaces/remade-unreadable", `{"metadata":{"uid":"uid-ru-new"}}`)
	f.fail["GET /api/v1/namespaces/remade-unreadable/persistentvolumeclaims"] = 500
	f.putJSON("/api/v1/namespaces/legacy-live", `{"metadata":{"uid":"whatever"}}`)
	f.fail["GET /api/v1/namespaces/unreadable"] = 500
	putOwnerRecords(t, f, map[string]workbench.Recorded{
		"live/" + workbench.Name:              {UID: "a", ActorRole: record.ActorRoleUser, ProjectUID: "uid-live"},
		"gone/" + workbench.Name:              {UID: "b", ActorRole: record.ActorRoleUser, ProjectUID: "uid-gone"},
		"remade/" + workbench.Name:            {StorageUID: "pvc-gone", ActorRole: record.ActorRoleUser, ProjectUID: "uid-remade-old"},
		"remade-pvc/" + workbench.Name:        {StorageUID: "pvc-live", ActorRole: record.ActorRoleUser, ProjectUID: "uid-remade-pvc-old"},
		"remade-nb/" + workbench.Name:         {UID: "nb", StorageUID: "pvc-x", ActorRole: record.ActorRoleUser, ProjectUID: "uid-remade-nb-old"},
		"remade-unreadable/" + workbench.Name: {StorageUID: "pvc-y", ActorRole: record.ActorRoleUser, ProjectUID: "uid-ru-old"},
		"legacy-live/" + workbench.Name:       {UID: "d"},
		"legacy-gone/" + workbench.Name:       {UID: "e"},
		"unreadable/" + workbench.Name:        {UID: "f", ActorRole: record.ActorRoleUser, ProjectUID: "uid-x"},
	})
	SweepGoneProjectRecords(c)
	rec, err := workbench.ReadRecord(c)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Projects(); !slices.Equal(got, []string{"legacy-live", "live", "remade-nb", "remade-pvc", "remade-unreadable", "unreadable"}) {
		t.Fatalf("workbench entries after the sweep: %v", got)
	}
}
