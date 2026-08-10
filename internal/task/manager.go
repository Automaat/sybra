package task

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/fsutil"
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
type StatusChangeHook func(taskID, from, to string, snapshot Task)

// DeleteHook is invoked after a task is successfully deleted.
type DeleteHook func(taskID string)

// Manager is the single entrypoint for task mutations. It wraps Store with
// per-task mutual exclusion and emits events on mutations.
//
// store and persist are separate seams on purpose. persist is where task
// CRUD actually lands — the file Store or a database adapter, chosen once at
// construction — but store stays the file Store unconditionally, because
// Comments/Plans/PlanDrafts, the trash-generation history, the leader-
// follower mirror's direct sidecar writes (Store()), and the file-watcher
// concerns (OnExternalUpdate/ProbeMutationTransport/MutationTransportIdentity)
// are not part of Persistence yet — see the follow-up issue linked from
// #3268. A Manager always has both regardless of which Persistence backs it.
type Manager struct {
	store        *Store
	persist      Persistence
	emitter      EventEmitter
	locks        fsutil.KeyedLocker
	onStatusHook StatusChangeHook
	onDeleteHook []DeleteHook

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
// Passing nil clears every registered delete hook.
func (m *Manager) SetDeleteHook(h DeleteHook) {
	if h == nil {
		m.onDeleteHook = nil
		return
	}
	m.onDeleteHook = append(m.onDeleteHook, h)
}

// NewManager constructs a Manager over the given Store, using it as both the
// file-specific store and the Persistence CRUD runs against. If emitter is
// nil, events are discarded.
func NewManager(store *Store, emitter EventEmitter) *Manager {
	return NewManagerWithPersistence(store, newFileBackend(store), emitter)
}

// NewManagerWithPersistence is NewManager for a caller that wants task CRUD
// to run against a different Persistence than store's own file backend —
// e.g. a database adapter, when database.backend selects one. store is still
// required and still backs Comments/Plans/PlanDrafts/trash/the file-watcher
// methods regardless.
func NewManagerWithPersistence(store *Store, persist Persistence, emitter EventEmitter) *Manager {
	if emitter == nil {
		emitter = NoopEmitter()
	}
	return &Manager{store: store, persist: persist, emitter: emitter}
}

// requireActor refuses a blank actor rather than silently recording an
// anonymous change — the same rule TransitionIntent's Actor already
// enforces, applied here so a mutation that goes through Manager directly
// (not via a status transition) gets the same guarantee.
func requireActor(actor string) error {
	if strings.TrimSpace(actor) == "" {
		return fmt.Errorf("task: actor is required")
	}
	return nil
}

// eventPath returns the string emitted as a task:created/updated/deleted
// event's data. The file backend always populates FilePath; the DB backend
// never does (no file exists), so a synthetic "<id>.md" stands in — every
// consumer of this event (taskEventEmitter, OnExternalUpdate,
// maybeStartWorkflowForExternalTask) only ever extracts the id from the
// basename, never reads the string as a real path.
func eventPath(t Task) string {
	if t.FilePath != "" {
		return t.FilePath
	}
	return t.ID + ".md"
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

	unlock := m.lock(id)
	// Only Status is needed here; use the parse-only read to avoid the
	// per-call sidecar dir scan on every external file change.
	t, err := m.store.read(id)
	if err != nil {
		unlock()
		return
	}
	newStatus := string(t.Status)

	prev, ok := m.lastFiredStatus(id)
	if ok && prev == newStatus {
		unlock()
		return
	}
	m.recordFiredStatus(id, newStatus)
	unlock()

	m.onStatusHook(id, prev, newStatus, t)
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

// Plans returns the underlying plan sidecar store.
func (m *Manager) Plans() *PlanningSidecarStore { return m.store.Plans() }

// PlanDrafts returns the underlying PlanDraftStore.
func (m *Manager) PlanDrafts() *PlanDraftStore { return m.store.PlanDrafts() }

func (m *Manager) lock(id string) func() {
	return m.locks.LockLocal(id)
}

// List returns all tasks (lock-free).
func (m *Manager) List() ([]Task, error) { return m.persist.List() }

// Get returns a single task by ID (lock-free).
func (m *Manager) Get(id string) (Task, error) { return m.persist.Get(id) }

// ProbeMutationTransport verifies that the task exists and that the same
// process-local plus cross-process lock used by every Manager mutation can be
// acquired. Its temporary write verifies real directory mutability without
// changing task contents/timestamps or emitting lifecycle events.
//
// Existence is checked through persist, the backend actually receiving this task's mutations. The lock and directory-write checks stay on store's task directory unconditionally, since Comments/Plans/sidecars still write there for every task regardless of backend, but the per-task flock is skipped for a DB-backed task (empty FilePath, no file at rest), because store.lockTask opens the task path with the create flag and would otherwise leave a stray empty <id>.md next to a task that has no file, and the DB backend's mutation atomicity already comes from its own row lock rather than this flock.
func (m *Manager) ProbeMutationTransport(id string) error {
	t, err := m.persist.Get(id)
	if err != nil {
		return err
	}
	if t.FilePath != "" {
		unlock, err := m.store.lockTask(id)
		if err != nil {
			return err
		}
		defer unlock()
	}
	probe, err := os.CreateTemp(m.store.Dir(), ".sybra-task-mutation-probe-")
	if err != nil {
		return err
	}
	name := probe.Name()
	if closeErr := probe.Close(); closeErr != nil {
		_ = os.Remove(name)
		return closeErr
	}
	return os.Remove(name)
}

// MutationTransportIdentity returns read-only cache evidence for the task
// store path used by ProbeMutationTransport. Permission or replacement
// changes therefore invalidate a run-environment certificate without exposing
// Store mutation authority to the certifier.
//
// A DB-backed task (empty FilePath) has no per-task file to fingerprint, unlike a task directory the DB connection is established once at process start rather than a filesystem path an attacker can swap out from under a running agent, so its identity is the directory fingerprint alone, which still matters because Comments/Plans/sidecars keep writing there under every backend.
func (m *Manager) MutationTransportIdentity(id string) (string, error) {
	t, err := m.persist.Get(id)
	if err != nil {
		return "", err
	}
	resolvedDir, err := filepath.EvalSymlinks(m.store.Dir())
	if err != nil {
		return "", err
	}
	dirInfo, err := os.Stat(resolvedDir)
	if err != nil {
		return "", err
	}
	// Do not include directory mtime/size: ProbeMutationTransport's own create-remove write changes them. Path + permission mode still detects the route changes certification needs to invalidate (replacement through a symlink and writable/read-only transitions).
	parts := []string{fmt.Sprintf("%s|%s", resolvedDir, dirInfo.Mode())}
	if t.FilePath == "" {
		return strings.Join(parts, "\x00"), nil
	}
	resolvedTask, err := filepath.EvalSymlinks(t.FilePath)
	if err != nil {
		return "", err
	}
	taskInfo, err := os.Stat(resolvedTask)
	if err != nil {
		return "", err
	}
	// Omit the task file's mtime and size for the same reason as the directory above: a run patches its own task constantly, so an ordinary status or body write would otherwise read as the mutation route having been replaced underneath a healthy run.
	parts = append(parts, fmt.Sprintf("%s|%s", resolvedTask, taskInfo.Mode()))
	return strings.Join(parts, "\x00"), nil
}

// Create persists a new task and emits task:created, attributed to
// LegacyActor. Prefer CreateBy for a caller that can name a real actor.
func (m *Manager) Create(title, body, mode string) (Task, error) {
	return m.CreateBy(title, body, mode, LegacyActor)
}

// CreateBy is Create naming actor, refused if blank.
func (m *Manager) CreateBy(title, body, mode, actor string) (Task, error) {
	return m.CreateFullBy(title, body, mode, actor, Update{})
}

// CreateFull persists a new task with initial field overrides applied atomically
// before the first emit, ensuring file-watchers see a complete task from the start.
// Attributed to LegacyActor; prefer CreateFullBy for a caller that can name a real actor.
func (m *Manager) CreateFull(title, body, mode string, init Update) (Task, error) {
	return m.CreateFullBy(title, body, mode, LegacyActor, init)
}

// CreateFullBy is CreateFull naming actor, refused if blank.
func (m *Manager) CreateFullBy(title, body, mode, actor string, init Update) (Task, error) {
	if err := requireActor(actor); err != nil {
		return Task{}, err
	}
	if err := validateTypedAutonomyEvidence(init); err != nil {
		return Task{}, fmt.Errorf("task: create-full: %w", err)
	}
	if init.Status != nil {
		if err := validateHumanRequiredTransition(StatusNew, *init.Status, init); err != nil {
			return Task{}, fmt.Errorf("task: create-full: %w", err)
		}
	}
	built, err := buildNewTask(title, body, mode, init)
	if err != nil {
		return Task{}, err
	}
	t, err := mintAndCreateBy(m.persist, built, actor)
	if err != nil {
		return t, err
	}
	metrics.TaskCreated()
	m.recordFiredStatus(t.ID, string(t.Status))
	m.emitter.Emit(events.TaskCreated, eventPath(t))
	return t, nil
}

// CreateWithStatus is CreateFull for a caller that needs the new task minted
// directly at a non-default status (e.g. an issue importer creating straight
// into "todo", an umbrella tracker minted at "in-progress"). status is a
// named parameter rather than a field on extra for the same reason
// TransitionIntent takes ToStatus separately from Extra: a caller setting the
// initial status has no prior state to protect with the transition API's
// precondition/idempotency machinery (there is no task to Get yet), but it
// should still never reach for Update.Status directly — extra.Status must be
// nil, keeping that field production-write-free everywhere outside this
// package. See #2726. Attributed to LegacyActor; prefer CreateWithStatusBy.
func (m *Manager) CreateWithStatus(title, body, mode string, status Status, extra Update) (Task, error) {
	return m.CreateWithStatusBy(title, body, mode, LegacyActor, status, extra)
}

// CreateWithStatusBy is CreateWithStatus naming actor, refused if blank.
func (m *Manager) CreateWithStatusBy(title, body, mode, actor string, status Status, extra Update) (Task, error) {
	if extra.Status != nil {
		return Task{}, fmt.Errorf("task: create-with-status: extra.status must be nil; pass status instead")
	}
	if err := validateHumanRequiredTransition(StatusNew, status, extra); err != nil {
		return Task{}, fmt.Errorf("task: create-with-status: %w", err)
	}
	extra.Status = &status
	return m.CreateFullBy(title, body, mode, actor, extra)
}

// Put writes a fully-formed task verbatim (upsert by ID) and drives the same
// lifecycle side-effects an in-process create/update would, so the pushed task
// dispatches through the normal workflow. It is the leader-follower execution
// mirror's write path: a first push emits task:created (todo/new tasks then
// dispatch via the external-task path), a subsequent push emits task:updated,
// and any status change — including a fresh push that lands directly at a stage
// like ready-review/testing/ready-pr — fires the status hook so stage dispatch
// runs. The fired-status dedupe is recorded under the per-task lock so the file
// watcher this write wakes cannot double-fire the hook. The returned bool
// reports whether the task was newly created (vs an in-place update). See
// Store.Put.
func (m *Manager) Put(t Task) (Task, bool, error) {
	return m.PutBy(t, LegacyActor)
}

// PutBy is Put naming actor, refused if blank.
func (m *Manager) PutBy(t Task, actor string) (Task, bool, error) {
	if err := requireActor(actor); err != nil {
		return Task{}, false, err
	}
	unlock := m.lock(t.ID)

	prev, getErr := m.persist.Get(t.ID)
	existed := getErr == nil
	saved, err := m.persist.PutBy(t, actor, nil)
	if err != nil {
		unlock()
		return saved, false, err
	}

	prevStatus := ""
	if existed {
		prevStatus = string(prev.Status)
	}
	newStatus := string(saved.Status)
	fireHook := m.onStatusHook != nil && newStatus != prevStatus
	if fireHook {
		m.recordFiredStatus(saved.ID, newStatus)
	}
	unlock()

	if existed {
		metrics.TaskUpdated()
		m.emitter.Emit(events.TaskUpdated, eventPath(saved))
	} else {
		metrics.TaskCreated()
		m.emitter.Emit(events.TaskCreated, eventPath(saved))
	}
	if fireHook {
		m.onStatusHook(saved.ID, prevStatus, newStatus, saved)
	}
	return saved, !existed, nil
}

// PutFn atomically reads an existing task and writes the fully-formed
// replacement computed by fn. The callback runs under both Manager's per-task
// mutex and the backend's own atomicity for the same cycle, making it safe to
// merge a stale long-running operation with the latest leader-side edit
// immediately before the write. It preserves Put's lifecycle events and
// status-hook behaviour. Attributed to LegacyActor; prefer PutFnBy.
func (m *Manager) PutFn(id string, fn func(cur Task) (Task, error)) (Task, bool, error) {
	return m.PutFnBy(id, LegacyActor, fn)
}

// PutFnBy is PutFn naming actor, refused if blank.
func (m *Manager) PutFnBy(id, actor string, fn func(cur Task) (Task, error)) (Task, bool, error) {
	if err := requireActor(actor); err != nil {
		return Task{}, false, err
	}
	unlock := m.lock(id)

	// prevStatus comes from inside PutFnBy's own locked read (the cur it
	// hands fn), not a separate pre-fetch: a pre-fetch would race a
	// cross-process writer landing between it and the locked read that
	// actually produces saved, reporting a prevStatus that was already stale
	// by the time this call's own write happened.
	var prevStatus string
	saved, err := m.persist.PutFnBy(id, actor, func(cur Task) (Task, []string, error) {
		prevStatus = string(cur.Status)
		next, ferr := fn(cur)
		return next, nil, ferr
	})
	if err != nil {
		unlock()
		return saved, false, err
	}

	newStatus := string(saved.Status)
	fireHook := m.onStatusHook != nil && newStatus != prevStatus
	if fireHook {
		m.recordFiredStatus(saved.ID, newStatus)
	}
	unlock()

	metrics.TaskUpdated()
	m.emitter.Emit(events.TaskUpdated, eventPath(saved))
	if fireHook {
		m.onStatusHook(saved.ID, prevStatus, newStatus, saved)
	}
	return saved, false, nil
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
	return m.UpdateBy(id, LegacyActor, u)
}

// UpdateBy is Update naming actor, refused if blank.
func (m *Manager) UpdateBy(id, actor string, u Update) (Task, error) {
	return m.UpdateFnBy(id, actor, func(Task) (Update, error) { return u, nil })
}

// UpdateFn atomically reads the current task and applies the Update computed
// by fn, under the same per-task lock — for read-modify-write callers (e.g. a
// tag merge gated on the current status) that would otherwise race with a
// concurrent Update for the same id between their read and their write.
// Attributed to LegacyActor; prefer UpdateFnBy.
func (m *Manager) UpdateFn(id string, fn func(cur Task) (Update, error)) (Task, error) {
	return m.UpdateFnBy(id, LegacyActor, fn)
}

// UpdateFnBy is UpdateFn naming actor, refused if blank.
func (m *Manager) UpdateFnBy(id, actor string, fn func(cur Task) (Update, error)) (Task, error) {
	if err := requireActor(actor); err != nil {
		return Task{}, err
	}
	unlock := m.lock(id)

	// statusSet is captured from inside compute, which runs against UpdateFieldsBy's own locked read — see PutFnBy for why a separate pre-fetch would race a cross-process writer instead.
	var statusSet bool
	t, prev, err := m.persist.UpdateFieldsBy(id, actor, func(cur Task) (Update, error) {
		u, err := fn(cur)
		if err != nil {
			return Update{}, err
		}
		statusSet = u.Status != nil
		if u.Status != nil {
			if err := validateHumanRequiredTransition(cur.Status, *u.Status, u); err != nil {
				return Update{}, fmt.Errorf("task: update: %w", err)
			}
		} else if cur.Status == StatusHumanRequired && u.AutonomyOutcome != nil {
			if err := validateHumanRequiredTransition(cur.Status, cur.Status, u); err != nil {
				return Update{}, fmt.Errorf("task: update: %w", err)
			}
		}
		return u, nil
	})
	if err != nil {
		unlock()
		return t, err
	}
	var (
		fireHook            bool
		prevStatus, newStat string
	)
	if statusSet && m.onStatusHook != nil {
		prevStatus = string(prev)
		newStat = string(t.Status)
		fireHook = newStat != prevStatus
	}
	// Record the dedupe entry before releasing the per-task lock, still covering the same critical section as the write above — otherwise the file watcher this write wakes (OnExternalUpdate, serialized on the same lock) can read the new status and win the dedupe race before this goroutine gets to record it, double-firing the hook.
	if fireHook {
		m.recordFiredStatus(id, newStat)
	}
	unlock()

	if fireHook {
		m.onStatusHook(id, prevStatus, newStat, t)
	}
	metrics.TaskUpdated()
	m.emitter.Emit(events.TaskUpdated, eventPath(t))
	return t, nil
}

// UpdateMap converts raw to a typed Update and applies it.
// Returns an error on unknown keys or wrong value types.
func (m *Manager) UpdateMap(id string, raw map[string]any) (Task, error) {
	return m.UpdateMapBy(id, LegacyActor, raw)
}

// UpdateMapBy is UpdateMap naming actor, refused if blank.
func (m *Manager) UpdateMapBy(id, actor string, raw map[string]any) (Task, error) {
	u, err := UpdateFromMap(raw)
	if err != nil {
		return Task{}, err
	}
	// UpdateMap is the raw operator boundary used by CLI/Wails callers. Attach
	// explicit operator evidence here so a direct status request cannot create
	// an untyped human-required record even when the caller only knows the
	// legacy status/status_reason vocabulary.
	if u.Status != nil && *u.Status == StatusHumanRequired && u.Escalation == nil {
		message := "operator moved task to human-required"
		if u.StatusReason != nil && strings.TrimSpace(*u.StatusReason) != "" {
			message = *u.StatusReason
		}
		u.Escalation = OperatorDecisionEvidence("operator.raw_status_change", message)
		u.AutonomyOutcome = HumanRequiredOutcome()
	}
	return m.UpdateFnBy(id, actor, func(cur Task) (Update, error) {
		if u.Status != nil {
			if err := validateHumanRequiredTransition(cur.Status, *u.Status, u); err != nil {
				return Update{}, fmt.Errorf("task: update-map: %w", err)
			}
		}
		return u, nil
	})
}

// AppendBody appends markdown to a task body under the per-task mutation lock.
// Attributed to LegacyActor; prefer AppendBodyBy.
func (m *Manager) AppendBody(id, content string) (Task, error) {
	return m.AppendBodyBy(id, LegacyActor, content)
}

// AppendBodyBy is AppendBody naming actor, refused if blank.
func (m *Manager) AppendBodyBy(id, actor, content string) (Task, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return m.Get(id)
	}
	return m.UpdateFnBy(id, actor, func(cur Task) (Update, error) {
		body := strings.TrimRight(cur.Body, "\n")
		if body != "" {
			body += "\n\n"
		}
		body += content + "\n"
		return Update{Body: &body}, nil
	})
}

// Touch bumps a task's updated_at and emits task:updated without changing any
// field. Used to wake the file watcher so out-of-process writers (e.g. a CLI
// appending a progress entry) can signal the desktop app to refetch.
// Attributed to LegacyActor; prefer TouchBy.
func (m *Manager) Touch(id string) (Task, error) {
	return m.UpdateBy(id, LegacyActor, Update{})
}

// TouchBy is Touch naming actor, refused if blank.
func (m *Manager) TouchBy(id, actor string) (Task, error) {
	return m.UpdateBy(id, actor, Update{})
}

// Delete removes a task and emits task:deleted. Attributed to LegacyActor;
// prefer DeleteBy.
func (m *Manager) Delete(id string) error {
	return m.DeleteBy(id, LegacyActor)
}

// DeleteBy is Delete naming actor, refused if blank. The emit and delete
// hooks fire after unlock deliberately: this Manager is its own emitter's
// target, so firing while still holding the per-task mutex self-deadlocks
// the goroutine on OnExternalUpdate's re-entrant lock attempt.
func (m *Manager) DeleteBy(id, actor string) error {
	if err := requireActor(actor); err != nil {
		return err
	}
	unlock := m.lock(id)
	t, err := m.persist.Get(id)
	if err != nil {
		unlock()
		return err
	}
	if err := m.persist.DeleteBy(id, actor); err != nil {
		unlock()
		return err
	}
	m.forgetFiredStatus(id)
	unlock()
	metrics.TaskDeleted()
	m.emitter.Emit(events.TaskDeleted, eventPath(t))
	for _, hook := range m.onDeleteHook {
		hook(id)
	}
	return nil
}

// RestoreFromTrash restores id and emits task:created — restored tasks
// re-enter the system the same way a fresh Create does, so watchers (file
// watcher, workflow engine) treat it as a new task rather than an update to
// one that "already existed". Attributed to LegacyActor; prefer RestoreBy.
func (m *Manager) RestoreFromTrash(id string) (Task, error) {
	return m.RestoreBy(id, LegacyActor)
}

// RestoreBy is RestoreFromTrash naming actor, refused if blank.
func (m *Manager) RestoreBy(id, actor string) (Task, error) {
	if err := requireActor(actor); err != nil {
		return Task{}, err
	}
	unlock := m.lock(id)
	err := m.persist.RestoreBy(id, actor)
	if err != nil {
		unlock()
		return Task{}, err
	}
	// RestoreBy returns only an error, not the restored Task, so the id must still resolve before unlock — a concurrent DeleteBy for the same id, released to run right after unlock, would otherwise make a restore that fully succeeded report a not-found error instead.
	t, err := m.persist.Get(id)
	unlock()
	if err != nil {
		return Task{}, err
	}
	m.recordFiredStatus(t.ID, string(t.Status))
	m.emitter.Emit(events.TaskCreated, eventPath(t))
	return t, nil
}

// ListTrash returns every trashed task generation, newest first.
func (m *Manager) ListTrash() ([]TrashEntry, error) {
	return m.store.ListTrash()
}

// PruneTrash permanently removes trash generations older than
// retentionDays. A negative retentionDays disables pruning.
func (m *Manager) PruneTrash(retentionDays int) (TrashPruneReport, error) {
	return m.store.PruneTrash(retentionDays)
}

// DeleteTrashedGeneration permanently removes id's newest trashed
// generation immediately, bypassing the retention window.
func (m *Manager) DeleteTrashedGeneration(id string) (bool, error) {
	return m.store.DeleteTrashedGeneration(id)
}

// PruneAllTrash permanently removes every trashed generation regardless of
// age.
func (m *Manager) PruneAllTrash() (TrashPruneReport, error) {
	return m.store.PruneAllTrash()
}

// AddRun appends an agent run to the task and emits task:updated. Attributed
// to LegacyActor; prefer AddRunBy.
func (m *Manager) AddRun(taskID string, run AgentRun) error {
	return m.AddRunWithStatusBy(taskID, LegacyActor, run, nil)
}

// AddRunBy is AddRun naming actor, refused if blank.
func (m *Manager) AddRunBy(taskID, actor string, run AgentRun) error {
	return m.AddRunWithStatusBy(taskID, actor, run, nil)
}

// AddRunWithStatus appends an agent run and optionally changes task status
// in one write. Attributed to LegacyActor; prefer AddRunWithStatusBy.
func (m *Manager) AddRunWithStatus(taskID string, run AgentRun, status *Status) error {
	return m.AddRunWithStatusBy(taskID, LegacyActor, run, status)
}

// AddRunWithStatusBy is AddRunWithStatus naming actor, refused if blank. The
// emit and status hook fire after unlock — see DeleteBy for why.
func (m *Manager) AddRunWithStatusBy(taskID, actor string, run AgentRun, status *Status) error {
	if err := requireActor(actor); err != nil {
		return err
	}
	unlock := m.lock(taskID)
	var prevStatus Status
	_, err := m.persist.PutFnBy(taskID, actor, func(cur Task) (Task, []string, error) {
		prevStatus = cur.Status
		next, ferr := applyAddRun(cur, run, status)
		return next, nil, ferr
	})
	var t Task
	if err == nil {
		// A full re-read rather than PutFnBy's own return value: the file backend's PutFnBy runs against a parse-only cur with no sidecars loaded (the same optimization the file watcher's read path uses), so its returned Task would otherwise hand the status hook and the emitted event a snapshot with every plan/review sidecar field zeroed even though the on-disk sidecar files themselves were never touched.
		t, err = m.persist.Get(taskID)
	}
	var (
		fireHook  bool
		newStatus string
	)
	if err == nil && status != nil && m.onStatusHook != nil {
		newStatus = string(t.Status)
		fireHook = newStatus != string(prevStatus)
	}
	if fireHook {
		m.recordFiredStatus(taskID, newStatus)
	}
	unlock()
	if err != nil {
		return err
	}
	m.emitter.Emit(events.TaskUpdated, eventPath(t))
	if fireHook {
		m.onStatusHook(taskID, string(prevStatus), newStatus, t)
	}
	return nil
}

// UpdateRun updates fields on a specific agent run and emits task:updated.
// Attributed to LegacyActor; prefer UpdateRunBy.
func (m *Manager) UpdateRun(taskID, agentID string, patch RunPatch) error {
	return m.UpdateRunBy(taskID, LegacyActor, agentID, patch)
}

// UpdateRunBy is UpdateRun naming actor, refused if blank.
func (m *Manager) UpdateRunBy(taskID, actor, agentID string, patch RunPatch) error {
	if err := requireActor(actor); err != nil {
		return err
	}
	unlock := m.lock(taskID)
	_, err := m.persist.PutFnBy(taskID, actor, func(cur Task) (Task, []string, error) {
		next, ferr := applyRunUpdate(cur, agentID, patch)
		return next, nil, ferr
	})
	var t Task
	if err == nil {
		// Same full re-read AddRunWithStatusBy takes, and for the same reason: PutFnBy's own return value would otherwise carry the file backend's parse-only cur forward with every sidecar field zeroed.
		t, err = m.persist.Get(taskID)
	}
	unlock()
	if err != nil {
		return err
	}
	m.emitter.Emit(events.TaskUpdated, eventPath(t))
	return nil
}
