package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// GET /api/operation per role while project actions run (the non-admin
// design, B4).

// localAction is a project action of this process: user's, in project
// "team-<uid>" (namespace UID uid), holding its lock (no lease: no
// bookkeeper). It returns its ID.
func localAction(t *testing.T, uid, user string) string {
	t.Helper()
	var id string
	refusal := acquireProjectLock(uid, user, func() *Operation {
		op := inflight.startProject("workbench", "Set up the workbench", user, "team-"+uid, uid, nil)
		id = op.ID
		return op
	}, nil)
	if refusal != nil {
		t.Fatalf("lock of %s: %+v", uid, refusal)
	}
	t.Cleanup(func() { releaseProjectLock(uid) })
	return id
}

// stubUserProjects makes the projects API with a user's token answer
// projects (name -> namespace UID) or err, and counts the calls.
func stubUserProjects(t *testing.T, projects map[string]string, err error) *int {
	t.Helper()
	orig := listUserDSProjects
	t.Cleanup(func() { listUserDSProjects = orig })
	calls := 0
	listUserDSProjects = func(*cluster.Client) ([]cluster.UserProject, error) {
		calls++
		var out []cluster.UserProject
		for name, uid := range projects {
			out = append(out, cluster.UserProject{Name: name, UID: uid})
		}
		return out, err
	}
	return &calls
}

func getUserOperation(t *testing.T, token string) (types.OperationSummary, string) {
	t.Helper()
	w := httptest.NewRecorder()
	HandleOperation(w, getAs(t, "/api/operation", token))
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var sum types.OperationSummary
	if err := json.Unmarshal(w.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	return sum, w.Body.String()
}

// What GET /api/operation tells each role while project actions run: an
// admin everything (who, where); a user the actions in their own projects,
// their own with its ID, other people's by label, project and start only,
// and nothing of other projects. No user's answer names another user, a
// pod or another person's action ID.
func TestBannerNamesNobodyForUsers(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	stubUserProjects(t, map[string]string{"team-a": "a", "team-b": "b", "team-c": "c"}, nil)
	// Live leases of another pod (renewed now; started two minutes ago).
	now := clk.Now()
	readOperationState = func(*cluster.Client) (*cluster.OperationRecord, error) {
		return &cluster.OperationRecord{ProjectLocks: map[string]*types.OperationLease{
			"a": otherPodProjectLease("a", "alice", now), // alice in carol's team-a
			"x": otherPodProjectLease("x", "dave", now),  // dave in team-x, not carol's
			"c": otherPodProjectLease("c", "carol", now), // carol's own, in another pod
		}}, nil
	}
	ownID := localAction(t, "b", "carol")

	sum, body := getUserOperation(t, "user:carol")
	byProject := map[string]types.OperationSummaryEntry{}
	for _, e := range sum.Operations {
		if e.Scope != scopeProject {
			t.Fatalf("a cluster entry while none runs: %+v", e)
		}
		byProject[e.Project] = e
	}
	if len(byProject) != 3 {
		t.Fatalf("carol sees %v", sum.Operations)
	}
	if e := byProject["team-b"]; !e.Own || e.ID != ownID || e.Label != "Set up the workbench" {
		t.Errorf("her own action here: %+v", e)
	}
	if e := byProject["team-c"]; !e.Own || e.ID != "remote-c" {
		t.Errorf("her own action in another pod: %+v", e)
	}
	if e := byProject["team-a"]; e.Own || e.ID != "" || e.Label != "Workbench" || e.StartedAt != now.Add(-2*time.Minute).UTC().Format(time.RFC3339) {
		t.Errorf("alice's action in carol's project: %+v", e)
	}
	for _, hidden := range []string{"alice", "dave", "team-x", "remote-a", "remote-x", "updater-old-pod", "rhoai-3.6", "Waiting for OLM"} {
		if strings.Contains(body, hidden) {
			t.Errorf("carol's answer names %q: %s", hidden, body)
		}
	}

	// A cluster operation: its label and start only.
	readOperationState = func(*cluster.Client) (*cluster.OperationRecord, error) {
		return &cluster.OperationRecord{Lease: otherPodLease(clk.Now())}, nil
	}
	releaseProjectLock("b")
	sum, body = getUserOperation(t, "user:carol")
	if len(sum.Operations) != 1 || sum.Operations[0].Scope != scopeCluster || sum.Operations[0].Project != "" || sum.Operations[0].ID != "" || strings.Contains(body, "alice") {
		t.Errorf("a cluster operation for a user: %s", body)
	}

	// An admin: who runs what where.
	readOperationState = func(*cluster.Client) (*cluster.OperationRecord, error) {
		return &cluster.OperationRecord{ProjectLocks: map[string]*types.OperationLease{"x": otherPodProjectLease("x", "dave", now)}}, nil
	}
	adminID := localAction(t, "b2", "carol")
	status := getAdminOperation(t)
	var local, remote *Operation
	for _, op := range status.Operations {
		switch op.ID {
		case adminID:
			local = op
		case "remote-x":
			remote = op
		}
	}
	if local == nil || local.User != "carol" || local.Project != "team-b2" {
		t.Errorf("an admin's view of this process's action: %+v", local)
	}
	if remote == nil || remote.User != "dave" || remote.Project != "team-x" {
		t.Errorf("an admin's view of another pod's action: %+v", remote)
	}
}

// A user's answer lists a project action only in a project that is theirs
// now with the same namespace UID, their own actions too: not in a project
// they lost access to (a revoked starter), not in one deleted and made
// again under its name, not when the projects cannot be read (fail closed),
// not without the user's identity. The projects are not read while no
// project action runs (idle polls).
func TestUserNeverSeesProjectsNotTheirs(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	now := clk.Now()
	readOperationState = func(*cluster.Client) (*cluster.OperationRecord, error) {
		return &cluster.OperationRecord{ProjectLocks: map[string]*types.OperationLease{
			"a": otherPodProjectLease("a", "alice", now),
			"c": otherPodProjectLease("c", "carol", now),
		}}, nil
	}
	for _, tc := range []struct {
		name     string
		projects map[string]string
		err      error
	}{
		{"neither project is theirs (carol's access to team-c was revoked)", map[string]string{"team-z": "z"}, nil},
		{"no projects", nil, nil},
		{"both projects made again under their names", map[string]string{"team-a": "a-new", "team-c": "c-new"}, nil},
		{"projects unreadable", map[string]string{"team-a": "a", "team-c": "c"}, errors.New("projects API down")},
	} {
		stubUserProjects(t, tc.projects, tc.err)
		sum, body := getUserOperation(t, "user:carol")
		if len(sum.Operations) != 0 {
			t.Errorf("%s: %s", tc.name, body)
		}
	}

	// Idle: the projects are not read.
	readOperationState = func(*cluster.Client) (*cluster.OperationRecord, error) { return &cluster.OperationRecord{}, nil }
	calls := stubUserProjects(t, map[string]string{"team-a": "a"}, nil)
	getUserOperation(t, "user:carol")
	readOperationState = func(*cluster.Client) (*cluster.OperationRecord, error) {
		return &cluster.OperationRecord{Lease: otherPodLease(clk.Now())}, nil
	}
	getUserOperation(t, "user:carol")
	if *calls != 0 {
		t.Errorf("projects read %d times with no project action running", *calls)
	}

	// No identity: no project action at all, the projects not even read.
	status := OperationStatus{Operations: []*Operation{
		{Scope: scopeCluster, Label: "Update to nightly", StartedAt: now},
		{Scope: scopeProject, Label: "Workbench", User: "alice", Project: "team-a", ProjectUID: "a", ID: "x"},
		{Scope: scopeProject, Label: "Workbench", User: "", Project: "team-b", ProjectUID: "b", ID: "y"},
	}}
	read := 0
	sum := operationSummary(status, "", func() ([]cluster.UserProject, error) {
		read++
		return []cluster.UserProject{{Name: "team-a", UID: "a"}, {Name: "team-b", UID: "b"}}, nil
	})
	if len(sum.Operations) != 1 || sum.Operations[0].Scope != scopeCluster || read != 0 {
		t.Errorf("no identity: %+v (projects read %d)", sum.Operations, read)
	}
}
