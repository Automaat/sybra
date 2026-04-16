package agent

import (
	"errors"
	"testing"
	"time"

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
	var ue *provider.UnhealthyError
	if !errors.As(err, &ue) {
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
