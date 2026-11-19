package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
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

// SessionToken is what the OAuth server recorded about a user's own access
// token (oauth.openshift.io/v1 UserOAuthAccessToken).
type SessionToken struct {
	Created time.Time
	// ExpiresIn: seconds after Created the token stops working; 0 never.
	ExpiresIn int64
	// InactivityTimeoutSeconds: the token stops working this long after
	// Created unless it is used; every use moves it on.
	InactivityTimeoutSeconds int64
	Scopes                   []string
	ClientName               string
}

// FullScope reports whether the token may act with the user's whole RBAC
// (scope user:full). A token of the scopes user:info and user:check-access,
// which a ServiceAccount OAuth client gets, can only identify the user.
func (s *SessionToken) FullScope() bool {
	for _, sc := range s.Scopes {
		if sc == "user:full" {
			return true
		}
	}
	return false
}

// Remaining is how long the token keeps working by its expiry at now, and
// false when it does not expire.
func (s *SessionToken) Remaining(now time.Time) (time.Duration, bool) {
	if s.ExpiresIn <= 0 {
		return 0, false
	}
	return s.Created.Add(time.Duration(s.ExpiresIn) * time.Second).Sub(now), true
}

// ErrNotAnOAuthToken: the token has no "sha256~" prefix, so it is not an
// OAuth access token of the OpenShift OAuth server and has no object to
// read (a ServiceAccount token, or a developer's token in dev mode).
var ErrNotAnOAuthToken = errors.New("not an OpenShift OAuth access token")

const sha256TokenPrefix = "sha256~"

// accessTokenObjectName is the name of the object of an OAuth access
// token: "sha256~" and the unpadded base64url SHA-256 of the token without
// its prefix, as the API server looks the token up
// (openshift/oauth-apiserver pkg/tokenvalidation/tokenauthenticator.go).
func accessTokenObjectName(token string) (string, error) {
	raw, ok := strings.CutPrefix(token, sha256TokenPrefix)
	if !ok || raw == "" {
		return "", ErrNotAnOAuthToken
	}
	sum := sha256.Sum256([]byte(raw))
	return sha256TokenPrefix + base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// ReadSessionToken reads the caller's own OAuth access token object with
// that token. Every user signed in through OAuth may read their own
// (the system:openshift:useroauthaccesstoken-manager ClusterRole, bound to
// system:authenticated:oauth), but only with a token whose scopes allow it:
// a user:info user:check-access token gets 403. A 401 is ErrUnauthenticated.
// The token itself never appears in an error: only its object's name,
// which is a hash.
func ReadSessionToken(ctx context.Context, userToken string) (*SessionToken, error) {
	name, err := accessTokenObjectName(userToken)
	if err != nil {
		return nil, err
	}
	userClient := NewUserClient(ctx, userToken)
	body, _, err := userClient.get("/apis/oauth.openshift.io/v1/useroauthaccesstokens/" + name)
	if err != nil {
		return nil, wrapUserTokenError("read the sign-in's token", err)
	}
	var obj struct {
		Metadata struct {
			CreationTimestamp time.Time `json:"creationTimestamp"`
		} `json:"metadata"`
		ExpiresIn                int64    `json:"expiresIn"`
		InactivityTimeoutSeconds int64    `json:"inactivityTimeoutSeconds"`
		Scopes                   []string `json:"scopes"`
		ClientName               string   `json:"clientName"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("parse the sign-in's token: %w", err)
	}
	return &SessionToken{
		Created: obj.Metadata.CreationTimestamp, ExpiresIn: obj.ExpiresIn,
		InactivityTimeoutSeconds: obj.InactivityTimeoutSeconds, Scopes: obj.Scopes, ClientName: obj.ClientName,
	}, nil
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
