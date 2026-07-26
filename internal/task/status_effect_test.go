package task

import (
	"testing"

	"github.com/Automaat/sybra/internal/events"
)

func TestManagerApplyStatusEffect_RecordsEffectAndFiresHookOnce(t *testing.T) {
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

	updated, err := m.ApplyStatusEffect(created.ID, StatusEffect{
		Source: "review.pr-monitor.closed-pr",
		Update: Update{
			Status:       Ptr(StatusDone),
			StatusReason: Ptr(""),
			Outcome:      Ptr("merged"),
		},
	})
	if err != nil {
		t.Fatalf("ApplyStatusEffect: %v", err)
	}
	if updated.Status != StatusDone {
		t.Fatalf("status = %q, want %q", updated.Status, StatusDone)
	}
	if updated.Outcome != "merged" {
		t.Fatalf("outcome = %q, want merged", updated.Outcome)
	}
	if len(updated.EffectLog) != 1 {
		t.Fatalf("effect log len = %d, want 1", len(updated.EffectLog))
	}
	if updated.EffectLog[0].CompletedAt == nil {
		t.Fatal("completed_at not recorded")
	}
	if updated.EffectLog[0].ID.Generation != created.Generation {
		t.Fatalf("effect generation = %d, want %d", updated.EffectLog[0].ID.Generation, created.Generation)
	}
	if len(transitions) != 1 || transitions[0] != "todo->done" {
		t.Fatalf("transitions = %v, want [todo->done]", transitions)
	}

	names := emitter.names()
	if len(names) != 2 || names[1] != events.TaskUpdated {
		t.Fatalf("events = %v, want create+update", names)
	}
}

func TestManagerApplyStatusEffect_DedupesCompletedEffect(t *testing.T) {
	t.Parallel()
	m, emitter := newTestManager(t)

	created, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	eff := StatusEffect{
		Source: "review.pr-monitor.closed-pr",
		Update: Update{
			Status:       Ptr(StatusDone),
			StatusReason: Ptr(""),
			Outcome:      Ptr("merged"),
		},
	}
	first, err := m.ApplyStatusEffect(created.ID, eff)
	if err != nil {
		t.Fatalf("first ApplyStatusEffect: %v", err)
	}
	second, err := m.ApplyStatusEffect(created.ID, eff)
	if err != nil {
		t.Fatalf("second ApplyStatusEffect: %v", err)
	}
	if len(second.EffectLog) != 1 {
		t.Fatalf("effect log len after replay = %d, want 1", len(second.EffectLog))
	}
	if second.Generation != first.Generation {
		t.Fatalf("generation after replay = %d, want unchanged %d", second.Generation, first.Generation)
	}

	names := emitter.names()
	if len(names) != 2 {
		t.Fatalf("events = %v, want only create+first update", names)
	}
}
