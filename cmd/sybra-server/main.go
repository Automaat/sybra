// sybra-server exposes all Sybra bound methods as a REST API, reusing the
// same internal packages as the desktop app. Intended for headless / web-only
// deployments where the Wails binary is not available.
//
// Environment variables:
//
//	SYBRA_BIND_ADDR  Listen address; overrides cluster.bind_addr(s)
//	SYBRA_PORT       HTTP listen port (default: 8080; a configured
//	                   cluster.bind_addr(s) wins over this)
//	SYBRA_HOST       HTTP listen host (default: all interfaces; a configured
//	                   cluster.bind_addr(s) wins over this)
//	SYBRA_AUTH_TOKEN Bearer token for the HTTP control plane
//	SYBRA_STATIC_DIR Directory to serve as /; set to frontend/dist for SPA
//	                   (optional — omit to skip static file serving)
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Automaat/sybra/internal/autoupdate"
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
	code, err := run()
	if err != nil {
		println("fatal:", err.Error())
		if code == 0 {
			code = 1
		}
	}
	if code != 0 {
		os.Exit(code)
	}
}

func run() (int, error) {
	cfg, err := config.Load()
	if err != nil {
		return 1, fmt.Errorf("config: %w", err)
	}

	if err := cfg.ValidateCluster(); err != nil {
		return 1, fmt.Errorf("config: %w", err)
	}

	logger, levelVar, cleanup, err := logging.New(cfg.Logging)
	if err != nil {
		return 1, fmt.Errorf("logger: %w", err)
	}
	defer cleanup()

	// Route Go's default log through slog at DEBUG.
	log.SetFlags(0)
	log.SetOutput(slogWriter{logger})

	if err := metrics.Init(cfg.Metrics); err != nil {
		return 1, fmt.Errorf("metrics: %w", err)
	}
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := metrics.Shutdown(shutCtx); err != nil {
			logger.Error("metrics.shutdown", "err", err)
		}
	}()

	broker := sse.New()
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()
	var restartRequested atomic.Bool

	app := sybra.NewApp(logger, levelVar, cfg,
		sybra.WithEmit(broker.Emit),
		sybra.WithSkillsFS(skills.FS),
		sybra.WithRestartRequest(func() {
			restartRequested.Store(true)
			rootCancel()
		}),
	)

	ctx, stop := signal.NotifyContext(rootCtx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := app.Startup(ctx); err != nil {
		return 1, fmt.Errorf("startup: %w", err)
	}
	defer app.Shutdown(ctx)
	if restartRequested.Load() {
		return autoupdate.RestartExitCode, nil
	}

	mux := buildMux(logger, broker, app)

	// authMiddleware gates the HTTP control plane behind the shared-secret
	// bearer token while leaving SPA/static delivery public; corsMiddleware
	// only echoes CORS headers back for origins on the configured allowlist
	// (no wildcard). Order matters: CORS must sit outside auth so preflight
	// OPTIONS requests (which never carry Authorization) are answered before
	// reaching it.
	handler := metricsMiddleware(cspMiddleware(corsMiddleware(cfg.Server.AllowedOrigins, authMiddleware(cfg.Server.AuthToken, logger, mux))))

	srv, errCh, err := serveAll(ctx, cfg, handler, logger)
	if err != nil {
		return 1, err
	}

	select {
	case <-ctx.Done():
		logger.Info("server.shutdown")
		go forceExitAfter(logger, shutdownHardDeadline, &restartRequested)
		// 30s window covers the agent manager's 20s grace (giving SIGTERM'd
		// claude/codex processes a chance to flush their final result event)
		// plus headroom for HTTP handlers to drain. Previously 10s, which
		// caused server.shutdown.err: context deadline exceeded on every
		// restart because HTTP + agent drains both ran inside that window.
		shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if shutErr := srv.Shutdown(shutCtx); shutErr != nil {
			logger.Error("server.shutdown.err", "err", shutErr)
		}
	case serveErr := <-errCh:
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return 1, fmt.Errorf("serve: %w", serveErr)
		}
	}
	if restartRequested.Load() {
		return autoupdate.RestartExitCode, nil
	}
	return 0, nil
}

const shutdownHardDeadline = 60 * time.Second

func forceExitAfter(logger *slog.Logger, d time.Duration, restart *atomic.Bool) {
	time.Sleep(d)
	code := 0
	if restart.Load() {
		code = autoupdate.RestartExitCode
	}
	logger.Error("shutdown.forced", "after", d.String(), "code", code)
	os.Exit(code)
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
	httpapi.Mount(mux, sybra.ServiceRegistry(app), logger)

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

// metricsMiddleware records every response's status code against the
// sybra_http_requests_total counter and the internal/httpstats sliding
// window the monitor's HTTP-error-rate SLO detector reads (see
// internal/monitor detectHTTPErrorRate). /health is excluded: it's a
// high-frequency container-orchestration liveness probe, not real API
// traffic, and would dilute the error-rate signal.
func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		metrics.RecordHTTPRequest(r.Context(), time.Now(), rec.status)
	})
}

// statusRecorder wraps http.ResponseWriter to capture the status code
// actually written. It preserves http.Flusher so it stays transparent to the
// SSE broker (internal/sse), which type-asserts the ResponseWriter it's
// given to flush each event as it's written.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.wroteHeader = true
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.WriteHeader(http.StatusOK)
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func cspMiddleware(next http.Handler) http.Handler {
	const policy = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; connect-src 'self' ws: wss:; font-src 'self'; manifest-src 'self'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", policy)
		next.ServeHTTP(w, r)
	})
}

// corsMiddleware echoes CORS headers back only for an Origin present in
// allowedOrigins (exact match) — no wildcard. Requests without a matching
// Origin still reach next (non-browser callers don't need CORS headers at
// all), they just won't be readable cross-origin from an unlisted site.
func corsMiddleware(allowedOrigins []string, next http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[o] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			if _, ok := allowed[origin]; ok {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authMiddleware gates only the HTTP control plane behind a shared-secret
// bearer token: `/api/*`, `/events`, `/api/events/*`, `/metrics`, and
// `/debug/pprof/*`. Browser EventSource cannot set request headers, so the
// SSE endpoints additionally accept the token as a `?token=` query param.
// Static SPA assets stay public so normal browser navigations can load the
// app shell before JS starts issuing authenticated API calls. A blank token
// fails every protected request closed rather than treating it as "auth
// disabled" — config.Load always generates one, so an empty value here means
// misconfiguration, not intent.
func authMiddleware(token string, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requestRequiresAuth(r) {
			next.ServeHTTP(w, r)
			return
		}
		if !requestAuthorized(r, token) {
			logger.Warn("server.auth.denied", "path", r.URL.Path, "remote", r.RemoteAddr)
			w.Header().Set("WWW-Authenticate", `Bearer realm="sybra"`)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, `{"error":"unauthorized","code":"unauthorized"}`)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requestRequiresAuth(r *http.Request) bool {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/health":
		return false
	case r.URL.Path == "/events":
		return true
	case r.URL.Path == "/metrics":
		return true
	case strings.HasPrefix(r.URL.Path, "/api/"):
		return true
	case strings.HasPrefix(r.URL.Path, "/debug/pprof/"):
		return true
	default:
		return false
	}
}

func requestAuthorized(r *http.Request, token string) bool {
	if token == "" {
		return false
	}
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && tokensEqual(bearer, token) {
		return true
	}
	if isSSEPath(r.URL.Path) {
		if qt := r.URL.Query().Get("token"); qt != "" && tokensEqual(qt, token) {
			return true
		}
	}
	return false
}

func isSSEPath(p string) bool {
	return p == "/events" || strings.HasPrefix(p, "/api/events/")
}

func tokensEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

type slogWriter struct{ logger *slog.Logger }

func (w slogWriter) Write(p []byte) (int, error) {
	w.logger.Debug("stdlib.log", "msg", string(p))
	return len(p), nil
}

func serveAll(ctx context.Context, cfg *config.Config, handler http.Handler, logger *slog.Logger) (*http.Server, chan error, error) {
	envHost, envPort := os.Getenv("SYBRA_HOST"), os.Getenv("SYBRA_PORT")
	addrs, envDiscarded := cfg.ListenAddrs(envHost, envPort)
	if envDiscarded {
		logger.Warn("server.bind.env_ignored",
			"bind", addrs, "env_host", envHost, "env_port", envPort,
			"hint", "cluster.bind_addr(s) wins; set SYBRA_BIND_ADDR to override")
	}
	if len(addrs) == 0 {
		return nil, nil, fmt.Errorf("no listen address resolved")
	}
	servesTLS := cfg.ServesTLS()

	srv := &http.Server{
		Addr:              addrs[0],
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	listeners, err := listenAll(ctx, addrs)
	if err != nil {
		return nil, nil, err
	}

	errCh := make(chan error, len(listeners))
	for i := range listeners {
		ln := listeners[i]
		logger.Info("server.listen", "addr", ln.Addr().String(), "tls", servesTLS, "role", cfg.ClusterRole())
		go func() {
			if servesTLS {
				errCh <- srv.ServeTLS(ln, cfg.Cluster.TLS.CertFile, cfg.Cluster.TLS.KeyFile)
				return
			}
			errCh <- srv.Serve(ln)
		}()
	}
	return srv, errCh, nil
}

func listenAll(ctx context.Context, addrs []string) ([]net.Listener, error) {
	var lc net.ListenConfig
	listeners := make([]net.Listener, 0, len(addrs))
	for _, addr := range addrs {
		ln, err := lc.Listen(ctx, "tcp", addr)
		if err != nil {
			for _, open := range listeners {
				_ = open.Close()
			}
			return nil, fmt.Errorf("listen %s: %w", addr, err)
		}
		listeners = append(listeners, ln)
	}
	return listeners, nil
}
