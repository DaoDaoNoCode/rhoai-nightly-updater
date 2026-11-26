package features

// The Kubernetes requests the RHOAI dashboard's gen-ai API makes with the
// caller's token.
//
// The playground's status, set up and delete go through the dashboard's
// gen-ai API (aaa/models, lsd/status, lsd/install, lsd/delete) with the
// signed-in user's own token, and the API makes its Kubernetes requests
// with that same token. The RBAC scan sees only the HTTP call, so the
// requests are listed here by hand, each with its place in the gen-ai
// API's source at GenAISource: the project actions' preflight (pkg/cluster
// project_actions.go) checks the set up's and the delete's before anything
// is sent. Set up as this tool runs it: enable_tracing and
// enable_guardrails false, no vector stores (no pgvector).

// GenAISource is the gen-ai API source the requests and their lines are
// read from (odh-dashboard packages/gen-ai/bff/internal/integrations/
// kubernetes/token_k8s_client.go). SECURITY.md names the same ref
// (TestHandListsMatchPinnedSource).
const GenAISource = "odh-dashboard rhoai-3.6 @efd01a3 packages/gen-ai/bff/internal/integrations/kubernetes/token_k8s_client.go"

// GenAIRequest is one Kubernetes request of the gen-ai API in the
// project, for the playground calls of Calls.
type GenAIRequest struct {
	Verb, Group, Resource, Name string
	// Calls are the gen-ai API calls that make it ("status": aaa/models
	// and lsd/status; "install": lsd/install; "delete": lsd/delete).
	Calls []string
	// Cite is where the gen-ai API makes it (lines of GenAISource), or
	// how it was established.
	Cite string
}

// GenAIRequests are the gen-ai API's Kubernetes requests in the project.
var GenAIRequests = []GenAIRequest{
	// Every call: the API's own access review (CanListOGXServers).
	{"list", "ogx.io", "ogxservers", "", []string{"status", "install", "delete"}, "CanListOGXServers :558-597; GetOGXServers :645-657, called at :1716"},
	// aaa/models and lsd/status (validated live on RHOAI 3.6 as the
	// minimum, DESIGN.md section 6; api/middleware.go RequireAccessToService).
	{"get", "ogx.io", "ogxservers", "lsd-genai-playground", []string{"status"}, "lsd/status"},
	{"get", "", "configmaps", "gen-ai-aa-custom-model-endpoints", []string{"status"}, "aaa/models: the optional custom endpoints"},
	{"get", "serving.kserve.io", "inferenceservices", "", []string{"status"}, "aaa/models: each model's endpoint (GetAAModels)"},
	{"list", "serving.kserve.io", "inferenceservices", "", []string{"status", "install"}, "aaa/models (GetAAModels); install :1194"},
	{"get", "serving.kserve.io", "llminferenceservices", "", []string{"status"}, "aaa/models: each model's endpoint (GetAAModels)"},
	{"list", "serving.kserve.io", "llminferenceservices", "", []string{"status", "install"}, "aaa/models (GetAAModels); install :1138"},
	// lsd/install.
	{"create", "ogx.io", "ogxservers", "", []string{"install"}, ":2143"},
	{"create", "", "configmaps", "", []string{"install"}, "the run config and the office MIME types ConfigMap :1969, :1981; the CA bundles :2029-2060"},
	{"update", "", "configmaps", "", []string{"install"}, "owner references :2182, :2192; the CA bundles :2029-2060, ensureOGXGatewayCABundle :213-266"},
	{"delete", "", "configmaps", "", []string{"install"}, "its own rollback :1984-2019, :2147-2159"},
	{"get", "", "configmaps", "", []string{"install"}, "the CA bundles :2029-2060, ensureOGXGatewayCABundle :213-266"},
	{"get", "serving.kserve.io", "servingruntimes", "", []string{"install"}, ":1257"},
	{"get", "", "services", "", []string{"install"}, ":2922-2972"},
	{"list", "", "services", "", []string{"install"}, ":2922-2972"},
	// lsd/delete.
	{"delete", "ogx.io", "ogxservers", "", []string{"delete"}, "DeleteOGXServer :3112, delete at :3156"},
}
