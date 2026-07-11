// Package agentqueue implements a priority queue primitive for dispatching
// agent runs against tasks. It is a pure, self-contained package: no part of
// Sybra wires into it yet (see task 8bcbe346, umbrella #1844).
package agentqueue

import (
	"time"

	"github.com/Automaat/sybra/internal/dispatchorder"
	"github.com/Automaat/sybra/internal/task"
)

// priorityRank orders task.Priority values from lowest (PriorityNone) to
// highest (PriorityUrgent) urgency.
func priorityRank(p task.Priority) int {
	switch p {
	case task.PriorityUrgent:
		return 4
	case task.PriorityHigh:
		return 3
	case task.PriorityMedium:
		return 2
	case task.PriorityLow:
		return 1
	case task.PriorityNone:
		return 0
	default:
		return 0
	}
}

// statusFloor returns the minimum priority a task's pipeline status imposes,
// regardless of the item's own declared Priority.
func statusFloor(status task.Status) task.Priority {
	switch status {
	case task.StatusInReview, task.StatusReadyPR:
		return task.PriorityHigh
	case task.StatusReadyReview, task.StatusTesting:
		return task.PriorityMedium
	default:
		return task.PriorityNone
	}
}

// effectivePriority is the greater of an item's declared Priority and the
// floor its status imposes.
func effectivePriority(it Item) task.Priority {
	floor := statusFloor(it.Status)
	if priorityRank(it.Priority) >= priorityRank(floor) {
		return it.Priority
	}
	return floor
}

// bumpTier raises p by exactly one priority tier, capped at PriorityUrgent.
// Unrecognized values fold to PriorityLow (a one-tier bump from the bottom),
// mirroring priorityRank which ranks both PriorityNone and unknown values at 0
// — so an unknown priority is never silently promoted straight to the top tier.
func bumpTier(p task.Priority) task.Priority {
	switch p {
	case task.PriorityLow:
		return task.PriorityMedium
	case task.PriorityMedium:
		return task.PriorityHigh
	case task.PriorityHigh, task.PriorityUrgent:
		return task.PriorityUrgent
	default:
		return task.PriorityLow
	}
}

// Less reports whether a should be dispatched before b. It is the single,
// total, clock-free ordering for the queue: effectivePriority DESC, then
// Manual DESC, then dispatchPriorityRank ASC, then Enqueued ASC. It is
// exported so later steps of umbrella #1844 (e.g. ResumeStalled) can reuse
// the exact same ordering outside the heap.
func Less(a, b Item) bool {
	return lessByPriority(a, effectivePriority(a), b, effectivePriority(b))
}

// lessBoosted is Less with an age-based starvation boost applied to each
// item's effective priority. Used only by PopReady's snapshot re-rank so the
// heap's own invariant (maintained by the clock-free Less) is never violated
// in place. after <= 0 disables the boost, making lessBoosted equivalent to
// Less.
func lessBoosted(a, b Item, now time.Time, after time.Duration) bool {
	return lessByPriority(a, boostedPriority(a, now, after), b, boostedPriority(b, now, after))
}

func boostedPriority(it Item, now time.Time, after time.Duration) task.Priority {
	eff := effectivePriority(it)
	if after <= 0 {
		return eff
	}
	if now.Sub(it.Enqueued) >= after {
		return bumpTier(eff)
	}
	return eff
}

func lessByPriority(a Item, aPri task.Priority, b Item, bPri task.Priority) bool {
	if ar, br := priorityRank(aPri), priorityRank(bPri); ar != br {
		return ar > br // effectivePriority DESC
	}
	if a.Manual != b.Manual {
		return a.Manual // Manual DESC (true sorts first)
	}
	if ar, br := dispatchorder.Rank(string(a.Status)), dispatchorder.Rank(string(b.Status)); ar != br {
		return ar < br // dispatchorder.Rank ASC
	}
	return a.Enqueued.Before(b.Enqueued) // Enqueued ASC
}
