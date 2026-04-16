// sybra-server exposes all Sybra bound methods as a REST API, reusing the
// same internal packages as the desktop app. Intended for headless / web-only
// deployments where the Wails binary is not available.
//
// Environment variables:
//
//	SYBRA_PORT       HTTP listen port (default: 8080)
//	SYBRA_STATIC_DIR Directory to serve as /; set to frontend/dist for SPA
//	                   (optional — omit to skip static file serving)
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/httpapi"
	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/skills"
	"github.com/Automaat/sybra/internal/sse"
	"github.com/Automaat/sybra/internal/sybra"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	if err := run(); err != nil {
		println("fatal:", err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logger, levelVar, cleanup, err := logging.New(cfg.Logging)
	if err != nil {
		return fmt.Errorf("logger: %w", err)
	}
	defer cleanup()

	// Route Go's default log through slog at DEBUG.
	log.SetFlags(0)
	log.SetOutput(slogWriter{logger})

	if err := metrics.Init(cfg.Metrics); err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := metrics.Shutdown(shutCtx); err != nil {
			logger.Error("metrics.shutdown", "err", err)
		}
	}()

	broker := sse.New()

	app := sybra.NewApp(logger, levelVar, cfg, sybra.WithEmit(broker.Emit), sybra.WithSkillsFS(skills.FS))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := app.Startup(ctx); err != nil {
		return fmt.Errorf("startup: %w", err)
	}
	defer app.Shutdown(ctx)

	mux := buildMux(logger, broker, app)

	// CORS for dev (permissive; tighten for production).
	handler := corsMiddleware(mux)

	port := os.Getenv("SYBRA_PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("server.listen", "addr", addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		logger.Info("server.shutdown")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if shutErr := srv.Shutdown(shutCtx); shutErr != nil {
			logger.Error("server.shutdown.err", "err", shutErr)
		}
	case serveErr := <-errCh:
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", serveErr)
		}
	}
	return nil
}

// buildMux wires every HTTP route the server exposes onto a fresh ServeMux:
// health, optional /metrics, optional /debug/pprof, SSE streams, the
// reflection-based /api/{service}/{method} dispatcher, and an optional SPA
// static file server. Extracted from run() so run() stays under the 100-line
// funlen cap without losing the explicit route declaration layout.
func buildMux(logger *slog.Logger, broker *sse.Broker, app *sybra.App) *http.ServeMux {
	mux := http.NewServeMux()

	// Health check endpoint for container orchestration.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"ok"}`)
	})

	// Prometheus scrape endpoint (opt-in via config.metrics.enabled). The
	// OTel Prometheus exporter registers instruments into the default
	// prometheus/client_golang registry, so promhttp.Handler serves them.
	if metrics.Enabled() {
		mux.Handle("GET /metrics", promhttp.Handler())
		logger.Info("metrics.listen", "path", "/metrics")
	}

	// pprof scrape endpoints (opt-in via SYBRA_PPROF=1). Mounted on the main
	// mux so perf tooling can pull heap / goroutine profiles over the same
	// port without opening a second listener. Off by default to avoid leaking
	// internals on shared deployments.
	if v := os.Getenv("SYBRA_PPROF"); v == "1" || v == "true" {
		mux.HandleFunc("GET /debug/pprof/", pprof.Index)
		mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
		logger.Info("pprof.listen", "path", "/debug/pprof/")
	}

	// Multiplexed SSE stream: all events over a single connection.
	mux.HandleFunc("GET /events", broker.ServeAll)

	// Per-event SSE endpoint (kept for debugging / backward compat).
	mux.HandleFunc("GET /api/events/{eventName}", broker.ServeHTTP)

	// API dispatch: POST /api/{service}/{method}
	httpapi.Mount(mux, app.ServiceRegistry(), logger)

	// Optional SPA static files.
	if staticDir := os.Getenv("SYBRA_STATIC_DIR"); staticDir != "" {
		sub, err := fs.Sub(os.DirFS(staticDir), ".")
		if err != nil {
			logger.Error("static.dir", "err", err)
		} else {
			fileServer := http.FileServer(http.FS(sub))
			mux.Handle("GET /", spaHandler{fileServer, staticDir})
		}
	}

	return mux
}

// spaHandler serves static files and falls back to index.html for unknown paths
// (supports client-side routing).
type spaHandler struct {
	fs        http.Handler
	staticDir string
}

func (h spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	urlPath := r.URL.Path
	if urlPath == "" {
		urlPath = "/"
	}
	if _, err := os.Stat(h.staticDir + urlPath); os.IsNotExist(err) {
		// Paths with a file extension (e.g. /favicon.ico) are static asset
		// requests, not SPA routes — return 404 so browsers don't treat an
		// HTML index.html response as a broken asset.
		if strings.Contains(path.Base(urlPath), ".") {
			http.NotFound(w, r)
			return
		}
		r2 := *r
		r2.URL.Path = "/"
		h.fs.ServeHTTP(w, &r2)
		return
	}
	h.fs.ServeHTTP(w, r)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type slogWriter struct{ logger *slog.Logger }

func (w slogWriter) Write(p []byte) (int, error) {
	w.logger.Debug("stdlib.log", "msg", string(p))
	return len(p), nil
}
