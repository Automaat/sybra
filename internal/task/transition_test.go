package task

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Automaat/sybra/internal/events"
)

func TestManagerApply_Success(t *testing.T) {
	t.Parallel()
	m, emitter := newTestManager(t)

	var transitions []string
	m.SetStatusChangeHook(func(_ string, from, to string) {
		transitions = append(transitions, from+"->"+to)
	})

	created, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	result, err := m.Apply(TransitionIntent{
		TaskID:   created.ID,
		ToStatus: StatusInProgress,
		Actor:    "test.actor",
		Extra:    Update{StatusReason: Ptr("started")},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
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
	m.SetStatusChangeHook(func(string, string, string) { fireCount++ })

	created, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
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
	}
	if !first.Applied {
		t.Fatal("first Applied = false, want true")
	}

	second, err := m.Apply(intent)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
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
	}

	intent := TransitionIntent{
		TaskID:         created.ID,
		ToStatus:       StatusHumanRequired,
		Actor:          "test.actor",
		IdempotencyKey: "retry-once",
	}
	first, err := m.Apply(intent)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}

	if _, err := m.Update(created.ID, Update{Status: Ptr(StatusInProgress)}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	second, err := m.Apply(intent)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
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
	}

	updated, err := m.ApplyStatusEffect(created.ID, StatusEffect{
		Source: "review.pr-monitor.closed-pr",
		Update: Update{
			Status:  Ptr(StatusDone),
			Outcome: Ptr("merged"),
		},
	})
	if err != nil {
		t.Fatalf("ApplyStatusEffect: %v", err)
	}
	if updated.Status != StatusDone {
		t.Fatalf("status = %q, want %q", updated.Status, StatusDone)
	}
	if !strings.HasPrefix(updated.EffectLog[0].ID.StepID, "external:") {
		t.Fatalf("step id = %q, want external:... prefix preserved by the Apply delegation", updated.EffectLog[0].ID.StepID)
	}
}
