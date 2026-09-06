package sybra

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/httpapi"
	"github.com/Automaat/sybra/internal/sybra/completion"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
	"github.com/Automaat/sybra/internal/workercontrol"
)

func recoveryFixture(t *testing.T) *App {
	t.Helper()
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	return &App{tasks: task.NewManager(store, nil), workerControl: workercontrol.New(dbtest.SQLite(t)), logger: discardLogger()}
}

func seedRecoveryTerminal(t *testing.T, app *App, taskID, runID string) executioncontract.EventEnvelope {
	t.Helper()
	now := time.Now().UTC()
	session, err := app.workerControl.Register(t.Context(), workercontrol.RegisterRequest{
		WorkerID:    "worker-" + runID,
		Negotiation: executioncontract.Negotiation{ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "fixture"},
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := executioncontract.RunSpec{
		Version: executioncontract.CurrentVersion(), BuildVersion: "fixture", RunID: runID, EffectID: "effect-" + runID, IdempotencyKey: "spec-" + runID,
		Fence: executioncontract.GenerationFence{TaskID: taskID, TaskGeneration: 1, WorkflowID: "fixture", WorkflowGeneration: 1, StepID: "implement"},
		Role:  string(agent.RoleImplementation), Provider: executioncontract.ProviderIntent{Provider: "test", Model: "fixture"},
		Prompt: executioncontract.Prompt{Text: "private fixture must not enter reports"}, Deadline: now.Add(time.Hour),
		Workspace: executioncontract.Workspace{RepositoryID: "fixture", BaseSHA: strings.Repeat("a", 40), BaseRef: "refs/heads/main", Roots: []executioncontract.LogicalRoot{executioncontract.RootWorktree}},
	}
	payload, err := json.Marshal(executioncontract.StartCommandPayload{Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.workerControl.Enqueue(t.Context(), session.SessionID, &spec, executioncontract.CommandEnvelope{
		Version: spec.Version, BuildVersion: spec.BuildVersion, CommandID: "start-" + runID, RunID: runID, IdempotencyKey: "command-" + runID,
		Type: executioncontract.CommandStart, SentAt: now, Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}
	terminal := executioncontract.EventEnvelope{Version: spec.Version, BuildVersion: spec.BuildVersion, RunID: runID, EventID: "terminal-" + runID,
		IdempotencyKey: "terminal-" + runID, Sequence: 1, Type: executioncontract.EventTerminal, ObservedAt: now,
		Payload: json.RawMessage(`{"state":"failed","artifactState":"failed","error":"private fixture failure"}`)}
	if _, err := app.workerControl.AppendEvents(t.Context(), workercontrol.EventBatch{SessionID: session.SessionID, Events: []executioncontract.EventEnvelope{terminal}}); err != nil {
		t.Fatal(err)
	}
	return terminal
}

func TestRemoteResultRecoveryDryRunPreservesLegacyAndAppliesOnlyProof(t *testing.T) {
	app := recoveryFixture(t)
	tk, err := app.tasks.Create("private fixture title", "private fixture body", "headless")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"proven", "legacy", "mismatch"} {
		terminal := seedRecoveryTerminal(t, app, tk.ID, id)
		receipt := ""
		switch id {
		case "proven":
			receipt = workercontrol.TerminalReceipt(terminal)
		case "mismatch":
			receipt = "v1:wrong"
		}
		if err := app.tasks.AddRun(tk.ID, task.AgentRun{AgentID: id, State: string(agent.StateStopped), Outcome: task.RunOutcomeFailure, CostUSD: 2.5, RemoteCompletionReceipt: receipt}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := app.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		report, err := app.ReconcileRemoteResults(t.Context(), false, "", 100)
		if err != nil || report.Scanned != 3 || report.Eligible != 1 || report.Preserved != 2 || report.Acknowledged != 0 {
			t.Fatalf("dry run: %+v, %v", report, err)
		}
		encoded, _ := json.Marshal(report)
		if strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), tk.ID) {
			t.Fatal("report exposed task-derived content")
		}
	}
	// Replace the App to model recovery with no live agents or relay handles.
	app = &App{tasks: app.tasks, workerControl: app.workerControl}
	report, err := app.ReconcileRemoteResults(t.Context(), true, "", 100)
	if err != nil || report.Acknowledged != 1 || report.Preserved != 2 {
		t.Fatalf("apply: %+v, %v", report, err)
	}
	repeated, err := app.ReconcileRemoteResults(t.Context(), true, "", 100)
	if err != nil || repeated.Acknowledged != 0 || repeated.Scanned != 2 {
		t.Fatalf("repeat apply: %+v, %v", repeated, err)
	}
	after, err := app.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	if !bytes.Equal(beforeJSON, afterJSON) {
		t.Fatal("acknowledgement changed canonical task, workflow, or cost")
	}
	page, err := app.ReconcileRemoteResults(t.Context(), false, "", 1)
	if err != nil || page.Scanned != 1 || page.NextAfter == "" {
		t.Fatalf("first page: %+v, %v", page, err)
	}
	page, err = app.ReconcileRemoteResults(t.Context(), false, page.NextAfter, 1)
	if err != nil || page.Scanned != 1 {
		t.Fatalf("second page: %+v, %v", page, err)
	}
}

type completionCaptureBackend struct {
	acceptingLocalBackend
	started chan agent.ExecutionStart
}

func (b completionCaptureBackend) Start(ctx context.Context, start agent.ExecutionStart) (agent.ExecutionHandle, error) {
	start.Sink.EmitExecutionEvent(ctx, "capture", agent.ExecutionEvent{Kind: agent.ExecutionStarted, BackendOwnsCompletion: true})
	b.started <- start
	return "capture", nil
}

func TestRemoteResultReceiptWaitsForDeferredCanonicalWrite(t *testing.T) {
	app := recoveryFixture(t)
	tk, err := app.tasks.Create("deferred receipt", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	persisted := make(chan struct{}, 1)
	handler := completion.New(completion.Config{Logger: app.logger, Tasks: app.tasks, RunResultPersisted: func(ag *agent.Agent) {
		app.remoteResultPersisted(ag)
		persisted <- struct{}{}
	}})
	manager := newTestAgentManager(t, t.Context(), func(string, any) {}, app.logger, t.TempDir(), agent.ManagerConfig{OnComplete: handler.OnComplete})
	capture := completionCaptureBackend{started: make(chan agent.ExecutionStart, 1)}
	manager.SetExecutionBackend(capture)
	ag, err := manager.Run(agent.RunConfig{Mode: "headless", TaskID: tk.ID, Dir: t.TempDir(), Prompt: "fixture", Role: agent.RoleImplementation})
	if err != nil {
		t.Fatal(err)
	}
	start := <-capture.started
	if err := app.tasks.AddRun(tk.ID, task.AgentRun{AgentID: ag.ID, State: string(agent.StateRunning), Mode: "headless"}); err != nil {
		t.Fatal(err)
	}
	terminal := seedRecoveryTerminal(t, app, tk.ID, ag.ID)
	output, _ := json.Marshal(agent.StreamEvent{Type: "result", Content: "fixture result", CostUSD: 2.5})
	start.Sink.EmitExecutionEvent(t.Context(), "capture", agent.ExecutionEvent{Kind: agent.ExecutionOutput, Output: output, OutputParsed: true})
	unlock, err := fsutil.LockFile(tk.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unlock() }()
	backend := &leaderExecutionBackend{app: app}
	if !backend.completeAfterHandback(t.Context(), "capture", &remoteExecution{sink: start.Sink}, terminal) {
		t.Fatal("terminal was not delivered to the manager")
	}
	if _, err := app.acknowledgeRemoteResult(t.Context(), ag.ID); !errors.Is(err, workercontrol.ErrCompletionUnproven) {
		t.Fatalf("ACK before canonical persistence: %v", err)
	}
	select {
	case <-persisted:
		t.Fatal("persistence callback fired while the task write was blocked")
	default:
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	unlock = func() error { return nil }
	select {
	case <-persisted:
	case <-time.After(5 * time.Second):
		t.Fatal("deferred result did not persist and acknowledge")
	}
	stored, err := app.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	run := stored.AgentRuns[0]
	if run.RemoteCompletionReceipt != workercontrol.TerminalReceipt(terminal) || run.CostUSD != 2.5 || run.Result != "fixture result" {
		t.Fatalf("receipt/result/cost were not persisted together: %+v", run)
	}
	result, err := app.workerControl.ResultForRun(t.Context(), ag.ID)
	if err != nil || result.PendingEvents != 0 {
		t.Fatalf("deferred completion did not ACK: %+v, %v", result, err)
	}
}

func TestRemoteResultRecoveryIsLocalOperatorOnly(t *testing.T) {
	app := recoveryFixture(t)
	service := ServiceRegistry(app)["App"]
	dispatcher := http.NewServeMux()
	httpapi.Mount(dispatcher, map[string]httpapi.Service{"App": service}, app.logger, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/App/ReconcileRemoteResults", strings.NewReader(`[false,"",100]`))
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set(httpapi.SandboxedCallerHeader, "1")
	response := httptest.NewRecorder()
	dispatcher.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("sandboxed caller status = %d", response.Code)
	}
}

func TestRemoteResultObservationCannotMintReceipt(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline waiting for artifact", true: "status lookup failed"}[missing], func(t *testing.T) {
			app := recoveryFixture(t)
			event := seedRecoveryTerminal(t, app, "fixture-task", "observed-run")
			event.Payload = json.RawMessage(`{"state":"succeeded","artifactState":"ready"}`)
			if workercontrol.TerminalReceipt(event) == "" {
				t.Fatal("fixture must have a valid terminal identity")
			}
			runID := event.RunID
			if missing {
				runID = "missing-status"
			}
			sink := &recordingExecutionSink{ready: make(chan struct{})}
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			backend := &leaderExecutionBackend{app: app}
			if !backend.completeAfterHandback(ctx, "observed", &remoteExecution{runID: runID, sink: sink}, event) {
				t.Fatal("expected failed observer completion")
			}
			sink.mu.Lock()
			defer sink.mu.Unlock()
			if len(sink.events) != 1 || sink.events[0].Err == nil || sink.events[0].RemoteCompletionReceipt != "" {
				t.Fatalf("observation failure minted durable proof: %+v", sink.events)
			}
		})
	}
}
