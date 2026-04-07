package main

import (
	"context"
	"slices"
	"time"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/task"
)

const orchestratorSession = "synapse-orchestrator"
const maxConcurrentAgents = 3

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
			a.worktrees.CleanupOrphaned()
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
	if err := a.orchSvc.StartOrchestrator(); err != nil {
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
			if err := a.reviewer.startReviewAgent(t); err != nil {
				a.logger.Error("auto-dispatch.review.failed", "task_id", t.ID, "err", err)
			}
			continue
		}
		if _, err := a.taskSvc.UpdateTask(t.ID, map[string]any{"status": string(task.StatusInProgress)}); err != nil {
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
			if err := a.planSvc.PlanTask(id); err != nil {
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
