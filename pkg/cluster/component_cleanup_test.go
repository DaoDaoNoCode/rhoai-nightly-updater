package cluster

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A finalizer is only removed when the CR has been deleting for a while AND
// the module operator that owns it is gone (RHOAI 3.6 module operators keep
// running while rhods-operator is reinstalled).
func TestCleanupStuckComponentCRs_OnlyWhenOwnerOperatorGone(t *testing.T) {
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	recent := time.Now().UTC().Format(time.RFC3339)
	var mu sync.Mutex
	var patched []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case r.Method == http.MethodPatch:
			mu.Lock()
			patched = append(patched, strings.TrimPrefix(p, "/apis/components.platform.opendatahub.io/v1alpha1/"))
			mu.Unlock()
			_, _ = fmt.Fprint(w, `{}`)
		case p == "/apis/components.platform.opendatahub.io":
			_, _ = fmt.Fprint(w, `{"preferredVersion":{"groupVersion":"components.platform.opendatahub.io/v1alpha1"}}`)
		case p == "/apis/components.platform.opendatahub.io/v1alpha1":
			_, _ = fmt.Fprint(w, `{"resources":[
				{"name":"aipipelines","verbs":["delete","get","list","patch","watch"]},
				{"name":"aipipelines/status","verbs":["get","patch","update"]},
				{"name":"dashboards","verbs":["get","list","patch"]},
				{"name":"kueues","verbs":["get","list","patch"]},
				{"name":"kserves","verbs":["get","list","patch"]},
				{"name":"readonly","verbs":["get","list"]}]}`)
		case strings.HasSuffix(p, "/aipipelines"):
			_, _ = fmt.Fprintf(w, `{"items":[
				{"metadata":{"name":"stuck","finalizers":["f"],"deletionTimestamp":%q}},
				{"metadata":{"name":"just-deleted","finalizers":["f"],"deletionTimestamp":%q}},
				{"metadata":{"name":"live","finalizers":["f"]}},
				{"metadata":{"name":"deleting-no-finalizer","deletionTimestamp":%q}}]}`, old, recent, old)
		case strings.HasSuffix(p, "/dashboards"):
			_, _ = fmt.Fprintf(w, `{"items":[{"metadata":{"name":"default-dashboard","finalizers":["f"],"deletionTimestamp":%q}}]}`, old)
		case strings.HasSuffix(p, "/kueues"):
			_, _ = fmt.Fprintf(w, `{"items":[{"metadata":{"name":"default-kueue","finalizers":["f"],"deletionTimestamp":%q}}]}`, old)
		case strings.HasSuffix(p, "/kserves"):
			w.WriteHeader(500)
			_, _ = fmt.Fprint(w, `{"kind":"Status","code":500}`)
		case strings.HasSuffix(p, "/deployments/dashboard-operator"):
			_, _ = fmt.Fprint(w, `{"metadata":{"name":"dashboard-operator"}}`) // still installed
		case strings.Contains(p, "/deployments/"):
			w.WriteHeader(404)
			_, _ = fmt.Fprint(w, `{"kind":"Status","code":404}`)
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
	text := strings.Join(warnings, "\n")
	for _, want := range []string{"aipipelines/just-deleted is being deleted", "dashboards/default-dashboard has been deleting", "dashboard-operator still exists", "kueues/default-kueue", "is unknown", "list kserves"} {
		if !strings.Contains(text, want) {
			t.Errorf("warnings missing %q:\n%s", want, text)
		}
	}
}

func TestCleanupStuckComponentCRs_NoComponentAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = fmt.Fprint(w, `{"kind":"Status","code":404}`)
	}))
	defer srv.Close()
	c := &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: context.Background()}
	if n, warnings := cleanupStuckComponentCRs(c); n != 0 || len(warnings) != 0 {
		t.Fatalf("fresh cluster: unstuck=%d warnings=%v", n, warnings)
	}
}
