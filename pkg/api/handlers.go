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
	opID     string // set by lockCluster

	// The start of a JSON response body, from which the operation's
	// outcome is read; streams report it in their final event instead.
	body      []byte
	streaming bool
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
	ec := ""
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

// writeJSON encodes v as JSON and writes it to w, logging any encoding error.
func writeJSON(w http.ResponseWriter, v interface{}, label string) {
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
