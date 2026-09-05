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
//	SYBRA_GITHUB_WEBHOOK_SECRET GitHub App webhook signing secret
//	SYBRA_STATIC_DIR Directory to serve as /; set to frontend/dist for SPA
//	                   (optional — omit to skip static file serving)
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Automaat/sybra/internal/autoupdate"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/httpapi"
	"github.com/Automaat/sybra/internal/httpserve"
	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/skills"
	"github.com/Automaat/sybra/internal/sse"
	"github.com/Automaat/sybra/internal/sybra"
	"github.com/Automaat/sybra/internal/task"
)

func main() {
	// Unattended: an unset agent.sandbox_mode resolves to "report", which
	// never wraps the spawn, so an omitted key is indistinguishable from a
	// deliberately unsandboxed one. Set before any config load so the
	// requirement covers startup, -check-config, and every later hot reload
	// through the same validator — a boot-only check is defeated by the
	// config watcher re-applying an edited file.
	config.RequireExplicitSandboxMode(true)

	checkConfig := flag.Bool("check-config", false, "load and validate the live config (SYBRA_HOME/config.yaml), then exit without starting the server: 0 if valid, 1 otherwise")
	flag.Parse()
	if *checkConfig {
		os.Exit(runCheckConfig())
	}

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

// runCheckConfig is the deploy-time preflight entry point (see deploy/bin/
// sybra-build.sh): it exercises the exact same config resolution/validation
// path run() does — including the unknown-key rejection in
// validateKnownConfigKeys and ValidateCluster — against the live
// SYBRA_HOME/config.yaml, but never binds a port, starts background
// pollers, or touches other process-level side effects. LoadNoPersist (not
// Load) is deliberate: a preflight run must never mutate the live
// config.yaml (migration rewrite, generated auth token, tightened perms) —
// only the eventual real startup should do that.
func runCheckConfig() int {
	cfg, err := config.LoadNoPersist()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config: invalid:", err)
		return 1
	}
	if err := cfg.ValidateCluster(); err != nil {
		fmt.Fprintln(os.Stderr, "config: invalid:", err)
		return 1
	}
	fmt.Println("config: ok")
	return 0
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
	shutdownCh := make(chan struct{}, 1)
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signalCh)
	var restartRequested atomic.Bool

	app := sybra.NewApp(logger, levelVar, cfg,
		sybra.WithEmit(broker.Emit),
		sybra.WithSkillsFS(skills.FS),
		sybra.WithRestartRequest(newRestartRequest(shutdownCh, &restartRequested)),
	)

	// Before Startup, not after: Startup's recovery pass dispatches agents for
	// runs it finds stale, and an agent that starts before its board is named
	// gets no target at all — a whole paid run whose every CLI call refuses.
	// The address comes from configuration, not from the listener, so it is
	// known this early; the listener follows a few lines down.
	app.SetAgentBoard(agentBoardTarget(cfg), cfg.Server.AuthToken, agentBoardCA(cfg))

	if err := app.Startup(rootCtx); err != nil {
		return 1, fmt.Errorf("startup: %w", err)
	}
	shutdownApp := true
	defer func() {
		if shutdownApp {
			app.Shutdown(context.Background())
		}
	}()
	if restartRequested.Load() {
		return autoupdate.RestartExitCode, nil
	}

	webhookSrv, webhookErrCh, err := startWebhookServer(rootCtx, cfg, app, logger)
	if err != nil {
		return 1, err
	}

	mux := buildMux(logger, broker, app)

	// authMiddleware gates the HTTP control plane behind the shared-secret
	// bearer token while leaving SPA/static delivery public; corsMiddleware
	// only echoes CORS headers back for origins on the configured allowlist
	// (no wildcard). Order matters: CORS must sit outside auth so preflight
	// OPTIONS requests (which never carry Authorization) are answered before
	// reaching it.
	handler := cspMiddleware(corsMiddleware(cfg.Server.AllowedOrigins, authMiddleware(cfg.Server.AuthToken, logger, mux)))

	srv, errCh, err := serveAll(rootCtx, cfg, handler, logger)
	if err != nil {
		shutdownBackgroundServer(webhookSrv, logger, "webhook")
		return 1, err
	}

	// Agents reach task state through sybra-cli, which has no filesystem path
	// to it. Name the board now that it is listening; loopback, so the token
	// never leaves the host.
	select {
	case sig := <-signalCh:
		logger.Info("server.signal", "signal", sig.String())
		shutdownApp = false
		runGracefulShutdown(logger, app, srv, webhookSrv, &restartRequested)
	case <-shutdownCh:
		logger.Info("server.restart.requested")
		shutdownApp = false
		runGracefulShutdown(logger, app, srv, webhookSrv, &restartRequested)
	case serveErr := <-errCh:
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return 1, fmt.Errorf("serve: %w", serveErr)
		}
	case webhookErr := <-webhookErrCh:
		if webhookErr != nil && !errors.Is(webhookErr, http.ErrServerClosed) {
			return 1, fmt.Errorf("webhook serve: %w", webhookErr)
		}
	}
	if restartRequested.Load() {
		return autoupdate.RestartExitCode, nil
	}
	return 0, nil
}

const (
	drainAdmissionWindow  = 1 * time.Second
	httpShutdownDeadline  = 15 * time.Second
	webhookShutdownBudget = 4 * time.Second
	shutdownHardDeadline  = 40 * time.Second
)

func newRestartRequest(shutdownCh chan<- struct{}, restart *atomic.Bool) func() {
	return func() {
		restart.Store(true)
		notifyShutdown(shutdownCh)
	}
}

func notifyShutdown(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func runGracefulShutdown(logger *slog.Logger, app *sybra.App, srv, webhookSrv *http.Server, restart *atomic.Bool) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownHardDeadline)
	defer cancel()
	app.BeginDrain()
	logger.Info("server.shutdown", "restart", restart.Load(), "deadline", shutdownHardDeadline.String(), "drain_window", drainAdmissionWindow.String())
	go forceExitAfter(logger, shutdownHardDeadline, restart)
	time.Sleep(drainAdmissionWindow)
	shutdownServer(shutdownCtx, logger, "server", srv, httpShutdownDeadline)
	shutdownServer(shutdownCtx, logger, "webhook", webhookSrv, webhookShutdownBudget)
	app.Shutdown(shutdownCtx)
}

func shutdownServer(ctx context.Context, logger *slog.Logger, name string, srv *http.Server, grace time.Duration) {
	if srv == nil {
		return
	}
	shutCtx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()
	if err := shutdownHTTPServer(shutCtx, srv); err != nil {
		logger.Error(name+".shutdown.err", "err", err)
	}
}

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
// buildMux, the middleware chain, and the SPA handler live in
// internal/httpserve so the desktop app serves the same surface. These aliases
// keep the server's own call sites and tests pointed at one name.
func buildMux(logger *slog.Logger, broker *sse.Broker, app *sybra.App) *http.ServeMux {
	return httpserve.BuildMux(serveOptions(logger, broker, app))
}

func serveOptions(logger *slog.Logger, broker *sse.Broker, app *sybra.App) httpserve.Options {
	return httpserve.Options{
		Logger:        logger,
		Broker:        broker,
		Services:      sybra.ServiceRegistry(app),
		Admit:         app.HTTPAdmission,
		StaticDir:     os.Getenv("SYBRA_STATIC_DIR"),
		EnablePprof:   httpserve.PprofEnabled(),
		Home:          config.HomeDir(),
		WorkerControl: sybra.WorkerControlHandler(app),
		// No SelfOrigin and no Token: a browser reaching this over the network
		// gets the origin alone and asks its operator for the token.
	}
}

type spaHandler = httpserve.SPAHandler

var (
	cspMiddleware  = httpserve.CSPMiddleware
	corsMiddleware = httpserve.CORSMiddleware
	authMiddleware = httpserve.AuthMiddleware
)

// agentBoardTarget is the address of the board this process serves, as an
// agent's own sybra-cli should dial it.
//
// Loopback first among several binds, so the token an agent sends never leaves
// the machine. Taking merely the first listed skipped a usable loopback
// listener sitting behind a LAN one and left every agent on that deployment
// refusing its own task CRUD, because a cleartext client declines a
// non-loopback target outright.
//
// A wildcard or unset bind answers on loopback; a concrete one does not, so an
// operator who locked the control plane to one interface is named there.
func agentBoardTarget(cfg *config.Config) string {
	addrs, _ := cfg.ListenAddrs(os.Getenv("SYBRA_HOST"), os.Getenv("SYBRA_PORT"))
	host, port := "127.0.0.1", config.DefaultServerPort
	if addr := preferredBoardBind(addrs); addr != "" {
		if h, p, err := net.SplitHostPort(addr); err == nil && strings.TrimSpace(p) != "" {
			port = p
			if strings.TrimSpace(h) != "" && h != "0.0.0.0" && h != "::" {
				host = h
			}
		}
	}
	if cfg.ServesTLS() {
		return "https://" + net.JoinHostPort(host, port)
	}
	return net.JoinHostPort(host, port)
}

// preferredBoardBind picks the bind an agent should dial: a loopback or
// wildcard one if this process listens on any, else the first.
func preferredBoardBind(addrs []string) string {
	for _, addr := range addrs {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			continue
		}
		host = strings.Trim(strings.TrimSpace(host), "[]")
		if host == "" || host == "0.0.0.0" || host == "::" {
			return addr
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return addr
		}
		if strings.EqualFold(host, "localhost") {
			return addr
		}
	}
	if len(addrs) > 0 {
		return addrs[0]
	}
	return ""
}

// agentBoardCA is the certificate an agent must pin to reach a TLS board, or
// empty when the board serves cleartext.
func agentBoardCA(cfg *config.Config) string {
	if !cfg.ServesTLS() {
		return ""
	}
	return cfg.Cluster.TLS.CertFile
}

type slogWriter struct{ logger *slog.Logger }

func (w slogWriter) Write(p []byte) (int, error) {
	w.logger.Debug("stdlib.log", "msg", string(p))
	return len(p), nil
}

const webhookSignatureHeader = "X-Sybra-Signature"

type webhookTaskCreator interface {
	CreateTaskWithInit(title, body, mode string, init task.Update) (task.Task, error)
	ListTasks() ([]task.Task, error)
}

type webhookAdmissionFunc func() error

type webhookTaskRequest struct {
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Mode      string   `json:"mode"`
	Tags      []string `json:"tags"`
	ProjectID string   `json:"project_id"`
}

type webhookTaskResponse struct {
	TaskID string `json:"task_id"`
}

type webhookErrorEnvelope struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func resolveWebhookTaskCreator(app *sybra.App) (webhookTaskCreator, error) {
	taskSvc, ok := sybra.ServiceRegistry(app)["TaskService"]
	if !ok {
		return nil, fmt.Errorf("webhook task service unavailable")
	}
	creator, ok := taskSvc.Impl.(webhookTaskCreator)
	if !ok {
		return nil, fmt.Errorf("webhook task service has unexpected type %T", taskSvc.Impl)
	}
	return creator, nil
}

func newWebhookHandler(logger *slog.Logger, secret string, creator webhookTaskCreator, admit webhookAdmissionFunc) http.Handler {
	return newWebhookMux(logger, secret, true, config.GitHubConfig{}, creator, admit)
}

func newWebhookHandlerWithGitHub(
	logger *slog.Logger,
	secret string,
	githubCfg config.GitHubConfig,
	creator webhookTaskCreator,
	admit webhookAdmissionFunc,
) http.Handler {
	taskRouteEnabled := githubCfg.Webhook.TaskEnabled || strings.TrimSpace(secret) != ""
	return newWebhookMux(logger, secret, taskRouteEnabled, githubCfg, creator, admit)
}

func newWebhookMux(
	logger *slog.Logger,
	secret string,
	taskRouteEnabled bool,
	githubCfg config.GitHubConfig,
	creator webhookTaskCreator,
	admit webhookAdmissionFunc,
) http.Handler {
	mux := http.NewServeMux()
	taskHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebhookError(w, logger, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, httpapi.MaxRequestBody)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeWebhookError(w, logger, http.StatusRequestEntityTooLarge, "payload_too_large", "request body too large")
				return
			}
			writeWebhookError(w, logger, http.StatusBadRequest, "validation_error", "failed to read request body")
			return
		}
		if secret != "" && !validWebhookSignature(secret, r.Header.Get(webhookSignatureHeader), body) {
			writeWebhookError(w, logger, http.StatusUnauthorized, "unauthorized", "invalid webhook signature")
			return
		}
		if admit != nil {
			if err := admit(); err != nil {
				writeWebhookAdmissionError(w, logger, err)
				return
			}
		}

		var req webhookTaskRequest
		if err := json.Unmarshal(body, &req); err != nil {
			writeWebhookError(w, logger, http.StatusBadRequest, "validation_error", "invalid JSON payload")
			return
		}
		title := strings.TrimSpace(req.Title)
		if title == "" {
			writeWebhookError(w, logger, http.StatusBadRequest, "validation_error", "title is required")
			return
		}
		mode := strings.TrimSpace(req.Mode)
		if mode == "" {
			mode = task.AgentModeHeadless
		}
		if _, err := task.ValidateMintableAgentMode(mode); err != nil {
			writeWebhookError(w, logger, http.StatusBadRequest, "validation_error", err.Error())
			return
		}

		init := task.Update{}
		if tags := normalizeWebhookTags(req.Tags); len(tags) > 0 {
			init.Tags = task.Ptr(tags)
		}
		if projectID := strings.TrimSpace(req.ProjectID); projectID != "" {
			init.ProjectID = task.Ptr(projectID)
		}

		created, err := creator.CreateTaskWithInit(title, req.Body, mode, init)
		if err != nil {
			logger.Error("webhook.create_task", "err", err)
			writeWebhookError(w, logger, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		writeWebhookJSON(w, http.StatusCreated, webhookTaskResponse{TaskID: created.ID})
	}
	if taskRouteEnabled {
		mux.HandleFunc("/webhook/task", taskHandler)
	}
	mux.Handle("/webhook/github", newGitHubWebhookHandler(logger, githubCfg, creator, admit))
	return mux
}

func normalizeWebhookTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		out = append(out, tag)
	}
	return out
}

func validWebhookSignature(secret, header string, body []byte) bool {
	sigHex, ok := strings.CutPrefix(header, "sha256=")
	if !ok || sigHex == "" {
		return false
	}
	got, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func writeWebhookJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeWebhookError(w http.ResponseWriter, logger *slog.Logger, status int, code, message string) {
	if status >= 500 {
		logger.Error("webhook.error", "status", status, "code", code)
	} else {
		logger.Warn("webhook.error", "status", status, "code", code)
	}
	writeWebhookJSON(w, status, webhookErrorEnvelope{Error: message, Code: code})
}

func writeWebhookAdmissionError(w http.ResponseWriter, logger *slog.Logger, err error) {
	var clientErr httpapi.ClientError
	if errors.As(err, &clientErr) {
		code := "validation_error"
		if clientErr.HTTPStatus() == http.StatusServiceUnavailable {
			code = string(httpapi.ErrCodeUnavailable)
		}
		writeWebhookError(w, logger, clientErr.HTTPStatus(), code, clientErr.Error())
		return
	}
	logger.Warn("webhook.admission.error", "err", err)
	writeWebhookError(w, logger, http.StatusInternalServerError, "internal_error", "internal error")
}

func startWebhookServer(ctx context.Context, cfg *config.Config, app *sybra.App, logger *slog.Logger) (*http.Server, chan error, error) {
	if cfg == nil || !cfg.GitHub.Webhook.Enabled {
		return nil, nil, nil
	}
	creator, err := resolveWebhookTaskCreator(app)
	if err != nil {
		return nil, nil, fmt.Errorf("webhook: %w", err)
	}
	admit := func() error {
		return app.HTTPAdmission("TaskService", "CreateTask", httpapi.MethodMeta{})
	}
	handler := newWebhookHandlerWithGitHub(logger, cfg.GitHub.Webhook.TaskSecret, cfg.GitHub, creator, admit)
	return startWebhookServerWithHandler(ctx, cfg.GitHub.Webhook, handler, logger)
}

func startWebhookServerWithHandler(ctx context.Context, cfg config.GitHubWebhookConfig, handler http.Handler, logger *slog.Logger) (*http.Server, chan error, error) {
	if !cfg.Enabled {
		return nil, nil, nil
	}
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	listeners, err := listenAll(ctx, []string{srv.Addr})
	if err != nil {
		return nil, nil, fmt.Errorf("webhook listen %s: %w", srv.Addr, err)
	}
	errCh := make(chan error, len(listeners))
	for i := range listeners {
		ln := listeners[i]
		logger.Info("webhook.listen", "addr", ln.Addr().String())
		go func() {
			errCh <- srv.Serve(ln)
		}()
	}
	return srv, errCh, nil
}

func shutdownHTTPServer(ctx context.Context, srv *http.Server) error {
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

func shutdownBackgroundServer(srv *http.Server, logger *slog.Logger, name string) {
	if srv == nil {
		return
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		logger.Error(name+".shutdown.err", "err", err)
	}
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
