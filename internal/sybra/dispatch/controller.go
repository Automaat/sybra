// Package dispatch owns durable admission leases for provider attempts.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/google/uuid"
)

var (
	ErrInvalidIntent        = errors.New("invalid attempt intent")
	ErrLeaseNotFound        = errors.New("attempt lease not found")
	ErrStaleLease           = errors.New("stale attempt lease")
	ErrLeaseOwner           = errors.New("attempt lease belongs to another owner epoch")
	ErrIntentReplayMismatch = errors.New("attempt intent replay does not match original intent")
)

type Status string

const (
	StatusAcquired    Status = "acquired"
	StatusBound       Status = "bound"
	StatusReconciling Status = "reconciling"
	StatusCompleted   Status = "completed"
)

type Limits struct {
	Global     int
	ByProvider map[string]int
}

type Options struct {
	Dir    string
	Owner  string
	Limits Limits
	TTL    time.Duration
	Now    func() time.Time
	// Store persists the ledger. Nil keeps the YAML document under Dir, which
	// is what an install with no database backend configured uses.
	Store Persistence
}

// Record is the persisted admission state exposed to restart reconciliation.
// Callers must use the versioned AttemptLease token for every mutation.
type Record struct {
	ID          string               `yaml:"id"`
	Version     uint64               `yaml:"version"`
	Intent      agent.AttemptIntent  `yaml:"intent"`
	OwnerEpoch  string               `yaml:"owner_epoch"`
	Status      Status               `yaml:"status"`
	Binding     agent.AttemptBinding `yaml:"binding,omitempty"`
	CreatedAt   time.Time            `yaml:"created_at"`
	HeartbeatAt time.Time            `yaml:"heartbeat_at"`
	ExpiresAt   time.Time            `yaml:"expires_at,omitempty"`
	CompletedAt time.Time            `yaml:"completed_at,omitempty"`
	Outcome     string               `yaml:"outcome,omitempty"`
}

type diskState struct {
	SchemaVersion int      `yaml:"schema_version"`
	Revision      uint64   `yaml:"revision"`
	Leases        []Record `yaml:"leases,omitempty"`
}

// Controller implements agent.AttemptAdmission. All read-modify-write cycles
// take both a process-local mutex and a cross-process file lock.
type Controller struct {
	mu     sync.Mutex
	dir    string
	path   string
	store  Persistence
	owner  string
	limits Limits
	ttl    time.Duration
	now    func() time.Time
}

var _ agent.AttemptAdmission = (*Controller)(nil)
var _ agent.AttemptLedgerReconciler = (*Controller)(nil)

func New(ctx context.Context, opts Options) (*Controller, error) {
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, errors.New("dispatch controller directory is required")
	}
	if opts.TTL <= 0 {
		return nil, fmt.Errorf("dispatch controller TTL must be > 0, got %s", opts.TTL)
	}
	if opts.Limits.Global < 0 {
		return nil, fmt.Errorf("dispatch global limit must be >= 0, got %d", opts.Limits.Global)
	}
	for provider, limit := range opts.Limits.ByProvider {
		if strings.TrimSpace(provider) == "" || limit < 0 {
			return nil, fmt.Errorf("invalid provider limit %q=%d", provider, limit)
		}
	}
	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("resolve dispatch controller directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create dispatch controller directory: %w", err)
	}
	owner := strings.TrimSpace(opts.Owner)
	if owner == "" {
		owner = uuid.NewString()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	path := filepath.Join(dir, "attempt-leases.yaml")
	store := opts.Store
	if store == nil {
		store = newFilePersistence(path)
	}
	c := &Controller{
		dir: dir, path: path, owner: owner, store: store,
		limits: Limits{Global: opts.Limits.Global, ByProvider: cloneLimits(opts.Limits.ByProvider)},
		ttl:    opts.TTL, now: now,
	}
	if err := c.update(ctx, func(s *diskState, now time.Time) (bool, error) {
		return expireLeases(s, now), nil
	}); err != nil {
		return nil, fmt.Errorf("initialize dispatch controller: %w", err)
	}
	return c, nil
}

func cloneLimits(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[strings.TrimSpace(k)] = v
	}
	return out
}

// ReplaceLimits applies a live config reload to future Acquire calls. Active
// leases are never evicted when a ceiling shrinks; they continue consuming
// capacity and new work remains parked until the count falls below the limit.
func (c *Controller) ReplaceLimits(global, perProvider int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.limits.Global = max(global, 0)
	for provider := range c.limits.ByProvider {
		c.limits.ByProvider[provider] = max(perProvider, 0)
	}
}

func (c *Controller) Acquire(ctx context.Context, intent agent.AttemptIntent) (agent.AttemptLease, error) {
	intent, err := normalizeIntent(intent)
	if err != nil {
		return agent.AttemptLease{}, err
	}
	var result agent.AttemptLease
	err = c.update(ctx, func(s *diskState, now time.Time) (bool, error) {
		changed := expireLeases(s, now)
		if rec := findIntent(s, intent.IntentID); rec != nil {
			if !replayIntentMatches(rec.Intent, intent) {
				return changed, fmt.Errorf("%w: %s", ErrIntentReplayMismatch, intent.IntentID)
			}
			switch rec.Status {
			case StatusAcquired, StatusBound:
				if rec.OwnerEpoch != c.owner {
					return changed, fmt.Errorf("%w: lease %s belongs to prior owner %s", agent.ErrAttemptNeedsReconciliation, rec.ID, rec.OwnerEpoch)
				}
				result = leaseOf(rec)
				result.Existing = true
				return changed, nil
			case StatusReconciling:
				return changed, fmt.Errorf("%w: lease %s", agent.ErrAttemptNeedsReconciliation, rec.ID)
			default:
				return changed, fmt.Errorf("%w: intent %s is already complete", agent.ErrAttemptConflict, intent.IntentID)
			}
		}
		if err := c.admit(s, intent); err != nil {
			return changed, err
		}
		rec := Record{
			ID: uuid.NewString(), Version: 1, Intent: intent, OwnerEpoch: c.owner,
			Status: StatusAcquired, CreatedAt: now, HeartbeatAt: now, ExpiresAt: now.Add(c.ttl),
		}
		s.Leases = append(s.Leases, rec)
		result = leaseOf(&s.Leases[len(s.Leases)-1])
		return true, nil
	})
	return result, err
}

func (c *Controller) Bind(ctx context.Context, lease agent.AttemptLease, binding agent.AttemptBinding) error {
	if strings.TrimSpace(binding.AgentID) == "" {
		return errors.New("attempt binding agent ID is required")
	}
	return c.mutateOwned(ctx, lease, false, func(rec *Record, now time.Time) error {
		if rec.Status == StatusCompleted {
			return fmt.Errorf("%w: lease %s is complete", ErrStaleLease, lease.ID)
		}
		if rec.Status == StatusReconciling {
			return fmt.Errorf("%w: lease %s", agent.ErrAttemptNeedsReconciliation, lease.ID)
		}
		if binding.ObservedAt.IsZero() {
			binding.ObservedAt = now
		}
		rec.Binding = binding
		rec.Status = StatusBound
		rec.HeartbeatAt = binding.ObservedAt.UTC()
		rec.ExpiresAt = now.Add(c.ttl)
		return nil
	})
}

func (c *Controller) Heartbeat(ctx context.Context, lease agent.AttemptLease, observedAt time.Time) error {
	return c.mutateOwned(ctx, lease, false, func(rec *Record, now time.Time) error {
		if rec.Status == StatusReconciling {
			return fmt.Errorf("%w: lease %s", agent.ErrAttemptNeedsReconciliation, lease.ID)
		}
		if rec.Status == StatusCompleted {
			return fmt.Errorf("%w: lease %s is complete", ErrStaleLease, lease.ID)
		}
		if observedAt.IsZero() {
			observedAt = now
		}
		rec.HeartbeatAt = observedAt.UTC()
		rec.ExpiresAt = now.Add(c.ttl)
		return nil
	})
}

func (c *Controller) Complete(ctx context.Context, lease agent.AttemptLease, outcome string) error {
	return c.mutateOwned(ctx, lease, true, func(rec *Record, now time.Time) error {
		if rec.Status == StatusCompleted {
			if rec.Outcome == outcome {
				return nil
			}
			return fmt.Errorf("%w: lease %s completed with a different outcome", ErrStaleLease, lease.ID)
		}
		rec.Status = StatusCompleted
		rec.Outcome = outcome
		rec.CompletedAt = now
		rec.ExpiresAt = time.Time{}
		return nil
	})
}

// Adopt transfers a persisted lease to this controller epoch after restart
// observation proves that its provider attempt is still alive. Adoption also
// revives a reconciling lease; mere expiry never does.
func (c *Controller) Adopt(ctx context.Context, intent agent.AttemptIntent, lease agent.AttemptLease, binding agent.AttemptBinding) (agent.AttemptLease, error) {
	intent, err := normalizeIntent(intent)
	if err != nil {
		return agent.AttemptLease{}, err
	}
	if strings.TrimSpace(binding.AgentID) == "" {
		return agent.AttemptLease{}, errors.New("adopted attempt binding agent ID is required")
	}
	var adopted agent.AttemptLease
	err = c.update(ctx, func(s *diskState, now time.Time) (bool, error) {
		changed := expireLeases(s, now)
		rec := findLease(s, lease.ID)
		if rec == nil {
			return changed, fmt.Errorf("%w: %s", ErrLeaseNotFound, lease.ID)
		}
		if rec.Version != lease.Version {
			return changed, staleVersion(rec, lease)
		}
		if !replayIntentMatches(rec.Intent, intent) {
			return changed, fmt.Errorf("%w: %s", ErrIntentReplayMismatch, intent.IntentID)
		}
		if rec.Status == StatusCompleted {
			return changed, fmt.Errorf("%w: lease %s is complete", ErrStaleLease, lease.ID)
		}
		if binding.ObservedAt.IsZero() {
			binding.ObservedAt = now
		}
		rec.OwnerEpoch = c.owner
		rec.Binding = binding
		rec.Status = StatusBound
		rec.HeartbeatAt = binding.ObservedAt.UTC()
		rec.ExpiresAt = now.Add(c.ttl)
		rec.Version++
		adopted = leaseOf(rec)
		return true, nil
	})
	return adopted, err
}

// Records returns a stable snapshot and durably marks newly expired leases as
// reconciling before exposing them.
func (c *Controller) Records(ctx context.Context) ([]Record, error) {
	var out []Record
	err := c.update(ctx, func(s *diskState, now time.Time) (bool, error) {
		changed := expireLeases(s, now)
		out = append([]Record(nil), s.Leases...)
		return changed, nil
	})
	return out, err
}

// NeedsReconciliation reports whether expiry has produced ledger state that
// must be compared with the survival registry. The caller uses this cheap
// gate before the more expensive orphan-process and worktree-preservation
// passes.
func (c *Controller) NeedsReconciliation(ctx context.Context) (bool, error) {
	needed := false
	err := c.update(ctx, func(s *diskState, now time.Time) (bool, error) {
		changed := expireLeases(s, now)
		for i := range s.Leases {
			if s.Leases[i].Status == StatusReconciling {
				needed = true
				break
			}
		}
		return changed, nil
	})
	return needed, err
}

// ReconcileUnobserved finalizes expired leases absent from the survival
// registry and live manager state. It must only be called after the caller has
// reaped unregistered owned provider processes and preserved their worktrees;
// expiry alone is deliberately insufficient evidence that an attempt is dead.
func (c *Controller) ReconcileUnobserved(ctx context.Context, observed []agent.AttemptLease) (int, error) {
	seen := make(map[string]struct{}, len(observed))
	for i := range observed {
		if observed[i].ID != "" {
			seen[observed[i].ID] = struct{}{}
		}
	}
	reconciled := 0
	err := c.update(ctx, func(s *diskState, now time.Time) (bool, error) {
		changed := expireLeases(s, now)
		for i := range s.Leases {
			rec := &s.Leases[i]
			if rec.Status != StatusReconciling {
				continue
			}
			if _, ok := seen[rec.ID]; ok {
				continue
			}
			rec.Status = StatusCompleted
			rec.Outcome = "orphan_reconciled"
			rec.CompletedAt = now
			rec.ExpiresAt = time.Time{}
			reconciled++
			changed = true
		}
		return changed, nil
	})
	return reconciled, err
}

func (c *Controller) mutateOwned(ctx context.Context, lease agent.AttemptLease, allowReconciling bool, mutate func(*Record, time.Time) error) error {
	return c.update(ctx, func(s *diskState, now time.Time) (bool, error) {
		changed := expireLeases(s, now)
		rec := findLease(s, lease.ID)
		if rec == nil {
			return changed, fmt.Errorf("%w: %s", ErrLeaseNotFound, lease.ID)
		}
		if rec.Version != lease.Version {
			return changed, staleVersion(rec, lease)
		}
		// Terminal reconciliation may be performed by a fresh process epoch
		// with the persisted, still-current token, whether the observed-dead
		// attempt reached its TTL or not. It cannot bind or heartbeat the lease,
		// and adoption increments the version before reviving a live attempt.
		if rec.OwnerEpoch != c.owner && !allowReconciling {
			return changed, fmt.Errorf("%w: lease %s", ErrLeaseOwner, lease.ID)
		}
		if rec.Status == StatusReconciling && !allowReconciling {
			return changed, fmt.Errorf("%w: lease %s", agent.ErrAttemptNeedsReconciliation, lease.ID)
		}
		if err := mutate(rec, now); err != nil {
			return changed, err
		}
		return true, nil
	})
}

func staleVersion(rec *Record, lease agent.AttemptLease) error {
	return fmt.Errorf("%w: lease %s version %d, current %d", ErrStaleLease, lease.ID, lease.Version, rec.Version)
}

func (c *Controller) admit(s *diskState, intent agent.AttemptIntent) error {
	global, provider := 0, 0
	for i := range s.Leases {
		rec := &s.Leases[i]
		if !occupiesCapacity(rec.Status) {
			continue
		}
		global++
		if rec.Intent.Provider == intent.Provider {
			provider++
		}
		if intent.Access == agent.AttemptAccessMutate && rec.Intent.Access == agent.AttemptAccessMutate &&
			((intent.TaskID != "" && intent.TaskID == rec.Intent.TaskID) ||
				(intent.Worktree != "" && intent.Worktree == rec.Intent.Worktree)) {
			if rec.Status == StatusReconciling {
				return fmt.Errorf("%w: lease %s", agent.ErrAttemptNeedsReconciliation, rec.ID)
			}
			return fmt.Errorf("%w: lease %s", agent.ErrAttemptConflict, rec.ID)
		}
	}
	if c.limits.Global > 0 && global >= c.limits.Global {
		return agent.ErrMaxConcurrentReached
	}
	if limit := c.limits.ByProvider[intent.Provider]; limit > 0 && provider >= limit {
		return fmt.Errorf("%w: %s (%d)", agent.ErrProviderCapacityReached, intent.Provider, limit)
	}
	return nil
}

func normalizeIntent(intent agent.AttemptIntent) (agent.AttemptIntent, error) {
	intent.IntentID = strings.TrimSpace(intent.IntentID)
	intent.TaskID = strings.TrimSpace(intent.TaskID)
	intent.Provider = strings.TrimSpace(intent.Provider)
	if intent.IntentID == "" || intent.Provider == "" {
		return intent, fmt.Errorf("%w: intent ID and provider are required", ErrInvalidIntent)
	}
	if intent.Access != agent.AttemptAccessMutate && intent.Access != agent.AttemptAccessObserve {
		return intent, fmt.Errorf("%w: explicit access is required", ErrInvalidIntent)
	}
	if !intent.CapabilityCertified {
		return intent, fmt.Errorf("%w: capability is not certified", ErrInvalidIntent)
	}
	if intent.Worktree != "" {
		abs, err := filepath.Abs(intent.Worktree)
		if err != nil {
			return intent, fmt.Errorf("%w: canonicalize worktree: %w", ErrInvalidIntent, err)
		}
		abs = filepath.Clean(abs)
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
		intent.Worktree = abs
	}
	return intent, nil
}

// replayIntentMatches accepts the one legacy verifier shape written before
// verifier admission was keyed to the canonical worktree. Those records used
// the disposable clone as Worktree, so a restart replays the same workflow
// effect with a different path. Verifiers are observe-only and every other
// identity field must still match; mutating attempts remain exact.
func replayIntentMatches(stored, replay agent.AttemptIntent) bool {
	if reflect.DeepEqual(stored, replay) {
		return true
	}
	if stored.Access != agent.AttemptAccessObserve || replay.Access != agent.AttemptAccessObserve ||
		!stored.Role.IsVerifier() || !replay.Role.IsVerifier() {
		return false
	}
	stored.Worktree = ""
	replay.Worktree = ""
	return reflect.DeepEqual(stored, replay)
}

func expireLeases(s *diskState, now time.Time) bool {
	changed := false
	for i := range s.Leases {
		rec := &s.Leases[i]
		if (rec.Status == StatusAcquired || rec.Status == StatusBound) && !rec.ExpiresAt.IsZero() && !now.Before(rec.ExpiresAt) {
			rec.Status = StatusReconciling
			changed = true
		}
	}
	return changed
}

func occupiesCapacity(status Status) bool {
	return status == StatusAcquired || status == StatusBound || status == StatusReconciling
}

func leaseOf(rec *Record) agent.AttemptLease {
	return agent.AttemptLease{ID: rec.ID, Version: rec.Version}
}

func findIntent(s *diskState, id string) *Record {
	for i := range s.Leases {
		if s.Leases[i].Intent.IntentID == id {
			return &s.Leases[i]
		}
	}
	return nil
}

func findLease(s *diskState, id string) *Record {
	for i := range s.Leases {
		if s.Leases[i].ID == id {
			return &s.Leases[i]
		}
	}
	return nil
}

func (c *Controller) update(ctx context.Context, fn func(*diskState, time.Time) (bool, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var opErr error
	err := c.store.Critical(ctx, func() error {
		s, err := c.store.Load(ctx)
		if err != nil {
			return err
		}
		var changed bool
		changed, opErr = fn(&s, c.now().UTC())
		if !changed {
			return nil
		}
		s.Revision++
		return c.store.Save(ctx, s)
	})
	if err != nil {
		return err
	}
	return opErr
}
