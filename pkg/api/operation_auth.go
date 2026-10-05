package api

import (
	"context"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// mutationRequirement is the permission every mutation endpoint, and the
// read-only UI switch, require: any verb on any resource in all namespaces.
//
// The ServiceAccount acts cluster-wide on the user's behalf: it installs an
// operator (OLM grants whatever the CSV asks for), writes the node pull
// secret in kube-system, deletes admission webhooks and patches CRDs. Adding
// operators is a cluster-admin task in OpenShift
// (https://docs.okd.io/latest/operators/admin/olm-adding-operators-to-cluster.html),
// so the gate asks for the cluster-admin role's own rule
// (apiGroups/resources/verbs "*", cluster-wide). RBAC matches a "*" request
// only against rules that themselves hold "*" (VerbMatches and friends in
// k8s.io/kubernetes/pkg/apis/rbac/v1/evaluation_helpers.go compare the rule's
// "*" or the exact requested value), so a namespace edit/admin binding, which grants "update subscriptions" in
// that namespace, no longer unlocks the tool. Checked on the live cluster:
// `oc auth can-i '*' '*' --all-namespaces` is "no" for a namespace admin and
// for dedicated-admins, and "yes" for members of cluster-admins.
var mutationRequirement = struct{ verb, resource, group, namespace string }{"*", "*", "*", ""}

// readOnlyMessage explains a 403 from the mutation gate.
const readOnlyMessage = "Read-only access: changing the cluster through this tool requires the cluster-admin role."

// checkPermission asks the API server; tests replace it.
var checkPermission = cluster.CheckUserPermissionWithToken

// mutationPermission evaluates the gate for each request with the user's
// own token.
var mutationPermission = func(ctx context.Context, token string) (bool, error) {
	// Explicit local development bypass; never bypass with a mounted SA token.
	if devModeWithoutServiceAccount() {
		return true, nil
	}
	return checkPermission(ctx, token,
		mutationRequirement.verb, mutationRequirement.resource, mutationRequirement.group, mutationRequirement.namespace)
}

func sendOperationResult(s *SSEWriter, result *types.OperationResponse, err error) {
	step := UpdateStep{Step: "operation_complete", Status: "failed", Message: "Operation failed"}
	if result != nil {
		step.Message = result.Message
		step.ErrorCode = result.ErrorCode
		if result.Success && err == nil {
			step.Status = "success"
		}
	}
	if err != nil {
		step.Message = err.Error()
	}
	_ = s.SendStep(step)
}
