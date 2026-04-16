package loopagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/provider"
)

// AgentRunner is the subset of agent.Manager the scheduler depends on.
// Defined as an interface so tests can swap in a fake.
type AgentRunner interface {
	Run(cfg agent.RunConfig) (*agent.Agent, error)
}

// EmitFunc forwards loopagent:updated events to the frontend so the GUI
// can refresh without polling. Optional — nil disables emission.
type EmitFunc func(event string, data any)

// Scheduler owns one goroutine per enabled loop agent. Goroutines tick on
// the loop's IntervalSec and call the agent runner each tick. Sync()
// reconciles the running goroutine set against the persisted store; it is
// called from the service layer after every CRUD mutation and once at boot.
type Scheduler struct {
	parent  context.Context
	store   *Store
	runner  AgentRunner
	logger  *slog.Logger
	emit    EmitFunc
	homeDir string

	mu          sync.Mutex
	fetchers    map[string]*runningFetcher
	agentToLoop map[string]string
	wg          sync.WaitGroup
}

type runningFetcher struct {
	cancel    context.CancelFunc
	updatedAt time.Time
	intervalS int
}

// NewScheduler builds a Scheduler. homeDir is the cwd handed to spawned
// agents — must be a directory whose .claude/skills/ holds the slash
// commands the loop prompts reference.
func NewScheduler(parent context.Context, store *Store, runner AgentRunner, logger *slog.Logger, emit EmitFunc, homeDir string) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{
		parent:      parent,
		store:       store,
		runner:      runner,
		logger:      logger,
		emit:        emit,
		homeDir:     homeDir,
		fetchers:    make(map[string]*runningFetcher),
		agentToLoop: make(map[string]string),
	}
}

// Sync reconciles running fetchers with the persisted store. Safe to call
// concurrently — it serializes via the scheduler mutex.
func (s *Scheduler) Sync() {
	all, err := s.store.List()
	if err != nil {
		s.logger.Error("loopagent.sync.list", "err", err)
		return
	}

	want := make(map[string]LoopAgent, len(all))
	for i := range all {
		if all[i].Enabled {
			want[all[i].ID] = all[i]
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Cancel fetchers whose record is gone, disabled, or has changed
	// timing fields. Restart-on-change ensures interval edits take effect.
	for id, rf := range s.fetchers {
		la, stillWanted := want[id]
		if !stillWanted {
			rf.cancel()
			delete(s.fetchers, id)
			s.logger.Info("loopagent.cancel", "id", id, "reason", "disabled-or-removed")
			continue
		}
		if !la.UpdatedAt.Equal(rf.updatedAt) || la.IntervalSec != rf.intervalS {
			rf.cancel()
			delete(s.fetchers, id)
			s.logger.Info("loopagent.restart", "id", id, "reason", "config-changed")
		}
	}

	// Start fetchers for enabled records that aren't running.
	for id := range want {
		if _, running := s.fetchers[id]; running {
			continue
		}
		s.startLocked(want[id])
	}

	if s.emit != nil {
		s.emit("loopagent:updated", nil)
	}
}

// startLocked must be called with s.mu held.
func (s *Scheduler) startLocked(la LoopAgent) {
	ctx, cancel := context.WithCancel(s.parent)
	s.fetchers[la.ID] = &runningFetcher{
		cancel:    cancel,
		updatedAt: la.UpdatedAt,
		intervalS: la.IntervalSec,
	}
	id := la.ID
	interval := time.Duration(la.IntervalSec) * time.Second

	s.wg.Go(func() {
		defer cancel() // idempotent — ctx already canceled by Sync/Stop when goroutine exits
		s.logger.Info("loopagent.start", "id", id, "name", la.Name, "interval_s", la.IntervalSec)

		timer := time.NewTimer(interval)
		defer timer.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				next := s.tick(ctx, id)
				if next <= 0 {
					next = interval
				}
				timer.Reset(next)
			}
		}
	})
}

// tick reloads the record (so live edits take effect on the next fire) and
// spawns one headless agent run. Returns the next interval to wait.
func (s *Scheduler) tick(ctx context.Context, loopID string) time.Duration {
	if ctx.Err() != nil {
		return 0
	}
	la, err := s.store.Get(loopID)
	if err != nil {
		s.logger.Error("loopagent.tick.get", "id", loopID, "err", err)
		return time.Minute
	}
	if !la.Enabled {
		// Sync should have cancelled us already, but be defensive.
		return time.Duration(la.IntervalSec) * time.Second
	}
	if _, err := s.fire(la); err != nil {
		if errors.Is(err, provider.ErrProviderUnhealthy) {
			s.logger.Info("loopagent.tick.skip", "id", loopID, "reason", "provider_unhealthy", "err", err)
		} else {
			s.logger.Error("loopagent.tick.fire", "id", loopID, "err", err)
		}
	}
	return time.Duration(la.IntervalSec) * time.Second
}

// fire spawns one headless agent and records LastRun{At,ID}. The cost is
// filled in later by OnAgentComplete when the agent finishes.
func (s *Scheduler) fire(la LoopAgent) (string, error) {
	cfg := agent.RunConfig{
		Name:                   la.AgentName(),
		Mode:                   "headless",
		Prompt:                 la.Prompt,
		Dir:                    s.homeDir,
		Provider:               la.Provider,
		Model:                  la.Model,
		AllowedTools:           la.AllowedTools,
		IgnoreConcurrencyLimit: true,
	}
	ag, err := s.runner.Run(cfg)
	if err != nil {
		return "", fmt.Errorf("spawn loop agent %s: %w", la.Name, err)
	}

	s.mu.Lock()
	s.agentToLoop[ag.ID] = la.ID
	s.mu.Unlock()

	la.LastRunAt = time.Now().UTC()
	la.LastRunID = ag.ID
	if _, err := s.persistRun(la.ID, func(rec *LoopAgent) {
		rec.LastRunAt = la.LastRunAt
		rec.LastRunID = la.LastRunID
	}); err != nil {
		s.logger.Error("loopagent.fire.persist", "id", la.ID, "err", err)
	}

	if s.emit != nil {
		s.emit("loopagent:updated", nil)
	}
	s.logger.Info("loopagent.fire", "id", la.ID, "name", la.Name, "agent_id", ag.ID)
	return ag.ID, nil
}

// RunNow fires a loop agent once immediately, regardless of schedule. The
// next tick still happens at its natural time — RunNow does not reset the
// fetcher's clock.
func (s *Scheduler) RunNow(id string) (string, error) {
	la, err := s.store.Get(id)
	if err != nil {
		return "", err
	}
	return s.fire(la)
}

// OnAgentComplete is invoked from the app's onAgentComplete hook for every
// finished agent. The scheduler ignores agents it didn't spawn.
func (s *Scheduler) OnAgentComplete(ag *agent.Agent) {
	if ag == nil {
		return
	}
	s.mu.Lock()
	loopID, ok := s.agentToLoop[ag.ID]
	if ok {
		delete(s.agentToLoop, ag.ID)
	}
	s.mu.Unlock()
	if !ok {
		return
	}

	cost := ag.GetCostUSD()
	if _, err := s.persistRun(loopID, func(rec *LoopAgent) {
		rec.LastRunCost = cost
	}); err != nil {
		s.logger.Error("loopagent.complete.persist", "loop_id", loopID, "agent_id", ag.ID, "err", err)
		return
	}
	if s.emit != nil {
		s.emit("loopagent:updated", nil)
	}
}

// persistRun applies a mutator to a record and writes it back. Crucially
// it does NOT bump UpdatedAt — that field tracks user config changes only.
// Bumping it here would trip Sync()'s change detection and restart the
// fetcher every time it fires.
func (s *Scheduler) persistRun(id string, mutate func(*LoopAgent)) (LoopAgent, error) {
	rec, err := s.store.Get(id)
	if err != nil {
		return LoopAgent{}, err
	}
	mutate(&rec)
	if err := s.store.writeFile(rec); err != nil {
		return LoopAgent{}, err
	}
	return rec, nil
}

// Stop cancels every running fetcher and waits for the goroutines to drain.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	for id, rf := range s.fetchers {
		rf.cancel()
		delete(s.fetchers, id)
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// RunningIDs returns a snapshot of the loop IDs with an active fetcher.
// Used by tests and the service layer for status reporting.
func (s *Scheduler) RunningIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.fetchers))
	for id := range s.fetchers {
		ids = append(ids, id)
	}
	return ids
}
