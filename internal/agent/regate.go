package agent

import (
	"maps"
)

// providerHealthyForSteer reports whether an already-running agent's current
// provider is still healthy and under its rate limit at a mid-run turn
// boundary. A steerable headless run never fails over to a per-turn peer at
// its steer boundary — a headless run recovers by re-dispatch, not hot-swap
// (see RescheduleRateLimitedAgent) — so the steer path only needs to know
// whether writing the next turn to the current provider is safe.
// Self-count-aware: the agent's own in-flight turn already occupies a slot in
// current's live bucket and must not judge itself at cap.
func (m *Manager) providerHealthyForSteer(a *Agent) bool {
	current := a.GetProvider()

	m.mu.RLock()
	g := m.gate
	lg := m.limitGate
	lp := m.limitPolicy
	maxInFlight := m.maxInFlightPerProvider
	live := maps.Clone(m.liveByProvider)
	m.mu.RUnlock()

	if live[current] > 0 {
		live[current]--
	}
	underCap := maxInFlight <= 0 || live[current] < maxInFlight
	limitAvailable := true
	if lg != nil {
		limitAvailable, _ = lg.ProviderAvailable(current, lp)
	}
	return (g == nil || g.IsHealthy(current)) && underCap && limitAvailable
}
