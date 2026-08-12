package agent

import (
	"context"
	"fmt"
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
	manager     *Manager
	agent       *Agent
	outputStart int
	lastEmit    *time.Time
}

func (s *managerExecutionSink) EmitExecutionEvent(ctx context.Context, handle ExecutionHandle, event ExecutionEvent) {
	s.manager.emitExecutionEvent(ctx, handle, event, s.agent, s.outputStart, s.lastEmit)
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
	return execution.backend.RespondApproval(m.ctx, execution.handle, toolUseID, approved)
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
	var execution activeExecution
	var a *Agent
	for agentID, candidate := range m.activeExecutions {
		if candidate.handle == handle {
			execution = candidate
			a = m.agents[agentID]
			break
		}
	}
	m.mu.RUnlock()
	if a == nil {
		// Callback adapters may report Started synchronously before Start has
		// returned its handle to the manager. No state transition depends on it.
		return
	}
	m.emitExecutionEvent(ctx, handle, event, a, execution.outputStart, execution.lastEmit)
}

func (m *Manager) emitExecutionEvent(ctx context.Context, _ ExecutionHandle, event ExecutionEvent, a *Agent, outputStart int, lastEmit *time.Time) {
	switch event.Kind {
	case ExecutionStarted:
		if event.Command != "" {
			a.SetCommand(event.Command)
		}
		m.emit(events.AgentState(a.ID), a)
	case ExecutionOutput:
		providerName := event.Provider
		if providerName == "" {
			providerName = a.Provider
		}
		m.processHeadlessLine(ctx, a, event.Output, lastEmit, providerByName(providerName))
	case ExecutionCompleted:
		if event.Err != nil {
			a.SetExitErr(event.Err)
		} else {
			m.finalizeFromResult(a, outputStart)
		}
		m.finalizeRun(ctx, a, "agent.execution.done")
	}
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
