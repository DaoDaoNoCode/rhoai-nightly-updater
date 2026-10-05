package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// TestRecordActivityConcurrent verifies that optimistic concurrency in
// RecordActivity properly handles concurrent callers so that no entries are lost.
//
// It spins up a mock K8s API server that stores ConfigMap state in memory
// and enforces resourceVersion checks (returning 409 Conflict on mismatch).
// It launches goroutines that each call RecordActivity once.  After all
// goroutines finish, it reads the ConfigMap and asserts that all entries
// are present.
//
// numGoroutines is kept <= maxConflictRetries to ensure every goroutine
// can succeed within its retry budget.
func TestRecordActivityConcurrent(t *testing.T) {
	t.Setenv("NAMESPACE", "test-ns")

	const numGoroutines = 3 // keep <= maxConflictRetries for deterministic success
	// Shared state: the stored ConfigMap body (starts empty -> 404).
	var mu sync.Mutex
	var storedCM []byte // nil means ConfigMap does not exist yet
	currentRV := ""
	rvCounter := 0

	// getCurrentRV extracts the resourceVersion from the stored ConfigMap body.
	getCurrentRV := func() string {
		return currentRV
	}

	// injectResourceVersion adds/updates resourceVersion in a ConfigMap JSON body
	// so that the optimistic concurrency PUT path works correctly.
	injectRV := func(body []byte) []byte {
		rvCounter++
		var cm map[string]interface{}
		if json.Unmarshal(body, &cm) != nil {
			return body
		}
		meta, _ := cm["metadata"].(map[string]interface{})
		if meta == nil {
			meta = map[string]interface{}{}
			cm["metadata"] = meta
		}
		currentRV = fmt.Sprintf("%d", rvCounter)
		meta["resourceVersion"] = currentRV
		out, _ := json.Marshal(cm)
		return out
	}

	// extractRV gets the resourceVersion from a request body.
	extractRV := func(body []byte) string {
		var cm map[string]interface{}
		if json.Unmarshal(body, &cm) != nil {
			return ""
		}
		meta, _ := cm["metadata"].(map[string]interface{})
		if meta == nil {
			return ""
		}
		rv, _ := meta["resourceVersion"].(string)
		return rv
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Accept both the specific ConfigMap path and the collection path (for POST)
		if r.URL.Path != "/api/v1/namespaces/test-ns/configmaps/rhoai-nightly-updater-activity" &&
			(r.Method != http.MethodPost || r.URL.Path != "/api/v1/namespaces/test-ns/configmaps") {
			// Activity recording must touch only its own ConfigMap.
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
			return
		}

		mu.Lock()
		defer mu.Unlock()

		switch r.Method {
		case "GET":
			if storedCM == nil {
				// ConfigMap doesn't exist yet -> 404.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"kind":"Status","status":"Failure","message":"not found","reason":"NotFound","code":404}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(storedCM)

		case "PATCH":
			// Server-side apply: store the full body as the new ConfigMap state.
			body, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"kind":"Status","status":"Failure","message":"read error","code":500}`))
				return
			}
			storedCM = injectRV(body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(storedCM)

		case "POST":
			// Create new ConfigMap — but reject if it already exists
			body, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"kind":"Status","status":"Failure","message":"read error","code":500}`))
				return
			}
			if storedCM != nil {
				// Already exists — return 409 Conflict
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				w.Write([]byte(`{"kind":"Status","status":"Failure","message":"already exists","reason":"AlreadyExists","code":409}`))
				return
			}
			storedCM = injectRV(body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			w.Write(storedCM)

		case "PUT":
			// Update ConfigMap with optimistic concurrency — check resourceVersion
			body, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"kind":"Status","status":"Failure","message":"read error","code":500}`))
				return
			}
			requestRV := extractRV(body)
			serverRV := getCurrentRV()
			if requestRV != serverRV {
				// resourceVersion mismatch — return 409 Conflict
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				w.Write([]byte(`{"kind":"Status","status":"Failure","message":"the object has been modified; please apply your changes to the latest version","reason":"Conflict","code":409}`))
				return
			}
			storedCM = injectRV(body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(storedCM)

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	client := &Client{
		baseURL:    server.URL,
		token:      "test-token",
		httpClient: &http.Client{},
		ctx:        context.Background(),
	}

	// Launch numGoroutines concurrent RecordActivity calls.
	var wg sync.WaitGroup
	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			RecordActivity(client, types.ActivityEntry{
				Timestamp: fmt.Sprintf("2025-01-01T00:00:%02dZ", idx),
				User:      fmt.Sprintf("user-%d", idx),
				Action:    "test",
				Detail:    fmt.Sprintf("entry-%d", idx),
				Success:   true,
			})
		}(i)
	}
	wg.Wait()

	// Read the final ConfigMap and verify all entries are present.
	mu.Lock()
	finalCM := storedCM
	mu.Unlock()

	if finalCM == nil {
		t.Fatal("ConfigMap was never created; expected it to contain entries")
	}

	var cm map[string]interface{}
	if err := json.Unmarshal(finalCM, &cm); err != nil {
		t.Fatalf("failed to parse final ConfigMap: %v", err)
	}

	data, ok := cm["data"].(map[string]interface{})
	if !ok {
		t.Fatal("ConfigMap has no 'data' field")
	}
	raw, ok := data["entries"].(string)
	if !ok || raw == "" {
		t.Fatal("ConfigMap data has no 'entries' field")
	}

	var entries []types.ActivityEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		t.Fatalf("failed to parse entries JSON: %v", err)
	}

	if len(entries) != numGoroutines {
		t.Fatalf("expected %d entries, got %d", numGoroutines, len(entries))
	}

	// Verify every goroutine's entry is present (by Detail field).
	seen := make(map[string]bool, numGoroutines)
	for _, e := range entries {
		seen[e.Detail] = true
	}
	for i := 0; i < numGoroutines; i++ {
		key := fmt.Sprintf("entry-%d", i)
		if !seen[key] {
			t.Errorf("missing entry for goroutine %d (Detail=%q)", i, key)
		}
	}
}

// TestRecordActivityExceedsMax verifies that when the total number of entries
// exceeds maxActivityEntries, the oldest entries are trimmed and exactly
// maxActivityEntries are kept.  Uses sequential calls to isolate the
// trimming logic from concurrency concerns.
func TestRecordActivityExceedsMax(t *testing.T) {
	t.Setenv("NAMESPACE", "test-ns")

	const numEntries = 25 // exceeds maxActivityEntries (20)

	var storedCM []byte
	rvCounter2 := 0

	injectRV2 := func(body []byte) []byte {
		rvCounter2++
		var cm map[string]interface{}
		if json.Unmarshal(body, &cm) != nil {
			return body
		}
		meta, _ := cm["metadata"].(map[string]interface{})
		if meta == nil {
			meta = map[string]interface{}{}
			cm["metadata"] = meta
		}
		meta["resourceVersion"] = fmt.Sprintf("%d", rvCounter2)
		out, _ := json.Marshal(cm)
		return out
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/test-ns/configmaps/rhoai-nightly-updater-activity" &&
			(r.Method != http.MethodPost || r.URL.Path != "/api/v1/namespaces/test-ns/configmaps") {
			// Activity recording must touch only its own ConfigMap.
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
			return
		}

		switch r.Method {
		case "GET":
			if storedCM == nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"kind":"Status","status":"Failure","message":"not found","reason":"NotFound","code":404}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(storedCM)

		case "POST":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			storedCM = injectRV2(body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			w.Write(storedCM)

		case "PUT":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			storedCM = injectRV2(body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(storedCM)

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	client := &Client{
		baseURL:    server.URL,
		token:      "test-token",
		httpClient: &http.Client{},
		ctx:        context.Background(),
	}

	// Call RecordActivity sequentially to isolate trimming behavior
	for i := 0; i < numEntries; i++ {
		RecordActivity(client, types.ActivityEntry{
			Timestamp: fmt.Sprintf("2025-01-01T00:00:%02dZ", i%60),
			User:      fmt.Sprintf("user-%d", i),
			Action:    "test",
			Detail:    fmt.Sprintf("entry-%d", i),
			Success:   true,
		})
	}

	if storedCM == nil {
		t.Fatal("ConfigMap was never created")
	}

	var cm map[string]interface{}
	if err := json.Unmarshal(storedCM, &cm); err != nil {
		t.Fatalf("failed to parse final ConfigMap: %v", err)
	}

	data, _ := cm["data"].(map[string]interface{})
	raw, _ := data["entries"].(string)

	var entries []types.ActivityEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		t.Fatalf("failed to parse entries: %v", err)
	}

	if len(entries) != maxActivityEntries {
		t.Fatalf("expected %d entries (maxActivityEntries), got %d", maxActivityEntries, len(entries))
	}
}

// TestGetActivityEmpty verifies GetActivity returns an empty slice when the
// ConfigMap does not exist.
func TestGetActivityEmpty(t *testing.T) {
	t.Setenv("NAMESPACE", "test-ns")

	client, cleanup := newMockClient(map[string]mockResponse{
		"/api/v1/namespaces/test-ns/configmaps/rhoai-nightly-updater-activity": {
			body:       `{"kind":"Status","status":"Failure","message":"not found","reason":"NotFound","code":404}`,
			statusCode: 404,
		},
	})
	defer cleanup()

	entries, err := GetActivity(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty entries, got %d", len(entries))
	}
}

// TestGetActivityReturnsEntries verifies GetActivity correctly parses stored
// entries from the ConfigMap.
func TestGetActivityReturnsEntries(t *testing.T) {
	t.Setenv("NAMESPACE", "test-ns")

	want := []types.ActivityEntry{
		{Timestamp: "2025-01-01T00:00:00Z", User: "alice", Action: "update", Detail: "img:v1", Success: true},
		{Timestamp: "2025-01-01T01:00:00Z", User: "bob", Action: "rollback", Detail: "", Success: false},
	}
	entriesJSON, _ := json.Marshal(want)

	cm := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"data": map[string]interface{}{
			"entries": string(entriesJSON),
		},
	}
	cmJSON, _ := json.Marshal(cm)

	client, cleanup := newMockClient(map[string]mockResponse{
		"/api/v1/namespaces/test-ns/configmaps/rhoai-nightly-updater-activity": {
			body:       string(cmJSON),
			statusCode: 200,
		},
	})
	defer cleanup()

	got, err := GetActivity(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d entries, got %d", len(want), len(got))
	}
	for i, e := range got {
		if e.User != want[i].User || e.Action != want[i].Action {
			t.Errorf("entry[%d]: got %+v, want %+v", i, e, want[i])
		}
	}
}
