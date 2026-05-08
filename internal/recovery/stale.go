package recovery

import (
	"slices"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

// restartStaleMinAge is the minimum age of the latest agent run before a
// stale in-progress task is eligible for respawn. Protects against
// dev-mode hot-reload loops spawning parallel agents onto the same task.
const restartStaleMinAge = 5 * time.Minute

// RestartStaleInProgress recovers in-progress tasks that lost their agent
// due to a crash or restart. Headless tasks are re-dispatched; interactive
// tasks drive the workflow engine forward via recoverStaleInteractive.
// Safe to call concurrently with the startup pass — each re-dispatch is
// guarded by HasRunningAgentForTask + the recent-run debounce.
func (r *Recovery) RestartStaleInProgress() {
	tasks, err := r.Tasks.List()
	if err != nil {
		return
	}
	for i := range tasks {
		t := tasks[i]
		if t.TaskType == task.TaskTypeChat {
			continue
		}
		if t.Status != task.StatusInProgress {
			continue
		}
		if r.Agents.HasRunningAgentForTask(t.ID) {
			continue
		}
		if slices.Contains(t.Tags, "review") {
			continue
		}
		// Tasks with a terminal workflow stuck at in-progress: restart the
		// workflow rather than spawning a bare agent. A bare agent spawn
		// would loop forever (completion callback can't advance a terminal
		// workflow), but restarting the workflow gives the callback a live
		// execution to advance.
		if r.WorkflowEngine != nil && t.Workflow != nil &&
			(t.Workflow.State == workflow.ExecCompleted || t.Workflow.State == workflow.ExecFailed) {
			wfID := t.Workflow.WorkflowID
			taskID := t.ID
			r.Logger.Info("restart-stale.restart-workflow", "task_id", taskID, "workflow", wfID)
			r.WG.Go(func() {
				if wfErr := r.WorkflowEngine.StartWorkflow(taskID, wfID); wfErr != nil {
					r.Logger.Error("restart-stale.restart-workflow.failed", "task_id", taskID, "err", wfErr)
				}
			})
			continue
		}
		// Debounce respawn when a previous run started recently. Covers
		// the dev-reload case: app restarts every few seconds, but a
		// headless subprocess from the prior lifecycle is still alive.
		if lr := lastAgentRun(&t); lr != nil && time.Since(lr.StartedAt) < restartStaleMinAge {
			r.Logger.Info("restart-stale.skip",
				"task_id", t.ID, "reason", "recent_run",
				"last_run_age_s", time.Since(lr.StartedAt).Seconds())
			continue
		}
		// Tasks whose last agent was a pr-fix should not be re-implemented.
		// Move them back to in-review so the reviews poller can re-detect
		// and fix. handlePRIssue spawns pr-fix agents directly without
		// registering a workflow, so onAgentComplete can't advance the task
		// back to in-review itself.
		if lastRun := lastAgentRun(&t); lastRun != nil && lastRun.Role == "pr-fix" {
			r.Logger.Info("restart-stale.revert-to-review", "task_id", t.ID)
			if _, updErr := r.Tasks.Update(t.ID, task.Update{Status: task.Ptr(task.StatusInReview)}); updErr != nil {
				r.Logger.Error("restart-stale.revert", "task_id", t.ID, "err", updErr)
			}
			continue
		}
		// Interactive: drive the workflow engine to advance the current
		// step using the stored agent run result — same mechanism as
		// onAgentComplete.
		if t.AgentMode != "headless" {
			r.recoverStaleInteractive(&t)
			continue
		}
		if t.ProjectID == "" {
			r.Logger.Warn("restart-stale.skip", "task_id", t.ID, "reason", "no project_id")
			continue
		}
		r.Logger.Info("restart.stale-in-progress", "task_id", t.ID, "run_role", t.RunRole)
		taskID := t.ID
		runRole := t.RunRole
		if runRole == "pr-fix" {
			r.WG.Go(func() {
				err := r.Orchestrator.StartPRFixAgent(taskID)
				metrics.OrchestratorStaleRestart(err == nil)
				r.Throttle.Log(r.Logger, "restart.pr-fix.failed", "pr-fix:"+taskID, err, "task_id", taskID)
			})
			continue
		}
		mode := t.AgentMode
		prFlag := " --draft"
		if proj, pErr := r.Projects.Get(t.ProjectID); pErr == nil && proj.Type == project.ProjectTypePet {
			prFlag = ""
		}
		prompt := "Continue implementing this task. When done, create a PR with `gh pr create" + prFlag + "`."
		r.WG.Go(func() {
			// Restart-stale only ever reaches this branch for headless
			// mode (interactive tasks are handled by recoverStaleInteractive
			// above), so OneShot is irrelevant — pass false.
			_, err := r.Orchestrator.StartAgent(taskID, mode, prompt, false)
			metrics.OrchestratorStaleRestart(err == nil)
			r.Throttle.Log(r.Logger, "restart-stale.failed", "stale:"+taskID, err, "task_id", taskID)
		})
	}
}

// recoverStaleInteractive handles interactive in-progress tasks whose
// agent died or disappeared across restarts. Marks the last agent run as
// stopped (if still claiming running) and drives the workflow engine to
// advance the current step using the stored result — mirroring the
// normal onAgentComplete callback so evaluate/next steps fire.
func (r *Recovery) recoverStaleInteractive(t *task.Task) {
	lr := lastAgentRun(t)
	if lr == nil {
		r.Logger.Info("recover-stale.skip", "task_id", t.ID, "reason", "no_agent_runs")
		return
	}
	// Only recover when the dead agent was interactive — headless
	// stragglers (triage/eval) are managed by their own error paths, and
	// we don't want to fake-complete a workflow step that needs real agent
	// output.
	if lr.Mode != "interactive" {
		return
	}
	if lr.State == string(agent.StateRunning) {
		if err := r.Tasks.UpdateRun(t.ID, lr.AgentID, map[string]any{
			"state":  string(agent.StateStopped),
			"result": "stale: agent gone, auto-recovered",
		}); err != nil {
			r.Logger.Error("recover-stale.update-run", "task_id", t.ID, "err", err)
		}
	}
	if r.WorkflowEngine == nil || t.Workflow == nil {
		r.Logger.Info("recover-stale.no-workflow", "task_id", t.ID)
		return
	}
	if t.Workflow.State == workflow.ExecCompleted || t.Workflow.State == workflow.ExecFailed {
		r.Logger.Info("recover-stale.workflow-terminal",
			"task_id", t.ID, "state", string(t.Workflow.State))
		return
	}
	// Mark the execution as recovered so the next step's template context
	// knows not to trust .Prev.Output (use recoveredOrPrev instead).
	// Persist before driving HandleAgentComplete so the engine reloads the
	// flag.
	t.Workflow.Recovered = true
	wf := t.Workflow
	if _, err := r.Tasks.Update(t.ID, task.Update{Workflow: &wf}); err != nil {
		r.Logger.Error("recover-stale.set-recovered", "task_id", t.ID, "err", err)
		return
	}
	r.Logger.Info("recover-stale.advance",
		"task_id", t.ID, "agent_id", lr.AgentID, "step", t.Workflow.CurrentStep)
	r.WorkflowEngine.HandleAgentComplete(t.ID, workflow.AgentCompletion{
		AgentID:  lr.AgentID,
		Provider: lr.Provider,
		Success:  true,
	})
}

func lastAgentRun(t *task.Task) *task.AgentRun {
	if len(t.AgentRuns) == 0 {
		return nil
	}
	return &t.AgentRuns[len(t.AgentRuns)-1]
}
