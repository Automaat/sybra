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
	if a.convo.hasStdinPipe() {
		return m.SendMessage(agentID, text)
	}

	// Per-turn conversational agents (codex/copilot): deliver via promptCh.
	if a.hasPromptChannel() {
		return m.sendConvoPrompt(agentID, text)
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

// CountLiveByRole returns the number of live agents (across all tasks) whose
// role — derived from the agent name prefix — matches role. Used to enforce
// the per-machine test-runner concurrency cap independently of the global
// MaxConcurrent limit.
func (m *Manager) CountLiveByRole(role Role) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, a := range m.agents {
		if isLive(a.GetState()) && RoleFromName(a.Name) == role {
			n++
		}
	}
	return n
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

// StopAgents stops the provided agents without waiting for their goroutines to
// exit. Callers that need deterministic teardown (task delete/worktree
// cleanup) should use KillAgentsForTask instead.
func (m *Manager) StopAgents(agents []*Agent) {
	for _, a := range agents {
		if a == nil {
			continue
		}
		m.logger.Info("agent.stop-for-task", "agent_id", a.ID, "task_id", a.TaskID)
		// Detached children do not observe stdin EOF or parent ctx cancel, so
		// signal them directly before canceling to actually free the pool slot.
		if a.isDetached() {
			a.MarkStopped()
			m.signalKill(a)
		}
		if a.cancel != nil {
			a.cancel()
		}
		a.SetState(StateStopped)
		a.convo.closeStdinPipe()
		m.emit(events.AgentState(a.ID), a)
	}
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

	m.StopAgents(targets)

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
	// session ID for --resume. Escalate to SIGKILL only after the grace
	// window. Detached agents (headless, or interactive whose stdin is a
	// never-EOF O_RDWR FIFO) cannot be stopped by closing stdin, so they
	// must be signalled by PID/handle.
	if a.Mode == "headless" || a.isDetached() {
		m.signalKill(a)
	}
	if a.cancel != nil {
		a.cancel()
	}
	a.SetState(StateStopped)
	// Close stdin to signal the claude process to exit.
	a.convo.closeStdinPipe()
	m.emit(events.AgentState(agentID), a)
	return nil
}

// StopCompletedAgent force-stops a headless agent whose stream already ended
// in a clean terminal result but whose process never exited (e.g. a
// non-detached run, or a detached run whose tailer goroutine died before it
// could finalize). Unlike StopAgent, it marks the agent as completed-by-result
// first, so the runner's own exit-status handling (runHeadlessAttemptPipe,
// tailHeadlessFile) derives completion from the terminal result event instead
// of misreading the kill signal in cmd.Wait()'s error as a failed/stopped run.
func (m *Manager) StopCompletedAgent(agentID string) error {
	m.mu.Lock()
	a, ok := m.agents[agentID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("agent %s not found", agentID)
	}
	if a.Mode != "headless" || !a.CompletedSuccessfully() {
		return fmt.Errorf("agent %s is not a completed headless agent", agentID)
	}
	a.setCompletedByResult(true)
	return m.StopAgent(agentID)
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
	// An in-flight dispatch counts as "running": the agent is mid-start and
	// not yet in the map, but a second dispatcher must not treat the task as
	// idle. Lets recovery / ResumeStalled / pr-fix pollers skip during the
	// worktree-prep window instead of racing the dispatch.
	if _, held := m.dispatchClaims[taskID]; held {
		return true
	}
	return m.hasLiveRegisteredAgent(taskID)
}

// HasLiveRegisteredAgentForTask reports whether a genuinely registered Agent
// (already past dispatch setup) is live for taskID — unlike
// HasRunningAgentForTask, it does NOT treat an in-flight dispatch claim as
// "running". Used to gate worktree mutation (worktree.AgentChecker) from
// inside the very dispatch call that holds the claim: that caller already IS
// the in-flight dispatch, so checking dispatchClaims there would always see
// its own claim and deadlock against itself. It must instead ask "is some
// OTHER, already-started agent process still using this worktree?".
func (m *Manager) HasLiveRegisteredAgentForTask(taskID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.hasLiveRegisteredAgent(taskID)
}

// HasLiveHeadlessAgentForTask reports whether a registered headless agent is
// still live for taskID. It intentionally ignores dispatch claims and
// conversational agents: the headless watchdog is the only alternate stall
// detector that can replace dwell escalation for a stale task file.
func (m *Manager) HasLiveHeadlessAgentForTask(taskID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, a := range m.agents {
		if a.TaskID != taskID || a.Mode != "headless" {
			continue
		}
		if a.done == nil {
			if a.GetState() == StateRunning {
				return true
			}
			continue
		}
		select {
		case <-a.done:
		default:
			return true
		}
	}
	return false
}

// hasLiveRegisteredAgent is the shared core of HasRunningAgentForTask and
// HasLiveRegisteredAgentForTask. Callers must hold m.mu (read lock is
// sufficient).
func (m *Manager) hasLiveRegisteredAgent(taskID string) bool {
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

// HasOtherRunningAgentForTask is HasRunningAgentForTask excluding the agent
// with exceptAgentID. verify_commits uses it to detect a sibling agent still
// working the task without false-positiving on the agent whose own completion
// is currently being processed — that agent's `done` channel is closed only
// after onComplete returns (see runner_headless), so it would otherwise still
// read as running.
func (m *Manager) HasOtherRunningAgentForTask(taskID, exceptAgentID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, held := m.dispatchClaims[taskID]; held {
		return true
	}
	for _, a := range m.agents {
		if a.TaskID != taskID || a.ID == exceptAgentID {
			continue
		}
		if a.done != nil {
			select {
			case <-a.done:
			default:
				return true
			}
		} else if a.GetState() == StateRunning {
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
	cancelled := make([]*Agent, 0, count)
	survived := 0
	for _, a := range m.agents {
		// Detached agents are meant to outlive the app: do not cancel
		// them and do not wait on their done channel (it stays open).
		if a.isDetached() {
			survived++
			continue
		}
		if a.cancel != nil {
			a.cancel()
		}
		if a.done != nil {
			cancelled = append(cancelled, a)
		}
	}
	m.mu.RUnlock()

	m.logger.Info("agent.shutdown", "count", count, "grace", grace, "wait", len(cancelled), "survived", survived)
	if len(cancelled) == 0 || grace <= 0 {
		return
	}

	deadline := time.After(grace)
	exited := 0
	for i, a := range cancelled {
		select {
		case <-a.done:
			exited++
		case <-deadline:
			m.logger.Warn("agent.shutdown.timeout",
				"exited", i, "remaining", len(cancelled)-i, "grace", grace)
			m.evictShutdownAgents(cancelled[:i])
			return
		}
	}
	m.logger.Info("agent.shutdown.done", "exited", exited)

	// The process is going away: markAgentDone already scheduled a delayed
	// eviction for each agent (deadAgentRetention), but there is no reader
	// left to benefit from that window once Shutdown returns, so evict
	// synchronously rather than leaking every agent that ever ran into a
	// registry no one will read again.
	m.evictShutdownAgents(cancelled)
}

// evictShutdownAgents removes the given agents from the live registry,
// guarding against an ID being reused by a new registration in the
// (vanishingly unlikely, shutdown-path) window between cancellation and
// eviction.
func (m *Manager) evictShutdownAgents(agents []*Agent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range agents {
		if cur, ok := m.agents[a.ID]; ok && cur == a {
			delete(m.agents, a.ID)
		}
	}
}
