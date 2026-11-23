package api

import (
	"net/http"
	"strings"
)

// Route is an endpoint of the API listener.
type Route struct {
	Pattern string // net/http.ServeMux pattern, with its method
	Handler http.HandlerFunc
}

// routes are the API endpoints. Every route that is not a GET changes the
// cluster and must go through withMutationAuth (the cluster-admin gate) and
// lockCluster (a recipe run and resume: lockRecipeRunner), except POST
// /api/pageview, which only counts page views, and POST /api/recipes/stop,
// which only asks the running recipe run (which holds the lock) to stop.
// TestEveryNonGetRouteIsGated checks this. Every GET route has a tier in
// getRouteTiers (route_tiers.go): who may read it; one without is
// admin-only.
var routes = []Route{
	{"GET /api/status", HandleStatus},
	{"GET /api/summary", HandleSummary},
	{"GET /api/operation", HandleOperation},
	{"POST /api/update", HandleUpdate}, // dry run only
	{"POST /api/update/stream", HandleUpdateStream},
	{"POST /api/reinstall/stream", HandleReinstallStream},
	{"POST /api/components/dsc/repair", HandleRepairDSC},
	{"GET /api/components/dsc", HandleDSCManager},
	{"GET /api/components/dsc/plan", HandleDSCPlan},
	{"POST /api/components/dsc/management", HandleDSCManagement},
	{"POST /api/components/dsc/fix", HandleDSCFix},
	{"POST /api/components/dsc/reset", HandleDSCSoftReset},
	{"POST /api/components/dsc/hard-reset/stream", HandleDSCHardResetStream},
	{"POST /api/components/dsc/hard-reset/finish", HandleDSCHardResetFinish},
	{"POST /api/refresh/stream", HandleRefreshStream},
	{"POST /api/assist-rollout", HandleAssistRollout},
	{"GET /api/activity", HandleActivity},
	{"GET /api/update-check", HandleUpdateCheck},
	{"GET /api/latest-nightly", HandleLatestNightly},
	{"GET /api/nightly-tags", HandleNightlyTags},
	{"GET /api/test-pull-secret", HandleTestPullSecret},
	{"POST /api/setup/pull-secret", HandleCreatePullSecret},
	{"GET /api/verify-nodes", HandleVerifyNodes},
	{"POST /api/setup/node-pull-check", HandleNodePullCheck},
	{"POST /api/setup/nodes/plan", HandleNodeSetupPlan},
	{"POST /api/setup/nodes/stream", HandleNodeSetupStream},
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
	{"POST /api/resources/s3/setup", HandleS3StorageSetup},
	{"POST /api/resources/s3/teardown", HandleS3StorageTeardown},
	{"POST /api/resources/s3/secure", HandleS3StorageSecure},
	{"GET /api/resources/s3/admin-password", HandleS3AdminPassword},
	{"POST /api/resources/s3/migrate/stream", HandleS3StorageMigrateStream},
	{"POST /api/resources/pipeline-server/setup", HandlePipelineServerSetup},
	{"POST /api/resources/pipeline-server/teardown", HandlePipelineServerTeardown},
	{"POST /api/resources/mlflow/setup", HandleMLflowSetup},
	{"POST /api/resources/mlflow/teardown", HandleMLflowTeardown},
	{"POST /api/resources/mlflow/deploy-pr", HandleMLflowDeployPR},
	{"POST /api/resources/mlflow/revert", HandleMLflowRevert},
	{"POST /api/resources/models/stream", HandleModelsStream},
	{"GET /api/resources/models/exposures", HandleModelExposures},
	{"GET /api/resources/playground", HandlePlaygroundStatus},
	{"POST /api/resources/playground/stream", HandlePlaygroundStream},
	{"GET /api/resources/maas", HandleMaaSStatus},
	{"GET /api/resources/maas/groups", HandleMaaSGroups},
	{"POST /api/resources/maas/stream", HandleMaaSStream},
	{"GET /api/resources/prerequisites", HandlePrerequisitesStatus},
	{"POST /api/resources/prerequisites/stream", HandlePrerequisitesStream},
	{"POST /api/resources/cleanup/stream", HandleCleanupStream},
	{"GET /api/resources/samples/mlflow", HandleMLflowSamplesStatus},
	{"POST /api/resources/samples/mlflow/stream", HandleMLflowSamplesStream},
	{"GET /api/resources/samples/pipeline/spec", HandlePipelineSampleSpec},
	{"GET /api/diagnostics", HandleDiagnostics},
	{"POST /api/diagnostics/fix", HandleDiagnosticsFix},
	{"POST /api/diagnostics/fix/acknowledged", HandleDiagnosticsFixAcknowledged},
	{"GET /api/setup/dsc/preview", HandleDSCPreview},
	{"POST /api/setup/dsc", HandleCreateDSC},
	{"POST /api/recipes/plan", HandleRecipePlan},
	{"POST /api/recipes/run", HandleRecipeRun},
	{"POST /api/recipes/resume", HandleRecipeResume},
	{"POST /api/recipes/stop", HandleRecipeStop},
	{"GET /api/recipes/run", HandleRecipeRunStatus},
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
	"GET /api/components/dsc":   withGitOpsAdmin(HandleDSCManager),
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
	"GET /api/summary":                             "The cluster summary",
	"POST /api/reinstall/stream":                   "Install a different build",
	"POST /api/components/dsc/repair":              "Repairing the DataScienceCluster",
	"GET /api/components/dsc/plan":                 "Changing the DataScienceCluster",
	"POST /api/components/dsc/management":          "Changing the DataScienceCluster",
	"POST /api/components/dsc/fix":                 "Changing the DataScienceCluster",
	"POST /api/components/dsc/reset":               "Resetting the DataScienceCluster",
	"POST /api/components/dsc/hard-reset/stream":   "Resetting the DataScienceCluster",
	"POST /api/components/dsc/hard-reset/finish":   "Resetting the DataScienceCluster",
	"POST /api/refresh/stream":                     "Re-deploy",
	"POST /api/assist-rollout":                     "Rollout assist",
	"GET /api/latest-nightly":                      "Looking up nightly builds on Quay",
	"GET /api/nightly-tags":                        "Looking up nightly builds on Quay",
	"GET /api/test-pull-secret":                    "Testing the pull secret",
	"POST /api/setup/pull-secret":                  "Setting up the pull secret",
	"GET /api/verify-nodes":                        "Checking the nodes",
	"POST /api/setup/node-pull-check":              "Checking that the nodes can pull",
	"POST /api/setup/nodes/plan":                   "Setting up the image mirror and the nodes' pull secret",
	"POST /api/setup/nodes/stream":                 "Setting up the image mirror and the nodes' pull secret",
	"GET /api/debug":                               "Debug information",
	"GET /api/build-explorer/tags":                 "Build Explorer",
	"GET /api/build-explorer/content":              "Build Explorer",
	"GET /api/build-explorer/contains":             "Build Explorer",
	"GET /api/dashboard/state":                     "Dashboard Dev",
	"GET /api/dashboard/builds":                    "Dashboard Dev",
	"POST /api/dashboard/deploy-pr":                "Dashboard Dev",
	"POST /api/dashboard/deploy-main":              "Dashboard Dev",
	"POST /api/dashboard/revert":                   "Dashboard Dev",
	"GET /api/resources/status":                    "The Environment page",
	"GET /api/resources/projects":                  "The Environment page",
	"POST /api/resources/s3/setup":                 "The Environment page",
	"POST /api/resources/s3/teardown":              "The Environment page",
	"POST /api/resources/s3/secure":                "The Environment page",
	"GET /api/resources/s3/admin-password":         "The Environment page",
	"POST /api/resources/s3/migrate/stream":        "The Environment page",
	"POST /api/resources/pipeline-server/setup":    "The Environment page",
	"POST /api/resources/pipeline-server/teardown": "The Environment page",
	"POST /api/resources/mlflow/setup":             "The Environment page",
	"POST /api/resources/mlflow/teardown":          "The Environment page",
	"POST /api/resources/mlflow/deploy-pr":         "The Environment page",
	"POST /api/resources/mlflow/revert":            "The Environment page",
	"POST /api/resources/models/stream":            "The Environment page",
	"GET /api/resources/models/exposures":          "The Environment page",
	"GET /api/resources/playground":                "The Environment page",
	"POST /api/resources/playground/stream":        "The Environment page",
	"GET /api/resources/maas":                      "The Environment page",
	"GET /api/resources/maas/groups":               "The Environment page",
	"POST /api/resources/maas/stream":              "The Environment page",
	"GET /api/resources/prerequisites":             "The Environment page",
	"POST /api/resources/prerequisites/stream":     "The Environment page",
	"POST /api/resources/cleanup/stream":           "The Environment page",
	"GET /api/resources/samples/mlflow":            "The Environment page",
	"POST /api/resources/samples/mlflow/stream":    "The Environment page",
	"GET /api/resources/samples/pipeline/spec":     "The Environment page",
	"POST /api/diagnostics/fix":                    "Diagnostics fixes",
	"POST /api/diagnostics/fix/acknowledged":       "Diagnostics fixes",
	"GET /api/setup/dsc/preview":                   "Creating the DataScienceCluster",
	"POST /api/setup/dsc":                          "Creating the DataScienceCluster",
	"POST /api/recipes/plan":                       "Environment recipes",
	"POST /api/recipes/run":                        "Environment recipes",
	"POST /api/recipes/resume":                     "Environment recipes",
	"POST /api/recipes/stop":                       "Environment recipes",
	"GET /api/recipes/run":                         "Environment recipes",
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
		// An admin-only GET (or one without a tier) refuses non-admins
		// before its handler runs (route_tiers.go).
		if strings.HasPrefix(r.Pattern, http.MethodGet+" ") && tierOf(r.Pattern) == tierAdmin {
			return adminOnly(r.Handler)
		}
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
