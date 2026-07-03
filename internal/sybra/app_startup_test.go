package sybra

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/events"
)

func TestAppStartupWiresSubsystemsAndServices(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)
	t.Setenv("SYBRA_DISABLE_WORKFLOWS", "0")

	cfg := startupTestConfig(home)
	logger := slog.New(slog.DiscardHandler)
	logLevel := &slog.LevelVar{}

	var emitted startupEventRecorder
	app := NewApp(logger, logLevel, cfg, WithEmit(func(event string, _ any) {
		emitted.append(event)
	}))

	// NewApp must pre-allocate the bound service structs before Startup so
	// desktop binding generation can see stable service targets.
	if app.taskSvc == nil || app.agentSvc == nil || app.projectSvc == nil || app.configSvc == nil {
		t.Fatal("NewApp did not pre-allocate bound services")
	}

	if err := app.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	t.Cleanup(func() {
		if app.agentSvc != nil && app.agentSvc.approval != nil {
			_ = app.agentSvc.approval.Shutdown(context.Background())
		}
		app.Shutdown(context.Background())
	})

	assertStartupCoreWiring(t, app, logger, cfg, logLevel)
	assertStartupServiceWiring(t, app)

	emittedEvents := emitted.snapshot()
	for _, event := range emittedEvents {
		if event == events.StartupDegraded {
			t.Fatalf("isolated startup emitted degraded event: %v", emittedEvents)
		}
	}
}

type startupEventRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *startupEventRecorder) append(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *startupEventRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func TestAppShutdownBeforeStartupDoesNotPanic(t *testing.T) {
	app := NewApp(slog.New(slog.DiscardHandler), &slog.LevelVar{}, startupTestConfig(t.TempDir()))

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Shutdown before Startup panicked: %v", r)
		}
	}()

	app.Shutdown(context.Background())
}

func startupTestConfig(home string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.TasksDir = filepath.Join(home, "tasks")
	cfg.SkillsDir = filepath.Join(home, "claude", "skills")
	cfg.RepoDir = home
	cfg.ProjectsDir = filepath.Join(home, "projects")
	cfg.ClonesDir = filepath.Join(home, "clones")
	cfg.WorktreesDir = filepath.Join(home, "worktrees")
	cfg.LoopAgentsDir = filepath.Join(home, "loop-agents")
	cfg.Logging.Dir = filepath.Join(home, "logs")

	// Keep every background automation disabled; this test only verifies startup wiring.
	cfg.Notification.Desktop = false
	cfg.GitHub.Enabled = false
	cfg.Renovate.Enabled = false
	cfg.Todoist.Enabled = false
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

func assertStartupCoreWiring(t *testing.T, app *App, logger *slog.Logger, cfg *config.Config, logLevel *slog.LevelVar) {
	t.Helper()

	if app.ctx == nil || app.cancel == nil {
		t.Fatal("startup context was not installed")
	}
	if app.cfg != cfg || app.logger != logger || app.logLevel != logLevel {
		t.Fatal("constructor-supplied app dependencies were not retained")
	}
	if app.tasks == nil {
		t.Fatal("task manager is nil")
	}
	if app.projects == nil {
		t.Fatal("project store is nil")
	}
	if app.loopAgents == nil || app.loopSched == nil {
		t.Fatal("loop agent store/scheduler were not initialized")
	}
	if app.bgops == nil {
		t.Fatal("background operation tracker is nil")
	}
	if app.agents == nil {
		t.Fatal("agent manager is nil")
	}
	if app.providerHealth != nil {
		t.Fatal("provider health checker should be nil when disabled")
	}
	if app.worktrees == nil || app.sandboxes == nil {
		t.Fatal("worktree/sandbox managers were not initialized")
	}
	if app.agentOrch == nil || app.reviewer == nil {
		t.Fatal("agent orchestrator/reviewer were not initialized")
	}
	if app.workflowEngine == nil || app.workflowStore == nil {
		t.Fatal("workflow engine/store were not initialized")
	}
	if app.agentCompletion == nil || app.recovery == nil {
		t.Fatal("completion/recovery handlers were not initialized")
	}
	if app.watcher == nil || app.configWatcher == nil {
		t.Fatal("file/config watchers were not started")
	}
	if app.notifier == nil || app.artifacts == nil || app.experience == nil || app.stats == nil || app.limits == nil {
		t.Fatal("support stores were not initialized")
	}
}

func assertStartupServiceWiring(t *testing.T, app *App) {
	t.Helper()

	if app.taskSvc.tasks != app.tasks || app.taskSvc.agents != app.agents || app.taskSvc.workflowEngine != app.workflowEngine {
		t.Fatal("task service was not wired after core managers")
	}
	if app.agentSvc.agents != app.agents || app.agentSvc.tasks != app.tasks || app.agentSvc.worktrees != app.worktrees {
		t.Fatal("agent service was not wired after agent/worktree managers")
	}
	if app.orchSvc.agents != app.agents || app.orchSvc.audit != app.audit {
		t.Fatal("orchestrator service was not wired")
	}
	if app.projectSvc.projects != app.projects || app.projectSvc.worktrees != app.worktrees || app.projectSvc.bgops != app.bgops {
		t.Fatal("project service was not wired")
	}
	if app.loopAgentSvc.store != app.loopAgents || app.loopAgentSvc.sched != app.loopSched {
		t.Fatal("loop-agent service was not wired")
	}
	if app.configSvc.cfg != app.cfg || app.configSvc.agents != app.agents || app.configSvc.limits != app.limits {
		t.Fatal("config service was not wired")
	}
	if app.intgSvc.tasks != app.tasks || app.intgSvc.projects != app.projects || app.intgSvc.workflowEngine != app.workflowEngine {
		t.Fatal("integration service was not wired")
	}
	if app.reviewSvc.reviewer != app.reviewer || app.reviewSvc.tasks != app.tasks {
		t.Fatal("review service was not wired")
	}
	if app.workflowSvc.engine != app.workflowEngine || app.workflowSvc.store != app.workflowStore {
		t.Fatal("workflow service was not wired")
	}
	if app.reviewer.WorkflowEngine != app.workflowEngine {
		t.Fatal("reviewer was not back-wired to the workflow engine")
	}
	if app.agentOrch.Sandboxes != app.sandboxes || app.agentOrch.Bgops != app.bgops {
		t.Fatal("agent orchestrator was not fully wired")
	}
	// agentOrch is constructed before reviewer (see the Startup dependency-graph
	// comment atop app.go), so the reverse wire — reviewer.RecoverStaleBranchConflict
	// feeding agentOrch.ConflictRecovery — can only happen after both exist. This
	// pins that ordering: a nil ConflictRecovery here means the two packages'
	// wiring order in app_init.go regressed silently.
	if app.agentOrch.ConflictRecovery == nil {
		t.Fatal("agentOrch.ConflictRecovery was not back-wired from reviewer.RecoverStaleBranchConflict")
	}
	if app.statsSvc.stats != app.stats || app.statsSvc.limits != app.limits || app.statsSvc.projects != app.projects {
		t.Fatal("stats service was not wired")
	}
}
