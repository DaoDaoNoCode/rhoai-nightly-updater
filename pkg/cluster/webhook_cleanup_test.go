package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

func webhookConfig(name string, labels map[string]string, owner string, svcNS, svcName string) map[string]interface{} {
	meta := map[string]interface{}{"name": name}
	if labels != nil {
		meta["labels"] = labels
	}
	if owner != "" {
		meta["ownerReferences"] = []interface{}{map[string]interface{}{"apiVersion": owner, "kind": "Kserve", "name": "default-kserve"}}
	}
	return map[string]interface{}{
		"metadata": meta,
		"webhooks": []interface{}{map[string]interface{}{
			"name":         name,
			"clientConfig": map[string]interface{}{"service": map[string]interface{}{"namespace": svcNS, "name": svcName}},
		}},
	}
}

// The configurations mirror the live RHOAI 3.6 cluster: OLM-owned operator
// webhooks plus operand webhooks owned by the Kserve component CR.
func TestCleanupStaleWebhooks_KeepsLiveWebhooks(t *testing.T) {
	olmLabels := func(csv string) map[string]string {
		return map[string]string{"olm.owner": csv, "olm.owner.namespace": SubNS, "olm.owner.kind": "ClusterServiceVersion"}
	}
	validating := []interface{}{
		webhookConfig("datasciencecluster-v2-validator.opendatahub.io-nfzvz", olmLabels("rhods-operator.3.6.0"), "", SubNS, "rhods-operator-service"),
		webhookConfig("dscinitialization-v1-validator.opendatahub.io-old", olmLabels("rhods-operator.3.5.0"), "", SubNS, "rhods-operator-service"),
		webhookConfig("validating.odh-model-controller.opendatahub.io", nil, "components.platform.opendatahub.io/v1alpha1", "redhat-ods-applications", "odh-model-controller-webhook-service"),
		webhookConfig("inferenceservice.serving.kserve.io", nil, "components.platform.opendatahub.io/v1alpha1", "redhat-ods-applications", "gone-kserve-service"),
		webhookConfig("authorino.example.io", map[string]string{"olm.owner": "authorino-operator.v1.4.3", "olm.owner.namespace": "openshift-operators"}, "", "openshift-operators", "gone-authorino"),
		webhookConfig("legacy.opendatahub.io", nil, "", "redhat-ods-applications", "gone-legacy"),
		webhookConfig("legacy-live.opendatahub.io", nil, "", "redhat-ods-applications", "odh-model-controller-webhook-service"),
		webhookConfig("broken-lookup.opendatahub.io", nil, "", "redhat-ods-applications", "error-service"),
	}
	mutating := []interface{}{
		webhookConfig("kuberay-mutating-webhook-configuration", nil, "", "redhat-ods-applications", "gone-kuberay"),
		webhookConfig("mutating.odh-model-controller.opendatahub.io", nil, "components.platform.opendatahub.io/v1alpha1", "redhat-ods-applications", "odh-model-controller-webhook-service"),
		webhookConfig("podmonitor-injector.opendatahub.io-4z47n", olmLabels("rhods-operator.3.6.0"), "", SubNS, "rhods-operator-service"),
	}
	existingServices := map[string]bool{
		SubNS + "/rhods-operator-service":                              true,
		"redhat-ods-applications/odh-model-controller-webhook-service": true,
	}

	var mu sync.Mutex
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case r.Method == http.MethodDelete:
			mu.Lock()
			deleted = append(deleted, p[strings.LastIndex(p, "/")+1:])
			mu.Unlock()
			fmt.Fprint(w, `{}`)
		case strings.HasSuffix(p, "/validatingwebhookconfigurations"):
			json.NewEncoder(w).Encode(map[string]interface{}{"items": validating})
		case strings.HasSuffix(p, "/mutatingwebhookconfigurations"):
			json.NewEncoder(w).Encode(map[string]interface{}{"items": mutating})
		case strings.HasSuffix(p, "/clusterserviceversions/rhods-operator.3.6.0"):
			fmt.Fprint(w, `{"metadata":{"name":"rhods-operator.3.6.0"}}`)
		case strings.Contains(p, "/services/error-service"):
			w.WriteHeader(500)
			fmt.Fprint(w, `{"kind":"Status","code":500}`)
		case strings.Contains(p, "/services/"):
			parts := strings.Split(p, "/")
			if existingServices[parts[4]+"/"+parts[6]] {
				fmt.Fprint(w, `{}`)
				return
			}
			w.WriteHeader(404)
			fmt.Fprint(w, `{"kind":"Status","code":404}`)
		default:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"kind":"Status","code":404}`)
		}
	}))
	defer srv.Close()
	c := &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: context.Background()}

	removed, warnings := removeStaleWebhooks(c)
	sort.Strings(deleted)
	want := []string{"dscinitialization-v1-validator.opendatahub.io-old", "inferenceservice.serving.kserve.io", "legacy.opendatahub.io"}
	if fmt.Sprint(deleted) != fmt.Sprint(want) {
		t.Fatalf("deleted %v, want %v", deleted, want)
	}
	if len(removed) != 3 || len(warnings) != 1 || !strings.Contains(warnings[0], "broken-lookup") {
		t.Fatalf("removed=%v warnings=%v", removed, warnings)
	}
	if !strings.Contains(strings.Join(removed, "\n"), "operator CSV rhods-operator.3.5.0 was removed") {
		t.Fatalf("removal reasons not reported: %v", removed)
	}
}

func TestIsRHOAIWebhook(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]interface{}
		want   bool
	}{
		{"datasciencecluster-v2-validator.opendatahub.io-x", map[string]interface{}{"olm.owner": "rhods-operator.3.6.0", "olm.owner.namespace": SubNS}, true},
		{"anything", map[string]interface{}{"olm.owner": "rhods-operator.3.6.0"}, true},
		{"x.opendatahub.io", map[string]interface{}{"olm.owner": "rhods-operator.3.6.0", "olm.owner.namespace": "elsewhere"}, false},
		{"x.opendatahub.io", map[string]interface{}{"olm.owner": "authorino-operator.v1"}, false},
		{"rhods-thing", map[string]interface{}{"olm.owner": "not-rhods-operator.v1"}, false},
		{"legacy.opendatahub.io", nil, true},
		{"kuberay-mutating-webhook-configuration", nil, false},
	} {
		if got := isRHOAIWebhook(tc.name, tc.labels); got != tc.want {
			t.Errorf("isRHOAIWebhook(%q, %v) = %v, want %v", tc.name, tc.labels, got, tc.want)
		}
	}
}
