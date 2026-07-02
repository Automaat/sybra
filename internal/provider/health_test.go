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

func TestCodexVersionAtLeast(t *testing.T) {
	cases := []struct {
		name string
		have string
		want bool
	}{
		{"equal", "0.142.2", true},
		{"newer_patch", "0.142.3", true},
		{"newer_minor", "0.143.0", true},
		{"newer_major", "1.0.0", true},
		{"older_patch", "0.142.1", false},
		{"older_minor", "0.141.9", false},
		{"shorter_equal_prefix", "0.142", false},
		{"longer_equal_prefix", "0.142.2.1", true},
		{"suffix_tolerated", "0.142.2-beta", true},
		{"unparseable_fails_open", "", true},
		{"garbage_fails_open", "vNext", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexVersionAtLeast(tc.have, minCodexVersion); got != tc.want {
				t.Errorf("codexVersionAtLeast(%q, %q): got %v want %v", tc.have, minCodexVersion, got, tc.want)
			}
		})
	}
}

func TestCodexVersionRegexExtracts(t *testing.T) {
	cases := map[string]string{
		"codex-cli 0.142.2": "0.142.2",
		"0.142.2\n":         "0.142.2",
		"codex 1.0":         "1.0",
		"no version here":   "",
	}
	for in, want := range cases {
		if got := codexVersionRe.FindString(in); got != want {
			t.Errorf("FindString(%q): got %q want %q", in, got, want)
		}
	}
}

func TestClassifyClaudeError(t *testing.T) {
	cases := []struct {
		name           string
		in             ErrorSample
		want           Signal
		wantReason     string
		wantRetryAfter time.Duration
	}{
		{"auth_401", ErrorSample{ErrorStatus: 401}, SignalAuthFailure, "logged_out", 0},
		{"auth_type", ErrorSample{ErrorType: "authentication_error"}, SignalAuthFailure, "logged_out", 0},
		{"rate_429", ErrorSample{ErrorStatus: 429}, SignalRateLimit, "rate_limited", 0},
		{"rate_type", ErrorSample{ErrorType: "rate_limit_error"}, SignalRateLimit, "rate_limit_error", 0},
		{"credit", ErrorSample{ErrorType: "credit_balance_too_low"}, SignalRateLimit, "credit_balance_too_low", 0},
		{"stderr_not_logged", ErrorSample{Stderr: "Error: Not logged in"}, SignalAuthFailure, "logged_out", 0},
		{"stderr_rate_limit", ErrorSample{Stderr: "rate limit exceeded"}, SignalRateLimit, "rate_limited", 0},
		{"content_session_limit", ErrorSample{Content: "You've hit your session limit · resets 4:30pm"}, SignalRateLimit, "rate_limited", 0},
		{"content_usage_limit", ErrorSample{Content: "usage limit reached for this period"}, SignalRateLimit, "rate_limited", 0},
		{"content_weekly_limit", ErrorSample{Content: "You've hit your weekly limit · resets Jul 1 at 5pm"}, SignalRateLimit, "weekly_limit", weeklyLimitCooldown},
		{"stderr_weekly_limit", ErrorSample{Stderr: "You've hit your weekly limit · resets Jul 1 at 5pm"}, SignalRateLimit, "weekly_limit", weeklyLimitCooldown},
		{"weekly_word_without_specific_phrase_stays_rate_limited", ErrorSample{Content: "usage limit reached · resets weekly on Monday"}, SignalRateLimit, "rate_limited", 0},
		{"clean_content_rate_limit_exceeded", ErrorSample{Content: "rate limit exceeded", ContentIsCleanResult: true}, SignalRateLimit, "rate_limited", 0},
		{"clean_content_rate_limit_reached", ErrorSample{Content: "rate limit reached", ContentIsCleanResult: true}, SignalRateLimit, "rate_limited", 0},
		{"clean_content_weekly_limit", ErrorSample{Content: "you've hit your weekly limit", ContentIsCleanResult: true}, SignalRateLimit, "weekly_limit", weeklyLimitCooldown},
		{"clean_content_mentions_rate_limit", ErrorSample{Content: "fixed rate limit handling", ContentIsCleanResult: true}, SignalNone, "", 0},
		{"overloaded_ignored", ErrorSample{ErrorStatus: 529, ErrorType: "overloaded_error"}, SignalNone, "", 0},
		{"unrelated", ErrorSample{Stderr: "random crash"}, SignalNone, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason, retryAfter := ClassifyClaudeError(tc.in)
			if got != tc.want {
				t.Errorf("signal: got %v want %v", got, tc.want)
			}
			if reason != tc.wantReason {
				t.Errorf("reason: got %q want %q", reason, tc.wantReason)
			}
			if retryAfter != tc.wantRetryAfter {
				t.Errorf("retryAfter: got %v want %v", retryAfter, tc.wantRetryAfter)
			}
		})
	}
}

func TestClassifyCodexError(t *testing.T) {
	cases := []struct {
		name           string
		in             ErrorSample
		want           Signal
		wantReason     string
		wantRetryAfter time.Duration
	}{
		{"auth_401", ErrorSample{ErrorStatus: 401}, SignalAuthFailure, "logged_out", 0},
		{"auth_unauthorized", ErrorSample{ErrorType: "unauthorized"}, SignalAuthFailure, "logged_out", 0},
		{"rate_429", ErrorSample{ErrorStatus: 429}, SignalRateLimit, "rate_limited", 0},
		{"stderr_not_logged", ErrorSample{Stderr: "Not logged in. Please run: codex login"}, SignalAuthFailure, "logged_out", 0},
		{"stderr_quota", ErrorSample{Stderr: "insufficient_quota"}, SignalRateLimit, "rate_limited", 0},
		{"stderr_weekly_limit", ErrorSample{Stderr: "you've hit your weekly limit"}, SignalRateLimit, "weekly_limit", weeklyLimitCooldown},
		{
			"connectivity_websocket_refused",
			ErrorSample{Stderr: "websocket connection refused: wss://chatgpt.com/backend-api/codex/responses"},
			SignalRateLimit, "connectivity", connectivityCooldown,
		},
		{
			"connectivity_mcp_transport",
			ErrorSample{Stderr: "MCP transport failure: chatgpt.com/backend-api/ps/mcp unreachable"},
			SignalRateLimit, "connectivity", connectivityCooldown,
		},
		{
			"connectivity_models_refresh",
			ErrorSample{Stderr: "failed to refresh available models: timeout"},
			SignalRateLimit, "connectivity", connectivityCooldown,
		},
		{
			"connectivity_via_content",
			ErrorSample{Content: "websocket connection refused: wss://chatgpt.com/backend-api/codex/responses"},
			SignalRateLimit, "connectivity", connectivityCooldown,
		},
		{"bare_websocket_no_host_is_none", ErrorSample{Stderr: "websocket connection closed unexpectedly"}, SignalNone, "", 0},
		{"unrelated", ErrorSample{Stderr: "panic goroutine"}, SignalNone, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason, retryAfter := ClassifyCodexError(tc.in)
			if got != tc.want {
				t.Errorf("signal: got %v want %v", got, tc.want)
			}
			if reason != tc.wantReason {
				t.Errorf("reason: got %q want %q", reason, tc.wantReason)
			}
			if retryAfter != tc.wantRetryAfter {
				t.Errorf("retryAfter: got %v want %v", retryAfter, tc.wantRetryAfter)
			}
		})
	}
}

func TestClassifyCopilotError(t *testing.T) {
	cases := []struct {
		name string
		in   ErrorSample
		want Signal
	}{
		{"auth_401", ErrorSample{ErrorStatus: 401}, SignalAuthFailure},
		{"rate_429", ErrorSample{ErrorStatus: 429}, SignalRateLimit},
		{"stderr_not_authenticated", ErrorSample{Stderr: "Error: not authenticated"}, SignalAuthFailure},
		{"stderr_copilot_login", ErrorSample{Stderr: "Please run: copilot login"}, SignalAuthFailure},
		{"stderr_premium_requests", ErrorSample{Stderr: "You have exceeded your premium request allowance"}, SignalRateLimit},
		{"unrelated", ErrorSample{Stderr: "panic goroutine"}, SignalNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, _ := ClassifyCopilotError(tc.in)
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

func (f *fakeEmitter) snapshot() []HealthEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]HealthEvent, len(f.events))
	copy(out, f.events)
	return out
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

func TestChecker_ProbeErrorRequiresConsecutiveFailures(t *testing.T) {
	c, fe, _ := newTestChecker(t)
	c.probeClaude = func(context.Context) (Status, error) {
		return Status{Provider: "claude", Healthy: true, Reason: "ok"}, nil
	}
	c.probeCodex = func(context.Context) (Status, error) {
		return Status{Provider: "codex", Healthy: true, Reason: "ok"}, nil
	}
	ctx := context.Background()
	c.checkAll(ctx)
	before := fe.count()

	c.probeClaude = func(context.Context) (Status, error) {
		return Status{}, errors.New("context deadline exceeded")
	}
	c.checkAll(ctx)
	if !c.IsHealthy("claude") {
		t.Fatalf("first generic probe_error should be suppressed; claude became unhealthy")
	}
	if got := c.Reason("claude"); got != "ok" {
		t.Fatalf("first generic probe_error should preserve prior reason, got %q", got)
	}
	if got := fe.count(); got != before {
		t.Fatalf("first generic probe_error emitted %d new events, want 0", got-before)
	}

	c.checkAll(ctx)
	if c.IsHealthy("claude") {
		t.Fatalf("second consecutive generic probe_error should mark claude unhealthy")
	}
	if got := c.Reason("claude"); got != "probe_error" {
		t.Fatalf("Reason = %q, want probe_error", got)
	}
	if got := fe.count(); got != before+1 {
		t.Fatalf("second consecutive probe_error should emit exactly one event: got count=%d before=%d", got, before)
	}
}

func TestChecker_ProbeSuccessResetsProbeErrorStreak(t *testing.T) {
	c, fe, _ := newTestChecker(t)
	c.probeClaude = func(context.Context) (Status, error) {
		return Status{Provider: "claude", Healthy: true, Reason: "ok"}, nil
	}
	c.probeCodex = func(context.Context) (Status, error) {
		return Status{Provider: "codex", Healthy: true, Reason: "ok"}, nil
	}
	ctx := context.Background()
	c.checkAll(ctx)
	before := fe.count()

	c.probeClaude = func(context.Context) (Status, error) {
		return Status{}, errors.New("context deadline exceeded")
	}
	c.checkAll(ctx)

	c.probeClaude = func(context.Context) (Status, error) {
		return Status{Provider: "claude", Healthy: true, Reason: "ok"}, nil
	}
	c.checkAll(ctx)

	c.probeClaude = func(context.Context) (Status, error) {
		return Status{}, errors.New("context deadline exceeded")
	}
	c.checkAll(ctx)

	if !c.IsHealthy("claude") {
		t.Fatalf("non-consecutive generic probe_error should stay suppressed")
	}
	if got := fe.count(); got != before {
		t.Fatalf("suppressed non-consecutive probe_error emitted %d new events, want 0", got-before)
	}
}

func TestChecker_SuppressedProbeErrorAdvancesLastCheck(t *testing.T) {
	c, _, clock := newTestChecker(t)
	c.probeClaude = func(context.Context) (Status, error) {
		return Status{Provider: "claude", Healthy: true, Reason: "ok"}, nil
	}
	c.probeCodex = func(context.Context) (Status, error) {
		return Status{Provider: "codex", Healthy: true, Reason: "ok"}, nil
	}
	ctx := context.Background()
	c.checkAll(ctx)
	healthyAt := c.Snapshot()["claude"].LastCheck

	clock.advance(time.Minute)
	c.probeClaude = func(context.Context) (Status, error) {
		return Status{}, errors.New("context deadline exceeded")
	}
	c.checkAll(ctx)

	snap := c.Snapshot()["claude"]
	if !snap.Healthy || snap.Reason != "ok" {
		t.Fatalf("suppressed probe_error must not flip status: healthy=%v reason=%q", snap.Healthy, snap.Reason)
	}
	if !snap.LastCheck.After(healthyAt) {
		t.Fatalf("suppressed probe_error should advance LastCheck: got %v, want after %v", snap.LastCheck, healthyAt)
	}
}

func TestChecker_SetProviderEnabledNoOpDoesNotEmit(t *testing.T) {
	c, fe, _ := newTestChecker(t)
	// claude is enabled by default — re-enabling is a no-op and must not emit.
	c.SetProviderEnabled("claude", true)
	if got := fe.count(); got != 0 {
		t.Fatalf("no-op enable emitted %d events, want 0", got)
	}

	// Disabling is a real change — one event.
	c.SetProviderEnabled("claude", false)
	if got := fe.count(); got != 1 {
		t.Fatalf("disable emitted %d events, want 1", got)
	}
	// Disabling again is a no-op — no further events.
	c.SetProviderEnabled("claude", false)
	if got := fe.count(); got != 1 {
		t.Fatalf("repeated disable emitted %d events, want 1", got)
	}
}

func TestChecker_BatchFailoverFlagsUseFinalStatuses(t *testing.T) {
	c, fe, _ := newTestChecker(t)
	c.probeClaude = func(context.Context) (Status, error) {
		return Status{Provider: "claude", Healthy: true, Reason: "ok"}, nil
	}
	c.probeCodex = func(context.Context) (Status, error) {
		return Status{Provider: "codex", Healthy: true, Reason: "ok"}, nil
	}
	ctx := context.Background()
	c.checkAll(ctx)
	seed := fe.count()

	c.probeClaude = func(context.Context) (Status, error) {
		return Status{Provider: "claude", Healthy: false, Reason: "logged_out"}, nil
	}
	c.probeCodex = func(context.Context) (Status, error) {
		return Status{Provider: "codex", Healthy: false, Reason: "logged_out"}, nil
	}
	c.checkAll(ctx)

	events := fe.snapshot()[seed:]
	if len(events) != 2 {
		t.Fatalf("events after batch failure = %d, want 2: %#v", len(events), events)
	}
	for _, ev := range events {
		if ev.FailoverActive {
			t.Fatalf("%s emitted stale failoverActive=true even though all enabled peers failed in the same probe batch: %#v", ev.Provider, ev)
		}
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

func TestFailover_PriorityChainCopilotLast(t *testing.T) {
	fe := &fakeEmitter{}
	c := New(Config{
		Interval:       time.Minute,
		ClaudeEnabled:  true,
		CodexEnabled:   true,
		CopilotEnabled: true,
		AutoFailover:   true,
	}, fe.emit, nil)
	healthy := func(p string) { c.setStatus(p, Status{Provider: p, Healthy: true, Reason: "ok"}, true) }
	down := func(p string) { c.setStatus(p, Status{Provider: p, Healthy: false, Reason: "logged_out"}, true) }
	healthy("claude")
	healthy("codex")
	healthy("copilot")

	// Copilot is never preferred while a higher-priority peer is healthy.
	if got := c.Failover("codex"); got != "claude" {
		t.Errorf("codex down, claude healthy → want claude, got %q", got)
	}
	if got := c.Failover("copilot"); got != "claude" {
		t.Errorf("copilot down → want claude (highest), got %q", got)
	}

	// claude down → codex (next in priority), still not copilot.
	down("claude")
	if got := c.Failover("claude"); got != "codex" {
		t.Errorf("claude down → want codex, got %q", got)
	}

	// claude + codex down → copilot is the only healthy peer left.
	down("codex")
	if got := c.Failover("claude"); got != "copilot" {
		t.Errorf("claude+codex down → want copilot, got %q", got)
	}

	// copilot can itself fail over to a recovered higher-priority peer.
	healthy("claude")
	if got := c.Failover("copilot"); got != "claude" {
		t.Errorf("copilot down, claude back → want claude, got %q", got)
	}

	// Disabled peers are skipped even when healthy.
	c.SetProviderEnabled("claude", false)
	down("codex")
	if got := c.Failover("codex"); got != "copilot" {
		t.Errorf("claude disabled, codex down → want copilot, got %q", got)
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
