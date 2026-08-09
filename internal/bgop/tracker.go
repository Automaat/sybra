package bgop

import (
	"log/slog"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/clock"
	"github.com/Automaat/sybra/internal/events"
	"github.com/google/uuid"
)

// completionTTL is how long completed/failed ops are kept in memory and on disk.
const completionTTL = 5 * time.Minute

// Tracker manages in-memory background operations and persists them to disk so
// the frontend can restore state after an app restart.
type Tracker struct {
	mu     sync.RWMutex
	ops    map[string]*Operation
	emit   func(string, any)
	store  Persistence
	logger *slog.Logger

	// clock is read without the mutex, including from methods that already
	// hold it. Set it once at construction time, before the tracker is
	// shared — SetClock is for test setup, not live reconfiguration.
	clock clock.Clock
}

// NewTracker creates a Tracker that broadcasts events via emit and persists to diskPath.
// A nil logger falls back to slog.Default().
func NewTracker(emit func(string, any), store Persistence, logger *slog.Logger) *Tracker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Tracker{
		ops:    make(map[string]*Operation),
		emit:   emit,
		store:  store,
		logger: logger,
		clock:  clock.System{},
	}
}

// SetClock injects the clock driving the completion-TTL window, so a test can
// step past the TTL instead of sleeping. Call it during setup, before the
// tracker is used: the field is read without the mutex.
func (t *Tracker) SetClock(c clock.Clock) {
	t.clock = clock.Or(c)
}

func (t *Tracker) now() time.Time {
	return clock.Or(t.clock).Now()
}

// Start records a new running operation and returns its ID.
func (t *Tracker) Start(opType Type, label, projectID, taskID string) string {
	id := uuid.NewString()
	op := &Operation{
		ID:        id,
		Type:      opType,
		Label:     label,
		Status:    StatusRunning,
		ProjectID: projectID,
		TaskID:    taskID,
		StartedAt: t.now().UTC(),
	}
	t.mu.Lock()
	t.ops[id] = op
	snapshot := *op
	t.mu.Unlock()

	t.emit(events.BgOpStarted, snapshot)
	t.saveToDisk()
	return id
}

// UpdatePhase updates the current phase text of a running operation. Phase is
// in-memory/event-only — it is not persisted to disk, so a restart loses the
// last phase text but keeps the operation's lifecycle status (see Start,
// Complete, Fail).
func (t *Tracker) UpdatePhase(id, phase string) {
	t.mu.Lock()
	op, ok := t.ops[id]
	if !ok {
		t.mu.Unlock()
		return
	}
	op.Phase = phase
	snapshot := *op
	t.mu.Unlock()

	t.emit(events.BgOpProgress, snapshot)
}

// Complete marks an operation as done.
func (t *Tracker) Complete(id string) {
	t.mu.Lock()
	op, ok := t.ops[id]
	if !ok {
		t.mu.Unlock()
		return
	}
	op.Status = StatusDone
	op.CompletedAt = t.now().UTC()
	op.Phase = ""
	snapshot := *op
	t.mu.Unlock()

	t.emit(events.BgOpCompleted, snapshot)
	t.saveToDisk()
}

// Fail marks an operation as failed with the given error.
func (t *Tracker) Fail(id string, err error) {
	t.mu.Lock()
	op, ok := t.ops[id]
	if !ok {
		t.mu.Unlock()
		return
	}
	op.Status = StatusFailed
	op.CompletedAt = t.now().UTC()
	op.Phase = ""
	if err != nil {
		op.Error = err.Error()
	}
	snapshot := *op
	t.mu.Unlock()

	t.emit(events.BgOpFailed, snapshot)
	t.saveToDisk()
}

// List returns active operations and completed/failed ones within completionTTL.
func (t *Tracker) List() []Operation {
	t.mu.RLock()
	defer t.mu.RUnlock()
	cutoff := t.now().Add(-completionTTL)
	out := make([]Operation, 0, len(t.ops))
	for _, op := range t.ops {
		if op.Status != StatusRunning && op.CompletedAt.Before(cutoff) {
			continue
		}
		out = append(out, *op)
	}
	return out
}

// LoadFromDisk restores persisted operations on startup. Running ops that
// survived a restart are marked failed (they cannot be resumed). Ops older
// than completionTTL are discarded.
func (t *Tracker) LoadFromDisk() {
	if t.store == nil {
		return
	}
	ops, err := t.store.Load()
	if err != nil {
		t.logger.Error("bgop.load", "err", err)
		return
	}
	cutoff := t.now().Add(-completionTTL)
	now := t.now().UTC()
	changed := false
	t.mu.Lock()
	for i := range ops {
		op := ops[i]
		if op.Status != StatusRunning && op.CompletedAt.Before(cutoff) {
			changed = true
			continue
		}
		// Running ops at shutdown are now stale — mark failed.
		if op.Status == StatusRunning {
			op.Status = StatusFailed
			op.Error = "interrupted by restart"
			op.CompletedAt = now
			op.Phase = ""
			changed = true
		}
		t.ops[op.ID] = &op
	}
	t.mu.Unlock()

	if changed {
		t.saveToDisk()
	}
}

func (t *Tracker) saveToDisk() {
	cutoff := t.now().Add(-completionTTL)
	t.mu.Lock()
	ops := make([]Operation, 0, len(t.ops))
	for _, op := range t.ops {
		if op.Status != StatusRunning && op.CompletedAt.Before(cutoff) {
			delete(t.ops, op.ID)
			continue
		}
		snapshot := *op
		snapshot.Phase = ""
		ops = append(ops, snapshot)
	}
	t.mu.Unlock()

	if t.store == nil {
		return
	}
	if err := t.store.Save(ops); err != nil {
		t.logger.Error("bgop.save", "err", err)
	}
}
