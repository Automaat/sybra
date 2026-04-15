package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/synapse/internal/events"
	"github.com/Automaat/synapse/internal/metrics"
	"github.com/Automaat/synapse/internal/provider"
	"github.com/google/uuid"
)

type EmitFunc func(event string, data any)

// Guardrails defines per-agent execution limits.
type Guardrails struct {
	MaxCostUSD float64
	MaxTurns   int
}

type Manager struct {
	agents        map[string]*Agent
	mu            sync.RWMutex
	liveCount     int
	ctx           context.Context
	emit          EmitFunc
	onComplete    func(ag *Agent)
	logger        *slog.Logger
	logDir        string
	maxConcurrent int
	defaultProv   string
	approvalAddr  string // localhost:port for the HTTP tool approval server
	guardrails    Guardrails
	gate          provider.HealthGate
}

func NewManager(ctx context.Context, emit EmitFunc, logger *slog.Logger, logDir string) *Manager {
	return &Manager{
		agents:      make(map[string]*Agent),
		ctx:         ctx,
		emit:        emit,
		logger:      logger,
		logDir:      logDir,
		defaultProv: "claude",
	}
}

func (m *Manager) SetOnComplete(fn func(ag *Agent)) {
	m.onComplete = fn
}

// SetApprovalAddr sets the HTTP address for the tool approval server.
func (m *Manager) SetApprovalAddr(addr string) {
	m.approvalAddr = addr
}

// SetMaxConcurrent sets the maximum number of concurrently running agents.
// A value of 0 means unlimited.
func (m *Manager) SetMaxConcurrent(n int) {
	m.mu.Lock()
	m.maxConcurrent = n
	m.mu.Unlock()
}

func (m *Manager) SetDefaultProvider(name string) {
	m.mu.Lock()
	m.defaultProv = normalizeProvider(name)
	m.mu.Unlock()
}

// SetHealthGate wires in a provider health checker so Run() can refuse or
// failover when the requested provider is unhealthy. A nil gate disables the
// check entirely (tests, feature-disabled mode).
func (m *Manager) SetHealthGate(g provider.HealthGate) {
	m.mu.Lock()
	m.gate = g
	m.mu.Unlock()
}

// ReportProviderSignal forwards a runner-side passive signal (rate-limit or
// auth failure) to the health gate. Safe to call with a nil gate.
func (m *Manager) ReportProviderSignal(name string, sig provider.Signal, reason string, retryAfter time.Duration) {
	m.mu.RLock()
	g := m.gate
	m.mu.RUnlock()
	if g == nil {
		return
	}
	switch sig {
	case provider.SignalAuthFailure:
		g.ReportAuthFailure(name, reason)
	case provider.SignalRateLimit:
		g.ReportRateLimit(name, retryAfter, reason)
	case provider.SignalNone:
		// no-op: caller decided not to escalate this run.
	}
}

// DefaultProvider returns the current default provider name.
func (m *Manager) DefaultProvider() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.defaultProv
}

// SetGuardrails configures cost and turn limits applied to all agents.
func (m *Manager) SetGuardrails(g Guardrails) {
	m.mu.Lock()
	m.guardrails = g
	m.mu.Unlock()
}

// RespondEscalation sends a human decision to a paused agent.
// continueRun=true lets the agent keep running; false kills it.
func (m *Manager) RespondEscalation(agentID string, continueRun bool) error {
	a, err := m.GetAgent(agentID)
	if err != nil {
		return err
	}
	if a.escalationCh == nil {
		return fmt.Errorf("agent %s has no pending escalation", agentID)
	}
	select {
	case a.escalationCh <- continueRun:
	default:
		return fmt.Errorf("agent %s escalation channel full or closed", agentID)
	}
	return nil
}

// recordCompletion is called from every runner's terminal site just before
// onComplete fires. Records duration + result into the metrics pipeline.
func (m *Manager) recordCompletion(a *Agent, ok bool) {
	dur := time.Since(a.StartedAt)
	result := "ok"
	if !ok {
		result = "error"
	}
	metrics.AgentCompleted(result, dur)
}

// RunningCount returns the number of currently running agents.
func (m *Manager) RunningCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.liveCount
}

func (m *Manager) StartAgent(taskID, taskTitle, mode, prompt, dir string, allowedTools []string) (*Agent, error) {
	return m.Run(RunConfig{TaskID: taskID, Name: taskTitle, Mode: mode, Prompt: prompt, AllowedTools: allowedTools, Dir: dir})
}

func (m *Manager) Run(cfg RunConfig) (*Agent, error) {
	// Hard guard: every agent must run in an explicit, existing directory.
	// An empty Dir means the spawned process inherits Synapse's cwd, which in
	// dev mode is the Synapse source repo — agents would then mutate its
	// branches via git checkout. Reject rather than silently leak.
	if strings.TrimSpace(cfg.Dir) == "" {
		return nil, fmt.Errorf("agent.Run: Dir is required (empty Dir would leak agent process into Synapse cwd)")
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
		State:       StateRunning,
		StartedAt:   now,
		LastEventAt: now,
		cancel:      cancel,
		sessionCWD:  cfg.Dir,
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
			go m.runCodexConversational(ctx, a, cfg)
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
		return buildCodexCommand(model, cfg.RequirePermissions), nil
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
func buildCodexCommand(model string, requirePerms bool) string {
	parts := []string{"codex", "exec", "--json", "--skip-git-repo-check"}
	if !requirePerms {
		parts = append(parts, "--full-auto")
	} else {
		parts = append(parts, "--sandbox", "workspace-write")
	}
	if model != "" {
		parts = append(parts, "--model", model)
	}
	return strings.Join(parts, " ")
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

// SendPromptToAgent delivers a follow-up prompt to an interactive agent.
func (m *Manager) SendPromptToAgent(agentID, text string) error {
	a, err := m.GetAgent(agentID)
	if err != nil {
		return err
	}
	if a.GetState() == StateStopped {
		return fmt.Errorf("agent %s is stopped", agentID)
	}

	// Conversational agents: write to stdin via SendMessage.
	a.stdinMu.Lock()
	hasStdin := a.stdinPipe != nil
	a.stdinMu.Unlock()
	if hasStdin {
		return m.SendMessage(agentID, text)
	}

	// Codex conversational agents: deliver via promptCh.
	a.mu.RLock()
	hasCh := a.promptCh != nil
	a.mu.RUnlock()
	if hasCh {
		return m.sendCodexPrompt(agentID, text)
	}

	return fmt.Errorf("agent %s has no active transport (no stdin pipe or prompt channel)", agentID)
}

// isLive reports whether an agent is still alive from the user's perspective.
// Conversational agents switch to StatePaused while idle between turns; they
// must still be findable so a follow-up prompt can be delivered without
// spawning a new session.
func isLive(s State) bool {
	return s == StateRunning || s == StatePaused
}

// FindRunningAgentForTask returns the first live agent for the given task
// matching the provided role. Returns nil if none found. "Live" includes
// paused conversational agents that are idle-waiting for a follow-up prompt.
func (m *Manager) FindRunningAgentForTask(taskID string, role Role) *Agent {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, a := range m.agents {
		if a.TaskID != taskID || !isLive(a.GetState()) {
			continue
		}
		if RoleFromName(a.Name) != role {
			continue
		}
		return a
	}
	return nil
}

// FindAllRunningAgentsForTask returns all live agents for the given task
// matching the provided role. An empty role matches all roles.
func (m *Manager) FindAllRunningAgentsForTask(taskID string, role Role) []*Agent {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []*Agent
	for _, a := range m.agents {
		if a.TaskID != taskID || !isLive(a.GetState()) {
			continue
		}
		if role != "" && RoleFromName(a.Name) != role {
			continue
		}
		result = append(result, a)
	}
	return result
}

// KillAgentsForTask stops all running agents for the given task ID and waits
// for their goroutines to exit (up to timeout). Safe to call from DeleteTask
// before worktree cleanup.
func (m *Manager) KillAgentsForTask(taskID string, timeout time.Duration) {
	m.mu.RLock()
	var targets []*Agent
	for _, a := range m.agents {
		if a.TaskID == taskID {
			targets = append(targets, a)
		}
	}
	m.mu.RUnlock()

	for _, a := range targets {
		m.logger.Info("agent.kill-for-task", "agent_id", a.ID, "task_id", taskID)
		if a.cancel != nil {
			a.cancel()
		}
		a.SetState(StateStopped)
		a.stdinMu.Lock()
		if a.stdinPipe != nil {
			_ = a.stdinPipe.Close()
			a.stdinPipe = nil
		}
		a.stdinMu.Unlock()
		m.emit(events.AgentState(a.ID), a)
	}

	deadline := time.After(timeout)
	for _, a := range targets {
		if a.done == nil {
			continue
		}
		select {
		case <-a.done:
		case <-deadline:
			m.logger.Warn("agent.kill-timeout", "agent_id", a.ID, "task_id", taskID)
			return
		}
	}
}

func (m *Manager) StopAgent(agentID string) error {
	m.mu.Lock()
	a, ok := m.agents[agentID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("agent %s not found", agentID)
	}

	m.logger.Info("agent.stop", "id", agentID)

	if a.cancel != nil {
		a.cancel()
	}
	a.SetState(StateStopped)
	// Close stdin to signal the claude process to exit.
	a.stdinMu.Lock()
	if a.stdinPipe != nil {
		_ = a.stdinPipe.Close()
		a.stdinPipe = nil
	}
	a.stdinMu.Unlock()
	m.emit(events.AgentState(agentID), a)
	return nil
}

func (m *Manager) GetAgent(agentID string) (*Agent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.agents[agentID]
	if !ok {
		return nil, fmt.Errorf("agent %s not found", agentID)
	}
	return a, nil
}

func (m *Manager) ListAgents() []*Agent {
	m.mu.RLock()
	defer m.mu.RUnlock()
	agents := make([]*Agent, 0, len(m.agents))
	for _, a := range m.agents {
		agents = append(agents, a)
	}
	return agents
}

// HasRunningAgentForTask returns true if any agent is currently running for the given task.
// For headless agents this checks whether the goroutine has truly exited (via the done
// channel) rather than the State field, which may be set to Stopped by StopAgent before
// the goroutine finishes — avoiding a race where the worktree is cleaned up while the
// agent process is still using it.
func (m *Manager) HasRunningAgentForTask(taskID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, a := range m.agents {
		if a.TaskID != taskID {
			continue
		}
		if a.done != nil {
			// headless: goroutine alive until done is closed
			select {
			case <-a.done:
				// goroutine exited
			default:
				return true
			}
		} else if a.GetState() == StateRunning {
			// interactive: no goroutine, rely on state
			return true
		}
	}
	return false
}

func (m *Manager) Shutdown() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	m.logger.Info("agent.shutdown", "count", len(m.agents))
	for _, a := range m.agents {
		if a.cancel != nil {
			a.cancel()
		}
	}
}

// safeArgRe matches only characters safe to embed in a shell command
// without quoting: alphanumerics, dot, underscore, hyphen, forward-slash.
var safeArgRe = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)
