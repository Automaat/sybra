package agent

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/synapse/internal/events"
	"github.com/Automaat/synapse/internal/tmux"
	"github.com/google/uuid"
)

type EmitFunc func(event string, data any)

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
	approvalAddr  string // localhost:port for the HTTP tool approval server
}

func NewManager(ctx context.Context, tm *tmux.Manager, emit EmitFunc, logger *slog.Logger, logDir string) *Manager {
	return &Manager{
		agents: make(map[string]*Agent),
		ctx:    ctx,
		tmux:   tm,
		emit:   emit,
		logger: logger,
		logDir: logDir,
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
		} else if a.State == StateRunning {
			count++
		}
	}
	return count
}

func (m *Manager) StartAgent(taskID, taskTitle, mode, prompt string, allowedTools []string) (*Agent, error) {
	return m.Run(RunConfig{TaskID: taskID, Name: taskTitle, Mode: mode, Prompt: prompt, AllowedTools: allowedTools})
}

func (m *Manager) StartAgentInDir(taskID, taskTitle, mode, prompt string, allowedTools []string, dir string) (*Agent, error) {
	return m.Run(RunConfig{TaskID: taskID, Name: taskTitle, Mode: mode, Prompt: prompt, AllowedTools: allowedTools, Dir: dir})
}

func (m *Manager) Run(cfg RunConfig) (*Agent, error) {
	id := uuid.NewString()[:8]
	ctx, cancel := context.WithCancel(m.ctx)

	now := time.Now().UTC()
	a := &Agent{
		ID:          id,
		TaskID:      cfg.TaskID,
		Name:        cfg.Name,
		Mode:        cfg.Mode,
		Model:       cfg.Model,
		State:       StateRunning,
		StartedAt:   now,
		LastEventAt: now,
		cancel:      cancel,
		sessionCWD:  cfg.Dir,
	}
	if cfg.Mode == "headless" || cfg.Mode == "interactive" {
		a.done = make(chan struct{})
	}

	m.mu.Lock()
	if m.maxConcurrent > 0 && m.runningCountLocked() >= m.maxConcurrent {
		m.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("max concurrent agents reached (%d)", m.maxConcurrent)
	}
	m.agents[id] = a
	m.mu.Unlock()

	m.logger.Info("agent.start", "id", id, "taskID", cfg.TaskID, "mode", cfg.Mode, "model", cfg.Model)

	switch cfg.Mode {
	case "headless":
		go m.runHeadless(ctx, a, cfg.Prompt, cfg.AllowedTools, cfg.RequirePermissions)
	case "interactive":
		a.approvalCh = make(chan ApprovalResponse, 1)
		go m.runConversational(ctx, a, cfg)
	default:
		cancel()
		return nil, fmt.Errorf("unknown mode: %s", cfg.Mode)
	}

	m.emit(events.AgentState(id), a)
	return a, nil
}

func (m *Manager) buildClaudeCmd(cfg RunConfig) (string, error) {
	if cfg.Model != "" && !safeArgRe.MatchString(cfg.Model) {
		return "", fmt.Errorf("invalid model %q: must match %s", cfg.Model, safeArgRe)
	}
	for _, tool := range cfg.AllowedTools {
		if !safeArgRe.MatchString(tool) {
			return "", fmt.Errorf("invalid tool %q: must match %s", tool, safeArgRe)
		}
	}

	parts := []string{"claude"}
	if len(cfg.AllowedTools) > 0 {
		parts = append(parts, "--allowedTools", strings.Join(cfg.AllowedTools, ","))
	} else if !cfg.RequirePermissions {
		parts = append(parts, "--dangerously-skip-permissions")
	}
	if cfg.Model != "" {
		parts = append(parts, "--model", cfg.Model)
	}
	return strings.Join(parts, " "), nil
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
			m.logger.Error("agent.interactive.timeout", "id", a.ID, "msg", "claude did not become ready in 30s")
			return
		case <-ticker.C:
			out, err := m.tmux.CapturePaneOutput(a.TmuxSession)
			if err != nil {
				continue
			}
			// Handle --dangerously-skip-permissions confirmation dialog
			if !bypassAccepted && strings.Contains(out, "Yes, I accept") {
				_ = m.tmux.SendRawKeys(a.TmuxSession, "Down")
				time.Sleep(200 * time.Millisecond)
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
				// Delay so Claude Code processes the paste before submitting
				time.Sleep(500 * time.Millisecond)
				_ = m.tmux.SendRawKeys(a.TmuxSession, "Enter")
				m.logger.Info("agent.interactive.prompt_sent", "id", a.ID)
				return
			}
		}
	}
}

// SendPromptToAgent pastes text into an interactive agent's tmux session
// and submits it. Assumes the agent is idle (at its chat prompt).
func (m *Manager) SendPromptToAgent(agentID, text string) error {
	a, err := m.GetAgent(agentID)
	if err != nil {
		return err
	}
	if a.Mode != "interactive" || a.TmuxSession == "" {
		return fmt.Errorf("agent %s is not an interactive tmux agent", agentID)
	}
	if !m.tmux.SessionExists(a.TmuxSession) {
		return fmt.Errorf("tmux session %s does not exist", a.TmuxSession)
	}
	if err := m.tmux.SendKeys(a.TmuxSession, text); err != nil {
		return fmt.Errorf("send keys: %w", err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := m.tmux.SendRawKeys(a.TmuxSession, "Enter"); err != nil {
		return fmt.Errorf("send enter: %w", err)
	}
	m.logger.Info("agent.interactive.message_sent", "id", a.ID)
	return nil
}

// FindRunningAgentForTask returns the first running agent for the given task
// matching the provided role. Returns nil if none found.
func (m *Manager) FindRunningAgentForTask(taskID string, role Role) *Agent {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, a := range m.agents {
		if a.TaskID != taskID || a.State != StateRunning {
			continue
		}
		if RoleFromName(a.Name) != role {
			continue
		}
		return a
	}
	return nil
}

// FindAllRunningAgentsForTask returns all running agents for the given task
// matching the provided role.
func (m *Manager) FindAllRunningAgentsForTask(taskID string, role Role) []*Agent {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []*Agent
	for _, a := range m.agents {
		if a.TaskID != taskID || a.State != StateRunning {
			continue
		}
		if RoleFromName(a.Name) != role {
			continue
		}
		result = append(result, a)
	}
	return result
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
	if a.cancel != nil {
		a.cancel()
	}
	a.State = StateStopped
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
		} else if a.State == StateRunning {
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
	if a.State == StateStopped {
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
	a.State = StateStopped
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
		if a.Mode == "interactive" && a.State == StateRunning && a.TmuxSession != "" {
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
		// Resolve claude session via tmux pane PID → ~/.claude/sessions/{pid}.json
		if a.SessionID == "" {
			m.resolveInteractiveSession(a)
		}
		if a.SessionID == "" {
			continue
		}
		state := inferState(a.sessionCWD, a.SessionID)
		if state == StateIdle {
			m.logger.Info("agent.interactive.idle", "id", a.ID, "session", a.TmuxSession, "claude_session", a.SessionID)
			m.markStopped(a)
		}
	}
}

// resolveInteractiveSession reads the tmux pane PID, then looks up
// ~/.claude/sessions/{pid}.json to find the claude session ID.
func (m *Manager) resolveInteractiveSession(a *Agent) {
	pidStr, err := m.tmux.PanePID(a.TmuxSession)
	if err != nil {
		return
	}
	pidStr = strings.TrimSpace(pidStr)
	sess := readClaudeSessionByPID(pidStr)
	if sess.SessionID != "" {
		a.SessionID = sess.SessionID
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
