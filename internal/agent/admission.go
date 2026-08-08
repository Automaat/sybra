package agent

import (
	"context"
	"errors"
	"time"
)

// ErrAttemptConflict marks a live or unreconciled attempt that already owns
// the task/worktree targeted by a new mutating run.
var ErrAttemptConflict = errors.New("attempt lease conflict")

// ErrAttemptNeedsReconciliation marks an expired lease whose process and
// workspace must be re-observed before another mutating attempt may start.
var ErrAttemptNeedsReconciliation = errors.New("attempt lease needs reconciliation")

// ErrProviderCapacityReached is a transient admission result. Callers must
// park and retry without consuming workflow retry budget or failing the task.
var ErrProviderCapacityReached = errors.New("provider concurrent agent limit reached")

func IsCapacityError(err error) bool {
	return errors.Is(err, ErrMaxConcurrentReached) || errors.Is(err, ErrProviderCapacityReached)
}

func IsAttemptConflict(err error) bool {
	return errors.Is(err, ErrAttemptConflict) || errors.Is(err, ErrAttemptNeedsReconciliation)
}

type AttemptAccess string

const (
	AttemptAccessMutate  AttemptAccess = "mutate"
	AttemptAccessObserve AttemptAccess = "observe"
)

// AttemptIntent is the typed, provider-resolved request presented to the one
// admission controller immediately before Manager registers or spawns a run.
type AttemptIntent struct {
	IntentID            string        `yaml:"intent_id"`
	TaskID              string        `yaml:"task_id,omitempty"`
	TaskGeneration      uint64        `yaml:"task_generation,omitempty"`
	Worktree            string        `yaml:"worktree,omitempty"`
	WorktreeGeneration  uint64        `yaml:"worktree_generation,omitempty"`
	Access              AttemptAccess `yaml:"access"`
	Role                Role          `yaml:"role,omitempty"`
	Provider            string        `yaml:"provider"`
	CapabilityCertified bool          `yaml:"capability_certified"`
}

type AttemptBinding struct {
	AgentID     string    `yaml:"agent_id"`
	PID         int       `yaml:"pid,omitempty"`
	ProcStarted string    `yaml:"proc_started,omitempty"`
	SessionID   string    `yaml:"session_id,omitempty"`
	ObservedAt  time.Time `yaml:"observed_at"`
}

type AttemptLease struct {
	ID      string
	Version uint64
	// Existing is true only when Acquire replayed an already-live IntentID.
	// The launch chokepoint must not bind or spawn another provider attempt in
	// that case. It is a response marker, not durable lease identity.
	Existing bool
}

// AttemptAdmission is implemented by the application-owned durable dispatch
// controller. Manager.RunContext is the sole production consumer: callers
// submit RunConfig intents, while admission, binding and terminal release stay
// centralized around the actual provider start.
type AttemptAdmission interface {
	Acquire(context.Context, AttemptIntent) (AttemptLease, error)
	Bind(context.Context, AttemptLease, AttemptBinding) error
	Heartbeat(context.Context, AttemptLease, time.Time) error
	Complete(context.Context, AttemptLease, string) error
	Adopt(context.Context, AttemptIntent, AttemptLease, AttemptBinding) (AttemptLease, error)
}

// AttemptLimitUpdater is the optional live-config extension implemented by
// admission controllers whose capacity policy can be replaced in place.
type AttemptLimitUpdater interface {
	ReplaceLimits(global, perProvider int)
}

// AttemptLedgerReconciler is the optional restart/maintenance extension used
// after owned orphan processes have been reaped and worktrees have been
// preserved. Only expired, unobserved leases may be finalized by this path.
type AttemptLedgerReconciler interface {
	NeedsReconciliation(context.Context) (bool, error)
	ReconcileUnobserved(context.Context, []AttemptLease) (int, error)
}
