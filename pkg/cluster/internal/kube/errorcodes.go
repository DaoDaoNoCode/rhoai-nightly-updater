package kube

import (
	"errors"
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
