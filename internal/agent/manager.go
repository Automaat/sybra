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
	return m.StartAgentInDir(taskID, taskTitle, mode, prompt, allowedTools, "")
}

func (m *Manager) StartAgentInDir(taskID, taskTitle, mode, prompt string, allowedTools []string, dir string) (*Agent, error) {
	id := uuid.NewString()[:8]
	ctx, cancel := context.WithCancel(m.ctx)

	a := &Agent{
		ID:         id,
		TaskID:     taskID,
		Name:       taskTitle,
		Mode:       mode,
		State:      StateRunning,
		StartedAt:  time.Now().UTC(),
		cancel:     cancel,
		sessionCWD: dir,
	}

	m.mu.Lock()
	m.agents[id] = a
	m.mu.Unlock()

	m.logger.Info("agent.start", "id", id, "taskID", taskID, "mode", mode)

	switch mode {
	case "headless":
		go m.runHeadless(ctx, a, prompt, allowedTools)
	case "interactive":
		a.TmuxSession = fmt.Sprintf("synapse-%s-%s", sanitizeSessionName(taskTitle), id)
		var createErr error
		if dir != "" {
			createErr = m.tmux.CreateSessionInDir(a.TmuxSession, "claude", dir)
		} else {
			createErr = m.tmux.CreateSession(a.TmuxSession, "claude")
		}
		if createErr != nil {
			cancel()
			m.mu.Lock()
			delete(m.agents, id)
			m.mu.Unlock()
			m.logger.Error("agent.tmux.create", "id", id, "err", createErr)
			return nil, fmt.Errorf("create tmux session: %w", createErr)
		}
		if prompt != "" {
			go m.sendInteractivePrompt(ctx, a, prompt)
		}
	default:
		cancel()
		return nil, fmt.Errorf("unknown mode: %s", mode)
	}

	m.emit("agent:state:"+id, a)
	return a, nil
}

func (m *Manager) sendInteractivePrompt(ctx context.Context, a *Agent, prompt string) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

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
			if strings.Contains(out, "❯") {
				if err := m.tmux.SendKeys(a.TmuxSession, prompt); err != nil {
					m.logger.Error("agent.interactive.sendkeys", "id", a.ID, "err", err)
				} else {
					m.logger.Info("agent.interactive.prompt_sent", "id", a.ID)
				}
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

	a.cancel()
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
func (m *Manager) HasRunningAgentForTask(taskID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, a := range m.agents {
		if a.TaskID == taskID && a.State == StateRunning {
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
	return m.tmux.CapturePaneOutput(a.TmuxSession)
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
