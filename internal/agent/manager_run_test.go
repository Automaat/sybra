package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

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

	m.markAgentDone(agents[0])
	assertAccountingInvariant(t, m)
	if got := m.InFlightByProvider()["claude"]; got != 1 {
		t.Fatalf("claude in-flight after one done = %d, want 1", got)
	}

	m.markAgentDone(agents[1])
	assertAccountingInvariant(t, m)
	if _, ok := m.InFlightByProvider()["claude"]; ok {
		t.Fatal("claude bucket should be deleted once its count reaches zero")
	}

	// Idempotent: a repeated terminal call must not double-decrement.
	m.markAgentDone(agents[1])
	assertAccountingInvariant(t, m)

	m.markAgentDone(agents[2])
	assertAccountingInvariant(t, m)
	if got := m.InFlightByProvider(); len(got) != 0 {
		t.Fatalf("expected empty in-flight map, got %+v", got)
	}
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

	m.markAgentDone(a)

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
	m.markAgentDone(a)
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

	m.markAgentDone(a)

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

	m.markAgentDone(stale)

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
			m.markAgentDone(a)
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
	ctx, cancel := context.WithCancel(context.Background())
	m := mustNewManager(t, ctx, func(string, any) {}, discardLogger(), t.TempDir())
	m.mu.Lock()
	m.dispatchJitterMs = 10_000
	m.mu.Unlock()

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := m.jitterDispatch()
	if err == nil {
		t.Fatal("expected context error when the manager shuts down mid-jitter")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("jitterDispatch did not abort promptly on ctx cancel: %s", elapsed)
	}
}

// TestRun_JitterSkippedForInteractiveMode verifies jitter is applied only to
// headless dispatch — interactive/chat must never be delayed.
func TestRun_JitterSkippedForInteractiveMode(t *testing.T) {
	m, _ := newTestManager(t)
	m.mu.Lock()
	m.dispatchJitterMs = 5_000
	m.mu.Unlock()

	dir := t.TempDir()
	start := time.Now()
	a, err := m.Run(RunConfig{TaskID: "t1", Mode: "interactive", Dir: dir, OneShot: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Cleanup(func() { _ = m.StopAgent(a.ID) })
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("interactive Run must skip jitter, took %s", elapsed)
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

func TestRegisterRunningAgent_IgnoreConcurrencyLimitBypassesCap(t *testing.T) {
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
	if err := m.registerRunningAgent(control, RunConfig{IgnoreConcurrencyLimit: true}, func() {}); err != nil {
		t.Fatalf("IgnoreConcurrencyLimit spawn at cap must succeed, got err = %v", err)
	}
	if _, err := m.GetAgent(control.ID); err != nil {
		t.Fatalf("control-plane agent should be registered: %v", err)
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

// TestMarkAgentDone_QueueNudge locks in that freeing a slot in markAgentDone
// delivers exactly one pending signal on QueueNudge.
func TestMarkAgentDone_QueueNudge(t *testing.T) {
	m, _ := newTestManager(t)

	a := &Agent{ID: "nudge-me", Provider: "claude", done: make(chan struct{})}
	if err := m.registerRunningAgent(a, RunConfig{}, func() {}); err != nil {
		t.Fatalf("registerRunningAgent: %v", err)
	}

	m.markAgentDone(a)

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
			m.markAgentDone(a)
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

	m.markAgentDone(a)
	m.markAgentDone(a)
	m.markAgentDone(a)

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
