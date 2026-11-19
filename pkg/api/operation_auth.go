package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

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
//
// It evaluates the token owner's full RBAC, not the token's OAuth scopes
// (the token's own scope is checked by sessionProblem: the change is made
// with that token). The only token accepted in the cluster is the one
// oauth-proxy obtained through its own OAuth flow for this tool
// (X-Forwarded-Access-Token; see extractUserToken). A token issued to
// another OAuth client cannot be presented here. This relies on the
// template not enabling --openshift-delegate-urls or
// --pass-user-bearer-token on oauth-proxy, which would let client bearer
// tokens through; TestOAuthProxyForwardsOnlyItsOwnSessionToken pins that.
var checkPermission = cluster.CheckUserPermissionWithToken

// readSessionToken reads the user's own OAuth access token; tests replace it.
var readSessionToken = cluster.ReadSessionToken

// sessionClock is the time sessionProblem compares the token's expiry
// with; tests replace it.
var sessionClock = time.Now

// sessionMargin is the room left beyond the longest time an accepted
// change acts with the user's token.
const sessionMargin = 5 * time.Minute

// sessionNeeded is how long the user's token must keep working when a
// change is accepted: the operation's deadline, the restore that follows a
// failure (it runs with the same token), and a margin.
func sessionNeeded() time.Duration {
	return cluster.OperationDeadline + cluster.RecoveryTimeout + sessionMargin
}

// signInAgainCode is the errorCode of a refusal the user resolves by
// logging out and in again (before anything changed).
const signInAgainCode = "sign_in_again"

// sessionProblem says why the user's sign-in cannot carry a change, as the
// message, errorCode and HTTP status of the refusal, or "" when it can.
// The change is made with the user's token, so it reads that token's own
// object first:
//   - a 401 is an expired or revoked sign-in (session_expired);
//   - a token without the user:full scope, or one whose scopes do not even
//     let it read its own object (403), cannot act: it is a sign-in from
//     before 4.0.0, made through the ServiceAccount OAuth client with
//     user:info user:check-access (the session cookie survives the upgrade);
//   - a token that expires before sessionNeeded is refused, so an
//     operation is not cut off half-done with nothing to restore it.
//
// When the object cannot be read for another reason (not found, the API
// unreachable, not an OAuth token at all), the change goes ahead and the
// reason is logged: the API server still checks the token on every
// request, and a 401 mid-operation stops it with a message that says to
// sign in again. Only a refusal that is known to be needed is made.
// Never logs or returns the token.
func sessionProblem(ctx context.Context, token string) (string, string, int) {
	if devModeWithoutServiceAccount() {
		return "", "", 0
	}
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tok, err := readSessionToken(readCtx, token)
	switch {
	case errors.Is(err, cluster.ErrUnauthenticated):
		return "Your OpenShift session has expired. Log in again.", "session_expired", http.StatusUnauthorized
	case cluster.IsK8sError(err, http.StatusForbidden):
		return scopeRefusal, signInAgainCode, http.StatusUnauthorized
	case err != nil:
		slog.Warn("cannot read the user's sign-in token; the change goes ahead without the expiry check", "error", err)
		return "", "", 0
	case !tok.FullScope():
		return scopeRefusal, signInAgainCode, http.StatusUnauthorized
	}
	if left, expires := tok.Remaining(sessionClock()); expires && left < sessionNeeded() {
		mins := int(left / time.Minute)
		if mins < 0 {
			mins = 0
		}
		return fmt.Sprintf("Your sign-in expires in %d %s, and a change may take up to %d minutes. Log out and log in again, then retry.",
			mins, map[bool]string{true: "minute", false: "minutes"}[mins == 1], int(sessionNeeded()/time.Minute)), signInAgainCode, http.StatusUnauthorized
	}
	return "", "", 0
}

// scopeRefusal: the session's token can identify the user but not act.
const scopeRefusal = "Your sign-in cannot make changes: it was made before this updater acted with your own OpenShift permissions. Log out and log in again."

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
