package agentqueue

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func mustQueue(t *testing.T, opts Options) *Queue {
	t.Helper()
	q, err := New(t.TempDir(), opts, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return q
}

func TestLess_Matrix(t *testing.T) {
	base := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	older := base.Add(-time.Hour)

	tests := []struct {
		name string
		a, b Item
		want bool
	}{
		{
			name: "higher declared priority wins",
			a:    Item{TaskID: "a", Priority: task.PriorityHigh, Enqueued: base},
			b:    Item{TaskID: "b", Priority: task.PriorityLow, Enqueued: base},
			want: true,
		},
		{
			name: "status floor beats lower declared priority",
			a:    Item{TaskID: "a", Priority: task.PriorityNone, Status: task.StatusInReview, Enqueued: base},
			b:    Item{TaskID: "b", Priority: task.PriorityMedium, Enqueued: base},
			want: true,
		},
		{
			name: "declared priority beats lower status floor",
			a:    Item{TaskID: "a", Priority: task.PriorityUrgent, Status: task.StatusNew, Enqueued: base},
			b:    Item{TaskID: "b", Priority: task.PriorityNone, Status: task.StatusTesting, Enqueued: base},
			want: true,
		},
		{
			name: "equal effective priority: manual breaks tie",
			a:    Item{TaskID: "a", Priority: task.PriorityMedium, Manual: true, Enqueued: base},
			b:    Item{TaskID: "b", Priority: task.PriorityMedium, Manual: false, Enqueued: base},
			want: true,
		},
		{
			name: "equal priority and manual: dispatchPriorityRank breaks tie",
			a:    Item{TaskID: "a", Priority: task.PriorityMedium, Status: task.StatusInReview, Enqueued: base},
			b:    Item{TaskID: "b", Priority: task.PriorityMedium, Status: task.StatusPlanning, Enqueued: base},
			want: true,
		},
		{
			name: "fully tied except age: older enqueued first",
			a:    Item{TaskID: "a", Priority: task.PriorityMedium, Enqueued: older},
			b:    Item{TaskID: "b", Priority: task.PriorityMedium, Enqueued: base},
			want: true,
		},
		{
			name: "identical items: neither less than the other",
			a:    Item{TaskID: "a", Priority: task.PriorityMedium, Enqueued: base},
			b:    Item{TaskID: "a", Priority: task.PriorityMedium, Enqueued: base},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Less(tt.a, tt.b); got != tt.want {
				t.Errorf("Less(a,b) = %v, want %v", got, tt.want)
			}
			if tt.want && Less(tt.b, tt.a) {
				t.Errorf("Less is not antisymmetric for %q", tt.name)
			}
		})
	}
}

func TestQueue_OfferDedupRefreshesInPlace(t *testing.T) {
	q := mustQueue(t, Options{})

	if !q.Offer(Item{TaskID: "t1", Role: "implementation", Priority: task.PriorityLow, Status: task.StatusNew}) {
		t.Fatal("first Offer of new TaskID should return true")
	}
	if q.Offer(Item{TaskID: "t1", Role: "implementation", Priority: task.PriorityLow, Status: task.StatusNew}) {
		t.Fatal("re-offer of existing TaskID should return false")
	}

	// Re-offer refreshes Priority, Status, Manual, and Role in place.
	if q.Offer(Item{TaskID: "t1", Role: "review", Priority: task.PriorityUrgent, Status: task.StatusInReview, Manual: true}) {
		t.Fatal("re-offer should return false even when fields change")
	}

	snap := q.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 item after dedup re-offers, got %d", len(snap))
	}
	got := snap[0]
	if got.Role != "review" || got.Priority != task.PriorityUrgent || got.Status != task.StatusInReview || !got.Manual {
		t.Errorf("re-offer did not refresh fields: %+v", got)
	}
}

func TestQueue_OfferPreservesEnqueuedOnReOffer(t *testing.T) {
	q := mustQueue(t, Options{})
	q.Offer(Item{TaskID: "t1", Priority: task.PriorityLow})
	first := q.Snapshot()[0].Enqueued

	q.Offer(Item{TaskID: "t1", Priority: task.PriorityHigh})
	second := q.Snapshot()[0].Enqueued

	if !first.Equal(second) {
		t.Errorf("re-offer changed Enqueued: first=%v second=%v", first, second)
	}
}

func TestQueue_PopReadySlotBound(t *testing.T) {
	q := mustQueue(t, Options{})
	priorities := []task.Priority{task.PriorityLow, task.PriorityUrgent, task.PriorityMedium, task.PriorityHigh, task.PriorityNone}
	for i, p := range priorities {
		q.Offer(Item{TaskID: string(rune('a' + i)), Priority: p})
	}

	if got := q.PopReady(0); got != nil {
		t.Errorf("PopReady(0) = %v, want nil", got)
	}
	if got := q.PopReady(-1); got != nil {
		t.Errorf("PopReady(-1) = %v, want nil", got)
	}

	got := q.PopReady(2)
	if len(got) != 2 {
		t.Fatalf("PopReady(2) returned %d items, want 2", len(got))
	}
	if got[0].Priority != task.PriorityUrgent || got[1].Priority != task.PriorityHigh {
		t.Errorf("PopReady(2) order = %v, want [urgent, high]", got)
	}

	remaining := q.Snapshot()
	if len(remaining) != 3 {
		t.Fatalf("expected 3 items remaining, got %d", len(remaining))
	}

	// n larger than the queue depth returns everything left, no panic.
	rest := q.PopReady(100)
	if len(rest) != 3 {
		t.Fatalf("PopReady(100) returned %d items, want 3", len(rest))
	}
	if len(q.Snapshot()) != 0 {
		t.Fatalf("queue should be empty after draining, got %d", len(q.Snapshot()))
	}
}

func TestQueue_MaxDepthBackpressure(t *testing.T) {
	q := mustQueue(t, Options{MaxDepth: 2})

	if !q.Offer(Item{TaskID: "t1"}) {
		t.Fatal("first offer under MaxDepth should succeed")
	}
	if !q.Offer(Item{TaskID: "t2"}) {
		t.Fatal("second offer reaching MaxDepth should succeed")
	}
	if q.Offer(Item{TaskID: "t3"}) {
		t.Fatal("third offer over MaxDepth should be rejected")
	}
	if len(q.Snapshot()) != 2 {
		t.Fatalf("expected depth to stay at 2, got %d", len(q.Snapshot()))
	}

	// Re-offering an already-queued TaskID still refreshes even at MaxDepth.
	if q.Offer(Item{TaskID: "t1", Priority: task.PriorityUrgent}) {
		t.Fatal("re-offer at MaxDepth should still return false (not newly added)")
	}
	if q.Snapshot()[0].Priority != task.PriorityUrgent {
		t.Fatal("re-offer at MaxDepth should still refresh the item")
	}
}

func TestQueue_DepthSnapshot(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		opts Options
		seed []Item
		want DepthSnapshot
	}{
		{
			name: "empty queue",
			want: DepthSnapshot{
				Depth:                0,
				TopEffectivePriority: task.PriorityNone,
			},
		},
		{
			name: "saturated queue",
			opts: Options{MaxDepth: 2},
			seed: []Item{
				{TaskID: "low", Priority: task.PriorityLow, Enqueued: now},
				{TaskID: "high", Priority: task.PriorityHigh, Enqueued: now.Add(time.Minute)},
			},
			want: DepthSnapshot{
				Depth:                2,
				TopEffectivePriority: task.PriorityHigh,
			},
		},
		{
			name: "status floor promotes top effective priority",
			seed: []Item{
				{TaskID: "promoted", Priority: task.PriorityNone, Status: task.StatusInReview, Enqueued: now},
				{TaskID: "medium", Priority: task.PriorityMedium, Enqueued: now.Add(time.Minute)},
			},
			want: DepthSnapshot{
				Depth:                2,
				TopEffectivePriority: task.PriorityHigh,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := mustQueue(t, tt.opts)
			for _, it := range tt.seed {
				if added := q.Offer(it); !added {
					t.Fatalf("Offer(%q) returned false, want true", it.TaskID)
				}
			}
			if got := q.DepthSnapshot(); got != tt.want {
				t.Fatalf("DepthSnapshot() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestQueue_StarvationBoost(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	q := mustQueue(t, Options{StarvationBoostAfter: time.Hour})
	q.now = func() time.Time { return now }

	q.Offer(Item{TaskID: "old", Priority: task.PriorityNone, Enqueued: now.Add(-2 * time.Hour)})
	q.Offer(Item{TaskID: "new", Priority: task.PriorityLow, Enqueued: now})

	// Without the boost, "new" (Low) outranks "old" (None). With the boost,
	// "old" has waited past StarvationBoostAfter and gets bumped to Low,
	// tying on priority, then winning on Enqueued (older first).
	got := q.PopReady(1)
	if len(got) != 1 || got[0].TaskID != "old" {
		t.Fatalf("PopReady(1) = %v, want [old] (starvation-boosted)", got)
	}

	// Less itself must stay clock-free: without boosting, "new" ranks first.
	rest := q.Snapshot()
	if len(rest) != 1 || rest[0].TaskID != "new" {
		t.Fatalf("Snapshot() = %v, want [new] remaining", rest)
	}
}

func TestQueue_StarvationBoostDisabledWhenZero(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	q := mustQueue(t, Options{}) // StarvationBoostAfter defaults to 0
	q.now = func() time.Time { return now }

	q.Offer(Item{TaskID: "old", Priority: task.PriorityNone, Enqueued: now.Add(-100 * time.Hour)})
	q.Offer(Item{TaskID: "new", Priority: task.PriorityLow, Enqueued: now})

	got := q.PopReady(1)
	if len(got) != 1 || got[0].TaskID != "new" {
		t.Fatalf("PopReady(1) = %v, want [new] (no boost configured)", got)
	}
}

func TestBumpTier(t *testing.T) {
	tests := []struct {
		in   task.Priority
		want task.Priority
	}{
		{task.PriorityNone, task.PriorityLow},
		{task.PriorityLow, task.PriorityMedium},
		{task.PriorityMedium, task.PriorityHigh},
		{task.PriorityHigh, task.PriorityUrgent},
		{task.PriorityUrgent, task.PriorityUrgent},
		// Unknown values fold to Low, mirroring priorityRank (which ranks them
		// at 0) rather than jumping straight to the top tier.
		{task.Priority("bogus"), task.PriorityLow},
	}
	for _, tt := range tests {
		if got := bumpTier(tt.in); got != tt.want {
			t.Errorf("bumpTier(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestQueue_Reconcile(t *testing.T) {
	q := mustQueue(t, Options{})
	q.Offer(Item{TaskID: "missing"})
	q.Offer(Item{TaskID: "done"})
	q.Offer(Item{TaskID: "cancelled"})
	q.Offer(Item{TaskID: "in-progress"})
	q.Offer(Item{TaskID: "keep"})

	exists := func(id string) (task.Task, bool) {
		switch id {
		case "missing":
			return task.Task{}, false
		case "done":
			return task.Task{ID: id, Status: task.StatusDone}, true
		case "cancelled":
			return task.Task{ID: id, Status: task.StatusCancelled}, true
		case "in-progress":
			return task.Task{ID: id, Status: task.StatusInProgress}, true
		case "keep":
			return task.Task{ID: id, Status: task.StatusInReview}, true
		default:
			return task.Task{}, false
		}
	}

	q.Reconcile(exists)

	snap := q.Snapshot()
	if len(snap) != 1 || snap[0].TaskID != "keep" {
		t.Fatalf("Reconcile left %v, want only [keep]", snap)
	}

	// Store files for pruned items must be gone too.
	dir := q.store.dir
	for _, id := range []string{"missing", "done", "cancelled", "in-progress"} {
		if _, err := os.Stat(filepath.Join(dir, id+".yaml")); !os.IsNotExist(err) {
			t.Errorf("expected store file for %q to be removed, stat err=%v", id, err)
		}
	}
}

func TestQueue_RemoveUnknownIsNoop(t *testing.T) {
	q := mustQueue(t, Options{})
	q.Offer(Item{TaskID: "t1"})
	q.Remove("does-not-exist")
	if len(q.Snapshot()) != 1 {
		t.Fatalf("Remove of unknown TaskID mutated the queue")
	}
}

func TestQueue_TaskIDPathSafety(t *testing.T) {
	q := mustQueue(t, Options{})

	unsafe := []string{"", "..", "../escape", "a/b", "a\\b", "a/../b"}
	for _, id := range unsafe {
		if q.Offer(Item{TaskID: id}) {
			t.Errorf("Offer(%q) should be rejected", id)
		}
		// Remove of an unsafe id must not panic and must remain a no-op.
		q.Remove(id)
	}
	if len(q.Snapshot()) != 0 {
		t.Fatalf("unsafe TaskIDs should never be queued, got %v", q.Snapshot())
	}
}

func TestQueue_ConcurrentOfferAndPopReady(t *testing.T) {
	q := mustQueue(t, Options{})
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			q.Offer(Item{TaskID: fmt.Sprintf("task-%d", i), Priority: task.PriorityMedium})
		})
	}
	for range 20 {
		wg.Go(func() {
			q.PopReady(1)
		})
	}
	wg.Wait()
}
