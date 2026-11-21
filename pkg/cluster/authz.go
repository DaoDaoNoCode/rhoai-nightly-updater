package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// ErrUnauthenticated means the API server rejected the user's own token
// (HTTP 401): the OAuth session has expired or was revoked, so the user has
// to log in again. It is distinct from the API being unreachable.
var ErrUnauthenticated = errors.New("the OpenShift session token was rejected")

// wrapUserTokenError marks a 401 from a request made with the user's token.
func wrapUserTokenError(op string, err error) error {
	if kube.IsK8sError(err, 401) {
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
	userClient := kube.NewClientWithContext(ctx, userToken)
	data, err := json.Marshal(map[string]any{
		"apiVersion": "authorization.openshift.io/v1", "kind": "SubjectAccessReview",
		"verb": verb, "resource": resource, "resourceAPIGroup": apiGroup,
		"namespace": namespace, "scopes": []string{},
	})
	if err != nil {
		return false, fmt.Errorf("marshal permission review: %w", err)
	}
	body, _, err := userClient.Post("/apis/authorization.openshift.io/v1/subjectaccessreviews", data)
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
	Scopes    []string
}

// FullScope reports whether the token may act with the user's whole RBAC
// (scope user:full). A token of the scopes user:info and user:check-access,
// which a ServiceAccount OAuth client gets, can only identify the user.
func (s *SessionToken) FullScope() bool {
	return slices.Contains(s.Scopes, "user:full")
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

// ErrSessionTokenGone: the API server answered that the caller's own access
// token object does not exist. A token that oauth-proxy got from the OAuth
// server always has one, so it was revoked (oc delete oauthaccesstokens) or
// removed with its user or OAuthClient; the API server may still accept the
// token for a while from its cache (live: about a minute).
var ErrSessionTokenGone = errors.New("the sign-in's access token no longer exists")

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
	userClient := kube.NewUserClient(ctx, userToken)
	body, _, err := userClient.Get("/apis/oauth.openshift.io/v1/useroauthaccesstokens/" + name)
	// Only a 404 about this very object says the sign-in is gone; a 404
	// without details (the resource type is not served) says nothing.
	var ke *K8sError
	if errors.As(err, &ke) && ke.Status == 404 && ke.DetailsKind == "useroauthaccesstokens" && ke.DetailsName == name {
		return nil, ErrSessionTokenGone
	}
	if err != nil {
		return nil, wrapUserTokenError("read the sign-in's token", err)
	}
	var obj struct {
		Metadata struct {
			CreationTimestamp time.Time `json:"creationTimestamp"`
		} `json:"metadata"`
		ExpiresIn int64    `json:"expiresIn"`
		Scopes    []string `json:"scopes"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("parse the sign-in's token: %w", err)
	}
	return &SessionToken{Created: obj.Metadata.CreationTimestamp, ExpiresIn: obj.ExpiresIn, Scopes: obj.Scopes}, nil
}

// LookupUserWithToken returns the name of the user who owns the token, as
// the API server authenticates it. Reading users/~ needs only the
// user:info OAuth scope, which oauth-proxy requests by default, and is the
// same call oauth-proxy's OpenShift provider uses to identify the user.
func LookupUserWithToken(ctx context.Context, userToken string) (string, error) {
	userClient := kube.NewClientWithContext(ctx, userToken)
	var user struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := userClient.GetInto("/apis/user.openshift.io/v1/users/~", &user); err != nil {
		if kube.IsDecodeError(err) {
			return "", fmt.Errorf("parse user: %w", err)
		}
		return "", wrapUserTokenError("identify user", err)
	}
	if user.Metadata.Name == "" {
		return "", fmt.Errorf("identify user: the API returned no user name")
	}
	return user.Metadata.Name, nil
}
