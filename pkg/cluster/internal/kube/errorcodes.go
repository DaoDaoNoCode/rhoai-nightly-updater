package kube

import (
	"errors"
	"net/http"
)

// ErrorCodeGitOpsManaged marks a refusal because Argo CD manages the object
// the action would change, and would undo the change.
const ErrorCodeGitOpsManaged = "gitops_managed"

// ErrorCodeProfileGitOps marks a request the gitops profile refuses
// (docs/GITOPS.md, "Profiles"): the route is off in that profile, or Update
// would have to take a path that profile does not have.
const ErrorCodeProfileGitOps = "profile_gitops"

// ErrorCodeFromK8sErr maps a K8s API error to a user-facing error code string.
func ErrorCodeFromK8sErr(err error) string {
	if IsNetworkError(err) {
		return "network"
	}
	var k8sErr *K8sError
	if errors.As(err, &k8sErr) && k8sErr.Status == 401 && k8sErr.User {
		// The signed-in user's token, which a change acts with, was
		// rejected while the operation ran: their sign-in ended (expired or
		// revoked), and part of the change may have run. Not the app's
		// credentials ("unauthorized"); a sign-in that ends before anything
		// runs is refused by the gate with "session_expired".
		return "sign_in_ended"
	}
	if IsK8sError(err, 401) {
		return "unauthorized"
	}
	if IsK8sError(err, 403) {
		return "forbidden"
	}
	return ""
}

func ErrorCodeOr(err error, fallback string) string {
	if code := ErrorCodeFromK8sErr(err); code != "" {
		return code
	}
	return fallback
}

// TemplateHint explains a 403 the API server answered to the updater's own
// ServiceAccount (a read, or the app's records), which after an upgrade
// means its RBAC still comes from an older deploy/template.yaml. A 403 to
// the signed-in user's token (every change) is about that user's RBAC and
// says so itself (K8sError.Error, UserForbiddenMessage), so it gets no
// hint.
func TemplateHint(err error) string {
	var ke *K8sError
	if !errors.As(err, &ke) || ke.Status != http.StatusForbidden || ke.User {
		return ""
	}
	return " The updater's ServiceAccount is not allowed to do this. If the updater was upgraded recently, its permissions still come from an older deploy/template.yaml: an admin should run `make upgrade` to re-apply it, then retry."
}
