package sybra

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

func TestAppInstanceRoleGates(t *testing.T) {
	ptr := func(b bool) *bool { return &b }

	tests := []struct {
		name          string
		orch          config.OrchestratorConfig
		wantScheduler bool
		wantBrain     bool
	}{
		{
			name:          "default full runs the scheduler, brain stays off",
			orch:          config.DefaultConfig().Orchestrator,
			wantScheduler: true,
			wantBrain:     false,
		},
		{
			name:          "agent-only runs neither",
			orch:          config.OrchestratorConfig{Role: config.InstanceRoleAgentOnly},
			wantScheduler: false,
			wantBrain:     false,
		},
		{
			name:          "agent-only with explicit scheduler",
			orch:          config.OrchestratorConfig{Role: config.InstanceRoleAgentOnly, SchedulerEnabled: ptr(true)},
			wantScheduler: true,
			wantBrain:     false,
		},
		{
			name:          "agent-only with explicit brain opt-in",
			orch:          config.OrchestratorConfig{Role: config.InstanceRoleAgentOnly, Enabled: ptr(true)},
			wantScheduler: false,
			wantBrain:     true,
		},
		{
			name:          "invalid role falls back to full scheduler, brain stays off",
			orch:          config.OrchestratorConfig{Role: "bogus"},
			wantScheduler: true,
			wantBrain:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &App{cfg: &config.Config{Orchestrator: tt.orch}}
			a.applyInstanceRole()
			if got := a.runsScheduler(); got != tt.wantScheduler {
				t.Errorf("runsScheduler() = %v, want %v", got, tt.wantScheduler)
			}
			if got := a.runsOrchestratorBrain(); got != tt.wantBrain {
				t.Errorf("runsOrchestratorBrain() = %v, want %v", got, tt.wantBrain)
			}
		})
	}
}

func TestAppInstanceRoleGatesNilConfig(t *testing.T) {
	a := &App{}
	a.applyInstanceRole()
	if !a.runsScheduler() {
		t.Error("runsScheduler() = false with nil cfg, want true")
	}
	if a.runsOrchestratorBrain() {
		t.Error("runsOrchestratorBrain() = true with nil cfg, want false")
	}
}

func TestQueueDrainPassDrainsManualStartsForEveryRole(t *testing.T) {
	tests := []struct {
		name      string
		role      string
		wantDepth int
	}{
		{name: "full drains", role: config.InstanceRoleFull, wantDepth: 0},
		{name: "agent-only still drains explicit starts", role: config.InstanceRoleAgentOnly, wantDepth: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := setupManualQueueApp(t, "", "", 1)
			a.cfg.Orchestrator.Role = tt.role
			a.applyInstanceRole()

			blocker := createTaskWithPriority(t, a.tasks, "blocker", task.PriorityMedium)
			blockerAgent, err := a.agentOrch.StartAgent(blocker.ID, "headless", "hold", false, false)
			if err != nil {
				t.Fatalf("StartAgent(blocker): %v", err)
			}
			queued := createTaskWithPriority(t, a.tasks, "queued", task.PriorityMedium)
			if _, err := a.StartAgent(queued.ID, "headless", "queued", false); err != nil {
				t.Fatalf("StartAgent(queued): %v", err)
			}
			if got := len(a.agentQueue.Snapshot()); got != 1 {
				t.Fatalf("queue depth before drain = %d, want 1", got)
			}

			if err := a.agents.StopAgent(blockerAgent.ID); err != nil {
				t.Fatalf("StopAgent(blocker): %v", err)
			}
			waitForFreeSlot(t, a)

			a.queueDrainPass(t.Context())

			if got := len(a.agentQueue.Snapshot()); got != tt.wantDepth {
				t.Fatalf("queue depth after drain with a free slot = %d, want %d", got, tt.wantDepth)
			}
		})
	}
}

func waitForFreeSlot(t *testing.T, a *App) {
	t.Helper()
	for range 200 {
		if a.agents.AvailableQueueDrainSlots(1) > 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("timed out waiting for a free agent pool slot")
}

func TestAgentOnlyStatusHookDoesNotDispatchWorkflow(t *testing.T) {
	tests := []struct {
		name         string
		role         string
		wantWorkflow bool
	}{
		{name: "full dispatches on status change", role: config.InstanceRoleFull, wantWorkflow: true},
		{name: "agent-only fails closed", role: config.InstanceRoleAgentOnly, wantWorkflow: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := setupApp(t)
			a.cfg = config.DefaultConfig()
			a.cfg.Orchestrator.Role = tt.role

			wfDir := t.TempDir()
			wfStore, err := workflow.NewStore(wfDir)
			if err != nil {
				t.Fatal(err)
			}
			const testReviewWF = `id: simple-task-review
name: Test Review
trigger:
  on: task.status_changed
  conditions:
    - field: task.status
      operator: equals
      value: ready-review
steps:
  - id: mark_testing
    name: Hand to Testing
    type: set_status
    config:
      status: testing
    next:
      - goto: ""
`
			if err := os.WriteFile(filepath.Join(wfDir, "simple-task-review.yaml"), []byte(testReviewWF), 0o644); err != nil {
				t.Fatal(err)
			}
			ta := &taskAdapter{tasks: a.tasks}
			aa := &agentAdapter{agents: a.agents, agentOrch: a.agentOrch, tasks: a.tasks}
			a.workflowEngine = workflow.NewEngine(wfStore, ta, aa, a.logger)
			a.applyInstanceRole()
			a.initStatusHook()

			created, err := a.tasks.Create("ready-review dispatch", "", "headless")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.tasks.UpdateMap(created.ID, map[string]any{
				"status": string(task.StatusReadyReview),
			}); err != nil {
				t.Fatal(err)
			}

			tk, err := a.tasks.Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := tk.Workflow != nil; got != tt.wantWorkflow {
				t.Fatalf("workflow attached = %v, want %v (status-change dispatch on role %q)",
					got, tt.wantWorkflow, tt.role)
			}
			if !tt.wantWorkflow && tk.Status != task.StatusReadyReview {
				t.Errorf("task status = %q, want ready-review untouched on an agent-only instance", tk.Status)
			}
		})
	}
}

func TestDispatchTaskCreatedWorkflowSkipsApprovedTodoPlan(t *testing.T) {
	// Reproduces #997b365c's stall→reset loop: a todo task that already
	// carries a valid, approved plan contract must not be swept back into
	// task.created/triage (which would overwrite it to "planning" and mint a
	// brand-new plan agent, discarding the approved contract). A todo task
	// with no plan contract — the ordinary brand-new path — must still
	// dispatch normally.
	const createdWF = `id: probe-created
name: Probe Created
trigger:
  on: task.created
steps:
  - id: mark_planning
    name: Mark Planning
    type: set_status
    config:
      status: planning
    next:
      - goto: ""
`
	cases := []struct {
		name            string
		hasPlanContract bool
		wantDispatch    bool
	}{
		{name: "todo with approved plan contract stays parked", hasPlanContract: true, wantDispatch: false},
		{name: "brand new todo with no plan contract still dispatches", hasPlanContract: false, wantDispatch: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := setupApp(t)
			a.cfg = config.DefaultConfig()

			wfDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(wfDir, "probe-created.yaml"), []byte(createdWF), 0o644); err != nil {
				t.Fatal(err)
			}
			wfStore, err := workflow.NewStore(wfDir)
			if err != nil {
				t.Fatal(err)
			}
			ta := &taskAdapter{tasks: a.tasks}
			aa := &agentAdapter{agents: a.agents, agentOrch: a.agentOrch, tasks: a.tasks}
			a.workflowEngine = workflow.NewEngine(wfStore, ta, aa, a.logger)
			a.applyInstanceRole()

			created, err := a.tasks.Create("todo plan-contract probe", "", "headless")
			if err != nil {
				t.Fatal(err)
			}
			planContract := ""
			if tc.hasPlanContract {
				planContract = validTestPlanContract(created.ID)
			}
			if _, err := a.tasks.Update(created.ID, task.Update{
				Status:       task.Ptr(task.StatusTodo),
				PlanContract: task.Ptr(planContract),
			}); err != nil {
				t.Fatal(err)
			}

			a.dispatchTaskCreatedWorkflow(created.ID)
			a.wg.Wait()

			tk, err := a.tasks.Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := tk.Workflow != nil; got != tc.wantDispatch {
				t.Fatalf("workflow attached = %v, want %v", got, tc.wantDispatch)
			}
			if tc.wantDispatch && tk.Status != task.StatusPlanning {
				t.Errorf("status = %q, want planning (probe workflow ran)", tk.Status)
			}
			if !tc.wantDispatch && tk.Status != task.StatusTodo {
				t.Errorf("status = %q, want todo (dispatch skipped, task left parked)", tk.Status)
			}
		})
	}
}

func TestTaskCreatedDispatchRespectsInstanceRole(t *testing.T) {
	const createdWF = `id: probe-created
name: Probe Created
trigger:
  on: task.created
steps:
  - id: mark_planning
    name: Mark Planning
    type: set_status
    config:
      status: planning
    next:
      - goto: ""
`
	sinks := []struct {
		name string
		call func(a *App, taskID string)
	}{
		{name: "direct", call: func(a *App, id string) { a.dispatchTaskCreatedWorkflow(id) }},
		{name: "external-task-file", call: func(a *App, id string) {
			a.maybeStartWorkflowForExternalTask(filepath.Join(a.tasksDir, id+".md"))
		}},
	}
	roles := []struct {
		role         string
		wantWorkflow bool
	}{
		{role: config.InstanceRoleFull, wantWorkflow: true},
		{role: config.InstanceRoleAgentOnly, wantWorkflow: false},
	}
	for _, sink := range sinks {
		for _, r := range roles {
			t.Run(sink.name+"/"+r.role, func(t *testing.T) {
				a := setupApp(t)
				a.cfg = config.DefaultConfig()
				a.cfg.Orchestrator.Role = r.role

				wfDir := t.TempDir()
				if err := os.WriteFile(filepath.Join(wfDir, "probe-created.yaml"), []byte(createdWF), 0o644); err != nil {
					t.Fatal(err)
				}
				wfStore, err := workflow.NewStore(wfDir)
				if err != nil {
					t.Fatal(err)
				}
				ta := &taskAdapter{tasks: a.tasks}
				aa := &agentAdapter{agents: a.agents, agentOrch: a.agentOrch, tasks: a.tasks}
				a.workflowEngine = workflow.NewEngine(wfStore, ta, aa, a.logger)
				a.applyInstanceRole()

				created, err := a.tasks.Create("dispatch sink probe", "", "headless")
				if err != nil {
					t.Fatal(err)
				}

				sink.call(a, created.ID)
				a.wg.Wait()

				tk, err := a.tasks.Get(created.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got := tk.Workflow != nil; got != r.wantWorkflow {
					t.Fatalf("%s on role %q: workflow attached = %v, want %v (workflow=%+v)",
						sink.name, r.role, got, r.wantWorkflow, tk.Workflow)
				}
			})
		}
	}
}

// TestNewRecovery_OrphanRootsIncludesSybraTestTmpGlob guards sybra#2210: the
// /sybra-test skill's fake-provider harness spawns its subject process
// directly under a fresh os.TempDir()/sybra-test-* sandbox, outside the
// normal task/worktree/sandbox lifecycle, so the boot-time orphan sweep must
// be told about that root explicitly.
func TestNewRecovery_OrphanRootsIncludesSybraTestTmpGlob(t *testing.T) {
	a := setupApp(t)
	rec := a.newRecovery()

	want := filepath.Join(os.TempDir(), "sybra-test-*")
	if !slices.Contains(rec.OrphanRoots, want) {
		t.Fatalf("OrphanRoots = %v, want to contain %q", rec.OrphanRoots, want)
	}
}

func TestRecoveryDispatchGateRespectsInstanceRole(t *testing.T) {
	tests := []struct {
		name     string
		role     string
		wantGate bool
	}{
		{name: "full restarts stale tasks", role: config.InstanceRoleFull, wantGate: true},
		{name: "agent-only does not", role: config.InstanceRoleAgentOnly, wantGate: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := setupApp(t)
			a.cfg = config.DefaultConfig()
			a.cfg.Orchestrator.Role = tt.role
			a.applyInstanceRole()

			rec := a.newRecovery()
			if rec.DispatchGate == nil {
				t.Fatal("newRecovery left DispatchGate nil; the startup stale-restart sweep would be ungated")
			}

			created, err := a.tasks.Create("stale probe", "", "headless")
			if err != nil {
				t.Fatal(err)
			}
			if got := rec.DispatchGate(created); got != tt.wantGate {
				t.Fatalf("recovery DispatchGate = %v on role %q, want %v", got, tt.role, tt.wantGate)
			}
		})
	}
}

// TestFullTaskLifecycle_SchedulerDispatchesBrainStaysDisabled exercises a
// default-config instance (Role "full", Enabled unset) across a real
// dispatch-and-run cycle: the deterministic scheduler starts and completes an
// agent for an active task while the orchestrator brain never auto-starts.
// orchSvc here carries a nil agent.Manager (see setupManualQueueApp), so a
// regression that let maybeStartOrchestrator call StartOrchestratorContext
// would panic on a nil-pointer dispatch, not just fail an assertion.
func TestFullTaskLifecycle_SchedulerDispatchesBrainStaysDisabled(t *testing.T) {
	a := setupManualQueueApp(t, "", "", 1)
	a.applyInstanceRole()
	if !a.runsScheduler() {
		t.Fatal("expected the default full role to run the scheduler")
	}
	if a.runsOrchestratorBrain() {
		t.Fatal("expected the orchestrator brain disabled by default (Enabled unset)")
	}

	created := createTaskWithPriority(t, a.tasks, "full lifecycle probe", task.PriorityMedium)
	// An active status so maybeStartOrchestrator's own active-task scan would
	// trip a would-be auto-start if the brain gate regressed.
	if _, err := a.tasks.Update(created.ID, task.Update{Status: task.Ptr(task.StatusInProgress)}); err != nil {
		t.Fatal(err)
	}

	ag, err := a.agentOrch.StartAgent(created.ID, "headless", "advance the task", false, false)
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	for range 5 {
		a.dispatchPass(t.Context())
		if a.orchSvc.IsOrchestratorRunning() {
			t.Fatal("orchestrator brain auto-started despite an active task and brain disabled by default")
		}
		time.Sleep(50 * time.Millisecond)
	}

	deadline := time.Now().Add(10 * time.Second)
	for ag.GetState() != agent.StateStopped && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if ag.GetState() != agent.StateStopped {
		t.Fatalf("agent did not reach StateStopped: state=%v", ag.GetState())
	}
	if a.orchSvc.IsOrchestratorRunning() {
		t.Fatal("orchestrator brain running after a scheduler-driven lifecycle completed")
	}
}
