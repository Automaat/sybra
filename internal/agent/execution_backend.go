package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrExecutionControlUnsupported reports an execution operation that a
// backend cannot provide. Callers may surface this as a capability conflict;
// they must not silently fall back to controlling a different execution.
var ErrExecutionControlUnsupported = errors.New("execution backend operation unsupported")

// ExecutionHandle is an opaque backend-owned run identity. The manager only
// stores and returns it; it never derives scheduling or workflow decisions
// from its contents.
type ExecutionHandle string

// ExecutionEventKind identifies process-level observations delivered by an
// execution backend. Backends report provider output and termination here;
// the manager remains the only component that translates those observations
// into canonical agent completion and application callbacks.
type ExecutionEventKind string

const (
	ExecutionStarted   ExecutionEventKind = "started"
	ExecutionOutput    ExecutionEventKind = "output"
	ExecutionCompleted ExecutionEventKind = "completed"
)

// ExecutionEvent is a backend observation. Output is one provider stream line
// without its trailing newline. Completed terminates the run; Err distinguishes
// failure/cancellation from a clean exit. Backends must emit events in order
// and exactly one Completed event for sink-driven runs.
type ExecutionEvent struct {
	Kind     ExecutionEventKind
	Provider string
	Output   []byte
	Err      error
	Command  string
}

// ExecutionEventSink receives process observations. It deliberately exposes
// no task store or workflow engine, so a backend cannot complete workflow
// effects directly. Emit may be called from backend goroutines and must return
// before a backend emits the next event for the same handle.
type ExecutionEventSink interface {
	EmitExecutionEvent(context.Context, ExecutionHandle, ExecutionEvent)
}

// ExecutionStart describes one already-admitted run. Provider choice, budget
// checks, task generation fencing, and capacity admission have all happened
// before Start is called. Agent and Config are execution inputs, not authority
// to mutate task or workflow state.
//
// runExisting is the compatibility bridge for the current local and Kubernetes
// implementations while their provider-specific loops remain in this package.
// A portable/fake backend ignores it and reports all observations through Sink.
type ExecutionStart struct {
	Agent    *Agent
	Config   RunConfig
	Provider Provider
	Sink     ExecutionEventSink

	runExisting func(context.Context)
	stop        func()
	steer       func(string) error
	approve     func(string, bool) error
	inspect     func() ExecutionInspection
	recover     func(context.Context, ExecutionEventSink) error
}

// ExecutionInspection is the backend's point-in-time process view. State is
// backend-defined but must be stable enough for recovery to distinguish live,
// terminal, and unknown executions. Agent is a manager snapshot, never a task.
type ExecutionInspection struct {
	Handle  ExecutionHandle
	State   string
	Command string
	Agent   View
}

// ExecutionBackend owns only execution lifecycle and control. Start must
// return after accepting the run; long-running work happens asynchronously.
// A Start error means no execution was accepted and no event may follow.
//
// Stop is idempotent. Cancellation is reported as a Completed event whose Err
// matches the run context when the backend is sink-driven. Recover attaches a
// fresh sink to an existing opaque handle and must never start replacement
// paid work; an unknown handle is an error.
type ExecutionBackend interface {
	Start(context.Context, ExecutionStart) (ExecutionHandle, error)
	Stop(context.Context, ExecutionHandle) error
	Steer(context.Context, ExecutionHandle, string) error
	RespondApproval(context.Context, ExecutionHandle, string, bool) error
	Inspect(context.Context, ExecutionHandle) (ExecutionInspection, error)
	Recover(context.Context, ExecutionHandle, ExecutionEventSink) error
}

type executionControl struct {
	stop    func()
	steer   func(string) error
	approve func(string, bool) error
	inspect func() ExecutionInspection
	recover func(context.Context, ExecutionEventSink) error
}

// callbackExecutionBackend adapts the existing in-process and Kubernetes
// runners to the common lifecycle contract. It contains process controls only;
// manager policy and application state remain above it.
type callbackExecutionBackend struct {
	name string

	mu   sync.RWMutex
	runs map[ExecutionHandle]executionControl
}

func newCallbackExecutionBackend(name string) *callbackExecutionBackend {
	return &callbackExecutionBackend{name: name, runs: make(map[ExecutionHandle]executionControl)}
}

func (b *callbackExecutionBackend) Start(ctx context.Context, start ExecutionStart) (ExecutionHandle, error) {
	if start.Agent == nil || start.runExisting == nil {
		return "", errors.New("execution backend: incomplete start request")
	}
	handle := ExecutionHandle(b.name + ":" + start.Agent.ID)
	b.mu.Lock()
	if _, exists := b.runs[handle]; exists {
		b.mu.Unlock()
		return "", fmt.Errorf("execution backend: handle %q already exists", handle)
	}
	b.runs[handle] = executionControl{
		stop: start.stop, steer: start.steer, approve: start.approve,
		inspect: start.inspect, recover: start.recover,
	}
	b.mu.Unlock()

	if start.Sink != nil {
		start.Sink.EmitExecutionEvent(ctx, handle, ExecutionEvent{Kind: ExecutionStarted})
	}
	go func() {
		start.runExisting(ctx)
		b.mu.Lock()
		delete(b.runs, handle)
		b.mu.Unlock()
	}()
	return handle, nil
}

func (b *callbackExecutionBackend) control(handle ExecutionHandle) (executionControl, error) {
	b.mu.RLock()
	control, ok := b.runs[handle]
	b.mu.RUnlock()
	if !ok {
		return executionControl{}, fmt.Errorf("execution backend: handle %q not found", handle)
	}
	return control, nil
}

func (b *callbackExecutionBackend) Stop(_ context.Context, handle ExecutionHandle) error {
	control, err := b.control(handle)
	if err != nil {
		return err
	}
	if control.stop == nil {
		return ErrExecutionControlUnsupported
	}
	control.stop()
	return nil
}

func (b *callbackExecutionBackend) Steer(_ context.Context, handle ExecutionHandle, text string) error {
	control, err := b.control(handle)
	if err != nil {
		return err
	}
	if control.steer == nil {
		return ErrExecutionControlUnsupported
	}
	return control.steer(text)
}

func (b *callbackExecutionBackend) RespondApproval(_ context.Context, handle ExecutionHandle, toolUseID string, approved bool) error {
	control, err := b.control(handle)
	if err != nil {
		return err
	}
	if control.approve == nil {
		return ErrExecutionControlUnsupported
	}
	return control.approve(toolUseID, approved)
}

func (b *callbackExecutionBackend) Inspect(_ context.Context, handle ExecutionHandle) (ExecutionInspection, error) {
	control, err := b.control(handle)
	if err != nil {
		return ExecutionInspection{}, err
	}
	if control.inspect == nil {
		return ExecutionInspection{}, ErrExecutionControlUnsupported
	}
	inspection := control.inspect()
	inspection.Handle = handle
	return inspection, nil
}

func (b *callbackExecutionBackend) Recover(ctx context.Context, handle ExecutionHandle, sink ExecutionEventSink) error {
	control, err := b.control(handle)
	if err != nil {
		return err
	}
	if control.recover == nil {
		return ErrExecutionControlUnsupported
	}
	return control.recover(ctx, sink)
}
