package agentqueue

import (
	"container/heap"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

// Item is one entry in the queue: a task/role pair awaiting dispatch.
type Item struct {
	TaskID   string        `yaml:"task_id"`
	Role     string        `yaml:"role"`
	Priority task.Priority `yaml:"priority"`
	Status   task.Status   `yaml:"status"`
	Manual   bool          `yaml:"manual"`
	Enqueued time.Time     `yaml:"enqueued"`
}

// Options configures queue behavior. These are package-local for P0 — no
// global agent.queue.* config keys exist yet.
type Options struct {
	// MaxDepth caps the number of distinct TaskIDs the queue holds. 0 means
	// unbounded. Once full, Offer rejects genuinely new TaskIDs (re-offers
	// of an already-queued TaskID still refresh in place).
	MaxDepth int
	// StarvationBoostAfter, when > 0, bumps an item's effective priority by
	// one tier (capped at Urgent) once it has waited at least this long.
	// The boost is applied only inside PopReady's snapshot re-rank via an
	// injected clock; it never mutates the heap's own clock-free ordering.
	StarvationBoostAfter time.Duration
}

// queueHeap implements container/heap.Interface ordered by Less, and keeps
// index in sync with every position change so Queue can locate an item by
// TaskID in O(1) for heap.Fix / heap.Remove.
type queueHeap struct {
	items []Item
	index map[string]int
}

func (h *queueHeap) Len() int { return len(h.items) }

func (h *queueHeap) Less(i, j int) bool { return Less(h.items[i], h.items[j]) }

func (h *queueHeap) Swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.index[h.items[i].TaskID] = i
	h.index[h.items[j].TaskID] = j
}

func (h *queueHeap) Push(x any) {
	it, _ := x.(Item)
	h.index[it.TaskID] = len(h.items)
	h.items = append(h.items, it)
}

func (h *queueHeap) Pop() any {
	old := h.items
	n := len(old)
	it := old[n-1]
	h.items = old[:n-1]
	delete(h.index, it.TaskID)
	return it
}

// Queue is a heap-backed, mutex-guarded priority queue of Items, deduplicated
// by TaskID. Every mutation is mirrored to a write-through per-TaskID YAML
// file store; store failures are logged and never fail the in-memory
// operation (the store is a durability mirror, not the authority).
type Queue struct {
	mu    sync.Mutex
	h     *queueHeap
	store *store
	log   *slog.Logger
	now   func() time.Time
	opts  Options
}

// New builds a Queue backed by dir, loading any items previously persisted
// there. dir is used as-is — the caller is responsible for resolving it
// (this package never calls config.HomeDir()). The only error case is dir
// failing to be created; per-file load failures are logged and skipped, and
// the returned queue is still usable.
func New(dir string, opts Options, log *slog.Logger) (*Queue, error) {
	if log == nil {
		log = slog.Default()
	}
	st, err := newStore(dir)
	if err != nil {
		return nil, fmt.Errorf("agentqueue: init store: %w", err)
	}

	q := &Queue{
		h:     &queueHeap{index: map[string]int{}},
		store: st,
		log:   log,
		now:   time.Now,
		opts:  opts,
	}
	for _, it := range st.load(log) {
		heap.Push(q.h, it)
	}
	return q, nil
}

// Offer enqueues it if its TaskID is not already present, returning true.
// If TaskID is already queued, Offer refreshes that item's Priority, Status,
// Manual, and Role in place (re-ranking the heap) and returns false. An
// empty, path-separator-containing, or ".."-containing TaskID is rejected
// (logged, no mutation). A genuinely new TaskID is rejected when
// Options.MaxDepth > 0 and the queue is already at that depth.
func (q *Queue) Offer(it Item) bool {
	if !safeTaskID(it.TaskID) {
		q.log.Warn("agentqueue.offer.unsafe-task-id", "task_id", it.TaskID)
		return false
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if pos, ok := q.h.index[it.TaskID]; ok {
		existing := q.h.items[pos]
		it.Enqueued = existing.Enqueued
		q.h.items[pos] = it
		heap.Fix(q.h, pos)
		q.persist(it)
		return false
	}

	if q.opts.MaxDepth > 0 && len(q.h.items) >= q.opts.MaxDepth {
		q.log.Warn("agentqueue.offer.max-depth", "task_id", it.TaskID, "max_depth", q.opts.MaxDepth)
		return false
	}

	if it.Enqueued.IsZero() {
		it.Enqueued = q.now()
	}
	heap.Push(q.h, it)
	q.persist(it)
	return true
}

// Remove drops taskID from the queue and its store file, if present. An
// unsafe or absent TaskID is a no-op.
func (q *Queue) Remove(taskID string) {
	if !safeTaskID(taskID) {
		return
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	pos, ok := q.h.index[taskID]
	if !ok {
		return
	}
	heap.Remove(q.h, pos)
	q.deletePersist(taskID)
}

// Snapshot returns a copy of every queued item, sorted by Less, without
// mutating the queue.
func (q *Queue) Snapshot() []Item {
	q.mu.Lock()
	defer q.mu.Unlock()

	out := make([]Item, len(q.h.items))
	copy(out, q.h.items)
	slices.SortFunc(out, func(a, b Item) int {
		switch {
		case Less(a, b):
			return -1
		case Less(b, a):
			return 1
		default:
			return 0
		}
	})
	return out
}

// PopReady returns up to n of the most urgent items and removes them from
// the queue and its store. n <= 0 returns nil without mutation. If
// Options.StarvationBoostAfter is set, items that have waited at least that
// long are re-ranked one priority tier higher (capped at Urgent) for this
// pop only — the exported Less ordering itself stays clock-free.
func (q *Queue) PopReady(n int) []Item {
	if n <= 0 {
		return nil
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.h.items) == 0 {
		return nil
	}

	now := q.now()
	after := q.opts.StarvationBoostAfter
	ranked := make([]Item, len(q.h.items))
	copy(ranked, q.h.items)
	slices.SortFunc(ranked, func(a, b Item) int {
		switch {
		case lessBoosted(a, b, now, after):
			return -1
		case lessBoosted(b, a, now, after):
			return 1
		default:
			return 0
		}
	})

	if n > len(ranked) {
		n = len(ranked)
	}
	out := ranked[:n]
	for _, it := range out {
		pos := q.h.index[it.TaskID]
		heap.Remove(q.h, pos)
		q.deletePersist(it.TaskID)
	}
	return out
}

// Reconcile prunes queued items whose task is missing, terminal (done or
// cancelled), or already in-progress, using the fresh task.Status exists
// returns rather than the item's own possibly-stale Status.
func (q *Queue) Reconcile(exists func(taskID string) (task.Task, bool)) {
	q.mu.Lock()
	defer q.mu.Unlock()

	stale := make([]string, 0, len(q.h.items))
	for _, it := range q.h.items {
		t, ok := exists(it.TaskID)
		if !ok || task.IsTerminalStatus(t.Status) || t.Status == task.StatusInProgress {
			stale = append(stale, it.TaskID)
		}
	}

	for _, id := range stale {
		pos, ok := q.h.index[id]
		if !ok {
			continue
		}
		heap.Remove(q.h, pos)
		q.deletePersist(id)
	}
}

// persist writes it to the store and logs (non-fatally) on failure. Caller
// must hold q.mu.
func (q *Queue) persist(it Item) {
	if err := q.store.put(it); err != nil {
		q.log.Warn("agentqueue.store.put-failed", "task_id", it.TaskID, "err", err)
	}
}

// deletePersist removes taskID's store file and logs (non-fatally) on
// failure. Caller must hold q.mu.
func (q *Queue) deletePersist(taskID string) {
	if err := q.store.del(taskID); err != nil {
		q.log.Warn("agentqueue.store.del-failed", "task_id", taskID, "err", err)
	}
}
