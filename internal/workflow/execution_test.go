package workflow

import (
	"testing"
	"time"
)

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
