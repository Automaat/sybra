package task

import (
	"sync"

	"github.com/Automaat/synapse/internal/events"
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

func (m *Manager) lockFor(id string) *sync.Mutex {
	existing, _ := m.locks.LoadOrStore(id, &sync.Mutex{})
	mu, _ := existing.(*sync.Mutex)
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
	m.emitter.Emit(events.TaskCreated, t.FilePath)
	return t, nil
}

// Update applies field updates to a task and emits task:updated.
// Serializes with other Update/AddRun/UpdateRun/Delete calls for the same id.
func (m *Manager) Update(id string, updates map[string]any) (Task, error) {
	mu := m.lockFor(id)
	mu.Lock()
	defer mu.Unlock()

	var prevStatus string
	_, wantsStatus := updates["status"].(string)
	if wantsStatus {
		if prev, getErr := m.store.Get(id); getErr == nil {
			prevStatus = string(prev.Status)
		}
	}

	t, err := m.store.Update(id, updates)
	if err != nil {
		return t, err
	}
	m.emitter.Emit(events.TaskUpdated, t.FilePath)
	if wantsStatus && m.onStatusHook != nil {
		newStatus := string(t.Status)
		if newStatus != prevStatus {
			m.onStatusHook(id, prevStatus, newStatus)
		}
	}
	return t, nil
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
	m.emitter.Emit(events.TaskDeleted, t.FilePath)
	return nil
}

// AddRun appends an agent run to the task and emits task:updated.
func (m *Manager) AddRun(taskID string, run AgentRun) error {
	mu := m.lockFor(taskID)
	mu.Lock()
	defer mu.Unlock()
	if err := m.store.AddRun(taskID, run); err != nil {
		return err
	}
	t, err := m.store.Get(taskID)
	if err == nil {
		m.emitter.Emit(events.TaskUpdated, t.FilePath)
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
