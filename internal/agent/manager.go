package agent

import (
	"context"
	"fmt"
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
}

func NewManager(ctx context.Context, tm *tmux.Manager, emit EmitFunc) *Manager {
	return &Manager{
		agents: make(map[string]*Agent),
		ctx:    ctx,
		tmux:   tm,
		emit:   emit,
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
			return nil, fmt.Errorf("create tmux session: %w", err)
		}
	default:
		cancel()
		return nil, fmt.Errorf("unknown mode: %s", mode)
	}

	m.emit("agent:state:"+id, a)
	return a, nil
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

func (m *Manager) Shutdown() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, a := range m.agents {
		a.cancel()
		if a.Mode == "interactive" && a.TmuxSession != "" {
			_ = m.tmux.KillSession(a.TmuxSession)
		}
	}
}
