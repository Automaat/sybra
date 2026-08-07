package workflow

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/project"
)

func newParallelGatesStep() *Step { return &Step{ID: "parallel_gates", Type: StepParallelGates} }

// newParallelGatesEngine builds an engine wired the same way
// newFocusedChecksEngine does (worktree + focused checks + verify commands),
// which is exactly what execParallelGates needs to run all three gates.
func newParallelGatesEngine(t *testing.T, wt string, focused []project.FocusedCheck, verify []string) (*Engine, *memTasks) {
	t.Helper()
	engine, tasks, _ := newFocusedChecksEngine(t, wt, focused, verify)
	return engine, tasks
}

func workflowSurfaceFocusedCheck(cmds ...string) []project.FocusedCheck {
	return []project.FocusedCheck{{
		Name:     "workflow",
		Paths:    []string{"internal/workflow/**"},
		Commands: cmds,
	}}
}

// TestExecParallelGates_RunsGatesConcurrently proves focused_checks and
// verify_checks overlap instead of running serially: each sleeps 500ms, so a
// serial chain would take >=1s while running them concurrently finishes well
// under that. The margin (850ms) is chosen wide enough to absorb scheduler
// overhead without flaking, while still being far below the 1s a serial run
// would take.
func TestExecParallelGates_RunsGatesConcurrently(t *testing.T) {
	t.Parallel()

	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	writeRepoFile(t, wt, "internal/workflow/model.go", "package workflow\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: touch workflow")

	engine, tasks := newParallelGatesEngine(t, wt,
		workflowSurfaceFocusedCheck("sleep 0.5"), []string{"sleep 0.5"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	start := time.Now()
	out, err := engine.execParallelGates("t1", newParallelGatesStep(), nil, TaskInfo{ID: "t1", Status: "in-progress"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("Output = %q, want clean", out.Output)
	}
	if elapsed >= 850*time.Millisecond {
		t.Fatalf("elapsed = %s, want well under 1s (gates should overlap, not run serially)", elapsed)
	}
}

// TestExecParallelGates_AllCleanAdvances is the happy path: every gate
// passes, task status is left unchanged, and the coordinator reports clean.
func TestExecParallelGates_AllCleanAdvances(t *testing.T) {
	t.Parallel()

	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	writeRepoFile(t, wt, "internal/workflow/model.go", "package workflow\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: touch workflow")

	engine, tasks := newParallelGatesEngine(t, wt, workflowSurfaceFocusedCheck("true"), []string{"true"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execParallelGates("t1", newParallelGatesStep(), nil, TaskInfo{ID: "t1", Status: "in-progress"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("Output = %q, want clean", out.Output)
	}
	if ti := mustGetTaskInfo(t, tasks, "t1"); ti.Status != "in-progress" {
		t.Fatalf("status = %q, want unchanged in-progress", ti.Status)
	}
}

// TestExecParallelGates_TamperFlaggedWins proves detect_tampering's own
// human-required verdict wins outright even though focused_checks and
// verify_checks both pass — each gate still recorded its own evidence, but
// only tamper's status write survives.
func TestExecParallelGates_TamperFlaggedWins(t *testing.T) {
	t.Parallel()

	base := "package foo\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {\n\tif Foo() != 1 {\n\t\tt.Errorf(\"bad\")\n\t}\n}\n"
	wt := makeBaseRepo(t, map[string]string{
		"README.md":                "init\n",
		"internal/foo/foo.go":      "package foo\n\nfunc Foo() int { return 1 }\n",
		"internal/foo/foo_test.go": base,
	})
	tampered := "package foo\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {\n\tt.Skip(\"flaky\")\n}\n"
	writeRepoFile(t, wt, "internal/foo/foo_test.go", tampered)
	gitRun(t, wt, "commit", "-am", "test: skip foo")

	engine, tasks := newParallelGatesEngine(t, wt, nil, []string{"true"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execParallelGates("t1", newParallelGatesStep(), nil, TaskInfo{ID: "t1", Status: "in-progress"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.Output, "human-required") || !strings.Contains(out.Output, "detect_tampering") {
		t.Fatalf("Output = %q, want human-required naming detect_tampering", out.Output)
	}
	if ti := mustGetTaskInfo(t, tasks, "t1"); ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", ti.Status)
	}
}

// TestExecParallelGates_VerifyBlockedWins proves a classified
// non-auto-fixable verify_checks failure (verifier infrastructure
// instability) blocks the task even though focused_checks and detect_tampering
// both pass.
func TestExecParallelGates_VerifyBlockedWins(t *testing.T) {
	t.Parallel()

	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	writeRepoFile(t, wt, "internal/workflow/model.go", "package workflow\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: touch workflow")

	engine, tasks := newParallelGatesEngine(t, wt,
		workflowSurfaceFocusedCheck("true"), []string{`echo "link: signal: terminated" >&2; exit 1`})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	wf := implementedExec()
	out, err := engine.execParallelGates("t1", newParallelGatesStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.Output, "blocked") {
		t.Fatalf("Output = %q, want blocked", out.Output)
	}
	if ti := mustGetTaskInfo(t, tasks, "t1"); ti.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", ti.Status)
	}
}

// TestExecParallelGates_BothFailuresMergedRewind proves that when
// focused_checks and verify_checks both fail with an auto-fixable failure in
// the same round, the coordinator performs a single merged rewind: both
// gates' reask notes and auto-fix counters end up on the shared *Execution,
// and the coordinator returns errStepParked exactly once.
func TestExecParallelGates_BothFailuresMergedRewind(t *testing.T) {
	t.Parallel()

	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	writeRepoFile(t, wt, "internal/workflow/model.go", "package workflow\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: touch workflow")

	engine, tasks := newParallelGatesEngine(t, wt,
		workflowSurfaceFocusedCheck("echo focused-boom >&2; exit 1"),
		[]string{"echo verify-boom >&2; exit 1"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	wf := implementedExec()
	_, err := engine.execParallelGates("t1", newParallelGatesStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	if wf.CurrentStep != verifyChecksImplStepID {
		t.Fatalf("CurrentStep = %q, want %q", wf.CurrentStep, verifyChecksImplStepID)
	}
	if wf.State != ExecWaiting {
		t.Fatalf("State = %q, want ExecWaiting", wf.State)
	}
	if !strings.Contains(wf.Variables[focusedChecksReaskNoteVar], "FAILED Sybra's focused checks") {
		t.Fatalf("focused reask note missing: %q", wf.Variables[focusedChecksReaskNoteVar])
	}
	if !strings.Contains(wf.Variables[verifyReaskNoteVar], "FAILED the project verify suite") {
		t.Fatalf("verify reask note missing: %q", wf.Variables[verifyReaskNoteVar])
	}
	if wf.Variables["step.focused_checks.auto_fix"] != "1" {
		t.Fatalf("focused auto_fix counter = %q, want 1", wf.Variables["step.focused_checks.auto_fix"])
	}
	if wf.Variables["step.verify_checks.auto_fix"] != "1" {
		t.Fatalf("verify auto_fix counter = %q, want 1", wf.Variables["step.verify_checks.auto_fix"])
	}
	if ti := mustGetTaskInfo(t, tasks, "t1"); ti.Status != "in-progress" {
		t.Fatalf("status = %q, want unchanged in-progress (rewind, not an escalation)", ti.Status)
	}
}

// TestExecParallelGates_FocusedUnconfiguredDegradesToTamperAndVerify proves
// that with no focused checks configured, the coordinator still runs
// detect_tampering and verify_checks concurrently and routes on their
// outcome alone.
func TestExecParallelGates_FocusedUnconfiguredDegradesToTamperAndVerify(t *testing.T) {
	t.Parallel()

	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	writeRepoFile(t, wt, "internal/workflow/model.go", "package workflow\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: touch workflow")

	engine, tasks := newParallelGatesEngine(t, wt, nil, []string{"true"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execParallelGates("t1", newParallelGatesStep(), nil, TaskInfo{ID: "t1", Status: "in-progress"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("Output = %q, want clean", out.Output)
	}
}
