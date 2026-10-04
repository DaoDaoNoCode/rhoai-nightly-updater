package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireJSONForMutations(t *testing.T) {
	cases := []struct {
		method, path, contentType string
		wantNext                  bool
	}{
		{"POST", "/api/refresh/stream", "", false},
		{"POST", "/api/refresh/stream", "text/plain", false},
		{"POST", "/api/refresh/stream", "application/x-www-form-urlencoded", false},
		{"POST", "/api/refresh/stream", "multipart/form-data; boundary=x", false},
		{"DELETE", "/api/anything", "", false},
		{"POST", "/api/refresh/stream", "application/json", true},
		{"POST", "/api/refresh/stream", "application/json; charset=utf-8", true},
		{"POST", "/api/refresh/stream", "Application/JSON", true},
		{"GET", "/api/status", "", true},
		{"HEAD", "/api/status", "", true},
		{"OPTIONS", "/api/update", "", true},
		{"POST", "/not-api", "", true},
	}
	for _, tc := range cases {
		called := false
		h := RequireJSONForMutations(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
		r := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.contentType != "" {
			r.Header.Set("Content-Type", tc.contentType)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if called != tc.wantNext {
			t.Errorf("%s %s (%q): called=%v want %v", tc.method, tc.path, tc.contentType, called, tc.wantNext)
		}
		if !tc.wantNext && w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("%s %s (%q): status %d", tc.method, tc.path, tc.contentType, w.Code)
		}
	}
}
