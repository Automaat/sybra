package sybra

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/gitexec"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
	"github.com/Automaat/sybra/internal/workercontrol"
	"github.com/Automaat/sybra/internal/workflow"
)

type recordingExecutionSink struct {
	mu     sync.Mutex
	events []agent.ExecutionEvent
	ready  chan struct{}
}

func TestLeaderRunPlacementOwnsFollowerHomedCanonicalTask(t *testing.T) {
	app := &App{
		cfg:           &config.Config{Cluster: config.ClusterConfig{Role: config.ClusterRoleLeader, Followers: []config.Follower{{Name: "old-follower", Homes: []string{"repo"}}}}},
		workerControl: workercontrol.New(dbtest.SQLite(t)),
	}
	if !app.runsTaskLocally(task.Task{ProjectID: "repo", AssignedNode: "old-follower"}) {
		t.Fatal("leader rejected its canonical task because of legacy follower-home metadata")
	}
}

func TestRemotePlanDecisionsOutputImportsIntoCanonicalTask(t *testing.T) {
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Put(task.Task{ID: "task-plan-decisions", Title: "plan", Status: task.StatusPlanning, Generation: 3,
		Workflow: &workflow.Execution{WorkflowID: "plan-workflow", CurrentStep: "plan", State: workflow.ExecWaiting}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	app := &App{tasks: task.NewManager(store, nil)}
	entry := executioncontract.ArtifactEntry{Name: "plan-decisions", Kind: "plan_decisions", Root: executioncontract.RootSidecar, Path: "decisions.md"}
	handback := workercontrol.ArtifactHandback{Spec: executioncontract.RunSpec{RunID: "run-plan"}, Manifest: executioncontract.ArtifactManifest{ManifestID: "manifest-plan", Artifacts: []executioncontract.ArtifactEntry{entry}}}
	member := executioncontract.ArtifactMember{Root: entry.Root, Path: entry.Path, Content: []byte("# Decisions\n")}
	handback.Spec.Fence.TaskID = created.ID
	handback.Spec.Fence.TaskGeneration = 3
	handback.Spec.Fence.WorkflowID = "plan-workflow"
	handback.Spec.Fence.StepID = "plan"
	if err := app.importRemoteOutputs(handback, []executioncontract.ArtifactMember{member}, false); err != nil {
		t.Fatal(err)
	}
	got, err := app.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PlanDecisions != "# Decisions\n" {
		t.Fatalf("PlanDecisions = %q", got.PlanDecisions)
	}
}

func TestLeaderExecutionBackendReclaimsExistingEffectAfterRestart(t *testing.T) {
	database := dbtest.SQLite(t)
	control := workercontrol.New(database)
	session, err := control.Register(t.Context(), workercontrol.RegisterRequest{
		WorkerID: "daemon-a", Capabilities: []string{"capacity=1", "provider=claude", "provider_health:claude=healthy", "trusted_work=true", "encrypted_work=true"},
		Negotiation: executioncontract.Negotiation{ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "worker-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := remoteBackendRepository(t)
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	canonical := task.Task{ID: "task-recover", Title: "recover", ProjectID: "repo", Branch: "main", Status: task.StatusInProgress,
		CreatedAt: now, UpdatedAt: now, Generation: 7,
		Workflow: &workflow.Execution{WorkflowID: "ship", CurrentStep: "review", State: workflow.ExecWaiting}}
	if _, err := store.Put(canonical); err != nil {
		t.Fatal(err)
	}
	app := &App{tasks: task.NewManager(store, nil), workerControl: control}
	intent := canonical.ID + ":ship:7:1:review:0"
	start := agent.ExecutionStart{Spec: agent.ExecutionSpec{ID: "agent-before-restart", TaskID: canonical.ID, Provider: providerid.Claude, Model: "sonnet"},
		Config: agent.RunConfig{TaskID: canonical.ID, Role: agent.RoleReview, Mode: "headless", Prompt: "review", Dir: dir,
			IntentID: intent, TaskGeneration: 7, Provider: providerid.Claude, Model: "sonnet"}}
	oldBackend := &leaderExecutionBackend{app: app, runs: make(map[agent.ExecutionHandle]*remoteExecution)}
	oldSink := &recordingExecutionSink{ready: make(chan struct{})}
	start.Sink = oldSink
	oldHandle, err := oldBackend.Start(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	oldRun, err := oldBackend.load(oldHandle)
	if err != nil {
		t.Fatal(err)
	}
	oldRun.cancel() // model the old leader observer disappearing

	recoveredBackend := &leaderExecutionBackend{app: app, runs: make(map[agent.ExecutionHandle]*remoteExecution)}
	recoveredSink := &recordingExecutionSink{ready: make(chan struct{})}
	start.Spec.ID = "agent-after-restart"
	start.Sink = recoveredSink
	recoveredHandle, err := recoveredBackend.Start(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredHandle != oldHandle {
		t.Fatalf("recovered handle = %q, want existing %q", recoveredHandle, oldHandle)
	}
	if _, err := control.RemoteRunStatus(t.Context(), start.Spec.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("replacement remote run exists: %v", err)
	}
	events := []executioncontract.EventEnvelope{
		remoteBackendEvent("agent-before-restart", 1, executioncontract.EventStarted, map[string]any{"agentId": "worker-agent"}),
		remoteBackendEvent("agent-before-restart", 2, executioncontract.EventOutput, map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "replayed"}}}}),
		remoteBackendEvent("agent-before-restart", 3, executioncontract.EventTerminal, map[string]any{"state": executioncontract.TerminalSucceeded, "artifactState": executioncontract.ArtifactsFailed}),
	}
	if _, err := control.AppendEvents(t.Context(), workercontrol.EventBatch{SessionID: session.SessionID, Events: events}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-recoveredSink.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("recovered observer did not receive durable terminal fate")
	}
	recoveredSink.mu.Lock()
	defer recoveredSink.mu.Unlock()
	var outputs, completions int
	for _, event := range recoveredSink.events {
		if event.Kind == agent.ExecutionOutput {
			outputs++
		}
		if event.Kind == agent.ExecutionCompleted {
			completions++
		}
	}
	if outputs != 1 || completions != 1 {
		t.Fatalf("recovered events = output:%d completed:%d (%+v)", outputs, completions, recoveredSink.events)
	}
}

func (s *recordingExecutionSink) EmitExecutionEvent(_ context.Context, _ agent.ExecutionHandle, event agent.ExecutionEvent) {
	s.mu.Lock()
	s.events = append(s.events, event)
	if event.Kind == agent.ExecutionCompleted {
		select {
		case <-s.ready:
		default:
			close(s.ready)
		}
	}
	s.mu.Unlock()
}

func TestLeaderExecutionBackendRelaysOneCanonicalCompletion(t *testing.T) {
	database := dbtest.SQLite(t)
	control := workercontrol.New(database)
	session, err := control.Register(t.Context(), workercontrol.RegisterRequest{
		WorkerID: "daemon-a", Capabilities: []string{"capacity=1", "provider=claude", "provider_health:claude=healthy", "sandbox=enforce", "trusted_work=true", "encrypted_work=true"},
		Negotiation: executioncontract.Negotiation{ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "worker-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir, base := remoteBackendRepository(t)
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	canonical := task.Task{ID: "task-remote", Title: "remote", ProjectID: "repo", Branch: "main", Status: task.StatusInProgress,
		CreatedAt: now, UpdatedAt: now, Generation: 4,
		Workflow: &workflow.Execution{WorkflowID: "ship", CurrentStep: "implement", State: workflow.ExecWaiting}}
	if _, err := store.Put(canonical); err != nil {
		t.Fatal(err)
	}
	app := &App{tasks: task.NewManager(store, nil), workerControl: control}
	backend := &leaderExecutionBackend{app: app, runs: make(map[agent.ExecutionHandle]*remoteExecution)}
	sink := &recordingExecutionSink{ready: make(chan struct{})}
	start := agent.ExecutionStart{Spec: agent.ExecutionSpec{ID: "agent-remote", TaskID: canonical.ID, Provider: providerid.Claude, Model: "sonnet"},
		Config: agent.RunConfig{TaskID: canonical.ID, Role: agent.RoleImplementation, Mode: "headless", Prompt: "work", Dir: dir,
			IntentID: canonical.ID + ":ship:4:1:implement:0", TaskGeneration: 4, Provider: providerid.Claude, Model: "sonnet", SandboxMode: "enforce"}, Sink: sink}
	handle, err := backend.Start(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	status, err := control.RemoteRunStatus(t.Context(), "agent-remote")
	if err != nil || status.SessionID != session.SessionID {
		t.Fatalf("remote status = %+v, %v", status, err)
	}
	events := []executioncontract.EventEnvelope{
		remoteBackendEvent("agent-remote", 1, executioncontract.EventStarted, map[string]any{"agentId": "worker-agent"}),
		remoteBackendEvent("agent-remote", 2, executioncontract.EventOutput, map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "finished"}}}}),
		remoteBackendEvent("agent-remote", 3, executioncontract.EventTerminal, map[string]any{"state": executioncontract.TerminalSucceeded, "artifactState": executioncontract.ArtifactsFailed}),
	}
	if _, err := control.AppendEvents(t.Context(), workercontrol.EventBatch{SessionID: session.SessionID, Events: events}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sink.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("remote completion was not relayed")
	}
	// An exact at-least-once replay is durable transport behavior, not a second
	// canonical completion.
	if _, err := control.AppendEvents(t.Context(), workercontrol.EventBatch{SessionID: session.SessionID, Events: []executioncontract.EventEnvelope{events[2]}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var started, output, completed int
	for _, event := range sink.events {
		switch event.Kind {
		case agent.ExecutionStarted:
			started++
		case agent.ExecutionOutput:
			output++
		case agent.ExecutionApproval:
		case agent.ExecutionCompleted:
			completed++
		}
	}
	if started != 1 || output != 1 || completed != 1 {
		t.Fatalf("canonical events = started:%d output:%d completed:%d (%+v)", started, output, completed, sink.events)
	}
	if got, err := app.tasks.Get(canonical.ID); err != nil || got.Status != canonical.Status || got.Workflow.CurrentStep != canonical.Workflow.CurrentStep {
		t.Fatalf("daemon transport mutated canonical task = %+v, %v", got, err)
	}
	if base == "" || handle == "" {
		t.Fatal("invalid repository or execution identity")
	}
}

func TestLeaderExecutionBackendVirtualizesSidecarPathAndFollowsResumedSession(t *testing.T) {
	database := dbtest.SQLite(t)
	control := workercontrol.New(database)
	session, err := control.Register(t.Context(), workercontrol.RegisterRequest{
		WorkerID: "daemon-resume", Capabilities: []string{"capacity=1", "provider=claude", "provider_health:claude=healthy", "trusted_work=true", "encrypted_work=true"},
		Negotiation: executioncontract.Negotiation{ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "worker-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := remoteBackendRepository(t)
	sidecar := filepath.Join(t.TempDir(), "leader-sidecars")
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	canonical := task.Task{ID: "task-resume", Title: "resume", ProjectID: "repo", Branch: "main", Status: task.StatusInProgress,
		CreatedAt: now, UpdatedAt: now, Generation: 2,
		Workflow: &workflow.Execution{WorkflowID: "ship", CurrentStep: "review", State: workflow.ExecWaiting}}
	if _, err := store.Put(canonical); err != nil {
		t.Fatal(err)
	}
	app := &App{tasks: task.NewManager(store, nil), workerControl: control, logger: discardLogger()}
	backend := &leaderExecutionBackend{app: app, runs: make(map[agent.ExecutionHandle]*remoteExecution)}
	sink := &recordingExecutionSink{ready: make(chan struct{})}
	intent := canonical.ID + ":ship:2:1:review:0"
	handle, err := backend.Start(t.Context(), agent.ExecutionStart{
		Spec: agent.ExecutionSpec{ID: "agent-resume", TaskID: canonical.ID, Provider: providerid.Claude, Model: "sonnet"},
		Config: agent.RunConfig{TaskID: canonical.ID, Role: agent.RoleReview, Mode: "headless", Prompt: "write " + sidecar + "/review.md", Dir: dir,
			SidecarDir: sidecar, IntentID: intent, TaskGeneration: 2, Provider: providerid.Claude, Model: "sonnet"}, Sink: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := control.RemoteRunForEffect(t.Context(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(remote.Spec.Prompt.Text, sidecar) || !strings.Contains(remote.Spec.Prompt.Text, agent.RemoteSidecarPathToken+"/review.md") {
		t.Fatalf("remote prompt leaked leader path: %q", remote.Spec.Prompt.Text)
	}
	replacement, err := control.Register(t.Context(), workercontrol.RegisterRequest{
		WorkerID: "daemon-resume", ResumeSessionID: session.SessionID,
		Capabilities: []string{"capacity=1", "provider=claude", "provider_health:claude=healthy", "trusted_work=true", "encrypted_work=true"},
		Negotiation:  executioncontract.Negotiation{ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "worker-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Steer(t.Context(), handle, "continue"); err != nil {
		t.Fatalf("steer after session replacement: %v", err)
	}
	commands, err := control.PollCommands(t.Context(), replacement.SessionID, 0, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 || commands[1].Envelope.Type != executioncontract.CommandSteer {
		t.Fatalf("replacement-session commands = %+v", commands)
	}
	run, err := backend.load(handle)
	if err != nil {
		t.Fatal(err)
	}
	run.cancel()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		if _, err := backend.load(handle); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("remote relay did not stop after leader observation was canceled")
}

func remoteBackendRepository(t *testing.T) (dir, base string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.invalid"}} {
		if err := gitexec.Run(t.Context(), gitexec.Options{Dir: dir}, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := gitexec.Run(t.Context(), gitexec.Options{Dir: dir}, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if err := gitexec.Run(t.Context(), gitexec.Options{Dir: dir}, "commit", "-m", "base"); err != nil {
		t.Fatal(err)
	}
	base, err := gitexec.Output(t.Context(), gitexec.Options{Dir: dir}, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return dir, base
}

func remoteBackendEvent(runID string, sequence uint64, kind executioncontract.EventType, payload any) executioncontract.EventEnvelope {
	body, _ := json.Marshal(payload)
	return executioncontract.EventEnvelope{Version: executioncontract.CurrentVersion(), BuildVersion: "worker-test", RunID: runID,
		Sequence: sequence, EventID: fmt.Sprintf("event:%d", sequence), IdempotencyKey: fmt.Sprintf("%s:%d", runID, sequence),
		Type: kind, ObservedAt: time.Now().UTC(), Payload: body}
}
