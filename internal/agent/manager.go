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
	"github.com/Automaat/synapse/internal/tmux"
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
	ctx           context.Context
	tmux          *tmux.Manager
	emit          EmitFunc
	onComplete    func(ag *Agent)
	logger        *slog.Logger
	logDir        string
	maxConcurrent int
	defaultProv   string
	approvalAddr  string // localhost:port for the HTTP tool approval server
	guardrails    Guardrails
}

func NewManager(ctx context.Context, tm *tmux.Manager, emit EmitFunc, logger *slog.Logger, logDir string) *Manager {
	return &Manager{
		agents:      make(map[string]*Agent),
		ctx:         ctx,
		tmux:        tm,
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

func (m *Manager) SetDefaultProvider(provider string) {
	m.mu.Lock()
	m.defaultProv = normalizeProvider(provider)
	m.mu.Unlock()
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

// RunningCount returns the number of currently running agents.
func (m *Manager) RunningCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.runningCountLocked()
}

func (m *Manager) runningCountLocked() int {
	count := 0
	for _, a := range m.agents {
		if a.done != nil {
			select {
			case <-a.done:
			default:
				count++
			}
		} else if a.GetState() == StateRunning {
			count++
		}
	}
	return count
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

	id := uuid.NewString()[:8]
	ctx, cancel := context.WithCancel(m.ctx)

	now := time.Now().UTC()
	a := &Agent{
		ID:          id,
		TaskID:      cfg.TaskID,
		Name:        cfg.Name,
		Mode:        cfg.Mode,
		Provider:    m.providerForRun(cfg.Provider),
		Model:       normalizeModel(m.providerForRun(cfg.Provider), cfg.Model),
		State:       StateRunning,
		StartedAt:   now,
		LastEventAt: now,
		cancel:      cancel,
		sessionCWD:  cfg.Dir,
	}
	if cfg.Mode == "headless" || cfg.Mode == "interactive" {
		a.done = make(chan struct{})
	}
	if cfg.Mode == "headless" {
		a.escalationCh = make(chan bool, 1)
	}

	m.mu.Lock()
	if !cfg.IgnoreConcurrencyLimit && m.maxConcurrent > 0 && m.runningCountLocked() >= m.maxConcurrent {
		m.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("max concurrent agents reached (%d)", m.maxConcurrent)
	}
	m.agents[id] = a
	m.mu.Unlock()

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

func (m *Manager) buildCommand(cfg RunConfig) (string, error) {
	provider := m.providerForRun(cfg.Provider)
	if cfg.Model != "" && !safeArgRe.MatchString(cfg.Model) {
		return "", fmt.Errorf("invalid model %q: must match %s", cfg.Model, safeArgRe)
	}
	for _, tool := range cfg.AllowedTools {
		if !safeArgRe.MatchString(tool) {
			return "", fmt.Errorf("invalid tool %q: must match %s", tool, safeArgRe)
		}
	}
	model := normalizeModel(provider, cfg.Model)
	switch provider {
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

func (m *Manager) providerForRun(provider string) string {
	m.mu.RLock()
	def := m.defaultProv
	m.mu.RUnlock()
	if provider == "" {
		provider = def
	}
	return normalizeProvider(provider)
}

func normalizeProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", "claude":
		return "claude"
	case "codex":
		return "codex"
	default:
		return "claude"
	}
}

func normalizeModel(provider, model string) string {
	switch normalizeProvider(provider) {
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

// waitForTmuxChange polls capture-pane until output differs from snapshot or
// timeout elapses. Respects ctx cancellation. On timeout the caller proceeds
// anyway — this is best-effort sync, not a hard gate.
func (m *Manager) waitForTmuxChange(ctx context.Context, session, snapshot string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		default:
		}
		out, err := m.tmux.CapturePaneOutput(session)
		if err == nil && out != snapshot {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (m *Manager) sendInteractivePrompt(ctx context.Context, a *Agent, prompt string) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	bypassAccepted := false
	timeout := time.After(30 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return
		case <-timeout:
			m.logger.Error("agent.interactive.timeout", "id", a.ID, "msg", "agent did not become ready in 30s")
			return
		case <-ticker.C:
			out, err := m.tmux.CapturePaneOutput(a.TmuxSession)
			if err != nil {
				continue
			}
			// Handle --dangerously-skip-permissions confirmation dialog
			if !bypassAccepted && strings.Contains(out, "Yes, I accept") {
				_ = m.tmux.SendRawKeys(a.TmuxSession, "Down")
				// Wait for dialog to re-render with cursor moved before Enter.
				m.waitForTmuxChange(ctx, a.TmuxSession, out, time.Second)
				_ = m.tmux.SendRawKeys(a.TmuxSession, "Enter")
				m.logger.Info("agent.interactive.bypass_accepted", "id", a.ID)
				bypassAccepted = true
				continue
			}
			// Wait for the actual chat prompt (not the bypass dialog)
			if strings.Contains(out, "❯") && !strings.Contains(out, "Yes, I accept") {
				if err := m.tmux.SendKeys(a.TmuxSession, prompt); err != nil {
					m.logger.Error("agent.interactive.sendkeys", "id", a.ID, "err", err)
					return
				}
				// Wait for pane to reflect the pasted text before submitting.
				m.waitForTmuxChange(ctx, a.TmuxSession, out, time.Second)
				_ = m.tmux.SendRawKeys(a.TmuxSession, "Enter")
				m.logger.Info("agent.interactive.prompt_sent", "id", a.ID)
				return
			}
		}
	}
}

// SendPromptToAgent delivers a follow-up prompt to an interactive agent.
// It dispatches on transport: conversational agents (stdin stream-json)
// receive the prompt via SendMessage; legacy tmux agents get the text pasted
// and submitted. Both transports are valid for Mode == "interactive".
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

	// Legacy tmux transport.
	if a.TmuxSession == "" {
		return fmt.Errorf("agent %s has no active transport (no stdin pipe or tmux session)", agentID)
	}
	if !m.tmux.SessionExists(a.TmuxSession) {
		return fmt.Errorf("tmux session %s does not exist", a.TmuxSession)
	}
	before, _ := m.tmux.CapturePaneOutput(a.TmuxSession)
	if err := m.tmux.SendKeys(a.TmuxSession, text); err != nil {
		return fmt.Errorf("send keys: %w", err)
	}
	// Wait for pane to reflect pasted text before submitting; context-aware.
	m.waitForTmuxChange(m.ctx, a.TmuxSession, before, 500*time.Millisecond)
	if err := m.tmux.SendRawKeys(a.TmuxSession, "Enter"); err != nil {
		return fmt.Errorf("send enter: %w", err)
	}
	m.logger.Info("agent.interactive.message_sent", "id", a.ID)
	return nil
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
// matching the provided role.
func (m *Manager) FindAllRunningAgentsForTask(taskID string, role Role) []*Agent {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []*Agent
	for _, a := range m.agents {
		if a.TaskID != taskID || !isLive(a.GetState()) {
			continue
		}
		if RoleFromName(a.Name) != role {
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

	// Both headless and interactive (conversational) agents run in goroutines.
	// Cancel context; goroutine calls onComplete and closes done after process exits.
	if a.TmuxSession != "" {
		m.markStopped(a)
		return nil
	}
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

func (m *Manager) CapturePane(agentID string) (string, error) {
	a, err := m.GetAgent(agentID)
	if err != nil {
		return "", err
	}
	if a.TmuxSession == "" {
		return "", fmt.Errorf("agent %s has no tmux session", agentID)
	}
	if a.GetState() == StateStopped {
		return "", nil
	}
	out, captureErr := m.tmux.CapturePaneOutput(a.TmuxSession)
	if captureErr != nil && !m.tmux.SessionExists(a.TmuxSession) {
		m.logger.Warn("agent.tmux.gone", "id", agentID, "session", a.TmuxSession)
		m.markStopped(a)
		return "", nil
	}
	return out, captureErr
}

// markStopped transitions an agent to stopped, kills its tmux session if any,
// and emits the state event.
func (m *Manager) markStopped(a *Agent) {
	a.SetState(StateStopped)
	if a.cancel != nil {
		a.cancel()
	}
	if a.TmuxSession != "" {
		_ = m.tmux.KillSession(a.TmuxSession)
	}
	// Close conversational stdin to signal process exit.
	a.stdinMu.Lock()
	if a.stdinPipe != nil {
		_ = a.stdinPipe.Close()
		a.stdinPipe = nil
	}
	a.stdinMu.Unlock()
	m.emit(events.AgentState(a.ID), a)
	if m.onComplete != nil {
		m.onComplete(a)
	}
}

// CheckInteractiveSessions detects dead or idle tmux sessions for interactive
// agents and marks them as stopped. Called periodically from the app layer.
func (m *Manager) CheckInteractiveSessions() {
	m.mu.RLock()
	var candidates []*Agent
	for _, a := range m.agents {
		if a.Mode == "interactive" && a.GetState() == StateRunning && a.TmuxSession != "" {
			candidates = append(candidates, a)
		}
	}
	m.mu.RUnlock()

	for _, a := range candidates {
		if !m.tmux.SessionExists(a.TmuxSession) {
			m.logger.Warn("agent.tmux.gone", "id", a.ID, "session", a.TmuxSession)
			m.markStopped(a)
			continue
		}
		// Plan agents deliberately sit idle between review rounds — don't
		// auto-stop them; only the dead-session check above applies.
		if RoleFromName(a.Name) == RolePlan {
			continue
		}
		// Resolve Claude session via tmux pane PID → ~/.claude/sessions/{pid}.json.
		// Codex agents use a different session model and do not have tmux sessions.
		if a.Provider != "codex" && a.GetSessionID() == "" {
			m.resolveInteractiveSession(a)
		}
		sid := a.GetSessionID()
		if sid == "" {
			continue
		}
		state := inferState(a.sessionCWD, sid)
		if state == StateIdle {
			m.logger.Info("agent.interactive.idle", "id", a.ID, "session", a.TmuxSession, "claude_session", sid)
			m.markStopped(a)
		}
	}
}

// resolveInteractiveSession reads the tmux pane PID, then looks up
// ~/.claude/sessions/{pid}.json to find the Claude session ID.
// Only valid for Claude agents; Codex does not use this mechanism.
func (m *Manager) resolveInteractiveSession(a *Agent) {
	pidStr, err := m.tmux.PanePID(a.TmuxSession)
	if err != nil {
		return
	}
	pidStr = strings.TrimSpace(pidStr)
	sess := readClaudeSessionByPID(pidStr)
	if sess.SessionID != "" {
		a.SetSessionID(sess.SessionID)
		if a.sessionCWD == "" {
			a.sessionCWD = sess.CWD
		}
	}
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

// ReconnectSessions scans tmux for surviving synapse-* sessions and rebuilds
// in-memory agent state for each. Called on startup so app restarts don't lose
// track of running interactive agents.
func (m *Manager) ReconnectSessions(tasks []TaskInfo) int {
	sessions, err := m.tmux.ListSessions()
	if err != nil {
		m.logger.Warn("reconnect.list", "err", err)
		return 0
	}

	taskBySession := make(map[string]TaskInfo)
	for _, t := range tasks {
		expected := fmt.Sprintf("synapse-%s-", sanitizeSessionName(t.Title))
		for _, s := range sessions {
			if strings.HasPrefix(s.Name, expected) {
				taskBySession[s.Name] = t
			}
		}
	}

	reconnected := 0
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, s := range sessions {
		if !strings.HasPrefix(s.Name, "synapse-") || s.Name == "synapse-orchestrator" {
			continue
		}
		// Skip if already tracked
		alreadyTracked := false
		for _, a := range m.agents {
			if a.TmuxSession == s.Name {
				alreadyTracked = true
				break
			}
		}
		if alreadyTracked {
			continue
		}

		// Extract short ID from session name (last segment after final -)
		parts := strings.Split(s.Name, "-")
		id := parts[len(parts)-1]

		t, hasTask := taskBySession[s.Name]
		if !hasTask {
			m.logger.Info("reconnect.kill-orphan", "session", s.Name)
			_ = m.tmux.KillSession(s.Name)
			continue
		}

		a := &Agent{
			ID:          id,
			Mode:        "interactive",
			State:       StateRunning,
			TmuxSession: s.Name,
			TaskID:      t.ID,
			Name:        t.Title,
			StartedAt:   time.Now().UTC(),
			LastEventAt: time.Now().UTC(),
		}

		m.agents[id] = a
		m.logger.Info("reconnect.session", "id", id, "session", s.Name, "task", a.TaskID)
		m.emit(events.AgentState(id), a)
		reconnected++
	}
	return reconnected
}

// TaskInfo is minimal task data needed for reconnection.
type TaskInfo struct {
	ID    string
	Title string
}

const maxSessionNameLen = 30

var sessionNameRe = regexp.MustCompile(`[^a-z0-9-]+`)

// safeArgRe matches only characters safe to embed in a tmux shell command
// without quoting: alphanumerics, dot, underscore, hyphen, forward-slash.
var safeArgRe = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)

func sanitizeSessionName(title string) string {
	s := strings.ToLower(strings.TrimSpace(title))
	s = strings.ReplaceAll(s, " ", "-")
	s = sessionNameRe.ReplaceAllString(s, "")
	s = strings.Trim(s, "-")
	if len(s) > maxSessionNameLen {
		s = s[:maxSessionNameLen]
		s = strings.TrimRight(s, "-")
	}
	if s == "" {
		return "task"
	}
	return s
}
