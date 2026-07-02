package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// SelfSubjectAccessReview types for the authorization.k8s.io/v1 API.
type selfSubjectAccessReview struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		ResourceAttributes *resourceAttributes `json:"resourceAttributes"`
	} `json:"spec"`
}

type resourceAttributes struct {
	Verb      string `json:"verb"`
	Resource  string `json:"resource"`
	Group     string `json:"group"`
	Namespace string `json:"namespace"`
}

type ssarResponse struct {
	Status struct {
		Allowed bool   `json:"allowed"`
		Reason  string `json:"reason,omitempty"`
	} `json:"status"`
}

// CheckUserPermissionWithToken performs a SelfSubjectAccessReview using the
// user's own OAuth token. This correctly resolves ALL permissions including
// group-based RBAC, IDP-granted roles, and implicit cluster-admin grants.
func CheckUserPermissionWithToken(ctx context.Context, userToken, verb, resource, apiGroup, namespace string) (bool, error) {
	userClient := NewClientWithContext(ctx, userToken)

	ssar := selfSubjectAccessReview{
		APIVersion: "authorization.k8s.io/v1",
		Kind:       "SelfSubjectAccessReview",
	}
	ssar.Spec.ResourceAttributes = &resourceAttributes{
		Verb:      verb,
		Resource:  resource,
		Group:     apiGroup,
		Namespace: namespace,
	}

	data, err := json.Marshal(ssar)
	if err != nil {
		return false, fmt.Errorf("marshal SSAR: %w", err)
	}

	body, statusCode, err := userClient.post("/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", data)
	if err != nil {
		return false, fmt.Errorf("SSAR request failed (status %d): %w", statusCode, err)
	}

	var resp ssarResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return false, fmt.Errorf("parse SSAR response: %w", err)
	}

	slog.Info("permission check", "verb", verb, "resource", resource, "namespace", namespace, "allowed", resp.Status.Allowed)
	return resp.Status.Allowed, nil
}
