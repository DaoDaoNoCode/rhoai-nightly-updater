package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func llmisvcWithAddresses(t *testing.T, ns, name string, urls ...string) serving.LLMISvcObject {
	t.Helper()
	addrs := make([]map[string]string, 0, len(urls))
	for _, u := range urls {
		addrs = append(addrs, map[string]string{"url": u})
	}
	raw, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"namespace": ns, "name": name},
		"status":   map[string]any{"addresses": addrs},
	})
	var m serving.LLMISvcObject
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// RHOAI 3.6 (live, 2026-11-18) lists the gateway's routing bases first; the
// in-cluster address is the model's own path on the gateway's internal
// Service, not the first in-cluster address.
func TestModelInClusterURL(t *testing.T) {
	const ext = "https://inference.apps.example.com"
	const gwSvc = "https://openshift-ai-inference-openshift-ai-inference.openshift-ingress.svc.cluster.local"
	for _, c := range []struct {
		name string
		urls []string
		want string
	}{
		{"on a gateway (RHOAI 3.6 order)", []string{
			ext + "/", ext + "/e2e-400/smollm2-135m-instruct", ext + "/publishers/e2e-400/models/smollm2-135m-instruct",
			gwSvc + "/", gwSvc + "/e2e-400/smollm2-135m-instruct", gwSvc + "/publishers/e2e-400/models/smollm2-135m-instruct",
		}, gwSvc + "/e2e-400/smollm2-135m-instruct"},
		{"no route: the model's own Service", []string{"https://smollm2-135m-instruct-kserve-workload-svc.e2e-400.svc.cluster.local"},
			"https://smollm2-135m-instruct-kserve-workload-svc.e2e-400.svc.cluster.local"},
		{"only routing bases", []string{ext + "/", gwSvc + "/"}, ""},
		{"another project's path", []string{gwSvc + "/other/smollm2-135m-instruct"}, ""},
		{"another model's Service", []string{"https://qwen-kserve-workload-svc.e2e-400.svc.cluster.local"}, ""},
		{"not https", []string{"http://smollm2-135m-instruct-kserve-workload-svc.e2e-400.svc.cluster.local"}, ""},
	} {
		m := llmisvcWithAddresses(t, "e2e-400", "smollm2-135m-instruct", c.urls...)
		if got := serving.ModelInClusterURL(m); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// The model's own Service is reported without its port; RHOAI 3.6 serves on
// 8000 (live: the address as reported does not connect). The port is read
// from the Service; a gateway path, a known port or 443 stay as they are,
// and an unreadable Service gives no address rather than a wrong one.
func TestWithServicePort(t *testing.T) {
	services := map[string]string{
		"/api/v1/namespaces/e2e-400/services/smollm2-135m-instruct-kserve-workload-svc": `{"spec":{"ports":[{"name":"metrics","port":9090},{"name":"https","appProtocol":"https","port":8000}]}}`,
		"/api/v1/namespaces/e2e-400/services/only-one":                                  `{"spec":{"ports":[{"name":"web","port":8443}]}}`,
		"/api/v1/namespaces/e2e-400/services/on-443":                                    `{"spec":{"ports":[{"name":"https","port":443}]}}`,
		"/api/v1/namespaces/e2e-400/services/two-plain":                                 `{"spec":{"ports":[{"name":"a","port":1},{"name":"b","port":2}]}}`,
		"/api/v1/namespaces/e2e-400/services/plain-http":                                `{"spec":{"ports":[{"name":"metrics","port":9090},{"name":"http","port":8080}]}}`,
		"/api/v1/namespaces/e2e-400/services/on-80":                                     `{"spec":{"ports":[{"name":"http","port":80}]}}`,
	}
	reads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		if b, ok := services[r.URL.Path]; ok && r.Method == http.MethodGet {
			_, _ = fmt.Fprint(w, b)
			return
		}
		w.WriteHeader(404)
		_, _ = fmt.Fprint(w, `{"kind":"Status","code":404}`)
	}))
	defer srv.Close()
	c := newTestClient(testClientOptions{BaseURL: srv.URL, HTTPClient: srv.Client(), Ctx: context.Background()})
	for _, x := range []struct{ in, want string }{
		{"https://smollm2-135m-instruct-kserve-workload-svc.e2e-400.svc.cluster.local", "https://smollm2-135m-instruct-kserve-workload-svc.e2e-400.svc.cluster.local:8000"},
		{"https://only-one.e2e-400.svc", "https://only-one.e2e-400.svc:8443"},
		{"https://on-443.e2e-400.svc.cluster.local", "https://on-443.e2e-400.svc.cluster.local"},
		{"https://two-plain.e2e-400.svc.cluster.local", ""},
		{"https://gone.e2e-400.svc.cluster.local", ""},
		{"https://gw.openshift-ingress.svc.cluster.local/e2e-400/m", "https://gw.openshift-ingress.svc.cluster.local/e2e-400/m"},
		{"https://x.e2e-400.svc.cluster.local:9443", "https://x.e2e-400.svc.cluster.local:9443"},
		{"", ""},
		// An http address takes the Service's http port; 80 is its default.
		{"http://plain-http.e2e-400.svc.cluster.local", "http://plain-http.e2e-400.svc.cluster.local:8080"},
		{"http://on-80.e2e-400.svc.cluster.local", "http://on-80.e2e-400.svc.cluster.local"},
	} {
		if got := serving.WithServicePort(c, x.in); got != x.want {
			t.Errorf("withServicePort(%q) = %q, want %q", x.in, got, x.want)
		}
	}
	if reads != 7 {
		t.Errorf("read %d Services, want 7 (only port-less Service addresses)", reads)
	}
	// A Service that cannot be read is an error for a check (fail closed);
	// one that names no port of the scheme is none.
	if got, err := serving.ServicePortAddress(c, "https://gone.e2e-400.svc.cluster.local"); got != "" || err == nil {
		t.Errorf("unreadable Service: %q, %v", got, err)
	}
	if got, err := serving.ServicePortAddress(c, "https://two-plain.e2e-400.svc.cluster.local"); got != "" || err != nil {
		t.Errorf("no https port: %q, %v", got, err)
	}
}

// A model with no route has no outside address, though KServe keeps
// status.url from the route it removed (live, after a MaaS-only unpublish).
func TestAddModelRouting_NoRouteHasNoOutsideAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces/p/services/m-kserve-workload-svc" {
			_, _ = fmt.Fprint(w, `{"spec":{"ports":[{"name":"https","port":8000}]}}`)
			return
		}
		w.WriteHeader(404)
		_, _ = fmt.Fprint(w, `{"kind":"Status","code":404}`)
	}))
	defer srv.Close()
	c := newTestClient(testClientOptions{BaseURL: srv.URL, HTTPClient: srv.Client(), Ctx: context.Background()})
	m := llmisvcWithAddresses(t, "p", "m", "https://m-kserve-workload-svc.p.svc.cluster.local")
	m.Status.URL = "https://maas.apps.example.com/p/m"
	s := types.ModelState{}
	s.URL = m.Status.URL
	serving.AddModelRouting(c, &s, m, serving.KServeGateway{}, map[string]string{})
	if s.Exposure != serving.ExposureInternal || s.URL != "" || s.InternalURL != "https://m-kserve-workload-svc.p.svc.cluster.local:8000" {
		t.Fatalf("exposure %q, url %q, internal %q", s.Exposure, s.URL, s.InternalURL)
	}
}
