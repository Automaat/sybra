package agent

import (
	"context"
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
