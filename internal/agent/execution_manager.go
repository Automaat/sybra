package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/events"
)

type activeExecution struct {
	backend     ExecutionBackend
	handle      ExecutionHandle
	outputStart int
	lastEmit    *time.Time
	sink        ExecutionEventSink
}

type managerExecutionSink struct {
	mu          sync.Mutex
	manager     *Manager
	agent       *Agent
	outputStart int
	lastEmit    *time.Time
	backend     ExecutionBackend
	handle      ExecutionHandle
	stopPending bool
}

func (s *managerExecutionSink) EmitExecutionEvent(ctx context.Context, handle ExecutionHandle, event ExecutionEvent) {
	stop := s.manager.emitExecutionEvent(ctx, handle, event, s.agent, s.outputStart, s.lastEmit)
	if stop {
		s.stop(ctx)
	}
}

func (s *managerExecutionSink) bind(ctx context.Context, backend ExecutionBackend, handle ExecutionHandle) {
	s.mu.Lock()
	s.backend, s.handle = backend, handle
	pending := s.stopPending
	s.stopPending = false
	s.mu.Unlock()
	if pending {
		_ = backend.Stop(ctx, handle)
	}
}

func (s *managerExecutionSink) stop(ctx context.Context) {
	s.mu.Lock()
	backend, handle := s.backend, s.handle
	if backend == nil {
		s.stopPending = true
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	if err := backend.Stop(ctx, handle); err != nil {
		s.manager.logger.Warn("agent.execution.guardrail-stop", "agent_id", s.agent.ID, "err", err)
	}
}

// SetExecutionBackend selects the backend used by future runs. Existing runs
// retain the backend and opaque handle they started with. Nil restores local
// execution. Provider routing and admission remain manager-owned.
func (m *Manager) SetExecutionBackend(backend ExecutionBackend) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if backend == nil {
		backend = m.localExecutionBackend
	}
	m.executionBackend = backend
}

// LocalExecutionBackend returns the manager-owned local execution adapter for
// a composite scheduler that must honor an explicit per-run local fallback.
// Callers must not retain canonical Agent state; the returned backend reports
// lifecycle observations through the same ExecutionEventSink contract.
func (m *Manager) LocalExecutionBackend() ExecutionBackend {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.localExecutionBackend
}

// SetExecutionApprovalResponder wires the local approval transport into the
// backend control seam. Remote backends can implement RespondApproval without
// this callback; nil makes local approval explicitly unsupported.
func (m *Manager) SetExecutionApprovalResponder(responder func(string, bool) error) {
	m.mu.Lock()
	m.approvalResponder = responder
	m.mu.Unlock()
}

func (m *Manager) respondExecutionApproval(toolUseID string, approved bool) error {
	m.mu.RLock()
	responder := m.approvalResponder
	m.mu.RUnlock()
	if responder == nil {
		return ErrExecutionControlUnsupported
	}
	return responder(toolUseID, approved)
}

// RespondExecutionApproval routes an approval response to the backend that
// owns agentID. The tool-use identity remains opaque to the manager.
func (m *Manager) RespondExecutionApproval(agentID, toolUseID string, approved bool) error {
	execution, err := m.executionForAgent(agentID)
	if err != nil {
		return err
	}
	if err := execution.backend.RespondApproval(m.ctx, execution.handle, toolUseID, approved); err != nil {
		return err
	}
	if a, err := m.GetAgent(agentID); err == nil {
		a.SetAwaitingApproval(false)
		a.SetState(StateRunning)
		m.emit(events.AgentState(agentID), a)
	}
	return nil
}

// InspectExecution returns the owning backend's process view without exposing
// task or workflow state to the backend.
func (m *Manager) InspectExecution(ctx context.Context, agentID string) (ExecutionInspection, error) {
	execution, err := m.executionForAgent(agentID)
	if err != nil {
		return ExecutionInspection{}, err
	}
	return execution.backend.Inspect(ctx, execution.handle)
}

// RecoverExecution reattaches the manager event sink to an accepted backend
// run. Recovery must observe that run; the backend contract forbids starting a
// replacement execution from this method.
func (m *Manager) RecoverExecution(ctx context.Context, agentID string) error {
	execution, err := m.executionForAgent(agentID)
	if err != nil {
		return err
	}
	return execution.backend.Recover(ctx, execution.handle, execution.sink)
}

func (m *Manager) executionForAgent(agentID string) (activeExecution, error) {
	m.mu.RLock()
	execution, ok := m.activeExecutions[agentID]
	m.mu.RUnlock()
	if !ok {
		return activeExecution{}, fmt.Errorf("agent %s has no active execution", agentID)
	}
	return execution, nil
}

// EmitExecutionEvent implements ExecutionEventSink. It is intentionally the
// sole translation point from backend process observations into Agent state
// and completion callbacks.
func (m *Manager) EmitExecutionEvent(ctx context.Context, handle ExecutionHandle, event ExecutionEvent) {
	m.mu.RLock()
	agentID := m.executionAgents[handle]
	execution := m.activeExecutions[agentID]
	a := m.agents[agentID]
	m.mu.RUnlock()
	if a == nil {
		// Callback adapters may report Started synchronously before Start has
		// returned its handle to the manager. No state transition depends on it.
		return
	}
	if stop := m.emitExecutionEvent(ctx, handle, event, a, execution.outputStart, execution.lastEmit); stop {
		m.stopBackendOwnedRun(ctx, execution.backend, handle, a)
	}
}

// stopBackendOwnedRun carries a guardrail's stop decision to the backend that
// owns the process.
//
// A locally spawned run is torn down by the runner goroutine that owns its
// pipe. A run placed on another machine has no such goroutine here: without
// this the agent is marked stopped on the leader while the worker's process
// keeps running and spending, and the run then reports both a guardrail stop
// and the success the worker eventually sends.
//
// The backend is the run's own, not whichever one the manager currently
// selects: a run keeps the backend and opaque handle it started with, so a
// backend swapped mid-run would be handed a handle that means nothing to it,
// and the worker process would stay alive.
func (m *Manager) stopBackendOwnedRun(ctx context.Context, backend ExecutionBackend, handle ExecutionHandle, a *Agent) {
	if !a.RemotelyExecuted() || backend == nil {
		return
	}
	if err := backend.Stop(context.WithoutCancel(ctx), handle); err != nil {
		m.logger.Error("agent.remote.stop", "id", a.ID, "reason", a.GetEscalationReason(), "err", err)
		return
	}
	m.logger.Warn("agent.remote.stop", "id", a.ID, "reason", a.GetEscalationReason())
}

func (m *Manager) emitExecutionEvent(ctx context.Context, _ ExecutionHandle, event ExecutionEvent, a *Agent, outputStart int, lastEmit *time.Time) bool {
	switch event.Kind {
	case ExecutionStarted:
		a.setBackendOwnsCompletion(event.BackendOwnsCompletion)
		if event.Command != "" {
			a.SetCommand(event.Command)
		}
		m.emit(events.AgentState(a.ID), a)
		return false
	case ExecutionOutput:
		providerName := event.Provider
		if providerName == "" {
			providerName = a.Provider
		}
		if event.OutputParsed {
			a.SetRemotelyExecuted()
			var parsed StreamEvent
			if err := json.Unmarshal(event.Output, &parsed); err != nil {
				m.logger.Warn("agent.execution.parsed-output", "id", a.ID, "err", err)
				return false
			}
			return m.applyHeadlessEvent(ctx, a, parsed, lastEmit, providerByName(providerName), false)
		}
		return m.processHeadlessLine(ctx, a, event.Output, lastEmit, providerByName(providerName))
	case ExecutionApproval:
		if event.Approval != nil {
			a.SetAwaitingApproval(true)
			a.SetState(StatePaused)
			m.emit(events.AgentApproval(a.ID), *event.Approval)
			m.emit(events.AgentState(a.ID), a)
		}
		return false
	case ExecutionCompleted:
		if event.PermanentFailure {
			a.SetEscalationReason(EscalationReasonPermanentExecution)
		}
		if event.Err != nil {
			a.SetExitErr(event.Err)
		} else {
			m.finalizeFromResult(a, outputStart)
		}
		m.finalizeRun(ctx, a, "agent.execution.done")
		return false
	}
	return false
}

func (m *Manager) unregisterExecution(agentID string) {
	m.mu.Lock()
	if execution, ok := m.activeExecutions[agentID]; ok {
		delete(m.executionAgents, execution.handle)
	}
	delete(m.activeExecutions, agentID)
	m.mu.Unlock()
}

func (m *Manager) stopLocalAgent(a *Agent) {
	if a == nil || a.GetState().IsTerminal() {
		return
	}
	a.MarkStopped()
	if a.Mode == "headless" || a.isDetached() {
		m.signalKill(a)
	}
	if a.cancel != nil {
		a.cancel()
	}
	a.SetState(StateStopped)
	a.convo.closeStdinPipe()
	m.emit(events.AgentState(a.ID), a)
}
