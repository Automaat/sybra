package sybra

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/agentgrant"
	"github.com/Automaat/sybra/internal/agentqueue"
	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/attachment"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/backoff"
	"github.com/Automaat/sybra/internal/bgop"
	"github.com/Automaat/sybra/internal/cleanup"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/diskreclaim"
	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/evidence"
	"github.com/Automaat/sybra/internal/experience"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/httpserve"
	"github.com/Automaat/sybra/internal/intervention"
	"github.com/Automaat/sybra/internal/learning"
	"github.com/Automaat/sybra/internal/limits"
	"github.com/Automaat/sybra/internal/loopagent"
	"github.com/Automaat/sybra/internal/monitor"
	"github.com/Automaat/sybra/internal/notification"
	"github.com/Automaat/sybra/internal/poll"
	"github.com/Automaat/sybra/internal/prcontent"
	"github.com/Automaat/sybra/internal/pressure"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/prompteval"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/recovery"
	"github.com/Automaat/sybra/internal/sandbox"
	"github.com/Automaat/sybra/internal/skillsync"
	"github.com/Automaat/sybra/internal/stats"
	"github.com/Automaat/sybra/internal/sybra/clusterlead"
	"github.com/Automaat/sybra/internal/sybra/dispatch"
	"github.com/Automaat/sybra/internal/sybra/review"
	"github.com/Automaat/sybra/internal/sybra/runenv"
	"github.com/Automaat/sybra/internal/sybra/verification"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/task/taskdb"
	"github.com/Automaat/sybra/internal/taskstatus"
	"github.com/Automaat/sybra/internal/toolledger"
	"github.com/Automaat/sybra/internal/umbrella"
	"github.com/Automaat/sybra/internal/watcher"
	"github.com/Automaat/sybra/internal/workercontrol"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/workflowpr"
)

const (
	liveLimitPollInterval       = 15 * time.Minute
	liveLimitPollAuthBackoffMax = 2 * time.Hour
)

type liveLimitPollState struct {
	next               map[string]time.Time
	claudeAuthFailures int
	claudeAuthOpen     bool
}

func newLiveLimitPollState(now time.Time) liveLimitPollState {
	return liveLimitPollState{
		next: map[string]time.Time{
			limits.ProviderClaude: now,
			limits.ProviderCodex:  now,
		},
	}
}

func (s *liveLimitPollState) dueProviders(now time.Time, policy limits.Policy) []string {
	out := make([]string, 0, 2)
	for _, provider := range []string{limits.ProviderClaude, limits.ProviderCodex} {
		if !liveLimitProviderEnabled(policy, provider) {
			continue
		}
		next := s.next[provider]
		if next.IsZero() || !next.After(now) {
			out = append(out, provider)
		}
	}
	return out
}

func (s *liveLimitPollState) nextWait(now time.Time, policy limits.Policy) time.Duration {
	next := time.Time{}
	for _, provider := range []string{limits.ProviderClaude, limits.ProviderCodex} {
		if !liveLimitProviderEnabled(policy, provider) {
			continue
		}
		candidate := s.next[provider]
		if candidate.IsZero() || candidate.Before(next) || next.IsZero() {
			next = candidate
		}
	}
	if next.IsZero() {
		return liveLimitPollInterval
	}
	if !next.After(now) {
		return 0
	}
	return next.Sub(now)
}

func (s *liveLimitPollState) recordResult(now time.Time, result limits.LiveRefreshResult, limitStore *limits.Store, logger *slog.Logger) {
	if result.ImportErr != nil {
		logger.Warn("limits.live_poll", "err", result.ImportErr)
	}
	if codex := result.Provider(limits.ProviderCodex); codex.Attempted {
		s.next[limits.ProviderCodex] = now.Add(liveLimitPollInterval)
		if codex.Err != nil {
			logger.Warn("limits.live_poll", "provider", limits.ProviderCodex, "err", codex.Err)
		}
	}

	claude := result.Provider(limits.ProviderClaude)
	if !claude.Attempted {
		return
	}
	if claude.Err == nil {
		s.next[limits.ProviderClaude] = now.Add(liveLimitPollInterval)
		if s.claudeAuthOpen {
			logger.Info("limits.live_poll.claude_auth_recovered")
		}
		s.claudeAuthFailures = 0
		s.claudeAuthOpen = false
		return
	}
	if limits.IsLivePollAuthError(claude.Err, limits.ProviderClaude) {
		if err := limitStore.InvalidateLiveExactSnapshot(limits.ProviderClaude); err != nil {
			logger.Warn("limits.live_poll.invalidate", "provider", limits.ProviderClaude, "err", err)
		}
		s.claudeAuthFailures++
		retryDelay := liveLimitAuthBackoff(s.claudeAuthFailures)
		s.next[limits.ProviderClaude] = now.Add(retryDelay)
		if !s.claudeAuthOpen {
			logger.Warn("limits.live_poll.claude_auth", "backoff", retryDelay, "err", claude.Err)
			s.claudeAuthOpen = true
		}
		return
	}
	if s.claudeAuthOpen && s.claudeAuthFailures > 0 {
		s.next[limits.ProviderClaude] = now.Add(liveLimitAuthBackoff(s.claudeAuthFailures))
	} else {
		s.next[limits.ProviderClaude] = now.Add(liveLimitPollInterval)
	}
	logger.Warn("limits.live_poll", "provider", limits.ProviderClaude, "err", claude.Err)
}

func liveLimitAuthBackoff(failures int) time.Duration {
	return backoff.ForAttempt(max(failures, 1), liveLimitPollInterval, liveLimitPollAuthBackoffMax).Delay
}

func liveLimitProviderEnabled(policy limits.Policy, providerName string) bool {
	if len(policy.ProviderEnabled) == 0 {
		return true
	}
	enabled, ok := policy.ProviderEnabled[providerName]
	return ok && enabled
}

type startupDegradedEvent struct {
	Subsystem string `json:"subsystem"`
	Reason    string `json:"reason"`
}

func (a *App) initBgops(ctx context.Context, emit func(string, any)) {
	a.bgops = bgop.NewTracker(emit, a.openBgopStore(ctx), a.logger)
	a.bgops.LoadFromDisk()
}

// initFileWatcher starts the tasks-directory watcher only for the file backend: every Manager mutation already emits task:created/updated/deleted directly (see eventPath's doc comment), so on the database backend there is no file for the watcher to see and nothing it would be first to notice.
func (a *App) initFileWatcher(ctx context.Context, emit func(string, any)) {
	if a.currentConfig().DatabaseEnabled() {
		return
	}
	w := watcher.New(a.tasksDir, emit, a.logger)
	if err := w.Start(ctx); err != nil {
		a.logger.Error("watcher.start", "err", err)
		return
	}
	a.watcher = w
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
	bl := p.WorkBlocklist()
	if bl == nil {
		return nil
	}
	return &WorkScrubContext{ProjectID: p.ID, Blocklist: bl}
}

// initIssuesFetcher constructs the GitHub Issues fetcher if enabled, returning
// nil otherwise. Kept separate so Startup stays under the funlen limit.
func (a *App) initIssuesFetcher(emit func(string, any)) *poll.IssuesFetcher {
	if !a.cfg.GitHub.RunsIssuesFetcher() {
		a.logger.Info("github.issues.disabled",
			"github_enabled", a.cfg.GitHub.Enabled,
			"issues_polling_enabled", a.cfg.GitHub.Polling.Issues.Enabled,
		)
		return nil
	}
	f := poll.NewIssuesFetcher(a.tasks, a.projects, emit, a.logger, a.allowsProjectType)
	f.SetPollInterval(a.cfg.GitHub.Issues())
	if phrase := strings.TrimSpace(a.cfg.GitHub.MentionTriggerPhrase); phrase != "" {
		f.SetMentionTrigger(phrase)
		a.logger.Info("github.issues.mention-trigger.enabled", "phrase", phrase)
	}
	if a.cfg.Umbrella.Enabled {
		model := a.cfg.Umbrella.Model
		ground := a.cfg.Umbrella.Ground
		minSubs := a.cfg.Umbrella.GroundMinSubIssues
		f.SetUmbrellaExpander(func(issueURL string) (umbrella.Result, error) {
			var opts []umbrella.ExpandOption
			if ground {
				opts = append(opts, umbrella.WithExpandGrounder(buildGroundLister(a.projects), minSubs))
			}
			return umbrella.Expand(a.ctx, a.tasks, umbrella.FallbackPlannerRunner(model, a.providerHealth), issueURL, opts...)
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
		if las, err := a.loopAgents.List(a.ctx); err == nil {
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
	promptevalRunner := prompteval.SelectRunner(a.cfg.Evaluation.Offline)
	a.logger.Info("app.automations",
		"instance_role", a.cfg.Orchestrator.InstanceRole(),
		"orchestrator", a.cfg.Orchestrator.RunsOrchestrator(),
		"scheduler", a.cfg.Orchestrator.RunsScheduler(),
		"github", a.cfg.GitHub.Enabled,
		"github_issues", a.cfg.GitHub.RunsIssuesFetcher(),
		"github_sybra_prs", a.cfg.GitHub.RunsSybraPRs(),
		"github_sybra_pr_searches", a.cfg.GitHub.RunsSybraPRSearches(),
		"github_assigned_prs", a.cfg.GitHub.RunsAssignedPRs(),
		"github_assigned_pr_searches", a.cfg.GitHub.RunsAssignedPRSearches(),
		"renovate", a.cfg.Renovate.Enabled,
		"triage", a.cfg.Triage.Enabled,
		"human_review", a.humanReview != nil,
		"project_types", projectTypes,
		"sandbox_mode", a.cfg.DefaultSandboxMode(),
		"loop_agents_enabled", loopAgentsEnabled,
		"prompteval_runner", promptevalRunner.Name(),
		"promptfoo_present", (&prompteval.PromptfooRunner{}).Available(),
		"providers", a.cfg.Providers.EnabledNames(),
	)
	a.warnThinFailoverChain()
}

// warnThinFailoverChain says at startup when this instance has fewer than two
// providers it can actually dispatch to. A one-leg chain has no failover at
// all, and that is how one weekly limit plus one usage limit turned into a dead
// board on 2026-08-05 — a state that was only visible by grepping the app log
// for provider.health.flip after the fact.
//
// Health is meaningful here because initProviderHealth's ProbeOnce has already
// run by the time the summary is logged; a nil checker means health checking is
// off, and only the configured count is knowable.
func (a *App) warnThinFailoverChain() {
	enabled := a.cfg.Providers.EnabledNames()
	switch len(enabled) {
	case 0:
		a.logger.Error("app.providers.none-enabled",
			"detail", "no provider is enabled; this instance cannot dispatch at all")
	case 1:
		a.logger.Warn("app.providers.no-failover",
			"enabled", enabled,
			"detail", "one provider enabled; a single rate limit stalls the board")
	}
	if a.providerHealth == nil {
		return
	}
	snap := a.providerHealth.Snapshot()
	healthy := make([]string, 0, len(enabled))
	for _, name := range enabled {
		if st, ok := snap[name]; ok && st.Healthy {
			healthy = append(healthy, name)
		}
	}
	switch {
	case len(healthy) == 0:
		a.logger.Error("app.providers.no-capacity",
			"enabled", enabled,
			"detail", "no enabled provider is healthy; nothing can dispatch")
	case len(healthy) < 2:
		a.logger.Warn("app.providers.thin-capacity",
			"enabled", enabled, "healthy", healthy,
			"detail", "only one healthy provider; a single rate limit stalls the board")
	default:
		a.logger.Info("app.providers.capacity", "enabled", enabled, "healthy", healthy)
	}
}

func (a *App) initStats(ctx context.Context) {
	if a.database != nil {
		importCtx, cancel := context.WithTimeout(ctx, importTimeout)
		defer cancel()
		if err := stats.Import(importCtx, a.database, config.StatsFile(), a.importScope(), a.logger); err != nil {
			a.logger.Error("stats.import", "err", err)
		} else if store, err := stats.NewSQLStore(a.database, a.logger); err != nil {
			a.logger.Error("stats.store.init", "backend", a.cfg.DatabaseBackend(), "err", err)
		} else {
			a.stats = store
			return
		}
	}
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

// initLocalStores wires the small local-only data stores that degrade to nil
// on failure rather than blocking startup.
func (a *App) initLocalStores(ctx context.Context) {
	a.initAttachments()
	a.initArtifacts()
	a.verification = verification.New(filepath.Join(config.HomeDir(), "verification"), a.artifacts, a.logger)
	a.initExperience(ctx)
	a.initIntervention()
	a.initLearning()
	a.initAgentQueue(ctx)
}

func (a *App) initAttachments() {
	store, err := attachment.NewStore(a.cfg.AttachmentsDir(), int64(a.cfg.Attachments.MaxSizeMB)*1024*1024)
	if err != nil {
		a.logger.Warn("attachments.init.degraded", "err", err)
		return
	}
	a.attachments = store
	a.tasks.SetDeleteHook(func(id string) {
		if err := store.DeleteTask(id); err != nil {
			a.logger.Warn("attachments.gc.delete", "task_id", id, "err", err)
		}
	})
}

func (a *App) initExperience(ctx context.Context) {
	dir := a.cfg.ExperiencesDir()
	if a.database != nil {
		if err := experience.Import(ctx, a.database, dir, a.importScope(), a.logger); err != nil {
			// Degrade to files rather than start on a half-populated table: an
			// empty advisory memory reads as "this project has no history",
			// which is a worse answer than the one the files still hold.
			a.logger.Error("experience.import", "err", err)
		} else if store, err := experience.NewSQLStore(a.database); err != nil {
			a.logger.Error("experience.store.init", "backend", a.cfg.DatabaseBackend(), "err", err)
		} else {
			a.experience = store
			return
		}
	}
	store, err := experience.New(dir)
	if err != nil {
		a.logger.Warn("experience.init.degraded", "err", err)
		return
	}
	a.experience = store
}

func (a *App) initIntervention() {
	store, err := intervention.New(a.cfg.InterventionsDir())
	if err != nil {
		a.logger.Warn("intervention.init.degraded", "err", err)
		return
	}
	a.intervention = store
}

// initLearning constructs the Learning Digest store. A failure degrades to a
// nil store (LearningService methods no-op) rather than blocking startup —
// the journal is advisory, not load-bearing for core task orchestration.
func (a *App) initLearning() {
	store, err := learning.New(config.LearningDir())
	if err != nil {
		a.logger.Warn("learning.init.degraded", "err", err)
		return
	}
	a.learning = store
}

func (a *App) initAgentQueue(ctx context.Context) {
	queue, err := agentqueue.New(config.AgentQueueDir(), agentqueue.Options{
		MaxDepth: a.cfg.Agent.Queue.MaxDepth,
		Store:    a.openAgentQueueStore(ctx),
	}, a.logger)
	if err != nil {
		a.logger.Warn("agentqueue.init.degraded", "err", err)
		return
	}
	a.agentQueue = queue
}

// initLimits opens the quota store, taking the store the caller already built
// so the import's context belongs to startup while the live poller keeps the
// app's own long-lived one.
func (a *App) initLimits(limitStore *limits.Store, err error) {
	if err != nil {
		a.logger.Warn("limits.init.degraded", "err", err)
		return
	}
	a.limits = limitStore
	policy := a.limitPolicy()
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
		if a.ctx != nil {
			a.startLiveLimitPolling(a.ctx, limitStore, policy)
		}
	}
}

func (a *App) initSandboxes() {
	mgr := sandbox.NewManager(filepath.Join(config.HomeDir(), "sandboxes"), a.logger)
	mgr.SetRetentionWindow(sandboxRetentionWindow(a.cfg))
	mgr.SetProtectedFindings(a.cleanupProtected)
	a.sandboxes = mgr
}

func (a *App) agentManagerConfig(approvalAddr string) agent.ManagerConfig {
	cfg := agent.ManagerConfig{
		Runtime:      a.agentRuntimeConfig(a.cfg),
		OnComplete:   a.onAgentComplete,
		ApprovalAddr: approvalAddr,
		SessionSink: func(taskID, agentID, sessionID string) error {
			return a.tasks.UpdateRunBy(taskID, "agentorch.record_session", agentID, task.RunPatch{SessionID: task.Ptr(sessionID)})
		},
		TaskExists: a.taskExistsForAgent,
		TaskStatus: a.taskStatusForAgent,
		TaskGeneration: func(taskID string) (int64, bool) {
			t, err := a.tasks.Get(taskID)
			return t.Generation, err == nil
		},
		LimitSink:              a.recordLimitSnapshot,
		Artifacts:              a.artifacts,
		SandboxHome:            a.sandboxes.SybraHomeDir,
		ControlHome:            config.HomeDir(),
		GhShimDir:              filepath.Join(config.HomeDir(), "shims"),
		AllowAmbientReviewAuth: a.cfg.GitHub.AllowAmbientReviewAuth,
		ControlEvent: func(kind string, data map[string]any) {
			a.logAudit(kind, "", "", data)
		},
	}
	if a.cfg.SurviveRestartEnabled() {
		cfg.SurviveRestartDir = config.AgentsDir()
	}
	return cfg
}

func (a *App) initAgentManager(ctx context.Context, emit func(string, any)) error {
	providerLimits := make(map[string]int, len(providerid.All()))
	for _, name := range providerid.All() {
		providerLimits[name] = a.cfg.Providers.Limits.MaxInFlightPerProvider
	}
	var err error
	var ledger dispatch.Persistence
	if a.database != nil {
		ledger, err = a.openAttemptLedger(ctx)
		if err != nil {
			return fmt.Errorf("dispatch admission: %w", err)
		}
	}
	a.attempts, err = dispatch.New(ctx, dispatch.Options{
		Dir:   config.AttemptLeasesDir(),
		Store: ledger,
		Limits: dispatch.Limits{
			Global:     a.cfg.Agent.MaxConcurrent,
			ByProvider: providerLimits,
		},
		TTL: time.Minute,
	})
	if err != nil {
		return fmt.Errorf("dispatch admission: %w", err)
	}
	approvalServer, approvalAddr := a.startApprovalServer(ctx, emit)
	agentCfg := a.agentManagerConfig(approvalAddr)
	agentCfg.AttemptAdmission = a.attempts
	if approvalServer != nil {
		agentCfg.ControlTarget = approvalAddr
		agentCfg.ControlToken = approvalServer.VerifierToken
	}
	a.agents, err = agent.NewManager(ctx, emit, a.logger, a.logDir, agentCfg)
	if err != nil && agentCfg.SurviveRestartDir != "" && errors.Is(err, agent.ErrSurvivalRegistry) {
		a.logger.Error("agent.survive-restart.init", "err", err)
		emit(events.StartupDegraded, startupDegradedEvent{
			Subsystem: "agents",
			Reason:    "agent survival registry failed to initialize; detached agents will not reconnect",
		})
		agentCfg.SurviveRestartDir = ""
		a.agents, err = agent.NewManager(ctx, emit, a.logger, a.logDir, agentCfg)
	}
	if err != nil {
		if approvalServer != nil {
			shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if shutdownErr := approvalServer.Shutdown(shutdownCtx); shutdownErr != nil {
				a.logger.Warn("approval-server.shutdown-after-agent-manager-init-failure", "err", shutdownErr)
			}
			cancel()
		}
		a.logger.Error("agent.manager.init", "err", err)
		return fmt.Errorf("agent manager: %w", err)
	}
	// One-shot classifier and judge calls run the same provider CLIs on this
	// host, so route them through the manager's sandbox rather than leaving
	// them to spawn unwrapped in the serving process's own directory (#3383).
	a.agents.RegisterOneShotCommands()
	a.agents.SetGHAppToken(github.CurrentAppToken)
	a.agents.SetGHVerifierAppToken(github.CurrentVerifierAppToken)
	// Applied at construction, not after Startup returns: the recovery pass
	// below dispatches agents, and one that starts before its board is named
	// spends a whole run on CLI calls that every one of them refuses.
	a.agents.SetBoard(a.boardTarget, a.boardToken, a.boardCA)
	// initToolLedger runs before this function, when a.agents is still nil, so
	// its own SetToolLedger call is skipped. Without re-binding here the
	// manager's ledger stays nil and Logger.Log's nil guard drops every record
	// silently — the ledger creates its directory, never writes a line, and
	// looks healthy while collecting nothing (#2788).
	a.agents.SetToolLedger(a.toolLedger)
	if agentCfg.SurviveRestartDir != "" {
		a.logger.Info("agent.survive-restart.enabled", "dir", agentCfg.SurviveRestartDir)
	}
	if approvalServer != nil {
		approvalServer.SetManager(a.agents)
		a.agentSvc.approval = approvalServer
		if a.verification != nil {
			a.verification.SetGrantRevoker(approvalServer.RevokeVerifierGrantForSandbox)
		}
	}
	return nil
}

func (a *App) agentRuntimeConfig(cfg *config.Config) agent.ManagerRuntimeConfig {
	policy := limits.DefaultPolicy()
	if a.limits != nil {
		policy = a.limitPolicy()
	}
	return agent.ManagerRuntimeConfig{
		MaxConcurrent:          cfg.Agent.MaxConcurrent,
		DefaultProvider:        cfg.Agent.Provider,
		DefaultModel:           cfg.Agent.Model,
		BashTimeoutMs:          cfg.BashTimeoutMs(),
		RetryWatchdog:          cfg.RetryWatchdog(),
		FallbackModel:          cfg.Agent.FallbackModel,
		LimitGate:              agent.LimitGateOrNil(a.limits),
		LimitPolicy:            policy,
		MaxInFlightPerProvider: cfg.Providers.Limits.MaxInFlightPerProvider,
		DispatchJitterMs:       cfg.Agent.DispatchJitterMs,
		HeadlessSteerable:      cfg.DefaultHeadlessSteerable(),
		SandboxMode:            cfg.DefaultSandboxMode(),
		SandboxReadMode:        cfg.DefaultSandboxReadMode(),
		PlaywrightMCPEnabled:   cfg.PlaywrightMCPEnabled(),
		PlaywrightMCPExtraArgs: cfg.PlaywrightMCPExtraArgs(),
		K8sJobsEnabled:         cfg.Agent.K8sJobs.Enabled,
		K8sJobs:                k8sJobRunnerConfigFromConfig(cfg.Agent.K8sJobs),
		RoleEffort:             cfg.Agent.RoleEffort,
		ClassReservations:      agent.ParseClassReservations(cfg.Agent.ClassReservations),
	}
}

func k8sJobRunnerConfigFromConfig(cfg config.K8sJobsConfig) agent.K8sJobRunnerConfig {
	out := agent.K8sJobRunnerConfig{
		Namespace: cfg.Namespace,
		Image:     cfg.Image,
		Command:   cfg.Command,
		TTL:       cfg.TTL,
		FailedTTL: cfg.FailedTTL,
		Mode:      cfg.Mode,
		CreatePR:  cfg.CreatePR,
	}
	for _, e := range cfg.Env {
		out.Env = append(out.Env, agent.K8sJobEnvVar{Name: e.Name, Value: e.Value})
	}
	for _, e := range cfg.SecretEnv {
		out.SecretEnv = append(out.SecretEnv, agent.K8sJobSecretEnvVar{
			Name:       e.Name,
			SecretName: e.SecretName,
			SecretKey:  e.SecretKey,
		})
	}
	for _, v := range cfg.Volumes {
		out.Volumes = append(out.Volumes, agent.K8sJobVolume{
			Name:      v.Name,
			ClaimName: v.ClaimName,
			MountPath: v.MountPath,
			ReadOnly:  v.ReadOnly,
		})
	}
	return out
}

func (a *App) onAgentComplete(ag *agent.Agent) {
	var lease verification.Lease
	var disposable bool
	if a.verification != nil {
		lease, disposable = a.verification.LeaseForAgent(ag.ID)
		if disposable {
			if lease.WorkspaceDir != "" {
				if err := a.verification.Finalize(context.Background(), lease, nil, agentOutputText(ag), lease.CertificateID); err != nil {
					ag.SetExitErr(err)
				}
			}
		}
	}
	revoked := true
	if a.agentSvc != nil && a.agentSvc.approval != nil && ag.EffectiveRole().IsVerifier() {
		if err := a.agentSvc.approval.RevokeVerifierGrantForSandbox(ag.SandboxHomeDir()); err != nil {
			revoked = false
			ag.SetExitErr(fmt.Errorf("revoke verifier control grant: %w", err))
		}
	}
	if disposable && revoked {
		a.verification.Release(lease)
	}
	if a.runenv != nil && agentRunEnvironmentFailed(ag) {
		a.runenv.InvalidateTask(ag.TaskID)
	}
	if a.agentCompletion == nil {
		a.logger.Warn("agent.complete.unwired", "id", ag.ID, "task_id", ag.TaskID)
		return
	}
	a.agentCompletion.OnComplete(ag)
}

func agentOutputText(ag *agent.Agent) string {
	var b strings.Builder
	outputs := ag.Output()
	for i := range outputs {
		event := &outputs[i]
		if event.Content != "" {
			b.WriteString(event.Content)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func agentRunEnvironmentFailed(ag *agent.Agent) bool {
	if runenv.IsEnvironmentFailure(ag.GetExitErr()) {
		return true
	}
	outputs := ag.Output()
	for i := range outputs {
		event := &outputs[i]
		if event.Type != "result" && event.ErrorType == "" && event.TerminalReason == "" {
			continue
		}
		diagnostic := strings.Join([]string{event.Content, event.ErrorType, event.TerminalReason}, " ")
		if runenv.IsEnvironmentFailure(errors.New(diagnostic)) {
			return true
		}
	}
	return false
}

func (a *App) recordLimitSnapshot(snapshot limits.Snapshot) {
	if a.limits == nil {
		return
	}
	if err := a.limits.UpdateSnapshot(snapshot); err != nil {
		a.logger.Warn("limits.snapshot", "provider", snapshot.Provider, "err", err)
	}
}

func (a *App) taskExistsForAgent(taskID string) bool {
	_, err := a.tasks.Get(taskID)
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	a.logger.Warn("agent.task-exists.error", "task_id", taskID, "err", err)
	return true
}

func (a *App) taskStatusForAgent(taskID string) (string, bool) {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			a.logger.Warn("agent.reattach.task-status", "task_id", taskID, "err", err)
		}
		return "", false
	}
	return string(t.Status), true
}

func (a *App) startLiveLimitPolling(ctx context.Context, limitStore *limits.Store, policy limits.Policy) {
	a.goWhileRunning(func() {
		state := newLiveLimitPollState(time.Now().UTC())
		for {
			if ctx.Err() != nil {
				return
			}
			now := time.Now().UTC()
			providers := state.dueProviders(now, policy)
			if len(providers) > 0 {
				result := a.refreshLiveLimits(ctx, limitStore, policy, providers...)
				if ctx.Err() != nil {
					return
				}
				state.recordResult(now, result, limitStore, a.logger)
				continue
			}
			timer := time.NewTimer(state.nextWait(now, policy))
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return
			case <-timer.C:
			}
		}
	})
}

func (a *App) refreshLiveLimits(ctx context.Context, limitStore *limits.Store, policy limits.Policy, providers ...string) limits.LiveRefreshResult {
	return limitStore.RefreshLiveSnapshots(ctx, policy, providers...)
}

func (a *App) limitPolicy() limits.Policy {
	p := limits.DefaultPolicy()
	p.Enabled = a.cfg.Providers.Limits.Enabled
	p.SessionThresholdPercent = a.cfg.Providers.Limits.SessionThresholdPercent
	p.WeeklyThresholdPercent = a.cfg.Providers.Limits.WeeklyThresholdPercent
	p.PreferUnderused = a.cfg.Providers.Limits.PreferUnderused
	p.SubscriptionMonthlyUSD = map[string]float64{
		providerid.Claude:   a.cfg.Providers.Claude.MonthlySubscriptionUSD,
		providerid.Codex:    a.cfg.Providers.Codex.MonthlySubscriptionUSD,
		providerid.Copilot:  a.cfg.Providers.Copilot.MonthlySubscriptionUSD,
		providerid.OpenCode: a.cfg.Providers.OpenCode.MonthlySubscriptionUSD,
	}
	p.ProviderEnabled = map[string]bool{
		providerid.Claude:   a.cfg.Providers.Claude.Enabled,
		providerid.Codex:    a.cfg.Providers.Codex.Enabled,
		providerid.Copilot:  a.cfg.Providers.Copilot.Enabled,
		providerid.OpenCode: a.cfg.Providers.OpenCode.Enabled,
	}
	return p
}

func (a *App) initStatusHook() {
	a.tasks.SetStatusChangeHook(func(taskID, from, to string, changed task.Task) {
		releaseTaskAgents := shouldReleaseTaskAgentsForStatus(task.Status(to))
		data := map[string]any{"from": from, "to": to}
		if !changed.Escalation.IsZero() {
			data["escalation_code"] = changed.Escalation.Code
			data["failure_owner"] = string(changed.Escalation.Owner)
			data["evidence_provenance"] = string(changed.Escalation.Provenance)
			data["autonomy_outcome"] = string(changed.AutonomyOutcome)
		}
		if to == string(task.StatusHumanRequired) {
			if kind := expectedHumanKind(changed); kind != "" {
				data["human_kind"] = kind
			}
		}
		a.logAudit(audit.EventTaskStatusChanged, taskID, "", data)
		if a.maybeQuarantineStatusBounce(taskID, from, to) {
			// Stop the agent the pause was meant to interrupt. Without this it
			// runs on, and its completion advances the workflow onto a step
			// that writes a live status back over the pause.
			a.releaseTaskAgents(taskID)
			return
		}

		local := true
		runsNoAgent := false
		if t, err := a.tasks.Get(taskID); err == nil {
			local = a.runsTaskLocally(t)
			runsNoAgent = t.TaskType == task.TaskTypeUmbrella
		}

		// Wake the dispatch pass immediately so a task that just became ready
		// (e.g. a dependency completing, a stage advancing) is picked up now
		// instead of waiting for the next fast tick.
		a.nudgeDispatch()

		// Advance workflows whose current run_agent step declares a
		// matching wait_for_status. This is how interactive agents (which
		// never exit between turns) signal step completion. Gated on
		// startupRecoveryDone: a step's completion can itself fire a
		// dispatch (e.g. maybeRecoverHumanRequiredAlreadyFixedOnMain), and
		// HasRunningAgentForTask reads an empty registry until reattach
		// finishes — see startupRecoveryPending's doc comment (#2752). Deferred
		// rather than dropped: nothing else re-delivers a wait_for_status match,
		// so a suppressed event would park the step until an operator intervenes.
		if local && a.workflowEngine != nil {
			if a.startupRecoveryDone() {
				a.workflowEngine.HandleStatusChange(taskID, to)
			} else {
				a.deferStatusChange(taskID)
			}
		}
		// HandleStatusChange may reroute a human-required self-escalation back
		// into the PR flow (missing live-PR blocker recovery). When it does the
		// task no longer sits at human-required, so skip the stale
		// human-required follow-up below — just release the agents and return.
		if to == string(task.StatusHumanRequired) {
			if t, err := a.tasks.Get(taskID); err == nil && t.Status != task.StatusHumanRequired {
				if local && releaseTaskAgents {
					a.releaseTaskAgents(taskID)
				}
				return
			}
		}
		if local && releaseTaskAgents {
			a.releaseTaskAgents(taskID)
		}

		switch to {
		case string(task.StatusTodo):
			// Todo is the entry point for new work. Approved plans now transition
			// directly to in-progress and skip this lane.
			if !runsNoAgent && from != string(task.StatusPlanReview) {
				a.dispatchTaskCreatedWorkflow(taskID)
			}
		case string(task.StatusPlanning):
			if !runsNoAgent {
				a.dispatchPlanningWorkflow(taskID)
			}
		case string(task.StatusInProgress):
			if !runsNoAgent {
				a.dispatchStatusWorkflow(taskID, task.StatusInProgress)
			}
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
			if local && a.runsScheduler() && a.startupRecoveryDone() && a.humanReview != nil {
				go a.humanReview.maybeSpawn(a.schedulerContext(), taskID, from)
			}
		case string(task.StatusReadyReview):
			if !runsNoAgent {
				a.dispatchStatusWorkflow(taskID, task.StatusReadyReview)
			}
		case string(task.StatusTesting):
			if !runsNoAgent {
				a.dispatchStatusWorkflow(taskID, task.StatusTesting)
			}
		case string(task.StatusReadyPR):
			if !runsNoAgent {
				a.dispatchStatusWorkflow(taskID, task.StatusReadyPR)
			}
		case string(task.StatusDone):
			if local {
				go a.closeLinkedIssueOnDone(taskID)
			}
		}
	})
}

func (a *App) maybeQuarantineStatusBounce(taskID, from, to string) bool {
	if !a.statusBounceTripped(taskID, from, to) {
		return false
	}
	reason := fmt.Sprintf("automatic status loop detected (%s → %s repeated); task paused", from, to)
	if _, err := a.tasks.Apply(task.TransitionIntent{
		TaskID: taskID, ToStatus: task.StatusBlocked, Actor: "app.status-bounce",
		ExpectedStatus: task.Ptr(task.Status(to)),
		Extra: task.Update{
			StatusReason:    task.Ptr(reason),
			Escalation:      task.MachineFailure("workflow.status_bounce", reason),
			AutonomyOutcome: task.QuarantinedOutcome(),
		},
		OperatorOverride: true,
	}); err != nil {
		a.logger.Error("task.status-bounce.pause-failed", "task_id", taskID, "err", err)
	} else {
		a.logger.Warn("task.status-bounce.paused", "task_id", taskID, "from", from, "to", to)
	}
	return true
}

const statusBounceLimit = 3

// statusBounceWindow bounds how long a transition is remembered. It is a
// memory bound rather than the discriminator: a contending pair can repeat on
// whatever tick its slowest dispatcher polls at, which is minutes, so a window
// tight enough to exclude sequential fix rounds would also stop catching the
// contention this detector exists for.
const statusBounceWindow = 2 * time.Hour

type statusBounceState struct {
	edges map[string][]bounceEdge
}

type bounceEdge struct {
	at    time.Time
	actor string
}

// recentActors returns the distinct actors that took an edge inside the
// window, dropping the entries that have aged out.
func (s *statusBounceState) recentActors(edge string, now time.Time) map[string]int {
	cutoff := now.Add(-statusBounceWindow)
	kept := s.edges[edge][:0]
	actors := make(map[string]int)
	for _, e := range s.edges[edge] {
		if !e.at.After(cutoff) {
			continue
		}
		kept = append(kept, e)
		actors[e.actor]++
	}
	s.edges[edge] = kept
	if len(kept) == 0 {
		delete(s.edges, edge)
	}
	return actors
}

// contendingActors reports how many times the busiest actor took an edge that
// some other actor also took. One automation driving a task through round
// after round writes both directions itself; two automations fighting over a
// task write the same pair against each other, and that difference is what
// separates ordinary work from a loop.
func contendingActors(forward, reverse map[string]int) int {
	worst := 0
	for actor, n := range forward {
		for other := range reverse {
			if other != actor && n > worst {
				worst = n
			}
		}
	}
	return worst
}

// statusBounceTripped reports whether this transition completes a repeated
// reciprocal loop (A→B and B→A). A single repeated transition is not enough:
// bulk status edits and legitimate retries can repeat one direction, whereas
// a reciprocal pair is the distinctive signature of competing automations.
func (a *App) statusBounceTripped(taskID, from, to string) bool {
	return a.statusBounceTrippedAt(taskID, from, to, a.tasks.LastStatusActor(taskID), time.Now())
}

func (a *App) statusBounceTrippedAt(taskID, from, to, actor string, now time.Time) bool {
	if taskID == "" || from == "" || to == "" || from == to ||
		from == string(task.StatusBlocked) || to == string(task.StatusBlocked) {
		return false
	}
	key := from + "\x00" + to
	reverse := to + "\x00" + from
	a.statusBounceMu.Lock()
	defer a.statusBounceMu.Unlock()
	if a.statusBounces == nil {
		a.statusBounces = make(map[string]*statusBounceState)
	}
	state := a.statusBounces[taskID]
	if state == nil {
		state = &statusBounceState{edges: make(map[string][]bounceEdge)}
		a.statusBounces[taskID] = state
	}
	state.edges[key] = append(state.edges[key], bounceEdge{at: now, actor: actor})
	forward := state.recentActors(key, now)
	back := state.recentActors(reverse, now)
	if len(state.edges) == 0 {
		delete(a.statusBounces, taskID)
	}
	return contendingActors(forward, back) >= statusBounceLimit &&
		contendingActors(back, forward) >= statusBounceLimit-1
}

func (a *App) closeLinkedIssueOnDone(taskID string) {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return
	}
	issue := strings.TrimSpace(t.Issue)
	if issue == "" {
		return
	}
	repo, num := github.ParseIssueURL(issue)
	if num == 0 || repo != t.ProjectID {
		return
	}
	closeFn := a.umbrellaCloseIssue
	if closeFn == nil {
		closeFn = github.CloseIssue
	}
	comment := "Completed by Sybra."
	if t.PRNumber > 0 {
		comment = fmt.Sprintf("Completed by Sybra via #%d.", t.PRNumber)
	}
	if closeErr := closeFn(repo, num, comment); closeErr != nil {
		a.logger.Warn("task.done.close-issue", "task_id", taskID, "issue", issue, "err", closeErr)
		return
	}
	a.logger.Info("task.done.close-issue", "task_id", taskID, "issue", issue)
}

func shouldReleaseTaskAgentsForStatus(status task.Status) bool {
	return status == task.StatusHumanRequired || task.IsTerminalStatus(status)
}

func (a *App) releaseTaskAgents(taskID string) {
	if a.taskAgentReleaser != nil {
		a.taskAgentReleaser(taskID)
		return
	}
	if a.agents == nil {
		return
	}
	targets := a.agents.FindAllRunningAgentsForTask(taskID, "")
	if len(targets) == 0 {
		return
	}
	// Agents started after the park are deliberate new dispatches, not
	// leftovers from before it, so they are not this release's to reap.
	//
	// The status hook fires with prev == "" the first time this process
	// observes a task, which happens on every restart. For an
	// already-parked task that fabricates a fresh transition into
	// human-required and reaps whatever is running — including a review
	// agent dispatched seconds earlier. Measured: a task parked at 08:08
	// had its review agent killed 1.6ms after start at 08:47, and since
	// human-required is what the review phase asserts to mean "needs you",
	// the only agent that could clear it was the one being killed.
	// Scoped to human-required only. releaseTaskAgents also runs for terminal
	// statuses, and a done/cancelled task must reap every agent regardless of
	// when it started — otherwise a restart can leave work running against a
	// task that is already finished.
	parkedAt := time.Time{}
	if t, err := a.tasks.Get(taskID); err == nil && t.Status == task.StatusHumanRequired {
		parkedAt = t.StatusChangedAt
	}
	filtered := make([]*agent.Agent, 0, len(targets))
	for _, ag := range targets {
		if ag.EffectiveRole().DiagnosesBlockedTask() {
			continue
		}
		// !Before, not After: a tie means the agent started in the same instant
		// the park was recorded, which is the dispatch that triggered it.
		if !parkedAt.IsZero() && !ag.StartedAt.Before(parkedAt) {
			a.logger.Info("task.status.release-agent.skip-newer",
				"task_id", taskID, "agent_id", ag.ID,
				"started_at", ag.StartedAt, "status_changed_at", parkedAt)
			continue
		}
		filtered = append(filtered, ag)
	}
	if len(filtered) == 0 {
		return
	}
	// Tracked by a.wg (not a bare `go func`): App.Shutdown waits on a.wg
	// before calling agents.Shutdown, which explicitly skips detached agents
	// so they survive a restart. A task landing (e.g. its own PR merging)
	// commonly races the very redeploy that lands it, so an untracked
	// goroutine here can be starved of scheduler time entirely — the process
	// exits before ever sending the stop signal, and a detached interactive
	// agent (a never-EOF stdin FIFO) then idles forever holding a
	// concurrency slot until happenstance reaps it on some later restart.
	// Tracking it here only needs to guarantee the signal is sent — once
	// StopAgent's SIGINT/SIGKILL reaches the OS, delivery no longer depends
	// on this process staying alive.
	a.goWhileRunning(func() {
		for _, ag := range filtered {
			var err error
			if ag.Mode == "headless" && ag.CompletedSuccessfully() {
				err = a.agents.StopCompletedAgent(ag.ID)
			} else {
				err = a.agents.StopAgent(ag.ID)
			}
			if err != nil {
				a.logger.Warn("task.status.release-agent", "task_id", taskID, "agent_id", ag.ID, "err", err)
			}
		}
	})
}

func (a *App) runsTaskLocally(t task.Task) bool {
	cfg := a.currentConfig()
	if cfg != nil && cfg.IsLeader() && a.workerControl != nil {
		// The leader owns every canonical workflow now; node metadata selects
		// only the execution backend for each run, never another task owner.
		return true
	}
	return cfg == nil || cfg.HomeNodeForTask(t.ProjectID, t.NodeOverride).Local
}

func (a *App) isWorkProject(projectID string) bool {
	if projectID == "" {
		return false
	}
	if a.projects == nil {
		return true
	}
	rawType, err := a.projects.RawType(projectID)
	if err != nil {
		return true
	}
	return rawType != project.ProjectTypePet
}

func (a *App) auditClusterBlock(taskID, node, reason string) {
	a.logAudit(audit.EventClusterAssignBlocked, taskID, "", map[string]any{"node": node, "reason": reason})
}

func (a *App) initCluster() {
	if a.cfg != nil && a.cfg.IsLeader() && a.workerControl != nil && a.agents != nil {
		a.agents.SetExecutionBackend(newLeaderExecutionBackend(a))
		if a.taskSvc != nil {
			a.taskSvc.leaderRunPlacement = true
		}
		if a.workflowEngine != nil {
			a.workflowEngine.SetDispatchGate(func(workflow.TaskInfo) bool { return true })
		}
		if len(a.cfg.Cluster.Followers) > 0 {
			a.logger.Warn("cluster.follower-mode.deprecated", "migration", "run sybra-agentd workers against this leader; task snapshot assignment and mirroring are disabled")
		}
		a.logger.Info("cluster.leader.run-placement.enabled")
		return
	}
	if a.workflowEngine != nil {
		a.workflowEngine.SetDispatchGate(func(ti workflow.TaskInfo) bool {
			cfg := a.currentConfig()
			return cfg == nil || cfg.HomeNodeForTask(ti.ProjectID, ti.NodeOverride).Local
		})
	}
	if a.cfg == nil || !a.cfg.IsLeader() {
		return
	}
	roster, err := clusterlead.NewRoster(a.cfg, a.logger)
	if err != nil {
		a.logger.Error("cluster.roster.init.failed", "err", err)
		return
	}
	if roster == nil || len(roster.Names()) == 0 {
		a.logger.Info("cluster.leader.no-followers")
		return
	}
	a.clusterRoster = roster
	a.assigner = clusterlead.NewAssigner(a.cfg, a.tasks, roster, a.isWorkProject, a.auditClusterBlock, a.logger)
	a.assigner.SetAttachments(a.attachments)
	a.assigner.SetStopLocalAgents(a.releaseTaskAgents)
	a.mirror = clusterlead.NewMirror(a.cfg, a.tasks, roster, a.logger, 0)
	a.mirror.SetAttachments(a.attachments)
	if a.clusterSvc != nil {
		a.clusterSvc.setRoster(roster)
		a.clusterSvc.setAssigner(a.assigner)
	}
	a.logger.Info("cluster.leader.enabled", "followers", roster.Names())
}

// dispatchStatusWorkflow starts the workflow matching a status-change event.
//
// ErrWorkflowAlreadyActive is benign for these status transitions:
//   - ready-review: when the cascade flips to ready-review
//     (simple-task-implement ending), this hook fires while the implement
//     workflow is still active; OnWorkflowComplete drives the cascade instead.
//     Only real errors are surfaced. This also enables the manual "move card to
//     Ready Review" path.
//   - testing: ErrWorkflowAlreadyActive is benign and the COMMON case. When
//     simple-task-review's done_review flips to testing, this hook fires while
//     the review workflow is still active, so the start is rejected here and the
//     cascade is driven by OnWorkflowComplete instead. Only a real, unexpected
//     error is worth surfacing. This also serves the genuine manual "move card
//     to Testing" path.
//   - ready-pr: ErrWorkflowAlreadyActive is benign and the COMMON case. When
//     testing-task flips to ready-pr on a PASS, this hook fires while the
//     testing workflow is still active, so the start is rejected here and the
//     cascade drives simple-task-pr via OnWorkflowComplete. The case that NEEDS
//     this is recovery: a task flipped to ready-pr out of band — `sybra-cli
//     update` (a separate process with no engine) or the UpdateTask UI path
//     after a tester infra failure — has no terminal cascade to open its PR, so
//     without this branch it sits inert with no PR. Mirrors the
//     testing/ready-review cases.
func (a *App) dispatchStatusWorkflow(taskID string, status task.Status) {
	if !a.runsScheduler() || !a.startupRecoveryDone() {
		return
	}
	if a.workflowEngine == nil {
		return
	}
	if t, err := a.tasks.Get(taskID); err == nil {
		if !a.runsTaskLocally(t) {
			return
		}
		// Umbrella tasks run no agent (agentorch.startAgent refuses them at
		// the dispatch choke point). Every status-triggered workflow here
		// ends in a run_agent step, so dispatching one onto a tracker is a
		// guaranteed 3-attempt circuit-breaker trip that flips the tracker
		// to human-required — and umbrella.rollup then flips it back to
		// in-progress on the next tick, looping forever.
		if t.TaskType == task.TaskTypeUmbrella {
			return
		}
	}

	statusValue := string(status)
	logKey := "workflow.dispatch." + statusValue

	if _, err := a.workflowEngine.DispatchEvent(
		taskID,
		"task.status_changed",
		map[string]string{"task.status": statusValue},
		nil,
	); err != nil && !errors.Is(err, workflow.ErrWorkflowAlreadyActive) {
		a.logger.Error(logKey, "task_id", taskID, "err", err)
	}
}

func expectedHumanKind(t task.Task) string {
	if !slices.Contains(t.Tags, "review") {
		return ""
	}
	switch {
	case t.ReviewPhase == review.ReviewPhaseDrafted ||
		strings.HasPrefix(t.StatusReason, "Draft review ready"):
		return "review_draft"
	case t.ReviewPhase == review.ReviewPhaseManual:
		return "review_manual"
	default:
		return ""
	}
}

// dispatchTaskCreatedWorkflow starts the matching task.created workflow for a
// task that is ready to enter or re-enter the front of the pipeline. This is
// used both for files created outside the GUI and for status updates that put a
// parked task back in todo/planning. Mirrors TaskService.startCreatedWorkflow.
// Idempotent: DispatchEvent serializes per task and rejects a task that already
// owns a non-terminal workflow, so duplicate watcher/status events are harmless.
// dispatchTaskCreatedWorkflow, dispatchPlanningWorkflow and dispatchStatusWorkflow
// are the three sinks through which a task auto-starts work, so each gates on
// runsScheduler (and startupRecoveryDone, see its doc comment) itself rather
// than trusting its callers. Gating call sites was tried and leaked twice: the
// watcher reaches these both via the status hook and via
// maybeStartWorkflowForExternalTask, and because the task store writes
// atomically (temp file + rename) fsnotify reports every external write — even a
// tags-only update — as CREATE, so the create path is far hotter than its name
// suggests.
func (a *App) dispatchTaskCreatedWorkflow(taskID string) {
	if !a.runsScheduler() || !a.startupRecoveryDone() {
		return
	}
	if a.workflowEngine == nil || a.tasks == nil || a.agents == nil {
		return
	}
	if taskID == "" {
		return
	}
	a.goWhileRunning(func() {
		t, err := a.tasks.Get(taskID)
		if err != nil {
			return
		}
		// Only fresh task.created entries. Planning re-entry has its own lane:
		// restarting simple-task-plan from step 1 would re-run triage, which can
		// hand a rejected/reset planning task straight back to implementation.
		if t.Status != task.StatusNew && t.Status != task.StatusTodo {
			return
		}
		if !a.runsTaskLocally(t) {
			// A follower-owned mirror write already carries AssignedNode; sending
			// it back through the leader's route path turns a no-op watcher wakeup
			// into unnecessary remote assignment traffic.
			if t.AssignedNode != "" {
				return
			}
			if a.assigner != nil {
				if _, err := a.assigner.Route(a.ctx, t); err != nil {
					a.logger.Warn("cluster.assign.failed", "task_id", taskID, "err", err)
				}
			}
			return
		}
		// Before approved plans handed directly to implementation, they landed
		// in todo. Promote those legacy records instead of replaying task.created
		// and discarding their approved contracts by re-running triage.
		// This runs after node routing, so a remote task is still assigned to its
		// owner before that owner starts implementation.
		if t.Status == task.StatusTodo && hasApprovedPlanContract(t) {
			if _, err := a.tasks.ApplyStatusEffect(taskID, task.StatusEffect{
				Source:         "workflow.legacy-approved-plan",
				ToStatus:       task.StatusInProgress,
				ExpectedStatus: t.Status,
			}); err != nil {
				a.logger.Error("workflow.approved-plan.promote", "task_id", taskID, "err", err)
			}
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
		if a.agents.HasRunningAgentForTask(taskID) {
			return
		}
		if _, err := a.workflowEngine.DispatchEvent(taskID, "task.created", nil, nil); err != nil &&
			!errors.Is(err, workflow.ErrWorkflowAlreadyActive) &&
			!errors.Is(err, workflow.ErrAutoDispatchDisabled) {
			a.logger.Error("workflow.task-created.failed", "task_id", taskID, "err", err)
		}
	})
}

// maybeStartWorkflowForExternalTask starts the matching task.created workflow
// for a task that appeared on disk outside the GUI CreateTask path — most
// importantly via sybra-cli, the documented primary task interface. Without
// this, CLI-created tasks never get a workflow and sit inert in todo: the
// orchestrator can triage them but no implementation ever starts.
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
	t, err := a.tasks.Get(id)
	if err != nil {
		return
	}
	switch t.Status {
	case task.StatusPlanning:
		a.dispatchPlanningWorkflow(id)
	default:
		a.dispatchTaskCreatedWorkflow(id)
	}
}

// initToolLedger opens the always-on tool-call ledger. A failure degrades to
// no recording rather than blocking startup: the ledger informs future policy,
// it does not gate anything running now.
func (a *App) initToolLedger(ctx context.Context) {
	l, err := a.openToolLedger(ctx)
	if err != nil {
		a.logger.Warn("tool_ledger.init.degraded", "err", err)
		return
	}
	a.toolLedger = l
	if a.agents != nil {
		a.agents.SetToolLedger(l)
	}
}

func (a *App) initAudit(ctx context.Context) {
	al, err := a.openAuditStore(ctx)
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
	// Through the store, not the package-level file cleanup: with a database
	// backend the day-files stop growing and pruning them frees nothing, while
	// audit_events — the highest-rate table there is — would grow forever.
	if err := a.audit.Cleanup(retentionDays); err != nil {
		a.logger.Warn("audit.cleanup", "err", err)
	}

	// Wired here (rather than per-caller) so every push-preflight failure —
	// from review.Handler's PR-fix path and workflow.Engine's PR-tail push
	// step alike — surfaces as one audit event internal/health's
	// checkGHPushAuthFailure turns into an operator-visible finding, instead
	// of only the per-task status_reason PreflightPushCredentials' callers
	// already set. See #2315: a task landed in human-required with no signal
	// beyond a log line when the host's only gh session expired.
	project.SetPushAuthFailureHook(func(err error) {
		a.logAudit(audit.EventGHPushAuthFailed, "", "", map[string]any{
			"err": err.Error(),
		})
	})
}

// initArtifacts constructs the artifact store, wires the task delete hook to
// GC artifact directories on task deletion, and sweeps orphaned artifact
// directories left by tasks that no longer exist.
func (a *App) initArtifacts() {
	a.artifacts = artifact.New(config.ArtifactsDir())
	// CompletionEvidence rides the same per-task artifact store/directory as
	// every other harness artifact — one more named blob, not a second store
	// to stand up or GC separately (initArtifacts' delete hook below already
	// covers it via a.artifacts.Delete).
	a.evidenceStore = evidence.NewStore(a.artifacts)
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
		Interval:            time.Duration(a.cfg.Providers.HealthCheck.IntervalSeconds) * time.Second,
		ClaudeEnabled:       a.cfg.Providers.Claude.Enabled,
		CodexEnabled:        a.cfg.Providers.Codex.Enabled,
		CopilotEnabled:      a.cfg.Providers.Copilot.Enabled,
		OpenCodeEnabled:     a.cfg.Providers.OpenCode.Enabled,
		AutoFailover:        a.cfg.Providers.AutoFailover,
		ClaudeRLCooldown:    time.Duration(a.cfg.Providers.Claude.RateLimitCooldownSeconds) * time.Second,
		CodexRLCooldown:     time.Duration(a.cfg.Providers.Codex.RateLimitCooldownSeconds) * time.Second,
		CopilotRLCooldown:   time.Duration(a.cfg.Providers.Copilot.RateLimitCooldownSeconds) * time.Second,
		OpenCodeRLCooldown:  time.Duration(a.cfg.Providers.OpenCode.RateLimitCooldownSeconds) * time.Second,
		AuthFailureCooldown: time.Duration(a.cfg.Providers.HealthCheck.AuthFailureCooldownSeconds) * time.Second,
	}, emit, a.logger)
	// New seeds every provider Healthy=false until probed; probe once here,
	// before the gate is live, so startLifecycle's dispatch never sees a
	// window where every provider reads unhealthy and fails closed.
	pc.ProbeOnce(ctx)
	a.providerHealth = pc
	a.agents.SetHealthGate(pc)
	a.wg.Go(func() { pc.Run(ctx) })
}

// emitDegradedWarnings fires startup:degraded for any subsystem that failed
// to initialize. Called after emit is configured so the frontend receives the events.
func (a *App) emitDegradedWarnings(emit func(string, any)) {
	if a.audit == nil {
		emit(events.StartupDegraded, startupDegradedEvent{"audit", "audit logger failed to initialize; audit trail unavailable"})
	}
	if a.stats == nil {
		emit(events.StartupDegraded, startupDegradedEvent{"stats", "stats store failed to initialize; metrics unavailable"})
	}
}

// initAutomations starts every per-machine task source in dependency order
// and returns the GitHub issues fetcher (still consumed by
// startBackgroundServices). Extracted so Startup stays under funlen.
func (a *App) initAutomations(ctx context.Context, emit func(string, any)) *poll.IssuesFetcher {
	a.initRenovate(emit)
	a.initPromptLab()
	a.initTriage()
	a.initHumanReview(ctx)
	return a.initIssuesFetcher(emit)
}

// initWorkflowEngine wires the engine onto wfStore, which the caller opens: the
// engine's own contexts outlive startup, so the import's belongs to the caller.
func (a *App) initWorkflowEngine(wfStore workflow.Repository) {
	if os.Getenv("SYBRA_DISABLE_WORKFLOWS") == "1" {
		a.logger.Info("workflow.disabled")
		return
	}
	q := a.agentQueue
	if q != nil && a.agentOrch != nil {
		a.agentOrch.SetQueue(q)
	}
	if wfStore == nil {
		return
	}
	a.workflowStore = wfStore
	// After the import, never before: seeding writes each builtin under its own
	// id, so an import that followed it would overwrite an operator's edited
	// copy with the shipped one.
	if syncErr := workflow.SyncBuiltins(wfStore); syncErr != nil {
		a.logger.Error("workflow.sync-builtins", "err", syncErr)
	}
	agentLauncher := a.newWorkflowAgentLauncher()
	if a.sandboxes == nil {
		panic("wire workflow engine: sandbox manager is nil")
	}
	engine, err := workflow.NewEngine(
		wfStore,
		&taskAdapter{tasks: a.tasks, projects: a.projects},
		agentLauncher,
		a.logger,
		a.workflowDependencies(agentLauncher),
	)
	if err != nil {
		panic("wire workflow engine: " + err.Error())
	}
	a.workflowEngine = engine
	a.configureWorkflowPolicies()
	if a.cfg.Evaluation.Offline.Enabled {
		gate := prompteval.NewGate(prompteval.New(config.PromptEvalDir()), a.cfg.Evaluation.Offline)
		a.workflowEngine.SetEvalGate(gate)
		a.agents.SetEvalPassed(func(variantID, digest string) bool {
			allow, _, gateErr := gate.AllowEnrollment(variantID, digest)
			return gateErr == nil && allow
		})
	}
	if a.artifacts != nil {
		a.workflowEngine.SetArtifactRecorder(&artifactRecorderAdapter{store: a.artifacts})
	}
	if a.evidenceStore != nil {
		a.workflowEngine.SetEvidenceRecorder(&evidenceRecorderAdapter{store: a.evidenceStore})
	}
	a.workflowEngine.SetContext(a.ctx)
	a.workflowEngine.SetDrainContext(a.schedulerCtx)
	// Recover worktree-prep rebase conflicts via the conflict pr-fix instead of
	// escalating to a human. Wired here (not at construction) because the
	// orchestrator is built before the reviewer. Routed through
	// workflowEngine.TryConflictRecovery (not reviewer.RecoverStaleBranchConflict
	// directly): a rebase conflict discovered here can be reached from deep
	// inside a still-executing StartWorkflow call (e.g. restart-stale's raw
	// StartWorkflow(implement) hitting the conflict during execRunAgent's own
	// worktree prep), and calling the reviewer directly in that case re-enters
	// StartWorkflowWithVars while this task's starting marker is still held —
	// ErrWorkflowAlreadyActive, silently dropped, straight to human-required.
	// TryConflictRecovery detects the held marker and queues the retry instead.
	if a.agentOrch != nil && a.workflowEngine != nil {
		a.agentOrch.SetConflictRecovery(a.workflowEngine.TryConflictRecovery)
	}
	// Same recovery for a push-time divergence surfaced by push_branch/create_pr
	// (e.g. a reused worktree rebased out from under an earlier merge-based
	// push) — otherwise it flips straight to human-required with no attempt
	// at the autonomous fix other divergence sources already get.
	if a.workflowEngine != nil && a.reviewer != nil {
		a.workflowEngine.SetConflictRecovery(a.reviewer.RecoverStaleBranchConflict)
	}
	// Order ResumeStalled's per-tick scan with the same agentqueue.Less
	// ordering the admission queue itself uses (priority/status-floor,
	// manual, dispatch-status, age), and prune stale queue entries first.
	// App-level manual draining only pops manual items; workflow-owned queue
	// entries stay visible here so ResumeStalled remains the sole owner of
	// workflow dispatch order and token consumption.
	// Wired here (not inside internal/workflow) so the engine package never
	// imports internal/agentqueue — see TaskInfo.Priority's doc comment.
	if q != nil {
		a.workflowEngine.SetDispatchComparator(func() func(x, y workflow.TaskInfo) int {
			snap := q.Snapshot()
			queued := make(map[string]agentqueue.Item, len(snap))
			for i := range snap {
				queued[snap[i].TaskID] = snap[i]
			}
			toItem := func(t workflow.TaskInfo) agentqueue.Item {
				it := agentqueue.Item{TaskID: t.ID, Priority: task.Priority(t.Priority), Status: t.Status}
				if qit, ok := queued[t.ID]; ok {
					it.Manual = qit.Manual
					it.Enqueued = qit.Enqueued
				}
				return it
			}
			return func(x, y workflow.TaskInfo) int {
				ai, bi := toItem(x), toItem(y)
				switch {
				case agentqueue.Less(ai, bi):
					return -1
				case agentqueue.Less(bi, ai):
					return 1
				default:
					return 0
				}
			}
		})
		a.workflowEngine.SetQueueReconciler(func() {
			q.Reconcile(func(id string) (task.Task, bool) {
				t, err := a.tasks.Get(id)
				return t, err == nil
			})
		})
	}
	// Workflow completion moves to wireServices so the callback closure binds
	// to the completion.Handler constructed there.
}

func (a *App) workflowDependencies(agentLauncher *agentAdapter) workflow.Dependencies {
	return workflow.Dependencies{
		PR: workflow.PRSurface{
			Linker:           workflowpr.LinkerAdapter{},
			ReviewRequester:  workflowpr.ReviewRequesterAdapter{},
			StateFetcher:     workflowpr.StateFetcherAdapter{},
			ThreadFetcher:    workflowpr.ThreadFetcherAdapter{},
			HeadFetcher:      workflowpr.HeadFetcherAdapter{},
			MetaFetcher:      workflowpr.MetaFetcherAdapter{},
			Creator:          workflowpr.CreatorAdapter{},
			Closer:           workflowpr.CloserAdapter{},
			Finder:           workflowpr.FinderAdapter{},
			AnyStateFinder:   workflowpr.FinderAdapter{},
			ExistenceChecker: workflowpr.ExistenceCheckerAdapter{},
			ContentGenerator: workflowpr.ContentGeneratorAdapter{Gen: &prcontent.FallbackGenerator{Logger: a.logger, Gate: a.providerHealth}},
		},
		Execution: workflow.ExecutionSurface{
			Worktrees:            &worktreeGetterAdapter{tasks: a.tasks, mgr: a.worktrees},
			SidecarDir:           a.sandboxes.SybraHomeDir,
			AttemptNotes:         &attemptNoteAppenderAdapter{},
			BranchSyncer:         &branchSyncerAdapter{tasks: a.tasks, mgr: a.worktrees},
			Checks:               &checkConfigGetterAdapter{tasks: a.tasks, projects: a.projects, mgr: a.worktrees},
			ManualTests:          &manualTestConfigGetterAdapter{tasks: a.tasks, projects: a.projects, mgr: a.worktrees},
			Classifier:           a.newTaskClassifierAdapter(),
			CostBudget:           agentLauncher,
			AttemptWorktrees:     &attemptWorktreeAdapter{tasks: a.tasks, mgr: a.worktrees},
			Verification:         &verificationWorkspaceAdapter{mgr: a.verification},
			VerificationCommands: agentLauncher,
			PostRun:              a.postRunReconciliation(),
		},
	}
}

func (a *App) configureWorkflowPolicies() {
	a.configureTestingEscalation()
	a.workflowEngine.SetMaxCheckpoints(a.cfg.MaxCheckpoints())
	a.workflowEngine.SetVerifyChecksMaxConcurrent(a.cfg.VerifyChecksMaxConcurrent())
	a.workflowEngine.SetABTestingConfig(a.abTestingConfig())
	a.configurePlanAutoApproval()
	a.configureAdmissionPolicy()
	a.configureEvidencePolicy()
}

func (a *App) configurePlanAutoApproval() {
	a.workflowEngine.SetAutoApprovePlansWithoutDecisions(a.cfg.Orchestrator.AutoApprovePlansWithoutDecisions)
	a.workflowEngine.SetPlanAutoApproveHook(func(t workflow.TaskInfo, reason string) {
		a.logAudit(audit.EventPlanApproved, t.ID, "", map[string]any{
			"auto":   true,
			"reason": reason,
		})
	})
}

// configureAdmissionPolicy wires the admission_preflight step's config and
// its audit hook. The hook writes admission.decided without making
// internal/workflow import internal/audit — mirrors configurePlanAutoApproval.
func (a *App) configureAdmissionPolicy() {
	a.workflowEngine.SetAdmissionConfig(a.cfg.Admission)
	a.workflowEngine.SetAdmissionDecisionHook(func(t workflow.TaskInfo, d workflow.AdmissionDecision) {
		data := map[string]any{
			"outcome":         d.Outcome,
			"risk_tier":       d.RiskTier,
			"permission_tier": d.PermissionTier,
			"blocker_kind":    d.BlockerKind,
			"reason":          d.Reason,
			"failure_code":    d.FailureCode,
			"task_generation": t.Generation,
		}
		if d.Outcome == string(taskstatus.Blocked) {
			data["preflight_detectable"] = true
			if a.stats != nil {
				cost, tokens, runs, usageKnown := preflightUsage(a.stats.AllForTask(t.ID), t.ID, t.Generation)
				data["usage_known"], data["cost_usd"], data["tokens"], data["prior_runs"] = usageKnown, cost, tokens, runs
			} else {
				data["usage_known"], data["cost_usd"], data["tokens"], data["prior_runs"] = false, 0.0, 0, 0
			}
		}
		a.logAudit(audit.EventAdmissionDecided, t.ID, "", data)
	})
}

func preflightUsage(records []stats.RunRecord, taskID string, generation int64) (cost float64, tokens, runs int, known bool) {
	legacyUnattributed := false
	var cohort uint64
	cohortKnown := false
	for i := range records {
		record := &records[i]
		if record.TaskID != taskID {
			continue
		}
		if !record.TaskGenerationKnown || generation < 0 {
			legacyUnattributed = true
			continue
		}
		// #nosec G115 -- the negative generation case is rejected above.
		if record.TaskGeneration > uint64(generation) {
			continue
		}
		if !cohortKnown || record.TaskGeneration > cohort {
			cohort, cohortKnown = record.TaskGeneration, true
		}
	}
	known = !legacyUnattributed
	for i := range records {
		record := &records[i]
		if !cohortKnown || record.TaskID != taskID || !record.TaskGenerationKnown || record.TaskGeneration != cohort {
			continue
		}
		runs++
		cost += record.CostUSD
		tokens += record.InputTokens + record.OutputTokens + record.CacheCreationInputTokens + record.CacheReadInputTokens + record.ReasoningTokens
		if record.CostUSD == 0 && record.InputTokens == 0 && record.OutputTokens == 0 && record.CacheCreationInputTokens == 0 && record.CacheReadInputTokens == 0 && record.ReasoningTokens == 0 && record.PremiumRequests == 0 {
			known = false
		}
	}
	if runs == 0 {
		known = !legacyUnattributed
	}
	return cost, tokens, runs, known
}

// configureEvidencePolicy wires the require_evidence step's config and its
// audit hook. The hook writes completion_evidence.verified/blocked without
// making internal/workflow import internal/audit — mirrors
// configureAdmissionPolicy.
func (a *App) configureEvidencePolicy() {
	a.workflowEngine.SetEvidenceConfig(a.cfg.Agent.Evidence)
	a.workflowEngine.SetEvidenceDecisionHook(func(t workflow.TaskInfo, d workflow.EvidenceDecision) {
		event := audit.EventCompletionEvidenceVerified
		if d.Outcome == "blocked" {
			event = audit.EventCompletionEvidenceBlocked
		}
		a.logAudit(event, t.ID, "", map[string]any{
			"outcome": d.Outcome,
			"reason":  d.Reason,
		})
	})
}

func (a *App) newWorkflowAgentLauncher() *agentAdapter {
	pressureGate := a.getPressureGate()
	adapter := &agentAdapter{
		agents:          a.agents,
		agentOrch:       a.agentOrch,
		tasks:           a.tasks,
		projects:        a.projects,
		sandboxes:       a.sandboxes,
		experience:      a.experience,
		pressure:        pressureGate,
		remotePlacement: a.cfg != nil && a.cfg.IsLeader() && a.workerControl != nil && a.agents != nil,
		runenv:          a.runenv,
		verification:    a.verification,
	}
	if a.agentOrch != nil {
		a.agentOrch.SetPressureGate(pressureGate)
		a.agentOrch.SetPressureAdmission(adapter.pressureAdmission)
	}
	return adapter
}

func (a *App) getPressureGate() *pressure.Gate {
	if a.pressureGate == nil {
		a.pressureGate = pressure.New(a.cfg.Orchestrator.Pressure, config.HomeDir(), a.logger)
		if a.pressureGate != nil {
			if reclaimer := a.getDiskReclaimer(); reclaimer != nil {
				a.pressureGate.SetReclaimTrigger(func() { reclaimer.TryRun() })
			}
		}
	}
	return a.pressureGate
}

func (a *App) getDiskReclaimer() *diskreclaim.Reclaimer {
	if a == nil || a.cfg == nil || a.tasks == nil {
		return nil
	}
	if a.diskReclaimer == nil {
		cooldown := time.Duration(a.cfg.Orchestrator.Pressure.ReclaimCooldownSeconds) * time.Second
		a.diskReclaimer = diskreclaim.New(a.cfg, a.tasks, cooldown, a.logger)
	}
	return a.diskReclaimer
}

// configureTestingEscalation wires the testing→escalation config knobs onto
// the workflow engine (split out of initWorkflowEngine to keep it under the
// funlen cap).
func (a *App) configureTestingEscalation() {
	a.workflowEngine.SetTestingMaxAttempts(a.cfg.TestingMaxAttempts())
	a.workflowEngine.SetReviewUntilClean(a.cfg.ReviewUntilClean())
	a.workflowEngine.SetReviewRoundsPerHour(a.cfg.Agent.ReviewRoundsPerHourLimit())
	a.workflowEngine.SetOpenPROnUnrunnableGate(a.cfg.TestingOpenPROnUnrunnableGateEnabled())
	a.warnUnboundedReviewLoop()
}

// warnUnboundedReviewLoop is kept for historical call sites. The shared review
// budget now carries a fixed lifetime cap in addition to the hourly limit, so
// review→fix cycles are always bounded even when the hourly cap is disabled.
func (a *App) warnUnboundedReviewLoop() {
}

func (a *App) initAgentConfig() {
	a.agents.SetGuardrails(agent.Guardrails{
		MaxCostUSD:              a.cfg.Agent.MaxCostUSD,
		MaxTurns:                a.cfg.Agent.MaxTurns,
		MaxCheckpoints:          a.cfg.MaxCheckpoints(),
		TurnCostFraction:        a.cfg.Agent.TurnCostFraction,
		TurnMultiplier:          a.cfg.Agent.TurnMultiplier,
		CheckpointOnTurnCeiling: a.cfg.CheckpointOnTurnCeilingEnabled(),
		MaxSubagentEvents:       a.cfg.Agent.MaxSubagentEvents,
	})
}

func (a *App) startApprovalServer(ctx context.Context, emit func(string, any)) (srv *agent.ApprovalServer, addr string) {
	controlDir := filepath.Join(config.HomeDir(), "control")
	srv, err := agent.NewDurableApprovalServer(ctx, emit, a.logger, a.cfg.Agent.ApprovalPort,
		filepath.Join(controlDir, "approval-port"), filepath.Join(controlDir, "verifier-token-hashes.json"))
	if err != nil {
		a.logger.Error("approval-server.init", "err", err)
		return nil, ""
	}
	return srv, srv.Addr()
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
func (a *App) initLoopAgents(ctx context.Context) error {
	if a.database != nil {
		importCtx, cancel := context.WithTimeout(ctx, importTimeout)
		defer cancel()
		// Degrade to the files on a failed import, as every other domain does.
		// Continuing to an empty table drops the operator's schedules for the
		// whole uptime while their definitions sit intact on disk, and the
		// first-boot seed then re-creates the built-in one under a new id — all
		// behind a single log line.
		if err := loopagent.Import(importCtx, a.database, a.cfg.LoopAgentsDir, a.importScope(), a.logger); err != nil {
			a.logger.Error("loopagent.import", "err", err)
		} else if store, err := loopagent.NewSQLStore(a.database); err != nil {
			a.logger.Error("loopagent.store.init", "backend", a.cfg.DatabaseBackend(), "err", err)
		} else {
			a.loopAgents = store
			return nil
		}
	}
	store, err := loopagent.NewStore(a.cfg.LoopAgentsDir)
	if err != nil {
		a.logger.Error("loopagent.store.init", "err", err)
		return err
	}
	a.loopAgents = store
	return nil
}

// initProjects opens the project metadata store and applies the resolved
// commit-signing policy to it and to the workflows no dispatcher seeds.
func (a *App) initProjects(ctx context.Context) error {
	projStore, err := a.openProjectStore(ctx)
	if err != nil {
		a.logger.Error("project.store.init", "err", err)
		return fmt.Errorf("project store: %w", err)
	}
	signingPolicy := project.NormalizeSigningPolicy(a.cfg.CommitSigning())
	projStore.SetSigningPolicy(signingPolicy)
	workflow.SetDefaultCommitSignFlags(signingPolicy.CommitFlags(ctx))
	a.projects = projStore
	// Retrofits maintenance.auto=false onto existing clones; see #2978.
	if err := projStore.MigrateDisableAutoMaintenance(ctx); err != nil {
		a.logger.Warn("project.store.migrate_maintenance_auto", "err", err)
	}
	return nil
}

// initDatabase opens the configured backend and applies pending migrations.
// A wrong or unreachable setting aborts startup here rather than surfacing as
// a store failure later, so the operator sees which setting is at fault.
func (a *App) initDatabase(ctx context.Context) error {
	if !a.cfg.DatabaseEnabled() {
		return nil
	}
	backend := a.cfg.DatabaseBackend()
	dsn := a.cfg.DatabaseDSN()
	database, err := db.Open(ctx, db.Options{
		Backend:         backend,
		DSN:             dsn,
		MaxOpenConns:    a.cfg.Database.MaxOpenConns,
		MaxIdleConns:    a.cfg.Database.MaxIdleConns,
		ConnMaxLifetime: time.Duration(a.cfg.Database.ConnMaxLifetimeSeconds) * time.Second,
	})
	if err != nil {
		a.logger.Error("db.open", "backend", backend, "dsn", db.RedactDSN(dsn), "err", err)
		return fmt.Errorf("database: %w", err)
	}
	version, err := db.SchemaVersion(ctx, database)
	if err != nil {
		_ = database.Close()
		a.logger.Error("db.schema_version", "backend", backend, "err", err)
		return fmt.Errorf("database: %w", err)
	}
	a.database = database
	runGrants, err := agentgrant.New(filepath.Join(config.HomeDir(), "run-grants.json"), 15*time.Minute)
	if err != nil {
		_ = database.Close()
		return fmt.Errorf("run grants: %w", err)
	}
	a.workerControl = workercontrol.NewWithGrantStore(database, runGrants)
	a.workerControl.SetGrantAuditSink(func(event agentgrant.AuditEvent) {
		eventType := audit.EventRunGrantUsed
		switch event.Kind {
		case "grant.issued":
			eventType = audit.EventRunGrantIssued
		case "grant.revoked":
			eventType = audit.EventRunGrantRevoked
		}
		_ = a.audit.Log(audit.Event{Type: eventType, TaskID: event.TaskID, Data: map[string]any{
			"run_id": event.RunID, "effect_id": event.EffectID, "workflow_generation": event.WorkflowGeneration,
			"action": event.Action, "allowed": event.Allowed,
		}})
	})
	a.logger.Info("db.ready", "backend", backend, "dsn", db.RedactDSN(dsn), "schema_version", version)
	return nil
}

func (a *App) closeDatabase() {
	if a.database == nil {
		return
	}
	if err := a.database.Close(); err != nil {
		a.logger.Warn("db.close", "err", err)
	}
	a.database = nil
	a.workerControl = nil
}

func (a *App) initLoopScheduler(ctx context.Context, emit func(string, any)) {
	a.loopSched = loopagent.NewScheduler(ctx, a.loopAgents, a.agents, a.logger, emit, config.HomeDir())
	a.seedDefaultLoopAgents(ctx)
	a.loopSched.SyncContext(ctx)
}

func (a *App) seedDefaultLoopAgents(ctx context.Context) {
	if a.loopAgents == nil {
		return
	}
	const name = "sybra-self-monitor"
	created, inserted, err := a.loopAgents.CreateIfAbsentByName(ctx, loopagent.LoopAgent{
		Name:         name,
		Prompt:       "/sybra-self-monitor",
		IntervalSec:  21600, // 6 hours
		AllowedTools: []string{"Bash", "Read", "Grep", "Glob"},
		Provider:     providerid.Claude,
		Model:        "sonnet",
		Enabled:      false,
	})
	if err != nil {
		a.logger.Warn("loopagent.seed.failed", "name", name, "err", err)
		return
	}
	if !inserted {
		return
	}
	a.logger.Info("loopagent.seed.created", "id", created.ID, "name", name)
}

// newRecovery wires the App's deps into a recovery.Recovery used for
// boot-time cleanup and the periodic restart-stale sweep called from the
// orchestrator loop. Holds a pointer to a.restartStaleErr so the throttle
// state is shared across both call sites.
func (a *App) newRecovery() *recovery.Recovery {
	var retention, gzipAfter time.Duration
	if days := a.cfg.DefaultLogRetentionDays(); days > 0 {
		retention = time.Duration(days) * 24 * time.Hour
	}
	if days := a.cfg.DefaultLogGzipAfterDays(); days > 0 {
		gzipAfter = time.Duration(days) * 24 * time.Hour
	}
	var maxTotalBytes int64
	if mb := a.cfg.DefaultLogRetentionMaxSizeMB(); mb > 0 {
		maxTotalBytes = int64(mb) * 1024 * 1024
	}
	r := &recovery.Recovery{
		Tasks:              a.tasks,
		Agents:             a.agents,
		Worktrees:          a.worktrees,
		Sandboxes:          a.sandboxes,
		WorkflowEngine:     a.workflowEngine,
		Orchestrator:       a.agentOrch,
		Projects:           a.projects,
		PRs:                newRecoveryPRResolver(a.projects),
		Reconciler:         a.postRunReconciliation(),
		Logger:             a.logger,
		Throttle:           a.restartStaleErr,
		WG:                 &a.wg,
		ProtectedFindings:  a.cleanupProtected,
		LogDir:             a.logDir,
		LogRetention:       retention,
		LogGzipAfter:       gzipAfter,
		LogMaxTotalBytes:   maxTotalBytes,
		TrashRetentionDays: a.cfg.DefaultTrashRetentionDays(),
		OrphanRoots: []string{
			filepath.Join(config.HomeDir(), "sandboxes"),
			filepath.Join(config.HomeDir(), "worktrees"),
			// The /sybra-test skill's fake-provider harness spawns its
			// subject process directly under a fresh os.TempDir()/sybra-test-*
			// sandbox, outside the normal task/worktree/sandbox lifecycle
			// (sybra#2210) — glob-expanded fresh on every sweep since each
			// test run gets its own directory.
			filepath.Join(os.TempDir(), "sybra-test-*"),
			filepath.Join(os.TempDir(), "sybra-k8s-poc-*"),
		},
		OwnedOrphanRoots: []string{
			// These roots can also contain operator-run provider processes, so
			// recovery requires the explicit SYBRA_AGENT_OWNER marker here.
			config.HomeDir(),
			a.repoDir,
		},
		// Also gate on the instance role: RunStartupCleanup calls
		// RestartStaleInProgress outside the (gated) maintenance pass, so an
		// agent-only instance would otherwise restart a stale in-progress task
		// on boot with no operator action. Evaluated per call, so it sees the
		// role applyInstanceRole resolves at the top of startLifecycle.
		DispatchGate: func(t task.Task) bool { return a.runsScheduler() && a.runsTaskLocally(t) },
	}
	if a.workflowEngine != nil {
		r.ConflictRecovery = a.workflowEngine.TryConflictRecovery
	}
	// Gate on the config, not just a non-nil snapshotter: the snapshotter is
	// always constructed, but when the feature is disabled its repo is never
	// created, so a pre-prune commit would fail on every sweep forever.
	if a.snapshotter != nil && a.cfg.TaskSnapshotEnabled() {
		r.CommitBeforePrune = a.snapshotter.CommitNow
	}
	return r
}

// syncSkillsBundle drives the skillsync package with the App's source/dst
// configuration. UserHomeDir is best-effort — when unavailable the user-home
// destinations (~/.claude/skills, ~/.codex/skills) are silently skipped so
// startup still succeeds in environments without a usable home dir.
func (a *App) syncSkillsBundle(signing project.SigningPolicy) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		a.logger.Debug("skills.sync.no_user_home", "err", err)
		userHome = ""
	}
	(&skillsync.Syncer{Logger: a.logger}).Run(skillsync.Options{
		RepoDir:              a.repoDir,
		SkillsFS:             a.skillsFS,
		PrimaryDst:           a.skillsDir,
		SybraHomeDir:         config.HomeDir(),
		UserHomeDir:          userHome,
		DowngradeCommitFlags: !signing.SignsCommits(context.Background()),
	})
}

// openWorkflowStore returns the definition store for the configured backend, importing the existing files the first time a database is used.
//
// A failed import falls back to files rather than starting on a half-populated table: an empty workflow set means no task can dispatch at all.
func (a *App) openWorkflowStore(ctx context.Context) (workflow.Repository, error) {
	dir := config.WorkflowsDir()
	if a.database != nil {
		ctx, cancel := context.WithTimeout(ctx, importTimeout)
		defer cancel()
		if err := workflow.Import(ctx, a.database, dir, a.importScope(), a.logger); err != nil {
			a.logger.Error("workflow.import", "err", err)
		} else if store, err := workflow.NewSQLStore(a.database); err != nil {
			a.logger.Error("workflow.store.init", "backend", a.cfg.DatabaseBackend(), "err", err)
		} else {
			return store, nil
		}
	}
	store, err := workflow.NewStore(dir)
	if err != nil {
		// Returned as a nil interface, not a nil *Store: a typed nil is a
		// non-nil interface, so every caller's nil check passes it through and
		// the first method call panics on the nil receiver.
		return nil, err
	}
	return store, nil
}

// importTimeout bounds a one-off copy of a domain's files into the database, on top of the startup context it derives from, so a stalled backend cannot hold startup open forever even while nothing has cancelled it.
const importTimeout = 2 * time.Minute

// openBgopStore returns where background operations survive a restart, importing the existing document the first time a database is used.
//
// A failed import degrades to the file: these records drive a progress panel, and losing them costs an operator visibility rather than work.
func (a *App) openBgopStore(ctx context.Context) bgop.Persistence {
	path := filepath.Join(config.HomeDir(), "bgops.json")
	if a.database != nil {
		ctx, cancel := context.WithTimeout(ctx, importTimeout)
		defer cancel()
		// The home this instance serves, digested: stable across its restarts so
		// it reclaims its own rows, and different from any other instance
		// sharing the board so it never deletes theirs.
		owner := a.importScope()
		if err := bgop.Import(ctx, a.database, path, owner, a.logger); err != nil {
			a.logger.Error("bgop.import", "err", err)
		} else if store, err := bgop.NewSQLPersistence(a.database, owner); err != nil {
			a.logger.Error("bgop.store.init", "backend", a.cfg.DatabaseBackend(), "err", err)
		} else {
			return store
		}
	}
	return bgop.NewFilePersistence(path)
}

// importScope identifies whose files an import is copying in.
//
// A postgres board is shared by several machines, each with its own home and
// its own files. Keyed by domain alone, whichever instance started first would
// claim the domain and every other machine's records would sit unimported
// forever with nothing reporting it. The digest is stable across this
// instance's restarts and differs between instances.
func (a *App) importScope() string {
	return httpserve.HomeID(config.HomeDir())
}

// openAuditStore returns the audit trail for the configured backend, importing
// the existing day-files the first time a database is used. A failed import
// degrades to the files: the trail is what an operator reads to explain what
// happened, and an empty one reads as "nothing happened".
func (a *App) openAuditStore(ctx context.Context) (audit.Store, error) {
	if a.database != nil {
		importCtx, cancel := context.WithTimeout(ctx, importTimeout)
		defer cancel()
		if err := audit.Import(importCtx, a.database, a.auditDir, a.importScope(), a.logger); err != nil {
			a.logger.Error("audit.import", "err", err)
		} else if store, err := audit.NewSQLStore(a.database); err != nil {
			a.logger.Error("audit.store.init", "backend", a.cfg.DatabaseBackend(), "err", err)
		} else {
			return store, nil
		}
	}
	store, err := audit.NewLogger(a.auditDir)
	if err != nil {
		return nil, err
	}
	return store, nil
}

// openToolLedger returns the tool-call ledger for the configured backend.
func (a *App) openToolLedger(ctx context.Context) (toolledger.Store, error) {
	if a.database != nil {
		importCtx, cancel := context.WithTimeout(ctx, importTimeout)
		defer cancel()
		if err := toolledger.Import(importCtx, a.database, a.cfg.ToolLedgerDir(), a.importScope(), a.logger); err != nil {
			a.logger.Error("tool_ledger.import", "err", err)
		} else if store, err := toolledger.NewSQLStore(a.database); err != nil {
			a.logger.Error("tool_ledger.store.init", "backend", a.cfg.DatabaseBackend(), "err", err)
		} else {
			return store, nil
		}
	}
	l, err := toolledger.New(a.cfg.ToolLedgerDir())
	if err != nil {
		return nil, err
	}
	return l, nil
}

// openLimitsStore returns the quota store for the configured backend. A failed
// import degrades to the file: quota state gates provider selection, and
// starting from nothing would read as unlimited headroom on every provider.
func (a *App) openLimitsStore(ctx context.Context) (*limits.Store, error) {
	if a.database != nil {
		importCtx, cancel := context.WithTimeout(ctx, importTimeout)
		defer cancel()
		if err := limits.Import(importCtx, a.database, config.LimitsFile(), a.importScope(), a.logger); err != nil {
			a.logger.Error("limits.import", "err", err)
		} else if persistence, err := limits.NewSQLPersistence(a.database); err != nil {
			a.logger.Error("limits.store.init", "backend", a.cfg.DatabaseBackend(), "err", err)
		} else if store, err := limits.NewStoreWith(persistence); err != nil {
			a.logger.Error("limits.store.init", "backend", a.cfg.DatabaseBackend(), "err", err)
		} else {
			return store, nil
		}
	}
	return limits.NewStore(config.LimitsFile())
}

// openTaskPersistence returns the task.Persistence Manager's CRUD runs against for the configured backend, importing existing task files the first time a database is used, or nil when the caller should build the Manager with task.NewManager(fileStore, ...) instead. That constructor wires fileStore through its own file-backed Persistence adapter internally, so a nil return here is never itself passed to task.NewManagerWithPersistence, which requires a non-nil Persistence.
//
// fileStore is still required either way: Comments/Plans/PlanDrafts, the
// trash-generation history, and the leader-follower mirror's direct sidecar
// writes are not part of Persistence yet (see the follow-up issue linked
// from #3268), so Manager keeps reaching the file store for those regardless
// of which Persistence backs task CRUD. A failed import degrades to the
// files the same way every other domain's does here.
func (a *App) openTaskPersistence(ctx context.Context) task.Persistence {
	if a.database == nil {
		return nil
	}
	importCtx, cancel := context.WithTimeout(ctx, importTimeout)
	defer cancel()
	if err := taskdb.Import(importCtx, a.database, a.tasksDir, a.importScope(), a.logger); err != nil {
		a.logger.Error("task.import", "err", err)
		return nil
	}
	// A board that already ran the import above under the sidecarsOnDisk
	// suffix bug fixed alongside this call is missing PlanContract/CodeReview/
	// SpecDecision sidecars permanently otherwise, since dbimport.Once never
	// retries a domain that already completed. This runs under its own
	// marker so it costs nothing once it has caught every affected task up;
	// a failure here does not fall back to the file backend the way the
	// import above does — the core task data already imported correctly, so
	// missing three sidecar kinds is a gap to log and retry next start, not
	// a reason to abandon the whole database backend. Own timeout, not
	// importCtx's already-derived deadline: a large first-time import can
	// spend most of that budget before this even starts scanning.
	backfillCtx, backfillCancel := context.WithTimeout(ctx, importTimeout)
	defer backfillCancel()
	if err := taskdb.BackfillMissingSidecarKinds(backfillCtx, a.database, a.tasksDir, a.importScope(), a.logger); err != nil {
		a.logger.Error("task.import.sidecar_backfill", "err", err)
	}
	sqlStore, err := taskdb.NewSQLStore(a.database)
	if err != nil {
		a.logger.Error("task.store.init", "backend", a.cfg.DatabaseBackend(), "err", err)
		return nil
	}
	sqlStore.SetMaxHistoryPerTask(a.cfg.Database.MaxTaskHistoryPerTask)
	projectionCtx, projectionCancel := context.WithTimeout(ctx, importTimeout)
	defer projectionCancel()
	if err := sqlStore.BackfillBoardProjections(projectionCtx); err != nil {
		// Core task rows remain usable through doc while every completed
		// projection row is already checkpointed. Keep the database backend and
		// retry the remaining legacy rows next start instead of failing startup.
		a.logger.Error("task.board_projection.backfill", "err", err)
	}
	a.maintainTaskStorage(ctx, sqlStore)
	return taskdb.NewPersistence(sqlStore)
}

// maintainTaskStorage brings a board that ran without the current document
// and history caps down to them.
//
// Off the startup path: the per-write trim already bounds every task from here
// on, so this is catch-up for what accumulated before, and a board large enough
// to need it is exactly the one that must not wait for it before serving. It is
// tracked on the App's wait group so Shutdown cannot close the database out from
// under it, and it runs after the board-projection backfill rather than beside
// it, because sqlite admits one writer and the two would otherwise contend.
func (a *App) maintainTaskStorage(ctx context.Context, store *taskdb.SQLStore) {
	a.wg.Go(func() {
		if compacted, err := store.CompactOversizedDocuments(ctx); err != nil {
			a.logger.Warn("task.document.compact_oversized", "compacted", compacted, "err", err)
		} else if compacted > 0 {
			a.logger.Info("task.document.compact_oversized", "compacted", compacted)
		}
		if err := store.TrimHistoryOverCap(ctx); err != nil {
			a.logger.Warn("task.history.trim_over_cap", "err", err)
		}
	})
}

// openProjectStore returns the project records for the configured backend,
// importing the existing files the first time a database is used.
//
// Clones stay on disk either way; only the record moves. A failed import
// degrades to the files: an empty project list reads as "nothing is
// registered", and every task carrying a project id would lose its repository.
func (a *App) openProjectStore(ctx context.Context) (*project.Store, error) {
	dir := filepath.Join(config.HomeDir(), "projects")
	clones := filepath.Join(config.HomeDir(), "clones")
	if a.database != nil {
		importCtx, cancel := context.WithTimeout(ctx, importTimeout)
		defer cancel()
		if err := project.Import(importCtx, a.database, dir, a.importScope(), a.logger); err != nil {
			a.logger.Error("project.import", "err", err)
		} else if store, err := project.NewSQLStore(a.database); err != nil {
			a.logger.Error("project.store.init", "backend", a.cfg.DatabaseBackend(), "err", err)
		} else {
			return project.NewStoreWith(dir, clones, store)
		}
	}
	return project.NewStore(dir, clones)
}

// openAttemptLedger returns the admission ledger for the configured backend,
// importing the existing document the first time a database is used.
//
// It fails closed rather than degrading. Every other store here falls back to
// files when its backend cannot be opened, because a degraded advisory store
// costs an operator information. This one is coordination: on a board shared by
// several machines, one instance falling back to its own file while the others
// use the database means two ledgers deciding admission independently — which
// is precisely the double-dispatch this backend exists to prevent, and worse
// than not starting.
//
// Call it only with a database configured; the file-backed deployment keeps the
// YAML ledger and never reaches here.
func (a *App) openAttemptLedger(ctx context.Context) (dispatch.Persistence, error) {
	if a.database == nil {
		return nil, errors.New("attempt lease store: no database configured")
	}
	importCtx, cancel := context.WithTimeout(ctx, importTimeout)
	defer cancel()
	if err := dispatch.Import(importCtx, a.database, config.AttemptLeasesDir(), a.importScope(), a.logger); err != nil {
		return nil, fmt.Errorf("attempt lease import: %w", err)
	}
	store, err := dispatch.NewSQLPersistence(a.database)
	if err != nil {
		return nil, fmt.Errorf("attempt lease store: %w", err)
	}
	return store, nil
}

// openAgentQueueStore returns the queue's durability mirror for the configured
// backend, importing the existing item files the first time a database is used.
//
// A failed import returns nil, which leaves the queue on its files. Starting on
// an empty mirror would drop every queued item on the next restart.
func (a *App) openAgentQueueStore(ctx context.Context) agentqueue.Persistence {
	if a.database == nil {
		return nil
	}
	importCtx, cancel := context.WithTimeout(ctx, importTimeout)
	defer cancel()
	if err := agentqueue.Import(importCtx, a.database, config.AgentQueueDir(), a.importScope(), a.logger); err != nil {
		a.logger.Error("agentqueue.import", "err", err)
		return nil
	}
	store, err := agentqueue.NewSQLStore(a.database, a.logger)
	if err != nil {
		a.logger.Error("agentqueue.store.init", "backend", a.cfg.DatabaseBackend(), "err", err)
		return nil
	}
	return store
}

// openIncidentStore returns the incident ledger for the configured backend,
// importing the existing files the first time a database is used.
//
// A failed import degrades to the files: an empty ledger reads as "nothing has
// ever failed", so the monitor would re-file every open incident as new.
func (a *App) openIncidentStore(ctx context.Context) (*monitor.IncidentStore, error) {
	dir := config.MonitorIncidentsDir()
	if a.database != nil {
		importCtx, cancel := context.WithTimeout(ctx, importTimeout)
		defer cancel()
		if err := monitor.ImportIncidents(importCtx, a.database, dir, a.importScope(), a.logger); err != nil {
			a.logger.Error("incidents.import", "err", err)
		} else if store, err := monitor.NewSQLIncidentStore(a.database); err != nil {
			a.logger.Error("incidents.store.init", "backend", a.cfg.DatabaseBackend(), "err", err)
		} else {
			return monitor.NewIncidentStoreWith(dir, store)
		}
	}
	return monitor.NewIncidentStore(dir)
}

// newDurableIssueSink returns the retrying GitHub issue sink for the configured
// backend, importing the pending outbox the first time a database is used.
//
// A failed import degrades to the files: an empty outbox reads as nothing
// pending, so a filing stranded by an expired credential would never be
// retried and the incident it belongs to would go unreported.
func (a *App) newDurableIssueSink(ctx context.Context, inner *monitor.GHIssueSink, name string) (*monitor.DurableGHIssueSink, error) {
	dir := filepath.Join(config.GHIssueOutboxDir(), name)
	if a.database != nil {
		ctx, cancel := context.WithTimeout(ctx, importTimeout)
		defer cancel()
		if err := monitor.ImportIssueOutbox(ctx, a.database, dir, a.importScope(), a.logger); err != nil {
			a.logger.Error("issueoutbox.import", "err", err)
		} else if store, err := monitor.NewSQLIssueOutbox(a.database, a.logger); err != nil {
			a.logger.Error("issueoutbox.store.init", "backend", a.cfg.DatabaseBackend(), "err", err)
		} else {
			return monitor.NewDurableGHIssueSinkWith(inner, store, name, a.logger, a.audit), nil
		}
	}
	return monitor.NewDurableGHIssueSink(inner, dir, name, a.logger, a.audit)
}

// openProtectedStore returns the protected-findings ledger for the configured
// backend, importing the existing document the first time a database is used.
//
// A failed import degrades to the document: an empty ledger reads as nothing
// protected, and the next cleanup pass is then free to delete the very paths
// the findings existed to hold.
func (a *App) openProtectedStore(ctx context.Context) *cleanup.ProtectedStore {
	path := cleanup.DefaultProtectedStorePath()
	if a.database != nil {
		importCtx, cancel := context.WithTimeout(ctx, importTimeout)
		defer cancel()
		if err := cleanup.ImportProtected(importCtx, a.database, path, a.importScope(), a.logger); err != nil {
			a.logger.Error("protectedfindings.import", "err", err)
		} else if store, err := cleanup.NewSQLProtectedStore(a.database); err != nil {
			a.logger.Error("protectedfindings.store.init", "backend", a.cfg.DatabaseBackend(), "err", err)
		} else {
			return cleanup.NewProtectedStoreWith(path, store)
		}
	}
	return cleanup.NewProtectedStore(path)
}
