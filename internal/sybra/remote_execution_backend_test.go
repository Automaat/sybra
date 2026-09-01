package sybra

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	agentdaemon "github.com/Automaat/sybra/internal/agentd"
	"github.com/Automaat/sybra/internal/agentworkspace"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/gitexec"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/remotehandback"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/testutil/backendconformance"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
	"github.com/Automaat/sybra/internal/workercontrol"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

type recordingExecutionSink struct {
	mu     sync.Mutex
	events []agent.ExecutionEvent
	ready  chan struct{}
}

type acceptingLocalBackend struct{}

func (acceptingLocalBackend) Start(_ context.Context, _ agent.ExecutionStart) (agent.ExecutionHandle, error) {
	return "local:accepted", nil
}
func (acceptingLocalBackend) Stop(context.Context, agent.ExecutionHandle) error { return nil }
func (acceptingLocalBackend) Steer(context.Context, agent.ExecutionHandle, string) error {
	return nil
}
func (acceptingLocalBackend) RespondApproval(context.Context, agent.ExecutionHandle, string, bool) error {
	return nil
}
func (acceptingLocalBackend) Inspect(context.Context, agent.ExecutionHandle) (agent.ExecutionInspection, error) {
	return agent.ExecutionInspection{}, nil
}
func (acceptingLocalBackend) Recover(context.Context, agent.ExecutionHandle, agent.ExecutionEventSink) error {
	return nil
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
	handback.Spec.Fence.WorkflowGeneration = 3
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

func TestRemotePlanDecisionsOutputAcceptsOwnedRunAfterWorkflowBookkeeping(t *testing.T) {
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	const (
		taskID = "task-plan-bookkeeping"
		runID  = "run-plan-bookkeeping"
	)
	created, err := store.Put(task.Task{ID: taskID, Title: "plan", Status: task.StatusPlanning, Generation: 3,
		Workflow: &workflow.Execution{WorkflowID: "plan-workflow", CurrentStep: "plan", State: workflow.ExecWaiting}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	manager := task.NewManager(store, nil)
	for i := range 3 {
		if _, err := manager.UpdateFnBy(created.ID, "workflow.bookkeeping", func(current task.Task) (task.Update, error) {
			next := current.Workflow.Clone()
			next.SetAgentRoute(runID, "plan")
			next.SetVar(fmt.Sprintf("bookkeeping-%d", i), "recorded")
			return task.Update{Workflow: &next}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	app := &App{tasks: manager}
	entry := executioncontract.ArtifactEntry{Name: "plan-decisions", Kind: "plan_decisions", Root: executioncontract.RootSidecar, Path: "decisions.md"}
	handback := workercontrol.ArtifactHandback{Spec: executioncontract.RunSpec{RunID: runID}, Manifest: executioncontract.ArtifactManifest{ManifestID: "manifest-bookkeeping", Artifacts: []executioncontract.ArtifactEntry{entry}}}
	handback.Spec.Fence = executioncontract.GenerationFence{TaskID: created.ID, TaskGeneration: 3, WorkflowID: "plan-workflow", WorkflowGeneration: 3, StepID: "plan"}
	member := executioncontract.ArtifactMember{Root: entry.Root, Path: entry.Path, Content: []byte("# Decisions\n")}
	if err := app.importRemoteOutputs(handback, []executioncontract.ArtifactMember{member}, false); err != nil {
		t.Fatal(err)
	}
	got, err := manager.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PlanDecisions != "# Decisions\n" {
		t.Fatalf("PlanDecisions = %q", got.PlanDecisions)
	}
	if !slices.Contains(got.Tags, remoteSidecarReceiptTag("plan-workflow", "plan", got.Generation, "plan_decisions")) {
		t.Fatalf("post-import generation receipt missing from tags: generation=%d tags=%v", got.Generation, got.Tags)
	}
	if err := app.importRemoteOutputs(handback, []executioncontract.ArtifactMember{member}, false); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if replayed, err := manager.Get(created.ID); err != nil || replayed.Generation != got.Generation {
		t.Fatalf("replay changed task generation: got=%d want=%d err=%v", replayed.Generation, got.Generation, err)
	}
}

func TestRemotePlanDecisionsOutputRejectsAdvancedUnownedRun(t *testing.T) {
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Put(task.Task{ID: "task-plan-stale", Title: "plan", Status: task.StatusPlanning, Generation: 3,
		Workflow: &workflow.Execution{WorkflowID: "plan-workflow", CurrentStep: "plan", State: workflow.ExecWaiting}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	manager := task.NewManager(store, nil)
	if _, err := manager.UpdateFnBy(created.ID, "workflow.bookkeeping", func(current task.Task) (task.Update, error) {
		next := current.Workflow.Clone()
		next.SetVar("bookkeeping", "recorded")
		return task.Update{Workflow: &next}, nil
	}); err != nil {
		t.Fatal(err)
	}
	app := &App{tasks: manager}
	entry := executioncontract.ArtifactEntry{Name: "plan-decisions", Kind: "plan_decisions", Root: executioncontract.RootSidecar, Path: "decisions.md"}
	handback := workercontrol.ArtifactHandback{Spec: executioncontract.RunSpec{RunID: "superseded-run"}, Manifest: executioncontract.ArtifactManifest{ManifestID: "manifest-stale", Artifacts: []executioncontract.ArtifactEntry{entry}}}
	handback.Spec.Fence = executioncontract.GenerationFence{TaskID: created.ID, TaskGeneration: 3, WorkflowID: "plan-workflow", WorkflowGeneration: 3, StepID: "plan"}
	member := executioncontract.ArtifactMember{Root: entry.Root, Path: entry.Path, Content: []byte("stale")}
	if err := app.importRemoteOutputs(handback, []executioncontract.ArtifactMember{member}, false); !errors.Is(err, remotehandback.ErrStale) {
		t.Fatalf("unowned handback error = %v, want stale", err)
	}
}

func TestRemotePlanDecisionsOutputRejectsRunRoutedToDifferentStep(t *testing.T) {
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Put(task.Task{ID: "task-plan-wrong-route", Title: "plan", Status: task.StatusPlanning, Generation: 3,
		Workflow: &workflow.Execution{WorkflowID: "plan-workflow", CurrentStep: "plan", State: workflow.ExecWaiting}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	manager := task.NewManager(store, nil)
	if _, err := manager.UpdateFnBy(created.ID, "workflow.bookkeeping", func(current task.Task) (task.Update, error) {
		next := current.Workflow.Clone()
		next.SetAgentRoute("stale-run", "implement")
		return task.Update{Workflow: &next}, nil
	}); err != nil {
		t.Fatal(err)
	}
	app := &App{tasks: manager}
	entry := executioncontract.ArtifactEntry{Name: "plan-decisions", Kind: "plan_decisions", Root: executioncontract.RootSidecar, Path: "decisions.md"}
	handback := workercontrol.ArtifactHandback{Spec: executioncontract.RunSpec{RunID: "stale-run"}, Manifest: executioncontract.ArtifactManifest{ManifestID: "manifest-wrong-route", Artifacts: []executioncontract.ArtifactEntry{entry}}}
	handback.Spec.Fence = executioncontract.GenerationFence{TaskID: created.ID, TaskGeneration: 3, WorkflowID: "plan-workflow", WorkflowGeneration: 3, StepID: "plan"}
	member := executioncontract.ArtifactMember{Root: entry.Root, Path: entry.Path, Content: []byte("stale")}
	if err := app.importRemoteOutputs(handback, []executioncontract.ArtifactMember{member}, false); !errors.Is(err, remotehandback.ErrStale) {
		t.Fatalf("different-step handback error = %v, want stale", err)
	}
}

func TestRemoteReceiptExpiresAfterLaterWorkflowMutation(t *testing.T) {
	current := task.Task{Generation: 8, Tags: []string{remoteReceiptTag("manifest-retry", 8)}}
	handback := workercontrol.ArtifactHandback{Manifest: executioncontract.ArtifactManifest{ManifestID: "manifest-retry"}}
	if !remoteReceiptApplied(current, handback) {
		t.Fatal("same-generation recovery receipt was not recognized")
	}
	current.Generation++
	if remoteReceiptApplied(current, handback) {
		t.Fatal("receipt survived a later workflow mutation")
	}
}

func TestRemoteReceiptLegacyTagBackwardCompat(t *testing.T) {
	// A task updated before generation-scoped receipt tags were introduced carries
	// the legacy format "remote-handback:<manifestID>" written at WorkflowGeneration
	// fence time, so the task generation is exactly fence+1 after bookkeeping.
	const fence = int64(7)
	handback := workercontrol.ArtifactHandback{
		Spec:     executioncontract.RunSpec{Fence: executioncontract.GenerationFence{WorkflowGeneration: fence}},
		Manifest: executioncontract.ArtifactManifest{ManifestID: "manifest-legacy"},
	}
	legacyTag := "remote-handback:manifest-legacy"

	// Generation == fence+1: legacy receipt is recognized.
	current := task.Task{Generation: fence + 1, Tags: []string{legacyTag}}
	if !remoteReceiptApplied(current, handback) {
		t.Fatal("legacy receipt tag was not recognized at fence+1 generation")
	}

	// Generation == fence+2 (a later mutation): legacy receipt expires.
	current.Generation = fence + 2
	if remoteReceiptApplied(current, handback) {
		t.Fatal("legacy receipt tag survived a generation beyond fence+1")
	}

	// A new-format tag at the current generation takes precedence and is also recognized.
	current.Generation = fence + 2
	current.Tags = []string{remoteReceiptTag("manifest-legacy", fence+2)}
	if !remoteReceiptApplied(current, handback) {
		t.Fatal("new-format receipt tag was not recognized")
	}
}

func TestLeaderExecutionBackendReclaimsExistingEffectAfterRestart(t *testing.T) {
	dir, base := remoteBackendRepository(t)
	database := dbtest.SQLite(t)
	control := workercontrol.New(database)
	session, err := control.Register(t.Context(), workercontrol.RegisterRequest{
		WorkerID: "daemon-a", Capabilities: syntheticDaemonCapabilities(base),
		Negotiation: executioncontract.Negotiation{ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "worker-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.Register(t.Context(), workercontrol.RegisterRequest{
		WorkerID: "daemon-b", Capabilities: syntheticDaemonCapabilities(base),
		Negotiation: executioncontract.Negotiation{ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "worker-test"},
	}); err != nil {
		t.Fatal(err)
	}
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
	var runCount int
	if err := database.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM remote_runs WHERE effect_id = ?`, intent).Scan(&runCount); err != nil || runCount != 1 {
		t.Fatalf("durable runs for recovered effect = %d, %v; want one", runCount, err)
	}
	remote, err := control.RemoteRunForEffect(t.Context(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := recoveredBackend.Stop(t.Context(), recoveredHandle); err != nil {
		t.Fatalf("Stop after leader upgrade: %v", err)
	}
	if err := recoveredBackend.Stop(t.Context(), recoveredHandle); err != nil {
		t.Fatalf("replayed Stop after leader upgrade: %v", err)
	}
	commands, err := control.PollCommands(t.Context(), session.SessionID, 0, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	stopBuild := ""
	for i := range commands {
		if commands[i].Envelope.Type == executioncontract.CommandStop {
			stopBuild = commands[i].Envelope.BuildVersion
		}
	}
	if stopBuild != remote.Spec.BuildVersion {
		t.Fatalf("recovered Stop build identity = %q, want durable run build %q", stopBuild, remote.Spec.BuildVersion)
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
	got, err := app.tasks.Get(canonical.ID)
	if err != nil || got.Generation != canonical.Generation || got.Workflow.CurrentStep != canonical.Workflow.CurrentStep {
		t.Fatalf("recovery transport duplicated workflow transition: %+v, %v", got, err)
	}
}

func TestRecoveredRemoteRunRequiresMatchingTaskGeneration(t *testing.T) {
	start := agent.ExecutionStart{
		Spec:   agent.ExecutionSpec{TaskID: "task"},
		Config: agent.RunConfig{IntentID: "effect", TaskGeneration: 7},
	}
	spec := executioncontract.RunSpec{EffectID: "effect", Fence: executioncontract.GenerationFence{
		TaskID: "task", TaskGeneration: 6, WorkflowID: "ship", WorkflowGeneration: 7, StepID: "review",
	}}
	if err := validateRecoveredRemoteRun(spec, start, "ship", "review", 7); err == nil {
		t.Fatal("recovered run with stale task generation was accepted")
	}
}

func TestRemoteRelayCancellationDetachesWithoutCompleting(t *testing.T) {
	sink := &recordingExecutionSink{ready: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	backend := &leaderExecutionBackend{
		app:  &App{workerControl: workercontrol.New(dbtest.SQLite(t))},
		runs: make(map[agent.ExecutionHandle]*remoteExecution),
	}
	run := &remoteExecution{runID: "still-running", sink: sink}
	backend.store("remote:still-running", run)
	backend.relay(ctx, "remote:still-running", run, 0)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, event := range sink.events {
		if event.Kind == agent.ExecutionCompleted {
			t.Fatalf("observer cancellation synthesized completion: %+v", event)
		}
	}
	if _, err := backend.load("remote:still-running"); err != nil {
		t.Fatalf("detached run is no longer recoverable: %v", err)
	}
}

func TestRemoteCanceledTerminalPreservesObservationDeadline(t *testing.T) {
	sink := &recordingExecutionSink{ready: make(chan struct{})}
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	backend := &leaderExecutionBackend{}
	run := &remoteExecution{runID: "deadline", sink: sink}
	event := remoteBackendEvent("deadline", 1, executioncontract.EventTerminal, map[string]any{
		"state": executioncontract.TerminalCanceled, "error": context.DeadlineExceeded.Error(), "artifactState": executioncontract.ArtifactsFailed,
	})
	backend.completeAfterHandback(ctx, "remote:deadline", run, event)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.events) != 1 || !errors.Is(sink.events[0].Err, context.DeadlineExceeded) {
		t.Fatalf("completion error = %+v, want deadline exceeded", sink.events)
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
	dir, base := remoteBackendRepository(t)
	database := dbtest.SQLite(t)
	control := workercontrol.New(database)
	session, err := control.Register(t.Context(), workercontrol.RegisterRequest{
		WorkerID: "daemon-a", Capabilities: syntheticDaemonCapabilities(base, "sandbox=enforce"),
		Negotiation: executioncontract.Negotiation{ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "worker-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
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
	backend := &leaderExecutionBackend{
		app: app, runs: make(map[agent.ExecutionHandle]*remoteExecution),
		admitLocal: func() (bool, string) { return false, "leader pressure" },
	}
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

func TestLeaderExecutionBackendDoesNotFallBackLocallyUnderLeaderPressure(t *testing.T) {
	database := dbtest.SQLite(t)
	control := workercontrol.New(database)
	dir, _ := remoteBackendRepository(t)
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	canonical := task.Task{
		ID: "task-pressure", Title: "pressure", ProjectID: "repo", Branch: "main", Status: task.StatusInProgress,
		CreatedAt: now, UpdatedAt: now, Generation: 4,
		Workflow: &workflow.Execution{WorkflowID: "ship", CurrentStep: "implement", State: workflow.ExecWaiting},
	}
	if _, err := store.Put(canonical); err != nil {
		t.Fatal(err)
	}
	app := &App{tasks: task.NewManager(store, nil), workerControl: control}
	backend := &leaderExecutionBackend{
		app: app, runs: make(map[agent.ExecutionHandle]*remoteExecution),
		admitLocal: func() (bool, string) { return false, "disk free 4.7% below minimum 5.0%" },
	}
	start := agent.ExecutionStart{
		Spec: agent.ExecutionSpec{ID: "agent-pressure", TaskID: canonical.ID, Provider: providerid.Claude, Model: "sonnet"},
		Config: agent.RunConfig{
			TaskID: canonical.ID, Role: agent.RoleImplementation, Mode: "headless", Prompt: "work", Dir: dir,
			IntentID: canonical.ID + ":ship:4:1:implement:0", TaskGeneration: 4,
			Provider: providerid.Claude, Model: "sonnet", SandboxMode: "enforce",
		},
		Sink: &recordingExecutionSink{ready: make(chan struct{})},
	}
	if _, err := backend.Start(t.Context(), start); !errors.Is(err, workflow.ErrResourcePressure) {
		t.Fatalf("Start() error = %v, want resource-pressure defer instead of local fallback", err)
	}
	if _, err := control.RemoteRunForEffect(t.Context(), start.Config.IntentID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("RemoteRunForEffect() error = %v, want no reservation after deferred placement", err)
	}
}

func TestLeaderExecutionBackendRechecksRemoteReserveBeforePlacement(t *testing.T) {
	database := dbtest.SQLite(t)
	control := workercontrol.New(database)
	dir, _ := remoteBackendRepository(t)
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	canonical := task.Task{
		ID: "task-remote-reserve", Title: "reserve", ProjectID: "repo", Branch: "main", Status: task.StatusInProgress,
		CreatedAt: now, UpdatedAt: now, Generation: 4,
		Workflow: &workflow.Execution{WorkflowID: "ship", CurrentStep: "implement", State: workflow.ExecWaiting},
	}
	if _, err := store.Put(canonical); err != nil {
		t.Fatal(err)
	}
	backend := &leaderExecutionBackend{
		app:         &App{tasks: task.NewManager(store, nil), workerControl: control},
		runs:        make(map[agent.ExecutionHandle]*remoteExecution),
		admitLocal:  func() (bool, string) { return true, "" },
		admitRemote: func() (bool, string) { return false, "leader disk below remote reserve" },
	}
	start := agent.ExecutionStart{
		Spec: agent.ExecutionSpec{ID: "agent-remote-reserve", TaskID: canonical.ID, Provider: providerid.Claude, Model: "sonnet"},
		Config: agent.RunConfig{
			TaskID: canonical.ID, Role: agent.RoleImplementation, Mode: "headless", Prompt: "work", Dir: dir,
			IntentID: canonical.ID + ":ship:4:1:implement:0", TaskGeneration: 4,
			Provider: providerid.Claude, Model: "sonnet", SandboxMode: "enforce",
		},
		Sink: &recordingExecutionSink{ready: make(chan struct{})},
	}
	if _, err := backend.Start(t.Context(), start); !errors.Is(err, workflow.ErrResourcePressure) {
		t.Fatalf("Start() error = %v, want remote-reserve pressure defer", err)
	}
	if _, err := control.RemoteRunForEffect(t.Context(), start.Config.IntentID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("RemoteRunForEffect() error = %v, want no placement below reserve", err)
	}
}

func TestLeaderExecutionBackendUsesSingleAdmissionBeforeClaimingLocalFallback(t *testing.T) {
	database := dbtest.SQLite(t)
	control := workercontrol.New(database)
	dir, _ := remoteBackendRepository(t)
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	canonical := task.Task{
		ID: "task-local-fallback", Title: "fallback", ProjectID: "repo", Branch: "main", Status: task.StatusInProgress,
		CreatedAt: now, UpdatedAt: now, Generation: 4,
		Workflow: &workflow.Execution{WorkflowID: "ship", CurrentStep: "implement", State: workflow.ExecWaiting},
	}
	if _, err := store.Put(canonical); err != nil {
		t.Fatal(err)
	}
	admissions := 0
	backend := &leaderExecutionBackend{
		app:   &App{tasks: task.NewManager(store, nil), workerControl: control},
		local: acceptingLocalBackend{}, runs: make(map[agent.ExecutionHandle]*remoteExecution),
		admitLocal: func() (bool, string) {
			admissions++
			return admissions == 1, "pressure changed after placement"
		},
	}
	start := agent.ExecutionStart{
		Spec: agent.ExecutionSpec{ID: "agent-local-fallback", TaskID: canonical.ID, Provider: providerid.Claude, Model: "sonnet"},
		Config: agent.RunConfig{
			TaskID: canonical.ID, Role: agent.RoleImplementation, Mode: "headless", Prompt: "work", Dir: dir,
			IntentID: canonical.ID + ":ship:4:1:implement:0", TaskGeneration: 4,
			Provider: providerid.Claude, Model: "sonnet", SandboxMode: "enforce",
		},
		Sink: &recordingExecutionSink{ready: make(chan struct{})},
	}
	if handle, err := backend.Start(t.Context(), start); err != nil || handle != "local:accepted" {
		t.Fatalf("Start() = (%q, %v), want accepted local fallback", handle, err)
	}
	if admissions != 1 {
		t.Fatalf("local admission calls = %d, want one before durable placement", admissions)
	}
}

func TestLeaderManagerCompletesPartitionRecoveryOnceAcrossTwoWorkers(t *testing.T) {
	dir, base := remoteBackendRepository(t)
	database := dbtest.SQLite(t)
	control := workercontrol.New(database)
	for _, worker := range []string{"daemon-a", "daemon-b"} {
		if _, err := control.Register(t.Context(), workercontrol.RegisterRequest{
			WorkerID: worker, Capabilities: syntheticDaemonCapabilities(base, "sandbox=enforce"),
			Negotiation: executioncontract.Negotiation{ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "worker-test"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	canonical := task.Task{ID: "task-manager-partition", Title: "partition", ProjectID: "repo", Branch: "main", Status: task.StatusInProgress,
		CreatedAt: now, UpdatedAt: now, Generation: 5,
		Workflow: &workflow.Execution{WorkflowID: "ship", CurrentStep: "implement", State: workflow.ExecWaiting}}
	if _, err := store.Put(canonical); err != nil {
		t.Fatal(err)
	}
	completed := make(chan *agent.Agent, 2)
	manager := newTestAgentManager(t, t.Context(), func(string, any) {}, discardLogger(), t.TempDir(), agent.ManagerConfig{
		OnComplete: func(a *agent.Agent) { completed <- a },
		TaskGeneration: func(taskID string) (int64, bool) {
			return canonical.Generation, taskID == canonical.ID
		},
	})
	app := &App{tasks: task.NewManager(store, nil), workerControl: control, agents: manager, logger: discardLogger()}
	backend := newLeaderExecutionBackend(app)
	manager.SetExecutionBackend(backend)
	intent := canonical.ID + ":ship:5:1:implement:0"
	running, err := manager.Run(agent.RunConfig{
		Mode: "headless", Name: "remote partition", TaskID: canonical.ID, Dir: dir, Prompt: "work",
		Role: agent.RoleImplementation, IntentID: intent, TaskGeneration: 5, Provider: providerid.Claude, Model: "sonnet", SandboxMode: "enforce",
	})
	if err != nil {
		t.Fatal(err)
	}
	var remote workercontrol.RemoteRun
	for deadline := time.Now().Add(5 * time.Second); ; {
		remote, err = control.RemoteRunForEffect(t.Context(), intent)
		if err == nil {
			break
		}
		if !errors.Is(err, sql.ErrNoRows) || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	// Model a leader observation partition. Recovery reattaches the existing
	// durable effect; it must not schedule onto the second eligible daemon.
	handle := agent.ExecutionHandle("remote:" + remote.Status.RunID)
	observed, err := backend.load(handle)
	if err != nil {
		t.Fatal(err)
	}
	observed.cancel()
	detached := false
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		observed.mu.RLock()
		detached = !observed.observing
		observed.mu.RUnlock()
		if detached {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !detached {
		t.Fatal("leader observer did not detach")
	}
	if err := manager.RecoverExecution(t.Context(), running.ID); err != nil {
		t.Fatal(err)
	}
	events := []executioncontract.EventEnvelope{
		remoteBackendEvent(remote.Status.RunID, 1, executioncontract.EventStarted, map[string]any{"agentId": "worker-agent"}),
		remoteBackendEvent(remote.Status.RunID, 2, executioncontract.EventOutput, map[string]any{"type": "assistant"}),
		remoteBackendEvent(remote.Status.RunID, 3, executioncontract.EventTerminal, map[string]any{"state": executioncontract.TerminalSucceeded, "artifactState": executioncontract.ArtifactsFailed}),
	}
	if _, err := control.AppendEvents(t.Context(), workercontrol.EventBatch{SessionID: remote.Status.SessionID, Events: events}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-completed:
		if got.ID != running.ID {
			t.Fatalf("canonical completion = %+v err=%v", got.View(), got.GetExitErr())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("manager did not complete recovered run")
	}
	if _, err := control.AppendEvents(t.Context(), workercontrol.EventBatch{SessionID: remote.Status.SessionID, Events: []executioncontract.EventEnvelope{events[2]}}); err != nil {
		t.Fatal(err)
	}
	select {
	case duplicate := <-completed:
		t.Fatalf("duplicate canonical completion: %+v", duplicate.View())
	case <-time.After(200 * time.Millisecond):
	}
	var runCount int
	if err := database.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM remote_runs WHERE effect_id = ?`, intent).Scan(&runCount); err != nil || runCount != 1 {
		t.Fatalf("remote run count = %d, %v", runCount, err)
	}
	got, err := app.tasks.Get(canonical.ID)
	if err != nil || got.Generation != canonical.Generation || got.Workflow.CurrentStep != canonical.Workflow.CurrentStep {
		t.Fatalf("backend duplicated workflow transition: %+v, %v", got, err)
	}
}

func TestLeaderTwoDaemonsPartitionCompletesProviderAndWorkflowExactlyOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell provider fixture")
	}
	root := t.TempDir()
	dir, _ := remoteBackendRepository(t)
	// Snapshot the daemon source before creating a leader-only commit. The
	// provider can start only if worker control transfers and the daemon imports
	// the content-addressed base bundle; sharing dir would mask this boundary.
	daemonSource := filepath.Join(root, "daemon-source")
	if err := gitexec.Run(t.Context(), gitexec.Options{}, "clone", "--no-local", "--", dir, daemonSource); err != nil {
		t.Fatal(err)
	}
	if err := gitexec.Run(t.Context(), gitexec.Options{Dir: dir}, "remote", "add", "origin", daemonSource); err != nil {
		t.Fatal(err)
	}
	if err := gitexec.Run(t.Context(), gitexec.Options{Dir: dir}, "fetch", "origin"); err != nil {
		t.Fatal(err)
	}
	if err := gitexec.Run(t.Context(), gitexec.Options{Dir: dir}, "remote", "set-head", "origin", "-a"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "leader-only.txt"), []byte("not pushed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := gitexec.Run(t.Context(), gitexec.Options{Dir: dir}, "add", "leader-only.txt"); err != nil {
		t.Fatal(err)
	}
	if err := gitexec.Run(t.Context(), gitexec.Options{Dir: dir}, "commit", "-m", "leader-only base"); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "provider-runs")
	provider := filepath.Join(bin, providerid.Claude)
	providerScript := `#!/bin/sh
printf 'run\n' >> "$SYBRA_CLUSTER_MARKER"
printf 'remote mutation\n' >> README.md
printf '%s\n' '{"type":"system","session_id":"partition-session"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"durable output"}]}}'
printf '%s\n' '{"type":"result","result":"done","session_id":"partition-session","total_cost_usd":0.01}'
`
	if err := os.WriteFile(provider, []byte(providerScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SYBRA_CLUSTER_MARKER", marker)
	t.Setenv("SYBRA_CLUSTER_TOKEN", "test-token")

	database := dbtest.SQLite(t)
	control := workercontrol.New(database)
	var partitioned atomic.Bool
	baseHandler := control.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if partitioned.Load() && (request.URL.Path == "/worker/v1/events" || request.URL.Path == "/worker/v1/artifacts") {
			http.Error(w, "partitioned", http.StatusServiceUnavailable)
			return
		}
		baseHandler.ServeHTTP(w, request)
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	daemonDone := make(chan error, 2)
	for _, node := range []string{"daemon-a", "daemon-b"} {
		daemon, err := agentdaemon.New(ctx, agentdaemon.Config{
			LeaderURL: server.URL, TokenEnv: "SYBRA_CLUSTER_TOKEN", NodeID: node, Capacity: 1,
			Providers: []string{providerid.Claude}, SandboxMode: "report", LeaseSeconds: 30, PollSeconds: 1,
			TrustedWork: true, EncryptedWork: true,
			WorkspaceRoot: filepath.Join(root, node, "workspaces"), StateRoot: filepath.Join(root, node, "state"),
			SpoolMaxBytes: 1 << 20, Repositories: map[string]string{"repo": daemonSource},
		}, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		go func() { daemonDone <- daemon.Run(ctx) }()
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		diagnostics, err := control.Diagnostics(t.Context())
		if err == nil && len(diagnostics) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("two daemons did not register: %+v, %v", diagnostics, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	store, err := task.NewStore(filepath.Join(root, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	canonical := task.Task{ID: "task-e2e-partition", Title: "partition", ProjectID: "repo", Branch: "main", WorktreeDir: dir, NodeOverride: "daemon-a",
		Status: task.StatusInProgress, CreatedAt: now, UpdatedAt: now, Generation: 7,
		Workflow: &workflow.Execution{WorkflowID: "ship", CurrentStep: "implement", State: workflow.ExecWaiting}}
	if _, err := store.Put(canonical); err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	var transitions atomic.Int32
	completed := make(chan *agent.Agent, 2)
	manager := newTestAgentManager(t, t.Context(), func(string, any) {}, discardLogger(), filepath.Join(root, "logs"), agent.ManagerConfig{
		OnComplete: func(a *agent.Agent) {
			transitions.Add(1)
			_, _ = tasks.UpdateFn(canonical.ID, func(cur task.Task) (task.Update, error) {
				next := *cur.Workflow
				next.CurrentStep = "review"
				nextPtr := &next
				return task.Update{Workflow: &nextPtr}, nil
			})
			completed <- a
		},
		TaskGeneration: func(taskID string) (int64, bool) { return canonical.Generation, taskID == canonical.ID },
	})
	app := &App{
		tasks: tasks, workerControl: control, agents: manager, logger: discardLogger(),
		worktrees: worktree.New(worktree.Config{WorktreesDir: filepath.Join(root, "leader-worktrees"), Tasks: tasks, Logger: discardLogger()}),
	}
	control.SetArtifactImporter(app.importRemoteHandback)
	leaderBackend := newLeaderExecutionBackend(app)
	manager.SetExecutionBackend(leaderBackend)
	partitioned.Store(true)
	intent := canonical.ID + ":ship:7:1:implement:0"
	running, err := manager.Run(agent.RunConfig{Mode: "headless", Name: "e2e partition", TaskID: canonical.ID, Dir: dir, Prompt: "work",
		Role: agent.RoleImplementation, IntentID: intent, TaskGeneration: 7, Provider: providerid.Claude, Model: "sonnet", SandboxMode: "report"})
	if err != nil {
		rows, _ := database.QueryContext(t.Context(), `SELECT worker_id, capabilities_json FROM worker_sessions`)
		var capabilities []string
		if rows != nil {
			defer rows.Close()
			for rows.Next() {
				var worker, encoded string
				_ = rows.Scan(&worker, &encoded)
				capabilities = append(capabilities, worker+":"+encoded)
			}
		}
		t.Fatalf("Run: %v; workers=%v", err, capabilities)
	}
	for deadline := time.Now().Add(10 * time.Second); ; {
		if data, readErr := os.ReadFile(marker); readErr == nil && strings.Count(string(data), "run\n") == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("partitioned provider did not finish exactly once")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Mirror the production workflow's post-dispatch bookkeeping: persisting
	// the route and completing the step effect advance task generation while
	// the immutable remote run still owns this step.
	for i := range 3 {
		if _, err := tasks.UpdateFnBy(canonical.ID, "workflow.bookkeeping", func(current task.Task) (task.Update, error) {
			next := current.Workflow.Clone()
			next.SetAgentRoute(running.ID, "implement")
			next.SetVar(fmt.Sprintf("bookkeeping-%d", i), "recorded")
			return task.Update{Workflow: &next}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	partitioned.Store(false)
	select {
	case got := <-completed:
		if got.ID != running.ID {
			t.Fatalf("completed agent = %s, want %s", got.ID, running.ID)
		}
	case <-time.After(30 * time.Second):
		remote, _ := control.RemoteRunForEffect(t.Context(), intent)
		events, _ := control.ReplayEvents(t.Context(), remote.Status.RunID, 0, 100)
		handle := agent.ExecutionHandle("remote:" + remote.Status.RunID)
		observing := false
		if run, loadErr := leaderBackend.load(handle); loadErr == nil {
			run.mu.RLock()
			observing = run.observing
			run.mu.RUnlock()
		}
		t.Fatalf("durable partition replay did not reach manager completion: status=%+v agent=%+v observing=%v events=%+v", remote.Status, running.View(), observing, events)
	}
	time.Sleep(200 * time.Millisecond)
	if transitions.Load() != 1 {
		t.Fatalf("workflow completion transitions = %d, want one", transitions.Load())
	}
	data, err := os.ReadFile(marker)
	if err != nil || strings.Count(string(data), "run\n") != 1 {
		t.Fatalf("paid provider runs = %q, %v; want one", data, err)
	}
	var runCount int
	if err := database.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM remote_runs WHERE effect_id = ?`, intent).Scan(&runCount); err != nil || runCount != 1 {
		t.Fatalf("durable runs = %d, %v; want one", runCount, err)
	}
	updated, err := tasks.Get(canonical.ID)
	if err != nil || updated.Workflow.CurrentStep != "review" {
		t.Fatalf("canonical workflow = %+v, %v", updated.Workflow, err)
	}
	readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil || !strings.Contains(string(readme), "remote mutation\n") {
		t.Fatalf("production handback did not publish provider mutation: %q, %v", readme, err)
	}
	cancel()
	for range 2 {
		select {
		case <-daemonDone:
		case <-time.After(5 * time.Second):
			t.Fatal("daemon did not stop")
		}
	}
}

func TestDaemonExecutionBackendCommonConformance(t *testing.T) {
	backendconformance.Run(t, daemonCommonConformanceFixture)
}

func daemonCommonConformanceFixture(t *testing.T, emit func(backendconformance.Event)) backendconformance.Fixture {
	t.Helper()
	dir, base := remoteBackendRepository(t)
	database := dbtest.SQLite(t)
	control := workercontrol.New(database)
	if _, err := control.Register(t.Context(), workercontrol.RegisterRequest{
		WorkerID: "daemon-conformance", Capabilities: syntheticDaemonCapabilities(base),
		Negotiation: executioncontract.Negotiation{ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "worker-test"},
	}); err != nil {
		t.Fatal(err)
	}
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	canonical := task.Task{ID: "task-daemon-conformance", Title: "conformance", ProjectID: "repo", Branch: "main", Status: task.StatusInProgress,
		CreatedAt: now, UpdatedAt: now, Generation: 1,
		Workflow: &workflow.Execution{WorkflowID: "ship", CurrentStep: "implement", State: workflow.ExecWaiting}}
	if _, err := store.Put(canonical); err != nil {
		t.Fatal(err)
	}
	app := &App{tasks: task.NewManager(store, nil), workerControl: control, logger: discardLogger()}
	backend := &leaderExecutionBackend{app: app, runs: make(map[agent.ExecutionHandle]*remoteExecution)}
	intent := canonical.ID + ":ship:1:1:implement:0"
	sink := executionSinkFunc(func(_ context.Context, _ agent.ExecutionHandle, event agent.ExecutionEvent) {
		emit(backendconformance.Event{Kind: daemonConformanceEventKind(event.Kind), Err: event.Err})
	})
	start := agent.ExecutionStart{
		Spec: agent.ExecutionSpec{ID: "agent-1", TaskID: canonical.ID, Provider: providerid.Claude, Model: "sonnet"},
		Config: agent.RunConfig{TaskID: canonical.ID, Role: agent.RoleImplementation, Mode: "headless", Prompt: "work", Dir: dir,
			IntentID: intent, TaskGeneration: 1, Provider: providerid.Claude, Model: "sonnet"},
		Sink: sink,
	}
	var releaseOnce, cancelOnce sync.Once
	var controlsMu sync.Mutex
	steered, approved := false, false
	appendTerminal := func(state executioncontract.TerminalState) {
		remote, lookupErr := control.RemoteRunForEffect(t.Context(), intent)
		if lookupErr != nil {
			return
		}
		events := []executioncontract.EventEnvelope{
			remoteBackendEvent(remote.Status.RunID, 1, executioncontract.EventStarted, map[string]any{"agentId": "worker-agent"}),
			remoteBackendEvent(remote.Status.RunID, 2, executioncontract.EventOutput, map[string]any{"type": "assistant"}),
			remoteBackendEvent(remote.Status.RunID, 3, executioncontract.EventTerminal, map[string]any{"state": state, "artifactState": executioncontract.ArtifactsFailed}),
		}
		_, _ = control.AppendEvents(t.Context(), workercontrol.EventBatch{SessionID: remote.Status.SessionID, Events: events})
	}
	return backendconformance.Fixture{
		Start: func() (string, error) {
			handle, startErr := backend.Start(t.Context(), start)
			return string(handle), startErr
		},
		InvalidStart: func() error {
			invalid := start
			invalid.Spec.TaskID = ""
			_, startErr := backend.Start(t.Context(), invalid)
			return startErr
		},
		Stop: func(handle string) error {
			stopErr := backend.Stop(t.Context(), agent.ExecutionHandle(handle))
			if stopErr == nil {
				cancelOnce.Do(func() {
					go func() { time.Sleep(20 * time.Millisecond); appendTerminal(executioncontract.TerminalCanceled) }()
				})
			}
			return stopErr
		},
		Recover: func(handle string, recovered func(backendconformance.Event)) error {
			recoveredSink := executionSinkFunc(func(_ context.Context, _ agent.ExecutionHandle, event agent.ExecutionEvent) {
				recovered(backendconformance.Event{Kind: daemonConformanceEventKind(event.Kind), Err: event.Err})
			})
			return backend.Recover(t.Context(), agent.ExecutionHandle(handle), recoveredSink)
		},
		Inspect: func(handle string) error {
			_, inspectErr := backend.Inspect(t.Context(), agent.ExecutionHandle(handle))
			return inspectErr
		},
		Release: func() { releaseOnce.Do(func() { appendTerminal(executioncontract.TerminalSucceeded) }) },
		Steer: func(handle, text string) error {
			controlErr := backend.Steer(t.Context(), agent.ExecutionHandle(handle), text)
			if controlErr == nil {
				controlsMu.Lock()
				steered = true
				controlsMu.Unlock()
			}
			return controlErr
		},
		Approve: func(handle string, answer bool) error {
			controlErr := backend.RespondApproval(t.Context(), agent.ExecutionHandle(handle), "tool", answer)
			if controlErr == nil {
				controlsMu.Lock()
				approved = true
				controlsMu.Unlock()
			}
			return controlErr
		},
		Steered:  func() bool { controlsMu.Lock(); defer controlsMu.Unlock(); return steered },
		Approved: func() bool { controlsMu.Lock(); defer controlsMu.Unlock(); return approved },
	}
}

type executionSinkFunc func(context.Context, agent.ExecutionHandle, agent.ExecutionEvent)

func (f executionSinkFunc) EmitExecutionEvent(ctx context.Context, handle agent.ExecutionHandle, event agent.ExecutionEvent) {
	f(ctx, handle, event)
}

func daemonConformanceEventKind(kind agent.ExecutionEventKind) string {
	switch kind {
	case agent.ExecutionStarted:
		return "started"
	case agent.ExecutionOutput:
		return "output"
	case agent.ExecutionCompleted:
		return "completed"
	default:
		return string(kind)
	}
}

func TestLeaderExecutionBackendVirtualizesSidecarPathAndFollowsResumedSession(t *testing.T) {
	dir, base := remoteBackendRepository(t)
	database := dbtest.SQLite(t)
	control := workercontrol.New(database)
	session, err := control.Register(t.Context(), workercontrol.RegisterRequest{
		WorkerID: "daemon-resume", Capabilities: syntheticDaemonCapabilities(base),
		Negotiation: executioncontract.Negotiation{ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "worker-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
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
		Capabilities: syntheticDaemonCapabilities(base),
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
		run.mu.RLock()
		observing := run.observing
		run.mu.RUnlock()
		if !observing {
			if _, err := backend.load(handle); err != nil {
				t.Fatalf("detached handle was not retained: %v", err)
			}
			recovered := &recordingExecutionSink{ready: make(chan struct{})}
			if err := backend.Recover(t.Context(), handle, recovered); err != nil {
				t.Fatalf("recover detached relay: %v", err)
			}
			run.cancel()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("remote relay did not stop after leader observation was canceled")
}

func TestRemoteRecoverCanceledContextPreservesObserver(t *testing.T) {
	run := &remoteExecution{runID: "healthy", sink: &recordingExecutionSink{}, observing: true, observerDone: make(chan struct{}), cancel: func() {}}
	backend := &leaderExecutionBackend{runs: map[agent.ExecutionHandle]*remoteExecution{"remote:healthy": run}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := backend.Recover(ctx, "remote:healthy", &recordingExecutionSink{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Recover error = %v, want canceled", err)
	}
	run.mu.RLock()
	defer run.mu.RUnlock()
	if !run.observing {
		t.Fatal("canceled recovery destroyed the healthy observer")
	}
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

func TestPrepareRemoteWorkspaceBaseUsesSharedAncestorForDivergedWorker(t *testing.T) {
	dir, common := remoteBackendRepository(t)
	if err := os.WriteFile(filepath.Join(dir, "leader.txt"), []byte("leader\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := gitexec.Run(t.Context(), gitexec.Options{Dir: dir}, "add", "leader.txt"); err != nil {
		t.Fatal(err)
	}
	if err := gitexec.Run(t.Context(), gitexec.Options{Dir: dir}, "commit", "-m", "leader"); err != nil {
		t.Fatal(err)
	}
	base, err := gitexec.Output(t.Context(), gitexec.Options{Dir: dir}, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := gitexec.Run(t.Context(), gitexec.Options{Dir: dir}, "switch", "-c", "worker", common); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worker.txt"), []byte("worker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := gitexec.Run(t.Context(), gitexec.Options{Dir: dir}, "add", "worker.txt"); err != nil {
		t.Fatal(err)
	}
	if err := gitexec.Run(t.Context(), gitexec.Options{Dir: dir}, "commit", "-m", "worker"); err != nil {
		t.Fatal(err)
	}
	workerAnchor, err := gitexec.Output(t.Context(), gitexec.Options{Dir: dir}, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	workerBundle := filepath.Join(t.TempDir(), "worker.bundle")
	if err := gitexec.Run(t.Context(), gitexec.Options{Dir: dir}, "bundle", "create", workerBundle, "worker"); err != nil {
		t.Fatal(err)
	}
	daemonDir := filepath.Join(t.TempDir(), "daemon")
	if err := gitexec.Run(t.Context(), gitexec.Options{}, "clone", workerBundle, daemonDir); err != nil {
		t.Fatal(err)
	}
	if err := gitexec.Run(t.Context(), gitexec.Options{Dir: daemonDir}, "cat-file", "-e", base+"^{commit}"); err == nil {
		t.Fatal("worker fixture unexpectedly already contains the leader-only base")
	}

	content, ref, err := prepareRemoteWorkspaceBase(t.Context(), dir, "run-diverged", base, workerAnchor, "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if len(content) == 0 || ref == nil {
		t.Fatal("diverged worker did not receive a base bundle")
	}
	incoming := filepath.Join(t.TempDir(), "incoming.bundle")
	if err := os.WriteFile(incoming, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := gitexec.Run(t.Context(), gitexec.Options{Dir: daemonDir}, "bundle", "verify", incoming); err != nil {
		t.Fatalf("thin bundle cannot be applied to advertised worker history: %v", err)
	}
	if err := gitexec.Run(t.Context(), gitexec.Options{Dir: daemonDir}, "fetch", incoming, agentworkspace.BaseBundleRef("run-diverged")); err != nil {
		t.Fatal(err)
	}
	if err := gitexec.Run(t.Context(), gitexec.Options{Dir: daemonDir}, "cat-file", "-e", base+"^{commit}"); err != nil {
		t.Fatalf("leader-only base missing after thin bundle import: %v", err)
	}
}

func syntheticDaemonCapabilities(base string, extra ...string) []string {
	capabilities := []string{
		"capacity=1", "provider=claude", "provider_health:claude=healthy",
		"trusted_work=true", "encrypted_work=true", "workspace_base_bundle=true",
		"repository=repo", "repository_head:repo=" + base,
	}
	return append(capabilities, extra...)
}

func remoteBackendEvent(runID string, sequence uint64, kind executioncontract.EventType, payload any) executioncontract.EventEnvelope {
	body, _ := json.Marshal(payload)
	return executioncontract.EventEnvelope{Version: executioncontract.CurrentVersion(), BuildVersion: "worker-test", RunID: runID,
		Sequence: sequence, EventID: fmt.Sprintf("event:%d", sequence), IdempotencyKey: fmt.Sprintf("%s:%d", runID, sequence),
		Type: kind, ObservedAt: time.Now().UTC(), Payload: body}
}
