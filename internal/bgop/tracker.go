package bgop

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/Automaat/synapse/internal/events"
	"github.com/google/uuid"
)

// completionTTL is how long completed/failed ops are kept in memory and on disk.
const completionTTL = 5 * time.Minute

// Tracker manages in-memory background operations and persists them to disk so
// the frontend can restore state after an app restart.
type Tracker struct {
	mu       sync.RWMutex
	ops      map[string]*Operation
	emit     func(string, any)
	diskPath string
}

// NewTracker creates a Tracker that broadcasts events via emit and persists to diskPath.
func NewTracker(emit func(string, any), diskPath string) *Tracker {
	return &Tracker{
		ops:      make(map[string]*Operation),
		emit:     emit,
		diskPath: diskPath,
	}
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
		StartedAt: time.Now().UTC(),
	}
	t.mu.Lock()
	t.ops[id] = op
	snapshot := *op
	t.mu.Unlock()

	t.emit(events.BgOpStarted, snapshot)
	t.saveToDisk()
	return id
}

// UpdatePhase updates the current phase text of a running operation.
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
	op.CompletedAt = time.Now().UTC()
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
	op.CompletedAt = time.Now().UTC()
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
	cutoff := time.Now().Add(-completionTTL)
	var out []Operation
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
	data, err := os.ReadFile(t.diskPath)
	if err != nil {
		return
	}
	var ops []Operation
	if err := json.Unmarshal(data, &ops); err != nil {
		return
	}
	cutoff := time.Now().Add(-completionTTL)
	t.mu.Lock()
	for i := range ops {
		op := ops[i]
		if op.Status != StatusRunning && op.CompletedAt.Before(cutoff) {
			continue
		}
		// Running ops at shutdown are now stale — mark failed.
		if op.Status == StatusRunning {
			op.Status = StatusFailed
			op.Error = "interrupted by restart"
			op.CompletedAt = time.Now().UTC()
		}
		t.ops[op.ID] = &op
	}
	t.mu.Unlock()
}

func (t *Tracker) saveToDisk() {
	t.mu.RLock()
	ops := make([]Operation, 0, len(t.ops))
	for _, op := range t.ops {
		ops = append(ops, *op)
	}
	t.mu.RUnlock()

	data, err := json.MarshalIndent(ops, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(t.diskPath, data, 0o644)
}
