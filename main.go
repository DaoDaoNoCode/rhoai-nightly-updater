package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
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
	api.Register(mux)
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
	if devMode && !isLoopbackHost(bindAddress) {
		// Every DEV_MODE request runs with the developer's token and no
		// mutation gate, and the Host check does not stop a client that
		// sends Host: 127.0.0.1 itself.
		if os.Getenv("DEV_ALLOW_REMOTE") != "true" {
			slog.Error("refusing to serve DEV_MODE on a non-loopback address: anyone who can reach it acts with your cluster token; "+
				"unset BIND_ADDRESS, or set DEV_ALLOW_REMOTE=true to accept that", "bindAddress", bindAddress)
			os.Exit(1)
		}
		slog.Warn("DEV_ALLOW_REMOTE=true: DEV_MODE serves a non-loopback address; anyone who can reach it acts with your cluster token and no permission check",
			"bindAddress", bindAddress)
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

	// Let accepted cluster mutations finish (they have a 15-minute deadline and
	// a bounded recovery step) instead of cutting them off half-way. The pod's
	// terminationGracePeriodSeconds must exceed this drain plus the shutdown.
	drainTimeout := 16 * time.Minute
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
	// A finished operation whose marker could not be cleared would be
	// reported as interrupted by the next process: try once more.
	flushCtx, cancelFlush := context.WithTimeout(context.Background(), 10*time.Second)
	api.FlushOperationMarker(flushCtx)
	cancelFlush()

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

// isLoopbackHost reports whether a BIND_ADDRESS host only accepts
// connections from this machine. An empty host listens on all interfaces.
func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func registerProbes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", api.HandleHealth)
	mux.HandleFunc("GET /api/health/ready", api.HandleReady)
	mux.HandleFunc("GET /api/version", api.HandleVersion)
}
