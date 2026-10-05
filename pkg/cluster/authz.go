package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrUnauthenticated means the API server rejected the user's own token
// (HTTP 401): the OAuth session has expired or was revoked, so the user has
// to log in again. It is distinct from the API being unreachable.
var ErrUnauthenticated = errors.New("the OpenShift session token was rejected")

// wrapUserTokenError marks a 401 from a request made with the user's token.
func wrapUserTokenError(op string, err error) error {
	if IsK8sError(err, 401) {
		return fmt.Errorf("%s: %w: %w", op, ErrUnauthenticated, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// CheckUserPermissionWithToken checks the token owner's full RBAC permissions.
// OpenShift's user:check-access OAuth scope permits a self SAR. An explicitly
// empty scopes array asks about full RBAC, rather than the OAuth token's limited
// scopes, as oauth-proxy does. Identity and groups come from the token itself.
// An empty namespace asks about the permission in all namespaces.
func CheckUserPermissionWithToken(ctx context.Context, userToken, verb, resource, apiGroup, namespace string) (bool, error) {
	userClient := NewClientWithContext(ctx, userToken)
	data, err := json.Marshal(map[string]interface{}{
		"apiVersion": "authorization.openshift.io/v1", "kind": "SubjectAccessReview",
		"verb": verb, "resource": resource, "resourceAPIGroup": apiGroup,
		"namespace": namespace, "scopes": []string{},
	})
	if err != nil {
		return false, fmt.Errorf("marshal permission review: %w", err)
	}
	body, _, err := userClient.post("/apis/authorization.openshift.io/v1/subjectaccessreviews", data)
	if err != nil {
		return false, wrapUserTokenError("permission review failed", err)
	}
	var response struct {
		Allowed         *bool  `json:"allowed"`
		EvaluationError string `json:"evaluationError"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return false, fmt.Errorf("parse permission review: %w", err)
	}
	if response.Allowed == nil || response.EvaluationError != "" {
		return false, fmt.Errorf("permission review incomplete: %s", response.EvaluationError)
	}
	return *response.Allowed, nil
}

// LookupUserWithToken returns the name of the user who owns the token, as
// the API server authenticates it. Reading users/~ needs only the
// user:info OAuth scope, which oauth-proxy requests by default, and is the
// same call oauth-proxy's OpenShift provider uses to identify the user.
func LookupUserWithToken(ctx context.Context, userToken string) (string, error) {
	userClient := NewClientWithContext(ctx, userToken)
	body, _, err := userClient.get("/apis/user.openshift.io/v1/users/~")
	if err != nil {
		return "", wrapUserTokenError("identify user", err)
	}
	var user struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &user); err != nil {
		return "", fmt.Errorf("parse user: %w", err)
	}
	if user.Metadata.Name == "" {
		return "", fmt.Errorf("identify user: the API returned no user name")
	}
	return user.Metadata.Name, nil
}
