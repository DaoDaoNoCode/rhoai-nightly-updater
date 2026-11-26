package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func getCapabilities(t *testing.T, project string) (*httptest.ResponseRecorder, types.ProjectCapabilities) {
	t.Helper()
	mux := http.NewServeMux()
	Register(mux)
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/resources/projects/capabilities?project="+project, nil)
	r.Header.Set("X-Forwarded-Access-Token", "user:alice")
	mux.ServeHTTP(w, r)
	var out types.ProjectCapabilities
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return w, out
}

// The capabilities of each role for every project action: what the gate
// would decide (TestProjectActionsPerRoleMatrix), with the reason a
// control shows.
func TestProjectCapabilitiesPerRole(t *testing.T) {
	for _, role := range []string{roleClusterAdmin, roleProjectAdmin, roleEditor, roleViewer, roleNothing} {
		t.Run(role, func(t *testing.T) {
			newProjectGateEnv(t, role)
			w, caps := getCapabilities(t, "team-a")
			if role == roleNothing {
				if w.Code != http.StatusForbidden || decodeError(t, w)["errorCode"] != cluster.ErrorCodeForbiddenForUser {
					t.Fatalf("%d %s", w.Code, w.Body.String())
				}
				return
			}
			if w.Code != http.StatusOK {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			for _, a := range cluster.ProjectActions() {
				cp, ok := caps.Actions[a.ID]
				if !ok {
					t.Fatalf("%s missing", a.ID)
				}
				adminOnlyHere := role == roleEditor && needsProjectAdmin(a)
				want := role == roleClusterAdmin && a.AdminRefusal == "" || (a.ForUsers && role != roleViewer && role != roleClusterAdmin && !adminOnlyHere)
				if cp.Allowed != want {
					t.Errorf("%s: allowed %v, want %v (%+v)", a.ID, cp.Allowed, want, cp)
				}
				switch {
				case want && (cp.Reason != "" || len(cp.Missing) != 0):
					t.Errorf("%s: allowed with a reason %+v", a.ID, cp)
				case !want && role == roleClusterAdmin && (cp.Code != notForClusterAdminsCode || cp.Reason != a.AdminRefusal):
					t.Errorf("%s: %+v", a.ID, cp)
				case !want && role != roleClusterAdmin && !a.ForUsers && cp.Code != adminOnlyCode:
					t.Errorf("%s: %+v", a.ID, cp)
				case !want && adminOnlyHere && (cp.Code != missingPermissionsCode || !strings.Contains(cp.Reason, "ask an admin of project team-a")):
					t.Errorf("%s: %+v", a.ID, cp)
				case !want && role == roleViewer && a.ForUsers && (cp.Code != missingPermissionsCode || len(cp.Missing) == 0 || !strings.Contains(cp.Reason, "edit role")):
					t.Errorf("%s: %+v", a.ID, cp)
				}
			}
			if wantRole := map[bool]string{true: roleAdmin, false: roleUser}[role == roleClusterAdmin]; caps.Role != wantRole {
				t.Errorf("role %q", caps.Role)
			}
		})
	}
}

// "Not yours" and "does not exist" get the same status and body, and no
// ServiceAccount fact and no review is read for either.
func TestCapabilitiesNotYoursEqualsDoesNotExist(t *testing.T) {
	env := newProjectGateEnv(t, roleEditor)
	facts := 0
	projectFeatureBlock = func(*cluster.Client, string) (string, string) { facts++; return "", "" }
	projectAppsNamespace = func(*cluster.Client) (string, error) { facts++; return "apps", nil }
	var bodies []string
	codes := map[int]bool{}
	for _, p := range []string{"someone-elses", "does-not-exist"} {
		w, _ := getCapabilities(t, p)
		codes[w.Code] = true
		bodies = append(bodies, strings.ReplaceAll(w.Body.String(), p, "P"))
	}
	if len(codes) != 1 || !codes[http.StatusForbidden] || bodies[0] != bodies[1] || facts != 0 || env.reviews != 0 {
		t.Fatalf("codes %v bodies %q facts %d reviews %d", codes, bodies, facts, env.reviews)
	}
}

// The reviews are made once per minute per user and project; a
// prerequisite that blocks names its code on each action of the feature.
func TestCapabilitiesCacheAndBlocks(t *testing.T) {
	env := newProjectGateEnv(t, roleEditor)
	projectFeatureBlock = func(_ *cluster.Client, feature string) (string, string) {
		if feature == "playground" {
			return "The Gen AI playground is turned off on this cluster.", "prerequisites"
		}
		return "", ""
	}
	getCapabilities(t, "team-a")
	_, caps := getCapabilities(t, "team-a")
	if env.reviews != 1 {
		t.Fatalf("%d reviews for two reads within the minute", env.reviews)
	}
	if cp := caps.Actions["playground.setup"]; cp.Allowed || len(cp.BlockedBy) != 1 || cp.BlockedBy[0] != "prerequisites" {
		t.Fatalf("%+v", cp)
	}
	if !caps.Actions["workbench.setup"].Allowed {
		t.Fatal("the playground's block reached the workbench")
	}
	testClock.Advance(capabilitiesTTL + 1)
	getCapabilities(t, "team-a")
	if env.reviews != 2 {
		t.Fatalf("%d reviews after the minute", env.reviews)
	}
}

// A user sees only their own activity entries, by the identity verified
// with their token (never the X-Forwarded-User header); a cluster-admin
// sees all.
func TestUserSeesOnlyOwnEntries(t *testing.T) {
	entries := []types.ActivityEntry{{User: "alice", Action: "workbench"}, {User: "bob", Action: "update"}, {User: "alice", Action: "playground"}}
	if got := ownActivity(entries, "alice"); len(got) != 2 || got[0].User != "alice" || got[1].User != "alice" {
		t.Fatalf("%+v", got)
	}
	if got := ownActivity(entries, ""); len(got) != 0 {
		t.Fatalf("no identity: %+v", got)
	}
	for _, role := range []string{roleClusterAdmin, roleEditor} {
		newProjectGateEnv(t, role)
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/activity", nil)
		r.Header.Set("X-Forwarded-Access-Token", "user:alice")
		r.Header.Set("X-Forwarded-User", "bob")
		_, r, ok := requestUser(httptest.NewRecorder(), r)
		if !ok || verifiedUser(r) != "alice" {
			t.Fatalf("identity %q", verifiedUser(r))
		}
		admin, _ := requestRole(httptest.NewRecorder(), r)
		if admin != (role == roleClusterAdmin) {
			t.Fatalf("%s: admin %v", role, admin)
		}
	}
}

// A user's workbench read names one of their projects, checked first, and
// shows that project's workbench only.
func TestWorkbenchStatusPerRole(t *testing.T) {
	orig, origProject := getWorkbenchStatus, getWorkbenchProjectStatus
	t.Cleanup(func() { getWorkbenchStatus, getWorkbenchProjectStatus = orig, origProject })
	reads, projectReads := 0, 0
	getWorkbenchProjectStatus = func(sa, u *cluster.Client, project string) (*types.WorkbenchStatus, error) {
		projectReads++
		return getWorkbenchStatus(sa, u, project)
	}
	getWorkbenchStatus = func(_, _ *cluster.Client, project string) (*types.WorkbenchStatus, error) {
		reads++
		return &types.WorkbenchStatus{Project: project, Items: []types.WorkbenchState{
			{ResourceState: types.ResourceState{Name: "nightly-workbench", Namespace: "team-a"}},
			{ResourceState: types.ResourceState{Name: "nightly-workbench", Namespace: "other"}},
		}, ItemsUnreadable: []string{"other"}, ItemsProblem: "Cannot read the workbench in other: x."}, nil
	}
	get := func(project string) *httptest.ResponseRecorder {
		mux := http.NewServeMux()
		Register(mux)
		w := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/resources/workbench?project="+project, nil)
		r.Header.Set("X-Forwarded-Access-Token", "user:alice")
		mux.ServeHTTP(w, r)
		return w
	}
	newProjectGateEnv(t, roleEditor)
	for _, p := range []string{"", "someone-elses"} {
		if w := get(p); w.Code < 400 || reads != 0 {
			t.Fatalf("%q: %d %s reads %d", p, w.Code, w.Body.String(), reads)
		}
	}
	w := get("team-a")
	var st types.WorkbenchStatus
	json.Unmarshal(w.Body.Bytes(), &st)
	if w.Code != http.StatusOK || len(st.Items) != 1 || st.Items[0].Namespace != "team-a" || len(st.ItemsUnreadable) != 0 || strings.Contains(w.Body.String(), "other") || projectReads != 1 {
		t.Fatalf("%d %s (project reads %d)", w.Code, w.Body.String(), projectReads)
	}
	newProjectGateEnv(t, roleClusterAdmin)
	w = get("team-a")
	json.Unmarshal(w.Body.Bytes(), &st)
	if w.Code != http.StatusOK || len(st.Items) != 2 || projectReads != 1 {
		t.Fatalf("admin: %d %s (project reads %d)", w.Code, w.Body.String(), projectReads)
	}
}

// A failure counts only for the actions it concerns: an applications
// namespace that cannot be read refuses the actions that read there
// (workbench set up), not Stop; a sign-in that cannot carry a change and
// a project whose UID cannot be read refuse every action, as the gate does.
func TestCapabilitiesScopeFailuresToTheirActions(t *testing.T) {
	newProjectGateEnv(t, roleEditor)
	projectAppsNamespace = func(*cluster.Client) (string, error) { return "", cluster.ErrAppsNamespaceUnknown }
	_, caps := getCapabilities(t, "team-a")
	if cp := caps.Actions["workbench.setup"]; cp.Allowed || cp.Code != "prerequisites_unknown" {
		t.Fatalf("setup: %+v", cp)
	}
	if !caps.Actions["workbench.stop"].Allowed || !caps.Actions["playground.teardown"].Allowed {
		t.Fatalf("an applications namespace failure reached other actions: %+v", caps.Actions)
	}

	newProjectGateEnv(t, roleEditor)
	readProjectUID = func(*cluster.Client, string) (string, error) { return "", errors.New("HTTP 500") }
	_, caps = getCapabilities(t, "team-a")
	for id, cp := range caps.Actions {
		if cp.Allowed {
			t.Errorf("%s allowed while the gate cannot read the project's UID", id)
		}
	}
}

// An action the checks give no answer for is never allowed.
func TestMissingVerdictIsNotAllowed(t *testing.T) {
	if v := verdictOf(map[string]actionVerdict{}, "workbench.setup"); v.reason == "" || v.code != "authorization_unavailable" {
		t.Fatalf("%+v", v)
	}
	newProjectGateEnv(t, roleEditor)
	reviewProjectActions = func(*cluster.Client, []string, string, string) (map[string]cluster.ActionReview, error) {
		// An answer that leaves an action out.
		return map[string]cluster.ActionReview{"workbench.stop": {}}, nil
	}
	_, caps := getCapabilities(t, "team-a")
	if !caps.Actions["workbench.stop"].Allowed {
		t.Fatal("the answered action is not allowed")
	}
	if cp := caps.Actions["workbench.setup"]; cp.Allowed || cp.Code != "authorization_unavailable" {
		t.Fatalf("an action without an answer: %+v", cp)
	}
	w := serveProjectAction(t, "/api/resources/workbench/stream", `{"action":"setup","project":"team-a"}`)
	if w.Code != http.StatusServiceUnavailable || decodeError(t, w)["errorCode"] != "authorization_unavailable" {
		t.Fatalf("the gate: %d %s", w.Code, w.Body.String())
	}
}
