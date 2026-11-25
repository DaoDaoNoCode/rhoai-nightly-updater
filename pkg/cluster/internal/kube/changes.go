package kube

import (
	"context"
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
	return ch != nil && (ch.made.Load() || ch.pending.Load() > 0)
}

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
// dry run (dryRun=All: nothing is persisted), or it creates one of the review
// objects the API server answers without storing. Otherwise it is a change
// unless the API server answered 4xx: such a request was refused before it
// was applied, as retryWait also assumes for a 429. A 5xx, or no answer at
// all, may have been applied.
func startAPIRequest(ctx context.Context, method string, u *url.URL) func(status int) {
	ch := changesOf(ctx)
	if ch == nil || readOnlyMethod(method) || u.Query().Get("dryRun") == "All" || reviewPath(u.Path) {
		return func(int) {}
	}
	ch.pending.Add(1)
	return func(status int) {
		if status < 400 || status >= 500 {
			ch.made.Store(true)
		}
		ch.pending.Add(-1)
	}
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
