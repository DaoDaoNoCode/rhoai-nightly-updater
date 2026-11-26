package mlflow

import "slices"

// The access reviews MLflow makes for the samples' calls.
//
// MLflow (mlflow-kubernetes-plugins) authorizes every call with access
// reviews of the caller's token in API group mlflow.kubeflow.org, in the
// project's workspace, so a samples change made with a user's token passes
// only when that user's RBAC allows each one. The RBAC scan cannot derive
// them from Kubernetes paths, so they are listed here, each with the
// plugin's source lines at the pinned release (AuthRulesSource): the
// project actions' preflight (pkg/cluster project_actions.go) checks them
// before anything is sent, and the tests' MLflow fake authorizes with the
// same table (samples_fake_test.go), so a call outside it fails a test.

// AuthRulesSource is the plugin release the rules and their lines are
// read from. SECURITY.md names the same release
// (TestHandListsMatchPinnedSource).
const AuthRulesSource = "mlflow-kubernetes-plugins v2.1.1 (9694e137)"

// AuthGroup is the API group of MLflow's access reviews.
const AuthGroup = "mlflow.kubeflow.org"

// AuthCheck is one access review a rule makes.
type AuthCheck struct{ Resource, Verb string }

// AuthRule is the plugin's rule for one route, as the samples call it.
type AuthRule struct {
	Method, Route string
	Checks        []AuthCheck
	// Name is how the plugin resolves the resource name ("" = no name
	// parser, so only the unnamed check counts).
	Name string
	// Names are the sample names the code can send to this route.
	Names []string
	// Cite is the plugin's source lines of the rule.
	Cite string
	// Actions are the samples actions ("add", "remove", "regenerate")
	// whose calls use the route.
	Actions []string
}

// AuthRules lists every MLflow route the samples call, with the
// authorization of the plugin at AuthRulesSource.
var AuthRules = []AuthRule{
	{"GET", "/api/2.0/mlflow/experiments/get-by-name", []AuthCheck{{"experiments", "get"}}, "query:experiment_name", []string{DemoExperiment, TrainingExperiment}, "rules_base.py:323-326", []string{"add", "remove", "regenerate"}},
	{"POST", "/api/2.0/mlflow/experiments/create", []AuthCheck{{"experiments", "create"}}, "", nil, "rules_base.py:319", []string{"add"}},
	{"POST", "/api/2.0/mlflow/experiments/restore", []AuthCheck{{"experiments", "update"}}, "body:experiment_id", []string{DemoExperiment, TrainingExperiment}, "rules_base.py:331-334", []string{"add"}},
	{"POST", "/api/2.0/mlflow/experiments/delete", []AuthCheck{{"experiments", "delete"}}, "body:experiment_id", []string{TrainingExperiment}, "rules_base.py:327-330", []string{"remove"}},
	{"POST", "/api/2.0/mlflow/experiments/set-experiment-tag", []AuthCheck{{"experiments", "update"}}, "body:experiment_id", []string{DemoExperiment}, "rules_base.py:342-345", []string{"add", "regenerate"}},
	{"POST", "/api/2.0/mlflow/experiments/delete-experiment-tag", []AuthCheck{{"experiments", "update"}}, "body:experiment_id", []string{DemoExperiment}, "rules_base.py:347-350", []string{"add"}},
	// SearchRuns: experiments list, else the experiment_ids the caller may
	// get by name (rules_base.py:653-656; collection_filters.py:123-139,
	// 161-174).
	{"POST", "/api/2.0/mlflow/runs/search", []AuthCheck{{"experiments", "list"}}, "body:experiment_ids:get", []string{TrainingExperiment}, "rules_base.py:653-656", []string{"add"}},
	{"POST", "/api/2.0/mlflow/runs/create", []AuthCheck{{"experiments", "update"}}, "body:experiment_id", []string{TrainingExperiment}, "rules_base.py:403-407", []string{"add"}},
	{"POST", "/api/2.0/mlflow/runs/log-batch", []AuthCheck{{"experiments", "update"}}, "body:run_id", []string{TrainingExperiment}, "rules_base.py:428-431", []string{"add"}},
	{"POST", "/api/2.0/mlflow/runs/update", []AuthCheck{{"experiments", "update"}}, "body:run_id", []string{TrainingExperiment}, "rules_base.py:420-423", []string{"add"}},
	{"GET", "/api/2.0/mlflow/registered-models/get", []AuthCheck{{"registeredmodels", "get"}}, "query:name", []string{TrainingModel}, "rules_base.py:659-662", []string{"add", "remove"}},
	{"POST", "/api/2.0/mlflow/registered-models/create", []AuthCheck{{"registeredmodels", "create"}}, "", nil, "rules_base.py:658", []string{"add"}},
	{"DELETE", "/api/2.0/mlflow/registered-models/delete", []AuthCheck{{"registeredmodels", "delete"}}, "body:name", []string{TrainingModel}, "rules_base.py:663-666", []string{"remove"}},
	{"GET", "/api/2.0/mlflow/model-versions/get", []AuthCheck{{"registeredmodels", "get"}}, "query:name", []string{TrainingModel}, "rules_base.py:738-741", []string{"add"}},
	{"POST", "/api/2.0/mlflow/model-versions/create", []AuthCheck{{"registeredmodels", "update"}}, "body:name", []string{TrainingModel}, "rules_base.py:682-685", []string{"add"}},
	{"POST", "/api/2.0/mlflow/registered-models/alias", []AuthCheck{{"registeredmodels", "update"}}, "body:name", []string{TrainingModel}, "rules_base.py:714-717", []string{"add"}},
	{"POST", "/ajax-api/3.0/mlflow/demo/generate", []AuthCheck{{"experiments", "create"}, {"datasets", "create"}, {"registeredmodels", "create"}}, "", nil, "rules_base.py:1178-1182", []string{"add", "regenerate"}},
	{"GET", "/api/3.0/mlflow/mcp-servers/{mcp}", []AuthCheck{{"mcpservers", "get"}}, "path", []string{MCPSampleName}, "rules_base.py:1432-1435", []string{"add", "remove"}},
	{"PATCH", "/api/3.0/mlflow/mcp-servers/{mcp}", []AuthCheck{{"mcpservers", "update"}}, "path", []string{MCPSampleName}, "rules_base.py:1436-1439", []string{"add"}},
	{"DELETE", "/api/3.0/mlflow/mcp-servers/{mcp}", []AuthCheck{{"mcpservers", "delete"}}, "path", []string{MCPSampleName}, "rules_base.py:1440-1443", []string{"remove"}},
	// A version POST auto-creates a missing parent server
	// (sqlalchemy_mixin.py:272-330) and is checked as update on it.
	{"POST", "/api/3.0/mlflow/mcp-servers/{mcp}/versions", []AuthCheck{{"mcpservers", "update"}}, "path", []string{MCPSampleName}, "rules_base.py:1351-1354", []string{"add"}},
	{"GET", "/api/3.0/mlflow/mcp-servers/{mcp}/versions", []AuthCheck{{"mcpservers", "get"}}, "path", []string{MCPSampleName}, "rules_base.py:1355-1358", []string{"add"}},
	{"GET", "/api/3.0/mlflow/mcp-servers/{mcp}/versions/{v}", []AuthCheck{{"mcpservers", "get"}}, "path", []string{MCPSampleName}, "rules_base.py:1359-1362", []string{"add", "remove"}},
	{"DELETE", "/api/3.0/mlflow/mcp-servers/{mcp}/versions/{v}", []AuthCheck{{"mcpservers", "update"}}, "path", []string{MCPSampleName}, "rules_base.py:1367-1373", []string{"add"}},
	{"PATCH", "/api/3.0/mlflow/mcp-servers/{mcp}/versions/{v}", []AuthCheck{{"mcpservers", "update"}}, "path", []string{MCPSampleName}, "rules_base.py:1363-1366", []string{"add", "remove"}},
	{"POST", "/api/3.0/mlflow/mcp-servers/{mcp}/endpoints", []AuthCheck{{"mcpservers", "update"}}, "path", []string{MCPSampleName}, "rules_base.py:1388-1391", []string{"add"}},
	{"POST", "/api/3.0/mlflow/mcp-servers/{mcp}/tags", []AuthCheck{{"mcpservers", "update"}}, "path", []string{MCPSampleName}, "rules_base.py:1411-1414", []string{"add"}},
	{"GET", "/api/3.0/mlflow/skills/{skill}", []AuthCheck{{"skills", "get"}}, "path", []string{SkillSampleName}, "rules_skill_registry.py:111-113", []string{"add", "remove"}},
	{"DELETE", "/api/3.0/mlflow/skills/{skill}", []AuthCheck{{"skills", "delete"}}, "path", []string{SkillSampleName}, "rules_skill_registry.py:117-119", []string{"remove"}},
	// "Like MCP, version registration uses parent update even if MLflow
	// creates the parent" (rules_skill_registry.py:30-33).
	{"POST", "/api/3.0/mlflow/skills/{skill}/versions", []AuthCheck{{"skills", "update"}}, "path", []string{SkillSampleName}, "rules_skill_registry.py:78-80", []string{"add"}},
	{"POST", "/api/3.0/mlflow/skills/{skill}/tags", []AuthCheck{{"skills", "update"}}, "path", []string{SkillSampleName}, "rules_skill_registry.py:105-107", []string{"add"}},
	{"GET", "/api/3.0/mlflow/skills/{skill}/versions", []AuthCheck{{"skills", "get"}}, "path", []string{SkillSampleName}, "rules_skill_registry.py:80-82", []string{"add"}},
	{"DELETE", "/api/3.0/mlflow/skills/{skill}/versions/{v}", []AuthCheck{{"skills", "update"}}, "path", []string{SkillSampleName}, "rules_skill_registry.py:89-91", []string{"add"}},
}

// AuthReview is one access review MLflow makes for a samples action:
// its verb and resource in AuthGroup, and the object's name ("" for an
// unnamed review).
type AuthReview struct{ Verb, Resource, Name string }

// ActionReviews are the access reviews of action's calls: a rule with a
// name parser is checked per name (the review the narrow grants answer;
// the plugin asks without a name first, auth/core.py:812-851), and
// SearchRuns' fallback is a named get (collection_filters.py:123-139).
func ActionReviews(action string) []AuthReview {
	var out []AuthReview
	seen := map[AuthReview]bool{}
	add := func(r AuthReview) {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	for _, rule := range AuthRules {
		if !slices.Contains(rule.Actions, action) {
			continue
		}
		for _, c := range rule.Checks {
			if rule.Name == "" {
				add(AuthReview{Verb: c.Verb, Resource: c.Resource})
				continue
			}
			verb := c.Verb
			if rule.Name == "body:experiment_ids:get" {
				verb = "get"
			}
			for _, n := range rule.Names {
				add(AuthReview{Verb: verb, Resource: c.Resource, Name: n})
			}
		}
	}
	return out
}
