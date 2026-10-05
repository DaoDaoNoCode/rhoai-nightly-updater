package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAPIServer points NewClientWithContext at an httptest TLS server.
func fakeAPIServer(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	t.Setenv("KUBERNETES_SERVICE_HOST", host)
	t.Setenv("KUBERNETES_SERVICE_PORT", port)
	previous := sharedTransport
	sharedTransport = srv.Client().Transport.(*http.Transport)
	t.Cleanup(func() { sharedTransport = previous })
}

func TestPermissionReviewUsesTokenOwnerFullRBAC(t *testing.T) {
	for _, tc := range []struct {
		name, body         string
		status             int
		allowed, wantError bool
		unauthenticated    bool
	}{
		{"group admin", `{"allowed":true}`, 200, true, false, false},
		{"viewer", `{"allowed":false}`, 200, false, false, false},
		{"expired token", `{}`, 401, false, true, true},
		{"forbidden review", `{}`, 403, false, true, false},
		{"service error", `{}`, 503, false, true, false},
		{"incomplete review", `{}`, 200, false, true, false},
		{"evaluation error", `{"allowed":true,"evaluationError":"RBAC lookup failed"}`, 200, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/apis/authorization.openshift.io/v1/subjectaccessreviews" || r.Header.Get("Authorization") != "Bearer scoped-user-token" {
					t.Errorf("wrong review request: %s %s", r.Method, r.URL.Path)
				}
				var review map[string]interface{}
				json.NewDecoder(r.Body).Decode(&review)
				if scopes, ok := review["scopes"].([]interface{}); !ok || len(scopes) != 0 {
					t.Error("full RBAC scopes must be explicitly empty")
				}
				if review["user"] != nil || review["groups"] != nil {
					t.Error("caller must not choose review identity")
				}
				if review["resource"] != "subscriptions" || review["verb"] != "update" || review["namespace"] != SubNS {
					t.Error(review)
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			allowed, err := CheckUserPermissionWithToken(context.Background(), "scoped-user-token", "update", "subscriptions", "operators.coreos.com", SubNS)
			if allowed != tc.allowed || (err != nil) != tc.wantError {
				t.Fatalf("allowed=%v err=%v", allowed, err)
			}
			if errors.Is(err, ErrUnauthenticated) != tc.unauthenticated {
				t.Fatalf("ErrUnauthenticated=%v for %v", errors.Is(err, ErrUnauthenticated), err)
			}
		})
	}
}

func TestLookupUserWithToken(t *testing.T) {
	for _, tc := range []struct {
		name, body      string
		status          int
		want            string
		wantErr         bool
		unauthenticated bool
	}{
		{"valid token", `{"metadata":{"name":"alice"}}`, 200, "alice", false, false},
		{"expired token", `{"kind":"Status","status":"Failure","reason":"Unauthorized"}`, 401, "", true, true},
		{"api unavailable", `{}`, 503, "", true, false},
		{"no name", `{"metadata":{}}`, 200, "", true, false},
		{"garbage", `not json`, 200, "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/apis/user.openshift.io/v1/users/~" || r.Header.Get("Authorization") != "Bearer user-token" {
					t.Errorf("wrong identity request: %s %s %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			got, err := LookupUserWithToken(context.Background(), "user-token")
			if got != tc.want || (err != nil) != tc.wantErr || errors.Is(err, ErrUnauthenticated) != tc.unauthenticated {
				t.Fatalf("got %q err=%v", got, err)
			}
		})
	}
}
