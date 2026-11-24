package kube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ListIntoAt returns the API server's time from its response's Date
// header, the zero time when there is none that parses, and ListInto's
// errors.
func TestListIntoAt(t *testing.T) {
	at := time.Date(2026, time.November, 24, 21, 12, 14, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dated":
			w.Header().Set("Date", at.Format(http.TimeFormat))
		case "/undated":
			w.Header()["Date"] = nil
		case "/baddate":
			w.Header().Set("Date", "yesterday")
		case "/refused":
			w.Header().Set("Date", at.Format(http.TimeFormat))
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"kind":"Status","status":"Failure","reason":"Forbidden","code":403,"message":"forbidden"}`))
			return
		case "/notjson":
			_, _ = w.Write([]byte(`<html>`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"name":"a"}]}`))
	}))
	defer srv.Close()
	c := NewTestClient(TestClientOptions{BaseURL: srv.URL, HTTPClient: srv.Client(), Ctx: context.Background()})
	type item struct {
		Name string `json:"name"`
	}
	for path, want := range map[string]time.Time{"/dated": at, "/undated": {}, "/baddate": {}} {
		var items []item
		got, err := c.ListIntoAt(path, &items)
		if err != nil || !got.Equal(want) || len(items) != 1 || items[0].Name != "a" {
			t.Errorf("%s: %v %v %v, want %v", path, got, items, err, want)
		}
	}
	var items []item
	if got, err := c.ListIntoAt("/refused", &items); !IsK8sError(err, http.StatusForbidden) || !got.IsZero() {
		t.Errorf("refused: %v %v", got, err)
	}
	if got, err := c.ListIntoAt("/notjson", &items); !IsDecodeError(err) || !got.IsZero() {
		t.Errorf("not JSON: %v %v", got, err)
	}
}
