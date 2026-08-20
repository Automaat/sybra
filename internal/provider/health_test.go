package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/clock"
	"github.com/Automaat/sybra/internal/providerid"
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
		{"equal", "0.145.0", true},
		{"newer_patch", "0.145.1", true},
		{"newer_minor", "0.146.0", true},
		{"newer_major", "1.0.0", true},
		{"older_patch", "0.144.9", false},
		{"older_minor", "0.142.2", false},
		// A missing trailing component reads as zero, so "0.145" == "0.145.0".
		{"shorter_equal_prefix", "0.145", true},
		{"shorter_older_prefix", "0.144", false},
		{"longer_equal_prefix", "0.145.0.1", true},
		{"suffix_tolerated", "0.145.0-beta", true},
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

func TestProbeCodexOldCLIMarksProviderUnhealthy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	body := "#!/bin/sh\n" +
		"case \"$1:$2\" in\n" +
		"  login:status) printf 'Logged in using ChatGPT\\n' ;;\n" +
		"  --version:) printf 'codex-cli 0.142.1\\n' ;;\n" +
		"  *) exit 2 ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	st, err := ProbeCodex(context.Background())
	if err != nil {
		t.Fatalf("ProbeCodex: %v", err)
	}
	if st.Healthy {
		t.Fatal("Healthy = true, want false for an old Codex CLI")
	}
	if st.Reason != "cli_too_old" {
		t.Fatalf("Reason = %q, want cli_too_old", st.Reason)
	}
	if !strings.Contains(st.Detail, "0.142.1") || !strings.Contains(st.Detail, minCodexVersion) {
		t.Fatalf("Detail = %q, want old and minimum versions", st.Detail)
	}
}

// classifierNow pins the classifier tests' clock a few days before the
// "resets Jul 1 at 5pm" fixtures, so the parsed instant is a concrete
// sub-clamp duration instead of depending on the wall clock.
var classifierNow = time.Date(2026, time.June, 28, 12, 0, 0, 0, time.Local)

// julyFirstReset is what the Jul 1 fixtures parse to under classifierNow.
var julyFirstReset = time.Date(2026, time.July, 1, 17, 0, 0, 0, time.Local).Sub(classifierNow)

func TestClassifyClaudeError(t *testing.T) {
	// Pin the clock: several fixtures carry "resets Jul 1 at 5pm", which is now
	// parsed into a concrete instant rather than falling back to the fixed
	// cooldown. julyFirstReset is that instant relative to the pinned now.
	pinNow(t, classifierNow)

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
		{"content_weekly_limit", ErrorSample{Content: "You've hit your weekly limit · resets Jul 1 at 5pm"}, SignalRateLimit, "weekly_limit", julyFirstReset},
		{"stderr_weekly_limit", ErrorSample{Stderr: "You've hit your weekly limit · resets Jul 1 at 5pm"}, SignalRateLimit, "weekly_limit", julyFirstReset},
		{"weekly_word_without_specific_phrase_stays_rate_limited", ErrorSample{Content: "usage limit reached · resets weekly on Monday"}, SignalRateLimit, "rate_limited", 0},
		{"clean_content_rate_limit_exceeded", ErrorSample{Content: "rate limit exceeded", ContentIsCleanResult: true}, SignalRateLimit, "rate_limited", 0},
		{"clean_content_rate_limit_reached", ErrorSample{Content: "rate limit reached", ContentIsCleanResult: true}, SignalRateLimit, "rate_limited", 0},
		{"clean_content_weekly_limit", ErrorSample{Content: "you've hit your weekly limit", ContentIsCleanResult: true}, SignalRateLimit, "weekly_limit", weeklyLimitCooldown},
		{"clean_content_mentions_rate_limit", ErrorSample{Content: "fixed rate limit handling", ContentIsCleanResult: true}, SignalNone, "", 0},
		{"clean_content_mentions_weekly_limit_bare", ErrorSample{Content: "implemented workflow fallback for weekly limit handling", ContentIsCleanResult: true}, SignalNone, "", 0},
		{"overloaded_ignored", ErrorSample{ErrorStatus: 529, ErrorType: "overloaded_error"}, SignalNone, "", 0},
		{"unrelated", ErrorSample{Stderr: "random crash"}, SignalNone, "", 0},
		{
			"structured_429_with_weekly_content_stays_weekly",
			ErrorSample{ErrorStatus: 429, Content: "You've hit your weekly limit · resets Jul 1 at 5pm"},
			SignalRateLimit, "weekly_limit", julyFirstReset,
		},
		{
			"structured_rate_limit_error_type_with_weekly_stderr_stays_weekly",
			ErrorSample{ErrorType: "rate_limit_error", Stderr: "you've hit your weekly limit"},
			SignalRateLimit, "weekly_limit", weeklyLimitCooldown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ClassifyClaudeError(tc.in)
			got, reason, retryAfter, _ := c.Signal, c.Reason, c.RetryAfter, c.Source
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
	// Pin the clock: several fixtures carry "resets Jul 1 at 5pm", which is now
	// parsed into a concrete instant rather than falling back to the fixed
	// cooldown. julyFirstReset is that instant relative to the pinned now.
	pinNow(t, classifierNow)

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
		{
			"bare_host_without_connectivity_phrase_is_none",
			ErrorSample{Stderr: "unexpected error calling chatgpt.com/backend-api/codex/responses: internal server error"},
			SignalNone, "", 0,
		},
		{"unrelated", ErrorSample{Stderr: "panic goroutine"}, SignalNone, "", 0},
		{
			"clean_result_mentioning_backend_host_is_none",
			ErrorSample{Content: "fixed the retry handling for wss://chatgpt.com/backend-api/codex/responses and failed to refresh available models errors", ContentIsCleanResult: true},
			SignalNone, "", 0,
		},
		{
			"clean_content_mentions_weekly_limit_bare",
			ErrorSample{Content: "implemented workflow fallback for weekly limit handling", ContentIsCleanResult: true},
			SignalNone, "", 0,
		},
		{
			"structured_429_with_weekly_content_stays_weekly",
			ErrorSample{ErrorStatus: 429, Content: "You've hit your weekly limit · resets Jul 1 at 5pm"},
			SignalRateLimit, "weekly_limit", julyFirstReset,
		},
		{
			"structured_rate_limit_type_with_weekly_stderr_stays_weekly",
			ErrorSample{ErrorType: "rate_limit", Stderr: "you've hit your weekly limit"},
			SignalRateLimit, "weekly_limit", weeklyLimitCooldown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ClassifyCodexError(tc.in)
			got, reason, retryAfter, _ := c.Signal, c.Reason, c.RetryAfter, c.Source
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
			c := ClassifyCopilotError(tc.in)
			got, _, _, _ := c.Signal, c.Reason, c.RetryAfter, c.Source
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

// TestNew_SeedsUnhealthyUntilProbed guards against re-introducing the
// incident where an enabled-but-never-probed provider (copilot with no CLI
// on PATH) was seeded Healthy=true and picked as a failover target before
// any probe had run.
func TestNew_SeedsUnhealthyUntilProbed(t *testing.T) {
	fe := &fakeEmitter{}
	c := New(Config{
		ClaudeEnabled:  true,
		CodexEnabled:   true,
		CopilotEnabled: true,
	}, fe.emit, nil)
	for _, p := range []string{"claude", "codex", "copilot"} {
		if c.IsHealthy(p) {
			t.Errorf("%s: seeded healthy before any probe ran", p)
		}
		if got := c.Reason(p); got != "unknown" {
			t.Errorf("%s: seeded reason = %q, want %q", p, got, "unknown")
		}
	}
	if c.IsHealthy("opencode") {
		t.Error("opencode: disabled provider seeded healthy")
	}
	if got := c.Reason("opencode"); got != "disabled" {
		t.Errorf("opencode: seeded reason = %q, want %q", got, "disabled")
	}
}

// TestProbeOnce_ClearsSeedBeforeGateWiring guards the fix for the window
// TestNew_SeedsUnhealthyUntilProbed's seed change opened: a caller wiring
// this Checker into a gate other goroutines immediately consult (see
// app_init.go's initProviderHealth) must be able to get a real status
// synchronously, without waiting for Run's background ticker to fire.
func TestProbeOnce_ClearsSeedBeforeGateWiring(t *testing.T) {
	fe := &fakeEmitter{}
	c := New(Config{ClaudeEnabled: true, CodexEnabled: true}, fe.emit, nil)
	c.probeClaude = func(context.Context) (Status, error) {
		return Status{Provider: "claude", Healthy: true, Reason: "ok"}, nil
	}
	c.probeCodex = func(context.Context) (Status, error) {
		return Status{Provider: "codex", Healthy: false, Reason: "logged_out"}, nil
	}
	if c.IsHealthy("claude") {
		t.Fatal("precondition: claude should read unhealthy before ProbeOnce")
	}
	c.ProbeOnce(context.Background())
	if !c.IsHealthy("claude") {
		t.Error("claude should be healthy after ProbeOnce resolves its probe true")
	}
	if c.IsHealthy("codex") {
		t.Error("codex should stay unhealthy after ProbeOnce resolves its probe false")
	}
}

func newTestChecker(t *testing.T) (*Checker, *fakeEmitter, *clock.Fake) {
	t.Helper()
	fe := &fakeEmitter{}
	fake := clock.NewFake(time.Unix(1_700_000_000, 0).UTC())
	c := New(Config{
		Interval:         time.Minute,
		ClaudeEnabled:    true,
		CodexEnabled:     true,
		AutoFailover:     true,
		ClaudeRLCooldown: 15 * time.Minute,
		CodexRLCooldown:  15 * time.Minute,
	}, fe.emit, nil)
	c.clock = fake
	return c, fe, fake
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
	c, _, fake := newTestChecker(t)
	c.probeClaude = func(context.Context) (Status, error) {
		return Status{Provider: "claude", Healthy: true, Reason: "ok"}, nil
	}
	c.probeCodex = func(context.Context) (Status, error) {
		return Status{Provider: "codex", Healthy: true, Reason: "ok"}, nil
	}
	ctx := context.Background()
	c.checkAll(ctx)
	healthyAt := c.Snapshot()["claude"].LastCheck

	fake.Advance(time.Minute)
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

// TestFailover_NoFixedPriority verifies that with multiple healthy peers
// available, Failover distributes across all of them instead of always
// preferring the same one — copilot in particular must be picked sometimes,
// not systematically last. A single-eligible-candidate case must still
// return deterministically.
func TestFailover_NoFixedPriority(t *testing.T) {
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

	seen := map[string]bool{}
	for range 200 {
		seen[c.Failover("opencode")] = true
	}
	for _, want := range []string{"claude", "codex", "copilot"} {
		if !seen[want] {
			t.Errorf("Failover never picked %q across 200 trials with all peers healthy: %v", want, seen)
		}
	}

	down("claude")
	down("codex")
	if got := c.Failover("claude"); got != "copilot" {
		t.Errorf("claude+codex down → want copilot, got %q", got)
	}

	healthy("claude")
	c.SetProviderEnabled("claude", false)
	if got := c.Failover("claude"); got != "copilot" {
		t.Errorf("claude disabled → want copilot, got %q", got)
	}
}

func TestChecker_PassiveAuthHoldsUntilCooldownExpires(t *testing.T) {
	c, _, fake := newTestChecker(t)
	c.setStatus(providerid.Claude, Status{Provider: providerid.Claude, Healthy: true, Reason: "ok"}, true)
	c.ReportAuthFailure(providerid.Claude, "logged_out")
	if c.IsHealthy(providerid.Claude) {
		t.Fatalf("should be unhealthy after passive signal")
	}
	// A second passive signal should not reset LastCheck needlessly or emit again.
	c.ReportAuthFailure(providerid.Claude, "logged_out")
	if c.IsHealthy(providerid.Claude) {
		t.Fatalf("still unhealthy")
	}
	c.setStatus(providerid.Claude, Status{Provider: providerid.Claude, Healthy: true, Reason: "ok"}, true)
	if c.IsHealthy(providerid.Claude) {
		t.Fatalf("a probe inside the hold window resurrected a provider a run reported logged out")
	}
	if c.RateLimited(providerid.Claude) {
		t.Fatalf("an auth hold must not read as a rate-limit cooldown")
	}
	fake.Advance(16 * time.Minute)
	c.setStatus(providerid.Claude, Status{Provider: providerid.Claude, Healthy: true, Reason: "ok"}, true)
	if !c.IsHealthy(providerid.Claude) {
		t.Fatalf("probe should clear the auth hold once the cooldown expires")
	}
}

func TestChecker_AuthHoldSurvivesAnUnhealthyProbe(t *testing.T) {
	c, _, fake := newTestChecker(t)
	c.setStatus(providerid.Claude, Status{Provider: providerid.Claude, Healthy: true, Reason: "ok"}, true)
	c.ReportAuthFailure(providerid.Claude, "logged_out")
	for _, intervening := range []Status{
		{Provider: providerid.Claude, Healthy: false, Reason: "logged_out"},
		{Provider: providerid.Claude, Healthy: false, Reason: "probe_error", Detail: "exec: no such file"},
		{Provider: providerid.Claude, Healthy: false, Reason: RateLimitReason},
	} {
		c.setStatus(providerid.Claude, intervening, true)
	}
	fake.Advance(5 * time.Minute)
	c.setStatus(providerid.Claude, Status{Provider: providerid.Claude, Healthy: true, Reason: "ok"}, true)
	if c.IsHealthy(providerid.Claude) {
		t.Fatalf("the hold was dropped by an intervening unhealthy probe")
	}
	fake.Advance(11 * time.Minute)
	c.setStatus(providerid.Claude, Status{Provider: providerid.Claude, Healthy: true, Reason: "ok"}, true)
	if !c.IsHealthy(providerid.Claude) {
		t.Fatalf("provider never recovered after the hold expired")
	}
}

func TestChecker_AuthHoldExtendsOnARepeatedRunFailure(t *testing.T) {
	c, _, fake := newTestChecker(t)
	c.setStatus(providerid.Claude, Status{Provider: providerid.Claude, Healthy: true, Reason: "ok"}, true)
	c.ReportAuthFailure(providerid.Claude, "logged_out")
	fake.Advance(10 * time.Minute)
	c.ReportAuthFailure(providerid.Claude, "logged_out")
	fake.Advance(6 * time.Minute)
	c.setStatus(providerid.Claude, Status{Provider: providerid.Claude, Healthy: true, Reason: "ok"}, true)
	if c.IsHealthy(providerid.Claude) {
		t.Fatalf("the second run failure did not extend the hold")
	}
	fake.Advance(10 * time.Minute)
	c.setStatus(providerid.Claude, Status{Provider: providerid.Claude, Healthy: true, Reason: "ok"}, true)
	if !c.IsHealthy(providerid.Claude) {
		t.Fatalf("provider never recovered after the extended hold expired")
	}
}

func TestChecker_LivenessOnlyProbeDoesNotClearPassiveAuthFailure(t *testing.T) {
	for _, provider := range []string{"copilot", "opencode"} {
		t.Run(provider, func(t *testing.T) {
			c, _, _ := newTestChecker(t)
			c.setStatus(provider, Status{Provider: provider, Healthy: true, Reason: "ok"}, true)
			c.ReportAuthFailure(provider, "logged_out")
			c.setStatus(provider, Status{Provider: provider, Healthy: true, Reason: "ok"}, true)
			if c.IsHealthy(provider) || c.Reason(provider) != "logged_out" {
				t.Fatalf("version-only probe resurrected logged-out %s: healthy=%v reason=%q", provider, c.IsHealthy(provider), c.Reason(provider))
			}
		})
	}
}

func TestChecker_RateLimitExpires(t *testing.T) {
	c, _, fake := newTestChecker(t)
	c.setStatus("claude", Status{Provider: "claude", Healthy: true, Reason: "ok"}, true)
	c.ReportRateLimit("claude", 10*time.Minute, "rate_limit_error", CooldownFromConfig)
	if c.IsHealthy("claude") {
		t.Fatalf("claude should be rate-limited")
	}
	fake.Advance(11 * time.Minute)
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
	c, _, fake := newTestChecker(t)
	c.setStatus("claude", Status{Provider: "claude", Healthy: true, Reason: "ok"}, true)

	// Mark rate-limited for 10m.
	c.ReportRateLimit("claude", 10*time.Minute, "rate_limit_error", CooldownFromConfig)
	if c.IsHealthy("claude") {
		t.Fatalf("claude should be rate-limited immediately after ReportRateLimit")
	}

	// Advance only 1 minute, well within the window.
	fake.Advance(1 * time.Minute)

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
	fake.Advance(20 * time.Minute)
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

// TestNilChecker_IsAnAbsentGateNotAPanic pins the nil-receiver contract of the
// HealthGate surface.
//
// providers.health_check.enabled=false leaves App.providerHealth nil, and every
// caller boxes it into a provider.HealthGate. An interface holding a nil
// pointer is not itself nil, so the callers' `if gate != nil` guards cannot
// catch it — these methods are reached with c == nil and must answer as the
// absent gate the disabled config promises. Before this, they panicked and took
// planning, triage, PR content, umbrella expansion, and the digest with them.
func TestNilChecker_IsAnAbsentGateNotAPanic(t *testing.T) {
	t.Parallel()

	// govet's nilness proves `gate == nil` is impossible here, which is the
	// whole trap in one line: a caller's nil check cannot save it either, and
	// the compiler will not warn them across a function boundary.
	var gate HealthGate = (*Checker)(nil)

	if !gate.IsHealthy("codex") {
		t.Error("IsHealthy = false; an absent gate blocks nothing")
	}
	if gate.RateLimited("codex") {
		t.Error("RateLimited = true; an absent gate knows of no cooldown")
	}
	if got := gate.Reason("codex"); got != "" {
		t.Errorf("Reason = %q, want empty", got)
	}
	if got := gate.Failover("codex"); got != "" {
		t.Errorf("Failover = %q, want empty: an absent gate has no view to fail over with", got)
	}
	gate.ReportAuthFailure("codex", "logged_out")
	gate.ReportRateLimit("codex", time.Minute, "429", CooldownFromConfig)
}

func TestChecker_AuthHoldKeepsDispatchOnAPeer(t *testing.T) {
	c, _, fake := newTestChecker(t)
	c.setStatus(providerid.Claude, Status{Provider: providerid.Claude, Healthy: true, Reason: "ok"}, true)
	c.setStatus(providerid.Codex, Status{Provider: providerid.Codex, Healthy: true, Reason: "ok"}, true)
	c.ReportAuthFailure(providerid.Claude, "logged_out")
	c.setStatus(providerid.Claude, Status{Provider: providerid.Claude, Healthy: true, Reason: "ok"}, true)
	if c.IsHealthy(providerid.Claude) {
		t.Fatalf("claude is schedulable again, so no dispatch would ever reach a peer")
	}
	if got := c.Failover(providerid.Claude); got != providerid.Codex {
		t.Fatalf("failover = %q, want codex while claude is held out", got)
	}
	fake.Advance(16 * time.Minute)
	c.setStatus(providerid.Claude, Status{Provider: providerid.Claude, Healthy: true, Reason: "ok"}, true)
	if !c.IsHealthy(providerid.Claude) {
		t.Fatalf("claude never returned after the hold expired")
	}
}

func TestChecker_AuthHoldReachesTheHealthEvent(t *testing.T) {
	c, fe, _ := newTestChecker(t)
	c.setStatus(providerid.Claude, Status{Provider: providerid.Claude, Healthy: true, Reason: "ok"}, true)
	c.ReportAuthFailure(providerid.Claude, "logged_out")
	fe.mu.Lock()
	defer fe.mu.Unlock()
	last := fe.events[len(fe.events)-1]
	if last.Provider != providerid.Claude || last.Healthy {
		t.Fatalf("last event = %+v, want an unhealthy claude event", last)
	}
	if last.AuthHeldUntil.IsZero() {
		t.Fatal("health event omits authHeldUntil, so an operator cannot see when the provider returns")
	}
	if !last.AuthHeldUntil.After(last.LastCheck) {
		t.Fatalf("authHeldUntil %v is not after lastCheck %v", last.AuthHeldUntil, last.LastCheck)
	}
}

func TestChecker_AuthHoldSurvivesAnExpiringRateLimit(t *testing.T) {
	c, _, fake := newTestChecker(t)
	c.setStatus(providerid.Claude, Status{Provider: providerid.Claude, Healthy: true, Reason: "ok"}, true)
	c.ReportAuthFailure(providerid.Claude, "logged_out")
	c.ReportRateLimit(providerid.Claude, time.Minute, "429", CooldownFromConfig)
	fake.Advance(90 * time.Second)
	c.clearExpiredRateLimits()
	if c.IsHealthy(providerid.Claude) {
		t.Fatalf("an expiring rate limit released a provider a run reported logged out")
	}
	fake.Advance(15 * time.Minute)
	c.setStatus(providerid.Claude, Status{Provider: providerid.Claude, Healthy: true, Reason: "ok"}, true)
	if !c.IsHealthy(providerid.Claude) {
		t.Fatalf("claude never returned after the hold expired")
	}
}

func TestChecker_LivenessProbeCannotResurrectAfterAReasonChange(t *testing.T) {
	for _, name := range []string{providerid.Copilot, providerid.OpenCode} {
		t.Run(name, func(t *testing.T) {
			c, _, fake := newTestChecker(t)
			c.SetProviderEnabled(name, true)
			c.setStatus(name, Status{Provider: name, Healthy: true, Reason: "ok"}, true)
			c.ReportAuthFailure(name, "logged_out")

			for range 3 {
				c.setStatus(name, Status{Provider: name, Healthy: false, Reason: "probe_error", Detail: "exec: no such file"}, true)
			}
			if got := c.Reason(name); got != "probe_error" {
				t.Fatalf("reason = %q, want the intervening probe errors to have overwritten it", got)
			}

			fake.Advance(30 * time.Minute)
			c.setStatus(name, Status{Provider: name, Healthy: true, Reason: "ok"}, true)

			if c.IsHealthy(name) {
				t.Fatalf("a version-only probe resurrected %s after a run reported it logged out", name)
			}
			if got := c.Reason(name); got != "logged_out" {
				t.Fatalf("reason = %q, want logged_out so the operator sees the real cause", got)
			}
		})
	}
}

func TestChecker_ReEnablingClearsARunAuthFailure(t *testing.T) {
	c, _, fake := newTestChecker(t)
	c.SetProviderEnabled(providerid.Copilot, true)
	c.setStatus(providerid.Copilot, Status{Provider: providerid.Copilot, Healthy: true, Reason: "ok"}, true)
	c.ReportAuthFailure(providerid.Copilot, "logged_out")
	fake.Advance(16 * time.Minute)

	c.SetProviderEnabled(providerid.Copilot, false)
	c.SetProviderEnabled(providerid.Copilot, true)
	c.setStatus(providerid.Copilot, Status{Provider: providerid.Copilot, Healthy: true, Reason: "ok"}, true)

	if !c.IsHealthy(providerid.Copilot) {
		t.Fatalf("re-enabling the provider left it held out: reason=%q", c.Reason(providerid.Copilot))
	}
}

func TestChecker_ExpiringRateLimitDoesNotReleaseARunAuthFailure(t *testing.T) {
	c, _, fake := newTestChecker(t)
	c.SetProviderEnabled(providerid.Copilot, true)
	c.setStatus(providerid.Copilot, Status{Provider: providerid.Copilot, Healthy: true, Reason: "ok"}, true)
	c.ReportAuthFailure(providerid.Copilot, "logged_out")
	c.ReportRateLimit(providerid.Copilot, time.Minute, "429", CooldownFromConfig)

	fake.Advance(16 * time.Minute)
	c.clearExpiredRateLimits()

	if c.IsHealthy(providerid.Copilot) {
		t.Fatalf("an expiring rate limit released a provider a run found logged out: reason=%q", c.Reason(providerid.Copilot))
	}
}

func TestChecker_BlockedLivenessProbeEmitsOneFlipAndKeepsDetailHonest(t *testing.T) {
	c, fe, _ := newTestChecker(t)
	c.SetProviderEnabled(providerid.Copilot, true)
	c.setStatus(providerid.Copilot, Status{Provider: providerid.Copilot, Healthy: true, Reason: "ok"}, true)
	c.ReportAuthFailure(providerid.Copilot, "logged_out")
	c.setStatus(providerid.Copilot, Status{Provider: providerid.Copilot, Healthy: false, Reason: "probe_error", Detail: "exec: no such file"}, true)

	before := fe.count()
	c.setStatus(providerid.Copilot, Status{Provider: providerid.Copilot, Healthy: true, Reason: "ok"}, true)
	if got := fe.count() - before; got != 1 {
		t.Fatalf("flips on the blocked probe = %d, want exactly 1", got)
	}
	for range 3 {
		c.setStatus(providerid.Copilot, Status{Provider: providerid.Copilot, Healthy: true, Reason: "ok"}, true)
	}
	if got := fe.count() - before; got != 1 {
		t.Fatalf("flips after repeated blocked probes = %d, want the first one only", got)
	}

	snap := c.Snapshot()[providerid.Copilot]
	if snap.Reason != "logged_out" {
		t.Fatalf("reason = %q, want logged_out", snap.Reason)
	}
	if snap.Detail != "" {
		t.Fatalf("detail = %q, want no unrelated explanation beside a logged_out reason", snap.Detail)
	}
}

func TestChecker_ReEnablingDoesNotCancelTheAuthCooldown(t *testing.T) {
	c, _, _ := newTestChecker(t)
	c.setStatus(providerid.Claude, Status{Provider: providerid.Claude, Healthy: true, Reason: "ok"}, true)
	c.ReportAuthFailure(providerid.Claude, "logged_out")

	c.SetProviderEnabled(providerid.Claude, false)
	c.SetProviderEnabled(providerid.Claude, true)
	c.setStatus(providerid.Claude, Status{Provider: providerid.Claude, Healthy: true, Reason: "ok"}, true)

	if c.IsHealthy(providerid.Claude) {
		t.Fatal("a toggle is not a login: the cooldown was cancelled and dispatch would resume against a logged-out provider")
	}
}

func TestChecker_AuthFailureUnderAnyReasonStillHolds(t *testing.T) {
	c, _, _ := newTestChecker(t)
	c.SetProviderEnabled(providerid.Copilot, true)
	c.setStatus(providerid.Copilot, Status{Provider: providerid.Copilot, Healthy: true, Reason: "ok"}, true)
	c.ReportAuthFailure(providerid.Copilot, "invalid_api_key")
	c.setStatus(providerid.Copilot, Status{Provider: providerid.Copilot, Healthy: true, Reason: "ok"}, true)

	if c.IsHealthy(providerid.Copilot) {
		t.Fatal("an auth failure reported under another reason left the provider schedulable")
	}
	if got := c.Reason(providerid.Copilot); got != "logged_out" {
		t.Fatalf("reason = %q, want the caller's wording kept out of the key the guards read", got)
	}
}

func TestChecker_BlockedProbeKeepsTheAuthDetail(t *testing.T) {
	c, _, _ := newTestChecker(t)
	c.SetProviderEnabled(providerid.Copilot, true)
	c.setStatus(providerid.Copilot, Status{Provider: providerid.Copilot, Healthy: true, Reason: "ok"}, true)
	c.ReportAuthFailure(providerid.Copilot, "invalid_api_key")

	c.setStatus(providerid.Copilot, Status{Provider: providerid.Copilot, Healthy: true, Reason: "ok"}, true)

	snap := c.Snapshot()[providerid.Copilot]
	if snap.Detail != "invalid_api_key" {
		t.Fatalf("detail = %q, want the caller's explanation of the auth failure kept", snap.Detail)
	}
}
