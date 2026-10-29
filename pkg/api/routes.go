package api

import "net/http"

// Route is an endpoint of the API listener.
type Route struct {
	Pattern string // net/http.ServeMux pattern, with its method
	Handler http.HandlerFunc
}

// routes are the API endpoints. Every route that is not a GET changes the
// cluster and must go through withMutationAuth (the cluster-admin gate) and
// lockCluster, except POST /api/pageview, which only counts page views.
// TestEveryNonGetRouteIsGated checks this.
var routes = []Route{
	{"GET /api/status", HandleStatus},
	{"GET /api/operation", HandleOperation},
	{"POST /api/update", HandleUpdate}, // dry run only
	{"POST /api/update/stream", HandleUpdateStream},
	{"POST /api/rollback/stream", HandleReinstallStream},
	{"POST /api/components/dsc/repair", HandleRepairDSC},
	{"POST /api/refresh/stream", HandleRefreshStream},
	{"POST /api/assist-rollout", HandleAssistRollout},
	{"GET /api/activity", HandleActivity},
	{"GET /api/update-check", HandleUpdateCheck},
	{"GET /api/latest-nightly", HandleLatestNightly},
	{"GET /api/nightly-tags", HandleNightlyTags},
	{"GET /api/test-pull-secret", HandleTestPullSecret},
	{"POST /api/setup/pull-secret", HandleCreatePullSecret},
	{"GET /api/verify-nodes", HandleVerifyNodes},
	{"GET /api/user/permissions", HandleUserPermissions},
	{"GET /api/components", HandleComponents},
	{"GET /api/debug", HandleDebug},
	{"GET /api/build-explorer/tags", HandleBuildExplorerTags},
	{"GET /api/build-explorer/content", HandleBuildExplorerContent},
	{"GET /api/build-explorer/contains", HandleBuildExplorerContains},
	{"GET /api/dashboard/state", HandleDashboardState},
	{"GET /api/dashboard/builds", HandleDashboardBuilds},
	{"POST /api/dashboard/deploy-pr", HandleDashboardDeployPR},
	{"POST /api/dashboard/deploy-main", HandleDashboardDeployMain},
	{"POST /api/dashboard/revert", HandleDashboardRevert},
	{"GET /api/resources/status", HandleResourcesStatus},
	{"GET /api/resources/projects", HandleDSProjects},
	{"POST /api/resources/minio/setup", HandleMinIOSetup},
	{"POST /api/resources/minio/teardown", HandleMinIOTeardown},
	{"POST /api/resources/pipeline-server/setup", HandlePipelineServerSetup},
	{"POST /api/resources/pipeline-server/teardown", HandlePipelineServerTeardown},
	{"POST /api/resources/mlflow/setup", HandleMLflowSetup},
	{"POST /api/resources/mlflow/teardown", HandleMLflowTeardown},
	{"POST /api/resources/mlflow/deploy-pr", HandleMLflowDeployPR},
	{"POST /api/resources/mlflow/revert", HandleMLflowRevert},
	{"GET /api/diagnostics", HandleDiagnostics},
	{"POST /api/diagnostics/fix", HandleDiagnosticsFix},
	{"GET /api/setup/dsc/preview", HandleDSCPreview},
	{"POST /api/setup/dsc", HandleCreateDSC},
	{"POST /api/pageview", HandlePageView},
}

// gitOpsRoutes is the gitops profile's allowlist: the routes the Status,
// Components and Diagnostics pages need to show the cluster, and Update.
// Every read needs cluster-admin (withGitOpsAdmin); Update streams through
// HandleUpdateStreamGitOps, which runs only Update's Argo CD path. The
// RBAC these handlers need is deploy/rbac-gitops.yaml, derived from them by
// pkg/cluster/rbac_coverage_test.go.
//
//   - GET /api/user/permissions makes no ServiceAccount call (the check
//     runs with the user's token), so it stays open: it is how the UI
//     learns that the user is not a cluster-admin.
//   - POST /api/pageview only counts page views in memory (no cluster
//     call, bounded labels), so the profile's use shows in /metrics.
//   - The dry run (POST /api/update) runs the profile's own read-only dry
//     run (HandleUpdateGitOps); the Update card offers it.
//   - Status, Components, Diagnostics and the update check have their own
//     handlers, which read no Secret and contact no host but the API server
//     (pkg/cluster/gitops_offline_test.go).
var gitOpsRoutes = map[string]http.HandlerFunc{
	"GET /api/status":           withGitOpsAdmin(HandleStatusGitOps),
	"GET /api/operation":        withGitOpsAdmin(HandleOperation),
	"GET /api/activity":         withGitOpsAdmin(HandleActivity),
	"GET /api/update-check":     withGitOpsAdmin(HandleUpdateCheckGitOps),
	"GET /api/components":       withGitOpsAdmin(HandleComponentsGitOps),
	"GET /api/diagnostics":      withGitOpsAdmin(HandleDiagnosticsGitOps),
	"GET /api/user/permissions": HandleUserPermissions,
	"POST /api/update/stream":   HandleUpdateStreamGitOps,
	"POST /api/update":          HandleUpdateGitOps,
	"POST /api/pageview":        HandlePageView,
}

// gitOpsRefused lists every other route with the feature the 403 names.
// TestEveryRouteHasAGitOpsProfileDecision fails for a route in neither
// table, so a new route is off in the gitops profile until it is added
// to gitOpsRoutes on purpose.
var gitOpsRefused = map[string]string{
	"POST /api/rollback/stream":                    "Reinstall",
	"POST /api/components/dsc/repair":              "Repairing the DataScienceCluster",
	"POST /api/refresh/stream":                     "Re-deploy",
	"POST /api/assist-rollout":                     "Rollout assist",
	"GET /api/latest-nightly":                      "Looking up nightly builds on Quay",
	"GET /api/nightly-tags":                        "Looking up nightly builds on Quay",
	"GET /api/test-pull-secret":                    "Testing the pull secret",
	"POST /api/setup/pull-secret":                  "Setting up the pull secret",
	"GET /api/verify-nodes":                        "Checking the nodes",
	"GET /api/debug":                               "Debug information",
	"GET /api/build-explorer/tags":                 "Build Explorer",
	"GET /api/build-explorer/content":              "Build Explorer",
	"GET /api/build-explorer/contains":             "Build Explorer",
	"GET /api/dashboard/state":                     "Dashboard Dev",
	"GET /api/dashboard/builds":                    "Dashboard Dev",
	"POST /api/dashboard/deploy-pr":                "Dashboard Dev",
	"POST /api/dashboard/deploy-main":              "Dashboard Dev",
	"POST /api/dashboard/revert":                   "Dashboard Dev",
	"GET /api/resources/status":                    "Test resources",
	"GET /api/resources/projects":                  "Test resources",
	"POST /api/resources/minio/setup":              "Test resources",
	"POST /api/resources/minio/teardown":           "Test resources",
	"POST /api/resources/pipeline-server/setup":    "Test resources",
	"POST /api/resources/pipeline-server/teardown": "Test resources",
	"POST /api/resources/mlflow/setup":             "Test resources",
	"POST /api/resources/mlflow/teardown":          "Test resources",
	"POST /api/resources/mlflow/deploy-pr":         "Test resources",
	"POST /api/resources/mlflow/revert":            "Test resources",
	"POST /api/diagnostics/fix":                    "Diagnostics fixes",
	"GET /api/setup/dsc/preview":                   "Creating the DataScienceCluster",
	"POST /api/setup/dsc":                          "Creating the DataScienceCluster",
}

// Register adds the API endpoints of the active profile to mux. In the
// gitops profile a route of gitOpsRoutes gets its handler there and every
// other route answers 403 profile_gitops.
func Register(mux *http.ServeMux) {
	for _, r := range routes {
		mux.HandleFunc(r.Pattern, handlerFor(activeProfile, r))
	}
}

func handlerFor(p Profile, r Route) http.HandlerFunc {
	if p != ProfileGitOps {
		return r.Handler
	}
	if h, ok := gitOpsRoutes[r.Pattern]; ok {
		return h
	}
	feature, ok := gitOpsRefused[r.Pattern]
	if !ok {
		feature = "This feature"
	}
	return refusedInGitOpsProfile(feature)
}
