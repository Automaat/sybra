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

	mu := m.lockFor(id)
	mu.Lock()

	cur, err := m.store.Get(id)
	if err != nil {
		mu.Unlock()
		return TransitionResult{}, err
	}

	if intent.ExpectedGeneration != nil && cur.Generation != *intent.ExpectedGeneration {
		mu.Unlock()
		return TransitionResult{}, &ConflictError{
			TaskID:             id,
			ExpectedGeneration: intent.ExpectedGeneration,
			ActualGeneration:   cur.Generation,
		}
	}
	if intent.ExpectedStatus != nil && cur.Status != *intent.ExpectedStatus {
		mu.Unlock()
		return TransitionResult{}, &ConflictError{
			TaskID:         id,
			ExpectedStatus: intent.ExpectedStatus,
			ActualStatus:   cur.Status,
		}
	}

	stepID := strings.TrimSpace(intent.IdempotencyKey)
	if stepID != "" && statusEffectApplied(cur.EffectLog, cur.Generation-1, stepID) {
		mu.Unlock()
		return TransitionResult{Task: cur, Applied: false}, nil
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

	t, prev, err := m.store.UpdateWithPrev(id, u)
	if err != nil {
		mu.Unlock()
		return TransitionResult{}, err
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
		m.recordFiredStatus(id, newStat)
	}
	mu.Unlock()

	if fireHook {
		m.onStatusHook(id, prevStatus, newStat)
	}
	metrics.TaskUpdated()
	m.emitter.Emit(events.TaskUpdated, t.FilePath)
	return TransitionResult{Task: t, Applied: true}, nil
}
