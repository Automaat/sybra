package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/limits"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/testutil/backendconformance"
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

type rejectingExecutionBackend struct{ err error }

func (b rejectingExecutionBackend) Start(context.Context, ExecutionStart) (ExecutionHandle, error) {
	return "", b.err
}
func (rejectingExecutionBackend) Stop(context.Context, ExecutionHandle) error { return nil }
func (rejectingExecutionBackend) Steer(context.Context, ExecutionHandle, string) error {
	return ErrExecutionControlUnsupported
}
func (rejectingExecutionBackend) RespondApproval(context.Context, ExecutionHandle, string, bool) error {
	return ErrExecutionControlUnsupported
}
func (rejectingExecutionBackend) Inspect(context.Context, ExecutionHandle) (ExecutionInspection, error) {
	return ExecutionInspection{}, ErrExecutionControlUnsupported
}
func (rejectingExecutionBackend) Recover(context.Context, ExecutionHandle, ExecutionEventSink) error {
	return ErrExecutionControlUnsupported
}

func newSinkDrivenFakeBackend() *sinkDrivenFakeBackend {
	return &sinkDrivenFakeBackend{release: make(chan struct{})}
}

func (b *sinkDrivenFakeBackend) Start(ctx context.Context, start ExecutionStart) (ExecutionHandle, error) {
	b.mu.Lock()
	b.handle = ExecutionHandle("fake:" + start.Spec.ID)
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
	return ExecutionInspection{Handle: handle, State: "running", Agent: View{ID: b.start.Spec.ID}}, nil
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

func TestExecutionBackendStartFailureLeavesNoRunningAgent(t *testing.T) {
	m, _ := newTestManager(t)
	wantErr := errors.New("kubernetes create rejected")
	m.SetExecutionBackend(rejectingExecutionBackend{err: wantErr})

	a, err := m.Run(RunConfig{
		Mode: "headless", Name: "rejected", TaskID: "task-rejected",
		Dir: t.TempDir(), Prompt: "unused",
	})
	if !errors.Is(err, wantErr) || a != nil {
		t.Fatalf("Run agent=%v err=%v, want nil and %v", a, err, wantErr)
	}
	if m.HasRunningAgentForTask("task-rejected") {
		t.Fatal("failed backend acceptance retained a running task agent")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.agents) != 1 {
		t.Fatalf("agents = %d, want retained terminal snapshot", len(m.agents))
	}
	for _, retained := range m.agents {
		if retained.GetState() != StateStopped || !errors.Is(retained.GetExitErr(), wantErr) {
			t.Fatalf("retained agent state=%s err=%v", retained.GetState(), retained.GetExitErr())
		}
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
	runExecutionBackendConformance(t, newCallbackConformanceFixture)
}

func TestCallbackExecutionBackendCommonConformance(t *testing.T) {
	backendconformance.Run(t, callbackCommonConformanceFixture)
}

func callbackCommonConformanceFixture(t *testing.T, emit func(backendconformance.Event)) backendconformance.Fixture {
	t.Helper()
	adapter := executionEventSinkFunc(func(_ context.Context, _ ExecutionHandle, event ExecutionEvent) {
		emit(backendconformance.Event{Kind: executionEventKind(event.Kind), Err: event.Err})
	})
	fixture := newCallbackConformanceFixture(t, adapter)
	return backendconformance.Fixture{
		Start: func() (string, error) {
			handle, err := fixture.backend.Start(t.Context(), fixture.start)
			return string(handle), err
		},
		InvalidStart: func() error {
			invalid := fixture.start
			invalid.Spec.ID = ""
			_, err := fixture.backend.Start(t.Context(), invalid)
			return err
		},
		Stop: func(handle string) error { return fixture.backend.Stop(t.Context(), ExecutionHandle(handle)) },
		Recover: func(handle string, recovered func(backendconformance.Event)) error {
			sink := executionEventSinkFunc(func(_ context.Context, _ ExecutionHandle, event ExecutionEvent) {
				recovered(backendconformance.Event{Kind: executionEventKind(event.Kind), Err: event.Err})
			})
			return fixture.backend.Recover(t.Context(), ExecutionHandle(handle), sink)
		},
		Inspect: func(handle string) error {
			_, err := fixture.backend.Inspect(t.Context(), ExecutionHandle(handle))
			return err
		},
		Release: fixture.release,
		Steer: func(handle, text string) error {
			return fixture.backend.Steer(t.Context(), ExecutionHandle(handle), text)
		},
		Approve: func(handle string, answer bool) error {
			return fixture.backend.RespondApproval(t.Context(), ExecutionHandle(handle), "tool", answer)
		},
		Steered:  func() bool { return fixture.steered() == "continue" },
		Approved: fixture.approved,
	}
}

type executionEventSinkFunc func(context.Context, ExecutionHandle, ExecutionEvent)

func (f executionEventSinkFunc) EmitExecutionEvent(ctx context.Context, handle ExecutionHandle, event ExecutionEvent) {
	f(ctx, handle, event)
}

func executionEventKind(kind ExecutionEventKind) string {
	switch kind {
	case ExecutionStarted:
		return "started"
	case ExecutionOutput:
		return "output"
	case ExecutionCompleted:
		return "completed"
	default:
		return string(kind)
	}
}

type backendConformanceFixture struct {
	backend  ExecutionBackend
	start    ExecutionStart
	release  func()
	steered  func() string
	approved func() bool
}

type conformanceExecutionSink struct {
	mu     sync.Mutex
	events []ExecutionEvent
}

type forwardingExecutionSink struct {
	*conformanceExecutionSink
	next ExecutionEventSink
}

func (s *forwardingExecutionSink) EmitExecutionEvent(ctx context.Context, handle ExecutionHandle, event ExecutionEvent) {
	s.conformanceExecutionSink.EmitExecutionEvent(ctx, handle, event)
	s.next.EmitExecutionEvent(ctx, handle, event)
}

func (s *conformanceExecutionSink) EmitExecutionEvent(_ context.Context, _ ExecutionHandle, event ExecutionEvent) {
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
}

func (s *conformanceExecutionSink) snapshot() []ExecutionEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ExecutionEvent(nil), s.events...)
}

func newCallbackConformanceFixture(_ *testing.T, sink ExecutionEventSink) backendConformanceFixture {
	done := make(chan struct{})
	var releaseOnce sync.Once
	var stopOnce sync.Once
	var mu sync.Mutex
	steered := ""
	approved := false
	canceled := false
	return backendConformanceFixture{
		backend: newCallbackExecutionBackend("test"),
		start: ExecutionStart{
			Spec: ExecutionSpec{ID: "agent-1"},
			Sink: sink,
			runExisting: func(ctx context.Context, runSink ExecutionEventSink, handle ExecutionHandle) {
				<-done
				mu.Lock()
				wasCanceled := canceled
				mu.Unlock()
				if wasCanceled {
					return
				}
				runSink.EmitExecutionEvent(ctx, handle, ExecutionEvent{Kind: ExecutionOutput, Output: []byte("output")})
				runSink.EmitExecutionEvent(ctx, handle, ExecutionEvent{Kind: ExecutionCompleted})
			},
			stop: func(ctx context.Context) error {
				stopOnce.Do(func() {
					mu.Lock()
					canceled = true
					mu.Unlock()
					sink.EmitExecutionEvent(ctx, "test:agent-1", ExecutionEvent{Kind: ExecutionCompleted, Err: context.Canceled})
					releaseOnce.Do(func() { close(done) })
				})
				return nil
			},
			steer: func(text string) error {
				mu.Lock()
				steered = text
				mu.Unlock()
				return nil
			},
			approve: func(_ string, answer bool) error {
				mu.Lock()
				approved = answer
				mu.Unlock()
				return nil
			},
			inspect: func() ExecutionInspection { return ExecutionInspection{State: "running", Agent: View{ID: "agent-1"}} },
		},
		release: func() { releaseOnce.Do(func() { close(done) }) },
		steered: func() string {
			mu.Lock()
			defer mu.Unlock()
			return steered
		},
		approved: func() bool {
			mu.Lock()
			defer mu.Unlock()
			return approved
		},
	}
}

// runExecutionBackendConformance is backend-generic: a daemon factory can
// provide the same observable controls without using callback adapter fields.
func runExecutionBackendConformance(t *testing.T, factory func(*testing.T, ExecutionEventSink) backendConformanceFixture) {
	t.Helper()
	t.Run("ordered completion and controls", func(t *testing.T) {
		initial := &conformanceExecutionSink{}
		fixture := factory(t, initial)
		handle, err := fixture.backend.Start(t.Context(), fixture.start)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := fixture.backend.Steer(t.Context(), handle, "continue"); err != nil || fixture.steered() != "continue" {
			t.Fatalf("Steer err=%v text=%q", err, fixture.steered())
		}
		if err := fixture.backend.RespondApproval(t.Context(), handle, "tool", true); err != nil || !fixture.approved() {
			t.Fatalf("RespondApproval err=%v approved=%v", err, fixture.approved())
		}
		inspection, err := fixture.backend.Inspect(t.Context(), handle)
		if err != nil || inspection.Handle != handle || inspection.Agent.ID != "agent-1" {
			t.Fatalf("Inspect = %+v, err=%v", inspection, err)
		}
		recovered := &conformanceExecutionSink{}
		if err := fixture.backend.Recover(t.Context(), handle, recovered); err != nil {
			t.Fatalf("Recover: %v", err)
		}
		fixture.release()
		if !pollUntil(time.Second, time.Millisecond, func() bool {
			events := recovered.snapshot()
			return len(events) >= 2 && events[len(events)-1].Kind == ExecutionCompleted
		}) {
			t.Fatalf("recovered events = %+v", recovered.snapshot())
		}
		initialEvents := initial.snapshot()
		if len(initialEvents) != 1 || initialEvents[0].Kind != ExecutionStarted {
			t.Fatalf("initial events = %+v, want only Started before recovery", initialEvents)
		}
		recoveredEvents := recovered.snapshot()
		if len(recoveredEvents) != 2 || recoveredEvents[0].Kind != ExecutionOutput || recoveredEvents[1].Kind != ExecutionCompleted {
			t.Fatalf("recovered events = %+v, want Output then exactly one Completed", recoveredEvents)
		}
		if err := fixture.backend.Recover(t.Context(), "unknown-handle", recovered); err == nil {
			t.Fatal("Recover accepted an unknown handle")
		}
	})

	t.Run("start error emits nothing", func(t *testing.T) {
		sink := &conformanceExecutionSink{}
		fixture := factory(t, sink)
		fixture.start.Spec.ID = ""
		if _, err := fixture.backend.Start(t.Context(), fixture.start); err == nil {
			t.Fatal("Start accepted an invalid request")
		}
		if events := sink.snapshot(); len(events) != 0 {
			t.Fatalf("events after Start error = %+v", events)
		}
		fixture.release()
	})

	t.Run("stop is idempotent and cancels", func(t *testing.T) {
		sink := &conformanceExecutionSink{}
		fixture := factory(t, sink)
		handle, err := fixture.backend.Start(t.Context(), fixture.start)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := fixture.backend.Stop(t.Context(), handle); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if err := fixture.backend.Stop(t.Context(), handle); err != nil {
			t.Fatalf("repeated Stop: %v", err)
		}
		if !pollUntil(time.Second, time.Millisecond, func() bool {
			for _, event := range sink.snapshot() {
				if event.Kind == ExecutionCompleted && errors.Is(event.Err, context.Canceled) {
					return true
				}
			}
			return false
		}) {
			t.Fatalf("stop events = %+v", sink.snapshot())
		}
		completed := 0
		for _, event := range sink.snapshot() {
			if event.Kind == ExecutionCompleted {
				completed++
			}
		}
		if completed != 1 {
			t.Fatalf("Completed count = %d, want 1", completed)
		}
	})
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

func TestLocalExecutionBackendRecoveryReceivesProviderEvents(t *testing.T) {
	binDir := t.TempDir()
	controlDir := t.TempDir()
	ready := filepath.Join(controlDir, "ready")
	release := filepath.Join(controlDir, "release")
	fakeClaude := filepath.Join(binDir, providerid.Claude)
	script := `#!/bin/sh
cat >/dev/null
touch "$SYBRA_TEST_READY"
while [ ! -f "$SYBRA_TEST_RELEASE" ]; do sleep 0.01; done
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"recovered output"}]}}'
printf '%s\n' '{"type":"result","subtype":"success","result":"done","session_id":"recovered-session","total_cost_usd":0}'
`
	if err := os.WriteFile(fakeClaude, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	completed := make(chan *Agent, 1)
	m, _ := newTestManager(t, ManagerConfig{
		OnComplete: func(a *Agent) { completed <- a },
		Runtime:    ManagerRuntimeConfig{DefaultProvider: providerid.Claude},
	})
	a, err := m.Run(RunConfig{
		Mode: "headless", Name: "local recovery", Dir: t.TempDir(), Prompt: "test",
		HeadlessSteerable: false,
		ExtraEnv:          []string{"SYBRA_TEST_READY=" + ready, "SYBRA_TEST_RELEASE=" + release},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !pollUntil(5*time.Second, time.Millisecond, func() bool {
		_, statErr := os.Stat(ready)
		return statErr == nil
	}) {
		t.Fatal("local provider did not become ready")
	}
	execution, err := m.executionForAgent(a.ID)
	if err != nil {
		t.Fatalf("executionForAgent: %v", err)
	}
	recovered := &forwardingExecutionSink{conformanceExecutionSink: &conformanceExecutionSink{}, next: execution.sink}
	if err := execution.backend.Recover(t.Context(), execution.handle, recovered); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if err := os.WriteFile(release, []byte("go"), 0o600); err != nil {
		t.Fatalf("release provider: %v", err)
	}
	select {
	case got := <-completed:
		if got.GetSessionID() != "recovered-session" || got.GetExitErr() != nil {
			t.Fatalf("completed agent session=%q err=%v", got.GetSessionID(), got.GetExitErr())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("local execution did not complete through recovered sink")
	}
	events := recovered.snapshot()
	if len(events) != 3 || events[0].Kind != ExecutionOutput || events[1].Kind != ExecutionOutput || events[2].Kind != ExecutionCompleted {
		t.Fatalf("recovered events = %+v, want assistant, result, Completed", events)
	}
}

func TestEmitExecutionEvent_BanksCostFromAPreParsedRemoteResult(t *testing.T) {
	// Given the terminal result of a run placed on another machine, which the
	// daemon forwards as this package's own StreamEvent
	m, _ := newTestManager(t)
	a := &Agent{ID: "a1", Provider: providerid.Codex}
	forwarded, err := json.Marshal(StreamEvent{
		Type: "result", Subtype: "success", SessionID: "remote-session",
		CostUSD: 0.42, InputTokens: 1200, OutputTokens: 340,
	})
	if err != nil {
		t.Fatal(err)
	}
	lastEmit := time.Time{}

	// When the leader receives it
	m.emitExecutionEvent(t.Context(), ExecutionHandle("h1"), ExecutionEvent{
		Kind: ExecutionOutput, Provider: providerid.Codex,
		Output: forwarded, OutputParsed: true,
	}, a, 0, &lastEmit)

	// Then the run's spend lands on the agent, so the board and the cumulative
	// task budget both see it
	if got := a.GetCostUSD(); got != 0.42 {
		t.Fatalf("CostUSD = %v, want 0.42", got)
	}
	if in, out := a.GetInputTokens(), a.GetOutputTokens(); in != 1200 || out != 340 {
		t.Fatalf("tokens = %d/%d, want 1200/340", in, out)
	}
	if a.GetSessionID() != "remote-session" {
		t.Fatalf("session = %q, want remote-session", a.GetSessionID())
	}
}

func TestEmitExecutionEvent_RemoteOutputLeavesThisHostAlone(t *testing.T) {
	// Given a follower's assistant event large enough to breach this host's
	// live cost ceiling, carrying that host's provider quota
	var snapshots int
	m, _ := newTestManager(t, ManagerConfig{
		LimitSink: func(limits.Snapshot) { snapshots++ },
	})
	m.SetGuardrails(Guardrails{MaxCostUSD: 0.01})
	a := &Agent{ID: "a1", Provider: providerid.Claude}
	forwarded, err := json.Marshal(StreamEvent{
		Type: "assistant", Content: "work", InputTokens: 2_000_000, OutputTokens: 2_000_000,
		LimitSnapshot: &limits.Snapshot{Provider: providerid.Codex, RateLimitReachedType: "weekly"},
	})
	if err != nil {
		t.Fatal(err)
	}
	lastEmit := time.Time{}

	// When the leader applies it
	m.emitExecutionEvent(t.Context(), ExecutionHandle("h1"), ExecutionEvent{
		Kind: ExecutionOutput, Provider: providerid.Claude,
		Output: forwarded, OutputParsed: true,
	}, a, 0, &lastEmit)

	// Then it does not stop a process it cannot reach, and does not record
	// another host's quota against this host's dispatch gate
	if a.WasStopped() {
		t.Fatal("marked a remote run stopped while its process keeps spending")
	}
	if got := a.GetEscalationReason(); got != "" {
		t.Fatalf("escalation = %q, want none", got)
	}
	if snapshots != 0 {
		t.Fatalf("recorded %d follower quota snapshots against this host", snapshots)
	}
}
