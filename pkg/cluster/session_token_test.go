package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The object name is the API server's lookup name: "sha256~" and the
// unpadded base64url SHA-256 of the token without its prefix.
func TestAccessTokenObjectName(t *testing.T) {
	sum := sha256.Sum256([]byte("abc123"))
	want := "sha256~" + base64.RawURLEncoding.EncodeToString(sum[:])
	got, err := accessTokenObjectName("sha256~abc123")
	if err != nil || got != want || strings.Contains(got, "=") {
		t.Fatalf("name = %q, %v; want %q", got, err, want)
	}
	for _, tok := range []string{"abc123", "sha256~", "eyJhbGciOi.serviceaccount"} {
		if _, err := accessTokenObjectName(tok); !errors.Is(err, ErrNotAnOAuthToken) {
			t.Errorf("%q: err = %v, want ErrNotAnOAuthToken", tok, err)
		}
	}
}

func TestReadSessionToken(t *testing.T) {
	const token = "sha256~secret-token-value"
	name, _ := accessTokenObjectName(token)
	created := time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)
	var status int
	var gotAuth, gotPath, notFound string
	fakeAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		if status == http.StatusNotFound {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(notFound))
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"kind":"Status","status":"Failure","code":%d,"reason":"x","message":"useroauthaccesstokens %q is refused"}`, status, name)
			return
		}
		fmt.Fprintf(w, `{"kind":"UserOAuthAccessToken","metadata":{"name":%q,"creationTimestamp":%q},"expiresIn":86400,"inactivityTimeoutSeconds":600,"scopes":["user:full"],"clientName":"app-ns"}`, name, created.Format(time.RFC3339))
	})
	s, err := ReadSessionToken(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer "+token || gotPath != "/apis/oauth.openshift.io/v1/useroauthaccesstokens/"+name {
		t.Fatalf("request %s with %q", gotPath, gotAuth)
	}
	if !s.FullScope() || s.ExpiresIn != 86400 || !s.Created.Equal(created) {
		t.Fatalf("token %+v", s)
	}
	if left, ok := s.Remaining(created.Add(23 * time.Hour)); !ok || left != time.Hour {
		t.Fatalf("remaining %v %v", left, ok)
	}
	if _, ok := (&SessionToken{Created: created}).Remaining(created); ok {
		t.Fatal("expiresIn 0 never expires")
	}
	if (&SessionToken{Scopes: []string{"user:info", "user:check-access"}}).FullScope() {
		t.Fatal("a user:info user:check-access token cannot act")
	}

	status = http.StatusUnauthorized
	_, err = ReadSessionToken(context.Background(), token)
	if !errors.Is(err, ErrUnauthenticated) || strings.Contains(err.Error(), "secret-token-value") {
		t.Fatalf("401: %v", err)
	}
	// A 404 about this very object (live body: details.kind
	// useroauthaccesstokens, details.name the object's name): revoked.
	notFound = fmt.Sprintf(`{"kind":"Status","status":"Failure","code":404,"reason":"NotFound","details":{"name":%q,"group":"oauth.openshift.io","kind":"useroauthaccesstokens"}}`, name)
	status = http.StatusNotFound
	if _, err = ReadSessionToken(context.Background(), token); !errors.Is(err, ErrSessionTokenGone) {
		t.Fatalf("own object gone: %v", err)
	}
	// A 404 without details (live: the resource type is not served), or
	// about another object, says nothing about the sign-in.
	for _, body := range []string{
		`{"kind":"Status","status":"Failure","code":404,"reason":"NotFound","details":{}}`,
		`{"kind":"Status","status":"Failure","code":404,"reason":"NotFound","details":{"name":"sha256~other","kind":"useroauthaccesstokens"}}`,
	} {
		notFound = body
		if _, err = ReadSessionToken(context.Background(), token); errors.Is(err, ErrSessionTokenGone) || !IsK8sError(err, http.StatusNotFound) {
			t.Fatalf("404 %s: %v", body, err)
		}
	}
	notFound = ""
	status = http.StatusForbidden
	_, err = ReadSessionToken(context.Background(), token)
	if !IsK8sError(err, http.StatusForbidden) || errors.Is(err, ErrUnauthenticated) || strings.Contains(err.Error(), "secret-token-value") {
		t.Fatalf("403: %v", err)
	}
}

// A 401 or 403 answered to the user's own token says that the user's
// sign-in or permissions refused it, and maps to session_expired; the same
// answer to the ServiceAccount's token does not.
func TestUserTokenErrorsNameTheUser(t *testing.T) {
	fakeAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		code := http.StatusUnauthorized
		if strings.Contains(r.URL.Path, "forbidden") {
			code = http.StatusForbidden
		}
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"kind":"Status","status":"Failure","code":%d,"reason":"r","message":"m"}`, code)
	})
	user := NewUserClient(context.Background(), "sha256~user-secret")
	sa := NewClientWithContext(context.Background(), "sa-secret")
	if !user.ActsAsUser() || sa.ActsAsUser() {
		t.Fatal("ActsAsUser")
	}
	_, _, err := user.get("/api/v1/namespaces/x")
	if !strings.Contains(err.Error(), UserTokenRejectedMessage) || strings.Contains(err.Error(), "user-secret") {
		t.Fatalf("user 401: %v", err)
	}
	if code, ec := HTTPStatusForError(err); code != 401 || ec != "session_expired" {
		t.Fatalf("user 401 maps to %d %s", code, ec)
	}
	_, _, err = user.get("/api/v1/namespaces/forbidden")
	if !strings.Contains(err.Error(), UserForbiddenMessage) {
		t.Fatalf("user 403: %v", err)
	}
	_, _, err = sa.get("/api/v1/namespaces/x")
	if strings.Contains(err.Error(), "sign-in") {
		t.Fatalf("SA 401 named the user's sign-in: %v", err)
	}
	if code, ec := HTTPStatusForError(err); code != 401 || ec != "unauthorized" {
		t.Fatalf("SA 401 maps to %d %s", code, ec)
	}
}
