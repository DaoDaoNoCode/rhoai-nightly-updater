package cluster

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

const platformsPath = "/apis/config.opendatahub.io/v1alpha1/platforms"

func TestPlatformModules_AdminAckAndRunlevelTimeout(t *testing.T) {
	f, c := newFakeAPI(t)
	f.json("GET", platformsPath, http.StatusOK, `{"items":[{"metadata":{"name":"default"},"status":{"phase":"Not Ready","conditions":[
		{"type":"ProvisioningProgress","status":"False","reason":"AdminAckRequired","message":"gate kserve-v2 requires acknowledgement"}]}}]}`)
	f.json("GET", "/api/v1/namespaces/redhat-ods-operator/configmaps/odh-upgrade-acks", http.StatusOK, `{"data":{"kserve-v2":"false","old":"true"}}`)
	out := checkPlatformModules(c)
	if out.check.Status != "fail" || len(out.problems) != 1 || out.problems[0].ID != "platform-admin-ack-required" || out.problems[0].AutoFixable {
		t.Fatalf("out = %+v", out)
	}
	ev := strings.Join(out.problems[0].Evidence, "\n")
	if !strings.Contains(ev, `kserve-v2="false"`) || strings.Contains(ev, "old=") {
		t.Fatalf("evidence = %s", ev)
	}

	f2, c2 := newFakeAPI(t)
	f2.json("GET", platformsPath, http.StatusOK, `{"items":[{"metadata":{"name":"default"},"status":{"conditions":[
		{"type":"ProvisioningProgress","status":"False","reason":"RunlevelTimeoutExceeded","message":"ray not ready after 10m"}]}}]}`)
	out = checkPlatformModules(c2)
	if out.check.Status != "warn" || len(out.problems) != 1 || out.problems[0].ID != "platform-runlevel-timeout" {
		t.Fatalf("out = %+v", out)
	}

	f3, c3 := newFakeAPI(t)
	f3.status("GET", platformsPath, http.StatusForbidden, "Forbidden")
	out = checkPlatformModules(c3)
	if out.check.Status != "warn" || !strings.Contains(out.check.Detail, "RBAC") {
		t.Fatalf("out = %+v", out)
	}
}

// A passing check must not read like a failure: the module result comes
// first, and a non-Ready Platform phase is labelled as the Platform's state.
func TestPlatformModules_PassDetailLeadsWithTheModuleResult(t *testing.T) {
	f, c := newFakeAPI(t)
	serveComponentGroup(f)
	f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1", http.StatusOK, `{"resources":[{"name":"rays","kind":"Ray"}]}`)
	f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1/rays", http.StatusOK, `{"items":[]}`)
	f.json("GET", platformsPath, http.StatusOK, `{"items":[{"metadata":{"name":"default"},"status":{"phase":"Not Ready"}}]}`)
	out := checkPlatformModules(c)
	want := "No module is stuck in deletion; Platform default phase: Not Ready (the DataScienceCluster check shows why)"
	if out.check.Status != "pass" || out.check.Detail != want {
		t.Fatalf("status %q, detail %q; want pass, %q", out.check.Status, out.check.Detail, want)
	}

	f2, c2 := newFakeAPI(t)
	serveComponentGroup(f2)
	f2.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1", http.StatusOK, `{"resources":[]}`)
	f2.json("GET", platformsPath, http.StatusOK, `{"items":[{"metadata":{"name":"default"},"status":{"phase":"Ready"}}]}`)
	out = checkPlatformModules(c2)
	if want := "No module is stuck in deletion; Platform default phase: Ready"; out.check.Detail != want {
		t.Fatalf("detail %q, want %q", out.check.Detail, want)
	}
}

func TestPlatformModules_StuckDashboardWithPausedOperator(t *testing.T) {
	f, c := newFakeAPI(t)
	serveComponentGroup(f)
	f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1", http.StatusOK,
		`{"resources":[{"name":"dashboards","kind":"Dashboard"},{"name":"rays","kind":"Ray"}]}`)
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	recent := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1/dashboards", http.StatusOK,
		`{"items":[{"metadata":{"name":"default-dashboard","deletionTimestamp":"`+old+`","finalizers":["components.platform.opendatahub.io/cleanup"]}}]}`)
	f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1/rays", http.StatusOK,
		`{"items":[{"metadata":{"name":"default-ray","deletionTimestamp":"`+recent+`","finalizers":["x"]}}]}`)
	f.json("GET", "/apis/apps/v1/namespaces/redhat-ods-applications/deployments/dashboard-operator", http.StatusOK, `{"spec":{"replicas":0},"status":{"readyReplicas":0}}`)

	out := checkPlatformModules(c)
	if len(out.problems) != 1 {
		t.Fatalf("problems = %+v", out.problems)
	}
	p := out.problems[0]
	if p.ID != "module-stuck-deleting-dashboard" || p.AutoFixable || !strings.Contains(p.Fix, "Revert") || !strings.Contains(strings.Join(p.Evidence, " "), "no ready pod") {
		t.Fatalf("problem = %+v", p)
	}
	assertWrites(t, f)
}

func TestManagedConfig_ReportsUnmanagedAndLeftovers(t *testing.T) {
	f, c := newFakeAPI(t)
	f.json("GET", dscV2List, http.StatusOK, `{"items":[{"metadata":{"name":"default-dsc"}}]}`)
	f.json("GET", dscV2List+"/default-dsc", http.StatusOK, `{"status":{"release":{"version":"3.6.0"}}}`)
	f.json("GET", "/apis/apps/v1/namespaces/redhat-ods-applications/deployments", http.StatusOK, `{"items":[
		{"metadata":{"name":"mlflow-operator-controller-manager","annotations":{"opendatahub.io/managed":"false","platform.opendatahub.io/version":"3.6.0-ea.1"}},"spec":{"replicas":1},"status":{"replicas":1,"updatedReplicas":1,"readyReplicas":1}},
		{"metadata":{"name":"odh-model-controller","annotations":{"platform.opendatahub.io/version":"3.6.0"}},"spec":{"replicas":1},"status":{"replicas":1,"updatedReplicas":1,"readyReplicas":1}},
		{"metadata":{"name":"kuberay-operator","annotations":{"platform.opendatahub.io/version":"3.6.0-ea.1","platform.opendatahub.io/instance.name":"default-ray"}},"spec":{"replicas":1},"status":{"replicas":1,"updatedReplicas":1,"readyReplicas":1}},
		{"metadata":{"name":"kserve-old","annotations":{"platform.opendatahub.io/version":"3.5.1","platform.opendatahub.io/instance.name":"default"}},"spec":{"replicas":1},"status":{"replicas":1,"updatedReplicas":1,"readyReplicas":1}},
		{"metadata":{"name":"odh-observability","generation":2,"annotations":{"`+assistRolloutAnnotation+`":"{\"maxUnavailable\":\"25%\"}"}},
		 "spec":{"replicas":1,"strategy":{"rollingUpdate":{"maxUnavailable":1}}},"status":{"observedGeneration":2,"replicas":1,"updatedReplicas":1,"readyReplicas":1}}]}`)
	f.json("GET", "/apis/apps/v1/namespaces/redhat-ods-operator/deployments", http.StatusOK, `{"items":[]}`)

	resp := checkManagedConfig(c)
	ids := map[string]Problem{}
	for _, p := range resp.problems {
		ids[p.ID] = p
	}
	un, ok := ids["deployment-unmanaged-mlflow-operator-controller-manager"]
	if !ok || un.AutoFixable || !strings.Contains(strings.Join(un.Evidence, " "), "3.6.0-ea.1") {
		t.Fatalf("problems = %+v", resp.problems)
	}
	if _, ok := ids["deployment-version-drift-kserve-old"]; !ok {
		t.Fatalf("missing drift: %+v", resp.problems)
	}
	if _, ok := ids["deployment-version-drift-kuberay-operator"]; ok {
		t.Fatal("module-operator version reported as platform drift")
	}
	if _, ok := ids["deployment-version-drift-odh-model-controller"]; ok {
		t.Fatal("current version reported as drift")
	}
	if _, ok := ids["deployment-version-drift-mlflow-operator-controller-manager"]; ok {
		t.Fatal("unmanaged deployment reported twice")
	}
	r := ids["rollout-strategy-patched-odh-observability"]
	if !r.AutoFixable || r.AutoFixAction != "restore-rollout-strategy:redhat-ods-applications/odh-observability" {
		t.Fatalf("restore problem = %+v", r)
	}
	if resp.check.Status != "fail" {
		t.Fatalf("check = %+v", resp.check)
	}
}

func TestChannelHeadBehind(t *testing.T) {
	f, c := newFakeAPI(t)
	f.json("GET", "/apis/packages.operators.coreos.com/v1/namespaces/openshift-marketplace/packagemanifests", http.StatusOK,
		`{"items":[{"metadata":{"name":"rhods-operator"},"status":{"packageName":"rhods-operator","catalogSource":"redhat-operators","catalogSourceNamespace":"openshift-marketplace","channels":[{"name":"stable-3.x","currentCSV":"rhods-operator.3.5.1"},{"name":"fast","currentCSV":"rhods-operator.3.7.0"}]}}]}`)
	sub := subscriptionState{Exists: true, Source: "redhat-operators", Channel: "stable-3.x", InstalledCSV: "rhods-operator.3.6.0"}
	if p := channelHeadBehind(c, sub); p == nil || p.ID != "subscription-channel-behind" {
		t.Fatalf("problem = %+v", p)
	}
	sub.Channel = "fast"
	if p := channelHeadBehind(c, sub); p != nil {
		t.Fatalf("newer head reported: %+v", p)
	}
}
