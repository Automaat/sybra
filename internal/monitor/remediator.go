package monitor

import (
	"context"
	"fmt"
	"slices"

	"github.com/Automaat/sybra/internal/task"
)

// taskAPI is the slice of task.Manager the remediator + service needs. Keeps
// fakes in tests one-line shims and avoids accidental coupling.
type taskAPI interface {
	List() ([]task.Task, error)
	Get(id string) (task.Task, error)
	Update(id string, u task.Update) (task.Task, error)
	UpdateRun(taskID, agentID string, updates map[string]any) error
}

// remediator runs the in-process actions for anomalies that don't need LLM
// judgment. It is split out from service.go so the service stays focused on
// orchestration.
type remediator struct {
	tasks taskAPI
}

func newRemediator(t taskAPI) *remediator { return &remediator{tasks: t} }

// Apply executes the action for one anomaly. Returns a label suitable for the
// Report.Remediated slice on success, or an error on failure. The caller logs
// errors but does not abort the cycle on them.
func (r *remediator) Apply(_ context.Context, a Anomaly) (string, error) {
	switch a.Kind {
	case KindLostAgent:
		return r.resetLostAgent(a)
	case KindUntriaged:
		return r.tagUntriaged(a)
	case KindStuckHumanBlocked:
		if isPlanReviewStuck(a) {
			return r.remediatePlanReviewStuck(a)
		}
		if isHumanRequiredStuck(a) {
			return r.remediateHumanRequiredStuck(a)
		}
		return "", fmt.Errorf("remediator: stuck_human_blocked remediation requires plan-review or human-required status")
	default:
		return "", fmt.Errorf("remediator: kind %q has no in-process action", a.Kind)
	}
}

func (r *remediator) resetLostAgent(a Anomaly) (string, error) {
	if a.TaskID == "" {
		return "", fmt.Errorf("lost_agent without task id")
	}
	// Best-effort: mark the last running agent run as stopped so the UI does
	// not continue to display it as active after recovery takes over.
	if t, err := r.tasks.Get(a.TaskID); err == nil {
		for i := range slices.Backward(t.AgentRuns) {
			if t.AgentRuns[i].State == "running" {
				_ = r.tasks.UpdateRun(a.TaskID, t.AgentRuns[i].AgentID, map[string]any{"state": "stopped"})
				break
			}
		}
	}
	upd := task.Update{
		StatusReason: task.Ptr("monitor: agent lost; recovery will resume"),
	}
	if _, err := r.tasks.Update(a.TaskID, upd); err != nil {
		return "", fmt.Errorf("mark lost_agent task %s for recovery: %w", a.TaskID, err)
	}
	return string(a.Kind) + ":" + a.TaskID, nil
}

func (r *remediator) tagUntriaged(a Anomaly) (string, error) {
	if a.TaskID == "" {
		return "", fmt.Errorf("untriaged without task id")
	}
	upd := task.Update{
		StatusReason: task.Ptr("monitor: awaiting triage"),
	}
	if _, err := r.tasks.Update(a.TaskID, upd); err != nil {
		return "", fmt.Errorf("tag untriaged task %s: %w", a.TaskID, err)
	}
	return string(a.Kind) + ":" + a.TaskID, nil
}

// remediatePlanReviewStuck sets a plan-review stuck task to human-required
// so it surfaces on the blocked queue without spawning a new meta-task.
// Only called when isPlanReviewStuck(a) is true.
func (r *remediator) remediatePlanReviewStuck(a Anomaly) (string, error) {
	if a.TaskID == "" {
		return "", fmt.Errorf("stuck_human_blocked without task id")
	}
	upd := task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("monitor: plan-review stalled"),
	}
	if _, err := r.tasks.Update(a.TaskID, upd); err != nil {
		return "", fmt.Errorf("set plan-review task %s human-required: %w", a.TaskID, err)
	}
	return string(a.Kind) + ":" + a.TaskID, nil
}

// remediateHumanRequiredStuck refreshes the status reason on a task that is
// already human-required and has exceeded its dwell budget. Updating the task
// file stamps a new UpdatedAt, resetting the dwell timer and suppressing
// repeated meta-task creation for the same block.
func (r *remediator) remediateHumanRequiredStuck(a Anomaly) (string, error) {
	if a.TaskID == "" {
		return "", fmt.Errorf("stuck_human_blocked without task id")
	}
	// Empty update: preserve existing StatusReason; Marshal stamps new UpdatedAt.
	upd := task.Update{}
	if _, err := r.tasks.Update(a.TaskID, upd); err != nil {
		return "", fmt.Errorf("tag human-required task %s stalled: %w", a.TaskID, err)
	}
	return string(a.Kind) + ":" + a.TaskID, nil
}

// isPlanReviewStuck reports whether a is a stuck_human_blocked anomaly for a
// plan-review task. These are remediated in-process (task promoted to
// human-required) and must not reach the issue sink, which would create a
// redundant meta-task.
func isPlanReviewStuck(a Anomaly) bool {
	if a.Kind != KindStuckHumanBlocked {
		return false
	}
	status, _ := a.Evidence["status"].(string)
	return status == string(task.StatusPlanReview)
}

// isHumanRequiredStuck reports whether a is a stuck_human_blocked anomaly for
// a task already in human-required status. These are remediated in-process
// (status reason refreshed to reset the dwell timer) and must not reach the
// issue sink, which would create a redundant meta-task.
func isHumanRequiredStuck(a Anomaly) bool {
	if a.Kind != KindStuckHumanBlocked {
		return false
	}
	status, _ := a.Evidence["status"].(string)
	return status == string(task.StatusHumanRequired)
}
