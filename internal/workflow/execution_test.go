package workflow

import (
	"testing"
	"time"
)

func TestLastAgentID(t *testing.T) {
	t.Run("empty history", func(t *testing.T) {
		e := &Execution{}
		if got := e.LastAgentID(); got != "" {
			t.Errorf("LastAgentID = %q, want empty", got)
		}
	})
	t.Run("most recent agent step wins, non-agent steps skipped", func(t *testing.T) {
		e := &Execution{}
		e.RecordStep(StepRecord{StepID: "implement", Status: "failed", AgentID: "agent-old"})
		e.RecordStep(StepRecord{StepID: "implement2", Status: "failed", AgentID: "agent-new"})
		e.RecordStep(StepRecord{StepID: "verify", Status: "completed"}) // no AgentID
		if got := e.LastAgentID(); got != "agent-new" {
			t.Errorf("LastAgentID = %q, want agent-new", got)
		}
	})
}

func TestLastAgentStepID(t *testing.T) {
	t.Run("empty history", func(t *testing.T) {
		e := &Execution{}
		if got := e.LastAgentStepID(); got != "" {
			t.Errorf("LastAgentStepID = %q, want empty", got)
		}
	})
	t.Run("returns step id of most recent agent step", func(t *testing.T) {
		e := &Execution{}
		e.RecordStep(StepRecord{StepID: "implement", Status: "failed", AgentID: "agent-1"})
		e.RecordStep(StepRecord{StepID: "verify", Status: "completed"}) // no AgentID
		if got := e.LastAgentStepID(); got != "implement" {
			t.Errorf("LastAgentStepID = %q, want implement", got)
		}
	})
}

func TestSetVar_InitializesNilMap(t *testing.T) {
	e := &Execution{}

	e.SetVar("key", "value")

	if e.Variables["key"] != "value" {
		t.Errorf("expected 'value', got %q", e.Variables["key"])
	}
}

func TestSetVar_OverwritesExisting(t *testing.T) {
	e := &Execution{Variables: map[string]string{"key": "old"}}

	e.SetVar("key", "new")

	if e.Variables["key"] != "new" {
		t.Errorf("expected 'new', got %q", e.Variables["key"])
	}
}

func TestRecordStep_AppendsToHistory(t *testing.T) {
	e := &Execution{}

	e.RecordStep(StepRecord{StepID: "step1", Status: "completed"})
	e.RecordStep(StepRecord{StepID: "step2", Status: "completed"})

	if len(e.StepHistory) != 2 {
		t.Fatalf("expected 2 records, got %d", len(e.StepHistory))
	}
	if e.StepHistory[0].StepID != "step1" {
		t.Errorf("first record should be step1, got %q", e.StepHistory[0].StepID)
	}
}

func TestRecordStep_TrimsAtMaxHistory(t *testing.T) {
	e := &Execution{}

	for i := range maxStepHistory + 10 {
		e.RecordStep(StepRecord{StepID: "step", Status: "completed", Output: string(rune('A' + i%26))})
	}

	if len(e.StepHistory) != maxStepHistory {
		t.Fatalf("expected %d records, got %d", maxStepHistory, len(e.StepHistory))
	}
	// First entries should have been trimmed.
	if e.StepHistory[0].Output == "A" {
		t.Error("expected first record to be trimmed")
	}
}

func TestCountStep_SurvivesHistoryTrim(t *testing.T) {
	e := &Execution{}

	total := maxStepHistory + 10
	for range total {
		e.RecordStep(StepRecord{StepID: "start_replan", Status: "completed"})
	}

	if len(e.StepHistory) != maxStepHistory {
		t.Fatalf("expected history trimmed to %d, got %d", maxStepHistory, len(e.StepHistory))
	}
	// CountStep must reflect every recorded step, not just the entries left
	// in StepHistory after trimming — otherwise churn silently resets a
	// replan/retry cap once early records are evicted.
	if got := e.CountStep("start_replan"); got != total {
		t.Errorf("expected CountStep to survive trim and report %d, got %d", total, got)
	}
}

func TestRecordStep_SeedsStepCountsFromExistingHistory(t *testing.T) {
	// Simulate a task loaded from disk before StepCounts existed: history is
	// already populated (and trimmed to the cap) while StepCounts is nil. The
	// first RecordStep must lazily seed StepCounts from the surviving history
	// window before appending, so CountStep reflects the pre-existing entries
	// plus the new one.
	e := &Execution{}
	for range maxStepHistory {
		e.StepHistory = append(e.StepHistory, StepRecord{StepID: "start_replan", Status: "completed"})
	}
	if e.StepCounts != nil {
		t.Fatalf("precondition: expected nil StepCounts, got %v", e.StepCounts)
	}

	e.RecordStep(StepRecord{StepID: "start_replan", Status: "completed"})

	if got := e.CountStep("start_replan"); got != maxStepHistory+1 {
		t.Errorf("expected seeded count %d, got %d", maxStepHistory+1, got)
	}
}

func TestLastRecord_ReturnsNilForEmptyHistory(t *testing.T) {
	e := &Execution{}

	if e.LastRecord() != nil {
		t.Error("expected nil for empty history")
	}
}

func TestLastRecord_ReturnsMostRecent(t *testing.T) {
	e := &Execution{}
	e.RecordStep(StepRecord{StepID: "first"})
	e.RecordStep(StepRecord{StepID: "second"})

	got := e.LastRecord()
	if got == nil || got.StepID != "second" {
		t.Errorf("expected 'second', got %v", got)
	}
}

func TestCountStep_CountsMatchingEntries(t *testing.T) {
	e := &Execution{}
	e.RecordStep(StepRecord{StepID: "triage", Status: "failed"})
	e.RecordStep(StepRecord{StepID: "triage", Status: "failed"})
	e.RecordStep(StepRecord{StepID: "triage", Status: "completed"})
	e.RecordStep(StepRecord{StepID: "implement", Status: "completed"})

	if got := e.CountStep("triage"); got != 3 {
		t.Errorf("expected 3 triage records, got %d", got)
	}
	if got := e.CountStep("implement"); got != 1 {
		t.Errorf("expected 1 implement record, got %d", got)
	}
	if got := e.CountStep("missing"); got != 0 {
		t.Errorf("expected 0 for missing step, got %d", got)
	}
}

func TestClearStepRecords_ResetsCountForStep(t *testing.T) {
	e := &Execution{}
	e.RecordStep(StepRecord{StepID: "run_test", Status: "failed"})
	e.RecordStep(StepRecord{StepID: "run_test", Status: "failed"})
	e.RecordStep(StepRecord{StepID: "implement", Status: "completed"})

	e.ClearStepRecords("run_test")

	if got := e.CountStep("run_test"); got != 0 {
		t.Errorf("expected 0 run_test records after clear, got %d", got)
	}
	if got := e.CountStep("implement"); got != 1 {
		t.Errorf("expected unrelated step records to survive, got %d", got)
	}

	// Re-arming should let a fresh in-step retry budget accrue again.
	e.RecordStep(StepRecord{StepID: "run_test", Status: "failed"})
	if got := e.CountStep("run_test"); got != 1 {
		t.Errorf("expected 1 run_test record after re-arm, got %d", got)
	}
}

func TestClearStepRecords_NoOpOnEmptyHistory(t *testing.T) {
	e := &Execution{}
	e.ClearStepRecords("run_test")
	if got := e.CountStep("run_test"); got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
}

func TestRecordForStep_ReturnsLatestMatch(t *testing.T) {
	e := &Execution{}
	e.RecordStep(StepRecord{StepID: "triage", Status: "failed", StartedAt: time.Now()})
	e.RecordStep(StepRecord{StepID: "triage", Status: "completed", StartedAt: time.Now()})

	got := e.RecordForStep("triage")
	if got == nil || got.Status != "completed" {
		t.Errorf("expected latest (completed), got %v", got)
	}
}

func TestRecordForStep_ReturnsNilForMissing(t *testing.T) {
	e := &Execution{}
	e.RecordStep(StepRecord{StepID: "triage"})

	if got := e.RecordForStep("missing"); got != nil {
		t.Errorf("expected nil for missing step, got %v", got)
	}
}
