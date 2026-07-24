package audit

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/runacct"
	"github.com/Automaat/sybra/internal/runoutcome"
)

func TestRunRecordsCanonicalizeMixedFixture(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

	events := []Event{
		{Timestamp: base, Type: EventAgentStarted, TaskID: "t-success", AgentID: "a-success"},
		{Timestamp: base.Add(time.Minute), Type: EventAgentCompleted, TaskID: "t-success", AgentID: "a-success", Data: map[string]any{"state": "stopped", "role": ""}},

		{Timestamp: base.Add(2 * time.Minute), Type: EventAgentStarted, TaskID: "t-fail", AgentID: "a-fail"},
		{Timestamp: base.Add(3 * time.Minute), Type: EventAgentCompleted, TaskID: "t-fail", AgentID: "a-fail", Data: map[string]any{"state": "error", "role": ""}},
		{Timestamp: base.Add(4 * time.Minute), Type: EventAgentFailed, TaskID: "t-fail", AgentID: "a-fail"},

		{Timestamp: base.Add(5 * time.Minute), Type: EventAgentCompleted, TaskID: "t-unknown", AgentID: "a-unknown", Data: map[string]any{"role": ""}},

		{Timestamp: base.Add(6 * time.Minute), Type: EventAgentCompleted, TaskID: "t-review", AgentID: "a-review", Data: map[string]any{"state": "error", "role": "review"}},

		{Timestamp: base.Add(7 * time.Minute), Type: EventAgentStarted, TaskID: "t-lost", AgentID: "a-lost"},
	}

	records := RunRecords(events)
	if len(records) != 4 {
		t.Fatalf("RunRecords() returned %d records, want 4", len(records))
	}

	gotOutcomes := []string{records[0].Outcome, records[1].Outcome, records[2].Outcome, records[3].Outcome}
	wantOutcomes := []string{runoutcome.Completed, runoutcome.Failed, runoutcome.Unknown, runoutcome.Failed}
	for i := range wantOutcomes {
		if gotOutcomes[i] != wantOutcomes[i] {
			t.Fatalf("record[%d] outcome = %q, want %q", i, gotOutcomes[i], wantOutcomes[i])
		}
	}

	counts := runacct.Count(records, nil, runacct.CountConfig{
		CountsTowardFailure: runacct.CountsTowardCodeAuthorFailureRate,
	})
	if counts.Runs != 4 || counts.Resolved != 2 || counts.Failures != 1 || counts.Unknown != 1 {
		t.Fatalf("counts = %+v, want runs=4 resolved=2 failures=1 unknown=1", counts)
	}
}
