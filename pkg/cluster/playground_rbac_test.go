package cluster

import (
	"os"
	"regexp"
	"testing"
)

// genAIDashboardRequests are the Kubernetes requests the RHOAI dashboard's
// gen-ai API makes with the caller's token (this ServiceAccount's) for the
// calls the Playground feature makes, in any data science project
// (odh-dashboard rhoai-3.6 packages/gen-ai/bff: api/middleware.go
// RequireAccessToService, integrations/kubernetes/token_k8s_client.go
// InstallOGXServer, DeleteOGXServer, GetAAModels; validated live on RHOAI
// 3.6 as the minimum, DESIGN.md §6). The pgvector objects and the router CA
// are the dashboard's own ServiceAccount's and are not listed.
var genAIDashboardRequests = []struct {
	verb, group, resource, name string
	why                         string
}{
	{"list", "ogx.io", "ogxservers", "", "the API's access review on every call, and the install's check for an existing server"},
	{"create", "ogx.io", "ogxservers", "", "install"},
	{"get", "ogx.io", "ogxservers", "lsd-genai-playground", "status, and the ownerReferences"},
	{"delete", "ogx.io", "ogxservers", "lsd-genai-playground", "delete"},
	{"create", "", "configmaps", "", "install: the configuration ConfigMaps"},
	{"get", "", "configmaps", "llama-stack-config", "install"},
	{"update", "", "configmaps", "llama-stack-config", "install: the ownerReference"},
	{"delete", "", "configmaps", "llama-stack-config", "an ownerReference needs delete on the object"},
	{"get", "", "configmaps", "ogx-office-mime-types", "install"},
	{"update", "", "configmaps", "ogx-office-mime-types", "install: the ownerReference"},
	{"delete", "", "configmaps", "ogx-office-mime-types", "an ownerReference needs delete on the object"},
	{"get", "", "configmaps", "ogx-router-ca-bundle", "install"},
	{"update", "", "configmaps", "ogx-router-ca-bundle", "install: the ownerReference"},
	{"delete", "", "configmaps", "ogx-router-ca-bundle", "an ownerReference needs delete on the object"},
	{"get", "", "configmaps", "ogx-trusted-ca-bundle", "install, without odh-trusted-ca-bundle"},
	{"update", "", "configmaps", "ogx-trusted-ca-bundle", "install: the ownerReference"},
	{"delete", "", "configmaps", "ogx-trusted-ca-bundle", "an ownerReference needs delete on the object"},
	{"get", "", "configmaps", "odh-trusted-ca-bundle", "install: the cluster's CA bundle"},
	{"update", "", "configmaps", "odh-trusted-ca-bundle", "install: the label ogx.io/watch=true the OGX operator's cache needs"},
	{"get", "", "configmaps", "gen-ai-aa-custom-model-endpoints", "aaa/models: the optional custom endpoints"},
	{"get", "serving.kserve.io", "inferenceservices", "", "aaa/models and install: each model's endpoint"},
	{"list", "serving.kserve.io", "inferenceservices", "", "aaa/models"},
	{"get", "serving.kserve.io", "llminferenceservices", "", "aaa/models and install: each model's endpoint"},
	{"list", "serving.kserve.io", "llminferenceservices", "", "aaa/models"},
}

func playgroundServerRequests() []rbacRequest {
	var out []rbacRequest
	for _, r := range genAIDashboardRequests {
		out = append(out, rbacRequest{Verb: r.verb, Group: r.group, Resource: r.resource, Namespace: anyNS, Name: r.name, Site: "dashboard gen-ai API: " + r.why})
	}
	return out
}

// The session token oauth-proxy passes keeps its default scope (user:info
// user:check-access): it identifies the user and runs access reviews, and
// cannot act. A --scope flag would put a full token in every session
// cookie; the Playground calls the dashboard with the ServiceAccount's
// token instead. Only deploy/template.yaml is read: the image build copies
// just that file, and scripts/test-chart.sh (CI's chart job) fails when the
// chart's copy differs from it.
func TestTemplateOAuthProxyKeepsTheDefaultScope(t *testing.T) {
	for _, path := range []string{"../../deploy/template.yaml"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if regexp.MustCompile(`(?m)^\s*-\s*--scope[= ]`).Match(data) {
			t.Errorf("%s sets oauth-proxy's --scope; the session token must stay user:info user:check-access", path)
		}
		if !regexp.MustCompile(`(?m)^\s*-\s*--pass-access-token=true`).Match(data) {
			t.Errorf("%s: oauth-proxy no longer passes the session token the app identifies users with", path)
		}
	}
}
