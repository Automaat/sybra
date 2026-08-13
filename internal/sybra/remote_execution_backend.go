package sybra

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/gitexec"
	"github.com/Automaat/sybra/internal/version"
	"github.com/Automaat/sybra/internal/workercontrol"
	"github.com/google/uuid"
)

type remoteExecution struct {
	mu               sync.RWMutex
	recoverMu        sync.Mutex
	runID, sessionID string
	sink             agent.ExecutionEventSink
	cancel           context.CancelFunc
	localHandle      agent.ExecutionHandle
	deadline         time.Time
	after            uint64
	observing        bool
	observerDone     chan struct{}
}

const remoteTerminalGrace = time.Minute

func (r *remoteExecution) emit(ctx context.Context, handle agent.ExecutionHandle, event agent.ExecutionEvent) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	r.sink.EmitExecutionEvent(ctx, handle, event)
}

// leaderExecutionBackend is the only adapter from durable daemon delivery to
// canonical Agent lifecycle. The worker never receives a task store or
// workflow callback; all output and one terminal fate flow back through the
// manager-owned sink used by local execution.
type leaderExecutionBackend struct {
	app   *App
	local agent.ExecutionBackend

	mu   sync.RWMutex
	runs map[agent.ExecutionHandle]*remoteExecution
}

func newLeaderExecutionBackend(app *App) *leaderExecutionBackend {
	return &leaderExecutionBackend{app: app, local: app.agents.LocalExecutionBackend(), runs: make(map[agent.ExecutionHandle]*remoteExecution)}
}

func (b *leaderExecutionBackend) Start(ctx context.Context, start agent.ExecutionStart) (agent.ExecutionHandle, error) {
	t, err := b.app.tasks.Get(start.Spec.TaskID)
	if err != nil {
		return "", err
	}
	if t.ProjectID == "" || t.NodeOverride == "local" {
		return b.startLocal(ctx, start)
	}
	if t.Workflow == nil || start.Config.IntentID == "" {
		return "", errors.New("remote execution requires a claimed workflow effect")
	}
	if recovered, recoverErr := b.app.workerControl.RemoteRunForEffect(ctx, start.Config.IntentID); recoverErr == nil {
		if err := validateRecoveredRemoteRun(recovered.Spec, start, t.Workflow.WorkflowID, t.Workflow.CurrentStep, t.Generation); err != nil {
			return "", err
		}
		return b.startRemoteRelay(ctx, start, recovered.Status.RunID, recovered.Status.SessionID, recovered.Spec.Deadline, "remote:recovered")
	} else if !errors.Is(recoverErr, sql.ErrNoRows) {
		return "", recoverErr
	}
	request, err := b.placementRequest(ctx, start)
	if err != nil {
		return "", err
	}
	placed, err := b.app.workerControl.ScheduleStart(ctx, request)
	if err != nil {
		return "", err
	}
	if placed.LocalFallback {
		if b.local == nil {
			return "", fmt.Errorf("remote scheduler selected local fallback: %+v", placed.Candidates)
		}
		return b.startLocal(ctx, start)
	}
	return b.startRemoteRelay(ctx, start, request.Spec.RunID, placed.SessionID, request.Spec.Deadline, "remote:"+placed.WorkerID)
}

func (b *leaderExecutionBackend) startRemoteRelay(ctx context.Context, start agent.ExecutionStart, runID, sessionID string, deadline time.Time, command string) (agent.ExecutionHandle, error) {
	handle := agent.ExecutionHandle("remote:" + runID)
	runCtx, cancel := context.WithDeadline(ctx, deadline.Add(remoteTerminalGrace))
	run := &remoteExecution{runID: runID, sessionID: sessionID, sink: start.Sink, cancel: cancel, deadline: deadline, observing: true, observerDone: make(chan struct{})}
	b.store(handle, run)
	start.Sink.EmitExecutionEvent(ctx, handle, agent.ExecutionEvent{Kind: agent.ExecutionStarted, Command: command})
	go b.relay(runCtx, handle, run, 0)
	return handle, nil
}

func validateRecoveredRemoteRun(spec executioncontract.RunSpec, start agent.ExecutionStart, workflowID, stepID string, generation int64) error {
	if generation < 0 || spec.EffectID != start.Config.IntentID || spec.Fence.TaskID != start.Spec.TaskID ||
		spec.Fence.TaskGeneration != uint64(generation) ||
		spec.Fence.WorkflowID != workflowID || spec.Fence.StepID != stepID ||
		spec.Fence.WorkflowGeneration != generation {
		return errors.New("remote execution recovery fence does not match the current workflow claim")
	}
	return nil
}

func (b *leaderExecutionBackend) startLocal(ctx context.Context, start agent.ExecutionStart) (agent.ExecutionHandle, error) {
	handle, err := b.local.Start(ctx, start)
	if err == nil {
		b.store(handle, &remoteExecution{sink: start.Sink, localHandle: handle})
	}
	return handle, err
}

func (b *leaderExecutionBackend) placementRequest(ctx context.Context, start agent.ExecutionStart) (workercontrol.PlacementRequest, error) {
	t, err := b.app.tasks.Get(start.Spec.TaskID)
	if err != nil {
		return workercontrol.PlacementRequest{}, err
	}
	if t.Workflow == nil || start.Config.IntentID == "" {
		return workercontrol.PlacementRequest{}, errors.New("remote execution requires a claimed workflow effect")
	}
	baseSHA, err := gitexec.Output(ctx, gitexec.Options{Dir: start.Config.Dir}, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return workercontrol.PlacementRequest{}, fmt.Errorf("remote execution base: %w", err)
	}
	clean := start
	clean.Config.ExtraEnv = nil
	clean.Config.BeforeStart = nil
	if clean.Config.SidecarDir != "" {
		clean.Config.Prompt = strings.ReplaceAll(clean.Config.Prompt, clean.Config.SidecarDir, agent.RemoteSidecarPathToken)
	}
	baseRef := t.Branch
	if baseRef == "" {
		baseRef, err = gitexec.Output(ctx, gitexec.Options{Dir: start.Config.Dir}, "symbolic-ref", "--short", "HEAD")
		if err != nil {
			return workercontrol.PlacementRequest{}, fmt.Errorf("remote execution base ref: %w", err)
		}
	}
	metadata := agent.RemoteRunMetadata{
		BuildVersion: version.Version, RunID: start.Spec.ID, EffectID: start.Config.IntentID,
		WorkflowID: t.Workflow.WorkflowID, WorkflowGeneration: t.Generation, WorkflowStepID: t.Workflow.CurrentStep,
		Deadline: time.Now().UTC().Add(24 * time.Hour), WorkspaceRepositoryID: t.ProjectID,
		WorkspaceBaseSHA: baseSHA, WorkspaceBaseRef: "refs/heads/" + baseRef,
		WorkspaceRoots:  []executioncontract.LogicalRoot{executioncontract.RootWorktree, executioncontract.RootSidecar, executioncontract.RootArtifact},
		ExpectedOutputs: append([]executioncontract.ExpectedOutput(nil), start.Config.RemoteExpectedOutputs...),
	}
	if clean.Config.SeedWorkingMemory {
		metadata.WorkspaceRoots = append(metadata.WorkspaceRoots, executioncontract.RootWorkingMemory)
	}
	spec, err := agent.RemoteRunSpec(clean, metadata)
	if err != nil {
		return workercontrol.PlacementRequest{}, err
	}
	payload, err := json.Marshal(executioncontract.StartCommandPayload{Spec: &spec})
	if err != nil {
		return workercontrol.PlacementRequest{}, err
	}
	command := executioncontract.CommandEnvelope{
		Version: executioncontract.CurrentVersion(), BuildVersion: version.Version, CommandID: "start-" + start.Spec.ID,
		RunID: spec.RunID, IdempotencyKey: "start:" + spec.IdempotencyKey, Type: executioncontract.CommandStart,
		SentAt: time.Now().UTC(), Payload: payload,
	}
	workType := ""
	if b.app.isWorkProject(t.ProjectID) {
		workType = "work"
	}
	return workercontrol.PlacementRequest{
		Spec: spec, Command: command, NodeOverride: t.NodeOverride, AssignedNode: t.AssignedNode,
		AllowAffinityFallback: true, AllowLocalFallback: true, WorkType: workType,
		RequireTrusted: b.app.isWorkProject(t.ProjectID), RequireEncrypted: b.app.isWorkProject(t.ProjectID),
		Sandbox: start.Config.SandboxMode,
	}, nil
}

func (b *leaderExecutionBackend) relay(ctx context.Context, handle agent.ExecutionHandle, run *remoteExecution, after uint64) {
	completed := false
	defer func() {
		if completed {
			b.remove(handle)
		}
		run.mu.Lock()
		run.observing = false
		if run.observerDone != nil {
			close(run.observerDone)
		}
		run.mu.Unlock()
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		events, err := b.app.workerControl.ReplayEvents(ctx, run.runID, after, 1000)
		if err == nil {
			for i := range events {
				event := events[i]
				after = event.Sequence
				switch event.Type {
				case executioncontract.EventStarted:
					// The manager already received its canonical Started event when
					// the leader durably accepted or recovered this placement.
				case executioncontract.EventOutput:
					run.emit(ctx, handle, agent.ExecutionEvent{Kind: agent.ExecutionOutput, Output: append([]byte(nil), event.Payload...)})
				case executioncontract.EventProgress:
					var progress struct {
						Kind    string                `json:"kind"`
						Request agent.ApprovalRequest `json:"request"`
					}
					if json.Unmarshal(event.Payload, &progress) == nil && progress.Kind == "approval_request" {
						run.emit(ctx, handle, agent.ExecutionEvent{Kind: agent.ExecutionApproval, Approval: &progress.Request})
					}
				case executioncontract.EventTerminal:
					if !b.completeAfterHandback(ctx, handle, run, event) {
						return
					}
					completed = true
					ackCtx := context.WithoutCancel(ctx)
					if sessionID, sessionErr := b.currentSession(ackCtx, run); sessionErr != nil {
						if b.app.logger != nil {
							b.app.logger.Warn("cluster.remote.ack-owner", "run_id", run.runID, "err", sessionErr)
						}
					} else if ackErr := b.app.workerControl.AckEvents(ackCtx, sessionID, run.runID, after); ackErr != nil {
						if b.app.logger != nil {
							b.app.logger.Warn("cluster.remote.ack", "run_id", run.runID, "err", ackErr)
						}
					}
					return
				}
				run.mu.Lock()
				run.after = after
				run.mu.Unlock()
			}
			// Do not acknowledge a prefix before its terminal event has crossed
			// the manager completion path. A leader crash after acknowledging a
			// provider result but before charging its budget must be able to replay
			// that result with the terminal fate on recovery.
		}
		select {
		case <-ctx.Done():
			// Cancellation tears down only this leader-side observation. The
			// durable run remains recoverable and its worker-owned terminal event
			// is still the sole authority for canonical completion.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				run.emit(ctx, handle, agent.ExecutionEvent{Kind: agent.ExecutionCompleted, Err: ctx.Err()})
				completed = true
			}
			return
		case <-ticker.C:
		}
	}
}

func (b *leaderExecutionBackend) completeAfterHandback(ctx context.Context, handle agent.ExecutionHandle, run *remoteExecution, event executioncontract.EventEnvelope) bool {
	var terminal struct {
		State         executioncontract.TerminalState `json:"state"`
		Error         string                          `json:"error"`
		ArtifactState executioncontract.ArtifactState `json:"artifactState"`
	}
	if err := json.Unmarshal(event.Payload, &terminal); err != nil {
		run.emit(ctx, handle, agent.ExecutionEvent{Kind: agent.ExecutionCompleted, Err: err})
		return true
	}
	if terminal.State != executioncontract.TerminalSucceeded && terminal.State != executioncontract.TerminalFailed && terminal.State != executioncontract.TerminalCanceled {
		run.emit(ctx, handle, agent.ExecutionEvent{Kind: agent.ExecutionCompleted, Err: fmt.Errorf("invalid remote terminal state %q", terminal.State)})
		return true
	}
	if terminal.ArtifactState == executioncontract.ArtifactsPending ||
		(terminal.ArtifactState == executioncontract.ArtifactsFailed && terminal.State == executioncontract.TerminalSucceeded) {
		terminal.State, terminal.Error = executioncontract.TerminalFailed, "remote artifact handback did not complete"
	}
	if terminal.ArtifactState == executioncontract.ArtifactsReady {
	artifactWait:
		for {
			status, err := b.app.workerControl.RemoteRunStatus(ctx, run.runID)
			if err != nil || status.ArtifactState == "imported" || status.ArtifactState == "rejected" {
				if err != nil || status.ArtifactState == "rejected" {
					terminal.State, terminal.Error = executioncontract.TerminalFailed, "remote artifact import failed"
				}
				break
			}
			select {
			case <-ctx.Done():
				if errors.Is(ctx.Err(), context.Canceled) {
					return false
				}
				terminal.State, terminal.Error = executioncontract.TerminalCanceled, ctx.Err().Error()
				break artifactWait
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}
	}
	var completionErr error
	switch terminal.State {
	case executioncontract.TerminalSucceeded:
	case executioncontract.TerminalCanceled:
		completionErr = context.Canceled
		if ctx.Err() != nil && terminal.Error == ctx.Err().Error() {
			completionErr = ctx.Err()
		}
	default:
		completionErr = errors.New(firstNonBlank(terminal.Error, "remote execution failed"))
	}
	run.emit(ctx, handle, agent.ExecutionEvent{Kind: agent.ExecutionCompleted, Err: completionErr})
	return true
}

func (b *leaderExecutionBackend) command(ctx context.Context, handle agent.ExecutionHandle, kind executioncontract.CommandType, payload any, key string) error {
	run, err := b.load(handle)
	if err != nil {
		return err
	}
	if run.localHandle != "" {
		return agent.ErrExecutionControlUnsupported
	}
	sessionID, err := b.currentSession(ctx, run)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	envelope := executioncontract.CommandEnvelope{Version: executioncontract.CurrentVersion(), BuildVersion: version.Version,
		CommandID: string(kind) + "-" + uuid.NewString(), RunID: run.runID, IdempotencyKey: key,
		Type: kind, SentAt: time.Now().UTC(), Payload: body}
	_, err = b.app.workerControl.Enqueue(ctx, sessionID, nil, envelope)
	return err
}

func (b *leaderExecutionBackend) currentSession(ctx context.Context, run *remoteExecution) (string, error) {
	status, err := b.app.workerControl.RemoteRunStatus(ctx, run.runID)
	if err != nil {
		return "", err
	}
	run.mu.Lock()
	run.sessionID = status.SessionID
	run.mu.Unlock()
	return status.SessionID, nil
}

func (b *leaderExecutionBackend) Stop(ctx context.Context, handle agent.ExecutionHandle) error {
	run, err := b.load(handle)
	if err != nil {
		return nil //nolint:nilerr // Stop is deliberately idempotent after terminal cleanup.
	}
	if run.localHandle != "" {
		return b.local.Stop(ctx, run.localHandle)
	}
	return b.command(ctx, handle, executioncontract.CommandStop, nil, "stop:"+run.runID)
}

func (b *leaderExecutionBackend) Steer(ctx context.Context, handle agent.ExecutionHandle, text string) error {
	run, err := b.load(handle)
	if err == nil && run.localHandle != "" {
		return b.local.Steer(ctx, run.localHandle, text)
	}
	return b.command(ctx, handle, executioncontract.CommandSteer, map[string]string{"text": text}, "steer:"+uuid.NewString())
}

func (b *leaderExecutionBackend) RespondApproval(ctx context.Context, handle agent.ExecutionHandle, toolUseID string, approved bool) error {
	run, err := b.load(handle)
	if err == nil && run.localHandle != "" {
		return b.local.RespondApproval(ctx, run.localHandle, toolUseID, approved)
	}
	return b.command(ctx, handle, executioncontract.CommandApprovalResponse, map[string]any{"toolUseId": toolUseID, "approved": approved}, "approval:"+toolUseID)
}

func (b *leaderExecutionBackend) Inspect(ctx context.Context, handle agent.ExecutionHandle) (agent.ExecutionInspection, error) {
	run, err := b.load(handle)
	if err != nil {
		return agent.ExecutionInspection{}, err
	}
	if run.localHandle != "" {
		return b.local.Inspect(ctx, run.localHandle)
	}
	status, err := b.app.workerControl.RemoteRunStatus(ctx, run.runID)
	return agent.ExecutionInspection{Handle: handle, State: status.State, Command: "remote:" + status.SessionID}, err
}

func (b *leaderExecutionBackend) Recover(ctx context.Context, handle agent.ExecutionHandle, sink agent.ExecutionEventSink) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	run, err := b.load(handle)
	if err != nil {
		return err
	}
	if run.localHandle != "" {
		return b.local.Recover(ctx, run.localHandle, sink)
	}
	run.recoverMu.Lock()
	defer run.recoverMu.Unlock()
	run.mu.RLock()
	observing, oldCancel, oldDone := run.observing, run.cancel, run.observerDone
	run.mu.RUnlock()
	if observing {
		oldCancel()
		// Once handoff begins it must finish even if the request is canceled;
		// otherwise a failed Recover call would destroy the healthy observer.
		<-oldDone
	}
	if _, err := b.load(handle); err != nil {
		return err
	}
	run.mu.RLock()
	deadline := run.deadline
	run.mu.RUnlock()
	runCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline.Add(remoteTerminalGrace))
	run.mu.Lock()
	run.sink = sink
	run.observing = true
	after := run.after
	run.cancel = cancel
	run.observerDone = make(chan struct{})
	run.mu.Unlock()
	go b.relay(runCtx, handle, run, after)
	return nil
}

func (b *leaderExecutionBackend) store(handle agent.ExecutionHandle, run *remoteExecution) {
	b.mu.Lock()
	b.runs[handle] = run
	b.mu.Unlock()
}
func (b *leaderExecutionBackend) remove(handle agent.ExecutionHandle) {
	b.mu.Lock()
	delete(b.runs, handle)
	b.mu.Unlock()
}
func (b *leaderExecutionBackend) load(handle agent.ExecutionHandle) (*remoteExecution, error) {
	b.mu.RLock()
	run := b.runs[handle]
	b.mu.RUnlock()
	if run == nil {
		return nil, fmt.Errorf("remote execution %q not found", handle)
	}
	return run, nil
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
