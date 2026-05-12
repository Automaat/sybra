package workflow

import (
	"strings"
	"testing"
)

func newValidatePlanStep() *Step {
	return &Step{ID: "validate_plan_refs", Type: StepValidatePlan}
}

func TestExecValidatePlan_CleanPlanPasses(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "fa6919fc", Status: "planning"})
	engine := newEngineForEval(t, tasks)

	plan := "# Plan\n\nBranch: `perf/bk-tree-on-per-base-buckets-fa6919fc`\nEdit src/cleaner.rs:60.\n"
	out, err := engine.execValidatePlan("fa6919fc", newValidatePlanStep(),
		TaskInfo{ID: "fa6919fc", Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("fa6919fc")
	if ti.Status == "human-required" {
		t.Errorf("clean plan flipped status to human-required: reason=%q", tasks.Reason("fa6919fc"))
	}
}

func TestExecValidatePlan_ForeignBranchRefFlipsHumanRequired(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "fa6919fc", Status: "planning"})
	engine := newEngineForEval(t, tasks)

	// Reproduces the fa6919fc → a9375bad incident: plan body cites a sibling
	// task's worktree path inside the same project.
	plan := "# Plan\n\nBranch: `perf/bk-tree-on-per-base-buckets-fa6919fc`\n" +
		"Worktree: /home/sybra/.sybra/worktrees/cross-base-typo-detection-a9375bad\n"
	out, err := engine.execValidatePlan("fa6919fc", newValidatePlanStep(),
		TaskInfo{ID: "fa6919fc", Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed (mechanical step)", out.Status)
	}
	ti, _ := tasks.GetTask("fa6919fc")
	if ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required", ti.Status)
	}
	reason := tasks.Reason("fa6919fc")
	if !strings.Contains(reason, "a9375bad") {
		t.Errorf("reason = %q, want substring 'a9375bad'", reason)
	}
	if !strings.Contains(reason, "contamination") {
		t.Errorf("reason = %q, want substring 'contamination'", reason)
	}
}

func TestExecValidatePlan_ForeignBranchRefAlone(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "fa6919fc", Status: "planning"})
	engine := newEngineForEval(t, tasks)

	// Branch ref alone (no worktree path) must still trip the validator.
	plan := "Use branch fix/cross-base-typo-detection-a9375bad as base."
	_, err := engine.execValidatePlan("fa6919fc", newValidatePlanStep(),
		TaskInfo{ID: "fa6919fc", Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	ti, _ := tasks.GetTask("fa6919fc")
	if ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required", ti.Status)
	}
}

func TestExecValidatePlan_EmptyPlanPasses(t *testing.T) {
	// require_plan upstream should have flagged this; validate_plan must not
	// double-flip or error.
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "fa6919fc", Status: "planning"})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execValidatePlan("fa6919fc", newValidatePlanStep(),
		TaskInfo{ID: "fa6919fc", Plan: "   \n\t\n"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("fa6919fc")
	if ti.Status == "human-required" {
		t.Errorf("empty plan should not flip status; reason=%q", tasks.Reason("fa6919fc"))
	}
}

func TestExecValidatePlan_GitShortShaIgnored(t *testing.T) {
	// 8-hex git short SHAs in code/commit references must NOT be flagged
	// as foreign task IDs. The validator only matches structural patterns
	// (<type>/<slug>-<id>, legacy sybra/<slug>-<id>, worktrees/<slug>-<id>).
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "fa6919fc", Status: "planning"})
	engine := newEngineForEval(t, tasks)

	plan := "# Plan\n\nFix builds on commit 8e273bcd. Then cherry-pick a1b2c3d4.\n"
	_, err := engine.execValidatePlan("fa6919fc", newValidatePlanStep(),
		TaskInfo{ID: "fa6919fc", Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	ti, _ := tasks.GetTask("fa6919fc")
	if ti.Status == "human-required" {
		t.Errorf("git short SHA tripped validator; reason=%q", tasks.Reason("fa6919fc"))
	}
}

func TestCollectForeignTaskIDs_DedupesAndSorts(t *testing.T) {
	plan := "fix/foo-aaaaaaaa and again sybra/bar-aaaaaaaa, plus worktrees/baz-bbbbbbbb."
	got := collectForeignTaskIDs(plan, "fa6919fc")
	want := []string{"aaaaaaaa", "bbbbbbbb"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCollectForeignTaskIDs_OwnIDIgnored(t *testing.T) {
	plan := "feat/my-slug-fa6919fc and worktrees/my-slug-fa6919fc"
	got := collectForeignTaskIDs(plan, "fa6919fc")
	if len(got) != 0 {
		t.Errorf("got %v, want empty (own ID should be ignored)", got)
	}
}
