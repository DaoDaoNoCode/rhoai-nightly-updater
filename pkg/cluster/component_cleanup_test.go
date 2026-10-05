package cluster

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestCleanupStuckComponentCRs_UsesDiscovery(t *testing.T) {
	var mu sync.Mutex
	var patched []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case r.Method == http.MethodPatch:
			mu.Lock()
			patched = append(patched, strings.TrimPrefix(p, "/apis/components.platform.opendatahub.io/v1alpha1/"))
			mu.Unlock()
			fmt.Fprint(w, `{}`)
		case p == "/apis/components.platform.opendatahub.io":
			fmt.Fprint(w, `{"preferredVersion":{"groupVersion":"components.platform.opendatahub.io/v1alpha1"}}`)
		case p == "/apis/components.platform.opendatahub.io/v1alpha1":
			fmt.Fprint(w, `{"resources":[
				{"name":"aipipelines","verbs":["delete","get","list","patch","watch"]},
				{"name":"aipipelines/status","verbs":["get","patch","update"]},
				{"name":"kserves","verbs":["get","list","patch"]},
				{"name":"readonly","verbs":["get","list"]}]}`)
		case strings.HasSuffix(p, "/aipipelines"):
			fmt.Fprint(w, `{"items":[
				{"metadata":{"name":"stuck","finalizers":["platform.opendatahub.io/finalizer"],"deletionTimestamp":"2026-10-01T00:00:00Z"}},
				{"metadata":{"name":"live","finalizers":["platform.opendatahub.io/finalizer"]}},
				{"metadata":{"name":"deleting-no-finalizer","deletionTimestamp":"2026-10-01T00:00:00Z"}}]}`)
		case strings.HasSuffix(p, "/kserves"):
			w.WriteHeader(500)
			fmt.Fprint(w, `{"kind":"Status","code":500}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: context.Background()}

	n, warnings := cleanupStuckComponentCRs(c)
	if n != 1 || fmt.Sprint(patched) != "[aipipelines/stuck]" {
		t.Fatalf("unstuck=%d patched=%v", n, patched)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "kserves") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestCleanupStuckComponentCRs_NoComponentAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		fmt.Fprint(w, `{"kind":"Status","code":404}`)
	}))
	defer srv.Close()
	c := &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: context.Background()}
	if n, warnings := cleanupStuckComponentCRs(c); n != 0 || len(warnings) != 0 {
		t.Fatalf("fresh cluster: unstuck=%d warnings=%v", n, warnings)
	}
}
