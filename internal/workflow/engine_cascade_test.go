package workflow

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// addStatusWorkflow registers a minimal mechanical workflow: a single
// set_status step that flips the task to setStatus, then ends. With an empty
// condStatus the trigger has no conditions (used for the task.created entry
// workflow); otherwise it fires on task.status_changed when task.status equals
// condStatus.
func addStatusWorkflow(t *testing.T, store *Store, id, on, condStatus, setStatus string) {
	t.Helper()
	trigger := Trigger{On: on}
	if condStatus != "" {
		trigger.Conditions = []Condition{
			{Field: "task.status", Operator: "equals", Value: condStatus},
		}
	}
	def := Definition{
		ID:      id,
		Name:    id,
		Trigger: trigger,
		Steps: []Step{
			{
				ID:     "flip",
				Name:   "Flip",
				Type:   StepSetStatus,
				Config: StepConfig{Status: setStatus},
				Next:   []Transition{{GoTo: ""}},
			},
		},
	}
	if err := store.Save(def); err != nil {
		t.Fatalf("save %s: %v", id, err)
	}
}

// cascadeOnComplete mirrors sybra's OnWorkflowComplete: on every workflow
// completion it re-dispatches a task.status_changed event for the task's
// current status, letting the next workflow in the chain pick up.
func cascadeOnComplete(engine *Engine, tasks *memTasks) func(CompletionInfo) {
	return func(info CompletionInfo) {
		cur, err := tasks.GetTask(info.TaskID)
		if err != nil {
			return
		}
		_, _ = engine.DispatchEvent(info.TaskID, "task.status_changed",
			map[string]string{"task.status": cur.Status}, nil)
	}
}

// TestCascade_SynchronousWorkflowChainsToSuccessor is the regression test for
// the handoff bug: a fully mechanical workflow (set_status only, no async step)
// completes synchronously inside the start/dispatch call, so its completion
// callback used to fire while the per-task start marker was still held and the
// cascade DispatchEvent was rejected as re-entrant — the next workflow never
// started. The fix fires onComplete only after the marker releases.
//
// Chain: "kickoff" (on task.created) → in-progress; "follow" (on
// task.status_changed where status==in-progress) → done.
func TestCascade_SynchronousWorkflowChainsToSuccessor(t *testing.T) {
	t.Run("via DispatchEvent (task.created)", func(t *testing.T) {
		store := newTestStoreWith(t)
		tasks := newMemTasks()
		agents := newMockAgents()
		engine := NewEngine(store, tasks, agents, discardLogger())
		engine.SetOnComplete(cascadeOnComplete(engine, tasks))

		addStatusWorkflow(t, store, "kickoff", "task.created", "", "in-progress")
		addStatusWorkflow(t, store, "follow", "task.status_changed", "in-progress", "done")
		tasks.Put(TaskInfo{ID: "t1", Status: "new", AgentMode: "headless"})

		if _, err := engine.DispatchEvent("t1", "task.created", nil, nil); err != nil {
			t.Fatalf("dispatch task.created: %v", err)
		}

		got, err := tasks.GetTask("t1")
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "done" {
			t.Fatalf("status = %q, want done — cascade to successor workflow did not run", got.Status)
		}
	})

	t.Run("via StartWorkflow (restart-stale path)", func(t *testing.T) {
		store := newTestStoreWith(t)
		tasks := newMemTasks()
		agents := newMockAgents()
		engine := NewEngine(store, tasks, agents, discardLogger())
		engine.SetOnComplete(cascadeOnComplete(engine, tasks))

		addStatusWorkflow(t, store, "kickoff", "task.created", "", "in-progress")
		addStatusWorkflow(t, store, "follow", "task.status_changed", "in-progress", "done")
		// restart-stale restarts a task's own terminal mechanical workflow by ID.
		tasks.Put(TaskInfo{
			ID:        "t2",
			Status:    "in-progress",
			AgentMode: "headless",
			Workflow:  &Execution{WorkflowID: "kickoff", State: ExecCompleted},
		})

		if err := engine.StartWorkflow("t2", "kickoff"); err != nil {
			t.Fatalf("start workflow: %v", err)
		}

		got, err := tasks.GetTask("t2")
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "done" {
			t.Fatalf("status = %q, want done — cascade to successor workflow did not run", got.Status)
		}
	})
}

// TestCascade_ParallelTerminalStepCascades guards the mirror bug the adversary
// surfaced: a workflow whose TERMINAL step is a `parallel` block can also
// complete synchronously — when every child fails at spawn, execParallel calls
// finalizeParallelParent inline, inside the dispatch markers. The completion
// must still thread up and cascade after the markers release, not be dropped.
func TestCascade_ParallelTerminalStepCascades(t *testing.T) {
	store := newTestStoreWith(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.failSpawn = errors.New("spawn boom") // force all parallel children to fail at spawn
	engine := NewEngine(store, tasks, agents, discardLogger())
	engine.SetOnComplete(cascadeOnComplete(engine, tasks))

	// kickoff ends on a parallel block (no set_status), so it leaves the task
	// at "new"; the successor triggers on that.
	par := Definition{
		ID:      "kickoff-par",
		Name:    "kickoff-par",
		Trigger: Trigger{On: "task.created"},
		Steps: []Step{{
			ID:   "fanout",
			Name: "Fanout",
			Type: StepParallel,
			Parallel: []Step{
				{ID: "a", Type: StepRunAgent, Config: StepConfig{Role: "plan", Mode: "headless", Prompt: "x"}},
				{ID: "b", Type: StepRunAgent, Config: StepConfig{Role: "plan", Mode: "headless", Prompt: "x"}},
			},
			Next: []Transition{{GoTo: ""}},
		}},
	}
	if err := store.Save(par); err != nil {
		t.Fatalf("save kickoff-par: %v", err)
	}
	addStatusWorkflow(t, store, "follow", "task.status_changed", "new", "done")
	tasks.Put(TaskInfo{ID: "t1", Status: "new", AgentMode: "headless"})

	if _, err := engine.DispatchEvent("t1", "task.created", nil, nil); err != nil {
		t.Fatalf("dispatch task.created: %v", err)
	}

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "done" {
		t.Fatalf("status = %q, want done — parallel-terminal completion did not cascade", got.Status)
	}
}

// TestCascade_DepthGuardStopsRunawayChain proves the recursion guard: two
// mechanical workflows that ping-pong a task between two non-terminal statuses
// would recurse synchronously forever (stack overflow) without maxCascadeDepth.
// The guard converts it into a bounded, logged stop. The test completing at all
// (instead of overflowing) is the assertion; the log line confirms the cause.
func TestCascade_DepthGuardStopsRunawayChain(t *testing.T) {
	store := newTestStoreWith(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	var logBuf bytes.Buffer
	engine := NewEngine(store, tasks, agents, slog.New(slog.NewTextHandler(&logBuf, nil)))
	engine.SetOnComplete(cascadeOnComplete(engine, tasks))

	// Ping-pong between two real non-terminal statuses (definition validation
	// rejects non-enum values).
	addStatusWorkflow(t, store, "kickoff", "task.created", "", "todo")
	addStatusWorkflow(t, store, "todo2prog", "task.status_changed", "todo", "in-progress")
	addStatusWorkflow(t, store, "prog2todo", "task.status_changed", "in-progress", "todo")
	tasks.Put(TaskInfo{ID: "t1", Status: "new", AgentMode: "headless"})

	// Without the guard this never returns; with it, it unwinds at the bound.
	if _, err := engine.DispatchEvent("t1", "task.created", nil, nil); err != nil {
		t.Fatalf("dispatch task.created: %v", err)
	}

	if !strings.Contains(logBuf.String(), "workflow.cascade.depth-exceeded") {
		t.Fatalf("expected a cascade depth-exceeded log, got:\n%s", logBuf.String())
	}
	// The cascade map must not leak after unwinding.
	engine.mu.Lock()
	leaked := len(engine.cascadeDepth)
	engine.mu.Unlock()
	if leaked != 0 {
		t.Fatalf("cascadeDepth leaked %d entries after unwind", leaked)
	}
}
