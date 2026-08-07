package task

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/workflow"
)

// ErrTransitionConflict is the sentinel a caller can match with errors.Is
// when Apply refuses to mutate a task because an expected precondition
// (generation or status) no longer holds. Callers that get this back must
// re-read the task and decide again rather than retry the same intent.
var ErrTransitionConflict = errors.New("transition: precondition does not match current task state")

// TransitionIntent is a request to move a task to ToStatus through the
// single sanctioned status-mutation entrypoint, Manager.Apply. It is the
// narrow command object every production status writer (workflow, review,
// monitor, watchdog, recovery, service code) should submit instead of
// calling Manager.Update/UpdateFn with a Status field directly — see #2726.
type TransitionIntent struct {
	// TaskID is the task to transition. Required.
	TaskID string
	// ToStatus is the target status. Required.
	ToStatus Status
	// Actor identifies the subsystem submitting the intent (e.g.
	// "workflow.engine", "agentorch.gate") for audit and idempotency-key
	// scoping. Required.
	Actor string

	// Extra carries additional field mutations applied atomically with the
	// status change (StatusReason, Blocker, Tags, PRNumber, ...). Its Status
	// field must be nil — set ToStatus instead; Apply rejects intents that
	// set both to avoid two conflicting sources of truth for the same write.
	Extra Update

	// ExpectedGeneration, given, is an optimistic-concurrency precondition:
	// Apply returns a *ConflictError instead of mutating the task if the
	// task's current Generation does not match. Nil skips the check.
	ExpectedGeneration *int64
	// ExpectedStatus, given, is a precondition on the task's current Status,
	// checked the same way as ExpectedGeneration. Nil skips the check.
	ExpectedStatus *Status

	// IdempotencyKey, given, makes replaying the identical intent against a
	// task that has not changed generation since it was applied a no-op
	// instead of reapplying — the safe way to retry after a crash or a
	// timeout between commit and the caller observing success. Empty means
	// every call applies unconditionally (bare unconditional writes, e.g.
	// funneling an existing scattered caller through Apply for the first
	// time without changing its replay semantics).
	IdempotencyKey string

	// OperatorOverride bypasses the allowed-transition table (see
	// transitions.go): set this only at a human-initiated entry point (a
	// button click or CLI command a person invoked directly), never at an
	// automated call site. It exists for the narrow set of moves that are
	// legitimate only as a deliberate human action — e.g. reopening a
	// terminal task — and must never happen automatically.
	OperatorOverride bool
}

// TransitionResult is the outcome of a successful Apply call.
type TransitionResult struct {
	// Task is the task as it stands after Apply returns — either freshly
	// mutated, or (when Applied is false) the unchanged current task.
	Task Task
	// Applied is false when Apply short-circuited on an idempotent replay:
	// a prior call already consumed this exact IdempotencyKey at the
	// generation this call would have consumed it at, so no audit, dispatch,
	// review, or notification effect fired a second time.
	Applied bool
}

// ConflictError reports that a TransitionIntent's expected precondition did
// not match the task's current state when Apply attempted it. Unwrap yields
// ErrTransitionConflict for errors.Is checks; the fields carry enough detail
// for a caller to log or re-decide without a second read.
type ConflictError struct {
	TaskID string

	ExpectedGeneration *int64
	ActualGeneration   int64

	ExpectedStatus *Status
	ActualStatus   Status
}

func (e *ConflictError) Error() string {
	var parts []string
	if e.ExpectedGeneration != nil {
		parts = append(parts, fmt.Sprintf("generation: expected %d, actual %d", *e.ExpectedGeneration, e.ActualGeneration))
	}
	if e.ExpectedStatus != nil {
		parts = append(parts, fmt.Sprintf("status: expected %q, actual %q", *e.ExpectedStatus, e.ActualStatus))
	}
	return fmt.Sprintf("transition: conflict applying intent to task %s (%s)", e.TaskID, strings.Join(parts, "; "))
}

func (e *ConflictError) Unwrap() error { return ErrTransitionConflict }

// Apply is the single sanctioned entrypoint for a production status
// mutation. It validates the intent, checks any expected-state
// precondition, persists the task and its durable effect record in one
// atomic write (task.Update.EffectLog is written by the same store call
// that changes Status — there is no separate "persisted but effect not yet
// recorded" window to recover from), and fires the same audit/dispatch/
// workflow/notification hook every other Manager write path fires.
//
// Replaying the same IdempotencyKey against a task that has not advanced
// generation since it was consumed returns TransitionResult{Applied: false}
// with no error and no second hook firing — safe to retry blindly after a
// crash or a timeout on the caller side.
//
// Apply intentionally does not take a context.Context: no other Manager
// method does either (Update/UpdateFn/ApplyStatusEffect/Create are all
// fire-and-persist, not cancelable), and this package has no request-scoped
// deadline to honor mid-write — the underlying store write is a single
// local file rename, not a network call.
func (m *Manager) Apply(intent TransitionIntent) (TransitionResult, error) {
	id := strings.TrimSpace(intent.TaskID)
	if id == "" {
		return TransitionResult{}, fmt.Errorf("transition: task id is required")
	}
	if intent.ToStatus == "" {
		return TransitionResult{}, fmt.Errorf("transition: to_status is required")
	}
	actor := strings.TrimSpace(intent.Actor)
	if actor == "" {
		return TransitionResult{}, fmt.Errorf("transition: actor is required")
	}
	if intent.Extra.Status != nil {
		return TransitionResult{}, fmt.Errorf("transition: intent.Extra.Status must be nil; set ToStatus instead")
	}

	unlock := m.lock(id)

	cur, err := m.store.Get(id)
	if err != nil {
		unlock()
		return TransitionResult{}, err
	}

	outcome, err := m.applyLocked(cur, intent)
	unlock()
	if err != nil {
		return TransitionResult{}, err
	}
	m.fireApplyOutcome(id, outcome)
	return outcome.result, nil
}

// ApplyFn is Apply's read-decide-write counterpart, for callers whose target
// status/fields depend on the task's current state (e.g. a retry count or
// tag merge derived from the task as it stands right now) — the same
// relationship UpdateFn has to Update. fn runs under the same per-task lock
// Apply itself takes, so the decision and the write are atomic; the write
// then goes through the same precondition/idempotency/hook path as Apply
// instead of a bare Manager.Update. fn's returned intent.TaskID is ignored
// (always id); return an error from fn (e.g. a skip sentinel) to abort
// without writing.
func (m *Manager) ApplyFn(id string, fn func(cur Task) (TransitionIntent, error)) (TransitionResult, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return TransitionResult{}, fmt.Errorf("transition: task id is required")
	}

	unlock := m.lock(id)

	cur, err := m.store.Get(id)
	if err != nil {
		unlock()
		return TransitionResult{}, err
	}
	intent, err := fn(cur)
	if err != nil {
		unlock()
		return TransitionResult{}, err
	}
	intent.TaskID = id

	outcome, err := m.applyLocked(cur, intent)
	unlock()
	if err != nil {
		return TransitionResult{}, err
	}
	m.fireApplyOutcome(id, outcome)
	return outcome.result, nil
}

// applyOutcome carries applyLocked's result plus the hook-firing decision
// out of the locked section, so the caller can fire the hook only after
// releasing the per-task mutex (see the ordering note on Manager.UpdateFn).
type applyOutcome struct {
	result     TransitionResult
	fireHook   bool
	prevStatus string
	newStatus  string
}

// fireApplyOutcome runs Apply/ApplyFn's unlocked tail: the status hook (only
// on a real write, matching Update/UpdateFn), then metrics/emit (only when
// the write actually happened — an idempotent replay skips both, same as
// the original single-function Apply did by returning early).
func (m *Manager) fireApplyOutcome(id string, outcome applyOutcome) {
	if outcome.fireHook {
		m.onStatusHook(id, outcome.prevStatus, outcome.newStatus, outcome.result.Task)
	}
	if outcome.result.Applied {
		metrics.TaskUpdated()
		m.emitter.Emit(events.TaskUpdated, outcome.result.Task.FilePath)
	}
}

// applyLocked is Apply/ApplyFn's shared core. cur must be the task the caller
// already read under its per-task lock, held for the duration of this call —
// applyLocked performs the precondition checks and the store write, but never
// fires the status hook or emits an event itself (see applyOutcome).
func (m *Manager) applyLocked(cur Task, intent TransitionIntent) (applyOutcome, error) {
	if intent.ToStatus == "" {
		return applyOutcome{}, fmt.Errorf("transition: to_status is required")
	}
	actor := strings.TrimSpace(intent.Actor)
	if actor == "" {
		return applyOutcome{}, fmt.Errorf("transition: actor is required")
	}
	if intent.Extra.Status != nil {
		return applyOutcome{}, fmt.Errorf("transition: intent.Extra.Status must be nil; set ToStatus instead")
	}

	if intent.ExpectedGeneration != nil && cur.Generation != *intent.ExpectedGeneration {
		return applyOutcome{}, &ConflictError{
			TaskID:             cur.ID,
			ExpectedGeneration: intent.ExpectedGeneration,
			ActualGeneration:   cur.Generation,
		}
	}
	if intent.ExpectedStatus != nil && cur.Status != *intent.ExpectedStatus {
		return applyOutcome{}, &ConflictError{
			TaskID:         cur.ID,
			ExpectedStatus: intent.ExpectedStatus,
			ActualStatus:   cur.Status,
		}
	}

	if !intent.OperatorOverride && !IsTransitionAllowed(cur.Status, intent.ToStatus) {
		return applyOutcome{}, &IllegalTransitionError{
			TaskID: cur.ID,
			From:   cur.Status,
			To:     intent.ToStatus,
			Actor:  actor,
		}
	}

	stepID := strings.TrimSpace(intent.IdempotencyKey)
	if stepID != "" && statusEffectApplied(cur.EffectLog, cur.Generation-1, stepID) {
		return applyOutcome{result: TransitionResult{Task: cur, Applied: false}}, nil
	}
	if err := validateHumanRequiredTransition(cur.Status, intent.ToStatus, intent.Extra); err != nil {
		return applyOutcome{}, err
	}

	u := intent.Extra
	toStatus := intent.ToStatus
	u.Status = &toStatus

	if stepID != "" {
		now := time.Now().UTC()
		log := slices.Clone(cur.EffectLog)
		idempotencyID, ok := statusEffectIDForStep(log, cur.Generation, stepID)
		if !ok {
			idempotencyID = workflow.EffectID{
				Generation: cur.Generation,
				StepSeq:    nextStatusEffectSeq(cur),
				StepID:     stepID,
				Pos:        0,
			}
		}
		record := workflow.EffectRecord{ID: idempotencyID, IntentAt: now}
		record.CompletedAt = &now
		log = append(log, record)
		if len(log) > maxTaskEffectLog {
			log = log[len(log)-maxTaskEffectLog:]
		}
		u.EffectLog = &log
	}

	t, prev, err := m.store.UpdateWithPrev(cur.ID, u)
	if err != nil {
		return applyOutcome{}, err
	}

	var (
		fireHook            bool
		prevStatus, newStat string
	)
	if m.onStatusHook != nil {
		prevStatus = string(prev)
		newStat = string(t.Status)
		fireHook = newStat != prevStatus
	}
	if fireHook {
		m.recordFiredStatus(cur.ID, newStat)
	}
	return applyOutcome{
		result:     TransitionResult{Task: t, Applied: true},
		fireHook:   fireHook,
		prevStatus: prevStatus,
		newStatus:  newStat,
	}, nil
}
