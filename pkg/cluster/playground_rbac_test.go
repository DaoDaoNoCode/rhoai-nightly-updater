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

// oauth-proxy signs users in through the install's own OAuthClient
// (${APP_NAME}-${NAMESPACE}, which install.sh and the chart's hook create)
// with --scope=user:full, and passes that token on: the app makes every
// change with it. A ServiceAccount as the OAuth client cannot get
// user:full (the OAuth server refuses it with "scope denied" after the
// login), so --openshift-service-account and the ServiceAccount's
// oauth-redirectreference annotation must be gone. --cookie-refresh stays
// off: openshift/oauth-proxy validates a refresh with the token in a query
// parameter, which the API server ignores, so it signs users out (and can
// log the token on a network error). Only deploy/template.yaml is read: the
// image build copies just that file, and scripts/test-chart.sh (CI's chart
// job) fails when the chart's copy differs from it. This is the sign-in of
// install.sh and of the chart's full profile; the chart's gitops profile,
// where nothing acts with the user's token, replaces exactly these
// arguments and the client Secret's volume with the ServiceAccount sign-in
// of 3.x (scopes user:info user:check-access): scripts/test-chart.sh pins
// both, and TestGitOpsProfileWorksWithAScopedSignIn (pkg/api) that the
// gitops profile never needs more.
func TestTemplateOAuthProxySignsInThroughTheOAuthClient(t *testing.T) {
	data, err := os.ReadFile(templateRel)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`--client-id=${APP_NAME}-${NAMESPACE}`,
		`--client-secret-file=/etc/proxy/oauth/client-secret`,
		`--scope=user:full`,
		`--pass-access-token=true`,
		`--cookie-secret-file=/etc/proxy/secrets/session_secret`,
		`--cookie-samesite=strict`,
	} {
		if !regexp.MustCompile(`(?m)^\s*-\s*` + regexp.QuoteMeta(want) + `$`).Match(data) {
			t.Errorf("deploy/template.yaml: oauth-proxy does not set %s", want)
		}
	}
	// Comments may name what is gone and why.
	objects := regexp.MustCompile(`(?m)^\s*#.*$`).ReplaceAll(data, nil)
	for _, gone := range []string{"--openshift-service-account", "oauth-redirectreference", "oauth-redirecturi", "--cookie-refresh", "--client-secret="} {
		if regexp.MustCompile(regexp.QuoteMeta(gone)).Match(objects) {
			t.Errorf("deploy/template.yaml still has %s", gone)
		}
	}
	if !regexp.MustCompile(`(?m)^\s*secretName: \$\{APP_NAME\}-oauth-client$`).Match(data) {
		t.Error("deploy/template.yaml: the oauth-proxy container does not mount Secret ${APP_NAME}-oauth-client")
	}
}
