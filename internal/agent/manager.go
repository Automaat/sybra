package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/synapse/internal/tmux"
	"github.com/google/uuid"
)

type EmitFunc func(event string, data any)

type Manager struct {
	agents map[string]*Agent
	mu     sync.RWMutex
	ctx    context.Context
	tmux   *tmux.Manager
	emit   EmitFunc
	logger *slog.Logger
	logDir string
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

func (m *Manager) StartAgent(taskID, mode, prompt string, allowedTools []string) (*Agent, error) {
	id := uuid.NewString()[:8]
	ctx, cancel := context.WithCancel(m.ctx)

	a := &Agent{
		ID:        id,
		TaskID:    taskID,
		Mode:      mode,
		State:     StateRunning,
		StartedAt: time.Now().UTC(),
		cancel:    cancel,
	}

	m.mu.Lock()
	m.agents[id] = a
	m.mu.Unlock()

	m.logger.Info("agent.start", "id", id, "taskID", taskID, "mode", mode)

	switch mode {
	case "headless":
		go m.runHeadless(ctx, a, prompt, allowedTools)
	case "interactive":
		a.TmuxSession = fmt.Sprintf("synapse-%s", id)
		if err := m.tmux.CreateSession(a.TmuxSession, "claude"); err != nil {
			cancel()
			m.mu.Lock()
			delete(m.agents, id)
			m.mu.Unlock()
			m.logger.Error("agent.tmux.create", "id", id, "err", err)
			return nil, fmt.Errorf("create tmux session: %w", err)
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
		a.cancel()
		if a.Mode == "interactive" && a.TmuxSession != "" {
			_ = m.tmux.KillSession(a.TmuxSession)
		}
	}
}
