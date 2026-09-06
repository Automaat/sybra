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
	ExecutionApproval  ExecutionEventKind = "approval"
	ExecutionCompleted ExecutionEventKind = "completed"
)

// ExecutionEvent is a backend observation. Output is one provider stream line
// without its trailing newline. Completed terminates the run; Err distinguishes
// failure/cancellation from a clean exit. Backends must emit events in order
// and exactly one Completed event for sink-driven runs.
type ExecutionEvent struct {
	Kind                  ExecutionEventKind
	Provider              string
	Output                []byte
	Err                   error
	Command               string
	BackendOwnsCompletion bool
	PermanentFailure      bool
	// AdmissionDeferred means the worker refused the run before spawning a
	// provider. It is infrastructure backpressure, not a coding attempt.
	AdmissionDeferred bool
	Approval          *ApprovalRequest
	// RemoteCompletionReceipt binds a consumed terminal event to the run's
	// durable result. Local execution and observer-only timeouts leave it empty.
	RemoteCompletionReceipt string
	// OutputParsed marks Output as this package's own StreamEvent rather than
	// the provider's wire format. A backend that runs the agent on another
	// machine forwards already-parsed events, and re-parsing them as provider
	// output drops the terminal result's cost, tokens and session.
	OutputParsed bool
}

// ExecutionEventSink receives process observations. It deliberately exposes
// no task store or workflow engine, so a backend cannot complete workflow
// effects directly. Emit may be called from backend goroutines and must return
// before a backend emits the next event for the same handle.
type ExecutionEventSink interface {
	EmitExecutionEvent(context.Context, ExecutionHandle, ExecutionEvent)
}

// ExecutionSpec is the immutable process identity handed to a backend. It is
// deliberately separate from Agent: canonical agent state belongs to Manager
// and can only be changed in response to events delivered through Sink.
type ExecutionSpec struct {
	ID              string
	TaskID          string
	Mode            string
	Provider        string
	Model           string
	ReasoningEffort string
}

func executionSpecFromAgent(a *Agent) ExecutionSpec {
	return ExecutionSpec{
		ID: a.ID, TaskID: a.TaskID, Mode: a.Mode, Provider: a.Provider,
		Model: a.Model, ReasoningEffort: a.ReasoningEffort,
	}
}

// ExecutionStart describes one already-admitted run. Provider choice, budget
// checks, task generation fencing, and capacity admission have all happened
// before Start is called. Spec and Config are immutable execution inputs, not
// authority to mutate canonical agent, task, or workflow state.
//
// runExisting is the compatibility bridge for the current local and Kubernetes
// implementations while their provider-specific loops remain in this package.
// A portable/fake backend ignores it and reports all observations through Sink.
type ExecutionStart struct {
	Spec     ExecutionSpec
	Config   RunConfig
	Provider Provider
	Sink     ExecutionEventSink

	runExisting  func(context.Context, ExecutionEventSink, ExecutionHandle)
	accept       func(context.Context) error
	stop         func(context.Context) error
	steer        func(string) error
	approve      func(string, bool) error
	inspect      func() ExecutionInspection
	startCommand string
	// Provider result output is observational for portable backends. Their
	// explicit Completed event remains the only terminal authority.
	backendOwnsCompletion bool
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
	stop    func(context.Context) error
	steer   func(string) error
	approve func(string, bool) error
	inspect func() ExecutionInspection
	sink    *executionSinkRelay
}

// executionSinkRelay lets recovery atomically attach a new observer without
// teaching a backend anything about Manager or canonical state.
type executionSinkRelay struct {
	mu   sync.RWMutex
	sink ExecutionEventSink
}

func (r *executionSinkRelay) EmitExecutionEvent(ctx context.Context, handle ExecutionHandle, event ExecutionEvent) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.sink != nil {
		r.sink.EmitExecutionEvent(ctx, handle, event)
	}
}

func (r *executionSinkRelay) attach(sink ExecutionEventSink) {
	r.mu.Lock()
	r.sink = sink
	r.mu.Unlock()
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
	if start.Spec.ID == "" || start.runExisting == nil || start.Sink == nil {
		return "", errors.New("execution backend: incomplete start request")
	}
	handle := ExecutionHandle(b.name + ":" + start.Spec.ID)
	relay := &executionSinkRelay{sink: start.Sink}
	start.Sink = relay
	b.mu.Lock()
	if _, exists := b.runs[handle]; exists {
		b.mu.Unlock()
		return "", fmt.Errorf("execution backend: handle %q already exists", handle)
	}
	b.runs[handle] = executionControl{
		stop: start.stop, steer: start.steer, approve: start.approve,
		inspect: start.inspect, sink: relay,
	}
	b.mu.Unlock()
	if start.accept != nil {
		if err := start.accept(ctx); err != nil {
			b.mu.Lock()
			delete(b.runs, handle)
			b.mu.Unlock()
			return "", err
		}
	}

	start.Sink.EmitExecutionEvent(ctx, handle, ExecutionEvent{
		Kind: ExecutionStarted, Command: start.startCommand, BackendOwnsCompletion: start.backendOwnsCompletion,
	})
	go func() {
		start.runExisting(ctx, start.Sink, handle)
		b.mu.Lock()
		delete(b.runs, handle)
		b.mu.Unlock()
	}()
	return handle, nil
}

func (b *callbackExecutionBackend) control(handle ExecutionHandle) (executionControl, error) {
	control, ok := b.lookup(handle)
	if !ok {
		return executionControl{}, fmt.Errorf("execution backend: handle %q not found", handle)
	}
	return control, nil
}

func (b *callbackExecutionBackend) lookup(handle ExecutionHandle) (executionControl, bool) {
	b.mu.RLock()
	control, ok := b.runs[handle]
	b.mu.RUnlock()
	return control, ok
}

func (b *callbackExecutionBackend) Stop(ctx context.Context, handle ExecutionHandle) error {
	control, ok := b.lookup(handle)
	if !ok {
		return nil
	}
	if control.stop == nil {
		return ErrExecutionControlUnsupported
	}
	return control.stop(ctx)
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

func (b *callbackExecutionBackend) Recover(_ context.Context, handle ExecutionHandle, sink ExecutionEventSink) error {
	control, err := b.control(handle)
	if err != nil {
		return err
	}
	if sink == nil {
		return errors.New("execution backend: recovery sink is required")
	}
	control.sink.attach(sink)
	return nil
}
