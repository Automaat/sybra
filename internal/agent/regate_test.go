package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/limits"
)

// TestRegateForTurn_HealthyCurrentNoOp verifies that when the agent's current
// provider remains healthy and per-turn-capable, regateForTurn leaves
// everything untouched: no switch, no provider/model/session mutation, no
// live-count move.
func TestRegateForTurn_HealthyCurrentNoOp(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"codex": true}})
	m.mu.Lock()
	m.liveByProvider["codex"] = 1
	m.mu.Unlock()

	a := &Agent{ID: "a1", Provider: "codex", Model: "gpt-5.5"}
	a.SetSessionID("sess-keep")
	cfg := RunConfig{Provider: "codex", TaskID: "t1"}

	got, switched, err := m.regateForTurn(t.Context(), a, cfg, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if switched {
		t.Fatal("expected no switch for a healthy current provider")
	}
	if got.Provider != "codex" {
		t.Errorf("cfg.Provider changed: got %q", got.Provider)
	}
	if a.Provider != "codex" {
		t.Errorf("agent provider changed: got %q", a.Provider)
	}
	if a.GetSessionID() != "sess-keep" {
		t.Errorf("session id must be untouched on no-op, got %q", a.GetSessionID())
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.liveByProvider["codex"] != 1 {
		t.Errorf("liveByProvider must be untouched on no-op, got %+v", m.liveByProvider)
	}
}

// TestRegateForTurn_FailoverToHealthyPeer verifies a capped current provider
// switches to a healthy per-turn-capable peer: provider/model are updated,
// the session is cleared (native resume can't cross providers), and the
// live-count bucket moves from the old provider to the new one.
func TestRegateForTurn_FailoverToHealthyPeer(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{
		healthy: map[string]bool{"codex": false, "copilot": true},
		reasons: map[string]string{"codex": "rate_limited"},
	})
	m.mu.Lock()
	m.liveByProvider["codex"] = 1
	m.mu.Unlock()

	a := &Agent{ID: "a2", Provider: "codex", Model: "gpt-5.5"}
	a.SetSessionID("sess-old")
	a.SetSessionFilePath("/tmp/old-session.jsonl")
	cfg := RunConfig{Provider: "codex", TaskID: "t2"}

	got, switched, err := m.regateForTurn(t.Context(), a, cfg, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !switched {
		t.Fatal("expected a switch to the healthy copilot peer")
	}
	if got.Provider != "copilot" {
		t.Errorf("cfg.Provider = %q, want copilot", got.Provider)
	}
	if a.Provider != "copilot" {
		t.Errorf("agent provider = %q, want copilot", a.Provider)
	}
	if a.GetSessionID() != "" {
		t.Errorf("session id must be cleared on switch, got %q", a.GetSessionID())
	}
	if a.GetSessionFilePath() != "" {
		t.Errorf("session file path must be cleared on switch, got %q", a.GetSessionFilePath())
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	if v, ok := m.liveByProvider["codex"]; ok {
		t.Errorf("codex bucket should be removed after moving its only count, got %d", v)
	}
	if m.liveByProvider["copilot"] != 1 {
		t.Errorf("copilot bucket should carry the moved count, got %+v", m.liveByProvider)
	}
}

// TestRegateForTurn_NoPerTurnPeerRejected verifies that when the only healthy
// alternative is persistent Claude (not per-turn-capable), regateForTurn
// refuses to spawn a turn and marks a rate-limit-compatible error kind so the
// existing reschedule/park behavior stays reachable.
func TestRegateForTurn_NoPerTurnPeerRejected(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{
		healthy: map[string]bool{"codex": false, "claude": true},
		reasons: map[string]string{"codex": "rate_limited"},
	})

	a := &Agent{ID: "a3", Provider: "codex"}
	cfg := RunConfig{Provider: "codex", TaskID: "t3"}

	got, switched, err := m.regateForTurn(t.Context(), a, cfg, nil)
	if err == nil {
		t.Fatal("expected error: claude is not a valid per-turn hot-swap target")
	}
	if switched {
		t.Fatal("switched must be false on rejection")
	}
	if got.Provider != "codex" {
		t.Errorf("cfg must be unmodified on rejection, got provider %q", got.Provider)
	}
	if a.Provider != "codex" {
		t.Errorf("agent provider must be unmodified on rejection, got %q", a.Provider)
	}
	if a.GetErrorKind() != "rate_limit" {
		t.Errorf("expected rate_limit error kind, got %q", a.GetErrorKind())
	}
}

// TestRegateForTurn_SelfCountAware verifies that the agent's own in-flight
// turn (already counted in its provider's live bucket) does not make its own
// healthy provider look "at cap" to itself.
func TestRegateForTurn_SelfCountAware(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"codex": true}})
	if err := m.ReplaceRuntimeConfig(ManagerRuntimeConfig{
		DefaultProvider:        "codex",
		MaxInFlightPerProvider: 1,
	}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.liveByProvider["codex"] = 1 // this agent's own slot
	m.mu.Unlock()

	a := &Agent{ID: "a4", Provider: "codex"}
	cfg := RunConfig{Provider: "codex"}

	_, switched, err := m.regateForTurn(t.Context(), a, cfg, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if switched {
		t.Fatal("agent's own live count must not make its healthy provider look at-cap")
	}
}

// TestRegateForTurn_UsesAgentProviderNotStaleCfg verifies the source of truth
// for "current provider" is a.Provider, not cfg.Provider — cfg carries the
// stale provider from an earlier dispatch/switch (or, after a reattach, an
// essentially empty RunConfig), so trusting it could resurrect a provider the
// agent already switched away from.
func TestRegateForTurn_UsesAgentProviderNotStaleCfg(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{
		healthy: map[string]bool{"codex": false, "copilot": true},
		reasons: map[string]string{"codex": "rate_limited"},
	})

	// Agent already switched to copilot in a prior turn; cfg is stale and
	// still says codex.
	a := &Agent{ID: "a5", Provider: "copilot"}
	cfg := RunConfig{Provider: "codex"}

	got, switched, err := m.regateForTurn(t.Context(), a, cfg, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if switched {
		t.Fatal("copilot is already healthy; must not switch based on stale cfg.Provider")
	}
	if a.Provider != "copilot" {
		t.Errorf("agent provider must remain copilot, got %q", a.Provider)
	}
	_ = got
}

// TestRegateForTurn_LiveByProviderMultiAgentBucket verifies moving a switched
// agent's count out of a shared bucket decrements it instead of deleting it
// when other agents still occupy that provider.
func TestRegateForTurn_LiveByProviderMultiAgentBucket(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{
		healthy: map[string]bool{"codex": false, "copilot": true},
		reasons: map[string]string{"codex": "rate_limited"},
	})
	m.mu.Lock()
	m.liveByProvider["codex"] = 2 // this agent plus one other still on codex
	m.mu.Unlock()

	a := &Agent{ID: "a6", Provider: "codex"}
	_, switched, err := m.regateForTurn(t.Context(), a, RunConfig{Provider: "codex"}, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !switched {
		t.Fatal("expected switch to copilot")
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.liveByProvider["codex"] != 1 {
		t.Errorf("codex bucket should be decremented (other agent remains), got %+v", m.liveByProvider)
	}
	if m.liveByProvider["copilot"] != 1 {
		t.Errorf("copilot bucket should carry the moved count, got %+v", m.liveByProvider)
	}
}

// TestRegateForTurn_LimitGateCappedFailsOverToPeer verifies a soft
// per-provider quota cap (surfaced via LimitGate, independent of the health
// gate) also triggers failover to a healthy per-turn peer.
func TestRegateForTurn_LimitGateCappedFailsOverToPeer(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"codex": true, "copilot": true}})
	if err := m.ReplaceRuntimeConfig(ManagerRuntimeConfig{
		DefaultProvider: "codex",
		LimitGate: &fakeLimitGate{
			available: map[string]bool{"codex": false},
			reasons:   map[string]string{"codex": "quota_exhausted"},
		},
		LimitPolicy: limits.Policy{},
	}); err != nil {
		t.Fatal(err)
	}

	a := &Agent{ID: "a7", Provider: "codex"}
	got, switched, err := m.regateForTurn(t.Context(), a, RunConfig{Provider: "codex"}, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !switched {
		t.Fatal("expected failover on quota-capped current provider")
	}
	if got.Provider != "copilot" {
		t.Errorf("got provider %q, want copilot", got.Provider)
	}
}

// TestRegateForTurn_PersistsSwitchToRegistry verifies a successful switch is
// saved to the survival registry immediately, so a restart before the next
// turn reattaches on the new provider (see reattachPerTurnConvo) instead of
// resurrecting the pre-switch provider/session.
func TestRegateForTurn_PersistsSwitchToRegistry(t *testing.T) {
	regDir := t.TempDir()
	m := mustNewManager(t, t.Context(), func(string, any) {}, discardLogger(), t.TempDir(), ManagerConfig{SurviveRestartDir: regDir})
	m.SetHealthGate(&fakeGate{
		healthy: map[string]bool{"codex": false, "copilot": true},
		reasons: map[string]string{"codex": "rate_limited"},
	})

	a := &Agent{ID: "a8", TaskID: "t8", Provider: "codex", Mode: "interactive"}
	a.SetSessionID("sess-old")

	if _, switched, err := m.regateForTurn(t.Context(), a, RunConfig{Provider: "codex", TaskID: "t8"}, nil); err != nil || !switched {
		t.Fatalf("regateForTurn: switched=%v err=%v", switched, err)
	}

	recs, err := m.reg.List()
	if err != nil {
		t.Fatalf("registry List: %v", err)
	}
	var rec *Record
	for i := range recs {
		if recs[i].ID == "a8" {
			rec = &recs[i]
		}
	}
	if rec == nil {
		t.Fatal("expected switch to persist a registry record")
	}
	if rec.Provider != "copilot" {
		t.Errorf("persisted provider = %q, want copilot", rec.Provider)
	}
	if rec.SessionID != "" {
		t.Errorf("persisted session id must be cleared, got %q", rec.SessionID)
	}
}

// TestRegateForTurn_WritesMarkerBeforePersistingSwitch verifies the
// convoProviderMarker line lands in the log before the switch is persisted to
// the registry, closing the crash window where a restart between the two
// would leave rehydratePerTurnConvoFromLog with a persisted provider but no
// marker to bound the pre-switch segment.
func TestRegateForTurn_WritesMarkerBeforePersistingSwitch(t *testing.T) {
	regDir := t.TempDir()
	m := mustNewManager(t, t.Context(), func(string, any) {}, discardLogger(), t.TempDir(), ManagerConfig{SurviveRestartDir: regDir})
	m.SetHealthGate(&fakeGate{
		healthy: map[string]bool{"codex": false, "copilot": true},
		reasons: map[string]string{"codex": "rate_limited"},
	})

	a := &Agent{ID: "a9", TaskID: "t9", Provider: "codex", Mode: "interactive"}
	var buf bytes.Buffer

	if _, switched, err := m.regateForTurn(t.Context(), a, RunConfig{Provider: "codex", TaskID: "t9"}, &buf); err != nil || !switched {
		t.Fatalf("regateForTurn: switched=%v err=%v", switched, err)
	}

	provider, ok := parseProviderMarkerLine(bytes.TrimRight(buf.Bytes(), "\n"))
	if !ok {
		t.Fatalf("expected a marker line to be written, got %q", buf.String())
	}
	if provider != "copilot" {
		t.Errorf("marker provider = %q, want copilot", provider)
	}
}

// TestRegateForTurn_PreservesRequirePermissionsAcrossSwitch verifies a
// provider switch carries the run's sandbox/approval posture
// (RequirePermissions) forward unchanged, so a sandboxed chat stays
// sandboxed on its new provider instead of silently becoming permissive.
func TestRegateForTurn_PreservesRequirePermissionsAcrossSwitch(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{
		healthy: map[string]bool{"copilot": false, "codex": true},
		reasons: map[string]string{"copilot": "rate_limited"},
	})
	a := &Agent{ID: "perm1", Provider: "copilot"}
	cfg := RunConfig{Provider: "copilot", RequirePermissions: true}

	got, switched, err := m.regateForTurn(t.Context(), a, cfg, nil)
	if err != nil || !switched {
		t.Fatalf("switched=%v err=%v", switched, err)
	}
	if !got.RequirePermissions {
		t.Fatal("RequirePermissions must be preserved across a provider switch")
	}

	args := buildCodexConvoArgsWithProvider(a, got, "hi", providerByName("codex"))
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--sandbox workspace-write") {
		t.Fatalf("expected sandboxed codex args after switch (RequirePermissions preserved), got %q", joined)
	}
}

// TestReattachPerTurnConvo_UsesSwitchedProviderAndClearedSession verifies
// that a registry record written after a mid-run regateForTurn switch (new
// provider, cleared session id) reattaches on the switched provider with no
// resurrected old-provider session — regateForTurn already persisted the
// post-switch state via saveRegistry, so ReattachAll only needs to trust it.
func TestReattachPerTurnConvo_UsesSwitchedProviderAndClearedSession(t *testing.T) {
	logDir := t.TempDir()
	regDir := t.TempDir()
	logPath := filepath.Join(logDir, "agents", "sw1.ndjson")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	history := marshalMarkerLine(t, "codex") + "\n" +
		`{"type":"thread.started","thread_id":"cx-1"}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}` + "\n" +
		marshalMarkerLine(t, "copilot") + "\n" +
		`{"type":"assistant.message","data":{"content":"post-switch"}}` + "\n"
	if err := os.WriteFile(logPath, []byte(history), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m := mustNewManager(t, t.Context(), func(string, any) {}, discardLogger(), logDir, ManagerConfig{SurviveRestartDir: regDir})
	// As regateForTurn would have persisted it: switched to copilot, session
	// cleared, requirePermissions carried over from the pre-switch run.
	rec := Record{
		ID: "sw1", TaskID: "t-sw", Mode: "interactive", Provider: "copilot",
		PID: 0, LogPath: logPath, SessionID: "", RequirePermissions: true,
		StartedAt: time.Now().UTC(),
	}
	if err := m.reg.Save(rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	got := m.ReattachAll()
	if len(got) != 1 {
		t.Fatalf("expected 1 reattached agent, got %d", len(got))
	}
	a, err := m.GetAgent("sw1")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if a.Provider != "copilot" {
		t.Errorf("expected reattach to use switched provider copilot, got %q", a.Provider)
	}
	if a.GetSessionID() != "" {
		t.Errorf("expected no resurrected pre-switch session id, got %q", a.GetSessionID())
	}
	if !a.requirePermissions {
		t.Error("expected requirePermissions preserved across the switch+restart")
	}
	// Both segments of the mixed-provider log rehydrate correctly via markers.
	if len(a.ConvoOutput()) != 3 {
		t.Errorf("expected 3 rehydrated events across both provider segments, got %d", len(a.ConvoOutput()))
	}

	if err := m.StopAgent("sw1"); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}
	select {
	case <-a.done:
	case <-time.After(3 * time.Second):
		t.Fatal("reattached agent did not exit after StopAgent")
	}
}

// TestRehydratePerTurnConvoFromLog_MixedProviderMarkers verifies that
// convoProviderMarker lines let rehydration parse each segment of a
// mixed-provider log (written across a mid-run provider switch) with the
// correct provider's schema, even though the agent's current provider only
// matches the LAST segment.
func TestRehydratePerTurnConvoFromLog_MixedProviderMarkers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mixed.ndjson")
	var lines []string
	lines = append(lines, marshalMarkerLine(t, "codex"))
	lines = append(lines, `{"type":"thread.started","thread_id":"cx-1"}`)
	lines = append(lines, `{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":2}}`)
	lines = append(lines, marshalMarkerLine(t, "copilot"))
	lines = append(lines, `{"type":"assistant.message","data":{"content":"hello"}}`)
	lines = append(lines, `{"type":"result","sessionId":"cop-9"}`)
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Agent's CURRENT provider is copilot (post-switch); markers must still
	// let the codex segment parse correctly.
	a := &Agent{ID: "mix", Provider: "copilot"}
	rehydratePerTurnConvoFromLog(a, path)

	events := a.ConvoOutput()
	if len(events) != 4 {
		t.Fatalf("expected 4 rehydrated events (2 codex + 2 copilot), got %d: %+v", len(events), events)
	}
	if a.GetSessionID() != "cop-9" {
		t.Errorf("expected final session id from copilot segment, got %q", a.GetSessionID())
	}
}

// TestRehydratePerTurnConvoFromLog_NoMarkersDropsMismatchedSegment documents
// the fallback behavior for a log written before provider markers existed:
// without a marker, every line is parsed with the agent's single current
// provider, so a segment written under a different (now-switched-away-from)
// provider whose event types don't overlap is silently dropped. This is the
// reason writeProviderMarkerLine exists — see the marker-covered test above
// for the fixed behavior.
func TestRehydratePerTurnConvoFromLog_NoMarkersDropsMismatchedSegment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.ndjson")
	content := `{"type":"thread.started","thread_id":"cx-1"}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":2}}` + "\n" +
		`{"type":"assistant.message","data":{"content":"hello"}}` + "\n" +
		`{"type":"result","sessionId":"cop-9"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	a := &Agent{ID: "legacy", Provider: "copilot"}
	rehydratePerTurnConvoFromLog(a, path)

	events := a.ConvoOutput()
	if len(events) != 2 {
		t.Fatalf("expected only the 2 copilot-parsed events (codex segment dropped without markers), got %d: %+v", len(events), events)
	}
}

func marshalMarkerLine(t *testing.T, provider string) string {
	t.Helper()
	return `{"__sybra_provider_marker__":"provider_switch","provider":"` + provider + `"}`
}
