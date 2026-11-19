package cluster

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The Dashboard Dev fixtures run where nothing is named as on RHOAI: the
// DSCInitialization names fixtureAppsNS and the dashboard module's operator
// is fixtureOperatorName, so a test passes only if both are discovered.
const (
	fixtureAppsNS       = "custom-apps"
	fixtureOperatorName = "dashboard-module-manager"
	fixtureOperatorSA   = "dashboard-module-sa"
)

// dashboardModuleDiscoveryRoutes are the GET bodies the dashboard operator
// discovery reads on a 3.6-shaped cluster (module_operators_test.go's live
// RBAC for the dashboard module), by path: the component API with the
// Dashboard kind, the DSCInitialization naming appsNS, and a ClusterRole
// that lets ServiceAccount appsNS/sa update dashboards and
// dashboards/finalizers. The operator Deployment itself comes from the
// fixture's Deployment list (dashboardModuleOperatorListItem).
func dashboardModuleDiscoveryRoutes(appsNS, sa string) map[string]string {
	all := []string{"create", "delete", "get", "list", "patch", "update", "watch"}
	roles := map[string]any{"items": []any{
		map[string]any{"metadata": map[string]any{"name": "dashboard-module-role"}, "rules": []any{
			rule(componentsGroup, []string{"dashboards"}, all),
			rule(componentsGroup, []string{"dashboards/finalizers"}, []string{"update"}),
			rule(componentsGroup, []string{"dashboards/status"}, []string{"get", "patch", "update"}),
		}},
		// cluster-admin's wildcards identify no operator.
		map[string]any{"metadata": map[string]any{"name": "cluster-admin"}, "rules": []any{rule([]string{"*"}, []string{"*"}, []string{"*"})}},
	}}
	bindings := map[string]any{"items": []any{
		crb("dashboard-module-rolebinding", "dashboard-module-role", saSubject(appsNS, sa)),
		crb("cluster-admin", "cluster-admin", map[string]any{"kind": "Group", "name": "system:masters"}),
	}}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return string(b)
	}
	return map[string]string{
		"/apis/components.platform.opendatahub.io": `{"kind":"APIGroup","name":"components.platform.opendatahub.io","versions":[{"groupVersion":"components.platform.opendatahub.io/v1alpha1","version":"v1alpha1"}],"preferredVersion":{"groupVersion":"components.platform.opendatahub.io/v1alpha1","version":"v1alpha1"}}`,
		"/apis/components.platform.opendatahub.io/v1alpha1": `{"resources":[{"name":"dashboards","kind":"Dashboard","namespaced":false,"verbs":["get","list","patch","update"]},{"name":"dashboards/status","kind":"Dashboard","namespaced":false,"verbs":["get","patch","update"]}]}`,
		"/apis/dscinitialization.opendatahub.io":                     `{"kind":"APIGroup","name":"dscinitialization.opendatahub.io","versions":[{"groupVersion":"dscinitialization.opendatahub.io/v2","version":"v2"}],"preferredVersion":{"groupVersion":"dscinitialization.opendatahub.io/v2","version":"v2"}}`,
		"/apis/dscinitialization.opendatahub.io/v2/dscinitializations": fmt.Sprintf(`{"items":[{"metadata":{"name":"default-dsci"},"spec":{"applicationsNamespace":%q,"monitoring":{"namespace":"custom-monitoring"}}}]}`, appsNS),
		"/apis/rbac.authorization.k8s.io/v1/clusterroles":             enc(roles),
		"/apis/rbac.authorization.k8s.io/v1/clusterrolebindings":      enc(bindings),
	}
}

// dashboardModuleOperatorListItem is operator as the Deployment list shows
// it: owned by Platform/default and running as sa.
func dashboardModuleOperatorListItem(operator dashboardDeployment, sa string) map[string]any {
	raw, _ := json.Marshal(operator)
	var item map[string]any
	_ = json.Unmarshal(raw, &item)
	meta := item["metadata"].(map[string]any)
	meta["ownerReferences"] = []any{map[string]any{"apiVersion": "config.opendatahub.io/v1alpha1", "kind": "Platform", "name": "default", "controller": true}}
	spec, _ := item["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
		item["spec"] = spec
	}
	tmpl, _ := spec["template"].(map[string]any)
	if tmpl == nil {
		tmpl = map[string]any{}
		spec["template"] = tmpl
	}
	podSpec, _ := tmpl["spec"].(map[string]any)
	if podSpec == nil {
		podSpec = map[string]any{}
		tmpl["spec"] = podSpec
	}
	podSpec["serviceAccountName"] = sa
	return item
}

// serveDashboardModule serves, on a fakeAPI, the discovery of a dashboard
// module whose operator is Deployment appsNS/name (body: its JSON), plus
// the applications namespace's Deployment list with it and others.
func serveDashboardModule(f *fakeAPI, appsNS, name, body string, others ...map[string]any) {
	for path, b := range dashboardModuleDiscoveryRoutes(appsNS, fixtureOperatorSA) {
		f.json("GET", path, 200, b)
	}
	var op dashboardDeployment
	_ = json.Unmarshal([]byte(body), &op)
	op.Metadata.Name = name
	if op.Metadata.UID == "" {
		op.Metadata.UID = "operator-uid"
	}
	items := append([]any{dashboardModuleOperatorListItem(op, fixtureOperatorSA)}, anySlice(others)...)
	f.obj("/apis/apps/v1/namespaces/"+appsNS+"/deployments", map[string]any{"items": items})
	f.json("GET", namespacedPath("apps/v1", "deployments", appsNS, name), 200, body)
}

func anySlice(ms []map[string]any) []any {
	out := make([]any, len(ms))
	for i, m := range ms {
		out[i] = m
	}
	return out
}

// legacyClusterRoute answers, for a hand-written test server of a cluster
// without a dashboard module (RHOAI 3.4 and older), the reads that tell the
// flows apart: the component API is not served, the DSCInitialization names
// redhat-ods-applications, and the installed CSV installs Deployment
// rhods-operator (which holds the legacy flow's RELATED_IMAGE_* values).
// It returns false for every other request.
func legacyClusterRoute(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	switch {
	case r.Method != http.MethodGet:
		return false
	case p == "/apis/"+componentAPIGroup || strings.HasPrefix(p, "/apis/"+componentAPIGroup+"/"),
		p == "/apis/dscinitialization.opendatahub.io":
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"kind":"Status","status":"Failure","reason":"NotFound","code":404}`)
	case p == dsciV2List:
		_, _ = io.WriteString(w, defaultDSCIList)
	case p == namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName):
		_, _ = io.WriteString(w, `{"status":{"installedCSV":"rhods-operator.3.4.0"}}`)
	case p == namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "rhods-operator.3.4.0"):
		_, _ = io.WriteString(w, `{"metadata":{"name":"rhods-operator.3.4.0"},"spec":{"version":"3.4.0","install":{"spec":{"deployments":[{"name":"rhods-operator"}]}}},"status":{"phase":"Succeeded"}}`)
	default:
		return false
	}
	return true
}

// defaultDSCIList is a DSCInitialization list naming RHOAI's default
// applications namespace, for mocks written against it.
const defaultDSCIList = `{"items":[{"metadata":{"name":"default-dsci"},"spec":{"applicationsNamespace":"redhat-ods-applications"}}]}`

// serveDSCI serves a DSCInitialization naming appsNS.
func serveDSCI(f *fakeAPI, appsNS string) {
	f.json("GET", dsciV2List, 200, fmt.Sprintf(`{"items":[{"metadata":{"name":"default-dsci"},"spec":{"applicationsNamespace":%q}}]}`, appsNS))
}
