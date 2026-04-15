package selfmonitor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Automaat/synapse/internal/health"
	"github.com/Automaat/synapse/internal/task"
)

// taskUpdater is the write-path slice of task.Manager the actor needs.
// Defined as a local interface so tests can inject a fake without pulling in
// the full filesystem-backed store.
type taskUpdater interface {
	Update(id string, u task.Update) (task.Task, error)
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
	switch health.Category(inv.Finding.Category) {
	case health.CatTriageMismatch:
		return a.flipAgentMode(inv)
	default:
		return ActionRecord{}
	}
}

// flipAgentMode updates the task's agent_mode from headless to interactive
// when a triage_mismatch finding is confirmed. The task already escalated to
// human-required at least once, so headless re-dispatch will loop again.
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

	mode := task.AgentModeInteractive
	if _, err := a.Tasks.Update(inv.Finding.TaskID, task.Update{AgentMode: &mode}); err != nil {
		rec.Error = fmt.Sprintf("update task: %s", err)
		a.Logger.Warn("actor.flip_agent_mode.failed",
			"task", inv.Finding.TaskID, "err", err)
		return rec
	}

	a.Logger.Info("actor.flip_agent_mode",
		"task", inv.Finding.TaskID, "fingerprint", inv.Fingerprint)
	return rec
}

func (a *Actor) logDryRun(kind, taskID string) {
	if a.Logger != nil {
		a.Logger.Info("actor.dry_run", "kind", kind, "task", taskID)
	}
}
