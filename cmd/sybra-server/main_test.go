package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/httpapi"
	"github.com/Automaat/sybra/internal/sse"
	"github.com/Automaat/sybra/internal/sybra"
	"github.com/Automaat/sybra/internal/task"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// syncBuffer is a concurrency-safe log sink: slog handlers are written to from
// whatever background goroutines the App starts, not just the test goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// stubProviderCLIs shadows the provider binaries with stubs that fail
// immediately, for the length of the test.
//
// With workflows enabled, creating a task drives the engine's classify step
// into llmexec, which shells out to whichever provider CLI is on PATH. On a
// developer machine that is the real, metered `claude` — so `go test ./...`
// would spend credits and block App.Shutdown for its whole grace waiting on
// the child. On CI, where no provider is installed, the exec fails instantly
// and the same test passes. Stubbing makes both behave like CI.
//
// Only the provider names are shadowed; git and everything else still resolve
// from the rest of PATH.
func stubProviderCLIs(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"claude", "codex", "copilot"} {
		stub := "#!/bin/sh\necho 'provider CLI stubbed in tests' >&2\nexit 1\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(stub), 0o755); err != nil {
			t.Fatalf("write %s stub: %v", name, err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// startupLikeApp boots a real App against a throwaway SYBRA_HOME and tears it
// down on cleanup.
//
// The home is deliberately not t.TempDir(). App.Shutdown waits a bounded grace
// (appShutdownWaitGrace) for background goroutines and then proceeds
// regardless, so on a loaded machine a straggler can still write into
// home/tasks after Shutdown returns. Under t.TempDir that surfaces as
// "TempDir RemoveAll cleanup: directory not empty" — a failure that names the
// filesystem rather than the goroutine, and lands on whichever test was
// running. Removal here is best-effort instead, and the condition that actually
// matters is asserted directly: if Shutdown's wait ever times out, the test
// fails with the goroutine dump Shutdown already logs.
func startupLikeApp(t *testing.T, opts ...sybra.Option) *sybra.App {
	t.Helper()
	home, err := os.MkdirTemp("", "sybra-server-test-*")
	if err != nil {
		t.Fatalf("create test home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("SYBRA_HOME", home)
	t.Setenv("SYBRA_DISABLE_WORKFLOWS", "0")
	stubProviderCLIs(t)

	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	app := sybra.NewApp(logger, &slog.LevelVar{}, startupLikeServerTestConfig(home), opts...)
	if err := app.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	t.Cleanup(func() {
		app.Shutdown(context.Background())
		if strings.Contains(logs.String(), "app.shutdown.wait_timeout") {
			t.Errorf("App.Shutdown timed out waiting for background goroutines; a straggler outlived shutdown and can still write into SYBRA_HOME:\n%s", logs.String())
		}
	})
	return app
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

type fakeWebhookTaskCreator struct {
	created task.Task
	err     error
	listed  []task.Task
	listErr error

	gotTitle string
	gotBody  string
	gotMode  string
	gotInit  task.Update
	calls    int
}

func (f *fakeWebhookTaskCreator) CreateTaskWithInit(title, body, mode string, init task.Update) (task.Task, error) {
	f.calls++
	f.gotTitle = title
	f.gotBody = body
	f.gotMode = mode
	f.gotInit = init
	if f.err != nil {
		return task.Task{}, f.err
	}
	return f.created, nil
}

func (f *fakeWebhookTaskCreator) ListTasks() ([]task.Task, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]task.Task(nil), f.listed...), nil
}

type recordedEvent struct {
	name string
	data any
}

type eventRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
}

func (r *eventRecorder) append(name string, data any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, recordedEvent{name: name, data: data})
}

func (r *eventRecorder) snapshot() []recordedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedEvent(nil), r.events...)
}

func startupLikeServerTestConfig(home string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.TasksDir = filepath.Join(home, "tasks")
	cfg.SkillsDir = filepath.Join(home, "claude", "skills")
	cfg.RepoDir = home
	cfg.ProjectsDir = filepath.Join(home, "projects")
	cfg.ClonesDir = filepath.Join(home, "clones")
	cfg.WorktreesDir = filepath.Join(home, "worktrees")
	cfg.LoopAgentsDir = filepath.Join(home, "loop-agents")
	cfg.Logging.Dir = filepath.Join(home, "logs")
	cfg.Notification.Desktop = false
	cfg.GitHub.Enabled = false
	cfg.Renovate.Enabled = false
	cfg.Triage.Enabled = false
	cfg.Umbrella.Enabled = false
	cfg.Watchdog.Enabled = false
	cfg.Monitor.Enabled = false
	cfg.SelfMonitor.Enabled = false
	cfg.Evaluation.Enabled = false
	cfg.HarnessEvolve.Enabled = false
	cfg.AutoUpdate.Enabled = false
	cfg.Providers.HealthCheck.Enabled = false
	cfg.Providers.Limits.Enabled = false
	return cfg
}

func webhookSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestAuthMiddlewareRejectsMissingToken(t *testing.T) {
	h := authMiddleware("secret", testLogger(), okHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/TaskService/ListTasks", http.NoBody)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestWebhookHandlerCreatesUnsignedTaskWithDefaultsAndMetadata(t *testing.T) {
	creator := &fakeWebhookTaskCreator{created: task.Task{ID: "task-1"}}
	handler := newWebhookHandler(testLogger(), "", creator, nil)
	body := []byte(`{"title":" from webhook ","body":"payload","tags":[" webhook ","","ops"],"project_id":" Automaat/sybra "}`)

	req := httptest.NewRequest(http.MethodPost, "/webhook/task", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rr.Code)
	}
	var resp webhookTaskResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.TaskID != "task-1" {
		t.Fatalf("task_id = %q, want task-1", resp.TaskID)
	}
	if creator.calls != 1 {
		t.Fatalf("CreateTaskWithInit calls = %d, want 1", creator.calls)
	}
	if creator.gotTitle != "from webhook" {
		t.Fatalf("title = %q, want trimmed title", creator.gotTitle)
	}
	if creator.gotBody != "payload" {
		t.Fatalf("body = %q, want payload", creator.gotBody)
	}
	if creator.gotMode != task.AgentModeHeadless {
		t.Fatalf("mode = %q, want %q", creator.gotMode, task.AgentModeHeadless)
	}
	if creator.gotInit.Tags == nil {
		t.Fatal("init.Tags = nil, want webhook tags")
	}
	if got := *creator.gotInit.Tags; len(got) != 2 || got[0] != "webhook" || got[1] != "ops" {
		t.Fatalf("tags = %v, want [webhook ops]", got)
	}
	if creator.gotInit.ProjectID == nil || *creator.gotInit.ProjectID != "Automaat/sybra" {
		t.Fatalf("project_id = %v, want Automaat/sybra", creator.gotInit.ProjectID)
	}
}

func TestWebhookHandlerCreatesSignedTask(t *testing.T) {
	creator := &fakeWebhookTaskCreator{created: task.Task{ID: "task-signed"}}
	handler := newWebhookHandler(testLogger(), "secret", creator, nil)
	body := []byte(`{"title":"signed","mode":"headless"}`)

	req := httptest.NewRequest(http.MethodPost, "/webhook/task", strings.NewReader(string(body)))
	req.Header.Set(webhookSignatureHeader, webhookSignature("secret", body))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rr.Code)
	}
	if creator.gotMode != task.AgentModeHeadless {
		t.Fatalf("mode = %q, want %q", creator.gotMode, task.AgentModeHeadless)
	}
}

func TestWebhookHandlerRejectsInteractiveMode(t *testing.T) {
	creator := &fakeWebhookTaskCreator{created: task.Task{ID: "task-rejected"}}
	handler := newWebhookHandler(testLogger(), "secret", creator, nil)
	body := []byte(`{"title":"signed","mode":"interactive"}`)

	req := httptest.NewRequest(http.MethodPost, "/webhook/task", strings.NewReader(string(body)))
	req.Header.Set(webhookSignatureHeader, webhookSignature("secret", body))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestWebhookHandlerRejectsMissingSignature(t *testing.T) {
	handler := newWebhookHandler(testLogger(), "secret", &fakeWebhookTaskCreator{}, nil)
	req := httptest.NewRequest(http.MethodPost, "/webhook/task", strings.NewReader(`{"title":"signed"}`))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestWebhookHandlerRejectsBadSignature(t *testing.T) {
	handler := newWebhookHandler(testLogger(), "secret", &fakeWebhookTaskCreator{}, nil)
	body := []byte(`{"title":"signed"}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook/task", strings.NewReader(string(body)))
	req.Header.Set(webhookSignatureHeader, webhookSignature("wrong", body))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestWebhookHandlerRejectsInvalidPayload(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		status int
	}{
		{name: "invalid json", body: `{"title":`, status: http.StatusBadRequest},
		{name: "missing title", body: `{"body":"x"}`, status: http.StatusBadRequest},
		{name: "invalid mode", body: `{"title":"x","mode":"bogus"}`, status: http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := newWebhookHandler(testLogger(), "", &fakeWebhookTaskCreator{}, nil)
			req := httptest.NewRequest(http.MethodPost, "/webhook/task", strings.NewReader(tc.body))
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tc.status {
				t.Fatalf("status = %d, want %d", rr.Code, tc.status)
			}
		})
	}
}

func TestWebhookHandlerRejectsWrongMethod(t *testing.T) {
	handler := newWebhookHandler(testLogger(), "", &fakeWebhookTaskCreator{}, nil)
	req := httptest.NewRequest(http.MethodGet, "/webhook/task", http.NoBody)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

func TestNewRestartRequestCoalescesWakeups(t *testing.T) {
	shutdownCh := make(chan struct{}, 1)
	var restart atomic.Bool

	restartReq := newRestartRequest(shutdownCh, &restart)
	restartReq()
	restartReq()

	if !restart.Load() {
		t.Fatal("restart flag = false, want true")
	}
	select {
	case <-shutdownCh:
	default:
		t.Fatal("shutdown signal not delivered")
	}
	select {
	case <-shutdownCh:
		t.Fatal("shutdown signal delivered twice; want coalesced wakeup")
	default:
	}
}

func TestShutdownHardDeadlineCoversSequentialGracefulBudgets(t *testing.T) {
	sequentialBudget := drainAdmissionWindow + httpShutdownDeadline + webhookShutdownBudget
	if shutdownHardDeadline <= sequentialBudget {
		t.Fatalf("shutdownHardDeadline = %s, want > server/webhook graceful budget %s", shutdownHardDeadline, sequentialBudget)
	}
	if shutdownHardDeadline >= 45*time.Second {
		t.Fatalf("shutdownHardDeadline = %s, want < systemd TimeoutStopSec 45s", shutdownHardDeadline)
	}
}

func TestWebhookHandlerPersistsTaskAndEmitsCreatedEvent(t *testing.T) {
	var emitted eventRecorder
	app := startupLikeApp(t, sybra.WithEmit(func(event string, data any) {
		emitted.append(event, data)
	}))

	creator, err := resolveWebhookTaskCreator(app)
	if err != nil {
		t.Fatalf("resolveWebhookTaskCreator: %v", err)
	}
	handler := newWebhookHandler(testLogger(), "", creator, nil)
	body := []byte(`{"title":"from webhook","body":"hook body","tags":["webhook","ext"],"project_id":"Automaat/sybra"}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook/task", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rr.Code)
	}
	var resp webhookTaskResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	taskSvc, ok := sybra.ServiceRegistry(app)["TaskService"].Impl.(*sybra.TaskService)
	if !ok {
		t.Fatal("TaskService impl missing")
	}
	created, err := taskSvc.GetTask(resp.TaskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if created.Title != "from webhook" {
		t.Fatalf("title = %q, want from webhook", created.Title)
	}
	if created.Body != "hook body" {
		t.Fatalf("body = %q, want hook body", created.Body)
	}
	if created.AgentMode != task.AgentModeHeadless {
		t.Fatalf("mode = %q, want %q", created.AgentMode, task.AgentModeHeadless)
	}
	if created.ProjectID != "Automaat/sybra" {
		t.Fatalf("project_id = %q, want Automaat/sybra", created.ProjectID)
	}
	if len(created.Tags) != 2 || created.Tags[0] != "webhook" || created.Tags[1] != "ext" {
		t.Fatalf("tags = %v, want [webhook ext]", created.Tags)
	}
	found := false
	for _, evt := range emitted.snapshot() {
		if evt.name == events.TaskCreated {
			if path, ok := evt.data.(string); ok && strings.HasSuffix(path, resp.TaskID+".md") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("task:created event for %s not observed", resp.TaskID)
	}
}

func TestWebhookHandlerRejectsTaskCreationDuringDrain(t *testing.T) {
	app := startupLikeApp(t)

	creator, err := resolveWebhookTaskCreator(app)
	if err != nil {
		t.Fatalf("resolveWebhookTaskCreator: %v", err)
	}
	handler := newWebhookHandler(testLogger(), "", creator, func() error {
		return app.HTTPAdmission("TaskService", "CreateTask", httpapi.MethodMeta{})
	})
	app.BeginDrain()

	req := httptest.NewRequest(http.MethodPost, "/webhook/task", strings.NewReader(`{"title":"from webhook"}`))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	var resp webhookErrorEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Code != string(httpapi.ErrCodeUnavailable) {
		t.Fatalf("code = %q, want %q", resp.Code, httpapi.ErrCodeUnavailable)
	}

	taskSvc, ok := sybra.ServiceRegistry(app)["TaskService"].Impl.(*sybra.TaskService)
	if !ok {
		t.Fatal("TaskService impl missing")
	}
	tasks, err := taskSvc.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("tasks created during drain = %d, want 0", len(tasks))
	}
}

func TestStartWebhookServerDisabledNoop(t *testing.T) {
	srv, errCh, err := startWebhookServerWithHandler(t.Context(), config.GitHubWebhookConfig{}, okHandler(), testLogger())
	if err != nil {
		t.Fatalf("startWebhookServerWithHandler: %v", err)
	}
	if srv != nil || errCh != nil {
		t.Fatalf("disabled webhook = (%v, %v), want nil,nil", srv, errCh)
	}
}

func TestStartWebhookServerFailsOnBindError(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	cfg := config.GitHubWebhookConfig{Enabled: true, Port: ln.Addr().(*net.TCPAddr).Port}
	srv, errCh, err := startWebhookServerWithHandler(t.Context(), cfg, okHandler(), testLogger())
	if err == nil {
		if srv != nil {
			_ = srv.Close()
		}
		t.Fatal("bind conflict succeeded, want error")
	}
	if srv != nil || errCh != nil {
		t.Fatalf("error path returned (%v, %v), want nil,nil", srv, errCh)
	}
}

func TestAuthMiddlewareAcceptsMatchingBearerToken(t *testing.T) {
	h := authMiddleware("secret", testLogger(), okHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/TaskService/ListTasks", http.NoBody)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestAuthMiddlewareRejectsWrongBearerToken(t *testing.T) {
	h := authMiddleware("secret", testLogger(), okHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/TaskService/ListTasks", http.NoBody)
	req.Header.Set("Authorization", "Bearer wrong")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestAuthMiddlewareAllowsHealthWithoutToken(t *testing.T) {
	h := authMiddleware("secret", testLogger(), okHandler())
	req := httptest.NewRequest(http.MethodGet, "/health", http.NoBody)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestAuthMiddlewareAllowsStaticRouteWithoutToken(t *testing.T) {
	h := authMiddleware("secret", testLogger(), okHandler())
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestAuthMiddlewareBlankTokenFailsClosed(t *testing.T) {
	h := authMiddleware("", testLogger(), okHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/TaskService/ListTasks", http.NoBody)
	req.Header.Set("Authorization", "Bearer ")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (blank server token must never authorize)", rr.Code)
	}
}

func TestAuthMiddlewareSSEAcceptsQueryToken(t *testing.T) {
	h := authMiddleware("secret", testLogger(), okHandler())
	req := httptest.NewRequest(http.MethodGet, "/events?token=secret", http.NoBody)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SSE path should accept query token)", rr.Code)
	}
}

func TestAuthMiddlewareNonSSEPathRejectsQueryToken(t *testing.T) {
	h := authMiddleware("secret", testLogger(), okHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/TaskService/ListTasks?token=secret", http.NoBody)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (query token must only be honored on SSE paths)", rr.Code)
	}
}

func TestCorsMiddlewareEchoesAllowedOrigin(t *testing.T) {
	h := corsMiddleware([]string{"https://allowed.example"}, okHandler())
	req := httptest.NewRequest(http.MethodGet, "/health", http.NoBody)
	req.Header.Set("Origin", "https://allowed.example")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://allowed.example" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the allowed origin", got)
	}
}

func TestCorsMiddlewarePreservesExistingVaryHeaders(t *testing.T) {
	h := corsMiddleware([]string{"https://allowed.example"}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/health", http.NoBody)
	req.Header.Set("Origin", "https://allowed.example")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	got := rr.Result().Header.Values("Vary")
	if len(got) != 2 || got[0] != "Origin" || got[1] != "Accept-Encoding" {
		t.Fatalf("Vary headers = %q, want [Origin Accept-Encoding]", got)
	}
}

func TestCorsMiddlewareOmitsHeadersForUnlistedOrigin(t *testing.T) {
	h := corsMiddleware([]string{"https://allowed.example"}, okHandler())
	req := httptest.NewRequest(http.MethodGet, "/health", http.NoBody)
	req.Header.Set("Origin", "https://evil.example")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty for an unlisted origin", got)
	}
}

func TestCorsMiddlewareNeverSetsWildcard(t *testing.T) {
	h := corsMiddleware(nil, okHandler())
	req := httptest.NewRequest(http.MethodGet, "/health", http.NoBody)
	req.Header.Set("Origin", "https://anything.example")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got == "*" {
		t.Error("Access-Control-Allow-Origin must never be a wildcard")
	}
}

func TestCorsMiddlewareHandlesPreflight(t *testing.T) {
	h := corsMiddleware([]string{"https://allowed.example"}, okHandler())
	req := httptest.NewRequest(http.MethodOptions, "/api/TaskService/ListTasks", http.NoBody)
	req.Header.Set("Origin", "https://allowed.example")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 for OPTIONS preflight", rr.Code)
	}
}

func TestServerHandlerServesSPAWithoutToken(t *testing.T) {
	staticDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(staticDir, "index.html"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYBRA_STATIC_DIR", staticDir)

	cfg := config.DefaultConfig()
	cfg.Server.AuthToken = "secret"

	app := sybra.NewApp(testLogger(), nil, cfg)
	handler := cspMiddleware("", corsMiddleware(nil, authMiddleware(cfg.Server.AuthToken, testLogger(), buildMux(testLogger(), sse.New(), app))))

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if body := rr.Body.String(); body != "ok" {
		t.Fatalf("body = %q, want %q", body, "ok")
	}
}

func TestSSEHandlerNeverSetsWildcardCORS(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Server.AuthToken = "secret"
	cfg.Server.AllowedOrigins = []string{"https://allowed.example"}

	app := sybra.NewApp(testLogger(), nil, cfg)
	handler := cspMiddleware("", corsMiddleware(cfg.Server.AllowedOrigins, authMiddleware(cfg.Server.AuthToken, testLogger(), buildMux(testLogger(), sse.New(), app))))

	for _, path := range []string{"/events?token=secret", "/api/events/task:updated?token=secret"} {
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest(http.MethodGet, path, http.NoBody).WithContext(ctx)
		req.Header.Set("Origin", "https://evil.example")
		rr := httptest.NewRecorder()

		done := make(chan struct{})
		go func() {
			handler.ServeHTTP(rr, req)
			close(done)
		}()
		<-time.After(20 * time.Millisecond)
		cancel()
		<-done

		if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("%s: Access-Control-Allow-Origin = %q, want empty for an unlisted origin", path, got)
		}
	}
}

func TestSPAHandlerFallsBackToIndexForUnknownRoute(t *testing.T) {
	staticDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(staticDir, "index.html"), []byte("index"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub, err := fs.Sub(os.DirFS(staticDir), ".")
	if err != nil {
		t.Fatal(err)
	}
	h := spaHandler{FS: http.FileServer(http.FS(sub)), Dir: staticDir}

	// Representative deep links across the frontend's URL-backed routes,
	// including one encoded dynamic segment (a project id containing '/').
	paths := []string{
		"/tasks/123",
		"/tasks",
		"/projects/owner%2Frepo",
		"/chats/agent-1",
		"/agents/agent-1",
		"/workflows/wf-1",
		"/settings",
	}

	for _, path := range paths {
		req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, rr.Code)
		}
		if body := rr.Body.String(); body != "index" {
			t.Fatalf("%s: body = %q, want %q", path, body, "index")
		}
	}
}

// TestRunCheckConfig exercises the -check-config deploy preflight (see
// deploy/bin/sybra-build.sh): it must accept a valid live config and reject
// one with an unknown key, in both cases without starting a server or
// mutating config.yaml on disk (LoadNoPersist, not Load).
func TestRunCheckConfig(t *testing.T) {
	t.Run("valid config", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("SYBRA_HOME", home)

		if code := runCheckConfig(); code != 0 {
			t.Fatalf("runCheckConfig() = %d, want 0", code)
		}
		if _, err := os.Stat(filepath.Join(home, "config.yaml")); !os.IsNotExist(err) {
			t.Fatalf("runCheckConfig must not persist config.yaml, stat err = %v", err)
		}
	})

	t.Run("unknown key rejected", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("SYBRA_HOME", home)
		configPath := filepath.Join(home, "config.yaml")
		if err := os.WriteFile(configPath, []byte("this_key_does_not_exist: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		if code := runCheckConfig(); code != 1 {
			t.Fatalf("runCheckConfig() = %d, want 1 for an unknown config key", code)
		}
		data, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "this_key_does_not_exist: true\n" {
			t.Fatalf("runCheckConfig must not rewrite an invalid config.yaml, got %q", data)
		}
	})
}
