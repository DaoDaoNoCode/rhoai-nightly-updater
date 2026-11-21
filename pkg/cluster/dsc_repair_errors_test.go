package cluster

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// RepairDSC returns the API server's refusal of its final patch, made with
// the signed-in user's token, as the request's own *K8sError (status and
// User kept), and a transport failure as *NetworkError: pkg/api
// HandleRepairDSC must answer each by its class, not as "validation".
func TestRepairDSCReturnsTheRequestsOwnError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		reason string
	}{
		{"conflict", http.StatusConflict, "Conflict"},
		{"sign-in ended", http.StatusUnauthorized, "Unauthorized"},
		{"forbidden", http.StatusForbidden, "Forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "datascienceclusters/"):
					w.WriteHeader(tc.status)
					io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"`+tc.reason+`","code":`+strconv.Itoa(tc.status)+`}`)
				case strings.Contains(r.URL.Path, "customresourcedefinitions/"):
					io.WriteString(w, testDSCCRD())
				case strings.Contains(r.URL.Path, "datascienceclusters/"):
					io.WriteString(w, `{"apiVersion":"datasciencecluster.opendatahub.io/v2","metadata":{"name":"my-dsc","resourceVersion":"42"},"spec":{"components":{"dashboard":{"managementState":"Removed","oldField":"x"}}}}`)
				default:
					io.WriteString(w, `{}`)
				}
			}))
			defer server.Close()
			c := newTestClient(testClientOptions{BaseURL: server.URL, HTTPClient: server.Client(), Ctx: context.Background(), AsUser: true})
			result, err := RepairDSC(c, "my-dsc", "remove-invalid", "", "datasciencecluster.opendatahub.io/v2", nil, nil)
			k, ok := errors.AsType[*K8sError](err)
			if result != nil || !ok || k.Status != tc.status || !k.User {
				t.Fatalf("RepairDSC = %+v, %v (%T)", result, err, err)
			}
		})
	}
	t.Run("network", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		server.Close() // nothing listens any more
		c := newTestClient(testClientOptions{BaseURL: server.URL, HTTPClient: server.Client(), Ctx: context.Background(), AsUser: true})
		result, err := RepairDSC(c, "my-dsc", "remove-invalid", "", "datasciencecluster.opendatahub.io/v2", nil, nil)
		if result != nil || !kube.IsNetworkError(err) {
			t.Fatalf("RepairDSC = %+v, %v (%T)", result, err, err)
		}
	})
}
