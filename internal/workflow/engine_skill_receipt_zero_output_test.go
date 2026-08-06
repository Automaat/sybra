package workflow

import (
	"maps"
	"testing"
)

// A provider child that emitted no turns never saw the skill instructions, so
// its missing receipt must not spend the conformance budget that exists to
// catch an agent ignoring a mandatory skill.
func zeroOutputSkillReceiptStore(t *testing.T) *Store {
	t.Helper()
	return newInlineTestStore(t, "skill-receipt", `id: skill-receipt
name: Skill Receipt
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
      prompt: "Run /fix-review now."
    next:
      - goto: done
  - id: done
    name: Done
    type: set_status
    config:
      status: done
`)
}

func zeroOutputSkillReceiptTask(t *testing.T, agentID string, vars map[string]string) TaskInfo {
	t.Helper()
	variables := map[string]string{WorkflowVarDir: t.TempDir()}
	maps.Copy(variables, vars)
	return TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "skill-receipt",
			CurrentStep: "run",
			State:       ExecWaiting,
			Variables:   variables,
		},
		AgentRuns: []AgentRunInfo{{
			AgentID:            agentID,
			Role:               "pr-fix",
			Provider:           "claude",
			RequestedSkill:     "fix-review",
			SkillExecutionMode: "injected",
			SkillConformance:   "unverified",
			TurnCount:          0,
		}},
	}
}

func TestHandleAgentComplete_ZeroOutputRunSparesConformanceBudget(t *testing.T) {
	store := zeroOutputSkillReceiptStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(zeroOutputSkillReceiptTask(t, "agent-1", nil))

	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "agent-1", Result: "", Success: false})

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got := ti.Workflow.Variables[skillReceiptRecoveryKey("run")]; got != "" {
		t.Fatalf("conformance retry var = %q, want unset for a zero-output run", got)
	}
	if got := ti.Workflow.Variables[skillReceiptZeroOutputKey("run")]; got != "1" {
		t.Fatalf("zero-output retry var = %q, want 1", got)
	}
	if ti.Status == "human-required" {
		t.Fatal("zero-output run parked the task instead of re-dispatching")
	}
	if len(agents.calls) != 1 {
		t.Fatalf("StartAgent calls = %d, want 1 retry", len(agents.calls))
	}
}

func TestHandleAgentComplete_ZeroOutputRunsEscalateAfterOwnCeiling(t *testing.T) {
	store := zeroOutputSkillReceiptStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	spent := map[string]string{
		skillReceiptZeroOutputKey("run"): "3",
	}
	tasks.Put(zeroOutputSkillReceiptTask(t, "agent-1", spent))

	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "agent-1", Result: "", Success: false})

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.Status != "human-required" {
		t.Fatalf("Status = %q, want human-required once the zero-output ceiling is spent", ti.Status)
	}
	if len(agents.calls) != 0 {
		t.Fatalf("StartAgent calls = %d, want no further retry", len(agents.calls))
	}
}
