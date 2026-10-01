package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
)

func TestMutationAuthStreamsProgressAndCompletion(t *testing.T) {
	setupDevMode(t)
	rec := httptest.NewRecorder()
	called := false
	handler := withMutationAuth(func(_ *cluster.Client, w http.ResponseWriter, _ *http.Request) {
		called = true
		sw, err := NewSSEWriter(w)
		if err != nil {
			t.Fatal(err)
		}
		defer sw.Close()
		if err := sw.EmitStep("validate_target", "running", "Validating"); err != nil {
			t.Fatal(err)
		}
		if !rec.Flushed {
			t.Fatal("progress was not flushed before the handler completed")
		}
		sw.SendHeartbeat()
		if err := sw.EmitStep("operation_complete", "success", "Installed"); err != nil {
			t.Fatal(err)
		}
	})
	req := httptest.NewRequest("POST", "/api/test-mutation-stream", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	handler.ServeHTTP(rec, req)
	if !called {
		t.Fatalf("handler not called: %d %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{`"step":"validate_target"`, ": heartbeat\n\n", `"step":"operation_complete"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("stream missing %q: %s", want, rec.Body.String())
		}
	}
}

func TestNewSSEWriter_SetsHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	_, err := NewSSEWriter(rec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	checks := map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"Connection":        "keep-alive",
		"X-Accel-Buffering": "no",
	}
	for header, want := range checks {
		got := rec.Header().Get(header)
		if got != want {
			t.Errorf("header %s = %q, want %q", header, got, want)
		}
	}
}

func TestSendStep_WritesSSEFormat(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, _ := NewSSEWriter(rec)

	err := sw.SendStep(UpdateStep{
		Step:      "apply_catalog",
		Status:    "running",
		Message:   "Applying catalog source",
		ElapsedMs: 1234,
	})
	if err != nil {
		t.Fatalf("SendStep error: %v", err)
	}

	body := rec.Body.String()
	if !strings.HasPrefix(body, "data: ") {
		t.Errorf("expected body to start with 'data: ', got %q", body)
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Errorf("expected body to end with '\\n\\n', got %q", body)
	}
	if !strings.Contains(body, `"step":"apply_catalog"`) {
		t.Errorf("expected step field in output, got %q", body)
	}
	if !strings.Contains(body, `"status":"running"`) {
		t.Errorf("expected status field in output, got %q", body)
	}
	if !strings.Contains(body, `"elapsedMs":1234`) {
		t.Errorf("expected elapsedMs field in output, got %q", body)
	}
}

func TestSendStep_OmitsEmptyOptionalFields(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, _ := NewSSEWriter(rec)

	sw.SendStep(UpdateStep{
		Step:      "test",
		Status:    "success",
		Message:   "done",
		ElapsedMs: 0,
	})

	body := rec.Body.String()
	if strings.Contains(body, `"detail"`) {
		t.Errorf("expected detail to be omitted (omitempty), got %q", body)
	}
	if strings.Contains(body, `"errorCode"`) {
		t.Errorf("expected errorCode to be omitted (omitempty), got %q", body)
	}
}

func TestEmitStep_CalculatesElapsedMs(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, _ := NewSSEWriter(rec)

	time.Sleep(10 * time.Millisecond)
	sw.EmitStep("test_step", "running", "testing elapsed")

	body := rec.Body.String()
	if !strings.Contains(body, `"elapsedMs":`) {
		t.Errorf("expected elapsedMs in output, got %q", body)
	}
	if strings.Contains(body, `"elapsedMs":0`) {
		t.Errorf("expected non-zero elapsedMs after sleep, got %q", body)
	}
}

func TestEmitStepWithDetail_IncludesDetail(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, _ := NewSSEWriter(rec)

	sw.EmitStepWithDetail("apply", "success", "Applied", "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5")

	body := rec.Body.String()
	if !strings.Contains(body, `"detail":"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5"`) {
		t.Errorf("expected detail field in output, got %q", body)
	}
}

func TestSendHeartbeat_WritesComment(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, _ := NewSSEWriter(rec)

	sw.SendHeartbeat()

	body := rec.Body.String()
	if body != ": heartbeat\n\n" {
		t.Errorf("expected ': heartbeat\\n\\n', got %q", body)
	}
}

func TestMultipleSteps_Accumulate(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, _ := NewSSEWriter(rec)

	sw.EmitStep("step1", "success", "first")
	sw.SendHeartbeat()
	sw.EmitStep("step2", "running", "second")

	body := rec.Body.String()
	parts := strings.Split(body, "\n\n")
	// 3 events + trailing empty string after final \n\n
	if len(parts) != 4 {
		t.Errorf("expected 4 parts (3 events + trailing), got %d: %v", len(parts), parts)
	}
	if !strings.HasPrefix(parts[0], "data: ") {
		t.Errorf("first event should be data, got %q", parts[0])
	}
	if parts[1] != ": heartbeat" {
		t.Errorf("second event should be heartbeat comment, got %q", parts[1])
	}
	if !strings.HasPrefix(parts[2], "data: ") {
		t.Errorf("third event should be data, got %q", parts[2])
	}
}
