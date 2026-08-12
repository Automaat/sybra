package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/providerid"
)

type sinkDrivenFakeBackend struct {
	mu      sync.Mutex
	handle  ExecutionHandle
	start   ExecutionStart
	release chan struct{}
	stops   int
	steers  []string
	answers []ApprovalResponse
}

func newSinkDrivenFakeBackend() *sinkDrivenFakeBackend {
	return &sinkDrivenFakeBackend{release: make(chan struct{})}
}

func (b *sinkDrivenFakeBackend) Start(ctx context.Context, start ExecutionStart) (ExecutionHandle, error) {
	b.mu.Lock()
	b.handle = ExecutionHandle("fake:" + start.Agent.ID)
	b.start = start
	handle := b.handle
	b.mu.Unlock()
	go func() {
		select {
		case <-b.release:
		case <-ctx.Done():
			return
		}
		start.Sink.EmitExecutionEvent(ctx, handle, ExecutionEvent{
			Kind: ExecutionOutput, Provider: providerid.Claude,
			Output: []byte(`{"type":"result","subtype":"success","result":"fake complete","session_id":"fake-session","total_cost_usd":0.01}`),
		})
		start.Sink.EmitExecutionEvent(ctx, handle, ExecutionEvent{Kind: ExecutionCompleted})
	}()
	return handle, nil
}

func (b *sinkDrivenFakeBackend) Stop(context.Context, ExecutionHandle) error {
	b.mu.Lock()
	b.stops++
	b.mu.Unlock()
	return nil
}

func (b *sinkDrivenFakeBackend) Steer(_ context.Context, _ ExecutionHandle, text string) error {
	b.mu.Lock()
	b.steers = append(b.steers, text)
	b.mu.Unlock()
	return nil
}

func (b *sinkDrivenFakeBackend) RespondApproval(_ context.Context, _ ExecutionHandle, toolUseID string, approved bool) error {
	b.mu.Lock()
	b.answers = append(b.answers, ApprovalResponse{ToolUseID: toolUseID, Approved: approved})
	b.mu.Unlock()
	return nil
}

func (b *sinkDrivenFakeBackend) Inspect(_ context.Context, handle ExecutionHandle) (ExecutionInspection, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if handle != b.handle {
		return ExecutionInspection{}, errors.New("unknown handle")
	}
	return ExecutionInspection{Handle: handle, State: "running", Agent: b.start.Agent.View()}, nil
}

func (b *sinkDrivenFakeBackend) Recover(_ context.Context, handle ExecutionHandle, sink ExecutionEventSink) error {
	if sink == nil || handle == "" {
		return errors.New("invalid recovery")
	}
	return nil
}

func TestExecutionBackendFakeDrivesCompleteManagerRun(t *testing.T) {
	completed := make(chan *Agent, 1)
	m, _ := newTestManager(t, ManagerConfig{OnComplete: func(a *Agent) { completed <- a }})
	backend := newSinkDrivenFakeBackend()
	m.SetExecutionBackend(backend)

	a, err := m.Run(RunConfig{Mode: "headless", Name: "fake run", Dir: t.TempDir(), Prompt: "not executed"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	inspection, err := m.InspectExecution(t.Context(), a.ID)
	if err != nil {
		t.Fatalf("InspectExecution: %v", err)
	}
	if inspection.Handle == "" || inspection.Agent.ID != a.ID {
		t.Fatalf("inspection = %+v", inspection)
	}
	if err := m.RespondExecutionApproval(a.ID, "tool-1", true); err != nil {
		t.Fatalf("RespondExecutionApproval: %v", err)
	}
	if err := m.SendMessage(a.ID, "continue remotely"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if err := m.RecoverExecution(t.Context(), a.ID); err != nil {
		t.Fatalf("RecoverExecution: %v", err)
	}

	close(backend.release)
	select {
	case got := <-completed:
		if got.ID != a.ID || got.GetExitErr() != nil {
			t.Fatalf("completed agent = %+v, err=%v", got.View(), got.GetExitErr())
		}
		if got.GetSessionID() != "fake-session" {
			t.Fatalf("session = %q, want fake-session", got.GetSessionID())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake backend did not complete manager run")
	}
	if !pollUntil(time.Second, time.Millisecond, func() bool { return m.RunningCount() == 0 }) {
		t.Fatalf("running count = %d, want 0", m.RunningCount())
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.steers) != 1 || backend.steers[0] != "continue remotely" {
		t.Fatalf("steers = %v", backend.steers)
	}
	if len(backend.answers) != 1 || backend.answers[0].ToolUseID != "tool-1" || !backend.answers[0].Approved {
		t.Fatalf("answers = %+v", backend.answers)
	}
}

func TestExecutionBackendManagerGuardrailStopsOwningBackend(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetGuardrails(Guardrails{MaxSubagentEvents: 1})
	backend := newSinkDrivenFakeBackend()
	m.SetExecutionBackend(backend)

	a, err := m.Run(RunConfig{Mode: "headless", Name: "guardrail", Dir: t.TempDir(), Prompt: "unused"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	backend.mu.Lock()
	start, handle := backend.start, backend.handle
	backend.mu.Unlock()
	start.Sink.EmitExecutionEvent(t.Context(), handle, ExecutionEvent{
		Kind: ExecutionOutput, Provider: providerid.Claude,
		Output: []byte(`{"type":"assistant","parent_tool_use_id":"fork-1","message":{"content":[{"type":"text","text":"still working"}]}}`),
	})

	backend.mu.Lock()
	stops := backend.stops
	backend.mu.Unlock()
	if stops != 1 {
		t.Fatalf("guardrail stops = %d, want 1", stops)
	}
	if !a.WasStopped() {
		t.Fatal("manager guardrail did not mark the canonical agent stopped")
	}
	close(backend.release)
}

func TestExecutionBackendImmediateCompletionDoesNotLeaveControlHandle(t *testing.T) {
	completed := make(chan struct{}, 1)
	m, _ := newTestManager(t, ManagerConfig{OnComplete: func(*Agent) { completed <- struct{}{} }})
	backend := newSinkDrivenFakeBackend()
	close(backend.release)
	m.SetExecutionBackend(backend)

	a, err := m.Run(RunConfig{Mode: "headless", Name: "immediate", Dir: t.TempDir(), Prompt: "unused"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case <-completed:
	case <-time.After(2 * time.Second):
		t.Fatal("immediate backend completion was lost")
	}
	if _, err := m.InspectExecution(t.Context(), a.ID); err == nil {
		t.Fatal("completed execution retained a stale control handle")
	}
	m.mu.RLock()
	indexed := len(m.executionAgents)
	m.mu.RUnlock()
	if indexed != 0 {
		t.Fatalf("completed execution retained %d handle index entries", indexed)
	}
}

func TestCallbackExecutionBackendControlConformance(t *testing.T) {
	runExecutionBackendConformance(t, func() ExecutionBackend { return newCallbackExecutionBackend("test") })
}

// runExecutionBackendConformance is the reusable lifecycle suite for every
// backend implementation, including future daemon transports.
func runExecutionBackendConformance(t *testing.T, factory func() ExecutionBackend) {
	t.Helper()
	backend := factory()
	done := make(chan struct{})
	stopped := false
	steered := ""
	approved := false
	a := &Agent{ID: "agent-1", State: StateRunning}
	handle, err := backend.Start(t.Context(), ExecutionStart{
		Agent: a,
		runExisting: func(context.Context) {
			<-done
		},
		stop:    func() { stopped = true },
		steer:   func(text string) error { steered = text; return nil },
		approve: func(_ string, answer bool) error { approved = answer; return nil },
		inspect: func() ExecutionInspection { return ExecutionInspection{State: "running", Agent: a.View()} },
		recover: func(context.Context, ExecutionEventSink) error { return nil },
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer close(done)
	if err := backend.Steer(t.Context(), handle, "continue"); err != nil || steered != "continue" {
		t.Fatalf("Steer err=%v text=%q", err, steered)
	}
	if err := backend.RespondApproval(t.Context(), handle, "tool", true); err != nil || !approved {
		t.Fatalf("RespondApproval err=%v approved=%v", err, approved)
	}
	inspection, err := backend.Inspect(t.Context(), handle)
	if err != nil || inspection.Handle != handle || inspection.Agent.ID != a.ID {
		t.Fatalf("Inspect = %+v, err=%v", inspection, err)
	}
	if err := backend.Recover(t.Context(), handle, nil); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if err := backend.Stop(t.Context(), handle); err != nil || !stopped {
		t.Fatalf("Stop err=%v stopped=%v", err, stopped)
	}
	if err := backend.Stop(t.Context(), handle); err != nil {
		t.Fatalf("repeated Stop must be idempotent: %v", err)
	}
	if err := backend.Recover(t.Context(), "unknown-handle", nil); err == nil {
		t.Fatal("Recover accepted an unknown handle")
	}
}

func TestK8sRunnerSelectedThroughExecutionBackendContract(t *testing.T) {
	m, _ := newTestManager(t, ManagerConfig{Runtime: ManagerRuntimeConfig{K8sJobsEnabled: true}})
	m.mu.RLock()
	_, ok := m.executionBackend.(*k8sExecutionBackend)
	m.mu.RUnlock()
	if !ok {
		t.Fatalf("execution backend = %T, want *k8sExecutionBackend", m.executionBackend)
	}
}
