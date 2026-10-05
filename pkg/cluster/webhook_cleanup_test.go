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

func cleanupWebhookConfig(name string, labels map[string]string, owner string, svcNS, svcName string) map[string]interface{} {
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
		cleanupWebhookConfig("datasciencecluster-v2-validator.opendatahub.io-nfzvz", olmLabels("rhods-operator.3.6.0"), "", SubNS, "rhods-operator-service"),
		cleanupWebhookConfig("dscinitialization-v1-validator.opendatahub.io-old", olmLabels("rhods-operator.3.5.0"), "", SubNS, "rhods-operator-service"),
		// OLM-owned, Service gone, but the CSV exists: OLM heals it, keep.
		cleanupWebhookConfig("dscinitialization-v2-validator.opendatahub.io-x", olmLabels("rhods-operator.3.6.0"), "", SubNS, "gone-operator-service"),
		cleanupWebhookConfig("validating.odh-model-controller.opendatahub.io", nil, "components.platform.opendatahub.io/v1alpha1", "redhat-ods-applications", "odh-model-controller-webhook-service"),
		cleanupWebhookConfig("inferenceservice.serving.kserve.io", nil, "components.platform.opendatahub.io/v1alpha1", "redhat-ods-applications", "gone-kserve-service"),
		cleanupWebhookConfig("authorino.example.io", map[string]string{"olm.owner": "authorino-operator.v1.4.3", "olm.owner.namespace": "openshift-operators"}, "", "openshift-operators", "gone-authorino"),
		cleanupWebhookConfig("legacy.opendatahub.io", nil, "", "redhat-ods-applications", "gone-legacy"),
		cleanupWebhookConfig("legacy-live.opendatahub.io", nil, "", "redhat-ods-applications", "odh-model-controller-webhook-service"),
		cleanupWebhookConfig("broken-lookup.opendatahub.io", nil, "", "redhat-ods-applications", "error-service"),
	}
	mutating := []interface{}{
		cleanupWebhookConfig("kuberay-mutating-webhook-configuration", nil, "", "redhat-ods-applications", "gone-kuberay"),
		cleanupWebhookConfig("mutating.odh-model-controller.opendatahub.io", nil, "components.platform.opendatahub.io/v1alpha1", "redhat-ods-applications", "odh-model-controller-webhook-service"),
		cleanupWebhookConfig("podmonitor-injector.opendatahub.io-4z47n", olmLabels("rhods-operator.3.6.0"), "", SubNS, "rhods-operator-service"),
	}
	existingServices := map[string]bool{
		SubNS + "/rhods-operator-service":                              true,
		"redhat-ods-applications/odh-model-controller-webhook-service": true,
	}

	for _, tc := range []struct {
		name     string
		csvPhase string
		want     []string
		warnings int
	}{
		{"operator settled", "Succeeded", []string{"dscinitialization-v1-validator.opendatahub.io-old", "inferenceservice.serving.kserve.io", "legacy.opendatahub.io"}, 1},
		// While an install is running only configs of removed CSVs go.
		{"operator installing", "Installing", []string{"dscinitialization-v1-validator.opendatahub.io-old"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var deleted []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				p := r.URL.Path
				switch {
				case r.Method == http.MethodDelete:
					mu.Lock()
					deleted = append(deleted, p[strings.LastIndex(p, "/")+1:])
					mu.Unlock()
					_, _ = fmt.Fprint(w, `{}`)
				case strings.HasSuffix(p, "/validatingwebhookconfigurations"):
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"items": validating})
				case strings.HasSuffix(p, "/mutatingwebhookconfigurations"):
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"items": mutating})
				case strings.HasSuffix(p, "/clusterserviceversions/rhods-operator.3.6.0"):
					_, _ = fmt.Fprint(w, `{"metadata":{"name":"rhods-operator.3.6.0"}}`)
				case strings.HasSuffix(p, "/clusterserviceversions"):
					_, _ = fmt.Fprintf(w, `{"items":[{"metadata":{"name":"rhods-operator.3.6.0"},"status":{"phase":%q}}]}`, tc.csvPhase)
				case strings.Contains(p, "/services/error-service"):
					w.WriteHeader(500)
					_, _ = fmt.Fprint(w, `{"kind":"Status","code":500}`)
				case strings.Contains(p, "/services/"):
					parts := strings.Split(p, "/")
					if existingServices[parts[4]+"/"+parts[6]] {
						_, _ = fmt.Fprint(w, `{}`)
						return
					}
					w.WriteHeader(404)
					_, _ = fmt.Fprint(w, `{"kind":"Status","code":404}`)
				default:
					w.WriteHeader(404)
					_, _ = fmt.Fprint(w, `{"kind":"Status","code":404}`)
				}
			}))
			defer srv.Close()
			c := &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: context.Background()}

			removed, warnings := removeStaleWebhooks(c)
			sort.Strings(deleted)
			if fmt.Sprint(deleted) != fmt.Sprint(tc.want) {
				t.Fatalf("deleted %v, want %v", deleted, tc.want)
			}
			if len(removed) != len(tc.want) || len(warnings) != tc.warnings {
				t.Fatalf("removed=%v warnings=%v", removed, warnings)
			}
			if !strings.Contains(strings.Join(removed, "\n"), "operator CSV rhods-operator.3.5.0 was removed") {
				t.Fatalf("removal reasons not reported: %v", removed)
			}
		})
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
