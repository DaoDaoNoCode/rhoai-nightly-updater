package cluster

import (
	"encoding/json"
	"testing"
)

func llmisvcWithAddresses(t *testing.T, ns, name string, urls ...string) llmisvcObject {
	t.Helper()
	addrs := make([]map[string]string, 0, len(urls))
	for _, u := range urls {
		addrs = append(addrs, map[string]string{"url": u})
	}
	raw, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"namespace": ns, "name": name},
		"status":   map[string]any{"addresses": addrs},
	})
	var m llmisvcObject
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
		if got := modelInClusterURL(m); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
