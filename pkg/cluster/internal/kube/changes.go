package kube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
)

// Changes records whether an operation sent a request that may have changed
// something: pkg/api gives a change's client a context that carries one
// (TrackChanges) and, when the change did not go through, gives the user's
// rate-limit slot back only if Made is false. It errs towards "made": a
// request counts from the moment it is sent, and only an answer that proves
// it changed nothing takes it back.
type Changes struct {
	made    atomic.Bool
	pending atomic.Int64 // requests sent whose answer has not come yet
}

type changesKey struct{}

// TrackChanges returns a context whose requests (this package's Client and
// the other HTTP calls that pass through NoteRequest) are recorded in the
// returned Changes.
func TrackChanges(ctx context.Context) (context.Context, *Changes) {
	ch := &Changes{}
	return context.WithValue(ctx, changesKey{}, ch), ch
}

// Made reports whether a recorded request may have changed something: one
// that did, or one still waiting for its answer.
func (ch *Changes) Made() bool {
	if ch == nil {
		return false
	}
	// pending first: a request that ends between the two loads stored made
	// before it left pending (startAPIRequest), so it is seen either way.
	pending := ch.pending.Load()
	if afterPendingLoad != nil {
		afterPendingLoad()
	}
	return pending > 0 || ch.made.Load()
}

// afterPendingLoad, when set by a test, runs between Made's two loads.
var afterPendingLoad func()

// withoutChanges hides ctx's Changes from the requests made with the
// returned context: the updater's own records (Bookkeeping) are not changes
// the user made.
func withoutChanges(ctx context.Context) context.Context {
	if ctx == nil || changesOf(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, changesKey{}, (*Changes)(nil))
}

func changesOf(ctx context.Context) *Changes {
	if ctx == nil {
		return nil
	}
	ch, _ := ctx.Value(changesKey{}).(*Changes)
	return ch
}

// NoteRequest records req, sent with a context of TrackChanges, as a change
// unless its method only reads (GET, HEAD, OPTIONS). It is for HTTP calls to
// services other than the API server (S3, the dashboard's gen-ai API, MLflow,
// a model), whose answers are not read here: every write counts, whatever it
// was answered.
func NoteRequest(req *http.Request) {
	if ch := changesOf(req.Context()); ch != nil && !readOnlyMethod(req.Method) {
		ch.made.Store(true)
	}
}

// startAPIRequest records an API server request about to be sent with ctx
// and returns the function that takes its answer (the status, 0 when none
// came). It is not a change when its method only reads, it is a server-side
// dry run (IsDryRun), or it creates one of the review objects the API
// server answers without storing. Otherwise it is a change unless the API
// server refused it before it was applied (refusedBeforeApplied); a 5xx, or
// no answer at all, may have been applied.
func startAPIRequest(ctx context.Context, method string, u *url.URL, body []byte) func(status int) {
	ch := changesOf(ctx)
	if ch == nil || readOnlyMethod(method) || IsDryRun(method, u, body) || reviewPath(u.Path) {
		return func(int) {}
	}
	ch.pending.Add(1)
	return func(status int) {
		if !refusedBeforeApplied(method, u.Path, status) {
			ch.made.Store(true)
		}
		ch.pending.Add(-1)
	}
}

// deleteOptionQueryKeys are the DeleteOptions a DELETE may also carry in its
// query (metav1.DeleteOptions' query parameters). The ListOptions a DELETE
// of a collection takes in its query (labelSelector, fieldSelector, ...)
// are not among them.
var deleteOptionQueryKeys = []string{"dryRun", "propagationPolicy", "gracePeriodSeconds", "orphanDependents", "ignoreStoreReadErrorWithClusterBreakingPotential"}

// deleteOptionInQuery returns the DeleteOptions key in the query of a
// DELETE with a body, "" when there is none: the API server reads only the
// body's options then (IsDryRun), so that option would be ignored.
func deleteOptionInQuery(method string, u *url.URL, body []byte) string {
	if method != http.MethodDelete || len(body) == 0 {
		return ""
	}
	q := u.Query()
	for _, key := range deleteOptionQueryKeys {
		if q.Has(key) {
			return key
		}
	}
	return ""
}

// UnsentRequestError is a request this updater refused to send, as the API
// server would not do what it says. Nothing was sent, so nothing changed.
type UnsentRequestError struct{ Reason string }

func (e *UnsentRequestError) Error() string {
	return "this updater did not send the request (" + e.Reason + "); nothing was changed. This is a bug in the updater"
}

// RefusedBy names who refused a request that failed with err: this
// updater, which did not send it (UnsentRequestError), or the API server.
func RefusedBy(err error) string {
	if _, ok := errors.AsType[*UnsentRequestError](err); ok {
		return "This updater, before sending it,"
	}
	return "The API server"
}

// IsDryRun reports whether the API server runs the request as a
// server-side dry run, reading its options where the API server does. A
// DELETE with a body takes all its DeleteOptions, dryRun included, from the
// body and ignores the query: k8s.io/apiserver
// pkg/endpoints/handlers/delete.go decodes the query only when the body is
// empty, in DeleteResource at :95-128 and DeleteCollection at :272-302
// (v0.35 to v0.37; :95-126 and :270-300 in v0.32 and v0.33).
// Every other request takes dryRun from the query (create,
// update and patch options are query parameters).
func IsDryRun(method string, u *url.URL, body []byte) bool {
	if method == http.MethodDelete && len(body) > 0 {
		var opts struct {
			DryRun []string `json:"dryRun"`
		}
		return json.Unmarshal(body, &opts) == nil && slices.Contains(opts.DryRun, "All")
	}
	return u.Query().Get("dryRun") == "All"
}

// refusedBeforeApplied reports whether a write the API server answered
// with status changed nothing. Only the answers of a refusal before
// storage count: 400 (undecodable), 401 and 403 (authentication,
// authorization, an admission denial), 404, 405, 409 (a conflict, an
// existing name or a failed precondition), 413, 415, 422 (validation, a
// failed json-patch test) and 429 (throttled before the handler). A
// request on one object is applied in one storage write, so such an answer
// means it was not. A DELETE of a collection is not: the registry deletes
// its items one by one, each through admission, and returns the first
// error after others were deleted (k8s.io/apiserver v0.35.8,
// pkg/registry/generic/registry/store.go:1237-1360, DeleteCollection), so
// only 401 and 429, which refuse the whole request before the handler,
// count for it. Any other status (a 5xx, a timeout, none) may have been
// applied.
func refusedBeforeApplied(method, path string, status int) bool {
	switch status {
	case 401, 429:
		return true
	case 400, 403, 404, 405, 409, 413, 415, 422:
		return method != http.MethodDelete || !collectionPath(path)
	}
	return false
}

// collectionPath reports whether path names a collection of resources
// rather than one object (or one object's subresource): what follows
// /api/v1 or /apis/<group>/<version>, and namespaces/<name> when it is
// namespaced, is a single segment. A path of another shape counts as a
// collection.
func collectionPath(path string) bool {
	var rest string
	switch {
	case strings.HasPrefix(path, "/api/v1/"):
		rest = strings.TrimPrefix(path, "/api/v1/")
	case strings.HasPrefix(path, "/apis/"):
		parts := strings.SplitN(strings.TrimPrefix(path, "/apis/"), "/", 3)
		if len(parts) < 3 {
			return true
		}
		rest = parts[2]
	default:
		return true
	}
	segs := strings.Split(strings.Trim(rest, "/"), "/")
	if len(segs) >= 3 && segs[0] == "namespaces" {
		segs = segs[2:]
	}
	return len(segs) < 2
}

func readOnlyMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// reviewResources are the resources whose create the API server answers
// without storing anything, by the group that serves them.
var reviewResources = map[string][]string{
	"/apis/authorization.k8s.io/":       {"subjectaccessreviews", "selfsubjectaccessreviews", "localsubjectaccessreviews", "selfsubjectrulesreviews"},
	"/apis/authorization.openshift.io/": {"subjectaccessreviews", "localsubjectaccessreviews", "selfsubjectrulesreviews", "subjectrulesreviews", "resourceaccessreviews", "localresourceaccessreviews"},
	"/apis/authentication.k8s.io/":      {"tokenreviews", "selfsubjectreviews"},
}

func reviewPath(path string) bool {
	last := path[strings.LastIndex(path, "/")+1:]
	for group, resources := range reviewResources {
		if strings.HasPrefix(path, group) && slices.Contains(resources, last) {
			return true
		}
	}
	return false
}
