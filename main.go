package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/api"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/middleware"
)

func main() {
	var logLevel slog.Level
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn", "warning":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})))
	api.LogStartupVersion()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	devMode := os.Getenv("DEV_MODE") == "true"

	staticDir := "./frontend/dist"
	if dir := os.Getenv("STATIC_DIR"); dir != "" {
		staticDir = dir
	}
	absStaticDir, err := filepath.Abs(staticDir)
	if err != nil {
		slog.Error("failed to resolve static dir", "error", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	registerProbes(mux)
	mux.HandleFunc("GET /api/status", api.HandleStatus)
	mux.HandleFunc("GET /api/operation", api.HandleOperation)
	mux.HandleFunc("POST /api/update", api.HandleUpdate) // dry run only
	mux.HandleFunc("POST /api/update/stream", api.HandleUpdateStream)
	mux.HandleFunc("POST /api/rollback/stream", api.HandleReinstallStream)
	mux.HandleFunc("POST /api/components/dsc/repair", api.HandleRepairDSC)
	mux.HandleFunc("POST /api/refresh/stream", api.HandleRefreshStream)
	mux.HandleFunc("POST /api/assist-rollout", api.HandleAssistRollout)
	mux.HandleFunc("GET /api/activity", api.HandleActivity)
	mux.HandleFunc("GET /api/latest-nightly", api.HandleLatestNightly)
	mux.HandleFunc("GET /api/nightly-tags", api.HandleNightlyTags)
	mux.HandleFunc("GET /api/test-pull-secret", api.HandleTestPullSecret)
	mux.HandleFunc("POST /api/setup/pull-secret", api.HandleCreatePullSecret)
	mux.HandleFunc("GET /api/verify-nodes", api.HandleVerifyNodes)
	mux.HandleFunc("GET /api/user/permissions", api.HandleUserPermissions)
	mux.HandleFunc("GET /api/components", api.HandleComponents)
	mux.HandleFunc("GET /api/debug", api.HandleDebug)
	mux.HandleFunc("GET /api/build-explorer/tags", api.HandleBuildExplorerTags)
	mux.HandleFunc("GET /api/build-explorer/content", api.HandleBuildExplorerContent)
	mux.HandleFunc("GET /api/dashboard/state", api.HandleDashboardState)
	mux.HandleFunc("POST /api/dashboard/deploy-pr", api.HandleDashboardDeployPR)
	mux.HandleFunc("POST /api/dashboard/deploy-main", api.HandleDashboardDeployMain)
	mux.HandleFunc("POST /api/dashboard/revert", api.HandleDashboardRevert)
	mux.HandleFunc("GET /api/resources/status", api.HandleResourcesStatus)
	mux.HandleFunc("GET /api/resources/projects", api.HandleDSProjects)
	mux.HandleFunc("POST /api/resources/minio/setup", api.HandleMinIOSetup)
	mux.HandleFunc("POST /api/resources/minio/teardown", api.HandleMinIOTeardown)
	mux.HandleFunc("POST /api/resources/pipeline-server/setup", api.HandlePipelineServerSetup)
	mux.HandleFunc("POST /api/resources/pipeline-server/teardown", api.HandlePipelineServerTeardown)
	mux.HandleFunc("POST /api/resources/mlflow/setup", api.HandleMLflowSetup)
	mux.HandleFunc("POST /api/resources/mlflow/teardown", api.HandleMLflowTeardown)
	mux.HandleFunc("POST /api/resources/mlflow/deploy-pr", api.HandleMLflowDeployPR)
	mux.HandleFunc("POST /api/resources/mlflow/revert", api.HandleMLflowRevert)
	mux.HandleFunc("GET /api/diagnostics", api.HandleDiagnostics)
	mux.HandleFunc("POST /api/diagnostics/fix", api.HandleDiagnosticsFix)
	mux.HandleFunc("GET /api/setup/dsc/preview", api.HandleDSCPreview)
	mux.HandleFunc("POST /api/setup/dsc", api.HandleCreateDSC)
	mux.HandleFunc("POST /api/pageview", api.HandlePageView)
	mux.HandleFunc("/api/", middleware.APINotFound)
	mux.Handle("/", middleware.StaticFiles(absStaticDir))

	handler := middleware.SecurityHeaders(middleware.RequireJSONForMutations(mux))
	if devMode {
		// DEV_MODE authenticates every request with the developer's own
		// token: accept only this machine's host names (DNS rebinding).
		frontendPort := os.Getenv("DEV_FRONTEND_PORT")
		if frontendPort == "" {
			frontendPort = "9000"
		}
		handler = middleware.LocalOnly([]string{port, frontendPort}, handler)
	}
	handler = middleware.RequestID(middleware.AccessLog(handler))

	// DEV_MODE must only be reachable from this machine. In-cluster the
	// template sets BIND_ADDRESS=127.0.0.1 so that only oauth-proxy (same
	// pod) reaches the API; probes and metrics use METRICS_PORT.
	bindAddress := os.Getenv("BIND_ADDRESS")
	if bindAddress == "" && devMode {
		bindAddress = "127.0.0.1"
	}

	srv := &http.Server{
		Addr:         bindAddress + ":" + port,
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 180 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	servers := []*http.Server{srv}

	// The metrics listener serves only probes and /metrics, so the Service
	// can expose it without exposing the API.
	if metricsPort := os.Getenv("METRICS_PORT"); metricsPort != "" {
		metricsMux := http.NewServeMux()
		registerProbes(metricsMux)
		metricsMux.HandleFunc("GET /metrics", api.HandleMetrics)
		servers = append(servers, &http.Server{
			Addr:         ":" + metricsPort,
			Handler:      middleware.RequestID(middleware.AccessLog(metricsMux)),
			ReadTimeout:  15 * time.Second,
			WriteTimeout: 30 * time.Second,
			IdleTimeout:  120 * time.Second,
		})
	} else {
		mux.HandleFunc("GET /metrics", api.HandleMetrics)
	}

	// Graceful shutdown on SIGTERM/SIGINT
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	for _, s := range servers {
		go func(s *http.Server) {
			slog.Info("starting server", "address", s.Addr, "staticDir", absStaticDir)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("server error", "address", s.Addr, "error", err)
				os.Exit(1)
			}
		}(s)
	}

	<-ctx.Done()
	slog.Info("shutting down server")

	// Let accepted cluster mutations finish (their deadline, the bounded
	// recovery step and bookkeeping) instead of cutting them off half-way.
	// The pod's terminationGracePeriodSeconds must exceed this drain plus the
	// shutdown (pkg/cluster/budget_test.go).
	drainTimeout := cluster.ShutdownDrainTimeout()
	if v := os.Getenv("SHUTDOWN_DRAIN_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			drainTimeout = d
		} else {
			slog.Warn("invalid SHUTDOWN_DRAIN_TIMEOUT, using default", "value", v, "default", drainTimeout)
		}
	}
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), drainTimeout)
	if running := api.DrainMutations(drainCtx); running > 0 {
		slog.Warn("shutting down with cluster operations still running", "count", running)
	}
	cancelDrain()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	exitCode := 0
	for _, s := range servers {
		if err := s.Shutdown(shutdownCtx); err != nil {
			slog.Error("server shutdown error", "address", s.Addr, "error", err)
			exitCode = 1
		}
	}
	slog.Info("server stopped")
	os.Exit(exitCode)
}

func registerProbes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", api.HandleHealth)
	mux.HandleFunc("GET /api/health/ready", api.HandleReady)
	mux.HandleFunc("GET /api/version", api.HandleVersion)
}
