package cluster

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An API response larger than maxResponseBody is an explicit error, not a
// body cut at the limit that later fails as JSON ("unexpected end of JSON
// input"); a response of exactly the limit is still read whole.
func TestClientRefusesAResponseOverTheLimit(t *testing.T) {
	chunk := bytes.Repeat([]byte("a"), 1<<20)
	serve := func(size int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			for left := size; left > 0; left -= len(chunk) {
				w.Write(chunk[:min(left, len(chunk))])
			}
		}))
	}
	for _, tc := range []struct {
		name    string
		size    int
		refused bool
	}{
		{"at the limit", maxResponseBody, false},
		{"over the limit", maxResponseBody + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(tc.size)
			defer srv.Close()
			c := newTestClient(testClientOptions{BaseURL: srv.URL, HTTPClient: srv.Client(), Ctx: context.Background()})
			body, status, err := c.Get("/apis/packages.operators.coreos.com/v1/packagemanifests")
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), "larger than") || body != nil || status != http.StatusOK {
					t.Fatalf("len(body)=%d status=%d err=%v", len(body), status, err)
				}
				return
			}
			if err != nil || len(body) != tc.size {
				t.Fatalf("len(body)=%d err=%v", len(body), err)
			}
		})
	}
}
