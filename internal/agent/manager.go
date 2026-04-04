package agent

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/synapse/internal/tmux"
	"github.com/google/uuid"
)

type EmitFunc func(event string, data any)

type Manager struct {
	agents     map[string]*Agent
	mu         sync.RWMutex
	ctx        context.Context
	tmux       *tmux.Manager
	emit       EmitFunc
	onComplete func(ag *Agent)
	logger     *slog.Logger
	logDir     string
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

func (m *Manager) StartAgent(taskID, taskTitle, mode, prompt string, allowedTools []string) (*Agent, error) {
	return m.Run(RunConfig{TaskID: taskID, Name: taskTitle, Mode: mode, Prompt: prompt, AllowedTools: allowedTools})
}

func (m *Manager) StartAgentInDir(taskID, taskTitle, mode, prompt string, allowedTools []string, dir string) (*Agent, error) {
	return m.Run(RunConfig{TaskID: taskID, Name: taskTitle, Mode: mode, Prompt: prompt, AllowedTools: allowedTools, Dir: dir})
}

func (m *Manager) Run(cfg RunConfig) (*Agent, error) {
	id := uuid.NewString()[:8]
	ctx, cancel := context.WithCancel(m.ctx)

	a := &Agent{
		ID:         id,
		TaskID:     cfg.TaskID,
		Name:       cfg.Name,
		Mode:       cfg.Mode,
		Model:      cfg.Model,
		State:      StateRunning,
		StartedAt:  time.Now().UTC(),
		cancel:     cancel,
		sessionCWD: cfg.Dir,
	}

	m.mu.Lock()
	m.agents[id] = a
	m.mu.Unlock()

	m.logger.Info("agent.start", "id", id, "taskID", cfg.TaskID, "mode", cfg.Mode, "model", cfg.Model)

	switch cfg.Mode {
	case "headless":
		go m.runHeadless(ctx, a, cfg.Prompt, cfg.AllowedTools)
	case "interactive":
		a.TmuxSession = fmt.Sprintf("synapse-%s-%s", sanitizeSessionName(cfg.Name), id)
		claudeCmd := m.buildClaudeCmd(cfg)
		var createErr error
		if cfg.Dir != "" {
			createErr = m.tmux.CreateSessionInDir(a.TmuxSession, claudeCmd, cfg.Dir)
		} else {
			createErr = m.tmux.CreateSession(a.TmuxSession, claudeCmd)
		}
		if createErr != nil {
			cancel()
			m.mu.Lock()
			delete(m.agents, id)
			m.mu.Unlock()
			m.logger.Error("agent.tmux.create", "id", id, "err", createErr)
			return nil, fmt.Errorf("create tmux session: %w", createErr)
		}
		if cfg.Prompt != "" {
			go m.sendInteractivePrompt(ctx, a, cfg.Prompt)
		}
	default:
		cancel()
		return nil, fmt.Errorf("unknown mode: %s", cfg.Mode)
	}

	m.emit("agent:state:"+id, a)
	return a, nil
}

func (m *Manager) buildClaudeCmd(cfg RunConfig) string {
	parts := []string{"claude"}
	if len(cfg.AllowedTools) > 0 {
		parts = append(parts, "--allowedTools", strings.Join(cfg.AllowedTools, ","))
	} else {
		parts = append(parts, "--dangerously-skip-permissions")
	}
	if cfg.Model != "" {
		parts = append(parts, "--model", cfg.Model)
	}
	return strings.Join(parts, " ")
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

func (m *Manager) StopAgent(agentID string) error {
	m.mu.Lock()
	a, ok := m.agents[agentID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("agent %s not found", agentID)
	}

	if a.cancel != nil {
		a.cancel()
	}
	a.State = StateStopped

	if a.Mode == "interactive" && a.TmuxSession != "" {
		_ = m.tmux.KillSession(a.TmuxSession)
	}

	m.logger.Info("agent.stop", "id", agentID)
	m.emit("agent:state:"+agentID, a)
	if m.onComplete != nil {
		m.onComplete(a)
	}
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
// For headless agents, verifies the process is still alive.
func (m *Manager) HasRunningAgentForTask(taskID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, a := range m.agents {
		if a.TaskID != taskID || a.State != StateRunning {
			continue
		}
		if a.Mode == "headless" && a.cmd != nil && a.cmd.ProcessState != nil {
			continue // process exited, state is stale
		}
		return true
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

// markStopped transitions an agent to stopped and emits the state event.
func (m *Manager) markStopped(a *Agent) {
	a.State = StateStopped
	if a.cancel != nil {
		a.cancel()
	}
	m.emit("agent:state:"+a.ID, a)
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
		// Only cancel headless agents — interactive tmux sessions survive restarts
		if a.Mode == "headless" && a.cancel != nil {
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

		a := &Agent{
			ID:          id,
			Mode:        "interactive",
			State:       StateRunning,
			TmuxSession: s.Name,
			StartedAt:   time.Now().UTC(),
		}
		if t, ok := taskBySession[s.Name]; ok {
			a.TaskID = t.ID
			a.Name = t.Title
		} else {
			a.Name = s.Name
		}

		m.agents[id] = a
		m.logger.Info("reconnect.session", "id", id, "session", s.Name, "task", a.TaskID)
		m.emit("agent:state:"+id, a)
		reconnected++
	}
	return reconnected
}

// TaskInfo is minimal task data needed for reconnection.
type TaskInfo struct {
	ID    string
	Title string
}

var sessionNameRe = regexp.MustCompile(`[^a-z0-9-]+`)

func sanitizeSessionName(title string) string {
	s := strings.ToLower(strings.TrimSpace(title))
	s = strings.ReplaceAll(s, " ", "-")
	s = sessionNameRe.ReplaceAllString(s, "")
	s = strings.Trim(s, "-")
	if len(s) > 30 {
		s = s[:30]
		s = strings.TrimRight(s, "-")
	}
	if s == "" {
		return "task"
	}
	return s
}
