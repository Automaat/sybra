package audit

import (
	"testing"
	"time"
)

func TestNormalizeAgentRuns(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

	events := []Event{
		{Timestamp: base, Type: EventAgentStarted, TaskID: "t-success", AgentID: "a-success"},
		{Timestamp: base.Add(time.Minute), Type: EventAgentCompleted, TaskID: "t-success", AgentID: "a-success", Data: map[string]any{"state": "stopped", "cost_usd": 0.1}},

		{Timestamp: base.Add(2 * time.Minute), Type: EventAgentStarted, TaskID: "t-fail", AgentID: "a-fail"},
		{Timestamp: base.Add(3 * time.Minute), Type: EventAgentCompleted, TaskID: "t-fail", AgentID: "a-fail", Data: map[string]any{"state": "error", "cost_usd": 0.2}},
		{Timestamp: base.Add(4 * time.Minute), Type: EventAgentFailed, TaskID: "t-fail", AgentID: "a-fail", Data: map[string]any{"cost_usd": 9.9}},

		{Timestamp: base.Add(5 * time.Minute), Type: EventAgentStarted, TaskID: "t-lost", AgentID: "a-lost"},

		{Timestamp: base.Add(6 * time.Minute), Type: EventAgentCompleted, TaskID: "t-reattached", AgentID: "a-reattached", Data: map[string]any{"state": "stopped"}},

		{Timestamp: base.Add(7 * time.Minute), Type: EventAgentCompleted, TaskID: "t-legacy", AgentID: "a-legacy", Data: map[string]any{}},

		{Timestamp: base.Add(8 * time.Minute), Type: EventAgentFailed, TaskID: "t-legacy-failed", AgentID: "a-legacy-failed", Data: map[string]any{"cost_usd": 0.3}},
	}

	got := NormalizeAgentRuns(events)
	if len(got) != 6 {
		t.Fatalf("NormalizeAgentRuns() returned %d runs, want 6", len(got))
	}

	assertRun := func(agentID string) RunLifecycle {
		t.Helper()
		for i := range got {
			if got[i].AgentID == agentID {
				return got[i]
			}
		}
		t.Fatalf("missing run for agent %s", agentID)
		return RunLifecycle{}
	}

	success := assertRun("a-success")
	if !success.Started || !success.Terminal || success.Failed || success.Reattached || success.Lost {
		t.Fatalf("success run mismatch: %+v", success)
	}
	if success.Compatibility != RunCompatCanonical {
		t.Fatalf("success compatibility = %q, want %q", success.Compatibility, RunCompatCanonical)
	}

	failed := assertRun("a-fail")
	if !failed.Started || !failed.Terminal || !failed.Failed || failed.Reattached || failed.Lost {
		t.Fatalf("failed run mismatch: %+v", failed)
	}
	if failed.Compatibility != RunCompatLegacyFailedShadowed {
		t.Fatalf("failed compatibility = %q, want %q", failed.Compatibility, RunCompatLegacyFailedShadowed)
	}
	if failed.TerminalEvent.Type != EventAgentCompleted {
		t.Fatalf("failed terminal event = %q, want %q", failed.TerminalEvent.Type, EventAgentCompleted)
	}
	if cost, _ := failed.TerminalEvent.Data["cost_usd"].(float64); cost != 0.2 {
		t.Fatalf("failed canonical cost = %v, want 0.2", failed.TerminalEvent.Data["cost_usd"])
	}

	lost := assertRun("a-lost")
	if !lost.Started || lost.Terminal || !lost.Lost || lost.Reattached {
		t.Fatalf("lost run mismatch: %+v", lost)
	}

	reattached := assertRun("a-reattached")
	if reattached.Started || !reattached.Terminal || reattached.Lost || !reattached.Reattached {
		t.Fatalf("reattached run mismatch: %+v", reattached)
	}

	missingState := assertRun("a-legacy")
	if missingState.Failed {
		t.Fatalf("missing-state completed run must not fail: %+v", missingState)
	}
	if missingState.Compatibility != RunCompatMissingStateAssumedStopped {
		t.Fatalf("missing-state compatibility = %q, want %q", missingState.Compatibility, RunCompatMissingStateAssumedStopped)
	}

	legacyFailed := assertRun("a-legacy-failed")
	if !legacyFailed.Failed || legacyFailed.Compatibility != RunCompatLegacyFailed || legacyFailed.Started {
		t.Fatalf("legacy failed run mismatch: %+v", legacyFailed)
	}
}
