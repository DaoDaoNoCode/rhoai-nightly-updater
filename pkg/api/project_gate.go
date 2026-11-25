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
	"/api/resources/samples/mlflow/stream": {feature: "mlflow-samples", maxBody: 4096, parse: func(body []byte) (string, string, string) {
		var req types.MLflowSamplesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return "", "", "invalid request body"
		}
		if !namespaceRegex.MatchString(req.Project) {
			return "", "", "a valid project is required"
		}
		return req.Project, req.Action, cluster.ValidateMLflowSamplesRequest(req)
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

// writeMissingPermissions answers 403 missing_permissions with the list.
func writeMissingPermissions(w http.ResponseWriter, label, project string, missing []cluster.MissingPermission) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	texts := make([]string, 0, len(missing))
	for _, m := range missing {
		texts = append(texts, m.Text)
	}
	body := map[string]any{"error": cluster.MissingSummary(label, project, missing), "errorCode": missingPermissionsCode, "missing": texts}
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

// preflight is step 7 for the actions ids: what each misses, with u's
// token. apps is read only when an action needs it.
func preflight(u *cluster.Client, ids []string, project string) (map[string][]cluster.MissingPermission, error) {
	apps := ""
	if cluster.NeedsAppsNamespace(ids) {
		ns, err := projectAppsNamespace(u)
		if err != nil {
			return nil, err
		}
		apps = ns
	}
	return reviewProjectActions(u, ids, project, apps)
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
		if !admin {
			// 5. An action users may run.
			if !known || !pa.ForUsers {
				writeError(w, adminOnlyActionMessage(label), http.StatusForbidden, adminOnlyCode)
				return
			}
			// 6. One of their projects, before any ServiceAccount read.
			if !userProjectCheck(w, u, project, "Nothing was changed.") {
				return
			}
			// 7. The preflight.
			missing, err := preflight(u, []string{id}, project)
			if errors.Is(err, cluster.ErrUnauthenticated) {
				writeError(w, "Your OpenShift session has expired. Log in again.", http.StatusUnauthorized, "session_expired")
				return
			}
			if err != nil {
				slog.Warn("project action preflight failed", "action", id, "project", project, "user", username, "error", err)
				writeError(w, fmt.Sprintf("Cannot check your permissions for %s in %s (%v). Nothing was changed; try again.", label, project, err), http.StatusServiceUnavailable, "authorization_unavailable")
				return
			}
			if m := missing[id]; len(m) > 0 {
				slog.Info("project action refused: missing permissions", "action", id, "project", project, "user", username, "missing", len(m))
				writeMissingPermissions(w, label, project, m)
				return
			}
		}
		// 8. The cluster's prerequisites (the ServiceAccount's facts).
		if reason, code := projectFeatureBlock(u, route.feature); reason != "" {
			writeError(w, reason+" Nothing was changed.", http.StatusConflict, code)
			return
		}
		// The project's lock is keyed by its namespace UID.
		uid, err := readProjectUID(u, project)
		if err != nil {
			status, code := cluster.HTTPStatusForError(err)
			if !admin {
				// A project that went away since step 6: the same
				// answer as one that is not the user's.
				status, code = http.StatusForbidden, cluster.ErrorCodeForbiddenForUser
				err = errors.New(notYourProjectMessage(project))
			}
			writeError(w, fmt.Sprintf("Cannot read project %s: %v. Nothing was changed.", project, err), status, code)
			return
		}

		// 9. The rate limit and the shutdown gate, as withMutationAuth.
		rateLimitKey := username + ":" + r.URL.Path
		slot, acquired := mutationLimiter.tryAcquire(rateLimitKey)
		if !acquired {
			wait := mutationLimiter.retryAfter(rateLimitKey)
			slog.Warn("rate limited", "user", username, "path", r.URL.Path, "retryAfter", wait)
			w.Header().Set("Retry-After", strconv.Itoa(wait))
			writeError(w, fmt.Sprintf("This tool allows one change per user every %d seconds on this action. Nothing was changed; try again in %d %s.", int(mutationLimiter.window/time.Second), wait, map[bool]string{true: "second", false: "seconds"}[wait == 1]), http.StatusTooManyRequests, "user_rate_limited")
			return
		}
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
		changesContext, changes := cluster.TrackChanges(opContext)
		bookkeeper := cluster.NewClientWithContext(opContext, saToken)
		bookkeeper.SetUsername(username)
		client := cluster.NewUserClient(changesContext, userToken)
		client.SetUsername(username)
		client.SetBookkeeping(bookkeeper)
		client.SetFacts(bookkeeper)
		if !admin {
			client.SetProjectUser()
		}
		sw := &statusWriter{ResponseWriter: w, path: r.URL.Path, user: username, bookkeeper: bookkeeper, cancelOp: cancelOp, admin: admin}
		refusal := acquireProjectLock(uid, username, func() *Operation {
			kind := operationTypes[sw.path]
			op := inflight.startProject(kind[0], label, username, project, uid, bookkeeper)
			inflight.update(op.ID, func(o *Operation) { o.sw = sw })
			sw.opID = op.ID
			return op
		}, cancelOp)
		if refusal != nil {
			mutationLimiter.release(rateLimitKey, slot)
			writeLockRefusal(sw, refusal)
			return
		}
		defer releaseProjectLock(uid)
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
	if err := beginProjectAction(sw.opID, target); err != nil {
		slog.Warn("project action not begun: its lock entry could not be written", "operation", sw.opID, "error", err)
		w.Header().Set("Retry-After", strconv.Itoa(int(lockUnavailableRetryAfter.Seconds())))
		writeError(w, lockUnavailableMessage, http.StatusServiceUnavailable, "lock_unavailable")
		return false
	}
	return true
}
