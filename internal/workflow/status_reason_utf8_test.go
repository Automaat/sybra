package workflow

import (
	"strings"
	"testing"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// The engine truncates raw agent output into a status_reason, which internal/task
// persists as YAML frontmatter (`yaml:"status_reason,omitempty"`). A cut landing
// inside a multibyte rune leaves invalid UTF-8, and yaml.v3 then re-encodes the
// whole field as an unreadable !!binary base64 block in the task file and on the
// board. Agent output routinely carries ellipses, arrows and emoji, so this
// drives the real engine path rather than the truncation helper alone.
func TestPlanningRetryStatusReasonSurvivesMultibyteAgentOutput(t *testing.T) {
	for name, output := range map[string]string{
		"ellipsis run":       strings.Repeat("…", 400),
		"arrow run":          strings.Repeat("→", 400),
		"emoji run":          strings.Repeat("🚀", 300),
		"box-drawing border": strings.Repeat("│─", 400),
	} {
		t.Run(name, func(t *testing.T) {
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
				if err := engine.AdvanceStep("t1", StepOutput{StepID: "plan", Status: "failed", Output: output}); err != nil {
					t.Fatal(err)
				}
			}

			ti, _ := tasks.GetTask("t1")
			if ti.Status != "human-required" {
				t.Fatalf("status = %q, want human-required", ti.Status)
			}
			if !utf8.ValidString(ti.StatusReason) {
				t.Fatalf("status_reason is not valid UTF-8: %q", ti.StatusReason)
			}

			data, err := yaml.Marshal(map[string]string{"status_reason": ti.StatusReason})
			if err != nil {
				t.Fatalf("yaml.Marshal: %v", err)
			}
			if strings.Contains(string(data), "!!binary") {
				t.Fatalf("status_reason marshalled as a binary block:\n%s", data)
			}
		})
	}
}

// Step output is also published as a workflow variable that reaches the frontend
// as JSON, where invalid UTF-8 is silently replaced by U+FFFD instead of failing.
func TestStepOutputVariableSurvivesMultibyteAgentOutput(t *testing.T) {
	store := newInlineTestStore(t, "test-simple", `
id: test-simple
name: test
trigger:
  on: task.created
steps:
  - id: triage
    type: run_agent
    config:
      role: implementation
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

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflowFromStepWithVars("t1", "test-simple", "triage", nil); err != nil {
		t.Fatal(err)
	}
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{
		StepID: "triage",
		Status: "completed",
		Output: strings.Repeat("…", 2000),
	}); err != nil {
		t.Fatal(err)
	}

	ti, _ := tasks.GetTask("t1")
	if ti.Workflow == nil {
		t.Fatal("workflow missing after advance")
	}
	got := ti.Workflow.Variables["step.triage.output"]
	if got == "" {
		t.Fatal("step output variable was not published")
	}
	if !utf8.ValidString(got) {
		t.Fatalf("step output variable is not valid UTF-8: %q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("step output variable contains U+FFFD replacement runes: %q", got)
	}
}
