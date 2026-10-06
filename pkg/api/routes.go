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

// Register adds the API endpoints to mux.
func Register(mux *http.ServeMux) {
	for _, r := range routes {
		mux.HandleFunc(r.Pattern, r.Handler)
	}
}
