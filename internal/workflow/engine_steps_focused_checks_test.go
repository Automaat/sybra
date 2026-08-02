package workflow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/project"
)

func newFocusedChecksStep() *Step { return &Step{ID: "focused_checks", Type: StepFocusedChecks} }

func newFocusedChecksEngine(t *testing.T, wt string, focused []project.FocusedCheck, verify []string) (*Engine, *memTasks, *recordingArtifactRecorder) {
	t.Helper()
	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wt, ok: true})
	engine.SetCheckConfigGetter(&fakeCheckGetter{focused: focused, cmds: verify})
	rec := &recordingArtifactRecorder{}
	engine.SetArtifactRecorder(rec)
	return engine, engine.tasks.(*memTasks), rec
}

func TestSelectFocusedChecks(t *testing.T) {
	t.Parallel()

	focused := []project.FocusedCheck{
		{
			Name:     "workflow",
			Paths:    []string{"internal/workflow/**"},
			Commands: []string{"go test ./internal/workflow/...", "go test ./internal/workflow/..."},
		},
		{
			Name:     "workflow-pkg",
			Packages: []string{"./internal/workflow/..."},
			Commands: []string{"go test ./internal/workflow/..."},
		},
		{
			Name:     "project",
			Packages: []string{"./internal/project"},
			Commands: []string{"go test ./internal/project"},
		},
		{
			Name:     "unsafe-path",
			Paths:    []string{"../secret/**"},
			Commands: []string{"echo nope"},
		},
		{
			Name:     "no-selectors",
			Commands: []string{"echo nope"},
		},
	}
	changed := []string{
		"internal/workflow/model.go",
		"internal/project/model.go",
		"frontend/src/App.svelte",
	}

	selected, cmds := selectFocusedChecks(focused, changed)
	if len(selected) != 3 {
		t.Fatalf("selected len = %d, want 3", len(selected))
	}
	if got := focusedSurfaceSummary(selected); got != "workflow, workflow-pkg, project" {
		t.Fatalf("focused surface summary = %q", got)
	}
	if want := []string{"go test ./internal/workflow/...", "go test ./internal/project"}; !slices.Equal(cmds, want) {
		t.Fatalf("commands = %v, want %v", cmds, want)
	}
	if !slices.Equal(selected[0].ChangedFiles, []string{"internal/workflow/model.go"}) {
		t.Fatalf("workflow changed files = %v", selected[0].ChangedFiles)
	}
	if !slices.Equal(selected[2].ChangedFiles, []string{"internal/project/model.go"}) {
		t.Fatalf("project changed files = %v", selected[2].ChangedFiles)
	}
}

func TestMatchRepoPath_MemoizesRepeatedDoubleStar(t *testing.T) {
	t.Parallel()

	done := make(chan bool, 1)
	go func() {
		done <- matchRepoPath(strings.Repeat("**/", 12)+"target.go", strings.TrimSuffix(strings.Repeat("seg/", 25), "/"))
	}()

	select {
	case matched := <-done:
		if matched {
			t.Fatal("matchRepoPath = true, want false")
		}
	case <-time.After(time.Second):
		t.Fatal("matchRepoPath did not finish for repeated ** pattern")
	}
}

func TestExecFocusedChecks_PassIsClean(t *testing.T) {
	t.Parallel()

	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	writeRepoFile(t, wt, "internal/workflow/model.go", "package workflow\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: touch workflow")

	engine, tasks, rec := newFocusedChecksEngine(t, wt, []project.FocusedCheck{{
		Name:     "workflow",
		Paths:    []string{"internal/workflow/**"},
		Packages: []string{"./internal/workflow/..."},
		Commands: []string{"true"},
	}}, []string{"false"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execFocusedChecks("t1", newFocusedChecksStep(), nil, TaskInfo{ID: "t1", Status: "in-progress"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("Output = %q, want clean", out.Output)
	}
	if len(rec.puts) != 1 || rec.puts[0].name != "focused-checks.json" {
		t.Fatalf("artifacts = %+v, want one focused-checks.json", rec.puts)
	}
	if !strings.Contains(rec.puts[0].content, `"workflow"`) || !strings.Contains(rec.puts[0].content, `"internal/workflow/model.go"`) {
		t.Fatalf("artifact content missing focused selection:\n%s", rec.puts[0].content)
	}
}

func TestExecFocusedChecks_ScaledTimeoutAbsorbsHostOversubscription(t *testing.T) {
	orig := workflowCheckLoadPerCPU
	workflowCheckLoadPerCPU = func() (float64, bool) { return 3.0, true }
	t.Cleanup(func() { workflowCheckLoadPerCPU = orig })

	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	writeRepoFile(t, wt, "internal/workflow/model.go", "package workflow\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: touch workflow")

	engine, tasks, _ := newFocusedChecksEngine(t, wt, []project.FocusedCheck{{
		Name:     "workflow",
		Paths:    []string{"internal/workflow/**"},
		Commands: []string{"sleep 0.2"},
	}}, nil)
	engine.SetVerifyTimeout(100 * time.Millisecond)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execFocusedChecks("t1", newFocusedChecksStep(), nil, TaskInfo{ID: "t1", Status: "in-progress"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("Output = %q, want clean (scaled timeout should cover host oversubscription)", out.Output)
	}
	if ti := mustGetTaskInfo(t, tasks, "t1"); ti.Status != "in-progress" {
		t.Fatalf("status = %q, want unchanged", ti.Status)
	}
}

func TestExecFocusedChecks_UnmappedChangesSkipWithoutVerifyFallback(t *testing.T) {
	t.Parallel()

	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	writeRepoFile(t, wt, "docs/readme.md", "hi\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "docs: touch readme")

	engine, tasks, rec := newFocusedChecksEngine(t, wt, []project.FocusedCheck{{
		Name:     "workflow",
		Paths:    []string{"internal/workflow/**"},
		Commands: []string{"false"},
	}}, []string{"true"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execFocusedChecks("t1", newFocusedChecksStep(), nil, TaskInfo{ID: "t1", Status: "in-progress"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "skipped: no safe focused mapping matched changed files" {
		t.Fatalf("Output = %q, want focused skip", out.Output)
	}
	if strings.Contains(rec.puts[0].content, `"fallback"`) {
		t.Fatalf("artifact content unexpectedly includes fallback:\n%s", rec.puts[0].content)
	}
	if strings.Contains(rec.puts[0].content, `"commands":`) {
		t.Fatalf("artifact content should not include verify fallback commands:\n%s", rec.puts[0].content)
	}
	if strings.Contains(rec.puts[0].content, `"true"`) {
		t.Fatalf("artifact content should not include verify fallback command:\n%s", rec.puts[0].content)
	}
}

func TestExecFocusedChecks_HeadBaseExcludesLocalDefaultBranchChanges(t *testing.T) {
	t.Parallel()

	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	writeRepoFile(t, wt, "frontend/src/App.svelte", "<script></script>\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: local default frontend")
	gitRun(t, wt, "checkout", "-b", "task")
	writeRepoFile(t, wt, "internal/project/model.go", "package project\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: task project")

	engine, tasks, rec := newFocusedChecksEngine(t, wt, []project.FocusedCheck{
		{
			Name:     "frontend",
			Paths:    []string{"frontend/**"},
			Commands: []string{"false"},
		},
		{
			Name:     "project",
			Paths:    []string{"internal/project/**"},
			Commands: []string{"true"},
		},
	}, nil)
	engine.checks.(*fakeCheckGetter).worktreeBaseRef = project.WorktreeBaseRefHead
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", ProjectID: "owner/repo"})

	out, err := engine.execFocusedChecks("t1", newFocusedChecksStep(), nil, TaskInfo{ID: "t1", Status: "in-progress", ProjectID: "owner/repo"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("Output = %q, want clean", out.Output)
	}
	if !strings.Contains(rec.puts[0].content, `"project"`) || strings.Contains(rec.puts[0].content, `"frontend"`) {
		t.Fatalf("artifact content selected wrong focused surface:\n%s", rec.puts[0].content)
	}
}

// TestExecFocusedChecks_HealsCorruptNodeModules proves the same toolchain
// self-heal verify_checks relies on (ensureNodeToolchain, reused via
// runVerifyCommands) also covers focused_checks — both gates route their
// commands through the shared runner, so a corrupt node_modules left by an
// interrupted install must not misattribute a broken toolchain to the diff
// under a focused (not full-suite) check either.
func TestExecFocusedChecks_HealsCorruptNodeModules(t *testing.T) {
	wt := makeBaseRepo(t, map[string]string{
		"README.md":         "init\n",
		"package.json":      "{}",
		"package-lock.json": "{}",
	})
	writeRepoFile(t, wt, "internal/workflow/model.go", "package workflow\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: touch workflow")

	// Corrupt node_modules/.bin: present but zero-byte, the exact shape a
	// killed `npm ci` in a concurrent worktree leaves behind (see
	// ensureNodeToolchain / repairCorruptToolchain).
	binDir := filepath.Join(wt, "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(binDir, "vite"), "")
	fakeNPM(t, "marker-npm-ci-ran")

	engine, tasks, _ := newFocusedChecksEngine(t, wt, []project.FocusedCheck{{
		Name:     "workflow",
		Paths:    []string{"internal/workflow/**"},
		Commands: []string{"test -f marker-npm-ci-ran"},
	}}, nil)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execFocusedChecks("t1", newFocusedChecksStep(), nil, TaskInfo{ID: "t1", Status: "in-progress"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("Output = %q, want clean (focused_checks should repair node_modules before running the check)", out.Output)
	}
	if _, err := os.Stat(filepath.Join(wt, "marker-npm-ci-ran")); err != nil {
		t.Errorf("expected npm ci repair marker in worktree root, got: %v", err)
	}
}

func TestExecFocusedChecks_FailureReasksImplement(t *testing.T) {
	t.Parallel()

	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	writeRepoFile(t, wt, "internal/workflow/model.go", "package workflow\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: touch workflow")

	engine, tasks, rec := newFocusedChecksEngine(t, wt, []project.FocusedCheck{{
		Name:     "workflow",
		Paths:    []string{"internal/workflow/**"},
		Packages: []string{"./internal/workflow/..."},
		Commands: []string{"echo boom >&2; exit 1"},
	}}, nil)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	wf := implementedExec()
	out, err := engine.execFocusedChecks("t1", newFocusedChecksStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	if out.StepID != "" || out.Status != "" {
		t.Fatalf("parked output should be zero, got %+v", out)
	}
	if wf.CurrentStep != verifyChecksImplStepID {
		t.Fatalf("CurrentStep = %q, want %q", wf.CurrentStep, verifyChecksImplStepID)
	}
	if wf.State != ExecWaiting {
		t.Fatalf("State = %q, want %q", wf.State, ExecWaiting)
	}
	note := wf.Variables[focusedChecksReaskNoteVar]
	if !strings.Contains(note, "FAILED Sybra's focused checks") || !strings.Contains(note, "workflow") || !strings.Contains(note, "internal/workflow/model.go") {
		t.Fatalf("reask note missing focused failure context:\n%s", note)
	}
	if wf.Variables[workflowRetryAfterVar] == "" {
		t.Fatalf("retry_after not set")
	}
	if wf.Variables["step.focused_checks.auto_fix"] != "1" {
		t.Fatalf("auto_fix counter = %q, want 1", wf.Variables["step.focused_checks.auto_fix"])
	}
	if ti := mustGetTaskInfo(t, tasks, "t1"); ti.Status != "in-progress" {
		t.Fatalf("status = %q, want in-progress", ti.Status)
	}
	if reason := tasks.Reason("t1"); !strings.Contains(reason, "focused checks failed for workflow") {
		t.Fatalf("reason = %q, want focused surface", reason)
	}
	if len(rec.puts) != 1 {
		t.Fatalf("artifacts = %+v, want 1 artifact", rec.puts)
	}
	t.Log(rec.puts[0].content)
	var report focusedChecksReport
	if err := json.Unmarshal([]byte(rec.puts[0].content), &report); err != nil {
		t.Fatalf("unmarshal artifact: %v", err)
	}
	if report.FailedCmd != "echo boom >&2; exit 1" {
		t.Fatalf("FailedCmd = %q", report.FailedCmd)
	}
	if !strings.Contains(report.OutputTail, "$ echo boom >&2; exit 1") || !strings.Contains(report.OutputTail, "boom") {
		t.Fatalf("artifact missing output tail:\n%s", report.OutputTail)
	}
}

func TestExecFocusedChecks_FailureReasksBelowCeiling(t *testing.T) {
	t.Parallel()

	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	writeRepoFile(t, wt, "internal/workflow/model.go", "package workflow\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: touch workflow")

	engine, tasks, _ := newFocusedChecksEngine(t, wt, []project.FocusedCheck{{
		Name:     "workflow",
		Paths:    []string{"internal/workflow/**"},
		Commands: []string{"exit 1"},
	}}, nil)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	// A code-fixable focused-check failure below the generic ceiling keeps
	// being re-asked.
	wf := implementedExec()
	wf.Variables["step.focused_checks.auto_fix"] = "2"
	out, err := engine.execFocusedChecks("t1", newFocusedChecksStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked (keep re-asking, never escalate)", err)
	}
	if out != (StepOutput{}) {
		t.Fatalf("parked output should be zero, got %+v", out)
	}
	if wf.CurrentStep != verifyChecksImplStepID || wf.State != ExecWaiting {
		t.Fatalf("workflow after rewind = %+v, want implement/ExecWaiting", wf)
	}
	if got := wf.Variables["step.focused_checks.auto_fix"]; got != "3" {
		t.Fatalf("auto_fix counter = %q, want 3", got)
	}
	if ti := mustGetTaskInfo(t, tasks, "t1"); ti.Status != "in-progress" {
		t.Fatalf("status = %q, want in-progress (never escalated to human)", ti.Status)
	}
}

func TestExecFocusedChecks_FailureCeilingEscalates(t *testing.T) {
	t.Parallel()

	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	writeRepoFile(t, wt, "internal/workflow/model.go", "package workflow\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: touch workflow")

	engine, tasks, _ := newFocusedChecksEngine(t, wt, []project.FocusedCheck{{
		Name:     "workflow",
		Paths:    []string{"internal/workflow/**"},
		Commands: []string{"exit 1"},
	}}, nil)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	// At the ceiling an unfixable focused-check failure escalates to a human.
	wf := implementedExec()
	wf.Variables["step.focused_checks.auto_fix"] = strconv.Itoa(verifyChecksAutoFixCeiling)
	out, err := engine.execFocusedChecks("t1", newFocusedChecksStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged", out.Output)
	}
	if ti := mustGetTaskInfo(t, tasks, "t1"); ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required at ceiling", ti.Status)
	}
	if reason := tasks.Reason("t1"); !strings.Contains(reason, "escalating after") {
		t.Fatalf("reason = %q, want exhaustion note", reason)
	}
}
