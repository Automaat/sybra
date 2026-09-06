package sybra

import (
	"context"
	"errors"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/workercontrol"
)

// RemoteResultRecoveryReport deliberately carries only counts and an opaque
// paging cursor. Task bodies, project identities, errors, and artifacts stay
// inside the leader's stores, including for work-typed projects.
type RemoteResultRecoveryReport struct {
	Apply        bool           `json:"apply"`
	Scanned      int            `json:"scanned"`
	Eligible     int            `json:"eligible"`
	Acknowledged int            `json:"acknowledged"`
	Preserved    int            `json:"preserved"`
	Events       int            `json:"events"`
	Reasons      map[string]int `json:"reasons"`
	NextAfter    string         `json:"nextAfter,omitempty"`
}

func (a *App) canonicalRemoteReceipt(result workercontrol.PendingResult) string {
	if a.tasks == nil {
		return ""
	}
	t, err := a.tasks.Get(result.TaskID)
	if err != nil {
		return ""
	}
	for i := range t.AgentRuns {
		run := &t.AgentRuns[i]
		if run.AgentID == result.RunID && run.State == string(agent.StateStopped) {
			return run.RemoteCompletionReceipt
		}
	}
	return ""
}

func (a *App) acknowledgeRemoteResult(ctx context.Context, runID string) (bool, error) {
	if a.workerControl == nil {
		return false, workercontrol.ErrCompletionUnproven
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, err := a.workerControl.ResultForRun(ctx, runID)
	if err != nil {
		return false, err
	}
	return a.workerControl.AcknowledgeCompletedResult(ctx, runID, a.canonicalRemoteReceipt(result))
}

func (a *App) remoteResultPersisted(ag *agent.Agent) {
	if ag.GetRemoteCompletionReceipt() == "" {
		return
	}
	// The completion handler may have retried its write after the relay's
	// context ended. A short independent context can finish that durable ACK;
	// a crash here is repaired by the same operator reconciliation path.
	if _, err := a.acknowledgeRemoteResult(context.Background(), ag.ID); err != nil &&
		!errors.Is(err, workercontrol.ErrCompletionUnproven) && a.logger != nil {
		a.logger.Warn("cluster.remote.completion-ack", "run_id", ag.ID, "err", err)
	}
}

// ReconcileRemoteResults acknowledges only terminal events already consumed
// into a durable canonical result. It never replays completion callbacks,
// imports work, transitions tasks, or starts agents. apply=false is read-only.
func (a *App) ReconcileRemoteResults(ctx context.Context, apply bool, after string, limit int) (RemoteResultRecoveryReport, error) {
	report := RemoteResultRecoveryReport{Apply: apply, Reasons: map[string]int{}}
	if a.workerControl == nil {
		return report, validationError("remote result recovery requires a database-backed leader")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	results, err := a.workerControl.PendingResults(ctx, after, limit)
	if err != nil {
		return report, err
	}
	for _, result := range results {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		report.Scanned++
		report.Events += result.PendingEvents
		receipt := a.canonicalRemoteReceipt(result)
		if reason := result.HoldReason(receipt); reason != "" {
			report.Preserved++
			report.Reasons[reason]++
			continue
		}
		report.Eligible++
		if apply {
			// Re-read canonical proof and revalidate the locked terminal row;
			// an earlier dry run is never authorization to apply a stale verdict.
			changed, err := a.acknowledgeRemoteResult(ctx, result.RunID)
			if errors.Is(err, workercontrol.ErrCompletionUnproven) {
				report.Eligible--
				report.Preserved++
				report.Reasons["proof_changed"]++
				continue
			}
			if err != nil {
				return report, err
			}
			if changed {
				report.Acknowledged++
			}
		}
	}
	if len(results) == limit {
		report.NextAfter = results[len(results)-1].RunID
	}
	return report, nil
}
