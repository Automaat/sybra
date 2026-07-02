package agent

import (
	"errors"
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
	store, err := limits.NewStore(t.TempDir() + "/limits.json")
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
