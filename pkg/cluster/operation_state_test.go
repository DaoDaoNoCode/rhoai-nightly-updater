package cluster

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// TestCompletedOperationRoundTrip covers the lastCompleted record next to
// the operation marker: created when the ConfigMap is missing, written with
// the marker clear in one merge patch, kept by later marker writes, and an
// unreadable value does not hide the marker.
func TestCompletedOperationRoundTrip(t *testing.T) {
	t.Setenv("NAMESPACE", "test-ns")
	const cmPath = "/api/v1/namespaces/test-ns/configmaps/rhoai-nightly-updater-operation"
	var mu sync.Mutex
	var data map[string]string // nil: the ConfigMap does not exist
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, r.Method+" "+r.Header.Get("Content-Type"))
		var body struct {
			Data map[string]string `json:"data"`
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == cmPath:
			if data == nil {
				w.WriteHeader(404)
				w.Write([]byte(`{"kind":"Status","status":"Failure","code":404}`))
				return
			}
			out, _ := json.Marshal(map[string]interface{}{"data": data})
			w.Write(out)
		case r.Method == http.MethodPatch && r.URL.Path == cmPath:
			if data == nil {
				w.WriteHeader(404)
				w.Write([]byte(`{"kind":"Status","status":"Failure","code":404}`))
				return
			}
			json.NewDecoder(r.Body).Decode(&body)
			for k, v := range body.Data { // merge (apply only ever sends "operation")
				data[k] = v
			}
			w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces/test-ns/configmaps":
			json.NewDecoder(r.Body).Decode(&body)
			data = body.Data
			w.WriteHeader(201)
			w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	c := &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: t.Context()}

	done := &types.CompletedOperation{ID: "op1", Type: "update", Label: "Update to nightly", User: "alice",
		Target: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6", StartedAt: "2026-10-05T10:00:00Z", FinishedAt: "2026-10-05T10:05:00Z", Success: true, Message: "done"}
	if err := SaveCompletedOperation(c, done, nil); err != nil {
		t.Fatal(err)
	}
	marker, last, err := GetOperationState(c)
	if err != nil || marker != nil || last == nil || *last != *done {
		t.Fatalf("after create: marker %+v last %+v err %v", marker, last, err)
	}

	next := &types.OperationMarker{ID: "op2", Type: "refresh", User: "bob", StartedAt: "2026-10-05T11:00:00Z", BootID: "b1"}
	if err := SaveOperationMarker(c, next); err != nil {
		t.Fatal(err)
	}
	marker, last, err = GetOperationState(c)
	if err != nil || marker == nil || *marker != *next || last == nil || last.ID != "op1" {
		t.Fatalf("marker write lost lastCompleted: marker %+v last %+v err %v", marker, last, err)
	}

	mu.Lock()
	data[lastCompletedKey] = "{not json"
	mu.Unlock()
	if marker, last, err := GetOperationState(c); err != nil || marker == nil || last != nil {
		t.Fatalf("unreadable lastCompleted: marker %+v last %+v err %v", marker, last, err)
	}

	done2 := *done
	done2.ID = "op2"
	if err := SaveCompletedOperation(c, &done2, nil); err != nil {
		t.Fatal(err)
	}
	if marker, last, err := GetOperationState(c); err != nil || marker != nil || last == nil || last.ID != "op2" {
		t.Fatalf("after completion: marker %+v last %+v err %v", marker, last, err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"PATCH application/merge-patch+json", "POST application/json", "GET ",
		"PATCH application/apply-patch+yaml", "GET ", "GET ", "PATCH application/merge-patch+json", "GET "}
	if len(requests) != len(want) {
		t.Fatalf("requests %v", requests)
	}
	for i := range want {
		if requests[i] != want[i] {
			t.Fatalf("request %d = %q, want %q (all: %v)", i, requests[i], want[i], requests)
		}
	}
}
