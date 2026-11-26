package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The gate of project actions (the non-admin design, section 2.2).
//
// A project action acts in one data science project (the workbench, the
// playground, the MLflow samples: cluster.ProjectActions). Its route goes
// through withProjectMutationAuth instead of withMutationAuth. A
// cluster-admin passes as before (their "*" rule covers every request); a
// signed-in user who is not one may run the actions cluster.ProjectActions
// marks ForUsers in one of their own data science projects when an access
// review with their own token allows every request the action can send.
// Every check runs before anything changes, in this order:
//
//  1. who the user is (requestUser: users/~ with their token);
//  2. the body names a valid project and action (before any lock);
//  3. their sign-in can carry the change (sessionProblem);
//  4. their role: cluster-admin (mutationPermission) skips 5 to 7;
//  5. the action is one users may run;
//  6. the project is one of their data science projects (the projects API
//     with their token), before anything is read with the ServiceAccount;
//     a project that does not exist gets the same answer;
//  7. the preflight: one SelfSubjectAccessReview per request of the action,
//     with their token (cluster.ReviewProjectActions); a review that fails
//     refuses, and a missing permission refuses with the full list;
//  8. the cluster's prerequisites, read with the ServiceAccount's facts
//     client (cluster.ProjectFeatureBlock);
//  9. the rate limit, the shutdown gate, and the project's lock (by its
//     namespace UID, read with the user's token).
//
// Each refusal says that nothing was changed.

// projectRoute is a route of project actions: its feature, its body's
// size limit, and how its body names the project and the action
// (validated as the handler validates it).
type projectRoute struct {
	feature string
	maxBody int64
	parse   func(body []byte) (project, action, problem string)
	// adminClusterLock: a cluster-admin's request takes the cluster lock,
	// not the project's, because it can change something shared: the
	// MLflow samples' Add and Regenerate of the demo remove an old memory
	// limit from the cluster-wide MLflow CR, which restarts MLflow for
	// every project (mlflow raiseOldMemoryLimit). A user's never does
	// (refused), so theirs holds the project's lock.
	adminClusterLock bool
}

// projectActionRoutes are the routes of project actions, by path: the
// only routes behind withProjectMutationAuth (TestEveryNonGetRouteIsGated).
var projectActionRoutes = map[string]projectRoute{
	"/api/resources/workbench/stream": {feature: "workbench", maxBody: 4096, parse: func(body []byte) (string, string, string) {
		var req types.WorkbenchRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return "", "", "invalid request body"
		}
		return req.Project, req.Action, cluster.ValidateWorkbenchRequest(req)
	}},
	"/api/resources/playground/stream": {feature: "playground", maxBody: 16384, parse: func(body []byte) (string, string, string) {
		var req types.PlaygroundRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return "", "", "invalid request body"
		}
		return req.Project, req.Action, cluster.ValidatePlaygroundRequest(req)
	}},
	"/api/resources/project-s3/stream": {feature: "project-s3", maxBody: 4096, parse: func(body []byte) (string, string, string) {
		var req types.ProjectStorageRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return "", "", "invalid request body"
		}
		return req.Project, req.Action, cluster.ValidateProjectStorageRequest(req)
	}},
	"/api/resources/project-pipeline-server/stream": {feature: "project-pipeline-server", maxBody: pipelineServerBodyLimit, parse: func(body []byte) (string, string, string) {
		var req types.ProjectPipelineServerRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return "", "", "invalid request body"
		}
		if !validConfirmWorkbenches(req.ConfirmWorkbenches) {
			return "", "", fmt.Sprintf("confirmWorkbenches maps at most %d workbench names to their UIDs", maxConfirmWorkbenches)
		}
		return req.Project, req.Action, cluster.ValidateProjectPipelineServerRequest(req)
	}},
	"/api/resources/samples/mlflow/stream": {feature: "mlflow-samples", maxBody: 4096, adminClusterLock: true, parse: func(body []byte) (string, string, string) {
		var req types.MLflowSamplesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return "", "", "invalid request body"
		}
		if !namespaceRegex.MatchString(req.Project) {
			return "", "", "a valid project is required"
		}
		return req.Project, req.Action, cluster.ValidateMLflowSamplesRequest(req)
	}},
	// A model in one project (cluster.RunProjectModel), reached only inside
	// the cluster: never a gateway, MaaS or anything outside the project,
	// so a cluster-admin's request holds the project's lock too. A request
	// for the gateway or token authentication is refused (validation).
	"/api/resources/models/project/stream": {feature: "models", maxBody: 4096, parse: func(body []byte) (string, string, string) {
		var req types.ProjectModelRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return "", "", "invalid request body"
		}
		return req.Project, cluster.ProjectModelAction(req), cluster.ValidateProjectModelRequest(req)
	}},
}

// The project actions' cluster reads and checks; tests replace them.
var (
	isUserProject        = cluster.IsUserDSProject
	readProjectUID       = cluster.ProjectUID
	reviewProjectActions = cluster.ReviewProjectActions
	projectFeatureBlock  = cluster.ProjectFeatureBlock
	projectAppsNamespace = cluster.ProjectAppsNamespace
)

// notYourProjectMessage answers a project that is not one of the user's
// data science projects, and one that does not exist: the same words, so
// the answer tells nothing about other projects.
func notYourProjectMessage(project string) string {
	return fmt.Sprintf("Project %s is not one of your data science projects.", project)
}

// adminOnlyActionMessage refuses a project action users may not run.
func adminOnlyActionMessage(label string) string {
	return label + " is done by a cluster-admin in this tool. Nothing was changed."
}

// missingPermissionsCode is the errorCode of a preflight refusal.
const missingPermissionsCode = "missing_permissions"

// notForClusterAdminsCode refuses a cluster-admin a project action that
// is not theirs to run (cluster.ProjectAction.AdminRefusal): setting up
// project S3 storage, and a pipeline server on it.
const notForClusterAdminsCode = "not_for_cluster_admins"

// writeMissingPermissions answers 403 missing_permissions with the list.
func writeMissingPermissions(w http.ResponseWriter, label, project string, missing []cluster.MissingPermission) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	body := map[string]any{"error": cluster.MissingSummary(label, project, missing), "errorCode": missingPermissionsCode, "missing": cluster.MissingTexts(missing)}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("response encode error", "label", "missing-permissions", "error", err)
	}
}

// projectCheckClient is the user's client of the gate's checks: their own
// token, with the ServiceAccount's client for the cluster facts only.
func projectCheckClient(ctx context.Context, userToken, username, saToken string) *cluster.Client {
	u := cluster.NewUserClient(ctx, userToken)
	u.SetUsername(username)
	sa := cluster.NewClientWithContext(ctx, saToken)
	sa.SetUsername(username)
	u.SetFacts(sa)
	return u
}

// userProjectCheck is step 6: whether project is one of the user's data
// science projects (u: their client). It answers the refusal itself and
// returns false when the request must stop.
func userProjectCheck(w http.ResponseWriter, u *cluster.Client, project, refused string) bool {
	ok, err := isUserProject(u, project)
	if errors.Is(err, cluster.ErrUnauthenticated) || cluster.IsK8sError(err, http.StatusUnauthorized) {
		writeError(w, "Your OpenShift session has expired. Log in again.", http.StatusUnauthorized, "session_expired")
		return false
	}
	if err != nil {
		slog.Warn("cannot list the user's data science projects", "error", err)
		writeError(w, "Cannot list your data science projects. "+refused, http.StatusServiceUnavailable, "authorization_unavailable")
		return false
	}
	if !ok {
		writeError(w, notYourProjectMessage(project)+" "+refused, http.StatusForbidden, cluster.ErrorCodeForbiddenForUser)
		return false
	}
	return true
}

// actionVerdict is a project action's answer to the gate's checks 5, 7
// and 8 (projectActionVerdicts): "" reason when it may run, else the
// refusal's HTTP status, errorCode and reason (without "Nothing was
// changed"), and for a preflight refusal what is missing.
type actionVerdict struct {
	status  int
	code    string
	reason  string
	missing []cluster.MissingPermission
}

// projectActionVerdicts runs the gate's checks after the project check,
// for the actions ids of a request (one) or of the capabilities (all), in
// the gate's order: the action is one the user may run; the cluster's
// prerequisites of its feature (the facts: a component that is off says
// so, not "ask for edit"); then the preflight with the user's token u,
// every distinct request once. A failure counts only for the actions it
// concerns: an applications namespace that cannot be read refuses only
// the actions that read there. Texts never carry an API server's,
// authorizer's or ServiceAccount's words, only an HTTP status or category.
func projectActionVerdicts(u *cluster.Client, admin bool, ids []string, project string) map[string]actionVerdict {
	out := map[string]actionVerdict{}
	blocks := map[string][2]string{}
	var review []string
	for _, id := range ids {
		a, known := cluster.ProjectActionByID(id)
		label := a.Label
		if !known {
			label = id
		}
		if !admin && (!known || !a.ForUsers) {
			out[id] = actionVerdict{status: http.StatusForbidden, code: adminOnlyCode, reason: label + " is done by a cluster-admin in this tool."}
			continue
		}
		if admin && a.AdminRefusal != "" {
			out[id] = actionVerdict{status: http.StatusForbidden, code: notForClusterAdminsCode, reason: a.AdminRefusal}
			continue
		}
		b, seen := blocks[a.Feature]
		if !seen {
			b[0], b[1] = projectFeatureBlock(u, a.Feature)
			blocks[a.Feature] = b
		}
		if b[0] != "" {
			out[id] = actionVerdict{status: http.StatusConflict, code: b[1], reason: b[0]}
			continue
		}
		if admin {
			out[id] = actionVerdict{}
			continue
		}
		review = append(review, id)
	}
	if len(review) == 0 {
		return out
	}
	apps := ""
	if cluster.NeedsAppsNamespace(review) {
		ns, err := projectAppsNamespace(u)
		if err != nil {
			// Only the actions that read in the applications namespace
			// cannot be checked; the others are.
			var rest []string
			for _, id := range review {
				if !cluster.NeedsAppsNamespace([]string{id}) {
					rest = append(rest, id)
					continue
				}
				a, _ := cluster.ProjectActionByID(id)
				out[id] = actionVerdict{status: http.StatusConflict, code: "prerequisites_unknown", reason: fmt.Sprintf("Cannot check %s in %s: %v.", a.Label, project, err)}
			}
			review = rest
			if len(review) == 0 {
				return out
			}
		}
		apps = ns
	}
	reviews, err := reviewProjectActions(u, review, project, apps)
	for _, id := range review {
		a, _ := cluster.ProjectActionByID(id)
		rv, answered := reviews[id]
		switch {
		case errors.Is(err, cluster.ErrUnauthenticated):
			out[id] = actionVerdict{status: http.StatusUnauthorized, code: "session_expired", reason: "Your OpenShift session has expired. Log in again."}
		case err != nil:
			out[id] = actionVerdict{status: http.StatusServiceUnavailable, code: "authorization_unavailable", reason: fmt.Sprintf("Cannot check your permissions for %s in %s (%v); try again.", a.Label, project, err)}
		case !answered:
			// A review that gave this action no answer: fail closed.
			out[id] = actionVerdict{status: http.StatusServiceUnavailable, code: "authorization_unavailable", reason: fmt.Sprintf("Cannot check your permissions for %s in %s; try again.", a.Label, project)}
		case rv.Err != nil:
			out[id] = actionVerdict{status: http.StatusConflict, code: "prerequisites_unknown", reason: fmt.Sprintf("Cannot check %s in %s: %v.", a.Label, project, rv.Err)}
		case len(rv.Missing) > 0:
			out[id] = actionVerdict{status: http.StatusForbidden, code: missingPermissionsCode, reason: cluster.MissingSummaryFor(a.Label, project, rv.Missing), missing: rv.Missing}
		default:
			out[id] = actionVerdict{}
		}
	}
	if err != nil {
		slog.Warn("project action preflight failed", "project", project, "error", err)
	}
	return out
}

// verdictOf is the verdict of id, fail closed: an action the checks gave
// no answer for is not allowed (authorization_unavailable).
func verdictOf(verdicts map[string]actionVerdict, id string) actionVerdict {
	v, ok := verdicts[id]
	if !ok {
		return actionVerdict{status: http.StatusServiceUnavailable, code: "authorization_unavailable", reason: "Your permissions for this action could not be checked; try again."}
	}
	return v
}

// projectStartProblem is what keeps an accepted project action from
// taking its lock, shared by the gate and the capabilities: the project's
// namespace UID (the lock's key) cannot be read with the user's token. A
// user gets the "not one of your projects" answer then.
func projectStartProblem(u *cluster.Client, project string, admin bool) (uid string, status int, code, reason string) {
	uid, err := readProjectUID(u, project)
	if err != nil {
		if !admin {
			return "", http.StatusForbidden, cluster.ErrorCodeForbiddenForUser, notYourProjectMessage(project)
		}
		status, code := cluster.HTTPStatusForError(err)
		return "", status, code, fmt.Sprintf("Cannot read project %s (%s).", project, cluster.FactReadProblem(err))
	}
	return uid, 0, "", ""
}

// withProjectMutationAuth wraps the handler of a project action's route
// (projectActionRoutes): the gate above, then the operation as
// withMutationAuth runs it (the user's client, the ServiceAccount's
// bookkeeper and facts, the 15-minute deadline), holding the project's
// lock instead of the cluster's. The handler decodes the body again (it
// is kept) and calls beginProject before its first change.
func withProjectMutationAuth(route projectRoute, fn func(c *cluster.Client, w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		RecordRequest()
		username, r, ok := requestUser(w, r)
		if !ok {
			return
		}
		// 2. The body, before any lock.
		r.Body = http.MaxBytesReader(w, r.Body, route.maxBody)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, "invalid request body", http.StatusBadRequest, "validation")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		project, action, problem := route.parse(body)
		if problem == "" && !namespaceRegex.MatchString(project) {
			problem = "a valid project is required"
		}
		if problem != "" {
			writeError(w, problem, http.StatusBadRequest, "validation")
			return
		}
		saToken := getClusterToken()
		if saToken == "" {
			writeError(w, "cluster token not available", http.StatusInternalServerError)
			return
		}
		userToken := extractUserToken(r)
		// 3. The sign-in.
		if msg, code, status := sessionProblem(r.Context(), userToken); msg != "" {
			writeError(w, msg+" Nothing was changed.", status, code)
			return
		}
		// 4. The role.
		admin, err := mutationPermission(r.Context(), userToken)
		if err != nil {
			writeAuthError(w, err, "Cannot verify your permissions. Nothing was changed.")
			return
		}
		id := cluster.ProjectActionID(route.feature, action)
		pa, known := cluster.ProjectActionByID(id)
		label := pa.Label
		if !known {
			label = action
		}
		u := projectCheckClient(r.Context(), userToken, username, saToken)
		// 5. An action users may run; one a cluster-admin may not.
		if !admin && (!known || !pa.ForUsers) {
			writeError(w, adminOnlyActionMessage(label), http.StatusForbidden, adminOnlyCode)
			return
		}
		if admin && pa.AdminRefusal != "" {
			writeError(w, pa.AdminRefusal+" Nothing was changed.", http.StatusForbidden, notForClusterAdminsCode)
			return
		}
		// 6. One of their projects, before any ServiceAccount read.
		if !admin && !userProjectCheck(w, u, project, "Nothing was changed.") {
			return
		}
		// The rate limit before the reviews: a burst of requests does not
		// become a burst of access reviews.
		rateLimitKey := username + ":" + r.URL.Path
		slot, acquired := mutationLimiter.tryAcquire(rateLimitKey)
		if !acquired {
			wait := mutationLimiter.retryAfter(rateLimitKey)
			slog.Warn("rate limited", "path", r.URL.Path, "retryAfter", wait)
			w.Header().Set("Retry-After", strconv.Itoa(wait))
			writeError(w, fmt.Sprintf("This tool allows one change per user every %d seconds on this action. Nothing was changed; try again in %d %s.", int(mutationLimiter.window/time.Second), wait, map[bool]string{true: "second", false: "seconds"}[wait == 1]), http.StatusTooManyRequests, "user_rate_limited")
			return
		}
		// 7 and 8. The prerequisites (facts), then the preflight.
		if v := verdictOf(projectActionVerdicts(u, admin, []string{id}, project), id); v.reason != "" {
			mutationLimiter.release(rateLimitKey, slot)
			if v.code == missingPermissionsCode {
				slog.Info("project action refused: missing permissions", "action", id, "missing", len(v.missing))
				writeMissingPermissions(w, label, project, v.missing)
				return
			}
			writeError(w, v.reason+" Nothing was changed.", v.status, v.code)
			return
		}
		// The project's lock is keyed by its namespace UID.
		uid, status, code, reason := projectStartProblem(u, project, admin)
		if reason != "" {
			mutationLimiter.release(rateLimitKey, slot)
			writeError(w, reason+" Nothing was changed.", status, code)
			return
		}

		// 9. The shutdown gate and the lock, as withMutationAuth.
		if !mutations.begin() {
			mutationLimiter.release(rateLimitKey, slot)
			w.Header().Set("Retry-After", "30")
			writeError(w, "The updater is restarting. No changes were made; retry once it is back.", http.StatusServiceUnavailable, "shutting_down")
			return
		}
		defer mutations.end()
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

		opContext, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Minute)
		defer cancel()
		opContext, cancelOp := context.WithCancelCause(opContext)
		defer cancelOp(nil)
		// Its activity entries are scoped to the project, whoever runs it.
		opContext = cluster.WithActionScope(opContext, project, uid)
		// A cancellable action checks a cancel request at its safe points
		// (operation_cancel.go); opID is set once the action holds its lock,
		// before it runs.
		var opID string
		opContext = cluster.WithCancelCheck(opContext, func() *cluster.ActionCancelledError { return inflight.cancelRequest(opID) })
		changesContext, changes := cluster.TrackChanges(opContext)
		bookkeeper := cluster.NewClientWithContext(opContext, saToken)
		bookkeeper.SetUsername(username)
		client := cluster.NewUserClient(changesContext, userToken)
		client.SetUsername(username)
		client.SetBookkeeping(bookkeeper)
		client.SetFacts(bookkeeper)
		if !admin {
			client.SetProjectUser(uid)
		}
		sw := &statusWriter{ResponseWriter: w, path: r.URL.Path, user: username, bookkeeper: bookkeeper, cancelOp: cancelOp, admin: admin}
		if admin && route.adminClusterLock {
			if !lockCluster(sw) {
				mutationLimiter.release(rateLimitKey, slot)
				return
			}
			defer releaseClusterMutationLock()
			runProjectAction(fn, client, sw, r, changes, rateLimitKey, slot)
			return
		}
		refusal := acquireProjectLock(uid, username, func() *Operation {
			kind := operationTypes[sw.path]
			op := inflight.startProject(kind[0], label, username, project, uid, bookkeeper)
			inflight.update(op.ID, func(o *Operation) { o.sw, o.cancelNote = sw, cancellableActions[id] })
			sw.opID, opID = op.ID, op.ID
			return op
		}, cancelOp)
		if refusal != nil {
			mutationLimiter.release(rateLimitKey, slot)
			writeLockRefusal(sw, refusal)
			return
		}
		defer releaseProjectLock(uid)
		runProjectAction(fn, client, sw, r, changes, rateLimitKey, slot)
	}
}

// runProjectAction runs the handler of an accepted project action and gives
// the user's rate-limit slot back when it did not go through and sent
// nothing that may have changed the cluster (as withMutationAuth).
func runProjectAction(fn func(c *cluster.Client, w http.ResponseWriter, r *http.Request), client *cluster.Client, sw *statusWriter, r *http.Request, changes *cluster.Changes, rateLimitKey string, slot time.Time) {
	completed := false
	defer func() {
		wentThrough := completed && sw.succeeded()
		if sw.Header().Get("X-Skip-Rate-Limit") == "true" || !wentThrough && !changes.Made() {
			mutationLimiter.release(rateLimitKey, slot)
		}
	}()
	fn(client, sw, r)
	completed = true
}

// beginProject marks the request's project action as begun before its
// first change (beginProjectAction: its lock entry records Begun, so the
// action shows as interrupted should this process die). It returns false
// after answering 503 lock_unavailable when the entry could not be written
// (the lock may be lost): nothing was changed then.
func beginProject(w http.ResponseWriter, target string) bool {
	sw, ok := w.(*statusWriter)
	if !ok {
		return true
	}
	// A cluster-admin's action that holds the cluster lock
	// (adminClusterLock) begins as a cluster operation: its marker.
	if op := inflight.get(sw.opID); op != nil && op.ProjectUID == "" {
		beginOperation(w, target)
		return true
	}
	if err := beginProjectAction(sw.opID, target); err != nil {
		slog.Warn("project action not begun: its lock entry could not be written", "operation", sw.opID, "error", err)
		w.Header().Set("Retry-After", strconv.Itoa(int(lockUnavailableRetryAfter.Seconds())))
		writeError(w, lockUnavailableMessage, http.StatusServiceUnavailable, "lock_unavailable")
		return false
	}
	return true
}
