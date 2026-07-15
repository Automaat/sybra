package evaluation

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
)

func TestPhaseOf(t *testing.T) {
	cases := map[string]string{
		"new":            PhaseQueued,
		"todo":           PhaseQueued,
		"planning":       PhasePlanning,
		"plan-review":    PhasePlanning,
		"in-progress":    PhaseImplementing,
		"testing":        PhaseTesting,
		"ready-review":   PhaseReview,
		"in-review":      PhaseReview,
		"ready-pr":       PhaseReview,
		"blocked":        PhaseWaiting,
		"human-required": PhaseWaiting,
		"done":           "",
		"cancelled":      "",
		"":               "", // missing status is not a phase
		"made-up-status": PhaseOther,
	}
	for status, want := range cases {
		if got := phaseOf(status); got != want {
			t.Errorf("phaseOf(%q) = %q, want %q", status, got, want)
		}
	}
}

// at returns base + the given hours, so scenario timelines read clearly.
func ev(typ, taskID string, ts time.Time, data map[string]any) audit.Event {
	return audit.Event{Type: typ, TaskID: taskID, Timestamp: ts, Data: data}
}

func statusChange(taskID string, ts time.Time, from, to string) audit.Event {
	return ev(audit.EventTaskStatusChanged, taskID, ts, map[string]any{"from": from, "to": to})
}

func TestComputePhaseDurations(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(h float64) time.Time { return base.Add(time.Duration(h * float64(time.Hour))) }

	events := []audit.Event{
		// Task A: queued 1h, planning 2h, implementing 3h, review 1h → total 7h.
		ev(audit.EventTaskCreated, "A", at(0), nil),
		statusChange("A", at(1), "todo", "planning"),
		statusChange("A", at(3), "planning", "in-progress"),
		statusChange("A", at(6), "in-progress", "in-review"),
		statusChange("A", at(7), "in-review", "done"),
		ev(audit.EventTaskLanded, "A", at(7), map[string]any{"outcome": "merged"}),

		// Task B: queued 0.5h, planning 1.5h, implementing 3h, testing 1h, review 3h → total 9h.
		ev(audit.EventTaskCreated, "B", at(0), nil),
		statusChange("B", at(0.5), "todo", "planning"),
		statusChange("B", at(2), "planning", "in-progress"),
		statusChange("B", at(5), "in-progress", "testing"),
		statusChange("B", at(6), "testing", "in-review"),
		statusChange("B", at(9), "in-review", "done"),
		ev(audit.EventTaskLanded, "B", at(9), map[string]any{"outcome": "merged"}),

		// Task C: landed OUTSIDE the cohort window → must be excluded.
		ev(audit.EventTaskCreated, "C", at(-100), nil),
		statusChange("C", at(-99), "todo", "in-progress"),
		ev(audit.EventTaskLanded, "C", at(-98), map[string]any{"outcome": "merged"}),
	}

	since := at(-24)
	until := at(24)
	rep := ComputePhaseDurations(events, since, until, 10)

	if rep.Cohort != 2 {
		t.Fatalf("cohort = %d, want 2 (C is out of window)", rep.Cohort)
	}

	totals := map[string]float64{}
	counts := map[string]int{}
	for _, p := range rep.Phases {
		totals[p.Phase] = p.TotalH
		counts[p.Phase] = p.Count
	}
	wantTotals := map[string]float64{
		PhaseQueued:       1.5, // 1 + 0.5
		PhasePlanning:     3.5, // 2 + 1.5
		PhaseImplementing: 6,   // 3 + 3
		PhaseTesting:      1,   // B only
		PhaseReview:       4,   // 1 + 3
	}
	for ph, want := range wantTotals {
		if got := totals[ph]; !approxEq(got, want) {
			t.Errorf("phase %q total = %.3f, want %.3f", ph, got, want)
		}
	}
	if counts[PhaseTesting] != 1 {
		t.Errorf("testing count = %d, want 1 (only B was tested)", counts[PhaseTesting])
	}
	if counts[PhaseImplementing] != 2 {
		t.Errorf("implementing count = %d, want 2", counts[PhaseImplementing])
	}

	// Phases must be emitted in canonical lifecycle order.
	wantOrder := []string{PhaseQueued, PhasePlanning, PhaseImplementing, PhaseTesting, PhaseReview}
	if len(rep.Phases) != len(wantOrder) {
		t.Fatalf("got %d phases, want %d", len(rep.Phases), len(wantOrder))
	}
	for i, ph := range wantOrder {
		if rep.Phases[i].Phase != ph {
			t.Errorf("phase[%d] = %q, want %q", i, rep.Phases[i].Phase, ph)
		}
	}

	// Slowest is sorted by total lead time desc; B (9h) before A (7h).
	if len(rep.Slowest) != 2 || rep.Slowest[0].TaskID != "B" || rep.Slowest[1].TaskID != "A" {
		t.Fatalf("slowest = %+v, want [B, A]", rep.Slowest)
	}
	if !approxEq(rep.Slowest[0].TotalH, 9) {
		t.Errorf("B total = %.3f, want 9", rep.Slowest[0].TotalH)
	}

	// slowestN <= 0 means no per-task detail, even though the cohort is non-empty.
	for _, n := range []int{0, -1} {
		if r := ComputePhaseDurations(events, since, until, n); len(r.Slowest) != 0 {
			t.Errorf("slowestN=%d: got %d detail rows, want 0", n, len(r.Slowest))
		}
	}
}

// TestComputePhaseDurations_EmptyFromStatus: a first transition with from=="" (a
// first-observation status flip) must not book the creation→first-flip gap to
// the "other" phase.
func TestComputePhaseDurations_EmptyFromStatus(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(h float64) time.Time { return base.Add(time.Duration(h * float64(time.Hour))) }
	events := []audit.Event{
		ev(audit.EventTaskCreated, "A", at(0), nil),
		statusChange("A", at(2), "", "in-progress"), // first observation: from is empty
		statusChange("A", at(5), "in-progress", "in-review"),
		statusChange("A", at(6), "in-review", "done"),
		ev(audit.EventTaskLanded, "A", at(6), map[string]any{"outcome": "merged"}),
	}
	rep := ComputePhaseDurations(events, at(-24), at(24), 5)
	for _, p := range rep.Phases {
		if p.Phase == PhaseOther {
			t.Errorf("empty from-status leaked into %q (%.3fh); want it skipped", PhaseOther, p.TotalH)
		}
	}
}

// TestComputePhaseDurations_NoCreatedEvent: without a creation timestamp the
// initial dwell is unknown, so queued is not counted, but later phases still are.
func TestComputePhaseDurations_NoCreatedEvent(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(h float64) time.Time { return base.Add(time.Duration(h * float64(time.Hour))) }
	events := []audit.Event{
		statusChange("A", at(1), "todo", "in-progress"),
		statusChange("A", at(4), "in-progress", "in-review"),
		statusChange("A", at(5), "in-review", "done"),
		ev(audit.EventTaskLanded, "A", at(5), map[string]any{"outcome": "merged"}),
	}
	rep := ComputePhaseDurations(events, at(-24), at(24), 0)
	if rep.Cohort != 1 {
		t.Fatalf("cohort = %d, want 1", rep.Cohort)
	}
	totals := map[string]float64{}
	for _, p := range rep.Phases {
		totals[p.Phase] = p.TotalH
	}
	if _, ok := totals[PhaseQueued]; ok {
		t.Errorf("queued must be absent without a creation timestamp, got %.3f", totals[PhaseQueued])
	}
	if !approxEq(totals[PhaseImplementing], 3) {
		t.Errorf("implementing = %.3f, want 3", totals[PhaseImplementing])
	}
	if !approxEq(totals[PhaseReview], 1) {
		t.Errorf("review = %.3f, want 1", totals[PhaseReview])
	}
}

func approxEq(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}
