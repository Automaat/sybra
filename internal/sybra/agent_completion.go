package sybra

import (
	"errors"
	"os/exec"
	"syscall"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/loopagent"
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
	loopSched      *loopagent.Scheduler
	humanReview    *humanReviewHandler
	prTracker      *github.IssueTracker
}

// OnComplete is the callback installed via agents.SetOnComplete. Called
// once per terminal agent state transition.
func (h *AgentCompletionHandler) OnComplete(ag *agent.Agent) {
	var resultContent string
	outputs := ag.Output()
	for i := range outputs {
		if outputs[i].Type == "result" {
			resultContent = outputs[i].Content
		}
	}

	// Snapshot mutable fields once under the agent's lock so both the
	// persistence write and the audit entry see a consistent view.
	state := ag.GetState()
	cost := ag.GetCostUSD()
	exitErr := ag.GetExitErr()

	// Audit logging always fires — orchestrator brain agents have no
	// parent task and skip the storage paths below, but their lifecycle
	// still belongs in the audit trail.
	duration := time.Since(ag.StartedAt).Seconds()
	h.logAudit(audit.EventAgentCompleted, ag.TaskID, ag.ID, map[string]any{
		"mode":       ag.Mode,
		"cost_usd":   cost,
		"duration_s": duration,
		"state":      string(state),
		"role":       agent.RoleFromName(ag.Name),
		"provider":   ag.Provider,
		"name":       ag.Name,
		"log_file":   ag.LogPath,
	})

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

	truncated := resultContent
	if len(truncated) > maxResultLen {
		truncated = truncated[:maxResultLen] + "\n... (truncated)"
	}
	if err := h.tasks.UpdateRun(ag.TaskID, ag.ID, map[string]any{
		"state":      string(state),
		"cost_usd":   cost,
		"result":     truncated,
		"log_file":   ag.LogPath,
		"session_id": ag.GetSessionID(),
	}); err != nil {
		h.logger.Error("task.update-run", "task_id", ag.TaskID, "agent_id", ag.ID, "err", err)
	}

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

	if h.workflowEngine != nil {
		if isSignalKill(exitErr) {
			// Agent was killed by a signal (e.g. SIGTERM on container/OS shutdown),
			// not a logical failure. Leave the workflow step stalled so
			// ResumeStalled re-dispatches the agent on the next tick.
			h.logger.Warn("agent.completion.signal-kill",
				"task_id", ag.TaskID, "agent_id", ag.ID)
			return
		}
		h.workflowEngine.HandleAgentComplete(ag.TaskID, workflow.AgentCompletion{
			AgentID:  ag.ID,
			Result:   resultContent,
			Provider: ag.Provider,
			Success:  exitErr == nil,
		})
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

// recordRunStats persists a stats.RunRecord for the completed agent.
// No-op when the stats store failed to initialize at startup.
func (h *AgentCompletionHandler) recordRunStats(ag *agent.Agent, cost, duration float64, exitErr error) {
	if h.stats == nil {
		return
	}
	in := ag.GetInputTokens()
	out := ag.GetOutputTokens()
	agCost := cost
	if agCost == 0 && ag.Provider == "codex" {
		agCost = stats.EstimateCost(ag.Model, in, out)
	}
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
		ID:           ag.ID,
		TaskID:       ag.TaskID,
		ProjectID:    projectID,
		Mode:         ag.Mode,
		Role:         string(agent.RoleFromName(ag.Name)),
		Model:        ag.Model,
		Provider:     ag.Provider,
		CostUSD:      agCost,
		DurationS:    duration,
		InputTokens:  in,
		OutputTokens: out,
		Outcome:      outcome,
		Timestamp:    time.Now(),
	})
}

// isSignalKill reports whether err represents a process killed by an OS signal.
// Signal kills (e.g. SIGTERM from container/OS shutdown) are infrastructure
// interruptions, not logical agent failures — the workflow should not advance.
func isSignalKill(err error) bool {
	if err == nil {
		return false
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled()
}

// OnWorkflowComplete is the callback installed via
// workflowEngine.SetOnComplete. Clears the PR-tracker cooldown for the
// resolved (issue, kind) pair so a future failure of the same kind is
// retried fresh.
func (h *AgentCompletionHandler) OnWorkflowComplete(info workflow.CompletionInfo) {
	kind := github.PRIssueKind(info.Variables["pr_issue_kind"])
	if kind == "" {
		return
	}
	h.prTracker.ClearCooldown(info.TaskID, kind)
	h.logger.Info("pr-tracker.cooldown-cleared",
		"task_id", info.TaskID, "kind", string(kind),
		"retries", h.prTracker.Retries(info.TaskID, kind))
}
