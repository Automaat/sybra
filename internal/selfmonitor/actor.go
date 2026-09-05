package selfmonitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Automaat/sybra/internal/health"
	"github.com/Automaat/sybra/internal/task"
)

// taskUpdater is the write-path slice of task.Manager the actor needs.
// Defined as a local interface so tests can inject a fake without pulling in
// the full filesystem-backed store.
type taskUpdater interface {
	Apply(intent task.TransitionIntent) (task.TransitionResult, error)
}

// Actor applies autonomous remediations to confirmed health findings.
// DryRun=true (the config default) logs the intended action without modifying
// any task — safe for operators who want to observe before enabling.
type Actor struct {
	Tasks  taskUpdater
	DryRun bool
	Logger *slog.Logger
}

// Act inspects a confirmed InvestigatedFinding and applies the appropriate
// remediation. Returns a zero ActionRecord (Kind=="") when the finding
// category has no actor handler or the verdict is not confirmed.
func (a *Actor) Act(_ context.Context, inv InvestigatedFinding) ActionRecord {
	if inv.Verdict.Classification != VerdictConfirmed {
		return ActionRecord{}
	}
	switch inv.Finding.Category {
	case health.CatTriageMismatch:
		return a.flipAgentMode(inv)
	default:
		return ActionRecord{}
	}
}

// flipAgentMode requeues a confirmed triage_mismatch task onto the headless
// execution path. The task already escalated to human-required at least
// once, so this re-dispatch is a retry, not a mode change — interactive is
// going away as a remediation target (see the interactive-removal umbrella).
func (a *Actor) flipAgentMode(inv InvestigatedFinding) ActionRecord {
	rec := ActionRecord{
		Category:    string(inv.Finding.Category),
		Fingerprint: inv.Fingerprint,
		Kind:        "flip_agent_mode",
		DryRun:      a.DryRun,
		TakenAt:     time.Now().UTC(),
	}

	if inv.Finding.TaskID == "" {
		rec.Error = "no task id"
		return rec
	}
	rec.Reference = inv.Finding.TaskID

	if a.DryRun {
		a.logDryRun("flip_agent_mode", inv.Finding.TaskID)
		return rec
	}

	mode := task.AgentModeHeadless
	expected := task.StatusHumanRequired
	if _, err := a.Tasks.Apply(task.TransitionIntent{
		TaskID:         inv.Finding.TaskID,
		ToStatus:       task.StatusTodo,
		Actor:          "selfmonitor.flip_agent_mode",
		ExpectedStatus: &expected,
		Extra:          task.Update{AgentMode: &mode},
	}); err != nil {
		var conflict *task.ConflictError
		if errors.As(err, &conflict) {
			if a.Logger != nil {
				a.Logger.Info("actor.flip_agent_mode.skipped",
					"task", inv.Finding.TaskID,
					"fingerprint", inv.Fingerprint,
					"status", conflict.ActualStatus)
			}
			return ActionRecord{}
		}
		rec.Error = fmt.Sprintf("update task: %s", err)
		if a.Logger != nil {
			a.Logger.Warn("actor.flip_agent_mode.failed",
				"task", inv.Finding.TaskID, "err", err)
		}
		return rec
	}

	if a.Logger != nil {
		a.Logger.Info("actor.flip_agent_mode",
			"task", inv.Finding.TaskID, "fingerprint", inv.Fingerprint)
	}
	return rec
}

func (a *Actor) logDryRun(kind, taskID string) {
	if a.Logger != nil {
		a.Logger.Info("actor.dry_run", "kind", kind, "task", taskID)
	}
}
