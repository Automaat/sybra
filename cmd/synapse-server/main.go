// synapse-server exposes all Synapse bound methods as a REST API, reusing the
// same internal packages as the desktop app. Intended for headless / web-only
// deployments where the Wails binary is not available.
//
// Environment variables:
//
//	SYNAPSE_PORT       HTTP listen port (default: 8080)
//	SYNAPSE_STATIC_DIR Directory to serve as /; set to frontend/dist for SPA
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
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/httpapi"
	"github.com/Automaat/synapse/internal/logging"
	"github.com/Automaat/synapse/internal/sse"
	"github.com/Automaat/synapse/internal/synapse"
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

	broker := sse.New()

	app := synapse.NewApp(logger, levelVar, cfg, synapse.WithEmit(broker.Emit))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := app.Startup(ctx); err != nil {
		return fmt.Errorf("startup: %w", err)
	}
	defer app.Shutdown(ctx)

	mux := http.NewServeMux()

	// Multiplexed SSE stream: all events over a single connection.
	mux.HandleFunc("GET /events", broker.ServeAll)

	// Per-event SSE endpoint (kept for debugging / backward compat).
	mux.HandleFunc("GET /api/events/{eventName}", broker.ServeHTTP)

	// API dispatch: POST /api/{service}/{method}
	httpapi.Mount(mux, app.ServiceRegistry(), logger)

	// Optional SPA static files.
	if staticDir := os.Getenv("SYNAPSE_STATIC_DIR"); staticDir != "" {
		sub, err := fs.Sub(os.DirFS(staticDir), ".")
		if err != nil {
			logger.Error("static.dir", "err", err)
		} else {
			fileServer := http.FileServer(http.FS(sub))
			mux.Handle("GET /", spaHandler{fileServer, staticDir})
		}
	}

	// CORS for dev (permissive; tighten for production).
	handler := corsMiddleware(mux)

	port := os.Getenv("SYNAPSE_PORT")
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

// spaHandler serves static files and falls back to index.html for unknown paths
// (supports client-side routing).
type spaHandler struct {
	fs        http.Handler
	staticDir string
}

func (h spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "" {
		path = "/"
	}
	// Check if the file exists; fall back to index.html if not.
	if _, err := os.Stat(h.staticDir + path); os.IsNotExist(err) {
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
