package workflow

import "testing"

// The stall lane never reaches HandleAgentComplete, so the skill-receipt
// ceilings cannot see these runs. Without a budget of its own a wedged CLI
// re-dispatches forever, burning a write timeout per tick and never escalating.
func promptUndeliveredStore(t *testing.T) *Store {
	t.Helper()
	return newInlineTestStore(t, "prompt-undelivered", `id: prompt-undelivered
name: Prompt Undelivered
trigger:
  on: task.created
steps:
  - id: run
    name: Run
    type: run_agent
    config:
      role: pr-fix
      mode: headless
      provider: claude
      prompt: "Fix the PR."
    next:
      - goto: done
  - id: done
    name: Done
    type: set_status
    config:
      status: done
`)
}

func promptUndeliveredTask(t *testing.T, spent string) TaskInfo {
	t.Helper()
	vars := map[string]string{WorkflowVarDir: t.TempDir()}
	if spent != "" {
		vars[promptUndeliveredKey("run")] = spent
	}
	return TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "prompt-undelivered",
			CurrentStep: "run",
			State:       ExecWaiting,
			Variables:   vars,
			AgentRoutes: map[string]string{"agent-1": "run"},
		},
	}
}

func TestReschedulePromptUndeliveredAgent_ChargesItsOwnBudget(t *testing.T) {
	store := promptUndeliveredStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(promptUndeliveredTask(t, ""))

	engine.ReschedulePromptUndeliveredAgent("t1", "agent-1")

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got := ti.Workflow.Variables[promptUndeliveredKey("run")]; got != "1" {
		t.Fatalf("prompt-undelivered counter = %q, want 1", got)
	}
	if ti.Status == "human-required" {
		t.Fatal("first undelivered prompt parked the task instead of retrying")
	}
}

func TestReschedulePromptUndeliveredAgent_EscalatesWhenBudgetSpent(t *testing.T) {
	store := promptUndeliveredStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(promptUndeliveredTask(t, "3"))

	engine.ReschedulePromptUndeliveredAgent("t1", "agent-1")

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.Status != "human-required" {
		t.Fatalf("Status = %q, want human-required once the budget is spent", ti.Status)
	}
	if len(agents.calls) != 0 {
		t.Fatalf("StartAgent calls = %d, want none after escalation", len(agents.calls))
	}
}
