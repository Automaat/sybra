package dispatch

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/taskstatus"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newTestController(t *testing.T, clock *fakeClock, owner string, limits Limits) *Controller {
	t.Helper()
	c, err := New(t.Context(), Options{Dir: t.TempDir(), Owner: owner, Limits: limits, TTL: time.Minute, Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func intent(id, taskID, wt, provider string, access agent.AttemptAccess) agent.AttemptIntent {
	return agent.AttemptIntent{IntentID: id, TaskID: taskID, Worktree: wt, Provider: provider, Access: access, CapabilityCertified: true}
}

func TestAcquireReplayAndMutationExclusivity(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)}
	c := newTestController(t, clock, "epoch-1", Limits{})
	wt := filepath.Join(t.TempDir(), "worktree")
	first, err := c.Acquire(t.Context(), intent("intent-1", "task-1", wt, providerid.Claude, agent.AttemptAccessMutate))
	if err != nil {
		t.Fatal(err)
	}
	replay, err := c.Acquire(t.Context(), intent("intent-1", "task-1", wt, providerid.Claude, agent.AttemptAccessMutate))
	if err != nil || replay.ID != first.ID || replay.Version != first.Version || !replay.Existing {
		t.Fatalf("replay = %+v, %v; want existing lease matching %+v", replay, err, first)
	}
	if first.Existing {
		t.Fatal("fresh lease marked Existing")
	}
	if _, err := c.Acquire(t.Context(), intent("intent-2", "task-1", "", providerid.Codex, agent.AttemptAccessMutate)); !errors.Is(err, agent.ErrAttemptConflict) {
		t.Fatalf("same-task error = %v", err)
	}
	if _, err := c.Acquire(t.Context(), intent("intent-3", "task-2", wt, providerid.Codex, agent.AttemptAccessMutate)); !errors.Is(err, agent.ErrAttemptConflict) {
		t.Fatalf("same-worktree error = %v", err)
	}
	if _, err := c.Acquire(t.Context(), intent("intent-1", "different", wt, providerid.Claude, agent.AttemptAccessMutate)); !errors.Is(err, ErrIntentReplayMismatch) {
		t.Fatalf("mismatched replay error = %v", err)
	}
}

func TestAcquireReplayAllowsLegacyVerifierWorktreeMigration(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)}
	c := newTestController(t, clock, "epoch-1", Limits{})
	disposable := filepath.Join(t.TempDir(), "verification", "runs", "review-1", "source")
	canonical := filepath.Join(t.TempDir(), "worktree")
	first, err := c.Acquire(t.Context(), agent.AttemptIntent{
		IntentID: "intent-1", TaskID: "task-1", Worktree: disposable,
		Provider: providerid.Claude, Access: agent.AttemptAccessMutate,
		Role: agent.RoleReview, CapabilityCertified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := c.Acquire(t.Context(), agent.AttemptIntent{
		IntentID: "intent-1", TaskID: "task-1", Worktree: canonical,
		Provider: providerid.Claude, Access: agent.AttemptAccessMutate,
		Role: agent.RoleReview, CapabilityCertified: true,
	})
	if err != nil {
		t.Fatalf("legacy verifier replay error = %v", err)
	}
	if replay.ID != first.ID || replay.Version != first.Version || !replay.Existing {
		t.Fatalf("replay = %+v, want existing lease matching %+v", replay, first)
	}
}

func TestObserversAreExplicitAndCompatible(t *testing.T) {
	clock := &fakeClock{t: time.Now().UTC()}
	c := newTestController(t, clock, "epoch", Limits{})
	wt := t.TempDir()
	if _, err := c.Acquire(t.Context(), intent("mutate", "task", wt, providerid.Claude, agent.AttemptAccessMutate)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"observer-1", "observer-2"} {
		if _, err := c.Acquire(t.Context(), intent(id, "task", wt, providerid.Claude, agent.AttemptAccessObserve)); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	bad := intent("implicit", "other", "", providerid.Claude, "")
	if _, err := c.Acquire(t.Context(), bad); !errors.Is(err, ErrInvalidIntent) {
		t.Fatalf("implicit access error = %v", err)
	}
}

func TestHardGlobalAndProviderLimitsUnderConcurrency(t *testing.T) {
	clock := &fakeClock{t: time.Now().UTC()}
	c := newTestController(t, clock, "epoch", Limits{Global: 4, ByProvider: map[string]int{providerid.Claude: 2}})
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := map[string]int{}
	for i := range 40 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			provider := providerid.Claude
			if i%2 == 1 {
				provider = providerid.Codex
			}
			if _, err := c.Acquire(context.Background(), intent(time.Now().String()+string(rune(i)), "", "", provider, agent.AttemptAccessObserve)); err == nil {
				mu.Lock()
				accepted[provider]++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if accepted[providerid.Claude] > 2 {
		t.Fatalf("claude accepted %d, want <= 2", accepted[providerid.Claude])
	}
	if accepted[providerid.Claude]+accepted[providerid.Codex] > 4 {
		t.Fatalf("global accepted %v, want <= 4", accepted)
	}
}

func TestReplaceLimitsParksNewWorkWithoutEvictingActiveLease(t *testing.T) {
	clock := &fakeClock{t: time.Now().UTC()}
	c := newTestController(t, clock, "epoch", Limits{Global: 2, ByProvider: map[string]int{providerid.Claude: 2}})
	if _, err := c.Acquire(t.Context(), intent("active", "task-1", "", providerid.Claude, agent.AttemptAccessMutate)); err != nil {
		t.Fatal(err)
	}
	c.ReplaceLimits(1, 1)
	if _, err := c.Acquire(t.Context(), intent("parked", "task-2", "", providerid.Claude, agent.AttemptAccessMutate)); !agent.IsCapacityError(err) {
		t.Fatalf("Acquire after shrinking limits = %v, want capacity error", err)
	}
}

func TestExpiredLeaseReconcilesAndBlocksReplacement(t *testing.T) {
	clock := &fakeClock{t: time.Now().UTC()}
	c := newTestController(t, clock, "epoch", Limits{})
	lease, err := c.Acquire(t.Context(), intent("old", "task", t.TempDir(), providerid.Claude, agent.AttemptAccessMutate))
	if err != nil {
		t.Fatal(err)
	}
	clock.add(2 * time.Minute)
	if _, err := c.Acquire(t.Context(), intent("replacement", "task", "", providerid.Claude, agent.AttemptAccessMutate)); !errors.Is(err, agent.ErrAttemptNeedsReconciliation) {
		t.Fatalf("replacement error = %v", err)
	}
	records, err := c.Records(t.Context())
	if err != nil || records[0].Status != StatusReconciling {
		t.Fatalf("records = %+v, %v", records, err)
	}
	if err := c.Heartbeat(t.Context(), lease, clock.now()); !errors.Is(err, agent.ErrAttemptNeedsReconciliation) {
		t.Fatalf("expired heartbeat = %v", err)
	}
	current := agent.AttemptLease{ID: records[0].ID, Version: records[0].Version}
	if err := c.Complete(t.Context(), current, "lost-reconciled"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Acquire(t.Context(), intent("replacement", "task", "", providerid.Claude, agent.AttemptAccessMutate)); err != nil {
		t.Fatalf("post-reconcile acquire: %v", err)
	}
}

func TestVersionFenceBindHeartbeatComplete(t *testing.T) {
	clock := &fakeClock{t: time.Now().UTC()}
	c := newTestController(t, clock, "epoch", Limits{})
	lease, err := c.Acquire(t.Context(), intent("run", "task", "", providerid.Claude, agent.AttemptAccessMutate))
	if err != nil {
		t.Fatal(err)
	}
	stale := lease
	stale.Version++
	bind := agent.AttemptBinding{AgentID: "agent-1", PID: 42}
	for name, fn := range map[string]func() error{
		"bind":      func() error { return c.Bind(t.Context(), stale, bind) },
		"heartbeat": func() error { return c.Heartbeat(t.Context(), stale, clock.now()) },
		"complete":  func() error { return c.Complete(t.Context(), stale, string(taskstatus.Done)) },
	} {
		if err := fn(); !errors.Is(err, ErrStaleLease) {
			t.Errorf("%s error = %v", name, err)
		}
	}
	if err := c.Bind(t.Context(), lease, bind); err != nil {
		t.Fatal(err)
	}
	if err := c.Heartbeat(t.Context(), lease, clock.now()); err != nil {
		t.Fatal(err)
	}
	if err := c.Complete(t.Context(), lease, string(taskstatus.Done)); err != nil {
		t.Fatal(err)
	}
	if err := c.Complete(t.Context(), lease, string(taskstatus.Done)); err != nil {
		t.Fatalf("idempotent complete: %v", err)
	}
}

func TestAdoptTransfersOwnerAndRevivesObservedExpiredLease(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{t: time.Now().UTC()}
	old, err := New(t.Context(), Options{Dir: dir, Owner: "old", TTL: time.Minute, Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	in := intent("run", "task", t.TempDir(), providerid.Claude, agent.AttemptAccessMutate)
	lease, err := old.Acquire(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Bind(t.Context(), lease, agent.AttemptBinding{AgentID: "agent-1", PID: 42}); err != nil {
		t.Fatal(err)
	}
	clock.add(2 * time.Minute)
	newController, err := New(t.Context(), Options{Dir: dir, Owner: "replacement", TTL: time.Minute, Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	records, err := newController.Records(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	current := agent.AttemptLease{ID: records[0].ID, Version: records[0].Version}
	adopted, err := newController.Adopt(t.Context(), in, current, agent.AttemptBinding{AgentID: "agent-1", PID: 42, ObservedAt: clock.now()})
	if err != nil {
		t.Fatal(err)
	}
	if adopted.Version != current.Version+1 {
		t.Fatalf("adopted version = %d, want %d", adopted.Version, current.Version+1)
	}
	if adopted.Existing {
		t.Fatal("adopted lease marked Existing")
	}
	if err := old.Heartbeat(t.Context(), current, clock.now()); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("old owner heartbeat = %v", err)
	}
	if err := newController.Heartbeat(t.Context(), adopted, clock.now()); err != nil {
		t.Fatalf("new owner heartbeat = %v", err)
	}
}

func TestConcurrentAdoptHasExactlyOneVersionFencedWinner(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{t: time.Now().UTC()}
	original, err := New(t.Context(), Options{Dir: dir, Owner: "original", TTL: time.Minute, Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	in := intent("run", "task", t.TempDir(), providerid.Claude, agent.AttemptAccessMutate)
	lease, err := original.Acquire(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	clock.add(2 * time.Minute)
	first, err := New(t.Context(), Options{Dir: dir, Owner: "restart-a", TTL: time.Minute, Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(t.Context(), Options{Dir: dir, Owner: "restart-b", TTL: time.Minute, Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		lease agent.AttemptLease
		err   error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for _, controller := range []*Controller{first, second} {
		go func(c *Controller) {
			<-start
			got, adoptErr := c.Adopt(context.Background(), in, lease, agent.AttemptBinding{AgentID: "agent-1", PID: 42, ObservedAt: clock.now()})
			results <- result{lease: got, err: adoptErr}
		}(controller)
	}
	close(start)
	wins, stale := 0, 0
	for range 2 {
		got := <-results
		switch {
		case got.err == nil:
			wins++
			if got.lease.Version != lease.Version+1 {
				t.Errorf("winning version = %d, want %d", got.lease.Version, lease.Version+1)
			}
		case errors.Is(got.err, ErrStaleLease):
			stale++
		default:
			t.Errorf("unexpected adopt result: %+v", got)
		}
	}
	if wins != 1 || stale != 1 {
		t.Fatalf("wins=%d stale=%d, want 1/1", wins, stale)
	}
}

func TestYAMLPersistenceSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{t: time.Now().UTC()}
	c, err := New(t.Context(), Options{Dir: dir, Owner: "one", TTL: time.Minute, Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := c.Acquire(t.Context(), intent("persist", "task", "", providerid.Claude, agent.AttemptAccessMutate))
	if err != nil {
		t.Fatal(err)
	}
	c2, err := New(t.Context(), Options{Dir: dir, Owner: "two", TTL: time.Minute, Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	records, err := c2.Records(t.Context())
	if err != nil || len(records) != 1 || records[0].ID != lease.ID {
		t.Fatalf("records = %+v, %v", records, err)
	}
}

func TestRestartEpochCanCompleteObservedDeadUnexpiredLease(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{t: time.Now().UTC()}
	original, err := New(t.Context(), Options{Dir: dir, Owner: "original", TTL: time.Minute, Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := original.Acquire(t.Context(), intent("run", "task", "", providerid.Claude, agent.AttemptAccessMutate))
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(t.Context(), Options{Dir: dir, Owner: "restart", TTL: time.Minute, Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Complete(t.Context(), lease, "lost"); err != nil {
		t.Fatalf("complete observed-dead lease: %v", err)
	}
	if _, err := restarted.Acquire(t.Context(), intent("replacement", "task", "", providerid.Claude, agent.AttemptAccessMutate)); err != nil {
		t.Fatalf("replacement after terminal reconciliation: %v", err)
	}
}

func TestReconcileUnobservedRequiresExpiryAndExplicitObservation(t *testing.T) {
	clock := &fakeClock{t: time.Now().UTC()}
	c := newTestController(t, clock, "epoch", Limits{})
	lease, err := c.Acquire(t.Context(), intent("orphan", "task", "", providerid.Claude, agent.AttemptAccessMutate))
	if err != nil {
		t.Fatal(err)
	}
	if needed, err := c.NeedsReconciliation(t.Context()); err != nil || needed {
		t.Fatalf("fresh lease reconciliation = %v, %v; want false", needed, err)
	}
	clock.add(2 * time.Minute)
	if needed, err := c.NeedsReconciliation(t.Context()); err != nil || !needed {
		t.Fatalf("expired lease reconciliation = %v, %v; want true", needed, err)
	}
	if n, err := c.ReconcileUnobserved(t.Context(), []agent.AttemptLease{lease}); err != nil || n != 0 {
		t.Fatalf("observed reconciliation = %d, %v; want 0", n, err)
	}
	if _, err := c.Acquire(t.Context(), intent("replacement-before-reconcile", "task", "", providerid.Claude, agent.AttemptAccessMutate)); !errors.Is(err, agent.ErrAttemptNeedsReconciliation) {
		t.Fatalf("observed expired lease admitted replacement: %v", err)
	}
	if n, err := c.ReconcileUnobserved(t.Context(), nil); err != nil || n != 1 {
		t.Fatalf("unobserved reconciliation = %d, %v; want 1", n, err)
	}
	if _, err := c.Acquire(t.Context(), intent("replacement", "task", "", providerid.Claude, agent.AttemptAccessMutate)); err != nil {
		t.Fatalf("replacement after explicit reconciliation: %v", err)
	}
}
