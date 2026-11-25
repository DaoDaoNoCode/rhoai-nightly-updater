package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The roles of the project actions' matrix, as the fakes below decide
// them: a cluster-admin passes the mutation gate; the project's admin and
// editor have every request of the B3 actions (Kubernetes admin and edit,
// RHOAI's aggregated roles); a viewer reads only; "nothing" has no binding
// in the project (it is not one of their projects).
const (
	roleClusterAdmin = "cluster-admin"
	roleProjectAdmin = "project-admin"
	roleEditor       = "editor"
	roleViewer       = "viewer"
	roleNothing      = "nothing"
)

// projectGateEnv fakes the gate's reads for one user of the given role in
// project team-a, and counts what reached the actions.
type projectGateEnv struct {
	role    string
	runs    int
	reviews int
	// userClients: the action's client was marked a project user's.
	projectUser bool
}

func newProjectGateEnv(t *testing.T, role string) *projectGateEnv {
	t.Helper()
	setupDevMode(t)
	env := &projectGateEnv{role: role}
	mutationPermission = func(context.Context, string) (bool, error) { return role == roleClusterAdmin, nil }
	isUserProject = func(_ *cluster.Client, project string) (bool, error) {
		return project == "team-a" && role != roleNothing, nil
	}
	reviewProjectActions = func(_ *cluster.Client, ids []string, project, apps string) (map[string]cluster.ActionReview, error) {
		env.reviews++
		out := map[string]cluster.ActionReview{}
		for _, id := range ids {
			out[id] = cluster.ActionReview{}
			a, _ := cluster.ProjectActionByID(id)
			for _, r := range a.Requests {
				if role == roleViewer && r.Verb != "get" && r.Verb != "list" {
					req := cluster.AccessRequest{Verb: r.Verb, Group: r.Group, Resource: r.Resource, Namespace: project, Name: r.Name}
					r := out[id]
					r.Missing = append(r.Missing, cluster.MissingPermission{Request: req, Text: req.String() + ": ask an admin of project " + project + " to give you the edit role there."})
					out[id] = r
				}
			}
		}
		return out, nil
	}
	origW, origP, origS := runWorkbench, runPlayground, runMLflowSamples
	t.Cleanup(func() { runWorkbench, runPlayground, runMLflowSamples = origW, origP, origS })
	done := &types.OperationResponse{Success: true, Message: "done"}
	runWorkbench = func(c *cluster.Client, _ types.WorkbenchRequest, _ func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		env.runs++
		env.projectUser = c.ProjectUser()
		return done, nil
	}
	runPlayground = func(c *cluster.Client, _ types.PlaygroundRequest, _ func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		env.runs++
		env.projectUser = c.ProjectUser()
		return done, nil
	}
	runMLflowSamples = func(c *cluster.Client, _ types.MLflowSamplesRequest, _ func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		env.runs++
		env.projectUser = c.ProjectUser()
		return done, nil
	}
	return env
}

// projectActionRequests are a request of every project action, by ID.
var projectActionRequests = map[string][2]string{
	"workbench.setup":           {"/api/resources/workbench/stream", `{"action":"setup","project":"team-a"}`},
	"workbench.start":           {"/api/resources/workbench/stream", `{"action":"start","project":"team-a"}`},
	"workbench.stop":            {"/api/resources/workbench/stream", `{"action":"stop","project":"team-a"}`},
	"workbench.teardown":        {"/api/resources/workbench/stream", `{"action":"teardown","project":"team-a"}`},
	"workbench.delete-storage":  {"/api/resources/workbench/stream", `{"action":"delete-storage","project":"team-a"}`},
	"playground.setup":          {"/api/resources/playground/stream", `{"action":"setup","project":"team-a","models":[{"name":"m","source":"namespace"}]}`},
	"playground.teardown":       {"/api/resources/playground/stream", `{"action":"teardown","project":"team-a"}`},
	"mlflow-samples.add":        {"/api/resources/samples/mlflow/stream", `{"action":"add","project":"team-a","samples":["training"]}`},
	"mlflow-samples.remove":     {"/api/resources/samples/mlflow/stream", `{"action":"remove","project":"team-a","samples":["training"]}`},
	"mlflow-samples.regenerate": {"/api/resources/samples/mlflow/stream", `{"action":"regenerate","project":"team-a","samples":["genai-demo"]}`},
}

func serveProjectAction(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	Register(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON(t, path, "user:alice", body))
	return w
}

// The per-role matrix of every project action: cluster-admins (unchanged),
// the project's admin and editor run the actions users may run; a viewer
// is refused with what is missing; a user without a role in the project
// gets the "not one of your projects" answer; Regenerate stays a
// cluster-admin's. Every refusal comes before the action runs.
func TestProjectActionsPerRoleMatrix(t *testing.T) {
	for _, a := range cluster.ProjectActions() {
		req, ok := projectActionRequests[a.ID]
		if !ok {
			t.Fatalf("no request for %s", a.ID)
		}
		for _, role := range []string{roleClusterAdmin, roleProjectAdmin, roleEditor, roleViewer, roleNothing} {
			t.Run(a.ID+"/"+role, func(t *testing.T) {
				env := newProjectGateEnv(t, role)
				w := serveProjectAction(t, req[0], req[1])
				body := w.Body.String()
				var wantStatus int
				var wantCode string
				switch {
				case role == roleClusterAdmin:
					wantStatus = http.StatusOK
				case !a.ForUsers:
					wantStatus, wantCode = http.StatusForbidden, adminOnlyCode
				case role == roleNothing:
					wantStatus, wantCode = http.StatusForbidden, cluster.ErrorCodeForbiddenForUser
				case role == roleViewer:
					wantStatus, wantCode = http.StatusForbidden, missingPermissionsCode
				default:
					wantStatus = http.StatusOK
				}
				if w.Code != wantStatus || wantCode != "" && decodeError(t, w)["errorCode"] != wantCode {
					t.Fatalf("%d %s; want %d %s", w.Code, body, wantStatus, wantCode)
				}
				if wantStatus == http.StatusOK {
					if env.runs != 1 || !strings.Contains(body, `"step":"operation_complete"`) {
						t.Fatalf("the action did not run: %s", body)
					}
					if env.projectUser != (role != roleClusterAdmin) {
						t.Errorf("project user mark = %v for %s", env.projectUser, role)
					}
					if role == roleClusterAdmin && env.reviews != 0 {
						t.Error("a cluster-admin's action ran the preflight")
					}
					return
				}
				if env.runs != 0 {
					t.Fatal("the action ran after a refusal")
				}
				if !strings.Contains(decodeError(t, w)["error"].(string), "Nothing was changed") {
					t.Errorf("the refusal does not say that nothing was changed: %s", body)
				}
				if locks.clusterHeld() || len(inflight.list()) != 0 {
					t.Error("a lock or an operation is left after the refusal")
				}
			})
		}
	}
}

// The viewer's refusal lists every missing request, each with who can
// grant it.
func TestMissingPermissionsListsAll(t *testing.T) {
	newProjectGateEnv(t, roleViewer)
	w := serveProjectAction(t, "/api/resources/workbench/stream", `{"action":"setup","project":"team-a"}`)
	body := decodeError(t, w)
	missing, _ := body["missing"].([]any)
	a, _ := cluster.ProjectActionByID("workbench.setup")
	writes := 0
	for _, r := range a.Requests {
		if r.Verb != "get" && r.Verb != "list" {
			writes++
		}
	}
	if w.Code != http.StatusForbidden || len(missing) != writes || writes == 0 {
		t.Fatalf("%d missing %d of %d writes: %s", w.Code, len(missing), writes, w.Body.String())
	}
	for _, m := range missing {
		if !strings.Contains(m.(string), "ask an admin of project team-a to give you the edit role") {
			t.Errorf("no grantor named: %v", m)
		}
	}
}

// A preflight that cannot be made (a review failed, the applications
// namespace unknown) refuses before any write: fail closed.
func TestPreflightErrorRefusesBeforeWrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
		setup  func()
	}{
		{"review failed", http.StatusServiceUnavailable, "authorization_unavailable", func() {
			reviewProjectActions = func(*cluster.Client, []string, string, string) (map[string]cluster.ActionReview, error) {
				return nil, errors.New("access review: HTTP 500")
			}
		}},
		{"applications namespace unknown", http.StatusConflict, "prerequisites_unknown", func() {
			projectAppsNamespace = func(*cluster.Client) (string, error) { return "", cluster.ErrAppsNamespaceUnknown }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newProjectGateEnv(t, roleEditor)
			tc.setup()
			w := serveProjectAction(t, "/api/resources/workbench/stream", `{"action":"setup","project":"team-a"}`)
			if w.Code != tc.status || decodeError(t, w)["errorCode"] != tc.code || env.runs != 0 {
				t.Fatalf("%d %s runs=%d", w.Code, w.Body.String(), env.runs)
			}
		})
	}
}

// A project that does not exist and a project that is not the user's get
// the same answer, and the gate reads nothing with the ServiceAccount
// before it (no facts, no review).
func TestNotYoursEqualsDoesNotExistForActions(t *testing.T) {
	env := newProjectGateEnv(t, roleEditor)
	facts := 0
	projectFeatureBlock = func(*cluster.Client, string) (string, string) { facts++; return "", "" }
	projectAppsNamespace = func(*cluster.Client) (string, error) { facts++; return "apps", nil }
	var bodies []string
	for _, p := range []string{"someone-elses", "does-not-exist"} {
		w := serveProjectAction(t, "/api/resources/workbench/stream", `{"action":"setup","project":"`+p+`"}`)
		bodies = append(bodies, strings.ReplaceAll(w.Body.String(), p, "P"))
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body.String())
		}
	}
	if bodies[0] != bodies[1] || facts != 0 || env.reviews != 0 || env.runs != 0 {
		t.Fatalf("bodies %q, facts %d, reviews %d, runs %d", bodies, facts, env.reviews, env.runs)
	}
}

// A prerequisite that blocks (read with the facts) refuses admins and
// users alike, before the action.
func TestProjectActionPrerequisitesBlock(t *testing.T) {
	for _, role := range []string{roleClusterAdmin, roleEditor} {
		env := newProjectGateEnv(t, role)
		projectFeatureBlock = func(*cluster.Client, string) (string, string) {
			return "Cannot read whether Workbenches is turned on: the DataScienceCluster could not be read (HTTP 500).", "prerequisites_unknown"
		}
		w := serveProjectAction(t, "/api/resources/workbench/stream", `{"action":"setup","project":"team-a"}`)
		if w.Code != http.StatusConflict || decodeError(t, w)["errorCode"] != "prerequisites_unknown" || env.runs != 0 {
			t.Fatalf("%s: %d %s", role, w.Code, w.Body.String())
		}
	}
}

// A project action holds its project's lock: a cluster operation refuses
// it with the no-name wording for a user, and another action of the same
// user elsewhere refuses it naming only their own action.
func TestProjectRoutesBusyRefusalNamesNobody(t *testing.T) {
	newProjectGateEnv(t, roleEditor)
	// A cluster operation of another user runs.
	if refusal := takeCluster(t); refusal != nil {
		t.Fatal(refusal)
	}
	w := serveProjectAction(t, "/api/resources/workbench/stream", `{"action":"setup","project":"team-a"}`)
	releaseClusterMutationLock()
	msg, _ := decodeError(t, w)["error"].(string)
	if w.Code != http.StatusConflict || !strings.HasPrefix(msg, "A cluster operation is running since ") || strings.Contains(msg, "test-user") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if _, has := decodeError(t, w)["operation"]; has {
		t.Error("a user's refusal carries the operation")
	}

	// Another user's action in this project: its label and start, no name.
	c := cluster.NewClientWithContext(context.Background(), "sa-token")
	refusal := acquireProjectLock("uid-team-a", "bob", func() *Operation {
		return inflight.startProject("workbench", "Set up the workbench", "bob", "team-a", "uid-team-a", c)
	}, func(error) {})
	if refusal != nil {
		t.Fatal(refusal)
	}
	w = serveProjectAction(t, "/api/resources/playground/stream", `{"action":"teardown","project":"team-a"}`)
	msg, _ = decodeError(t, w)["error"].(string)
	if w.Code != http.StatusConflict || !strings.Contains(msg, "An action (Set up the workbench) is running in this project since") || strings.Contains(msg, "bob") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	releaseProjectLock("uid-team-a")

	// The user's own action in another project: named as theirs.
	refusal = acquireProjectLock("uid-team-b", "alice", func() *Operation {
		return inflight.startProject("workbench", "Set up the workbench", "alice", "team-b", "uid-team-b", c)
	}, func(error) {})
	if refusal != nil {
		t.Fatal(refusal)
	}
	defer releaseProjectLock("uid-team-b")
	w = serveProjectAction(t, "/api/resources/workbench/stream", `{"action":"setup","project":"team-a"}`)
	msg, _ = decodeError(t, w)["error"].(string)
	if w.Code != http.StatusConflict || msg != "You already have an action running (Set up the workbench in project team-b). Wait for it to finish." {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

// A cluster-admin's project action holds the project's lock, not the
// cluster's, and leaves neither behind.
func TestAdminProjectActionHoldsTheProjectLock(t *testing.T) {
	env := newProjectGateEnv(t, roleClusterAdmin)
	var held, clusterHeld bool
	runWorkbench = func(*cluster.Client, types.WorkbenchRequest, func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		env.runs++
		held, clusterHeld = locks.projectOp("uid-team-a") != "", locks.clusterHeld()
		return &types.OperationResponse{Success: true}, nil
	}
	w := serveProjectAction(t, "/api/resources/workbench/stream", `{"action":"stop","project":"team-a"}`)
	if w.Code != http.StatusOK || !held || clusterHeld {
		t.Fatalf("%d project lock %v cluster lock %v: %s", w.Code, held, clusterHeld, w.Body.String())
	}
	if locks.projectOp("uid-team-a") != "" || len(inflight.list()) != 0 {
		t.Fatal("the project lock is left")
	}
}

// Dev mode without a ServiceAccount counts the developer as a
// cluster-admin; DEV_ROLE=user turns that off, so the request's own token
// decides and a project action runs the preflight.
func TestDevBypassMakesAdminOnlyWithoutServiceAccount(t *testing.T) {
	setupDevMode(t)
	if !devAdminBypass() {
		t.Fatal("dev mode without a ServiceAccount token is not the developer's bypass")
	}
	cfg := Dev()
	cfg.Role = DevRoleUser
	setDev(t, cfg)
	if devAdminBypass() {
		t.Fatal("DEV_ROLE=user keeps the bypass")
	}
	if got := readDevConfig(func(k string) string { return map[string]string{"DEV_MODE": "true", "DEV_ROLE": "user"}[k] }); got.Role != DevRoleUser {
		t.Fatalf("DEV_ROLE not read: %+v", got)
	}
}

func TestDevRoleUserRunsThePreflight(t *testing.T) {
	env := newProjectGateEnv(t, roleEditor)
	cfg := Dev()
	cfg.Role = DevRoleUser
	setDev(t, cfg)
	asked := 0
	checkPermission = func(context.Context, string, string, string, string, string) (bool, error) {
		asked++
		return false, nil
	}
	mutationPermission = checkMutationPermission
	readSessionToken = func(context.Context, string) (*cluster.SessionToken, error) {
		return &cluster.SessionToken{Scopes: []string{"user:full"}}, nil
	}
	w := serveProjectAction(t, "/api/resources/workbench/stream", `{"action":"stop","project":"team-a"}`)
	if w.Code != http.StatusOK || asked == 0 || env.reviews != 1 || !env.projectUser {
		t.Fatalf("%d asked %d reviews %d user %v: %s", w.Code, asked, env.reviews, env.projectUser, w.Body.String())
	}
}

// A cluster-admin's MLflow samples action holds the cluster lock (it may
// restart the shared MLflow); a user's holds the project's.
func TestAdminSamplesHoldTheClusterLock(t *testing.T) {
	for _, role := range []string{roleClusterAdmin, roleEditor} {
		env := newProjectGateEnv(t, role)
		var clusterHeld, projectHeld bool
		runMLflowSamples = func(*cluster.Client, types.MLflowSamplesRequest, func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
			env.runs++
			clusterHeld, projectHeld = locks.clusterHeld(), locks.projectOp("uid-team-a") != ""
			return &types.OperationResponse{Success: true}, nil
		}
		w := serveProjectAction(t, "/api/resources/samples/mlflow/stream", `{"action":"add","project":"team-a","samples":["genai-demo"]}`)
		if w.Code != http.StatusOK || clusterHeld != (role == roleClusterAdmin) || projectHeld == (role == roleClusterAdmin) {
			t.Fatalf("%s: %d cluster %v project %v", role, w.Code, clusterHeld, projectHeld)
		}
		if locks.clusterHeld() || locks.projectOp("uid-team-a") != "" {
			t.Fatalf("%s: a lock is left", role)
		}
	}
}

// The prerequisites come before the preflight: a component that is off
// says so to a viewer too, instead of asking for the edit role, and no
// review is made.
func TestPrerequisitesBeforePreflight(t *testing.T) {
	env := newProjectGateEnv(t, roleViewer)
	projectFeatureBlock = func(*cluster.Client, string) (string, string) {
		return "Workbenches is turned off on this cluster.", "prerequisites"
	}
	w := serveProjectAction(t, "/api/resources/workbench/stream", `{"action":"setup","project":"team-a"}`)
	if w.Code != http.StatusConflict || decodeError(t, w)["errorCode"] != "prerequisites" || env.reviews != 0 || env.runs != 0 {
		t.Fatalf("%d %s reviews %d", w.Code, w.Body.String(), env.reviews)
	}
}

// The rate limit comes before the reviews: a second request within the
// window is refused without a review, and a refused request (here the
// viewer's) gives its slot back.
func TestRateLimitBeforePreflight(t *testing.T) {
	env := newProjectGateEnv(t, roleEditor)
	body := `{"action":"stop","project":"team-a"}`
	if w := serveProjectAction(t, "/api/resources/workbench/stream", body); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	w := serveProjectAction(t, "/api/resources/workbench/stream", body)
	if w.Code != http.StatusTooManyRequests || env.reviews != 1 {
		t.Fatalf("%d %s reviews %d", w.Code, w.Body.String(), env.reviews)
	}
	viewer := newProjectGateEnv(t, roleViewer)
	for range 2 {
		if w := serveProjectAction(t, "/api/resources/workbench/stream", body); w.Code != http.StatusForbidden {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	if viewer.reviews != 2 {
		t.Fatalf("a refusal kept the rate-limit slot: %d reviews", viewer.reviews)
	}
}

// No log line of a project action names the user.
func TestProjectActionLogsNameNobody(t *testing.T) {
	logs := captureLogs(t)
	newProjectGateEnv(t, roleViewer)
	serveProjectAction(t, "/api/resources/workbench/stream", `{"action":"setup","project":"team-a"}`)
	serveProjectAction(t, "/api/resources/workbench/stream", `{"action":"setup","project":"team-a"}`)
	newProjectGateEnv(t, roleEditor)
	reviewProjectActions = func(*cluster.Client, []string, string, string) (map[string]cluster.ActionReview, error) {
		return nil, errors.New("access review failed (HTTP 500)")
	}
	serveProjectAction(t, "/api/resources/workbench/stream", `{"action":"setup","project":"team-a"}`)
	if logs.Len() == 0 || strings.Contains(logs.String(), "alice") {
		t.Fatalf("logs:\n%s", logs.String())
	}
}
