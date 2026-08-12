package agent

import (
	"context"
	"fmt"
	"time"
)

// SendPromptToAgent delivers a follow-up prompt to a steerable headless
// claude agent via its stdin transport.
func (m *Manager) SendPromptToAgent(agentID, text string) error {
	a, err := m.GetAgent(agentID)
	if err != nil {
		return err
	}
	if a.GetState() == StateStopped {
		return conflictError(fmt.Sprintf("agent %s is stopped", agentID))
	}

	if _, execErr := m.executionForAgent(agentID); execErr == nil {
		return m.SendMessage(agentID, text)
	}
	if a.convo.hasStdinPipe() {
		return m.SendMessage(agentID, text)
	}

	return conflictError(fmt.Sprintf("agent %s has no active transport (no stdin pipe)", agentID))
}

// SendMessage sends a follow-up user message to a live steerable headless
// claude run (see RunConfig.HeadlessSteerable). When the agent is mid-turn
// (StateRunning), the message is appended to a pending queue and flushed on
// the next "result" event, so users can pile up follow-ups without waiting
// for each turn to settle. A run that has begun finalizing (no steer message
// was pending at its last result, so stdin was already closed) rejects
// rather than queuing a message that would never be delivered.
func (m *Manager) SendMessage(agentID, text string) error {
	a, err := m.GetAgent(agentID)
	if err != nil {
		return err
	}
	if a.Mode != "headless" {
		return conflictError(fmt.Sprintf("agent %s is not in headless steerable mode", agentID))
	}
	if execution, execErr := m.executionForAgent(agentID); execErr == nil {
		return execution.backend.Steer(m.ctx, execution.handle, text)
	}
	if !a.convo.hasStdinPipe() {
		return conflictError(fmt.Sprintf("agent %s has no stdin transport for follow-up messages", agentID))
	}
	// Restart-adopted legacy executions predate backend handles.
	return m.sendHeadlessSteerMessage(a, text)
}

// GetConvoOutput returns the full conversation event buffer for an agent.
func (m *Manager) GetConvoOutput(agentID string) ([]ConvoEvent, error) {
	a, err := m.GetAgent(agentID)
	if err != nil {
		return nil, err
	}
	return a.ConvoOutput(), nil
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
		if a.EffectiveRole() != role {
			continue
		}
		return a
	}
	return nil
}

// CountLiveByRole returns the number of live agents (across all tasks) whose
// role matches role. Used to enforce
// the per-machine test-runner concurrency cap independently of the global
// MaxConcurrent limit.
func (m *Manager) CountLiveByRole(role Role) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, a := range m.agents {
		if isLive(a.GetState()) && a.EffectiveRole() == role {
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
		if role != "" && a.EffectiveRole() != role {
			continue
		}
		result = append(result, a)
	}
	return result
}

// ReleaseStaleStoppedAgentsForTask releases manager liveness for agents that
// are already marked stopped but whose runner goroutine never closed its done
// channel. It does not stop running/paused agents. The caller supplies grace so
// short StopAgent races still keep protecting the worktree and dispatch path.
func (m *Manager) ReleaseStaleStoppedAgentsForTask(ctx context.Context, taskID string, grace time.Duration) int {
	if grace <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-grace)
	var stale []*Agent
	m.mu.RLock()
	for _, a := range m.agents {
		if a.TaskID != taskID || a.done == nil || a.GetState() != StateStopped {
			continue
		}
		if last := a.GetLastEventAt(); !last.IsZero() && last.After(cutoff) {
			continue
		}
		select {
		case <-a.done:
		default:
			stale = append(stale, a)
		}
	}
	m.mu.RUnlock()

	for _, a := range stale {
		if m.logger != nil {
			m.logger.Warn("agent.stale-stopped.release", "agent_id", a.ID, "task_id", taskID, "last_event_at", a.GetLastEventAt())
		}
		m.markAgentDone(ctx, a)
	}
	return len(stale)
}

// ReleaseDeadAgentsForTask releases manager liveness for task-scoped agents
// whose recorded PID is already gone. Unlike ReleaseStaleStoppedAgentsForTask
// this only fires when the OS confirms the process is dead, so recovery can
// safely clear a wedged "running" gate without cancelling a healthy agent.
func (m *Manager) ReleaseDeadAgentsForTask(ctx context.Context, taskID string) int {
	var dead []*Agent
	m.mu.RLock()
	for _, a := range m.agents {
		if a.TaskID != taskID || a.External {
			continue
		}
		pid := a.GetPID()
		if pid <= 0 || processAlive(pid) {
			continue
		}
		if a.done != nil {
			select {
			case <-a.done:
				continue
			default:
			}
		} else if !isLive(a.GetState()) {
			continue
		}
		dead = append(dead, a)
	}
	m.mu.RUnlock()

	for _, a := range dead {
		a.MarkStopped()
		if m.logger != nil {
			m.logger.Warn("agent.dead.release", "agent_id", a.ID, "task_id", taskID, "pid", a.GetPID())
		}
		m.markAgentDone(ctx, a)
	}
	return len(dead)
}

// StopAgents stops the provided agents without waiting for their goroutines to
// exit. Callers that need deterministic teardown (task delete/worktree
// cleanup) should use KillAgentsForTask instead.
func (m *Manager) StopAgents(agents []*Agent) {
	for _, a := range agents {
		if a == nil || a.GetState().IsTerminal() {
			continue
		}
		m.logger.Info("agent.stop-for-task", "agent_id", a.ID, "task_id", a.TaskID)
		if execution, err := m.executionForAgent(a.ID); err == nil {
			if err := execution.backend.Stop(m.ctx, execution.handle); err != nil {
				m.logger.Warn("agent.stop-for-task.backend", "agent_id", a.ID, "task_id", a.TaskID, "err", err)
			}
			continue
		}
		// Restart-adopted legacy executions have no backend handle yet.
		m.stopLocalAgent(a)
	}
}

// KillAgentsForTask stops all running agents for the given task ID and waits
// for their goroutines to exit (up to timeout). Safe to call from DeleteTask
// before worktree cleanup. allExited is false when the deadline hit before
// every targeted agent's goroutine confirmed exit — callers that need to know
// whether it is now actually safe to touch the task's worktree (as opposed to
// callers that just want a best-effort stop, like DeleteTask) must check it
// rather than assume a timed-out call still stopped everything.
func (m *Manager) KillAgentsForTask(taskID string, timeout time.Duration) (allExited bool) {
	return m.killAgentsForTask(taskID, "", timeout)
}

// KillOtherAgentsForTask is KillAgentsForTask excluding exceptAgentID. This is
// for completion callbacks where the completing agent's done channel cannot
// close until the callback returns.
func (m *Manager) KillOtherAgentsForTask(taskID, exceptAgentID string, timeout time.Duration) (allExited bool) {
	return m.killAgentsForTask(taskID, exceptAgentID, timeout)
}

func (m *Manager) killAgentsForTask(taskID, exceptAgentID string, timeout time.Duration) (allExited bool) {
	m.mu.RLock()
	var targets []*Agent
	for _, a := range m.agents {
		if a.TaskID == taskID && a.ID != exceptAgentID {
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
			return false
		}
	}
	return true
}

func (m *Manager) StopAgent(agentID string) error {
	m.mu.Lock()
	a, ok := m.agents[agentID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("agent %s not found", agentID)
	}
	if a.GetState().IsTerminal() {
		// Already stopped: no signal, log, or state-change event to repeat.
		// A caller that stops the same agent twice (e.g. reconciliation
		// re-selecting a not-yet-evicted terminal registry entry) must not
		// turn history into recurring control-plane work.
		return nil
	}

	m.logger.Info("agent.stop", "id", agentID)
	if execution, execErr := m.executionForAgent(agentID); execErr == nil {
		return execution.backend.Stop(m.ctx, execution.handle)
	}
	// Restart-adopted legacy executions have no backend handle yet.
	m.stopLocalAgent(a)
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

// ListLiveAgents returns only agents that are still live (isLive: running,
// or paused between conversational turns). A stopped agent's registry entry
// is retained after termination (see deadAgentRetention) purely so a caller
// polling right after a stop still observes its final state — that retained
// entry is history, not something still running. Callers that pick a live
// singleton or otherwise decide what needs stopping (e.g. orchestrator
// reconciliation) must use this instead of ListAgents, or they will keep
// re-selecting an already-terminal entry for as long as it sits in the
// retention window.
func (m *Manager) ListLiveAgents() []*Agent {
	m.mu.RLock()
	defer m.mu.RUnlock()
	agents := make([]*Agent, 0, len(m.agents))
	for _, a := range m.agents {
		if !isLive(a.GetState()) {
			continue
		}
		agents = append(agents, a)
	}
	return agents
}

// ActiveLogPaths returns the set of output-log paths owned by currently live
// agents (isLive: running, or paused between conversational turns). The
// per-agent log retention sweep (internal/logging.EnforceAgentLogRetention)
// uses this as a never-touch allowlist so a live agent's own NDJSON stream —
// and its .gz/.stderr siblings — is never deleted or compressed out from
// under it while it's still appending.
func (m *Manager) ActiveLogPaths() map[string]bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]bool, len(m.agents))
	for _, a := range m.agents {
		if !isLive(a.GetState()) {
			continue
		}
		if p := a.GetLogPath(); p != "" {
			out[p] = true
		}
	}
	return out
}

// HasRunningAgentForTask returns true if any agent is currently running for the given task.
// For headless agents this checks whether the goroutine has truly exited (via the done
// channel) rather than the State field, which may be set to Stopped by StopAgent before
// the goroutine finishes — avoiding a race where the worktree is cleaned up while the
// agent process is still using it.
func (m *Manager) HasRunningAgentForTask(taskID string) bool {
	now := time.Now()
	m.mu.RLock()
	// An in-flight dispatch counts as "running": the agent is mid-start and
	// not yet in the map, but a second dispatcher must not treat the task as
	// idle. Lets recovery / ResumeStalled / pr-fix pollers skip during the
	// worktree-prep window instead of racing the dispatch.
	dispatching, stale := m.dispatchClaimHeldReadLocked(taskID, now)
	live := m.hasLiveRegisteredAgent(taskID)
	m.mu.RUnlock()
	if !stale {
		return dispatching || live
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dispatchClaimHeldLocked(taskID, now) || m.hasLiveRegisteredAgent(taskID)
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
	now := time.Now()
	m.mu.RLock()
	dispatching, stale := m.dispatchClaimHeldReadLocked(taskID, now)
	live := m.hasLiveRegisteredAgentExcept(taskID, exceptAgentID)
	m.mu.RUnlock()
	if !stale {
		return dispatching || live
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dispatchClaimHeldLocked(taskID, now) || m.hasLiveRegisteredAgentExcept(taskID, exceptAgentID)
}

func (m *Manager) hasLiveRegisteredAgentExcept(taskID, exceptAgentID string) bool {
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
