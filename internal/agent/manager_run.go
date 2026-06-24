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
	"github.com/Automaat/sybra/internal/provider"
	"github.com/google/uuid"
)

func (m *Manager) StartAgent(taskID, taskTitle, mode, prompt, dir string, allowedTools []string) (*Agent, error) {
	return m.Run(RunConfig{TaskID: taskID, Name: taskTitle, Mode: mode, Prompt: prompt, AllowedTools: allowedTools, Dir: dir})
}

func (m *Manager) Run(cfg RunConfig) (*Agent, error) {
	// Hard guard: every agent must run in an explicit, existing directory.
	// An empty Dir means the spawned process inherits Sybra's cwd, which in
	// dev mode is the Sybra source repo — agents would then mutate its
	// branches via git checkout. Reject rather than silently leak.
	if strings.TrimSpace(cfg.Dir) == "" {
		return nil, fmt.Errorf("agent.Run: Dir is required (empty Dir would leak agent process into Sybra cwd)")
	}
	if info, err := os.Stat(cfg.Dir); err != nil {
		return nil, fmt.Errorf("agent.Run: Dir %q not accessible: %w", cfg.Dir, err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("agent.Run: Dir %q is not a directory", cfg.Dir)
	}

	resolvedProvider, gateErr := m.gateProvider(cfg)
	if gateErr != nil {
		return nil, gateErr
	}

	if cfg.BashTimeoutMs == 0 {
		m.mu.RLock()
		cfg.BashTimeoutMs = m.bashTimeoutMs
		m.mu.RUnlock()
	}

	id := uuid.NewString()[:8]
	ctx, cancel := context.WithCancel(m.ctx)

	now := time.Now().UTC()
	a := &Agent{
		ID:          id,
		TaskID:      cfg.TaskID,
		Name:        cfg.Name,
		Mode:        cfg.Mode,
		Provider:    resolvedProvider,
		Model:       normalizeModel(resolvedProvider, cfg.Model),
		Prompt:      cfg.Prompt,
		State:       StateRunning,
		StartedAt:   now,
		LastEventAt: now,
		cancel:      cancel,
		sessionCWD:  cfg.Dir,
		MaxTurns:    cfg.MaxTurns,
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
	// Pre-mark detached so a shutdown racing the runner goroutine already
	// knows to leave this agent's subprocess running. The runner re-asserts
	// it after a successful Start. Detached applies to headless and to
	// interactive Claude (one-shot via file-arg prompt, sessions via FIFO);
	// codex interactive (spawns per turn) stays on the legacy path.
	if m.survives() && willDetach(cfg) {
		a.setDetached(true)
	}

	m.mu.Lock()
	if !cfg.IgnoreConcurrencyLimit && m.maxConcurrent > 0 && m.liveCount >= m.maxConcurrent {
		m.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("max concurrent agents reached (%d)", m.maxConcurrent)
	}
	m.agents[id] = a
	if a.done != nil {
		m.liveCount++
	}
	m.mu.Unlock()

	metrics.AgentStarted(a.Provider, a.Mode)
	m.logger.Info("agent.start", "id", id, "taskID", cfg.TaskID, "mode", cfg.Mode, "provider", a.Provider, "model", a.Model)

	switch cfg.Mode {
	case "headless":
		go m.runHeadless(ctx, a, cfg)
	case "interactive":
		if a.Provider == "codex" {
			a.promptCh = make(chan string, 1)
			go m.runCodexConversational(ctx, a, cfg, false)
		} else {
			a.approvalCh = make(chan ApprovalResponse, 1)
			go m.runConversational(ctx, a, cfg)
		}
	default:
		cancel()
		return nil, fmt.Errorf("unknown mode: %s", cfg.Mode)
	}

	m.emit(events.AgentState(id), a)
	return a, nil
}

func (m *Manager) markAgentDone(a *Agent) {
	if a == nil || a.done == nil {
		return
	}
	a.doneOnce.Do(func() {
		close(a.done)
		// Close the stdin pipe/FIFO and drop the registry record + FIFO file
		// so a completed agent leaks neither an fd nor an on-disk pipe.
		a.stdinMu.Lock()
		if a.stdinPipe != nil {
			_ = a.stdinPipe.Close()
			a.stdinPipe = nil
		}
		a.stdinMu.Unlock()
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
	prov := m.providerForRun(cfg.Provider)
	if cfg.Model != "" && !safeArgRe.MatchString(cfg.Model) {
		return "", fmt.Errorf("invalid model %q: must match %s", cfg.Model, safeArgRe)
	}
	for _, tool := range cfg.AllowedTools {
		if !safeArgRe.MatchString(tool) {
			return "", fmt.Errorf("invalid tool %q: must match %s", tool, safeArgRe)
		}
	}
	model := normalizeModel(prov, cfg.Model)
	switch prov {
	case "codex":
		return buildCodexCommand(model, cfg.RequirePermissions, cfg.Mode == "headless"), nil
	default:
		return buildClaudeCommand(model, cfg.AllowedTools, cfg.RequirePermissions), nil
	}
}

// buildClaudeCommand builds the display command string for a Claude agent.
func buildClaudeCommand(model string, allowedTools []string, requirePerms bool) string {
	parts := []string{"claude"}
	if len(allowedTools) > 0 {
		parts = append(parts, "--allowedTools", strings.Join(allowedTools, ","))
	} else if !requirePerms {
		parts = append(parts, "--dangerously-skip-permissions")
	}
	if model != "" {
		parts = append(parts, "--model", model)
	}
	return strings.Join(parts, " ")
}

// buildCodexCommand builds the display command string for a Codex agent.
func buildCodexCommand(model string, requirePerms, headless bool) string {
	parts := []string{"codex", "exec", "--json", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules"}
	parts = append(parts, codexSandboxArgs(requirePerms, headless)...)
	if model != "" {
		parts = append(parts, "--model", model)
	}
	return strings.Join(parts, " ")
}

// codexSandboxArgs returns the sandbox/permission flags for `codex exec`.
// When SYBRA_DISABLE_CODEX_SANDBOX=1 is set, the bwrap-backed sandbox is
// replaced with `--sandbox danger-full-access`. Required when running
// inside a Docker/LXC container whose kernel blocks unprivileged user
// namespaces (kernel.unprivileged_userns_clone=0), where bwrap crashes
// before the agent can execute any command.
//
// headless must be true for headless runs. --sandbox workspace-write asks
// for user approval on writes outside the workspace; in headless mode there
// is no UI to serve those approval prompts, so they auto-reject and the run
// fails. Bypass mode is used instead.
func codexSandboxArgs(requirePerms, headless bool) []string {
	if os.Getenv("SYBRA_DISABLE_CODEX_SANDBOX") == "1" {
		return []string{"--sandbox", "danger-full-access"}
	}
	if !requirePerms || headless {
		// Bypass all approval prompts and sandbox restrictions.
		// For headless runs this is required regardless of RequirePermissions:
		// --sandbox workspace-write auto-rejects approval requests since there
		// is no TTY/UI to serve them, which silently breaks the agent run.
		return []string{"--dangerously-bypass-approvals-and-sandbox"}
	}
	return []string{"--sandbox", "workspace-write"}
}

// gateProvider resolves the run's provider through the health gate. If the
// configured provider is unhealthy and auto-failover can supply a healthy
// peer, the peer is returned. Otherwise returns a typed UnhealthyError so
// callers can detect via errors.Is(err, provider.ErrProviderUnhealthy).
func (m *Manager) gateProvider(cfg RunConfig) (string, error) {
	resolved := m.providerForRun(cfg.Provider)
	if cfg.IgnoreHealthGate {
		return resolved, nil
	}
	m.mu.RLock()
	g := m.gate
	m.mu.RUnlock()
	if g == nil {
		return resolved, nil
	}
	if g.IsHealthy(resolved) {
		return resolved, nil
	}
	if alt := g.Failover(resolved); alt != "" {
		metrics.AgentFailover(resolved, alt)
		m.logger.Warn("agent.run.failover", "from", resolved, "to", alt, "task", cfg.TaskID, "reason", g.Reason(resolved))
		return alt, nil
	}
	reason := g.Reason(resolved)
	metrics.AgentGated(resolved, reason)
	return "", &provider.UnhealthyError{
		Provider: resolved,
		Reason:   reason,
	}
}

func (m *Manager) providerForRun(name string) string {
	m.mu.RLock()
	def := m.defaultProv
	m.mu.RUnlock()
	if name == "" {
		name = def
	}
	return normalizeProvider(name)
}

func normalizeProvider(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "claude":
		return "claude"
	case "codex":
		return "codex"
	default:
		return "claude"
	}
}

func normalizeModel(prov, model string) string {
	switch normalizeProvider(prov) {
	case "codex":
		switch strings.TrimSpace(model) {
		case "", "sonnet", "opus":
			return "gpt-5.4"
		case "haiku":
			return "gpt-5.4-mini"
		default:
			return model
		}
	default:
		if strings.TrimSpace(model) == "" {
			return "sonnet"
		}
		return model
	}
}

// safeArgRe matches only characters safe to embed in a shell command
// without quoting: alphanumerics, dot, underscore, hyphen, forward-slash.
var safeArgRe = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)
