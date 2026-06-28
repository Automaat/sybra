package workflow

import (
	"fmt"
	"strings"
	"testing"
)

func validPlanContract(taskID string) string {
	return fmt.Sprintf(`{
  "task_id": %q,
  "branch": "feat/example-%s",
  "worktree": "/home/sybra/.sybra/worktrees/example-%s",
  "files": [
    {"path": "internal/workflow/engine.go", "purpose": "edit", "symbols": ["Engine"]}
  ],
  "steps": ["wire the contract through the workflow"],
  "verification": [
    {"command": "go test ./internal/workflow", "expected": "tests pass"}
  ],
  "acceptance_criteria": ["implementation prompt includes the contract"],
  "risk_tier": "medium",
  "permission_tier": "repo-write",
  "rollback": "revert the workflow and sidecar changes"
}`, taskID, taskID, taskID)
}

func newValidatePlanContractStep() *Step {
	return &Step{ID: "validate_plan_contract", Type: StepValidatePlanContract}
}

func TestExecValidatePlanContract_ValidContractPasses(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "fa6919fc", Status: "planning"})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execValidatePlanContract("fa6919fc", newValidatePlanContractStep(),
		TaskInfo{ID: "fa6919fc", PlanContract: validPlanContract("fa6919fc")})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("fa6919fc")
	if ti.Status == "human-required" {
		t.Errorf("valid contract flipped status to human-required: reason=%q", tasks.Reason("fa6919fc"))
	}
}

func TestExecValidatePlanContract_MissingContractPassesForMigration(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "fa6919fc", Status: "planning"})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execValidatePlanContract("fa6919fc", newValidatePlanContractStep(),
		TaskInfo{ID: "fa6919fc", PlanContract: " \n\t"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "markdown-only migration fallback") {
		t.Errorf("Output = %q, want migration fallback", out.Output)
	}
	ti, _ := tasks.GetTask("fa6919fc")
	if ti.Status == "human-required" {
		t.Errorf("missing contract should not flip status during migration: reason=%q", tasks.Reason("fa6919fc"))
	}
}

func TestExecValidatePlanContract_RejectsWrongTaskID(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "fa6919fc", Status: "planning"})
	engine := newEngineForEval(t, tasks)

	_, err := engine.execValidatePlanContract("fa6919fc", newValidatePlanContractStep(),
		TaskInfo{ID: "fa6919fc", PlanContract: validPlanContract("a9375bad")})
	if err != nil {
		t.Fatal(err)
	}
	ti, _ := tasks.GetTask("fa6919fc")
	if ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", ti.Status)
	}
	if reason := tasks.Reason("fa6919fc"); !strings.Contains(reason, "task_id") || !strings.Contains(reason, "a9375bad") {
		t.Errorf("reason = %q, want wrong task_id", reason)
	}
}

func TestExecValidatePlanContract_RejectsMissingVerificationCriteria(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "fa6919fc", Status: "planning"})
	engine := newEngineForEval(t, tasks)
	contract := strings.Replace(validPlanContract("fa6919fc"),
		`  "verification": [
    {"command": "go test ./internal/workflow", "expected": "tests pass"}
  ],`,
		`  "verification": [],`, 1)

	_, err := engine.execValidatePlanContract("fa6919fc", newValidatePlanContractStep(),
		TaskInfo{ID: "fa6919fc", PlanContract: contract})
	if err != nil {
		t.Fatal(err)
	}
	if reason := tasks.Reason("fa6919fc"); !strings.Contains(reason, "verification must include") {
		t.Errorf("reason = %q, want missing verification", reason)
	}
}

func TestValidatePlanContractForTask_RequiresSourceAcceptanceCriteria(t *testing.T) {
	taskBody := "## Problem\nBuild the thing.\n\n" +
		"## Acceptance Criteria\n\n" +
		"- First source criterion.\n" +
		"- Wrapped source criterion is preserved before reuse in any public\n" +
		"  artifact.\n"
	contract := strings.Replace(validPlanContract("fa6919fc"),
		`"acceptance_criteria": ["implementation prompt includes the contract"]`,
		`"acceptance_criteria": ["First source criterion."]`, 1)

	problems := ValidatePlanContractForTask(contract, "fa6919fc", taskBody)
	joined := strings.Join(problems, "\n")
	if !strings.Contains(joined, "acceptance_criteria missing source criterion") ||
		!strings.Contains(joined, "Wrapped source criterion is preserved before reuse in any public artifact.") {
		t.Fatalf("problems = %v, want missing wrapped source criterion", problems)
	}
}

func TestValidatePlanContractForTask_AcceptsCopiedSourceAcceptanceCriteria(t *testing.T) {
	taskBody := "## Acceptance Criteria\n\n" +
		"1. First source criterion.\n" +
		"2. Wrapped source criterion is preserved before reuse in any public\n" +
		"   artifact.\n\n" +
		"## Notes\nNot an acceptance criterion.\n"
	contract := strings.Replace(validPlanContract("fa6919fc"),
		`"acceptance_criteria": ["implementation prompt includes the contract"]`,
		`"acceptance_criteria": ["First source criterion.", "Wrapped source criterion is preserved before reuse in any public artifact."]`, 1)

	if problems := ValidatePlanContractForTask(contract, "fa6919fc", taskBody); len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
}

func TestExecValidatePlanContract_RejectsMalformedFiles(t *testing.T) {
	for _, tc := range []struct {
		name      string
		path      string
		wantError string
	}{
		{name: "absolute unix", path: "/tmp/evil.go", wantError: "repository-relative"},
		{name: "absolute windows backslash", path: `C:\tmp\evil.go`, wantError: "forward slashes"},
		{name: "absolute windows slash", path: "C:/tmp/evil.go", wantError: "repository-relative"},
		{name: "relative backslash", path: `internal\workflow\engine.go`, wantError: "forward slashes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tasks := newMemTasks()
			tasks.Put(TaskInfo{ID: "fa6919fc", Status: "planning"})
			engine := newEngineForEval(t, tasks)
			contract := strings.Replace(validPlanContract("fa6919fc"), `"internal/workflow/engine.go"`, fmt.Sprintf("%q", tc.path), 1)

			_, err := engine.execValidatePlanContract("fa6919fc", newValidatePlanContractStep(),
				TaskInfo{ID: "fa6919fc", PlanContract: contract})
			if err != nil {
				t.Fatal(err)
			}
			if reason := tasks.Reason("fa6919fc"); !strings.Contains(reason, tc.wantError) {
				t.Errorf("reason = %q, want %q", reason, tc.wantError)
			}
		})
	}
}

func TestExecValidatePlanContract_RejectsForeignWorktreeOrBranch(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "fa6919fc", Status: "planning"})
	engine := newEngineForEval(t, tasks)
	contract := strings.ReplaceAll(validPlanContract("fa6919fc"), "example-fa6919fc", "example-a9375bad")

	_, err := engine.execValidatePlanContract("fa6919fc", newValidatePlanContractStep(),
		TaskInfo{ID: "fa6919fc", PlanContract: contract})
	if err != nil {
		t.Fatal(err)
	}
	if reason := tasks.Reason("fa6919fc"); !strings.Contains(reason, "foreign task ID a9375bad") {
		t.Errorf("reason = %q, want foreign task ID", reason)
	}
}
