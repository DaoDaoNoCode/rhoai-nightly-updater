package cluster

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
)

func TestHTTPStatusForError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantEC   string
	}{
		{"401", &K8sError{Status: 401}, 401, "unauthorized"},
		{"403 wrapped", fmt.Errorf("fetching DSC list: %w", &K8sError{Status: 403}), 403, "forbidden"},
		{"404", &K8sError{Status: 404}, 404, "not_found"},
		{"429 throttled by API priority and fairness", &K8sError{Status: 429}, 503, "rate_limited"},
		{"500", &K8sError{Status: 500}, 502, "cluster_unavailable"},
		{"503", &K8sError{Status: 503}, 502, "cluster_unavailable"},
		{"409", &K8sError{Status: 409}, 502, "cluster_error"},
		{"network", wrapNetworkError(errors.New("connection refused")), 502, "network"},
		{"timeout inside network error", wrapNetworkError(&url.Error{Op: "Get", URL: "x", Err: context.DeadlineExceeded}), 504, "timeout"},
		{"canceled", fmt.Errorf("x: %w", context.Canceled), 503, "canceled"},
		{"other", errors.New("parse failure"), 500, "internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, ec := HTTPStatusForError(tc.err)
			if code != tc.wantCode || ec != tc.wantEC {
				t.Fatalf("got (%d, %q), want (%d, %q)", code, ec, tc.wantCode, tc.wantEC)
			}
		})
	}
}
