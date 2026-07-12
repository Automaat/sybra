package agent

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/limits"
	"github.com/Automaat/sybra/internal/provider"
)

// fakeGate lets a test control what Manager sees from the health gate without
// spinning up a real Checker.
type fakeGate struct {
	healthy  map[string]bool
	failover map[string]string
	reasons  map[string]string

	reportedAuth    []string
	reportedRLName  []string
	reportedRLDelay []time.Duration
}

func (f *fakeGate) IsHealthy(p string) bool       { return f.healthy[p] }
func (f *fakeGate) RateLimited(p string) bool     { return !f.healthy[p] }
func (f *fakeGate) Failover(p string) string      { return f.failover[p] }
func (f *fakeGate) Reason(p string) string        { return f.reasons[p] }
func (f *fakeGate) ReportAuthFailure(p, _ string) { f.reportedAuth = append(f.reportedAuth, p) }
func (f *fakeGate) ReportRateLimit(p string, d time.Duration, _ string) {
	f.reportedRLName = append(f.reportedRLName, p)
	f.reportedRLDelay = append(f.reportedRLDelay, d)
}

func TestGateProvider_HealthyPassesThrough(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"claude": true}})
	got, err := m.gateProvider(RunConfig{Provider: "claude"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "claude" {
		t.Errorf("got %q, want claude", got)
	}
}

func TestGateProvider_UnhealthyWithFailover(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{
		healthy:  map[string]bool{"claude": false, "codex": true},
		failover: map[string]string{"claude": "codex"},
		reasons:  map[string]string{"claude": "logged_out"},
	})
	got, err := m.gateProvider(RunConfig{Provider: "claude"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "codex" {
		t.Errorf("expected failover to codex, got %q", got)
	}
}

// TestPrepareRunConfig_InteractiveFailsOverLikeHeadless is a regression guard
// for the "interactive doesn't fail over" premise: it does. Both modes resolve
// the provider through the same prepareRunConfig -> gateProvider path, so an
// unhealthy requested provider fails over to a healthy peer regardless of mode.
// If a future change re-adds a provider-pinned interactive launcher that skips
// the gate, the interactive subtest fails.
func TestPrepareRunConfig_InteractiveFailsOverLikeHeadless(t *testing.T) {
	for _, mode := range []string{"headless", "interactive"} {
		t.Run(mode, func(t *testing.T) {
			m, _ := newTestManager(t)
			m.SetHealthGate(&fakeGate{
				healthy:  map[string]bool{"claude": false, "codex": true},
				failover: map[string]string{"claude": "codex"},
				reasons:  map[string]string{"claude": "rate_limited"},
			})
			cfg, prov, err := m.prepareRunConfig(RunConfig{
				Provider: "claude",
				Mode:     mode,
				Dir:      t.TempDir(),
			})
			if err != nil {
				t.Fatalf("prepareRunConfig(%s): %v", mode, err)
			}
			if prov.Name() != "codex" {
				t.Errorf("mode %s: resolved provider = %q, want codex (failover)", mode, prov.Name())
			}
			if cfg.provider == nil || cfg.provider.Name() != "codex" {
				t.Errorf("mode %s: cfg.provider = %v, want codex", cfg.provider, "codex")
			}
		})
	}
}

// TestPrepareRunConfig_AppendsBackgroundTaskGuardrailForHeadlessCodeAuthor
// locks in that prepareRunConfig — the single chokepoint every headless and
// interactive run passes through — wires the background-task guardrail into
// the final prompt for a headless, code-authoring run (SeedWorkingMemory),
// and leaves other shapes untouched.
func TestPrepareRunConfig_AppendsBackgroundTaskGuardrailForHeadlessCodeAuthor(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"claude": true}})

	cfg, _, err := m.prepareRunConfig(RunConfig{
		Provider:          "claude",
		Mode:              "headless",
		Prompt:            "implement the task",
		SeedWorkingMemory: true,
		Dir:               t.TempDir(),
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}
	if cfg.Prompt == "implement the task" {
		t.Fatal("expected background-task guardrail to be appended to a headless code-author prompt")
	}

	cfg, _, err = m.prepareRunConfig(RunConfig{
		Provider:          "claude",
		Mode:              "headless",
		Prompt:            "review the diff",
		SeedWorkingMemory: false,
		Dir:               t.TempDir(),
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}
	if cfg.Prompt != "review the diff" {
		t.Errorf("verifier role (SeedWorkingMemory=false) should not get the guardrail, got: %q", cfg.Prompt)
	}
}

// TestPrepareRunConfig_HeadlessSteerableGatedByRole is a regression guard for
// #1825: the review/human-review/fix-review dispatch paths hardcode
// Mode: "headless" for unattended, poller-driven runs that nothing ever steers.
// Forcing the global headless_steerable transport onto that dispatch left the
// process waiting on a FIFO stdin no caller intended to feed. Only roles a
// human may actively watch and steer from the GUI should get the transport.
func TestPrepareRunConfig_HeadlessSteerableGatedByRole(t *testing.T) {
	m, _ := newTestManager(t, ManagerConfig{
		Runtime:     ManagerRuntimeConfig{HeadlessSteerable: true},
		SandboxHome: testSandboxHome(t),
	})
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"claude": true}})

	tests := []struct {
		name string
		want bool
	}{
		{RoleImplementation.AgentName("Impl"), true},
		{"", true}, // legacy unprefixed name maps to implementation
		{RolePlan.AgentName("Plan"), true},
		{RolePRFix.AgentName("PR Fix"), true},
		{RoleReview.AgentName("Review"), false},
		{RoleFixReview.AgentName("Fix"), false},
		{RoleHumanReview.AgentName("Diagnose"), false},
		{RoleTestRunner.AgentName("Test"), false},
		{RoleEval.AgentName("Eval"), false},
		{RoleTriage.AgentName("Triage"), false},
		{RolePlanCritic.AgentName("Critic"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, _, err := m.prepareRunConfig(RunConfig{
				Provider: "claude",
				Mode:     "headless",
				Name:     tt.name,
				Dir:      t.TempDir(),
			})
			if err != nil {
				t.Fatalf("prepareRunConfig: %v", err)
			}
			if cfg.HeadlessSteerable != tt.want {
				t.Errorf("Name=%q: HeadlessSteerable = %v, want %v", tt.name, cfg.HeadlessSteerable, tt.want)
			}
		})
	}
}

func TestGateProvider_BothUnhealthyReturnsTypedError(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{
		healthy: map[string]bool{"claude": false, "codex": false},
		reasons: map[string]string{"claude": "rate_limited"},
	})
	_, err := m.gateProvider(RunConfig{Provider: "claude"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, provider.ErrProviderUnhealthy) {
		t.Fatalf("error should match ErrProviderUnhealthy: %v", err)
	}
	ue, ok := errors.AsType[*provider.UnhealthyError](err)
	if !ok {
		t.Fatalf("error should unwrap to UnhealthyError")
	}
	if ue.Provider != "claude" || ue.Reason != "rate_limited" {
		t.Errorf("unexpected UnhealthyError fields: %+v", ue)
	}
}

func TestGateProvider_IgnoreHealthGateBypasses(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"claude": false}})
	got, err := m.gateProvider(RunConfig{Provider: "claude", IgnoreHealthGate: true})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "claude" {
		t.Errorf("got %q, want claude", got)
	}
}

// fakeLimitGate lets a test control ProviderAvailable/ChooseProvider
// decisions without a real limits.Store.
type fakeLimitGate struct {
	available      map[string]bool
	reasons        map[string]string
	chooseReason   string
	chooseNone     bool // force ChooseProvider to find no fully-available peer
	softPeer       string
	softPeerReason string
}

func (f *fakeLimitGate) ProviderAvailable(p string, _ limits.Policy) (available bool, reason string) {
	if f.available == nil {
		return true, ""
	}
	if ok, exists := f.available[p]; exists {
		return ok, f.reasons[p]
	}
	return true, ""
}

func (f *fakeLimitGate) ChooseProvider(requested string, candidates []string, healthy func(string) bool, _ limits.Policy) (chosen, reason string) {
	if f.chooseNone {
		return "", ""
	}
	for _, c := range candidates {
		if c == requested {
			continue
		}
		if healthy(c) {
			return c, f.chooseReason
		}
	}
	return "", ""
}

func (f *fakeLimitGate) ChooseSoftLimitedPeer(requested string, candidates []string, healthy func(string) bool, _ limits.Policy) (chosen, reason string) {
	if f.softPeer == "" || f.softPeer == requested {
		return "", ""
	}
	for _, c := range candidates {
		if c == f.softPeer && healthy(c) {
			return f.softPeer, f.softPeerReason
		}
	}
	return "", ""
}

// TestGateProvider_CapRedirectsEvenWhenPreferUnderusedFalse verifies the
// per-provider in-flight cap steers dispatch away from an at-cap resolved
// provider regardless of PreferUnderused — otherwise the cap silently no-ops
// on the common "prefer_underused: false" configuration.
func TestGateProvider_CapRedirectsEvenWhenPreferUnderusedFalse(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"claude": true, "codex": true}})
	if err := m.ReplaceRuntimeConfig(ManagerRuntimeConfig{
		DefaultProvider:        "claude",
		LimitGate:              &fakeLimitGate{},
		LimitPolicy:            limits.Policy{PreferUnderused: false},
		MaxInFlightPerProvider: 1,
	}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.liveByProvider["claude"] = 1
	m.mu.Unlock()

	got, err := m.gateProvider(RunConfig{Provider: "claude"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "codex" {
		t.Errorf("got %q, want codex (cap must redirect even with PreferUnderused=false)", got)
	}
}

// TestGateProvider_CapWithNoHealthyAlternativeFallsBackToResolved verifies the
// cap is soft: when no other candidate is healthy, dispatch is never blocked
// — it falls back to the at-cap resolved provider instead.
func TestGateProvider_CapWithNoHealthyAlternativeFallsBackToResolved(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"claude": true}})
	if err := m.ReplaceRuntimeConfig(ManagerRuntimeConfig{
		DefaultProvider:        "claude",
		LimitGate:              &fakeLimitGate{},
		LimitPolicy:            limits.Policy{PreferUnderused: false},
		MaxInFlightPerProvider: 1,
	}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.liveByProvider["claude"] = 1
	m.mu.Unlock()

	got, err := m.gateProvider(RunConfig{Provider: "claude"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "claude" {
		t.Errorf("got %q, want claude (no healthy alt: soft cap must not block dispatch)", got)
	}
}

// TestGateProvider_PreferUnderusedStillUsesChooseProviderUnderCap verifies
// the pre-existing PreferUnderused behavior is preserved when the resolved
// provider is under its cap: selection still goes through the limits store's
// quota-aware ChooseProvider (unlike the at-cap path, which bypasses it).
func TestGateProvider_PreferUnderusedStillUsesChooseProviderUnderCap(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"claude": true, "codex": true}})
	if err := m.ReplaceRuntimeConfig(ManagerRuntimeConfig{
		DefaultProvider: "claude",
		LimitGate:       &fakeLimitGate{chooseReason: "lower quota pressure"},
		LimitPolicy:     limits.Policy{PreferUnderused: true},
		// No cap configured — the resolved provider is always "under cap",
		// so any redirect observed here must come from ChooseProvider.
	}); err != nil {
		t.Fatal(err)
	}

	got, err := m.gateProvider(RunConfig{Provider: "claude"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "codex" {
		t.Errorf("got %q, want codex (PreferUnderused path must still route via ChooseProvider)", got)
	}
}

// TestGateProvider_CapDisabledByDefaultNeverRedirects verifies
// MaxInFlightPerProvider=0 (the default) disables the cap entirely.
func TestGateProvider_CapDisabledByDefaultNeverRedirects(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"claude": true, "codex": true}})
	if err := m.ReplaceRuntimeConfig(ManagerRuntimeConfig{
		DefaultProvider: "claude",
		LimitGate:       &fakeLimitGate{},
		LimitPolicy:     limits.Policy{PreferUnderused: false},
	}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.liveByProvider["claude"] = 1000
	m.mu.Unlock()

	got, err := m.gateProvider(RunConfig{Provider: "claude"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "claude" {
		t.Errorf("got %q, want claude (cap disabled, no redirect expected)", got)
	}
}

// TestResolveProvider_MatchesGateProviderFailover pins ResolveProvider to
// gateProvider's own decision: a caller predicting the provider a Run(cfg)
// call will dispatch to (e.g. to scope resume-session selection) must see
// the same failover target Run itself will use, not the requested/default
// provider. Without this, a caller that assumes the requested provider
// always wins can hand a same-provider session to a run that actually
// failed over to a different provider.
func TestResolveProvider_MatchesGateProviderFailover(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{
		healthy:  map[string]bool{"claude": false, "codex": true},
		failover: map[string]string{"claude": "codex"},
		reasons:  map[string]string{"claude": "logged_out"},
	})
	cfg := RunConfig{Provider: "claude"}

	predicted, err := m.ResolveProvider(cfg)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	actual, err := m.gateProvider(cfg)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if predicted != actual {
		t.Fatalf("ResolveProvider %q disagrees with gateProvider %q", predicted, actual)
	}
	if predicted != "codex" {
		t.Errorf("expected failover to codex, got %q", predicted)
	}
}

func TestLimitPolicy_DefensiveCopiesMaps(t *testing.T) {
	m, _ := newTestManager(t)
	policy := limits.DefaultPolicy()
	policy.ProviderEnabled[limits.ProviderCodex] = true
	policy.SubscriptionMonthlyUSD[limits.ProviderCodex] = 200

	if err := m.ReplaceRuntimeConfig(ManagerRuntimeConfig{
		DefaultProvider: m.DefaultProvider(),
		LimitPolicy:     policy,
	}); err != nil {
		t.Fatal(err)
	}
	policy.ProviderEnabled[limits.ProviderCodex] = false
	policy.SubscriptionMonthlyUSD[limits.ProviderCodex] = 0

	got := m.LimitPolicy()
	if !got.ProviderEnabled[limits.ProviderCodex] {
		t.Fatal("ReplaceRuntimeConfig retained caller-owned ProviderEnabled map")
	}
	if got.SubscriptionMonthlyUSD[limits.ProviderCodex] != 200 {
		t.Fatalf("ReplaceRuntimeConfig retained caller-owned SubscriptionMonthlyUSD map: %+v", got.SubscriptionMonthlyUSD)
	}

	got.ProviderEnabled[limits.ProviderCodex] = false
	got.SubscriptionMonthlyUSD[limits.ProviderCodex] = 0
	again := m.LimitPolicy()
	if !again.ProviderEnabled[limits.ProviderCodex] {
		t.Fatal("LimitPolicy returned manager-owned ProviderEnabled map")
	}
	if again.SubscriptionMonthlyUSD[limits.ProviderCodex] != 200 {
		t.Fatalf("LimitPolicy returned manager-owned SubscriptionMonthlyUSD map: %+v", again.SubscriptionMonthlyUSD)
	}
}

func TestReportProviderSignal_DispatchesByKind(t *testing.T) {
	m, _ := newTestManager(t)
	fg := &fakeGate{healthy: map[string]bool{"claude": true}}
	m.SetHealthGate(fg)
	m.ReportProviderSignal("claude", provider.SignalAuthFailure, "logged_out", 0)
	m.ReportProviderSignal("codex", provider.SignalRateLimit, "rate_limit_error", 30*time.Minute)
	if len(fg.reportedAuth) != 1 || fg.reportedAuth[0] != "claude" {
		t.Errorf("auth report missing: %+v", fg.reportedAuth)
	}
	if len(fg.reportedRLName) != 1 || fg.reportedRLName[0] != "codex" {
		t.Errorf("rate-limit report missing: %+v", fg.reportedRLName)
	}
	if fg.reportedRLDelay[0] != 30*time.Minute {
		t.Errorf("rate-limit delay wrong: %v", fg.reportedRLDelay[0])
	}
}

func TestReportProviderSignal_NilGateSafe(t *testing.T) {
	m, _ := newTestManager(t)
	// Do not call SetHealthGate.
	m.ReportProviderSignal("claude", provider.SignalAuthFailure, "", 0)
}

func TestReportProviderHealthSignal_CleanLimitResultMarksRateLimit(t *testing.T) {
	m, _ := newTestManager(t)
	fg := &fakeGate{healthy: map[string]bool{"claude": true}}
	m.SetHealthGate(fg)
	ag := &Agent{Provider: "claude"}

	sig := m.reportCleanProviderHealthSignal(ag, "", []StreamEvent{
		{Type: "result", Content: "You've hit your weekly limit · resets Jul 1 at 5pm"},
	})

	if sig != provider.SignalRateLimit {
		t.Fatalf("signal = %v, want rate limit", sig)
	}
	if ag.GetErrorKind() != "rate_limit" {
		t.Fatalf("agent error kind = %q, want rate_limit", ag.GetErrorKind())
	}
	if len(fg.reportedRLName) != 1 || fg.reportedRLName[0] != "claude" {
		t.Fatalf("rate-limit report missing: %+v", fg.reportedRLName)
	}
}

func TestLimitGateOrNil_NilStoreYieldsNilInterface(t *testing.T) {
	var store *limits.Store
	gate := LimitGateOrNil(store)
	if gate != nil {
		t.Fatalf("expected nil interface for nil *limits.Store, got %#v", gate)
	}
}

func TestLimitGateOrNil_NonNilStorePassesThrough(t *testing.T) {
	store, err := limits.NewStore(filepath.Join(t.TempDir(), "limits.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	gate := LimitGateOrNil(store)
	if gate == nil {
		t.Fatal("expected non-nil LimitGate for non-nil store")
	}
}

// TestGateProvider_DegradedLimitsStoreNoPanic reproduces a degraded
// limits.Store init (nil *limits.Store) wired through LimitGateOrNil, as
// app_init.go and svc_config.go do. Before the fix, assigning the nil
// *limits.Store directly to the LimitGate interface field produced a
// non-nil interface holding a nil pointer, defeating the manager's
// `lg == nil` guard and panicking on Store.ProviderAvailable's nil
// receiver mutex.
func TestGateProvider_DegradedLimitsStoreNoPanic(t *testing.T) {
	var degradedStore *limits.Store
	m, _ := newTestManager(t, ManagerConfig{
		Runtime: ManagerRuntimeConfig{
			DefaultProvider: "claude",
			LimitGate:       LimitGateOrNil(degradedStore),
			LimitPolicy:     limits.DefaultPolicy(),
		},
	})
	got, err := m.gateProvider(RunConfig{Provider: "claude"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "claude" {
		t.Errorf("got %q, want claude", got)
	}
}

func TestReportProviderHealthSignal_CleanSuccessMentioningRateLimitDoesNotMarkRateLimit(t *testing.T) {
	m, _ := newTestManager(t)
	fg := &fakeGate{healthy: map[string]bool{"claude": true}}
	m.SetHealthGate(fg)
	ag := &Agent{Provider: "claude"}

	sig := m.reportCleanProviderHealthSignal(ag, "", []StreamEvent{
		{Type: "result", Content: "Fixed rate limit handling and added tests."},
	})

	if sig != provider.SignalNone {
		t.Fatalf("signal = %v, want none", sig)
	}
	if ag.GetErrorKind() != "" {
		t.Fatalf("agent error kind = %q, want empty", ag.GetErrorKind())
	}
	if len(fg.reportedRLName) != 0 {
		t.Fatalf("unexpected rate-limit report: %+v", fg.reportedRLName)
	}
}

// TestGateProvider_SoftThresholdLastResortUsesRemainingBudget guards the
// soft-threshold last-resort path: when the requested provider is only blocked
// by a soft reserve threshold (budget remains) and no healthy peer exists to
// fail over to, the run must use the remaining budget rather than gate.
// Otherwise the reserved headroom is stranded and the task escalates with
// usable quota left.
func TestGateProvider_SoftThresholdLastResortUsesRemainingBudget(t *testing.T) {
	for _, reason := range []string{"weekly limit near threshold", "session limit near threshold"} {
		t.Run(reason, func(t *testing.T) {
			m, _ := newTestManager(t)
			m.SetHealthGate(&fakeGate{healthy: map[string]bool{"claude": true}})
			if err := m.ReplaceRuntimeConfig(ManagerRuntimeConfig{
				DefaultProvider: "claude",
				LimitGate: &fakeLimitGate{
					available: map[string]bool{"claude": false},
					reasons:   map[string]string{"claude": reason},
				},
				LimitPolicy: limits.Policy{},
			}); err != nil {
				t.Fatal(err)
			}
			got, err := m.gateProvider(RunConfig{Provider: "claude"})
			if err != nil {
				t.Fatalf("soft threshold with no peer must not gate: %v", err)
			}
			if got != "claude" {
				t.Errorf("got %q, want claude (use remaining budget as last resort)", got)
			}
		})
	}
}

// TestGateProvider_HardLimitStillGatesWithNoPeer verifies a hard block (provider
// actually reports rate limit reached) keeps gating when no peer is available —
// only soft reserve thresholds fall back to the remaining budget.
func TestGateProvider_HardLimitStillGatesWithNoPeer(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"claude": true}})
	if err := m.ReplaceRuntimeConfig(ManagerRuntimeConfig{
		DefaultProvider: "claude",
		LimitGate: &fakeLimitGate{
			available: map[string]bool{"claude": false},
			reasons:   map[string]string{"claude": "provider reports rate limit reached"},
		},
		LimitPolicy: limits.Policy{},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := m.gateProvider(RunConfig{Provider: "claude"})
	if err == nil {
		t.Fatal("hard limit with no peer must gate, got nil error")
	}
	if !errors.Is(err, provider.ErrProviderUnhealthy) {
		t.Fatalf("want ErrProviderUnhealthy, got %v", err)
	}
}

// TestGateProvider_HardLimitFailsOverToSoftLimitedPeer is a regression guard
// for task c61f327a: codex hard rate-limited (ProviderAvailable=false with a
// hard reason) and ChooseProvider finds no fully-available peer, but claude
// is only soft-threshold limited (still safely dispatching its own direct
// runs, per TestGateProvider_SoftThresholdLastResortUsesRemainingBudget).
// Dispatch must fail over to claude as a last resort instead of failing
// closed system-wide.
func TestGateProvider_HardLimitFailsOverToSoftLimitedPeer(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"claude": true, "codex": true}})
	if err := m.ReplaceRuntimeConfig(ManagerRuntimeConfig{
		DefaultProvider: "codex",
		LimitGate: &fakeLimitGate{
			available:      map[string]bool{"codex": false},
			reasons:        map[string]string{"codex": "provider reports rate limit reached"},
			softPeer:       "claude",
			softPeerReason: "session limit near threshold",
		},
		LimitPolicy: limits.Policy{},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := m.gateProvider(RunConfig{Provider: "codex"})
	if err != nil {
		t.Fatalf("hard-limited provider with a soft-limited peer must fail over, got err: %v", err)
	}
	if got != "claude" {
		t.Errorf("got %q, want claude (soft-limited peer used as last-resort failover)", got)
	}
}

// TestGateProvider_SoftThresholdDoesNotFailOverToSoftLimitedPeer ensures the
// new soft-limited-peer last-resort path only applies when the resolved
// provider is hard-blocked. If resolved is itself merely near threshold, it
// should keep using its remaining budget instead of bouncing to another
// soft-limited peer.
func TestGateProvider_SoftThresholdDoesNotFailOverToSoftLimitedPeer(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"claude": true, "codex": true}})
	if err := m.ReplaceRuntimeConfig(ManagerRuntimeConfig{
		DefaultProvider: "codex",
		LimitGate: &fakeLimitGate{
			available:      map[string]bool{"codex": false},
			reasons:        map[string]string{"codex": "session limit near threshold"},
			chooseNone:     true,
			softPeer:       "claude",
			softPeerReason: "weekly limit near threshold",
		},
		LimitPolicy: limits.Policy{},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := m.gateProvider(RunConfig{Provider: "codex"})
	if err != nil {
		t.Fatalf("soft-threshold provider with only a soft-limited peer must keep remaining budget, got err: %v", err)
	}
	if got != "codex" {
		t.Errorf("got %q, want codex (soft-threshold last resort must not fail over)", got)
	}
}

// TestGateProvider_HardLimitFailoverDisabledSkipsSoftLimitedPeer verifies
// DisableProviderFailover suppresses the new soft-limited-peer path too, not
// just the exact-quota ChooseProvider path.
func TestGateProvider_HardLimitFailoverDisabledSkipsSoftLimitedPeer(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"claude": true, "codex": true}})
	if err := m.ReplaceRuntimeConfig(ManagerRuntimeConfig{
		DefaultProvider: "codex",
		LimitGate: &fakeLimitGate{
			available:      map[string]bool{"codex": false},
			reasons:        map[string]string{"codex": "provider reports rate limit reached"},
			softPeer:       "claude",
			softPeerReason: "session limit near threshold",
		},
		LimitPolicy: limits.Policy{},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := m.gateProvider(RunConfig{Provider: "codex", DisableProviderFailover: true})
	if err == nil {
		t.Fatal("DisableProviderFailover must not use the soft-limited peer, got nil error")
	}
	if !errors.Is(err, provider.ErrProviderUnhealthy) {
		t.Fatalf("want ErrProviderUnhealthy, got %v", err)
	}
}

// TestProviderCanFailover_ReportsSoftLimitedPeer guards that ProviderCanFailover
// mirrors resolveProviderDecision's last-resort path: when ChooseProvider finds
// no fully-available peer but a soft-threshold-limited peer exists, failover is
// still possible, so recovery must re-dispatch the stranded in-progress task
// rather than skip it as provider_rate_limited.
func TestProviderCanFailover_ReportsSoftLimitedPeer(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"claude": true, "codex": true}})
	if err := m.ReplaceRuntimeConfig(ManagerRuntimeConfig{
		DefaultProvider: "codex",
		LimitGate: &fakeLimitGate{
			available:      map[string]bool{"codex": false},
			reasons:        map[string]string{"codex": "provider reports rate limit reached"},
			chooseNone:     true,
			softPeer:       "claude",
			softPeerReason: "session limit near threshold",
		},
		LimitPolicy: limits.Policy{},
	}); err != nil {
		t.Fatal(err)
	}
	if !m.ProviderCanFailover("codex") {
		t.Fatal("ProviderCanFailover must report true when only a soft-limited peer is available")
	}
}

func TestProviderCanFailover_SoftThresholdDoesNotReportSoftLimitedPeer(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"claude": true, "codex": true}})
	if err := m.ReplaceRuntimeConfig(ManagerRuntimeConfig{
		DefaultProvider: "codex",
		LimitGate: &fakeLimitGate{
			available:      map[string]bool{"codex": false},
			reasons:        map[string]string{"codex": "session limit near threshold"},
			chooseNone:     true,
			softPeer:       "claude",
			softPeerReason: "weekly limit near threshold",
		},
		LimitPolicy: limits.Policy{},
	}); err != nil {
		t.Fatal(err)
	}
	if m.ProviderCanFailover("codex") {
		t.Fatal("ProviderCanFailover must stay false when the resolved provider is only soft-threshold limited")
	}
}
