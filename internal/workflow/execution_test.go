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
	e.RecordEffectIntent(makeID("run_test", 0), time.Now())
	e.RecordEffectIntent(makeID("implement", 0), time.Now())

	e.ClearStepRecords("run_test")

	if got := e.CountStep("run_test"); got != 0 {
		t.Errorf("expected 0 run_test records after clear, got %d", got)
	}
	if got := e.CountStep("implement"); got != 1 {
		t.Errorf("expected unrelated step records to survive, got %d", got)
	}
	if _, ok := e.EffectIDForStep("run_test", 0); ok {
		t.Fatal("expected run_test effect log entries to be cleared")
	}
	if _, ok := e.EffectIDForStep("implement", 0); !ok {
		t.Fatal("expected unrelated effect log entries to survive")
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

func makeID(stepID string, pos int) EffectID {
	return EffectID{Generation: 1, StepSeq: 1, StepID: stepID, Pos: pos}
}

func TestEffectLog_RecordIntentIdempotent(t *testing.T) {
	e := &Execution{}
	id := makeID("implement", 0)
	now := time.Now()
	e.RecordEffectIntent(id, now)
	e.RecordEffectIntent(id, now.Add(time.Second))
	if len(e.EffectLog) != 1 {
		t.Fatalf("expected 1 record, got %d", len(e.EffectLog))
	}
}

func TestEffectLog_RecordCompletionOnAbsent(t *testing.T) {
	e := &Execution{}
	id := makeID("implement", 0)
	e.RecordEffectCompletion(id, time.Now())
	if len(e.EffectLog) != 0 {
		t.Fatalf("completion before intent should add no record, got %d", len(e.EffectLog))
	}
}

func TestEffectLog_RecordCompletionIdempotent(t *testing.T) {
	e := &Execution{}
	id := makeID("implement", 0)
	now := time.Now()
	e.RecordEffectIntent(id, now)
	e.RecordEffectCompletion(id, now.Add(time.Second))
	first := *e.EffectLog[0].CompletedAt
	e.RecordEffectCompletion(id, now.Add(2*time.Second))
	if *e.EffectLog[0].CompletedAt != first {
		t.Fatalf("double completion should be idempotent, CompletedAt changed")
	}
}

func TestEffectLog_EffectAppliedAndPending(t *testing.T) {
	e := &Execution{}
	id := makeID("implement", 0)
	now := time.Now()

	// neither intent nor completion
	if e.EffectApplied(id) || e.EffectPending(id) {
		t.Fatal("no record: both should be false")
	}

	// intent only
	e.RecordEffectIntent(id, now)
	if e.EffectApplied(id) {
		t.Fatal("intent-only: EffectApplied should be false")
	}
	if !e.EffectPending(id) {
		t.Fatal("intent-only: EffectPending should be true")
	}

	// intent + completion
	e.RecordEffectCompletion(id, now.Add(time.Second))
	if !e.EffectApplied(id) {
		t.Fatal("after completion: EffectApplied should be true")
	}
	if e.EffectPending(id) {
		t.Fatal("after completion: EffectPending should be false")
	}
}

func TestEffectLog_EffectIDForStep(t *testing.T) {
	e := &Execution{}
	now := time.Now()
	id1 := EffectID{Generation: 1, StepSeq: 1, StepID: "implement", Pos: 0}
	id2 := EffectID{Generation: 1, StepSeq: 2, StepID: "implement", Pos: 0}
	e.RecordEffectIntent(id1, now)
	e.RecordEffectIntent(id2, now.Add(time.Second))

	got, ok := e.EffectIDForStep("implement", 0)
	if !ok {
		t.Fatal("expected hit for implement/0")
	}
	if !got.Equal(id2) {
		t.Fatalf("expected most-recent id2, got %v", got)
	}

	_, ok = e.EffectIDForStep("missing", 0)
	if ok {
		t.Fatal("expected miss for unknown step")
	}

	_, ok = e.EffectIDForStep("implement", 99)
	if ok {
		t.Fatal("expected miss for wrong pos")
	}
}

func TestEffectLog_TrimToMaxCap(t *testing.T) {
	e := &Execution{}
	now := time.Now()
	for i := range maxEffectLog + 5 {
		id := EffectID{Generation: 1, StepSeq: i, StepID: "step", Pos: 0}
		e.RecordEffectIntent(id, now)
	}
	if len(e.EffectLog) != maxEffectLog {
		t.Fatalf("expected log trimmed to %d, got %d", maxEffectLog, len(e.EffectLog))
	}
	// oldest entries evicted; first remaining should be index 5
	if e.EffectLog[0].ID.StepSeq != 5 {
		t.Fatalf("expected oldest remaining StepSeq=5, got %d", e.EffectLog[0].ID.StepSeq)
	}
}

func TestEffectLog_CloneIsolation(t *testing.T) {
	e := &Execution{}
	id := makeID("implement", 0)
	e.RecordEffectIntent(id, time.Now())

	cloned := e.Clone()
	if cloned == nil {
		t.Fatal("Clone of non-nil Execution returned nil")
	}
	now2 := time.Now().Add(time.Second)
	cloned.RecordEffectCompletion(id, now2)

	if e.EffectApplied(id) {
		t.Fatal("mutating clone's EffectLog should not affect original")
	}
}

func TestEffectLog_CloneDeepCopiesCompletedAt(t *testing.T) {
	e := &Execution{}
	id := makeID("implement", 0)
	e.RecordEffectIntent(id, time.Now())
	e.RecordEffectCompletion(id, time.Now())

	cloned := e.Clone()
	if cloned == nil {
		t.Fatal("Clone of non-nil Execution returned nil")
	}
	if cloned.EffectLog[0].CompletedAt == e.EffectLog[0].CompletedAt {
		t.Fatal("clone should not alias original's CompletedAt pointer")
	}

	original := *e.EffectLog[0].CompletedAt
	*cloned.EffectLog[0].CompletedAt = original.Add(time.Hour)
	if !e.EffectLog[0].CompletedAt.Equal(original) {
		t.Fatal("mutating clone's CompletedAt pointee should not affect original")
	}
}

func TestEffectLog_CloneDeepCopiesLeaseExpiry(t *testing.T) {
	e := &Execution{}
	id := makeID("implement", 0)
	now := time.Now().UTC()
	expiresAt := now.Add(time.Minute)
	e.EffectLog = []EffectRecord{{
		ID:             id,
		IntentAt:       now,
		Owner:          "engine-1",
		LeaseExpiresAt: &expiresAt,
	}}

	cloned := e.Clone()
	if cloned == nil {
		t.Fatal("Clone of non-nil Execution returned nil")
	}
	if cloned.EffectLog[0].LeaseExpiresAt == e.EffectLog[0].LeaseExpiresAt {
		t.Fatal("clone should not alias original's LeaseExpiresAt pointer")
	}

	original := *e.EffectLog[0].LeaseExpiresAt
	*cloned.EffectLog[0].LeaseExpiresAt = original.Add(time.Hour)
	if !e.EffectLog[0].LeaseExpiresAt.Equal(original) {
		t.Fatal("mutating clone's LeaseExpiresAt pointee should not affect original")
	}
}
