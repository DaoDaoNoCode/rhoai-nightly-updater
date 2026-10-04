package cluster

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func verificationCatalogServer(t *testing.T, state, channels string) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/catalogsources/") && r.Method == http.MethodGet:
			fmt.Fprintf(w, `{"status":{"connectionState":{"lastObservedState":%q}}}`, state)
		case strings.HasSuffix(r.URL.Path, "/packagemanifests"):
			source := strings.TrimPrefix(r.URL.Query().Get("labelSelector"), "catalog=")
			fmt.Fprintf(w, `{"items":[{"metadata":{"name":"rhods-operator"},"status":{"packageName":"rhods-operator","catalogSource":%q,"catalogSourceNamespace":"openshift-marketplace","channels":%s}}]}`, source, channels)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: context.Background()}
}

func TestPreflightReinstallCatalog_ExplainsFailure(t *testing.T) {
	image := "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5"
	channels := `[{"name":"stable-3.4","currentCSV":"rhods-operator.3.4.0"},{"name":"beta","currentCSV":"rhods-operator.3.4.0"}]`
	cases := []struct {
		name, state, override, want string
	}{
		{"not ready", "CONNECTING", "", "catalog state is CONNECTING"},
		{"missing override", "READY", "fast", `channel "fast" is not in the catalog (available: beta, stable-3.4)`},
		{"no matching release", "READY", "", "no channel matches the image's release (available: beta, stable-3.4)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := preflightReinstallCatalog(verificationCatalogServer(t, tc.state, channels), image, tc.override)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "<nil>") {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestPreflightReinstallCatalog_ReturnsOverride(t *testing.T) {
	channels := `[{"name":"fast","currentCSV":"rhods-operator.3.5.0"}]`
	ch, err := preflightReinstallCatalog(verificationCatalogServer(t, "READY", channels), "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5", "fast")
	if err != nil || ch != "fast" {
		t.Fatalf("got %q, %v", ch, err)
	}
}
