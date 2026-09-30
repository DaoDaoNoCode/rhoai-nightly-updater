package cluster

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPermissionReviewUsesTokenOwnerFullRBAC(t *testing.T) {
	for _, tc := range []struct {
		name, body         string
		status             int
		allowed, wantError bool
	}{
		{"group admin", `{"allowed":true}`, 200, true, false},
		{"viewer", `{"allowed":false}`, 200, false, false},
		{"expired token", `{}`, 401, false, true},
		{"service error", `{}`, 503, false, true},
		{"incomplete review", `{}`, 200, false, true},
		{"evaluation error", `{"allowed":true,"evaluationError":"RBAC lookup failed"}`, 200, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/apis/authorization.openshift.io/v1/subjectaccessreviews" || r.Header.Get("Authorization") != "Bearer scoped-user-token" {
					t.Error("wrong review identity or endpoint")
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
			}))
			defer srv.Close()
			host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
			t.Setenv("KUBERNETES_SERVICE_HOST", host)
			t.Setenv("KUBERNETES_SERVICE_PORT", port)
			previous := sharedTransport
			sharedTransport = srv.Client().Transport.(*http.Transport)
			defer func() { sharedTransport = previous }()
			allowed, err := CheckUserPermissionWithToken(context.Background(), "scoped-user-token", "update", "subscriptions", "operators.coreos.com", SubNS)
			if allowed != tc.allowed || (err != nil) != tc.wantError {
				t.Fatalf("allowed=%v err=%v", allowed, err)
			}
		})
	}
}
