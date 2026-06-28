package sybra

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/limits"
	"github.com/Automaat/sybra/internal/loopagent"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/sandbox"
	"github.com/Automaat/sybra/internal/stats"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

// maxResultLen bounds how much of the agent's last result message we
// persist into the task file. Larger values bloat task files (and the
// cross-process diff stream); smaller values truncate enough context
// that humans can't read past failures from the UI.
const maxResultLen = 2000

// newAgentCompletionHandler constructs the handler with every dependency
// the App holds. Called from wireServices once all subsystems are
// initialized — by then loopSched, humanReview, workflowEngine, and stats
// are either populated or intentionally nil (degraded init), and the
// handler's nil-checks at call time handle the latter.
func (a *App) newAgentCompletionHandler(emit func(string, any)) *AgentCompletionHandler {
	return &AgentCompletionHandler{
		DomainHandler: DomainHandler{
			logger: a.logger,
			audit:  a.audit,
			emit:   emit,
		},
		tasks:          a.tasks,
		worktrees:      a.worktrees,
		sandboxes:      a.sandboxes,
		workflowEngine: a.workflowEngine,
		stats:          a.stats,
		limits:         a.limits,
		loopSched:      a.loopSched,
		humanReview:    a.humanReview,
		prTracker:      a.prTracker,
	}
}

// AgentCompletionHandler reacts to agent.Manager and workflow.Engine
// terminal-state callbacks: persists the result, records stats, advances
// workflow, triggers worktree/sandbox cleanup. Mirrors the previous
// (a *App) onAgentComplete + recordAgentRunStats + onWorkflowComplete
// trio that lived inline in app.go.
//
// All optional dependencies (workflowEngine, stats, loopSched, humanReview,
// sandboxes) are nil-safe — the test harness wires only the deps the
// scenario exercises.
type AgentCompletionHandler struct {
	DomainHandler // logger + audit + emit

	tasks          *task.Manager
	worktrees      *worktree.Manager
	sandboxes      *sandbox.Manager
	workflowEngine *workflow.Engine
	stats          *stats.Store
	limits         *limits.Store
	loopSched      *loopagent.Scheduler
	humanReview    *humanReviewHandler
	prTracker      *github.IssueTracker
}

// OnComplete is the callback installed via agents.SetOnComplete. Called
// once per terminal agent state transition.
func (h *AgentCompletionHandler) OnComplete(ag *agent.Agent) {
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
		resultContent = lastAssistantText(ag)
	}

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
	duration := time.Since(ag.StartedAt).Seconds()
	auditData := map[string]any{
		"mode":       ag.Mode,
		"cost_usd":   cost,
		"duration_s": duration,
		"state":      string(state),
		"role":       agent.RoleFromName(ag.Name),
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

	h.recordRunStats(ag, cost, duration, exitErr)

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

	truncated := resultContent
	if len(truncated) > maxResultLen {
		truncated = truncated[:maxResultLen] + "\n... (truncated)"
	}
	runUpdates := map[string]any{
		"state":            string(state),
		"cost_usd":         cost,
		"premium_requests": premiumRequests,
		"result":           truncated,
		"log_file":         ag.LogPath,
		"session_id":       ag.GetSessionID(),
		"model":            ag.Model,
		"provider":         ag.Provider,
	}
	addRunMetadata(runUpdates, ag)
	// For human-review agents, parse the verdict from the live (untruncated)
	// output and persist it in its own field so detector.go can read it even
	// when the full result text is longer than maxResultLen.
	if agent.RoleFromName(ag.Name) == agent.RoleHumanReview {
		if v, err := parseVerdict(finalAssistantText(ag)); err == nil {
			runUpdates["verdict"] = v.Decision
		}
	}
	// Capture the worktree HEAD while it still exists (cleanup below is async) so
	// landing can later detect human edits after the agent (merged_with_edits).
	if sha := h.captureHeadSHA(ag.TaskID); sha != "" {
		runUpdates["head_sha"] = sha
	}
	if err := h.tasks.UpdateRun(ag.TaskID, ag.ID, runUpdates); err != nil {
		h.logger.Error("task.update-run", "task_id", ag.TaskID, "agent_id", ag.ID, "err", err)
	}
	h.markCompletedReview(ag, exitErr)

	// Human-review agents are out-of-band diagnostics — they must not
	// feed into the workflow engine (which would advance the step that
	// originally caused the human-required transition based on the
	// diagnostic verdict).
	if agent.RoleFromName(ag.Name) == agent.RoleHumanReview {
		if h.humanReview != nil {
			h.humanReview.onComplete(ag)
		}
		return
	}

	if agent.RoleFromName(ag.Name) == agent.RoleFixReview && exitErr == nil {
		h.pushFixReviewBranch(ag)
	}

	if !h.notifyWorkflowEngine(ag, resultContent, exitErr) {
		return
	}

	// Worktree and sandbox cleanup for terminal tasks (after engine
	// advances, so status is final).
	if t, err := h.tasks.Get(ag.TaskID); err == nil && task.IsTerminalStatus(t.Status) {
		go h.worktrees.Remove(ag.TaskID)
		if h.sandboxes != nil {
			go h.sandboxes.Stop(ag.TaskID)
		}
	}
}

// emitPermissionDenialAudits emits one agent.permission_denied audit event per
// auto-mode denial recorded during the run. Batched at completion time so the
// audit log is not spammed mid-run; a killed run may drop its denial events —
// the permission_posture on agent.started is the durable observability signal.
func (h *AgentCompletionHandler) emitPermissionDenialAudits(ag *agent.Agent) {
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

func addRunMetadata(updates map[string]any, ag *agent.Agent) {
	if ag.ExperimentID != "" {
		updates["experiment_id"] = ag.ExperimentID
		updates["variant_id"] = ag.VariantID
		updates["assignment_unit"] = ag.AssignmentUnit
		updates["assignment_key"] = ag.AssignmentKey
	}
	if ag.ReasoningEffort != "" {
		updates["reasoning_effort"] = ag.ReasoningEffort
	}
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
func (h *AgentCompletionHandler) notifyWorkflowEngine(ag *agent.Agent, resultContent string, exitErr error) bool {
	if h.workflowEngine == nil {
		return true
	}
	rateLimited := isRateLimitedRun(ag, exitErr)
	if isSignalKill(exitErr) || ag.WasStopped() || rateLimited {
		h.logger.Warn("agent.completion.stall",
			"task_id", ag.TaskID, "agent_id", ag.ID,
			"signaled", isSignalKill(exitErr), "stopped", ag.WasStopped(),
			"rate_limited", rateLimited)
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

func (h *AgentCompletionHandler) markCompletedReview(ag *agent.Agent, exitErr error) {
	if agent.RoleFromName(ag.Name) != agent.RoleReview || exitErr != nil {
		return
	}
	reviewed := true
	if _, err := h.tasks.Update(ag.TaskID, task.Update{Reviewed: &reviewed}); err != nil {
		h.logger.Warn("task.mark-reviewed", "task_id", ag.TaskID, "agent_id", ag.ID, "err", err)
	}
}

func (h *AgentCompletionHandler) pushFixReviewBranch(ag *agent.Agent) {
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
		branch, err = project.CurrentBranch(wtPath)
		if err != nil {
			h.logger.Warn("fix-review.push.branch", "task_id", ag.TaskID, "agent_id", ag.ID, "path", wtPath, "err", err)
			return
		}
	}

	if err := project.PushSync(wtPath, branch); err != nil {
		if errors.Is(err, project.ErrBranchMissing) {
			h.logger.Info("fix-review.push-skipped", "task_id", ag.TaskID, "agent_id", ag.ID, "branch", branch, "reason", "local branch ref missing")
			return
		}
		h.logger.Warn("fix-review.push", "task_id", ag.TaskID, "agent_id", ag.ID, "branch", branch, "err", err)
		return
	}
	h.logger.Info("fix-review.pushed", "task_id", ag.TaskID, "agent_id", ag.ID, "branch", branch)
}

// captureHeadSHA returns the worktree HEAD commit for the task, or "" when the
// task has no live worktree or git fails. Best-effort; called before the async
// worktree cleanup so the directory still exists.
func (h *AgentCompletionHandler) captureHeadSHA(taskID string) string {
	if h.worktrees == nil || h.tasks == nil {
		return ""
	}
	t, err := h.tasks.Get(taskID)
	if err != nil || !h.worktrees.Exists(t) {
		return ""
	}
	out, err := exec.Command("git", "-C", h.worktrees.PathFor(t), "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// recordRunStats persists a stats.RunRecord for the completed agent.
// No-op when the stats store failed to initialize at startup.
func (h *AgentCompletionHandler) recordRunStats(ag *agent.Agent, cost, duration float64, exitErr error) {
	if h.stats == nil {
		return
	}
	in := ag.GetInputTokens()
	out := ag.GetOutputTokens()
	cacheCreate := ag.GetCacheCreationInputTokens()
	cacheRead := ag.GetCacheReadInputTokens()
	reasoning := ag.GetReasoningTokens()
	agCost := estimatedRunCost(ag, cost, ag.GetPremiumRequests())
	outcome := "failed"
	if exitErr == nil {
		outcome = "completed"
	}
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
		Role:                     string(agent.RoleFromName(ag.Name)),
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
//     Exited()==true/Signaled()==false, so ws.Signaled() misses them.
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
func (h *AgentCompletionHandler) OnWorkflowComplete(info workflow.CompletionInfo) {
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
