package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

var imageRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]+(:[a-zA-Z0-9._-]+)?(@sha256:[a-f0-9]{64})?$`)
var namespaceRegex = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var channelRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// customFBCImageRegex accepts any build of the RHOAI FBC repository (by tag
// and/or digest). An FBC is installed with Automatic approval into an
// AllNamespaces OperatorGroup, so it must not come from an arbitrary
// registry or Quay organization.
var customFBCImageRegex = regexp.MustCompile(`^quay\.io/rhoai/rhoai-fbc-fragment(:[a-zA-Z0-9._-]+)?(@sha256:[a-f0-9]{64})?$`)
var dscNameRegex = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

// allowedImagePrefixes is the whitelist of registries from which images may be pulled.
var allowedImagePrefixes = []string{
	"quay.io/rhoai/",
	"registry.redhat.io/rhoai/",
}

// forwardSteps returns an emitter that streams pipeline steps to the client.
// A failed write means the client went away; the operation keeps running
// (it is detached from the request) and SSEWriter still records the step
// for GET /api/operation, so the error is only logged.
func forwardSteps(sseWriter *SSEWriter) func(cluster.UpdateStepEvent) {
	return func(event cluster.UpdateStepEvent) {
		if err := sseWriter.SendStep(UpdateStep{
			Step:      event.Step,
			Status:    event.Status,
			Message:   event.Message,
			Detail:    event.Detail,
			ElapsedMs: time.Since(sseWriter.StartTime()).Milliseconds(),
			ErrorCode: event.ErrorCode,
		}); err != nil {
			slog.Debug("sse: step not delivered; the operation continues", "step", event.Step, "error", err)
		}
	}
}

// sseHeartbeat sends periodic SSE comments to keep the connection alive
// through proxies with idle timeouts. Stops when done is closed.
func sseHeartbeat(w *SSEWriter, done <-chan struct{}) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			w.SendHeartbeat()
		}
	}
}

// isAllowedImage checks whether the image reference starts with an allowed registry prefix.
func isAllowedImage(image string) bool {
	lower := strings.ToLower(image)
	for _, prefix := range allowedImagePrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// statusWriter records the response status of a mutation and carries the
// request's identity to lockCluster, which publishes the running operation.
type statusWriter struct {
	http.ResponseWriter
	status int

	path string
	user string
	// bookkeeper is the ServiceAccount client of the operation's lease and
	// marker (never the client the operation acts with).
	bookkeeper *cluster.Client
	// cancelOp ends the operation's context with a cause.
	cancelOp context.CancelCauseFunc
	// newBookkeeper makes a ServiceAccount client of the updater's own
	// records with the ServiceAccount token as it is now: for a recipe
	// run, whose steps outlive the request (recipe_runner.go). It is part
	// of the wrapper's bookkeeping, like bookkeeper.
	newBookkeeper func(ctx context.Context) *cluster.Client
	// gate checks again what the wrapper checked before the request was
	// accepted: the user's sign-in can carry a change (sessionProblem)
	// and they are still a cluster-admin (mutationPermission). It returns
	// the refusal's message and errorCode, "" when the change may go on.
	// A recipe run calls it before each step.
	gate func(ctx context.Context) (msg, code string)
	opID string // set by lockCluster
	// admin: the caller is a cluster-admin (withMutationAuth checked it).
	admin bool

	// The start of a JSON response body, from which the operation's
	// outcome is read; streams report it in their final event instead.
	body      []byte
	streaming bool
	// panicked: the handler recovered a panic of its operation
	// (reportOperationPanic), whatever status it had written.
	panicked bool
}

// maxCapturedResponse bounds the response bytes kept for the outcome.
const maxCapturedResponse = 16 << 10

func (sw *statusWriter) WriteHeader(code int) {
	if sw.status == 0 {
		sw.status = code
		sw.streaming = strings.HasPrefix(sw.Header().Get("Content-Type"), "text/event-stream")
	}
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if sw.status == 0 {
		sw.status = http.StatusOK
		sw.streaming = strings.HasPrefix(sw.Header().Get("Content-Type"), "text/event-stream")
	}
	if !sw.streaming && len(sw.body) < maxCapturedResponse {
		sw.body = append(sw.body, b[:min(len(b), maxCapturedResponse-len(sw.body))]...)
	}
	return sw.ResponseWriter.Write(b)
}

// Unwrap lets ResponseController reach streaming and deadline support on the
// underlying writer while the wrapper continues to track response status.
func (sw *statusWriter) Unwrap() http.ResponseWriter {
	return sw.ResponseWriter
}

// writeError writes a JSON error response with an optional error code.
// The response always includes both "error" and "errorCode" fields for a
// consistent client-facing shape. When no explicit errorCode is provided,
// a default is derived from the HTTP status code.
func writeError(w http.ResponseWriter, msg string, code int, errorCode ...string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	var ec string
	if len(errorCode) > 0 && errorCode[0] != "" {
		ec = errorCode[0]
	} else {
		ec = defaultErrorCode(code)
	}
	respMap := map[string]string{"error": msg, "errorCode": ec}
	resp, err := json.Marshal(respMap)
	if err != nil {
		w.Write([]byte(`{"error":"internal server error","errorCode":"internal"}`))
		return
	}
	w.Write(resp)
}

// defaultErrorCode returns a sensible errorCode string for the given HTTP status.
func defaultErrorCode(httpStatus int) string {
	switch httpStatus {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusUnprocessableEntity:
		return "unprocessable"
	default:
		return "internal"
	}
}

// operationErrorStatus is the HTTP status and errorCode of the non-nil
// error of a change (the second result of a cluster operation: see
// CONTRIBUTING "Errors and operation results"), taken from the error
// itself; known is false for an error of no known class.
//
// It is cluster.HTTPStatusForError (the reads' mapping) with two changes
// for a change: the API server's 409 is "conflict" (the object changed
// since it was read: a resourceVersion precondition, or it exists), not a
// "cluster_error", and the user's own 403 is "forbidden" (their RBAC
// refused the change, errorCodeFromK8sErr), not the reads'
// "forbidden_for_user" ("you cannot see this"). A 401 to the user's token
// stays "session_expired", as in writeAuthError: every operation that
// returns an error does so before or at its first refused write.
func operationErrorStatus(err error) (status int, errorCode string, known bool) {
	switch {
	case errors.Is(err, cluster.ErrUnauthenticated):
		return http.StatusUnauthorized, "session_expired", true
	case cluster.IsK8sError(err, http.StatusConflict):
		return http.StatusConflict, "conflict", true
	case cluster.IsK8sError(err, http.StatusForbidden):
		return http.StatusForbidden, "forbidden", true
	}
	status, errorCode = cluster.HTTPStatusForError(err)
	return status, errorCode, errorCode != "internal"
}

// writeOperationError answers the non-nil error of a change, which the
// handler has logged: with the error's own status and errorCode
// (operationErrorStatus) and msg followed by the error's text, or, for an
// error of no known class, 500 "internal" with msg alone.
func writeOperationError(w http.ResponseWriter, msg string, err error) {
	if status, errorCode, known := operationErrorStatus(err); known {
		writeError(w, msg+": "+err.Error(), status, errorCode)
		return
	}
	writeError(w, msg, http.StatusInternalServerError, "internal")
}

// writeReadError answers the error of a read, which the handler has
// logged, with msg and the status and errorCode of the reads' mapping
// (cluster.HTTPStatusForError, as HandleDebug and the status do).
func writeReadError(w http.ResponseWriter, msg string, err error) {
	status, errorCode := cluster.HTTPStatusForError(err)
	writeError(w, msg, status, errorCode)
}

// writeJSON encodes v as JSON and writes it to w, logging any encoding error.
func writeJSON(w http.ResponseWriter, v any, label string) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("response encode error", "label", label, "error", err)
	}
}

// writeOperationResult writes a standard OperationResponse.
// On success it returns 200 with the result as JSON.
// On failure (result.Success == false) it returns 422 with the result as JSON.
func writeOperationResult(w http.ResponseWriter, result *types.OperationResponse, label string) {
	if !result.Success {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		if err := json.NewEncoder(w).Encode(result); err != nil {
			slog.Error("response encode error", "label", label, "error", err)
		}
		return
	}
	writeJSON(w, result, label)
}

// readBody decodes the request's JSON body into v, reading at most limit
// bytes. With allowEmpty an empty body is no error (v keeps its zero
// value); malformed JSON or a body over the limit always is. It writes no
// response: handlers that answer a bad body with their own message use it,
// the others use decodeBody.
func readBody(w http.ResponseWriter, r *http.Request, limit int64, v any, allowEmpty bool) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	err := json.NewDecoder(r.Body).Decode(v)
	if allowEmpty && errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// decodeBody is readBody answering a bad body with 400 "invalid request
// body" (errorCode "validation"). It returns false when the handler must
// stop.
func decodeBody(w http.ResponseWriter, r *http.Request, limit int64, v any, allowEmpty bool) bool {
	if err := readBody(w, r, limit, v, allowEmpty); err != nil {
		writeError(w, "invalid request body", http.StatusBadRequest, "validation")
		return false
	}
	return true
}
