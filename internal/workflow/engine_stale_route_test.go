package workflow

import (
	"log/slog"
	"testing"
	"time"
)

// gatedRecorder is an ArtifactRecorder that parks RecordTrace until released.
// HandleAgentComplete calls it after it has resolved the completion route but
// before AdvanceStep takes the per-task inflight lock, which is precisely the
// mid-advance window the stale-route pruner must not touch — the agent has
// finished, no inflight lock is held, and only the persisted route (or the
// completion marker replacing it) says the task is still busy.
type gatedRecorder struct {
	entered chan struct{}
	release chan struct{}
}

func newGatedRecorder() *gatedRecorder {
	return &gatedRecorder{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (g *gatedRecorder) RecordTrace(string, any) error {
	select {
	case g.entered <- struct{}{}:
	default:
	}
	<-g.release
	return nil
}

func (g *gatedRecorder) PutPlanSnapshot(string, string, string, string, string) error { return nil }
func (g *gatedRecorder) PutGeneric(string, string, string, string) error              { return nil }

// staleRouteEngine builds an engine parked on test-simple's implement step with
// the given persisted agent routes and no agent running for the task.
func staleRouteEngine(t *testing.T, logger *slog.Logger, routes map[string]string) (*Engine, *memTasks, *mockAgents) {
	t.Helper()
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, logger)

	wf := &Execution{
		WorkflowID:  "test-simple",
		CurrentStep: "implement",
		State:       ExecWaiting,
	}
	for agentID, stepID := range routes {
		wf.SetAgentRoute(agentID, stepID)
	}
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", AgentMode: "headless", Workflow: wf})
	return engine, tasks, agents
}

func implementStep(t *testing.T, engine *Engine) *Step {
	t.Helper()
	def, err := engine.store.Get("test-simple")
	if err != nil {
		t.Fatal(err)
	}
	step := def.StepByID("implement")
	if step == nil {
		t.Fatal("implement step missing from test-simple")
	}
	return step
}

// TestTryMarkResumeDispatching_StaleRoute covers both directions of #2824: a
// route whose agent no longer exists must be retired so the task resumes, while
// every route that still has an owner — a live agent, or a completion the
// engine has not finished routing — must keep blocking the resume.
func TestTryMarkResumeDispatching_StaleRoute(t *testing.T) {
	tests := []struct {
		name           string
		routes         map[string]string
		agentRunning   bool
		completing     bool
		wantAcquired   bool
		wantReason     string
		wantRoutesLeft int
		wantWarn       bool
	}{
		{
			name:           "route naming a dead agent is retired and the task resumes",
			routes:         map[string]string{"agent-ghost": "implement"},
			wantAcquired:   true,
			wantRoutesLeft: 0,
			wantWarn:       true,
		},
		{
			name:           "route backed by a live agent still blocks the resume",
			routes:         map[string]string{"agent-1": "implement"},
			agentRunning:   true,
			wantReason:     "agent-pending-completion",
			wantRoutesLeft: 1,
		},
		{
			name:           "route with a completion in flight still blocks the resume",
			routes:         map[string]string{"agent-1": "implement"},
			completing:     true,
			wantReason:     "agent-pending-completion",
			wantRoutesLeft: 1,
		},
		{
			name:           "route for an unrelated step is left alone",
			routes:         map[string]string{"agent-ghost": "triage"},
			wantAcquired:   true,
			wantRoutesLeft: 1,
		},
		{
			name: "only the matching stale route is retired",
			routes: map[string]string{
				"agent-ghost": "implement",
				"agent-other": "triage",
			},
			wantAcquired:   true,
			wantRoutesLeft: 1,
			wantWarn:       true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var records []slog.Record
			logger := slog.New(&demotionRecordHandler{records: &records})
			engine, tasks, agents := staleRouteEngine(t, logger, tc.routes)
			step := implementStep(t, engine)

			if tc.agentRunning {
				agents.mu.Lock()
				agents.running["t1"] = "agent-1"
				agents.mu.Unlock()
			}
			if tc.completing {
				defer engine.enterCompletion("t1")()
			}

			reason, acquired := engine.tryMarkResumeDispatching("t1", step)
			if acquired != tc.wantAcquired {
				t.Fatalf("acquired = %v, want %v (reason %q)", acquired, tc.wantAcquired, reason)
			}
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}

			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatal(err)
			}
			if n := len(got.Workflow.AgentRoutes); n != tc.wantRoutesLeft {
				t.Fatalf("routes left = %d (%v), want %d", n, got.Workflow.AgentRoutes, tc.wantRoutesLeft)
			}

			warned := false
			for _, r := range records {
				if r.Message != "workflow.resume-stalled.stale-route" {
					continue
				}
				warned = true
				if r.Level != slog.LevelWarn {
					t.Fatalf("stale-route log level = %v, want Warn", r.Level)
				}
				if got := recordAttr(r, "step"); got != "implement" {
					t.Fatalf("stale-route step attr = %q, want implement", got)
				}
			}
			if warned != tc.wantWarn {
				t.Fatalf("stale-route warning logged = %v, want %v", warned, tc.wantWarn)
			}
		})
	}
}

// TestResumeStalled_RecoversFromStaleAgentRoute is the end-to-end shape of the
// wedge: a task whose only blocker is a route left behind by a finished agent
// used to log "agent-pending-completion" on every tick forever. It must now
// re-dispatch instead.
func TestResumeStalled_RecoversFromStaleAgentRoute(t *testing.T) {
	engine, _, agents := staleRouteEngine(t, discardLogger(), map[string]string{"agent-ghost": "implement"})

	engine.ResumeStalled()

	if got := roleStartCount(agents, "implementation"); got != 1 {
		t.Fatalf("implementation dispatches = %d, want 1 — a stale route must not wedge ResumeStalled", got)
	}
}

// TestResumeStalled_MidAdvanceCompletionBlocksDuplicateDispatch is the
// regression guard the route check was written for. It drives the exact
// interleaving recovery's lost-callback bridge produces: HandleAgentComplete
// runs for an agent the manager no longer reports as running, and a
// ResumeStalled tick lands while that completion is between route resolution
// and AdvanceStep. No second agent may be started.
func TestResumeStalled_MidAdvanceCompletionBlocksDuplicateDispatch(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	recorder := newGatedRecorder()
	engine.SetArtifactRecorder(recorder)

	wf := &Execution{
		WorkflowID:  "test-simple",
		CurrentStep: "implement",
		State:       ExecWaiting,
	}
	wf.SetAgentRoute("agent-1", "implement")
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", AgentMode: "headless", Workflow: wf})

	completeDone := make(chan struct{})
	go func() {
		defer close(completeDone)
		engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "agent-1", Success: true, Result: "done"})
	}()

	select {
	case <-recorder.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("HandleAgentComplete never reached the trace recorder")
	}

	// The completion is parked mid-advance: the agent is gone from the manager
	// and no inflight lock is held, so only the completion marker can hold the
	// resume off.
	if agents.HasRunningAgent("t1") {
		t.Fatal("mock reports a running agent; the mid-advance window is not being exercised")
	}
	engine.ResumeStalled()
	if got := roleStartCount(agents, "implementation"); got != 0 {
		t.Fatalf("implementation dispatches = %d, want 0 — a mid-advance completion must not be duplicated", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Workflow.AgentRoute("agent-1"); !ok {
		t.Fatal("route for the completing agent was pruned mid-advance")
	}

	close(recorder.release)
	<-completeDone

	after, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Workflow.CurrentStep == "implement" {
		t.Fatalf("workflow did not advance past implement after completion: %+v", after.Workflow)
	}
}
