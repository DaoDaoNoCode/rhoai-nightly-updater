package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/api"
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

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", api.HandleHealth)
	mux.HandleFunc("GET /api/health/ready", api.HandleReady)
	mux.HandleFunc("GET /api/status", api.HandleStatus)
	mux.HandleFunc("POST /api/update", api.HandleUpdate)
	mux.HandleFunc("POST /api/update/stream", api.HandleUpdateStream)
	mux.HandleFunc("POST /api/rollback", api.HandleRollback)
	mux.HandleFunc("POST /api/rollback/stream", api.HandleReinstallStream)
	mux.HandleFunc("POST /api/refresh", api.HandleRefreshOperator)
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
	mux.HandleFunc("POST /api/setup/dsc", api.HandleCreateDSC)
	mux.HandleFunc("GET /metrics", api.HandleMetrics)
	mux.HandleFunc("POST /api/pageview", api.HandlePageView)

	staticDir := "./frontend/dist"
	if dir := os.Getenv("STATIC_DIR"); dir != "" {
		staticDir = dir
	}

	absStaticDir, err := filepath.Abs(staticDir)
	if err != nil {
		slog.Error("failed to resolve static dir", "error", err)
		os.Exit(1)
	}

	fs := http.FileServer(http.Dir(absStaticDir))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Clean the path and verify it stays within staticDir to prevent traversal
		cleanPath := filepath.Clean(r.URL.Path)
		fullPath := filepath.Join(absStaticDir, cleanPath)
		if !strings.HasPrefix(fullPath, absStaticDir) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		if _, err := os.Stat(fullPath); os.IsNotExist(err) && r.URL.Path != "/" {
			http.ServeFile(w, r, filepath.Join(absStaticDir, "index.html"))
			return
		}
		fs.ServeHTTP(w, r)
	})

	handler := middleware.RequestID(middleware.SecurityHeaders(mux))

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 180 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown on SIGTERM/SIGINT
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		slog.Info("starting server", "port", port, "staticDir", absStaticDir)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down server")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("server shutdown error", "error", err)
		os.Exit(1)
	}
	slog.Info("server stopped")
}
