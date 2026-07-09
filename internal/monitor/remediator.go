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
	UpdateRun(taskID, agentID string, patch task.RunPatch) error
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
		if isHumanRequiredStuck(a) {
			return r.remediateHumanRequiredStuck(a)
		}
		return "", fmt.Errorf("remediator: stuck_human_blocked remediation requires human-required status")
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
				_ = r.tasks.UpdateRun(a.TaskID, t.AgentRuns[i].AgentID, task.RunPatch{State: task.Ptr("stopped")})
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

// remediateHumanRequiredStuck refreshes the status reason on a task that is
// already human-required and has exceeded its dwell budget. Updating the task
// file stamps a new UpdatedAt, resetting the dwell timer and suppressing
// repeated meta-task creation for the same block.
//
// When the anomaly's stall correlates with a known, already-tracked
// lost_agent investigation (detectStuckHumanBlocked's
// known_lost_agent_investigation evidence), this instead auto-retries the
// task once — see retryKnownLostAgentStuck.
func (r *remediator) remediateHumanRequiredStuck(a Anomaly) (string, error) {
	if a.TaskID == "" {
		return "", fmt.Errorf("stuck_human_blocked without task id")
	}
	if known, _ := a.Evidence["known_lost_agent_investigation"].(bool); known {
		return r.retryKnownLostAgentStuck(a)
	}
	// Empty update: preserve existing StatusReason; Marshal stamps new UpdatedAt.
	upd := task.Update{}
	if _, err := r.tasks.Update(a.TaskID, upd); err != nil {
		return "", fmt.Errorf("tag human-required task %s stalled: %w", a.TaskID, err)
	}
	return string(a.Kind) + ":" + a.TaskID, nil
}

// retryKnownLostAgentStuck moves a task out of human-required back to
// in-progress when its stall already has an open, monitor-filed lost_agent
// investigation tracking the root cause — no point leaving it parked for a
// human when recovery can retry it and the finding is already recorded
// elsewhere. Stamps monitorAutoRetriedTag so a second stall on the same task
// (the auto-retry did not help) falls back to the normal human-review path
// instead of bouncing between in-progress and human-required forever.
func (r *remediator) retryKnownLostAgentStuck(a Anomaly) (string, error) {
	t, err := r.tasks.Get(a.TaskID)
	if err != nil {
		return "", fmt.Errorf("retry known lost_agent stuck task %s: %w", a.TaskID, err)
	}
	tags := append(slices.Clone(t.Tags), monitorAutoRetriedTag)
	upd := task.Update{
		Status:       task.Ptr(task.StatusInProgress),
		StatusReason: task.Ptr("monitor: stall matches an already-tracked lost_agent investigation; auto-retrying"),
		Tags:         &tags,
	}
	if _, err := r.tasks.Update(a.TaskID, upd); err != nil {
		return "", fmt.Errorf("retry known lost_agent stuck task %s: %w", a.TaskID, err)
	}
	return string(a.Kind) + ":retry:" + a.TaskID, nil
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
