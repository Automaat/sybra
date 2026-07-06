package task

import (
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/metrics"
)

// EventEmitter publishes task lifecycle events.
type EventEmitter interface {
	Emit(event string, data any)
}

// EmitterFunc adapts a function into an EventEmitter.
type EmitterFunc func(event string, data any)

func (f EmitterFunc) Emit(event string, data any) { f(event, data) }

type noopEmitter struct{}

func (noopEmitter) Emit(string, any) {}

// NoopEmitter returns an EventEmitter that discards events.
func NoopEmitter() EventEmitter { return noopEmitter{} }

// StatusChangeHook is invoked synchronously on every status transition
// that happens through Manager.Update. Empty `from` means previous state
// could not be read.
type StatusChangeHook func(taskID, from, to string)

// DeleteHook is invoked after a task is successfully deleted.
type DeleteHook func(taskID string)

// Manager is the single entrypoint for task mutations. It wraps Store with
// per-task mutual exclusion and emits events on mutations.
type Manager struct {
	store        *Store
	emitter      EventEmitter
	locks        sync.Map // string -> *sync.Mutex
	onStatusHook StatusChangeHook
	onDeleteHook DeleteHook

	// firedMu/firedStatus tracks the most recent status value that has
	// triggered onStatusHook. OnExternalUpdate uses it to dedupe repeated
	// file events for the same status, while still detecting genuine
	// cross-process transitions (where firedStatus is stale or unset).
	firedMu     sync.RWMutex
	firedStatus map[string]string
}

// SetStatusChangeHook registers a callback fired on every status transition.
// Passing nil disables the hook.
func (m *Manager) SetStatusChangeHook(h StatusChangeHook) { m.onStatusHook = h }

// SetDeleteHook registers a callback fired after a task is successfully deleted.
// Passing nil disables the hook.
func (m *Manager) SetDeleteHook(h DeleteHook) { m.onDeleteHook = h }

// NewManager constructs a Manager over the given Store. If emitter is nil,
// events are discarded.
func NewManager(store *Store, emitter EventEmitter) *Manager {
	if emitter == nil {
		emitter = NoopEmitter()
	}
	return &Manager{store: store, emitter: emitter}
}

// Store returns the underlying Store. Use for operations not covered by Manager.
func (m *Manager) Store() *Store { return m.store }

// OnExternalUpdate is invoked by the file watcher when a task file is
// modified outside this Manager — typically by `sybra-cli` running in
// a separate process. It invalidates the store cache and, when the
// on-disk status differs from the value that last triggered the hook,
// fires the registered status-change hook so workflow steps using
// wait_for_status can advance.
//
// Without this, status flips written by out-of-process tools never
// reach engine.HandleStatusChange and interactive plan agents leave
// their workflow stranded on the plan step.
//
// Takes the same per-task lock as UpdateFn/AddRunWithStatus around the
// read+dedupe-check+record sequence. Without it, an in-process status write
// (e.g. UpdateTaskStatus from a workflow step) races the file watcher that
// its own write wakes: the watcher can read the new status and record+fire
// the hook itself before the writer's own recordFiredStatus call runs,
// double-firing the SAME transition from two goroutines — one of which then
// races a legitimate cascade dispatch (e.g. OnWorkflowComplete) into
// double-dispatching the same successor workflow.
func (m *Manager) OnExternalUpdate(path string) {
	base := filepath.Base(path)
	if IsSidecarFile(base) {
		m.store.InvalidatePath(path)
		return
	}
	if !strings.HasSuffix(base, ".md") {
		return
	}
	id := strings.TrimSuffix(base, ".md")
	if id == "" {
		return
	}

	m.store.InvalidatePath(path)

	if m.onStatusHook == nil {
		return
	}

	mu := m.lockFor(id)
	// TryLock, not Lock: every Manager write path now releases lockFor(id)
	// before emitting (see AppendBody/Delete/UpdateFn/AddRunWithStatus), so
	// this lock is free by the time an in-process emit reaches here. If a
	// future change reintroduces an emit-under-lock, blocking here would
	// re-wedge the caller's goroutine on itself (and, since this same path
	// serves the fsnotify watcher, freeze external-update processing for
	// every task). Skipping this one dedupe-check instead of deadlocking is
	// safe: the writer's own state is already durable, and the next genuine
	// external file event still resolves the status via a fresh TryLock.
	if !mu.TryLock() {
		slog.Default().Warn("task.OnExternalUpdate.lock_busy", "id", id)
		return
	}
	// Only Status is needed here; use the parse-only read to avoid the
	// per-call sidecar dir scan on every external file change.
	t, err := m.store.read(id)
	if err != nil {
		mu.Unlock()
		return
	}
	newStatus := string(t.Status)

	prev, ok := m.lastFiredStatus(id)
	if ok && prev == newStatus {
		mu.Unlock()
		return
	}
	m.recordFiredStatus(id, newStatus)
	mu.Unlock()

	m.onStatusHook(id, prev, newStatus)
}

func (m *Manager) recordFiredStatus(id, status string) {
	m.firedMu.Lock()
	defer m.firedMu.Unlock()
	if m.firedStatus == nil {
		m.firedStatus = make(map[string]string)
	}
	m.firedStatus[id] = status
}

func (m *Manager) lastFiredStatus(id string) (string, bool) {
	m.firedMu.RLock()
	defer m.firedMu.RUnlock()
	if m.firedStatus == nil {
		return "", false
	}
	s, ok := m.firedStatus[id]
	return s, ok
}

func (m *Manager) forgetFiredStatus(id string) {
	m.firedMu.Lock()
	defer m.firedMu.Unlock()
	delete(m.firedStatus, id)
}

// Comments returns the underlying CommentStore.
func (m *Manager) Comments() *CommentStore { return m.store.Comments() }

// Plans returns the underlying PlanStore.
func (m *Manager) Plans() *PlanStore { return m.store.Plans() }

// PlanDrafts returns the underlying PlanDraftStore.
func (m *Manager) PlanDrafts() *PlanDraftStore { return m.store.PlanDrafts() }

func (m *Manager) lockFor(id string) *sync.Mutex {
	existing, _ := m.locks.LoadOrStore(id, &sync.Mutex{})
	mu, ok := existing.(*sync.Mutex)
	if !ok {
		mu = &sync.Mutex{}
	}
	return mu
}

// List returns all tasks (lock-free).
func (m *Manager) List() ([]Task, error) { return m.store.List() }

// Get returns a single task by ID (lock-free).
func (m *Manager) Get(id string) (Task, error) { return m.store.Get(id) }

// Create persists a new task and emits task:created.
func (m *Manager) Create(title, body, mode string) (Task, error) {
	t, err := m.store.Create(title, body, mode)
	if err != nil {
		return t, err
	}
	metrics.TaskCreated()
	m.recordFiredStatus(t.ID, string(t.Status))
	m.emitter.Emit(events.TaskCreated, t.FilePath)
	return t, nil
}

// CreateFull persists a new task with initial field overrides applied atomically
// before the first emit, ensuring file-watchers see a complete task from the start.
func (m *Manager) CreateFull(title, body, mode string, init Update) (Task, error) {
	t, err := m.store.CreateFull(title, body, mode, init)
	if err != nil {
		return t, err
	}
	metrics.TaskCreated()
	m.recordFiredStatus(t.ID, string(t.Status))
	m.emitter.Emit(events.TaskCreated, t.FilePath)
	return t, nil
}

// CreateChat persists a synthetic chat task and emits task:created.
func (m *Manager) CreateChat(projectID string) (Task, error) {
	t, err := m.store.CreateChat(projectID)
	if err != nil {
		return t, err
	}
	metrics.TaskCreated()
	m.emitter.Emit(events.TaskCreated, t.FilePath)
	return t, nil
}

// Update applies field updates to a task and emits task:updated.
// Serializes with other Update/AddRun/UpdateRun/Delete calls for the same id.
//
// Note on hook ordering: the status-change hook is invoked *after* the
// per-task mutex is released. Hooks commonly call back into the task
// manager (e.g. the workflow engine advancing a step, which writes the
// workflow field via taskAdapter.SetWorkflow → Manager.Update). Calling
// the hook while still holding the lock would deadlock that re-entry.
func (m *Manager) Update(id string, u Update) (Task, error) {
	return m.UpdateFn(id, func(Task) (Update, error) { return u, nil })
}

// UpdateFn atomically reads the current task and applies the Update computed
// by fn, under the same per-task lock — for read-modify-write callers (e.g. a
// tag merge gated on the current status) that would otherwise race with a
// concurrent Update for the same id between their read and their write.
func (m *Manager) UpdateFn(id string, fn func(cur Task) (Update, error)) (Task, error) {
	mu := m.lockFor(id)
	mu.Lock()

	cur, err := m.store.Get(id)
	if err != nil {
		mu.Unlock()
		return cur, err
	}
	u, err := fn(cur)
	if err != nil {
		mu.Unlock()
		return cur, err
	}

	t, prev, err := m.store.UpdateWithPrev(id, u)
	if err != nil {
		mu.Unlock()
		return t, err
	}
	var (
		fireHook            bool
		prevStatus, newStat string
	)
	if u.Status != nil && m.onStatusHook != nil {
		prevStatus = string(prev)
		newStat = string(t.Status)
		fireHook = newStat != prevStatus
	}
	// Record the dedupe entry before releasing the per-task lock, still
	// covering the same critical section as the write above. Otherwise the
	// file watcher this write wakes (OnExternalUpdate, serialized on the same
	// lock) can read the new status and win the dedupe race before this
	// goroutine gets to record it, double-firing the hook.
	if fireHook {
		m.recordFiredStatus(id, newStat)
	}
	mu.Unlock()

	if fireHook {
		m.onStatusHook(id, prevStatus, newStat)
	}
	metrics.TaskUpdated()
	m.emitter.Emit(events.TaskUpdated, t.FilePath)
	return t, nil
}

// UpdateMap converts raw to a typed Update and applies it.
// Returns an error on unknown keys or wrong value types.
func (m *Manager) UpdateMap(id string, raw map[string]any) (Task, error) {
	u, err := UpdateFromMap(raw)
	if err != nil {
		return Task{}, err
	}
	return m.Update(id, u)
}

// AppendBody appends markdown to a task body under the per-task mutation lock.
func (m *Manager) AppendBody(id, content string) (Task, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return m.Get(id)
	}
	mu := m.lockFor(id)
	mu.Lock()
	t, err := m.store.Get(id)
	if err != nil {
		mu.Unlock()
		return Task{}, err
	}
	body := strings.TrimRight(t.Body, "\n")
	if body != "" {
		body += "\n\n"
	}
	body += content + "\n"
	t, _, err = m.store.UpdateWithPrev(id, Update{Body: &body})
	mu.Unlock()
	if err != nil {
		return t, err
	}
	// Emit after releasing the lock — see UpdateFn/AddRunWithStatus for why:
	// this Manager is its own emitter's target (app.go routes task:updated
	// back into OnExternalUpdate), so firing while still holding the lock
	// self-deadlocks the goroutine on its own non-reentrant mutex.
	metrics.TaskUpdated()
	m.emitter.Emit(events.TaskUpdated, t.FilePath)
	return t, nil
}

// Delete removes a task and emits task:deleted.
func (m *Manager) Delete(id string) error {
	mu := m.lockFor(id)
	mu.Lock()
	t, err := m.store.Get(id)
	if err != nil {
		mu.Unlock()
		return err
	}
	if err := m.store.Delete(id); err != nil {
		mu.Unlock()
		return err
	}
	m.locks.Delete(id)
	m.forgetFiredStatus(id)
	mu.Unlock()
	// Emit/hook after releasing the lock — see UpdateFn/AddRunWithStatus for
	// why: this Manager is its own emitter's target (app.go routes
	// task:updated back into OnExternalUpdate), so firing while still holding
	// the lock self-deadlocks the goroutine on its own non-reentrant mutex.
	metrics.TaskDeleted()
	m.emitter.Emit(events.TaskDeleted, t.FilePath)
	if m.onDeleteHook != nil {
		m.onDeleteHook(id)
	}
	return nil
}

// AddRun appends an agent run to the task and emits task:updated.
func (m *Manager) AddRun(taskID string, run AgentRun) error {
	return m.AddRunWithStatus(taskID, run, nil)
}

// AddRunWithStatus appends an agent run and optionally changes task status in one write.
func (m *Manager) AddRunWithStatus(taskID string, run AgentRun, status *Status) error {
	mu := m.lockFor(taskID)
	mu.Lock()
	var prevStatus string
	if status != nil {
		if prev, getErr := m.store.Get(taskID); getErr == nil {
			prevStatus = string(prev.Status)
		}
	}
	if err := m.store.AddRunWithStatus(taskID, run, status); err != nil {
		mu.Unlock()
		return err
	}
	t, err := m.store.Get(taskID)
	var (
		fireHook  bool
		newStatus string
	)
	if status != nil && m.onStatusHook != nil && err == nil {
		newStatus = string(t.Status)
		fireHook = newStatus != prevStatus
	}
	// Record before unlocking — see UpdateFn for why this must stay inside
	// the per-task critical section that also covers the write.
	if fireHook {
		m.recordFiredStatus(taskID, newStatus)
	}
	mu.Unlock()
	// Emit after releasing the lock, same as UpdateFn: OnExternalUpdate takes
	// the same per-task lock, and this Manager is its own emitter's target
	// (app.go routes task:updated back into OnExternalUpdate), so firing
	// while still holding the lock self-deadlocks the goroutine on its own
	// non-reentrant mutex.
	if err == nil {
		m.emitter.Emit(events.TaskUpdated, t.FilePath)
	}
	if fireHook {
		m.onStatusHook(taskID, prevStatus, newStatus)
	}
	return nil
}

// UpdateRun updates fields on a specific agent run and emits task:updated.
func (m *Manager) UpdateRun(taskID, agentID string, patch RunPatch) error {
	mu := m.lockFor(taskID)
	mu.Lock()
	if err := m.store.UpdateRun(taskID, agentID, patch); err != nil {
		mu.Unlock()
		return err
	}
	t, err := m.store.Get(taskID)
	mu.Unlock()
	// Emit after releasing the lock — see AddRunWithStatus for why.
	if err == nil {
		m.emitter.Emit(events.TaskUpdated, t.FilePath)
	}
	return nil
}
