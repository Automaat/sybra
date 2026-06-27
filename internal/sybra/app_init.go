package sybra

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/bgop"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/limits"
	"github.com/Automaat/sybra/internal/loopagent"
	"github.com/Automaat/sybra/internal/monitor"
	"github.com/Automaat/sybra/internal/notification"
	"github.com/Automaat/sybra/internal/poll"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/recovery"
	"github.com/Automaat/sybra/internal/skillsync"
	"github.com/Automaat/sybra/internal/stats"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
	"github.com/Automaat/sybra/internal/watcher"
	"github.com/Automaat/sybra/internal/workflow"
)

func (a *App) initBgops(emit func(string, any)) {
	a.bgops = bgop.NewTracker(emit, filepath.Join(config.HomeDir(), "bgops.json"))
	a.bgops.LoadFromDisk()
}

func (a *App) initFileWatcher(ctx context.Context, emit func(string, any)) {
	w := watcher.New(a.tasksDir, emit, a.logger)
	a.watcher = w
	if err := w.Start(ctx); err != nil {
		a.logger.Error("watcher.start", "err", err)
	}
}

// MonitorReportBinding is the Wails-friendly envelope for the latest
// monitor report. Keeping the struct here (rather than in internal/monitor)
// avoids the frontend bindings needing to handle a `monitor.Report | null`
// union — Enabled/Ready flags say whether Report is populated.
type MonitorReportBinding struct {
	Enabled bool           `json:"enabled"`
	Ready   bool           `json:"ready"`
	Report  monitor.Report `json:"report"`
}

// allowsProjectType reports whether project-scoped automations on this machine
// should act on the given project type. Used to route automation work between
// instances (e.g., pet projects on the server, work projects on the laptop).
func (a *App) allowsProjectType(t project.ProjectType) bool {
	return a.cfg.AllowsProjectType(string(t))
}

// WorkScrubContext carries the redaction blocklist for a task whose project
// is work-typed. Returned from App.workScrubContextForTask; a nil result
// means "not a work-typed task — file artifacts normally". A non-nil result
// signals automations to scrub their output through Blocklist and route to
// a local sybra task instead of the public sybra repo. See CLAUDE.md —
// Work-Data Confidentiality.
type WorkScrubContext struct {
	// ProjectID of the originating work task, retained for tagging and
	// audit only — must not be echoed into any scrubbed artifact body.
	ProjectID string
	// Blocklist of literal strings to redact before persistence. Derived
	// from the project record (owner, repo, full id, repo URL).
	Blocklist []string
}

// workScrubContextForTask reports whether artifacts derived from a task
// must be scrubbed and rerouted to a local sybra task. Returns nil when the
// task is unscoped (no project_id), its project lookup misses, or the
// project is not work-typed. Returns a populated context when the project
// resolves to project.ProjectTypeWork.
//
// Fail-open behaviour (unknown projects → nil) is intentional: the absence
// of a project record means we have no upstream work source to leak, so
// scrubbing would only hide signal without adding safety.
func (a *App) workScrubContextForTask(projectID string) *WorkScrubContext {
	if projectID == "" {
		return nil
	}
	p, err := a.projects.Get(projectID)
	if err != nil {
		return nil
	}
	if p.Type != project.ProjectTypeWork {
		return nil
	}
	bl := []string{p.ID, p.Owner, p.Repo}
	if p.URL != "" {
		bl = append(bl, p.URL)
	}
	return &WorkScrubContext{ProjectID: p.ID, Blocklist: bl}
}

// initIssuesFetcher constructs the GitHub Issues fetcher if enabled, returning
// nil otherwise. Kept separate so Startup stays under the funlen limit.
func (a *App) initIssuesFetcher(emit func(string, any)) *poll.IssuesFetcher {
	if !a.cfg.GitHub.Enabled {
		a.logger.Info("github.disabled")
		return nil
	}
	f := poll.NewIssuesFetcher(a.tasks, a.projects, emit, a.logger, a.allowsProjectType)
	if a.cfg.Umbrella.Enabled {
		model := a.cfg.Umbrella.Model
		f.SetUmbrellaExpander(func(issueURL string) (umbrella.Result, error) {
			return umbrella.Expand(context.Background(), a.tasks, umbrella.ClaudePlannerRunner(model), issueURL)
		})
		a.logger.Info("umbrella.autodetect.enabled")
	}
	return f
}

// logAutomationsSummary logs a one-line snapshot of which automations this
// machine runs. Useful when comparing two instances side by side.
func (a *App) logAutomationsSummary() {
	loopAgentsEnabled := 0
	if a.loopAgents != nil {
		if las, err := a.loopAgents.List(); err == nil {
			for i := range las {
				if las[i].Enabled {
					loopAgentsEnabled++
				}
			}
		}
	}
	projectTypes := a.cfg.ProjectTypes
	if len(projectTypes) == 0 {
		projectTypes = []string{"*"}
	}
	a.logger.Info("app.automations",
		"todoist", a.cfg.Todoist.Enabled && a.cfg.Todoist.APIToken != "",
		"github", a.cfg.GitHub.Enabled,
		"renovate", a.cfg.Renovate.Enabled,
		"triage", a.cfg.Triage.Enabled,
		"human_review", a.humanReview != nil,
		"project_types", projectTypes,
		"loop_agents_enabled", loopAgentsEnabled,
	)
}

func (a *App) initStats() {
	statsStore, err := stats.NewStore(config.StatsFile())
	if err != nil {
		a.logger.Warn("stats.init.degraded", "err", err)
		// a.stats remains nil; StatsService.GetStats() guards against nil.
		return
	}
	a.stats = statsStore
	if err := statsStore.Backfill(a.auditDir); err != nil {
		a.logger.Warn("stats.backfill", "err", err)
	}
}

func (a *App) initLimits() {
	limitStore, err := limits.NewStore(config.LimitsFile())
	if err != nil {
		a.logger.Warn("limits.init.degraded", "err", err)
		return
	}
	a.limits = limitStore
	policy := a.limitPolicy()
	a.agents.SetLimitGate(limitStore, policy)
	a.agents.SetLimitSink(func(snapshot limits.Snapshot) {
		if err := limitStore.UpdateSnapshot(snapshot); err != nil {
			a.logger.Warn("limits.snapshot", "provider", snapshot.Provider, "err", err)
		}
	})
	if policy.Enabled {
		cutoff := time.Now().AddDate(0, 0, -a.cfg.Providers.Limits.BackfillDays)
		backfillCtx := a.ctx
		if backfillCtx == nil {
			backfillCtx = context.Background()
		}
		a.wg.Go(func() {
			a.logger.Info("limits.backfill.start", "cutoff", cutoff)
			if err := limitStore.BackfillLocalSessionFiles(backfillCtx, cutoff); err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				a.logger.Warn("limits.backfill", "err", err)
				return
			}
			a.logger.Info("limits.backfill.done")
		})
	}
}

func (a *App) limitPolicy() limits.Policy {
	p := limits.DefaultPolicy()
	p.Enabled = a.cfg.Providers.Limits.Enabled
	p.SessionThresholdPercent = a.cfg.Providers.Limits.SessionThresholdPercent
	p.WeeklyThresholdPercent = a.cfg.Providers.Limits.WeeklyThresholdPercent
	p.PreferUnderused = a.cfg.Providers.Limits.PreferUnderused
	p.SubscriptionMonthlyUSD = map[string]float64{
		"claude":  a.cfg.Providers.Claude.MonthlySubscriptionUSD,
		"codex":   a.cfg.Providers.Codex.MonthlySubscriptionUSD,
		"copilot": a.cfg.Providers.Copilot.MonthlySubscriptionUSD,
	}
	p.ProviderEnabled = map[string]bool{
		"claude":  a.cfg.Providers.Claude.Enabled,
		"codex":   a.cfg.Providers.Codex.Enabled,
		"copilot": a.cfg.Providers.Copilot.Enabled,
	}
	return p
}

func (a *App) initStatusHook() {
	a.tasks.SetStatusChangeHook(func(taskID, from, to string) {
		a.logAudit(audit.EventTaskStatusChanged, taskID, "", map[string]any{"from": from, "to": to})

		// Wake the dispatch pass immediately so a task that just became ready
		// (e.g. a dependency completing, a stage advancing) is picked up now
		// instead of waiting for the next fast tick.
		a.nudgeDispatch()

		// Advance workflows whose current run_agent step declares a
		// matching wait_for_status. This is how interactive agents (which
		// never exit between turns) signal step completion.
		if a.workflowEngine != nil {
			a.workflowEngine.HandleStatusChange(taskID, to)
		}

		switch to {
		case string(task.StatusInReview):
			msg := taskID
			if t, err := a.tasks.Get(taskID); err == nil {
				msg = t.Title
			}
			if a.notifier != nil {
				a.notifier.Send(notification.LevelInfo, "Ready for review", msg, taskID, "")
			}
		case string(task.StatusHumanRequired):
			msg := taskID
			if t, err := a.tasks.Get(taskID); err == nil {
				msg = t.Title
			}
			if a.notifier != nil {
				a.notifier.Send(notification.LevelWarning, "Needs human", msg, taskID, "")
			}
			if a.humanReview != nil {
				go a.humanReview.maybeSpawn(taskID, from)
			}
		case string(task.StatusReadyReview):
			if a.workflowEngine != nil {
				// ErrWorkflowAlreadyActive is benign: when the cascade flips to
				// ready-review (simple-task-implement ending), this hook fires
				// while the implement workflow is still active; OnWorkflowComplete
				// drives the cascade instead. Only real errors are surfaced —
				// this also enables the manual "move card to Ready Review" path.
				if _, err := a.workflowEngine.DispatchEvent(
					taskID,
					"task.status_changed",
					map[string]string{"task.status": string(task.StatusReadyReview)},
					nil,
				); err != nil && !errors.Is(err, workflow.ErrWorkflowAlreadyActive) {
					a.logger.Error("workflow.dispatch.ready-review", "task_id", taskID, "err", err)
				}
			}
		case string(task.StatusTesting):
			if a.workflowEngine != nil {
				// ErrWorkflowAlreadyActive is benign and the COMMON case: when
				// simple-task-review's done_review flips to testing, this hook
				// fires while the review workflow is still active, so the start
				// is rejected here and the cascade is driven by OnWorkflowComplete
				// instead. Only a real, unexpected error is worth surfacing —
				// this also serves the genuine manual "move card to Testing" path.
				if _, err := a.workflowEngine.DispatchEvent(
					taskID,
					"task.status_changed",
					map[string]string{"task.status": string(task.StatusTesting)},
					nil,
				); err != nil && !errors.Is(err, workflow.ErrWorkflowAlreadyActive) {
					a.logger.Error("workflow.dispatch.testing", "task_id", taskID, "err", err)
				}
			}
		}
	})
}

// maybeStartWorkflowForExternalTask starts the matching task.created workflow
// for a task that appeared on disk outside the GUI CreateTask path — most
// importantly via sybra-cli, the documented primary task interface. Without
// this, CLI-created tasks never get a workflow and sit inert in todo: the
// orchestrator can triage them but no implementation ever starts. Mirrors
// TaskService.startCreatedWorkflow. Idempotent: DispatchEvent serializes per
// task and rejects a task that already owns a non-terminal workflow, so the
// watcher firing TaskCreated several times for one file is harmless.
func (a *App) maybeStartWorkflowForExternalTask(path string) {
	if a.workflowEngine == nil || a.tasks == nil || a.agents == nil {
		return
	}
	base := filepath.Base(path)
	// Sidecar files (plan/critique/review) share the tasks dir and also fire
	// TaskCreated; they are not tasks, so skip them up front rather than
	// spending a goroutine + Get that would fail anyway.
	if task.IsSidecarFile(base) {
		return
	}
	id := strings.TrimSuffix(base, ".md")
	if id == "" {
		return
	}
	a.wg.Go(func() {
		t, err := a.tasks.Get(id)
		if err != nil {
			return
		}
		// Only fresh, pre-implementation tasks. simple-task-plan's trigger has
		// no status condition, so without this guard a task.created dispatch
		// could restart planning on an in-review/done task.
		if t.Status != task.StatusNew && t.Status != task.StatusTodo {
			return
		}
		// pr-fix / ordinary existing-PR tasks are driven outside task.created.
		// Explicit handoff entry points are the exception: they intentionally
		// route through task.created even when a PR number is already known.
		if skipTaskCreatedWorkflow(t) {
			return
		}
		if t.Workflow != nil &&
			t.Workflow.State != "" &&
			t.Workflow.State != workflow.ExecCompleted &&
			t.Workflow.State != workflow.ExecFailed {
			return
		}
		if a.agents.HasRunningAgentForTask(id) {
			return
		}
		if _, err := a.workflowEngine.DispatchEvent(id, "task.created", nil, nil); err != nil &&
			!errors.Is(err, workflow.ErrWorkflowAlreadyActive) {
			a.logger.Error("workflow.external-create.failed", "task_id", id, "err", err)
		}
	})
}

func (a *App) initAudit() {
	al, err := audit.NewLogger(a.auditDir)
	if err != nil {
		a.logger.Warn("audit.init.degraded", "err", err)
		// a.audit remains nil; logAudit() is a no-op when audit is nil.
		return
	}
	a.audit = al
	retentionDays := a.cfg.Audit.RetentionDays
	if retentionDays <= 0 {
		retentionDays = 30
	}
	if err := audit.Cleanup(a.auditDir, retentionDays); err != nil {
		a.logger.Warn("audit.cleanup", "err", err)
	}
}

// initArtifacts constructs the artifact store, wires the task delete hook to
// GC artifact directories on task deletion, and sweeps orphaned artifact
// directories left by tasks that no longer exist.
func (a *App) initArtifacts() {
	a.artifacts = artifact.New(config.ArtifactsDir())
	a.tasks.SetDeleteHook(func(id string) {
		if err := a.artifacts.Delete(id); err != nil {
			a.logger.Warn("artifact.gc.delete", "task_id", id, "err", err)
		}
	})
	ids, err := a.artifacts.ListTaskIDs()
	if err != nil {
		a.logger.Warn("artifact.gc.list", "err", err)
		return
	}
	for _, id := range ids {
		if _, getErr := a.tasks.Get(id); getErr != nil {
			if delErr := a.artifacts.Delete(id); delErr != nil {
				a.logger.Warn("artifact.gc.orphan-sweep", "task_id", id, "err", delErr)
				continue
			}
			a.logger.Info("artifact.orphan.swept", "task_id", id)
		}
	}
}

// initProviderHealth constructs the provider health checker, wires it into
// the agent manager as a gate, and starts its background probe loop. When
// providers.health_check.enabled=false the checker is skipped entirely and
// the manager runs with a nil gate (no blocking).
func (a *App) initProviderHealth(ctx context.Context, emit func(string, any)) {
	if !a.cfg.Providers.HealthCheck.Enabled {
		a.logger.Info("provider.health.disabled")
		return
	}
	pc := provider.New(provider.Config{
		Interval:          time.Duration(a.cfg.Providers.HealthCheck.IntervalSeconds) * time.Second,
		ClaudeEnabled:     a.cfg.Providers.Claude.Enabled,
		CodexEnabled:      a.cfg.Providers.Codex.Enabled,
		CopilotEnabled:    a.cfg.Providers.Copilot.Enabled,
		AutoFailover:      a.cfg.Providers.AutoFailover,
		ClaudeRLCooldown:  time.Duration(a.cfg.Providers.Claude.RateLimitCooldownSeconds) * time.Second,
		CodexRLCooldown:   time.Duration(a.cfg.Providers.Codex.RateLimitCooldownSeconds) * time.Second,
		CopilotRLCooldown: time.Duration(a.cfg.Providers.Copilot.RateLimitCooldownSeconds) * time.Second,
	}, emit, a.logger)
	a.providerHealth = pc
	a.agents.SetHealthGate(pc)
	a.wg.Go(func() { pc.Run(ctx) })
}

// emitDegradedWarnings fires startup:degraded for any subsystem that failed
// to initialize. Called after emit is configured so the frontend receives the events.
func (a *App) emitDegradedWarnings(emit func(string, any)) {
	type degraded struct {
		Subsystem string `json:"subsystem"`
		Reason    string `json:"reason"`
	}
	if a.audit == nil {
		emit(events.StartupDegraded, degraded{"audit", "audit logger failed to initialize; audit trail unavailable"})
	}
	if a.stats == nil {
		emit(events.StartupDegraded, degraded{"stats", "stats store failed to initialize; metrics unavailable"})
	}
}

// initAutomations starts every per-machine task source in dependency order
// and returns the GitHub issues fetcher (still consumed by
// startBackgroundServices). Extracted so Startup stays under funlen.
func (a *App) initAutomations(emit func(string, any)) *poll.IssuesFetcher {
	a.initTodoist(emit)
	a.initRenovate(emit)
	a.initTriage()
	a.initHumanReview()
	return a.initIssuesFetcher(emit)
}

func (a *App) initWorkflowEngine() {
	if os.Getenv("SYBRA_DISABLE_WORKFLOWS") == "1" {
		a.logger.Info("workflow.disabled")
		return
	}
	wfStore, err := workflow.NewStore(config.WorkflowsDir())
	if err != nil {
		a.logger.Error("workflow.store.init", "err", err)
		return
	}
	a.workflowStore = wfStore
	if syncErr := workflow.SyncBuiltins(wfStore); syncErr != nil {
		a.logger.Error("workflow.sync-builtins", "err", syncErr)
	}
	a.workflowEngine = workflow.NewEngine(
		wfStore,
		&taskAdapter{tasks: a.tasks, projects: a.projects},
		&agentAdapter{agents: a.agents, agentOrch: a.agentOrch, tasks: a.tasks, sandboxes: a.sandboxes},
		a.logger,
	)
	a.workflowEngine.SetPRLinker(prLinkerAdapter{})
	a.workflowEngine.SetPRReviewRequester(prReviewRequesterAdapter{})
	a.workflowEngine.SetWorktreeGetter(&worktreeGetterAdapter{tasks: a.tasks, mgr: a.worktrees})
	a.workflowEngine.SetCheckConfigGetter(&checkConfigGetterAdapter{tasks: a.tasks, projects: a.projects, mgr: a.worktrees})
	a.workflowEngine.SetTestingMaxAttempts(a.cfg.TestingMaxAttempts())
	a.workflowEngine.SetABTestingConfig(a.cfg.ABTesting)
	if a.artifacts != nil {
		a.workflowEngine.SetArtifactRecorder(&artifactRecorderAdapter{store: a.artifacts})
	}
	a.workflowEngine.SetContext(a.ctx)
	// SetOnComplete moves to wireServices so the callback closure binds
	// to the AgentCompletionHandler constructed there.
}

func (a *App) initAgentConfig() {
	a.agents.SetMaxConcurrent(a.cfg.Agent.MaxConcurrent)
	a.agents.SetBashTimeoutMs(a.cfg.BashTimeoutMs())
	a.agents.SetRetryWatchdog(a.cfg.RetryWatchdog())
	a.agents.SetFallbackModel(a.cfg.Agent.FallbackModel)
	if a.cfg.SurviveRestartEnabled() {
		if err := a.agents.EnableSurviveRestart(config.AgentsDir()); err != nil {
			a.logger.Error("agent.survive-restart.init", "err", err)
		} else {
			a.logger.Info("agent.survive-restart.enabled", "dir", config.AgentsDir())
		}
		// Bridge a crashed agent's session id into its AgentRun on
		// dead-reattach so restart-stale recovery resumes via --resume. The
		// error is returned (not swallowed) so the manager retains the
		// registry record for retry and logs the failure at Warn.
		a.agents.SetSessionSink(func(taskID, agentID, sessionID string) error {
			return a.tasks.UpdateRun(taskID, agentID, map[string]any{"session_id": sessionID})
		})
		// Skip recreating a codex chat agent whose task was deleted. Only a
		// definite not-exist returns false; a transient error fails open
		// (retain the record) so a surviving chat is not lost to a flaky read.
		a.agents.SetTaskExists(func(taskID string) bool {
			_, err := a.tasks.Get(taskID)
			if err == nil {
				return true
			}
			if errors.Is(err, os.ErrNotExist) {
				return false
			}
			a.logger.Warn("agent.task-exists.error", "task_id", taskID, "err", err)
			return true
		})
	}
	a.agents.SetGuardrails(agent.Guardrails{
		MaxCostUSD:       a.cfg.Agent.MaxCostUSD,
		MaxTurns:         a.cfg.Agent.MaxTurns,
		TurnCostFraction: a.cfg.Agent.TurnCostFraction,
		TurnMultiplier:   a.cfg.Agent.TurnMultiplier,
	})
}

func (a *App) initApprovalServer(emit func(string, any)) {
	srv, err := agent.NewApprovalServer(emit, a.logger, a.cfg.Agent.ApprovalPort)
	if err != nil {
		a.logger.Error("approval-server.init", "err", err)
		return
	}
	srv.SetManager(a.agents)
	a.agents.SetApprovalAddr(srv.Addr())
	a.agentSvc.approval = srv
}

func (a *App) logAudit(eventType, taskID, agentID string, data map[string]any) {
	if a.audit == nil {
		return
	}
	if err := a.audit.Log(audit.Event{
		Type:    eventType,
		TaskID:  taskID,
		AgentID: agentID,
		Data:    data,
	}); err != nil {
		a.logger.Error("audit.log", "type", eventType, "err", err)
	}
}

// seedDefaultLoopAgents creates the built-in sybra-self-monitor loop on
// first boot only. It is disabled by default so the user can review the
// configuration in the GUI before enabling. Idempotent: if a record with
// the same Name already exists this is a no-op.
func (a *App) initLoopAgents() error {
	store, err := loopagent.NewStore(a.cfg.LoopAgentsDir)
	if err != nil {
		a.logger.Error("loopagent.store.init", "err", err)
		return err
	}
	a.loopAgents = store
	return nil
}

func (a *App) initLoopScheduler(ctx context.Context, emit func(string, any)) {
	a.loopSched = loopagent.NewScheduler(ctx, a.loopAgents, a.agents, a.logger, emit, config.HomeDir())
	a.seedDefaultLoopAgents()
	a.loopSched.Sync()
}

func (a *App) seedDefaultLoopAgents() {
	if a.loopAgents == nil {
		return
	}
	const name = "sybra-self-monitor"
	if _, ok := a.loopAgents.FindByName(name); ok {
		return
	}
	created, err := a.loopAgents.Create(loopagent.LoopAgent{
		Name:         name,
		Prompt:       "/sybra-self-monitor",
		IntervalSec:  21600, // 6 hours
		AllowedTools: []string{"Bash", "Read", "Grep", "Glob"},
		Provider:     "claude",
		Model:        "sonnet",
		Enabled:      false,
	})
	if err != nil {
		a.logger.Warn("loopagent.seed.failed", "name", name, "err", err)
		return
	}
	a.logger.Info("loopagent.seed.created", "id", created.ID, "name", name)
}

// newRecovery wires the App's deps into a recovery.Recovery used for
// boot-time cleanup and the periodic restart-stale sweep called from the
// orchestrator loop. Holds a pointer to a.restartStaleErr so the throttle
// state is shared across both call sites.
func (a *App) newRecovery() *recovery.Recovery {
	var retention time.Duration
	if days := a.cfg.DefaultLogRetentionDays(); days > 0 {
		retention = time.Duration(days) * 24 * time.Hour
	}
	return &recovery.Recovery{
		Tasks:          a.tasks,
		Agents:         a.agents,
		Worktrees:      a.worktrees,
		WorkflowEngine: a.workflowEngine,
		Orchestrator:   a.agentOrch,
		Projects:       a.projects,
		Logger:         a.logger,
		Throttle:       a.restartStaleErr,
		WG:             &a.wg,
		LogDir:         a.logDir,
		LogRetention:   retention,
	}
}

// syncSkillsBundle drives the skillsync package with the App's source/dst
// configuration. UserHomeDir is best-effort — when unavailable the user-home
// destinations (~/.claude/skills, ~/.codex/skills) are silently skipped so
// startup still succeeds in environments without a usable home dir.
func (a *App) syncSkillsBundle() {
	userHome, err := os.UserHomeDir()
	if err != nil {
		a.logger.Debug("skills.sync.no_user_home", "err", err)
		userHome = ""
	}
	(&skillsync.Syncer{Logger: a.logger}).Run(skillsync.Options{
		RepoDir:      a.repoDir,
		SkillsFS:     a.skillsFS,
		PrimaryDst:   a.skillsDir,
		SybraHomeDir: config.HomeDir(),
		UserHomeDir:  userHome,
	})
}
