package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func TestRateLimiter_FirstCallNotLimited(t *testing.T) {
	rl := &rateLimiter{window: 30 * time.Second}
	if rl.isRateLimited("alice") {
		t.Error("expected first call to not be rate limited")
	}
}

func TestRateLimiter_SecondCallWithinWindowIsLimited(t *testing.T) {
	rl := &rateLimiter{window: 30 * time.Second}
	rl.recordMutation("alice")
	if !rl.isRateLimited("alice") {
		t.Error("expected second call within window to be rate limited")
	}
}

func TestRateLimiter_DifferentUsersIndependent(t *testing.T) {
	rl := &rateLimiter{window: 30 * time.Second}
	rl.recordMutation("alice")
	if rl.isRateLimited("bob") {
		t.Error("expected bob to not be rate limited when only alice recorded a mutation")
	}
}

func TestRateLimiter_ExpiredWindowNotLimited(t *testing.T) {
	rl := &rateLimiter{window: 1 * time.Millisecond}
	rl.recordMutation("alice")
	time.Sleep(5 * time.Millisecond)
	if rl.isRateLimited("alice") {
		t.Error("expected call after window expiry to not be rate limited")
	}
}

func TestRecordMutation_StoresTimestamp(t *testing.T) {
	rl := &rateLimiter{window: 30 * time.Second}
	before := time.Now()
	rl.recordMutation("alice")
	after := time.Now()

	val, ok := rl.users.Load("alice")
	if !ok {
		t.Fatal("expected mutation timestamp to be stored")
	}
	ts := val.(time.Time)
	if ts.Before(before) || ts.After(after) {
		t.Errorf("stored timestamp %v not between %v and %v", ts, before, after)
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

// --- rateLimiter evictExpiredEntries tests ---

func TestRateLimiter_EvictExpiredEntries_RemovesExpiredKeys(t *testing.T) {
	rl := &rateLimiter{window: 1 * time.Millisecond}
	rl.recordMutation("alice")
	rl.recordMutation("bob")

	// Wait for entries to expire
	time.Sleep(5 * time.Millisecond)

	rl.evictExpiredEntries()

	if _, ok := rl.users.Load("alice"); ok {
		t.Error("expected 'alice' to be evicted after expiry")
	}
	if _, ok := rl.users.Load("bob"); ok {
		t.Error("expected 'bob' to be evicted after expiry")
	}
}

func TestRateLimiter_EvictExpiredEntries_KeepsFreshKeys(t *testing.T) {
	rl := &rateLimiter{window: 10 * time.Second}

	// Record a mutation that is well within the window
	rl.recordMutation("alice")

	rl.evictExpiredEntries()

	if _, ok := rl.users.Load("alice"); !ok {
		t.Error("expected 'alice' to remain after eviction (still within window)")
	}
}

func TestRateLimiter_EvictExpiredEntries_MixedExpiredAndFresh(t *testing.T) {
	rl := &rateLimiter{window: 50 * time.Millisecond}

	// Manually store a timestamp far in the past so it is already expired
	rl.users.Store("expired-user", time.Now().Add(-1*time.Second))

	// Record a fresh entry that is well within the window
	rl.recordMutation("fresh-user")

	rl.evictExpiredEntries()

	if _, ok := rl.users.Load("expired-user"); ok {
		t.Error("expected 'expired-user' to be evicted")
	}
	if _, ok := rl.users.Load("fresh-user"); !ok {
		t.Error("expected 'fresh-user' to remain after eviction")
	}
}
