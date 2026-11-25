package authz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// AccessRequest is one request a project action sends, as Kubernetes RBAC
// decides it: the resourceAttributes of a SelfSubjectAccessReview
// (authorization.k8s.io/v1). An empty Name asks about every name (RBAC then
// allows it only through a rule without resourceNames).
type AccessRequest struct {
	Verb        string `json:"verb"`
	Group       string `json:"group"`
	Resource    string `json:"resource"`
	Subresource string `json:"subresource,omitempty"`
	Namespace   string `json:"namespace"`
	Name        string `json:"name,omitempty"`
}

// String says the request for people: "create persistentvolumeclaims in
// project p", with the API group and the name when there are.
func (r AccessRequest) String() string {
	res := r.Resource
	if r.Subresource != "" {
		res += "/" + r.Subresource
	}
	if r.Group != "" {
		res += "." + r.Group
	}
	if r.Name != "" {
		res += " " + r.Name
	}
	out := r.Verb + " " + res
	if r.Namespace != "" {
		out += " in namespace " + r.Namespace
	}
	return out
}

// AccessReviewConcurrency bounds the reviews one call of ReviewUserAccess
// sends at once; maxReviewsInFlight bounds those of every call of this
// process together, so concurrent preflights and capability reads cannot
// multiply the load on the API server.
const (
	AccessReviewConcurrency = 8
	maxReviewsInFlight      = 32
)

var reviewsInFlight = make(chan struct{}, maxReviewsInFlight)

// waitingForReviewSlot runs, with the call's context, when a review is
// about to wait for a process slot; tests replace it (before any call) to
// learn that a review waits.
var waitingForReviewSlot = func(context.Context) {}

// ErrAccessReviewFailed: an access review could not be made or came back
// without a decision. Its text names only how it failed (an HTTP status or
// a category), never the API server's or the authorizer's words.
var ErrAccessReviewFailed = errors.New("an access review with your token could not be made")

// ReviewUserAccess asks the API server, with the signed-in user's own token
// (u, NewUserClient), whether that user's RBAC allows each request: one
// SelfSubjectAccessReview per request, at most AccessReviewConcurrency at
// once (and maxReviewsInFlight in the process). Every authenticated user
// may create one (Kubernetes system:basic-user, OpenShift basic-user, both
// bound to system:authenticated). It returns the requests that are not
// allowed, in the order of reqs.
//
// It fails closed: a review that cannot be sent, is refused, or comes back
// without a decision makes the call fail (ErrAccessReviewFailed), and no
// answer is returned for any request. A 401 is ErrUnauthenticated. The
// failures are logged with their category only.
func ReviewUserAccess(u *kube.Client, reqs []AccessRequest) ([]AccessRequest, error) {
	if !u.ActsAsUser() {
		return nil, ErrNotUserClient
	}
	allowed := make([]bool, len(reqs))
	errs := make([]error, len(reqs))
	slots := make(chan struct{}, AccessReviewConcurrency)
	var wg sync.WaitGroup
	for i := range reqs {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			waitingForReviewSlot(u.Context())
			select {
			case reviewsInFlight <- struct{}{}:
			case <-u.Context().Done():
				errs[i] = u.Context().Err()
				return
			}
			defer func() { <-reviewsInFlight }()
			allowed[i], errs[i] = reviewOne(u, reqs[i])
		})
	}
	wg.Wait()
	failed := 0
	var first error
	for _, err := range errs {
		if err == nil {
			continue
		}
		if isUnauthenticated(err) {
			return nil, fmt.Errorf("access review: %w", ErrUnauthenticated)
		}
		failed++
		if first == nil {
			first = err
		}
	}
	if failed > 0 {
		kube.LogFactReadFailure("access reviews with the user's token failed", "SelfSubjectAccessReview", first, "failed", failed, "of", len(reqs))
		return nil, fmt.Errorf("%w (%d of %d failed: %s)", ErrAccessReviewFailed, failed, len(reqs), reviewProblem(first))
	}
	var denied []AccessRequest
	for i, ok := range allowed {
		if !ok {
			denied = append(denied, reqs[i])
		}
	}
	return denied, nil
}

// errNoDecision: a review came back without "allowed".
var errNoDecision = errors.New("no decision")

// reviewProblem says how a review failed, without its text.
func reviewProblem(err error) string {
	if errors.Is(err, errNoDecision) {
		return "no decision"
	}
	return kube.FactReadProblem(err)
}

func isUnauthenticated(err error) bool {
	return kube.IsK8sError(err, 401)
}

// reviewOne sends one SelfSubjectAccessReview with u's token. Its
// decision is status.allowed: RBAC allows a request when one rule allows
// it, so an evaluationError beside allowed true (a rule that could not be
// read) does not change it, and beside allowed false the request is denied
// (fail closed). The evaluationError is logged, never returned: it may
// name roles and bindings.
func reviewOne(u *kube.Client, r AccessRequest) (bool, error) {
	attrs := map[string]any{"verb": r.Verb, "group": r.Group, "resource": r.Resource, "namespace": r.Namespace}
	if r.Subresource != "" {
		attrs["subresource"] = r.Subresource
	}
	if r.Name != "" {
		attrs["name"] = r.Name
	}
	data, err := json.Marshal(map[string]any{
		"apiVersion": "authorization.k8s.io/v1", "kind": "SelfSubjectAccessReview",
		"spec": map[string]any{"resourceAttributes": attrs},
	})
	if err != nil {
		return false, err
	}
	body, _, err := u.Post("/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", data)
	if err != nil {
		return false, err
	}
	var review struct {
		Status struct {
			Allowed         *bool  `json:"allowed"`
			EvaluationError string `json:"evaluationError"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &review); err != nil {
		return false, errNoDecision
	}
	if review.Status.Allowed == nil {
		return false, errNoDecision
	}
	if review.Status.EvaluationError != "" {
		slog.Debug("an access review came back with an evaluation error", "verb", r.Verb, "resource", r.Resource, "allowed", *review.Status.Allowed, "evaluationError", review.Status.EvaluationError)
	}
	return *review.Status.Allowed, nil
}

// ProjectUID reads the UID of project's namespace with the signed-in
// user's own token: the key of the project's lock. A user may get their
// own project's namespace (Kubernetes view, edit and admin grant get on
// "namespaces" for the namespace they are bound in).
func ProjectUID(u *kube.Client, project string) (string, error) {
	if !u.ActsAsUser() {
		return "", ErrNotUserClient
	}
	var ns struct {
		Metadata kube.ObjectMeta `json:"metadata"`
	}
	if err := u.GetInto("/api/v1/namespaces/"+project, &ns); err != nil {
		return "", wrapUserTokenError("read project "+project, err)
	}
	if ns.Metadata.UID == "" {
		return "", fmt.Errorf("read project %s: the API returned no UID", project)
	}
	return ns.Metadata.UID, nil
}
