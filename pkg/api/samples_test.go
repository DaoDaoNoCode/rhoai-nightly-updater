package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSamplesStatusNeedsAValidProject(t *testing.T) {
	setupDevMode(t)
	for _, h := range []http.HandlerFunc{HandleMLflowSamplesStatus} {
		for _, q := range []string{"", "?project=", "?project=Bad_Name", "?project=a/b"} {
			req := httptest.NewRequest(http.MethodGet, "/api/resources/samples/x"+q, nil)
			rec := httptest.NewRecorder()
			h(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%q: status %d, want 400", q, rec.Code)
			}
		}
	}
}
