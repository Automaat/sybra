package sybra

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/agentqueue"
	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/bgop"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/experience"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/learning"
	"github.com/Automaat/sybra/internal/limits"
	"github.com/Automaat/sybra/internal/loopagent"
	"github.com/Automaat/sybra/internal/monitor"
	"github.com/Automaat/sybra/internal/notification"
	"github.com/Automaat/sybra/internal/poll"
	"github.com/Automaat/sybra/internal/prcontent"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/prompteval"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/recovery"
	"github.com/Automaat/sybra/internal/skillsync"
	"github.com/Automaat/sybra/internal/stats"
	"github.com/Automaat/sybra/internal/sybra/clusterlead"
	"github.com/Automaat/sybra/internal/sybra/review"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/triage"
	"github.com/Automaat/sybra/internal/umbrella"
	"github.com/Automaat/sybra/internal/watcher"
	"github.com/Automaat/sybra/internal/workflow"
)

const liveLimitPollInterval = 15 * time.Minute

type startupDegradedEvent struct {
	Subsystem string `json:"subsystem"`
	Reason    string `json:"reason"`
}

func (a *App) initBgops(emit func(string, any)) {
	a.bgops = bgop.NewTracker(emit, filepath.Join(config.HomeDir(), "bgops.json"), a.logger)
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
			"issues_enabled", a.cfg.GitHub.IssuesEnabled,
		)
		return nil
	}
	f := poll.NewIssuesFetcher(a.tasks, a.projects, emit, a.logger, a.allowsProjectType)
	f.SetPollInterval(a.cfg.GitHub.Issues())
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
	promptevalRunner := prompteval.SelectRunner(a.cfg.Evaluation.Offline)
	a.logger.Info("app.automations",
		"todoist", a.cfg.Todoist.Enabled && a.cfg.Todoist.APIToken != "",
		"github", a.cfg.GitHub.Enabled,
		"github_issues", a.cfg.GitHub.RunsIssuesFetcher(),
		"github_reviews", a.cfg.GitHub.RunsReviewer(),
		"renovate", a.cfg.Renovate.Enabled,
		"triage", a.cfg.Triage.Enabled,
		"human_review", a.humanReview != nil,
		"project_types", projectTypes,
		"loop_agents_enabled", loopAgentsEnabled,
		"prompteval_runner", promptevalRunner.Name(),
		"promptfoo_present", (&prompteval.PromptfooRunner{}).Available(),
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

// initLocalStores wires the small local-only data stores that degrade to nil
// on failure rather than blocking startup.
func (a *App) initLocalStores() {
	a.initArtifacts()
	a.initExperience()
	a.initLearning()
	a.initAgentQueue()
}

func (a *App) initExperience() {
	store, err := experience.New(a.cfg.ExperiencesDir())
	if err != nil {
		a.logger.Warn("experience.init.degraded", "err", err)
		return
	}
	a.experience = store
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

func (a *App) initAgentQueue() {
	queue, err := agentqueue.New(config.AgentQueueDir(), agentqueue.Options{MaxDepth: a.cfg.Agent.Queue.MaxDepth}, a.logger)
	if err != nil {
		a.logger.Warn("agentqueue.init.degraded", "err", err)
		return
	}
	a.agentQueue = queue
}

func (a *App) initLimits() {
	limitStore, err := limits.NewStore(config.LimitsFile())
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

func (a *App) agentManagerConfig(approvalAddr string) agent.ManagerConfig {
	cfg := agent.ManagerConfig{
		Runtime:      a.agentRuntimeConfig(a.cfg),
		OnComplete:   a.onAgentComplete,
		ApprovalAddr: approvalAddr,
		SessionSink: func(taskID, agentID, sessionID string) error {
			return a.tasks.UpdateRun(taskID, agentID, task.RunPatch{SessionID: task.Ptr(sessionID)})
		},
		TaskExists:  a.taskExistsForAgent,
		TaskStatus:  a.taskStatusForAgent,
		LimitSink:   a.recordLimitSnapshot,
		SandboxHome: a.sandboxes.SybraHomeDir,
		ControlHome: config.HomeDir(),
	}
	if a.cfg.SurviveRestartEnabled() {
		cfg.SurviveRestartDir = config.AgentsDir()
	}
	return cfg
}

func (a *App) initAgentManager(ctx context.Context, emit func(string, any)) error {
	approvalServer, approvalAddr := a.startApprovalServer(ctx, emit)
	agentCfg := a.agentManagerConfig(approvalAddr)
	var err error
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
	a.agents.SetGHAppToken(github.CurrentAppToken)
	if agentCfg.SurviveRestartDir != "" {
		a.logger.Info("agent.survive-restart.enabled", "dir", agentCfg.SurviveRestartDir)
	}
	if approvalServer != nil {
		approvalServer.SetManager(a.agents)
		a.agentSvc.approval = approvalServer
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
		BashTimeoutMs:          cfg.BashTimeoutMs(),
		RetryWatchdog:          cfg.RetryWatchdog(),
		FallbackModel:          cfg.Agent.FallbackModel,
		LimitGate:              agent.LimitGateOrNil(a.limits),
		LimitPolicy:            policy,
		MaxInFlightPerProvider: cfg.Providers.Limits.MaxInFlightPerProvider,
		DispatchJitterMs:       cfg.Agent.DispatchJitterMs,
		HeadlessSteerable:      cfg.DefaultHeadlessSteerable(),
		PlaywrightMCPEnabled:   cfg.PlaywrightMCPEnabled(),
		PlaywrightMCPExtraArgs: cfg.PlaywrightMCPExtraArgs(),
	}
}

func (a *App) onAgentComplete(ag *agent.Agent) {
	if a.agentCompletion == nil {
		a.logger.Warn("agent.complete.unwired", "id", ag.ID, "task_id", ag.TaskID)
		return
	}
	a.agentCompletion.OnComplete(ag)
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
	a.wg.Go(func() {
		a.refreshLiveLimits(ctx, limitStore, policy)
		ticker := time.NewTicker(liveLimitPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.refreshLiveLimits(ctx, limitStore, policy)
			}
		}
	})
}

func (a *App) refreshLiveLimits(ctx context.Context, limitStore *limits.Store, policy limits.Policy) {
	if err := limitStore.RefreshLiveSnapshots(ctx, policy); err != nil {
		if ctx.Err() != nil {
			return
		}
		a.logger.Warn("limits.live_poll", "err", err)
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
		releaseTaskAgents := shouldReleaseTaskAgentsForStatus(task.Status(to))
		data := map[string]any{"from": from, "to": to}
		if to == string(task.StatusHumanRequired) {
			if t, err := a.tasks.Get(taskID); err == nil {
				if kind := expectedHumanKind(t); kind != "" {
					data["human_kind"] = kind
				}
			}
		}
		a.logAudit(audit.EventTaskStatusChanged, taskID, "", data)

		local := true
		if t, err := a.tasks.Get(taskID); err == nil {
			local = a.runsTaskLocally(t)
		}

		// Wake the dispatch pass immediately so a task that just became ready
		// (e.g. a dependency completing, a stage advancing) is picked up now
		// instead of waiting for the next fast tick.
		a.nudgeDispatch()

		// Advance workflows whose current run_agent step declares a
		// matching wait_for_status. This is how interactive agents (which
		// never exit between turns) signal step completion.
		if local && a.workflowEngine != nil {
			a.workflowEngine.HandleStatusChange(taskID, to)
		}
		if local && releaseTaskAgents {
			a.releaseTaskAgents(taskID)
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
			if local && a.humanReview != nil {
				go a.humanReview.maybeSpawn(taskID, from)
			}
		case string(task.StatusReadyReview):
			a.dispatchStatusWorkflow(taskID, task.StatusReadyReview)
		case string(task.StatusTesting):
			a.dispatchStatusWorkflow(taskID, task.StatusTesting)
		case string(task.StatusReadyPR):
			a.dispatchStatusWorkflow(taskID, task.StatusReadyPR)
		case string(task.StatusDone):
			if local {
				go a.closeLinkedIssueOnDone(taskID)
			}
		}
	})
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
	filtered := make([]*agent.Agent, 0, len(targets))
	for _, ag := range targets {
		if agent.RoleFromName(ag.Name) == agent.RoleHumanReview {
			continue
		}
		filtered = append(filtered, ag)
	}
	if len(filtered) == 0 {
		return
	}
	go func(agentsToStop []*agent.Agent) {
		for _, ag := range agentsToStop {
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
	}(filtered)
}

func (a *App) runsTaskLocally(t task.Task) bool {
	return a.cfg == nil || a.cfg.HomeNodeFor(t.ProjectID).Local
}

func (a *App) initCluster() {
	if a.workflowEngine != nil {
		a.workflowEngine.SetDispatchGate(func(ti workflow.TaskInfo) bool {
			return a.cfg == nil || a.cfg.HomeNodeFor(ti.ProjectID).Local
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
	a.assigner = clusterlead.NewAssigner(a.cfg, a.tasks, roster, a.logger)
	a.mirror = clusterlead.NewMirror(a.cfg, a.tasks, roster, a.logger, 0)
	if a.clusterSvc != nil {
		a.clusterSvc.SetRoster(roster)
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
	if a.workflowEngine == nil {
		return
	}
	if t, err := a.tasks.Get(taskID); err == nil && !a.runsTaskLocally(t) {
		return
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
	case t.ReviewPhase == review.ReviewPhaseManual ||
		strings.HasPrefix(t.StatusReason, "PR too small for agent review"):
		return "review_manual"
	default:
		return ""
	}
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
		if !a.runsTaskLocally(t) {
			if a.assigner != nil {
				if _, err := a.assigner.Route(a.ctx, t); err != nil {
					a.logger.Warn("cluster.assign.failed", "task_id", id, "err", err)
				}
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
func (a *App) initAutomations(emit func(string, any)) *poll.IssuesFetcher {
	a.initTodoist(emit)
	a.initRenovate(emit)
	a.initPromptLab()
	a.initTriage()
	a.initHumanReview()
	return a.initIssuesFetcher(emit)
}

func (a *App) initWorkflowEngine() {
	if os.Getenv("SYBRA_DISABLE_WORKFLOWS") == "1" {
		a.logger.Info("workflow.disabled")
		return
	}
	q := a.agentQueue
	if q != nil && a.agentOrch != nil {
		a.agentOrch.SetQueue(q)
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
	agentLauncher := &agentAdapter{agents: a.agents, agentOrch: a.agentOrch, tasks: a.tasks, projects: a.projects, sandboxes: a.sandboxes, experience: a.experience}
	a.workflowEngine = workflow.NewEngine(
		wfStore,
		&taskAdapter{tasks: a.tasks, projects: a.projects},
		agentLauncher,
		a.logger,
	)
	a.workflowEngine.SetPRLinker(prLinkerAdapter{})
	a.workflowEngine.SetPRStateFetcher(prStateFetcherAdapter{})
	a.workflowEngine.SetPRHeadFetcher(prHeadFetcherAdapter{})
	a.workflowEngine.SetPRCreator(prCreatorAdapter{})
	a.workflowEngine.SetPRFinder(prFinderAdapter{})
	a.workflowEngine.SetPRContentGenerator(prContentGeneratorAdapter{gen: &prcontent.FallbackGenerator{Logger: a.logger, Gate: a.providerHealth}})
	a.workflowEngine.SetTaskClassifier(&taskClassifierAdapter{
		tasks:      a.tasks,
		projects:   a.projects,
		classifier: &triage.FallbackClassifier{Model: a.cfg.Triage.Model, Logger: a.logger, Gate: a.providerHealth},
		audit:      a.audit,
	})
	a.workflowEngine.SetPRReviewRequester(prReviewRequesterAdapter{})
	a.workflowEngine.SetWorktreeGetter(&worktreeGetterAdapter{tasks: a.tasks, mgr: a.worktrees})
	a.workflowEngine.SetAttemptNoteAppender(&attemptNoteAppenderAdapter{})
	a.workflowEngine.SetBranchSyncer(&branchSyncerAdapter{tasks: a.tasks, mgr: a.worktrees})
	a.workflowEngine.SetCheckConfigGetter(&checkConfigGetterAdapter{tasks: a.tasks, projects: a.projects, mgr: a.worktrees})
	a.workflowEngine.SetCostBudgetChecker(agentLauncher)
	a.workflowEngine.SetAttemptWorktreeManager(&attemptWorktreeAdapter{tasks: a.tasks, mgr: a.worktrees})
	a.workflowEngine.SetManualTestConfigGetter(&manualTestConfigGetterAdapter{tasks: a.tasks, projects: a.projects, mgr: a.worktrees})
	a.workflowEngine.SetTestingMaxAttempts(a.cfg.TestingMaxAttempts())
	a.workflowEngine.SetMaxCheckpoints(a.cfg.MaxCheckpoints())
	a.workflowEngine.SetABTestingConfig(a.cfg.ABTesting)
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
	a.workflowEngine.SetContext(a.ctx)
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
				it := agentqueue.Item{TaskID: t.ID, Priority: task.Priority(t.Priority), Status: task.Status(t.Status)}
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

func (a *App) initAgentConfig() {
	a.agents.SetGuardrails(agent.Guardrails{
		MaxCostUSD:              a.cfg.Agent.MaxCostUSD,
		MaxTurns:                a.cfg.Agent.MaxTurns,
		MaxCheckpoints:          a.cfg.MaxCheckpoints(),
		TurnCostFraction:        a.cfg.Agent.TurnCostFraction,
		TurnMultiplier:          a.cfg.Agent.TurnMultiplier,
		CheckpointOnTurnCeiling: a.cfg.CheckpointOnTurnCeilingEnabled(),
	})
}

func (a *App) startApprovalServer(ctx context.Context, emit func(string, any)) (srv *agent.ApprovalServer, addr string) {
	srv, err := agent.NewApprovalServer(ctx, emit, a.logger, a.cfg.Agent.ApprovalPort)
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
	a.loopSched.SyncContext(ctx)
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
	r := &recovery.Recovery{
		Tasks:              a.tasks,
		Agents:             a.agents,
		Worktrees:          a.worktrees,
		Sandboxes:          a.sandboxes,
		WorkflowEngine:     a.workflowEngine,
		Orchestrator:       a.agentOrch,
		Projects:           a.projects,
		PRs:                newRecoveryPRResolver(),
		Logger:             a.logger,
		Throttle:           a.restartStaleErr,
		WG:                 &a.wg,
		LogDir:             a.logDir,
		LogRetention:       retention,
		TrashRetentionDays: a.cfg.DefaultTrashRetentionDays(),
		OrphanRoots: []string{
			filepath.Join(config.HomeDir(), "sandboxes"),
			filepath.Join(config.HomeDir(), "worktrees"),
		},
		DispatchGate: a.runsTaskLocally,
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
func (a *App) syncSkillsBundle() {
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
		DowngradeCommitFlags: !project.GPGSigningAvailable(context.Background()),
	})
}
