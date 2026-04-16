package provider

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestParseClaudeAuthStatus(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantOK   bool
		wantHC   bool
		wantReas string
	}{
		{"logged_in", `{"loggedIn":true,"subscriptionType":"max"}`, true, true, "ok"},
		{"logged_out", `{"loggedIn":false}`, true, false, "logged_out"},
		{"empty", ``, false, false, "probe_error"},
		{"malformed", `not-json`, false, false, "probe_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := parseClaudeAuthStatus([]byte(tc.raw))
			gotOK := err == nil
			if gotOK != tc.wantOK {
				t.Fatalf("err: got %v want ok=%v", err, tc.wantOK)
			}
			if st.Healthy != tc.wantHC {
				t.Errorf("healthy: got %v want %v", st.Healthy, tc.wantHC)
			}
			if st.Reason != tc.wantReas {
				t.Errorf("reason: got %q want %q", st.Reason, tc.wantReas)
			}
			if st.Provider != "claude" {
				t.Errorf("provider: got %q", st.Provider)
			}
		})
	}
}

func TestParseCodexLoginStatus(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantOK   bool
		wantHC   bool
		wantReas string
	}{
		{"logged_in_chatgpt", "Logged in using ChatGPT", true, true, "ok"},
		{"logged_in_apikey", "Logged in using API key", true, true, "ok"},
		{"not_logged_in", "Not logged in. Please run: codex login", true, false, "logged_out"},
		{"empty", "", false, false, "probe_error"},
		{"unrecognized", "something weird", false, false, "probe_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := parseCodexLoginStatus([]byte(tc.raw))
			gotOK := err == nil
			if gotOK != tc.wantOK {
				t.Fatalf("err: got %v want ok=%v", err, tc.wantOK)
			}
			if st.Healthy != tc.wantHC {
				t.Errorf("healthy: got %v want %v", st.Healthy, tc.wantHC)
			}
			if st.Reason != tc.wantReas {
				t.Errorf("reason: got %q want %q", st.Reason, tc.wantReas)
			}
		})
	}
}

func TestClassifyClaudeError(t *testing.T) {
	cases := []struct {
		name string
		in   ErrorSample
		want Signal
	}{
		{"auth_401", ErrorSample{ErrorStatus: 401}, SignalAuthFailure},
		{"auth_type", ErrorSample{ErrorType: "authentication_error"}, SignalAuthFailure},
		{"rate_429", ErrorSample{ErrorStatus: 429}, SignalRateLimit},
		{"rate_type", ErrorSample{ErrorType: "rate_limit_error"}, SignalRateLimit},
		{"credit", ErrorSample{ErrorType: "credit_balance_too_low"}, SignalRateLimit},
		{"stderr_not_logged", ErrorSample{Stderr: "Error: Not logged in"}, SignalAuthFailure},
		{"stderr_rate_limit", ErrorSample{Stderr: "rate limit exceeded"}, SignalRateLimit},
		{"overloaded_ignored", ErrorSample{ErrorStatus: 529, ErrorType: "overloaded_error"}, SignalNone},
		{"unrelated", ErrorSample{Stderr: "random crash"}, SignalNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, _ := ClassifyClaudeError(tc.in)
			if got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestClassifyCodexError(t *testing.T) {
	cases := []struct {
		name string
		in   ErrorSample
		want Signal
	}{
		{"auth_401", ErrorSample{ErrorStatus: 401}, SignalAuthFailure},
		{"auth_unauthorized", ErrorSample{ErrorType: "unauthorized"}, SignalAuthFailure},
		{"rate_429", ErrorSample{ErrorStatus: 429}, SignalRateLimit},
		{"stderr_not_logged", ErrorSample{Stderr: "Not logged in. Please run: codex login"}, SignalAuthFailure},
		{"stderr_quota", ErrorSample{Stderr: "insufficient_quota"}, SignalRateLimit},
		{"unrelated", ErrorSample{Stderr: "panic goroutine"}, SignalNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, _ := ClassifyCodexError(tc.in)
			if got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

// fakeEmitter records emitted events for assertions.
type fakeEmitter struct {
	mu     sync.Mutex
	events []HealthEvent
}

func (f *fakeEmitter) emit(event string, data any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if event != ProviderHealthEvent {
		return
	}
	if ev, ok := data.(HealthEvent); ok {
		f.events = append(f.events, ev)
	}
}

func (f *fakeEmitter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

func newTestChecker(t *testing.T) (*Checker, *fakeEmitter, *fakeClock) {
	t.Helper()
	fe := &fakeEmitter{}
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	c := New(Config{
		Interval:         time.Minute,
		ClaudeEnabled:    true,
		CodexEnabled:     true,
		AutoFailover:     true,
		ClaudeRLCooldown: 15 * time.Minute,
		CodexRLCooldown:  15 * time.Minute,
	}, fe.emit, nil)
	c.now = clock.Now
	return c, fe, clock
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

func TestChecker_FlipsOnFailure(t *testing.T) {
	c, fe, _ := newTestChecker(t)
	claudeHealthy := true
	c.probeClaude = func(context.Context) (Status, error) {
		if claudeHealthy {
			return Status{Provider: "claude", Healthy: true, Reason: "ok"}, nil
		}
		return Status{Provider: "claude", Healthy: false, Reason: "logged_out"}, nil
	}
	c.probeCodex = func(context.Context) (Status, error) {
		return Status{Provider: "codex", Healthy: true, Reason: "ok"}, nil
	}
	ctx := context.Background()
	c.checkAll(ctx)
	before := fe.count()
	claudeHealthy = false
	c.checkAll(ctx)
	if fe.count() <= before {
		t.Fatalf("expected flip emission, got %d events", fe.count())
	}
	if !c.IsHealthy("codex") {
		t.Errorf("codex should remain healthy")
	}
	if c.IsHealthy("claude") {
		t.Errorf("claude should be unhealthy")
	}
}

func TestFailover_PicksHealthyPeer(t *testing.T) {
	c, _, _ := newTestChecker(t)
	c.setStatus("claude", Status{Provider: "claude", Healthy: true, Reason: "ok"}, true)
	c.setStatus("codex", Status{Provider: "codex", Healthy: true, Reason: "ok"}, true)
	if got := c.Failover("claude"); got != "codex" {
		t.Errorf("want codex peer, got %q", got)
	}
	c.setStatus("claude", Status{Provider: "claude", Healthy: false, Reason: "logged_out"}, true)
	if got := c.Failover("claude"); got != "codex" {
		t.Errorf("claude unhealthy → codex, got %q", got)
	}
	c.setStatus("codex", Status{Provider: "codex", Healthy: false, Reason: "logged_out"}, true)
	if got := c.Failover("claude"); got != "" {
		t.Errorf("both unhealthy → no peer, got %q", got)
	}
	// Auto-failover off: no peer even when healthy.
	c.SetAutoFailover(false)
	c.setStatus("codex", Status{Provider: "codex", Healthy: true, Reason: "ok"}, true)
	if got := c.Failover("claude"); got != "" {
		t.Errorf("auto-failover off → no peer, got %q", got)
	}
}

func TestChecker_PassiveAuthPersistsUntilProbe(t *testing.T) {
	c, _, _ := newTestChecker(t)
	c.setStatus("claude", Status{Provider: "claude", Healthy: true, Reason: "ok"}, true)
	c.ReportAuthFailure("claude", "logged_out")
	if c.IsHealthy("claude") {
		t.Fatalf("should be unhealthy after passive signal")
	}
	// A second passive signal should not reset LastCheck needlessly or emit again.
	c.ReportAuthFailure("claude", "logged_out")
	if c.IsHealthy("claude") {
		t.Fatalf("still unhealthy")
	}
	// Successful probe clears it.
	c.setStatus("claude", Status{Provider: "claude", Healthy: true, Reason: "ok"}, true)
	if !c.IsHealthy("claude") {
		t.Fatalf("probe should clear passive failure")
	}
}

func TestChecker_RateLimitExpires(t *testing.T) {
	c, _, clock := newTestChecker(t)
	c.setStatus("claude", Status{Provider: "claude", Healthy: true, Reason: "ok"}, true)
	c.ReportRateLimit("claude", 10*time.Minute, "rate_limit_error")
	if c.IsHealthy("claude") {
		t.Fatalf("claude should be rate-limited")
	}
	clock.advance(11 * time.Minute)
	c.clearExpiredRateLimits()
	if !c.IsHealthy("claude") {
		t.Fatalf("rate-limit window should have expired")
	}
}

func TestUnhealthyErrorIs(t *testing.T) {
	err := &UnhealthyError{Provider: "claude", Reason: "logged_out"}
	if !errors.Is(err, ErrProviderUnhealthy) {
		t.Fatalf("errors.Is should match sentinel")
	}
}

// TestChecker_FlapEmitsPerFlipNotPerProbe exercises rapid health flapping:
// four probes alternating healthy → unhealthy → healthy → unhealthy. The
// checker must emit exactly once per state change (4 flips = 4 events), not
// once per probe (which would storm the UI). A regression that skipped the
// statusChanged guard would produce 8 events; a regression that missed a flip
// would produce fewer than 4.
func TestChecker_FlapEmitsPerFlipNotPerProbe(t *testing.T) {
	c, fe, _ := newTestChecker(t)
	c.probeCodex = func(context.Context) (Status, error) {
		return Status{Provider: "codex", Healthy: true, Reason: "ok"}, nil
	}

	healthy := true
	c.probeClaude = func(context.Context) (Status, error) {
		if healthy {
			return Status{Provider: "claude", Healthy: true, Reason: "ok"}, nil
		}
		return Status{Provider: "claude", Healthy: false, Reason: "logged_out"}, nil
	}

	ctx := context.Background()
	// Seed first probe as healthy — this is a flip from the initial "unknown"
	// reason seeded in New(), so it counts as one emission.
	c.checkAll(ctx)
	seed := fe.count()

	// Four alternating probes, each must be one flip.
	healthy = false
	c.checkAll(ctx)
	healthy = true
	c.checkAll(ctx)
	healthy = false
	c.checkAll(ctx)
	healthy = true
	c.checkAll(ctx)

	flips := fe.count() - seed
	if flips != 4 {
		t.Errorf("flap emissions = %d, want exactly 4 (one per state change). total=%d seed=%d", flips, fe.count(), seed)
	}

	// Repeat the last state — must NOT emit, since nothing changed.
	c.checkAll(ctx)
	if extra := fe.count() - seed - flips; extra != 0 {
		t.Errorf("repeated healthy probe emitted %d extra events; want 0 (same-state probe should be a no-op)", extra)
	}
}

// TestChecker_ProbeHealthyPreservesActiveRateLimit pins the rate-limit
// precedence rule at setStatus line ~217: when a probe reports the provider
// as healthy but a rate-limit window is still active, the probe result is
// overridden with Reason=rate_limited and the window is preserved. A
// regression that cleared the window on successful probe would release the
// gate early and let the agent hit the real rate limit again.
func TestChecker_ProbeHealthyPreservesActiveRateLimit(t *testing.T) {
	c, _, clock := newTestChecker(t)
	c.setStatus("claude", Status{Provider: "claude", Healthy: true, Reason: "ok"}, true)

	// Mark rate-limited for 10m.
	c.ReportRateLimit("claude", 10*time.Minute, "rate_limit_error")
	if c.IsHealthy("claude") {
		t.Fatalf("claude should be rate-limited immediately after ReportRateLimit")
	}

	// Advance only 1 minute, well within the window.
	clock.advance(1 * time.Minute)

	// Simulate an active probe that would otherwise flip us to healthy —
	// the window must override it.
	c.probeClaude = func(context.Context) (Status, error) {
		return Status{Provider: "claude", Healthy: true, Reason: "ok"}, nil
	}
	c.probeCodex = func(context.Context) (Status, error) {
		return Status{Provider: "codex", Healthy: true, Reason: "ok"}, nil
	}
	c.checkAll(context.Background())

	if c.IsHealthy("claude") {
		t.Fatalf("probe-healthy during active rate-limit window must not release the gate")
	}
	if got := c.Reason("claude"); got != "rate_limited" {
		t.Errorf("Reason = %q, want rate_limited (window should have overridden probe's 'ok' reason)", got)
	}

	// Advance past the window; clearExpiredRateLimits must release.
	clock.advance(20 * time.Minute)
	c.clearExpiredRateLimits()
	if !c.IsHealthy("claude") {
		t.Errorf("claude should be healthy after rate-limit window expires")
	}
}

// TestFailover_BothUnhealthySymmetric covers the "both providers down at the
// same time" scenario explicitly from both sides. With no healthy peer,
// Failover must return the empty string from either direction — the caller
// then surfaces an UnhealthyError instead of looping between two dead
// providers. A regression that returned the unhealthy provider itself (or
// the other unhealthy provider) would cause agents to retry in a tight loop.
func TestFailover_BothUnhealthySymmetric(t *testing.T) {
	c, _, _ := newTestChecker(t)
	c.setStatus("claude", Status{Provider: "claude", Healthy: false, Reason: "logged_out"}, true)
	c.setStatus("codex", Status{Provider: "codex", Healthy: false, Reason: "rate_limited"}, true)

	if peer := c.Failover("claude"); peer != "" {
		t.Errorf("Failover(claude) with both unhealthy = %q, want \"\"", peer)
	}
	if peer := c.Failover("codex"); peer != "" {
		t.Errorf("Failover(codex) with both unhealthy = %q, want \"\"", peer)
	}

	// Recovery path: one provider comes back healthy → failover resolves to it.
	c.setStatus("codex", Status{Provider: "codex", Healthy: true, Reason: "ok"}, true)
	if peer := c.Failover("claude"); peer != "codex" {
		t.Errorf("Failover(claude) after codex recovery = %q, want codex", peer)
	}
}
