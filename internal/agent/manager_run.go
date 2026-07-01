package agent

import (
	"context"
	"fmt"
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
	cfg.ReasoningEffort = defaultReasoningEffort(cfg.ReasoningEffort, prov)

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

func defaultReasoningEffort(effort string, prov Provider) string {
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

// gateProvider resolves the run's provider through the health gate. If the
// configured provider is unhealthy and auto-failover can supply a healthy
// peer, the peer is returned. Otherwise returns a typed UnhealthyError so
// callers can detect via errors.Is(err, provider.ErrProviderUnhealthy).
func (m *Manager) gateProvider(cfg RunConfig) (string, error) {
	resolved, err := m.providerForRun(cfg.Provider)
	if err != nil {
		return "", err
	}
	if cfg.IgnoreHealthGate {
		return resolved, nil
	}
	m.mu.RLock()
	g := m.gate
	lg := m.limitGate
	lp := m.limitPolicy
	m.mu.RUnlock()
	healthy := func(p string) bool {
		return g == nil || g.IsHealthy(p)
	}
	if g != nil && !g.IsHealthy(resolved) {
		if cfg.DisableProviderFailover {
			reason := g.Reason(resolved)
			metrics.AgentGated(resolved, reason)
			return "", &provider.UnhealthyError{
				Provider: resolved,
				Reason:   reason,
			}
		}
		if alt := g.Failover(resolved); alt != "" {
			altProv, err := lookupProvider(alt)
			if err != nil {
				return "", err
			}
			metrics.AgentFailover(resolved, altProv.Name())
			m.logger.Warn("agent.run.failover", "from", resolved, "to", altProv.Name(), "task", cfg.TaskID, "reason", g.Reason(resolved))
			resolved = altProv.Name()
		} else {
			reason := g.Reason(resolved)
			metrics.AgentGated(resolved, reason)
			return "", &provider.UnhealthyError{
				Provider: resolved,
				Reason:   reason,
			}
		}
	}
	if lg == nil {
		return resolved, nil
	}
	if ok, reason := lg.ProviderAvailable(resolved, lp); ok {
		if lp.PreferUnderused && !cfg.DisableProviderFailover {
			if alt, altReason := lg.ChooseProvider(resolved, []string{"claude", "codex", "copilot"}, healthy, lp); alt != "" {
				metrics.AgentFailover(resolved, alt)
				m.logger.Info("agent.run.limit_select", "from", resolved, "to", alt, "task", cfg.TaskID, "reason", altReason)
				return alt, nil
			}
		}
		return resolved, nil
	} else if !cfg.DisableProviderFailover {
		if alt, altReason := lg.ChooseProvider(resolved, []string{"claude", "codex", "copilot"}, healthy, lp); alt != "" {
			metrics.AgentFailover(resolved, alt)
			m.logger.Warn("agent.run.limit_failover", "from", resolved, "to", alt, "task", cfg.TaskID, "reason", reason, "alt_reason", altReason)
			return alt, nil
		}
		metrics.AgentGated(resolved, reason)
		return "", &provider.UnhealthyError{
			Provider: resolved,
			Reason:   reason,
		}
	} else {
		metrics.AgentGated(resolved, reason)
		return "", &provider.UnhealthyError{
			Provider: resolved,
			Reason:   reason,
		}
	}
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
