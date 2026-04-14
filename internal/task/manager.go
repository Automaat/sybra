package task

import (
	"sync"

	"github.com/Automaat/synapse/internal/events"
	"github.com/Automaat/synapse/internal/metrics"
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

// Manager is the single entrypoint for task mutations. It wraps Store with
// per-task mutual exclusion and emits events on mutations.
type Manager struct {
	store        *Store
	emitter      EventEmitter
	locks        sync.Map // string -> *sync.Mutex
	onStatusHook StatusChangeHook
}

// SetStatusChangeHook registers a callback fired on every status transition.
// Passing nil disables the hook.
func (m *Manager) SetStatusChangeHook(h StatusChangeHook) { m.onStatusHook = h }

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

// Comments returns the underlying CommentStore.
func (m *Manager) Comments() *CommentStore { return m.store.Comments() }

// Plans returns the underlying PlanStore.
func (m *Manager) Plans() *PlanStore { return m.store.Plans() }

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
	mu := m.lockFor(id)
	mu.Lock()

	var prevStatus string
	if u.Status != nil {
		if prev, getErr := m.store.Get(id); getErr == nil {
			prevStatus = string(prev.Status)
		}
	}

	t, err := m.store.Update(id, u)
	if err != nil {
		mu.Unlock()
		return t, err
	}
	metrics.TaskUpdated()
	m.emitter.Emit(events.TaskUpdated, t.FilePath)

	var (
		fireHook  bool
		newStatus string
	)
	if u.Status != nil && m.onStatusHook != nil {
		newStatus = string(t.Status)
		fireHook = newStatus != prevStatus
	}
	mu.Unlock()

	if fireHook {
		m.onStatusHook(id, prevStatus, newStatus)
	}
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

// Delete removes a task and emits task:deleted.
func (m *Manager) Delete(id string) error {
	mu := m.lockFor(id)
	mu.Lock()
	defer mu.Unlock()
	t, err := m.store.Get(id)
	if err != nil {
		return err
	}
	if err := m.store.Delete(id); err != nil {
		return err
	}
	m.locks.Delete(id)
	metrics.TaskDeleted()
	m.emitter.Emit(events.TaskDeleted, t.FilePath)
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
	if err == nil {
		m.emitter.Emit(events.TaskUpdated, t.FilePath)
	}
	var (
		fireHook  bool
		newStatus string
	)
	if status != nil && m.onStatusHook != nil && err == nil {
		newStatus = string(t.Status)
		fireHook = newStatus != prevStatus
	}
	mu.Unlock()
	if fireHook {
		m.onStatusHook(taskID, prevStatus, newStatus)
	}
	return nil
}

// UpdateRun updates fields on a specific agent run and emits task:updated.
func (m *Manager) UpdateRun(taskID, agentID string, updates map[string]any) error {
	mu := m.lockFor(taskID)
	mu.Lock()
	defer mu.Unlock()
	if err := m.store.UpdateRun(taskID, agentID, updates); err != nil {
		return err
	}
	t, err := m.store.Get(taskID)
	if err == nil {
		m.emitter.Emit(events.TaskUpdated, t.FilePath)
	}
	return nil
}
