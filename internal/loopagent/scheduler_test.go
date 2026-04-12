package loopagent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/synapse/internal/agent"
)

type fakeRunner struct {
	mu     sync.Mutex
	calls  []agent.RunConfig
	nextID string
	err    error
}

func (f *fakeRunner) Run(cfg agent.RunConfig) (*agent.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.calls = append(f.calls, cfg)
	id := f.nextID
	if id == "" {
		id = "fakeagent"
	}
	return &agent.Agent{ID: id, Name: cfg.Name, StartedAt: time.Now()}, nil
}

func (f *fakeRunner) Calls() []agent.RunConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]agent.RunConfig, len(f.calls))
	copy(out, f.calls)
	return out
}

func newSched(t *testing.T) (*Scheduler, *Store, *fakeRunner) {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	runner := &fakeRunner{}
	sched := NewScheduler(context.Background(), store, runner, nil, nil, t.TempDir())
	return sched, store, runner
}

func TestSchedulerSyncStartsAndStopsFetchers(t *testing.T) {
	sched, store, _ := newSched(t)
	defer sched.Stop()

	la, err := store.Create(LoopAgent{Name: "a", Prompt: "/a", IntervalSec: 60, Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	sched.Sync()
	if got := sched.RunningIDs(); len(got) != 1 || got[0] != la.ID {
		t.Fatalf("expected one running fetcher for %s, got %v", la.ID, got)
	}

	// Disable → Sync should cancel.
	la.Enabled = false
	if _, err := store.Update(la); err != nil {
		t.Fatalf("disable: %v", err)
	}
	sched.Sync()
	if got := sched.RunningIDs(); len(got) != 0 {
		t.Fatalf("expected zero running fetchers after disable, got %v", got)
	}
}

func TestSchedulerSyncRestartsOnConfigChange(t *testing.T) {
	sched, store, _ := newSched(t)
	defer sched.Stop()

	la, _ := store.Create(LoopAgent{Name: "a", Prompt: "/a", IntervalSec: 60, Enabled: true})
	sched.Sync()

	sched.mu.Lock()
	rfBefore := sched.fetchers[la.ID]
	sched.mu.Unlock()
	if rfBefore == nil {
		t.Fatal("fetcher not started")
	}

	la.IntervalSec = 120
	if _, err := store.Update(la); err != nil {
		t.Fatalf("update: %v", err)
	}
	sched.Sync()

	sched.mu.Lock()
	rfAfter := sched.fetchers[la.ID]
	sched.mu.Unlock()
	if rfAfter == nil {
		t.Fatal("fetcher gone after restart")
	}
	if rfAfter == rfBefore {
		t.Fatal("expected fresh runningFetcher after config change")
	}
	if rfAfter.intervalS != 120 {
		t.Fatalf("interval not picked up: %d", rfAfter.intervalS)
	}
}

func TestSchedulerFireUpdatesLastRunFields(t *testing.T) {
	sched, store, runner := newSched(t)
	defer sched.Stop()

	runner.nextID = "ag123abc"
	la, _ := store.Create(LoopAgent{Name: "self", Prompt: "/self-monitor", IntervalSec: 60, Model: "sonnet", Enabled: true})

	agentID, err := sched.fire(la)
	if err != nil {
		t.Fatalf("fire: %v", err)
	}
	if agentID != "ag123abc" {
		t.Fatalf("agent id mismatch: %s", agentID)
	}

	calls := runner.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 runner.Run call, got %d", len(calls))
	}
	got := calls[0]
	if got.Mode != "headless" || got.Provider != "claude" || got.Prompt != "/self-monitor" || got.Model != "sonnet" {
		t.Fatalf("RunConfig mismatch: %+v", got)
	}
	if got.Name != "loop:self" {
		t.Fatalf("Name should be loop:self, got %q", got.Name)
	}
	if !got.IgnoreConcurrencyLimit {
		t.Fatal("loop runs must bypass the concurrency limit")
	}

	// Persisted run fields.
	stored, err := store.Get(la.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.LastRunID != "ag123abc" {
		t.Fatalf("LastRunID not persisted: %s", stored.LastRunID)
	}
	if stored.LastRunAt.IsZero() {
		t.Fatal("LastRunAt not persisted")
	}
	if !stored.UpdatedAt.Equal(la.UpdatedAt) {
		t.Fatalf("fire must NOT bump UpdatedAt (would trip Sync change detection): before=%v after=%v", la.UpdatedAt, stored.UpdatedAt)
	}
}

func TestSchedulerOnAgentCompleteUpdatesCost(t *testing.T) {
	sched, store, runner := newSched(t)
	defer sched.Stop()

	runner.nextID = "ag-cost"
	la, _ := store.Create(LoopAgent{Name: "self", Prompt: "/p", IntervalSec: 60, Enabled: true})
	if _, err := sched.fire(la); err != nil {
		t.Fatalf("fire: %v", err)
	}

	ag := &agent.Agent{ID: "ag-cost", Name: "loop:self"}
	ag.AddResultStats("sess", 0.42, 100, 50)
	sched.OnAgentComplete(ag)

	stored, _ := store.Get(la.ID)
	if stored.LastRunCost != 0.42 {
		t.Fatalf("expected cost 0.42, got %v", stored.LastRunCost)
	}

	// Mapping cleared so a second OnAgentComplete for the same id is a noop.
	sched.OnAgentComplete(ag)
	if got := sched.RunningIDs(); len(got) != 0 {
		// Sync was never called so this should still be zero.
		t.Fatalf("RunningIDs unexpectedly populated: %v", got)
	}
}

func TestSchedulerOnAgentCompleteIgnoresUnknown(t *testing.T) {
	sched, _, _ := newSched(t)
	defer sched.Stop()

	// Should not panic, should not modify any record.
	sched.OnAgentComplete(&agent.Agent{ID: "unknown"})
}

func TestSchedulerRunNowFiresOutsideSchedule(t *testing.T) {
	sched, store, runner := newSched(t)
	defer sched.Stop()

	runner.nextID = "ag-rn"
	la, _ := store.Create(LoopAgent{Name: "self", Prompt: "/p", IntervalSec: 3600, Enabled: false})

	id, err := sched.RunNow(la.ID)
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if id != "ag-rn" {
		t.Fatalf("agent id mismatch: %s", id)
	}
	if len(runner.Calls()) != 1 {
		t.Fatal("RunNow did not fire")
	}
}

func TestSchedulerStopWaitsForGoroutines(t *testing.T) {
	sched, store, _ := newSched(t)
	la, _ := store.Create(LoopAgent{Name: "a", Prompt: "/a", IntervalSec: 60, Enabled: true})
	sched.Sync()
	if len(sched.RunningIDs()) != 1 {
		t.Fatal("fetcher not running")
	}
	_ = la

	done := make(chan struct{})
	go func() {
		sched.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return within 2s")
	}
	if len(sched.RunningIDs()) != 0 {
		t.Fatal("RunningIDs should be empty after Stop")
	}
}
