package worktree

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/worktreeerr"
)

// ErrPreparationInFlight indicates a mutating worktree operation was refused
// because another one is already running against the same path. Alias of
// worktreeerr.ErrPreparationInFlight for the same import-cycle reason as
// ErrAgentRunning.
var ErrPreparationInFlight = worktreeerr.ErrPreparationInFlight

// pathLocks is the per-path exclusion that keeps two mutating operations off
// one worktree directory.
//
// The dispatch claim (internal/agent.Manager.ClaimTaskDispatch) used to be the
// only thing standing between two concurrent preparations of the same path,
// and it is released on elapsed time alone: any claim older than
// StaleDispatchClaimAge is deleted so a dispatcher that crashed mid-prepare
// cannot wedge a task forever. Nothing bounds a live holder to that age —
// PrepareForTask's fetch/rebase/push run on the caller's context, which on the
// dispatch path has no deadline — so a preparation stalled against an
// unresponsive remote outlives its own claim, and the next dispatcher is
// granted the claim and starts a second `git worktree add`/rebase on the same
// directory (#3114).
//
// Deliberately try-lock, not a blocking mutex: the losing caller is a
// dispatcher, and every dispatcher already knows how to retry a transient
// agent-start failure. Blocking would park a scheduler goroutine for as long
// as the hung operation runs — the exact unbounded wait this is here to stop.
type pathLocks struct {
	mu   sync.Mutex
	held map[string]time.Time
}

// lockPath reserves path for one mutating operation and returns its release
// func. It never blocks: a path already reserved yields ErrPreparationInFlight
// naming how long the incumbent has held it, so the log line distinguishes a
// normal overlap from a wedged holder.
func (m *Manager) lockPath(path string) (release func(), err error) {
	key := filepath.Clean(path)

	m.paths.mu.Lock()
	defer m.paths.mu.Unlock()
	if since, busy := m.paths.held[key]; busy {
		return nil, fmt.Errorf("%w: %s (held %s)", ErrPreparationInFlight, key, time.Since(since).Round(time.Second))
	}
	if m.paths.held == nil {
		m.paths.held = make(map[string]time.Time)
	}
	m.paths.held[key] = time.Now()

	return func() {
		m.paths.mu.Lock()
		defer m.paths.mu.Unlock()
		delete(m.paths.held, key)
	}, nil
}

// WithMutationLock serializes an external canonical-worktree mutation with
// every prepare, cleanup, retry, and promotion operation owned by Manager.
func (m *Manager) WithMutationLock(path string, fn func() error) error {
	release, err := m.lockPath(path)
	if err != nil {
		return err
	}
	defer release()
	return fn()
}
