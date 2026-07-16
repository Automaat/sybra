package sybra

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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
			name:          "default full runs both",
			orch:          config.DefaultConfig().Orchestrator,
			wantScheduler: true,
			wantBrain:     true,
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
			name:          "invalid role falls back to full",
			orch:          config.OrchestratorConfig{Role: "bogus"},
			wantScheduler: true,
			wantBrain:     true,
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
	if !a.runsOrchestratorBrain() {
		t.Error("runsOrchestratorBrain() = false with nil cfg, want true")
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

			blocker := createResearchTaskWithPriority(t, a.tasks, "blocker", task.PriorityMedium)
			blockerAgent, err := a.agentOrch.StartAgent(blocker.ID, "headless", "hold", false, false)
			if err != nil {
				t.Fatalf("StartAgent(blocker): %v", err)
			}
			queued := createResearchTaskWithPriority(t, a.tasks, "queued", task.PriorityMedium)
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
