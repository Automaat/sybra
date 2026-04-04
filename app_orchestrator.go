package main

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/events"
	"github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/task"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const orchestratorSession = "synapse-orchestrator"
const maxConcurrentAgents = 3

// StartOrchestrator creates the orchestrator tmux session running claude.
func (a *App) StartOrchestrator() error {
	if a.tmux.SessionExists(orchestratorSession) {
		return fmt.Errorf("orchestrator already running")
	}
	if err := a.tmux.CreateSessionInDir(orchestratorSession, "claude", config.HomeDir()); err != nil {
		return fmt.Errorf("create orchestrator session: %w", err)
	}
	a.logger.Info("orchestrator.started")
	a.logAudit(audit.EventOrchestratorStart, "", "", nil)
	runtime.EventsEmit(a.ctx, events.OrchestratorState, "running")
	return nil
}

// StopOrchestrator kills the orchestrator tmux session.
func (a *App) StopOrchestrator() error {
	if err := a.tmux.KillSession(orchestratorSession); err != nil {
		return fmt.Errorf("stop orchestrator: %w", err)
	}
	a.logger.Info("orchestrator.stopped")
	a.logAudit(audit.EventOrchestratorStop, "", "", nil)
	runtime.EventsEmit(a.ctx, events.OrchestratorState, "stopped")
	return nil
}

// IsOrchestratorRunning reports whether the orchestrator tmux session exists.
func (a *App) IsOrchestratorRunning() bool {
	return a.tmux.SessionExists(orchestratorSession)
}

// CaptureOrchestratorPane returns the current terminal output of the orchestrator.
func (a *App) CaptureOrchestratorPane() (string, error) {
	if !a.tmux.SessionExists(orchestratorSession) {
		return "", fmt.Errorf("orchestrator not running")
	}
	return a.tmux.CapturePaneOutput(orchestratorSession)
}

// AttachOrchestrator opens the orchestrator tmux session in Ghostty.
func (a *App) AttachOrchestrator() error {
	if !a.tmux.SessionExists(orchestratorSession) {
		return fmt.Errorf("orchestrator not running")
	}
	return openTmuxInGhostty(orchestratorSession, "Orchestrator")
}

func (a *App) orchestratorLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.agents.CheckInteractiveSessions()
			a.maybeStartOrchestrator()
			a.maybeDispatchTasks()
			a.maybeResumePlanning()
		}
	}
}

func (a *App) maybeStartOrchestrator() {
	if a.tmux.SessionExists(orchestratorSession) {
		return
	}

	tasks, err := a.tasks.List()
	if err != nil {
		return
	}

	hasActive := false
	for i := range tasks {
		switch tasks[i].Status {
		case task.StatusPlanning, task.StatusPlanReview, task.StatusInProgress, task.StatusInReview:
			hasActive = true
		default:
		}
		if hasActive {
			break
		}
	}
	if !hasActive {
		return
	}

	a.logger.Info("orchestrator.auto-start", "reason", "active tasks detected")
	if err := a.StartOrchestrator(); err != nil {
		a.logger.Error("orchestrator.auto-start.failed", "err", err)
	}
}

func (a *App) maybeDispatchTasks() {
	running := 0
	for _, ag := range a.agents.ListAgents() {
		if ag.State == agent.StateRunning {
			running++
		}
	}
	if running >= maxConcurrentAgents {
		return
	}

	tasks, err := a.tasks.List()
	if err != nil {
		return
	}

	var candidates []task.Task
	for i := range tasks {
		if tasks[i].Status != task.StatusTodo {
			continue
		}
		if tasks[i].AgentMode == "" || len(tasks[i].Tags) == 0 {
			continue
		}
		if slices.Contains(tasks[i].Tags, "large") {
			continue
		}
		if a.agents.HasRunningAgentForTask(tasks[i].ID) {
			continue
		}
		if tasks[i].PRNumber > 0 && tasks[i].ProjectID != "" {
			prState, err := github.FetchPRState(tasks[i].ProjectID, tasks[i].PRNumber)
			if err == nil && prState.ReadyToMerge() {
				a.logger.Info("auto-dispatch.skip", "task_id", tasks[i].ID, "reason", "pr_ready_to_merge",
					"pr", tasks[i].PRNumber, "mergeable", prState.Mergeable, "ci", prState.CIStatus())
				continue
			}
		}
		candidates = append(candidates, tasks[i])
	}
	if len(candidates) == 0 {
		return
	}

	slices.SortFunc(candidates, func(a, b task.Task) int {
		pa, pb := taskPriority(a.Tags), taskPriority(b.Tags)
		if pa != pb {
			return pa - pb
		}
		sa, sb := taskSize(a.Tags), taskSize(b.Tags)
		return sa - sb
	})

	slots := maxConcurrentAgents - running
	for i := range min(slots, len(candidates)) {
		t := candidates[i]
		a.logger.Info("auto-dispatch", "task_id", t.ID, "title", t.Title)
		if slices.Contains(t.Tags, "review") {
			if _, err := a.tasks.Update(t.ID, map[string]any{"status": string(task.StatusInProgress)}); err != nil {
				a.logger.Error("auto-dispatch.review.status", "task_id", t.ID, "err", err)
				continue
			}
			if err := a.startReviewAgent(t); err != nil {
				a.logger.Error("auto-dispatch.review.failed", "task_id", t.ID, "err", err)
			}
			continue
		}
		if _, err := a.UpdateTask(t.ID, map[string]any{"status": string(task.StatusInProgress)}); err != nil {
			a.logger.Error("auto-dispatch.failed", "task_id", t.ID, "err", err)
		}
	}
}

func (a *App) maybeResumePlanning() {
	tasks, err := a.tasks.List()
	if err != nil {
		return
	}
	for i := range tasks {
		if tasks[i].Status != task.StatusPlanning {
			continue
		}
		if a.agents.HasRunningAgentForTask(tasks[i].ID) {
			continue
		}
		a.logger.Info("plan.resume", "task_id", tasks[i].ID, "title", tasks[i].Title)
		go func(id string) {
			if err := a.PlanTask(id); err != nil {
				a.logger.Error("plan.resume.failed", "task_id", id, "err", err)
			}
		}(tasks[i].ID)
	}
}

func taskPriority(tags []string) int {
	for _, t := range tags {
		switch t {
		case "urgent":
			return 0
		case "high":
			return 1
		case "normal":
			return 2
		case "low":
			return 3
		}
	}
	return 2
}

func taskSize(tags []string) int {
	for _, t := range tags {
		switch t {
		case "small":
			return 0
		case "medium":
			return 1
		}
	}
	return 1
}
