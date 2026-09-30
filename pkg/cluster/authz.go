package cluster

import (
	"context"
	"encoding/json"
	"fmt"
)

// CheckUserPermissionWithToken checks the token owner's full RBAC permissions.
// OpenShift's user:check-access OAuth scope permits a self SAR. An explicitly
// empty scopes array asks about full RBAC, rather than the OAuth token's limited
// scopes, as oauth-proxy does. Identity and groups come from the token itself.
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
		return false, fmt.Errorf("permission review failed: %w", err)
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
