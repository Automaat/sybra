package sybra

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/scrub"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/verdict"
	"github.com/Automaat/sybra/internal/workflow"
)

// humanReviewPromptHeadTail bounds how many lines of the host log file are
// pulled into the review prompt. Keeps the prompt size predictable.
const (
	humanReviewLogTail        = 200
	humanReviewMaxAgentRuns   = 5
	humanReviewMaxAgentTurns  = 40
	humanReviewMaxAgentResult = 4000
	humanReviewWindow         = time.Hour
	humanReviewFallbackModel  = "haiku"

	// humanReviewMaxPerTaskPerWindow caps how many times a SINGLE task can
	// spawn a review agent within humanReviewWindow. The global window
	// (allowSpawnLocked) bounds total fleet spend but does not stop one
	// flapping task (e.g. status oscillating todo<->human-required) from
	// consuming every slot in the window and starving every other task's
	// diagnosis. This is a per-task budget layered on top of that global one.
	humanReviewMaxPerTaskPerWindow = 2
)

type humanReviewAgentRunner interface {
	ApplyABVariant(cfg agent.RunConfig, ab abtest.Config, taskID, role string) agent.RunConfig
	Run(cfg agent.RunConfig) (*agent.Agent, error)
}

// humanReviewHandler spawns a headless review agent each time a task
// transitions into status=human-required. The agent inspects task state,
// agent runs and Sybra logs/source, then emits a structured verdict
// (verdict.Decision, enforced via --json-schema) which the handler turns
// into a side-effect. sybra_bug verdicts are retained as task notes by
// default; richer issue filing is handled by the why-human workflow.
type humanReviewHandler struct {
	cfg       *config.Config
	abTesting func() abtest.Config
	tasks     *task.Manager
	agents    humanReviewAgentRunner
	audit     *audit.Logger
	logger    *slog.Logger
	homeDir   string
	logFile   string
	now       func() time.Time

	// workCtx returns a non-nil WorkScrubContext when the task's project is
	// work-typed. When set, the handler still spawns the review agent but
	// reroutes the sybra_bug verdict to a local sybra task with the body
	// scrubbed through ctx.Blocklist — no GH issue on Automaat/sybra is
	// filed. See CLAUDE.md — Work-Data Confidentiality. Nil during tests
	// disables the work-project path.
	workCtx func(projectID string) *WorkScrubContext
	// dispatchFromHumanRequired routes an acknowledged human-required task back
	// through the same guarded status->workflow re-entry path the UI uses.
	// Nil-safe for focused tests: applyUnblockedRecovery falls back to the
	// legacy direct status write when dispatch wiring is unavailable.
	dispatchFromHumanRequired func(id, target, reason, completingAgentID string) (task.Task, error)
	// landClosedPR runs the same merged/closed PR landing pipeline the review
	// monitor uses. completingAgentID is excluded from the stop/wait phase.
	landClosedPR func(ctx context.Context, taskID string, prNumber int, state, completingAgentID string) error
	fetchPRState func(repo string, number int) (github.PRState, error)

	mu       sync.Mutex
	inflight map[string]string      // taskID -> agent ID
	recent   []time.Time            // spawn timestamps (rolling window), global cap
	perTask  map[string][]time.Time // taskID -> spawn timestamps (rolling window), per-task cap
}

var humanReviewPRNumberRE = regexp.MustCompile(`(?i)\bpr\s*#(\d+)\b`)

// verdictDecision is the agent's structured output, produced via
// --json-schema (verdict.Schema) and parsed by verdict.Parse. Aliased here
// so the rest of this file (and its tests) keep the historical local name.
type verdictDecision = verdict.Decision

type humanReviewSpawnOptions struct {
	Provider              string
	Model                 string
	IgnoreRenderedVerdict bool
	SkipABVariant         bool
	RetryReason           string
}

func (h *humanReviewHandler) abTestingConfig() abtest.Config {
	if h.abTesting != nil {
		return h.abTesting()
	}
	if h.cfg == nil {
		return abtest.Config{}
	}
	return cloneABTestingConfig(h.cfg.ABTesting)
}

func newHumanReviewHandler(
	cfg *config.Config,
	tasks *task.Manager,
	agents humanReviewAgentRunner,
	al *audit.Logger,
	logger *slog.Logger,
	homeDir, logFile string,
	workCtx func(projectID string) *WorkScrubContext,
) *humanReviewHandler {
	return &humanReviewHandler{
		cfg:          cfg,
		tasks:        tasks,
		agents:       agents,
		audit:        al,
		logger:       logger,
		homeDir:      homeDir,
		logFile:      logFile,
		now:          time.Now,
		workCtx:      workCtx,
		fetchPRState: github.FetchPRStateViaREST,
		inflight:     make(map[string]string),
		perTask:      make(map[string][]time.Time),
	}
}

// initHumanReview is called once during App.Startup. It is a no-op when the
// feature is disabled or no Sybra source dir is configured.
func (a *App) initHumanReview() {
	if a.cfg == nil || !a.cfg.HumanReview.Enabled {
		return
	}
	dir := strings.TrimSpace(a.cfg.HumanReview.SybraRepoDir)
	if dir == "" {
		a.logger.Warn("human-review.disabled", "reason", "sybra_repo_dir is empty")
		return
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		a.logger.Warn("human-review.disabled", "reason", "sybra_repo_dir not a directory", "dir", dir, "err", err)
		return
	}
	logFile := filepath.Join(a.logDir, "sybra.log")
	a.humanReview = newHumanReviewHandler(a.cfg, a.tasks, a.agents, a.audit, a.logger, config.HomeDir(), logFile, a.workScrubContextForTask)
	a.humanReview.abTesting = a.abTestingConfig
	a.logger.Info("human-review.enabled", "dir", dir, "repo", a.cfg.HumanReviewRepo(), "model", a.cfg.HumanReviewModel())
}

// humanReviewDispatchDir resolves the human-review agent's working directory.
// Prefers the task's own project worktree — matching how every other
// dispatched role (test-runner, implementation, review, ...) resolves its
// Dir via resolveWorktreeDir — so the agent's UNBLOCK actions (fix + commit +
// push in the task's own repo, per writeAutonomyMandate phase 2) land
// somewhere the OS process sandbox actually allows writes, instead of being
// silently confined to the configured Sybra source checkout regardless of
// which project the task belongs to. Falls back to the Sybra source tree
// when the task has no worktree yet (e.g. it never made it past triage), or
// when the recorded worktree no longer exists on disk (e.g. cleaned up) —
// diagnosing Sybra's own source is still this agent's primary mission.
//
// readOnly reports whether the fallback branch was taken: the Sybra source
// checkout is a diagnostic-only Read/Grep/Glob target, never a place this
// agent should write to (unlike a task's own worktree, which it may fix,
// commit, and push per writeAutonomyMandate phase 2) — callers must feed it
// into RunConfig.ReadOnlyDir so the process sandbox denies writes there.
func humanReviewDispatchDir(t task.Task, sybraRepoDir string) (dir string, readOnly bool) {
	if d := strings.TrimSpace(t.WorktreeDir); d != "" {
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			return d, false
		}
	}
	return sybraRepoDir, true
}

// maybeSpawn is called from the status hook when a task lands in
// human-required. Returns immediately if the feature is disabled, the task
// already has an in-flight review, or the rolling rate limit is exceeded.
// The bool return reports whether a review agent was actually dispatched —
// retryAfterCrash relies on it to tell a real retry apart from a silent skip.
func (h *humanReviewHandler) maybeSpawn(taskID, prevStatus string) bool {
	if h == nil {
		return false
	}
	// Don't loop when an unblock flow flips blocked → todo → human-required.
	if prevStatus == string(task.StatusBlocked) {
		h.skip(taskID, "prev_status_blocked")
		return false
	}
	t, err := h.tasks.Get(taskID)
	if err != nil {
		h.logger.Error("human-review.task.get", "task_id", taskID, "err", err)
		return false
	}
	// Status guard: the status hook launches maybeSpawn asynchronously
	// (go a.humanReview.maybeSpawn), so a fast recovery path
	// (human-required -> ready-pr / in-review) can flip the task out of
	// human-required before this goroutine re-reads it. Bail if the task is
	// no longer parked at human-required — spawning the autonomy agent
	// against an already-recovered task would race the recovery flow and let
	// the agent's unblock actions rewrite the task to the wrong state.
	if t.Status != task.StatusHumanRequired {
		h.skip(taskID, "stale_status_"+string(t.Status))
		return false
	}
	// Idempotency gate: a prior run already produced a verdict — re-spawning
	// on app restart or repeated status hooks would just duplicate the diagnosis.
	if verdictAlreadyRendered(t) {
		h.skip(taskID, "verdict_rendered")
		return false
	}
	if strings.TrimSpace(t.ProjectID) == "" {
		h.skip(taskID, "no_project")
		return false
	}
	return h.spawnReview(t, prevStatus, humanReviewSpawnOptions{})
}

func (h *humanReviewHandler) spawnReview(t task.Task, prevStatus string, opts humanReviewSpawnOptions) bool {
	taskID := t.ID
	if strings.TrimSpace(t.ProjectID) == "" {
		h.skip(taskID, "no_project")
		return false
	}
	var wctx *WorkScrubContext
	if h.workCtx != nil {
		wctx = h.workCtx(t.ProjectID)
	}

	h.mu.Lock()
	if _, busy := h.inflight[taskID]; busy {
		h.mu.Unlock()
		h.skip(taskID, "in_flight")
		return false
	}
	if !h.allowSpawnLocked() {
		h.mu.Unlock()
		h.skip(taskID, "rate_limited")
		return false
	}
	if !h.allowSpawnForTaskLocked(taskID) {
		h.mu.Unlock()
		h.skip(taskID, "task_rate_limited")
		return false
	}
	h.inflight[taskID] = ""
	now := h.now()
	h.recent = append(h.recent, now)
	h.perTask[taskID] = append(h.perTask[taskID], now)
	h.mu.Unlock()

	dir, readOnlyDir := humanReviewDispatchDir(t, h.cfg.HumanReview.SybraRepoDir)
	prompt := h.buildPrompt(t, dir, wctx)
	model := h.cfg.HumanReviewModel()
	if strings.TrimSpace(opts.Model) != "" {
		model = opts.Model
	}
	cfg := agent.RunConfig{
		TaskID:                 taskID,
		Name:                   agent.RoleHumanReview.AgentName(t.Title),
		Mode:                   "headless",
		Provider:               strings.TrimSpace(opts.Provider),
		Model:                  model,
		Prompt:                 prompt,
		Dir:                    dir,
		ReadOnlyDir:            readOnlyDir,
		RequirePermissions:     false,
		OneShot:                true,
		OutputSchema:           verdict.Schema,
		IgnoreConcurrencyLimit: true,
	}
	if !opts.SkipABVariant {
		cfg = h.agents.ApplyABVariant(cfg, h.abTestingConfig(), taskID, string(agent.RoleHumanReview))
	}
	if !h.preRunEligible(taskID, now, opts.IgnoreRenderedVerdict) {
		return false
	}
	ag, err := h.agents.Run(cfg)
	if err != nil {
		h.clearInflight(taskID)
		h.logger.Error("human-review.spawn", "task_id", taskID, "provider", cfg.Provider, "model", cfg.Model, "err", err)
		h.logAudit(audit.EventHumanReviewSkipped, taskID, "", map[string]any{
			"reason": "spawn_failed", "err": err.Error(),
			"provider": cfg.Provider, "model": cfg.Model, "retry_reason": opts.RetryReason,
		})
		return false
	}
	h.mu.Lock()
	h.inflight[taskID] = ag.ID
	h.mu.Unlock()

	if err := h.tasks.AddRun(taskID, task.AgentRun{
		AgentID:         ag.ID,
		Role:            string(agent.RoleHumanReview),
		Mode:            "headless",
		Provider:        ag.Provider,
		Model:           ag.Model,
		ExperimentID:    ag.ExperimentID,
		VariantID:       ag.VariantID,
		RoutingReason:   ag.RoutingReason,
		AssignmentUnit:  ag.AssignmentUnit,
		AssignmentKey:   ag.AssignmentKey,
		DecisionVersion: ag.DecisionVersion,
		State:           string(agent.StateRunning),
		StartedAt:       ag.StartedAt,
		Prompt:          cfg.Prompt,
	}); err != nil {
		h.logger.Error("human-review.add-run", "task_id", taskID, "agent_id", ag.ID, "err", err)
	}
	h.logAudit(audit.EventHumanReviewSpawned, taskID, ag.ID, map[string]any{
		"prev_status":  prevStatus,
		"provider":     ag.Provider,
		"model":        ag.Model,
		"prompt_hash":  ag.GetPromptHash(),
		"fallback":     opts.RetryReason != "",
		"retry_reason": opts.RetryReason,
	})
	return true
}

func (h *humanReviewHandler) handleStructuredVerdictFailure(current task.Task, ag *agent.Agent, final string, parseErr error) bool {
	if !ag.HasOutputSchema() {
		return false
	}
	retryProvider, retryModel, ok := humanReviewFallbackTarget(ag.Provider)
	if !ok || h.humanReviewCycleRunCount(current) >= humanReviewMaxPerTaskPerWindow {
		return false
	}
	h.markVerdictRendered(current.ID, ag.ID)
	h.logAudit(audit.EventHumanReviewSkipped, current.ID, ag.ID, map[string]any{
		"reason":         "structured_verdict_retry",
		"err":            parseErr.Error(),
		"provider":       ag.Provider,
		"model":          ag.Model,
		"retry_provider": retryProvider,
		"retry_model":    retryModel,
		"empty_output":   strings.TrimSpace(final) == "",
	})
	h.logger.Warn("human-review.verdict.retry",
		"task_id", current.ID, "agent_id", ag.ID,
		"provider", ag.Provider, "model", ag.Model,
		"retry_provider", retryProvider, "retry_model", retryModel,
		"err", parseErr)
	h.mu.Lock()
	delete(h.inflight, current.ID)
	h.mu.Unlock()
	return h.spawnReview(current, string(task.StatusHumanRequired), humanReviewSpawnOptions{
		Provider:              retryProvider,
		Model:                 retryModel,
		IgnoreRenderedVerdict: true,
		SkipABVariant:         true,
		RetryReason:           "structured_verdict_failure",
	})
}

func humanReviewFallbackTarget(currentProvider string) (provider, model string, ok bool) {
	currentProvider = strings.TrimSpace(currentProvider)
	switch currentProvider {
	case "claude":
		return "codex", humanReviewFallbackModel, true
	case "codex":
		return "claude", humanReviewFallbackModel, true
	}
	for _, candidate := range []string{"codex", "claude"} {
		if candidate == currentProvider || !agent.ProviderSupportsOutputSchema(candidate) {
			continue
		}
		return candidate, humanReviewFallbackModel, true
	}
	return "", "", false
}

func (h *humanReviewHandler) humanReviewCycleRunCount(t task.Task) int {
	var count int
	for i := range t.AgentRuns {
		run := t.AgentRuns[i]
		if run.Role != string(agent.RoleHumanReview) {
			continue
		}
		if t.TestingCycleStartedAt != nil && run.StartedAt.Before(*t.TestingCycleStartedAt) {
			continue
		}
		count++
	}
	return count
}

func (h *humanReviewHandler) applyUnblockedRecovery(current task.Task, agentID string, v verdictDecision) {
	note := h.scrubForTask(current.ProjectID, v.Summary)
	status, ok := safeHumanReviewRecoveryStatus(v.RecoverableAction)
	if !ok || current.Status != task.StatusHumanRequired {
		if h.appendNote(current.ID, "Auto-review: unblocked claim not verified", note) {
			h.markVerdictRendered(current.ID, agentID)
		}
		return
	}
	if status == task.StatusDone {
		h.applyDoneRecovery(current, agentID, note, v)
		return
	}
	if h.verifyUnblocked(current) {
		statusReason := "auto-review recovery: " + strings.TrimSpace(v.Summary)
		if h.dispatchFromHumanRequired != nil {
			target, err := h.prepareRecoveryDispatch(current, status)
			if err != nil {
				h.logger.Error("human-review.unblocked.prepare-dispatch",
					"task_id", current.ID, "agent_id", agentID, "status", status, "err", err)
				return
			}
			if _, err := h.dispatchFromHumanRequired(current.ID, string(target), statusReason, agentID); err != nil {
				h.logger.Error("human-review.unblocked.dispatch",
					"task_id", current.ID, "agent_id", agentID, "target", target, "err", err)
				return
			}
			if h.appendNote(current.ID, "Auto-review: unblocked", note) {
				h.markVerdictRendered(current.ID, agentID)
			}
			return
		}
		newBody := appendSection(current.Body, "Auto-review: unblocked", note)
		updated, err := h.tasks.Update(current.ID, task.Update{
			Body:         &newBody,
			Status:       task.Ptr(status),
			StatusReason: task.Ptr(statusReason),
		})
		if err != nil {
			h.logger.Error("human-review.unblocked.update", "task_id", current.ID, "agent_id", agentID, "status", status, "err", err)
			return
		}
		if updated.Status != status {
			// The store applies updates under a per-task lock, so this can
			// only mean a status guard elsewhere in the write path silently
			// declined the transition — treat that the same as an update
			// error rather than latch a verdict that never actually moved
			// the task off human-required.
			h.logger.Error("human-review.unblocked.status-mismatch",
				"task_id", current.ID, "agent_id", agentID, "want_status", status, "got_status", updated.Status)
			return
		}
		h.markVerdictRendered(current.ID, agentID)
		return
	}
	if h.appendNote(current.ID, "Auto-review: unblocked claim not verified", note) {
		h.markVerdictRendered(current.ID, agentID)
	}
}

func (h *humanReviewHandler) applyDoneRecovery(current task.Task, agentID, note string, v verdictDecision) {
	prNumber := humanReviewRecoveryPRNumber(current, v)
	mergedPR := false
	if prNumber > 0 {
		if !h.verifyDoneRecoveryMergedPR(current, agentID, note, prNumber) {
			return
		}
		mergedPR = true
	}
	if mergedPR && h.landClosedPR != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := h.landClosedPR(ctx, current.ID, prNumber, "MERGED", agentID); err != nil {
			h.logger.Error("human-review.unblocked.land-merged",
				"task_id", current.ID, "agent_id", agentID, "pr", prNumber, "err", err)
			return
		}
		if err := h.finalizeDoneRecovery(current.ID, prNumber, mergedPR); err != nil {
			h.logger.Error("human-review.unblocked.finalize-done",
				"task_id", current.ID, "agent_id", agentID, "pr", prNumber, "err", err)
			return
		}
		if h.appendNote(current.ID, "Auto-review: unblocked", note) {
			h.markVerdictRendered(current.ID, agentID)
		}
		return
	}

	statusReason := "auto-review recovery: " + strings.TrimSpace(v.Summary)
	if h.dispatchFromHumanRequired != nil {
		target, err := h.prepareRecoveryDispatch(current, task.StatusDone)
		if err != nil {
			h.logger.Error("human-review.unblocked.prepare-dispatch",
				"task_id", current.ID, "agent_id", agentID, "status", task.StatusDone, "err", err)
			return
		}
		if _, err := h.dispatchFromHumanRequired(current.ID, string(target), statusReason, agentID); err != nil {
			h.logger.Error("human-review.unblocked.dispatch",
				"task_id", current.ID, "agent_id", agentID, "target", target, "err", err)
			return
		}
		if err := h.finalizeDoneRecovery(current.ID, prNumber, mergedPR); err != nil {
			h.logger.Error("human-review.unblocked.finalize-done", "task_id", current.ID, "agent_id", agentID, "pr", prNumber, "err", err)
			return
		}
		if h.appendNote(current.ID, "Auto-review: unblocked", note) {
			h.markVerdictRendered(current.ID, agentID)
		}
		return
	}

	newBody := appendSection(current.Body, "Auto-review: unblocked", note)
	update := task.Update{
		Body:         &newBody,
		Status:       task.Ptr(task.StatusDone),
		StatusReason: task.Ptr(""),
	}
	if mergedPR && current.PRNumber == 0 {
		update.PRNumber = task.Ptr(prNumber)
	}
	if mergedPR {
		update.Outcome = task.Ptr("merged")
	}
	updated, err := h.tasks.Update(current.ID, update)
	if err != nil {
		h.logger.Error("human-review.unblocked.update", "task_id", current.ID, "agent_id", agentID, "status", task.StatusDone, "err", err)
		return
	}
	if updated.Status != task.StatusDone {
		h.logger.Error("human-review.unblocked.status-mismatch",
			"task_id", current.ID, "agent_id", agentID, "want_status", task.StatusDone, "got_status", updated.Status)
		return
	}
	h.markVerdictRendered(current.ID, agentID)
}

func (h *humanReviewHandler) verifyDoneRecoveryMergedPR(current task.Task, agentID, note string, prNumber int) bool {
	if current.ProjectID == "" || h.fetchPRState == nil {
		h.logger.Warn("human-review.unblocked.pr-state-unavailable",
			"task_id", current.ID, "agent_id", agentID, "pr", prNumber, "project_id", current.ProjectID)
		if h.appendNote(current.ID, "Auto-review: unblocked claim not verified", note) {
			h.markVerdictRendered(current.ID, agentID)
		}
		return false
	}
	state, err := h.fetchPRState(current.ProjectID, prNumber)
	if err != nil {
		h.logger.Error("human-review.unblocked.pr-state",
			"task_id", current.ID, "agent_id", agentID, "pr", prNumber, "err", err)
		if h.appendNote(current.ID, "Auto-review: unblocked claim not verified", note) {
			h.markVerdictRendered(current.ID, agentID)
		}
		return false
	}
	if state.State != "MERGED" {
		h.logger.Warn("human-review.unblocked.pr-not-merged",
			"task_id", current.ID, "agent_id", agentID, "pr", prNumber, "state", state.State)
		if h.appendNote(current.ID, "Auto-review: unblocked claim not verified", note) {
			h.markVerdictRendered(current.ID, agentID)
		}
		return false
	}
	return true
}

func (h *humanReviewHandler) finalizeDoneRecovery(taskID string, prNumber int, mergedPR bool) error {
	update := task.Update{
		StatusReason: task.Ptr(""),
	}
	if mergedPR {
		update.Outcome = task.Ptr("merged")
	}
	if mergedPR {
		current, err := h.tasks.Get(taskID)
		if err != nil {
			return err
		}
		if current.PRNumber == 0 {
			update.PRNumber = task.Ptr(prNumber)
		}
	}
	_, err := h.tasks.Update(taskID, update)
	return err
}

func humanReviewRecoveryPRNumber(current task.Task, v verdictDecision) int {
	if current.PRNumber > 0 {
		return current.PRNumber
	}
	m := humanReviewPRNumberRE.FindStringSubmatch(v.Summary)
	if len(m) != 2 {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func (h *humanReviewHandler) prepareRecoveryDispatch(current task.Task, status task.Status) (task.Status, error) {
	target := status
	if target == task.StatusReadyPR && current.PRNumber == 0 && workflow.IsTamperFlaggedReason(current.StatusReason) {
		target = task.StatusInProgress
	}
	if target == task.StatusInReview && current.PRNumber == 0 {
		target = task.StatusReadyReview
	}
	if target == task.StatusReadyReview && current.PRNumber != 0 {
		target = task.StatusInReview
	}
	if recoveryNeedsTamperBless(current, target) {
		if err := h.ensureTaskTag(current.ID, workflow.TamperBlessedTag); err != nil {
			return "", err
		}
	}
	return target, nil
}

func (h *humanReviewHandler) recoverRenderedUnblockedTasks() {
	if h == nil || h.tasks == nil || h.dispatchFromHumanRequired == nil {
		return
	}
	tasks, err := h.tasks.List()
	if err != nil {
		h.logger.Warn("human-review.recover-rendered.list", "err", err)
		return
	}
	for i := range tasks {
		t := tasks[i]
		if !missingCachedWorktreeCircuitBreaker(t) {
			continue
		}
		agentID, v, ok := latestHumanReviewUnblockedVerdict(t)
		if !ok {
			continue
		}
		status, ok := safeHumanReviewRecoveryStatus(v.RecoverableAction)
		if !ok {
			continue
		}
		target, prepErr := h.prepareRecoveryDispatch(t, status)
		if prepErr != nil {
			h.logger.Warn("human-review.recover-rendered.prepare",
				"task_id", t.ID, "agent_id", agentID, "target", status, "err", prepErr)
			continue
		}
		reason := "auto-review recovery retry: " + strings.TrimSpace(v.Summary)
		if _, err := h.dispatchFromHumanRequired(t.ID, string(target), reason, agentID); err != nil {
			h.logger.Warn("human-review.recover-rendered.dispatch",
				"task_id", t.ID, "agent_id", agentID, "target", target, "err", err)
			continue
		}
		h.logger.Info("human-review.recover-rendered.dispatched",
			"task_id", t.ID, "agent_id", agentID, "target", target)
	}
}

func missingCachedWorktreeCircuitBreaker(t task.Task) bool {
	if t.Status != task.StatusHumanRequired {
		return false
	}
	reason := strings.ToLower(t.StatusReason)
	return strings.Contains(reason, "circuit breaker: agent start failed") &&
		strings.Contains(reason, "dir ") &&
		strings.Contains(reason, "not accessible") &&
		strings.Contains(reason, "/worktrees/")
}

func latestHumanReviewUnblockedVerdict(t task.Task) (agentID string, v verdictDecision, ok bool) {
	for i := range slices.Backward(t.AgentRuns) {
		run := &t.AgentRuns[i]
		if run.Role != string(agent.RoleHumanReview) || !run.VerdictRendered || strings.TrimSpace(run.Result) == "" {
			continue
		}
		parsed, _, err := verdict.Parse(run.Result)
		if err != nil || parsed.Decision != "unblocked" {
			continue
		}
		return run.AgentID, parsed, true
	}
	return "", verdictDecision{}, false
}

func recoveryNeedsTamperBless(current task.Task, target task.Status) bool {
	if !workflow.IsTamperFlaggedReason(current.StatusReason) {
		return false
	}
	return target == task.StatusInProgress || target == task.StatusReadyReview
}

func (h *humanReviewHandler) ensureTaskTag(taskID, tag string) error {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return nil
	}
	cur, err := h.tasks.Get(taskID)
	if err != nil {
		return err
	}
	if slices.Contains(cur.Tags, tag) {
		return nil
	}
	tags := append(append([]string{}, cur.Tags...), tag)
	_, err = h.tasks.Update(taskID, task.Update{Tags: task.Ptr(tags)})
	return err
}

// verifyUnblocked re-validates an "unblocked" verdict against the task's real
// worktree state instead of trusting the agent's self-report. See #2347: an
// auto-review note once claimed the task had self-unblocked while GitHub and
// task state both still showed human-required against a dirty PR — the
// review layer never checked the agent's claimed fix actually left its local
// checkout. The branch must be clean (no uncommitted edits, no leftover merge
// state) and pushed (local HEAD present on the remote) before the status flip
// is trusted.
//
// A task with no worktree on disk (e.g. it never made it past triage, or the
// worktree was already cleaned up) has nothing to verify against and passes
// through unchanged — the recovery predates this check for those tasks.
func (h *humanReviewHandler) verifyUnblocked(t task.Task) bool {
	dir := strings.TrimSpace(t.WorktreeDir)
	if dir == "" {
		return true
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dirty, err := project.IsWorktreeDirty(ctx, dir)
	if err != nil {
		h.logger.Warn("human-review.unblocked.verify-dirty", "task_id", t.ID, "err", err)
		return false
	}
	if dirty {
		h.logger.Warn("human-review.unblocked.dirty-worktree", "task_id", t.ID)
		return false
	}

	branch, err := project.CurrentBranch(ctx, dir)
	if err != nil || strings.TrimSpace(branch) == "" {
		h.logger.Warn("human-review.unblocked.verify-branch", "task_id", t.ID, "err", err)
		return false
	}

	localHead, err := project.CurrentCommit(ctx, dir)
	if err != nil {
		h.logger.Warn("human-review.unblocked.verify-head", "task_id", t.ID, "err", err)
		return false
	}

	remote := project.PushRemote(ctx, dir)
	remoteHead, err := project.RemoteBranchHead(ctx, dir, remote, branch)
	if err != nil {
		h.logger.Warn("human-review.unblocked.verify-remote", "task_id", t.ID, "branch", branch, "err", err)
		return false
	}
	if remoteHead == "" || remoteHead != localHead {
		h.logger.Warn("human-review.unblocked.unpushed",
			"task_id", t.ID, "branch", branch, "local_head", localHead, "remote_head", remoteHead)
		return false
	}
	return true
}

func safeHumanReviewRecoveryStatus(action string) (task.Status, bool) {
	switch strings.TrimSpace(action) {
	case "todo":
		return task.StatusTodo, true
	case "planning":
		return task.StatusPlanning, true
	case "plan-review":
		return task.StatusPlanReview, true
	case "in-progress":
		return task.StatusInProgress, true
	case "ready-review":
		return task.StatusReadyReview, true
	case "in-review":
		return task.StatusInReview, true
	case "testing":
		return task.StatusTesting, true
	case "ready-pr":
		return task.StatusReadyPR, true
	case "done":
		return task.StatusDone, true
	default:
		return "", false
	}
}

// onComplete is the routing target inside App.onAgentComplete for runs whose
// role is RoleHumanReview. It parses the verdict and applies side-effects.
func (h *humanReviewHandler) onComplete(ag *agent.Agent) {
	if h == nil || ag == nil {
		return
	}
	taskID := ag.TaskID
	defer func() {
		h.mu.Lock()
		// Only clear the slot if it still belongs to this completing agent.
		// A crash-retry dispatched earlier in this same call (see
		// retryAfterCrash) registers a new inflight agent for taskID before
		// this defer runs; blindly deleting here would wipe that fresh
		// registration and reopen the in_flight guard for the retry.
		if h.inflight[taskID] == ag.ID {
			delete(h.inflight, taskID)
		}
		h.mu.Unlock()
	}()

	current, err := h.tasks.Get(taskID)
	if err != nil {
		h.logger.Error("human-review.task.get-on-complete", "task_id", taskID, "agent_id", ag.ID, "err", err)
		return
	}
	final := finalAssistantText(ag)
	v, source, parseErr := verdict.Parse(final)

	if parseErr == nil && v.Decision == "unblocked" {
		h.recordUnblocked(current, ag.ID, v, source)
		return
	}

	if current.Status != task.StatusHumanRequired {
		h.logger.Info("human-review.verdict.stale",
			"task_id", taskID, "agent_id", ag.ID, "status", current.Status)
		if parseErr == nil {
			h.markVerdictRendered(taskID, ag.ID)
		}
		h.logAudit(audit.EventHumanReviewSkipped, taskID, ag.ID, map[string]any{
			"reason": "status_changed",
			"status": string(current.Status),
		})
		return
	}

	if parseErr != nil {
		if ag.GetErrorKind() == "rate_limit" {
			h.logger.Warn("human-review.verdict.deferred",
				"task_id", taskID, "agent_id", ag.ID, "reason", "provider_rate_limited")
			h.logAudit(audit.EventHumanReviewSkipped, taskID, ag.ID, map[string]any{"reason": "provider_rate_limited"})
			return
		}
		// Without the zero-tool-calls check, a run that did real diagnostic
		// work and only errored at the very end would be treated the same as
		// an instant crash and get retried/discarded instead of routing
		// through the ordinary unparseable-verdict path below.
		if ag.HadTerminalError() && ag.GetToolCalls() == 0 {
			h.handleCrashedVerdict(taskID, ag)
			return
		}
		if h.handleStructuredVerdictFailure(current, ag, final, parseErr) {
			return
		}
		h.handleUnparseableVerdict(ag, current, final, parseErr)
		return
	}
	h.logAudit(audit.EventHumanReviewVerdict, taskID, ag.ID, map[string]any{
		"decision": v.Decision, "summary": v.Summary, "verdict_source": string(source),
		"recoverable_action": v.RecoverableAction, "confidence": v.Confidence,
	})

	switch v.Decision {
	case "human":
		if h.appendNote(taskID, "Auto-review verdict: needs human", h.scrubForTask(current.ProjectID, v.Summary)) {
			h.markVerdictRendered(taskID, ag.ID)
		}
	case "sybra_bug":
		h.handleSybraBugVerdict(taskID, ag, v)
	default:
		h.logger.Warn("human-review.verdict.unknown", "task_id", taskID, "decision", v.Decision)
		if h.appendNote(taskID, "Auto-review (unknown decision)", h.scrubForTask(current.ProjectID, final)) {
			h.markVerdictRendered(taskID, ag.ID)
		}
	}
}

// handleUnparseableVerdict applies the completion side-effects for a run whose
// output did not parse into a verdict, after onComplete has already deferred
// the rate_limit and zero-tool-call-crash cases upstream. A run that crashed
// after doing some work (HadTerminalError with tool calls > 0) records an
// exit error and often emits no text at all, so the fall-through path below
// would call appendNote("") — which no-ops and latches nothing, stranding the
// task in human-required with no rendered diagnosis and no re-trigger. Render
// an honest, non-empty crash note here and latch instead: the task correctly
// stays in human-required for a human, now with accurate context. The task is
// known to be in human-required (caller-guarded).
func (h *humanReviewHandler) handleUnparseableVerdict(ag *agent.Agent, current task.Task, final string, parseErr error) {
	taskID := ag.TaskID
	if exitErr := ag.GetExitErr(); exitErr != nil {
		h.logger.Warn("human-review.verdict.crashed",
			"task_id", taskID, "agent_id", ag.ID, "reason", "execution_error", "err", exitErr)
		note := "The auto-review agent crashed before producing a verdict: " + exitErr.Error()
		if h.appendNote(taskID, "Auto-review did not complete (execution error)", h.scrubForTask(current.ProjectID, note)) {
			h.markVerdictRendered(taskID, ag.ID)
		}
		h.logAudit(audit.EventHumanReviewVerdict, taskID, ag.ID, map[string]any{
			"decision": "crashed", "verdict_source": "execution_error", "reason": exitErr.Error(),
		})
		return
	}
	h.logger.Warn("human-review.verdict.parse", "task_id", taskID, "agent_id", ag.ID, "err", parseErr)
	body := strings.TrimSpace(final)
	if body == "" {
		body = "The auto-review agent did not return a usable structured verdict.\n\nError: " + parseErr.Error()
	}
	if h.appendNote(taskID, "Auto-review (unparseable verdict)", h.scrubForTask(current.ProjectID, body)) {
		h.markVerdictRendered(taskID, ag.ID)
	}
	h.logAudit(audit.EventHumanReviewVerdict, taskID, ag.ID, map[string]any{
		"decision": "unparseable", "verdict_source": "unparseable", "reason": parseErr.Error(),
	})
}

// handleCrashedVerdict is onComplete's branch for a review run that produced
// no usable output because it crashed (HadTerminalError). It retries once via
// the existing per-task spawn budget rather than falling through to the
// generic unparseable-verdict path, which would mark verdict_rendered and
// permanently disable further autonomy attempts for a run that never
// actually diagnosed anything.
func (h *humanReviewHandler) handleCrashedVerdict(taskID string, ag *agent.Agent) {
	h.logger.Warn("human-review.verdict.crashed", "task_id", taskID, "agent_id", ag.ID)
	h.logAudit(audit.EventHumanReviewSkipped, taskID, ag.ID, map[string]any{"reason": "crashed_before_output"})
	if h.retryAfterCrash(taskID) {
		return
	}
	h.logger.Warn("human-review.verdict.crashed-exhausted", "task_id", taskID, "agent_id", ag.ID)
	// Deliberately silent on retry count: retryAfterCrash's true/false only
	// says whether maybeSpawn actually dispatched a retry, not how many were
	// attempted, so this note must not assert a specific attempt count.
	if h.appendNote(taskID, "Auto-review (crashed)",
		"The autonomy-mandate agent crashed before producing a diagnosis, and an "+
			"automatic retry could not be dispatched (spawn budget exhausted, or the "+
			"retry attempt itself failed to start). This is a genuine human-required "+
			"case, but note the failure itself has not actually been reviewed — the "+
			"automated recovery never completed a run.") {
		h.markVerdictRendered(taskID, ag.ID)
	}
	h.logAudit(audit.EventHumanReviewVerdict, taskID, ag.ID, map[string]any{
		"decision": "crashed_exhausted", "verdict_source": "unparseable",
	})
}

// handleSybraBugVerdict is onComplete's branch for a "sybra_bug" verdict. It
// re-checks the work context at completion time (rather than reusing the one
// captured before the review agent ran) so a project re-typed work/pet during
// the run still routes to the right sink, then dispatches per
// HumanReviewSybraBugAction.
func (h *humanReviewHandler) handleSybraBugVerdict(taskID string, ag *agent.Agent, v verdictDecision) {
	var wctx *WorkScrubContext
	if h.workCtx != nil {
		if t, err := h.tasks.Get(taskID); err == nil {
			wctx = h.workCtx(t.ProjectID)
		}
	}
	switch h.cfg.HumanReviewSybraBugAction() {
	case config.HumanReviewSybraBugActionNoteOnly:
		if wctx != nil {
			v = scrubVerdict(v, wctx)
		}
		h.noteSybraBugOnly(taskID, ag.ID, v)
	case config.HumanReviewSybraBugActionBlockOnly:
		if wctx != nil {
			v = scrubVerdict(v, wctx)
		}
		h.blockSybraBugOnly(taskID, ag.ID, v)
	case config.HumanReviewSybraBugActionLocalTask:
		if wctx != nil {
			h.fileLocalScrubbed(taskID, ag.ID, v, wctx)
		} else {
			h.fileLocalConfigured(taskID, ag.ID, v)
		}
	default:
		if wctx != nil {
			v = scrubVerdict(v, wctx)
		}
		h.noteSybraBugOnly(taskID, ag.ID, v)
	}
}

// scrubForTask redacts text through the work-project blocklist for
// projectID, if one applies. Used before persisting raw agent output (e.g.
// an unparseable or unrecognized verdict) into a task body, so a leaked
// work-repo identifier in the model's response never lands in a public
// artifact.
func (h *humanReviewHandler) scrubForTask(projectID, text string) string {
	if h.workCtx == nil {
		return text
	}
	wctx := h.workCtx(projectID)
	if wctx == nil {
		return text
	}
	scrubbed, _ := scrub.Scrub(text, wctx.Blocklist)
	return scrubbed
}

func (h *humanReviewHandler) noteSybraBugOnly(taskID, agentID string, v verdictDecision) {
	if h.appendNote(taskID, "Auto-review verdict: Sybra bug (note only)", sybraBugNoteBody(v, "")) {
		h.markVerdictRendered(taskID, agentID)
	}
}

func (h *humanReviewHandler) blockSybraBugOnly(taskID, agentID string, v verdictDecision) {
	t, err := h.tasks.Get(taskID)
	if err != nil {
		h.logger.Error("human-review.block-only.get", "task_id", taskID, "err", err)
		return
	}
	newBody := appendSection(t.Body, "Auto-review verdict: blocked by Sybra bug (issue filing disabled)", sybraBugNoteBody(v, ""))
	upd := task.Update{
		Body:         &newBody,
		Status:       task.Ptr(task.StatusBlocked),
		StatusReason: task.Ptr("auto-review: " + strings.TrimSpace(v.Summary)),
	}
	if _, err := h.tasks.Update(taskID, upd); err != nil {
		h.logger.Error("human-review.block-only.update", "task_id", taskID, "err", err)
		return
	}
	h.markVerdictRendered(taskID, agentID)
}

func (h *humanReviewHandler) fileLocalConfigured(taskID, agentID string, v verdictDecision) {
	if strings.TrimSpace(v.IssueTitle) == "" || strings.TrimSpace(v.IssueBody) == "" {
		h.logger.Warn("human-review.local-configured.empty", "task_id", taskID, "agent_id", agentID)
		if h.appendNote(taskID, "Auto-review verdict: sybra_bug (no issue payload)", v.Summary) {
			h.markVerdictRendered(taskID, agentID)
		}
		return
	}
	if existing, ok := h.findExistingLocalBugTask(v.IssueTitle); ok {
		h.linkExistingLocalBug(taskID, agentID, existing, v)
		return
	}
	body := strings.TrimSpace(v.IssueBody) + "\n\n## Filing route\n\nGitHub issue filing disabled by `human_review.sybra_bug_action: local_task`; Sybra created this local task instead."
	tags := append([]string{"sybra-bug", "local"}, v.IssueLabels...)
	init := task.Update{Tags: &tags}
	if projectID := strings.TrimSpace(h.cfg.HumanReviewRepo()); projectID != "" {
		init.ProjectID = &projectID
	}
	if existing := h.findExistingLocalBugTaskOnRoute(v.IssueTitle, "local"); existing != nil {
		h.logAudit(audit.EventHumanReviewIssue, taskID, agentID, map[string]any{
			"created": false, "url": "", "title": v.IssueTitle, "local_task_id": existing.ID,
		})
		h.blockOriginOnLocalBug(taskID, agentID, "Auto-review verdict: blocked by existing Sybra bug (local task)", v.Summary, existing.ID, "")
		return
	}
	newTask, err := h.tasks.CreateFull(v.IssueTitle, body, task.AgentModeHeadless, init)
	if err != nil {
		h.logger.Error("human-review.local-configured.create", "task_id", taskID, "agent_id", agentID, "err", err)
		if h.appendNote(taskID, "Auto-review verdict: sybra_bug (local task creation failed)", sybraBugNoteBody(v, err.Error())) {
			h.markVerdictRendered(taskID, agentID)
		}
		return
	}
	h.logAudit(audit.EventHumanReviewIssue, taskID, agentID, map[string]any{
		"created": true, "url": "", "title": v.IssueTitle, "local_task_id": newTask.ID,
	})

	h.blockOriginOnLocalBug(taskID, agentID, "Auto-review verdict: blocked by Sybra bug (local task)", v.Summary, newTask.ID, "")
}

func sybraBugNoteBody(v verdictDecision, extra string) string {
	var b strings.Builder
	if summary := strings.TrimSpace(v.Summary); summary != "" {
		b.WriteString(summary)
	}
	if title := strings.TrimSpace(v.IssueTitle); title != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("Suggested issue title: ")
		b.WriteString(title)
	}
	if body := strings.TrimSpace(v.IssueBody); body != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(body)
	}
	if extra = strings.TrimSpace(extra); extra != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("Error: ")
		b.WriteString(extra)
	}
	return b.String()
}

func scrubVerdict(v verdictDecision, wctx *WorkScrubContext) verdictDecision {
	if wctx == nil {
		return v
	}
	v.Summary, _ = scrub.Scrub(v.Summary, wctx.Blocklist)
	v.IssueTitle, _ = scrub.Scrub(v.IssueTitle, wctx.Blocklist)
	v.IssueBody, _ = scrub.Scrub(v.IssueBody, wctx.Blocklist)
	return v
}

// fileLocalScrubbed is the work-project fallback for the sybra_bug verdict.
// Instead of opening a GitHub issue on Automaat/sybra, it scrubs the agent-
// authored title and body through wctx.Blocklist and creates a local sybra
// task tagged sybra-bug,scrubbed. The originating task is flipped to blocked
// with a pointer to the new local task — same UX shape as the public path,
// just routed away from the public repo.
func (h *humanReviewHandler) fileLocalScrubbed(taskID, agentID string, v verdictDecision, wctx *WorkScrubContext) {
	if strings.TrimSpace(v.IssueTitle) == "" || strings.TrimSpace(v.IssueBody) == "" {
		h.logger.Warn("human-review.local.empty", "task_id", taskID, "agent_id", agentID)
		if h.appendNote(taskID, "Auto-review verdict: sybra_bug (no issue payload)", v.Summary) {
			h.markVerdictRendered(taskID, agentID)
		}
		return
	}
	title, titleRed := scrub.Scrub(v.IssueTitle, wctx.Blocklist)
	body, bodyRed := scrub.Scrub(v.IssueBody, wctx.Blocklist)
	summary, _ := scrub.Scrub(v.Summary, wctx.Blocklist)

	if existing, ok := h.findExistingLocalBugTask(title); ok {
		h.linkExistingLocalBug(taskID, agentID, existing, verdictDecision{Summary: summary})
		return
	}

	tags := append([]string{"sybra-bug", "scrubbed"}, v.IssueLabels...)
	init := task.Update{Tags: &tags}
	if projectID := strings.TrimSpace(h.cfg.HumanReviewRepo()); projectID != "" {
		init.ProjectID = &projectID
	}
	if existing := h.findExistingLocalBugTaskOnRoute(title, "scrubbed"); existing != nil {
		h.logAudit(audit.EventHumanReviewIssue, taskID, agentID, map[string]any{
			"created": false, "url": "", "title": title,
			"local_task_id": existing.ID, "redactions_title": titleRed, "redactions_body": bodyRed,
			"scrubbed": true,
		})
		h.blockOriginOnLocalBug(taskID, agentID, "Auto-review verdict: blocked by existing Sybra bug (scrubbed)", summary, existing.ID, "")
		return
	}
	newTask, err := h.tasks.CreateFull(title, body, task.AgentModeHeadless, init)
	if err != nil {
		h.logger.Error("human-review.local.create", "task_id", taskID, "agent_id", agentID, "err", err)
		// The diagnosis note IS the durable artifact here; mark rendered so a
		// restart does not re-spawn the reviewer and duplicate it (the local
		// task creation failing does not undo the appended note).
		if h.appendNote(taskID, "Auto-review verdict: sybra_bug (local task creation failed)", summary+"\n\nError: "+err.Error()) {
			h.markVerdictRendered(taskID, agentID)
		}
		return
	}
	h.logAudit(audit.EventHumanReviewIssue, taskID, agentID, map[string]any{
		"created": true, "url": "", "title": title,
		"local_task_id": newTask.ID, "redactions_title": titleRed, "redactions_body": bodyRed,
		"scrubbed": true,
	})

	h.blockOriginOnLocalBug(taskID, agentID, "Auto-review verdict: blocked by Sybra bug (scrubbed)", summary, newTask.ID, "")
}

func (h *humanReviewHandler) recordUnblocked(current task.Task, agentID string, v verdictDecision, source verdict.Source) {
	h.logAudit(audit.EventHumanReviewVerdict, current.ID, agentID, map[string]any{
		"decision": v.Decision, "summary": v.Summary, "verdict_source": string(source),
		"recoverable_action": v.RecoverableAction, "confidence": v.Confidence,
	})
	h.applyUnblockedRecovery(current, agentID, v)
}

func (h *humanReviewHandler) findExistingLocalBugTaskOnRoute(title, routeTag string) *task.Task {
	title = strings.TrimSpace(title)
	routeTag = strings.TrimSpace(routeTag)
	if title == "" || routeTag == "" {
		return nil
	}
	all, err := h.tasks.List()
	if err != nil {
		h.logger.Warn("human-review.local-dedupe.list", "title", title, "route_tag", routeTag, "err", err)
		return nil
	}
	for i := range all {
		t := &all[i]
		if strings.TrimSpace(t.Title) != title {
			continue
		}
		if slices.Contains(t.Tags, "sybra-bug") && slices.Contains(t.Tags, routeTag) {
			return t
		}
	}
	return nil
}

func (h *humanReviewHandler) blockOriginOnLocalBug(taskID, agentID, header, summary, localTaskID, extra string) bool {
	origin, err := h.tasks.Get(taskID)
	if err != nil {
		h.logger.Error("human-review.local.origin-get", "task_id", taskID, "err", err)
		return false
	}
	noteBody := fmt.Sprintf("**Linked local Sybra bug:** %s\n\n%s", localTaskID, summary)
	if extra = strings.TrimSpace(extra); extra != "" {
		noteBody += "\n\n" + extra
	}
	newBody := appendSection(origin.Body, header, noteBody)
	statusReason := fmt.Sprintf("auto-review: %s (local task %s)", summary, localTaskID)
	if strings.Contains(strings.ToLower(extra), "issue filing failed") {
		statusReason = fmt.Sprintf("auto-review: %s (local task %s; issue filing failed)", summary, localTaskID)
	}
	upd := task.Update{
		Body:         &newBody,
		Status:       task.Ptr(task.StatusBlocked),
		StatusReason: task.Ptr(statusReason),
	}
	if _, err := h.tasks.Update(taskID, upd); err != nil {
		h.logger.Error("human-review.local.origin-update", "task_id", taskID, "err", err)
		return false
	}
	h.markVerdictRendered(taskID, agentID)
	return true
}

// findExistingLocalBugTask returns an already-filed local sybra-bug task with
// an exact title match, if one exists. This dedupes the local_task and
// work-scrubbed routes so a task that cycles human-required -> blocked ->
// todo -> human-required for the same root cause points back at the one
// already filed instead of creating duplicates.
func (h *humanReviewHandler) findExistingLocalBugTask(title string) (task.Task, bool) {
	title = strings.TrimSpace(title)
	if title == "" {
		return task.Task{}, false
	}
	all, err := h.tasks.List()
	if err != nil {
		h.logger.Warn("human-review.local.dedup-list", "err", err)
		return task.Task{}, false
	}
	for i := range all {
		if all[i].Title == title &&
			slices.Contains(all[i].Tags, "sybra-bug") &&
			hasAnyTag(all[i].Tags, "local", "scrubbed", "issue-filing-failed") {
			return all[i], true
		}
	}
	return task.Task{}, false
}

func hasAnyTag(tags []string, needles ...string) bool {
	for _, needle := range needles {
		if slices.Contains(tags, needle) {
			return true
		}
	}
	return false
}

// linkExistingLocalBug re-blocks taskID against an already-filed local
// sybra-bug task instead of creating a duplicate.
func (h *humanReviewHandler) linkExistingLocalBug(taskID, agentID string, existing task.Task, v verdictDecision) {
	h.logAudit(audit.EventHumanReviewIssue, taskID, agentID, map[string]any{
		"created": false, "url": "", "title": existing.Title, "local_task_id": existing.ID, "deduped": true,
	})
	origin, err := h.tasks.Get(taskID)
	if err != nil {
		h.logger.Error("human-review.local.dedup-origin-get", "task_id", taskID, "err", err)
		return
	}
	summary := strings.TrimSpace(v.Summary)
	noteBody := fmt.Sprintf("**Linked local sybra task (already filed):** %s\n\n%s", existing.ID, summary)
	newBody := appendSection(origin.Body, "Auto-review verdict: blocked by Sybra bug (already filed)", noteBody)
	upd := task.Update{
		Body:         &newBody,
		Status:       task.Ptr(task.StatusBlocked),
		StatusReason: task.Ptr(fmt.Sprintf("auto-review: %s (local task %s, already filed)", summary, existing.ID)),
	}
	if _, err := h.tasks.Update(taskID, upd); err != nil {
		h.logger.Error("human-review.local.dedup-origin-update", "task_id", taskID, "err", err)
		return
	}
	h.markVerdictRendered(taskID, agentID)
}

// appendNote appends a section to the task body. Returns true if the update
// succeeded so callers can conditionally mark the verdict as rendered.
func (h *humanReviewHandler) appendNote(taskID, header, body string) bool {
	if strings.TrimSpace(body) == "" {
		return false
	}
	t, err := h.tasks.Get(taskID)
	if err != nil {
		h.logger.Error("human-review.append.task-get", "task_id", taskID, "err", err)
		return false
	}
	newBody := appendSection(t.Body, header, body)
	if _, err := h.tasks.Update(taskID, task.Update{Body: &newBody}); err != nil {
		h.logger.Error("human-review.append.task-update", "task_id", taskID, "err", err)
		return false
	}
	return true
}

// markVerdictRendered sets VerdictRendered on the matching AgentRun, proving
// that onComplete applied all side-effects. verdictAlreadyRendered reads this
// field as the durable rendered-marker.
func (h *humanReviewHandler) markVerdictRendered(taskID, agentID string) {
	if err := h.tasks.UpdateRun(taskID, agentID, task.RunPatch{VerdictRendered: task.Ptr(true)}); err != nil {
		h.logger.Warn("human-review.mark-rendered", "task_id", taskID, "agent_id", agentID, "err", err)
	}
}

func (h *humanReviewHandler) skip(taskID, reason string) {
	h.logger.Info("human-review.skip", "task_id", taskID, "reason", reason)
	h.logAudit(audit.EventHumanReviewSkipped, taskID, "", map[string]any{"reason": reason})
}

// allowSpawnLocked must be called with h.mu held. Trims expired entries from
// h.recent and returns false if the configured rate is exceeded.
func (h *humanReviewHandler) allowSpawnLocked() bool {
	limit := h.cfg.HumanReviewMaxPerHour()
	if limit <= 0 {
		return false
	}
	cutoff := h.now().Add(-humanReviewWindow)
	kept := h.recent[:0]
	for _, ts := range h.recent {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	h.recent = kept
	return len(h.recent) < limit
}

// allowSpawnForTaskLocked must be called with h.mu held. Trims expired
// entries from h.perTask[taskID] and returns false once that single task has
// spawned humanReviewMaxPerTaskPerWindow reviews within the window — the
// per-task counterpart to allowSpawnLocked's fleet-wide budget. Without this,
// a single task whose status keeps oscillating into human-required can spend
// every slot the global window allows, starving every other task's
// diagnosis (see task 90befcef's origin incident: 235 flaps drained the
// fleet's review budget on one task).
func (h *humanReviewHandler) allowSpawnForTaskLocked(taskID string) bool {
	cutoff := h.now().Add(-humanReviewWindow)
	kept := h.perTask[taskID][:0]
	for _, ts := range h.perTask[taskID] {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	if len(kept) == 0 {
		delete(h.perTask, taskID)
	} else {
		h.perTask[taskID] = kept
	}
	return len(kept) < humanReviewMaxPerTaskPerWindow
}

// retryAfterCrash re-spawns the autonomy-mandate agent once for a task whose
// review run crashed before producing any output. It deliberately reuses the
// existing per-task spawn budget (allowSpawnForTaskLocked,
// humanReviewMaxPerTaskPerWindow) as the retry bound instead of adding a
// second counter: the crashed run already consumed one slot, so at most one
// further attempt fits within the window before maybeSpawn's own budget
// checks start rejecting further spawns.
//
// This method has no budget check of its own; maybeSpawn's bool return is
// the only source of truth on whether a retry actually dispatched.
//
// Called from onComplete before its own `defer delete(h.inflight, taskID)`
// runs, so the crashed run's own slot must be freed here first — otherwise
// maybeSpawn's in_flight guard sees the (still-registered) crashed run and
// silently skips the retry.
func (h *humanReviewHandler) retryAfterCrash(taskID string) bool {
	h.mu.Lock()
	delete(h.inflight, taskID)
	h.mu.Unlock()
	return h.maybeSpawn(taskID, "human-required")
}

func (h *humanReviewHandler) logAudit(evt, taskID, agentID string, data map[string]any) {
	if h.audit == nil {
		return
	}
	if err := h.audit.Log(audit.Event{
		Timestamp: h.now(),
		Type:      evt,
		TaskID:    taskID,
		AgentID:   agentID,
		Data:      data,
	}); err != nil {
		h.logger.Warn("human-review.audit.log", "type", evt, "task_id", taskID, "err", err)
	}
}

func (h *humanReviewHandler) clearInflight(taskID string) {
	h.mu.Lock()
	delete(h.inflight, taskID)
	h.mu.Unlock()
}

func removeOneTimestamp(timestamps []time.Time, ts time.Time) []time.Time {
	for i := range slices.Backward(timestamps) {
		if timestamps[i].Equal(ts) {
			return slices.Delete(timestamps, i, i+1)
		}
	}
	return timestamps
}

func (h *humanReviewHandler) releaseReservedSlot(taskID string, reservedAt time.Time) {
	h.mu.Lock()
	delete(h.inflight, taskID)
	h.recent = removeOneTimestamp(h.recent, reservedAt)
	if taskRecent, ok := h.perTask[taskID]; ok {
		taskRecent = removeOneTimestamp(taskRecent, reservedAt)
		if len(taskRecent) == 0 {
			delete(h.perTask, taskID)
		} else {
			h.perTask[taskID] = taskRecent
		}
	}
	h.mu.Unlock()
}

// preRunEligible re-reads the task at the last safe point before Run: the
// status-hook goroutine can sit behind unrelated work long enough for another
// actor to move the task off human-required, and launching a reviewer after
// that only creates a stale run whose completion we later discard.
func (h *humanReviewHandler) preRunEligible(taskID string, reservedAt time.Time, ignoreRendered bool) bool {
	current, err := h.tasks.Get(taskID)
	if err != nil {
		h.releaseReservedSlot(taskID, reservedAt)
		h.logger.Error("human-review.task.reget-before-spawn", "task_id", taskID, "err", err)
		h.logAudit(audit.EventHumanReviewSkipped, taskID, "", map[string]any{"reason": "task_reget_failed", "err": err.Error()})
		return false
	}
	if current.Status != task.StatusHumanRequired {
		h.releaseReservedSlot(taskID, reservedAt)
		h.skip(taskID, "status_"+string(current.Status))
		return false
	}
	if !ignoreRendered && verdictAlreadyRendered(current) {
		h.releaseReservedSlot(taskID, reservedAt)
		h.skip(taskID, "verdict_rendered")
		return false
	}
	if strings.TrimSpace(current.ProjectID) == "" {
		h.releaseReservedSlot(taskID, reservedAt)
		h.skip(taskID, "no_project")
		return false
	}
	return true
}

// buildPrompt assembles the review agent's instructions and context. The
// agent runs in the task's worktree when one exists (so it can fix/verify/
// push the task's code), otherwise in the Sybra source tree; either way it
// has skip-permissions and can freely grep the codebase + read host logs to
// diagnose the transition.
//
// When wctx is non-nil the task originates from a work-typed project; the
// prompt is augmented with explicit redaction rules and the verdict will be
// routed to a local sybra task instead of a public GH issue. The regex
// scrubber is the floor — these instructions are the semantic ceiling.
func writeAutonomyMandate(b *strings.Builder) {
	b.WriteString("You are Sybra's autonomy agent (Sybra is a desktop task orchestrator). A user task just transitioned to status=human-required. Your job is NOT merely to diagnose — it is to get this task PROGRESSING again without a human wherever it is safe to do so, and to make Sybra more autonomous so this class of block never needs a human again. You run with full permissions and have git, gh, and sybra-cli. Work through three phases in order:\n\n")
	b.WriteString("1. ROOT CAUSE — determine exactly why the task landed in human-required: a deterministic check failure (lint/test/build), a workflow misfire, an un-runnable gate (e.g. a manual smoke the harness cannot perform), an ambiguous spec, a missing credential, or an external system the agent cannot reach. Re-run the exact failing command in the task's worktree to confirm; never infer 'flaky/transient/infra' from reasoning alone.\n\n")
	b.WriteString("2. UNBLOCK — do what you safely can to move the task forward:\n")
	b.WriteString("   - Deterministic failures you can fix (lint/test/build): fix them in the task's worktree, re-run the exact check and SEE it pass, commit + push, then return an `unblocked` verdict with the workflow status the host should resume from. Default to re-entering verification/review/testing; do NOT jump straight to `ready-pr` just because one targeted check passed.\n")
	b.WriteString("   - Work is complete but stuck on a gate the harness genuinely cannot run: open or link a PR (`gh pr create` / `sybra-cli update <id> --pr N --status ready-pr`) and move it to review so CI + Copilot + a human reviewer verify it. Never fabricate or fake the verification you could not run.\n")
	b.WriteString("   - If the task is parked on a pending GitHub review draft, pre-flight the draft before submitting anything: determine whether it is COMMENT, REQUEST_CHANGES, or APPROVE. COMMENT / REQUEST_CHANGES drafts may be submitted when that safely unblocks the task. APPROVE drafts must NEVER be auto-submitted: approval authority is human-only. If you cannot prove the draft is COMMENT or REQUEST_CHANGES, do not submit it.\n")
	b.WriteString("   - A Sybra workflow bug: work around it to unblock the task if you safely can, then record the autonomy gap in your verdict (phase 3).\n")
	b.WriteString("   HARD LIMITS: never fabricate results, never force-merge a PR, never push code whose checks you did not run and see pass. Only LEAVE the task at human-required when a human genuinely must decide — scope, creative direction, missing credentials, or an unreachable external system.\n\n")
	b.WriteString("3. AUTONOMY — for anything that needed a human, OR that you had to do by hand here, ask: 'how should Sybra have handled this itself?' If there is a real gap, describe it in the issue_* fields so a follow-up workflow can turn it into a high-quality tracker. Every human-required transition is a bug in Sybra's autonomy until proven otherwise. Do NOT run `gh issue create` yourself.\n\n")
}

func (h *humanReviewHandler) writePromptTaskDetails(b *strings.Builder, t task.Task, dir string) {
	fmt.Fprintf(b, "- ID: %s\n- Title: %s\n- Status: %s\n", t.ID, t.Title, t.Status)
	if t.StatusReason != "" {
		fmt.Fprintf(b, "- Status reason: %s\n", t.StatusReason)
	}
	if t.ProjectID != "" {
		fmt.Fprintf(b, "- Project: %s\n", t.ProjectID)
	}
	if t.PRNumber > 0 {
		fmt.Fprintf(b, "- PR: #%d\n", t.PRNumber)
	}
	if t.WorktreeDir != "" {
		fmt.Fprintf(b, "- Worktree (this is your working directory — fix/verify/push the task's code here): %s\n", t.WorktreeDir)
	}
	if t.Branch != "" {
		fmt.Fprintf(b, "- Branch: %s\n", t.Branch)
	}
	if sybraDir := strings.TrimSpace(h.cfg.HumanReview.SybraRepoDir); sybraDir != "" && sybraDir != dir {
		fmt.Fprintf(b, "- Sybra's own source tree (cd here to grep/read internal/ when diagnosing a Sybra bug, then cd back to fix the task): %s\n", sybraDir)
	}
}

func (h *humanReviewHandler) buildPrompt(t task.Task, dir string, wctx *WorkScrubContext) string {
	var b strings.Builder
	b.WriteString("# Sybra auto-review of human-required transition\n\n")
	if wctx != nil {
		b.WriteString("## Work-Data Confidentiality (CRITICAL)\n")
		b.WriteString("This task originated from a work-typed project. Your verdict will be persisted to a LOCAL sybra task, not a public GitHub issue, but you must still scrub work identifiers from your output:\n\n")
		b.WriteString("- NEVER include the project_id, repo URL, owner, or repo name.\n")
		b.WriteString("- NEVER quote code, branch names, commit SHAs, or ticket IDs from the task body.\n")
		b.WriteString("- NEVER include author emails, customer names, or internal hostnames.\n")
		b.WriteString("- DO describe the sybra bug abstractly: which workflow step misfired, which sybra subsystem is implicated, what state was inconsistent.\n\n")
	}
	writeAutonomyMandate(&b)
	b.WriteString("## Task\n")
	h.writePromptTaskDetails(&b, t, dir)
	b.WriteString("\n### Task body\n")
	b.WriteString(strings.TrimSpace(t.Body))
	b.WriteString("\n\n")

	b.WriteString("## Recent agent runs\n")
	runs := t.AgentRuns
	if len(runs) > humanReviewMaxAgentRuns {
		runs = runs[len(runs)-humanReviewMaxAgentRuns:]
	}
	if len(runs) == 0 {
		b.WriteString("(none)\n")
	} else {
		for i := range runs {
			r := runs[i]
			fmt.Fprintf(&b, "\n### Run %d — role=%s mode=%s provider=%s state=%s started=%s cost=%.4f session=%s\n",
				i+1, defaultStr(r.Role, "implementation"), r.Mode, defaultStr(r.Provider, "default"),
				r.State, r.StartedAt.Format(time.RFC3339), r.CostUSD, defaultStr(r.SessionID, "(empty)"))
			if r.ProtocolViolation != "" || r.TestOutcome != "" || r.TestFailureFingerprint != "" {
				fmt.Fprintf(&b, "Deterministic test metadata: protocol_violation=%s test_outcome=%s fingerprint=%s\n",
					defaultStr(r.ProtocolViolation, "(none)"), defaultStr(r.TestOutcome, "(none)"), defaultStr(r.TestFailureFingerprint, "(none)"))
			}
			if r.Result != "" {
				res := r.Result
				if len(res) > humanReviewMaxAgentResult {
					res = res[:humanReviewMaxAgentResult] + "\n... (truncated)"
				}
				b.WriteString("Result:\n```\n")
				b.WriteString(strings.TrimSpace(res))
				b.WriteString("\n```\n")
			}
			if r.LogFile != "" {
				fmt.Fprintf(&b, "Log file (read with Read tool, last %d turns are most useful): %s\n", humanReviewMaxAgentTurns, r.LogFile)
			}
		}
	}
	b.WriteString("\n")

	if t.Workflow != nil {
		b.WriteString("## Workflow execution\n")
		fmt.Fprintf(&b, "- Workflow ID: %s\n- Started: %s\n- Current step: %s\n",
			t.Workflow.WorkflowID, t.Workflow.StartedAt.Format(time.RFC3339), t.Workflow.CurrentStep)
		b.WriteString("\n")
	}

	if tail := tailFile(h.logFile, humanReviewLogTail); tail != "" {
		b.WriteString("## Sybra host log (tail)\n```\n")
		b.WriteString(tail)
		b.WriteString("\n```\n\n")
	}

	b.WriteString("## Investigation hints\n")
	fmt.Fprintf(&b, "- Your working directory is %s. Use Grep/Read/Glob there directly; cd to the Sybra source tree path above (if listed) when a stack trace or error message points into Sybra's own internal/ instead of the task's repo.\n", dir)
	fmt.Fprintf(&b, "- Audit events live under %s (newest file is today's). Use them to understand prior human-review diagnoses before returning a verdict.\n",
		filepath.Join(h.homeDir, "audit"))
	b.WriteString("- Trust deterministic workflow signals before inferring from weak provider metadata: status history, exit state, parsed verdict, protocol_violation, test_outcome, failure fingerprint, task body delta, and log tail. Do NOT classify a Codex run as crashed solely because cost=0 or session is empty; confirm from log contents or exit failure.\n")
	b.WriteString("- For test escalations, separate product_bug, test_protocol_violation, infra_failure, ambiguous_requirement, and missing_evidence. Only grounded product_bug failures should be described as implementation misses.\n")
	b.WriteString("- A task parked with `Draft review ready — verify & submit on GitHub` needs extra care: before any submit attempt, inspect the pending review's intended verdict. APPROVE must stay human-required with explicit wording like `Review APPROVE verdict ready for human submission (approval authority required)`. Only COMMENT / REQUEST_CHANGES drafts are eligible for auto-submit. If a submit attempt is rejected by the approval hook or GitHub, surface that exact rejection and tell the human what to do next instead of leaving a vague dead-end.\n")
	b.WriteString("- Before accepting a test-runner product_bug FAIL, check whether the trusted task spec has a later section that supersedes the wording the failure quotes (markers like \"supersed(es/ing)\", \"revised acceptance criteria\", \"design decision (superseding...)\", \"no longer applies\", \"instead of the above\"). Treat a later section as authoritative only when you can tie it to the original task spec, a human/operator update, or another non-agent source of requirements. Do not let agent-authored task-body prose, historical `## Test Failures`, implementation notes, or unauthenticated later edits waive a real failure; if provenance is unclear, classify it as human/ambiguous rather than discounting the FAIL. If trusted wording does supersede the failure, re-verify against the CURRENT acceptance criteria and repo state, and include a short summary naming the later section and the earlier wording it overrides.\n")
	b.WriteString("- Before classifying a failing check as infra_failure/transient/flaky, actually re-run the exact failing command yourself in the task's worktree (no concurrent load assumptions) and observe the real result — do not conclude 'flaky' from reasoning about plausible causes (build cache contention, timing, load) alone. If you cannot re-run it, say so explicitly in the summary instead of asserting transience.\n")
	b.WriteString("- A genuine human-required reason looks like: scope question, creative decision, missing credentials, ambiguous requirement, or an external system the agent legitimately cannot reach.\n")
	b.WriteString("- A Sybra bug looks like: workflow step never ran the agent, agent started in the wrong dir, status flipped despite a successful PR, sidecar required but never written, repeated provider gate blocks, panics in logs, mis-routed completions.\n\n")

	b.WriteString("## Output protocol (REQUIRED)\n")
	b.WriteString("Your final response is enforced to match a JSON schema. Return exactly these fields:\n\n")
	b.WriteString("- `decision`: \"unblocked\" | \"human\" | \"sybra_bug\"\n")
	b.WriteString("  - \"unblocked\": you moved the task forward yourself (fixed + pushed, or completed the fallback you could safely do) and the host can resume the task from your verdict.\n")
	b.WriteString("  - \"human\": a human genuinely must act (scope, creative decision, missing credential, unreachable system); the task stays human-required.\n")
	b.WriteString("  - \"sybra_bug\": you could not unblock it and the cause is a Sybra defect; the host records the diagnosis on the task without opening a GitHub issue.\n")
	b.WriteString("- `reason`: one sentence — what you did to unblock it, what a human must decide, or the diagnosis.\n")
	b.WriteString("- `recoverable_action`: one of `none`, `todo`, `planning`, `plan-review`, `in-progress`, `ready-review`, `in-review`, `testing`, `ready-pr`, `done`.\n")
	b.WriteString("  - Use `none` when you already moved the task yourself or when no safe host-side status change applies.\n")
	b.WriteString("  - For `unblocked`, set this to the target workflow status when the host should move the task back into the pipeline for you. Prefer the first safe downstream stage (`in-progress`/`testing`/`in-review`) over `ready-pr`; use `ready-pr` only when deterministic gates are genuinely unavailable and a PR fallback is required.\n")
	b.WriteString("- `confidence`: `low` | `medium` | `high`.\n")
	b.WriteString("- `issue_title` / `issue_body` / `issue_labels`: optional autonomy-gap tracker payload. Fill these only when there is a real Sybra gap worth tracking; otherwise set them to null. For sybra_bug, these fields are useful context but do not cause the host to open a GitHub issue. `issue_title` should be conventional-commit format (e.g. fix(workflow): ...); `issue_body` = \"## What happened\\n...\\n\\n## Repro\\n...\\n\\n## Suspected cause\\n...\\n\\n## Autonomy fix\\n...\".\n\n")
	b.WriteString("Set unused issue_* fields to null (the schema requires the keys to be present).\n")
	return b.String()
}

// finalAssistantText walks the assistant turns backward and returns the
// first one that actually decodes via verdict.Parse — this avoids selecting
// an earlier turn that merely echoes the schema or discusses "the decision"
// in prose that happens to parse as JSON. If no turn parses (e.g. the run
// produced no valid verdict at all), it falls back to the last turn that at
// least looks verdict-shaped, then the last result turn, purely so callers
// have raw text to surface for diagnostics.
func finalAssistantText(ag *agent.Agent) string {
	out := ag.Output()
	for i := range slices.Backward(out) {
		if out[i].Type != "assistant" {
			continue
		}
		if _, _, err := verdict.Parse(out[i].Content); err == nil {
			return out[i].Content
		}
	}
	for i := range slices.Backward(out) {
		if out[i].Type == "assistant" && (strings.Contains(out[i].Content, "sybra-verdict") || strings.Contains(out[i].Content, "\"decision\"")) {
			return out[i].Content
		}
	}
	for i := range slices.Backward(out) {
		if out[i].Type == "result" {
			return out[i].Content
		}
	}
	return ""
}

// appendSection appends a `## header` section to body, separated by a blank
// line. Keeps the source body intact even if it already ends without newline.
func appendSection(body, header, content string) string {
	body = strings.TrimRight(body, "\n")
	if body != "" {
		body += "\n\n"
	}
	body += "## " + header + "\n\n" + strings.TrimSpace(content) + "\n"
	return body
}

func defaultStr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// verdictAlreadyRendered reports whether any completed human-review agent run
// has VerdictRendered set, proving onComplete ran to completion and applied all
// side-effects (note appended, issue filed, local task created). Verdict alone
// is not sufficient — it is persisted before onComplete runs, so if the process
// crashed between persisting the verdict and rendering the diagnosis, we allow a
// re-spawn so the task is not permanently stranded in human-required.
func verdictAlreadyRendered(t task.Task) bool {
	for i := range t.AgentRuns {
		if t.AgentRuns[i].Role != string(agent.RoleHumanReview) || !t.AgentRuns[i].VerdictRendered {
			continue
		}
		if t.TestingCycleStartedAt == nil || !t.AgentRuns[i].StartedAt.Before(*t.TestingCycleStartedAt) {
			return true
		}
	}
	return false
}

// tailFile reads the last n lines of path. Best-effort: returns "" on error
// or when the file is missing (server containers often don't ship the log).
func tailFile(path string, n int) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
