// Package completion reacts to agent.Manager and workflow.Engine
// terminal-state callbacks: persists the result, records stats, advances
// workflow, triggers worktree/sandbox cleanup, and routes fix-review/human-
// review completions. Extracted from internal/sybra (agent_completion.go)
// per the "Extracting a Concern" convention in CLAUDE.md.
package completion

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/limits"
	"github.com/Automaat/sybra/internal/loopagent"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/sandbox"
	"github.com/Automaat/sybra/internal/stats"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/verdict"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

// maxResultLen bounds how much of the agent's last result message we
// persist into the task file. Larger values bloat task files (and the
// cross-process diff stream); smaller values truncate enough context
// that humans can't read past failures from the UI.
const maxResultLen = 2000

// Config carries every dependency Handler needs. Only Logger and Tasks are
// load-bearing; every other field is nil-safe so tests and degraded init
// (a subsystem that failed to start) can leave it unset.
type Config struct {
	Logger *slog.Logger
	Audit  *audit.Logger
	Emit   func(string, any)

	Tasks          *task.Manager
	Worktrees      *worktree.Manager
	Sandboxes      *sandbox.Manager
	WorkflowEngine *workflow.Engine
	Stats          *stats.Store
	Limits         *limits.Store
	LoopSched      *loopagent.Scheduler
	PRTracker      *github.IssueTracker
	Cfg            *config.Config
	Artifacts      *artifact.Store
	WorkScrub      func(projectID string) *WorkScrubContext

	// HumanReviewComplete routes a completed human-review agent's verdict
	// back into task state. The caller (internal/sybra) wires this to its
	// own humanReviewHandler.onComplete method value — passing the concrete
	// type here would create an import cycle, since internal/sybra
	// constructs this Handler. Nil-safe: Handler checks before calling.
	HumanReviewComplete func(*agent.Agent)

	// ConflictRecovery dispatches the autonomous conflict-fix agent for a
	// diverged fix-review push instead of Sybra's own process force-pushing.
	// May be nil (degraded init / tests): treated as "no recovery available".
	ConflictRecovery func(taskID string) bool
}

// WorkScrubContext carries the subset of work-task scrub state the completion
// package needs when importing local evidence artifacts.
type WorkScrubContext struct {
	Blocklist []string
}

// Handler reacts to agent.Manager and workflow.Engine terminal-state
// callbacks. All optional dependencies are nil-safe — the test harness
// wires only the deps the scenario exercises.
type Handler struct {
	logger *slog.Logger
	audit  *audit.Logger
	emit   func(string, any)

	tasks          *task.Manager
	worktrees      *worktree.Manager
	sandboxes      *sandbox.Manager
	workflowEngine *workflow.Engine
	stats          *stats.Store
	limits         *limits.Store
	loopSched      *loopagent.Scheduler
	prTracker      *github.IssueTracker
	cfg            *config.Config
	// artifacts is the local per-task artifact store. Used to import a
	// completed test-runner's Playwright MCP evidence (screenshots/console
	// logs) before terminal worktree cleanup. Nil-safe: importTestRunnerEvidence
	// no-ops when unset (degraded init / tests).
	artifacts *artifact.Store
	// workScrub resolves a project ID to a WorkScrubContext (App.workScrubContextForTask).
	// Used by importTestRunnerEvidence to redact work-repo identifiers from
	// captured evidence before it lands in the local artifact store. Nil-safe:
	// a nil func or nil-returning lookup means "not work-typed — import as-is".
	workScrub func(projectID string) *WorkScrubContext

	humanReviewComplete func(*agent.Agent)
	conflictRecovery    func(taskID string) bool
}

// New constructs a Handler from cfg.
func New(cfg Config) *Handler {
	return &Handler{
		logger:              cfg.Logger,
		audit:               cfg.Audit,
		emit:                cfg.Emit,
		tasks:               cfg.Tasks,
		worktrees:           cfg.Worktrees,
		sandboxes:           cfg.Sandboxes,
		workflowEngine:      cfg.WorkflowEngine,
		stats:               cfg.Stats,
		limits:              cfg.Limits,
		loopSched:           cfg.LoopSched,
		prTracker:           cfg.PRTracker,
		cfg:                 cfg.Cfg,
		artifacts:           cfg.Artifacts,
		workScrub:           cfg.WorkScrub,
		humanReviewComplete: cfg.HumanReviewComplete,
		conflictRecovery:    cfg.ConflictRecovery,
	}
}

func (h *Handler) logAudit(eventType, taskID, agentID string, data map[string]any) {
	audit.LogEvent(h.audit, h.logger, eventType, taskID, agentID, data)
}

// OnComplete is called by the manager's construction-time completion callback.
// once per terminal agent state transition.
// runDurationSeconds measures against the last observed stream activity,
// not finalize time: a process that emits its terminal result but lingers
// before the agent record is finalized (reattach, stop, app restart) would
// otherwise have that idle gap counted as run time — real runs have shown
// multi-hour "durations" for work that took minutes.
func runDurationSeconds(ag *agent.Agent) float64 {
	return max(ag.GetLastEventAt().Sub(ag.StartedAt).Seconds(), 0)
}

func (h *Handler) OnComplete(ag *agent.Agent) {
	resultContent := terminalResultContent(ag)

	// Snapshot mutable fields once under the agent's lock so both the
	// persistence write and the audit entry see a consistent view.
	state := ag.GetState()
	cost := ag.GetCostUSD()
	premiumRequests := ag.GetPremiumRequests()
	cost = estimatedRunCost(ag, cost, premiumRequests)
	exitErr := ag.GetExitErr()
	reasoning := ag.GetReasoningTokens()

	// Audit logging always fires — orchestrator brain agents have no
	// parent task and skip the storage paths below, but their lifecycle
	// still belongs in the audit trail.
	duration := runDurationSeconds(ag)
	role := h.roleForAgentName(ag.Name)
	auditData := map[string]any{
		"mode":       ag.Mode,
		"cost_usd":   cost,
		"duration_s": duration,
		"state":      string(state),
		"role":       role,
		"provider":   ag.Provider,
		"name":       ag.Name,
		"log_file":   ag.LogPath,
	}
	if reasoning > 0 {
		auditData["reasoning_tokens"] = reasoning
	}
	if premiumRequests > 0 {
		auditData["premium_requests"] = premiumRequests
	}
	h.logAudit(audit.EventAgentCompleted, ag.TaskID, ag.ID, auditData)

	h.recordRunStats(ag, role, cost, duration, exitErr, resultContent)

	// Loop agents run without a TaskID — let the scheduler record cost
	// before the early return below kicks in.
	if h.loopSched != nil {
		h.loopSched.OnAgentComplete(ag)
	}

	// Orchestrator brain agents run with TaskID="" (rooted at ~/.sybra,
	// no parent task). Calling UpdateRun / HandleAgentComplete / Get with
	// an empty ID joins to ".sybra/tasks/.md" and crashes the handler.
	if ag.TaskID == "" {
		return
	}

	h.emitPermissionDenialAudits(ag)

	runUpdates := h.buildRunPatch(ag, state, cost, premiumRequests, resultContent, exitErr)
	if err := h.tasks.UpdateRun(ag.TaskID, ag.ID, runUpdates); err != nil {
		h.logger.Error("task.update-run", "task_id", ag.TaskID, "agent_id", ag.ID, "err", err)
	}
	h.markCompletedReview(ag, exitErr)

	// Human-review agents are out-of-band diagnostics — they must not
	// feed into the workflow engine (which would advance the step that
	// originally caused the human-required transition based on the
	// diagnostic verdict).
	if agent.RoleFromName(ag.Name) == agent.RoleHumanReview {
		if h.humanReviewComplete != nil {
			h.humanReviewComplete(ag)
		}
		return
	}

	if agent.RoleFromName(ag.Name) == agent.RoleFixReview && exitErr == nil {
		h.handleFixReviewCompletion(ag)
	}

	if !h.notifyWorkflowEngine(ag, resultContent, exitErr) {
		return
	}

	if agent.RoleFromName(ag.Name) == agent.RoleTestRunner {
		h.importEvidenceForAgent(ag)
	}

	// Worktree and sandbox cleanup for terminal tasks (after engine
	// advances, so status is final).
	if t, err := h.tasks.Get(ag.TaskID); err == nil && task.IsTerminalStatus(t.Status) {
		// context.Background(): OnComplete implements agent.Manager's
		// onComplete callback, a fixed func(*Agent) signature with no ctx.
		go h.worktrees.Remove(context.Background(), ag.TaskID)
		if h.sandboxes != nil {
			go h.sandboxes.Stop(ag.TaskID)
		}
	}
}

func terminalResultContent(ag *agent.Agent) string {
	var resultContent string
	var hasResultEvent bool
	outputs := ag.Output()
	for i := range outputs {
		if outputs[i].Type == "result" {
			resultContent = outputs[i].Content
			hasResultEvent = true
		}
	}
	// Codex's terminal turn.completed event carries no text — the final message
	// arrives as an assistant StreamEvent. When a result event was seen but
	// carried empty content, fall back to the last assistant turn so that
	// result-dependent paths (extractTestVerdict, PR-URL scraping) work for
	// codex too. Guard with hasResultEvent so agents that exit-0 without
	// emitting any result event (no_result scenario) are NOT back-filled.
	if hasResultEvent && resultContent == "" {
		return lastAssistantText(ag)
	}
	return resultContent
}

// emitPermissionDenialAudits emits one agent.permission_denied audit event per
// auto-mode denial recorded during the run. Batched at completion time so the
// audit log is not spammed mid-run; a killed run may drop its denial events —
// the permission_posture on agent.started is the durable observability signal.
func (h *Handler) emitPermissionDenialAudits(ag *agent.Agent) {
	posture := ag.GetHeadlessPermissionMode()
	for _, d := range ag.GetPermissionDenials() {
		toolID := d.ToolUseID
		if toolID == "" {
			toolID = "unknown"
		}
		h.logAudit(audit.EventAgentPermissionDenied, ag.TaskID, ag.ID, map[string]any{
			"tool":    toolID,
			"reason":  d.Reason,
			"posture": posture,
		})
	}
}

func addRunMetadata(updates *task.RunPatch, ag *agent.Agent) {
	if ag.ExperimentID != "" {
		updates.ExperimentID = task.Ptr(ag.ExperimentID)
		updates.VariantID = task.Ptr(ag.VariantID)
		updates.AssignmentUnit = task.Ptr(ag.AssignmentUnit)
		updates.AssignmentKey = task.Ptr(ag.AssignmentKey)
	}
	if ag.ReasoningEffort != "" {
		updates.ReasoningEffort = task.Ptr(ag.ReasoningEffort)
	}
}

func (h *Handler) buildRunPatch(ag *agent.Agent, state agent.State, cost, premiumRequests float64, resultContent string, exitErr error) task.RunPatch {
	truncated := resultContent
	if len(truncated) > maxResultLen {
		truncated = truncated[:maxResultLen] + "\n... (truncated)"
	}
	runUpdates := task.RunPatch{
		State:           task.Ptr(string(state)),
		CostUSD:         task.Ptr(cost),
		PremiumRequests: task.Ptr(premiumRequests),
		Result:          task.Ptr(truncated),
		LogFile:         task.Ptr(ag.LogPath),
		SessionID:       task.Ptr(ag.GetSessionID()),
		Model:           task.Ptr(ag.Model),
		Provider:        task.Ptr(ag.Provider),
	}
	if outcome := runTerminalOutcome(ag, exitErr); outcome != "" {
		runUpdates.Outcome = task.Ptr(outcome)
	}
	addRunMetadata(&runUpdates, ag)
	// For human-review agents, parse the verdict from the live (untruncated)
	// output and persist it in its own field so detector.go can read it even
	// when the full result text is longer than maxResultLen.
	if agent.RoleFromName(ag.Name) == agent.RoleHumanReview {
		if v, _, err := verdict.Parse(finalAssistantText(ag)); err == nil {
			runUpdates.Verdict = task.Ptr(v.Decision)
		}
	}
	// Capture the worktree HEAD while it still exists (cleanup below is async) so
	// landing can later detect human edits after the agent (merged_with_edits).
	if sha := h.captureHeadSHA(ag.TaskID); sha != "" {
		runUpdates.HeadSHA = task.Ptr(sha)
	}
	return runUpdates
}

// notifyWorkflowEngine advances the workflow engine for a completed agent.
// Returns false if the caller should return immediately. Signal kills and
// Sybra-initiated stops stall for recovery; rate limits immediately re-drive
// the same step so provider failover can choose a healthy peer. Auth failures
// fall through (need human login) — they take the normal failed path.
//
// Load-bearing: PR #722's SIGINT-first path lets default Go binaries (e.g.
// fake-claude in tests) exit with code 2 (NOT WaitStatus.Signaled), so
// isSignalKill alone misses Sybra-initiated stops — the failure mode from #641.
// Rate limits reschedule rather than marking failed to avoid stranding tasks in
// human-required while a healthy peer is available.
//
// WasStopped() is deliberately overridden by WasCompletedByResult(): the
// watchdog's StopCompletedAgent (and the runner's own post-result-hang
// reaper) both mark an agent stopped in order to reap its now-orphaned
// process, but its work already finished cleanly via a terminal result event
// — treating it as a stall would silently re-queue already-completed work
// instead of finalizing it.
func (h *Handler) notifyWorkflowEngine(ag *agent.Agent, resultContent string, exitErr error) bool {
	if h.workflowEngine == nil {
		return true
	}
	stalled, rateLimited, stopStalled := classifyStall(ag, exitErr)
	if stalled {
		h.logger.Warn("agent.completion.stall",
			"task_id", ag.TaskID, "agent_id", ag.ID,
			"signaled", isSignalKill(exitErr), "stopped", stopStalled, "rate_limited", rateLimited)
		if rateLimited {
			h.workflowEngine.RescheduleRateLimitedAgent(ag.TaskID, ag.ID)
		} else {
			h.workflowEngine.ClearAgentStep(ag.ID)
		}
		return false
	}
	h.workflowEngine.HandleAgentComplete(ag.TaskID, workflow.AgentCompletion{
		AgentID:  ag.ID,
		Result:   resultContent,
		Provider: ag.Provider,
		Success:  exitErr == nil,
	})
	return true
}

// classifyStall reports whether a completion is an infra stall — signal kill,
// stop-before-result, or provider rate limit — rather than the agent's actual
// terminal outcome. buildRunPatch and notifyWorkflowEngine both key off this
// so the persisted AgentRun.Outcome and the workflow's Success signal can
// never diverge: a stalled run is retried, so it must be neither a persisted
// success nor a persisted failure.
func classifyStall(ag *agent.Agent, exitErr error) (stalled, rateLimited, stopStalled bool) {
	rateLimited = isRateLimitedRun(ag, exitErr)
	// Cost guardrails intentionally hard-stop the subprocess, but they are a
	// budget failure, not an infra stall. Let them flow through the bounded
	// failed-completion path instead of ClearAgentStep/ResumeStalled.
	costStopped := ag.WasStopped() && ag.GetEscalationReason() == "cost"
	stopStalled = ag.WasStopped() && !ag.WasCompletedByResult() && !costStopped
	stalled = isSignalKill(exitErr) || stopStalled || rateLimited
	return stalled, rateLimited, stopStalled
}

// runTerminalOutcome derives the AgentRun.Outcome value for a completed run:
// empty for a stall (see classifyStall — retried, not a definitive result),
// otherwise success/failure keyed off the same exitErr notifyWorkflowEngine
// uses for AgentCompletion.Success.
func runTerminalOutcome(ag *agent.Agent, exitErr error) string {
	if stalled, _, _ := classifyStall(ag, exitErr); stalled {
		return ""
	}
	if exitErr == nil {
		return task.RunOutcomeSuccess
	}
	return task.RunOutcomeFailure
}

func (h *Handler) markCompletedReview(ag *agent.Agent, exitErr error) {
	if agent.RoleFromName(ag.Name) != agent.RoleReview || exitErr != nil {
		return
	}
	reviewed := true
	if _, err := h.tasks.Update(ag.TaskID, task.Update{Reviewed: &reviewed}); err != nil {
		h.logger.Warn("task.mark-reviewed", "task_id", ag.TaskID, "agent_id", ag.ID, "err", err)
	}
}

// handleFixReviewCompletion routes a finished manual fix-review agent. Without
// the hold it auto-pushes the branch as before. Under review-hold the agent
// drafted its replies into a pending review; Sybra still runs the deterministic
// push backstop in `push` mode so a fix isn't stranded when the agent forgot to
// push, then parks the task for a human. In `push_nits`/`hold` the push is
// conditional/none and owned by the agent per its prompt (a blind backstop would
// push a non-nit diff or break the "hold everything" contract), so Sybra parks
// without forcing one.
func (h *Handler) handleFixReviewCompletion(ag *agent.Agent) {
	if !h.cfg.ReviewHoldEnabled() {
		h.pushFixReviewBranch(ag)
		return
	}
	if h.cfg.ReviewHoldMode() == config.ReviewHoldModePush {
		h.pushFixReviewBranch(ag)
	}
	h.holdFixReviewForHuman(ag)
}

// holdFixReviewForHuman parks a completed manual fix-review task in
// human-required. Under review-hold the agent has drafted its replies into a
// pending review (never posted live), so the human verifies the pending review
// and any local diff, then submits on GitHub. Logs the mode and branch so a
// stranded fix is diagnosable — whether a push was expected (push vs hold) and
// which branch may hold unpushed commits.
func (h *Handler) holdFixReviewForHuman(ag *agent.Agent) {
	if h.tasks == nil {
		return
	}
	const reason = "review-hold: replies drafted as a pending review — verify & submit on GitHub"
	t, err := h.tasks.Update(ag.TaskID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr(reason),
	})
	if err != nil {
		h.logger.Error("fix-review.hold.human-required", "task_id", ag.TaskID, "agent_id", ag.ID, "err", err)
		return
	}
	h.logger.Info("fix-review.hold",
		"task_id", ag.TaskID, "agent_id", ag.ID,
		"mode", h.cfg.ReviewHoldMode(), "branch", t.Branch)
}

func (h *Handler) pushFixReviewBranch(ag *agent.Agent) {
	if h.worktrees == nil || h.tasks == nil {
		return
	}

	t, err := h.tasks.Get(ag.TaskID)
	if err != nil {
		h.logger.Warn("fix-review.push.task", "task_id", ag.TaskID, "agent_id", ag.ID, "err", err)
		return
	}
	if t.ProjectID == "" {
		return
	}

	wtPath := h.worktrees.PathFor(t)
	if _, err := os.Stat(wtPath); err != nil {
		h.logger.Warn("fix-review.push.worktree", "task_id", ag.TaskID, "agent_id", ag.ID, "path", wtPath, "err", err)
		return
	}

	branch := t.Branch
	if branch == "" {
		// context.Background(): pushFixReviewBranch runs off agent.Manager's
		// onComplete callback, a fixed func(*Agent) signature with no ctx.
		branch, err = project.CurrentBranch(context.Background(), wtPath)
		if err != nil {
			h.logger.Warn("fix-review.push.branch", "task_id", ag.TaskID, "agent_id", ag.ID, "path", wtPath, "err", err)
			return
		}
	}

	if err := project.PushSync(context.Background(), wtPath, branch); err != nil {
		if errors.Is(err, project.ErrBranchMissing) {
			h.logger.Info("fix-review.push-skipped", "task_id", ag.TaskID, "agent_id", ag.ID, "branch", branch, "reason", "local branch ref missing")
			return
		}
		if errors.Is(err, project.ErrDivergedNeedsResolve) {
			h.recoverDivergedFixReviewPush(ag, branch, err)
			return
		}
		h.logger.Warn("fix-review.push", "task_id", ag.TaskID, "agent_id", ag.ID, "branch", branch, "err", err)
		return
	}
	h.logger.Info("fix-review.pushed", "task_id", ag.TaskID, "agent_id", ag.ID, "branch", branch)
}

// recoverDivergedFixReviewPush handles a fix-review push backstop that hit
// project.ErrDivergedNeedsResolve: Sybra's own process must never force-push
// (see project.PushSync's doc), so the fix sitting in the worktree is handed
// to the autonomous conflict-fix agent instead — it merges the branch cleanly
// and pushes as its own tool call, which unlike this Go process's shell-out
// is interceptable by hooks. Escalates to human-required when no recovery
// callback is wired or it declines (e.g. retry budget spent), so the fix is
// never silently stranded on local disk.
func (h *Handler) recoverDivergedFixReviewPush(ag *agent.Agent, branch string, pushErr error) {
	if h.conflictRecovery != nil && h.conflictRecovery(ag.TaskID) {
		h.logger.Info("fix-review.push-diverged.recovered", "task_id", ag.TaskID, "agent_id", ag.ID, "branch", branch)
		return
	}
	h.logger.Warn("fix-review.push-diverged", "task_id", ag.TaskID, "agent_id", ag.ID, "branch", branch, "err", pushErr)
	reason := "fix-review: branch diverged from remote and needs manual rebase/merge before the fix can be pushed"
	if _, err := h.tasks.Update(ag.TaskID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr(reason),
	}); err != nil {
		h.logger.Error("fix-review.push-diverged.human-required", "task_id", ag.TaskID, "agent_id", ag.ID, "err", err)
	}
}

// captureHeadSHA returns the worktree HEAD commit for the task, or "" when the
// task has no live worktree or git fails. Best-effort; called before the async
// worktree cleanup so the directory still exists.
func (h *Handler) captureHeadSHA(taskID string) string {
	if h.worktrees == nil || h.tasks == nil {
		return ""
	}
	t, err := h.tasks.Get(taskID)
	if err != nil || !h.worktrees.Exists(t) {
		return ""
	}
	out, err := exec.CommandContext(context.Background(), "git", "-C", h.worktrees.PathFor(t), "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// runOutcome derives the stats.RunRecord outcome ("completed"/"failed") for a
// terminal agent run. Test-runner is special-cased: its exitErr reflects
// whether the *process* exited clean, not whether it correctly did its job
// (proving or failing to disprove the implementation). A test-runner that
// produced a protocol-valid PASS/FAIL verdict succeeded at its job even if a
// benign post-result glitch (e.g. a trailing stream hiccup) left exitErr
// non-nil, so it is recorded as "completed" rather than inflating the role's
// failure_rate with a run that was never actually a failure. Other roles are
// unaffected: their exitErr already reflects whether they completed the task.
func runOutcome(role agent.Role, exitErr error, resultContent string) string {
	if exitErr == nil {
		return "completed"
	}
	if role == agent.RoleTestRunner {
		if v := workflow.ExtractTestVerdict(resultContent); v == "PASS" || v == "FAIL" {
			return "completed"
		}
	}
	return "failed"
}

func (h *Handler) roleForAgentName(name string) agent.Role {
	role, ok := agent.ParseRoleFromName(name)
	if ok || !strings.Contains(name, ":") {
		return role
	}
	h.logger.Warn("agent.role.unknown-prefix", "name", name)
	return role
}

// recordRunStats persists a stats.RunRecord for the completed agent.
// No-op when the stats store failed to initialize at startup. resultContent
// is the agent's final message text, forwarded to runOutcome for test-runner
// verdict recovery.
func (h *Handler) recordRunStats(ag *agent.Agent, role agent.Role, cost, duration float64, exitErr error, resultContent string) {
	if h.stats == nil {
		return
	}
	in := ag.GetInputTokens()
	out := ag.GetOutputTokens()
	cacheCreate := ag.GetCacheCreationInputTokens()
	cacheRead := ag.GetCacheReadInputTokens()
	reasoning := ag.GetReasoningTokens()
	agCost := estimatedRunCost(ag, cost, ag.GetPremiumRequests())
	outcome := runOutcome(role, exitErr, resultContent)
	var projectID string
	if ag.TaskID != "" {
		if t, err := h.tasks.Get(ag.TaskID); err == nil {
			projectID = t.ProjectID
		}
	}
	_ = h.stats.Record(stats.RunRecord{
		ID:                       ag.ID,
		TaskID:                   ag.TaskID,
		ProjectID:                projectID,
		Mode:                     ag.Mode,
		Role:                     string(role),
		Model:                    ag.Model,
		Provider:                 ag.Provider,
		ReasoningEffort:          ag.ReasoningEffort,
		ExperimentID:             ag.ExperimentID,
		VariantID:                ag.VariantID,
		AssignmentUnit:           ag.AssignmentUnit,
		AssignmentKey:            ag.AssignmentKey,
		CostUSD:                  agCost,
		DurationS:                duration,
		InputTokens:              in,
		OutputTokens:             out,
		CacheCreationInputTokens: cacheCreate,
		CacheReadInputTokens:     cacheRead,
		ReasoningTokens:          reasoning,
		PremiumRequests:          ag.GetPremiumRequests(),
		TurnCount:                ag.GetTurnCount(),
		ToolCalls:                ag.GetToolCalls(),
		Outcome:                  outcome,
		Timestamp:                time.Now(),
	})
	if h.limits != nil {
		_ = h.limits.RecordUsage(limits.UsageEvent{
			ID:                       "sybra-run:" + ag.ID,
			Provider:                 ag.Provider,
			Source:                   limits.SourceRunStats,
			TaskID:                   ag.TaskID,
			AgentID:                  ag.ID,
			SessionID:                ag.GetSessionID(),
			Model:                    ag.Model,
			CostUSD:                  agCost,
			InputTokens:              in,
			OutputTokens:             out,
			CacheCreationInputTokens: cacheCreate,
			CacheReadInputTokens:     cacheRead,
			ReasoningTokens:          reasoning,
			PremiumRequests:          ag.GetPremiumRequests(),
			Timestamp:                time.Now(),
		})
	}
}

func estimatedRunCost(ag *agent.Agent, cost, premiumRequests float64) float64 {
	if cost > 0 {
		return cost
	}
	if ag.Provider == "copilot" {
		return stats.EstimateCopilotCost(premiumRequests)
	}
	if ag.Provider == "codex" {
		return stats.EstimateCostDetailed(
			ag.Model,
			ag.GetInputTokens(),
			ag.GetOutputTokens(),
			0,
			ag.GetCacheReadInputTokens(),
			ag.GetReasoningTokens(),
			ag.StartedAt,
		)
	}
	return 0
}

// isRateLimitedRun reports whether a run was rejected by a transient provider
// limit (rate/session/usage limit) rather than crashing. Some CLIs report this
// as an exit-0 result, so the agent error kind is authoritative.
func isRateLimitedRun(ag *agent.Agent, exitErr error) bool {
	return ag.GetErrorKind() == "rate_limit"
}

// isSignalKill reports whether err represents a process killed by an OS signal.
// Signal kills cover both infrastructure interruptions (SIGTERM from container/OS
// shutdown) and Sybra's own StopAgent (cancel ctx → process killed). Neither
// completes the agent's work, so the workflow should not advance.
//
// Two paths are detected:
//   - ws.Signaled() == true: the kernel delivered the signal directly (rare for
//     claude/codex, which install their own signal handlers).
//   - Exit codes 130/143/137 (128+SIGINT/SIGTERM/SIGKILL): claude/codex catch the
//     signal, clean up, then exit with this conventional code. Go reports these as
//     Exited()==true/Signaled()==false, so ws.Signaled() misses these.
func isSignalKill(err error) bool {
	if err == nil {
		return false
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return true
	}
	// claude/codex install signal handlers and convert SIGINT/SIGTERM/SIGKILL
	// into conventional 128+signal exit codes (Exited()==true, Signaled()==false),
	// so ws.Signaled() misses an externally-interrupted run. Treat those codes as
	// kills so the workflow stalls instead of advancing on incomplete work.
	switch exitErr.ExitCode() {
	case 130, 143, 137: // 128+SIGINT, 128+SIGTERM, 128+SIGKILL
		return true
	}
	return false
}

// finalAssistantText walks the assistant turns backward and returns the
// first one that actually decodes via verdict.Parse — this avoids selecting
// an earlier turn that merely echoes the schema or discusses "the decision"
// in prose that happens to parse as JSON. If no turn parses (e.g. the run
// produced no valid verdict at all), it falls back to the last turn that at
// least looks verdict-shaped, then the last result turn, purely so callers
// have raw text to surface for diagnostics.
//
// Duplicated from internal/sybra's copy (app_human_review.go) rather than
// shared, to avoid completion importing sybra (import cycle, since sybra
// constructs completion.Handler) or sybra importing completion just for a
// text-extraction helper unrelated to the completion pipeline.
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

// lastAssistantText returns the content of the last assistant-typed stream event.
// Unlike finalAssistantText it applies no sybra-verdict gating — it is the
// general "what did the model say last" accessor used to fill c.Result for
// providers (codex) whose terminal turn.completed event carries no text.
func lastAssistantText(ag *agent.Agent) string {
	out := ag.Output()
	for i := range slices.Backward(out) {
		if out[i].Type == "assistant" {
			return out[i].Content
		}
	}
	return ""
}

// OnWorkflowComplete is the callback installed via
// workflowEngine.SetOnComplete. Two responsibilities:
//
//  1. Clear the PR-tracker cooldown for the resolved (issue, kind) pair so a
//     future failure of the same kind is retried fresh.
//  2. Bridge the workflow cascade: simple-task-plan flips status to
//     in-progress, then ends; simple-task-implement flips status to
//     ready-review, then ends. The status hook in initStatusHook fires
//     DispatchEvent while *this* workflow is still active and gets rejected
//     with ErrWorkflowAlreadyActive. By re-dispatching here — *after* the
//     workflow has reached ExecCompleted — we let the next workflow in the
//     chain pick up where this one left off. Idempotent: terminal statuses
//     (done/cancelled/in-review/human-required/blocked) match no triggers,
//     so DispatchEvent returns "" without starting anything.
func (h *Handler) OnWorkflowComplete(info workflow.CompletionInfo) {
	if h.workflowEngine == nil {
		return
	}
	t, err := h.tasks.Get(info.TaskID)
	if err != nil {
		return
	}
	if task.IsTerminalStatus(t.Status) {
		return
	}
	dispatched, err := h.workflowEngine.DispatchEvent(
		info.TaskID,
		"task.status_changed",
		map[string]string{"task.status": string(t.Status)},
		nil,
	)
	if err != nil {
		// ErrWorkflowAlreadyActive is benign here: a parallel start (e.g. the
		// status hook race) has already won, so the cascade is in good hands.
		if !errors.Is(err, workflow.ErrWorkflowAlreadyActive) {
			h.logger.Error("workflow.cascade.dispatch",
				"task_id", info.TaskID, "from_workflow", info.WorkflowID,
				"status", string(t.Status), "err", err)
		}
		return
	}
	if dispatched != "" {
		h.logger.Info("workflow.cascade.dispatched",
			"task_id", info.TaskID, "from_workflow", info.WorkflowID,
			"status", string(t.Status), "to_workflow", dispatched)
	}
}
