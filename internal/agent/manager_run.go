package agent

import (
	"context"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/notes"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/google/uuid"
)

func (m *Manager) StartAgent(taskID, taskTitle, mode, prompt, dir string, allowedTools []string) (*Agent, error) {
	return m.Run(RunConfig{TaskID: taskID, Name: taskTitle, Mode: mode, Prompt: prompt, AllowedTools: allowedTools, Dir: dir})
}

func (a *Agent) setAssignment(cfg RunConfig) {
	a.ExperimentID = cfg.ExperimentID
	a.VariantID = cfg.VariantID
	a.AssignmentUnit = cfg.AssignmentUnit
	a.AssignmentKey = cfg.AssignmentKey
}

func (m *Manager) Run(cfg RunConfig) (*Agent, error) {
	if cfg.Mode == "headless" {
		if err := m.jitterDispatch(); err != nil {
			return nil, err
		}
	}

	cfg, prov, err := m.prepareRunConfig(cfg)
	if err != nil {
		return nil, err
	}

	id := uuid.NewString()[:8]
	ctx, cancel := context.WithCancel(m.ctx)
	a := newRunningAgent(id, cfg, prov, cancel)
	if m.survives() && willDetach(cfg) {
		a.setDetached(true)
	}

	if err := m.registerRunningAgent(a, cfg, cancel); err != nil {
		return nil, err
	}

	metrics.AgentStarted(a.Provider, a.Mode)
	m.logger.Info("agent.start", "id", id, "taskID", cfg.TaskID, "mode", cfg.Mode, "provider", a.Provider, "model", a.Model)

	if err := m.startAgentRunner(ctx, a, cfg, prov, cancel); err != nil {
		return nil, err
	}

	m.emit(events.AgentState(id), a)
	return a, nil
}

// jitterDispatch sleeps a uniform random [0, dispatchJitterMs] duration so a
// wave of ready headless dispatches does not all probe the provider health
// gate in the same tick. Returns the context error if the manager shuts down
// mid-sleep, so the caller aborts the dispatch instead of racing shutdown.
func (m *Manager) jitterDispatch() error {
	m.mu.RLock()
	ms := m.dispatchJitterMs
	m.mu.RUnlock()
	if ms <= 0 {
		return nil
	}
	// Exclusive upper bound (ms, not ms+1) avoids overflow when ms == MaxInt
	// while keeping the jitter within [0, ms).
	d := time.Duration(rand.N(ms)) * time.Millisecond
	if d <= 0 {
		return nil
	}
	select {
	case <-time.After(d):
		return nil
	case <-m.ctx.Done():
		return m.ctx.Err()
	}
}

func (m *Manager) prepareRunConfig(cfg RunConfig) (RunConfig, Provider, error) {
	if err := validateRunDir(cfg.Dir); err != nil {
		return cfg, nil, err
	}
	if cfg.SeedWorkingMemory {
		cfg.Prompt = notes.SeedPrompt(cfg.Prompt, cfg.Dir)
	}
	resolvedProvider, gateErr := m.gateProvider(cfg)
	if gateErr != nil {
		return cfg, nil, gateErr
	}
	prov, providerErr := lookupProvider(resolvedProvider)
	if providerErr != nil {
		return cfg, nil, providerErr
	}
	cfg.provider = prov
	cfg.ReasoningEffort = defaultReasoningEffort(cfg.ReasoningEffort)

	m.mu.RLock()
	if cfg.BashTimeoutMs == 0 {
		cfg.BashTimeoutMs = m.bashTimeoutMs
	}
	if cfg.RetryWatchdog == 0 {
		cfg.RetryWatchdog = m.retryWatchdog
	}
	if cfg.FallbackModel == "" {
		cfg.FallbackModel = m.fallbackModel
	}
	m.mu.RUnlock()
	return cfg, prov, nil
}

func defaultReasoningEffort(effort string) string {
	if effort != "" {
		return effort
	}
	return DefaultReasoningEffort
}

func validateRunDir(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("agent.Run: Dir is required (empty Dir would leak agent process into Sybra cwd)")
	}
	if info, err := os.Stat(dir); err != nil {
		return fmt.Errorf("agent.Run: Dir %q not accessible: %w", dir, err)
	} else if !info.IsDir() {
		return fmt.Errorf("agent.Run: Dir %q is not a directory", dir)
	}
	return nil
}

func newRunningAgent(id string, cfg RunConfig, prov Provider, cancel context.CancelFunc) *Agent {
	now := time.Now().UTC()
	a := &Agent{
		ID:                     id,
		TaskID:                 cfg.TaskID,
		Name:                   cfg.Name,
		Mode:                   cfg.Mode,
		Provider:               prov.Name(),
		Model:                  prov.NormalizeModel(cfg.Model),
		ReasoningEffort:        cfg.ReasoningEffort,
		Prompt:                 cfg.Prompt,
		State:                  StateRunning,
		StartedAt:              now,
		LastEventAt:            now,
		cancel:                 cancel,
		sessionCWD:             cfg.Dir,
		MaxTurns:               cfg.MaxTurns,
		oneShot:                cfg.OneShot,
		requirePermissions:     cfg.RequirePermissions,
		headlessPermissionMode: cfg.HeadlessPermissionMode,
	}
	if cfg.ResumeSessionID != "" {
		a.SetSessionID(cfg.ResumeSessionID)
	}
	if cfg.Mode == "headless" || cfg.Mode == "interactive" {
		a.done = make(chan struct{})
	}
	if cfg.Mode == "headless" {
		a.escalationCh = make(chan bool, 1)
	}
	a.setAssignment(cfg)
	return a
}

func (m *Manager) registerRunningAgent(a *Agent, cfg RunConfig, cancel context.CancelFunc) error {
	m.mu.Lock()
	if !cfg.IgnoreConcurrencyLimit && m.maxConcurrent > 0 && m.liveCount >= m.maxConcurrent {
		m.mu.Unlock()
		cancel()
		return fmt.Errorf("max concurrent agents reached (%d)", m.maxConcurrent)
	}
	m.agents[a.ID] = a
	if a.done != nil {
		m.liveCount++
		if a.Provider != "" {
			m.liveByProvider[a.Provider]++
		}
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) startAgentRunner(ctx context.Context, a *Agent, cfg RunConfig, prov Provider, cancel context.CancelFunc) error {
	switch cfg.Mode {
	case "headless":
		go m.runHeadless(ctx, a, cfg)
	case "interactive":
		// codex and copilot use the per-turn conversational runner (each turn
		// spawns a fresh process); claude uses the persistent approval-hook
		// runner. Copilot's permission model is CLI-flag based (no HTTP
		// approval hook), so the per-turn shape fits it like codex.
		if prov.UsesPerTurnConvo() {
			a.setPromptChannel(make(chan string, 1))
			go m.runPerTurnConversational(ctx, a, cfg, false)
		} else {
			go m.runConversational(ctx, a, cfg)
		}
	default:
		cancel()
		return fmt.Errorf("unknown mode: %s", cfg.Mode)
	}
	return nil
}

func (m *Manager) markAgentDone(a *Agent) {
	if a == nil || a.done == nil {
		return
	}
	a.doneOnce.Do(func() {
		close(a.done)
		// Close the stdin pipe/FIFO and drop the registry record + FIFO file
		// so a completed agent leaks neither an fd nor an on-disk pipe.
		a.convo.closeStdinPipe()
		if reg := m.registry(); reg != nil {
			_ = reg.Delete(a.ID)
		}
		m.mu.Lock()
		if m.liveCount > 0 {
			m.liveCount--
		}
		if a.Provider != "" {
			if v, ok := m.liveByProvider[a.Provider]; ok {
				if v <= 1 {
					delete(m.liveByProvider, a.Provider)
				} else {
					m.liveByProvider[a.Provider] = v - 1
				}
			}
		}
		m.mu.Unlock()
	})
}

func (m *Manager) buildCommand(cfg RunConfig) (string, error) {
	resolved, err := m.providerForRun(cfg.Provider)
	if err != nil {
		return "", err
	}
	prov, err := lookupProvider(resolved)
	if err != nil {
		return "", err
	}
	model := prov.NormalizeModel(cfg.Model)
	if model != "" && !safeArgRe.MatchString(model) {
		return "", fmt.Errorf("invalid model %q: must match %s", cfg.Model, safeArgRe)
	}
	for _, tool := range cfg.AllowedTools {
		if !safeArgRe.MatchString(tool) {
			return "", fmt.Errorf("invalid tool %q: must match %s", tool, safeArgRe)
		}
	}
	return prov.BuildCommand(cfg, model), nil
}

// ResolveProvider predicts the provider a Run call with this cfg would
// dispatch to, applying the same health-gate / limit-gate failover logic as
// prepareRunConfig. Callers that need to make provider-scoped decisions
// before Run actually runs (e.g. selecting a resumable session) can use this
// to avoid assuming the requested/default provider always wins. Unlike
// gateProvider, this never emits metrics or logs — it only computes the
// decision — so calling it ahead of Run and then again inside Run cannot
// double-emit AgentGated/AgentFailover metrics or agent.run.* log lines,
// though the two calls can in principle disagree if gate state changes in
// between.
func (m *Manager) ResolveProvider(cfg RunConfig) (string, error) {
	resolved, _, err := m.resolveProviderDecision(cfg)
	return resolved, err
}

// providerGateEvent records a metrics/log side effect that gateProvider
// would emit for a given routing decision, deferred so resolveProviderDecision
// can stay side-effect free and be reused by both gateProvider and the
// prediction-only ResolveProvider.
type providerGateEvent struct {
	kind      string // "gated" or "failover"
	provider  string // gated: the gated provider
	from, to  string // failover: source and destination provider
	reason    string
	altReason string
	logKey    string // failover: log message key
	logLevel  string // failover: "warn" or "info"
	taskID    string
}

// gateProvider resolves the run's provider through the health gate. If the
// configured provider is unhealthy and auto-failover can supply a healthy
// peer, the peer is returned. Otherwise returns a typed UnhealthyError so
// callers can detect via errors.Is(err, provider.ErrProviderUnhealthy).
func (m *Manager) gateProvider(cfg RunConfig) (string, error) {
	resolved, gateEvents, err := m.resolveProviderDecision(cfg)
	m.emitProviderGateEvents(gateEvents)
	return resolved, err
}

// emitProviderGateEvents applies the metrics/log side effects recorded by
// resolveProviderDecision. Only gateProvider calls this — ResolveProvider
// discards the events so prediction stays observation-free.
func (m *Manager) emitProviderGateEvents(gateEvents []providerGateEvent) {
	for i := range gateEvents {
		e := &gateEvents[i]
		switch e.kind {
		case "gated":
			metrics.AgentGated(e.provider, e.reason)
		case "failover":
			metrics.AgentFailover(e.from, e.to)
			fields := []any{"from", e.from, "to", e.to, "task", e.taskID}
			if e.reason != "" {
				fields = append(fields, "reason", e.reason)
			}
			if e.altReason != "" {
				fields = append(fields, "alt_reason", e.altReason)
			}
			if e.logLevel == "warn" {
				m.logger.Warn(e.logKey, fields...)
			} else {
				m.logger.Info(e.logKey, fields...)
			}
		}
	}
}

// resolveProviderDecision computes the provider a Run call with this cfg
// would dispatch to, applying health-gate / limit-gate failover logic. It is
// side-effect free: metrics/log emissions are returned as providerGateEvent
// values for the caller to apply (gateProvider) or discard (ResolveProvider).
func (m *Manager) resolveProviderDecision(cfg RunConfig) (string, []providerGateEvent, error) {
	resolved, err := m.providerForRun(cfg.Provider)
	if err != nil {
		return "", nil, err
	}
	if cfg.IgnoreHealthGate {
		return resolved, nil, nil
	}
	m.mu.RLock()
	g := m.gate
	lg := m.limitGate
	lp := m.limitPolicy
	maxInFlight := m.maxInFlightPerProvider
	live := maps.Clone(m.liveByProvider)
	m.mu.RUnlock()
	underCap := func(p string) bool {
		return maxInFlight <= 0 || live[p] < maxInFlight
	}
	healthy := func(p string) bool {
		return (g == nil || g.IsHealthy(p)) && underCap(p)
	}
	var gateEvents []providerGateEvent
	candidateProviders := []string{"claude", "codex", "copilot"}
	if g != nil && !g.IsHealthy(resolved) {
		if cfg.DisableProviderFailover {
			reason := g.Reason(resolved)
			gateEvents = append(gateEvents, providerGateEvent{kind: "gated", provider: resolved, reason: reason})
			return "", gateEvents, &provider.UnhealthyError{
				Provider: resolved,
				Reason:   reason,
			}
		}
		alt := g.Failover(resolved)
		if alt != "" && !underCap(alt) {
			// g.Failover only consults health, so its pick may already be at
			// its in-flight cap. AutoFailover being enabled is established by
			// alt != "", so it's safe to look for another peer that is both
			// healthy and under cap before settling for the at-cap pick.
			if capAlt := firstHealthyProvider(resolved, candidateProviders, healthy); capAlt != "" {
				alt = capAlt
			}
		}
		if alt != "" {
			altProv, err := lookupProvider(alt)
			if err != nil {
				return "", gateEvents, err
			}
			gateEvents = append(gateEvents, providerGateEvent{
				kind: "failover", from: resolved, to: altProv.Name(),
				reason: g.Reason(resolved), logKey: "agent.run.failover", logLevel: "warn", taskID: cfg.TaskID,
			})
			resolved = altProv.Name()
		} else {
			reason := g.Reason(resolved)
			gateEvents = append(gateEvents, providerGateEvent{kind: "gated", provider: resolved, reason: reason})
			return "", gateEvents, &provider.UnhealthyError{
				Provider: resolved,
				Reason:   reason,
			}
		}
	}
	if lg == nil {
		return resolved, gateEvents, nil
	}
	if ok, reason := lg.ProviderAvailable(resolved, lp); ok {
		if !cfg.DisableProviderFailover {
			// The soft in-flight cap must redirect regardless of
			// PreferUnderused and regardless of quota data: the limits
			// store's ChooseProvider only recommends an alternative from
			// exact quota-pressure data (see limits.Store.ChooseProvider),
			// so it silently no-ops when the requested provider merely sits
			// at its in-flight cap with no quota signal. Picking the first
			// healthy, under-cap candidate directly — bypassing that
			// quota heuristic — is what makes the cap actually redirect.
			if !underCap(resolved) {
				if alt := firstHealthyProvider(resolved, candidateProviders, healthy); alt != "" {
					gateEvents = append(gateEvents, providerGateEvent{
						kind: "failover", from: resolved, to: alt, logKey: "agent.run.cap_redirect", logLevel: "info", taskID: cfg.TaskID,
					})
					return alt, gateEvents, nil
				}
			} else if lp.PreferUnderused {
				if alt, altReason := lg.ChooseProvider(resolved, candidateProviders, healthy, lp); alt != "" {
					gateEvents = append(gateEvents, providerGateEvent{
						kind: "failover", from: resolved, to: alt, reason: altReason, logKey: "agent.run.limit_select", logLevel: "info", taskID: cfg.TaskID,
					})
					return alt, gateEvents, nil
				}
			}
		}
		return resolved, gateEvents, nil
	} else if !cfg.DisableProviderFailover {
		if alt, altReason := lg.ChooseProvider(resolved, candidateProviders, healthy, lp); alt != "" {
			gateEvents = append(gateEvents, providerGateEvent{
				kind: "failover", from: resolved, to: alt, reason: reason, altReason: altReason, logKey: "agent.run.limit_failover", logLevel: "warn", taskID: cfg.TaskID,
			})
			return alt, gateEvents, nil
		}
		gateEvents = append(gateEvents, providerGateEvent{kind: "gated", provider: resolved, reason: reason})
		return "", gateEvents, &provider.UnhealthyError{
			Provider: resolved,
			Reason:   reason,
		}
	} else {
		gateEvents = append(gateEvents, providerGateEvent{kind: "gated", provider: resolved, reason: reason})
		return "", gateEvents, &provider.UnhealthyError{
			Provider: resolved,
			Reason:   reason,
		}
	}
}

// firstHealthyProvider returns the first candidate (other than exclude) for
// which healthy reports true, or "" if none qualify. Used for the in-flight
// cap redirect, which must pick deterministically without consulting the
// limits store's quota-pressure heuristics.
func firstHealthyProvider(exclude string, candidates []string, healthy func(string) bool) string {
	for _, p := range candidates {
		if p == exclude {
			continue
		}
		if healthy(p) {
			return p
		}
	}
	return ""
}

func (m *Manager) providerForRun(name string) (string, error) {
	m.mu.RLock()
	def := m.defaultProv
	m.mu.RUnlock()
	if name == "" {
		name = def
	}
	prov, err := lookupProvider(name)
	if err != nil {
		return "", err
	}
	return prov.Name(), nil
}

// safeArgRe matches only characters safe to embed in a shell command
// without quoting: alphanumerics, dot, underscore, hyphen, forward-slash.
var safeArgRe = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)
