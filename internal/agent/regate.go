package agent

import (
	"context"
	"fmt"
	"io"
	"maps"
)

// regateForTurn re-consults provider health/limit gating for a per-turn
// conversational agent (codex/copilot) immediately before it starts a new
// turn's process. Dispatch-time gating (prepareRunConfig/gateProvider) only
// runs once, at Run(); a provider that caps mid-conversation would otherwise
// strand the agent on a dead provider for the rest of the chat. This is the
// per-turn analogue, called from runPerTurnConversational before every turn.
//
// It differs from gateProvider in two ways required by an already-running
// agent: it is self-count-aware (the agent's own turn already occupies a
// slot in its current provider's live bucket, so that bucket must not judge
// itself "at cap"), and candidate providers are restricted to ones where
// Provider.UsesPerTurnConvo() is true — persistent Claude is never a valid
// hot-swap target for this runner.
//
// Returns the (possibly updated) RunConfig, whether a provider switch
// occurred, and a non-nil error only when the current provider is unhealthy
// and no per-turn-capable peer is usable. On that error path cfg is returned
// unmodified and the caller must not spawn a turn; the agent's error kind is
// set to "rate_limit" so the existing isRateLimitedRun /
// RescheduleRateLimitedAgent reschedule-and-park behavior stays reachable,
// exactly as it would for an in-flight run that hit the same cap.
//
// logWriter, if non-nil, receives the convoProviderMarker line for a switch
// before the registry is persisted (saveRegistry below) — writing the
// marker first closes the crash window where a restart between a persisted
// switch and a not-yet-written marker would make rehydratePerTurnConvoFromLog
// parse the entire pre-switch segment under the new provider's schema.
func (m *Manager) regateForTurn(ctx context.Context, a *Agent, cfg RunConfig, logWriter io.Writer) (RunConfig, bool, error) {
	current := a.Provider

	m.mu.RLock()
	g := m.gate
	lg := m.limitGate
	lp := m.limitPolicy
	maxInFlight := m.maxInFlightPerProvider
	live := maps.Clone(m.liveByProvider)
	m.mu.RUnlock()

	// Self-count-aware: this agent's own in-flight turn already occupies one
	// slot in current's live bucket. Exclude it so a healthy current provider
	// is never wrongly judged "at cap" by itself.
	if live[current] > 0 {
		live[current]--
	}

	underCap := func(p string) bool {
		return maxInFlight <= 0 || live[p] < maxInFlight
	}
	perTurnCapable := func(p string) bool {
		prov, err := lookupProvider(p)
		return err == nil && prov.UsesPerTurnConvo()
	}
	limitAvailable := func(p string) bool {
		if lg == nil {
			return true
		}
		ok, _ := lg.ProviderAvailable(p, lp)
		return ok
	}
	healthy := func(p string) bool {
		return perTurnCapable(p) && (g == nil || g.IsHealthy(p)) && underCap(p) && limitAvailable(p)
	}

	if healthy(current) {
		return cfg, false, nil
	}

	candidateProviders := []string{"claude", "codex", "copilot"}
	alt := firstHealthyProvider(current, candidateProviders, healthy)
	if alt == "" && lg != nil {
		if chosen, _ := lg.ChooseProvider(current, candidateProviders, healthy, lp); chosen != "" {
			alt = chosen
		}
	}
	if alt == "" {
		reason := "no healthy per-turn-capable provider peer available"
		switch {
		case g != nil && !g.IsHealthy(current):
			reason = g.Reason(current)
		case lg != nil:
			if ok, r := lg.ProviderAvailable(current, lp); !ok && r != "" {
				reason = r
			}
		}
		a.SetError("rate_limit", reason)
		return cfg, false, fmt.Errorf("agent regate: %s capped, no per-turn-capable peer available: %s", current, reason)
	}

	altProv, err := lookupProvider(alt)
	if err != nil {
		return cfg, false, err
	}
	newModel := altProv.NormalizeModel(cfg.Model)

	m.logger.Warn("agent.convo.failover", "id", a.ID, "task", cfg.TaskID, "from", current, "to", alt)

	// Native --resume/--session-id is provider-specific: a peer cannot
	// continue the old provider's session, so start it fresh rather than
	// attempting a cross-provider resume.
	a.SetProviderAndModel(alt, newModel)
	a.SetSessionID("")
	a.SetSessionFilePath("")
	m.moveLiveProviderCount(current, alt)

	cfg.Provider = alt
	cfg.provider = altProv

	// Marker must land before the registry write below: on a crash between
	// the two, rehydration must find a marker for every persisted provider
	// change, never a persisted provider with an unmarked log segment.
	writeProviderMarkerLine(logWriter, alt)

	if m.survives() {
		m.saveRegistry(ctx, a)
	}

	return cfg, true, nil
}

// moveLiveProviderCount migrates one live-agent count from oldProv to
// newProv after a mid-run provider switch, preserving the
// sum(liveByProvider) == liveCount invariant maintained elsewhere by
// registerRunningAgent/markAgentDone/the reattach paths.
func (m *Manager) moveLiveProviderCount(oldProv, newProv string) {
	if oldProv == newProv {
		return
	}
	m.mu.Lock()
	if oldProv != "" {
		if v, ok := m.liveByProvider[oldProv]; ok {
			if v <= 1 {
				delete(m.liveByProvider, oldProv)
			} else {
				m.liveByProvider[oldProv] = v - 1
			}
		}
	}
	if newProv != "" {
		m.liveByProvider[newProv]++
	}
	m.mu.Unlock()
}
