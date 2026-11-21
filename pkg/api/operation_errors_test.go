package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const repairBody = `{"name":"default-dsc","mode":"remove-invalid","expectedAPIVersion":"datasciencecluster.opendatahub.io/v2"}`

// The repair answers an error by its class: the API server's conflict,
// the user's ended sign-in, the user's RBAC and an unreachable API keep
// their own status and errorCode; its own refusals ("refresh and review
// again") stay 422 validation, and its OperationResponse answers stay as
// they were. A rejected request gives the user's rate-limit slot back.
func TestRepairDSCAnswersTheErrorsClass(t *testing.T) {
	for _, tc := range []struct {
		name      string
		result    *types.OperationResponse
		err       error
		status    int
		errorCode string
	}{
		// What changes (K1).
		{"conflict", nil, &cluster.K8sError{Status: 409, Reason: "Conflict", User: true}, http.StatusConflict, "conflict"},
		{"sign-in ended", nil, &cluster.K8sError{Status: 401, Reason: "Unauthorized", User: true}, http.StatusUnauthorized, "session_expired"},
		{"forbidden", nil, &cluster.K8sError{Status: 403, Reason: "Forbidden", User: true}, http.StatusForbidden, "forbidden"},
		{"network", nil, &cluster.NetworkError{Err: errors.New("dial tcp: connection refused")}, http.StatusBadGateway, "network"},
		{"api unavailable", nil, fmt.Errorf("cannot repair DSC without its installed schema: %w", &cluster.K8sError{Status: 503, User: true}), http.StatusBadGateway, "cluster_unavailable"},
		// What must not change (pins).
		{"refusal", nil, errors.New("operator version changed since the defaults preview; refresh and review defaults for 3.6.0"), http.StatusUnprocessableEntity, "validation"},
		{"refused result", &types.OperationResponse{Success: false, Message: "blocked", ErrorCode: "prerequisites"}, nil, http.StatusUnprocessableEntity, "prerequisites"},
		{"gitops result", &types.OperationResponse{Success: false, Message: "managed", ErrorCode: cluster.ErrorCodeGitOpsManaged}, nil, http.StatusUnprocessableEntity, cluster.ErrorCodeGitOpsManaged},
		{"success", &types.OperationResponse{Success: true, Message: "Removed invalid DSC fields: x"}, nil, http.StatusOK, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupDevMode(t)
			allowMutations(t)
			prev := repairDSC
			t.Cleanup(func() { repairDSC = prev })
			repairDSC = func(*cluster.Client, string, string, string, string, []string, []string) (*types.OperationResponse, error) {
				return tc.result, tc.err
			}
			mux := http.NewServeMux()
			Register(mux)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, postJSON(t, "/api/components/dsc/repair", "user:admin", repairBody))
			body := decodeError(t, w)
			if w.Code != tc.status || (tc.errorCode != "" && body["errorCode"] != tc.errorCode) {
				t.Fatalf("status=%d body=%s; want %d %s", w.Code, w.Body.String(), tc.status, tc.errorCode)
			}
			if tc.err != nil && body["error"] == "" {
				t.Fatalf("no message: %s", w.Body.String())
			}
			// The slot is used only by an accepted change (pin).
			if limited := mutationLimiter.isRateLimited("admin:/api/components/dsc/repair"); limited != (tc.status == http.StatusOK) {
				t.Fatalf("rate limited = %v after %d", limited, w.Code)
			}
		})
	}
}

// A handler that answered every error of its operation with a bare 500
// "internal" now answers by the error's class too, and an error of no
// known class is still a 500 internal with the handler's own message.
func TestFixAcknowledgedAnswersTheErrorsClass(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		status    int
		errorCode string
	}{
		{"sign-in ended", &cluster.K8sError{Status: 401, User: true}, http.StatusUnauthorized, "session_expired"},
		{"network", &cluster.NetworkError{Err: errors.New("connection refused")}, http.StatusBadGateway, "network"},
		{"unknown", errors.New("marshal patch: unsupported value"), http.StatusInternalServerError, "internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupDevMode(t)
			allowMutations(t)
			prev := applyFixAcknowledged
			t.Cleanup(func() { applyFixAcknowledged = prev })
			applyFixAcknowledged = func(*cluster.Client, string, cluster.AckBinding) (*types.OperationResponse, error) {
				return nil, tc.err
			}
			mux := http.NewServeMux()
			Register(mux)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, postJSON(t, "/api/diagnostics/fix/acknowledged", "user:admin", `{"problemId":"p","acknowledged":true,"binding":{"hash":"h"}}`))
			body := decodeError(t, w)
			if w.Code != tc.status || body["errorCode"] != tc.errorCode {
				t.Fatalf("status=%d body=%s; want %d %s", w.Code, w.Body.String(), tc.status, tc.errorCode)
			}
		})
	}
}

// operationErrorStatus is the reads' mapping (cluster.HTTPStatusForError)
// except for a change's 409 and the user's 403.
func TestOperationErrorStatus(t *testing.T) {
	for _, tc := range []struct {
		err       error
		status    int
		errorCode string
		known     bool
	}{
		{fmt.Errorf("check access: %w: %w", cluster.ErrUnauthenticated, &cluster.K8sError{Status: 401, User: true}), 401, "session_expired", true},
		{&cluster.K8sError{Status: 401, User: true}, 401, "session_expired", true},
		{&cluster.K8sError{Status: 401}, 401, "unauthorized", true},
		{&cluster.K8sError{Status: 403, User: true}, 403, "forbidden", true},
		{&cluster.K8sError{Status: 403}, 403, "forbidden", true},
		{fmt.Errorf("patch: %w", &cluster.K8sError{Status: 409}), 409, "conflict", true},
		{&cluster.K8sError{Status: 404}, 404, "not_found", true},
		{&cluster.K8sError{Status: 429}, 503, "rate_limited", true},
		{&cluster.K8sError{Status: 500}, 502, "cluster_unavailable", true},
		{&cluster.K8sError{Status: 422}, 502, "cluster_error", true},
		{&cluster.NetworkError{Err: errors.New("refused")}, 502, "network", true},
		{fmt.Errorf("wait: %w", context.DeadlineExceeded), 504, "timeout", true},
		{context.Canceled, 503, "canceled", true},
		{errors.New("marshal: bad"), 500, "internal", false},
	} {
		status, code, known := operationErrorStatus(tc.err)
		if status != tc.status || code != tc.errorCode || known != tc.known {
			t.Errorf("%v: %d %s %v; want %d %s %v", tc.err, status, code, known, tc.status, tc.errorCode, tc.known)
		}
	}
}
