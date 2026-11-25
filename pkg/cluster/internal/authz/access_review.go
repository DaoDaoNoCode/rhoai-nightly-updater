package authz

import (
	"encoding/json"
	"fmt"
	"strings"
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
// sends at once.
const AccessReviewConcurrency = 8

// ReviewUserAccess asks the API server, with the signed-in user's own token
// (u, NewUserClient), whether that user's RBAC allows each request: one
// SelfSubjectAccessReview per request, at most AccessReviewConcurrency at
// once. Every authenticated user may create one (Kubernetes
// system:basic-user, OpenShift basic-user, both bound to
// system:authenticated). It returns the requests that are not allowed, in
// the order of reqs.
//
// It fails closed: a review that cannot be sent, is refused, or comes back
// without a decision (no "allowed", or an evaluationError) makes the call
// fail, and no answer is returned for any request. A 401 is
// ErrUnauthenticated.
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
			allowed[i], errs[i] = reviewOne(u, reqs[i])
		})
	}
	wg.Wait()
	var failed []string
	for i, err := range errs {
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", reqs[i], err))
		}
	}
	if len(failed) > 0 {
		for _, err := range errs {
			if err != nil && isUnauthenticated(err) {
				return nil, fmt.Errorf("access review: %w", ErrUnauthenticated)
			}
		}
		return nil, fmt.Errorf("cannot check your permissions (%d of %d access reviews failed): %s", len(failed), len(reqs), strings.Join(failed, "; "))
	}
	var denied []AccessRequest
	for i, ok := range allowed {
		if !ok {
			denied = append(denied, reqs[i])
		}
	}
	return denied, nil
}

func isUnauthenticated(err error) bool {
	return kube.IsK8sError(err, 401)
}

// reviewOne sends one SelfSubjectAccessReview with u's token.
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
		return false, fmt.Errorf("parse the access review: %w", err)
	}
	if review.Status.Allowed == nil || review.Status.EvaluationError != "" {
		return false, fmt.Errorf("the access review came back without a decision (%s)", kube.FirstNonEmpty(review.Status.EvaluationError, "no allowed field"))
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
