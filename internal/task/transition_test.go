package task

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Automaat/sybra/internal/autonomy"
	"github.com/Automaat/sybra/internal/events"
)

func TestManagerApply_Success(t *testing.T) {
	t.Parallel()
	m, emitter := newTestManager(t)

	var transitions []string
	m.SetStatusChangeHook(func(_ string, from, to string, _ Task) {
		transitions = append(transitions, from+"->"+to)
	})

	created, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
		panic("unreachable")
	}

	result, err := m.Apply(TransitionIntent{
		TaskID:   created.ID,
		ToStatus: StatusInProgress,
		Actor:    "test.actor",
		Extra:    Update{StatusReason: Ptr("started")},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
		panic("unreachable")
	}
	if !result.Applied {
		t.Fatal("Applied = false, want true")
	}
	if result.Task.Status != StatusInProgress {
		t.Fatalf("status = %q, want %q", result.Task.Status, StatusInProgress)
	}
	if result.Task.StatusReason != "started" {
		t.Fatalf("status reason = %q, want %q", result.Task.StatusReason, "started")
	}
	if result.Task.Generation != created.Generation+1 {
		t.Fatalf("generation = %d, want %d", result.Task.Generation, created.Generation+1)
	}
	if len(transitions) != 1 || transitions[0] != "todo->in-progress" {
		t.Fatalf("transitions = %v, want [todo->in-progress]", transitions)
	}

	names := emitter.names()
	if len(names) != 2 || names[1] != events.TaskUpdated {
		t.Fatalf("events = %v, want create+update", names)
	}
}

func TestManagerApply_RejectsMissingFields(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)
	created, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
		panic("unreachable")
	}

	cases := []struct {
		name   string
		intent TransitionIntent
	}{
		{"missing task id", TransitionIntent{ToStatus: StatusTodo, Actor: "a"}},
		{"missing to_status", TransitionIntent{TaskID: created.ID, Actor: "a"}},
		{"missing actor", TransitionIntent{TaskID: created.ID, ToStatus: StatusTodo}},
		{"extra sets status", TransitionIntent{TaskID: created.ID, ToStatus: StatusTodo, Actor: "a", Extra: Update{Status: Ptr(StatusDone)}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, err := m.Apply(c.intent); err == nil {
				t.Fatal("Apply: want error, got nil")
				panic("unreachable")
			}
		})
	}
}

func TestManagerApply_UnknownTaskFailsBeforeAnyMutation(t *testing.T) {
	t.Parallel()
	m, emitter := newTestManager(t)

	_, err := m.Apply(TransitionIntent{
		TaskID:   "does-not-exist",
		ToStatus: StatusInProgress,
		Actor:    "test.actor",
	})
	if err == nil {
		t.Fatal("Apply: want error for unknown task, got nil")
		panic("unreachable")
	}
	if len(emitter.names()) != 0 {
		t.Fatalf("events = %v, want none — a failed read must not emit", emitter.names())
	}
}

func TestManagerApply_ConflictOnStaleExpectedGeneration(t *testing.T) {
	t.Parallel()
	m, emitter := newTestManager(t)

	created, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
		panic("unreachable")
	}

	// A concurrent writer advances the task past the generation this intent
	// still expects.
	if _, err := m.Update(created.ID, Update{StatusReason: Ptr("someone else moved this")}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	baseline := emitter.names()

	stale := created.Generation
	_, err = m.Apply(TransitionIntent{
		TaskID:             created.ID,
		ToStatus:           StatusInProgress,
		Actor:              "test.actor",
		ExpectedGeneration: &stale,
	})
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Apply err = %v, want *ConflictError", err)
	}
	if !errors.Is(err, ErrTransitionConflict) {
		t.Fatal("errors.Is(err, ErrTransitionConflict) = false, want true")
	}
	if conflict.ActualGeneration == stale {
		t.Fatalf("conflict actual generation = %d, want different from stale expectation %d", conflict.ActualGeneration, stale)
	}

	after, err := m.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
		panic("unreachable")
	}
	if after.Status != StatusTodo {
		t.Fatalf("status after rejected conflict = %q, want unchanged %q", after.Status, StatusTodo)
	}
	if len(emitter.names()) != len(baseline) {
		t.Fatalf("events after rejected conflict = %v, want unchanged %v — a conflict must not emit", emitter.names(), baseline)
	}
}

func TestManagerApply_ConflictOnStaleExpectedStatus(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)

	created, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
		panic("unreachable")
	}
	if _, err := m.Update(created.ID, Update{Status: Ptr(StatusInProgress)}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	expected := StatusTodo
	_, err = m.Apply(TransitionIntent{
		TaskID:         created.ID,
		ToStatus:       StatusHumanRequired,
		Actor:          "test.actor",
		ExpectedStatus: &expected,
	})
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Apply err = %v, want *ConflictError", err)
	}
	if conflict.ActualStatus != StatusInProgress {
		t.Fatalf("conflict actual status = %q, want %q", conflict.ActualStatus, StatusInProgress)
	}

	after, err := m.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
		panic("unreachable")
	}
	if after.Status != StatusInProgress {
		t.Fatalf("status after rejected conflict = %q, want unchanged %q", after.Status, StatusInProgress)
	}
}

func TestManagerApply_ExpectedStatusMatchSucceeds(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)

	created, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
		panic("unreachable")
	}
	if _, err := m.Update(created.ID, Update{Status: Ptr(StatusInProgress)}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	expected := StatusInProgress
	result, err := m.Apply(TransitionIntent{
		TaskID:         created.ID,
		ToStatus:       StatusTodo,
		Actor:          "test.actor",
		ExpectedStatus: &expected,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
		panic("unreachable")
	}
	if result.Task.Status != StatusTodo {
		t.Fatalf("status = %q, want %q", result.Task.Status, StatusTodo)
	}
}

// TestManagerApply_IdempotentReplay covers the "replaying an idempotency key
// does not duplicate effects" acceptance criterion: a second Apply call with
// the same IdempotencyKey against a task that has not advanced generation
// since the first call is a no-op — no second hook firing, no duplicate
// effect-log entry, no second task:updated event.
func TestManagerApply_IdempotentReplay(t *testing.T) {
	t.Parallel()
	m, emitter := newTestManager(t)

	var fireCount int
	m.SetStatusChangeHook(func(string, string, string, Task) { fireCount++ })

	created, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
		panic("unreachable")
	}

	intent := TransitionIntent{
		TaskID:         created.ID,
		ToStatus:       StatusInProgress,
		Actor:          "test.actor",
		IdempotencyKey: "retry-42",
	}

	first, err := m.Apply(intent)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
		panic("unreachable")
	}
	if !first.Applied {
		t.Fatal("first Applied = false, want true")
	}

	second, err := m.Apply(intent)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
		panic("unreachable")
	}
	if second.Applied {
		t.Fatal("second Applied = true, want false (idempotent replay)")
	}
	if second.Task.Generation != first.Task.Generation {
		t.Fatalf("generation after replay = %d, want unchanged %d", second.Task.Generation, first.Task.Generation)
	}
	if len(second.Task.EffectLog) != 1 {
		t.Fatalf("effect log len after replay = %d, want 1", len(second.Task.EffectLog))
	}
	if fireCount != 1 {
		t.Fatalf("hook fire count = %d, want 1 (replay must not refire)", fireCount)
	}
	// create + first apply's update; replay must not add a third.
	if len(emitter.names()) != 2 {
		t.Fatalf("events = %v, want only create+first update", emitter.names())
	}
}

func TestManagerApply_SameIdempotencyKeyReappliesAfterGenerationAdvances(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)

	created, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
		panic("unreachable")
	}

	intent := TransitionIntent{
		TaskID:         created.ID,
		ToStatus:       StatusHumanRequired,
		Actor:          "test.actor",
		IdempotencyKey: "retry-once",
		Extra: Update{
			Escalation:      OperatorDecisionRequired("test.operator_decision", "operator decision required"),
			AutonomyOutcome: HumanRequiredOutcome(),
		},
	}
	first, err := m.Apply(intent)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
		panic("unreachable")
	}

	if _, err := m.Update(created.ID, Update{Status: Ptr(StatusInProgress)}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	second, err := m.Apply(intent)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
		panic("unreachable")
	}
	if !second.Applied {
		t.Fatal("second Applied = false, want true — generation advanced since the first consume")
	}
	if second.Task.Generation == first.Task.Generation {
		t.Fatal("generation unchanged after a genuine reapply")
	}
	if len(second.Task.EffectLog) != 2 {
		t.Fatalf("effect log len = %d, want 2", len(second.Task.EffectLog))
	}
}

func TestHumanRequiredGuardRejectsMissingAndMachineOwnedReasons(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		extra Update
	}{
		{name: "missing", extra: Update{}},
		{name: "machine", extra: Update{
			Escalation:      MachineFailure("runenv.unwritable", "source is read-only"),
			AutonomyOutcome: HumanRequiredOutcome(),
		}},
		{name: "wrong outcome", extra: Update{
			Escalation:      OperatorDecisionRequired("operator.choose", "choose a recovery"),
			AutonomyOutcome: QuarantinedOutcome(),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m, _ := newTestManager(t)
			created, err := m.Create("Title", "", "headless")
			if err != nil {
				t.Fatal(err)
				panic("unreachable")
			}
			_, err = m.Apply(TransitionIntent{
				TaskID: created.ID, ToStatus: StatusHumanRequired,
				Actor: "test.guard", Extra: tt.extra,
			})
			if err == nil {
				t.Fatal("machine/malformed escalation entered human-required")
				panic("unreachable")
			}
			got, getErr := m.Get(created.ID)
			if getErr != nil {
				t.Fatal(getErr)
				panic("unreachable")
			}
			if got.Status == StatusHumanRequired {
				t.Fatal("rejected transition mutated task")
			}
		})
	}
}

func TestHumanRequiredGuardPersistsTypedReason(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)
	created, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	reason := OperatorAuthorityRequired("github.credentials_required", "configure credentials")
	result, err := m.Apply(TransitionIntent{
		TaskID: created.ID, ToStatus: StatusHumanRequired, Actor: "test.guard",
		Extra: Update{Escalation: reason, AutonomyOutcome: HumanRequiredOutcome()},
	})
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if result.Task.Escalation.Code != "github.credentials_required" ||
		result.Task.AutonomyOutcome != "human_required" {
		t.Fatalf("typed escalation not persisted: %#v / %q", result.Task.Escalation, result.Task.AutonomyOutcome)
	}
}

func TestHumanRequiredGuardRejectsMachineOwnedReasonOnSameStatus(t *testing.T) {
	mgr, _ := newTestManager(t)
	created, err := mgr.Create("Title", "", "headless")
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	first, err := mgr.Apply(TransitionIntent{
		TaskID: created.ID, ToStatus: StatusHumanRequired, Actor: "test",
		Extra: Update{
			Escalation:      OperatorDecisionRequired("test.operator_decision", "choose a path"),
			AutonomyOutcome: HumanRequiredOutcome(),
		}, OperatorOverride: true,
	})
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	_, err = mgr.Apply(TransitionIntent{
		TaskID: first.Task.ID, ToStatus: StatusHumanRequired, Actor: "test",
		Extra: Update{
			Escalation:      MachineFailure("test.machine_failure", "repairable locally"),
			AutonomyOutcome: HumanRequiredOutcome(),
		}, OperatorOverride: true,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot transition to human-required") {
		t.Fatalf("same-status machine escalation error = %v; want eligibility rejection", err)
		panic("unreachable")
	}
}

func TestHumanRequiredGuardRejectsContradictoryOutcomeOnSameStatus(t *testing.T) {
	mgr, _ := newTestManager(t)
	created, err := mgr.Create("Title", "", "headless")
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	first, err := mgr.Apply(TransitionIntent{
		TaskID: created.ID, ToStatus: StatusHumanRequired, Actor: "test",
		Extra: Update{
			Escalation:      OperatorDecisionRequired("test.operator_decision", "choose a path"),
			AutonomyOutcome: HumanRequiredOutcome(),
		}, OperatorOverride: true,
	})
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	_, err = mgr.Apply(TransitionIntent{
		TaskID: first.Task.ID, ToStatus: StatusHumanRequired, Actor: "test",
		Extra: Update{AutonomyOutcome: QuarantinedOutcome()}, OperatorOverride: true,
	})
	if err == nil || !strings.Contains(err.Error(), "requires autonomy outcome human_required") {
		t.Fatalf("same-status contradictory outcome error = %v", err)
		panic("unreachable")
	}
}

func TestManagerUpdateMapCannotBypassHumanRequiredGuard(t *testing.T) {
	mgr, _ := newTestManager(t)
	created, err := mgr.Create("Title", "", "headless")
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	updated, err := mgr.UpdateMap(created.ID, map[string]any{"status": string(StatusHumanRequired)})
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if updated.Escalation.Code != "operator.raw_status_change" ||
		updated.Escalation.Owner != autonomy.FailureOwnerOperatorDecision ||
		updated.AutonomyOutcome != autonomy.OutcomeHumanRequired {
		t.Fatalf("raw update persisted untyped evidence: %#v / %q", updated.Escalation, updated.AutonomyOutcome)
	}
}

func TestManagerCreateFullPersistsTypedHumanRequiredEvidence(t *testing.T) {
	mgr, _ := newTestManager(t)
	status := StatusHumanRequired
	created, err := mgr.CreateFull("Title", "", "headless", Update{
		Status:          &status,
		Escalation:      OperatorDecisionRequired("test.operator_decision", "choose a path"),
		AutonomyOutcome: HumanRequiredOutcome(),
	})
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if created.Escalation.Code != "test.operator_decision" || created.AutonomyOutcome != "human_required" {
		t.Fatalf("typed create evidence = %#v / %q", created.Escalation, created.AutonomyOutcome)
	}
}

func TestManagerCreateFullRejectsMalformedTypedEvidence(t *testing.T) {
	mgr, _ := newTestManager(t)
	unknown := autonomy.Outcome("future_value")
	tests := []Update{
		{Escalation: OperatorDecisionRequired("test.operator_decision", "choose")},
		{AutonomyOutcome: &unknown},
	}
	for _, init := range tests {
		if _, err := mgr.CreateFull("Title", "", "headless", init); err == nil {
			t.Fatalf("CreateFull(%#v) accepted malformed autonomy evidence", init)
			panic("unreachable")
		}
	}
}

func TestHumanRequiredGuardRejectsStatuslessContradictoryOutcome(t *testing.T) {
	mgr, _ := newTestManager(t)
	status := StatusHumanRequired
	created, err := mgr.CreateFull("Title", "", "headless", Update{
		Status:          &status,
		Escalation:      OperatorDecisionRequired("test.operator_decision", "choose"),
		AutonomyOutcome: HumanRequiredOutcome(),
	})
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if _, err := mgr.Update(created.ID, Update{AutonomyOutcome: QuarantinedOutcome()}); err == nil {
		t.Fatal("status-less update replaced human_required outcome with quarantined")
	}
}

// TestManagerApply_ConcurrentCASOnlyOneWinner is a concurrency proof for
// "stale expected state/version returns a typed conflict and cannot
// overwrite newer state": N goroutines race Apply against the same task
// with the same ExpectedGeneration precondition. Exactly one may succeed;
// every other goroutine must observe a *ConflictError, never a silently
// dropped or double-applied write.
func TestManagerApply_ConcurrentCASOnlyOneWinner(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)

	created, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
		panic("unreachable")
	}

	const n = 16
	var wg sync.WaitGroup
	var successes int64
	var conflicts int64
	baseline := created.Generation

	for range n {
		wg.Go(func() {
			_, err := m.Apply(TransitionIntent{
				TaskID:             created.ID,
				ToStatus:           StatusInProgress,
				Actor:              "test.actor",
				ExpectedGeneration: &baseline,
			})
			var conflict *ConflictError
			switch {
			case err == nil:
				atomic.AddInt64(&successes, 1)
			case errors.As(err, &conflict):
				atomic.AddInt64(&conflicts, 1)
			default:
				t.Errorf("unexpected Apply error: %v", err)
			}
		})
	}
	wg.Wait()

	if successes != 1 {
		t.Fatalf("successes = %d, want exactly 1", successes)
	}
	if conflicts != n-1 {
		t.Fatalf("conflicts = %d, want %d", conflicts, n-1)
	}

	final, err := m.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
		panic("unreachable")
	}
	if final.Generation != baseline+1 {
		t.Fatalf("final generation = %d, want %d (exactly one applied write)", final.Generation, baseline+1)
	}
}

func TestApplyStatusEffect_RoutesThroughApply(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)

	created, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
		panic("unreachable")
	}
	if _, err := m.Update(created.ID, Update{Status: Ptr(StatusInReview)}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	updated, err := m.ApplyStatusEffect(created.ID, StatusEffect{
		Source:   "review.pr-monitor.closed-pr",
		ToStatus: StatusDone,
		Extra: Update{
			Outcome: Ptr("merged"),
		},
	})
	if err != nil {
		t.Fatalf("ApplyStatusEffect: %v", err)
		panic("unreachable")
	}
	if updated.Status != StatusDone {
		t.Fatalf("status = %q, want %q", updated.Status, StatusDone)
	}
	if !strings.HasPrefix(updated.EffectLog[0].ID.StepID, "external:") {
		t.Fatalf("step id = %q, want external:... prefix preserved by the Apply delegation", updated.EffectLog[0].ID.StepID)
	}
}
