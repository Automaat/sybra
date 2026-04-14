package monitor

import (
	"context"
	"fmt"

	"github.com/Automaat/synapse/internal/task"
)

// taskAPI is the slice of task.Manager the remediator + service needs. Keeps
// fakes in tests one-line shims and avoids accidental coupling.
type taskAPI interface {
	List() ([]task.Task, error)
	Get(id string) (task.Task, error)
	Update(id string, u task.Update) (task.Task, error)
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
	default:
		return "", fmt.Errorf("remediator: kind %q has no in-process action", a.Kind)
	}
}

func (r *remediator) resetLostAgent(a Anomaly) (string, error) {
	if a.TaskID == "" {
		return "", fmt.Errorf("lost_agent without task id")
	}
	upd := task.Update{
		Status:       task.Ptr(task.StatusTodo),
		StatusReason: task.Ptr("monitor: agent lost, resetting"),
	}
	if _, err := r.tasks.Update(a.TaskID, upd); err != nil {
		return "", fmt.Errorf("reset lost_agent task %s: %w", a.TaskID, err)
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
