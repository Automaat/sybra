package selfmonitor

import (
	"context"
	"log/slog"

	"github.com/Automaat/sybra/internal/health"
	"github.com/Automaat/sybra/internal/task"
)

// Retained for compatible construction; future deterministic repairs must
// supply their own evidence and freshness fences before calling Apply.
type taskUpdater interface {
	Apply(intent task.TransitionIntent) (task.TransitionResult, error)
}

type Actor struct {
	Tasks  taskUpdater
	DryRun bool
	Logger *slog.Logger
}

// Act deliberately retires the old triage_mismatch -> flip_agent_mode action.
// Headless is the only mintable execution mode: setting it again and requeueing
// an unchanged task repairs nothing, even if a judge confirms the finding.
// Planning/specification failures need new input; infrastructure belongs to the
// incident repair path. Reporting and incident remediation remain enabled.
func (a *Actor) Act(_ context.Context, inv InvestigatedFinding) ActionRecord {
	if inv.Verdict.Classification == VerdictConfirmed && inv.Finding.Category == health.CatTriageMismatch && a.Logger != nil {
		a.Logger.Info("actor.triage_retry.refused", "fingerprint", inv.Fingerprint, "cause", inv.Finding.Cause,
			"reason", "changing execution mode does not repair the observed cause")
	}
	return ActionRecord{}
}
