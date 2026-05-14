package agent

import (
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/events"
)

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

	a.MarkStopped()
	// Send SIGINT first so CC can restore terminal modes and persist the
	// session ID for --resume. Escalate to SIGKILL only after the grace window.
	if a.Mode == "headless" {
		if cmd := a.GetCmd(); cmd != nil {
			stopWithSIGINT(cmd, a.done, stopSIGINTGrace)
		}
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

// defaultShutdownGrace bounds how long Shutdown waits for in-flight agents
// to notice the cancel signal and exit cleanly. Without this, agents get
// SIGKILL'd mid-stream and the final `result` NDJSON line is truncated,
// leaving operators with a half-written run log and a "signal: killed"
// exit error that carries no diagnostic value.
const defaultShutdownGrace = 20 * time.Second

// Shutdown cancels all running agents and blocks until they exit or the
// grace window elapses — whichever comes first. Subprocess runners use
// SIGTERM + cmd.WaitDelay (see shutdownSignal in runner helpers) so this
// grace gives claude/codex time to flush the final `result` event.
func (m *Manager) Shutdown() {
	m.ShutdownWithGrace(defaultShutdownGrace)
}

// ShutdownWithGrace is Shutdown with an explicit grace window. Used by
// tests (short grace) and callers that want to override the default.
func (m *Manager) ShutdownWithGrace(grace time.Duration) {
	m.mu.RLock()
	count := len(m.agents)
	dones := make([]chan struct{}, 0, count)
	for _, a := range m.agents {
		if a.cancel != nil {
			a.cancel()
		}
		if a.done != nil {
			dones = append(dones, a.done)
		}
	}
	m.mu.RUnlock()

	m.logger.Info("agent.shutdown", "count", count, "grace", grace, "wait", len(dones))
	if len(dones) == 0 || grace <= 0 {
		return
	}

	deadline := time.After(grace)
	for i, done := range dones {
		select {
		case <-done:
		case <-deadline:
			m.logger.Warn("agent.shutdown.timeout",
				"exited", i, "remaining", len(dones)-i, "grace", grace)
			return
		}
	}
	m.logger.Info("agent.shutdown.done", "exited", len(dones))
}
