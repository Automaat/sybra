package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/taskstatus"
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
// verify_checks overlap instead of running serially: each publishes a marker
// and refuses to finish until it observes its peer's marker. A serial
// implementation therefore fails deterministically instead of relying on a
// wall-clock threshold that flakes under package-wide parallel test load.
func TestExecParallelGates_RunsGatesConcurrently(t *testing.T) {
	t.Parallel()

	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	writeRepoFile(t, wt, "internal/workflow/model.go", "package workflow\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: touch workflow")

	barrier := t.TempDir()
	focusedMarker := filepath.Join(barrier, "focused")
	verifyMarker := filepath.Join(barrier, "verify")
	waitForPeer := func(own, peer string) string {
		return fmt.Sprintf("touch %q; i=0; while [ ! -f %q ] && [ $i -lt 100 ]; do sleep 0.02; i=$((i+1)); done; [ -f %q ]", own, peer, peer)
	}
	engine, tasks := newParallelGatesEngine(t, wt,
		workflowSurfaceFocusedCheck(waitForPeer(focusedMarker, verifyMarker)),
		[]string{waitForPeer(verifyMarker, focusedMarker)})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execParallelGates("t1", newParallelGatesStep(), nil, TaskInfo{ID: "t1", Status: "in-progress"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("Output = %q, want clean", out.Output)
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

func TestExecParallelGates_VerificationFatalPreventsFocusedRewind(t *testing.T) {
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	writeRepoFile(t, wt, "internal/workflow/model.go", "package workflow\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: touch workflow")
	engine, tasks := newParallelGatesEngine(t, wt,
		workflowSurfaceFocusedCheck("false"), []string{"true"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	mgr := &copyingVerificationManager{root: t.TempDir(), finalizeErr: errors.New("source moved")}
	engine.execution.Verification = mgr
	wf := &Execution{CurrentStep: "parallel_gates", State: ExecRunning}
	now := time.Now()
	wf.RecordStep(StepRecord{StepID: verifyChecksImplStepID, Status: "completed", StartedAt: now, EndedAt: now})

	_, err := engine.execParallelGates("t1", newParallelGatesStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if err == nil || !strings.Contains(err.Error(), "source moved") {
		t.Fatalf("execParallelGates error = %v, want source movement", err)
	}
	if got := mustGetTaskInfo(t, tasks, "t1").Status; got != "in-progress" {
		t.Fatalf("task status = %q, want unchanged", got)
	}
	if wf.CountStep(verifyChecksImplStepID) == 0 {
		t.Fatal("focused rewind mutated workflow before verification fatal error")
	}
	if len(wf.Variables) != 0 {
		t.Fatalf("workflow variables mutated before verification fatal error: %v", wf.Variables)
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

// TestExecParallelGates_TerminalFocusedSurvivesVerifyRewind proves a terminal
// focused_checks escalation is not erased by verify_checks' auto-fix rewind in
// the same joined round. The rewind re-writes the STALE pre-gate task status,
// so applying it after focused_checks flagged would resume implementation on a
// task that must stop for a human — the serial chain never could, because it
// never ran verify_checks once focused_checks escalated.
func TestExecParallelGates_TerminalFocusedSurvivesVerifyRewind(t *testing.T) {
	t.Parallel()

	// The two verdicts are handed in directly rather than provoked by real
	// commands: the pairing under test is "focused_checks blew its time budget
	// (terminal) while verify_checks failed auto-fixably (wants to rewind)",
	// and both gates share one timeout budget, so driving it through real
	// commands would make which verdict lands a race against host load.
	engine, tasks := newParallelGatesEngine(t, t.TempDir(), nil, nil)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	focusedVerdict := focusedChecksVerdict{
		cmds:         []string{"go test ./internal/workflow/..."},
		changedFiles: []string{"internal/workflow/model.go"},
		runErr:       context.DeadlineExceeded,
		timeout:      time.Minute,
	}
	verifyVerdict := verifyChecksVerdict{
		report: verifyChecksReport{
			Commands:   []string{"go test ./..."},
			FailedCmd:  "go test ./...",
			OutputTail: "--- FAIL: TestFoo",
		},
		timeout: time.Minute,
	}

	wf := implementedExec()
	out, err := engine.resolveParallelGates("t1", newParallelGatesStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"},
		StepOutput{StepID: gateTamperStepID, Status: "completed", Output: "clean"},
		&Step{ID: gateFocusedStepID}, focusedVerdict,
		&Step{ID: gateVerifyStepID}, verifyChecksPreflight{needsRun: true}, verifyVerdict)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.Output, "human-required") {
		t.Fatalf("Output = %q, want human-required", out.Output)
	}
	if ti := mustGetTaskInfo(t, tasks, "t1"); ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required (verify's rewind must not restore the pre-gate status)", ti.Status)
	}
	if wf.State == ExecWaiting || wf.CurrentStep == verifyChecksImplStepID {
		t.Fatalf("workflow rewound to %q/%q, want no rewind alongside a terminal gate", wf.CurrentStep, wf.State)
	}
	if got := wf.Variables["step.verify_checks.auto_fix"]; got != "" {
		t.Fatalf("verify auto_fix counter = %q, want unset (its verdict must not be applied)", got)
	}
}

// TestExecParallelGates_AutoFixCeilingSurvivesPeerRewind covers the same
// invariant for the gate that only turns terminal at apply time: focused_checks
// has spent its auto-fix ceiling, so its apply escalates instead of rewinding.
// It must apply first (more prior attempts = closest to the ceiling) and stop
// the round before verify_checks' rewind can overwrite the escalation.
func TestExecParallelGates_AutoFixCeilingSurvivesPeerRewind(t *testing.T) {
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
	wf.SetVar("step.focused_checks.auto_fix", strconv.Itoa(verifyChecksAutoFixCeiling))

	out, err := engine.execParallelGates("t1", newParallelGatesStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.Output, "human-required") {
		t.Fatalf("Output = %q, want human-required", out.Output)
	}
	if ti := mustGetTaskInfo(t, tasks, "t1"); ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required (verify's rewind must not restore the pre-gate status)", ti.Status)
	}
	if wf.State == ExecWaiting || wf.CurrentStep == verifyChecksImplStepID {
		t.Fatalf("workflow rewound to %q/%q, want no rewind after the auto-fix ceiling escalated", wf.CurrentStep, wf.State)
	}
	if got := wf.Variables["step.verify_checks.auto_fix"]; got != "" {
		t.Fatalf("verify auto_fix counter = %q, want unset (its verdict must not be applied)", got)
	}
}

// TestResumeStalled_ResumesParkedParallelGates walks the whole backpressure
// park: with the single verify slot held by a peer task, the coordinator parks
// itself as ExecWaiting with a retry-after, and once the slot frees
// ResumeStalled must re-enter it. It only does so if parallel_gates is a
// resumable step type — otherwise the task stays parked forever.
func TestResumeStalled_ResumesParkedParallelGates(t *testing.T) {
	wt1 := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	wt2 := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	writeRepoFile(t, wt2, "internal/workflow/model.go", "package workflow\n")
	gitRun(t, wt2, "add", ".")
	gitRun(t, wt2, "commit", "-m", "feat: touch workflow")

	blocking := "touch .verify-entered; while [ ! -f .verify-release ]; do sleep 0.05; done"

	store := newTestStore(t)
	if err := store.Save(Definition{
		ID:   "gates-wf",
		Name: "gates wf",
		Steps: []Step{
			{ID: "parallel_gates", Type: StepParallelGates, Next: []Transition{{GoTo: "set_ready_review"}}},
			{ID: "set_ready_review", Type: StepSetStatus, Config: StepConfig{Status: "ready-review"}, Next: []Transition{{GoTo: ""}}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	engine := NewTestEngine(store, newMemTasks(), newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{ok: true, paths: map[string]string{"t1": wt1, "t2": wt2}})
	engine.setCheckConfigGetterForTest(&fakeCheckGetter{
		focused:    workflowSurfaceFocusedCheck("true"),
		cmdsByTask: map[string][]string{"t1": {blocking}, "t2": {"true"}},
	})
	tasks := engine.tasks.(*memTasks)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	tasks.Put(TaskInfo{
		ID:     "t2",
		Status: "in-progress",
		Workflow: &Execution{
			WorkflowID: "gates-wf", CurrentStep: "parallel_gates",
			State: ExecRunning, Variables: map[string]string{},
		},
	})

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, _ = engine.execParallelGates("t1", newParallelGatesStep(), nil, TaskInfo{ID: "t1", Status: "in-progress"})
	}()
	waitForTestFile(t, filepath.Join(wt1, ".verify-entered"))

	parked, _ := tasks.GetTask("t2")
	_, err := engine.execParallelGates("t2", newParallelGatesStep(), parked.Workflow, TaskInfo{ID: "t2", Status: taskstatus.InProgress})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked (verify slot is held by t1)", err)
	}
	parked, _ = tasks.GetTask("t2")
	if parked.Workflow.CurrentStep != "parallel_gates" || parked.Workflow.State != ExecWaiting {
		t.Fatalf("parked at %q/%q, want parallel_gates/waiting", parked.Workflow.CurrentStep, parked.Workflow.State)
	}

	if err := os.WriteFile(filepath.Join(wt1, ".verify-release"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for t1's verify to release the slot")
	}

	// The park's own backoff has not elapsed; rewind it so this tick is
	// eligible, which is exactly what a later maintenance tick would see.
	parked.Workflow.Variables[workflowRetryAfterVar] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	tasks.Put(parked)

	engine.ResumeStalled()

	resumed, _ := tasks.GetTask("t2")
	if resumed.Status != "ready-review" {
		t.Fatalf("status = %q, want ready-review — the parked parallel_gates was never resumed (reason=%q)",
			resumed.Status, tasks.Reason("t2"))
	}
}

func waitForTestFile(t *testing.T, path string) {
	t.Helper()
	ok := pollUntil(5*time.Second, 10*time.Millisecond, func() bool {
		_, err := os.Stat(path)
		return err == nil
	})
	if !ok {
		t.Fatalf("timed out waiting for %s", path)
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
	tasks.Put(TaskInfo{ID: "t1", Status: taskstatus.InProgress})

	out, err := engine.execParallelGates("t1", newParallelGatesStep(), nil, TaskInfo{ID: "t1", Status: taskstatus.InProgress})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("Output = %q, want clean", out.Output)
	}
}
