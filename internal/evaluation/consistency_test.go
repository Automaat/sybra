package evaluation

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/health"
	"github.com/Automaat/sybra/internal/stats"
)

// TestReconcileReports proves the Scorecard and health.Stats computed over
// the same audit event stream agree on run totals and failure outcomes — the
// #2441 "report-consistency" requirement. Both derive their counts from
// runacct.Count with the identical CountsTowardCodeAuthorFailureRate config
// (Compute/scanReliability here, health.BuildStats there); this test proves
// that today, and would catch either side silently drifting from the other.
func TestReconcileReports(t *testing.T) {
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(time.Hour)

	events := []audit.Event{
		{Type: audit.EventAgentStarted, TaskID: "A", AgentID: "a1", Timestamp: since.Add(time.Minute), Data: map[string]any{"role": "implementation"}},
		{Type: audit.EventAgentCompleted, TaskID: "A", AgentID: "a1", Timestamp: since.Add(2 * time.Minute), Data: map[string]any{"role": "implementation", "state": "stopped"}},
		{Type: audit.EventAgentFailed, TaskID: "B", AgentID: "b1", Timestamp: since.Add(3 * time.Minute), Data: map[string]any{"role": "implementation"}},
		{Type: audit.EventAgentFailed, TaskID: "C", AgentID: "c1", Timestamp: since.Add(4 * time.Minute), Data: map[string]any{"role": "implementation"}},
	}
	records := []stats.RunRecord{
		{TaskID: "A", Role: "implementation", Outcome: stats.OutcomeCompleted, Timestamp: since.Add(2 * time.Minute)},
		{TaskID: "B", Role: "implementation", Outcome: stats.OutcomeFailed, Timestamp: since.Add(3 * time.Minute)},
		{TaskID: "C", Role: "implementation", Outcome: stats.OutcomeFailed, Timestamp: since.Add(4 * time.Minute)},
	}

	sc := Compute(records, events, since, until)
	hs := health.BuildStats(events)

	if got := ReconcileReports(sc, hs); len(got) != 0 {
		t.Fatalf("ReconcileReports = %+v, want no discrepancies (same event stream, same accounting)", got)
	}
}

// TestReconcileReports_DetectsDrift proves the reconciliation is not a
// vacuous always-pass check: two genuinely different inputs must surface a
// Discrepancy.
func TestReconcileReports_DetectsDrift(t *testing.T) {
	sc := Scorecard{AgentRuns: 5, AgentFailures: 2, AgentResolvedRuns: 5, AgentStalls: 0}
	hs := health.Stats{TotalAgentRuns: 5, FailedAgentRuns: 1, ResolvedRuns: 5, StalledRuns: 0}

	got := ReconcileReports(sc, hs)
	if len(got) != 1 {
		t.Fatalf("ReconcileReports = %+v, want exactly 1 discrepancy (agent_failures)", got)
	}
	if got[0].Field != "agent_failures" || got[0].Scorecard != 2 || got[0].Health != 1 {
		t.Errorf("discrepancy = %+v, want field=agent_failures scorecard=2 health=1", got[0])
	}
}
