package workflow

import (
	"cmp"
	"context"
	"errors"
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

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/attribution"
	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/dispatchorder"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/prompteval"
	providerpkg "github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/watchdogreason"
	"github.com/Automaat/sybra/internal/worktreeerr"
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
		engine := NewEngine(store, tasks, agents, discardLogger())

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
		engine := NewEngine(store, tasks, agents, discardLogger())

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
		engine := NewEngine(store, tasks, agents, discardLogger())

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
		engine := NewEngine(store, tasks, agents, discardLogger())

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
		engine := NewEngine(store, tasks, agents, discardLogger())

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
		engine := NewEngine(store, tasks, agents, discardLogger())

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
		engine := NewEngine(store, tasks, agents, discardLogger())

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
}

func TestResumeStalled_RecordsFallbackMetric(t *testing.T) {
	if !metrics.Enabled() {
		if err := metrics.Init(config.MetricsConfig{Enabled: true}); err != nil {
			t.Fatalf("metrics.Init: %v", err)
		}
	}

	before := workflowMetricValue(t, "sybra_orchestrator_resume_stalled_fallbacks_total", nil)

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
			},
		},
	}, gets: map[string]int{}}
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	engine.ResumeStalled()

	if len(agents.calls) != 1 {
		t.Fatalf("StartAgent calls = %d, want 1", len(agents.calls))
	}
	after := workflowMetricValue(t, "sybra_orchestrator_resume_stalled_fallbacks_total", nil)
	if after != before+1 {
		t.Fatalf("fallback metric delta = %.0f, want 1 (before=%.0f after=%.0f)", after-before, before, after)
	}
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
	steers           map[string]string
	gets             map[string]int
	onGet            func(id string, t *TaskInfo, count int)
	onSetWorkflow    func(id string)
	appendErr        error
	failGet          bool
	failSetWorkflow  bool
	failSetWorkflowN int
}

func newMemTasks() *memTasks {
	return &memTasks{
		tasks:   make(map[string]*TaskInfo),
		reasons: make(map[string]string),
		steers:  make(map[string]string),
		gets:    make(map[string]int),
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

func (m *memTasks) UpdateTaskStatus(id, status, reason string) error {
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

func (m *memTasks) ClearTaskStatusReasonIf(id, expectedStatus, expectedReason string) (bool, error) {
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

func (m *memTasks) UpdateTaskBlocker(id, status, reason string, state blocker.State) error {
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

func (m *memTasks) WriteSidecar(id, kind, content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
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
func (m *memTasks) SetStatus(id, status string) {
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

func (m *mockAgents) DefaultProvider() string { return "claude" }

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
	engine := NewEngine(store, tasks, agents, discardLogger())

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

	// Workflow should be completed; mechanical evaluate flips to human-required.
	ti, _ = tasks.GetTask("t1")
	if ti.Workflow.State != ExecCompleted {
		t.Fatalf("expected completed, got %q (current step %q)", ti.Workflow.State, ti.Workflow.CurrentStep)
	}
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
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
	engine := NewEngine(store, tasks, agents, discardLogger())

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
	engine := NewEngine(store, tasks, agents, discardLogger())

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
	if ti.Workflow.State != ExecCompleted {
		t.Fatalf("expected completed, got %q", ti.Workflow.State)
	}
}

func TestPlanReject_ThenApprove(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Triage → planning path.
	tasks.SetStatus("t1", "planning")
	agents.SimulateComplete("t1")
	_ = engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"})

	// Plan completes → wait_human.
	agents.SimulateComplete("t1")
	_ = engine.AdvanceStep("t1", StepOutput{StepID: "plan", Status: "completed"})

	// Reject with feedback.
	if err := engine.HandleHumanAction("t1", "reject", map[string]string{"feedback": "needs more detail"}); err != nil {
		t.Fatal(err)
	}

	// Should go back to plan step. Since reuse_agent=true and the plan agent
	// is still in the roles map, it should send a prompt instead of starting new.
	prompts := agents.SentPrompts()
	if len(prompts) == 0 {
		t.Fatal("expected SendPrompt to be called for reuse_agent")
	}
	if prompts[len(prompts)-1].Message == "" {
		t.Fatal("expected non-empty feedback message")
	}

	// Plan agent completes again → wait_human again.
	agents.SimulateComplete("t1")
	_ = engine.AdvanceStep("t1", StepOutput{StepID: "plan", Status: "completed"})

	// Now approve.
	if err := engine.HandleHumanAction("t1", "approve", nil); err != nil {
		t.Fatal(err)
	}

	if agents.LastCall().Role != "implementation" {
		t.Fatalf("expected implementation after approve, got %q", agents.LastCall().Role)
	}
}

func TestTriageRetry_Success(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Triage fails twice.
	for range 2 {
		agents.SimulateComplete("t1")
		if err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "failed"}); err != nil {
			t.Fatal(err)
		}
	}

	// Should have retried — 3 StartAgent calls total (1 initial + 2 retries).
	triageCalls := 0
	for _, c := range agents.calls {
		if c.Role == "triage" {
			triageCalls++
		}
	}
	if triageCalls != 3 {
		t.Fatalf("expected 3 triage calls, got %d", triageCalls)
	}

	// Third attempt succeeds.
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"}); err != nil {
		t.Fatal(err)
	}

	// Should advance to set_in_progress → implement.
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Fatalf("expected in-progress, got %q", ti.Status)
	}
}

func TestTriageRetry_Exhausted(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Fail 4 times (initial + 3 retries = 4 total, exceeds max_retries: 3).
	for range 4 {
		agents.SimulateComplete("t1")
		// Ignore errors — last one may fail transition resolution.
		_ = engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "failed"})
	}

	// After exhaustion, the transition should resolve (fallback goto: set_in_progress).
	ti, _ := tasks.GetTask("t1")
	// Workflow should have advanced past triage or failed.
	if ti.Workflow.CurrentStep == "triage" && ti.Workflow.State == ExecRunning {
		t.Fatal("expected workflow to advance past triage after retry exhaustion")
	}
}

func TestPlanningRetry_ExhaustedParksHumanRequired(t *testing.T) {
	store := newInlineTestStore(t, "simple-task-plan", `
id: simple-task-plan
name: test planning retry
trigger:
  on: task.created
steps:
  - id: plan
    type: run_agent
    config:
      role: plan
      max_retries: 1
    next:
      - goto: done
  - id: done
    type: set_status
    config:
      status: todo
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "planning", AgentMode: "headless"})
	if err := engine.StartWorkflowFromStepWithVars("t1", "simple-task-plan", "plan", nil); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		agents.SimulateComplete("t1")
		if err := engine.AdvanceStep("t1", StepOutput{StepID: "plan", Status: "failed", Output: "planner crashed"}); err != nil {
			t.Fatal(err)
		}
	}

	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", ti.Status)
	}
	if !strings.Contains(ti.StatusReason, "planning plan retry budget exhausted") ||
		!strings.Contains(ti.StatusReason, "planner crashed") {
		t.Fatalf("status_reason = %q, want retry exhaustion with output", ti.StatusReason)
	}
	if ti.Workflow == nil || ti.Workflow.State != ExecFailed || ti.Workflow.CurrentStep != "" {
		t.Fatalf("workflow = %+v, want failed terminal workflow", ti.Workflow)
	}
}

func TestResumeStalled_WatchdogHangRetriesThenEscalates(t *testing.T) {
	tests := []struct {
		name       string
		retries    string
		wantStarts int
		wantStatus string
		wantReason string
		wantRetry  string
		baseline   string
		wantClean  string
	}{
		{
			name:       "first hang retries",
			wantStarts: 1,
			wantStatus: "in-progress",
			wantRetry:  "1",
			baseline:   "abc123",
			wantClean:  "abc123",
		},
		{
			name:       "budget exhausted escalates",
			retries:    "2",
			wantStarts: 0,
			wantStatus: "human-required",
			wantReason: "watchdog hang: retry budget exhausted after 2 clean re-dispatches",
			wantRetry:  "2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewEngine(store, tasks, agents, discardLogger())
			vars := map[string]string{}
			if tc.retries != "" {
				vars[watchdogHangRetryKey("implement")] = tc.retries
			}
			if tc.baseline != "" {
				vars[tamperBaselineVar("implement")] = tc.baseline
			}
			tasks.Put(TaskInfo{
				ID:           "t1",
				Status:       "in-progress",
				StatusReason: "watchdog hang: no stream activity",
				AgentMode:    "headless",
				Workflow: &Execution{
					WorkflowID:  "test-simple",
					CurrentStep: "implement",
					State:       ExecWaiting,
					Variables:   vars,
					StartedAt:   time.Now().UTC(),
				},
			})

			engine.ResumeStalled()

			if got := agents.CallCount(); got != tc.wantStarts {
				t.Fatalf("StartAgent calls = %d, want %d", got, tc.wantStarts)
			}
			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.StatusReason != tc.wantReason {
				t.Fatalf("status_reason = %q, want %q", got.StatusReason, tc.wantReason)
			}
			if got.Workflow.Variables[watchdogHangRetryKey("implement")] != tc.wantRetry {
				t.Fatalf("hang retry var = %q, want %q", got.Workflow.Variables[watchdogHangRetryKey("implement")], tc.wantRetry)
			}
			if tc.wantStatus == "human-required" && got.Workflow.State != ExecFailed {
				t.Fatalf("workflow state = %q, want ExecFailed so the operator can re-dispatch", got.Workflow.State)
			}
			if tc.wantStarts > 0 {
				if got := agents.calls[0].CleanRetryRef; got != tc.wantClean {
					t.Fatalf("clean retry ref = %q, want %q", got, tc.wantClean)
				}
				if got.Workflow.Variables[watchdogHangCleanRetryKey("implement")] != "" {
					t.Fatalf("clean retry marker = %q, want cleared after dispatch", got.Workflow.Variables[watchdogHangCleanRetryKey("implement")])
				}
			}
		})
	}
}

func TestResumeStalled_WatchdogStopImplementationRetriesThenEscalates(t *testing.T) {
	tests := []struct {
		name       string
		reason     string
		retries    string
		wantStarts int
		wantStatus string
		wantReason string
		wantRetry  string
		baseline   string
		wantClean  string
	}{
		{
			name:       "structured first retry requeues implementation",
			reason:     "watchdog: loop stop: looping on toolchain setup",
			wantStarts: 1,
			wantStatus: "in-progress",
			wantRetry:  "1",
			baseline:   "abc123",
			wantClean:  "abc123",
		},
		{
			name:       "legacy first retry requeues implementation",
			reason:     "watchdog: looping on toolchain setup",
			wantStarts: 1,
			wantStatus: "in-progress",
			wantRetry:  "1",
			wantClean:  "HEAD",
		},
		{
			name:       "budget exhausted stays human required",
			reason:     "watchdog: loop stop: looping on toolchain setup",
			retries:    "2",
			wantStarts: 0,
			wantStatus: "human-required",
			wantReason: "watchdog stop: retry budget exhausted after 2 clean re-dispatches",
			wantRetry:  "2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewEngine(store, tasks, agents, discardLogger())
			vars := map[string]string{}
			if tc.retries != "" {
				vars[watchdogStopRetryKey("implement")] = tc.retries
			}
			if tc.baseline != "" {
				vars[tamperBaselineVar("implement")] = tc.baseline
			}
			tasks.Put(TaskInfo{
				ID:           "t1",
				Status:       "human-required",
				StatusReason: tc.reason,
				AgentMode:    "headless",
				Workflow: &Execution{
					WorkflowID:  "test-simple",
					CurrentStep: "implement",
					State:       ExecWaiting,
					Variables:   vars,
					StartedAt:   time.Now().UTC(),
				},
			})

			engine.ResumeStalled()

			if got := agents.CallCount(); got != tc.wantStarts {
				t.Fatalf("StartAgent calls = %d, want %d", got, tc.wantStarts)
			}
			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.StatusReason != tc.wantReason {
				t.Fatalf("status_reason = %q, want %q", got.StatusReason, tc.wantReason)
			}
			if got.Workflow.Variables[watchdogStopRetryKey("implement")] != tc.wantRetry {
				t.Fatalf("stop retry var = %q, want %q", got.Workflow.Variables[watchdogStopRetryKey("implement")], tc.wantRetry)
			}
			if tc.wantStatus == "human-required" && got.Workflow.State != ExecFailed {
				t.Fatalf("workflow state = %q, want ExecFailed after retry exhaustion", got.Workflow.State)
			}
			if tc.wantStarts > 0 {
				if got := agents.calls[0].CleanRetryRef; got != tc.wantClean {
					t.Fatalf("clean retry ref = %q, want %q", got, tc.wantClean)
				}
				if got.Workflow.Variables[watchdogHangCleanRetryKey("implement")] != "" {
					t.Fatalf("clean retry marker = %q, want cleared after dispatch", got.Workflow.Variables[watchdogHangCleanRetryKey("implement")])
				}
			}
		})
	}
}

func TestResumeStalled_WatchdogRewardHackingRetriesThenEscalates(t *testing.T) {
	tests := []struct {
		name       string
		role       string
		status     string
		stepID     string
		workflowID string
		retries    string
		plan       string
		wantStarts int
		wantStatus string
		wantReason string
		wantRetry  string
		wantClean  string
	}{
		{
			name:       "implementation retries from same worktree",
			role:       "implementation",
			status:     "in-progress",
			stepID:     "implement",
			workflowID: "test-simple",
			wantStarts: 1,
			wantStatus: "in-progress",
			wantRetry:  "1",
			wantClean:  "HEAD",
		},
		{
			name:       "planning retries when plan artifacts exist",
			role:       "plan",
			status:     "planning",
			stepID:     "plan",
			workflowID: "test-simple",
			plan:       "# Execution Plan\n\n- retry from existing artifacts",
			wantStarts: 1,
			wantStatus: "planning",
			wantRetry:  "1",
			wantClean:  "HEAD",
		},
		{
			name:       "implementation exhaustion escalates",
			role:       "implementation",
			status:     "in-progress",
			stepID:     "implement",
			workflowID: "test-simple",
			retries:    "2",
			wantStarts: 0,
			wantStatus: "human-required",
			wantReason: "watchdog: reward-hacking retry budget exhausted after 2 clean re-dispatches",
			wantRetry:  "2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewEngine(store, tasks, agents, discardLogger())
			vars := map[string]string{}
			if tc.retries != "" {
				vars[watchdogRewardHackingRetryKey(tc.stepID)] = tc.retries
			}
			tasks.Put(TaskInfo{
				ID:           "t1",
				Role:         tc.role,
				Status:       tc.status,
				StatusReason: "watchdog: reward-hacking retry: repeated search without editing",
				AgentMode:    "headless",
				Plan:         tc.plan,
				Workflow: &Execution{
					WorkflowID:  tc.workflowID,
					CurrentStep: tc.stepID,
					State:       ExecWaiting,
					Variables:   vars,
					StartedAt:   time.Now().UTC(),
				},
			})

			engine.ResumeStalled()

			if got := agents.CallCount(); got != tc.wantStarts {
				t.Fatalf("StartAgent calls = %d, want %d", got, tc.wantStarts)
			}
			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.StatusReason != tc.wantReason {
				t.Fatalf("status_reason = %q, want %q", got.StatusReason, tc.wantReason)
			}
			if got.Workflow.Variables[watchdogRewardHackingRetryKey(tc.stepID)] != tc.wantRetry {
				t.Fatalf("reward-hacking retry var = %q, want %q", got.Workflow.Variables[watchdogRewardHackingRetryKey(tc.stepID)], tc.wantRetry)
			}
			if tc.wantStatus == "human-required" && got.Workflow.State != ExecFailed {
				t.Fatalf("workflow state = %q, want ExecFailed after retry exhaustion", got.Workflow.State)
			}
			if tc.wantStarts > 0 {
				if got := agents.calls[0].CleanRetryRef; got != tc.wantClean {
					t.Fatalf("clean retry ref = %q, want %q", got, tc.wantClean)
				}
				if got.Workflow.Variables[watchdogHangCleanRetryKey(tc.stepID)] != "" {
					t.Fatalf("clean retry marker = %q, want cleared after dispatch", got.Workflow.Variables[watchdogHangCleanRetryKey(tc.stepID)])
				}
			}
		})
	}
}

func TestResumeStalled_WorktreeRepairRetriesThenExhausts(t *testing.T) {
	tests := []struct {
		name       string
		attempts   string
		wantStarts int
		wantStatus string
		wantRetry  string
		wantExh    bool
	}{
		{
			name:       "first attempt retries in place",
			wantStarts: 1,
			wantStatus: "in-progress",
			wantRetry:  "1",
		},
		{
			name:       "budget exhausted stays blocked and marks exhausted",
			attempts:   "2",
			wantStarts: 0,
			wantStatus: "blocked",
			wantRetry:  "2",
			wantExh:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewEngine(store, tasks, agents, discardLogger())
			vars := map[string]string{}
			if tc.attempts != "" {
				vars[worktreeRepairRetryKey("implement")] = tc.attempts
			}
			tasks.Put(TaskInfo{
				ID:           "t1",
				Status:       "blocked",
				StatusReason: worktreeerr.RebaseBlockedReason,
				AgentMode:    "headless",
				Blocker: blocker.State{
					Kind:       blocker.KindWorktreeRepair,
					Actor:      blocker.ActorWorkflow,
					Code:       "rebase_failed",
					NextAction: "repair_worktree",
				},
				Workflow: &Execution{
					WorkflowID:  "test-simple",
					CurrentStep: "implement",
					State:       ExecWaiting,
					Variables:   vars,
					StartedAt:   time.Now().UTC(),
				},
			})

			engine.ResumeStalled()

			if got := agents.CallCount(); got != tc.wantStarts {
				t.Fatalf("StartAgent calls = %d, want %d", got, tc.wantStarts)
			}
			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.Workflow.Variables[worktreeRepairRetryKey("implement")] != tc.wantRetry {
				t.Fatalf("retry var = %q, want %q", got.Workflow.Variables[worktreeRepairRetryKey("implement")], tc.wantRetry)
			}
			if got.Blocker.Exhausted != tc.wantExh {
				t.Fatalf("blocker.Exhausted = %v, want %v", got.Blocker.Exhausted, tc.wantExh)
			}
			if !tc.wantExh && !got.Blocker.IsZero() {
				t.Fatalf("blocker state should be cleared on successful retry, got %+v", got.Blocker)
			}
		})
	}
}

func TestResumeStalled_WatchdogStopVerifiedFailureDoesNotRetry(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "human-required",
		StatusReason: "watchdog: verify suite still fails after loop stop: go test ./cmd/sybra-cli\nFAIL",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			StartedAt:   time.Now().UTC(),
		},
	})

	engine.ResumeStalled()

	if got := agents.CallCount(); got != 0 {
		t.Fatalf("StartAgent calls = %d, want 0 for verified blocker", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if got.StatusReason == "" || !strings.Contains(got.StatusReason, "verify suite still fails") {
		t.Fatalf("status_reason = %q, want verified-failure reason preserved", got.StatusReason)
	}
}

func TestResumeStalled_WatchdogStopRateLimitedProviderKeepsHumanRequired(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetProviderRateLimited(true)
	engine := NewEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "human-required",
		StatusReason: "watchdog: loop stop: looping on toolchain setup",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			StartedAt:   time.Now().UTC(),
		},
	})

	engine.ResumeStalled()

	if got := agents.CallCount(); got != 0 {
		t.Fatalf("StartAgent calls = %d, want 0 while provider is rate-limited", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if got.StatusReason != "watchdog: loop stop: looping on toolchain setup" {
		t.Fatalf("status_reason = %q, want retryable stop marker preserved", got.StatusReason)
	}
	if got.Workflow.Variables[watchdogStopRetryKey("implement")] != "" {
		t.Fatalf("stop retry var = %q, want empty when dispatch is skipped", got.Workflow.Variables[watchdogStopRetryKey("implement")])
	}
}

func TestResumeStalled_WatchdogHangExhaustedRunTestOpensPR(t *testing.T) {
	store := newInlineTestStore(t, "testing-task", `
id: testing-task
steps:
  - id: run_test
    type: run_agent
    config:
      role: test-runner
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	var completed []CompletionInfo
	engine.SetOnComplete(func(info CompletionInfo) { completed = append(completed, info) })
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "testing",
		StatusReason: "watchdog hang: no stream activity",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "testing-task",
			CurrentStep: "run_test",
			State:       ExecWaiting,
			Variables: map[string]string{
				watchdogHangRetryKey("run_test"): strconv.Itoa(maxWatchdogHangRetries),
			},
			StartedAt: time.Now().UTC(),
		},
	})

	engine.ResumeStalled()

	if got := agents.CallCount(); got != 0 {
		t.Fatalf("StartAgent calls = %d, want 0", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "ready-pr" {
		t.Fatalf("status = %q, want ready-pr", got.Status)
	}
	if got.StatusReason != "manual testing gate could not be run after auto-retries (harness/infra limitation, not a product defect) — opening PR for CI and human review" {
		t.Fatalf("status_reason = %q", got.StatusReason)
	}
	if got.Workflow.State != ExecCompleted {
		t.Fatalf("workflow state = %q, want %q", got.Workflow.State, ExecCompleted)
	}
	if got.Workflow.CurrentStep != "" {
		t.Fatalf("workflow current_step = %q, want empty", got.Workflow.CurrentStep)
	}
	if len(completed) != 1 {
		t.Fatalf("workflow completions = %d, want 1", len(completed))
	}
	if completed[0].WorkflowID != "testing-task" {
		t.Fatalf("completion workflow = %q, want testing-task", completed[0].WorkflowID)
	}
}

func TestResumeStalled_WatchdogHangExhaustedRunTestHumanRequiredWhenOpenPRDisabled(t *testing.T) {
	store := newInlineTestStore(t, "testing-task", `
id: testing-task
steps:
  - id: run_test
    type: run_agent
    config:
      role: test-runner
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	engine.SetOpenPROnUnrunnableGate(false)
	var completed []CompletionInfo
	engine.SetOnComplete(func(info CompletionInfo) { completed = append(completed, info) })
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "testing",
		StatusReason: "watchdog hang: no stream activity",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "testing-task",
			CurrentStep: "run_test",
			State:       ExecWaiting,
			Variables: map[string]string{
				watchdogHangRetryKey("run_test"): strconv.Itoa(maxWatchdogHangRetries),
			},
			StartedAt: time.Now().UTC(),
		},
	})

	engine.ResumeStalled()

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if got.Workflow.State != ExecFailed {
		t.Fatalf("workflow state = %q, want %q", got.Workflow.State, ExecFailed)
	}
	if len(completed) != 0 {
		t.Fatalf("workflow completions = %d, want 0", len(completed))
	}
}

func TestResumeStalled_TransientFetchRetriesThenEscalates(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetFailSpawn(worktreeerr.ErrTransientFetch)
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: transientFetchStatusReason,
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   map[string]string{},
			StartedAt:   time.Now().UTC(),
		},
	})

	for attempt := 1; attempt <= maxTransientFetchRetries; attempt++ {
		engine.ResumeStalled()
		got, err := tasks.GetTask("t1")
		if err != nil {
			t.Fatalf("get task after attempt %d: %v", attempt, err)
		}
		if got.Status != "in-progress" {
			t.Fatalf("attempt %d status = %q, want in-progress", attempt, got.Status)
		}
		if got.Workflow.Variables[transientFetchRetryKey("implement")] != strconv.Itoa(attempt) {
			t.Fatalf("attempt %d retry var = %q, want %q", attempt, got.Workflow.Variables[transientFetchRetryKey("implement")], strconv.Itoa(attempt))
		}
		if got.StatusReason != transientFetchStatusReason {
			t.Fatalf("attempt %d status_reason = %q, want %q", attempt, got.StatusReason, transientFetchStatusReason)
		}
	}

	engine.ResumeStalled()

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required after retry exhaustion", got.Status)
	}
	wantReason := "agent start blocked: transient network retry budget exhausted after 2 attempts reconciling worktree with remote"
	if got.StatusReason != wantReason {
		t.Fatalf("status_reason = %q, want %q", got.StatusReason, wantReason)
	}
	if got.Workflow.Variables[transientFetchRetryKey("implement")] != strconv.Itoa(maxTransientFetchRetries) {
		t.Fatalf("retry var = %q, want %d", got.Workflow.Variables[transientFetchRetryKey("implement")], maxTransientFetchRetries)
	}
}

func TestResumeStalled_WatchdogHangDoesNotBurnBudgetWhileAgentRunning(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "watchdog hang: no stream activity",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   map[string]string{},
			StartedAt:   time.Now().UTC(),
		},
	})
	if _, _, _, err := agents.StartAgent("t1", "implementation", "headless", "sonnet", "", "p", "", nil, false, false, "", "", AgentAssignment{}); err != nil {
		t.Fatal(err)
	}

	engine.ResumeStalled()

	if got := agents.CallCount(); got != 1 {
		t.Fatalf("StartAgent calls = %d, want only the pre-existing running agent", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Workflow.Variables[watchdogHangRetryKey("implement")] != "" {
		t.Fatalf("hang retry var = %q, want empty while agent is still running", got.Workflow.Variables[watchdogHangRetryKey("implement")])
	}
	if got.StatusReason != "watchdog hang: no stream activity" {
		t.Fatalf("status_reason = %q, want marker preserved until no agent is running", got.StatusReason)
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

func TestImplementRetry_SkipsWhenTaskAlreadyHumanRequired(t *testing.T) {
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
	if ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", ti.Status)
	}
	if ti.Workflow.State != ExecCompleted {
		t.Fatalf("workflow state = %q, want completed after terminal transition", ti.Workflow.State)
	}
}

func startTestSimpleImplement(t *testing.T) (*Store, *memTasks, *mockAgents, *Engine) {
	t.Helper()
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

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
const alreadyFixedOnMainVerdict = "Already fixed on main; no PR needed. Duplicate task, safe to close/mark done."

func TestAdvanceStep_ManualVerificationBlockerRoutesToReadyPR(t *testing.T) {
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
	engine := NewEngine(store, tasks, agents, discardLogger())
	var completed []CompletionInfo
	engine.SetOnComplete(func(info CompletionInfo) { completed = append(completed, info) })

	_, wtPath := newPRWorktree(t, "feat/live-proof")
	commitFile(t, wtPath, "proof.txt", "proof")
	runGit(t, wtPath, "push", "-u", "origin", "feat/live-proof")

	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
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

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.Status != "ready-pr" {
		t.Fatalf("status = %q, want ready-pr", ti.Status)
	}
	if ti.StatusReason != readyPRRecoveryReason {
		t.Fatalf("status reason = %q, want %q", ti.StatusReason, readyPRRecoveryReason)
	}
	if ti.PRNumber != 0 {
		t.Fatalf("pr number = %d, want 0", ti.PRNumber)
	}
	if ti.Workflow == nil || ti.Workflow.State != ExecCompleted {
		t.Fatalf("workflow = %+v, want completed", ti.Workflow)
	}
	if ti.Workflow.CurrentStep != "" {
		t.Fatalf("current step = %q, want empty", ti.Workflow.CurrentStep)
	}
	if len(completed) != 1 || completed[0].WorkflowID != "pr-recovery" {
		t.Fatalf("completions = %+v, want one pr-recovery completion", completed)
	}
}

func TestAdvanceStep_AlreadyFixedOnMainMarksDone(t *testing.T) {
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
	engine := NewEngine(store, tasks, agents, discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: makeGitRepo(t, false), ok: true})
	var completed []CompletionInfo
	engine.SetOnComplete(func(info CompletionInfo) { completed = append(completed, info) })

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		AgentMode:    "headless",
		ProjectID:    "acme/widgets",
		StatusReason: "",
	})
	if err := engine.StartWorkflow("t1", "pr-recovery"); err != nil {
		t.Fatal(err)
	}
	if err := tasks.UpdateTaskStatus("t1", "human-required", alreadyFixedOnMainVerdict); err != nil {
		t.Fatal(err)
	}

	agentID := agents.LastID()
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{
		StepID:  "implement",
		AgentID: agentID,
		Status:  "completed",
		Output:  alreadyFixedOnMainVerdict,
	}); err != nil {
		t.Fatal(err)
	}

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.Status != "done" {
		t.Fatalf("status = %q, want done", ti.Status)
	}
	if ti.StatusReason != alreadyFixedOnMainRecoveryReason {
		t.Fatalf("status reason = %q, want %q", ti.StatusReason, alreadyFixedOnMainRecoveryReason)
	}
	if ti.Workflow == nil || ti.Workflow.State != ExecCompleted || ti.Workflow.CurrentStep != "" {
		t.Fatalf("workflow = %+v, want completed terminal workflow", ti.Workflow)
	}
	if len(completed) != 1 || completed[0].WorkflowID != "pr-recovery" {
		t.Fatalf("completions = %+v, want one pr-recovery completion", completed)
	}
}

func TestAdvanceStep_AlreadyFixedOnMainEmptyOutputStaysHumanRequired(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
	}{
		{name: "empty", output: ""},
		{name: "whitespace", output: " \n\t "},
	} {
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
			engine := NewEngine(store, tasks, agents, discardLogger())
			engine.SetWorktreeGetter(&fakeWorktreeGetter{path: makeGitRepo(t, false), ok: true})

			tasks.Put(TaskInfo{
				ID:        "t1",
				Status:    "in-progress",
				AgentMode: "headless",
				ProjectID: "acme/widgets",
			})
			if err := engine.StartWorkflow("t1", "pr-recovery"); err != nil {
				t.Fatal(err)
			}
			if err := tasks.UpdateTaskStatus("t1", "human-required", alreadyFixedOnMainVerdict); err != nil {
				t.Fatal(err)
			}

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

			ti, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatal(err)
			}
			if ti.Status != "human-required" {
				t.Fatalf("status = %q, want human-required", ti.Status)
			}
			if ti.StatusReason != alreadyFixedOnMainVerdict {
				t.Fatalf("status reason = %q, want stale human-required reason preserved", ti.StatusReason)
			}
			if ti.Workflow == nil || ti.Workflow.State != ExecCompleted || ti.Workflow.CurrentStep != "" {
				t.Fatalf("workflow = %+v, want completed terminal workflow", ti.Workflow)
			}
		})
	}
}

func TestHandleStatusChange_AlreadyFixedOnMainMarksDone(t *testing.T) {
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
	engine := NewEngine(store, tasks, agents, discardLogger())
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
	if err := tasks.UpdateTaskStatus("t1", "human-required", alreadyFixedOnMainVerdict); err != nil {
		t.Fatal(err)
	}

	engine.HandleStatusChange("t1", "human-required")

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.Status != "done" {
		t.Fatalf("status = %q, want done", ti.Status)
	}
	if ti.StatusReason != alreadyFixedOnMainRecoveryReason {
		t.Fatalf("status reason = %q, want %q", ti.StatusReason, alreadyFixedOnMainRecoveryReason)
	}
	if ti.Workflow == nil || ti.Workflow.State != ExecCompleted || ti.Workflow.CurrentStep != "" {
		t.Fatalf("workflow = %+v, want completed terminal workflow", ti.Workflow)
	}
	if len(completed) != 1 || completed[0].WorkflowID != "pr-recovery" {
		t.Fatalf("completions = %+v, want one pr-recovery completion", completed)
	}
}

func TestAdvanceStep_AlreadyFixedOnMainRequiresDuplicateSignal(t *testing.T) {
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
	engine := NewEngine(store, tasks, agents, discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: makeGitRepo(t, false), ok: true})

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		ProjectID: "acme/widgets",
	})
	if err := engine.StartWorkflow("t1", "pr-recovery"); err != nil {
		t.Fatal(err)
	}
	reason := "need human decision about product direction"
	if err := tasks.UpdateTaskStatus("t1", "human-required", reason); err != nil {
		t.Fatal(err)
	}

	agentID := agents.LastID()
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{
		StepID:  "implement",
		AgentID: agentID,
		Status:  "completed",
		Output:  reason,
	}); err != nil {
		t.Fatal(err)
	}

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", ti.Status)
	}
	if ti.StatusReason != reason {
		t.Fatalf("status reason = %q, want %q", ti.StatusReason, reason)
	}
	if ti.Workflow == nil || ti.Workflow.State != ExecCompleted || ti.Workflow.CurrentStep != "" {
		t.Fatalf("workflow = %+v, want completed terminal workflow", ti.Workflow)
	}
}

func TestAdvanceStep_ManualVerificationBlockerLinksExistingPR(t *testing.T) {
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
	engine := NewEngine(store, tasks, agents, discardLogger())
	var completed []CompletionInfo
	engine.SetOnComplete(func(info CompletionInfo) { completed = append(completed, info) })

	_, wtPath := newPRWorktree(t, "feat/live-proof")
	commitFile(t, wtPath, "proof.txt", "proof")
	runGit(t, wtPath, "push", "-u", "origin", "feat/live-proof")

	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	engine.SetPRFinder(&fakePRFinder{number: 42, found: true})
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

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.Status != "in-review" {
		t.Fatalf("status = %q, want in-review", ti.Status)
	}
	if ti.StatusReason != "" {
		t.Fatalf("status reason = %q, want empty", ti.StatusReason)
	}
	if ti.PRNumber != 42 {
		t.Fatalf("pr number = %d, want 42", ti.PRNumber)
	}
	if ti.Workflow == nil || ti.Workflow.State != ExecCompleted {
		t.Fatalf("workflow = %+v, want completed", ti.Workflow)
	}
	if len(completed) != 1 || completed[0].WorkflowID != "pr-recovery" {
		t.Fatalf("completions = %+v, want one pr-recovery completion", completed)
	}
}

func TestHandleStatusChange_ManualVerificationBlockerRoutesToReadyPR(t *testing.T) {
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
	engine := NewEngine(store, tasks, agents, discardLogger())
	var completed []CompletionInfo
	engine.SetOnComplete(func(info CompletionInfo) { completed = append(completed, info) })

	_, wtPath := newPRWorktree(t, "feat/live-proof")
	commitFile(t, wtPath, "proof.txt", "proof")
	runGit(t, wtPath, "push", "-u", "origin", "feat/live-proof")

	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
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

	engine.HandleStatusChange("t1", "human-required")

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.Status != "ready-pr" {
		t.Fatalf("status = %q, want ready-pr", ti.Status)
	}
	if ti.StatusReason != readyPRRecoveryReason {
		t.Fatalf("status reason = %q, want %q", ti.StatusReason, readyPRRecoveryReason)
	}
	if ti.PRNumber != 0 {
		t.Fatalf("pr number = %d, want 0", ti.PRNumber)
	}
	if ti.Workflow == nil || ti.Workflow.State != ExecCompleted {
		t.Fatalf("workflow = %+v, want completed", ti.Workflow)
	}
	if len(completed) != 1 || completed[0].WorkflowID != "pr-recovery" {
		t.Fatalf("completions = %+v, want one pr-recovery completion", completed)
	}
}

func TestHandleStatusChange_ManualVerificationBlockerLinksExistingPR(t *testing.T) {
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
	engine := NewEngine(store, tasks, agents, discardLogger())
	var completed []CompletionInfo
	engine.SetOnComplete(func(info CompletionInfo) { completed = append(completed, info) })

	_, wtPath := newPRWorktree(t, "feat/live-proof")
	commitFile(t, wtPath, "proof.txt", "proof")
	runGit(t, wtPath, "push", "-u", "origin", "feat/live-proof")

	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	engine.SetPRFinder(&fakePRFinder{number: 42, found: true})
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

	engine.HandleStatusChange("t1", "human-required")

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.Status != "in-review" {
		t.Fatalf("status = %q, want in-review", ti.Status)
	}
	if ti.StatusReason != "" {
		t.Fatalf("status reason = %q, want empty", ti.StatusReason)
	}
	if ti.PRNumber != 42 {
		t.Fatalf("pr number = %d, want 42", ti.PRNumber)
	}
	if ti.Workflow == nil || ti.Workflow.State != ExecCompleted {
		t.Fatalf("workflow = %+v, want completed", ti.Workflow)
	}
	if len(completed) != 1 || completed[0].WorkflowID != "pr-recovery" {
		t.Fatalf("completions = %+v, want one pr-recovery completion", completed)
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
	engine := NewEngine(store, tasks, agents, discardLogger())

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
	engine := NewEngine(store, tasks, agents, discardLogger())

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
	engine := NewEngine(store, tasks, agents, discardLogger())

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

func TestMatchWorkflow_ReviewTag(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	// Task WITH review tag should NOT match test-simple.
	review := TaskInfo{ID: "t1", Tags: []string{"review"}}
	if def := engine.MatchWorkflow(review, "task.created"); def != nil {
		t.Fatalf("expected no match for review tag, got %s", def.ID)
	}

	// Task WITHOUT review tag should match.
	normal := TaskInfo{ID: "t2", Tags: []string{"backend"}}
	if def := engine.MatchWorkflow(normal, "task.created"); def == nil {
		t.Fatal("expected match for normal task")
	}
}

func TestMatchWorkflow_NoMatch(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	// Wrong event type.
	normal := TaskInfo{ID: "t1"}
	if def := engine.MatchWorkflow(normal, "pr.event"); def != nil {
		t.Fatalf("expected no match for pr.event, got %s", def.ID)
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

func TestDispatchEvent_MatchesAndStarts(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	addPREventWorkflow(t, store, "pr-fix-test", 0, "ci_failure")
	tasks.Put(TaskInfo{ID: "t1", Status: "in-review", AgentMode: "headless"})

	wfID, err := engine.DispatchEvent("t1", "pr.event",
		map[string]string{"pr.issue_kind": "ci_failure"},
		map[string]string{"prompt": "fix the thing"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if wfID != "pr-fix-test" {
		t.Fatalf("wfID = %q, want pr-fix-test", wfID)
	}
	if agents.CallCount() != 1 {
		t.Fatalf("expected 1 agent call, got %d", agents.CallCount())
	}
	if got := agents.LastCall().Prompt; got != "fix the thing" {
		t.Errorf("prompt = %q, want 'fix the thing'", got)
	}
}

func TestDispatchEvent_NoMatchReturnsEmpty(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	addPREventWorkflow(t, store, "pr-fix-test", 0, "ci_failure")
	tasks.Put(TaskInfo{ID: "t1", Status: "in-review"})

	// Extra fields miss the condition (kind=conflict, workflow wants ci_failure).
	wfID, err := engine.DispatchEvent("t1", "pr.event",
		map[string]string{"pr.issue_kind": "conflict"}, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if wfID != "" {
		t.Fatalf("wfID = %q, want empty", wfID)
	}
	if agents.CallCount() != 0 {
		t.Fatalf("expected no agent calls, got %d", agents.CallCount())
	}
}

func TestDispatchEvent_AlreadyActiveRejected(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	addPREventWorkflow(t, store, "pr-fix-test", 0, "ci_failure")
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "simple-task-plan",
			CurrentStep: "implement",
			State:       ExecWaiting,
		},
	})

	_, err := engine.DispatchEvent("t1", "pr.event",
		map[string]string{"pr.issue_kind": "ci_failure"}, nil)
	if !errors.Is(err, ErrWorkflowAlreadyActive) {
		t.Fatalf("expected ErrWorkflowAlreadyActive, got %v", err)
	}
	if agents.CallCount() != 0 {
		t.Fatalf("expected no agent start on rejected dispatch, got %d", agents.CallCount())
	}
}

func TestDispatchEvent_TerminalWorkflowReplaced(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	addPREventWorkflow(t, store, "pr-fix-test", 0, "ci_failure")
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "in-review",
		Workflow: &Execution{
			WorkflowID:  "simple-task-plan",
			CurrentStep: "",
			State:       ExecCompleted, // terminal — dispatch should replace
		},
	})

	wfID, err := engine.DispatchEvent("t1", "pr.event",
		map[string]string{"pr.issue_kind": "ci_failure"},
		map[string]string{"prompt": "fix"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if wfID != "pr-fix-test" {
		t.Fatalf("wfID = %q, want pr-fix-test", wfID)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.WorkflowID != "pr-fix-test" {
		t.Errorf("workflow on task = %q, want pr-fix-test", ti.Workflow.WorkflowID)
	}
}

func TestHasActiveWorkflow(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "no-wf", Status: "todo"})
	tasks.Put(TaskInfo{
		ID: "running", Status: "in-progress",
		Workflow: &Execution{WorkflowID: "x", State: ExecRunning},
	})
	tasks.Put(TaskInfo{
		ID: "waiting", Status: "in-progress",
		Workflow: &Execution{WorkflowID: "x", State: ExecWaiting},
	})
	tasks.Put(TaskInfo{
		ID: "completed", Status: "done",
		Workflow: &Execution{WorkflowID: "x", State: ExecCompleted},
	})
	tasks.Put(TaskInfo{
		ID: "failed", Status: "human-required",
		Workflow: &Execution{WorkflowID: "x", State: ExecFailed},
	})

	cases := []struct {
		id   string
		want bool
	}{
		{"no-wf", false},
		{"running", true},
		{"waiting", true},
		{"completed", false},
		{"failed", false},
		{"unknown-task", false},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			t.Parallel()
			if got := engine.HasActiveWorkflow(c.id); got != c.want {
				t.Errorf("HasActiveWorkflow(%q) = %v, want %v", c.id, got, c.want)
			}
		})
	}
}

func TestCancelWorkflow(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID: "active", Status: "in-progress",
		Workflow: &Execution{
			WorkflowID:  "pr-fix",
			CurrentStep: "fix",
			State:       ExecWaiting,
			Variables:   map[string]string{"pr_issue_kind": "ci_failure"},
		},
	})
	tasks.Put(TaskInfo{
		ID: "completed", Status: "done",
		Workflow: &Execution{WorkflowID: "pr-fix", State: ExecCompleted},
	})
	tasks.Put(TaskInfo{ID: "no-wf", Status: "todo"})

	// Pretend an agent is running for "active" so we can verify it's stopped.
	if _, _, _, err := agents.StartAgent("active", "pr-fix", "headless", "sonnet", "claude", "p", "", nil, false, false, "", "", AgentAssignment{}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	step, err := engine.CancelWorkflow("active", "ci_failure resolved")
	if err != nil {
		t.Fatalf("CancelWorkflow active: %v", err)
	}
	if step != "fix" {
		t.Errorf("returned step = %q, want %q", step, "fix")
	}
	got, err := tasks.GetTask("active")
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	if got.Workflow.State != ExecCompleted {
		t.Errorf("State = %s, want %s", got.Workflow.State, ExecCompleted)
	}
	if got.Workflow.CurrentStep != "" {
		t.Errorf("CurrentStep = %q, want empty", got.Workflow.CurrentStep)
	}
	if got.Workflow.CompletedAt == nil {
		t.Error("CompletedAt not set")
	}
	if got.Workflow.Variables["cancel_reason"] != "ci_failure resolved" {
		t.Errorf("cancel_reason = %q", got.Workflow.Variables["cancel_reason"])
	}
	if agents.HasRunningAgent("active") {
		t.Error("agent still running after cancel")
	}

	// Already-terminal workflow → no-op, no error.
	if step, err := engine.CancelWorkflow("completed", "x"); err != nil || step != "" {
		t.Errorf("cancel completed: step=%q err=%v", step, err)
	}

	// No workflow attached → no-op, no error.
	if step, err := engine.CancelWorkflow("no-wf", "x"); err != nil || step != "" {
		t.Errorf("cancel no-wf: step=%q err=%v", step, err)
	}
}

func TestStartWorkflowRejectsTamperFlaggedRestart(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	originalWorkflow := &Execution{
		WorkflowID: "simple-task-implement",
		State:      ExecCompleted,
		Variables:  map[string]string{tamperDeletionAllowlistVar: `{"exact_paths":{},"basenames":{}}`},
	}
	tasks.Put(TaskInfo{
		ID:           "tamper",
		Status:       "human-required",
		StatusReason: TamperFlaggedReasonPrefix + " removed tests/foo_test.go",
		Tags:         []string{TamperBlessedTag},
		Workflow:     originalWorkflow,
	})

	err := engine.StartWorkflow("tamper", "test-simple")
	if !errors.Is(err, ErrTamperBlessRequired) {
		t.Fatalf("StartWorkflow err = %v, want ErrTamperBlessRequired", err)
	}
	got, getErr := tasks.GetTask("tamper")
	if getErr != nil {
		t.Fatalf("get task: %v", getErr)
	}
	if got.Workflow.WorkflowID != originalWorkflow.WorkflowID ||
		got.Workflow.State != originalWorkflow.State ||
		got.Workflow.CurrentStep != originalWorkflow.CurrentStep {
		t.Fatalf("workflow changed: %+v, want original %+v", got.Workflow, originalWorkflow)
	}
	if agents.HasRunningAgent("tamper") {
		t.Fatal("StartWorkflow launched an agent for a tamper-flagged task")
	}
}

func TestStartWorkflowAllowsNonTamperHumanRequiredRestart(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "human",
		Status:       "human-required",
		StatusReason: "operator needs more context",
		Workflow:     &Execution{WorkflowID: "old", State: ExecCompleted},
	})

	if err := engine.StartWorkflow("human", "test-simple"); err != nil {
		t.Fatalf("StartWorkflow err = %v, want nil", err)
	}
	if !agents.HasRunningAgent("human") {
		t.Fatal("StartWorkflow did not launch an agent for non-tamper human-required task")
	}
}

func TestMatchWorkflow_PriorityTieBreak(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	// Two workflows match the same event + field — higher priority wins.
	addPREventWorkflow(t, store, "pr-fix-generic", 0, "ci_failure")
	addPREventWorkflow(t, store, "pr-fix-specialized", 10, "ci_failure")

	tasks.Put(TaskInfo{ID: "t1", Status: "in-review"})

	wfID, err := engine.DispatchEvent("t1", "pr.event",
		map[string]string{"pr.issue_kind": "ci_failure"},
		map[string]string{"prompt": "fix"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if wfID != "pr-fix-specialized" {
		t.Errorf("wfID = %q, want pr-fix-specialized (priority 10 should beat 0)", wfID)
	}
}

func TestMatchWorkflow_EqualPriorityDeterministic(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	// Two workflows with equal priority — alphabetical (store order) wins.
	addPREventWorkflow(t, store, "pr-fix-zebra", 5, "ci_failure")
	addPREventWorkflow(t, store, "pr-fix-alpha", 5, "ci_failure")

	tasks.Put(TaskInfo{ID: "t1", Status: "in-review"})

	wfID, err := engine.DispatchEvent("t1", "pr.event",
		map[string]string{"pr.issue_kind": "ci_failure"},
		map[string]string{"prompt": "fix"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if wfID != "pr-fix-alpha" {
		t.Errorf("wfID = %q, want pr-fix-alpha (alphabetical tiebreak)", wfID)
	}
}

func TestNoWorkflowField(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo"}) // no Workflow

	// Should not panic or error fatally.
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "agent-1", Result: "result", Success: true})
}

func TestDispatchPriorityRank(t *testing.T) {
	cases := []struct {
		status string
		want   int
	}{
		{"in-review", 0},
		{"ready-pr", 0},
		{"ready-review", 1},
		{"testing", 1},
		{"in-progress", 2},
		{"planning", 3},
		{"plan-review", 3},
		{"todo", 4},
		{"new", 4},
		{"blocked", 4},
		{"", 4},
	}
	for _, tc := range cases {
		if got := dispatchorder.Rank(tc.status); got != tc.want {
			t.Errorf("dispatchorder.Rank(%q) = %d, want %d", tc.status, got, tc.want)
		}
	}
}

func TestResumeStalled_PrioritizesReviewOverNewWork(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	parked := func(id, status string) TaskInfo {
		return TaskInfo{
			ID:        id,
			Status:    status,
			AgentMode: "headless",
			Workflow: &Execution{
				WorkflowID:  "test-simple",
				CurrentStep: "implement",
				State:       ExecWaiting,
				Variables:   make(map[string]string),
			},
		}
	}
	tasks.Put(parked("t-new", "todo"))
	tasks.Put(parked("t-mid", "in-progress"))
	tasks.Put(parked("t-review", "in-review"))

	engine.ResumeStalled()

	var order []string
	for _, c := range agents.calls {
		order = append(order, c.TaskID)
	}
	want := []string{"t-review", "t-mid", "t-new"}
	if !slices.Equal(order, want) {
		t.Fatalf("dispatch order = %v, want %v", order, want)
	}
}

// TestResumeStalled_DispatchComparatorOverridesDefaultOrder pins that an
// injected SetDispatchComparator replaces the built-in
// dispatchorder.Rank(status)-only sort entirely, so app wiring (agentqueue.Less)
// controls ordering without internal/workflow importing internal/agentqueue.
func TestResumeStalled_DispatchComparatorOverridesDefaultOrder(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	parked := func(id, status string) TaskInfo {
		return TaskInfo{
			ID:        id,
			Status:    status,
			AgentMode: "headless",
			Workflow: &Execution{
				WorkflowID:  "test-simple",
				CurrentStep: "implement",
				State:       ExecWaiting,
				Variables:   make(map[string]string),
			},
		}
	}
	tasks.Put(parked("t-new", "todo"))
	tasks.Put(parked("t-review", "in-review"))

	// Reverse alphabetical order by ID — deliberately nothing like the
	// built-in status-rank sort, so a passing test proves the comparator
	// actually drove the ordering rather than coincidentally matching it.
	engine.SetDispatchComparator(func() func(a, b TaskInfo) int {
		return func(a, b TaskInfo) int {
			return cmp.Compare(b.ID, a.ID)
		}
	})

	engine.ResumeStalled()

	var order []string
	for _, c := range agents.calls {
		order = append(order, c.TaskID)
	}
	want := []string{"t-review", "t-new"}
	if !slices.Equal(order, want) {
		t.Fatalf("dispatch order = %v, want %v (injected comparator must win over the default status-rank sort)", order, want)
	}
}

// TestResumeStalled_QueueReconcilerInvokedBeforeDispatch pins that a wired
// SetQueueReconciler runs once per ResumeStalled tick, before the dispatch
// scan (see the doc comment on Engine.queueReconciler).
func TestResumeStalled_QueueReconcilerInvokedBeforeDispatch(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "todo",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   make(map[string]string),
		},
	})

	var calls int
	var calledBeforeDispatch bool
	engine.SetQueueReconciler(func() {
		calls++
		calledBeforeDispatch = agents.CallCount() == 0
	})

	engine.ResumeStalled()

	if calls != 1 {
		t.Fatalf("queue reconciler called %d times, want 1", calls)
	}
	if !calledBeforeDispatch {
		t.Fatal("queue reconciler must run before the dispatch scan, not after")
	}

	engine.ResumeStalled()
	if calls != 2 {
		t.Fatalf("queue reconciler called %d times across two ticks, want 2", calls)
	}
}

func TestResumeStalled_ReconcilesWaitHumanStatus(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(Definition{
		ID:   "wait-human-wf",
		Name: "wait human wf",
		Steps: []Step{
			{
				ID:     "review_plan",
				Name:   "Review Plan",
				Type:   StepWaitHuman,
				Config: StepConfig{Status: "plan-review", HumanActions: []string{"approve", "reject"}},
				Next:   []Transition{{GoTo: ""}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "todo",
		Workflow: &Execution{
			WorkflowID:  "wait-human-wf",
			CurrentStep: "review_plan",
			State:       ExecWaiting,
			Variables:   make(map[string]string),
		},
	})

	engine.ResumeStalled()

	ti, _ := tasks.GetTask("t1")
	if ti.Status != "plan-review" {
		t.Fatalf("status = %q, want plan-review (wait_human status must be reconciled)", ti.Status)
	}
	if agents.CallCount() != 0 {
		t.Fatalf("wait_human step must not spawn an agent, got %d", agents.CallCount())
	}
}

func TestResumeStalled_WaitHumanStatusRespectsSkipStatuses(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(Definition{
		ID:   "wait-human-wf",
		Name: "wait human wf",
		Steps: []Step{
			{
				ID:     "review_plan",
				Name:   "Review Plan",
				Type:   StepWaitHuman,
				Config: StepConfig{Status: "plan-review", HumanActions: []string{"approve", "reject"}},
				Next:   []Transition{{GoTo: ""}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	for _, keep := range []string{"cancelled", "done", "human-required"} {
		t.Run(keep, func(t *testing.T) {
			tasks := newMemTasks()
			engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
			tasks.Put(TaskInfo{
				ID:     "t1",
				Status: keep,
				Workflow: &Execution{
					WorkflowID:  "wait-human-wf",
					CurrentStep: "review_plan",
					State:       ExecWaiting,
					Variables:   make(map[string]string),
				},
			})

			engine.ResumeStalled()

			ti, _ := tasks.GetTask("t1")
			if ti.Status != keep {
				t.Fatalf("status = %q, want %q unchanged (skip status must not be reconciled)", ti.Status, keep)
			}
		})
	}
}

func TestResumeStalled_ReroutesStaleConditionBranch(t *testing.T) {
	store := newInlineTestStore(t, "condition-reroute", `
id: condition-reroute
steps:
  - id: maybe_critique
    type: condition
    next:
      - when:
          field: task.tags
          operator: not_contains
          value: nocritic
        goto: critique_plan
      - goto: review_plan
  - id: critique_plan
    type: run_agent
    config:
      role: plan-critic
      mode: headless
      prompt: critique
    next:
      - goto: review_plan
  - id: review_plan
    type: wait_human
    config:
      status: plan-review
      human_actions: [approve, reject]
    next:
      - goto: ""
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	now := time.Now().UTC()
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "planning",
		Tags:   []string{"nocritic"},
		Workflow: &Execution{
			WorkflowID:  "condition-reroute",
			CurrentStep: "critique_plan",
			State:       ExecWaiting,
			Variables:   make(map[string]string),
			StepHistory: []StepRecord{{
				StepID:    "maybe_critique",
				Status:    "completed",
				StartedAt: now.Add(-time.Minute),
				EndedAt:   now.Add(-time.Minute),
			}},
			EffectLog: []EffectRecord{
				{
					ID:          EffectID{Generation: 1, StepSeq: 0, StepID: "maybe_critique", Pos: effectPosStepAction},
					IntentAt:    now.Add(-time.Minute),
					CompletedAt: &now,
				},
				{
					ID:       EffectID{Generation: 1, StepSeq: 1, StepID: "critique_plan", Pos: effectPosStepAction},
					IntentAt: now.Add(-time.Minute),
				},
			},
		},
	})
	def, err := store.Get("condition-reroute")
	if err != nil {
		t.Fatal(err)
	}
	step := def.StepByID("critique_plan")
	if step == nil {
		t.Fatal("critique_plan step missing")
	}
	pre, _ := tasks.GetTask("t1")
	condition := latestConditionPredecessor(&def, pre.Workflow, step.ID)
	if condition == nil {
		t.Fatal("precondition: condition predecessor not detected")
	}
	nextID, err := ResolveTransition(condition.Next, engine.transitionFields(pre, pre.Workflow))
	if err != nil {
		t.Fatal(err)
	}
	if nextID != "review_plan" {
		t.Fatalf("precondition: condition routes to %q, want review_plan", nextID)
	}

	engine.ResumeStalled()

	ti, _ := tasks.GetTask("t1")
	if ti.Workflow == nil {
		t.Fatal("workflow missing")
	}
	if ti.Workflow.CurrentStep != "review_plan" {
		t.Fatalf("CurrentStep = %q, want review_plan", ti.Workflow.CurrentStep)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Fatalf("State = %q, want %q", ti.Workflow.State, ExecWaiting)
	}
	if ti.Status != "plan-review" {
		t.Fatalf("Status = %q, want plan-review", ti.Status)
	}
	if agents.CallCount() != 0 {
		t.Fatalf("critique agent dispatched %d times, want 0", agents.CallCount())
	}
	for _, rec := range ti.Workflow.EffectLog {
		if rec.ID.StepID == "critique_plan" {
			t.Fatalf("stale critique_plan effect was not cleared: %+v", ti.Workflow.EffectLog)
		}
	}
}

func TestResumeStalled_DoesNotRerouteConditionWhileDispatchClaimed(t *testing.T) {
	store := newInlineTestStore(t, "condition-reroute-claimed", `
id: condition-reroute-claimed
steps:
  - id: maybe_critique
    type: condition
    next:
      - when:
          field: task.tags
          operator: not_contains
          value: nocritic
        goto: critique_plan
      - goto: review_plan
  - id: critique_plan
    type: run_agent
    config:
      role: plan-critic
      mode: headless
      prompt: critique
    next:
      - goto: review_plan
  - id: review_plan
    type: wait_human
    config:
      status: plan-review
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	now := time.Now().UTC()
	tasks.Put(TaskInfo{
		ID: "t1", Status: "planning", Tags: []string{"nocritic"},
		Workflow: &Execution{
			WorkflowID: "condition-reroute-claimed", CurrentStep: "critique_plan", State: ExecWaiting,
			StepHistory: []StepRecord{{StepID: "maybe_critique", Status: "completed", StartedAt: now, EndedAt: now}},
			AgentRoutes: map[string]string{"agent-1": "critique_plan"},
		},
	})

	// This models recovery's lost-callback bridge: the manager no longer sees
	// the agent, but HandleAgentComplete has already claimed the task while it
	// advances the persisted route.
	engine.mu.Lock()
	engine.dispatching["t1"] = struct{}{}
	engine.mu.Unlock()
	defer engine.clearResumeDispatching("t1")

	engine.ResumeStalled()

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.Workflow.CurrentStep != "critique_plan" {
		t.Fatalf("CurrentStep = %q, want critique_plan while completion owns dispatch", ti.Workflow.CurrentStep)
	}
	if _, ok := ti.Workflow.AgentRoute("agent-1"); !ok {
		t.Fatal("condition reroute cleared the completing agent route")
	}
	if agents.CallCount() != 0 {
		t.Fatalf("agent starts = %d, want 0", agents.CallCount())
	}
}

func TestResumeStalled_ConditionRerouteDoesNotOverwriteFreshCompletion(t *testing.T) {
	store := newInlineTestStore(t, "condition-reroute-fresh", `
id: condition-reroute-fresh
steps:
  - id: maybe_critique
    type: condition
    next:
      - when:
          field: task.tags
          operator: not_contains
          value: nocritic
        goto: critique_plan
      - goto: review_plan
  - id: critique_plan
    type: run_agent
    config:
      role: plan-critic
      mode: headless
      prompt: critique
    next:
      - goto: review_plan
  - id: review_plan
    type: wait_human
    config:
      status: plan-review
`)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	now := time.Now().UTC()
	stale := TaskInfo{
		ID: "t1", Status: "planning", Tags: []string{"nocritic"},
		Workflow: &Execution{
			WorkflowID: "condition-reroute-fresh", CurrentStep: "critique_plan", State: ExecWaiting,
			StepHistory: []StepRecord{{StepID: "maybe_critique", Status: "completed", StartedAt: now, EndedAt: now}},
			AgentRoutes: map[string]string{"agent-1": "critique_plan"},
		},
	}
	tasks.Put(stale)

	// A recovered completion advances after ResumeStalled read stale but before
	// its reroute claim. The reroute must re-read rather than write stale back.
	completed := stale
	completed.Workflow = stale.Workflow.Clone()
	if completed.Workflow == nil {
		t.Fatal("stale workflow clone is nil")
	}
	completed.Workflow.CurrentStep = "review_plan"
	completed.Workflow.ClearAgentRoute("agent-1")
	tasks.Put(completed)

	def, err := store.Get("condition-reroute-fresh")
	if err != nil {
		t.Fatal(err)
	}
	step := def.StepByID("critique_plan")
	if step == nil {
		t.Fatal("critique_plan step missing")
	}
	if !engine.resumeStalledRerouteStaleConditionBranch(&stale, &def, step) {
		t.Fatal("stale reroute was not consumed")
	}

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow.CurrentStep != "review_plan" {
		t.Fatalf("CurrentStep = %q, want completion's review_plan", got.Workflow.CurrentStep)
	}
}

func TestResumeStalled_RunAgent(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	// Simulate a task stuck at "implement" step with no running agent.
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecRunning,
			Variables:   make(map[string]string),
		},
	})

	engine.ResumeStalled()

	// Should have started an agent for the implement step.
	if agents.CallCount() != 1 {
		t.Fatalf("expected 1 agent start, got %d", agents.CallCount())
	}
	if agents.LastCall().Role != "implementation" {
		t.Fatalf("expected implementation, got %q", agents.LastCall().Role)
	}
}

func TestResumeStalled_RunAgentAfterCompletedDispatchEffect(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	completedAt := time.Now().UTC()
	tasks.Put(TaskInfo{
		ID:         "t1",
		Generation: 1,
		Status:     "in-progress",
		AgentMode:  "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecRunning,
			Variables:   make(map[string]string),
			EffectLog: []EffectRecord{{
				ID:          EffectID{Generation: 1, StepSeq: 0, StepID: "implement", Pos: effectPosStepAction},
				IntentAt:    completedAt.Add(-time.Second),
				Owner:       engine.ownerID,
				CompletedAt: &completedAt,
			}},
		},
	})

	engine.ResumeStalled()

	if agents.CallCount() != 1 {
		t.Fatalf("expected 1 agent start, got %d", agents.CallCount())
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if len(got.Workflow.EffectLog) != 2 {
		t.Fatalf("effect log len = %d, want 2: %+v", len(got.Workflow.EffectLog), got.Workflow.EffectLog)
	}
	if got.Workflow.EffectLog[1].ID.StepSeq <= got.Workflow.EffectLog[0].ID.StepSeq {
		t.Fatalf("new effect StepSeq = %d, want greater than prior completed StepSeq %d",
			got.Workflow.EffectLog[1].ID.StepSeq, got.Workflow.EffectLog[0].ID.StepSeq)
	}
	if got.Workflow.EffectLog[1].CompletedAt == nil {
		t.Fatalf("new dispatch effect was not completed: %+v", got.Workflow.EffectLog[1])
	}
}

func TestResumeStalled_DispatchesRateLimitedProviderForFailover(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetProviderRateLimited(true)
	agents.SetProviderFailover(true)
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecRunning,
			Variables:   make(map[string]string),
		},
	})

	engine.ResumeStalled()

	if agents.CallCount() != 1 {
		t.Fatalf("expected workflow to dispatch so agent manager can fail over, got %d starts", agents.CallCount())
	}
}

func TestRescheduleRateLimitedAgent_RerunsCurrentStep(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   make(map[string]string),
		},
	})
	setWorkflowAgentRoute(t, tasks, "t1", "limited-agent", "implement")

	engine.RescheduleRateLimitedAgent("t1", "limited-agent")

	if agents.CallCount() != 1 {
		t.Fatalf("expected replacement agent start, got %d", agents.CallCount())
	}
	if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "limited-agent"); tracked {
		t.Fatal("rate-limited agent step mapping was not cleared")
	}
}

func TestRescheduleInterruptedAgent_UnparksHumanRequiredCurrentStep(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "human-required",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   make(map[string]string),
		},
	})
	if err := tasks.UpdateTaskStatus("t1", "human-required", "provider run aborted after tool use was rejected"); err != nil {
		t.Fatalf("UpdateTaskStatus: %v", err)
	}
	setWorkflowAgentRoute(t, tasks, "t1", "interrupted-agent", "implement")

	engine.RescheduleInterruptedAgent("t1", "interrupted-agent")

	if got := agents.CallCount(); got != 1 {
		t.Fatalf("expected replacement agent start, got %d", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "in-progress" || got.StatusReason != "" {
		t.Fatalf("status/reason = %q/%q, want in-progress with empty reason", got.Status, got.StatusReason)
	}
	if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "interrupted-agent"); tracked {
		t.Fatal("interrupted agent step mapping was not cleared")
	}
}

func TestRescheduleInterruptedAgent_SkipsBlockedAndTerminalStatuses(t *testing.T) {
	for _, status := range []string{"blocked", "done", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewEngine(store, tasks, agents, discardLogger())

			tasks.Put(TaskInfo{
				ID:        "t1",
				Status:    status,
				AgentMode: "headless",
				Workflow: &Execution{
					WorkflowID:  "test-simple",
					CurrentStep: "implement",
					State:       ExecWaiting,
					Variables:   make(map[string]string),
				},
			})
			setWorkflowAgentRoute(t, tasks, "t1", "interrupted-agent", "implement")

			engine.RescheduleInterruptedAgent("t1", "interrupted-agent")

			if got := agents.CallCount(); got != 0 {
				t.Fatalf("replacement agent starts = %d, want 0", got)
			}
			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != status {
				t.Fatalf("status = %q, want %q", got.Status, status)
			}
			if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "interrupted-agent"); tracked {
				t.Fatal("interrupted agent step mapping was not cleared")
			}
		})
	}
}

func TestInterruptedRecoveryStatus_PlanningWorkflow(t *testing.T) {
	t.Parallel()

	if got := interruptedRecoveryStatus("simple-task-plan"); got != "planning" {
		t.Fatalf("interruptedRecoveryStatus(simple-task-plan) = %q, want planning", got)
	}
	if got := interruptedRecoveryStatus("testing-task"); got != "testing" {
		t.Fatalf("interruptedRecoveryStatus(testing-task) = %q, want testing", got)
	}
	if got := interruptedRecoveryStatus("simple-task-implement"); got != "in-progress" {
		t.Fatalf("interruptedRecoveryStatus(simple-task-implement) = %q, want in-progress", got)
	}
}

func TestRescheduleRateLimitedAgent_WatchdogRetriesThenEscalates(t *testing.T) {
	tests := []struct {
		name       string
		retries    string
		wantStarts int
		wantStatus string
		wantReason string
		wantRetry  string
	}{
		{
			name:       "first watchdog rate limit reruns",
			wantStarts: 1,
			wantStatus: "in-progress",
			wantReason: "",
			wantRetry:  "1",
		},
		{
			name:       "budget exhausted escalates",
			retries:    "2",
			wantStarts: 0,
			wantStatus: "human-required",
			wantReason: "watchdog: rate limit retry budget exhausted after 2 clean re-dispatches",
			wantRetry:  "2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewEngine(store, tasks, agents, discardLogger())

			vars := map[string]string{}
			if tc.retries != "" {
				vars[watchdogRateLimitRetryKey("implement")] = tc.retries
			}
			tasks.Put(TaskInfo{
				ID:           "t1",
				Status:       "in-progress",
				StatusReason: "watchdog: rate limit: org-level quota exhausted",
				AgentMode:    "headless",
				Workflow: &Execution{
					WorkflowID:  "test-simple",
					CurrentStep: "implement",
					State:       ExecWaiting,
					Variables:   vars,
				},
			})
			setWorkflowAgentRoute(t, tasks, "t1", "limited-agent", "implement")

			engine.RescheduleRateLimitedAgent("t1", "limited-agent")

			if got := agents.CallCount(); got != tc.wantStarts {
				t.Fatalf("expected replacement agent starts = %d, got %d", tc.wantStarts, got)
			}
			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.StatusReason != tc.wantReason {
				t.Fatalf("status_reason = %q, want %q", got.StatusReason, tc.wantReason)
			}
			if got.Workflow.Variables[watchdogRateLimitRetryKey("implement")] != tc.wantRetry {
				t.Fatalf("rate-limit retry var = %q, want %q", got.Workflow.Variables[watchdogRateLimitRetryKey("implement")], tc.wantRetry)
			}
			if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "limited-agent"); tracked {
				t.Fatal("rate-limited agent step mapping was not cleared")
			}
		})
	}
}

func TestResumeStalled_WatchdogZeroOutputUsesSharedRetryBudget(t *testing.T) {
	oldStartedAt := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name             string
		retries          string
		freshUsed        bool
		wantStarts       int
		wantStatus       string
		wantReason       string
		wantRetry        string
		wantFresh        string
		wantSessionFence bool
	}{
		{
			name:       "resume consumes persisted retry and reruns",
			retries:    "1",
			wantStarts: 1,
			wantStatus: "in-progress",
			wantReason: "",
			wantRetry:  "2",
		},
		{
			// Exhausting the *resume* budget is not terminal: the poisoned
			// session is fenced with a reset budget so the next tick dispatches a
			// clean session, instead of latching a permanent blocked deadlock
			// (2026-07-23). This tick is consumed (no dispatch yet).
			name:             "resume budget exhausted grants one fresh-session round",
			retries:          strconv.Itoa(maxWatchdogRateLimitRetries),
			wantStarts:       0,
			wantStatus:       "in-progress",
			wantReason:       watchdogreason.RateLimit(watchdogreason.ZeroOutputBeforeStartup),
			wantRetry:        "0",
			wantFresh:        "1",
			wantSessionFence: true,
		},
		{
			name:             "fresh round also exhausts, blocks and fences off the poisoned session",
			retries:          strconv.Itoa(maxWatchdogRateLimitRetries),
			freshUsed:        true,
			wantStarts:       0,
			wantStatus:       "blocked",
			wantReason:       "watchdog: zero-output startup retry budget exhausted after 3 identical attempts",
			wantRetry:        strconv.Itoa(maxWatchdogRateLimitRetries),
			wantFresh:        "1",
			wantSessionFence: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewEngine(store, tasks, agents, discardLogger())

			vars := map[string]string{}
			if tc.retries != "" {
				vars[watchdogRateLimitRetryKey("implement")] = tc.retries
			}
			if tc.freshUsed {
				vars[watchdogZeroOutputFreshRetryKey("implement")] = "1"
			}
			tasks.Put(TaskInfo{
				ID:           "t1",
				Status:       "in-progress",
				StatusReason: watchdogreason.RateLimit(watchdogreason.ZeroOutputBeforeStartup),
				AgentMode:    "headless",
				Workflow: &Execution{
					WorkflowID:  "test-simple",
					CurrentStep: "implement",
					State:       ExecWaiting,
					Variables:   vars,
					StartedAt:   oldStartedAt,
				},
			})

			engine.ResumeStalled()

			if got := agents.CallCount(); got != tc.wantStarts {
				t.Fatalf("replacement agent starts = %d, want %d", got, tc.wantStarts)
			}
			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.StatusReason != tc.wantReason {
				t.Fatalf("status_reason = %q, want %q", got.StatusReason, tc.wantReason)
			}
			if got.Workflow.Variables[watchdogRateLimitRetryKey("implement")] != tc.wantRetry {
				t.Fatalf("rate-limit retry var = %q, want %q", got.Workflow.Variables[watchdogRateLimitRetryKey("implement")], tc.wantRetry)
			}
			if got.Workflow.Variables[watchdogZeroOutputFreshRetryKey("implement")] != tc.wantFresh {
				t.Fatalf("fresh-retry var = %q, want %q", got.Workflow.Variables[watchdogZeroOutputFreshRetryKey("implement")], tc.wantFresh)
			}
			// sybra#2542: exhausting the zero-output-stall budget must bump
			// Workflow.StartedAt past every prior agent run, so
			// PickImplementationResumeSession's StartedAt fence rejects the
			// poisoned session and the next dispatch starts fresh instead of
			// resuming the same session that just hung.
			if tc.wantSessionFence && !got.Workflow.StartedAt.After(oldStartedAt) {
				t.Fatalf("workflow.started_at = %v, want it bumped past %v to fence off the poisoned resume session", got.Workflow.StartedAt, oldStartedAt)
			}
			if !tc.wantSessionFence && !got.Workflow.StartedAt.Equal(oldStartedAt) {
				t.Fatalf("workflow.started_at = %v, want unchanged %v", got.Workflow.StartedAt, oldStartedAt)
			}
		})
	}
}

// TestResumeStalled_WatchdogZeroOutputFreshRoundDispatches proves the deadlock
// break end-to-end: a poisoned resume that has exhausted its resume budget is
// granted a fresh round (tick 1 consumed, fenced) and then actually
// re-dispatches a clean session on the next tick — rather than latching a
// permanent blocked state as it did during the 2026-07-23 board freeze.
func TestResumeStalled_WatchdogZeroOutputFreshRoundDispatches(t *testing.T) {
	oldStartedAt := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: watchdogreason.RateLimit(watchdogreason.ZeroOutputBeforeStartup),
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   map[string]string{watchdogRateLimitRetryKey("implement"): strconv.Itoa(maxWatchdogRateLimitRetries)},
			StartedAt:   oldStartedAt,
		},
	})

	// Tick 1: resume budget already exhausted -> fence + reset, no dispatch yet.
	engine.ResumeStalled()
	if got := agents.CallCount(); got != 0 {
		t.Fatalf("tick 1 dispatched %d agents, want 0 (fence-and-wait)", got)
	}
	afterFence, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if afterFence.Status != "in-progress" {
		t.Fatalf("tick 1 status = %q, want in-progress (not blocked)", afterFence.Status)
	}
	if !afterFence.Workflow.StartedAt.After(oldStartedAt) {
		t.Fatalf("tick 1 did not fence the poisoned session (started_at %v)", afterFence.Workflow.StartedAt)
	}

	// Tick 2: fenced clean session actually dispatches.
	engine.ResumeStalled()
	if got := agents.CallCount(); got != 1 {
		t.Fatalf("tick 2 dispatched %d agents total, want 1 (fresh session ran)", got)
	}
	final, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if final.Status == "blocked" {
		t.Fatalf("task latched blocked despite a fresh-session recovery being available")
	}
}

func TestResumeStalled_WatchdogRateLimitPoolBusyDoesNotBurnRetryBudget(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetFailSpawn(ErrAgentPoolBusy)
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "watchdog: rate limit: org-level quota exhausted",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})

	// The first retry is armed then hits a benign capacity park. A later tick
	// must retry dispatch normally instead of re-arming the rate-limit policy.
	for range maxWatchdogRateLimitRetries + 1 {
		engine.ResumeStalled()
	}

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "in-progress" {
		t.Fatalf("status = %q, want in-progress", got.Status)
	}
	if got.StatusReason != "" {
		t.Fatalf("status_reason = %q, want cleared after arming retry", got.StatusReason)
	}
	if retry := got.Workflow.Variables[watchdogRateLimitRetryKey("implement")]; retry != "1" {
		t.Fatalf("rate-limit retry var = %q, want 1 after repeated pool-busy parks", retry)
	}
	if got.Workflow.State == ExecFailed {
		t.Fatal("pool-busy parks must not exhaust the watchdog retry budget")
	}
}

func TestResumeStalled_WatchdogHangPoolBusyRetainsReaskNote(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetFailSpawn(ErrAgentPoolBusy)
	engine := NewEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{
		ID: "t1", Status: "in-progress", StatusReason: "watchdog hang: no stream activity", AgentMode: "headless",
		Workflow: &Execution{WorkflowID: "test-simple", CurrentStep: "implement", State: ExecWaiting, Variables: map[string]string{}, StartedAt: time.Now().UTC()},
	})

	engine.ResumeStalled()

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if note := got.Workflow.Variables[watchdogReaskNoteVar]; note == "" {
		t.Fatal("pool-busy park cleared watchdog guidance before an agent started")
	}
	if retry := got.Workflow.Variables[watchdogHangRetryKey("implement")]; retry != "1" {
		t.Fatalf("hang retry = %q, want 1", retry)
	}
}

func TestResumeStalled_WatchdogRateLimitDoesNotClearConcurrentFailure(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "watchdog: rate limit: org-level quota exhausted",
		AgentMode:    "headless",
		Workflow:     &Execution{WorkflowID: "test-simple", CurrentStep: "implement", State: ExecWaiting, Variables: map[string]string{}},
	})
	var once sync.Once
	tasks.onSetWorkflow = func(id string) {
		once.Do(func() {
			if err := tasks.UpdateTaskStatus(id, "human-required", "concurrent permanent failure"); err != nil {
				t.Errorf("set concurrent failure: %v", err)
			}
		})
	}

	engine.ResumeStalled()

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "human-required" || got.StatusReason != "concurrent permanent failure" {
		t.Fatalf("concurrent failure was overwritten: status=%q reason=%q", got.Status, got.StatusReason)
	}
	if starts := agents.CallCount(); starts != 0 {
		t.Fatalf("concurrent failure must fence dispatch, started %d agents", starts)
	}
}

// TestRescheduleRateLimitedAgent_ParksWhileProviderRateLimitedNoFailover is the
// regression guard for sybra#1585: a reschedule that fires while the step's
// provider is still inside its rate-limit cooldown (and no healthy peer exists
// to fail over to) must PARK the task — not consume a watchdog retry budget and
// not escalate to human-required. Even with the retry budget already exhausted,
// the task waits for the cooldown to expire instead of bothering a human.
func TestRescheduleRateLimitedAgent_ParksWhileProviderRateLimitedNoFailover(t *testing.T) {
	tests := []struct {
		name    string
		retries string
	}{
		{name: "fresh budget parks", retries: ""},
		{name: "exhausted budget still parks", retries: strconv.Itoa(maxWatchdogRateLimitRetries)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			// Provider throttled, no peer to fail over to → must park and wait.
			agents.SetProviderRateLimited(true)
			engine := NewEngine(store, tasks, agents, discardLogger())

			vars := map[string]string{}
			if tc.retries != "" {
				vars[watchdogRateLimitRetryKey("implement")] = tc.retries
			}
			tasks.Put(TaskInfo{
				ID:           "t1",
				Status:       "in-progress",
				StatusReason: "watchdog: rate limit: org-level quota exhausted",
				AgentMode:    "headless",
				Workflow: &Execution{
					WorkflowID:  "test-simple",
					CurrentStep: "implement",
					State:       ExecWaiting,
					Variables:   vars,
				},
			})
			setWorkflowAgentRoute(t, tasks, "t1", "limited-agent", "implement")

			engine.RescheduleRateLimitedAgent("t1", "limited-agent")

			if got := agents.CallCount(); got != 0 {
				t.Fatalf("expected no replacement agent starts while parked, got %d", got)
			}
			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != "in-progress" {
				t.Fatalf("status = %q, want in-progress (parked, not escalated)", got.Status)
			}
			// Budget untouched: the parked attempt never counted.
			if got := got.Workflow.Variables[watchdogRateLimitRetryKey("implement")]; got != tc.retries {
				t.Fatalf("rate-limit retry var = %q, want unchanged %q", got, tc.retries)
			}
			// Breaker untouched: a transient throttle is not a dispatch failure.
			if _, tripped := got.Workflow.Variables[circuitBreakerFailureKey("implement")]; tripped {
				t.Fatalf("circuit breaker recorded a failure for a transient rate limit: %v", got.Workflow.Variables)
			}
			if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "limited-agent"); tracked {
				t.Fatal("rate-limited agent step mapping was not cleared")
			}
		})
	}
}

func TestRescheduleRateLimitedAgent_EmptyTaskIDClearsTrackedAgent(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})
	setWorkflowAgentRoute(t, tasks, "t1", "limited-agent", "implement")

	engine.RescheduleRateLimitedAgent("", "limited-agent")

	if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "limited-agent"); tracked {
		t.Fatal("tracked agent step mapping should be cleared for empty task IDs")
	}
	if got := agents.CallCount(); got != 0 {
		t.Fatalf("unexpected replacement agent start count = %d, want 0", got)
	}
}

func TestRescheduleRateLimitedAgent_SkippedInflightDoesNotConsumeWatchdogRetry(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "watchdog: rate limit: org-level quota exhausted",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables: map[string]string{
				watchdogRateLimitRetryKey("implement"): "1",
			},
		},
	})
	setWorkflowAgentRoute(t, tasks, "t1", "limited-agent", "implement")

	mu := engine.taskInflightMutex("t1")
	mu.Lock()
	engine.RescheduleRateLimitedAgent("t1", "limited-agent")
	mu.Unlock()

	if got := agents.CallCount(); got != 0 {
		t.Fatalf("unexpected replacement agent starts = %d, want 0", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "in-progress" {
		t.Fatalf("status = %q, want in-progress", got.Status)
	}
	if got.StatusReason != "watchdog: rate limit: org-level quota exhausted" {
		t.Fatalf("status_reason = %q", got.StatusReason)
	}
	if got.Workflow.Variables[watchdogRateLimitRetryKey("implement")] != "1" {
		t.Fatalf("rate-limit retry var = %q, want 1", got.Workflow.Variables[watchdogRateLimitRetryKey("implement")])
	}
}

func TestRescheduleRateLimitedAgent_SkippedSharedClaimDoesNotConsumeWatchdogRetry(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "watchdog: rate limit: org-level quota exhausted",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables: map[string]string{
				watchdogRateLimitRetryKey("implement"): "1",
			},
		},
	})
	setWorkflowAgentRoute(t, tasks, "t1", "limited-agent", "implement")

	agents.SetDispatchClaimed("t1", true)
	engine.RescheduleRateLimitedAgent("t1", "limited-agent")

	if got := agents.CallCount(); got != 0 {
		t.Fatalf("unexpected replacement agent starts = %d, want 0", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "in-progress" {
		t.Fatalf("status = %q, want in-progress", got.Status)
	}
	if got.StatusReason != "watchdog: rate limit: org-level quota exhausted" {
		t.Fatalf("status_reason = %q", got.StatusReason)
	}
	if got.Workflow.Variables[watchdogRateLimitRetryKey("implement")] != "1" {
		t.Fatalf("rate-limit retry var = %q, want 1", got.Workflow.Variables[watchdogRateLimitRetryKey("implement")])
	}
}

func TestRescheduleRateLimitedAgent_SkipsBlockedStatus(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "blocked",
		StatusReason: "watchdog: zero-output startup retry budget exhausted after 3 identical attempts",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecFailed,
			Variables: map[string]string{
				watchdogRateLimitRetryKey("implement"): strconv.Itoa(maxWatchdogRateLimitRetries),
			},
		},
	})
	setWorkflowAgentRoute(t, tasks, "t1", "limited-agent", "implement")

	engine.RescheduleRateLimitedAgent("t1", "limited-agent")

	if got := agents.CallCount(); got != 0 {
		t.Fatalf("unexpected replacement agent starts = %d, want 0", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", got.Status)
	}
	if got.StatusReason != "watchdog: zero-output startup retry budget exhausted after 3 identical attempts" {
		t.Fatalf("status_reason = %q", got.StatusReason)
	}
	if got.Workflow.Variables[watchdogRateLimitRetryKey("implement")] != strconv.Itoa(maxWatchdogRateLimitRetries) {
		t.Fatalf("rate-limit retry var = %q, want %d", got.Workflow.Variables[watchdogRateLimitRetryKey("implement")], maxWatchdogRateLimitRetries)
	}
	if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "limited-agent"); tracked {
		t.Fatal("rate-limited agent step mapping was not cleared")
	}
}

func TestRescheduleRateLimitedAgent_HoldsDispatchClaimAcrossRescheduleAttempt(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.startEntered = make(chan struct{}, 1)
	agents.startGate = make(chan struct{})
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "watchdog: rate limit: org-level quota exhausted",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})

	setWorkflowAgentRoute(t, tasks, "t1", "limited-agent-1", "implement")
	setWorkflowAgentRoute(t, tasks, "t1", "limited-agent-2", "implement")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		engine.RescheduleRateLimitedAgent("t1", "limited-agent-1")
	}()

	<-agents.startEntered

	go func() {
		defer wg.Done()
		engine.RescheduleRateLimitedAgent("t1", "limited-agent-2")
	}()

	close(agents.startGate)
	wg.Wait()

	if got := agents.CallCount(); got != 1 {
		t.Fatalf("replacement agent starts = %d, want 1", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Workflow.Variables[watchdogRateLimitRetryKey("implement")] != "1" {
		t.Fatalf("rate-limit retry var = %q, want 1", got.Workflow.Variables[watchdogRateLimitRetryKey("implement")])
	}
}

func TestRescheduleCheckpointedAgent_RerunsCurrentStepSameWorktree(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.claimInsideStart = true
	engine := NewEngine(store, tasks, agents, discardLogger())
	engine.SetMaxCheckpoints(3)

	const worktreeDir = "/tmp/checkpoint-worktree"
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables: map[string]string{
				WorkflowVarDir:                    worktreeDir,
				"step.implement.checkpoint_count": "1",
			},
		},
	})
	setWorkflowAgentRoute(t, tasks, "t1", "checkpoint-agent", "implement")

	engine.RescheduleCheckpointedAgent("t1", "checkpoint-agent")

	if got := agents.CallCount(); got != 1 {
		t.Fatalf("expected replacement agent start, got %d", got)
	}
	agents.mu.Lock()
	call := agents.calls[len(agents.calls)-1]
	agents.mu.Unlock()
	if call.Dir != worktreeDir {
		t.Fatalf("replacement dir = %q, want %q", call.Dir, worktreeDir)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Workflow.Variables["step.implement.checkpoint_count"] != "2" {
		t.Fatalf("checkpoint count = %q, want 2", got.Workflow.Variables["step.implement.checkpoint_count"])
	}
	if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "checkpoint-agent"); tracked {
		t.Fatal("checkpointed agent step mapping was not cleared")
	}
}

func TestRescheduleCheckpointedAgent_RetriesGhostDispatchPark(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.failSpawnOnce = ErrDispatchInFlight
	engine := NewEngine(store, tasks, agents, discardLogger())
	engine.SetMaxCheckpoints(3)

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables: map[string]string{
				WorkflowVarDir:                    "/tmp/checkpoint-worktree",
				"step.implement.checkpoint_count": "1",
			},
		},
	})
	setWorkflowAgentRoute(t, tasks, "t1", "checkpoint-agent", "implement")

	engine.RescheduleCheckpointedAgent("t1", "checkpoint-agent")

	if got := agents.CallCount(); got != 1 {
		t.Fatalf("replacement agent starts = %d, want 1 after retrying ghost park", got)
	}
}

func TestRescheduleCheckpointedAgent_ParksAtCap(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	engine.SetMaxCheckpoints(2)

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables: map[string]string{
				"step.implement.checkpoint_count": "2",
			},
		},
	})
	setWorkflowAgentRoute(t, tasks, "t1", "checkpoint-agent", "implement")

	engine.RescheduleCheckpointedAgent("t1", "checkpoint-agent")

	if got := agents.CallCount(); got != 0 {
		t.Fatalf("unexpected replacement agent starts = %d, want 0", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if got.Workflow.Variables["step.implement.checkpoint_count"] != "2" {
		t.Fatalf("checkpoint count = %q, want unchanged 2", got.Workflow.Variables["step.implement.checkpoint_count"])
	}
}

func TestHandleAgentComplete_CheckpointFailedParksWithoutRetry(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "todo",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "triage",
			State:       ExecWaiting,
		},
	})
	setWorkflowAgentRoute(t, tasks, "t1", "checkpoint-agent", "triage")

	engine.HandleAgentComplete("t1", AgentCompletion{
		AgentID:          "checkpoint-agent",
		Provider:         "claude",
		Success:          false,
		EscalationReason: "checkpoint_failed",
	})

	if got := agents.CallCount(); got != 0 {
		t.Fatalf("replacement agent starts = %d, want 0", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if got.StatusReason != "checkpoint_failed: checkpoint commit failed — no durable checkpoint state created" {
		t.Fatalf("status reason = %q", got.StatusReason)
	}
	if got.Workflow == nil {
		t.Fatal("workflow = nil")
	}
	if got.Workflow.State != ExecCompleted {
		t.Fatalf("workflow state = %q, want completed", got.Workflow.State)
	}
	if got.Workflow.CurrentStep != "" {
		t.Fatalf("workflow current step = %q, want empty", got.Workflow.CurrentStep)
	}
	if got.Workflow.CountStep("triage") != 1 {
		t.Fatalf("step count = %d, want 1", got.Workflow.CountStep("triage"))
	}
	if len(got.Workflow.StepHistory) != 1 || got.Workflow.StepHistory[0].Status != "failed" {
		t.Fatalf("step history = %+v, want one failed triage record", got.Workflow.StepHistory)
	}
	if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "checkpoint-agent"); tracked {
		t.Fatal("checkpoint_failed agent step mapping was not cleared")
	}
}
func TestRescheduleRateLimitedAgent_RerunsParallelChild(t *testing.T) {
	store := newTestStoreWith(t, "test-parallel.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-parallel",
			CurrentStep: "plan",
			State:       ExecWaiting,
			Variables:   make(map[string]string),
			ParallelInflight: map[string]*ParallelChildren{
				"plan": {
					ParentStepID: "plan",
					Children: map[string]*ChildStatus{
						"plan_a": {Status: "pending", AgentID: "limited-agent", Provider: "claude"},
						"plan_b": {Status: "pending", AgentID: "other-agent", Provider: "codex"},
					},
				},
			},
		},
	})
	setWorkflowAgentRoute(t, tasks, "t1", "limited-agent", "plan_a")

	engine.RescheduleRateLimitedAgent("t1", "limited-agent")

	if agents.CallCount() != 1 {
		t.Fatalf("expected replacement parallel child start, got %d", agents.CallCount())
	}
	if got := agents.LastCall(); got.Prompt != "Plan A t1" {
		t.Fatalf("unexpected replacement call: %+v", got)
	}
	if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "limited-agent"); tracked {
		t.Fatal("rate-limited child step mapping was not cleared")
	}
	if spawnedStep, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "agent-1"); !tracked || spawnedStep != "plan_a" {
		t.Fatalf("replacement child mapping = (%q, %v), want plan_a/true", spawnedStep, tracked)
	}
	updated, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Workflow == nil || updated.Workflow.ParallelInflight == nil {
		t.Fatalf("workflow parallel state missing: %+v", updated.Workflow)
	}
	rec := updated.Workflow.ParallelInflight["plan"]
	if rec == nil || rec.Children == nil {
		t.Fatalf("parallel record missing: %+v", rec)
	}
	child := rec.Children["plan_a"]
	if child == nil {
		t.Fatal("child plan_a missing")
		return
	}
	if child.AgentID != "agent-1" || child.Status != "pending" {
		t.Fatalf("child status = %+v, want pending agent-1", child)
	}
}

func TestRescheduleRateLimitedAgent_ParallelChildWatchdogRetriesThenEscalates(t *testing.T) {
	tests := []struct {
		name       string
		retries    string
		wantStarts int
		wantStatus string
		wantReason string
		wantRetry  string
	}{
		{
			name:       "first watchdog rate limit reruns child",
			wantStarts: 1,
			wantStatus: "in-progress",
			wantReason: "",
			wantRetry:  "1",
		},
		{
			name:       "budget exhausted escalates instead of looping forever",
			retries:    "2",
			wantStarts: 0,
			wantStatus: "human-required",
			wantReason: "watchdog: rate limit retry budget exhausted after 2 clean re-dispatches",
			wantRetry:  "2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStoreWith(t, "test-parallel.yaml")
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewEngine(store, tasks, agents, discardLogger())

			vars := map[string]string{}
			if tc.retries != "" {
				vars[watchdogRateLimitRetryKey("plan_a")] = tc.retries
			}
			tasks.Put(TaskInfo{
				ID:           "t1",
				Status:       "in-progress",
				StatusReason: "watchdog: rate limit: org-level quota exhausted",
				AgentMode:    "headless",
				Workflow: &Execution{
					WorkflowID:  "test-parallel",
					CurrentStep: "plan",
					State:       ExecWaiting,
					Variables:   vars,
					ParallelInflight: map[string]*ParallelChildren{
						"plan": {
							ParentStepID: "plan",
							Children: map[string]*ChildStatus{
								"plan_a": {Status: "pending", AgentID: "limited-agent", Provider: "claude"},
								"plan_b": {Status: "pending", AgentID: "other-agent", Provider: "codex"},
							},
						},
					},
				},
			})
			setWorkflowAgentRoute(t, tasks, "t1", "limited-agent", "plan_a")

			engine.RescheduleRateLimitedAgent("t1", "limited-agent")

			if got := agents.CallCount(); got != tc.wantStarts {
				t.Fatalf("expected replacement agent starts = %d, got %d", tc.wantStarts, got)
			}
			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.StatusReason != tc.wantReason {
				t.Fatalf("status_reason = %q, want %q", got.StatusReason, tc.wantReason)
			}
			if got.Workflow.Variables[watchdogRateLimitRetryKey("plan_a")] != tc.wantRetry {
				t.Fatalf("rate-limit retry var = %q, want %q", got.Workflow.Variables[watchdogRateLimitRetryKey("plan_a")], tc.wantRetry)
			}
			if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "limited-agent"); tracked {
				t.Fatal("rate-limited child step mapping was not cleared")
			}
		})
	}
}

func TestResumeStalled_ParallelProviderUnhealthyLeavesChildPending(t *testing.T) {
	store := newTestStoreWith(t, "test-parallel.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetFailSpawn(&providerpkg.UnhealthyError{Provider: "claude", Reason: "rate_limited"})
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-parallel",
			CurrentStep: "plan",
			State:       ExecWaiting,
			Variables:   make(map[string]string),
			ParallelInflight: map[string]*ParallelChildren{
				"plan": {
					ParentStepID: "plan",
					Children: map[string]*ChildStatus{
						"plan_a": {Status: "pending"},
						"plan_b": {Status: "pending"},
					},
				},
			},
		},
	})

	engine.ResumeStalled()

	updated, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Workflow == nil || updated.Workflow.ParallelInflight == nil {
		t.Fatalf("workflow parallel state missing: %+v", updated.Workflow)
	}
	rec := updated.Workflow.ParallelInflight["plan"]
	if rec == nil || rec.Children == nil {
		t.Fatalf("parallel record missing: %+v", rec)
	}
	for id, child := range rec.Children {
		if child == nil {
			t.Fatalf("child %s missing", id)
			return
		}
		if child.Status != "pending" {
			t.Fatalf("child %s status = %q, want pending after transient provider block", id, child.Status)
		}
	}
	if updated.Status != "in-progress" {
		t.Fatalf("task status = %q, want in-progress", updated.Status)
	}
	if got := tasks.Reason("t1"); !strings.Contains(got, "provider claude unhealthy") {
		t.Fatalf("reason = %q, want provider unhealthy reason", got)
	}
}

func TestResumeStalled_FailoversRateLimitedProvider(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetProviderRateLimited(true)
	agents.SetProviderFailover(true)
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecRunning,
			Variables:   make(map[string]string),
		},
	})

	engine.ResumeStalled()

	if agents.CallCount() != 1 {
		t.Fatalf("expected 1 agent start when failover is available, got %d", agents.CallCount())
	}
}

func TestResumeStalled_SkipsWorkflowRetryUntil(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	wf := &Execution{
		WorkflowID:  "test-simple",
		CurrentStep: "implement",
		State:       ExecWaiting,
		Variables: map[string]string{
			workflowRetryAfterVar: time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		},
	}
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow:  wf,
	})

	engine.ResumeStalled()
	if agents.CallCount() != 0 {
		t.Fatalf("agent starts before retry window = %d, want 0", agents.CallCount())
	}

	wf.Variables[workflowRetryAfterVar] = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow:  wf,
	})

	engine.ResumeStalled()
	if agents.CallCount() != 1 {
		t.Fatalf("agent starts after retry window = %d, want 1", agents.CallCount())
	}
}

// TestResumeStalled_SkipLogsPromotedToThrottledInfo proves ResumeStalled's
// skip-reason logs are visible at the default (INFO) log level instead of
// being silently swallowed at Debug: a dropped/skipped resume was previously
// invisible in production logs. The first occurrence of a given skip under a
// task logs at INFO; identical repeats on later ticks are throttled to Debug
// so a long-lived cooldown (e.g. a multi-hour retry_after window) doesn't
// flood the log with a duplicate INFO line every tick.
func TestResumeStalled_SkipLogsPromotedToThrottledInfo(t *testing.T) {
	var records []slog.Record
	logger := slog.New(&demotionRecordHandler{records: &records})

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, logger)

	retryAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	wf := &Execution{
		WorkflowID:  "test-simple",
		CurrentStep: "implement",
		State:       ExecWaiting,
		Variables: map[string]string{
			workflowRetryAfterVar: retryAt,
		},
	}
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow:  wf,
	})

	skipRecords := func() []slog.Record {
		var out []slog.Record
		for _, r := range records {
			if r.Message == "workflow.resume-stalled.skip" {
				out = append(out, r)
			}
		}
		return out
	}

	engine.ResumeStalled()
	first := skipRecords()
	if len(first) != 1 {
		t.Fatalf("got %d skip records after first tick, want 1: %+v", len(first), first)
	}
	if first[0].Level != slog.LevelInfo {
		t.Fatalf("first skip level = %v, want Info", first[0].Level)
	}
	if got := recordAttr(first[0], "reason"); got != "retry_after" {
		t.Fatalf("reason = %q, want retry_after", got)
	}

	records = nil
	engine.ResumeStalled()
	repeat := skipRecords()
	if len(repeat) != 1 {
		t.Fatalf("got %d skip records after repeat tick, want 1: %+v", len(repeat), repeat)
	}
	if repeat[0].Level != slog.LevelDebug {
		t.Fatalf("repeat skip level = %v, want Debug (throttled)", repeat[0].Level)
	}
}

func TestResumeStalled_SkipWaitHuman(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	// Task at wait_human step.
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "plan-review",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "review_plan",
			State:       ExecWaiting,
			Variables:   make(map[string]string),
		},
	})

	engine.ResumeStalled()

	// Should NOT start any agent.
	if agents.CallCount() != 0 {
		t.Fatalf("expected 0 agent starts for wait_human, got %d", agents.CallCount())
	}
}

// TestResumeStalled_SkipsHumanRequired reproduces the post-restart dispatch bug
// where a review task was parked on human-required but ResumeStalled
// re-dispatched its workflow's run_agent step after restart, overriding the
// triage verdict.
func TestResumeStalled_SkipsHumanRequired(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	// Simulate a review task whose pr-review workflow is at the implement
	// (run_agent) step but whose status was flipped to human-required before
	// the restart.
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "human-required",
		AgentMode: "headless",
		Tags:      []string{"review"},
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecRunning,
			Variables:   make(map[string]string),
		},
	})

	engine.ResumeStalled()

	// Must NOT dispatch an agent: human-required overrides the workflow.
	if agents.CallCount() != 0 {
		t.Fatalf("expected 0 agent starts for human-required task, got %d", agents.CallCount())
	}
}

func TestResumeStalled_SkipsBlockedStatus(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "blocked",
		StatusReason: "watchdog: zero-output startup retry budget exhausted after 3 identical attempts",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecFailed,
			Variables: map[string]string{
				watchdogRateLimitRetryKey("implement"): strconv.Itoa(maxWatchdogRateLimitRetries),
			},
		},
	})

	engine.ResumeStalled()

	if agents.CallCount() != 0 {
		t.Fatalf("expected 0 agent starts for blocked task, got %d", agents.CallCount())
	}
}

func TestResumeStalled_SkipsDoneStatus(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	// Simulate a task whose PR merged (flipping Status to done) while its
	// workflow was still paused Running/Waiting at an earlier step — e.g.
	// advanceClosedTaskPRsWithFetch racing ResumeStalled before the workflow
	// is cancelled. Resuming would rebase the already-merged branch against
	// origin/main and self-conflict, flipping the done task back to
	// human-required.
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "done",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecRunning,
			Variables:   make(map[string]string),
		},
	})

	engine.ResumeStalled()

	if agents.CallCount() != 0 {
		t.Fatalf("expected 0 agent starts for done task, got %d", agents.CallCount())
	}
}

func TestResumeStalled_SkipsTerminalStatusAfterFreshRead(t *testing.T) {
	tests := []struct {
		name   string
		status string
	}{
		{name: "done", status: "done"},
		{name: "cancelled", status: "cancelled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewEngine(store, tasks, agents, discardLogger())

			tasks.Put(TaskInfo{
				ID:        "t1",
				Status:    "in-review",
				AgentMode: "headless",
				Workflow: &Execution{
					WorkflowID:  "test-simple",
					CurrentStep: "implement",
					State:       ExecRunning,
					Variables:   make(map[string]string),
				},
			})
			tasks.SetGetTaskHook(func(id string, task *TaskInfo, count int) {
				if id == "t1" && count == 1 {
					task.Status = tc.status
				}
			})

			engine.ResumeStalled()

			if got := agents.CallCount(); got != 0 {
				t.Fatalf("expected 0 agent starts after status flipped to %s, got %d", tc.status, got)
			}
			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != tc.status {
				t.Fatalf("status = %q, want %q", got.Status, tc.status)
			}
		})
	}
}

func TestRescheduleRateLimitedAgent_ParallelChildSkipsTerminalStatusAfterFreshRead(t *testing.T) {
	tests := []struct {
		name   string
		status string
	}{
		{name: "done", status: "done"},
		{name: "cancelled", status: "cancelled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStoreWith(t, "test-parallel.yaml")
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewEngine(store, tasks, agents, discardLogger())

			tasks.Put(TaskInfo{
				ID:        "t1",
				Status:    "in-progress",
				AgentMode: "headless",
				Workflow: &Execution{
					WorkflowID:  "test-parallel",
					CurrentStep: "plan",
					State:       ExecWaiting,
					Variables:   make(map[string]string),
					ParallelInflight: map[string]*ParallelChildren{
						"plan": {
							ParentStepID: "plan",
							Children: map[string]*ChildStatus{
								"plan_a": {Status: "pending", AgentID: "limited-agent", Provider: "claude"},
								"plan_b": {Status: "pending", AgentID: "other-agent", Provider: "codex"},
							},
						},
					},
				},
			})
			setWorkflowAgentRoute(t, tasks, "t1", "limited-agent", "plan_a")
			tasks.SetGetTaskHook(func(id string, task *TaskInfo, count int) {
				if id == "t1" && count == 2 {
					task.Status = tc.status
				}
			})

			engine.RescheduleRateLimitedAgent("t1", "limited-agent")

			if got := agents.CallCount(); got != 0 {
				t.Fatalf("expected 0 replacement agent starts after status flipped to %s, got %d", tc.status, got)
			}
			if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "limited-agent"); tracked {
				t.Fatal("rate-limited child step mapping was not cleared")
			}
			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != tc.status {
				t.Fatalf("status = %q, want %q", got.Status, tc.status)
			}
		})
	}
}

func TestHandleHumanAction_NotWaiting(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "in-progress",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecRunning,
			Variables:   make(map[string]string),
		},
	})

	err := engine.HandleHumanAction("t1", "approve", nil)
	if err == nil {
		t.Fatal("expected error for non-waiting task")
	}
	var ce *ClientError
	if !errors.As(err, &ce) {
		t.Fatalf("want *ClientError so httpapi surfaces it as a 4xx instead of a sanitized 500, got %T: %v", err, err)
	}
	if ce.HTTPStatus() != http.StatusConflict {
		t.Fatalf("status = %d, want %d", ce.HTTPStatus(), http.StatusConflict)
	}
}

func TestHandleHumanAction_InvalidActionAlreadyWaitingDoesNotMutate(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "plan-review",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "review_plan",
			State:       ExecWaiting,
			Variables:   map[string]string{"existing": "value"},
			StepHistory: []StepRecord{{StepID: "plan", Status: "completed"}},
		},
	})

	err := engine.HandleHumanAction("t1", "bogus", nil)
	if err == nil || !strings.Contains(err.Error(), "invalid human action") {
		t.Fatalf("HandleHumanAction error = %v, want invalid human action", err)
	}
	var ce *ClientError
	if !errors.As(err, &ce) {
		t.Fatalf("want *ClientError so httpapi surfaces it as a 4xx instead of a sanitized 500, got %T: %v", err, err)
	}
	if ce.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", ce.HTTPStatus(), http.StatusBadRequest)
	}

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow.CurrentStep != "review_plan" {
		t.Fatalf("CurrentStep = %q, want review_plan", got.Workflow.CurrentStep)
	}
	if got.Workflow.State != ExecWaiting {
		t.Fatalf("State = %q, want %q", got.Workflow.State, ExecWaiting)
	}
	if _, ok := got.Workflow.Variables["human_action"]; ok {
		t.Fatal("human_action var was set for rejected action")
	}
	if got.Workflow.Variables["existing"] != "value" {
		t.Fatalf("existing var = %q, want value", got.Workflow.Variables["existing"])
	}
	if len(got.Workflow.StepHistory) != 1 || got.Workflow.StepHistory[0].StepID != "plan" {
		t.Fatalf("StepHistory changed: %+v", got.Workflow.StepHistory)
	}
}

func TestConcurrentAdvance(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

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

func TestStartWorkflow_InvalidWorkflowID(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})

	err := engine.StartWorkflow("t1", "nonexistent-workflow")
	if err == nil {
		t.Fatal("expected error for invalid workflow ID")
	}
}

func TestAdvanceStep_UnknownStepID(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// An advance for a step that does not match the workflow's current step
	// is a stale completion (e.g. a duplicate agent from a ResumeStalled race,
	// or a stray callback after the workflow advanced). The engine must
	// silently no-op instead of crashing or mutating step history — that
	// guard is what stops a second plan agent from driving review_plan into
	// ExecFailed when its delayed completion arrives after the human gate.
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "nonexistent-step", Status: "completed"}); err != nil {
		t.Fatalf("stale stepID should be a no-op, got err: %v", err)
	}

	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "triage" {
		t.Errorf("CurrentStep = %q, want unchanged triage", ti.Workflow.CurrentStep)
	}
	if got := len(ti.Workflow.StepHistory); got != 0 {
		t.Errorf("step history len = %d, want 0 — stale advance must not append", got)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Errorf("state = %q, want ExecWaiting (unchanged)", ti.Workflow.State)
	}
}

func TestAdvanceStep_TaskWithoutWorkflow(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})

	err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"})
	if err == nil {
		t.Fatal("expected error for task without workflow")
	}
}

func TestResumeStalled_SkipsTaskWithRunningAgent(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecRunning,
			Variables:   make(map[string]string),
		},
	})
	// Simulate an agent already running.
	_, _, _, _ = agents.StartAgent("t1", "implementation", "headless", "sonnet", "", "test", "", nil, false, false, "", "", AgentAssignment{})

	initialCalls := agents.CallCount()
	engine.ResumeStalled()

	if agents.CallCount() != initialCalls {
		t.Fatal("ResumeStalled should not start another agent when one is already running")
	}
}

func TestResumeStalled_SkipsCompletedWorkflow(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	now := time.Now().UTC()
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "done",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "",
			State:       ExecCompleted,
			CompletedAt: &now,
			Variables:   make(map[string]string),
		},
	})

	engine.ResumeStalled()

	if agents.CallCount() != 0 {
		t.Fatal("ResumeStalled should skip completed workflows")
	}
}

func TestShellStep_ExecutesCommand(t *testing.T) {
	// Test the shell step directly using a simple echo command.
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	step := &Step{
		ID:   "shell1",
		Type: StepShell,
		Config: StepConfig{
			Command: "echo hello-from-shell",
		},
	}

	ctx := TemplateContext{
		Task: TaskInfo{ID: "t1", Title: "test"},
		Step: *step,
		Vars: make(map[string]string),
	}

	output, err := engine.execShell(step, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if output.Status != "completed" {
		t.Fatalf("expected completed, got %q", output.Status)
	}
	if output.Output != "hello-from-shell\n" {
		t.Fatalf("expected 'hello-from-shell\\n', got %q", output.Output)
	}
}

// TestShellStep_StdinReaderExitsOnEOF covers a subtle deadlock: execShell
// does not wire stdin, so commands that call `read` or `cat` inherit a
// nil/closed stdin and should exit immediately with EOF. A regression that
// passed through os.Stdin (or left the pipe dangling) would cause the shell
// step to hang for the full shellTimeout (30s). The 5-second deadline here
// proves the command exits promptly.
func TestShellStep_StdinReaderExitsOnEOF(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	step := &Step{
		ID:   "stdin-reader",
		Type: StepShell,
		Config: StepConfig{
			// `read` exits non-zero on EOF. `cat` exits 0 immediately since
			// its stdin is empty. Both should be fast.
			Command: "cat",
		},
	}
	ctx := TemplateContext{
		Task: TaskInfo{ID: "t1"},
		Step: *step,
		Vars: make(map[string]string),
	}

	done := make(chan error, 1)
	go func() {
		_, err := engine.execShell(step, ctx)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execShell: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("execShell hung on stdin-reading command — sybra provides no stdin, `cat` should EOF immediately")
	}
}

// TestShellStep_ContextCancelKillsCommand verifies that cancelling the
// engine's parent context terminates a long-running shell step promptly
// rather than waiting out the 30s shellTimeout. execShell derives its own
// context via context.WithTimeout(e.ctx, shellTimeout); cancelling e.ctx
// must propagate down and kill the subprocess via exec.CommandContext.
// A regression that used context.Background() instead of e.ctx would
// leave the command running after app shutdown.
func TestShellStep_ContextCancelKillsCommand(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	parentCtx, cancel := context.WithCancel(context.Background())
	engine.SetContext(parentCtx)

	step := &Step{
		ID:   "long-sleep",
		Type: StepShell,
		Config: StepConfig{
			Command: "sleep 30",
		},
	}
	ctx := TemplateContext{
		Task: TaskInfo{ID: "t1"},
		Step: *step,
		Vars: make(map[string]string),
	}

	// Cancel after 200ms; the sleep would otherwise run 30 seconds.
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	output, err := engine.execShell(step, ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("execShell: %v", err)
	}
	// Killed subprocess is "failed", not "completed".
	if output.Status != "failed" {
		t.Errorf("status = %q, want failed (subprocess killed by ctx cancel)", output.Status)
	}
	// Must return within a handful of seconds — certainly well under 30s
	// shellTimeout. 10s is plenty of slack for slow CI.
	if elapsed > 10*time.Second {
		t.Errorf("execShell took %v after ctx cancel — should return promptly", elapsed)
	}
}

func TestShellStep_FailingCommandSetsStatusFailed(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	step := &Step{
		ID:   "shell1",
		Type: StepShell,
		Config: StepConfig{
			Command: "exit 1",
		},
	}

	ctx := TemplateContext{
		Task: TaskInfo{ID: "t1"},
		Step: *step,
		Vars: make(map[string]string),
	}

	output, err := engine.execShell(step, ctx)
	if err != nil {
		t.Fatal(err) // execShell doesn't return error on command failure
	}
	if output.Status != "failed" {
		t.Fatalf("expected failed, got %q", output.Status)
	}
}

func TestShellStep_EmptyRenderedDirFailsClosed(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	step := &Step{
		ID:   "shell-empty-dir",
		Type: StepShell,
		Config: StepConfig{
			Command: "pwd",
			Dir:     "{{getvar .Vars \"missing_dir\"}}",
		},
	}

	ctx := TemplateContext{
		Task: TaskInfo{ID: "t1"},
		Step: *step,
		Vars: make(map[string]string),
	}

	_, err := engine.execShell(step, ctx)
	if err == nil {
		t.Fatal("expected error for empty rendered dir")
	}
	if !strings.Contains(err.Error(), "resolved to empty path") {
		t.Fatalf("err = %v, want empty-path failure", err)
	}
}

func TestExecRunAgent_DefaultModeAndModel(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})

	step := &Step{
		ID:   "agent1",
		Type: StepRunAgent,
		Config: StepConfig{
			Role:   "triage",
			Prompt: "test prompt",
			// Mode and Model intentionally empty.
		},
	}

	wfExec := &Execution{
		WorkflowID: "test-simple",
		State:      ExecRunning,
		Variables:  make(map[string]string),
	}

	ctx := TemplateContext{
		Task: TaskInfo{ID: "t1"},
		Step: *step,
		Vars: wfExec.Variables,
	}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}

	call := agents.LastCall()
	if call.Mode != "headless" {
		t.Errorf("expected default mode 'headless', got %q", call.Mode)
	}
	if call.Model != "sonnet" {
		t.Errorf("expected default model 'sonnet', got %q", call.Model)
	}
}

func TestResolveRunAgentModel(t *testing.T) {
	t.Parallel()

	ctx := TemplateContext{
		Task: TaskInfo{ID: "t1"},
		Vars: map[string]string{},
	}

	if got := resolveRunAgentModel("", ctx); got != "sonnet" {
		t.Fatalf("resolveRunAgentModel(empty) = %q, want sonnet", got)
	}

	ctx.Vars[verifyRetryModelVar] = "expensive"
	model := `  {{if getvar .Vars "verify_retry_model"}}{{getvar .Vars "verify_retry_model"}}{{else}}cheap{{end}}  `
	if got := resolveRunAgentModel(model, ctx); got != "expensive" {
		t.Fatalf("resolveRunAgentModel(template with %s) = %q, want expensive", verifyRetryModelVar, got)
	}

	delete(ctx.Vars, verifyRetryModelVar)
	if got := resolveRunAgentModel(model, ctx); got != "cheap" {
		t.Fatalf("resolveRunAgentModel(template without %s) = %q, want cheap", verifyRetryModelVar, got)
	}
}

func TestExecRunAgent_PersistsPreparedWorktreeDir(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", AgentMode: "headless"})
	step := &Step{
		ID:   "code_review",
		Type: StepRunAgent,
		Config: StepConfig{
			Role:          "review",
			Prompt:        "review",
			NeedsWorktree: true,
		},
	}
	wfExec := &Execution{
		WorkflowID: "test-simple",
		State:      ExecRunning,
		Variables:  map[string]string{},
	}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(os.TempDir(), "sybra-test-t1")
	if got.Workflow.Variables[WorkflowVarDir] != wantDir {
		t.Fatalf("%s = %q, want %q", WorkflowVarDir, got.Workflow.Variables[WorkflowVarDir], wantDir)
	}
}

func TestExecRunAgent_UsesConfiguredScratchDirForPlan(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	scratchDir := t.TempDir()
	tasks.Put(TaskInfo{ID: "t1", Status: "plan-needed", AgentMode: "headless"})
	step := &Step{
		ID:   "plan",
		Type: StepRunAgent,
		Config: StepConfig{
			Role:          "plan",
			Prompt:        "plan",
			NeedsWorktree: true,
			Dir:           scratchDir,
		},
	}
	wfExec := &Execution{
		WorkflowID: "plan-scratch",
		State:      ExecRunning,
		Variables: map[string]string{
			WorkflowVarDir: filepath.Join(os.TempDir(), "sybra-test-t1"),
		},
	}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}

	if len(agents.calls) != 1 {
		t.Fatalf("StartAgent calls = %d, want 1", len(agents.calls))
	}
	if agents.calls[0].Dir != scratchDir {
		t.Fatalf("StartAgent dir = %q, want configured scratch dir %q", agents.calls[0].Dir, scratchDir)
	}

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow.Variables[WorkflowVarDir] != scratchDir {
		t.Fatalf("%s = %q, want configured scratch dir %q", WorkflowVarDir, got.Workflow.Variables[WorkflowVarDir], scratchDir)
	}
}

func TestExecRunAgent_ABTestingOverridesProviderModel(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	enabled := true
	engine.SetABTestingConfig(abtest.Config{
		Enabled: &enabled,
		Experiments: []abtest.Experiment{{
			ID:             "exp",
			AssignmentUnit: "stage",
			Roles:          []string{"implementation"},
			Variants: []abtest.Variant{{
				ID: "codex-gpt", Provider: "codex", Model: "gpt-5.5", ReasoningEffort: "high", Weight: 1,
			}},
		}},
	})
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	step := &Step{
		ID:   "implement",
		Type: StepRunAgent,
		Config: StepConfig{
			Role:   "implementation",
			Model:  "sonnet",
			Prompt: "test prompt",
		},
	}
	wfExec := &Execution{WorkflowID: "test-simple", State: ExecRunning, Variables: make(map[string]string)}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}
	call := agents.LastCall()
	if call.Provider != "codex" || call.Model != "gpt-5.5" {
		t.Fatalf("provider/model = %q/%q, want codex/gpt-5.5", call.Provider, call.Model)
	}
	if call.Assignment.ExperimentID != "exp" || call.Assignment.VariantID != "codex-gpt" || call.Assignment.ReasoningEffort != "high" {
		t.Fatalf("assignment = %+v", call.Assignment)
	}
	if call.Assignment.RoutingReason != "ab" {
		t.Fatalf("assignment routing reason = %q, want ab", call.Assignment.RoutingReason)
	}
}

func TestExecRunAgent_DefaultProviderPathWinsWhenABTestingOmitted(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	engine.SetABTestingConfig(abtest.DefaultConfig())
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	step := &Step{
		ID:   "implement",
		Type: StepRunAgent,
		Config: StepConfig{
			Role:   "implementation",
			Model:  "sonnet",
			Prompt: "test prompt",
		},
	}
	wfExec := &Execution{WorkflowID: "test-simple", State: ExecRunning, Variables: make(map[string]string)}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}
	call := agents.LastCall()
	if call.Provider != "" || call.Model != "sonnet" {
		t.Fatalf("provider/model = %q/%q, want empty/sonnet so manager default-provider routing stays in control", call.Provider, call.Model)
	}
	if call.Assignment.ExperimentID != "" || call.Assignment.VariantID != "" {
		t.Fatalf("assignment = %+v, want no A/B assignment when ab_testing.enabled is omitted", call.Assignment)
	}
	if call.Assignment.RoutingReason != "" {
		t.Fatalf("assignment routing reason = %q, want empty", call.Assignment.RoutingReason)
	}
}

func TestExecRunAgentVariantPrompt(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	enabled := true
	engine.SetABTestingConfig(abtest.Config{Enabled: &enabled, Experiments: []abtest.Experiment{{
		ID:             "prompt-exp",
		Kind:           "prompt",
		AssignmentUnit: "stage",
		Roles:          []string{"implementation"},
		Subject:        &abtest.Subject{StepID: "implement"},
		Variants: []abtest.Variant{{
			ID: "copy-v2", Provider: "claude", Model: "sonnet", Weight: 1,
			PromptTransform: &abtest.PromptTransform{Op: "template", Text: "variant {{.Task.ID}} {{.Step.ID}}"},
		}},
	}}})
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	step := &Step{ID: "implement", Type: StepRunAgent, Config: StepConfig{Role: "implementation", Prompt: "control {{.Task.ID}}"}}
	wfExec := &Execution{WorkflowID: "test-simple", State: ExecRunning, Variables: map[string]string{}}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}
	call := agents.LastCall()
	if call.Prompt != "variant t1 implement" {
		t.Fatalf("Prompt = %q", call.Prompt)
	}
	if call.Assignment.PromptTransform == nil || call.Assignment.PromptTransform.Op != "template" {
		t.Fatalf("assignment prompt transform = %+v", call.Assignment.PromptTransform)
	}
}

// TestExecRunAgent_EvalGateBlocksFailingDigestedVariant wires a real
// prompteval.Gate (not a nil predicate) into the engine and proves a stored
// FAIL verdict for a digested variant keeps it out of online A/B enrollment
// on the production selectABVariant path — closing the gap where
// abtest.SelectEligibleWithEval existed but nothing in the dispatch hot path
// called it.
func TestExecRunAgent_EvalGateBlocksFailingDigestedVariant(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	evalStore := prompteval.New(t.TempDir())
	failingDigest := prompteval.Digest([]byte("failing prompt"))
	if err := evalStore.Write(prompteval.VariantVerdict{
		VariantID: "failing-variant",
		Digest:    failingDigest,
		Status:    prompteval.StatusFail,
		Runner:    "native",
	}); err != nil {
		t.Fatalf("Write verdict: %v", err)
	}

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	engine.SetEvalGate(prompteval.NewGate(evalStore, config.OfflineEvalConfig{}))
	enabled := true
	engine.SetABTestingConfig(abtest.Config{Enabled: &enabled, Experiments: []abtest.Experiment{{
		ID:             "gated-exp",
		AssignmentUnit: "stage",
		Roles:          []string{"implementation"},
		Variants: []abtest.Variant{
			{ID: "failing-variant", Provider: "codex", Model: "gpt-5.5", Digest: failingDigest, Weight: 1},
			{ID: "clean-variant", Provider: "claude", Model: "sonnet", Weight: 1},
		},
	}}})
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	step := &Step{ID: "implement", Type: StepRunAgent, Config: StepConfig{Role: "implementation", Prompt: "test prompt"}}
	wfExec := &Execution{WorkflowID: "test-simple", State: ExecRunning, Variables: map[string]string{}}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}
	call := agents.LastCall()
	if call.Assignment.VariantID != "clean-variant" {
		t.Fatalf("assignment = %+v, want clean-variant (failing-variant must be excluded by the eval gate)", call.Assignment)
	}
}

func TestExecRunAgentSkillAlias(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	enabled := true
	engine.SetABTestingConfig(abtest.Config{Enabled: &enabled, Experiments: []abtest.Experiment{{
		ID:             "skill-exp",
		Kind:           "skill",
		AssignmentUnit: "stage",
		Roles:          []string{"implementation"},
		Subject:        &abtest.Subject{StepID: "implement", SkillName: "sybra-test"},
		Variants: []abtest.Variant{{
			ID: "skill-v2", Provider: "codex", Model: "gpt-5.5", Weight: 1,
			SkillAliases: map[string]string{"sybra-test": "sybra-test-v2"},
		}},
	}}})
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	step := &Step{ID: "implement", Type: StepRunAgent, Config: StepConfig{Role: "implementation", Prompt: "Run /sybra-test, not /tmp/sybra-test.md."}}
	wfExec := &Execution{WorkflowID: "test-simple", State: ExecRunning, Variables: map[string]string{}}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}
	call := agents.LastCall()
	if call.Prompt != "Run /sybra-test-v2, not /tmp/sybra-test.md." {
		t.Fatalf("Prompt = %q", call.Prompt)
	}
	if got := call.Assignment.SkillAliases["sybra-test"]; got != "sybra-test-v2" {
		t.Fatalf("assignment alias = %q", got)
	}
}

func TestExecRunAgentVariantSubjectMismatchDoesNotApplyPayload(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	tests := []struct {
		name    string
		subject *abtest.Subject
		step    Step
		wfID    string
	}{
		{
			name:    "step",
			subject: &abtest.Subject{StepID: "implement", SkillName: "sybra-test"},
			step:    Step{ID: "evaluate", Type: StepRunAgent, Config: StepConfig{Role: "implementation", Prompt: "control /sybra-test"}},
			wfID:    "test-simple",
		},
		{
			name:    "workflow",
			subject: &abtest.Subject{WorkflowID: "target-workflow", SkillName: "sybra-test"},
			step:    Step{ID: "implement", Type: StepRunAgent, Config: StepConfig{Role: "implementation", Prompt: "control /sybra-test"}},
			wfID:    "other-workflow",
		},
		{
			name:    "skill",
			subject: &abtest.Subject{StepID: "implement", SkillName: "sybra-test"},
			step:    Step{ID: "implement", Type: StepRunAgent, Config: StepConfig{Role: "implementation", Prompt: "control /other-skill"}},
			wfID:    "test-simple",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewEngine(store, tasks, agents, discardLogger())
			enabled := true
			engine.SetABTestingConfig(abtest.Config{Enabled: &enabled, Experiments: []abtest.Experiment{{
				ID:             "subject-exp",
				Kind:           "skill",
				AssignmentUnit: "stage",
				Roles:          []string{"implementation"},
				Subject:        tt.subject,
				Variants: []abtest.Variant{{
					ID: "variant", Provider: "claude", Model: "sonnet", Weight: 1,
					PromptTransform: &abtest.PromptTransform{Op: "prepend", Text: "variant: "},
					SkillAliases:    map[string]string{"sybra-test": "sybra-test-v2"},
				}},
			}}})
			tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
			wfExec := &Execution{WorkflowID: tt.wfID, State: ExecRunning, Variables: map[string]string{}}
			ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: tt.step, Vars: wfExec.Variables}

			if err := engine.execRunAgent("t1", &tt.step, wfExec, ctx); err != nil {
				t.Fatal(err)
			}
			call := agents.LastCall()
			if call.Assignment.ExperimentID != "" {
				t.Fatalf("assignment applied despite subject mismatch: %+v", call.Assignment)
			}
			if call.Prompt != tt.step.Config.Prompt {
				t.Fatalf("prompt = %q, want %q", call.Prompt, tt.step.Config.Prompt)
			}
		})
	}
}

func TestExecRunAgentReuseAgentSkillAlias(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	enabled := true
	engine.SetABTestingConfig(abtest.Config{Enabled: &enabled, Experiments: []abtest.Experiment{{
		ID:             "skill-exp",
		Kind:           "skill",
		AssignmentUnit: "stage",
		Roles:          []string{"implementation"},
		Subject:        &abtest.Subject{StepID: "followup", SkillName: "sybra-test"},
		Variants: []abtest.Variant{{
			ID: "skill-v2", Provider: "claude", Model: "sonnet", Weight: 1,
			SkillAliases: map[string]string{"sybra-test": "sybra-test-v2"},
		}},
	}}})
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if _, _, _, err := agents.StartAgent("t1", "implementation", "headless", "sonnet", "claude", "initial", "", nil, false, false, "", "", AgentAssignment{}); err != nil {
		t.Fatal(err)
	}
	step := &Step{ID: "followup", Type: StepRunAgent, Config: StepConfig{Role: "implementation", ReuseAgent: true, Prompt: "Run /sybra-test"}}
	wfExec := &Execution{WorkflowID: "test-simple", State: ExecRunning, Variables: map[string]string{}}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}
	sent := agents.SentPrompts()
	if len(sent) != 1 {
		t.Fatalf("SendPrompt count = %d, want 1", len(sent))
	}
	if sent[0].Message != "Run /sybra-test-v2" {
		t.Fatalf("sent prompt = %q", sent[0].Message)
	}
}

func TestSelectABVariantPropagatesExperimentKind(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	enabled := true
	engine.SetABTestingConfig(abtest.Config{
		Enabled: &enabled,
		Experiments: []abtest.Experiment{{
			ID:             "prompt-exp",
			Kind:           "prompt",
			AssignmentUnit: "stage",
			Roles:          []string{"implementation"},
			Subject:        &abtest.Subject{StepID: "implement"},
			Variants: []abtest.Variant{{
				ID: "copy-v2", Provider: "claude", Model: "sonnet", Weight: 1,
			}},
		}},
	})

	assignment, ok, err := engine.selectABVariant(abtest.SelectionContext{
		TaskID:     "t1",
		WorkflowID: "test-simple",
		Role:       "implementation",
		StepID:     "implement",
		Prompt:     "Run /sybra-test",
	})
	if err != nil || !ok {
		t.Fatalf("selectABVariant ok=%v err=%v", ok, err)
	}
	if assignment.Kind != "prompt" {
		t.Fatalf("Kind = %q, want prompt", assignment.Kind)
	}
}

func TestExecRunAgent_ABTestingSkipsRateLimitedProvider(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetProviderRateLimitedFor("codex", true)
	engine := NewEngine(store, tasks, agents, discardLogger())
	enabled := true
	engine.SetABTestingConfig(abtest.Config{
		Enabled: &enabled,
		Experiments: []abtest.Experiment{{
			ID:             "exp",
			AssignmentUnit: "stage",
			Roles:          []string{"implementation"},
			Variants: []abtest.Variant{
				{ID: "codex-gpt", Provider: "codex", Model: "gpt-5.5", Weight: 1},
				{ID: "claude-opus", Provider: "claude", Model: "opus", Weight: 1},
			},
		}},
	})
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	step := &Step{
		ID:   "implement",
		Type: StepRunAgent,
		Config: StepConfig{
			Role:   "implementation",
			Prompt: "test prompt",
		},
	}
	wfExec := &Execution{WorkflowID: "test-simple", State: ExecRunning, Variables: make(map[string]string)}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}
	call := agents.LastCall()
	if call.Provider == "codex" {
		t.Fatalf("rate-limited provider was selected: %+v", call)
	}
	if call.Provider != "claude" || call.Assignment.VariantID != "claude-opus" {
		t.Fatalf("provider/assignment = %q/%q, want claude/claude-opus", call.Provider, call.Assignment.VariantID)
	}
}

func TestExecRunAgent_ABTestingSkipsConfigDisabledProvider(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	// copilot's CLI is on PATH and it isn't rate-limited, but it is
	// administratively disabled via config — the health gate reports it
	// unhealthy. selectABVariant must not deterministically wedge every
	// task on this step onto a provider that will always fail to start.
	agents.SetProviderUnhealthy("copilot", true)
	engine := NewEngine(store, tasks, agents, discardLogger())
	enabled := true
	engine.SetABTestingConfig(abtest.Config{
		Enabled: &enabled,
		Experiments: []abtest.Experiment{{
			ID:             "exp",
			AssignmentUnit: "stage",
			Roles:          []string{"implementation"},
			Variants: []abtest.Variant{
				{ID: "copilot-variant", Provider: "copilot", Model: "gpt-5.5", Weight: 1},
				{ID: "claude-opus", Provider: "claude", Model: "opus", Weight: 1},
			},
		}},
	})
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	step := &Step{
		ID:   "implement",
		Type: StepRunAgent,
		Config: StepConfig{
			Role:   "implementation",
			Prompt: "test prompt",
		},
	}
	wfExec := &Execution{WorkflowID: "test-simple", State: ExecRunning, Variables: make(map[string]string)}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}
	call := agents.LastCall()
	if call.Provider == "copilot" {
		t.Fatalf("config-disabled provider was selected: %+v", call)
	}
	if call.Provider != "claude" || call.Assignment.VariantID != "claude-opus" {
		t.Fatalf("provider/assignment = %q/%q, want claude/claude-opus", call.Provider, call.Assignment.VariantID)
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

// TestExecRunAgent_ProviderDemotionEmitsThrottledSignal proves selection-time
// provider filtering (here: rate limiting) that changes the A/B outcome
// surfaces a first-class demotion signal — wanted/selected/reason — logged at
// Error on first occurrence and throttled to Debug on identical repeats, so a
// sustained rate limit does not flood the log with duplicate errors.
func TestExecRunAgent_ProviderDemotionEmitsThrottledSignal(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	var records []slog.Record
	logger := slog.New(&demotionRecordHandler{records: &records})

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetProviderRateLimitedFor("codex", true)
	engine := NewEngine(store, tasks, agents, logger)
	enabled := true
	// codex carries almost all the weight so the unfiltered ("wanted")
	// selection is codex with overwhelming probability for any task ID,
	// making the test deterministic without hand-computing the hash.
	engine.SetABTestingConfig(abtest.Config{
		Enabled: &enabled,
		Experiments: []abtest.Experiment{{
			ID:             "exp",
			AssignmentUnit: "stage",
			Roles:          []string{"implementation"},
			Variants: []abtest.Variant{
				{ID: "codex-variant", Provider: "codex", Model: "gpt-5.5", Weight: 100},
				{ID: "claude-variant", Provider: "claude", Model: "opus", Weight: 1},
			},
		}},
	})
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	step := &Step{ID: "implement", Type: StepRunAgent, Config: StepConfig{Role: "implementation", Prompt: "test prompt"}}

	runOnce := func(taskID string) {
		wfExec := &Execution{WorkflowID: "test-simple", State: ExecRunning, Variables: make(map[string]string)}
		ctx := TemplateContext{Task: TaskInfo{ID: taskID}, Step: *step, Vars: wfExec.Variables}
		if err := engine.execRunAgent(taskID, step, wfExec, ctx); err != nil {
			t.Fatal(err)
		}
	}
	runOnce("t1")
	call := agents.LastCall()
	if call.Provider != "claude" {
		t.Fatalf("provider = %q, want claude (codex is rate-limited)", call.Provider)
	}

	var demotions []slog.Record
	for _, r := range records {
		if r.Message == "workflow.ab.provider_demoted" {
			demotions = append(demotions, r)
		}
	}
	if len(demotions) != 1 {
		t.Fatalf("got %d demotion records after first run, want 1: %+v", len(demotions), demotions)
	}
	first := demotions[0]
	if first.Level != slog.LevelError {
		t.Fatalf("first demotion level = %v, want Error", first.Level)
	}
	if got := recordAttr(first, "wanted_provider"); got != "codex" {
		t.Fatalf("wanted_provider = %q, want codex", got)
	}
	if got := recordAttr(first, "selected_provider"); got != "claude" {
		t.Fatalf("selected_provider = %q, want claude", got)
	}
	if got := recordAttr(first, "reason"); got != "rate_limited" {
		t.Fatalf("reason = %q, want rate_limited", got)
	}
	if got := recordAttr(first, "task_id"); got != "t1" {
		t.Fatalf("task_id = %q, want t1", got)
	}

	// A second identical demotion for a different task must still be
	// throttled to Debug — otherwise a sustained outage floods the log with
	// one Error per dispatch across the fleet.
	records = nil
	tasks.Put(TaskInfo{ID: "t2", Status: "todo", AgentMode: "headless"})
	runOnce("t2")
	var second []slog.Record
	for _, r := range records {
		if r.Message == "workflow.ab.provider_demoted" {
			second = append(second, r)
		}
	}
	if len(second) != 1 {
		t.Fatalf("got %d demotion records after second run, want 1: %+v", len(second), second)
	}
	if second[0].Level != slog.LevelDebug {
		t.Fatalf("repeat demotion level = %v, want Debug (throttled)", second[0].Level)
	}
	if got := recordAttr(second[0], "task_id"); got != "t2" {
		t.Fatalf("task_id = %q, want t2 on throttled cross-task repeat", got)
	}
}

// TestExecRunAgent_ProviderShutoutEmitsSignal proves the single-provider
// fallback is observable: when provider filtering excludes *every* variant of
// an experiment (all variants share one unhealthy provider), the experiment
// degrades silently to non-A/B dispatch — but that total shutout emits a
// distinct throttled signal so an operator can tell it apart from A/B being
// disabled or no experiment matching the role.
func TestExecRunAgent_ProviderShutoutEmitsSignal(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	var records []slog.Record
	logger := slog.New(&demotionRecordHandler{records: &records})

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	// Every variant is on codex, and codex is rate-limited — provider
	// filtering zeroes out the whole experiment, forcing the fallback.
	agents.SetProviderRateLimitedFor("codex", true)
	engine := NewEngine(store, tasks, agents, logger)
	enabled := true
	engine.SetABTestingConfig(abtest.Config{
		Enabled: &enabled,
		Experiments: []abtest.Experiment{{
			ID:             "exp",
			AssignmentUnit: "stage",
			Roles:          []string{"implementation"},
			Variants: []abtest.Variant{
				{ID: "codex-a", Provider: "codex", Model: "gpt-5.5", Weight: 1},
				{ID: "codex-b", Provider: "codex", Model: "gpt-5.5-mini", Weight: 1},
			},
		}},
	})
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	step := &Step{ID: "implement", Type: StepRunAgent, Config: StepConfig{Role: "implementation", Prompt: "test prompt"}}
	wfExec := &Execution{WorkflowID: "test-simple", State: ExecRunning, Variables: make(map[string]string)}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	// Must not error the whole dispatch — falls back to normal selection.
	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}

	var shutouts []slog.Record
	for _, r := range records {
		if r.Message == "workflow.ab.provider_shutout" {
			shutouts = append(shutouts, r)
		}
	}
	if len(shutouts) != 1 {
		t.Fatalf("got %d shutout records, want 1: %+v", len(shutouts), shutouts)
	}
	first := shutouts[0]
	if first.Level != slog.LevelError {
		t.Fatalf("shutout level = %v, want Error", first.Level)
	}
	if got := recordAttr(first, "wanted_provider"); got != "codex" {
		t.Fatalf("wanted_provider = %q, want codex", got)
	}
	if got := recordAttr(first, "reason"); got != "rate_limited" {
		t.Fatalf("reason = %q, want rate_limited", got)
	}
	if got := recordAttr(first, "experiment_id"); got != "exp" {
		t.Fatalf("experiment_id = %q, want exp", got)
	}
}

func TestHandleAgentComplete_CompletedWorkflowIsNoop(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Run through full lifecycle to completion.
	agents.SimulateComplete("t1")
	_ = engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"})
	agents.SimulateComplete("t1")
	_ = engine.AdvanceStep("t1", StepOutput{StepID: "implement", Status: "completed"})
	agents.SimulateComplete("t1")
	_ = engine.AdvanceStep("t1", StepOutput{StepID: "evaluate", Status: "completed"})

	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.State != ExecCompleted {
		t.Fatalf("precondition: expected completed, got %q", ti.Workflow.State)
	}
	if ti.Workflow.CurrentStep != "" {
		t.Fatalf("precondition: expected empty current step after completion, got %q", ti.Workflow.CurrentStep)
	}
	historyBefore := len(ti.Workflow.StepHistory)

	// Another agent complete on an already-completed workflow should not
	// start new agents, mutate step history, or record an error.
	callsBefore := agents.CallCount()
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "stale-agent", Result: "late result", Success: true})

	if agents.CallCount() != callsBefore {
		t.Error("HandleAgentComplete on completed workflow should not start new agents")
	}

	tiAfter, _ := tasks.GetTask("t1")
	if got := len(tiAfter.Workflow.StepHistory); got != historyBefore {
		t.Errorf("StepHistory len = %d, want %d — stale completion must not append",
			got, historyBefore)
	}
	if tiAfter.Workflow.State != ExecCompleted {
		t.Errorf("State = %q, want ExecCompleted — stale completion must not mutate state",
			tiAfter.Workflow.State)
	}
	if tiAfter.Workflow.CurrentStep != "" {
		t.Errorf("CurrentStep = %q, want empty — stale completion must not mutate current step",
			tiAfter.Workflow.CurrentStep)
	}
}

func TestHandleAgentComplete_UntrackedRoleMismatchDoesNotAdvanceCurrentStep(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
		AgentRuns: []AgentRunInfo{
			{AgentID: "plan-agent", Role: "plan"},
		},
	})

	engine.HandleAgentComplete("t1", AgentCompletion{
		AgentID: "plan-agent",
		Result:  "late plan completion",
		Success: true,
	})

	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "implement" {
		t.Fatalf("CurrentStep = %q, want implement — plan completion must not satisfy implementation step",
			ti.Workflow.CurrentStep)
	}
	if len(ti.Workflow.StepHistory) != 0 {
		t.Fatalf("StepHistory = %+v, want no recorded implementation completion", ti.Workflow.StepHistory)
	}
}

func TestHandleAgentComplete_UnverifiedSkillRetriesWithInjectedSkill(t *testing.T) {
	store := newInlineTestStore(t, "skill-receipt", `id: skill-receipt
name: Skill Receipt
trigger:
  on: task.created
steps:
  - id: run
    name: Run
    type: run_agent
    config:
      role: plan-critic
      mode: headless
      provider: codex
      prompt: "Run /sybra-test now."
      import_sidecar:
        from: '{{getvar .Vars "_dir"}}/.sybra-plan-{{.Task.ID}}.md'
        kind: plan
        required: true
    next:
      - goto: done
  - id: done
    name: Done
    type: set_status
    config:
      status: done
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	recorder := &recordingArtifactRecorder{}
	engine := NewEngine(store, tasks, agents, discardLogger())
	engine.SetArtifactRecorder(recorder)

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Plan:      "# fake first pass\n",
		Workflow: &Execution{
			WorkflowID:  "skill-receipt",
			CurrentStep: "run",
			State:       ExecWaiting,
			Variables: map[string]string{
				WorkflowVarDir: t.TempDir(),
			},
		},
		AgentRuns: []AgentRunInfo{{
			AgentID:            "agent-1",
			Role:               "plan-critic",
			Provider:           "codex",
			RequestedSkill:     "sybra-test",
			SkillExecutionMode: "native",
			SkillConformance:   "unverified",
		}},
	})

	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "agent-1", Result: "done", Success: true})

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got := ti.Workflow.CurrentStep; got != "run" {
		t.Fatalf("CurrentStep = %q, want run", got)
	}
	if got := ti.Workflow.Variables[skillReceiptRecoveryKey("run")]; got != "1" {
		t.Fatalf("skill receipt retry var = %q, want 1", got)
	}
	if len(ti.Workflow.StepHistory) != 0 {
		t.Fatalf("StepHistory = %+v, want no recorded completion before retry", ti.Workflow.StepHistory)
	}
	if len(agents.calls) != 1 {
		t.Fatalf("StartAgent calls = %d, want 1 retry", len(agents.calls))
	}
	if !agents.calls[0].Assignment.ForceInjectedSkill {
		t.Fatal("retry assignment did not force injected skill delivery")
	}
	if !agents.calls[0].Assignment.SkillRecoveryAttempt {
		t.Fatal("retry assignment missing recovery-attempt marker")
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.puts) != 1 {
		t.Fatalf("diagnostic artifacts = %+v, want one preserved first-pass sidecar", recorder.puts)
	}
	if recorder.puts[0].name != "skill-receipt-first-run-plan.md" {
		t.Fatalf("diagnostic artifact name = %q, want skill-receipt-first-run-plan.md", recorder.puts[0].name)
	}
	if recorder.puts[0].content != "# fake first pass\n" {
		t.Fatalf("diagnostic artifact content = %q, want first-pass plan", recorder.puts[0].content)
	}
}

func TestHandleAgentComplete_UnverifiedSkillAfterRetryEscalatesHumanRequired(t *testing.T) {
	store := newInlineTestStore(t, "skill-receipt", `id: skill-receipt
name: Skill Receipt
trigger:
  on: task.created
steps:
  - id: run
    name: Run
    type: run_agent
    config:
      role: plan-critic
      mode: headless
      provider: codex
      prompt: "Run /sybra-test now."
    next:
      - goto: done
  - id: done
    name: Done
    type: set_status
    config:
      status: done
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	var completed []CompletionInfo
	engine.SetOnComplete(func(info CompletionInfo) {
		completed = append(completed, info)
	})

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "skill-receipt",
			CurrentStep: "run",
			State:       ExecWaiting,
			Variables: map[string]string{
				skillReceiptRecoveryKey("run"): "1",
			},
		},
		AgentRuns: []AgentRunInfo{{
			AgentID:            "agent-2",
			Role:               "plan-critic",
			Provider:           "codex",
			RequestedSkill:     "sybra-test",
			SkillExecutionMode: "injected",
			SkillConformance:   "unverified",
		}},
	})

	engine.HandleAgentComplete("t1", AgentCompletion{
		AgentID: "agent-2",
		Result: `{"verdict":"FAIL","outcome":"product_bug","failures_markdown":"## Test Failures\n\nClassification: product_bug: repo does not compile\n\nObserved output:\n` +
			"```text\npkg/api-server/resource_inspect_endpoints.go:14: dangling import\n```" +
			`"}`,
		Success: true,
	})

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.Status != "human-required" {
		t.Fatalf("Status = %q, want human-required", ti.Status)
	}
	if !strings.Contains(ti.StatusReason, "no conformance receipt after automatic recovery retry") {
		t.Fatalf("StatusReason = %q, want receipt-retry exhaustion", ti.StatusReason)
	}
	if !strings.Contains(ti.StatusReason, "product_bug: repo does not compile") {
		t.Fatalf("StatusReason = %q, want parsed verdict summary", ti.StatusReason)
	}
	if got := ti.Workflow.Variables[skillReceiptRecoveryKey("run")]; got != "" {
		t.Fatalf("skill receipt retry var = %q, want cleared after exhaustion", got)
	}
	if len(agents.calls) != 0 {
		t.Fatalf("StartAgent calls = %d, want no third attempt", len(agents.calls))
	}
	if ti.Workflow.State != ExecCompleted {
		t.Fatalf("Workflow.State = %q, want %q so a later recovery trigger is not blocked by ErrWorkflowAlreadyActive", ti.Workflow.State, ExecCompleted)
	}
	if ti.Workflow.CurrentStep != "" {
		t.Fatalf("Workflow.CurrentStep = %q, want empty", ti.Workflow.CurrentStep)
	}
	if len(completed) != 1 {
		t.Fatalf("workflow completion callbacks = %d, want 1 exhausted completion for downstream recovery", len(completed))
	}
	if completed[0].TaskID != "t1" || completed[0].WorkflowID != "skill-receipt" {
		t.Fatalf("completion = %+v, want task/workflow ids for exhausted run", completed[0])
	}
	if got := completed[0].Variables[skillReceiptRecoveryKey("run")]; got != "" {
		t.Fatalf("completion retry var = %q, want cleared before downstream recovery sees completion", got)
	}
}

func TestHandleAgentComplete_UnverifiedSkillAfterRetryContinuesWithImportedSidecar(t *testing.T) {
	store := newInlineTestStore(t, "skill-receipt", `id: skill-receipt
name: Skill Receipt
trigger:
  on: task.created
steps:
  - id: run
    name: Run
    type: run_agent
    config:
      role: review
      mode: headless
      provider: codex
      prompt: "Run /adversarial-review now."
      import_sidecar:
        from: '{{getvar .Vars "_dir"}}/.sybra-review-{{.Task.ID}}.md'
        kind: code_review
        required: true
    next:
      - goto: require_review
  - id: require_review
    name: Require Review
    type: require_sidecar
    config:
      sidecar: code_review
    next:
      - goto: done
  - id: done
    name: Done
    type: set_status
    config:
      status: done
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".sybra-review-t1.md"), []byte("Review Verdict: CLEAN\n\nNo findings.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "skill-receipt",
			CurrentStep: "run",
			State:       ExecWaiting,
			Variables: map[string]string{
				WorkflowVarDir:                 dir,
				skillReceiptRecoveryKey("run"): "1",
			},
		},
		AgentRuns: []AgentRunInfo{{
			AgentID:            "agent-2",
			Role:               "review",
			Provider:           "codex",
			RequestedSkill:     "adversarial-review",
			SkillExecutionMode: "injected",
			SkillConformance:   "unverified",
		}},
	})

	engine.HandleAgentComplete("t1", AgentCompletion{
		AgentID: "agent-2",
		Result:  "review complete without receipt",
		Success: true,
	})

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.Status != "done" {
		t.Fatalf("Status = %q, want done", ti.Status)
	}
	if !strings.Contains(ti.CodeReview, "Review Verdict: CLEAN") {
		t.Fatalf("CodeReview = %q, want imported review sidecar", ti.CodeReview)
	}
	if got := ti.Workflow.Variables[skillReceiptRecoveryKey("run")]; got != "" {
		t.Fatalf("skill receipt retry var = %q, want cleared after sidecar continuation", got)
	}
	if len(agents.calls) != 0 {
		t.Fatalf("StartAgent calls = %d, want no third attempt", len(agents.calls))
	}
}

// TestHandleAgentComplete_UnverifiedSkillExhaustionAllowsFreshWorkflowStart
// covers the human-review recovery handoff: once skill-receipt exhaustion
// marks a task human-required, a subsequent recovery attempt must be able to
// start a fresh workflow instance rather than fail with
// ErrWorkflowAlreadyActive against the exhausted, never-finalized Execution
// (the bug in #5ba88ecc — a later genuinely passing run stayed parked at
// human-required because the stale Execution was still "active").
func TestHandleAgentComplete_UnverifiedSkillExhaustionAllowsFreshWorkflowStart(t *testing.T) {
	store := newInlineTestStore(t, "skill-receipt", `id: skill-receipt
name: Skill Receipt
trigger:
  on: task.created
steps:
  - id: run
    name: Run
    type: run_agent
    config:
      role: plan-critic
      mode: headless
      provider: codex
      prompt: "Run /sybra-test now."
    next:
      - goto: done
  - id: done
    name: Done
    type: set_status
    config:
      status: done
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "skill-receipt",
			CurrentStep: "run",
			State:       ExecWaiting,
			Variables: map[string]string{
				skillReceiptRecoveryKey("run"): "1",
			},
		},
		AgentRuns: []AgentRunInfo{{
			AgentID:            "agent-2",
			Role:               "plan-critic",
			Provider:           "codex",
			RequestedSkill:     "sybra-test",
			SkillExecutionMode: "injected",
			SkillConformance:   "unverified",
		}},
	})

	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "agent-2", Result: "still no receipt", Success: true})

	if err := engine.StartWorkflow("t1", "skill-receipt"); err != nil {
		t.Fatalf("StartWorkflow after exhaustion = %v, want nil (fresh recovery trigger must not be rejected as already active)", err)
	}

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got := ti.Workflow.Variables[skillReceiptRecoveryKey("run")]; got != "" {
		t.Fatalf("skill receipt retry var = %q, want a fresh budget on the new Execution", got)
	}
	if ti.Workflow.State == ExecCompleted {
		t.Fatalf("Workflow.State = %q, want the fresh restart to be running/waiting again", ti.Workflow.State)
	}
}

// TestAdvanceStep_EmptyStepIDIsNoop covers the direct-call variant: a caller
// that passes an empty StepID (e.g. because t.Workflow.CurrentStep was reset
// to "" by a previous completion) used to error with "step not found in
// workflow", which the agent-complete path would log as ERROR and still
// persist via RecordStep. The guard must return nil and leave state intact.
func TestAdvanceStep_EmptyStepIDIsNoop(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}
	// Force the workflow into the pathological state observed in prod:
	// state=completed, current_step="" — mirrors what resolveNext leaves
	// behind when a terminal step evaluates to goto: "".
	ti, _ := tasks.GetTask("t1")
	ti.Workflow.State = ExecCompleted
	ti.Workflow.CurrentStep = ""
	if err := tasks.SetWorkflow("t1", ti.Workflow); err != nil {
		t.Fatal(err)
	}
	historyBefore := len(ti.Workflow.StepHistory)

	err := engine.AdvanceStep("t1", StepOutput{StepID: "", Status: "completed"})
	if err != nil {
		t.Errorf("AdvanceStep with empty StepID = %v, want nil (no-op)", err)
	}

	tiAfter, _ := tasks.GetTask("t1")
	if got := len(tiAfter.Workflow.StepHistory); got != historyBefore {
		t.Errorf("StepHistory len = %d, want %d — empty-step advance must not append",
			got, historyBefore)
	}
	if tiAfter.Workflow.State != ExecCompleted {
		t.Errorf("State = %q, want ExecCompleted", tiAfter.Workflow.State)
	}
}

// TestAdvanceStep_FailedWorkflowIsNoop pins the other terminal state:
// workflows that hit ExecFailed also must refuse further advances.
func TestAdvanceStep_FailedWorkflowIsNoop(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "in-progress",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "triage",
			State:       ExecFailed,
			Variables:   make(map[string]string),
		},
	})

	err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"})
	if err != nil {
		t.Errorf("AdvanceStep on failed workflow = %v, want nil (no-op)", err)
	}
	if agents.CallCount() != 0 {
		t.Errorf("agents.CallCount = %d, want 0 — failed workflow must not spawn", agents.CallCount())
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name  string
		input string
		limit int
		want  string
	}{
		{"under limit", "short", 100, "short"},
		{"at limit", "exact", 5, "exact"},
		{"over limit", "this is too long", 7, "this is\n... (truncated)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.input, tt.limit)
			if got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.limit, got, tt.want)
			}
		})
	}
}

func TestAgentModeTemplate(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Advance past triage to implement step.
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"}); err != nil {
		t.Fatal(err)
	}

	// Implement step should have mode resolved from template.
	if agents.LastCall().Mode != "headless" {
		t.Fatalf("expected headless mode from template, got %q", agents.LastCall().Mode)
	}
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
	engine := NewEngine(store, tasks, agents, discardLogger())

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

func TestHandleStatusChange_AdvancesRunAgentWhenWaitForStatusMatches(t *testing.T) {
	store := newTestStoreWith(t, "test-plan-reuse.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "planning", AgentMode: "interactive"})
	if err := engine.StartWorkflow("t1", "test-plan-reuse"); err != nil {
		t.Fatal(err)
	}

	// Before the status flips, we're still in the plan run_agent step.
	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "plan" {
		t.Fatalf("precondition: expected plan step, got %q", ti.Workflow.CurrentStep)
	}

	// The plan agent flips the task status — engine should advance to
	// review_plan without the agent process having to exit.
	tasks.SetStatus("t1", "plan-review")
	engine.HandleStatusChange("t1", "plan-review")

	ti, _ = tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "review_plan" {
		t.Errorf("CurrentStep = %q, want review_plan", ti.Workflow.CurrentStep)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Errorf("State = %q, want ExecWaiting", ti.Workflow.State)
	}
}

func TestHandleStatusChange_NoOp(t *testing.T) {
	tests := []struct {
		name      string
		newStatus string
		// mutate lets each case set up its own pre-state after the
		// default "workflow started, sitting in plan step" arrangement.
		mutate func(tasks *memTasks)
	}{
		{
			name:      "status does not match wait_for_status",
			newStatus: "todo",
		},
		{
			name:      "current step is not a run_agent",
			newStatus: "plan-review",
			mutate: func(tasks *memTasks) {
				ti, _ := tasks.GetTask("t1")
				ti.Workflow.CurrentStep = "review_plan"
				_ = tasks.SetWorkflow("t1", ti.Workflow)
			},
		},
		{
			name:      "workflow already completed",
			newStatus: "plan-review",
			mutate: func(tasks *memTasks) {
				ti, _ := tasks.GetTask("t1")
				ti.Workflow.State = ExecCompleted
				_ = tasks.SetWorkflow("t1", ti.Workflow)
			},
		},
		{
			name:      "task has no workflow",
			newStatus: "plan-review",
			mutate: func(tasks *memTasks) {
				ti, _ := tasks.GetTask("t1")
				ti.Workflow = nil
				tasks.Put(ti)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStoreWith(t, "test-plan-reuse.yaml")
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewEngine(store, tasks, agents, discardLogger())

			tasks.Put(TaskInfo{ID: "t1", Status: "planning", AgentMode: "interactive"})
			if err := engine.StartWorkflow("t1", "test-plan-reuse"); err != nil {
				t.Fatal(err)
			}

			if tt.mutate != nil {
				tt.mutate(tasks)
			}

			// Snapshot the current step so we can detect any advance.
			before, _ := tasks.GetTask("t1")
			wantStep := ""
			if before.Workflow != nil {
				wantStep = before.Workflow.CurrentStep
			}

			engine.HandleStatusChange("t1", tt.newStatus)

			after, _ := tasks.GetTask("t1")
			gotStep := ""
			if after.Workflow != nil {
				gotStep = after.Workflow.CurrentStep
			}
			if gotStep != wantStep {
				t.Errorf("CurrentStep changed to %q, want %q (no advance)", gotStep, wantStep)
			}
		})
	}
}

func TestHandleStatusChange_UnknownTaskDoesNotPanic(t *testing.T) {
	store := newTestStoreWith(t, "test-plan-reuse.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	// Act — must not panic even though the task was never registered.
	engine.HandleStatusChange("ghost", "plan-review")
}

func TestPlanReuse_RejectResetsStatusAndReusesAgentWithFeedback(t *testing.T) {
	engine, tasks, agents := startPlanReuseAtReviewPlan(t)

	// Arrange — the plan agent is still "running" (reuse_agent relies on
	// FindRunningAgentForRole). Record how many SendPrompt calls we've
	// seen so we can assert exactly one more is added by the reject.
	sentBefore := len(agents.SentPrompts())

	// Act — user rejects the plan with free-text feedback. The reject
	// branch routes review_plan → start_replan (set_status planning) →
	// plan, which hits the reuse_agent path.
	if err := engine.HandleHumanAction("t1", "reject", map[string]string{"feedback": "add error handling"}); err != nil {
		t.Fatalf("reject: %v", err)
	}

	// Assert 1 — task status was reset by start_replan, so the next
	// plan-review transition is observable as a real change event.
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "planning" {
		t.Errorf("Status = %q, want planning (reset by start_replan)", ti.Status)
	}

	// Assert 2 — the workflow re-entered the plan run_agent step.
	if ti.Workflow.CurrentStep != "plan" {
		t.Errorf("CurrentStep = %q, want plan", ti.Workflow.CurrentStep)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Errorf("State = %q, want ExecWaiting", ti.Workflow.State)
	}

	// Assert 3 — the reused agent received exactly one new prompt
	// carrying the feedback (verbatim, via the rendered template).
	sent := agents.SentPrompts()
	if len(sent) != sentBefore+1 {
		t.Fatalf("SendPrompt count = %d, want %d", len(sent), sentBefore+1)
	}
	msg := sent[len(sent)-1].Message
	if !strings.Contains(msg, "Plan rejected") {
		t.Errorf("prompt missing rejection header: %q", msg)
	}
	if !strings.Contains(msg, "add error handling") {
		t.Errorf("prompt missing feedback: %q", msg)
	}

	// Assert 4 — no new agent was spawned (reuse, not restart).
	if got := agents.CallCount(); got != 1 {
		t.Errorf("StartAgent called %d times, want 1 (reuse only)", got)
	}
}

func TestPlanReuse_RejectThenReplanAdvancesOnStatusChange(t *testing.T) {
	engine, tasks, _ := startPlanReuseAtReviewPlan(t)

	// Reject — workflow should re-enter plan step waiting for the agent.
	if err := engine.HandleHumanAction("t1", "reject", map[string]string{"feedback": "needs detail"}); err != nil {
		t.Fatal(err)
	}

	// Simulate the plan agent delivering a revised plan and flipping
	// the status back to plan-review.
	tasks.SetStatus("t1", "plan-review")
	engine.HandleStatusChange("t1", "plan-review")

	// The workflow should be back at review_plan waiting for a fresh
	// human action. Without the set_status reset, the status would
	// already be plan-review when the agent ran and no hook would fire.
	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "review_plan" {
		t.Errorf("CurrentStep = %q, want review_plan", ti.Workflow.CurrentStep)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Errorf("State = %q, want ExecWaiting", ti.Workflow.State)
	}
}

func TestPlanReuse_ApproveAdvancesPastReviewPlan(t *testing.T) {
	engine, tasks, _ := startPlanReuseAtReviewPlan(t)

	if err := engine.HandleHumanAction("t1", "approve", nil); err != nil {
		t.Fatal(err)
	}

	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Errorf("Status = %q, want in-progress (set by done step)", ti.Status)
	}
	if ti.Workflow.State != ExecCompleted {
		t.Errorf("State = %q, want ExecCompleted", ti.Workflow.State)
	}
}

func TestAutoApprovePlanReview_NoOpenDecisionsAdvances(t *testing.T) {
	engine, tasks := startAutoApprovePlanReview(t, TaskInfo{
		ID:            "t1",
		Status:        "planning",
		PlanDecisions: "# Decisions\n\nNo open decisions. The recommended execution contract is fully specified.",
		PlanContract:  validPlanContract("t1"),
	})
	engine.SetAutoApprovePlansWithoutDecisions(true)

	step := autoApproveReviewStep()
	wf := &Execution{
		WorkflowID:  "simple-task-plan",
		CurrentStep: "review_plan",
		State:       ExecRunning,
		Variables:   map[string]string{},
	}
	if err := engine.execWaitHuman("t1", step, wf); err != nil {
		t.Fatal(err)
	}

	waitForTaskStatus(t, tasks, "t1", "in-progress")
	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.State != ExecCompleted {
		t.Errorf("State = %q, want ExecCompleted", ti.Workflow.State)
	}
	if got := ti.Workflow.Variables["human.auto_approved"]; got != "true" {
		t.Errorf("human.auto_approved = %q, want true", got)
	}
}

func TestAutoApprovePlanReview_OpenDecisionsStayWaiting(t *testing.T) {
	engine, tasks := startAutoApprovePlanReview(t, TaskInfo{
		ID:     "t1",
		Status: "planning",
		PlanDecisions: "# Decisions\n\n## Scope\nQuestion: Which scope?\nRecommended: Small\n\nOptions:\n" +
			"- Small - Minimal change\n- Large - Broader change",
		PlanContract: validPlanContract("t1"),
	})
	engine.SetAutoApprovePlansWithoutDecisions(true)

	if err := engine.execWaitHuman("t1", autoApproveReviewStep(), &Execution{
		WorkflowID:  "simple-task-plan",
		CurrentStep: "review_plan",
		State:       ExecRunning,
		Variables:   map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}

	assertRemainsPlanReviewWaiting(t, tasks, "t1", 200*time.Millisecond)
}

func TestAutoApprovePlanReview_InvalidContractStaysWaiting(t *testing.T) {
	engine, tasks := startAutoApprovePlanReview(t, TaskInfo{
		ID:            "t1",
		Status:        "planning",
		PlanDecisions: "# Decisions\n\nNo open decisions. The recommended execution contract is fully specified.",
		PlanContract:  strings.Replace(validPlanContract("t1"), `"task_id": "t1"`, `"task_id": "other"`, 1),
	})
	engine.SetAutoApprovePlansWithoutDecisions(true)

	if err := engine.execWaitHuman("t1", autoApproveReviewStep(), &Execution{
		WorkflowID:  "simple-task-plan",
		CurrentStep: "review_plan",
		State:       ExecRunning,
		Variables:   map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}

	assertRemainsPlanReviewWaiting(t, tasks, "t1", 200*time.Millisecond)
}

func TestAutoApprovePlanReview_UnfavorableVerdictStaysWaiting(t *testing.T) {
	cases := []struct {
		name     string
		critique string
	}{
		{
			name:     "REFINE",
			critique: "## Verdict: REFINE\n\nMissing caller file in the file list; compile will break.",
		},
		{
			name:     "REJECT",
			critique: "## Verdict: REJECT\n\nApproach fundamentally unsound; needs a full replan.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, tasks := startAutoApprovePlanReview(t, TaskInfo{
				ID:            "t1",
				Status:        "planning",
				PlanDecisions: "# Decisions\n\nNo open decisions. The recommended execution contract is fully specified.",
				PlanContract:  validPlanContract("t1"),
				PlanCritique:  tc.critique,
			})
			engine.SetAutoApprovePlansWithoutDecisions(true)

			if err := engine.execWaitHuman("t1", autoApproveReviewStep(), &Execution{
				WorkflowID:  "simple-task-plan",
				CurrentStep: "review_plan",
				State:       ExecRunning,
				Variables:   map[string]string{},
			}); err != nil {
				t.Fatal(err)
			}

			// A REFINE/REJECT verdict names concrete blockers a human must look
			// at even though the decisions sidecar has nothing left for a human
			// to choose between — "no open decisions" is not the same as "safe
			// to execute as-is". Auto-approve must not paper over that.
			assertRemainsPlanReviewWaiting(t, tasks, "t1", 200*time.Millisecond)
		})
	}
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
	return NewEngine(store, tasks, newMockAgents(), discardLogger()), tasks
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
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ti, err := tasks.GetTask(id)
		if err == nil && ti.Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	ti, _ := tasks.GetTask(id)
	t.Fatalf("timed out waiting for status %q, got %q", want, ti.Status)
}

func assertRemainsPlanReviewWaiting(t *testing.T, tasks *memTasks, id string, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		ti, err := tasks.GetTask(id)
		if err != nil {
			t.Fatal(err)
		}
		if ti.Status != "plan-review" || ti.Workflow == nil || ti.Workflow.State != ExecWaiting {
			t.Fatalf("task left plan-review wait state: status=%q workflow=%+v", ti.Status, ti.Workflow)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPlanReuse_ApproveRepairsMissedWaitForStatus(t *testing.T) {
	store := newTestStoreWith(t, "test-plan-reuse.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "planning", AgentMode: "interactive"})
	if err := engine.StartWorkflow("t1", "test-plan-reuse"); err != nil {
		t.Fatalf("start workflow: %v", err)
	}

	// Simulate a cross-process sybra-cli status write that happened while the
	// app was down, or whose watcher event was missed: the task is visibly
	// plan-review, but the workflow is still parked on the run_agent step.
	tasks.SetStatus("t1", "plan-review")
	stuck, _ := tasks.GetTask("t1")
	if stuck.Workflow.CurrentStep != "plan" {
		t.Fatalf("precondition: CurrentStep = %q, want plan", stuck.Workflow.CurrentStep)
	}

	if err := engine.HandleHumanAction("t1", "approve", nil); err != nil {
		t.Fatal(err)
	}

	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Errorf("Status = %q, want in-progress", ti.Status)
	}
	if ti.Workflow == nil || ti.Workflow.CurrentStep != "" {
		t.Fatalf("CurrentStep = %v, want completed workflow", ti.Workflow)
	}
	if ti.Workflow.State != ExecCompleted {
		t.Errorf("State = %q, want %q", ti.Workflow.State, ExecCompleted)
	}
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

func TestExecRerequestReview_NoRequesterSkips(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	ti := TaskInfo{ID: "t1", ProjectID: "owner/repo", PRNumber: 5}
	out, err := engine.execRerequestReview("t1", newRerequestReviewStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "no pr review requester") {
		t.Errorf("Output = %q, want no requester skip", out.Output)
	}
}

func TestExecRerequestReview_MissingFieldsSkip(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	requester := &fakePRReviewRequester{}
	engine.SetPRReviewRequester(requester)

	out, err := engine.execRerequestReview("t1", newRerequestReviewStep(), TaskInfo{ID: "t1", ProjectID: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if requester.calls != 0 {
		t.Fatalf("requester calls = %d, want 0", requester.calls)
	}
	if !strings.Contains(out.Output, "missing pr or project") {
		t.Errorf("Output = %q, want missing fields skip", out.Output)
	}
}

func TestExecRerequestReview_RequestsReviewers(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	requester := &fakePRReviewRequester{reviewers: []string{"alice", "bob"}}
	engine.SetPRReviewRequester(requester)

	ti := TaskInfo{ID: "t1", ProjectID: "owner/repo", PRNumber: 5}
	out, err := engine.execRerequestReview("t1", newRerequestReviewStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if requester.calls != 1 || requester.repo != "owner/repo" || requester.prNumber != 5 {
		t.Fatalf("requester = calls:%d repo:%q pr:%d", requester.calls, requester.repo, requester.prNumber)
	}
	if !strings.Contains(out.Output, "@alice") || !strings.Contains(out.Output, "@bob") {
		t.Errorf("Output = %q, want requested reviewers", out.Output)
	}
}

func TestExecRerequestReview_ErrorIsNonFatal(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	engine.SetPRReviewRequester(&fakePRReviewRequester{err: fmt.Errorf("boom")})

	ti := TaskInfo{ID: "t1", ProjectID: "owner/repo", PRNumber: 5}
	out, err := engine.execRerequestReview("t1", newRerequestReviewStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" || !strings.Contains(out.Output, "request failed") {
		t.Errorf("output = %+v, want completed failure note", out)
	}
}

func TestExecEnsurePRClosesIssue_NoLinkerSkips(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	ti := TaskInfo{ID: "t1", ProjectID: "owner/repo", PRNumber: 5, Issue: "https://github.com/owner/repo/issues/7"}
	out, err := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "no pr linker") {
		t.Errorf("Output = %q, want 'no pr linker' skip reason", out.Output)
	}
}

func TestExecEnsurePRClosesIssue_MissingFieldsSkip(t *testing.T) {
	tests := []struct {
		name string
		ti   TaskInfo
	}{
		{"no issue", TaskInfo{ID: "t1", ProjectID: "owner/repo", PRNumber: 5}},
		{"no pr", TaskInfo{ID: "t1", ProjectID: "owner/repo", Issue: "https://github.com/owner/repo/issues/7"}},
		{"no project", TaskInfo{ID: "t1", PRNumber: 5, Issue: "https://github.com/owner/repo/issues/7"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewEngine(store, tasks, agents, discardLogger())
			engine.SetPRLinker(&fakePRLinker{})

			out, err := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), tt.ti)
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != "completed" {
				t.Errorf("Status = %q, want completed", out.Status)
			}
			if !strings.Contains(out.Output, "skipped") {
				t.Errorf("Output = %q, want 'skipped' reason", out.Output)
			}
		})
	}
}

func TestExecEnsurePRClosesIssue_CrossRepoSkips(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{}
	engine.SetPRLinker(linker)

	ti := TaskInfo{
		ID:        "t1",
		ProjectID: "owner/repo",
		PRNumber:  5,
		Issue:     "https://github.com/other/elsewhere/issues/7",
	}
	out, _ := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), ti)
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "cross-repo") {
		t.Errorf("Output = %q, want cross-repo skip", out.Output)
	}
	if linker.getCalls != 0 {
		t.Errorf("GetClosingIssues called %d times, want 0 (skip before fetch)", linker.getCalls)
	}
}

func TestExecEnsurePRClosesIssue_AlreadyLinkedNoEdit(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{
		getQueue: []getResult{{issues: []int{7}, body: "original"}},
	}
	engine.SetPRLinker(linker)

	ti := TaskInfo{
		ID: "t1", ProjectID: "owner/repo", PRNumber: 5,
		Issue: "https://github.com/owner/repo/issues/7",
	}
	out, err := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" || !strings.Contains(out.Output, "already linked") {
		t.Errorf("output = %+v, want completed/already linked", out)
	}
	if linker.editCalls != 0 {
		t.Errorf("EditBody called %d times, want 0", linker.editCalls)
	}
}

func TestExecEnsurePRClosesIssue_EditAppendsAndVerifies(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{
		getQueue: []getResult{
			{issues: nil, body: "Implements the feature."},
			{issues: []int{7}, body: "Implements the feature.\n\nCloses https://github.com/owner/repo/issues/7"},
		},
	}
	engine.SetPRLinker(linker)

	tasks.Put(TaskInfo{ID: "t1", Status: "in-review"})

	ti := TaskInfo{
		ID: "t1", ProjectID: "owner/repo", PRNumber: 5,
		Issue: "https://github.com/owner/repo/issues/7",
	}
	out, err := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if linker.editCalls != 1 {
		t.Errorf("EditBody called %d times, want 1", linker.editCalls)
	}
	wantBody := "Implements the feature.\n\nCloses https://github.com/owner/repo/issues/7"
	if linker.lastBody != wantBody {
		t.Errorf("edit body = %q, want %q", linker.lastBody, wantBody)
	}
	// Status must not have been changed on success.
	after, _ := tasks.GetTask("t1")
	if after.Status != "in-review" {
		t.Errorf("Status = %q, want in-review (unchanged)", after.Status)
	}
}

func TestExecEnsurePRClosesIssue_EmptyBodyNoLeadingNewlines(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{
		getQueue: []getResult{
			{issues: nil, body: ""},
			{issues: []int{7}, body: "Closes https://github.com/owner/repo/issues/7"},
		},
	}
	engine.SetPRLinker(linker)

	ti := TaskInfo{
		ID: "t1", ProjectID: "owner/repo", PRNumber: 5,
		Issue: "https://github.com/owner/repo/issues/7",
	}
	if _, err := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), ti); err != nil {
		t.Fatal(err)
	}
	if linker.lastBody != "Closes https://github.com/owner/repo/issues/7" {
		t.Errorf("edit body = %q, want no leading newlines", linker.lastBody)
	}
}

func TestExecEnsurePRClosesIssue_EditFailureFlipsHumanRequired(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{
		getQueue: []getResult{{issues: nil, body: "body"}},
		editErr:  fmt.Errorf("403 forbidden"),
	}
	engine.SetPRLinker(linker)

	tasks.Put(TaskInfo{ID: "t1", Status: "in-review"})

	ti := TaskInfo{
		ID: "t1", ProjectID: "owner/repo", PRNumber: 5,
		Issue: "https://github.com/owner/repo/issues/7",
	}
	out, err := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "failed" {
		t.Errorf("Status = %q, want failed", out.Status)
	}
	after, _ := tasks.GetTask("t1")
	if after.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", after.Status)
	}
}

// Verification lag is a false negative: gh pr edit succeeded, the
// body contains "Closes <url>", but GitHub hasn't re-parsed
// closingIssuesReferences yet. The step must trust the body and
// leave the task status alone instead of flipping to human-required.
func TestExecEnsurePRClosesIssue_VerifyLagTrustsBody(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{
		getQueue: []getResult{
			// 1 pre-check + 4 verify attempts, all miss.
			{issues: nil, body: "body"},
			{issues: nil, body: "body\n\nCloses https://github.com/owner/repo/issues/7"},
			{issues: nil, body: "body\n\nCloses https://github.com/owner/repo/issues/7"},
			{issues: nil, body: "body\n\nCloses https://github.com/owner/repo/issues/7"},
			{issues: nil, body: "body\n\nCloses https://github.com/owner/repo/issues/7"},
		},
	}
	engine.SetPRLinker(linker)

	tasks.Put(TaskInfo{ID: "t1", Status: "in-review"})

	ti := TaskInfo{
		ID: "t1", ProjectID: "owner/repo", PRNumber: 5,
		Issue: "https://github.com/owner/repo/issues/7",
	}
	out, err := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed (verification lag is soft-fail)", out.Status)
	}
	if !strings.Contains(out.Output, "trusting body") {
		t.Errorf("Output = %q, want 'trusting body' message", out.Output)
	}
	after, _ := tasks.GetTask("t1")
	if after.Status != "in-review" {
		t.Errorf("task status = %q, want in-review (unchanged)", after.Status)
	}
	// 1 pre-check + 1 initial verify + 3 retries = 5 fetches.
	if linker.getCalls != 5 {
		t.Errorf("GetClosingIssues calls = %d, want 5 (pre-check + 4 verify attempts)", linker.getCalls)
	}
}

// Verification should retry: first post-edit fetch misses (GitHub
// lagging), second fetch sees the parsed closing reference.
func TestExecEnsurePRClosesIssue_VerifyRetrySucceeds(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{
		getQueue: []getResult{
			{issues: nil, body: "body"},                    // pre-check miss → triggers edit
			{issues: nil, body: "body\n\nCloses ..."},      // verify attempt 0: still stale
			{issues: []int{7}, body: "body\n\nCloses ..."}, // verify attempt 1: parsed
		},
	}
	engine.SetPRLinker(linker)

	tasks.Put(TaskInfo{ID: "t1", Status: "in-review"})

	ti := TaskInfo{
		ID: "t1", ProjectID: "owner/repo", PRNumber: 5,
		Issue: "https://github.com/owner/repo/issues/7",
	}
	out, err := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" || !strings.Contains(out.Output, "linked issue #7") {
		t.Errorf("out = %+v, want completed/linked issue #7", out)
	}
	if linker.getCalls != 3 {
		t.Errorf("GetClosingIssues calls = %d, want 3 (pre-check + 2 verify attempts)", linker.getCalls)
	}
}

// Verification fetch that errors on every retry is still a soft-fail:
// the edit went through, so trust the body we wrote.
func TestExecEnsurePRClosesIssue_VerifyErrorTrustsBody(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{
		getQueue: []getResult{
			{issues: nil, body: "body"},
			{err: errors.New("network timeout")},
			{err: errors.New("network timeout")},
			{err: errors.New("network timeout")},
			{err: errors.New("network timeout")},
		},
	}
	engine.SetPRLinker(linker)

	tasks.Put(TaskInfo{ID: "t1", Status: "in-review"})

	ti := TaskInfo{
		ID: "t1", ProjectID: "owner/repo", PRNumber: 5,
		Issue: "https://github.com/owner/repo/issues/7",
	}
	out, err := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "trusting body") {
		t.Errorf("Output = %q, want 'trusting body' message", out.Output)
	}
	after, _ := tasks.GetTask("t1")
	if after.Status != "in-review" {
		t.Errorf("task status = %q, want in-review (unchanged)", after.Status)
	}
}

func TestExecEnsurePRClosesIssue_FetchErrorIsSoftFail(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{
		getQueue: []getResult{{err: errors.New("network timeout")}},
	}
	engine.SetPRLinker(linker)

	tasks.Put(TaskInfo{ID: "t1", Status: "in-review"})

	ti := TaskInfo{
		ID: "t1", ProjectID: "owner/repo", PRNumber: 5,
		Issue: "https://github.com/owner/repo/issues/7",
	}
	out, err := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	// Fetch failure must not block the workflow or flip status.
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed (fetch errors are soft-fail)", out.Status)
	}
	after, _ := tasks.GetTask("t1")
	if after.Status != "in-review" {
		t.Errorf("task status = %q, want in-review (unchanged)", after.Status)
	}
}

// --- stamp_pr_attribution step ---

func newStampPRStep() *Step {
	return &Step{ID: "stamp", Type: StepStampPRAttribution}
}

func TestExecStampPRAttribution_NoLinkerSkips(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	ti := TaskInfo{ID: "t1", ProjectID: "owner/repo", PRNumber: 5}
	out, err := engine.execStampPRAttribution("t1", newStampPRStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" || !strings.Contains(out.Output, "no pr linker") {
		t.Errorf("out = %+v, want completed no-linker skip", out)
	}
}

func TestExecStampPRAttribution_MissingFieldsSkip(t *testing.T) {
	tests := []struct {
		name string
		ti   TaskInfo
	}{
		{"no pr", TaskInfo{ID: "t1", ProjectID: "owner/repo"}},
		{"no project", TaskInfo{ID: "t1", PRNumber: 5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewEngine(store, tasks, agents, discardLogger())
			linker := &fakePRLinker{}
			engine.SetPRLinker(linker)

			out, err := engine.execStampPRAttribution("t1", newStampPRStep(), tt.ti)
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != "completed" || !strings.Contains(out.Output, "missing pr or project") {
				t.Errorf("out = %+v, want missing-fields skip", out)
			}
			if linker.getCalls != 0 || linker.editCalls != 0 {
				t.Errorf("linker touched: get=%d edit=%d, want 0/0", linker.getCalls, linker.editCalls)
			}
		})
	}
}

func TestExecStampPRAttribution_AppendsFooter(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{
		getQueue: []getResult{{body: "## Motivation\nfix it\n\nCloses https://github.com/owner/repo/issues/7"}},
	}
	engine.SetPRLinker(linker)

	ti := TaskInfo{ID: "t1", ProjectID: "owner/repo", PRNumber: 5}
	out, err := engine.execStampPRAttribution("t1", newStampPRStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" || !strings.Contains(out.Output, "stamped") {
		t.Errorf("out = %+v, want stamped", out)
	}
	if linker.editCalls != 1 {
		t.Fatalf("editCalls = %d, want 1", linker.editCalls)
	}
	if !strings.HasSuffix(linker.lastBody, attribution.Footer) {
		t.Errorf("body = %q, want footer suffix", linker.lastBody)
	}
}

func TestExecStampPRAttribution_EmptyBodySkips(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{
		getQueue: []getResult{{body: "   \n\t"}},
	}
	engine.SetPRLinker(linker)

	ti := TaskInfo{ID: "t1", ProjectID: "owner/repo", PRNumber: 5}
	out, err := engine.execStampPRAttribution("t1", newStampPRStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" || !strings.Contains(out.Output, "empty pr body") {
		t.Errorf("out = %+v, want empty-body skip message", out)
	}
	if linker.editCalls != 0 {
		t.Errorf("editCalls = %d, want 0 (nothing to stamp)", linker.editCalls)
	}
}

func TestExecStampPRAttribution_IdempotentNoEdit(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{
		getQueue: []getResult{{body: "## Motivation\nfix it\n\n" + attribution.Footer}},
	}
	engine.SetPRLinker(linker)

	ti := TaskInfo{ID: "t1", ProjectID: "owner/repo", PRNumber: 5}
	out, err := engine.execStampPRAttribution("t1", newStampPRStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" || !strings.Contains(out.Output, "already stamped") {
		t.Errorf("out = %+v, want already-stamped", out)
	}
	if linker.editCalls != 0 {
		t.Errorf("editCalls = %d, want 0 (idempotent)", linker.editCalls)
	}
}

func TestExecStampPRAttribution_FetchErrorIsSoftFail(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{getQueue: []getResult{{err: errors.New("network timeout")}}}
	engine.SetPRLinker(linker)

	ti := TaskInfo{ID: "t1", ProjectID: "owner/repo", PRNumber: 5}
	out, err := engine.execStampPRAttribution("t1", newStampPRStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" || linker.editCalls != 0 {
		t.Errorf("out = %+v edits=%d, want soft-fail no edit", out, linker.editCalls)
	}
}

func TestExecStampPRAttribution_EditErrorIsSoftFail(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{
		getQueue: []getResult{{body: "body without footer"}},
		editErr:  errors.New("gh edit failed"),
	}
	engine.SetPRLinker(linker)

	ti := TaskInfo{ID: "t1", ProjectID: "owner/repo", PRNumber: 5}
	out, err := engine.execStampPRAttribution("t1", newStampPRStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" || !strings.Contains(out.Output, "edit failed") {
		t.Errorf("out = %+v, want completed edit-failed note", out)
	}
}

// TestDuplicatePlanAgent_StaleCompletionDoesNotFailWaitHuman reproduces the
// production bug that left task 5a5ad276 stuck: a ResumeStalled race spawned
// two plan agents; the first completed and advanced plan → review_plan
// (wait_human); the second completed seconds later and the engine credited
// its completion to the current step (review_plan), ran resolveNext with no
// human_action var set, failed to match any transition, and set state to
// ExecFailed. HandleHumanAction then refused the user's reject click with
// "task X is not waiting for human action".
//
// The fix: HandleAgentComplete uses the step the agent was actually spawned
// for (tracked on the workflow), and AdvanceStep drops completions whose
// StepID doesn't match the workflow's current step.
func TestDuplicatePlanAgent_StaleCompletionDoesNotFailWaitHuman(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "interactive"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Triage runs → agent flips status to planning → advance into plan step.
	triageAgent := agents.LastID()
	tasks.SetStatus("t1", "planning")
	agents.SimulateComplete("t1")
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: triageAgent, Result: "triaged", Success: true})

	planAgent1 := agents.LastID()
	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "plan" {
		t.Fatalf("precondition: current_step = %q, want plan", ti.Workflow.CurrentStep)
	}

	// Inject a duplicate plan agent as if a ResumeStalled ticker fired
	// during the interactive-spawn window and raced the first agent. The
	// workflow route records what execRunAgent would have set.
	agents.mu.Lock()
	agents.counter++
	planAgent2 := fmt.Sprintf("agent-%d", agents.counter)
	agents.calls = append(agents.calls, startCall{TaskID: "t1", Role: "plan", Mode: "interactive"})
	agents.running["t1"] = planAgent2
	agents.roles["t1/plan"] = planAgent2
	agents.mu.Unlock()
	setWorkflowAgentRoute(t, tasks, "t1", planAgent2, "plan")

	// Agent 1 completes first → workflow advances to review_plan/wait_human.
	agents.SimulateComplete("t1")
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: planAgent1, Result: "plan ready", Success: true})

	ti, _ = tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "review_plan" {
		t.Fatalf("after first plan completion: current_step = %q, want review_plan", ti.Workflow.CurrentStep)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Fatalf("after first plan completion: state = %q, want ExecWaiting", ti.Workflow.State)
	}

	// Agent 2 (the duplicate) finishes seconds later. Old behavior would
	// drive review_plan into ExecFailed. New behavior: dropped as stale.
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: planAgent2, Result: "plan ready", Success: true})

	ti, _ = tasks.GetTask("t1")
	if ti.Workflow.State != ExecWaiting {
		t.Errorf("after stale completion: state = %q, want ExecWaiting", ti.Workflow.State)
	}
	if ti.Workflow.CurrentStep != "review_plan" {
		t.Errorf("after stale completion: current_step = %q, want review_plan", ti.Workflow.CurrentStep)
	}

	// The human's rejection must now succeed — this is the end-to-end
	// symptom the user reported ("task is not waiting for human action").
	if err := engine.HandleHumanAction("t1", "reject", map[string]string{"feedback": "try again"}); err != nil {
		t.Fatalf("HandleHumanAction reject after stale duplicate: %v", err)
	}

	ti, _ = tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "plan" {
		t.Errorf("after reject: current_step = %q, want plan (loop back)", ti.Workflow.CurrentStep)
	}
}

// TestHandleAgentComplete_WaitHumanWithoutActionIsNoop is the defense-in-depth
// guard for the same bug. If a stray agent completion slips past the stale-
// step check and lands on a wait_human step without a human_action var set
// (e.g. an untracked legacy agent where HandleAgentComplete falls back to
// CurrentStep), AdvanceStep must still refuse to run resolveNext. Otherwise
// the workflow would fail on an unmatched transition and permanently seal
// the human review gate.
func TestHandleAgentComplete_WaitHumanWithoutActionIsNoop(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	// Put a task directly at the wait_human step with no agent tracked.
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "plan-review",
		AgentMode: "interactive",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "review_plan",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})

	// Agent callback arrives for the current (wait_human) step with no
	// human_action set. Must be a no-op.
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "untracked-legacy-agent", Result: "unexpected result", Success: true})

	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.State != ExecWaiting {
		t.Errorf("state = %q, want ExecWaiting — stray completion on wait_human must not fail the workflow",
			ti.Workflow.State)
	}
	if ti.Workflow.CurrentStep != "review_plan" {
		t.Errorf("current_step = %q, want review_plan", ti.Workflow.CurrentStep)
	}
	if got := len(ti.Workflow.StepHistory); got != 0 {
		t.Errorf("step_history len = %d, want 0 — stray wait_human completion must not append", got)
	}

	// Rejection still works after the defense kicks in.
	if err := engine.HandleHumanAction("t1", "approve", nil); err != nil {
		t.Fatalf("HandleHumanAction approve: %v", err)
	}
}

// TestResumeStalled_SkipsInflightDispatch exercises the ResumeStalled → race
// that actually produced the duplicate spawn in prod. The ResumeStalled
// ticker fires during the 1-3s window while an interactive plan step is
// still preparing its worktree and starting the claude process — at that
// point no agent is registered yet so HasRunningAgent returns false.
// Without the inflight guard the ticker would call executeSteps → execRunAgent
// and spawn a second agent for the same step. With the guard it must skip.
func TestResumeStalled_SkipsInflightDispatch(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	// Task sitting at an interactive run_agent step with no running agent —
	// the shape ResumeStalled normally resumes.
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "planning",
		AgentMode: "interactive",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "plan",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})

	// Simulate the original dispatch being mid-flight inside AdvanceStep —
	// the per-task advance mutex is held, no agent registered yet (worktree
	// still being created in the real system, fake-claude hasn't started).
	heldMu := engine.taskInflightMutex("t1")
	heldMu.Lock()

	before := agents.CallCount()
	engine.ResumeStalled()
	if got := agents.CallCount(); got != before {
		t.Errorf("ResumeStalled spawned a duplicate agent: calls %d → %d (expected no change while inflight)",
			before, got)
	}

	// Once the original dispatch finishes and releases the advance mutex,
	// a subsequent tick is allowed to resume — that's the real recovery path.
	heldMu.Unlock()

	engine.ResumeStalled()
	if got := agents.CallCount(); got != before+1 {
		t.Errorf("ResumeStalled after inflight cleared: calls %d → %d (want +1)", before, got)
	}
}

// TestResumeStalled_SkipsClaimHeldByOutOfBandDispatcher verifies ResumeStalled
// consults the shared agent.Manager dispatch-claim coordinator (IsDispatching)
// in addition to workflow completion-routing bookkeeping. A claim
// held by a dispatcher the engine has no local visibility into — e.g.
// recovery.RestartStaleInProgress launching via agentorch.Orchestrator
// outside the workflow engine entirely — must still block a concurrent
// ResumeStalled redispatch, closing the exact split-ownership race the
// engine-local route tracking alone cannot see.
func TestResumeStalled_SkipsClaimHeldByOutOfBandDispatcher(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "planning",
		AgentMode: "interactive",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "plan",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})

	// Simulate an out-of-band dispatcher (recovery.RestartStaleInProgress)
	// holding the shared agent.Manager claim for t1 — invisible to the
	// engine's own completion-routing state.
	agents.SetDispatchClaimed("t1", true)

	before := agents.CallCount()
	engine.ResumeStalled()
	if got := agents.CallCount(); got != before {
		t.Errorf("ResumeStalled spawned a duplicate agent while the shared claim was held elsewhere: calls %d → %d",
			before, got)
	}

	// Once the out-of-band dispatcher releases its claim, ResumeStalled may
	// proceed as normal.
	agents.SetDispatchClaimed("t1", false)
	engine.ResumeStalled()
	if got := agents.CallCount(); got != before+1 {
		t.Errorf("ResumeStalled after shared claim released: calls %d → %d (want +1)", before, got)
	}
}

// TestResumeStalled_ConcurrentCallsReserveDispatchWindow verifies the engine
// still keeps its own short-lived per-task reservation between ResumeStalled's
// preflight and the eventual StartAgent call. The shared agent.Manager claim is
// only acquired inside StartAgent, so without this earlier reservation two
// concurrent ResumeStalled loops can both enter executeSteps before any claim
// exists and race a duplicate start.
func TestResumeStalled_ConcurrentCallsReserveDispatchWindow(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.startGate = make(chan struct{})
	agents.startEntered = make(chan struct{}, 2)
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "planning",
		AgentMode: "interactive",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "plan",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})

	var wg sync.WaitGroup
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			engine.ResumeStalled()
		}()
	}

	select {
	case <-agents.startEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first ResumeStalled never reached StartAgent")
	}

	select {
	case <-agents.startEntered:
		t.Fatal("second ResumeStalled reached StartAgent while the first dispatch window was still reserved")
	case <-time.After(100 * time.Millisecond):
	}

	close(agents.startGate)
	wg.Wait()

	if got := agents.CallCount(); got != 1 {
		t.Fatalf("StartAgent call count = %d, want 1", got)
	}
}

// TestDispatchEvent_SkipsClaimHeldByOutOfBandDispatcher verifies DispatchEvent
// also treats a shared agent.Manager dispatch claim held for the task as busy,
// even when the workflow engine has no active local route for the task.
func TestDispatchEvent_SkipsClaimHeldByOutOfBandDispatcher(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "new"})
	agents.SetDispatchClaimed("t1", true)

	_, err := engine.DispatchEvent("t1", "task.created", nil, nil)
	if !errors.Is(err, ErrWorkflowAlreadyActive) {
		t.Fatalf("DispatchEvent with shared claim held elsewhere: err = %v, want ErrWorkflowAlreadyActive", err)
	}
}

// TestExecRunAgent_ConsumesSupervisorSteer verifies the workflow half of a
// watchdog headless nudge: when a step is (re-)dispatched and a steer is
// pending, execRunAgent prepends the correction to the agent's prompt and the
// steer is consumed exactly once. This is the path ResumeStalled drives when it
// re-runs a stalled run_agent step — the case a prior design missed entirely.
func TestExecRunAgent_ConsumesSupervisorSteer(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", AgentMode: "headless"})
	tasks.SetSteer("t1", "stop retrying the failing command")

	step := &Step{
		ID:     "implement",
		Type:   StepRunAgent,
		Config: StepConfig{Role: "implementation", Mode: "headless", Prompt: "do the work"},
	}
	wfExec := &Execution{WorkflowID: "test-simple", CurrentStep: "implement", State: ExecRunning, Variables: map[string]string{}}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}

	got := agents.LastCall().Prompt
	want := "Supervisor course-correction: stop retrying the failing command\n\ndo the work"
	if got != want {
		t.Fatalf("dispatched prompt = %q, want %q", got, want)
	}

	// One-shot: a second dispatch (steer consumed) carries only the step prompt.
	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}
	if got := agents.LastCall().Prompt; got != "do the work" {
		t.Fatalf("second dispatch prompt = %q, want unsteered (steer already consumed)", got)
	}
}

func TestExecRunAgent_ResourcePressureParksWithoutConsumingSteer(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", AgentMode: "headless"})
	tasks.SetSteer("t1", "keep this steer")
	agents.SetAdmitDispatch("disk free 1.0% below minimum 5.0%")

	step := &Step{
		ID:     "implement",
		Type:   StepRunAgent,
		Config: StepConfig{Role: "implementation", Mode: "headless", Prompt: "do the work"},
	}
	wfExec := &Execution{WorkflowID: "test-simple", CurrentStep: "implement", State: ExecRunning, Variables: map[string]string{}}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1", Status: "in-progress"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}
	if got := agents.CallCount(); got != 0 {
		t.Fatalf("StartAgent call count = %d, want 0", got)
	}
	if wfExec.State != ExecWaiting {
		t.Fatalf("workflow state = %s, want waiting", wfExec.State)
	}
	if got := tasks.Reason("t1"); got != "work paused: machine under resource pressure — disk free 1.0% below minimum 5.0%" {
		t.Fatalf("status_reason = %q", got)
	}

	agents.SetAdmitDispatch("")
	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}
	got := agents.LastCall().Prompt
	want := "Supervisor course-correction: keep this steer\n\ndo the work"
	if got != want {
		t.Fatalf("prompt after pressure clears = %q, want %q", got, want)
	}
}

// TestExecRunAgent_TracksSpawnedStep verifies that execRunAgent persists the
// workflow agent route so HandleAgentComplete can route completions back to
// the right step. Without this mapping, a delayed completion from a duplicate
// agent would be credited to whatever CurrentStep happens to be at the moment
// — the exact bug that corrupted review_plan.
func TestExecRunAgent_TracksSpawnedStep(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "planning", AgentMode: "interactive"})

	step := &Step{
		ID:   "plan",
		Type: StepRunAgent,
		Config: StepConfig{
			Role:   "plan",
			Mode:   "interactive",
			Prompt: "p",
		},
	}
	wfExec := &Execution{
		WorkflowID:  "test-simple",
		CurrentStep: "plan",
		State:       ExecRunning,
		Variables:   map[string]string{},
	}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}
	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}

	agentID := agents.LastID()
	if gotStep, tracked := lookupWorkflowAgentRoute(t, engine, "t1", agentID); !tracked {
		t.Fatalf("agentRoutes missing entry for agent %s", agentID)
	} else if gotStep != "plan" {
		t.Errorf("agentRoutes[%s] = %q, want plan", agentID, gotStep)
	}

	// Completing the agent must clear its mapping so the map doesn't grow
	// unbounded across long-lived sessions.
	tasks.SetStatus("t1", "plan-review")
	agents.SimulateComplete("t1")
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: agentID, Result: "done", Success: true})

	if _, stillThere := lookupWorkflowAgentRoute(t, engine, "t1", agentID); stillThere {
		t.Errorf("agentRoutes still has %s after completion — mapping leaked", agentID)
	}
}

func TestExecuteSteps_CycleDetection(t *testing.T) {
	store := newTestStoreWith(t, "test-cycle.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "cycle1", Status: "todo", AgentMode: "headless"})

	err := engine.StartWorkflow("cycle1", "test-cycle")
	if err == nil {
		t.Fatal("expected error for cyclic workflow, got nil")
	}

	cycleErr, ok := errors.AsType[*CycleError](err)
	if !ok {
		t.Fatalf("expected *CycleError, got %T: %v", err, err)
	}
	if cycleErr.StepID == "" {
		t.Error("CycleError.StepID is empty")
	}
	if cycleErr.At <= cycleErr.FirstAt {
		t.Errorf("CycleError.At (%d) should be > FirstAt (%d)", cycleErr.At, cycleErr.FirstAt)
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

	// Create initial commit so origin/main exists.
	f := filepath.Join(dir, "README.md")
	if err := os.WriteFile(f, []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "init")

	// Teach git about a local "remote" so origin/main resolves.
	// We use the repo itself as its own origin (bare clone not needed for tests).
	run("remote", "add", "origin", dir)
	run("fetch", "origin")

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

// TestExecRunAgent_DispatchInFlightWaits asserts a run_agent step whose
// StartAgent loses the per-task dispatch claim parks the workflow in
// ExecWaiting (the claim holder's agent will drive it) rather than failing the
// step and routing toward human-required.
func TestExecRunAgent_DispatchInFlightWaits(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	agents := newMockAgents()
	agents.SetFailSpawn(ErrDispatchInFlight)
	engine := NewEngine(store, tasks, agents, discardLogger())

	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatalf("StartWorkflow returned error, want nil (dispatch-in-flight is benign): %v", err)
	}

	ti, _ := tasks.GetTask("t1")
	if ti.Workflow == nil || ti.Workflow.State != ExecWaiting {
		t.Fatalf("workflow state = %v, want ExecWaiting", ti.Workflow)
	}
	if ti.Status == "human-required" {
		t.Errorf("dispatch-in-flight must not flip task to human-required")
	}
	if agents.CallCount() != 0 {
		t.Errorf("no agent should have been recorded as started, got %d calls", agents.CallCount())
	}
}

func TestExecRunAgent_PanicClearsDispatchingClaim(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	agents := &panicStartAgents{mockAgents: newMockAgents()}
	engine := NewEngine(store, tasks, agents, discardLogger())

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = engine.StartWorkflow("t1", "test-simple")
	}()

	if recovered == nil {
		t.Fatal("expected StartWorkflow to panic when StartAgent panics")
		return
	}
	if engine.hasTrackedAgentForTaskStep("t1", "triage") {
		t.Fatal("pending step start leaked after panic")
	}
}

// TestExecRunAgent_RealSpawnErrorPropagates is the contrast to the test above:
// a genuine (non-dispatch-in-flight) spawn error must still surface.
func TestExecRunAgent_RealSpawnErrorPropagates(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	agents := newMockAgents()
	agents.SetFailSpawn(errors.New("worktree boom"))
	engine := NewEngine(store, tasks, agents, discardLogger())

	if err := engine.StartWorkflow("t1", "test-simple"); err == nil {
		t.Fatal("StartWorkflow should propagate a real spawn error")
	}
}

// TestStartWorkflow_InitialDispatchFailure_EscalatesPermanentError guards
// against the workflow.external-create.failed gap: a permanent dispatch
// error (e.g. ErrNoProjectAssigned) on the very FIRST execution of a
// workflow — StartWorkflow/DispatchEvent, not a ResumeStalled retry — must
// classify and escalate exactly like ResumeStalled already does. Before the
// fix, startWorkflowCore returned the raw error to its caller (who only logs
// it) and left the task's Workflow live/non-terminal, so the task silently
// sat in limbo until some later resume attempt happened to escalate it.
func TestStartWorkflow_InitialDispatchFailure_EscalatesPermanentError(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	agents := newMockAgents()
	agents.SetFailSpawn(fmt.Errorf("task t1 has no project_id: refusing to start triage agent without isolated worktree: %w", ErrNoProjectAssigned))
	engine := NewEngine(store, tasks, agents, discardLogger())

	if err := engine.StartWorkflow("t1", "test-simple"); err == nil {
		t.Fatal("StartWorkflow should propagate the spawn error")
	}

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required on the very first dispatch attempt", got.Status)
	}
	if !strings.Contains(got.StatusReason, "no project could be assigned") {
		t.Errorf("status reason = %q, want it to mention the classified no-project reason", got.StatusReason)
	}
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
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())

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

// TestExecVerifyCommits_ParksWhileSiblingRunning asserts the step re-arms the
// implement run_agent step and parks the workflow in ExecWaiting (returning
// errStepParked) when another agent is still working the task — instead of
// flipping a task with live work to human-required OR completing the workflow
// (whose status-change cascade would re-dispatch over the sibling).
func TestExecVerifyCommits_ParksWhileSiblingRunning(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID: "t1", Status: "in-progress",
		Workflow: &Execution{WorkflowID: "x", CurrentStep: "verify_commits", State: ExecRunning},
	})
	agents := newMockAgents()
	agents.mu.Lock()
	agents.running["t1"] = "sibling" // a different agent than the completer
	agents.mu.Unlock()
	engine := NewEngine(store, tasks, agents, discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: makeGitRepo(t, false /* no commits */), ok: true})

	wfExec := &Execution{WorkflowID: "x", CurrentStep: "verify_commits", State: ExecRunning}
	wfExec.RecordStep(StepRecord{StepID: "implement", Status: "failed", AgentID: "completer"})

	_, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), wfExec, TaskInfo{ID: "t1"})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	if wfExec.State != ExecWaiting {
		t.Errorf("State = %q, want ExecWaiting", wfExec.State)
	}
	if wfExec.CurrentStep != "implement" {
		t.Errorf("CurrentStep = %q, want implement (re-armed run_agent step)", wfExec.CurrentStep)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged in-progress (no human-required flip)", ti.Status)
	}
}

// TestExecVerifyCommits_ExcludesCompletingAgent guards against the self-deadlock
// the deferral could otherwise cause: the agent whose completion triggered the
// step still reads as running (its done channel closes after onComplete), so it
// must be excluded. With no genuine sibling, the normal no-commits verdict fires.
func TestExecVerifyCommits_ExcludesCompletingAgent(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	agents.mu.Lock()
	agents.running["t1"] = "completer" // the same agent whose completion drives this step
	agents.mu.Unlock()
	engine := NewEngine(store, tasks, agents, discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: makeGitRepo(t, false /* no commits */), ok: true})

	wfExec := &Execution{}
	wfExec.RecordStep(StepRecord{StepID: "implement", Status: "failed", AgentID: "completer"})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), wfExec, TaskInfo{ID: "t1"})
	if errors.Is(err, errStepParked) {
		t.Fatal("must not park when only the completing agent appears running")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "human-required") {
		t.Errorf("Output = %q, want the human-required verdict", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required (genuine crash, no sibling)", ti.Status)
	}
}

// TestExecuteSteps_VerifyCommitsParkDoesNotComplete is the regression test for
// the Copilot finding: a deferred verify_commits must NOT complete the workflow,
// because OnWorkflowComplete cascades on the (still in-progress) status and would
// re-dispatch simple-task-implement — whose execRunAgent StopAgentsForTask would
// kill the still-running sibling. executeSteps must return a nil completion
// (parked, ExecWaiting), never a CompletionInfo.
func TestExecuteSteps_VerifyCommitsParkDoesNotComplete(t *testing.T) {
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var impl *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-implement" {
			impl = &defs[i]
			break
		}
	}
	if impl == nil {
		t.Fatal("simple-task-implement builtin not found")
		return
	}

	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID: "t1", Status: "in-progress",
		Workflow: &Execution{WorkflowID: "simple-task-implement", CurrentStep: "verify_commits", State: ExecRunning},
	})
	agents := newMockAgents()
	agents.mu.Lock()
	agents.running["t1"] = "sibling"
	agents.mu.Unlock()
	engine := NewEngine(store, tasks, agents, discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: makeGitRepo(t, false /* no commits */), ok: true})

	wf := &Execution{WorkflowID: "simple-task-implement", CurrentStep: "verify_commits", State: ExecRunning}
	wf.RecordStep(StepRecord{StepID: "implement", Status: "failed", AgentID: "completer"})

	verifyStep := impl.StepByID("verify_commits")
	if verifyStep == nil {
		t.Fatal("verify_commits step not found in simple-task-implement")
		return
	}
	comp, err := engine.executeSteps("t1", impl, verifyStep, wf)
	if err != nil {
		t.Fatalf("executeSteps: %v", err)
	}
	if comp != nil {
		t.Errorf("executeSteps returned a completion (workflow finished → cascade would re-dispatch over the sibling); want nil (parked)")
	}
	if wf.State != ExecWaiting {
		t.Errorf("State = %q, want ExecWaiting", wf.State)
	}
	if wf.CurrentStep != "implement" {
		t.Errorf("CurrentStep = %q, want implement (re-armed for ResumeStalled)", wf.CurrentStep)
	}
}

func TestExecVerifyCommits_NoGetterSkips(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	// worktrees nil by default

	ti := TaskInfo{ID: "t1"}
	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "skipped") {
		t.Errorf("Output = %q, want skipped", out.Output)
	}
}

func TestExecVerifyCommits_NoWorktreeSkips(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{ok: false})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "skipped") {
		t.Errorf("Output = %q, want skipped", out.Output)
	}
}

func TestExecVerifyCommits_WithCommitsVerified(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	wtDir := makeGitRepo(t, true /* withExtraCommit */)
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "commits verified") {
		t.Errorf("Output = %q, want 'commits verified'", out.Output)
	}
	// Task status must not change.
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Errorf("task status = %q, want in-progress", ti.Status)
	}
}

func TestExecVerifyCommits_GitErrorFlipsHumanRequired(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	// Path exists but is not a git repo — git log returns non-zero,
	// simulating the broken-worktree scenario from the synapse→sybra rename.
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: t.TempDir(), ok: true})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "git error") {
		t.Errorf("Output = %q, want 'git error'", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
	if reason := tasks.Reason("t1"); !strings.Contains(reason, "worktree git error") {
		t.Errorf("status reason = %q, want 'worktree git error'", reason)
	}
}

func TestExecVerifyCommits_GitErrorReasonContainsDiagnosis(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	// Path exists but is not a git repo — `git log` and `git status`
	// both fail with the same fatal so diagnosis surfaces it.
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: t.TempDir(), ok: true})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	reason := tasks.Reason("t1")
	if !strings.Contains(reason, "worktree git error") {
		t.Errorf("status reason = %q, want 'worktree git error'", reason)
	}
	if !strings.Contains(reason, "git status") {
		t.Errorf("status reason = %q, want diagnosis from `git status`", reason)
	}
}

func TestExecVerifyCommits_RetriesAfterTransientFailure(t *testing.T) {
	prev := verifyCommitsRetrySleep
	t.Cleanup(func() { verifyCommitsRetrySleep = prev })

	wtDir := makeGitRepo(t, true /* withExtraCommit */)

	// Simulate transient lock: place .git/index.lock that will be removed
	// during the retry sleep, so the second `git log` succeeds. The lock
	// path differs between linked and primary worktrees; here makeGitRepo
	// returns a primary repo, so .git is a directory.
	lockPath := filepath.Join(wtDir, ".git", "index.lock")
	if err := os.WriteFile(lockPath, []byte("locked\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	verifyCommitsRetrySleep = func(_ time.Duration) {
		_ = os.Remove(lockPath)
	}

	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	// First call may or may not actually be blocked by the lock depending
	// on git behavior — but the retry path always runs once per error,
	// and we just need the final outcome to be "commits verified".
	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "commits verified") {
		t.Errorf("Output = %q, want 'commits verified'", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Errorf("task status = %q, want in-progress", ti.Status)
	}
}

func TestExecVerifyCommits_RetriesTransientBadHEAD(t *testing.T) {
	prevSleep := verifyCommitsRetrySleep
	prevBackoffs := verifyCommitsRetryBackoffs
	t.Cleanup(func() {
		verifyCommitsRetrySleep = prevSleep
		verifyCommitsRetryBackoffs = prevBackoffs
	})
	verifyCommitsRetrySleep = func(time.Duration) {}
	verifyCommitsRetryBackoffs = []time.Duration{time.Nanosecond, time.Nanosecond}

	wtDir := makeGitRepo(t, true /* withExtraCommit */)
	countFile := filepath.Join(t.TempDir(), "git-log-count")
	withFakeGit(t, `#!/bin/sh
if [ "$1" = "log" ]; then
  count=0
  if [ -f "`+countFile+`" ]; then
    count=$(cat "`+countFile+`")
  fi
  count=$((count + 1))
  printf "%s" "$count" > "`+countFile+`"
  if [ "$count" = "1" ]; then
    echo "fatal: bad object HEAD" >&2
    exit 128
  fi
fi
exec "{{REAL_GIT}}" "$@"
`)

	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "commits verified") {
		t.Errorf("Output = %q, want 'commits verified'", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Errorf("task status = %q, want in-progress", ti.Status)
	}
	if got := strings.TrimSpace(readFile(t, countFile)); got != "2" {
		t.Errorf("git log calls = %q, want 2", got)
	}
}

func TestExecVerifyCommits_DurableBadHEADEscalatesAfterRetries(t *testing.T) {
	prevSleep := verifyCommitsRetrySleep
	prevBackoffs := verifyCommitsRetryBackoffs
	t.Cleanup(func() {
		verifyCommitsRetrySleep = prevSleep
		verifyCommitsRetryBackoffs = prevBackoffs
	})
	verifyCommitsRetrySleep = func(time.Duration) {}
	verifyCommitsRetryBackoffs = []time.Duration{time.Nanosecond, time.Nanosecond}

	wtDir := makeGitRepo(t, true /* withExtraCommit */)
	countFile := filepath.Join(t.TempDir(), "git-log-count")
	withFakeGit(t, `#!/bin/sh
if [ "$1" = "log" ]; then
  count=0
  if [ -f "`+countFile+`" ]; then
    count=$(cat "`+countFile+`")
  fi
  count=$((count + 1))
  printf "%s" "$count" > "`+countFile+`"
  echo "fatal: bad object HEAD" >&2
  exit 128
fi
exec "{{REAL_GIT}}" "$@"
`)

	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "git error") {
		t.Errorf("Output = %q, want git error", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
	if reason := tasks.Reason("t1"); !strings.Contains(reason, "bad object HEAD") {
		t.Errorf("status reason = %q, want bad object HEAD", reason)
	}
	if got := strings.TrimSpace(readFile(t, countFile)); got != "3" {
		t.Errorf("git log calls = %q, want 3", got)
	}
}

func TestExecVerifyCommits_FetchesMissingLocalHeadObject(t *testing.T) {
	prevSleep := verifyCommitsRetrySleep
	prevBackoffs := verifyCommitsRetryBackoffs
	t.Cleanup(func() {
		verifyCommitsRetrySleep = prevSleep
		verifyCommitsRetryBackoffs = prevBackoffs
	})
	verifyCommitsRetrySleep = func(time.Duration) {}
	verifyCommitsRetryBackoffs = []time.Duration{time.Nanosecond}

	remote := filepath.Join(t.TempDir(), "origin.git")
	runGitAt(t, "", "init", "--bare", remote)

	wtDir := t.TempDir()
	runGitAt(t, wtDir, "init", "-b", "main")
	runGitAt(t, wtDir, "config", "user.email", "test@test.com")
	runGitAt(t, wtDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(wtDir, "README.md"), []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitAt(t, wtDir, "add", "README.md")
	runGitAt(t, wtDir, "commit", "-m", "init")
	runGitAt(t, wtDir, "remote", "add", "origin", remote)
	runGitAt(t, wtDir, "push", "-u", "origin", "main")
	runGitAt(t, wtDir, "checkout", "-b", "fix/missing-object")
	if err := os.WriteFile(filepath.Join(wtDir, "change.txt"), []byte("change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitAt(t, wtDir, "add", "change.txt")
	runGitAt(t, wtDir, "commit", "-m", "feat: task work")
	head := strings.TrimSpace(runGitAt(t, wtDir, "rev-parse", "HEAD"))
	runGitAt(t, wtDir, "push", "-u", "origin", "HEAD:fix/missing-object")
	runGitAt(t, wtDir, "fetch", "origin",
		"+refs/heads/fix/missing-object:refs/remotes/origin/fix/missing-object",
		"+refs/heads/main:refs/remotes/origin/main")
	if got := strings.TrimSpace(runGitAt(t, wtDir, "rev-parse", "refs/remotes/origin/fix/missing-object")); got != head {
		t.Fatalf("origin/fix/missing-object = %q, want %q", got, head)
	}

	badSiblingRef := filepath.Join(wtDir, ".git", "refs", "heads", "feat", "bad-sibling")
	if err := os.MkdirAll(filepath.Dir(badSiblingRef), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badSiblingRef, []byte(strings.Repeat("f", 40)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	objectPath := filepath.Join(wtDir, ".git", "objects", head[:2], head[2:])
	if err := os.Remove(objectPath); err != nil {
		t.Fatalf("remove local head object %s: %v", objectPath, err)
	}
	if out, err := gitCombinedAt(wtDir, "status", "--short", "--branch"); err == nil || !strings.Contains(out, "bad object HEAD") {
		t.Fatalf("git status after object removal err=%v out=%q, want bad object HEAD", err, out)
	}

	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", Branch: "fix/missing-object"})
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, TaskInfo{ID: "t1", Branch: "fix/missing-object"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "commits verified") {
		t.Fatalf("Output = %q, reason = %q, want commits verified after fetch recovery", out.Output, tasks.Reason("t1"))
	}
	if got := strings.TrimSpace(runGitAt(t, wtDir, "cat-file", "-t", head)); got != "commit" {
		t.Fatalf("recovered object type = %q, want commit", got)
	}
	if _, err := os.Stat(badSiblingRef); !os.IsNotExist(err) {
		t.Fatalf("broken sibling ref still exists after recovery: stat err=%v", err)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Fatalf("task status = %q, want unchanged in-progress", ti.Status)
	}
}

// TestExecVerifyCommits_BranchAtBaseFlipsHumanRequired covers the case where
// the agent reported success but committed nothing: HEAD == origin/main. This
// used to mark the task done on the theory that the fix might already be on
// origin/main via a different branch, but that check (branchMergedIntoBase)
// can never actually distinguish "already merged elsewhere" from "nothing was
// committed" at this call site — an empty baseRef..HEAD log range and "HEAD
// is an ancestor of baseRef" are the same git fact, so it was true on every
// call. Confirmed live: two foundational tasks landed `done` with prNumber 0
// and a branch byte-identical to origin/main — zero code shipped. A human
// must see this instead.
func TestExecVerifyCommits_BranchAtBaseFlipsHumanRequired(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	wtDir := makeGitRepo(t, false /* no extra commit; HEAD == origin/main */)
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "no commits") {
		t.Errorf("Output = %q, want 'no commits'", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
	if reason := tasks.Reason("t1"); !strings.Contains(reason, "no commits") {
		t.Errorf("status reason = %q, want 'no commits'", reason)
	}
}

func TestExecVerifyCommits_NoCommitAuthorRunRetriesOnce(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	wtDir := makeGitRepo(t, false /* no extra commit; HEAD == origin/main */)
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	wfExec := &Execution{Variables: map[string]string{}}
	wfExec.RecordStep(StepRecord{StepID: "implement", Status: "completed", AgentID: "a1", Provider: "claude"})
	ti := TaskInfo{
		ID: "t1", Status: "in-progress",
		AgentRuns: []AgentRunInfo{{AgentID: "a1", Role: "implementation"}},
	}

	_, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), wfExec, ti)
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	if wfExec.CurrentStep != "implement" || wfExec.State != ExecWaiting {
		t.Fatalf("workflow = %+v, want rearmed implement/ExecWaiting", wfExec)
	}
	if got := wfExec.Variables["step.verify_commits.no_commit_retry"]; got != "1" {
		t.Fatalf("no_commit_retry = %q, want 1", got)
	}
	if got := wfExec.Variables[verifyReaskNoteVar]; !strings.Contains(got, "without producing commits") {
		t.Fatalf("verify reask note = %q, want no-commit guidance", got)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Fatalf("status = %q, want in-progress retry", ti.Status)
	}
}

func TestExecVerifyCommits_NoCommitAuthorRunRetriesOnceWithSubagentDiagnosis(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	wtDir := makeGitRepo(t, false /* no extra commit; HEAD == origin/main */)
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	wfExec := &Execution{Variables: map[string]string{}}
	wfExec.RecordStep(StepRecord{StepID: "implement", Status: "completed", AgentID: "a1", Provider: "claude"})
	ti := TaskInfo{
		ID: "t1", Status: "in-progress",
		AgentRuns: []AgentRunInfo{{AgentID: "a1", Role: "implementation", SubagentCallCount: 2}},
	}

	_, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), wfExec, ti)
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	got := wfExec.Variables[verifyReaskNoteVar]
	if !strings.Contains(got, "background subagent") {
		t.Fatalf("verify reask note = %q, want background-subagent diagnosis", got)
	}
	if reason := tasks.Reason("t1"); !strings.Contains(reason, "background subagent handoff") {
		t.Fatalf("status reason = %q, want background subagent diagnosis", reason)
	}
}

func TestExecVerifyCommits_NoCommitAuthorRunEscalatesAfterRetry(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())

	wtDir := makeGitRepo(t, false /* no extra commit; HEAD == origin/main */)
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	wfExec := &Execution{Variables: map[string]string{"step.verify_commits.no_commit_retry": "1"}}
	wfExec.RecordStep(StepRecord{StepID: "implement", Status: "completed", AgentID: "a1", Provider: "claude"})
	ti := TaskInfo{
		ID: "t1", Status: "in-progress",
		AgentRuns: []AgentRunInfo{{AgentID: "a1", Role: "implementation"}},
	}

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), wfExec, ti)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.Output, "no commits") {
		t.Fatalf("Output = %q, want no commits", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required after one retry", ti.Status)
	}
}

// TestExecVerifyCommits_BranchAncestorOfBaseFlipsHumanRequired covers the
// regression from issue #670: HEAD is an ancestor of origin/main (branch tip
// equals an older commit on main, with newer commits on top — typical of
// squash-merge followed by additional PRs). `git log origin/main..HEAD` is
// empty AND HEAD != base.tip. #670 originally fixed this by marking the task
// done outright (the theory: the work must already be on origin). That
// theory cannot be verified from ancestry alone — "HEAD is an ancestor of
// base because its own commits already landed" and "HEAD is an ancestor of
// base because it never had any commits to begin with" are the same git
// fact, indistinguishable by `merge-base --is-ancestor`. Silently marking
// done was proven live to misfire on the second case (issues #2658, #2659
// landed `done` with zero code). A human must confirm either way now.
func TestExecVerifyCommits_BranchAncestorOfBaseFlipsHumanRequired(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	wtDir := makeGitRepoBehindOrigin(t)
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "no commits") {
		t.Errorf("Output = %q, want 'no commits'", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
}

// TestExecVerifyCommits_AgentFailedFlipsHumanRequired covers the false-positive
// auto-close: a fresh worktree branch sits exactly on origin/main (no commits
// ahead, HEAD == base.tip — git-identical to "already merged") *because the
// implementation agent crashed before committing*, not because the fix shipped.
// With a failed agent step in history, the task must flip to human-required so
// the run is surfaced, never silently marked done.
func TestExecVerifyCommits_AgentFailedFlipsHumanRequired(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	// Same git state as TestExecVerifyCommits_BranchAtBaseFlipsHumanRequired:
	// HEAD == origin/main.
	wtDir := makeGitRepo(t, false /* no extra commit; HEAD == origin/main */)
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	// The upstream implement step failed (agent died mid-run).
	wfExec := &Execution{StepHistory: []StepRecord{
		{StepID: "implement", Status: "failed", AgentID: "a1", Provider: "claude"},
	}}

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), wfExec, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "agent failed before commit") {
		t.Errorf("Output = %q, want 'agent failed before commit'", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
	if reason := tasks.Reason("t1"); !strings.Contains(reason, "agent failed before committing") {
		t.Errorf("status reason = %q, want 'agent failed before committing'", reason)
	}
}

// TestExecVerifyCommits_AutoCommitsUncommittedWork verifies that a worktree
// with no commits ahead of base but dirty (uncommitted) files is recovered by
// auto-committing rather than escalated to human-required — the scenario
// where an implementation agent finished its work but was interrupted (or
// simply forgot) before running `git commit`.
func TestExecVerifyCommits_AutoCommitsUncommittedWork(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	// HEAD == origin/main (no commits ahead), but leave uncommitted work
	// sitting dirty in the worktree.
	wtDir := makeGitRepo(t, false /* no extra commit */)
	if err := os.WriteFile(filepath.Join(wtDir, "uncommitted.txt"), []byte("finished work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "commits verified") {
		t.Errorf("Output = %q, want 'commits verified'", out.Output)
	}
	// Task must not be escalated — the dirty work was recovered.
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Errorf("task status = %q, want in-progress (not escalated)", ti.Status)
	}
	// The file must now be committed on the branch.
	cmd := exec.Command("git", "log", "origin/main..HEAD", "--name-only")
	cmd.Dir = wtDir
	log, err := cmd.Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(string(log), "uncommitted.txt") {
		t.Errorf("git log = %q, want it to contain the auto-committed file", log)
	}
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = wtDir
	statusOut, err := statusCmd.Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if strings.TrimSpace(string(statusOut)) != "" {
		t.Errorf("worktree still dirty after auto-commit: %q", statusOut)
	}
}

func TestExecVerifyCommits_AutoCommitRemoteReconcileGitErrorEscalates(t *testing.T) {
	countFile := filepath.Join(t.TempDir(), "read-tree-count")
	withFakeGit(t, `#!/bin/sh
if [ "$1" = "read-tree" ] && [ "$2" = "HEAD" ]; then
  count=0
  if [ -f "`+countFile+`" ]; then
    count=$(cat "`+countFile+`")
  fi
  count=$((count + 1))
  printf "%s" "$count" > "`+countFile+`"
  if [ "$count" = "2" ]; then
    echo "fatal: synthetic read-tree failure" >&2
    exit 128
  fi
fi
exec "{{REAL_GIT}}" "$@"
`)

	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())

	wtDir := makeGitRepo(t, false /* no extra commit */)
	if err := os.WriteFile(filepath.Join(wtDir, "uncommitted.txt"), []byte("finished work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, TaskInfo{ID: "t1", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "git error") {
		t.Fatalf("Output = %q, want git error after remote reconcile failure", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "human-required" {
		t.Fatalf("task status = %q, want human-required", ti.Status)
	}
	if reason := tasks.Reason("t1"); !strings.Contains(reason, "after auto-commit remote reconcile") {
		t.Fatalf("status reason = %q, want auto-commit remote reconcile context", reason)
	}
	if got := strings.TrimSpace(readFile(t, countFile)); got != "2" {
		t.Fatalf("read-tree calls = %q, want 2 (pre- and post-auto-commit probes)", got)
	}
}

func TestExecVerifyCommits_AutoCommitAdoptsEquivalentRemoteCommitAfterRetry(t *testing.T) {
	prevSleep := verifyCommitsRetrySleep
	prevBackoffs := verifyCommitsRetryBackoffs
	t.Cleanup(func() {
		verifyCommitsRetrySleep = prevSleep
		verifyCommitsRetryBackoffs = prevBackoffs
	})

	remote := filepath.Join(t.TempDir(), "origin.git")
	runGitAt(t, "", "init", "--bare", remote)

	wtDir := t.TempDir()
	runGitAt(t, wtDir, "init", "-b", "main")
	runGitAt(t, wtDir, "config", "user.email", "test@test.com")
	runGitAt(t, wtDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(wtDir, "README.md"), []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitAt(t, wtDir, "add", "README.md")
	runGitAt(t, wtDir, "commit", "-m", "init")
	runGitAt(t, wtDir, "remote", "add", "origin", remote)
	runGitAt(t, wtDir, "push", "-u", "origin", "main")

	const branch = "feat/verify-commits-race"
	runGitAt(t, wtDir, "checkout", "-b", branch)
	runGitAt(t, wtDir, "push", "-u", "origin", branch)

	const (
		fileName = "verify-commits-race.txt"
		content  = "verify_commits race\n"
	)
	if err := os.WriteFile(filepath.Join(wtDir, fileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	stageDir := t.TempDir()
	var pushed sync.Once
	verifyCommitsRetryBackoffs = []time.Duration{time.Nanosecond}
	verifyCommitsRetrySleep = func(time.Duration) {
		pushed.Do(func() {
			repoDir := filepath.Join(stageDir, "repo")
			runGitAt(t, "", "clone", remote, repoDir)
			runGitAt(t, repoDir, "checkout", "-B", branch, "origin/"+branch)
			if err := os.WriteFile(filepath.Join(repoDir, fileName), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			runGitAt(t, repoDir, "add", fileName)
			runGitAt(t, repoDir, "-c", "user.email=fake-claude@test.local", "-c", "user.name=Fake Claude", "commit", "-m", "feat: remote race")
			runGitAt(t, repoDir, "push", "origin", "HEAD:refs/heads/"+branch)
		})
	}

	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, TaskInfo{ID: "t1", Branch: branch})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "commits verified") {
		t.Fatalf("Output = %q, want commits verified", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Fatalf("task status = %q, want in-progress", ti.Status)
	}

	runGitAt(t, wtDir, "fetch", "origin", "+refs/heads/"+branch+":refs/remotes/origin/"+branch)
	localHead := strings.TrimSpace(runGitAt(t, wtDir, "rev-parse", "HEAD"))
	remoteHead := strings.TrimSpace(runGitAt(t, wtDir, "rev-parse", "refs/remotes/origin/"+branch))
	if localHead != remoteHead {
		t.Fatalf("local HEAD %q != remote HEAD %q; want verify_commits to adopt the equivalent remote commit", localHead, remoteHead)
	}
	if got := strings.TrimSpace(runGitAt(t, wtDir, "rev-list", "--count", "origin/main..HEAD")); got != "1" {
		t.Fatalf("origin/main..HEAD commit count = %q, want 1 implementation lineage", got)
	}
	if status := strings.TrimSpace(runGitAt(t, wtDir, "status", "--porcelain")); status != "" {
		t.Fatalf("worktree dirty after reconcile: %q", status)
	}
}

// TestExecVerifyCommits_EmptyRemoteBranchFlipsHumanRequired covers the
// equivalent-tree remote-adopt bug: a task branch is pushed to origin but is
// byte-identical to base (zero commits ahead), e.g. because the
// implementation agent handed off to a background subagent and exited
// without producing any work. verify_commits must not treat the pushed,
// empty branch as completed work just because its tree matches the local
// (also-empty) worktree tree.
func TestExecVerifyCommits_EmptyRemoteBranchFlipsHumanRequired(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "origin.git")
	runGitAt(t, "", "init", "--bare", remote)

	wtDir := t.TempDir()
	runGitAt(t, wtDir, "init", "-b", "main")
	runGitAt(t, wtDir, "config", "user.email", "test@test.com")
	runGitAt(t, wtDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(wtDir, "README.md"), []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitAt(t, wtDir, "add", "README.md")
	runGitAt(t, wtDir, "commit", "-m", "init")
	runGitAt(t, wtDir, "remote", "add", "origin", remote)
	runGitAt(t, wtDir, "push", "-u", "origin", "main")

	// Task branch pushed to origin with no extra commits — byte-identical to
	// base on both ends.
	const branch = "feat/verify-commits-empty-remote"
	runGitAt(t, wtDir, "checkout", "-b", branch)
	runGitAt(t, wtDir, "push", "-u", "origin", branch)

	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, TaskInfo{ID: "t1", Branch: branch})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "no commits") {
		t.Fatalf("Output = %q, want 'no commits' (empty pushed branch must not be adopted)", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Fatalf("task status = %q, want human-required", ti.Status)
	}
}

func TestRecordFinalCommitState_ContextCancelDoesNotHang(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentRuns: []AgentRunInfo{{AgentID: "a1"}},
	})
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())

	wtDir := makeGitRepo(t, true /* withExtraCommit */)
	withFakeGit(t, `#!/bin/sh
if [ "$1" = "rev-parse" ] && [ "$2" = "--verify" ] && [ "$3" = "HEAD^{commit}" ]; then
  sleep 30
fi
exec "{{REAL_GIT}}" "$@"
`)

	parentCtx, cancel := context.WithCancel(context.Background())
	engine.SetContext(parentCtx)
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	wfExec := &Execution{StepHistory: []StepRecord{{StepID: "implement", Status: "completed", AgentID: "a1"}}}

	start := time.Now()
	engine.recordFinalCommitState("t1", wfExec, wtDir, finalCommitSourceAgent)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("recordFinalCommitState took %v after ctx cancel; want prompt return", elapsed)
	}
	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.AgentRuns[0].HeadSHA != "" {
		t.Fatalf("head_sha = %q, want empty when rev-parse is canceled", ti.AgentRuns[0].HeadSHA)
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

func TestExecRequireSidecar_PlanCritiquePresent(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "planning", PlanCritique: "# critique\n"})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execRequireSidecar("t1", newRequireSidecarStep("plan_critique"), TaskInfo{ID: "t1", PlanCritique: "# critique\n"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "present") {
		t.Errorf("Output = %q, want 'present'", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status == "human-required" {
		t.Errorf("unexpected status flip to human-required")
	}
}

func TestExecRequireSidecar_PlanCritiqueMissingFlipsHumanRequired(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "planning"})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execRequireSidecar("t1", newRequireSidecarStep("plan_critique"), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed (mechanical step)", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
	if !strings.Contains(tasks.Reason("t1"), "plan critique") {
		t.Errorf("reason = %q, want substring 'plan critique'", tasks.Reason("t1"))
	}
}

func TestExecRequireSidecar_AllowMissingDoesNotFlipHumanRequired(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "planning"})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execRequireSidecar("t1", newRequireSidecarAllowMissingStep("plan_critique"), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "skipped") {
		t.Errorf("Output = %q, want skipped warning", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status == "human-required" {
		t.Fatal("allow_missing should not flip task to human-required")
	}
}

func TestExecRequireSidecar_AllowMissingRejectedOutsidePlanCritique(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "planning"})
	engine := newEngineForEval(t, tasks)

	step := newRequireSidecarStep("code_review")
	step.Config.AllowMissing = true
	_, err := engine.execRequireSidecar("t1", step, TaskInfo{ID: "t1"})
	if err == nil {
		t.Fatal("expected allow_missing validation error")
	}
	if !strings.Contains(err.Error(), "allow_missing is only supported") {
		t.Fatalf("error = %v, want allow_missing validation", err)
	}
}

func TestExecRequireSidecar_CodeReviewMissingFlipsHumanRequired(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execRequireSidecar("t1", newRequireSidecarStep("code_review"), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
	if !strings.Contains(tasks.Reason("t1"), "code review") {
		t.Errorf("reason = %q, want substring 'code review'", tasks.Reason("t1"))
	}
}

func TestExecRequireSidecar_PlanDecisionsPresent(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "planning", PlanDecisions: "# Decisions\n"})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execRequireSidecar("t1", newRequireSidecarStep("plan_decisions"), TaskInfo{ID: "t1", PlanDecisions: "# Decisions\n"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "plan decisions present") {
		t.Errorf("Output = %q, want plan decisions present", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status == "human-required" {
		t.Errorf("unexpected status flip to human-required")
	}
}

func TestExecRequireSidecar_WhitespaceOnlyTreatedAsMissing(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "planning"})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execRequireSidecar("t1", newRequireSidecarStep("plan_critique"), TaskInfo{ID: "t1", PlanCritique: "   \n\t  \n"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
}

func TestExecRequireSidecar_UnknownSidecarErrors(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "planning"})
	engine := newEngineForEval(t, tasks)

	_, err := engine.execRequireSidecar("t1", newRequireSidecarStep("bogus"), TaskInfo{ID: "t1"})
	if err == nil {
		t.Fatal("expected error for unknown sidecar, got nil")
	}
}

func TestExecRequireSidecar_EmptySidecarConfigErrors(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "planning"})
	engine := newEngineForEval(t, tasks)

	_, err := engine.execRequireSidecar("t1", newRequireSidecarStep(""), TaskInfo{ID: "t1"})
	if err == nil {
		t.Fatal("expected error for empty sidecar config, got nil")
	}
}

// --- flag_plan_critique step ---

func newFlagPlanCritiqueStep() *Step {
	return &Step{ID: "flag_plan_critique_verdict", Type: StepFlagPlanCritique}
}

func TestParsePlanCritiqueVerdict(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"approve", "# Plan Review: APPROVE\n\n## Verdict\n\nLooks good.", "APPROVE"},
		{"refine", "# Plan Review: REFINE\n\n## Findings\n\n- missing file", "REFINE"},
		{"reject", "# Plan Review: REJECT\n\nToo vague.", "REJECT"},
		{"lowercase verdict word", "# plan review: refine\n", "REFINE"},
		{"leading blank line", "\n# Plan Review: REFINE\n", "REFINE"},
		{"mention in prose is not the verdict line", "# Plan Review: APPROVE\n\nNo REFINE needed here.", "APPROVE"},
		{"no marker at all", "Looks fine to me.", ""},
		{"empty", "", ""},
		{
			"bare title falls back to Verdict section prose",
			"# Plan Review\n\n## Verdict\n\nThis plan needs REFINE — the rollback step is missing and the test command doesn't match the project.",
			"REFINE",
		},
		{
			"verdict section fallback is bounded to that section",
			"# Plan Review\n\n## Verdict\n\nSound overall.\n\n## Findings\n\n- [nit] refine the error message wording",
			"",
		},
		{
			"inflected verdict word still resolves to its base form",
			"# Plan Review: REJECTED\n\n## Verdict\n\nThis plan is rejected due to missing rollback safety verification steps.",
			"REJECT",
		},
		{
			"heading-level drift on the title line still matches",
			"## Plan Review: REFINE\n\n## Verdict\n\nSeveral steps need adjustment.",
			"REFINE",
		},
		{
			"a longer unrelated word is not a boundary-crossing false match",
			"# Plan Review\n\n## Verdict\n\nNo concerns; this is not a rejectionist take, just a sanity check.",
			"",
		},
		{
			"current skill contract: verdict on the Verdict heading line",
			"# Plan Review\n\n## Verdict: REFINE\n\n**One-line summary:** Missing rollback step.\n\n## Findings\n\n- [high] no rollback",
			"REFINE",
		},
		{
			"current skill contract: APPROVE on the Verdict heading line",
			"# Plan Review\n\n## Verdict: APPROVE\n\n**One-line summary:** Looks executable as-is.",
			"APPROVE",
		},
		{
			"current skill contract: unrendered template brackets still resolve",
			"# Plan Review\n\n## Verdict: [REJECT]\n\n**One-line summary:** Too vague to execute.",
			"REJECT",
		},
		{
			"colon-line format takes priority over a conflicting title line",
			"# Plan Review: APPROVE\n\n## Verdict: REFINE\n\n**One-line summary:** Needs edits despite the stale title.",
			"REFINE",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parsePlanCritiqueVerdict(tt.content); got != tt.want {
				t.Errorf("parsePlanCritiqueVerdict(%q) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}

func TestExecFlagPlanCritique_ApproveDoesNotAppendNote(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "planning", PlanCritique: "# Plan Review: APPROVE\n\nLooks good."})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execFlagPlanCritique("t1", newFlagPlanCritiqueStep(), TaskInfo{ID: "t1", PlanCritique: "# Plan Review: APPROVE\n\nLooks good."})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if strings.Contains(ti.Body, "Plan Critic Verdict") {
		t.Errorf("APPROVE should not append a verdict note; body = %q", ti.Body)
	}
}

func TestExecFlagPlanCritique_RefineAppendsDistinguishableNote(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "planning", PlanCritique: "# Plan Review: REFINE\n\n## Findings\n\n- [high] missing file"})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execFlagPlanCritique("t1", newFlagPlanCritiqueStep(), TaskInfo{ID: "t1", PlanCritique: "# Plan Review: REFINE\n\n## Findings\n\n- [high] missing file"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "REFINE") {
		t.Errorf("Output = %q, want it to report the REFINE verdict", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if !strings.Contains(ti.Body, "Plan Critic Verdict: REFINE") {
		t.Errorf("expected a distinguishable REFINE note in body; got:\n%s", ti.Body)
	}
	if ti.Status == "human-required" {
		t.Error("flag_plan_critique must not block progression by itself")
	}
}

func TestExecFlagPlanCritique_RejectAppendsDistinguishableNote(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "planning", PlanCritique: "# Plan Review: REJECT\n\nToo vague."})
	engine := newEngineForEval(t, tasks)

	_, err := engine.execFlagPlanCritique("t1", newFlagPlanCritiqueStep(), TaskInfo{ID: "t1", PlanCritique: "# Plan Review: REJECT\n\nToo vague."})
	if err != nil {
		t.Fatal(err)
	}
	ti, _ := tasks.GetTask("t1")
	if !strings.Contains(ti.Body, "Plan Critic Verdict: REJECT") {
		t.Errorf("expected a distinguishable REJECT note in body; got:\n%s", ti.Body)
	}
}

func TestExecFlagPlanCritique_UnparseableVerdictDoesNotAppendNote(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "planning", PlanCritique: "Some free-form text with no verdict marker."})
	engine := newEngineForEval(t, tasks)

	_, err := engine.execFlagPlanCritique("t1", newFlagPlanCritiqueStep(), TaskInfo{ID: "t1", PlanCritique: "Some free-form text with no verdict marker."})
	if err != nil {
		t.Fatal(err)
	}
	ti, _ := tasks.GetTask("t1")
	if strings.Contains(ti.Body, "Plan Critic Verdict") {
		t.Errorf("an unparseable verdict should behave like APPROVE (no note); body = %q", ti.Body)
	}
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

func TestAdvanceStep_ImplementHumanRequiredGitHubAuthParksForRetry(t *testing.T) {
	store := newStoreWithBuiltin(t, "simple-task-implement")
	tasks := newMemTasks()
	wf := &Execution{
		WorkflowID:  "simple-task-implement",
		CurrentStep: "implement",
		State:       ExecWaiting,
		Variables:   map[string]string{},
	}
	reason := "push failed: X Failed to log in to github.com using token (GH_TOKEN)\n- The token in GH_TOKEN is invalid."
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "human-required",
		StatusReason: reason,
		AgentMode:    "headless",
		Workflow:     wf,
	})
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())

	err := engine.AdvanceStep("t1", StepOutput{
		StepID:  "implement",
		Status:  "completed",
		Output:  reason,
		AgentID: "a1",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "in-progress" {
		t.Fatalf("status = %q, want in-progress", got.Status)
	}
	if got.StatusReason != implementPushRetryStatusReason {
		t.Fatalf("status_reason = %q, want %q", got.StatusReason, implementPushRetryStatusReason)
	}
	if got.Workflow == nil {
		t.Fatal("workflow missing")
	}
	if got.Workflow.CurrentStep != "implement" {
		t.Errorf("CurrentStep = %q, want implement", got.Workflow.CurrentStep)
	}
	if got.Workflow.State != ExecWaiting {
		t.Errorf("State = %q, want ExecWaiting", got.Workflow.State)
	}
	if got.Workflow.Variables[implementPushAttemptsVar] != "1" {
		t.Errorf("%s = %q, want 1", implementPushAttemptsVar, got.Workflow.Variables[implementPushAttemptsVar])
	}
	if _, ok := workflowRetryAfter(got.Workflow); !ok {
		t.Errorf("%s not set to a valid retry timestamp", workflowRetryAfterVar)
	}
}

func TestAdvanceStep_ImplementGitHubAuthRetryCapFallsThroughToHumanRequired(t *testing.T) {
	store := newStoreWithBuiltin(t, "simple-task-implement")
	tasks := newMemTasks()
	wf := &Execution{
		WorkflowID:  "simple-task-implement",
		CurrentStep: "implement",
		State:       ExecWaiting,
		Variables: map[string]string{
			implementPushAttemptsVar: strconv.Itoa(maxImplementPushRetries),
		},
	}
	reason := "push failed: gh auth status: token is invalid"
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "human-required",
		StatusReason: reason,
		AgentMode:    "headless",
		Workflow:     wf,
	})
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())

	err := engine.AdvanceStep("t1", StepOutput{
		StepID:  "implement",
		Status:  "completed",
		Output:  reason,
		AgentID: "a1",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if got.StatusReason != reason {
		t.Fatalf("status_reason = %q, want original reason %q", got.StatusReason, reason)
	}
	if got.Workflow == nil || got.Workflow.State != ExecCompleted || got.Workflow.CurrentStep != "" {
		t.Fatalf("workflow = %+v, want completed terminal workflow", got.Workflow)
	}
}

func TestAdvanceStep_ImplementNonGitHubAuthHumanRequiredFallsThrough(t *testing.T) {
	store := newStoreWithBuiltin(t, "simple-task-implement")
	tasks := newMemTasks()
	wf := &Execution{
		WorkflowID:  "simple-task-implement",
		CurrentStep: "implement",
		State:       ExecWaiting,
		Variables:   map[string]string{},
	}
	reason := "application auth provider rejected invalid token fixture"
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "human-required",
		StatusReason: reason,
		AgentMode:    "headless",
		Workflow:     wf,
	})
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())

	err := engine.AdvanceStep("t1", StepOutput{
		StepID:  "implement",
		Status:  "completed",
		Output:  reason,
		AgentID: "a1",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if got.Workflow == nil || got.Workflow.State != ExecCompleted || got.Workflow.CurrentStep != "" {
		t.Fatalf("workflow = %+v, want completed terminal workflow", got.Workflow)
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
	return NewEngine(store, tasks, agents, discardLogger())
}

func TestExecEvaluate_LastAgentFailedFlipsHumanRequired(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "failed", AgentID: "a1", Output: "rate limit exceeded"},
		},
	}

	out, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("step Status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
	if got := tasks.Reason("t1"); got != "rate limit exceeded" {
		t.Errorf("reason = %q, want %q", got, "rate limit exceeded")
	}
}

func TestExecEvaluate_LastAgentFailedKeepsFullReason(t *testing.T) {
	long := strings.Repeat("x", 500)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "failed", AgentID: "a1", Output: long},
		},
	}

	if _, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{}); err != nil {
		t.Fatal(err)
	}
	got := tasks.Reason("t1")
	if strings.Contains(got, "(truncated)") {
		t.Errorf("reason should not be truncated: %q", got)
	}
	if got != long {
		t.Errorf("reason = %d chars, want the full %d-char output preserved", len(got), len(long))
	}
}

func TestExecEvaluate_LastAgentSucceededFlipsHumanRequiredWithDefault(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "completed", AgentID: "a1", Output: "Implementation done."},
		},
	}

	if _, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{}); err != nil {
		t.Fatal(err)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
	if got := tasks.Reason("t1"); got != "commits pushed but no PR created" {
		t.Errorf("reason = %q, want %q", got, "commits pushed but no PR created")
	}
}

func TestExecEvaluate_PRCreateRateLimitParksForRetry(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		WorkflowID:  "simple-task-pr",
		CurrentStep: "evaluate",
		State:       ExecRunning,
		Variables:   map[string]string{},
		StepHistory: []StepRecord{
			{
				StepID:  "create_pr",
				Status:  "completed",
				AgentID: "a1",
				Output:  "GitHub GraphQL rate limit exhausted; I will wait for reset.",
			},
		},
	}

	_, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{ID: "t1", Status: "ready-pr"})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	if wfExec.CurrentStep != "create_pr" {
		t.Errorf("CurrentStep = %q, want create_pr", wfExec.CurrentStep)
	}
	if wfExec.State != ExecWaiting {
		t.Errorf("State = %q, want ExecWaiting", wfExec.State)
	}
	if _, ok := workflowRetryAfter(wfExec); !ok {
		t.Errorf("%s not set to a valid retry timestamp", workflowRetryAfterVar)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "ready-pr" {
		t.Errorf("task status = %q, want ready-pr", ti.Status)
	}
	if got := tasks.Reason("t1"); got != prCreateRetryStatusReason {
		t.Errorf("reason = %q, want %q", got, prCreateRetryStatusReason)
	}
}

func TestExecEvaluate_PRCreateTransientOutageParksForRetry(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		WorkflowID:  "simple-task-pr",
		CurrentStep: "evaluate",
		State:       ExecRunning,
		Variables:   map[string]string{},
		StepHistory: []StepRecord{
			{
				StepID:  "create_pr",
				Status:  "completed",
				AgentID: "a1",
				Output:  "Network/auth is broken: connection refused to api.github.com. Please check connectivity.",
			},
		},
	}

	_, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{ID: "t1", Status: "ready-pr"})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	if wfExec.CurrentStep != "create_pr" {
		t.Errorf("CurrentStep = %q, want create_pr", wfExec.CurrentStep)
	}
	if wfExec.State != ExecWaiting {
		t.Errorf("State = %q, want ExecWaiting", wfExec.State)
	}
	if _, ok := workflowRetryAfter(wfExec); !ok {
		t.Errorf("%s not set to a valid retry timestamp", workflowRetryAfterVar)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "ready-pr" {
		t.Errorf("task status = %q, want ready-pr", ti.Status)
	}
	if got := tasks.Reason("t1"); got != prCreateTransientStatusReason {
		t.Errorf("reason = %q, want %q", got, prCreateTransientStatusReason)
	}
}

func TestExecEvaluate_PRCreateAuthFailureRetriesThenEscalates(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr"})
	engine := newEngineForEval(t, tasks)
	newWfExec := func(attempts string) *Execution {
		vars := map[string]string{}
		if attempts != "" {
			vars[prCreateAuthAttemptsVar] = attempts
		}
		return &Execution{
			WorkflowID:  "simple-task-pr",
			CurrentStep: "evaluate",
			State:       ExecRunning,
			Variables:   vars,
			StepHistory: []StepRecord{
				{
					StepID:  "create_pr",
					Status:  "completed",
					AgentID: "a1",
					Output:  "gh: Bad credentials (HTTP 401)",
				},
			},
		}
	}

	// First three attempts (0, 1, 2) park for retry and increment the counter.
	for i := range maxPRCreateAuthRetries {
		wfExec := newWfExec(strconv.Itoa(i))
		_, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{ID: "t1", Status: "ready-pr"})
		if !errors.Is(err, errStepParked) {
			t.Fatalf("attempt %d: err = %v, want errStepParked", i, err)
		}
		if wfExec.Variables[prCreateAuthAttemptsVar] != strconv.Itoa(i+1) {
			t.Errorf("attempt %d: %s = %q, want %q", i, prCreateAuthAttemptsVar, wfExec.Variables[prCreateAuthAttemptsVar], strconv.Itoa(i+1))
		}
		if got := tasks.Reason("t1"); got != prCreateAuthRetryReason {
			t.Errorf("attempt %d: reason = %q, want %q", i, got, prCreateAuthRetryReason)
		}
	}

	// After exhausting the budget, it escalates to human-required instead of
	// retrying a broken credential forever.
	wfExec := newWfExec(strconv.Itoa(maxPRCreateAuthRetries))
	if _, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{ID: "t1", Status: "ready-pr"}); err != nil {
		t.Fatal(err)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
	wantReason := fmt.Sprintf("PR creation failing due to invalid or expired GitHub credentials after %d retries", maxPRCreateAuthRetries)
	if got := tasks.Reason("t1"); got != wantReason {
		t.Errorf("reason = %q, want %q", got, wantReason)
	}
}

func TestExecEvaluate_PRCreatePushedNoPRRetriesThenEscalates(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr"})
	engine := newEngineForEval(t, tasks)
	newWfExec := func(attempts string) *Execution {
		vars := map[string]string{}
		if attempts != "" {
			vars[prCreateAttemptsVar] = attempts
		}
		return &Execution{
			WorkflowID:  "simple-task-pr",
			CurrentStep: "evaluate",
			State:       ExecRunning,
			Variables:   vars,
			StepHistory: []StepRecord{
				{
					StepID:  "create_pr",
					Status:  "completed",
					AgentID: "a1",
					Output:  "I was unable to create the PR due to an unexpected issue.",
				},
			},
		}
	}

	// First three attempts (0, 1, 2) park for retry and increment the counter.
	for i := range maxPRCreatePushedNoPRRetries {
		wfExec := newWfExec(strconv.Itoa(i))
		_, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{ID: "t1", Status: "ready-pr"})
		if !errors.Is(err, errStepParked) {
			t.Fatalf("attempt %d: err = %v, want errStepParked", i, err)
		}
		if wfExec.Variables[prCreateAttemptsVar] != strconv.Itoa(i+1) {
			t.Errorf("attempt %d: %s = %q, want %q", i, prCreateAttemptsVar, wfExec.Variables[prCreateAttemptsVar], strconv.Itoa(i+1))
		}
		if got := tasks.Reason("t1"); got != prCreatePushedNoPRReason {
			t.Errorf("attempt %d: reason = %q, want %q", i, got, prCreatePushedNoPRReason)
		}
	}

	// After exhausting the budget, it escalates to human-required.
	wfExec := newWfExec(strconv.Itoa(maxPRCreatePushedNoPRRetries))
	if _, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{ID: "t1", Status: "ready-pr"}); err != nil {
		t.Fatal(err)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
	wantReason := fmt.Sprintf("commits pushed but no PR created after %d retries", maxPRCreatePushedNoPRRetries)
	if got := tasks.Reason("t1"); got != wantReason {
		t.Errorf("reason = %q, want %q", got, wantReason)
	}
}

func TestExecEvaluate_SkipsMechanicalStepsInHistory(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "failed", AgentID: "a1", Output: "real error"},
			{StepID: "verify_commits", Status: "completed"},
			{StepID: "link_pr_and_review", Status: "completed", Output: "no pr found"},
		},
	}

	if _, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{}); err != nil {
		t.Fatal(err)
	}
	if got := tasks.Reason("t1"); got != "real error" {
		t.Errorf("reason = %q, want %q (mechanical steps must be skipped)", got, "real error")
	}
}

func TestExecEvaluate_EmptyHistory(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{}

	if _, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{}); err != nil {
		t.Fatal(err)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
	if got := tasks.Reason("t1"); got != "no agent result to evaluate" {
		t.Errorf("reason = %q, want %q", got, "no agent result to evaluate")
	}
}

func TestExecEvaluate_FailedWithEmptyOutput(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "failed", AgentID: "a1", Output: "   "},
		},
	}

	if _, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{}); err != nil {
		t.Fatal(err)
	}
	if got := tasks.Reason("t1"); got != "agent failed with no output" {
		t.Errorf("reason = %q, want %q", got, "agent failed with no output")
	}
}

func TestExecEvaluate_NoPRFallsThrough(t *testing.T) {
	// When ProjectID+Branch are set but gh pr list finds nothing, the step must
	// still fall through to human-required (not panic or error).
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", ProjectID: "owner/repo", Branch: "feature-branch"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "failed", AgentID: "a1", Output: "timed out"},
		},
	}

	ti := TaskInfo{ID: "t1", ProjectID: "owner/repo", Branch: "feature-branch"}
	if _, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, ti); err != nil {
		t.Fatal(err)
	}
	got, _ := tasks.GetTask("t1")
	if got.Status != "human-required" {
		t.Errorf("status = %q, want human-required", got.Status)
	}
}

func newLinkPRStep() *Step {
	return &Step{ID: "link_pr_and_review", Type: StepLinkPRAndReview}
}

func TestExecLinkPRAndReview_PRAlreadyLinked(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", PRNumber: 42})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execLinkPRAndReview("t1", newLinkPRStep(), &Execution{}, TaskInfo{ID: "t1", PRNumber: 42})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-review" {
		t.Errorf("task status = %q, want in-review", ti.Status)
	}
	if ti.PRNumber != 42 {
		t.Errorf("pr_number = %d, want 42", ti.PRNumber)
	}
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

func TestExecLinkPRAndReview_PRNumberNotInRepoFallsThrough(t *testing.T) {
	// A pr_number that doesn't resolve against the project's own repo (e.g.
	// an agent that ran a bare `gh pr create` inside a fork worktree and got
	// a PR opened in the fork itself) must not be trusted blindly — it
	// should fall through to the other discovery paths instead of flipping
	// straight to in-review against a PR nobody upstream will ever see.
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", PRNumber: 8, ProjectID: "kumahq/kuma"})
	engine := newEngineForEval(t, tasks)
	engine.SetPRExistenceChecker(fakePRExistenceChecker{exists: false})
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "completed", AgentID: "a1", Output: "changes pushed"},
		},
	}

	out, err := engine.execLinkPRAndReview("t1", newLinkPRStep(), wfExec, TaskInfo{ID: "t1", PRNumber: 8, ProjectID: "kumahq/kuma"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Errorf("task status = %q, want in-progress (must not trust the wrong-repo pr_number)", ti.Status)
	}
}

func TestExecLinkPRAndReview_PRNumberUnverifiedFallsThrough(t *testing.T) {
	// A checker that fails to confirm (gh unavailable/unauthenticated,
	// network) must be treated the same as "not confirmed" — never as proof
	// the PR is absent, but also never trusted outright.
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", PRNumber: 8, ProjectID: "kumahq/kuma"})
	engine := newEngineForEval(t, tasks)
	engine.SetPRExistenceChecker(fakePRExistenceChecker{err: errors.New("gh: authentication failed")})

	out, err := engine.execLinkPRAndReview("t1", newLinkPRStep(), &Execution{}, TaskInfo{ID: "t1", PRNumber: 8, ProjectID: "kumahq/kuma"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Errorf("task status = %q, want in-progress (must not trust an unverifiable pr_number)", ti.Status)
	}
}

func TestExecLinkPRAndReview_PRNumberVerifiedInRepoTrusted(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", PRNumber: 8, ProjectID: "kumahq/kuma"})
	engine := newEngineForEval(t, tasks)
	engine.SetPRExistenceChecker(fakePRExistenceChecker{exists: true})

	out, err := engine.execLinkPRAndReview("t1", newLinkPRStep(), &Execution{}, TaskInfo{ID: "t1", PRNumber: 8, ProjectID: "kumahq/kuma"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-review" {
		t.Errorf("task status = %q, want in-review", ti.Status)
	}
	if ti.PRNumber != 8 {
		t.Errorf("pr_number = %d, want 8", ti.PRNumber)
	}
}

func TestExecLinkPRAndReview_NoCheckerTrustsPRNumber(t *testing.T) {
	// Guards the documented "operates with a nil checker" fallback contract.
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", PRNumber: 8, ProjectID: "kumahq/kuma"})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execLinkPRAndReview("t1", newLinkPRStep(), &Execution{}, TaskInfo{ID: "t1", PRNumber: 8, ProjectID: "kumahq/kuma"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-review" {
		t.Errorf("task status = %q, want in-review", ti.Status)
	}
}

func TestExecLinkPRAndReview_FullURLInAgentOutput(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{
				StepID: "implement", Status: "completed", AgentID: "a1",
				Output: "PR created: https://github.com/owner/repo/pull/123",
			},
		},
	}

	out, err := engine.execLinkPRAndReview("t1", newLinkPRStep(), wfExec, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-review" {
		t.Errorf("task status = %q, want in-review", ti.Status)
	}
	if ti.PRNumber != 123 {
		t.Errorf("pr_number = %d, want 123", ti.PRNumber)
	}
}

func TestExecLinkPRAndReview_ShortRefInAgentOutput(t *testing.T) {
	// Agents sometimes output "owner/repo#N" instead of a full GitHub URL.
	// The step must parse this shorthand and link the PR.
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{
				StepID: "implement", Status: "completed", AgentID: "a1",
				Output: "PR created: Automaat/sybra#444\n\nChanges applied.",
			},
		},
	}

	out, err := engine.execLinkPRAndReview("t1", newLinkPRStep(), wfExec, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-review" {
		t.Errorf("task status = %q, want in-review", ti.Status)
	}
	if ti.PRNumber != 444 {
		t.Errorf("pr_number = %d, want 444", ti.PRNumber)
	}
}

func TestExecLinkPRAndReview_NoPRFallsThrough(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "completed", AgentID: "a1", Output: "changes pushed"},
		},
	}

	out, err := engine.execLinkPRAndReview("t1", newLinkPRStep(), wfExec, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Errorf("task status = %q, want in-progress (must not change)", ti.Status)
	}
}

func TestAdvanceStep_MarkReviewedAfterReviewRole(t *testing.T) {
	// After a run_agent step with role=review completes successfully,
	// the task must be marked reviewed so re-triggered workflows skip code_review.
	store := newTestStoreWith(t, "test-review-fix.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-review-fix"); err != nil {
		t.Fatal(err)
	}

	// implement → maybe_review → code_review
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "implement", Status: "completed", AgentID: "a1"}); err != nil {
		t.Fatal(err)
	}
	// Workflow should now be waiting at code_review.
	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "code_review" {
		t.Fatalf("expected code_review step, got %q", ti.Workflow.CurrentStep)
	}

	// Complete code_review (role=review) → must mark reviewed.
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "code_review", Status: "completed", AgentID: "a2", Output: "review done"}); err != nil {
		t.Fatal(err)
	}

	ti, _ = tasks.GetTask("t1")
	if !ti.Reviewed {
		t.Error("task.Reviewed = false after review-role step completed; want true")
	}
}

// TestAdvanceStep_WorkflowDefinitionDeletedMidRun covers the case where a
// workflow YAML file is removed from disk while an execution is in flight.
// loadAdvanceContext re-reads the definition from the store for every
// AdvanceStep call, so a deleted file must surface a clear error instead of
// panicking or silently reusing stale state. The task's workflow reference
// stays put — the caller decides whether to reset it.
func TestAdvanceStep_WorkflowDefinitionDeletedMidRun(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// The definition file disappears (user-edit, git clean, rm -rf).
	if err := store.Delete("test-simple"); err != nil {
		t.Fatalf("Delete definition: %v", err)
	}

	agents.SimulateComplete("t1")
	err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed", Output: "triaged"})
	if err == nil {
		t.Fatal("AdvanceStep after definition delete returned nil; expected error")
	}
	if !strings.Contains(err.Error(), "test-simple") && !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "no such file") {
		t.Errorf("error should reference the missing workflow; got %q", err)
	}

	// Task workflow reference must remain intact so the caller can inspect /
	// recover from the error rather than silently losing state.
	ti, _ := tasks.GetTask("t1")
	if ti.Workflow == nil {
		t.Error("task.Workflow was cleared on definition-delete error; callers need it for recovery")
	}
	if ti.Workflow.WorkflowID != "test-simple" {
		t.Errorf("task.Workflow.WorkflowID = %q, want %q", ti.Workflow.WorkflowID, "test-simple")
	}
}

// TestStartWorkflow_ConcurrentSameTaskSingleWinner verifies the per-task
// `starting` mutex serializes concurrent StartWorkflowWithVars calls for
// the same task. Exactly one caller wins; the others get
// ErrWorkflowAlreadyActive. Without the lock, both callers would spawn
// duplicate agents for the same task (the original bug this test pins).
func TestStartWorkflow_ConcurrentSameTaskSingleWinner(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})

	const callers = 5
	var wg sync.WaitGroup
	errs := make([]error, callers)
	start := make(chan struct{})
	for i := range callers {
		wg.Go(func() {
			<-start
			errs[i] = engine.StartWorkflow("t1", "test-simple")
		})
	}
	close(start)
	wg.Wait()

	successCount := 0
	rejectedCount := 0
	for _, err := range errs {
		switch {
		case err == nil:
			successCount++
		case errors.Is(err, ErrWorkflowAlreadyActive):
			rejectedCount++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if successCount != 1 {
		t.Errorf("got %d successful starts, want exactly 1", successCount)
	}
	if rejectedCount != callers-1 {
		t.Errorf("got %d rejections, want %d (all losers must be rejected with ErrWorkflowAlreadyActive)", rejectedCount, callers-1)
	}

	// Exactly one agent was spawned — the bug this test guards against is
	// two concurrent callers both reaching executeSteps.
	if got := agents.CallCount(); got != 1 {
		t.Errorf("agent spawn count = %d, want 1 (lock should prevent duplicate spawns)", got)
	}

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.Workflow == nil || ti.Workflow.WorkflowID != "test-simple" {
		t.Errorf("task workflow not set correctly: %+v", ti.Workflow)
	}
}
