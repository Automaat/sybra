package workflow

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/autonomy"
	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/clock"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/taskstatus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// TestTaskFields_PlanCritiqueAndReplanCount locks the two fields the
// simple-task-plan replan/critique path depends on: task.plan_critique
// mirrors TaskInfo.PlanCritique verbatim (the human gate renders it as
// advisory context), and task.replan_count is start_replan's own
// step-history count as of the current call — the number of full opus
// replan cycles already spent, not counting the one about to start.
func TestTaskFields_PlanCritiqueAndReplanCount(t *testing.T) {
	t.Parallel()

	t.Run("plan_critique mirrors TaskInfo verbatim", func(t *testing.T) {
		ti := TaskInfo{ID: "t1", PlanCritique: "# Plan Review: APPROVE\n"}
		fields := taskFields(ti)
		if got := fields["task.plan_critique"]; got != ti.PlanCritique {
			t.Errorf("task.plan_critique = %q, want %q", got, ti.PlanCritique)
		}
	})

	t.Run("replan_count absent without a workflow", func(t *testing.T) {
		fields := taskFields(TaskInfo{ID: "t1"})
		if _, ok := fields["task.replan_count"]; ok {
			t.Errorf("task.replan_count = %q, want absent when Workflow is nil", fields["task.replan_count"])
		}
	})

	t.Run("replan_count reflects start_replan history", func(t *testing.T) {
		wf := &Execution{}
		wf.RecordStep(StepRecord{StepID: "start_replan", Status: "completed"})
		wf.RecordStep(StepRecord{StepID: "start_replan", Status: "completed"})
		ti := TaskInfo{ID: "t1", Workflow: wf}
		fields := taskFields(ti)
		if got := fields["task.replan_count"]; got != "2" {
			t.Errorf("task.replan_count = %q, want %q", got, "2")
		}
	})
}

func TestTaskFields_Role(t *testing.T) {
	t.Parallel()

	ti := TaskInfo{ID: "t1", Role: "review"}
	fields := taskFields(ti)
	if got := fields["task.role"]; got != ti.Role {
		t.Fatalf("task.role = %q, want %q", got, ti.Role)
	}
}

// --- Test helpers ---

func init() {
	// Skip real backoff waits in the ensure_pr_closes_issue verify
	// retry loop — tests drive attempt counts via the linker queue.
	prVerifySleep = func(time.Duration) {}
	prVerifyBackoffs = []time.Duration{0, 0, 0}
	// Skip real backoff waits in the classify_task retry loop.
	classifyTaskRetryBackoffs = []time.Duration{0, 0, 0}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// newTestStore creates a Store backed by a temp dir and copies the
// testdata/test-simple.yaml workflow into it.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return newTestStoreWith(t, "test-simple.yaml")
}

// newTestStoreWith copies one or more testdata yaml files into a fresh
// Store. Use this when a test needs a different workflow definition than
// the default test-simple.yaml.
func newTestStoreWith(t *testing.T, files ...string) *Store {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		src, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("read test workflow %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func newInlineTestStore(t *testing.T, name, yaml string) *Store {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write inline workflow %s: %v", name, err)
	}
	return store
}

func TestReplayPersistedEffects(t *testing.T) {
	t.Run("pending intent replays through executeSteps", func(t *testing.T) {
		store := newTestStore(t)
		tasks := &memTasks{tasks: map[string]*TaskInfo{
			"t1": {
				ID:         "t1",
				Generation: 1,
				Status:     "in-progress",
				Workflow: &Execution{
					WorkflowID:  "test-simple",
					CurrentStep: "implement",
					State:       ExecWaiting,
					EffectLog: []EffectRecord{{
						ID:       EffectID{Generation: 1, StepSeq: 0, StepID: "implement", Pos: effectPosStepAction},
						IntentAt: time.Now().UTC(),
					}},
				},
			},
		}, gets: map[string]int{}}
		agents := newMockAgents()
		engine := NewTestEngine(store, tasks, agents, discardLogger())

		engine.ReplayPersistedEffects()

		if len(agents.calls) != 1 {
			t.Fatalf("StartAgent calls = %d, want 1", len(agents.calls))
		}
		got, err := tasks.GetTask("t1")
		if err != nil {
			t.Fatal(err)
		}
		if got.Workflow == nil {
			t.Fatal("workflow = nil, want active workflow after replay")
		}
		if got.Workflow.CurrentStep != "implement" {
			t.Fatalf("current step = %q, want implement", got.Workflow.CurrentStep)
		}
		if len(got.Workflow.StepHistory) != 0 {
			t.Fatalf("step history = %+v, want no sync completion for async replay", got.Workflow.StepHistory)
		}
		if got.Workflow.EffectLog[0].CompletedAt == nil {
			t.Fatalf("effect log = %+v, want completed replayed effect", got.Workflow.EffectLog)
		}
	})

	t.Run("pending intent survives unrelated task generation change", func(t *testing.T) {
		store := newTestStore(t)
		originalID := EffectID{Generation: 1, StepSeq: 0, StepID: "implement", Pos: effectPosStepAction}
		tasks := &memTasks{tasks: map[string]*TaskInfo{
			"t1": {
				ID:         "t1",
				Generation: 2,
				Status:     "in-progress",
				Workflow: &Execution{
					WorkflowID:  "test-simple",
					CurrentStep: "implement",
					State:       ExecWaiting,
					EffectLog: []EffectRecord{{
						ID:       originalID,
						IntentAt: time.Now().UTC(),
					}},
				},
			},
		}, gets: map[string]int{}}
		agents := newMockAgents()
		engine := NewTestEngine(store, tasks, agents, discardLogger())

		engine.ReplayPersistedEffects()

		if len(agents.calls) != 1 {
			t.Fatalf("StartAgent calls = %d, want 1", len(agents.calls))
		}
		got, err := tasks.GetTask("t1")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Workflow.EffectLog) != 1 {
			t.Fatalf("effect log len = %d, want 1: %+v", len(got.Workflow.EffectLog), got.Workflow.EffectLog)
		}
		if !got.Workflow.EffectLog[0].ID.Equal(originalID) {
			t.Fatalf("effect ID = %s, want original %s", got.Workflow.EffectLog[0].ID.String(), originalID.String())
		}
		if got.Workflow.EffectLog[0].CompletedAt == nil {
			t.Fatalf("effect log = %+v, want completed replayed effect", got.Workflow.EffectLog)
		}
	})

	t.Run("pending intent does not reconcile stale downstream wait status", func(t *testing.T) {
		store := newInlineTestStore(t, "replay-stale-status", `
id: replay-stale-status
name: Replay Stale Status
trigger:
  on: task.created
steps:
  - id: implement
    type: run_agent
    config:
      role: implementation
      mode: headless
      model: sonnet
      wait_for_status: agent-finished
      prompt: "Implement {{.Task.ID}}"
    next:
      - goto: review_plan
  - id: review_plan
    type: wait_human
    config:
      status: plan-review
    next:
      - goto: ""
`)
		tasks := &memTasks{tasks: map[string]*TaskInfo{
			"t1": {
				ID:         "t1",
				Generation: 1,
				Status:     "plan-review",
				AgentMode:  "headless",
				Workflow: &Execution{
					WorkflowID:  "replay-stale-status",
					CurrentStep: "implement",
					State:       ExecWaiting,
					EffectLog: []EffectRecord{{
						ID:       EffectID{Generation: 1, StepSeq: 0, StepID: "implement", Pos: effectPosStepAction},
						IntentAt: time.Now().UTC(),
					}},
				},
			},
		}, gets: map[string]int{}}
		agents := newMockAgents()
		engine := NewTestEngine(store, tasks, agents, discardLogger())

		engine.ReplayPersistedEffects()

		if len(agents.calls) != 1 {
			t.Fatalf("StartAgent calls = %d, want 1", len(agents.calls))
		}
		got, err := tasks.GetTask("t1")
		if err != nil {
			t.Fatal(err)
		}
		if got.Workflow == nil {
			t.Fatal("workflow = nil, want active workflow")
		}
		if got.Workflow.CurrentStep != "implement" {
			t.Fatalf("current step = %q, want implement", got.Workflow.CurrentStep)
		}
		if got.Workflow.State != ExecWaiting {
			t.Fatalf("state = %q, want %q", got.Workflow.State, ExecWaiting)
		}
		if len(got.Workflow.EffectLog) != 1 || got.Workflow.EffectLog[0].CompletedAt == nil {
			t.Fatalf("effect log = %+v, want replayed effect completed", got.Workflow.EffectLog)
		}
	})

	t.Run("pending intent waits for retry_after", func(t *testing.T) {
		store := newInlineTestStore(t, "replay-verify", `
id: replay-verify
name: Replay Verify
trigger:
  on: task.created
steps:
  - id: verify_checks
    type: verify_checks
    next:
      - goto: ""
`)
		tasks := &memTasks{tasks: map[string]*TaskInfo{
			"t1": {
				ID:         "t1",
				Generation: 1,
				Status:     "in-progress",
				Workflow: &Execution{
					WorkflowID:  "replay-verify",
					CurrentStep: "verify_checks",
					State:       ExecWaiting,
					Variables: map[string]string{
						workflowRetryAfterVar: time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
					},
					EffectLog: []EffectRecord{{
						ID:       EffectID{Generation: 1, StepSeq: 0, StepID: "verify_checks", Pos: effectPosStepAction},
						IntentAt: time.Now().UTC(),
					}},
				},
			},
		}, gets: map[string]int{}}
		agents := newMockAgents()
		engine := NewTestEngine(store, tasks, agents, discardLogger())

		engine.ReplayPersistedEffects()

		got, err := tasks.GetTask("t1")
		if err != nil {
			t.Fatal(err)
		}
		if got.Workflow == nil {
			t.Fatal("workflow = nil, want active workflow")
		}
		if got.Workflow.CurrentStep != "verify_checks" {
			t.Fatalf("current step = %q, want verify_checks", got.Workflow.CurrentStep)
		}
		if got.Workflow.EffectLog[0].CompletedAt != nil {
			t.Fatalf("effect log = %+v, want pending effect while retry_after is in future", got.Workflow.EffectLog)
		}
	})

	t.Run("task-scoped replay only touches requested task", func(t *testing.T) {
		store := newTestStore(t)
		tasks := &memTasks{tasks: map[string]*TaskInfo{
			"t1": {
				ID:         "t1",
				Generation: 1,
				Status:     "in-progress",
				Workflow: &Execution{
					WorkflowID:  "test-simple",
					CurrentStep: "implement",
					State:       ExecWaiting,
					EffectLog: []EffectRecord{{
						ID:       EffectID{Generation: 1, StepSeq: 0, StepID: "implement", Pos: effectPosStepAction},
						IntentAt: time.Now().UTC(),
					}},
				},
			},
			"t2": {
				ID:         "t2",
				Generation: 1,
				Status:     "in-progress",
				Workflow: &Execution{
					WorkflowID:  "test-simple",
					CurrentStep: "implement",
					State:       ExecWaiting,
					EffectLog: []EffectRecord{{
						ID:       EffectID{Generation: 1, StepSeq: 0, StepID: "implement", Pos: effectPosStepAction},
						IntentAt: time.Now().UTC(),
					}},
				},
			},
		}, gets: map[string]int{}}
		agents := newMockAgents()
		engine := NewTestEngine(store, tasks, agents, discardLogger())

		if handled := engine.ReplayPersistedEffectsForTask("t2"); !handled {
			t.Fatal("ReplayPersistedEffectsForTask returned false, want true")
		}
		if len(agents.calls) != 1 {
			t.Fatalf("StartAgent calls = %d, want 1", len(agents.calls))
		}
		if agents.calls[0].TaskID != "t2" {
			t.Fatalf("replayed task = %q, want t2", agents.calls[0].TaskID)
		}
		got1, err := tasks.GetTask("t1")
		if err != nil {
			t.Fatal(err)
		}
		if got1.Workflow.EffectLog[0].CompletedAt != nil {
			t.Fatalf("t1 effect log = %+v, want pending untouched effect", got1.Workflow.EffectLog)
		}
		got2, err := tasks.GetTask("t2")
		if err != nil {
			t.Fatal(err)
		}
		if got2.Workflow.EffectLog[0].CompletedAt == nil {
			t.Fatalf("t2 effect log = %+v, want completed replayed effect", got2.Workflow.EffectLog)
		}
	})

	t.Run("pending run_agent intent waits for provider cooldown", func(t *testing.T) {
		store := newTestStore(t)
		tasks := &memTasks{tasks: map[string]*TaskInfo{
			"t1": {
				ID:         "t1",
				Generation: 1,
				Status:     "in-progress",
				Workflow: &Execution{
					WorkflowID:  "test-simple",
					CurrentStep: "implement",
					State:       ExecWaiting,
					EffectLog: []EffectRecord{{
						ID:       EffectID{Generation: 1, StepSeq: 0, StepID: "implement", Pos: effectPosStepAction},
						IntentAt: time.Now().UTC(),
					}},
				},
			},
		}, gets: map[string]int{}}
		agents := newMockAgents()
		agents.SetProviderRateLimited(true)
		engine := NewTestEngine(store, tasks, agents, discardLogger())

		engine.ReplayPersistedEffects()

		if len(agents.calls) != 0 {
			t.Fatalf("StartAgent calls = %d, want 0", len(agents.calls))
		}
		got, err := tasks.GetTask("t1")
		if err != nil {
			t.Fatal(err)
		}
		if got.Workflow.EffectLog[0].CompletedAt != nil {
			t.Fatalf("effect log = %+v, want pending effect while provider is rate-limited", got.Workflow.EffectLog)
		}
	})

	t.Run("completed async effect reconciles as no-op", func(t *testing.T) {
		now := time.Now().UTC()
		completedAt := now
		store := newTestStore(t)
		tasks := &memTasks{tasks: map[string]*TaskInfo{
			"t1": {
				ID:         "t1",
				Generation: 1,
				Status:     "in-progress",
				Workflow: &Execution{
					WorkflowID:  "test-simple",
					CurrentStep: "implement",
					State:       ExecWaiting,
					EffectLog: []EffectRecord{{
						ID:          EffectID{Generation: 1, StepSeq: 0, StepID: "implement", Pos: effectPosStepAction},
						IntentAt:    now.Add(-time.Minute),
						CompletedAt: &completedAt,
					}},
				},
			},
		}, gets: map[string]int{}}
		agents := newMockAgents()
		engine := NewTestEngine(store, tasks, agents, discardLogger())

		engine.ReplayPersistedEffects()

		if len(agents.calls) != 0 {
			t.Fatalf("StartAgent calls = %d, want 0", len(agents.calls))
		}
		got, err := tasks.GetTask("t1")
		if err != nil {
			t.Fatal(err)
		}
		if got.Workflow == nil {
			t.Fatal("workflow = nil, want workflow preserved")
		}
		if got.Workflow.CurrentStep != "implement" {
			t.Fatalf("current step = %q, want implement", got.Workflow.CurrentStep)
		}
		if got.Workflow.State != ExecWaiting {
			t.Fatalf("state = %q, want %q", got.Workflow.State, ExecWaiting)
		}
		if len(got.Workflow.StepHistory) != 0 {
			t.Fatalf("step history = %+v, want unchanged workflow", got.Workflow.StepHistory)
		}
	})

	t.Run("terminal current-step headless run defers replay to stale recovery", func(t *testing.T) {
		store := newInlineTestStore(t, "replay-review", `
id: replay-review
name: Replay Review
trigger:
  on: task.created
steps:
  - id: review
    name: Review
    type: run_agent
    config:
      role: review
      mode: headless
      model: sonnet
      prompt: "Review {{.Task.ID}}"
`)
		tasks := &memTasks{tasks: map[string]*TaskInfo{
			"t1": {
				ID:         "t1",
				Generation: 1,
				Status:     "in-progress",
				Workflow: &Execution{
					WorkflowID:  "replay-review",
					CurrentStep: "review",
					State:       ExecWaiting,
					AgentRoutes: map[string]string{"review-1": "review"},
					EffectLog: []EffectRecord{{
						ID:       EffectID{Generation: 1, StepSeq: 0, StepID: "review", Pos: effectPosStepAction},
						IntentAt: time.Now().UTC(),
					}},
				},
				AgentRuns: []AgentRunInfo{{
					AgentID:   "review-1",
					Role:      "review",
					Mode:      "headless",
					State:     "stopped",
					Outcome:   "failure",
					StartedAt: time.Now().Add(-10 * time.Minute),
				}},
			},
		}, gets: map[string]int{}}
		agents := newMockAgents()
		engine := NewTestEngine(store, tasks, agents, discardLogger())

		if consumed := engine.ReplayPersistedEffectsForTask("t1"); consumed {
			t.Fatal("ReplayPersistedEffectsForTask consumed tick, want stale recovery to handle terminal run")
		}
		if len(agents.calls) != 0 {
			t.Fatalf("StartAgent calls = %d, want 0 while terminal run is pending stale recovery", len(agents.calls))
		}
	})

	t.Run("legacy terminal verifier run without route defers replay to stale recovery", func(t *testing.T) {
		store := newInlineTestStore(t, "replay-review", `
id: replay-review
name: Replay Review
trigger:
  on: task.created
steps:
  - id: review
    name: Review
    type: run_agent
    config:
      role: review
      mode: headless
      model: sonnet
      prompt: "Review {{.Task.ID}}"
`)
		tasks := &memTasks{tasks: map[string]*TaskInfo{
			"t1": {
				ID:         "t1",
				Generation: 1,
				Status:     "in-progress",
				Workflow: &Execution{
					WorkflowID:  "replay-review",
					CurrentStep: "review",
					State:       ExecWaiting,
					EffectLog: []EffectRecord{{
						ID:       EffectID{Generation: 1, StepSeq: 0, StepID: "review", Pos: effectPosStepAction},
						IntentAt: time.Now().UTC(),
					}},
				},
				AgentRuns: []AgentRunInfo{{
					AgentID:   "review-legacy",
					Role:      "review",
					Mode:      "headless",
					State:     "stopped",
					Outcome:   "failure",
					StartedAt: time.Now().Add(-10 * time.Minute),
				}},
			},
		}, gets: map[string]int{}}
		agents := newMockAgents()
		engine := NewTestEngine(store, tasks, agents, discardLogger())

		if consumed := engine.ReplayPersistedEffectsForTask("t1"); consumed {
			t.Fatal("ReplayPersistedEffectsForTask consumed tick, want stale recovery to own legacy verifier migration")
		}
		if len(agents.calls) != 0 {
			t.Fatalf("StartAgent calls after effect replay = %d, want 0 for legacy verifier recovery", len(agents.calls))
		}

		engine.ResumeStalled()

		if len(agents.calls) != 1 {
			t.Fatalf("StartAgent calls after ResumeStalled = %d, want 1", len(agents.calls))
		}
		if got := agents.calls[0].Role; got != "review" {
			t.Fatalf("resumed role = %q, want review", got)
		}
	})

	t.Run("legacy terminal non-verifier run without route stays replay-owned", func(t *testing.T) {
		store := newInlineTestStore(t, "replay-implement", `
id: replay-implement
name: Replay Implement
trigger:
  on: task.created
steps:
  - id: implement
    name: Implement
    type: run_agent
    config:
      role: implementation
      mode: headless
      model: sonnet
      prompt: "Implement {{.Task.ID}}"
`)
		tasks := &memTasks{tasks: map[string]*TaskInfo{
			"t1": {
				ID:         "t1",
				Generation: 1,
				Status:     "in-progress",
				Workflow: &Execution{
					WorkflowID:  "replay-implement",
					CurrentStep: "implement",
					State:       ExecWaiting,
					EffectLog: []EffectRecord{{
						ID:       EffectID{Generation: 1, StepSeq: 0, StepID: "implement", Pos: effectPosStepAction},
						IntentAt: time.Now().UTC(),
					}},
				},
				AgentRuns: []AgentRunInfo{{
					AgentID:   "impl-legacy",
					Role:      "implementation",
					Mode:      "headless",
					State:     "stopped",
					Outcome:   "failure",
					StartedAt: time.Now().Add(-10 * time.Minute),
				}},
			},
		}, gets: map[string]int{}}
		agents := newMockAgents()
		engine := NewTestEngine(store, tasks, agents, discardLogger())

		if consumed := engine.ReplayPersistedEffectsForTask("t1"); !consumed {
			t.Fatal("ReplayPersistedEffectsForTask returned false, want replay to keep ownership for non-verifier route-less runs")
		}
		if len(agents.calls) != 1 {
			t.Fatalf("StartAgent calls after effect replay = %d, want 1", len(agents.calls))
		}
		if got := agents.calls[0].Role; got != "implementation" {
			t.Fatalf("replayed role = %q, want implementation", got)
		}
	})
}

func workflowMetricValue(t *testing.T, name string, labels []string) float64 {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody)
	rec := httptest.NewRecorder()
	promhttp.Handler().ServeHTTP(rec, req)
	for line := range strings.SplitSeq(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, "#") || !strings.HasPrefix(line, name) {
			continue
		}
		matches := true
		for _, label := range labels {
			if !strings.Contains(line, label) {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			t.Fatalf("parse metric %q from %q: %v", name, line, err)
		}
		return v
	}
	return 0
}

// --- In-memory TaskProvider ---

type memTasks struct {
	mu               sync.Mutex
	tasks            map[string]*TaskInfo
	reasons          map[string]string
	escalations      map[string]autonomy.EscalationReason
	outcomes         map[string]autonomy.Outcome
	steers           map[string]string
	gets             map[string]int
	onGet            func(id string, t *TaskInfo, count int)
	onSetWorkflow    func(id string)
	appendErr        error
	failGet          bool
	failSetWorkflow  bool
	failSetWorkflowN int
	incompleteRuns   []string
}

func newMemTasks() *memTasks {
	return &memTasks{
		tasks:       make(map[string]*TaskInfo),
		reasons:     make(map[string]string),
		escalations: make(map[string]autonomy.EscalationReason),
		outcomes:    make(map[string]autonomy.Outcome),
		steers:      make(map[string]string),
		gets:        make(map[string]int),
	}
}

// SetSteer arms a pending supervisor steer for a task, as the watchdog's
// headless nudge would. Consumed (and cleared) by ConsumeSupervisorSteer.
func (m *memTasks) SetSteer(id, steer string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.steers[id] = steer
}

func (m *memTasks) ConsumeSupervisorSteer(taskID, prompt string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	steer := m.steers[taskID]
	if steer == "" {
		return prompt, nil
	}
	delete(m.steers, taskID)
	return "Supervisor course-correction: " + steer + "\n\n" + prompt, nil
}

func (m *memTasks) Put(t TaskInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[t.ID] = &t
}

// mustGetTask returns the task's current in-memory state, failing the test
// if it was never Put — a guarded lookup so callers don't dereference a
// possibly-absent map entry directly.
func (m *memTasks) mustGetTask(t *testing.T, id string) TaskInfo {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	tk, ok := m.tasks[id]
	if !ok {
		t.Fatalf("task %q not found", id)
	}
	return *tk
}

func (m *memTasks) SetGetTaskHook(hook func(id string, t *TaskInfo, count int)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onGet = hook
}

// SetFailGet forces GetTask to error even when the task exists, simulating a
// transient store read hiccup. Other operations (UpdateTaskStatus, etc.) keep
// working, so it can model a read failure concurrent with a live task.
func (m *memTasks) SetFailGet(fail bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failGet = fail
}

func (m *memTasks) GetTask(id string) (TaskInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failGet {
		return TaskInfo{}, fmt.Errorf("task %s: simulated store read failure", id)
	}
	t, ok := m.tasks[id]
	if !ok {
		return TaskInfo{}, fmt.Errorf("task %s not found", id)
	}
	m.gets[id]++
	if m.onGet != nil {
		m.onGet(id, t, m.gets[id])
	}
	cp := *t
	if t.Workflow != nil {
		cp.Workflow = t.Workflow.Clone()
	}
	return cp, nil
}

func (m *memTasks) ListTasks() ([]TaskInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []TaskInfo
	for _, t := range m.tasks {
		out = append(out, *t)
	}
	return out, nil
}

func (m *memTasks) UpdateTaskStatus(id string, status taskstatus.Status, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	t.Status = status
	t.StatusReason = reason
	m.reasons[id] = reason
	return nil
}

func (m *memTasks) ClearTaskStatusReasonIf(id string, expectedStatus taskstatus.Status, expectedReason string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return false, fmt.Errorf("task %s not found", id)
	}
	if t.Status != expectedStatus || t.StatusReason != expectedReason {
		return false, nil
	}
	t.StatusReason = ""
	m.reasons[id] = ""
	return true, nil
}

func (m *memTasks) ClearTaskStatusReasonAndSetWorkflowIf(id, expectedStatus, expectedReason string, wf *Execution) (bool, error) {
	m.mu.Lock()
	if m.failSetWorkflow || m.failSetWorkflowN > 0 {
		if m.failSetWorkflowN > 0 {
			m.failSetWorkflowN--
		}
		m.mu.Unlock()
		return false, fmt.Errorf("simulated write failure for task %s", id)
	}
	hook := m.onSetWorkflow
	m.mu.Unlock()
	// Fire the concurrency hook ahead of the compare rather than after the
	// write: this call is a single atomic store write, so a racing update can
	// only land in front of it — and must then lose the compare instead of
	// being overwritten.
	if hook != nil {
		hook(id)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return false, fmt.Errorf("task %s not found", id)
	}
	if string(t.Status) != expectedStatus || t.StatusReason != expectedReason {
		// Mismatch writes nothing at all — the workflow included.
		return false, nil
	}
	t.StatusReason = ""
	m.reasons[id] = ""
	t.Workflow = wf.Clone()
	return true, nil
}
func (m *memTasks) UpdateTaskBlocker(id string, status taskstatus.Status, reason string, state blocker.State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	t.Status = status
	t.StatusReason = reason
	t.Blocker = state
	m.reasons[id] = reason
	return nil
}

// Reason returns the last status reason recorded for a task.
func (m *memTasks) Reason(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reasons[id]
}

func (m *memTasks) UpdateTaskPR(id string, prNumber int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	t.PRNumber = prNumber
	return nil
}

func (m *memTasks) MarkTaskReviewed(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	t.Reviewed = true
	m.tasks[id] = t
	return nil
}

func (m *memTasks) SetCodeReviewVerdict(id, verdict string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	t.CodeReviewVerdict = verdict
	m.tasks[id] = t
	return nil
}

func (m *memTasks) MarkAgentRunProtocolViolation(taskID, agentID, violation string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[taskID]
	if !ok {
		return fmt.Errorf("task %s not found", taskID)
	}
	for i := range t.AgentRuns {
		if t.AgentRuns[i].AgentID == agentID {
			t.AgentRuns[i].ProtocolViolation = violation
			return nil
		}
	}
	return fmt.Errorf("agent run %s not found for task %s", agentID, taskID)
}

func (m *memTasks) MarkAgentRunTestOutcome(taskID, agentID, outcome, fingerprint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[taskID]
	if !ok {
		return fmt.Errorf("task %s not found", taskID)
	}
	for i := range t.AgentRuns {
		if t.AgentRuns[i].AgentID == agentID {
			t.AgentRuns[i].TestOutcome = outcome
			t.AgentRuns[i].TestFailureFingerprint = fingerprint
			return nil
		}
	}
	return fmt.Errorf("agent run %s not found for task %s", agentID, taskID)
}

func (m *memTasks) IncompleteRuns() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.incompleteRuns...)
}

func (m *memTasks) MarkAgentRunIncomplete(taskID, agentID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.incompleteRuns = append(m.incompleteRuns, taskID+"/"+agentID)
	return nil
}

func (m *memTasks) RecordAgentRunFinalCommit(taskID, agentID, headSHA, source string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[taskID]
	if !ok {
		return fmt.Errorf("task %s not found", taskID)
	}
	for i := range t.AgentRuns {
		if t.AgentRuns[i].AgentID == agentID {
			t.AgentRuns[i].HeadSHA = headSHA
			t.AgentRuns[i].FinalCommitSource = source
			return nil
		}
	}
	return fmt.Errorf("agent run %s not found for task %s", agentID, taskID)
}

func (m *memTasks) AppendTaskBody(id, content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.appendErr != nil {
		return m.appendErr
	}
	t, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	body := strings.TrimRight(t.Body, "\n")
	if body != "" {
		body += "\n\n"
	}
	t.Body = body + content + "\n"
	return nil
}

func (m *memTasks) ReplaceTaskBody(id, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	t.Body = body
	return nil
}

func (m *memTasks) SetWorkflow(id string, wf *Execution) error {
	m.mu.Lock()
	if m.failSetWorkflow || m.failSetWorkflowN > 0 {
		if m.failSetWorkflowN > 0 {
			m.failSetWorkflowN--
		}
		m.mu.Unlock()
		return fmt.Errorf("simulated write failure for task %s", id)
	}
	t, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("task %s not found", id)
	}
	t.Workflow = wf.Clone()
	hook := m.onSetWorkflow
	m.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	return nil
}

func (m *memTasks) SetStatusAndWorkflow(id, status, reason string, wf *Execution) error {
	m.mu.Lock()
	if m.failSetWorkflow || m.failSetWorkflowN > 0 {
		if m.failSetWorkflowN > 0 {
			m.failSetWorkflowN--
		}
		m.mu.Unlock()
		return fmt.Errorf("simulated write failure for task %s", id)
	}
	t, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("task %s not found", id)
	}
	t.Status = taskstatus.Status(status)
	t.StatusReason = reason
	m.reasons[id] = reason
	t.Workflow = wf.Clone()
	hook := m.onSetWorkflow
	m.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	return nil
}

func (m *memTasks) SetEscalationAndWorkflow(id, status, reason string, escalation autonomy.EscalationReason, outcome autonomy.Outcome, wf *Execution) error {
	if err := m.SetStatusAndWorkflow(id, status, reason, wf); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.escalations[id] = escalation
	m.outcomes[id] = outcome
	return nil
}

func assertWorkflowMachineQuarantine(t *testing.T, tasks *memTasks, id, code string) {
	t.Helper()
	tasks.mu.Lock()
	defer tasks.mu.Unlock()
	escalation := tasks.escalations[id]
	if escalation.Code != code || escalation.Owner != autonomy.FailureOwnerMachine || escalation.Provenance != autonomy.ProvenanceControlPlane {
		t.Fatalf("escalation = %+v, want typed machine-owned %s", escalation, code)
	}
	if outcome := tasks.outcomes[id]; outcome != autonomy.OutcomeQuarantined {
		t.Fatalf("autonomy outcome = %q, want %q", outcome, autonomy.OutcomeQuarantined)
	}
}

func (m *memTasks) SetBlockerAndWorkflow(id, status, reason string, state blocker.State, wf *Execution) error {
	m.mu.Lock()
	if m.failSetWorkflow || m.failSetWorkflowN > 0 {
		if m.failSetWorkflowN > 0 {
			m.failSetWorkflowN--
		}
		m.mu.Unlock()
		return fmt.Errorf("simulated write failure for task %s", id)
	}
	t, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("task %s not found", id)
	}
	t.Status = taskstatus.Status(status)
	t.StatusReason = reason
	t.Blocker = state
	m.reasons[id] = reason
	t.Workflow = wf.Clone()
	hook := m.onSetWorkflow
	m.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	return nil
}
func (m *memTasks) SetWorkflowIf(id string, fence WorkflowWriteFence, wf *Execution) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return false, fmt.Errorf("task %s not found", id)
	}
	if t.Generation != fence.Generation || t.Status != fence.Status ||
		t.StatusReason != fence.StatusReason || t.Workflow == nil ||
		t.Workflow.WorkflowID != fence.WorkflowID || t.Workflow.CurrentStep != fence.CurrentStep ||
		t.Workflow.State != fence.State {
		return false, nil
	}
	t.Workflow = wf.Clone()
	return true, nil
}

func (m *memTasks) SetStatusAndWorkflowIf(id string, fence WorkflowWriteFence, status taskstatus.Status, reason string, wf *Execution) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return false, fmt.Errorf("task %s not found", id)
	}
	if t.Generation != fence.Generation || t.Status != fence.Status ||
		t.StatusReason != fence.StatusReason || t.Workflow == nil ||
		t.Workflow.WorkflowID != fence.WorkflowID || t.Workflow.CurrentStep != fence.CurrentStep ||
		t.Workflow.State != fence.State {
		return false, nil
	}
	t.Status = status
	t.StatusReason = reason
	m.reasons[id] = reason
	t.Workflow = wf.Clone()
	return true, nil
}

func (m *memTasks) ClaimWorkflowEffect(id string, claim EffectClaim) (EffectClaimResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return EffectClaimResult{}, fmt.Errorf("task %s not found", id)
	}
	if t.Workflow == nil {
		return EffectClaimResult{}, fmt.Errorf("task %s has no workflow", id)
	}
	wf := t.Workflow.Clone()
	result, err := wf.ClaimEffect(claim)
	result.Workflow = wf
	if err != nil {
		return result, err
	}
	t.Workflow = wf
	return result, nil
}

func (m *memTasks) CompleteWorkflowEffect(id string, claim EffectClaim) (EffectClaimResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return EffectClaimResult{}, fmt.Errorf("task %s not found", id)
	}
	if t.Workflow == nil {
		return EffectClaimResult{}, fmt.Errorf("task %s has no workflow", id)
	}
	wf := t.Workflow.Clone()
	result, err := wf.CompleteEffect(claim)
	result.Workflow = wf
	if err != nil {
		return result, err
	}
	t.Workflow = wf
	return result, nil
}

func (m *memTasks) ReleaseWorkflowEffect(id string, claim EffectClaim) (EffectClaimResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return EffectClaimResult{}, fmt.Errorf("task %s not found", id)
	}
	if t.Workflow == nil {
		return EffectClaimResult{}, fmt.Errorf("task %s has no workflow", id)
	}
	wf := t.Workflow.Clone()
	result, err := wf.ReleaseEffect(claim)
	result.Workflow = wf
	if err != nil {
		return result, err
	}
	t.Workflow = wf
	return result, nil
}

func (m *memTasks) WriteSidecar(id, kind, content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.appendErr != nil {
		return m.appendErr
	}
	t, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	if name, isDraft := strings.CutPrefix(kind, "plan_draft."); isDraft {
		if t.PlanDrafts == nil {
			t.PlanDrafts = map[string]string{}
		}
		t.PlanDrafts[name] = content
		return nil
	}
	switch kind {
	case "plan":
		t.Plan = content
	case "plan_contract":
		t.PlanContract = content
	case "code_review":
		t.CodeReview = content
	case "plan_critique":
		t.PlanCritique = content
	case "plan_research":
		t.PlanResearch = content
	case "plan_decisions":
		t.PlanDecisions = content
	case "plan_brief":
		t.PlanBrief = content
	case "current_test_failures":
		t.CurrentTestFailures = content
	case "acceptance_ledger":
		t.AcceptanceLedger = content
	case "spec_decision":
		t.SpecDecision = content
	default:
		return fmt.Errorf("unknown sidecar kind %q", kind)
	}
	return nil
}

func setWorkflowAgentRoute(t *testing.T, tasks *memTasks, taskID, agentID, stepID string) {
	t.Helper()
	ti, err := tasks.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask(%s): %v", taskID, err)
	}
	if ti.Workflow == nil {
		t.Fatalf("task %s has no workflow", taskID)
	}
	ti.Workflow.SetAgentRoute(agentID, stepID)
	if err := tasks.SetWorkflow(taskID, ti.Workflow); err != nil {
		t.Fatalf("SetWorkflow(%s): %v", taskID, err)
	}
}

func lookupWorkflowAgentRoute(t *testing.T, engine *Engine, taskID, agentID string) (string, bool) {
	t.Helper()
	return engine.lookupAgentStep(taskID, agentID)
}

// SetStatus is a test helper to simulate an agent changing task status.
func (m *memTasks) SetStatus(id string, status taskstatus.Status) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tasks[id]; ok {
		t.Status = status
	}
}

// --- Mock AgentLauncher ---

type startCall struct {
	TaskID, Role, Mode, Model, Provider, Prompt, Dir string
	AllowedTools                                     []string
	NeedsWorktree                                    bool
	OneShot                                          bool
	OutputSchema                                     string
	CleanRetryRef                                    string
	Assignment                                       AgentAssignment
}

type sentPrompt struct {
	AgentID, Message string
}

type mockAgents struct {
	mu                sync.Mutex
	calls             []startCall
	prompts           []sentPrompt
	running           map[string]string // taskID -> agentID
	roles             map[string]string // taskID+"/"+role -> agentID
	counter           int
	failSpawn         error           // when non-nil, StartAgent returns this error and records nothing
	providerRateLimit bool            // when true, ProviderRateLimited reports true for every provider
	providerFailover  bool            // when true, ProviderCanFailover reports true
	rateLimited       map[string]bool // provider -> rate-limited
	unhealthy         map[string]bool // provider -> unhealthy (config-disabled/probe-down); absent = healthy
	dispatchClaimed   map[string]bool // taskID -> a claim is held by an out-of-band dispatcher (e.g. simulated recovery)
	defaultProvider   string
	claimInsideStart  bool
	failSpawnOnce     error
	startGate         chan struct{}
	startEntered      chan struct{}
	admitDenyReason   string // when non-empty, AdmitDispatch denies with this reason
}

type panicStartAgents struct{ *mockAgents }

func (p *panicStartAgents) StartAgent(taskID, role, mode, model, provider, prompt, dir string, allowedTools []string, needsWorktree, oneShot bool, outputSchema, cleanRetryRef string, assignment AgentAssignment) (agentID, startedDir, baselineRef string, err error) {
	panic("boom")
}

func newMockAgents() *mockAgents {
	return &mockAgents{
		running: make(map[string]string),
		roles:   make(map[string]string),
	}
}

func (m *mockAgents) StartAgent(taskID, role, mode, model, provider, prompt, dir string, allowedTools []string, needsWorktree, oneShot bool, outputSchema, cleanRetryRef string, assignment AgentAssignment) (agentID, startedDir, baselineRef string, err error) {
	if m.startEntered != nil {
		select {
		case m.startEntered <- struct{}{}:
		default:
		}
	}
	if m.startGate != nil {
		<-m.startGate
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.claimInsideStart {
		if m.dispatchClaimed == nil {
			m.dispatchClaimed = make(map[string]bool)
		}
		if m.dispatchClaimed[taskID] {
			return "", "", "", ErrDispatchInFlight
		}
		m.dispatchClaimed[taskID] = true
		defer delete(m.dispatchClaimed, taskID)
	}
	if m.failSpawnOnce != nil {
		err := m.failSpawnOnce
		m.failSpawnOnce = nil
		return "", "", "", err
	}
	if m.failSpawn != nil {
		return "", "", "", m.failSpawn
	}
	m.counter++
	id := fmt.Sprintf("agent-%d", m.counter)
	m.calls = append(m.calls, startCall{
		TaskID: taskID, Role: role, Mode: mode, Model: model, Provider: provider,
		Prompt: prompt, Dir: dir, AllowedTools: allowedTools,
		NeedsWorktree: needsWorktree, OneShot: oneShot, OutputSchema: outputSchema,
		CleanRetryRef: cleanRetryRef,
		Assignment:    assignment,
	})
	m.running[taskID] = id
	m.roles[taskID+"/"+role] = id
	startedDir = dir
	if startedDir == "" && needsWorktree {
		startedDir = filepath.Join(os.TempDir(), "sybra-test-"+taskID)
	}
	return id, startedDir, "base-" + id, nil
}

// SetFailSpawn arms the mock so the next StartAgent calls return err. Pass
// nil to disarm. Used to simulate spawn-time failures (e.g. unregistered
// project) that the workflow engine must handle without deadlocking.
func (m *mockAgents) SetFailSpawn(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failSpawn = err
}

func (m *mockAgents) HasRunningAgent(taskID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.running[taskID]
	return ok
}

func (m *mockAgents) HasOtherRunningAgentForTask(taskID, exceptAgentID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.running[taskID]
	return ok && id != exceptAgentID
}

func (m *mockAgents) ProviderRateLimited(provider string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rateLimited != nil {
		return m.rateLimited[provider]
	}
	return m.providerRateLimit
}

func (m *mockAgents) ProviderCanFailover(string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.providerFailover
}

func (m *mockAgents) ProviderHealthy(provider string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.unhealthy[provider]
}

// SetProviderUnhealthy marks a provider as unhealthy (e.g. config-disabled),
// simulating the health gate's IsHealthy returning false.
func (m *mockAgents) SetProviderUnhealthy(provider string, v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unhealthy == nil {
		m.unhealthy = make(map[string]bool)
	}
	m.unhealthy[provider] = v
}

func (m *mockAgents) SetProviderRateLimited(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.providerRateLimit = v
}

func (m *mockAgents) SetProviderRateLimitedFor(provider string, v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rateLimited == nil {
		m.rateLimited = make(map[string]bool)
	}
	m.rateLimited[provider] = v
}

func (m *mockAgents) SetProviderFailover(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.providerFailover = v
}

func (m *mockAgents) FindRunningAgentForRole(taskID, role string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.roles[taskID+"/"+role]
	return id, ok
}

func (m *mockAgents) StopAgentsForTask(taskID, _ string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.running, taskID)
}

func (m *mockAgents) SendPrompt(agentID, message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prompts = append(m.prompts, sentPrompt{AgentID: agentID, Message: message})
	return nil
}

func (m *mockAgents) DefaultProvider() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.defaultProvider != "" {
		return m.defaultProvider
	}
	return "claude"
}

func (m *mockAgents) SetDefaultProvider(p string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaultProvider = p
}

func (m *mockAgents) TryClaimDispatch(taskID string) (DispatchClaim, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dispatchClaimed == nil {
		m.dispatchClaimed = make(map[string]bool)
	}
	if m.dispatchClaimed[taskID] {
		return nil, false
	}
	m.dispatchClaimed[taskID] = true
	return mockDispatchClaim{m: m, taskID: taskID}, true
}

func (m *mockAgents) IsDispatching(taskID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dispatchClaimed[taskID]
}

func (m *mockAgents) AdmitDispatch(taskID, role, mode string) (admit bool, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.admitDenyReason == "" {
		return true, ""
	}
	return false, m.admitDenyReason
}

// SetAdmitDispatch arms the mock so AdmitDispatch denies with reason. Pass
// "" to disarm (the default: always admit).
func (m *mockAgents) SetAdmitDispatch(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.admitDenyReason = reason
}

type mockDispatchClaim struct {
	m      *mockAgents
	taskID string
}

func (c mockDispatchClaim) Release() {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	if c.m.dispatchClaimed != nil {
		delete(c.m.dispatchClaimed, c.taskID)
	}
}

// SetDispatchClaimed simulates an out-of-band dispatcher (e.g.
// recovery.RestartStaleInProgress) holding the shared agent.Manager
// dispatch claim for taskID, independent of this engine's own bookkeeping.
func (m *mockAgents) SetDispatchClaimed(taskID string, v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dispatchClaimed == nil {
		m.dispatchClaimed = make(map[string]bool)
	}
	m.dispatchClaimed[taskID] = v
}

// SimulateComplete marks the agent for a task as no longer running.
func (m *mockAgents) SimulateComplete(taskID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.running, taskID)
}

// LastCall returns the most recent StartAgent call.
func (m *mockAgents) LastCall() startCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		return startCall{}
	}
	return m.calls[len(m.calls)-1]
}

// LastID returns the most recent agent ID.
func (m *mockAgents) LastID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fmt.Sprintf("agent-%d", m.counter)
}

// RunningAgentID returns the agent ID currently assigned to a task.
func (m *mockAgents) RunningAgentID(taskID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running[taskID]
}

// CallCount returns total StartAgent calls.
func (m *mockAgents) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// SentPrompts returns all recorded SendPrompt calls.
func (m *mockAgents) SentPrompts() []sentPrompt {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]sentPrompt, len(m.prompts))
	copy(out, m.prompts)
	return out
}

// --- Tests ---

func TestFullLifecycle_DirectImplement(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})

	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Triage agent should have started.
	if agents.LastCall().Role != "triage" {
		t.Fatalf("expected triage, got %q", agents.LastCall().Role)
	}

	// Simulate triage completes — status stays "todo" → direct implement path.
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed", Output: "triaged"}); err != nil {
		t.Fatal(err)
	}

	// Should have advanced through set_in_progress → implement.
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Fatalf("expected in-progress, got %q", ti.Status)
	}
	if agents.LastCall().Role != "implementation" {
		t.Fatalf("expected implementation, got %q", agents.LastCall().Role)
	}

	// Simulate implement completes. The mechanical evaluate step runs
	// inline during AdvanceStep and terminates the workflow without
	// spawning a new agent.
	implCallCount := agents.CallCount()
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "implement", Status: "completed", Output: "Done.", AgentID: "agent-impl"}); err != nil {
		t.Fatal(err)
	}

	if got := agents.CallCount(); got != implCallCount {
		t.Errorf("evaluate spawned an agent (calls before=%d, after=%d) — should be mechanical", implCallCount, got)
	}

	// Workflow should fail closed; mechanical evaluate records a machine quarantine
	// and must not emit the ordinary completion cascade.
	ti, _ = tasks.GetTask("t1")
	if ti.Workflow.State != ExecFailed {
		t.Fatalf("expected failed quarantine, got %q (current step %q)", ti.Workflow.State, ti.Workflow.CurrentStep)
	}
	if ti.Status != "blocked" {
		t.Errorf("task status = %q, want blocked", ti.Status)
	}
	assertWorkflowMachineQuarantine(t, tasks, "t1", "workflow.evaluate_no_pr")
}

func TestResolveNext_BlockedTaskFailsWithoutCompletionOrNextStep(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: taskstatus.Blocked})
	engine := NewTestEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	current := &Step{ID: "quarantine", Next: []Transition{{GoTo: "erase_quarantine"}}}
	def := &Definition{ID: "blocked-terminal", Steps: []Step{
		*current,
		{ID: "erase_quarantine", Type: StepSetStatus, Config: StepConfig{Status: "in-progress"}},
	}}
	wfExec := &Execution{WorkflowID: def.ID, CurrentStep: current.ID, State: ExecRunning}

	next, completion, err := engine.resolveNext("t1", def, current, wfExec, TaskInfo{ID: "t1", Status: taskstatus.Blocked})
	if err != nil {
		t.Fatal(err)
	}
	if next != nil {
		t.Fatalf("next step = %q, want nil for blocked task", next.ID)
	}
	if completion != nil {
		t.Fatalf("completion = %+v, want nil for quarantined workflow", completion)
	}
	if wfExec.State != ExecFailed || wfExec.CurrentStep != "" || wfExec.CompletedAt == nil {
		t.Fatalf("workflow = %+v, want failed quarantine without next step", wfExec)
	}
}

// TestOneShot_ComputedFromStepConfig verifies that a legacy interactive step
// mode is coerced to headless by the engine and that no run_agent step is ever
// dispatched one-shot: interactive dispatch no longer exists, so a steerable
// headless run self-finalizes on its first completed turn (drainOrCloseHeadlessSteer)
// rather than needing OneShot to close stdin.
func TestOneShot_ComputedFromStepConfig(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	// Legacy interactive-mode task forces the templated implement step into
	// interactive mode via {{.Task.AgentMode}}, exercising the coercion path.
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "interactive"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Triage is headless → never one-shot.
	triageCall := agents.LastCall()
	if triageCall.Role != "triage" {
		t.Fatalf("expected triage, got %q", triageCall.Role)
	}
	if triageCall.OneShot {
		t.Errorf("triage (headless) should not be one-shot")
	}

	// Advance through triage → planning so plan fires.
	tasks.SetStatus("t1", "planning")
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed", Output: "plan please"}); err != nil {
		t.Fatal(err)
	}

	// Plan step's legacy interactive mode is coerced to headless; reuse_agent=true
	// keeps the steerable agent alive across turns, so it must NOT be one-shot.
	planCall := agents.LastCall()
	if planCall.Role != "plan" {
		t.Fatalf("expected plan, got %q", planCall.Role)
	}
	if planCall.Mode != "headless" {
		t.Fatalf("plan mode = %q, want headless", planCall.Mode)
	}
	if planCall.OneShot {
		t.Errorf("plan step has reuse_agent=true — must not be one-shot")
	}

	// Approve plan → set_in_progress → implement. The implement step resolves
	// its mode from the task's legacy interactive AgentMode, which is coerced
	// to headless. A headless run without reuse_agent / wait_for_status is not
	// one-shot — it self-finalizes on its first completed turn.
	tasks.SetStatus("t1", "plan-review")
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "plan", Status: "completed", Output: "plan ready"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.HandleHumanAction("t1", "approve", nil); err != nil {
		t.Fatal(err)
	}

	implCall := agents.LastCall()
	if implCall.Role != "implementation" {
		t.Fatalf("expected implementation, got %q", implCall.Role)
	}
	if implCall.Mode != "headless" {
		t.Fatalf("impl mode = %q, want headless", implCall.Mode)
	}
	if implCall.OneShot {
		t.Errorf("coerced-headless implement must not be one-shot — a steerable headless run self-finalizes")
	}
}

func TestFullLifecycle_PlanPath(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})

	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Triage completes, agent set status to "planning".
	tasks.SetStatus("t1", "planning")
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed", Output: "needs planning"}); err != nil {
		t.Fatal(err)
	}

	// Plan agent started. The plan step's legacy mode: interactive is coerced
	// to headless by resolveRunAgentMode — interactive dispatch no longer exists.
	if agents.LastCall().Role != "plan" {
		t.Fatalf("expected plan, got %q", agents.LastCall().Role)
	}
	if agents.LastCall().Mode != "headless" {
		t.Fatalf("expected headless, got %q", agents.LastCall().Mode)
	}

	// Plan agent completes → review_plan (wait_human).
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "plan", Status: "completed", Output: "plan ready"}); err != nil {
		t.Fatal(err)
	}

	ti, _ := tasks.GetTask("t1")
	if ti.Status != "plan-review" {
		t.Fatalf("expected plan-review, got %q", ti.Status)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Fatalf("expected waiting, got %q", ti.Workflow.State)
	}

	// Approve plan.
	if err := engine.HandleHumanAction("t1", "approve", nil); err != nil {
		t.Fatal(err)
	}

	// Should advance through set_in_progress → implement.
	ti, _ = tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Fatalf("expected in-progress, got %q", ti.Status)
	}
	if agents.LastCall().Role != "implementation" {
		t.Fatalf("expected implementation, got %q", agents.LastCall().Role)
	}

	// Implement → evaluate → done.
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "implement", Status: "completed", Output: "done"}); err != nil {
		t.Fatal(err)
	}
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "evaluate", Status: "completed", Output: "ok"}); err != nil {
		t.Fatal(err)
	}

	ti, _ = tasks.GetTask("t1")
	if ti.Workflow.State != ExecFailed {
		t.Fatalf("expected failed quarantine, got %q", ti.Workflow.State)
	}
}

func TestImplementRetry_SucceedsWithinBudget(t *testing.T) {
	_, tasks, agents, engine := startTestSimpleImplement(t)

	for range 2 {
		agentID := agents.RunningAgentID("t1")
		agents.SimulateComplete("t1")
		if err := engine.AdvanceStep("t1", StepOutput{
			StepID:  "implement",
			Status:  "failed",
			AgentID: agentID,
			Output:  "provider connection closed",
		}); err != nil {
			t.Fatal(err)
		}
	}

	if got := roleStartCount(agents, "implementation"); got != 3 {
		t.Fatalf("implementation calls after retries = %d, want 3", got)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status == "human-required" {
		t.Fatalf("status = %q, want retry to keep task active", ti.Status)
	}
	if ti.Workflow.CurrentStep != "implement" {
		t.Fatalf("current step = %q, want implement", ti.Workflow.CurrentStep)
	}

	agentID := agents.RunningAgentID("t1")
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{
		StepID:  "implement",
		Status:  "completed",
		AgentID: agentID,
		Output:  "done",
	}); err != nil {
		t.Fatal(err)
	}

	ti, _ = tasks.GetTask("t1")
	if ti.Workflow.CurrentStep == "implement" && ti.Workflow.State == ExecRunning {
		t.Fatal("implement success after retries did not advance")
	}
	if got := ti.Workflow.CountStep("implement"); got != 3 {
		t.Fatalf("implement records = %d, want 3", got)
	}
}

func TestImplementRetry_ExhaustedFallsThrough(t *testing.T) {
	_, tasks, agents, engine := startTestSimpleImplement(t)

	for range 3 {
		agentID := agents.RunningAgentID("t1")
		agents.SimulateComplete("t1")
		if err := engine.AdvanceStep("t1", StepOutput{
			StepID:  "implement",
			Status:  "failed",
			AgentID: agentID,
			Output:  "provider connection closed",
		}); err != nil {
			t.Fatal(err)
		}
	}

	if got := roleStartCount(agents, "implementation"); got != 3 {
		t.Fatalf("implementation calls = %d, want 3 total attempts", got)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.CurrentStep == "implement" && ti.Workflow.State == ExecRunning {
		t.Fatal("expected workflow to advance after retry exhaustion")
	}
	if got := ti.Workflow.CountStep("implement"); got != 3 {
		t.Fatalf("implement records = %d, want 3", got)
	}
}

func TestImplementRetry_SkipsAndQuarantinesWhenTaskAlreadyHumanRequired(t *testing.T) {
	_, tasks, agents, engine := startTestSimpleImplement(t)

	agentID := agents.RunningAgentID("t1")
	if err := tasks.UpdateTaskStatus("t1", "human-required", "watchdog escalation"); err != nil {
		t.Fatal(err)
	}
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{
		StepID:  "implement",
		Status:  "failed",
		AgentID: agentID,
		Output:  "agent stopped after watchdog escalation",
	}); err != nil {
		t.Fatal(err)
	}

	if got := roleStartCount(agents, "implementation"); got != 1 {
		t.Fatalf("implementation calls = %d, want no retry after human-required", got)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "blocked" {
		t.Fatalf("status = %q, want blocked machine quarantine", ti.Status)
	}
	assertWorkflowMachineQuarantine(t, tasks, "t1", "workflow.evaluate_no_pr")
	if ti.Workflow.State != ExecFailed {
		t.Fatalf("workflow state = %q, want failed quarantine after terminal transition", ti.Workflow.State)
	}
}

func startTestSimpleImplement(t *testing.T) (*Store, *memTasks, *mockAgents, *Engine) {
	t.Helper()
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed", Output: "triaged"}); err != nil {
		t.Fatal(err)
	}
	if got := agents.LastCall().Role; got != "implementation" {
		t.Fatalf("last agent role = %q, want implementation", got)
	}
	return store, tasks, agents, engine
}

const missingLivePRProofReason = "manual verification blocker: no current open PR could be verified as allFlaky for the required live pr-monitor proof"
const alreadyFixedOnMainVerdict = `{"decision":"already-fixed-on-main","reason":"landed in an earlier PR"}`

// alreadyFixedOnMainProse is the shape recovery used to act on. It must never
// close a task on its own now that a declaration is required.
const alreadyFixedOnMainProse = "Already fixed on main; no PR needed. Duplicate task, safe to close/mark done."

const alreadyFixedOnMainDeclaredReason = alreadyFixedOnMainRecoveryReason + " — agent declared: landed in an earlier PR"

// TestManualVerificationBlockerRecovery covers the missing-live-PR-proof
// recovery across both triggers (AdvanceStep and HandleStatusChange) and both
// outcomes (no PR found → ready-pr; PR found → link and go straight to
// in-review). Adding a new manual-verification-blocker case is one table
// entry instead of a copy-pasted ~90-line test function.
func TestManualVerificationBlockerRecovery(t *testing.T) {
	cases := []struct {
		name            string
		viaStatusChange bool
		prFound         bool
		wantStatus      taskstatus.Status
		wantReason      string
		wantPR          int
	}{
		{
			name:       "AdvanceStep with no PR routes to ready-pr",
			wantStatus: "ready-pr",
			wantReason: readyPRRecoveryReason,
		},
		{
			name:       "AdvanceStep with an existing PR links it and moves to in-review",
			prFound:    true,
			wantStatus: "in-review",
			wantPR:     42,
		},
		{
			name:            "HandleStatusChange with no PR routes to ready-pr",
			viaStatusChange: true,
			wantStatus:      "ready-pr",
			wantReason:      readyPRRecoveryReason,
		},
		{
			name:            "HandleStatusChange with an existing PR links it and moves to in-review",
			viaStatusChange: true,
			prFound:         true,
			wantStatus:      "in-review",
			wantPR:          42,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newInlineTestStore(t, "pr-recovery", `
id: pr-recovery
name: PR Recovery
trigger:
  on: task.created
steps:
  - id: implement
    name: Implement
    type: run_agent
    config:
      role: implementation
      mode: headless
      prompt: "implement"
    next:
      - when:
          field: task.status
          operator: equals
          value: human-required
        goto: ""
      - goto: verify
  - id: verify
    name: Verify
    type: set_status
    config:
      status: done
    next:
      - goto: ""
`)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewTestEngine(store, tasks, agents, discardLogger())
			var completed []CompletionInfo
			engine.SetOnComplete(func(info CompletionInfo) { completed = append(completed, info) })

			_, wtPath := newPRWorktree(t, "feat/live-proof")
			commitFile(t, wtPath, "proof.txt", "proof")
			runGit(t, wtPath, "push", "-u", "origin", "feat/live-proof")

			engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
			if tc.prFound {
				engine.SetPRFinder(&fakePRFinder{number: 42, found: true})
			}
			tasks.Put(TaskInfo{
				ID:        "t1",
				Status:    "in-progress",
				AgentMode: "headless",
				ProjectID: "acme/widgets",
				Branch:    "feat/live-proof",
			})
			if err := engine.StartWorkflow("t1", "pr-recovery"); err != nil {
				t.Fatal(err)
			}
			if err := tasks.UpdateTaskStatus("t1", "human-required", missingLivePRProofReason); err != nil {
				t.Fatal(err)
			}

			if tc.viaStatusChange {
				engine.HandleStatusChange("t1", "human-required")
			} else {
				agentID := agents.LastID()
				agents.SimulateComplete("t1")
				if err := engine.AdvanceStep("t1", StepOutput{
					StepID:  "implement",
					AgentID: agentID,
					Status:  "completed",
					Output:  missingLivePRProofReason,
				}); err != nil {
					t.Fatal(err)
				}
			}

			ti, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatal(err)
			}
			if ti.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", ti.Status, tc.wantStatus)
			}
			if ti.StatusReason != tc.wantReason {
				t.Fatalf("status reason = %q, want %q", ti.StatusReason, tc.wantReason)
			}
			if ti.PRNumber != tc.wantPR {
				t.Fatalf("pr number = %d, want %d", ti.PRNumber, tc.wantPR)
			}
			if ti.Workflow == nil || ti.Workflow.State != ExecCompleted || ti.Workflow.CurrentStep != "" {
				t.Fatalf("workflow = %+v, want completed terminal workflow", ti.Workflow)
			}
			if len(completed) != 1 || completed[0].WorkflowID != "pr-recovery" {
				t.Fatalf("completions = %+v, want one pr-recovery completion", completed)
			}
		})
	}
}

// alreadyFixedOnMainCase is one declarative entry in the already-fixed-on-main
// recovery scenario table: it sets up a task parked human-required with
// parkReason, drives it back through the pr-recovery workflow either via a
// completed run_agent output or a raw status-change re-probe, and asserts
// where it lands. Adding a new already-fixed-on-main case is one table entry
// instead of a copy-pasted ~90-line test function.
type alreadyFixedOnMainCase struct {
	name             string
	parkReason       string // defaults to alreadyFixedOnMainProse
	output           string // AdvanceStep's StepOutput.Output; ignored when viaStatusChange
	viaStatusChange  bool   // trigger via HandleStatusChange instead of AdvanceStep
	wantStatus       taskstatus.Status
	wantReason       string // exact expected status_reason
	wantReasonMaxLen int    // when >0, checked instead of wantReason (bounded-length reasons)
	wantCompleted    bool
}

func TestAlreadyFixedOnMainRecovery(t *testing.T) {
	long := strings.Repeat("x", 5000)
	duplicateReason := "need human decision about product direction"
	cases := []alreadyFixedOnMainCase{
		{
			name:          "declared verdict via AdvanceStep marks done",
			parkReason:    alreadyFixedOnMainVerdict,
			output:        alreadyFixedOnMainVerdict,
			wantStatus:    "done",
			wantReason:    alreadyFixedOnMainDeclaredReason,
			wantCompleted: true,
		},
		{
			name:       "affirmative prose undeclared stays human-required",
			output:     alreadyFixedOnMainProse,
			wantStatus: "human-required",
			wantReason: alreadyFixedOnMainProse,
		},
		{
			name:       "negated prose undeclared stays human-required",
			output:     "I checked whether this was already fixed on main; it is NOT. Ran out of context before committing, parking for a human.",
			wantStatus: "human-required",
			wantReason: alreadyFixedOnMainProse,
		},
		{
			name:       "incidental main.go mention undeclared stays human-required",
			output:     "Already fixed the nil deref in main.go but could not push.",
			wantStatus: "human-required",
			wantReason: alreadyFixedOnMainProse,
		},
		{
			name:       "verdict json quoted inside prose stays human-required",
			output:     `Per the contract I would emit {"decision": "already-fixed-on-main", "reason": "change already on base"} only if the work were landed. It is not, so I am NOT declaring that. No commits were made.`,
			wantStatus: "human-required",
			wantReason: alreadyFixedOnMainProse,
		},
		{
			name:       "verdict fence quoted and disclaimed stays human-required",
			output:     quotedFenceDisclaimed,
			wantStatus: "human-required",
			wantReason: alreadyFixedOnMainProse,
		},
		{
			// Pins the channel split: only the status reason declares. A
			// response that is nothing but the JSON object used to close a
			// task whose reason refused to declare one.
			name:       "response text is not a declaration channel",
			parkReason: "I am NOT declaring a recovery verdict. I made no commits and a human must review this.",
			output:     alreadyFixedOnMainVerdict,
			wantStatus: "human-required",
			wantReason: "I am NOT declaring a recovery verdict. I made no commits and a human must review this.",
		},
		{
			// Keeps an agent-authored reason out of the task frontmatter at
			// full length; the declaration lives in the status reason set
			// before recovery runs, not in this run's (empty) output.
			name:             "long declared reason is bounded",
			parkReason:       `{"decision":"already-fixed-on-main","reason":"` + long + `"}`,
			output:           "",
			wantStatus:       "done",
			wantReasonMaxLen: len(alreadyFixedOnMainRecoveryReason) + maxDeclaredReasonBytes + 64,
		},
		{
			// The run tried to declare and produced something unparseable:
			// it stays parked, and the board says why rather than only the
			// app log.
			name:       "unreadable declaration records reason",
			parkReason: `{"decision":"close-it"}`,
			output:     "",
			wantStatus: "human-required",
			wantReason: unreadableRecoveryVerdictReason,
		},
		{
			name:       "empty output stays human-required",
			output:     "",
			wantStatus: "human-required",
			wantReason: alreadyFixedOnMainProse,
		},
		{
			name:       "whitespace-only output stays human-required",
			output:     " \n\t ",
			wantStatus: "human-required",
			wantReason: alreadyFixedOnMainProse,
		},
		{
			name:            "declared verdict via HandleStatusChange marks done",
			parkReason:      alreadyFixedOnMainVerdict,
			viaStatusChange: true,
			wantStatus:      "done",
			wantReason:      alreadyFixedOnMainDeclaredReason,
			wantCompleted:   true,
		},
		{
			// An ordinary human-required park (not an already-fixed-on-main
			// signal) must never be treated as a duplicate-close: the
			// recovery requires the explicit duplicate signal, not just any
			// echoed reason.
			name:       "non-duplicate human-required reason requires duplicate signal",
			parkReason: duplicateReason,
			output:     duplicateReason,
			wantStatus: "human-required",
			wantReason: duplicateReason,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parkReason := tc.parkReason
			if parkReason == "" {
				parkReason = alreadyFixedOnMainProse
			}

			store := newInlineTestStore(t, "pr-recovery", `
id: pr-recovery
name: PR Recovery
trigger:
  on: task.created
steps:
  - id: implement
    name: Implement
    type: run_agent
    config:
      role: implementation
      mode: headless
      prompt: "implement"
    next:
      - when:
          field: task.status
          operator: equals
          value: human-required
        goto: ""
      - goto: verify
  - id: verify
    name: Verify
    type: set_status
    config:
      status: done
    next:
      - goto: ""
`)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewTestEngine(store, tasks, agents, discardLogger())
			engine.SetWorktreeGetter(&fakeWorktreeGetter{path: makeGitRepo(t, false), ok: true})
			var completed []CompletionInfo
			engine.SetOnComplete(func(info CompletionInfo) { completed = append(completed, info) })

			tasks.Put(TaskInfo{
				ID:        "t1",
				Status:    "in-progress",
				AgentMode: "headless",
				ProjectID: "acme/widgets",
			})
			if err := engine.StartWorkflow("t1", "pr-recovery"); err != nil {
				t.Fatal(err)
			}
			if err := tasks.UpdateTaskStatus("t1", "human-required", parkReason); err != nil {
				t.Fatal(err)
			}

			if tc.viaStatusChange {
				engine.HandleStatusChange("t1", "human-required")
			} else {
				agentID := agents.LastID()
				agents.SimulateComplete("t1")
				if err := engine.AdvanceStep("t1", StepOutput{
					StepID:  "implement",
					AgentID: agentID,
					Status:  "completed",
					Output:  tc.output,
				}); err != nil {
					t.Fatal(err)
				}
			}

			ti, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatal(err)
			}
			if ti.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", ti.Status, tc.wantStatus)
			}
			if tc.wantReasonMaxLen > 0 {
				if len(ti.StatusReason) > tc.wantReasonMaxLen {
					t.Fatalf("status reason is %d bytes, want the declared reason bounded to %d", len(ti.StatusReason), tc.wantReasonMaxLen)
				}
			} else if ti.StatusReason != tc.wantReason {
				t.Fatalf("status reason = %q, want %q", ti.StatusReason, tc.wantReason)
			}
			if ti.Workflow == nil || ti.Workflow.State != ExecCompleted || ti.Workflow.CurrentStep != "" {
				t.Fatalf("workflow = %+v, want completed terminal workflow", ti.Workflow)
			}
			// Only the "marks done" cases originally asserted on the
			// completion callback; the rest never registered on it.
			if tc.wantCompleted && (len(completed) != 1 || completed[0].WorkflowID != "pr-recovery") {
				t.Fatalf("completions = %+v, want one pr-recovery completion", completed)
			}
		})
	}
}

// TestRetry_SupersededAgentLateCompletionDropped reproduces the production bug
// where a stopped/superseded agent's late (double-delivered) completion was
// credited to the still-current step and burned its retry budget before the
// retry agent could produce a result. A headless test-runner that is
// interrupted can emit more than one terminal completion (e.g. an
// aborted_streaming result followed by a provider-error exit); both used to
// land on the run_test step and exhaust max_retries instantly.
//
// After the fix, launching the retry stops the original agent AND clears its
// step mapping, so its second completion is untracked and dropped by the
// phantom-completion guard rather than triggering a further retry.
func TestRetry_SupersededAgentLateCompletionDropped(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// First triage agent is dispatched and tracked by execRunAgent.
	agentA := agents.LastID()

	// Agent A fails once → workflow retries and dispatches agent B (still on the
	// triage step, max_retries: 3). Driving this through AdvanceStep (rather than
	// HandleAgentComplete) reproduces the production race: A's step mapping is
	// NOT yet cleared when its late completion arrives, because the two
	// completions run on different goroutines and HandleAgentComplete's trailing
	// clearAgentStep for the first hasn't executed yet.
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", AgentID: agentA, Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	agentB := agents.LastID()
	if agentB == agentA {
		t.Fatalf("retry did not dispatch a new agent: still %s", agentA)
	}
	if got := triageStartCount(agents); got != 2 {
		t.Fatalf("after first failure: triage calls = %d, want 2 (initial + 1 retry)", got)
	}

	// Agent A (now superseded/stopped) double-fires a second late completion
	// while its mapping is still live. This must be dropped, not counted as
	// another retry attempt against the step's retry budget.
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: agentA, Success: false})

	if got := triageStartCount(agents); got != 2 {
		t.Fatalf("after superseded late completion: triage calls = %d, want 2 — stale completion must not trigger another retry", got)
	}
	ti, _ := tasks.GetTask("t1")
	if got := ti.Workflow.CountStep("triage"); got != 1 {
		t.Errorf("CountStep(triage) = %d, want 1 — superseded completion must not be recorded", got)
	}
	if ti.Workflow.CurrentStep != "triage" {
		t.Errorf("current_step = %q, want triage — retry agent B should still own the step", ti.Workflow.CurrentStep)
	}
}

func triageStartCount(agents *mockAgents) int {
	return roleStartCount(agents, "triage")
}

func roleStartCount(agents *mockAgents, role string) int {
	agents.mu.Lock()
	defer agents.mu.Unlock()
	n := 0
	for i := range agents.calls {
		if agents.calls[i].Role == role {
			n++
		}
	}
	return n
}

func TestTriageRetryableReasonTreatsCompletedRunAsFailed(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	ti.StatusReason = triageRetryableStatusReasonPrefix + "classifier failed: exit status 1"
	tasks.Put(ti)

	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"}); err != nil {
		t.Fatal(err)
	}

	triageCalls := 0
	for _, c := range agents.calls {
		if c.Role == "triage" {
			triageCalls++
		}
	}
	if triageCalls != 2 {
		t.Fatalf("triage calls = %d, want retry to start a second triage agent", triageCalls)
	}
	ti, _ = tasks.GetTask("t1")
	if ti.Status == "human-required" {
		t.Fatal("retryable classifier failure escalated to human-required")
	}
	if ti.Workflow.CurrentStep != "triage" {
		t.Fatalf("current step = %q, want retrying triage", ti.Workflow.CurrentStep)
	}
}

func TestTriageRetryableExhaustionBlocksNonHuman(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	reason := triageRetryableStatusReasonPrefix + "classifier failed: provider unavailable"
	for range 4 {
		ti, err := tasks.GetTask("t1")
		if err != nil {
			t.Fatal(err)
		}
		ti.StatusReason = reason
		tasks.Put(ti)
		agents.SimulateComplete("t1")
		if err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"}); err != nil {
			t.Fatal(err)
		}
	}

	ti, _ := tasks.GetTask("t1")
	if ti.Status != "blocked" {
		t.Fatalf("status = %q, want blocked after retry exhaustion", ti.Status)
	}
	if ti.Status == "human-required" {
		t.Fatal("retryable classifier exhaustion escalated to human-required")
	}
	if ti.Workflow.State != ExecFailed {
		t.Fatalf("workflow state = %q, want failed", ti.Workflow.State)
	}
	if got := tasks.Reason("t1"); got != reason {
		t.Fatalf("reason = %q, want %q", got, reason)
	}
}

// addPREventWorkflow writes a minimal pr.event triggered workflow definition
// to the store with the given id, priority, and trigger value. All generated
// workflows share the same single run_agent step that reads its prompt from
// the "prompt" variable — enough to exercise dispatch and variable plumbing.
func addPREventWorkflow(t *testing.T, store *Store, id string, priority int, prIssueKind string) {
	t.Helper()
	def := Definition{
		ID:   id,
		Name: id,
		Trigger: Trigger{
			On:       "pr.event",
			Priority: priority,
			Conditions: []Condition{
				{Field: "pr.issue_kind", Operator: "equals", Value: prIssueKind},
			},
		},
		Steps: []Step{
			{
				ID:   "fix",
				Name: "Fix",
				Type: StepRunAgent,
				Config: StepConfig{
					Role:   "pr-fix",
					Mode:   "headless",
					Model:  "sonnet",
					Prompt: `{{ getvar .Vars "prompt" }}`,
				},
				Next: []Transition{{GoTo: ""}},
			},
		},
	}
	if err := store.Save(def); err != nil {
		t.Fatalf("save %s: %v", id, err)
	}
}

func TestResolveExecutionDefinition_UsesPinnedSnapshotOnLiveMismatch(t *testing.T) {
	t.Parallel()

	store := newInlineTestStore(t, "pin-test", `
id: pin-test
name: Pin Test
trigger:
  on: task.created
steps:
  - id: wait
    name: Wait Original
    type: wait_human
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine, err := NewEngine(store, tasks, agents, discardLogger(), completeDependencies())
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})
	if err := engine.StartWorkflow("t1", "pin-test"); err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	if err := store.Save(Definition{
		ID:   "pin-test",
		Name: "Pin Test",
		Trigger: Trigger{
			On: "task.created",
		},
		Steps: []Step{{
			ID:   "wait",
			Name: "Wait Updated",
			Type: StepWaitHuman,
		}},
	}); err != nil {
		t.Fatalf("Save updated workflow: %v", err)
	}

	ti, _ := tasks.GetTask("t1")
	def, err := engine.resolveExecutionDefinition("t1", ti)
	if err != nil {
		t.Fatalf("resolveExecutionDefinition: %v", err)
	}
	if step := def.StepByID("wait"); step == nil || step.Name != "Wait Original" {
		t.Fatalf("resolved step = %+v, want original snapshot content", step)
	}
}

func TestNoWorkflowField(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo"}) // no Workflow

	// Should not panic or error fatally.
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "agent-1", Result: "result", Success: true})
}

func TestEngineClockCanBeReplacedWhileRunning(t *testing.T) {
	t.Parallel()

	e := &Engine{}
	first := clock.NewFake(time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC))
	second := clock.NewFake(first.Now().Add(time.Hour))
	var wg sync.WaitGroup
	wg.Add(5)
	go func() {
		defer wg.Done()
		for i := range 1_000 {
			if i%2 == 0 {
				e.SetClock(first)
			} else {
				e.SetClock(second)
			}
		}
	}()
	for range 4 {
		go func() {
			defer wg.Done()
			for range 1_000 {
				if got := e.now(); got.IsZero() {
					t.Error("now returned the zero time during concurrent clock replacement")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestConcurrentAdvance(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	agents.SimulateComplete("t1")

	// Fire two concurrent AdvanceStep calls.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Go(func() {
			errs[i] = engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"})
		})
	}
	wg.Wait()

	// At least one should succeed, at most one should be skipped (nil error, no-op).
	// The engine's inflight guard prevents double-advance.
	successCount := 0
	for _, err := range errs {
		if err == nil {
			successCount++
		}
	}
	if successCount == 0 {
		t.Fatal("expected at least one successful advance")
	}
}

// demotionRecordHandler captures slog records for assertions, mirroring
// internal/sybra's recordHandler test helper.
type demotionRecordHandler struct {
	records *[]slog.Record
}

func (h *demotionRecordHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *demotionRecordHandler) Handle(_ context.Context, r slog.Record) error {
	*h.records = append(*h.records, r.Clone())
	return nil
}
func (h *demotionRecordHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *demotionRecordHandler) WithGroup(_ string) slog.Handler      { return h }

func recordAttr(r slog.Record, key string) string {
	var out string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			out = a.Value.String()
			return false
		}
		return true
	})
	return out
}

// --- HandleStatusChange + plan-reuse flow ---
//
// These tests cover the fix for interactive plan agents that never exit on
// their own: the workflow must advance from the run_agent step to the
// wait_human review step when the task status flips to the step's
// declared wait_for_status, and reject must re-enter the plan step via a
// set_status intermediate so the next plan-review transition can fire.

// startPlanReuseAtReviewPlan sets up a test-plan-reuse workflow, starts the
// plan agent, and drives it to the review_plan waiting state by flipping
// the task status to plan-review. Returns the configured engine/mocks for
// further assertions.
func startPlanReuseAtReviewPlan(t *testing.T) (*Engine, *memTasks, *mockAgents) {
	t.Helper()
	store := newTestStoreWith(t, "test-plan-reuse.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "planning", AgentMode: "interactive"})
	if err := engine.StartWorkflow("t1", "test-plan-reuse"); err != nil {
		t.Fatalf("start workflow: %v", err)
	}

	if got := agents.LastCall().Role; got != "plan" {
		t.Fatalf("expected plan agent started, got %q", got)
	}

	// Simulate the plan agent flipping the task status — this is what
	// the agent would do via `sybra-cli update --status plan-review`.
	tasks.SetStatus("t1", "plan-review")
	engine.HandleStatusChange("t1", "plan-review")

	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "review_plan" {
		t.Fatalf("expected review_plan after status advance, got %q", ti.Workflow.CurrentStep)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Fatalf("expected ExecWaiting at review_plan, got %q", ti.Workflow.State)
	}
	return engine, tasks, agents
}

func startAutoApprovePlanReview(t *testing.T, task TaskInfo) (*Engine, *memTasks) {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Definition{
		ID: "simple-task-plan",
		Steps: []Step{
			{
				ID:   "review_plan",
				Type: StepWaitHuman,
				Config: StepConfig{
					Status:       "plan-review",
					HumanActions: []string{"approve", "reject"},
				},
				Next: []Transition{{When: &Condition{Field: "vars.human_action", Operator: "equals", Value: "approve"}, GoTo: "done"}},
			},
			{
				ID:     "done",
				Type:   StepSetStatus,
				Config: StepConfig{Status: "in-progress"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	tasks.Put(task)
	return NewTestEngine(store, tasks, newMockAgents(), discardLogger()), tasks
}

func autoApproveReviewStep() *Step {
	return &Step{
		ID:   "review_plan",
		Type: StepWaitHuman,
		Config: StepConfig{
			Status:       "plan-review",
			HumanActions: []string{"approve", "reject"},
		},
	}
}

func waitForTaskStatus(t *testing.T, tasks *memTasks, id, want string) {
	t.Helper()
	if pollUntil(2*time.Second, 10*time.Millisecond, func() bool {
		ti, err := tasks.GetTask(id)
		return err == nil && ti.Status == taskstatus.Status(want)
	}) {
		return
	}
	ti, _ := tasks.GetTask(id)
	t.Fatalf("timed out waiting for status %q, got %q", want, ti.Status)
}

func assertRemainsPlanReviewWaiting(t *testing.T, tasks *memTasks, id string, window time.Duration) {
	t.Helper()
	check := func() bool {
		ti, err := tasks.GetTask(id)
		if err != nil {
			t.Fatal(err)
		}
		if ti.Status != "plan-review" || ti.Workflow == nil || ti.Workflow.State != ExecWaiting {
			t.Fatalf("task left plan-review wait state: status=%q workflow=%+v", ti.Status, ti.Workflow)
		}
		return false // never satisfied: keep polling for the full window
	}
	pollUntil(window, 10*time.Millisecond, check)
}

// --- ensure_pr_closes_issue step ---

// fakePRLinker is a scripted PRLinker used by executor tests.
type fakePRLinker struct {
	// getQueue yields successive GetClosingIssues results.
	getQueue []getResult
	getCalls int

	editErr   error
	editCalls int
	lastBody  string
}

type getResult struct {
	issues []int
	body   string
	err    error
}

func (f *fakePRLinker) GetClosingIssues(_ string, _ int) (issues []int, body string, err error) {
	idx := f.getCalls
	f.getCalls++
	if idx >= len(f.getQueue) {
		idx = len(f.getQueue) - 1
	}
	r := f.getQueue[idx]
	return r.issues, r.body, r.err
}

func (f *fakePRLinker) EditBody(_ string, _ int, body string) error {
	f.editCalls++
	f.lastBody = body
	return f.editErr
}

func newEnsurePRStep() *Step {
	return &Step{ID: "ensure", Type: StepEnsurePRClosesIssue}
}

type fakePRReviewRequester struct {
	reviewers       []string
	err             error
	copilotErr      error
	calls           int
	copilotCalls    int
	repo            string
	prNumber        int
	copilotRepo     string
	copilotPRNumber int
}

func (f *fakePRReviewRequester) RerequestReview(repo string, prNumber int) ([]string, error) {
	f.calls++
	f.repo = repo
	f.prNumber = prNumber
	return f.reviewers, f.err
}

func (f *fakePRReviewRequester) RequestCopilotReview(_ context.Context, repo string, prNumber int) error {
	f.copilotCalls++
	f.copilotRepo = repo
	f.copilotPRNumber = prNumber
	return f.copilotErr
}

func newRerequestReviewStep() *Step {
	return &Step{ID: "rerequest", Type: StepRerequestReview}
}

// --- stamp_pr_attribution step ---

func newStampPRStep() *Step {
	return &Step{ID: "stamp", Type: StepStampPRAttribution}
}

func TestEngineKeyedLocksReclaimBurst(t *testing.T) {
	t.Parallel()
	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Go(func() {
			key := fmt.Sprintf("task-%d", i)
			unlockInflight := engine.inflightLocks.LockLocal(key)
			unlockInflight()
			unlockRoute := engine.routeLocks.LockLocal(key)
			unlockRoute()
		})
	}
	wg.Wait()
	if got := engine.inflightLocks.Len(); got != 0 {
		t.Fatalf("inflight lock entries = %d, want burst tasks reclaimed", got)
	}
	if got := engine.routeLocks.Len(); got != 0 {
		t.Fatalf("route lock entries = %d, want burst tasks reclaimed", got)
	}
}

// --- verify_commits step ---

// fakeWorktreeGetter is a scripted WorktreeGetter for tests.
type fakeWorktreeGetter struct {
	path  string
	ok    bool
	paths map[string]string
}

func (f *fakeWorktreeGetter) GetWorktreePath(taskID string) (string, bool) {
	if f.paths != nil {
		path, ok := f.paths[taskID]
		return path, ok
	}
	return f.path, f.ok
}

func newVerifyCommitsStep() *Step {
	return &Step{ID: "verify", Type: StepVerifyCommits}
}

// makeGitRepo creates a bare-minimum git repo with an initial commit on main
// and optionally an extra commit on the current HEAD (simulating a task branch
// that is ahead of origin/main).
//
// Returns the worktree directory path.
func makeGitRepo(t *testing.T, withExtraCommit bool) string {
	t.Helper()
	dir := t.TempDir()
	remoteDir := filepath.Join(t.TempDir(), "origin.git")
	gitEnv := slices.DeleteFunc(slices.Clone(os.Environ()), func(s string) bool {
		return strings.HasPrefix(s, "GIT_OBJECT_DIRECTORY=") ||
			strings.HasPrefix(s, "GIT_ALTERNATE_OBJECT_DIRECTORIES=")
	})

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = gitEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	run("init", "-b", "main")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "Test")

	// Create initial commit so origin/main exists.
	f := filepath.Join(dir, "README.md")
	if err := os.WriteFile(f, []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "init")

	// Use the same bare-clone helper the app/worktree tests use; local Git on
	// this host rejects "repo points origin at itself" setups.
	if err := project.CloneBare(context.Background(), dir, remoteDir); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}
	runBare := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "safe.bareRepository=all", "-C", remoteDir}, args...)...)
		cmd.Env = gitEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runBare("config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	runBare("fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")
	run("remote", "add", "origin", remoteDir)
	run("fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")

	if withExtraCommit {
		f2 := filepath.Join(dir, "change.txt")
		if err := os.WriteFile(f2, []byte("change\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", "change.txt")
		run("commit", "-m", "feat: task work")
	}

	return dir
}

func runGitAt(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitCombinedAt(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

func gitCombinedAt(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-c", "safe.bareRepository=all"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func withFakeGit(t *testing.T, script string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath git: %v", err)
	}
	binDir := t.TempDir()
	fakeGit := filepath.Join(binDir, "git")
	script = strings.ReplaceAll(script, "{{REAL_GIT}}", realGit)
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestSurfaceInitialDispatchFailure_ReadFailureDoesNotWriteEmptyStatus covers
// the joint-failure edge: a store hiccup makes the current-status read fail at
// the same moment a dispatch fails. Without the read guard, a non-permanent
// classification would fall through with an empty target and push
// UpdateTaskStatus(id, "", reason) — rejected and swallowed under a misleading
// resume-stalled log while corrupting the task's status. The guard must skip
// the surface entirely and leave the task's status untouched.
func TestSurfaceInitialDispatchFailure_ReadFailureDoesNotWriteEmptyStatus(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", AgentMode: "headless"})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())

	tasks.SetFailGet(true)
	wf := &Execution{WorkflowID: "x", CurrentStep: "run_agent", State: ExecRunning}
	// A non-permanent error would target the (now unreadable) current status.
	engine.surfaceInitialDispatchFailure("t1", wf, "run_agent", fmt.Errorf("git fetch: timeout"))

	tasks.SetFailGet(false)
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != "in-progress" {
		t.Fatalf("status = %q, want unchanged in-progress (no empty-status write)", got.Status)
	}
}

// makeGitRepoBehindOrigin builds a worktree where HEAD is an ancestor of
// origin/main: origin/main points at commit B, HEAD is reset to commit A
// (a parent of B). `git log origin/main..HEAD` is empty AND HEAD != base.tip.
func makeGitRepoBehindOrigin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	run("init", "-b", "main")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "Test")

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-m", "A")
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "b.txt")
	run("commit", "-m", "B")

	// origin tracks current HEAD (commit B).
	run("remote", "add", "origin", dir)
	run("fetch", "origin")

	// Rewind HEAD to commit A — now origin/main is ahead of HEAD.
	run("reset", "--hard", "HEAD~1")

	return dir
}

// --- require_sidecar step ---

func newRequireSidecarStep(kind string) *Step {
	return &Step{
		ID:     "require",
		Type:   StepRequireSidecar,
		Config: StepConfig{Sidecar: kind},
	}
}

func newRequireSidecarAllowMissingStep(kind string) *Step {
	step := newRequireSidecarStep(kind)
	step.ID = "require_plan_critique"
	step.Config.AllowMissing = true
	return step
}

// --- flag_plan_critique step ---

func newFlagPlanCritiqueStep() *Step {
	return &Step{ID: "flag_plan_critique_verdict", Type: StepFlagPlanCritique}
}

// --- evaluate step ---

func newEvaluateStep() *Step {
	return &Step{ID: "evaluate", Type: StepEvaluate}
}

func TestLooksLikeTransientGitHub(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{"connection refused", "dial tcp: connection refused", true},
		{"connection reset", "read: connection reset by peer", true},
		{"dns failure", "could not resolve host: api.github.com", true},
		{"name resolution failure", "temporary failure in name resolution", true},
		{"network unreachable", "network is unreachable", true},
		{"i/o timeout", "context deadline exceeded (i/o timeout)", true},
		{"bare context deadline exceeded", "gh pr create: : context deadline exceeded", true},
		{"502", "502 Bad Gateway", true},
		{"503", "503 Service Unavailable", true},
		{"bare HTTP 502", "gh failed: HTTP 502", true},
		{"bare HTTP 503", "gh failed: HTTP 503", true},
		{"tls handshake", "remote error: tls: handshake failure", true},
		{"unrelated failure", "PR title does not follow conventional commit format", false},
		{"empty", "", false},
		{"rate limit alone handled elsewhere", "API rate limit exceeded", false},
		{"auth failure handled elsewhere", "gh: Bad credentials (HTTP 401)", false},
		{"bare dns mention is not a network error", "cannot title DNS cache fix", false},
		{"5-digit numeral containing 502 is not a gateway status", "task 150290 failed validation", false},
		{"letters around 502 are not a gateway status", "abc502def", false},
		{"status glued to HTTP token is not a gateway status", "gh failed: http502", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeTransientGitHub(tt.output); got != tt.want {
				t.Errorf("looksLikeTransientGitHub(%q) = %v, want %v", tt.output, got, tt.want)
			}
		})
	}
}

func TestLooksLikeAuthFailure(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{"bad credentials", "gh: Bad credentials (HTTP 401)", true},
		{"auth failed", "authentication failed for repository", true},
		{"github app token invalid", "X Failed to log in to github.com using token (GH_TOKEN)\n- The token in GH_TOKEN is invalid.", true},
		{"expired token", "fatal: token has expired", true},
		{"git https username prompt", "fatal: could not read Username for 'https://github.com': No such device or address", true},
		{"gh auth hint", "run gh auth login to authenticate", true},
		{"401", "401 Unauthorized", true},
		{"unrelated failure", "PR title does not follow conventional commit format", false},
		{"empty", "", false},
		{"network failure handled elsewhere", "dial tcp: connection refused", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeAuthFailure(tt.output); got != tt.want {
				t.Errorf("looksLikeAuthFailure(%q) = %v, want %v", tt.output, got, tt.want)
			}
		})
	}
}

func newStoreWithBuiltin(t *testing.T, id string) *Store {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	for i := range defs {
		if defs[i].ID == id {
			if err := store.Save(defs[i]); err != nil {
				t.Fatalf("Save(%s): %v", id, err)
			}
			return store
		}
	}
	t.Fatalf("builtin %s not found", id)
	return nil
}

func newEngineForEval(t *testing.T, tasks *memTasks) *Engine {
	t.Helper()
	store := newTestStore(t)
	agents := newMockAgents()
	return NewTestEngine(store, tasks, agents, discardLogger())
}

func newLinkPRStep() *Step {
	return &Step{ID: "link_pr_and_review", Type: StepLinkPRAndReview}
}

// fakePRExistenceChecker is a canned workflow.PRExistenceChecker for tests
// that need to control whether link_pr_and_review trusts task.pr_number.
type fakePRExistenceChecker struct {
	exists bool
	err    error
}

func (f fakePRExistenceChecker) PRExists(context.Context, string, int) (bool, error) {
	return f.exists, f.err
}

// The Set* methods run on the config-reload goroutine while dispatch reads the
// same fields. A -race run under concurrent reloads caught reviewLoopDisabled
// and reviewRoundsPerHour; maxTestAttempts and openPROnUnrunnableGate have the
// identical shape and were simply not hit by that workload, so all four are
// exercised here. Run under -race; plain fields fail.
func TestEngineGuardrails_ConcurrentSetAndRead(t *testing.T) {
	store := newTestStore(t)
	engine := NewTestEngine(store, newMemTasks(), newMockAgents(), discardLogger())

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 300 {
				engine.SetReviewUntilClean(j%2 == 0)
				engine.SetReviewRoundsPerHour(i + j)
				engine.SetTestingMaxAttempts(i + j)
				engine.SetOpenPROnUnrunnableGate(j%2 == 0)
			}
		})
	}
	for range 8 {
		wg.Go(func() {
			for range 300 {
				_ = engine.reviewLoopDisabled.Load()
				_ = engine.reviewRoundsPerHour.Load()
				_ = engine.maxTestAttempts.Load()
				_ = engine.openPROnUnrunnableGate.Load()
			}
		})
	}
	wg.Wait()
}

// A parked run_agent step always carries a completed step-action effect, so the
// replay no-op fires every maintenance tick for the whole park — the issue's
// own ~3,600-lines-per-task number, at INFO under a different message.
func TestReplayPersistedEffects_NoopIsThrottled(t *testing.T) {
	var records []slog.Record
	logger := slog.New(&demotionRecordHandler{records: &records})

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, logger)

	completed := time.Now().UTC()
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			StartedAt:   completed,
			EffectLog: []EffectRecord{{
				ID:          EffectID{Generation: 1, StepSeq: 0, StepID: "implement", Pos: effectPosStepAction},
				IntentAt:    completed,
				CompletedAt: &completed,
			}},
		},
	})

	noops := func() int {
		n := 0
		for _, r := range records {
			if r.Message == "workflow.effect-replay.noop" {
				n++
			}
		}
		return n
	}

	engine.ReplayPersistedEffects()
	if got := noops(); got != 1 {
		t.Fatalf("first replay logged %d no-ops, want 1", got)
	}

	records = nil
	for range 20 {
		engine.ReplayPersistedEffects()
	}
	if got := noops(); got != 0 {
		t.Errorf("got %d no-op records from repeat ticks, want 0", got)
	}

	// A different step is a different no-op. Keying the value on the task
	// alone would suppress it as a repeat of the one above.
	records = nil
	tasks.Put(TaskInfo{
		ID: "t1", Status: "in-progress", AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID: "test-simple", CurrentStep: "evaluate",
			State: ExecWaiting, StartedAt: completed,
			EffectLog: []EffectRecord{{
				ID:          EffectID{Generation: 1, StepSeq: 1, StepID: "evaluate", Pos: effectPosStepAction},
				IntentAt:    completed,
				CompletedAt: &completed,
			}},
		},
	})
	engine.ReplayPersistedEffects()
	if got := noops(); got != 1 {
		t.Errorf("no-op on a different step logged %d times, want 1", got)
	}
}

// Effect replay runs before ResumeStalled on every tick and can dispatch for
// real, ending a park. Once it does, ResumeStalled's own re-arm is unreachable
// — its preflight short-circuits on HasRunningAgent — so a later park on this
// task would be dropped rather than logged.
func TestReplayPersistedEffects_DispatchReArmsThePark(t *testing.T) {
	var records []slog.Record
	logger := slog.New(&demotionRecordHandler{records: &records})

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetProviderRateLimited(true)
	engine := NewTestEngine(store, tasks, agents, logger)

	put := func() {
		tasks.Put(TaskInfo{
			ID: "t1", Status: "in-progress", AgentMode: "headless",
			Workflow: &Execution{
				WorkflowID: "test-simple", CurrentStep: "implement",
				State: ExecWaiting, StartedAt: time.Now().UTC(),
				EffectLog: []EffectRecord{{
					ID:       EffectID{Generation: 1, StepSeq: 0, StepID: "implement", Pos: effectPosStepAction},
					IntentAt: time.Now().UTC(),
				}},
			},
		})
	}

	parks := func() int {
		n := 0
		for _, r := range records {
			if recordAttr(r, "reason") == "provider_rate_limited" && r.Level == slog.LevelInfo {
				n++
			}
		}
		return n
	}

	put()
	engine.ReplayPersistedEffects()
	if got := parks(); got != 1 {
		t.Fatalf("first park logged %d times, want 1", got)
	}

	// The limit lifts and replay dispatches for real, ending the park.
	agents.SetProviderRateLimited(false)
	before := agents.CallCount()
	put()
	engine.ReplayPersistedEffects()
	if agents.CallCount() == before {
		t.Fatal("replay never dispatched, so the park never ended")
	}
	agents.SimulateComplete("t1")

	records = nil
	agents.SetProviderRateLimited(true)
	put()
	engine.ReplayPersistedEffects()
	if got := parks(); got != 1 {
		t.Errorf("second park logged %d times, want 1: the replay route never re-armed", got)
	}
}
