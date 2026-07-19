package workflow

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func implementedExec() *Execution {
	wf := &Execution{Variables: map[string]string{}, StartedAt: time.Now().UTC()}
	now := time.Now().UTC()
	wf.RecordStep(StepRecord{StepID: verifyChecksImplStepID, Status: "completed", StartedAt: now, EndedAt: now})
	return wf
}

func makeLintVerifyRepo(t *testing.T) string {
	t.Helper()
	wt := makeBaseRepo(t, map[string]string{
		"go.mod":                   "module example.com/verifyrepo\n\ngo 1.26.5\n",
		"internal/bar/bar.go":      "package bar\n\nfunc Value() int { return 1 }\n",
		"internal/foo/foo.go":      "package foo\n\nfunc Value() (int, error) { return 1, nil }\n",
		"internal/foo/foo_test.go": "package foo\n",
		"internal/keep/keep.go":    "package keep\n\nfunc Value() int { return 1 }\n",
	})
	writeRepoFile(t, wt, "internal/foo/foo.go", "package foo\n\nfunc Value() (int, error) { return 2, nil }\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: change foo")
	return wt
}

func lintVerifyCommand(file string) string {
	return "grep -q 'func Value() (v int, err error)' " + file +
		" || { printf '" + file + ":3:1: unnamedResult: consider giving a name to these results (gocritic)\\n' >&2; exit 1; } # golangci-lint"
}

func TestExecVerifyChecks_AutoFixRewindsToImplement(t *testing.T) {
	t.Parallel()
	wt := makeLintVerifyRepo(t)
	engine, tasks := newVerifyChecksEngine(t, wt, []string{lintVerifyCommand("internal/foo/foo.go")})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	wf := implementedExec()
	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	if out.StepID != "" || out.Status != "" {
		t.Fatalf("parked output should be zero, got %+v", out)
	}
	if wf.CurrentStep != verifyChecksImplStepID {
		t.Errorf("CurrentStep = %q, want %q", wf.CurrentStep, verifyChecksImplStepID)
	}
	if wf.State != ExecWaiting {
		t.Errorf("State = %q, want %q", wf.State, ExecWaiting)
	}
	if wf.Variables["step.verify_checks.auto_fix"] != "1" {
		t.Errorf("auto_fix counter = %q, want 1", wf.Variables["step.verify_checks.auto_fix"])
	}
	note := wf.Variables[verifyReaskNoteVar]
	if !strings.Contains(note, "FAILED the project verify suite") ||
		!strings.Contains(note, "internal/foo/foo.go:3:1: unnamedResult") ||
		!strings.Contains(note, "golangci-lint") {
		t.Errorf("reask note missing failure context:\n%s", note)
	}
	if wf.Variables[workflowRetryAfterVar] == "" {
		t.Errorf("retry-after not set")
	}
	if ti := mustGetTaskInfo(t, tasks, "t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want in-progress (not escalated on first failure)", ti.Status)
	}
}

func TestExecVerifyChecks_AutoFixCapEscalates(t *testing.T) {
	t.Parallel()
	wt := makeLintVerifyRepo(t)
	engine, tasks := newVerifyChecksEngine(t, wt, []string{lintVerifyCommand("internal/foo/foo.go")})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	wf := implementedExec()
	wf.Variables["step.verify_checks.auto_fix"] = "2"
	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Errorf("Output = %q, want flagged", out.Output)
	}
	if ti := mustGetTaskInfo(t, tasks, "t1"); ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required after cap", ti.Status)
	}
}

func TestExecVerifyChecks_LintRepairAdvancesToReadyReview(t *testing.T) {
	t.Parallel()
	wt := makeLintVerifyRepo(t)
	store := newInlineTestStore(t, "verify-lint-repair", `
id: verify-lint-repair
name: Verify lint repair
steps:
  - id: set_in_progress
    type: set_status
    config:
      status: in-progress
    next:
      - goto: implement
  - id: implement
    type: run_agent
    config:
      role: implementation
      mode: headless
      model: sonnet
      prompt: "Implement {{.Task.ID}}"
    next:
      - goto: verify_checks
  - id: verify_checks
    type: verify_checks
    next:
      - when:
          field: task.status
          operator: equals
          value: blocked
        goto: ""
      - when:
          field: task.status
          operator: equals
          value: human-required
        goto: ""
      - goto: set_ready_review
  - id: set_ready_review
    type: set_status
    config:
      status: ready-review
    next:
      - goto: ""
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wt, ok: true})
	engine.SetCheckConfigGetter(&fakeCheckGetter{cmds: []string{lintVerifyCommand("internal/foo/foo.go")}})
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})

	if err := engine.StartWorkflow("t1", "verify-lint-repair"); err != nil {
		t.Fatal(err)
	}
	if got := agents.LastCall().Role; got != "implementation" {
		t.Fatalf("first agent role = %q, want implementation", got)
	}

	firstAgentID := agents.LastID()
	agents.SimulateComplete("t1")
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: firstAgentID, Success: true, Result: "first pass"})

	first := mustGetTaskInfo(t, tasks, "t1")
	if first.Status != "in-progress" {
		t.Fatalf("status after lint reask = %q, want in-progress", first.Status)
	}
	if first.Workflow == nil || first.Workflow.CurrentStep != verifyChecksImplStepID || first.Workflow.State != ExecWaiting {
		t.Fatalf("workflow after lint reask = %+v, want implement/ExecWaiting", first.Workflow)
	}
	if first.Workflow.Variables["step.verify_checks.auto_fix"] != "1" {
		t.Fatalf("auto_fix counter after first failure = %q, want 1", first.Workflow.Variables["step.verify_checks.auto_fix"])
	}
	if !strings.Contains(first.Workflow.Variables[verifyReaskNoteVar], "unnamedResult") {
		t.Fatalf("verify_reask_note missing lint detail:\n%s", first.Workflow.Variables[verifyReaskNoteVar])
	}

	writeRepoFile(t, wt, "internal/foo/foo.go", "package foo\n\nfunc Value() (v int, err error) { return 2, nil }\n")

	first.Workflow.SetVar(workflowRetryAfterVar, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))
	if err := tasks.SetWorkflow("t1", first.Workflow); err != nil {
		t.Fatal(err)
	}

	engine.ResumeStalled()
	if got := agents.CallCount(); got != 2 {
		t.Fatalf("StartAgent calls after resume = %d, want 2", got)
	}
	secondAgentID := agents.LastID()
	agents.SimulateComplete("t1")
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: secondAgentID, Success: true, Result: "repaired"})

	final := mustGetTaskInfo(t, tasks, "t1")
	if final.Status != "ready-review" {
		t.Fatalf("final status = %q, want ready-review", final.Status)
	}
	if final.Workflow == nil || final.Workflow.State != ExecCompleted || final.Workflow.CurrentStep != "" {
		t.Fatalf("final workflow = %+v, want completed terminal workflow", final.Workflow)
	}
	if strings.Contains(final.StatusReason, "human-required") {
		t.Fatalf("final status reason unexpectedly escalated: %q", final.StatusReason)
	}
}

func TestExecVerifyChecks_NoImplementStepEscalates(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, tasks := newVerifyChecksEngine(t, wt, []string{"exit 1"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	wf := &Execution{Variables: map[string]string{}, StartedAt: time.Now().UTC()}
	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Errorf("Output = %q, want flagged (no implement step to rewind to)", out.Output)
	}
	if ti := mustGetTaskInfo(t, tasks, "t1"); ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required", ti.Status)
	}
}
