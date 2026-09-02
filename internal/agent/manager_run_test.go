package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/limits"
	"github.com/Automaat/sybra/internal/provider"
)

// TestRegisterMarkAgentDone_ProviderAccountingInvariant locks in that
// liveByProvider is incremented/decremented in lockstep with liveCount across
// registerRunningAgent and markAgentDone, including the zero-clamp and
// bucket-delete-at-zero behavior.
func TestRegisterMarkAgentDone_ProviderAccountingInvariant(t *testing.T) {
	m, _ := newTestManager(t)

	agents := []*Agent{
		{ID: "a1", Provider: "claude", done: make(chan struct{})},
		{ID: "a2", Provider: "claude", done: make(chan struct{})},
		{ID: "a3", Provider: "codex", done: make(chan struct{})},
	}
	for _, a := range agents {
		if err := m.registerRunningAgent(a, RunConfig{}, func() {}); err != nil {
			t.Fatalf("registerRunningAgent(%s): %v", a.ID, err)
		}
	}
	assertAccountingInvariant(t, m)
	if got := m.InFlightByProvider()["claude"]; got != 2 {
		t.Fatalf("claude in-flight = %d, want 2", got)
	}
	if got := m.InFlightByProvider()["codex"]; got != 1 {
		t.Fatalf("codex in-flight = %d, want 1", got)
	}

	m.markAgentDone(context.Background(), agents[0])
	assertAccountingInvariant(t, m)
	if got := m.InFlightByProvider()["claude"]; got != 1 {
		t.Fatalf("claude in-flight after one done = %d, want 1", got)
	}

	m.markAgentDone(context.Background(), agents[1])
	assertAccountingInvariant(t, m)
	if _, ok := m.InFlightByProvider()["claude"]; ok {
		t.Fatal("claude bucket should be deleted once its count reaches zero")
	}

	// Idempotent: a repeated terminal call must not double-decrement.
	m.markAgentDone(context.Background(), agents[1])
	assertAccountingInvariant(t, m)

	m.markAgentDone(context.Background(), agents[2])
	assertAccountingInvariant(t, m)
	if got := m.InFlightByProvider(); len(got) != 0 {
		t.Fatalf("expected empty in-flight map, got %+v", got)
	}
}

func TestReleaseStaleStoppedAgentsForTask_ReleasesDoneGate(t *testing.T) {
	m, _ := newTestManager(t)
	m.deadAgentRetention = 0
	stale := &Agent{
		ID:          "stale-stopped",
		TaskID:      "task-1",
		Provider:    "claude",
		State:       StateStopped,
		LastEventAt: time.Now().Add(-30 * time.Minute),
		done:        make(chan struct{}),
	}
	if err := m.registerRunningAgent(stale, RunConfig{}, func() {}); err != nil {
		t.Fatalf("registerRunningAgent: %v", err)
	}
	if !m.HasRunningAgentForTask("task-1") {
		t.Fatal("precondition: stopped agent with open done channel should still gate liveness")
	}

	if got := m.ReleaseStaleStoppedAgentsForTask(context.Background(), "task-1", 15*time.Minute); got != 1 {
		t.Fatalf("released = %d, want 1", got)
	}
	if m.HasRunningAgentForTask("task-1") {
		t.Fatal("stale stopped agent should no longer gate dispatch")
	}
}

func TestReleaseStaleStoppedAgentsForTask_KeepsFreshStopRace(t *testing.T) {
	m, _ := newTestManager(t)
	fresh := &Agent{
		ID:          "fresh-stopped",
		TaskID:      "task-1",
		Provider:    "claude",
		State:       StateStopped,
		LastEventAt: time.Now().Add(-30 * time.Minute),
		done:        make(chan struct{}),
	}
	fresh.MarkStopped()
	if err := m.registerRunningAgent(fresh, RunConfig{}, func() {}); err != nil {
		t.Fatalf("registerRunningAgent: %v", err)
	}
	t.Cleanup(func() { m.markAgentDone(context.Background(), fresh) })

	if got := m.ReleaseStaleStoppedAgentsForTask(context.Background(), "task-1", 15*time.Minute); got != 0 {
		t.Fatalf("released = %d, want 0", got)
	}
	if !m.HasRunningAgentForTask("task-1") {
		t.Fatal("fresh stopped agent should still gate until its runner exits")
	}
}

func TestReleaseDeadAgentsForTask_ReleasesDeadRunningGate(t *testing.T) {
	m, _ := newTestManager(t)
	m.deadAgentRetention = 0
	dead := &Agent{
		ID:       "dead-running",
		TaskID:   "task-1",
		Provider: "claude",
		State:    StateRunning,
		PID:      9999999,
		done:     make(chan struct{}),
	}
	if err := m.registerRunningAgent(dead, RunConfig{}, func() {}); err != nil {
		t.Fatalf("registerRunningAgent: %v", err)
	}
	if !m.HasRunningAgentForTask("task-1") {
		t.Fatal("precondition: dead running agent should still gate before release")
	}

	if got := m.ReleaseDeadAgentsForTask(context.Background(), "task-1"); got != 1 {
		t.Fatalf("released = %d, want 1", got)
	}
	if m.HasRunningAgentForTask("task-1") {
		t.Fatal("dead running agent should no longer gate dispatch")
	}
}

func TestReleaseDeadAgentsForTask_KeepsLiveProcess(t *testing.T) {
	m, _ := newTestManager(t)
	alive := &Agent{
		ID:       "alive-running",
		TaskID:   "task-1",
		Provider: "claude",
		State:    StateRunning,
		PID:      os.Getpid(),
		done:     make(chan struct{}),
	}
	if err := m.registerRunningAgent(alive, RunConfig{}, func() {}); err != nil {
		t.Fatalf("registerRunningAgent: %v", err)
	}
	t.Cleanup(func() { m.markAgentDone(context.Background(), alive) })

	if got := m.ReleaseDeadAgentsForTask(context.Background(), "task-1"); got != 0 {
		t.Fatalf("released = %d, want 0", got)
	}
	if !m.HasRunningAgentForTask("task-1") {
		t.Fatal("live process must keep gating liveness")
	}
}

func TestClaimTaskDispatch_ExpiresLeakedClaim(t *testing.T) {
	m, _ := newTestManager(t)
	if !m.ClaimTaskDispatch("task-1") {
		t.Fatal("initial claim failed")
	}
	if m.ClaimTaskDispatch("task-1") {
		t.Fatal("fresh duplicate claim should be rejected")
	}

	m.mu.Lock()
	m.dispatchClaims["task-1"] = time.Now().Add(-StaleDispatchClaimAge - time.Minute)
	m.mu.Unlock()

	if !m.ClaimTaskDispatch("task-1") {
		t.Fatal("stale leaked claim should be released and reacquired")
	}
	m.ReleaseTaskDispatch("task-1")
}

// TestMarkAgentDone_EvictsFromRegistry locks in that a finished agent is
// eventually removed from m.agents once its terminal path runs, so a
// long-lived server does not accumulate output buffers and prompts forever
// for every agent that ever ran (#1532). Eviction uses deadAgentRetention set
// to 0 here for deterministic, synchronous eviction; see
// TestMarkAgentDone_RetainsCompletedAgentUntilGracePeriod for the real
// (delayed) production behavior callers rely on to read final state.
func TestMarkAgentDone_EvictsFromRegistry(t *testing.T) {
	m, _ := newTestManager(t)
	m.deadAgentRetention = 0

	a := &Agent{ID: "evict-me", done: make(chan struct{})}
	if err := m.registerRunningAgent(a, RunConfig{}, func() {}); err != nil {
		t.Fatalf("registerRunningAgent: %v", err)
	}
	if _, err := m.GetAgent(a.ID); err != nil {
		t.Fatalf("agent should be registered before completion: %v", err)
	}

	m.markAgentDone(context.Background(), a)

	if _, err := m.GetAgent(a.ID); err == nil {
		t.Fatal("expected evicted agent to be absent from the registry")
	}
	for _, la := range m.ListAgents() {
		if la.ID == a.ID {
			t.Fatal("evicted agent still present in ListAgents()")
		}
	}

	// Idempotent: a repeated terminal call must not panic or misbehave once
	// the entry is already gone.
	m.markAgentDone(context.Background(), a)
}

// TestMarkAgentDone_RetainsCompletedAgentUntilGracePeriod locks in that a
// completed agent stays readable via GetAgent/ListAgents for
// deadAgentRetention after markAgentDone, then is evicted once the grace
// period elapses. Production callers (StopAgent's caller polling GetAgent for
// StateStopped, GetConvoOutput readers) routinely read final state in the
// seconds right after a terminal transition, so eviction must not race that
// window (#1532).
func TestMarkAgentDone_RetainsCompletedAgentUntilGracePeriod(t *testing.T) {
	m, _ := newTestManager(t)
	m.deadAgentRetention = 20 * time.Millisecond

	a := &Agent{ID: "evict-me-later", done: make(chan struct{})}
	if err := m.registerRunningAgent(a, RunConfig{}, func() {}); err != nil {
		t.Fatalf("registerRunningAgent: %v", err)
	}

	m.markAgentDone(context.Background(), a)

	if _, err := m.GetAgent(a.ID); err != nil {
		t.Fatalf("agent should still be readable inside the grace period: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := m.GetAgent(a.ID); err != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expected agent to be evicted once deadAgentRetention elapsed")
}

// TestMarkAgentDone_DoesNotEvictReplacementAgent guards against evicting a
// still-live agent that reused the same ID as one whose terminal path is
// racing behind it (e.g. a fresh dispatch landing while a stale finalize for
// the same task/agent id is still unwinding).
func TestMarkAgentDone_DoesNotEvictReplacementAgent(t *testing.T) {
	m, _ := newTestManager(t)
	m.deadAgentRetention = 0

	stale := &Agent{ID: "reused-id", done: make(chan struct{})}
	if err := m.registerRunningAgent(stale, RunConfig{}, func() {}); err != nil {
		t.Fatalf("registerRunningAgent(stale): %v", err)
	}

	fresh := &Agent{ID: "reused-id", done: make(chan struct{})}
	if err := m.registerRunningAgent(fresh, RunConfig{}, func() {}); err != nil {
		t.Fatalf("registerRunningAgent(fresh): %v", err)
	}

	m.markAgentDone(context.Background(), stale)

	got, err := m.GetAgent(fresh.ID)
	if err != nil {
		t.Fatalf("fresh registration should survive stale markAgentDone: %v", err)
	}
	if got != fresh {
		t.Fatal("registry entry was replaced unexpectedly")
	}
}

// TestRegisterRunningAgent_ConcurrentAccountingRace spreads concurrent
// registrations across providers (mirrors a dispatch wave under the soft
// in-flight cap) and asserts the invariant holds under the race detector.
func TestRegisterRunningAgent_ConcurrentAccountingRace(t *testing.T) {
	m, _ := newTestManager(t)
	providers := []string{"claude", "codex", "copilot"}
	const n = 30

	var wg sync.WaitGroup
	registered := make([]*Agent, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := &Agent{ID: fmt.Sprintf("a%d", i), Provider: providers[i%len(providers)], done: make(chan struct{})}
			if err := m.registerRunningAgent(a, RunConfig{}, func() {}); err != nil {
				t.Errorf("registerRunningAgent(%s): %v", a.ID, err)
				return
			}
			registered[i] = a
		}(i)
	}
	wg.Wait()

	assertAccountingInvariant(t, m)
	if got := m.RunningCount(); got != n {
		t.Fatalf("RunningCount = %d, want %d", got, n)
	}

	for _, a := range registered {
		if a != nil {
			m.markAgentDone(context.Background(), a)
		}
	}
	assertAccountingInvariant(t, m)
	if got := m.RunningCount(); got != 0 {
		t.Fatalf("RunningCount after all done = %d, want 0", got)
	}
	if got := m.InFlightByProvider(); len(got) != 0 {
		t.Fatalf("expected empty in-flight map, got %+v", got)
	}
}

// TestJitterDispatch_DisabledIsNoop verifies a zero dispatchJitterMs (the
// interactive/chat default posture, and headless when the knob is disabled)
// never sleeps.
func TestJitterDispatch_DisabledIsNoop(t *testing.T) {
	m, _ := newTestManager(t)
	start := time.Now()
	if err := m.jitterDispatch(); err != nil {
		t.Fatalf("jitterDispatch: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("expected no delay when dispatchJitterMs=0, took %s", elapsed)
	}
}

func TestJitterRunDispatch_ManualHeadlessSkipsDelay(t *testing.T) {
	m, _ := newTestManager(t)
	m.dispatchJitterMs = 10_000

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.jitterRunDispatchContext(ctx, RunConfig{Mode: "headless", SkipDispatchJitter: true}); err != nil {
		t.Fatalf("manual dispatch should skip jitter, got %v", err)
	}
	if err := m.jitterRunDispatchContext(ctx, RunConfig{Mode: "headless"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("automated dispatch should retain jitter, got %v", err)
	}
}

// TestJitterDispatch_SleepsWithinBound verifies the sleep is uniformly bounded
// by [0, dispatchJitterMs] rather than always waiting the full window.
func TestJitterDispatch_SleepsWithinBound(t *testing.T) {
	m, _ := newTestManager(t)
	m.mu.Lock()
	m.dispatchJitterMs = 50
	m.mu.Unlock()

	start := time.Now()
	if err := m.jitterDispatch(); err != nil {
		t.Fatalf("jitterDispatch: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("jitter slept too long: %s, want <= ~50ms bound", elapsed)
	}
}

// TestJitterDispatch_AbortsOnContextCancel verifies a manager shutdown mid-
// jitter aborts the dispatch promptly instead of blocking for the full window.
func TestJitterDispatch_AbortsOnContextCancel(t *testing.T) {
	// cancelAfter is the only timing this test may reason about. The window
	// itself is drawn from [0, ms), so a draw shorter than it finishes on its
	// own and returns nil with nothing to interrupt — legitimately. slack
	// covers the scheduler gap between the window elapsing and the cancel
	// goroutine actually running, inside which either outcome is correct.
	const cancelAfter = 20 * time.Millisecond
	const slack = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	m := mustNewManager(t, ctx, func(string, any) {}, discardLogger(), t.TempDir())
	m.mu.Lock()
	m.dispatchJitterMs = 10_000
	m.mu.Unlock()

	go func() {
		time.Sleep(cancelAfter)
		cancel()
	}()

	start := time.Now()
	err := m.jitterDispatch()
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("jitterDispatch did not abort promptly on ctx cancel: %s", elapsed)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// Only a call that outlived the cancel by a clear margin and still
	// reported success is a defect. Thresholding on anything shorter than the
	// cancel delay fails whenever the draw legitimately beat it.
	if err == nil && elapsed > cancelAfter+slack {
		t.Fatalf("jitter ran %s, outlasting the cancel, and still returned nil", elapsed)
	}
}

// TestJitterDispatch_RefusesADeadContext pins the half of the contract that
// holds for every draw: a manager already shut down does not dispatch, whether
// or not it drew a window to sleep through.
func TestJitterDispatch_RefusesADeadContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := mustNewManager(t, ctx, func(string, any) {}, discardLogger(), t.TempDir())
	// A jitter of 1 makes the window rand.N(1), which is always zero, so this
	// takes the zero-draw early return every time — the one path that skipped
	// the select and so ignored a cancelled context. A larger window would
	// hide the defect behind the select in all but one run per ms.
	m.mu.Lock()
	m.dispatchJitterMs = 1
	m.mu.Unlock()
	cancel()

	if err := m.jitterDispatch(); err == nil {
		t.Fatal("jitterDispatch proceeded on a cancelled context")
	}
}

func TestNewProviderUnhealthy_RateLimitedFlag(t *testing.T) {
	cases := []struct {
		reason string
		want   bool
	}{
		{provider.RateLimitReason, true},
		{"provider reports rate limit reached", true},
		{"provider disabled", false},
		{"logged out", false},
		{"", false},
	}
	for _, c := range cases {
		ue := newProviderUnhealthy("codex", c.reason)
		if ue.RateLimited != c.want {
			t.Errorf("newProviderUnhealthy(%q).RateLimited = %v, want %v", c.reason, ue.RateLimited, c.want)
		}
		if ue.Provider != "codex" || ue.Reason != c.reason {
			t.Errorf("newProviderUnhealthy dropped fields: %+v", ue)
		}
	}
}

func TestResolveProviderDecision_ComputesRoutingReason(t *testing.T) {
	t.Run("default provider when request omitted", func(t *testing.T) {
		m, _ := newTestManager(t)
		m.SetHealthGate(&fakeGate{healthy: map[string]bool{"claude": true}})

		got, reason, _, err := m.resolveProviderDecision(RunConfig{})
		if err != nil {
			t.Fatalf("resolveProviderDecision: %v", err)
		}
		if got != "claude" {
			t.Fatalf("provider = %q, want claude", got)
		}
		if reason != "default" {
			t.Fatalf("routing reason = %q, want default", reason)
		}
	})

	t.Run("explicit provider stays explicit when healthy", func(t *testing.T) {
		m, _ := newTestManager(t)
		m.SetHealthGate(&fakeGate{healthy: map[string]bool{"codex": true}})

		got, reason, _, err := m.resolveProviderDecision(RunConfig{Provider: "codex"})
		if err != nil {
			t.Fatalf("resolveProviderDecision: %v", err)
		}
		if got != "codex" {
			t.Fatalf("provider = %q, want codex", got)
		}
		if reason != "explicit" {
			t.Fatalf("routing reason = %q, want explicit", reason)
		}
	})

	t.Run("health failover marks failover", func(t *testing.T) {
		m, _ := newTestManager(t)
		m.SetHealthGate(&fakeGate{
			healthy:  map[string]bool{"claude": false, "codex": true},
			failover: map[string]string{"claude": "codex"},
			reasons:  map[string]string{"claude": "rate_limited"},
		})

		got, reason, _, err := m.resolveProviderDecision(RunConfig{Provider: "claude"})
		if err != nil {
			t.Fatalf("resolveProviderDecision: %v", err)
		}
		if got != "codex" {
			t.Fatalf("provider = %q, want codex", got)
		}
		if reason != "failover" {
			t.Fatalf("routing reason = %q, want failover", reason)
		}
	})

	t.Run("limit redirect marks limit", func(t *testing.T) {
		m, _ := newTestManager(t)
		m.SetHealthGate(&fakeGate{healthy: map[string]bool{"claude": true, "codex": true}})
		if err := m.ReplaceRuntimeConfig(ManagerRuntimeConfig{
			DefaultProvider: "claude",
			LimitGate:       &fakeLimitGate{chooseReason: "lower quota pressure"},
			LimitPolicy:     providerLimitTestPolicy(true),
		}); err != nil {
			t.Fatal(err)
		}

		got, reason, _, err := m.resolveProviderDecision(RunConfig{Provider: "claude"})
		if err != nil {
			t.Fatalf("resolveProviderDecision: %v", err)
		}
		if got != "codex" {
			t.Fatalf("provider = %q, want codex", got)
		}
		if reason != "limit" {
			t.Fatalf("routing reason = %q, want limit", reason)
		}
	})
}

func providerLimitTestPolicy(preferUnderused bool) limits.Policy {
	policy := limits.DefaultPolicy()
	policy.PreferUnderused = preferUnderused
	return policy
}

func TestRegisterRunningAgent_IgnoreConcurrencyLimitCannotBypassCap(t *testing.T) {
	m, _ := newTestManager(t)
	m.maxConcurrent = 2

	for i := range m.maxConcurrent {
		a := &Agent{ID: fmt.Sprintf("live%d", i), Provider: "claude", done: make(chan struct{})}
		if err := m.registerRunningAgent(a, RunConfig{}, func() {}); err != nil {
			t.Fatalf("registerRunningAgent(live%d): %v", i, err)
		}
	}

	normal := &Agent{ID: "normal", Provider: "claude", done: make(chan struct{})}
	if err := m.registerRunningAgent(normal, RunConfig{}, func() {}); !errors.Is(err, ErrMaxConcurrentReached) {
		t.Fatalf("normal spawn at cap: err = %v, want ErrMaxConcurrentReached", err)
	}

	control := &Agent{ID: "control-plane", Provider: "claude", done: make(chan struct{})}
	if err := m.registerRunningAgent(control, RunConfig{IgnoreConcurrencyLimit: true}, func() {}); !errors.Is(err, ErrMaxConcurrentReached) {
		t.Fatalf("IgnoreConcurrencyLimit spawn at cap: err = %v, want ErrMaxConcurrentReached", err)
	}
}

// TestRegisterRunningAgent_ClassGate proves the per-workload-class admission
// gate holds a reserved class's guarantee even while the pool is otherwise
// saturated by another class, and rejects a further dispatch of a class with
// no remaining guarantee or borrowable capacity — the acceptance criterion
// for reserve-with-borrowing.
func TestRegisterRunningAgent_ClassGate(t *testing.T) {
	m, _ := newTestManager(t)
	m.mu.Lock()
	m.maxConcurrent = 3
	m.classFloors = map[WorkloadClass]int{ClassCompletion: 1}
	m.mu.Unlock()

	// Saturate the pool with system-class work (no floor of its own).
	sys1 := &Agent{ID: "sys1", Provider: "claude", Role: RoleMonitor, done: make(chan struct{})}
	if err := m.registerRunningAgent(sys1, RunConfig{Role: RoleMonitor}, func() {}); err != nil {
		t.Fatalf("registerRunningAgent(sys1): %v", err)
	}
	sys2 := &Agent{ID: "sys2", Provider: "claude", Role: RoleMonitor, done: make(chan struct{})}
	if err := m.registerRunningAgent(sys2, RunConfig{Role: RoleMonitor}, func() {}); err != nil {
		t.Fatalf("registerRunningAgent(sys2): %v", err)
	}

	// A completion-class dispatch must still be admitted: it hasn't consumed
	// its reserved floor of 1 yet, and reserved classes cannot be starved.
	fix := &Agent{ID: "fix1", Provider: "claude", Role: RolePRFix, done: make(chan struct{})}
	if err := m.registerRunningAgent(fix, RunConfig{Role: RolePRFix}, func() {}); err != nil {
		t.Fatalf("registerRunningAgent(fix, protected by floor): %v", err)
	}

	// A further system-class dispatch must now be rejected: the pool is at
	// its cap (3 live against maxConcurrent effectively bounded by the floor
	// math) and completion's floor must not be stranded.
	sys3 := &Agent{ID: "sys3", Provider: "claude", Role: RoleMonitor, done: make(chan struct{})}
	if err := m.registerRunningAgent(sys3, RunConfig{Role: RoleMonitor}, func() {}); !errors.Is(err, ErrMaxConcurrentReached) {
		t.Fatalf("registerRunningAgent(sys3) err = %v, want ErrMaxConcurrentReached", err)
	}

	m.mu.RLock()
	gotLive := m.liveByClass[ClassSystem]
	gotCompletion := m.liveByClass[ClassCompletion]
	m.mu.RUnlock()
	if gotLive != 2 {
		t.Fatalf("liveByClass[system] = %d, want 2", gotLive)
	}
	if gotCompletion != 1 {
		t.Fatalf("liveByClass[completion] = %d, want 1", gotCompletion)
	}

	m.markAgentDone(context.Background(), fix)
	m.mu.RLock()
	gotCompletion = m.liveByClass[ClassCompletion]
	m.mu.RUnlock()
	if gotCompletion != 0 {
		t.Fatalf("liveByClass[completion] after markAgentDone = %d, want 0", gotCompletion)
	}
}

// TestTryReserveSlot exercises the three capacity postures TryReserveSlot
// must report: under cap, at cap, and cap disabled. It is an advisory peek
// only — the assertions never look at liveCount mutation, since
// TryReserveSlot must not mutate anything.
func TestTryReserveSlot(t *testing.T) {
	m, _ := newTestManager(t)
	m.mu.Lock()
	m.maxConcurrent = 1
	m.mu.Unlock()

	if !m.TryReserveSlot() {
		t.Fatal("expected a slot to be available under the cap")
	}

	a := &Agent{ID: "occupant", Provider: "claude", done: make(chan struct{})}
	if err := m.registerRunningAgent(a, RunConfig{}, func() {}); err != nil {
		t.Fatalf("registerRunningAgent: %v", err)
	}

	if m.TryReserveSlot() {
		t.Fatal("expected no slot available once at maxConcurrent")
	}

	m.mu.Lock()
	m.maxConcurrent = 0
	m.mu.Unlock()
	if !m.TryReserveSlot() {
		t.Fatal("expected maxConcurrent<=0 to always report a slot available")
	}
}

func TestTryHoldCapacity(t *testing.T) {
	m, _ := newTestManager(t)
	m.mu.Lock()
	m.maxConcurrent = 1
	m.mu.Unlock()

	reservation, ok := m.TryHoldCapacity()
	if !ok || reservation == nil {
		t.Fatal("expected reservation under the cap")
	}
	if m.TryReserveSlot() {
		t.Fatal("held reservation must consume visible capacity")
	}
	if _, ok := m.TryHoldCapacity(); ok {
		t.Fatal("second reservation at cap must fail")
	}

	reservation.Release()
	if !m.TryReserveSlot() {
		t.Fatal("released reservation must free capacity")
	}
}

// TestTryHoldCapacityWithLimit proves the SLO throttle's admission hold is
// reservation-aware: a burst of concurrent holds against a limit below the raw
// pool cap cannot collectively overshoot the limit, because outstanding
// reservations (not just live agents) are counted under the lock. This is the
// TOCTOU gap the earlier live-only precheck left open.
func TestTryHoldCapacityWithLimit(t *testing.T) {
	m, _ := newTestManager(t)
	m.mu.Lock()
	m.maxConcurrent = 10
	m.mu.Unlock()

	// Halved ceiling of 5 against a raw cap of 10: exactly 5 holds succeed even
	// though none has converted to a live agent yet (liveCount stays 0).
	var held []*CapacityReservation
	for i := range 5 {
		res, ok := m.TryHoldCapacityWithLimit(5)
		if !ok {
			t.Fatalf("hold %d under limit 5 must succeed (reserved=%d)", i, len(held))
		}
		held = append(held, res)
	}
	if _, ok := m.TryHoldCapacityWithLimit(5); ok {
		t.Fatal("6th hold must fail: reservations count toward the limit even with liveCount==0")
	}
	// Raw pool still has room (5 of 10 reserved), so a non-throttled hold (no
	// extra limit) still admits.
	if _, ok := m.TryHoldCapacityWithLimit(0); !ok {
		t.Fatal("un-throttled hold must still claim the raw pool's free capacity")
	}
	for _, res := range held {
		res.Release()
	}
}

func TestAvailableQueueDrainSlots(t *testing.T) {
	m, _ := newTestManager(t)

	if got := m.AvailableQueueDrainSlots(0); got != 0 {
		t.Fatalf("AvailableQueueDrainSlots(0) = %d, want 0", got)
	}
	if got := m.AvailableQueueDrainSlots(-1); got != 0 {
		t.Fatalf("AvailableQueueDrainSlots(-1) = %d, want 0", got)
	}

	m.mu.Lock()
	m.maxConcurrent = 2
	m.mu.Unlock()
	if got := m.AvailableQueueDrainSlots(5); got != 2 {
		t.Fatalf("under cap AvailableQueueDrainSlots(5) = %d, want 2", got)
	}

	a := &Agent{ID: "occupant", Provider: "claude", done: make(chan struct{})}
	if err := m.registerRunningAgent(a, RunConfig{}, func() {}); err != nil {
		t.Fatalf("registerRunningAgent: %v", err)
	}
	if got := m.AvailableQueueDrainSlots(5); got != 1 {
		t.Fatalf("partially full AvailableQueueDrainSlots(5) = %d, want 1", got)
	}

	b := &Agent{ID: "occupant-2", Provider: "claude", done: make(chan struct{})}
	if err := m.registerRunningAgent(b, RunConfig{}, func() {}); err != nil {
		t.Fatalf("registerRunningAgent(second): %v", err)
	}
	if got := m.AvailableQueueDrainSlots(5); got != 0 {
		t.Fatalf("at cap AvailableQueueDrainSlots(5) = %d, want 0", got)
	}

	m.mu.Lock()
	m.maxConcurrent = 0
	m.mu.Unlock()
	if got := m.AvailableQueueDrainSlots(3); got != 3 {
		t.Fatalf("cap disabled AvailableQueueDrainSlots(3) = %d, want 3", got)
	}
}

// TestAvailableQueueDrainSlots_ClassAware proves the manual-drain batch size
// (always implementation-class items, see enqueueManualStart) reflects a
// floor reserved for a different class: capacity that class_reservations
// protects for completion must not be reported as drainable to
// implementation, even though the raw free-slot count would suggest it is.
func TestAvailableQueueDrainSlots_ClassAware(t *testing.T) {
	m, _ := newTestManager(t)
	m.mu.Lock()
	m.maxConcurrent = 3
	m.classFloors = map[WorkloadClass]int{ClassCompletion: 1}
	m.mu.Unlock()

	// Raw free slots = 3, but 1 must stay protected for completion's floor,
	// so only 2 are drainable to implementation.
	if got := m.AvailableQueueDrainSlots(5); got != 2 {
		t.Fatalf("AvailableQueueDrainSlots(5) with completion floor reserved = %d, want 2", got)
	}

	// A live completion agent already satisfies its own floor, so nothing is
	// protected from implementation anymore: all 2 remaining raw slots drain.
	fix := &Agent{ID: "fix1", Provider: "claude", Role: RolePRFix, done: make(chan struct{})}
	if err := m.registerRunningAgent(fix, RunConfig{Role: RolePRFix}, func() {}); err != nil {
		t.Fatalf("registerRunningAgent(fix): %v", err)
	}
	if got := m.AvailableQueueDrainSlots(5); got != 2 {
		t.Fatalf("AvailableQueueDrainSlots(5) with completion floor already met = %d, want 2", got)
	}
}

// TestMarkAgentDone_QueueNudge locks in that freeing a slot in markAgentDone
// delivers exactly one pending signal on QueueNudge.
func TestMarkAgentDone_QueueNudge(t *testing.T) {
	m, _ := newTestManager(t)

	a := &Agent{ID: "nudge-me", Provider: "claude", done: make(chan struct{})}
	if err := m.registerRunningAgent(a, RunConfig{}, func() {}); err != nil {
		t.Fatalf("registerRunningAgent: %v", err)
	}

	m.markAgentDone(context.Background(), a)

	select {
	case <-m.QueueNudge():
	default:
		t.Fatal("expected a pending nudge after markAgentDone freed a slot")
	}

	select {
	case <-m.QueueNudge():
		t.Fatal("expected only one pending nudge, buffer should be drained")
	default:
	}
}

// TestMarkAgentDone_QueueNudgeCoalesces verifies the buffer-1 coalescing
// contract: back-to-back completions while a nudge is already pending must
// not block and must leave at most one pending signal.
func TestMarkAgentDone_QueueNudgeCoalesces(t *testing.T) {
	m, _ := newTestManager(t)

	agents := []*Agent{
		{ID: "coalesce-1", Provider: "claude", done: make(chan struct{})},
		{ID: "coalesce-2", Provider: "claude", done: make(chan struct{})},
		{ID: "coalesce-3", Provider: "claude", done: make(chan struct{})},
	}
	for _, a := range agents {
		if err := m.registerRunningAgent(a, RunConfig{}, func() {}); err != nil {
			t.Fatalf("registerRunningAgent(%s): %v", a.ID, err)
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, a := range agents {
			m.markAgentDone(context.Background(), a)
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("markAgentDone should never block on a full queueNudge buffer")
	}

	select {
	case <-m.QueueNudge():
	default:
		t.Fatal("expected one coalesced pending nudge after three completions")
	}
	select {
	case <-m.QueueNudge():
		t.Fatal("expected coalescing to retain at most one pending nudge")
	default:
	}
}

// TestMarkAgentDone_DoneOnceSingleNudge verifies doneOnce gates the nudge
// fire site: a repeated markAgentDone call on an already-completed agent
// must not fire a second nudge.
func TestMarkAgentDone_DoneOnceSingleNudge(t *testing.T) {
	m, _ := newTestManager(t)

	a := &Agent{ID: "repeat-done", Provider: "claude", done: make(chan struct{})}
	if err := m.registerRunningAgent(a, RunConfig{}, func() {}); err != nil {
		t.Fatalf("registerRunningAgent: %v", err)
	}

	m.markAgentDone(context.Background(), a)
	m.markAgentDone(context.Background(), a)
	m.markAgentDone(context.Background(), a)

	select {
	case <-m.QueueNudge():
	default:
		t.Fatal("expected exactly one nudge from the first markAgentDone call")
	}
	select {
	case <-m.QueueNudge():
		t.Fatal("repeated markAgentDone on the same agent must not fire a second nudge")
	default:
	}
}

// TestTryReserveSlot_RegisterNoDoubleCount verifies a TryReserveSlot peek
// followed by the authoritative registerRunningAgent claim converts into
// exactly one liveCount/liveByProvider increment, with the accounting
// invariant intact.
func TestTryReserveSlot_RegisterNoDoubleCount(t *testing.T) {
	m, _ := newTestManager(t)

	if !m.TryReserveSlot() {
		t.Fatal("expected a slot to be available on a fresh manager")
	}

	a := &Agent{ID: "reserve-then-register", Provider: "claude", done: make(chan struct{})}
	if err := m.registerRunningAgent(a, RunConfig{}, func() {}); err != nil {
		t.Fatalf("registerRunningAgent: %v", err)
	}

	if got := m.RunningCount(); got != 1 {
		t.Fatalf("RunningCount = %d, want 1", got)
	}
	if got := m.InFlightByProvider()["claude"]; got != 1 {
		t.Fatalf("claude in-flight = %d, want 1", got)
	}
	assertAccountingInvariant(t, m)
}

func TestRunWithCapacityReservation_ConsumesHeldSlot(t *testing.T) {
	m, _ := newTestManager(t)
	m.mu.Lock()
	m.maxConcurrent = 1
	m.mu.Unlock()

	reservation, ok := m.TryHoldCapacity()
	if !ok || reservation == nil {
		t.Fatal("expected reservation under the cap")
	}

	a := &Agent{ID: "held-slot", Provider: "claude", done: make(chan struct{})}
	if err := m.registerRunningAgent(a, RunConfig{capacityReservation: reservation}, func() {}); err != nil {
		t.Fatalf("registerRunningAgent with reservation: %v", err)
	}
	if got := m.RunningCount(); got != 1 {
		t.Fatalf("RunningCount = %d, want 1", got)
	}
	if m.TryReserveSlot() {
		t.Fatal("live agent should still occupy the only slot after reservation consumption")
	}

	// Consumed reservation is already inactive; a deferred caller Release must
	// be a no-op rather than freeing the live agent's slot.
	reservation.Release()
	if m.TryReserveSlot() {
		t.Fatal("Release after consumption must not free a live agent slot")
	}
	assertAccountingInvariant(t, m)
}

// TestFirstHealthyProvider_DistributesAcrossEligiblePeers guards against
// re-introducing a fixed-order pick: with several equally-eligible
// candidates, firstHealthyProvider must not always land on the same one.
func TestFirstHealthyProvider_DistributesAcrossEligiblePeers(t *testing.T) {
	candidates := []string{"claude", "codex", "copilot"}
	healthy := func(string) bool { return true }

	seen := map[string]bool{}
	for range 200 {
		seen[firstHealthyProvider("opencode", candidates, healthy)] = true
	}
	for _, want := range candidates {
		if !seen[want] {
			t.Errorf("firstHealthyProvider never picked %q across 200 trials: %v", want, seen)
		}
	}
}

func TestFirstHealthyProvider_ExcludesAndFiltersUnhealthy(t *testing.T) {
	candidates := []string{"claude", "codex", "copilot"}
	healthy := func(p string) bool { return p == "codex" }

	if got := firstHealthyProvider("claude", candidates, healthy); got != "codex" {
		t.Errorf("got %q, want codex (only healthy candidate)", got)
	}
	if got := firstHealthyProvider("codex", candidates, healthy); got != "" {
		t.Errorf("got %q, want none (only healthy candidate is excluded)", got)
	}
}

// A branch name with multiple path segments (e.g. "fix/issue/2967/slug")
// nests several directories deep under refs/heads/ — only the file's own
// basename must be joined onto branchRefDir/branchLogDir, not the full
// branch name, or the computed grant would double up the nested segments.
func TestGitBranchSingleFiles_MultiSegmentBranchName(t *testing.T) {
	roots := gitSandboxRoots{
		branchRef:    "refs/heads/fix/issue/2967/darwin-git-admin-dir",
		branchRefDir: "/data/clones/repo.git/refs/heads/fix/issue/2967",
		branchLogDir: "/data/clones/repo.git/logs/refs/heads/fix/issue/2967",
	}

	refFile, refLockFile, logFile, logLockFile := gitBranchSingleFiles(roots)

	wantRef := "/data/clones/repo.git/refs/heads/fix/issue/2967/darwin-git-admin-dir"
	wantLog := "/data/clones/repo.git/logs/refs/heads/fix/issue/2967/darwin-git-admin-dir"
	if refFile != wantRef {
		t.Errorf("refFile = %q, want %q", refFile, wantRef)
	}
	if refLockFile != wantRef+".lock" {
		t.Errorf("refLockFile = %q, want %q", refLockFile, wantRef+".lock")
	}
	if logFile != wantLog {
		t.Errorf("logFile = %q, want %q", logFile, wantLog)
	}
	if logLockFile != wantLog+".lock" {
		t.Errorf("logLockFile = %q, want %q", logLockFile, wantLog+".lock")
	}
}

// Two sibling tasks with branches nesting under the same parent segment
// (both under refs/heads/fix/) resolve to distinct single-file grants — the
// isolation property the whole literal-not-subpath design depends on.
func TestGitBranchSingleFiles_SiblingBranchesUnderSameParentDiffer(t *testing.T) {
	a := gitSandboxRoots{
		branchRef:    "refs/heads/fix/task-a",
		branchRefDir: "/data/clones/repo.git/refs/heads/fix",
		branchLogDir: "/data/clones/repo.git/logs/refs/heads/fix",
	}
	b := gitSandboxRoots{
		branchRef:    "refs/heads/fix/task-b",
		branchRefDir: "/data/clones/repo.git/refs/heads/fix",
		branchLogDir: "/data/clones/repo.git/logs/refs/heads/fix",
	}

	aRef, aRefLock, aLog, aLogLock := gitBranchSingleFiles(a)
	bRef, bRefLock, bLog, bLogLock := gitBranchSingleFiles(b)

	if aRef == bRef || aRefLock == bRefLock || aLog == bLog || aLogLock == bLogLock {
		t.Fatalf("sibling branches under the same parent dir must resolve to distinct files: a=%v b=%v",
			[]string{aRef, aRefLock, aLog, aLogLock}, []string{bRef, bRefLock, bLog, bLogLock})
	}
	if aRefLock == bRef || aRef == bRefLock {
		t.Fatalf("a branch's ref/lock must not collide with the sibling's own files")
	}
}

func TestGitBranchSingleFiles_EmptyOnDetachedHead(t *testing.T) {
	refFile, refLockFile, logFile, logLockFile := gitBranchSingleFiles(gitSandboxRoots{})
	if refFile != "" || refLockFile != "" || logFile != "" || logLockFile != "" {
		t.Fatalf("detached HEAD must produce no branch-file grants, got %q %q %q %q",
			refFile, refLockFile, logFile, logLockFile)
	}
}

func TestGitCommonDirSingleFiles_DerivesOrdinaryWorkflowPaths(t *testing.T) {
	files := gitCommonDirSingleFiles(gitSandboxRoots{commonDir: "/data/clones/repo.git"})

	want := gitCommonDirFiles{
		packedRefsLock: "/data/clones/repo.git/packed-refs.lock",
		shallow:        "/data/clones/repo.git/shallow",
		shallowLock:    "/data/clones/repo.git/shallow.lock",
		stashRef:       "/data/clones/repo.git/refs/stash",
		stashRefLock:   "/data/clones/repo.git/refs/stash.lock",
		stashLog:       "/data/clones/repo.git/logs/refs/stash",
		stashLogLock:   "/data/clones/repo.git/logs/refs/stash.lock",
	}
	if files != want {
		t.Fatalf("gitCommonDirSingleFiles() = %+v, want %+v", files, want)
	}
}

func TestGitCommonDirSingleFiles_EmptyWithoutCommonDir(t *testing.T) {
	if got := (gitCommonDirSingleFiles(gitSandboxRoots{})); got != (gitCommonDirFiles{}) {
		t.Fatalf("gitCommonDirSingleFiles() = %+v, want zero value", got)
	}
}

func TestGitLooseObjectPattern(t *testing.T) {
	if got := gitLooseObjectPattern(""); got != "" {
		t.Fatalf("empty object dir must produce no sandbox pattern, got %q", got)
	}
	got := gitLooseObjectPattern("/data/repo+.git/objects")
	writePattern := regexp.MustCompile(got)
	for _, path := range []string{
		"/data/repo+.git/objects/tmp_obj_publish",
		"/data/repo+.git/objects/ab/tmp_obj_publish",
		"/data/repo+.git/objects/ab/" + strings.Repeat("c", 38),
		"/data/repo+.git/objects/ab/" + strings.Repeat("d", 62),
	} {
		if !writePattern.MatchString(path) {
			t.Errorf("write pattern rejected valid object path %q", path)
		}
	}
	for _, path := range []string{
		"/data/repo+.git/objects/ab/not-an-object",
		"/data/repo+.git/objects/ab/" + strings.Repeat("c", 37),
		"/data/repo+.git/objects/abc/" + strings.Repeat("c", 38),
	} {
		if writePattern.MatchString(path) {
			t.Errorf("write pattern accepted noncanonical object path %q", path)
		}
	}
	fanoutPattern := regexp.MustCompile(gitLooseObjectFanoutPattern("/data/repo+.git/objects"))
	if !fanoutPattern.MatchString("/data/repo+.git/objects/ab/" + strings.Repeat("c", 38)) {
		t.Error("fanout pattern rejected canonical SHA-1 object")
	}
	if fanoutPattern.MatchString("/data/repo+.git/objects/ab/tmp_obj_publish") {
		t.Error("fanout unlink pattern accepted a publication temp file")
	}
}
