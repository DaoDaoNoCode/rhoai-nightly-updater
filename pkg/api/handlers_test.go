package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func TestValidateReinstallRequestCustomBuilds(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, image := range []string{
		"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + digest,
		"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.3@sha256:" + digest,
		"quay.io/rhoai/rhoai-fbc-fragment@sha256:" + digest,
		"quay.io/rhoai/rhoai-fbc-fragment:build-42",
	} {
		req := types.ReinstallRequest{TargetType: "custom", Image: " " + image + " ", Channel: " stable-3.3 "}
		if err := validateReinstallRequest(&req); err != nil {
			t.Errorf("rejected %s: %v", image, err)
		}
		if req.Image != image || req.Channel != "stable-3.3" {
			t.Fatal("request not normalized")
		}
	}
	for _, req := range []types.ReinstallRequest{
		{TargetType: "custom"},
		{TargetType: "custom", Image: "quay.io/rhoai/fbc"},
		{TargetType: "custom", Image: "quay.io/rhoai/fbc@sha256:abc"},
		{TargetType: "custom", Image: "quay.io.evil.test/rhoai/fbc:build"},
		{TargetType: "custom", Image: "docker.io/rhoai/fbc:build"},
		{TargetType: "custom", Image: "quay.io/rhoai/rhoai-fbc-fragment:build", Channel: "bad channel"},
		// Only the RHOAI FBC repository: an FBC is installed with Automatic
		// approval, so another Quay org or repo could install any operator.
		{TargetType: "custom", Image: "quay.io/another-team/custom-fbc:build-42"},
		{TargetType: "custom", Image: "quay.io/attacker/evil-catalog:v1"},
		{TargetType: "custom", Image: "quay.io/rhoai/other-repo:build"},
		{TargetType: "custom", Image: "quay.io/rhoai/rhoai-fbc-fragment-evil:build"},
		{TargetType: "custom", Image: "quay.io/rhoai/rhoai-fbc-fragment/sub:build"},
		{TargetType: "nightly", Image: "docker.io/evil/fbc:1"},
		{TargetType: "nightly", Image: "quay.io/rhoaix/fbc:1"},
		{TargetType: "unknown"},
	} {
		if err := validateReinstallRequest(&req); err == nil {
			t.Errorf("accepted invalid request: %+v", req)
		}
	}
	legacy := types.ReinstallRequest{}
	if err := validateReinstallRequest(&legacy); err != nil || legacy.TargetType != "stable" {
		t.Fatal("legacy stable request rejected")
	}
}

func TestResolveUsername_XForwardedUser(t *testing.T) {
	req, _ := http.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-User", "jsmith")
	got := resolveUsername(req)
	if got != "jsmith" {
		t.Errorf("expected 'jsmith', got %q", got)
	}
}

func TestResolveUsername_DevUser(t *testing.T) {
	req, _ := http.NewRequest("GET", "/", nil)
	// No X-Forwarded-User header, and DEV_MODE not set
	got := resolveUsername(req)
	if got != "unknown" {
		t.Errorf("expected 'unknown' when no header and no DEV_MODE, got %q", got)
	}
}

func TestResolveUsername_FallbackUnknown(t *testing.T) {
	req, _ := http.NewRequest("GET", "/", nil)
	got := resolveUsername(req)
	if got != "unknown" {
		t.Errorf("expected 'unknown', got %q", got)
	}
}

func TestNamespaceRegex_Valid(t *testing.T) {
	valid := []string{
		"my-project",
		"test123",
		"a",
		"ab",
		"a-b",
		"project-with-dashes-01",
	}
	for _, ns := range valid {
		if !namespaceRegex.MatchString(ns) {
			t.Errorf("expected %q to be valid", ns)
		}
	}
}

func TestNamespaceRegex_Invalid(t *testing.T) {
	invalid := []string{
		"",
		"-starts-with-dash",
		"ends-with-dash-",
		"UPPER",
		"has_underscore",
		"has.dot",
		"has space",
		"a@b",
	}
	for _, ns := range invalid {
		if namespaceRegex.MatchString(ns) {
			t.Errorf("expected %q to be invalid", ns)
		}
	}
}

// --- writeError tests ---

func TestWriteError_WithErrorCode_IncludesBothFields(t *testing.T) {
	w := httptest.NewRecorder()
	writeError(w, "something went wrong", http.StatusBadRequest, "validation")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}

	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to unmarshal response body: %v", err)
	}
	if _, ok := body["error"]; !ok {
		t.Error("response JSON missing 'error' field")
	}
	if body["error"] != "something went wrong" {
		t.Errorf("expected error message 'something went wrong', got %q", body["error"])
	}
	if _, ok := body["errorCode"]; !ok {
		t.Error("response JSON missing 'errorCode' field")
	}
	if body["errorCode"] != "validation" {
		t.Errorf("expected errorCode 'validation', got %q", body["errorCode"])
	}
}

func TestWriteError_WithoutErrorCode_HasDefaultErrorCode(t *testing.T) {
	w := httptest.NewRecorder()
	writeError(w, "internal failure", http.StatusInternalServerError)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status %d, got %d", http.StatusInternalServerError, w.Code)
	}

	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to unmarshal response body: %v", err)
	}
	if _, ok := body["error"]; !ok {
		t.Error("response JSON missing 'error' field")
	}
	if body["error"] != "internal failure" {
		t.Errorf("expected error message 'internal failure', got %q", body["error"])
	}
	// When no explicit errorCode is supplied, writeError derives a default from the HTTP status
	if _, ok := body["errorCode"]; !ok {
		t.Error("response JSON missing 'errorCode' field (should have default)")
	}
	if body["errorCode"] != "internal" {
		t.Errorf("expected default errorCode 'internal' for 500, got %q", body["errorCode"])
	}
}

func TestWriteError_EmptyErrorCode_UsesDefault(t *testing.T) {
	w := httptest.NewRecorder()
	writeError(w, "bad", http.StatusBadRequest, "")

	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to unmarshal response body: %v", err)
	}
	// An empty errorCode string is treated the same as omitting it:
	// writeError falls back to the default for the HTTP status code.
	if _, ok := body["errorCode"]; !ok {
		t.Error("response JSON missing 'errorCode' field (should have default)")
	}
	if body["errorCode"] != "bad_request" {
		t.Errorf("expected default errorCode 'bad_request' for 400, got %q", body["errorCode"])
	}
}

// --- writeOperationResult tests ---

func TestWriteOperationResult_FailureReturns422(t *testing.T) {
	w := httptest.NewRecorder()
	result := &types.OperationResponse{
		Success: false,
		Message: "prerequisites not met",
	}
	writeOperationResult(w, result, "test-op")

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected status %d, got %d", http.StatusUnprocessableEntity, w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}

	var body types.OperationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to unmarshal response body: %v", err)
	}
	if body.Success {
		t.Error("expected Success to be false in response body")
	}
	if body.Message != "prerequisites not met" {
		t.Errorf("expected message 'prerequisites not met', got %q", body.Message)
	}
}

func TestWriteOperationResult_SuccessReturns200(t *testing.T) {
	w := httptest.NewRecorder()
	result := &types.OperationResponse{
		Success: true,
		Message: "operation completed",
		Logs:    []string{"step 1 done", "step 2 done"},
	}
	writeOperationResult(w, result, "test-op")

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}

	var body types.OperationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to unmarshal response body: %v", err)
	}
	if !body.Success {
		t.Error("expected Success to be true in response body")
	}
	if body.Message != "operation completed" {
		t.Errorf("expected message 'operation completed', got %q", body.Message)
	}
	if len(body.Logs) != 2 {
		t.Errorf("expected 2 log entries, got %d", len(body.Logs))
	}
}
