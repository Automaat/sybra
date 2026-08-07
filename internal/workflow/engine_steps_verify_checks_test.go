package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/buildcache"
	"github.com/Automaat/sybra/internal/evidence"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/taskstatus"
)

type fakeCheckGetter struct {
	cmds            []string
	cmdsByTask      map[string][]string
	codegen         []string
	setup           []string
	focused         []project.FocusedCheck
	worktreeBaseRef string
}

type copyingVerificationManager struct {
	root          string
	released      bool
	finalized     bool
	validateCalls int
	finalizeErr   error
}

func (m *copyingVerificationManager) PrepareVerification(_ context.Context, _, _, canonical string) (VerificationWorkspace, error) {
	dir := filepath.Join(m.root, "source")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return VerificationWorkspace{}, err
	}
	entries, err := os.ReadDir(canonical)
	if err != nil {
		return VerificationWorkspace{}, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(canonical, entry.Name()))
		if readErr != nil {
			return VerificationWorkspace{}, readErr
		}
		if writeErr := os.WriteFile(filepath.Join(dir, entry.Name()), data, 0o600); writeErr != nil {
			return VerificationWorkspace{}, writeErr
		}
	}
	return VerificationWorkspace{ID: "lease", Dir: dir, SourceSHA: "source"}, nil
}

func (m *copyingVerificationManager) FinalizeVerification(context.Context, VerificationWorkspace, []string, string) error {
	m.finalized = true
	return m.finalizeErr
}

func (m *copyingVerificationManager) ValidateVerification(context.Context, VerificationWorkspace) error {
	m.validateCalls++
	return m.finalizeErr
}

func TestValidVerificationCacheHitDoesNotRefreshBeforeFinalize(t *testing.T) {
	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	recorder := newFakeEvidenceRecorder()
	originalTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recorder.Set("t1", evidence.CompletionEvidence{Criteria: []evidence.CriterionEvidence{{
		Criterion:  evidenceCriterionVerifyChecks,
		ExitStatus: 0,
		FinalRev:   "source",
		TreeSHA:    "tree",
		ChecksHash: "checks",
		StepID:     "old-step",
		Timestamp:  originalTime,
	}}})
	engine.SetEvidenceRecorder(recorder)
	mgr := &copyingVerificationManager{finalizeErr: errors.New("source moved")}
	engine.execution.Verification = mgr

	_, hit, err := engine.validVerificationCacheHit("t1", newVerifyChecksStep(), VerificationWorkspace{ID: "lease"}, "source", "tree", "checks", []string{"true"})
	if err == nil || hit {
		t.Fatalf("cache result hit=%v err=%v, want rejected candidate", hit, err)
	}
	got, loadErr := recorder.Evidence("t1")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	entry, ok := got.ByCriterion(evidenceCriterionVerifyChecks)
	if !ok || entry.Timestamp != originalTime || entry.StepID != "old-step" {
		t.Fatalf("rejected candidate refreshed evidence: %+v", entry)
	}
}

func (m *copyingVerificationManager) ReleaseVerification(VerificationWorkspace) {
	m.released = true
	_ = os.RemoveAll(m.root)
}

func (f *fakeCheckGetter) CodegenCommands(context.Context, string) []string { return f.codegen }

func (f *fakeCheckGetter) VerifyCommands(_ context.Context, taskID string) []string {
	if f.cmdsByTask != nil {
		return f.cmdsByTask[taskID]
	}
	return f.cmds
}

func (f *fakeCheckGetter) SetupCommands(context.Context, string) []string { return f.setup }

func (f *fakeCheckGetter) FocusedChecks(context.Context, string) []project.FocusedCheck {
	return f.focused
}

func (f *fakeCheckGetter) WorktreeBaseRef(context.Context, string) string { return f.worktreeBaseRef }

func newVerifyChecksStep() *Step { return &Step{ID: "verify_checks", Type: StepVerifyChecks} }

func newVerifyChecksEngine(t *testing.T, wt string, cmds []string) (*Engine, *memTasks) {
	t.Helper()
	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wt, ok: true})
	engine.setCheckConfigGetterForTest(&fakeCheckGetter{cmds: cmds})
	return engine, engine.tasks.(*memTasks)
}

func TestExecVerifyChecks_DefaultGetterSkips(t *testing.T) {
	t.Parallel()
	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: t.TempDir(), ok: true})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "skipped: no verify commands configured" {
		t.Errorf("Output = %q, want skip", out.Output)
	}
}

func TestExecVerifyChecks_NoCommandsSkips(t *testing.T) {
	t.Parallel()
	engine, tasks := newVerifyChecksEngine(t, t.TempDir(), nil)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "skipped: no verify commands configured" {
		t.Errorf("Output = %q, want skip", out.Output)
	}
}

func TestVerifyTaskNow_NoGetterNotVerified(t *testing.T) {
	t.Parallel()
	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: t.TempDir(), ok: true})

	verified, passed, _, _, err := engine.VerifyTaskNow(t.Context(), "t1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if verified || passed {
		t.Fatalf("verified=%v passed=%v, want both false with no CheckConfigGetter", verified, passed)
	}
}

func TestVerifyTaskNow_NoCommandsNotVerified(t *testing.T) {
	t.Parallel()
	engine, _ := newVerifyChecksEngine(t, t.TempDir(), nil)

	verified, passed, _, _, err := engine.VerifyTaskNow(t.Context(), "t1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if verified || passed {
		t.Fatalf("verified=%v passed=%v, want both false with no configured commands", verified, passed)
	}
}

func TestVerifyTaskNow_NoWorktreeNotVerified(t *testing.T) {
	t.Parallel()
	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{ok: false})
	engine.setCheckConfigGetterForTest(&fakeCheckGetter{cmds: []string{"true"}})

	verified, passed, _, _, err := engine.VerifyTaskNow(t.Context(), "t1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if verified || passed {
		t.Fatalf("verified=%v passed=%v, want both false with no worktree", verified, passed)
	}
}

func TestVerifyTaskNow_CommandsPass(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, _ := newVerifyChecksEngine(t, wt, []string{"true", "echo ok"})

	verified, passed, failedCmd, _, err := engine.VerifyTaskNow(t.Context(), "t1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !verified || !passed {
		t.Fatalf("verified=%v passed=%v, want both true", verified, passed)
	}
	if failedCmd != "" {
		t.Errorf("failedCmd = %q, want empty", failedCmd)
	}
}

func TestVerifyTaskNowUsesDisposableWorkspace(t *testing.T) {
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, _ := newVerifyChecksEngine(t, wt, []string{"touch generated-by-watchdog"})
	mgr := &copyingVerificationManager{root: filepath.Join(t.TempDir(), "verification")}
	engine.execution.Verification = mgr

	verified, passed, _, output, err := engine.VerifyTaskNow(t.Context(), "t1")
	if err != nil || !verified || !passed {
		t.Fatalf("VerifyTaskNow = verified %v passed %v err %v output %q", verified, passed, err, output)
	}
	if _, err := os.Stat(filepath.Join(wt, "generated-by-watchdog")); !os.IsNotExist(err) {
		t.Fatalf("watchdog mutated authoritative checkout: %v", err)
	}
	if !mgr.finalized || !mgr.released {
		t.Fatalf("verification lifecycle finalized=%v released=%v", mgr.finalized, mgr.released)
	}
}

func TestVerifyTaskNowRunsSetupInsideDisposableWorkspace(t *testing.T) {
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, _ := newVerifyChecksEngineWithSetup(t, wt, []string{"test -f .env"}, []string{"touch .env"})
	mgr := &copyingVerificationManager{root: filepath.Join(t.TempDir(), "verification")}
	engine.execution.Verification = mgr

	verified, passed, failedCmd, output, err := engine.VerifyTaskNow(t.Context(), "t1")
	if err != nil || !verified || !passed || failedCmd != "" {
		t.Fatalf("VerifyTaskNow = verified %v passed %v failed %q err %v output %q", verified, passed, failedCmd, err, output)
	}
	if _, err := os.Stat(filepath.Join(wt, ".env")); !os.IsNotExist(err) {
		t.Fatalf("setup output escaped into authoritative checkout: %v", err)
	}
	if !mgr.finalized || !mgr.released {
		t.Fatalf("verification lifecycle finalized=%v released=%v", mgr.finalized, mgr.released)
	}
}

func TestVerifyTaskNow_CommandFails(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, _ := newVerifyChecksEngine(t, wt, []string{"true", "false"})

	verified, passed, failedCmd, output, err := engine.VerifyTaskNow(t.Context(), "t1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !verified || passed {
		t.Fatalf("verified=%v passed=%v, want verified=true passed=false", verified, passed)
	}
	if failedCmd != "false" {
		t.Errorf("failedCmd = %q, want %q", failedCmd, "false")
	}
	if !strings.Contains(output, "false") {
		t.Errorf("output = %q, want it to contain the failing command", output)
	}
}

func TestVerifyTaskNow_TimeoutFailsClosed(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, _ := newVerifyChecksEngine(t, wt, []string{"sleep 1"})
	engine.SetVerifyTimeout(50 * time.Millisecond)

	verified, passed, _, _, err := engine.VerifyTaskNow(t.Context(), "t1")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if !verified || passed {
		t.Fatalf("verified=%v passed=%v, want verified=true passed=false", verified, passed)
	}
}

func TestVerifyTaskNow_ScaledTimeoutAbsorbsHostOversubscription(t *testing.T) {
	orig := workflowCheckLoadPerCPU
	// Stubbed at the scale ceiling, not just above 1: the assertion needs the
	// scaled budget to clear `sleep 0.2` plus real process-spawn cost on a
	// machine already running the rest of the suite. At load 3 the budget was
	// 300ms against a 200ms sleep, and that 100ms margin is what made this
	// flake under `go test ./...` while passing in isolation. At the ceiling
	// the budget is 800ms, while the unscaled 100ms still fails without
	// scaling — so the test proves the same thing with 4x the headroom.
	workflowCheckLoadPerCPU = func() (float64, bool) { return verifyTimeoutScaleCeilingLoad, true }
	t.Cleanup(func() { workflowCheckLoadPerCPU = orig })

	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, _ := newVerifyChecksEngine(t, wt, []string{"sleep 0.2"})
	engine.SetVerifyTimeout(100 * time.Millisecond)

	verified, passed, failedCmd, output, err := engine.VerifyTaskNow(t.Context(), "t1")
	if err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, output)
	}
	if !verified || !passed {
		t.Fatalf("verified=%v passed=%v failedCmd=%q output=%q, want both true", verified, passed, failedCmd, output)
	}
}

func TestVerifyTaskNow_RepairsTornNodeModulesBeforeRunning(t *testing.T) {
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	frontend := filepath.Join(wt, "frontend")
	nm := filepath.Join(frontend, "node_modules")
	if err := os.MkdirAll(filepath.Join(nm, "vite"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(frontend, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(frontend, "package-lock.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	npmScript := "#!/bin/sh\nmkdir -p node_modules/.bin\ntouch node_modules/.package-lock.json\n"
	if err := os.WriteFile(filepath.Join(binDir, "npm"), []byte(npmScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cmd := "test -d frontend/node_modules/.bin"
	engine, _ := newVerifyChecksEngine(t, wt, []string{cmd})

	verified, passed, failedCmd, output, err := engine.VerifyTaskNow(t.Context(), "t1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !verified || !passed {
		t.Fatalf("verified=%v passed=%v failedCmd=%q output=%q, want both true (repair should have fixed node_modules first)",
			verified, passed, failedCmd, output)
	}
}

func TestExecVerifyChecks_PassIsClean(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, tasks := newVerifyChecksEngine(t, wt, []string{"true", "echo ok"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("Output = %q, want clean", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged", ti.Status)
	}
}

func TestExecVerifyChecks_FailureFlags(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	// First command passes, second fails — the failing one is reported.
	engine, tasks := newVerifyChecksEngine(t, wt, []string{"true", "exit 1"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required", ti.Status)
	}
	if r := tasks.Reason("t1"); r == "" {
		t.Errorf("expected a non-empty status reason")
	}
}

// TestExecVerifyChecks_CacheHitSkipsUnchangedTree is the memoization happy
// path: a second execVerifyChecks call against the same tree SHA and verify
// commands must not re-run the suite at all — the counter file (written
// outside the worktree, so it cannot itself perturb the tree SHA the second
// call computes) proves the command executed exactly once.
func TestExecVerifyChecks_CacheHitSkipsUnchangedTree(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	counter := filepath.Join(t.TempDir(), "count")
	cmd := "echo run >> " + counter
	engine, tasks := newVerifyChecksEngine(t, wt, []string{cmd})
	engine.SetEvidenceRecorder(newFakeEvidenceRecorder())
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out1, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("first run: unexpected error: %v", err)
	}
	if out1.Output != "clean" {
		t.Fatalf("first run: Output = %q, want clean", out1.Output)
	}

	out2, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("second run: unexpected error: %v", err)
	}
	if !strings.HasPrefix(out2.Output, "clean (cached:") {
		t.Fatalf("second run: Output = %q, want a cache-hit output", out2.Output)
	}

	runs, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("expected counter file to exist: %v", err)
	}
	if got := strings.Count(string(runs), "run"); got != 1 {
		t.Fatalf("verify command ran %d times, want exactly 1 (second call should have hit the cache)", got)
	}
}

// TestExecVerifyChecks_WorktreeEditForcesRerun proves a changed tree SHA
// invalidates the memo: editing a tracked file between two otherwise-identical
// execVerifyChecks calls must force the suite to run again.
func TestExecVerifyChecks_WorktreeEditForcesRerun(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	counter := filepath.Join(t.TempDir(), "count")
	cmd := "echo run >> " + counter
	engine, tasks := newVerifyChecksEngine(t, wt, []string{cmd})
	engine.SetEvidenceRecorder(newFakeEvidenceRecorder())
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	if _, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"}); err != nil {
		t.Fatalf("first run: unexpected error: %v", err)
	}

	writeRepoFile(t, wt, "README.md", "changed\n")

	out2, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("second run: unexpected error: %v", err)
	}
	if out2.Output != "clean" {
		t.Fatalf("second run: Output = %q, want a fresh clean run (not cached)", out2.Output)
	}

	runs, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("expected counter file to exist: %v", err)
	}
	if got := strings.Count(string(runs), "run"); got != 2 {
		t.Fatalf("verify command ran %d times, want exactly 2 (tree edit should have forced a re-run)", got)
	}
}

func TestExecVerifyChecks_EmptyCommitForcesRerun(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	counter := filepath.Join(t.TempDir(), "count")
	cmd := "echo run >> " + counter
	engine, tasks := newVerifyChecksEngine(t, wt, []string{cmd})
	engine.SetEvidenceRecorder(newFakeEvidenceRecorder())
	tasks.Put(TaskInfo{ID: "t1", Status: taskstatus.InProgress})

	if _, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	gitCmd := exec.Command("git", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "source identity changes")
	gitCmd.Dir = wt
	if out, err := gitCmd.CombinedOutput(); err != nil {
		t.Fatalf("empty commit: %v: %s", err, out)
	}
	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("second run output = %q, want uncached clean", out.Output)
	}
	runs, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(runs), "run"); got != 2 {
		t.Fatalf("verify command ran %d times, want 2 after source commit changed", got)
	}
}

// TestExecVerifyChecks_CommandChangeForcesRerun proves a changed verify
// commands set invalidates the memo even against the identical tree SHA.
func TestExecVerifyChecks_CommandChangeForcesRerun(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	counter := filepath.Join(t.TempDir(), "count")

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wt, ok: true})
	getter := &fakeCheckGetter{cmds: []string{"echo run >> " + counter}}
	engine.setCheckConfigGetterForTest(getter)
	engine.SetEvidenceRecorder(newFakeEvidenceRecorder())
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	if _, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"}); err != nil {
		t.Fatalf("first run: unexpected error: %v", err)
	}

	getter.cmds = []string{"echo run >> " + counter, "true"}

	out2, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("second run: unexpected error: %v", err)
	}
	if out2.Output != "clean" {
		t.Fatalf("second run: Output = %q, want a fresh clean run (not cached)", out2.Output)
	}

	runs, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("expected counter file to exist: %v", err)
	}
	if got := strings.Count(string(runs), "run"); got != 2 {
		t.Fatalf("verify command ran %d times, want exactly 2 (commands change should have forced a re-run)", got)
	}
}

func TestBoundedTail_UnderCapKeepsEverything(t *testing.T) {
	t.Parallel()
	tail := &boundedTail{max: 100}
	_, _ = io.WriteString(tail, "hello world")
	if got := tail.String(); got != "hello world" {
		t.Fatalf("String() = %q, want the untouched input", got)
	}
}

// Regression: boundedTail used to keep only the last `max` bytes, so a
// failure signal near the START of a long-running command's output (the
// realistic shape of `go test ./internal/...`: the failing package's
// `--- FAIL:` line, followed by many other packages' trailing `ok` lines)
// would be evicted before it ever reached the artifact layer, independent of
// any cap applied when persisting the artifact. Both the start and the end
// of the stream must survive; only the middle may be elided.
func TestBoundedTail_OverCapKeepsHeadAndTail(t *testing.T) {
	t.Parallel()
	tail := &boundedTail{max: 100}
	_, _ = io.WriteString(tail, "START-MARKER "+strings.Repeat("x", 500)+" END-MARKER")

	got := tail.String()
	if !strings.Contains(got, "START-MARKER") {
		t.Errorf("output lost the start of the stream: %q", got)
	}
	if !strings.Contains(got, "END-MARKER") {
		t.Errorf("output lost the end of the stream: %q", got)
	}
	if !strings.Contains(got, "elided") {
		t.Errorf("output has no elision marker despite exceeding the cap: %q", got)
	}
}

// Regression: the artifact used to cap stored output to the last 8000 bytes
// (tailString), which for a verbose failing command silently drops whatever
// ran before the cut — including, in production, the actual `--- FAIL:` line
// a human needs to diagnose the task. The stored output must never be cut.
func TestExecVerifyChecks_LongOutputNotTruncated(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, tasks := newVerifyChecksEngine(t, wt, []string{"echo MARKER_START; yes x | head -c 9000; exit 1"})
	rec := &recordingArtifactRecorder{}
	engine.SetArtifactRecorder(rec)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged", out.Output)
	}

	var report *verifyChecksReport
	for _, put := range rec.puts {
		if put.name != "verify-checks.json" {
			continue
		}
		var r verifyChecksReport
		if err := json.Unmarshal([]byte(put.content), &r); err != nil {
			t.Fatalf("unmarshal artifact: %v", err)
		}
		report = &r
	}
	if report == nil {
		t.Fatalf("no verify-checks.json artifact recorded; puts: %+v", rec.puts)
	}
	if len(report.OutputTail) < 9000 {
		t.Fatalf("OutputTail = %d bytes, want the full >9000-byte output preserved", len(report.OutputTail))
	}
	if !strings.Contains(report.OutputTail, "MARKER_START") {
		t.Errorf("artifact lost the start of the output — the old tail-only truncation would have dropped it:\n%s", report.OutputTail[:min(200, len(report.OutputTail))])
	}
}

// Regression: fixing the artifact-level 8000-byte cap is not enough on its
// own — the upstream boundedTail streaming buffer that produces `output` in
// the first place (verifyChecksMaxOutput, 64KB) used the same tail-only
// eviction strategy, so a run large enough to exceed *that* cap (realistic
// for `go test ./internal/...` across ~80 packages) would still lose the
// failure signal before the artifact layer ever saw it. This drives output
// past the 64KB streaming cap, not just the old 8KB artifact cap.
func TestExecVerifyChecks_OutputPastStreamingCapKeepsStartAndEnd(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	// 100000 alone exceeds verifyChecksMaxOutput, independent of flake retries.
	engine, tasks := newVerifyChecksEngine(t, wt,
		[]string{"echo MARKER_START; yes x | head -c 100000; echo MARKER_END; exit 1"})
	rec := &recordingArtifactRecorder{}
	engine.SetArtifactRecorder(rec)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged", out.Output)
	}

	var report *verifyChecksReport
	for _, put := range rec.puts {
		if put.name != "verify-checks.json" {
			continue
		}
		var r verifyChecksReport
		if err := json.Unmarshal([]byte(put.content), &r); err != nil {
			t.Fatalf("unmarshal artifact: %v", err)
		}
		report = &r
	}
	if report == nil {
		t.Fatalf("no verify-checks.json artifact recorded; puts: %+v", rec.puts)
	}
	if !strings.Contains(report.OutputTail, "MARKER_START") {
		t.Errorf("artifact lost the start of a >64KB run — the streaming buffer evicted it before the artifact layer ever saw it")
	}
	if !strings.Contains(report.OutputTail, "MARKER_END") {
		t.Errorf("artifact lost the end of a >64KB run")
	}
}

func TestExecVerifyChecks_BlessedTagSkips(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, tasks := newVerifyChecksEngine(t, wt, []string{"exit 1"}) // would fail
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil,
		TaskInfo{ID: "t1", Tags: []string{"verify-blessed"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "blessed" {
		t.Errorf("Output = %q, want blessed (tag short-circuits before running)", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged (blessed)", ti.Status)
	}
}

func TestExecVerifyChecks_TimeoutFailsClosed(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, tasks := newVerifyChecksEngine(t, wt, []string{"sleep 30"}) // hangs past budget
	engine.SetVerifyTimeout(150 * time.Millisecond)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged (our own timeout must fail closed)", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required (an agent could hang a test to dodge)", ti.Status)
	}
}

func TestResolveWorkflowCheckTimeout_ScalesWithHostLoad(t *testing.T) {
	orig := workflowCheckLoadPerCPU
	workflowCheckLoadPerCPU = func() (float64, bool) { return 2.4, true }
	t.Cleanup(func() { workflowCheckLoadPerCPU = orig })

	if got, want := resolveWorkflowCheckTimeout(2*time.Second), 6*time.Second; got != want {
		t.Fatalf("resolveWorkflowCheckTimeout() = %s, want %s", got, want)
	}
}

func TestExecVerifyChecks_ScaledTimeoutAbsorbsHostOversubscription(t *testing.T) {
	orig := workflowCheckLoadPerCPU
	// Ceiling load, not 3.0 — see the sibling tests: a 300ms budget against a
	// 200ms sleep left only 100ms for process spawn and flaked under suite load.
	workflowCheckLoadPerCPU = func() (float64, bool) { return verifyTimeoutScaleCeilingLoad, true }
	t.Cleanup(func() { workflowCheckLoadPerCPU = orig })

	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, tasks := newVerifyChecksEngine(t, wt, []string{"sleep 0.2"})
	engine.SetVerifyTimeout(100 * time.Millisecond)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("Output = %q, want clean (scaled timeout should cover host oversubscription)", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged", ti.Status)
	}
}

func TestExecVerifyChecks_TimeoutRetryAbsorbsLoadSpike(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	slowOnce := "test -f .timeout-marker || { touch .timeout-marker; exec sleep 10; }"
	engine, tasks := newVerifyChecksEngine(t, wt, []string{slowOnce})
	engine.SetVerifyTimeout(2 * time.Second)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("Output = %q, want clean (one-off timeout absorbed by suite retry)", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged (retry passed, no human-required)", ti.Status)
	}
}

func TestExecVerifyChecks_BackpressureParksWhilePeerVerifyInFlight(t *testing.T) {
	wt1 := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	wt2 := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	blocking := "touch .verify-entered; while [ ! -f .verify-release ]; do sleep 0.05; done"

	store := newTestStore(t)
	if err := store.Save(Definition{
		ID:   "wf2",
		Name: "verify checks retry",
		Steps: []Step{
			{ID: "verify_checks", Type: StepVerifyChecks, Next: []Transition{{GoTo: ""}}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	engine := NewTestEngine(store, newMemTasks(), newMockAgents(), slog.New(slog.NewTextHandler(&logs, nil)))
	engine.SetWorktreeGetter(&fakeWorktreeGetter{
		ok: true,
		paths: map[string]string{
			"t1": wt1,
			"t2": wt2,
		},
	})
	engine.setCheckConfigGetterForTest(&fakeCheckGetter{cmdsByTask: map[string][]string{
		"t1": {blocking},
		"t2": {"true"},
	}})
	tasks := engine.tasks.(*memTasks)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	tasks.Put(TaskInfo{ID: "t2", Status: "in-progress"})

	type result struct {
		out StepOutput
		err error
	}
	firstDone := make(chan result, 1)
	go func() {
		out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(),
			&Execution{WorkflowID: "wf1", CurrentStep: "verify_checks", State: ExecRunning, Variables: map[string]string{}},
			TaskInfo{ID: "t1", Status: "in-progress"})
		firstDone <- result{out: out, err: err}
	}()

	waitForFile := func(path string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(path); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", path)
	}
	waitForFile(filepath.Join(wt1, ".verify-entered"))

	wf2 := &Execution{WorkflowID: "wf2", CurrentStep: "verify_checks", State: ExecRunning, Variables: map[string]string{}}
	out, err := engine.execVerifyChecks("t2", newVerifyChecksStep(), wf2, TaskInfo{ID: "t2", Status: "in-progress"})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	if out != (StepOutput{}) {
		t.Fatalf("out = %+v, want zero StepOutput when parked", out)
	}
	if wf2.CurrentStep != "verify_checks" {
		t.Fatalf("CurrentStep = %q, want verify_checks", wf2.CurrentStep)
	}
	if wf2.State != ExecWaiting {
		t.Fatalf("State = %q, want ExecWaiting", wf2.State)
	}
	if _, ok := workflowRetryAfter(wf2); !ok {
		t.Fatalf("%s not set to a valid retry timestamp", workflowRetryAfterVar)
	}
	if got := tasks.Reason("t2"); got != verifyChecksBusyReason {
		t.Fatalf("reason = %q, want %q", got, verifyChecksBusyReason)
	}

	if err := os.WriteFile(filepath.Join(wt1, ".verify-release"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-firstDone:
		if got.err != nil {
			t.Fatalf("first verify err = %v", got.err)
		}
		if got.out.Output != "clean" {
			t.Fatalf("first verify output = %q, want clean", got.out.Output)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first verify to finish")
	}
	if got := len(engine.verifyChecksSlot()); got != 0 {
		t.Fatalf("verify slot still occupied after first verify finished: len=%d", got)
	}

	ti2, err := tasks.GetTask("t2")
	if err != nil {
		t.Fatal(err)
	}
	ti2.Workflow.SetVar(workflowRetryAfterVar, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))
	if err := tasks.SetWorkflow("t2", ti2.Workflow); err != nil {
		t.Fatal(err)
	}

	engine.ResumeStalled()

	ti2, err = tasks.GetTask("t2")
	if err != nil {
		t.Fatal(err)
	}
	if ti2.Workflow == nil {
		t.Fatal("workflow missing after ResumeStalled")
	}
	if ti2.Workflow.State != ExecCompleted {
		t.Fatalf("workflow state = %q, want completed - parked verify_checks was not resumed (reason=%q)\nlogs:\n%s",
			ti2.Workflow.State, tasks.Reason("t2"), logs.String())
	}
	if ti2.Workflow.CurrentStep != "" {
		t.Fatalf("CurrentStep = %q, want empty after resumed verify completes", ti2.Workflow.CurrentStep)
	}
	if got := ti2.Workflow.LastRecord(); got == nil || got.StepID != "verify_checks" || got.Output != "clean" {
		t.Fatalf("last step = %+v, want verify_checks clean", got)
	}
}

func newVerifyChecksEngineWithSetup(t *testing.T, wt string, cmds, setup []string) (*Engine, *memTasks) {
	t.Helper()
	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wt, ok: true})
	engine.setCheckConfigGetterForTest(&fakeCheckGetter{cmds: cmds, setup: setup})
	return engine, engine.tasks.(*memTasks)
}

func TestExecVerifyChecks_ToolchainHealRepairs(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	cmds := []string{`test -f .healed || { echo "vite: command not found" >&2; exit 1; }`}
	engine, tasks := newVerifyChecksEngineWithSetup(t, wt, cmds, []string{"touch .healed"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("Output = %q, want clean (setup re-run repaired the toolchain)", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged (heal succeeded)", ti.Status)
	}
}

func TestExecVerifyChecks_ToolchainHealMatchesDashNotFound(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	cmds := []string{`test -f .healed || { echo "sh: 1: vite: not found" >&2; exit 1; }`}
	engine, tasks := newVerifyChecksEngineWithSetup(t, wt, cmds, []string{"touch .healed"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("Output = %q, want clean (dash '<cmd>: not found' must trigger heal)", out.Output)
	}
}

func TestExecVerifyChecks_ToolchainHealSetupFailsEscalates(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	cmds := []string{`echo "vite: command not found" >&2; exit 1`}
	engine, tasks := newVerifyChecksEngineWithSetup(t, wt, cmds, []string{"false"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged (setup could not repair)", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required", ti.Status)
	}
}

func TestExecVerifyChecks_RealFailureSkipsToolchainHeal(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	cmds := []string{`test -f .healed || { echo "assertion failed: got 4 want 5" >&2; exit 1; }`}
	engine, tasks := newVerifyChecksEngineWithSetup(t, wt, cmds, []string{"touch .healed"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged (non-toolchain failure must not trigger setup heal)", out.Output)
	}
}

func TestExecVerifyChecks_GoInfraFailureBlocks(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, tasks := newVerifyChecksEngine(t, wt, []string{`echo "link: signal: terminated" >&2; exit 1`})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	wf := implementedExec()
	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "blocked" {
		t.Fatalf("Output = %q, want blocked", out.Output)
	}
	ti := mustGetTaskInfo(t, tasks, "t1")
	if ti.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", ti.Status)
	}
	if !strings.Contains(ti.StatusReason, "verifier infrastructure") {
		t.Fatalf("reason = %q, want verifier-infrastructure classification", ti.StatusReason)
	}
	if wf.Variables["step.verify_checks.auto_fix"] != "" {
		t.Fatalf("auto-fix counter = %q, want empty for blocked infra failure", wf.Variables["step.verify_checks.auto_fix"])
	}
}

func TestExecVerifyChecks_UnrelatedGoPackageFailureBlocks(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{
		"go.mod":                          "module example.com/verifyrepo\n\ngo 1.26.5\n",
		"internal/agent/agent.go":         "package agent\n\nfunc Ready() bool { return true }\n",
		"internal/promptlab/promptlab.go": "package promptlab\n\nfunc Title() string { return \"a\" }\n",
	})
	writeRepoFile(t, wt, "internal/promptlab/promptlab.go", "package promptlab\n\nfunc Title() string { return \"b\" }\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: change promptlab")

	cmd := `go test ./... >/dev/null 2>&1; printf '# example.com/verifyrepo/internal/agent\n--- FAIL: TestAdversarial_LiveBackgroundTaskAtResultDoesNotCompleteCleanly (60.00s)\nFAIL\texample.com/verifyrepo/internal/agent\t60.064s\nFAIL\n' >&2; exit 1`
	engine, tasks := newVerifyChecksEngine(t, wt, []string{cmd})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	wf := implementedExec()
	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "blocked" {
		t.Fatalf("Output = %q, want blocked", out.Output)
	}
	ti := mustGetTaskInfo(t, tasks, "t1")
	if ti.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", ti.Status)
	}
	if !strings.Contains(ti.StatusReason, "untouched Go package(s): internal/agent") {
		t.Fatalf("reason = %q, want untouched package classification", ti.StatusReason)
	}
	if wf.Variables["step.verify_checks.auto_fix"] != "" {
		t.Fatalf("auto-fix counter = %q, want empty for unrelated failure", wf.Variables["step.verify_checks.auto_fix"])
	}
}

func TestClassifyVerifyFailure_UntouchedLintFileIsNotCodeFixable(t *testing.T) {
	t.Parallel()
	wt := makeLintVerifyRepo(t)
	engine, _ := newVerifyChecksEngine(t, wt, nil)

	classification := engine.classifyVerifyFailure(
		"t1",
		wt,
		lintVerifyCommand("internal/bar/bar.go"),
		"internal/bar/bar.go:3:1: unnamedResult: consider giving a name to these results (gocritic)\n",
	)
	if classification != nil && classification.Kind == "code_fixable_lint" {
		t.Fatalf("classification = %+v, must not auto-fix lint in untouched file", classification)
	}
}

func TestClassifyVerifyFailure_MixedTouchedAndUntouchedLintFilesNotCodeFixable(t *testing.T) {
	t.Parallel()
	wt := makeLintVerifyRepo(t)
	engine, _ := newVerifyChecksEngine(t, wt, nil)

	classification := engine.classifyVerifyFailure(
		"t1",
		wt,
		"golangci-lint run ./internal/...",
		lintVerifyOutput("internal/foo/foo.go", "internal/bar/bar.go"),
	)
	if classification != nil && classification.Kind == "code_fixable_lint" {
		t.Fatalf("classification = %+v, must not auto-fix mixed touched/untouched lint files", classification)
	}
}

func TestExecVerifyChecks_DependentGoPackageFailureStillAutoFixes(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{
		"go.mod":                    "module example.com/verifyrepo\n\ngo 1.26.5\n",
		"internal/shared/shared.go": "package shared\n\nfunc Value() int { return 1 }\n",
		"internal/agent/agent.go":   "package agent\n\nimport \"example.com/verifyrepo/internal/shared\"\n\nfunc Ready() int { return shared.Value() }\n",
	})
	writeRepoFile(t, wt, "internal/shared/shared.go", "package shared\n\nfunc Value() int { return 2 }\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: change shared")

	cmd := `go test ./... >/dev/null 2>&1; printf '# example.com/verifyrepo/internal/agent\nFAIL\texample.com/verifyrepo/internal/agent [build failed]\nFAIL\n' >&2; exit 1`
	engine, tasks := newVerifyChecksEngine(t, wt, []string{cmd})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	wf := implementedExec()
	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	if out != (StepOutput{}) {
		t.Fatalf("parked output = %+v, want zero value", out)
	}
	ti := mustGetTaskInfo(t, tasks, "t1")
	if ti.Status != "in-progress" {
		t.Fatalf("status = %q, want in-progress (related package failure should auto-fix)", ti.Status)
	}
	if wf.Variables["step.verify_checks.auto_fix"] != "1" {
		t.Fatalf("auto-fix counter = %q, want 1", wf.Variables["step.verify_checks.auto_fix"])
	}
	if got := wf.Variables[verifyRetryModelVar]; got != "expensive" {
		t.Fatalf("%s = %q, want expensive", verifyRetryModelVar, got)
	}
}

func TestExecVerifyChecks_FlakeRetryPasses(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	// Fails the first run, succeeds on retry (the marker persists in the
	// worktree between attempts) — a nondeterministic flake must not block.
	flaky := "test -f .flake-marker || { touch .flake-marker; exit 1; }"
	engine, tasks := newVerifyChecksEngine(t, wt, []string{flaky})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("Output = %q, want clean (flake absorbed by retry)", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged (retry passed)", ti.Status)
	}
}

func TestRunVerifyCommands_DeadlineReturnsCtxErr(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	failed, _, err := engine.runVerifyCommands(ctx, "t1", wt, []string{"sleep 20"})
	if err == nil {
		t.Fatal("expected a context error on deadline, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if failed != "" {
		t.Errorf("failedCmd = %q, want empty (deadline is not a command failure)", failed)
	}
}

func TestRunVerifyCommands_GOCACHEIsPerTaskWhileGOMODCACHEStaysShared(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	ctx := context.Background()
	cmd := `printf '%s|%s' "$GOCACHE" "$GOMODCACHE"`

	_, out1, err := engine.runVerifyCommands(ctx, "task-1", wt, []string{cmd})
	if err != nil {
		t.Fatalf("runVerifyCommands(task-1): %v", err)
	}
	_, out2, err := engine.runVerifyCommands(ctx, "task-2", wt, []string{cmd})
	if err != nil {
		t.Fatalf("runVerifyCommands(task-2): %v", err)
	}

	line1 := strings.TrimSpace(strings.TrimPrefix(out1, "$ "+cmd))
	line2 := strings.TrimSpace(strings.TrimPrefix(out2, "$ "+cmd))
	parts1 := strings.Split(line1, "|")
	parts2 := strings.Split(line2, "|")
	if len(parts1) != 2 || len(parts2) != 2 {
		t.Fatalf("unexpected output: %q / %q", out1, out2)
	}
	if got, want := parts1[0], buildcache.TaskGoBuildDir("task-1"); got != want {
		t.Fatalf("task-1 GOCACHE = %q, want %q", got, want)
	}
	if got, want := parts2[0], buildcache.TaskGoBuildDir("task-2"); got != want {
		t.Fatalf("task-2 GOCACHE = %q, want %q", got, want)
	}
	if parts1[0] == parts2[0] {
		t.Fatalf("GOCACHE must differ per task, got shared %q", parts1[0])
	}
	if got, want := parts1[1], buildcache.SharedGoModDir(); got != want {
		t.Fatalf("task-1 GOMODCACHE = %q, want %q", got, want)
	}
	if got, want := parts2[1], buildcache.SharedGoModDir(); got != want {
		t.Fatalf("task-2 GOMODCACHE = %q, want %q", got, want)
	}
}

func TestDiscoverEnvtestPlan_UsesExplicitVersionAndControllerRuntimeRelease(t *testing.T) {
	wt := t.TempDir()
	writeTestFile(t, filepath.Join(wt, "go.mod"), `module example.com/operator

go 1.26

require (
	k8s.io/api v0.32.4
	sigs.k8s.io/controller-runtime v0.20.3
)
`)
	writeTestFile(t, filepath.Join(wt, "Makefile"), "ENVTEST_K8S_VERSION ?= 1.31\n")
	writeTestFile(t, filepath.Join(wt, "internal", "controller", "suite_test.go"), `package controller

import "sigs.k8s.io/controller-runtime/pkg/envtest"
`)

	plan, err := discoverEnvtestPlan(wt)
	if err != nil {
		t.Fatalf("discoverEnvtestPlan: %v", err)
	}
	if !plan.Needed {
		t.Fatal("Needed = false, want true")
	}
	if got, want := plan.VersionSpec, "1.31"; got != want {
		t.Fatalf("VersionSpec = %q, want %q", got, want)
	}
	if got, want := plan.SetupEnvtestRef, "release-0.20"; got != want {
		t.Fatalf("SetupEnvtestRef = %q, want %q", got, want)
	}
}

func TestDiscoverEnvtestPlan_IgnoresToolingMentionsWithoutEnvtestImport(t *testing.T) {
	wt := t.TempDir()
	writeTestFile(t, filepath.Join(wt, "go.mod"), `module example.com/operator

go 1.26

require (
	k8s.io/api v0.32.4
	sigs.k8s.io/controller-runtime v0.20.3
)
`)
	writeTestFile(t, filepath.Join(wt, ".github", "workflows", "ci.yml"), `name: ci
jobs:
  test:
    steps:
      - run: echo KUBEBUILDER_ASSETS setup-envtest
`)
	writeTestFile(t, filepath.Join(wt, "hack", "envtest.sh"), `#!/bin/sh
echo "setup-envtest use 1.31"
`)
	writeTestFile(t, filepath.Join(wt, "internal", "controller", "suite_test.go"), `package controller

func TestUnit(t *testing.T) {}
`)

	plan, err := discoverEnvtestPlan(wt)
	if err != nil {
		t.Fatalf("discoverEnvtestPlan: %v", err)
	}
	if plan.Needed {
		t.Fatal("Needed = true, want false")
	}
	if got, want := plan.VersionSpec, "1.31"; got != want {
		t.Fatalf("VersionSpec = %q, want %q", got, want)
	}
	if got, want := plan.SetupEnvtestRef, "release-0.20"; got != want {
		t.Fatalf("SetupEnvtestRef = %q, want %q", got, want)
	}
}

func TestDiscoverEnvtestPlan_SkipsOversizeFiles(t *testing.T) {
	wt := t.TempDir()
	writeTestFile(t, filepath.Join(wt, "go.mod"), `module example.com/operator

go 1.26

require (
	k8s.io/api v0.32.4
	sigs.k8s.io/controller-runtime v0.20.3
)
`)
	writeTestFile(t, filepath.Join(wt, "internal", "controller", "suite_test.go"), `package controller

import "sigs.k8s.io/controller-runtime/pkg/envtest"
`)
	writeTestFile(t, filepath.Join(wt, "hack", "envtest.sh"), "ENVTEST_K8S_VERSION=1.31\n")
	oversize := strings.Repeat("# filler\n", maxEnvtestInspectFileSize/8+1)
	writeTestFile(t, filepath.Join(wt, ".github", "workflows", "generated.yml"), oversize+"ENVTEST_K8S_VERSION=1.99\n")

	plan, err := discoverEnvtestPlan(wt)
	if err != nil {
		t.Fatalf("discoverEnvtestPlan: %v", err)
	}
	if !plan.Needed {
		t.Fatal("Needed = false, want true")
	}
	if got, want := plan.VersionSpec, "1.31"; got != want {
		t.Fatalf("VersionSpec = %q, want %q", got, want)
	}
}

func TestVerifyCommandEnv_InjectsEnvtestAssetsForGoCommands(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
	wt := t.TempDir()
	writeTestFile(t, filepath.Join(wt, "go.mod"), `module example.com/operator

go 1.26

require (
	k8s.io/api v0.32.4
	sigs.k8s.io/controller-runtime v0.20.3
)
`)
	writeTestFile(t, filepath.Join(wt, "internal", "controller", "suite_test.go"), `package controller

import "sigs.k8s.io/controller-runtime/pkg/envtest"
`)

	assetsDir := filepath.Join(t.TempDir(), "kubebuilder", "bin")
	argsLog := fakeSetupEnvtest(t, assetsDir)
	env, err := verifyCommandEnv(context.Background(), "t1", wt, "mise exec -- go test ./...")
	if err != nil {
		t.Fatalf("verifyCommandEnv: %v", err)
	}
	if got := envValue(env, "KUBEBUILDER_ASSETS"); got != assetsDir {
		t.Fatalf("KUBEBUILDER_ASSETS = %q, want %q", got, assetsDir)
	}
	args, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("read args log: %v", err)
	}
	if got := strings.TrimSpace(string(args)); got != "use -p path 1.32.x!" {
		t.Fatalf("setup-envtest args = %q, want %q", got, "use -p path 1.32.x!")
	}
}

func TestGoPackageAffectedByChanges_SkipsEnvtestProvisionForGoList(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
	wt := t.TempDir()
	writeTestFile(t, filepath.Join(wt, "go.mod"), `module example.com/operator

go 1.26

require (
	k8s.io/api v0.32.4
	sigs.k8s.io/controller-runtime v0.20.3
)
`)
	writeTestFile(t, filepath.Join(wt, "internal", "controller", "suite_test.go"), `package controller

import "sigs.k8s.io/controller-runtime/pkg/envtest"
`)
	argsLog := fakeSetupEnvtest(t, filepath.Join(t.TempDir(), "kubebuilder", "bin"))

	affected, err := goPackageAffectedByChanges(context.Background(), "t1", wt, "internal/controller", map[string]bool{
		"internal/other": true,
	}, "example.com/operator")
	if err != nil {
		t.Fatalf("goPackageAffectedByChanges: %v", err)
	}
	if affected {
		t.Fatal("affected = true, want false")
	}
	if _, err := os.Stat(argsLog); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup-envtest should not run, stat err = %v", err)
	}
}

func TestVerifyCommandEnv_InjectsEnvtestAssetsForGoListCommands(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
	wt := t.TempDir()
	writeTestFile(t, filepath.Join(wt, "go.mod"), `module example.com/operator

go 1.26

require (
	k8s.io/api v0.32.4
	sigs.k8s.io/controller-runtime v0.20.3
)
`)
	writeTestFile(t, filepath.Join(wt, "internal", "controller", "suite_test.go"), `package controller

import "sigs.k8s.io/controller-runtime/pkg/envtest"
`)

	assetsDir := filepath.Join(t.TempDir(), "kubebuilder", "bin")
	argsLog := fakeSetupEnvtest(t, assetsDir)
	env, err := verifyCommandEnv(context.Background(), "t1", wt, "go list -deps ./internal/controller")
	if err != nil {
		t.Fatalf("verifyCommandEnv: %v", err)
	}
	if got := envValue(env, "KUBEBUILDER_ASSETS"); got != assetsDir {
		t.Fatalf("KUBEBUILDER_ASSETS = %q, want %q", got, assetsDir)
	}
	args, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("read args log: %v", err)
	}
	if got := strings.TrimSpace(string(args)); got != "use -p path 1.32.x!" {
		t.Fatalf("setup-envtest args = %q, want %q", got, "use -p path 1.32.x!")
	}
}

func TestVerifyCommandEnv_SkipsEnvtestForNonGoCommands(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
	wt := t.TempDir()
	writeTestFile(t, filepath.Join(wt, "go.mod"), `module example.com/operator

go 1.26

require (
	k8s.io/api v0.32.4
	sigs.k8s.io/controller-runtime v0.20.3
)
`)
	writeTestFile(t, filepath.Join(wt, "internal", "controller", "suite_test.go"), `package controller

import "sigs.k8s.io/controller-runtime/pkg/envtest"
`)

	argsLog := fakeSetupEnvtest(t, filepath.Join(t.TempDir(), "kubebuilder", "bin"))
	env, err := verifyCommandEnv(context.Background(), "t1", wt, "(cd frontend && npm run check)")
	if err != nil {
		t.Fatalf("verifyCommandEnv: %v", err)
	}
	if got := envValue(env, "KUBEBUILDER_ASSETS"); got != "" {
		t.Fatalf("KUBEBUILDER_ASSETS = %q, want empty", got)
	}
	if _, err := os.Stat(argsLog); err == nil {
		t.Fatal("setup-envtest should not run for non-Go commands")
	}
}

func TestEnsureNodeToolchain_RepairsCorruptBin(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "package.json"), "{}")
	binDir := filepath.Join(dir, "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Present but zero-byte — the exact corruption shape from the bug report
	// (ls lists entries, du -sh reports 0 bytes).
	writeTestFile(t, filepath.Join(binDir, "vite"), "")

	fakeNPM(t, "marker-npm-ci-ran")

	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	tail := &boundedTail{max: 4096}
	engine.ensureNodeToolchain(context.Background(), "t1", dir, "npm run build:web", tail)

	if _, err := os.Stat(filepath.Join(dir, "marker-npm-ci-ran")); err != nil {
		t.Errorf("expected npm ci to have run and left a marker, got: %v", err)
	}
	if out := tail.String(); !strings.Contains(out, "corrupt") || !strings.Contains(out, "repair completed") {
		t.Errorf("tail output = %q, want corrupt+repair-completed messages", out)
	}
}

func TestEnsureNodeToolchain_RepairsMissingBin(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "package.json"), "{}")
	// node_modules/.bin never created — a truncated install can wipe out the
	// whole directory, not just leave zero-byte entries behind.

	fakeNPM(t, "marker-npm-ci-ran")

	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	tail := &boundedTail{max: 4096}
	engine.ensureNodeToolchain(context.Background(), "t1", dir, "npm run build:web", tail)

	if _, err := os.Stat(filepath.Join(dir, "marker-npm-ci-ran")); err != nil {
		t.Errorf("expected npm ci to have run for a missing node_modules/.bin, got: %v", err)
	}
}

func TestEnsureNodeToolchain_IntactBinSkipsRepair(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "package.json"), "{}")
	binDir := filepath.Join(dir, "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(binDir, "vite"), "#!/bin/sh\necho vite\n")

	fakeNPM(t, "marker-npm-ci-ran")

	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	tail := &boundedTail{max: 4096}
	engine.ensureNodeToolchain(context.Background(), "t1", dir, "npm run build:web", tail)

	if _, err := os.Stat(filepath.Join(dir, "marker-npm-ci-ran")); err == nil {
		t.Error("npm ci should not have run — toolchain looked intact")
	}
}

// TestEnsureNodeToolchain_ConcurrentCallersInstallOnce covers the fan-out
// hazard execParallelGates introduced: focused_checks and verify_checks run
// concurrently against the SAME worktree, and an absent node_modules is left
// alone by the pre-fan-out repair (never installed is not corruption), so both
// gates can reach this repair path for one directory. Two `npm ci` runs each
// delete and rewrite node_modules, tearing the install the other is about to
// use. The repair must serialize per directory and re-check under the lock, so
// exactly one install runs.
func TestEnsureNodeToolchain_ConcurrentCallersInstallOnce(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "package.json"), "{}")
	// node_modules/.bin absent: the "never installed" shape both gates see.

	runLog := filepath.Join(t.TempDir(), "npm-runs.log")
	fakeBin := t.TempDir()
	script := "#!/bin/sh\n" +
		"echo run >> " + runLog + "\n" +
		"sleep 0.2\n" +
		"mkdir -p node_modules/.bin\n" +
		"printf '#!/bin/sh\\n' > node_modules/.bin/vite\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "npm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())

	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			engine.ensureNodeToolchain(context.Background(), "t1", dir, "npm run build:web", &boundedTail{max: 4096})
		})
	}
	wg.Wait()

	data, err := os.ReadFile(runLog)
	if err != nil {
		t.Fatalf("npm never ran: %v", err)
	}
	if runs := strings.Count(string(data), "run"); runs != 1 {
		t.Fatalf("npm ci ran %d times, want exactly 1 — concurrent gates must not race two installs", runs)
	}
}

func TestEnsureNodeToolchain_ResolvesCdPrefix(t *testing.T) {
	root := t.TempDir()
	frontend := filepath.Join(root, "frontend")
	if err := os.MkdirAll(frontend, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(frontend, "package.json"), "{}")
	binDir := filepath.Join(frontend, "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(binDir, "vite"), "")

	fakeNPM(t, "marker-npm-ci-ran")

	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	tail := &boundedTail{max: 4096}
	engine.ensureNodeToolchain(context.Background(), "t1", root, "(cd frontend && mise exec -- npm run build:web)", tail)

	if _, err := os.Stat(filepath.Join(frontend, "marker-npm-ci-ran")); err != nil {
		t.Errorf("expected npm ci to run in the cd-resolved frontend/ dir, got: %v", err)
	}
}

func TestEnsureNodeToolchain_CdSubstringIsNotAFalseMatch(t *testing.T) {
	// `test:cd` must not match the cd-prefix pattern and resolve a bogus
	// `main` subdirectory — the worktree root is the only dir checked.
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "package.json"), "{}")
	binDir := filepath.Join(root, "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(binDir, "vite"), "#!/bin/sh\necho vite\n") // intact
	fakeNPM(t, "marker-npm-ci-ran")

	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	tail := &boundedTail{max: 4096}
	engine.ensureNodeToolchain(context.Background(), "t1", root, "npm run test:cd main", tail)

	if _, err := os.Stat(filepath.Join(root, "marker-npm-ci-ran")); err == nil {
		t.Error("npm ci should not have run — root toolchain is intact and `test:cd` is not a cd prefix")
	}
	if _, err := os.Stat(filepath.Join(root, "main")); err == nil {
		t.Error("a bogus `main` dir must never be created")
	}
}

func TestEnsureNodeToolchain_QuotedDirWithSpace(t *testing.T) {
	root := t.TempDir()
	spaced := filepath.Join(root, "my dir")
	binDir := filepath.Join(spaced, "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(spaced, "package.json"), "{}")
	writeTestFile(t, filepath.Join(binDir, "vite"), "") // corrupt
	fakeNPM(t, "marker-npm-ci-ran")

	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	tail := &boundedTail{max: 4096}
	engine.ensureNodeToolchain(context.Background(), "t1", root, `cd "my dir" && npm run build`, tail)

	if _, err := os.Stat(filepath.Join(spaced, "marker-npm-ci-ran")); err != nil {
		t.Errorf("expected npm ci to run in the quoted spaced dir, got: %v", err)
	}
}

func TestEnsureNodeToolchain_ChainedCdRepairsEachLeg(t *testing.T) {
	root := t.TempDir()
	for _, leg := range []string{"frontend", "backend"} {
		binDir := filepath.Join(root, leg, "node_modules", ".bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(root, leg, "package.json"), "{}")
		writeTestFile(t, filepath.Join(binDir, "vite"), "") // corrupt in both
	}
	// Shared fake npm on PATH; it drops the marker in whichever cwd it runs.
	fakeNPM(t, "marker-npm-ci-ran")

	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	tail := &boundedTail{max: 4096}
	engine.ensureNodeToolchain(context.Background(), "t1", root,
		"(cd frontend && npm run build) && (cd backend && npm test)", tail)

	for _, leg := range []string{"frontend", "backend"} {
		if _, err := os.Stat(filepath.Join(root, leg, "marker-npm-ci-ran")); err != nil {
			t.Errorf("expected npm ci to run in %s, got: %v", leg, err)
		}
	}
}

func TestEnsureNodeToolchain_TraversalIsRejected(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	binDir := filepath.Join(outside, "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(outside, "package.json"), "{}")
	writeTestFile(t, filepath.Join(binDir, "vite"), "") // corrupt
	fakeNPM(t, "marker-npm-ci-ran")

	wt := filepath.Join(root, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	tail := &boundedTail{max: 4096}
	engine.ensureNodeToolchain(context.Background(), "t1", wt, "cd ../outside && npm run build", tail)

	if _, err := os.Stat(filepath.Join(outside, "marker-npm-ci-ran")); err == nil {
		t.Error("npm ci must not run outside the worktree via a `..` traversal")
	}
}

func TestEnsureNodeToolchain_NonNodeDirSkips(t *testing.T) {
	dir := t.TempDir() // no package.json
	fakeNPM(t, "marker-npm-ci-ran")

	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	tail := &boundedTail{max: 4096}
	engine.ensureNodeToolchain(context.Background(), "t1", dir, "go test ./...", tail)

	if _, err := os.Stat(filepath.Join(dir, "marker-npm-ci-ran")); err == nil {
		t.Error("npm ci should not have run for a non-Node directory")
	}
}

// fakeNPM prepends a fake `npm` executable onto PATH for the duration of the
// test, so ensureNodeToolchain's `sh -c "npm ci"` is observable without
// depending on a real npm install. Running it drops a marker file named
// markerName into its working directory.
func fakeNPM(t *testing.T, markerName string) {
	t.Helper()
	fakeBin := t.TempDir()
	script := "#!/bin/sh\ntouch \"" + markerName + "\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "npm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func fakeSetupEnvtest(t *testing.T, assetsDir string) string {
	t.Helper()
	fakeBin := t.TempDir()
	argsLog := filepath.Join(t.TempDir(), "setup-envtest.args")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' \"$*\" > %q\nprintf '%%s\\n' %q\n", argsLog, assetsDir)
	if err := os.WriteFile(filepath.Join(fakeBin, "setup-envtest"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsLog
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIsCorruptedNodeModules(t *testing.T) {
	t.Parallel()

	newProject := func(t *testing.T, withPackageJSON bool, lockfiles []string, nmEntries map[string]string) string {
		t.Helper()
		dir := t.TempDir()
		if withPackageJSON {
			if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for _, name := range lockfiles {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if nmEntries != nil {
			nm := filepath.Join(dir, "node_modules")
			if err := os.MkdirAll(nm, 0o755); err != nil {
				t.Fatal(err)
			}
			for name, content := range nmEntries {
				full := filepath.Join(nm, name)
				if content == "<dir>" {
					if err := os.MkdirAll(full, 0o755); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		return dir
	}

	tests := []struct {
		name            string
		withPackageJSON bool
		lockfiles       []string
		nmEntries       map[string]string
		want            bool
	}{
		{
			name:            "no package.json",
			withPackageJSON: false,
			lockfiles:       []string{"package-lock.json"},
			nmEntries:       map[string]string{"vite": "<dir>"},
			want:            false,
		},
		{
			name:            "no node_modules at all (not yet installed)",
			withPackageJSON: true,
			lockfiles:       []string{"package-lock.json"},
			nmEntries:       nil,
			want:            false,
		},
		{
			name:            "empty node_modules",
			withPackageJSON: true,
			lockfiles:       []string{"package-lock.json"},
			nmEntries:       map[string]string{},
			want:            false,
		},
		{
			name:            "missing .bin — killed npm ci",
			withPackageJSON: true,
			lockfiles:       []string{"package-lock.json"},
			nmEntries:       map[string]string{"vite": "<dir>", ".package-lock.json": "{}"},
			want:            true,
		},
		{
			name:            "missing .package-lock.json — killed npm ci",
			withPackageJSON: true,
			lockfiles:       []string{"package-lock.json"},
			nmEntries:       map[string]string{"vite": "<dir>", ".bin": "<dir>"},
			want:            true,
		},
		{
			name:            "npm-shrinkwrap.json counts as npm lockfile",
			withPackageJSON: true,
			lockfiles:       []string{"npm-shrinkwrap.json"},
			nmEntries:       map[string]string{"vite": "<dir>", ".bin": "<dir>"},
			want:            true,
		},
		{
			name:            "complete install",
			withPackageJSON: true,
			lockfiles:       []string{"package-lock.json"},
			nmEntries:       map[string]string{"vite": "<dir>", ".bin": "<dir>", ".package-lock.json": "{}"},
			want:            false,
		},
		{
			name:            "no npm lockfile — not npm-owned",
			withPackageJSON: true,
			lockfiles:       nil,
			nmEntries:       map[string]string{"vite": "<dir>", ".bin": "<dir>"},
			want:            false,
		},
		{
			name:            "pnpm workspace — another package manager owns install",
			withPackageJSON: true,
			lockfiles:       []string{"pnpm-lock.yaml"},
			nmEntries:       map[string]string{"vite": "<dir>"},
			want:            false,
		},
		{
			name:            "yarn workspace — another package manager owns install",
			withPackageJSON: true,
			lockfiles:       []string{"yarn.lock"},
			nmEntries:       map[string]string{"vite": "<dir>"},
			want:            false,
		},
		{
			name:            "bun workspace — another package manager owns install",
			withPackageJSON: true,
			lockfiles:       []string{"bun.lockb"},
			nmEntries:       map[string]string{"vite": "<dir>"},
			want:            false,
		},
		{
			name:            "pnpm lockfile wins over stray npm lockfile",
			withPackageJSON: true,
			lockfiles:       []string{"package-lock.json", "pnpm-lock.yaml"},
			nmEntries:       map[string]string{"vite": "<dir>"},
			want:            false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := newProject(t, tt.withPackageJSON, tt.lockfiles, tt.nmEntries)
			if got := isCorruptedNodeModules(dir); got != tt.want {
				t.Errorf("isCorruptedNodeModules(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestRepairCorruptedNodeModules_RepairsPartialInstall(t *testing.T) {
	wt := t.TempDir()
	frontend := filepath.Join(wt, "frontend")
	nm := filepath.Join(frontend, "node_modules")
	if err := os.MkdirAll(filepath.Join(nm, "vite"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(frontend, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(frontend, "package-lock.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fake `npm` on PATH standing in for a real install: `npm ci` writes the
	// two files whose absence signals a killed install, into the invoking
	// directory ($PWD, i.e. cmd.Dir).
	binDir := t.TempDir()
	npmScript := "#!/bin/sh\nmkdir -p node_modules/.bin\ntouch node_modules/.package-lock.json\n"
	if err := os.WriteFile(filepath.Join(binDir, "npm"), []byte(npmScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.repairCorruptedNodeModules(context.Background(), "t1", wt)

	if _, err := os.Stat(filepath.Join(nm, ".bin")); err != nil {
		t.Errorf("expected node_modules/.bin to be repaired: %v", err)
	}
	if _, err := os.Stat(filepath.Join(nm, ".package-lock.json")); err != nil {
		t.Errorf("expected node_modules/.package-lock.json to be repaired: %v", err)
	}
}

func TestExecVerifyChecks_NodeModulesRepairFailureFlags(t *testing.T) {
	wt := t.TempDir()
	frontend := filepath.Join(wt, "frontend")
	nm := filepath.Join(frontend, "node_modules")
	if err := os.MkdirAll(filepath.Join(nm, "vite"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(frontend, "package.json"), "{}")
	writeTestFile(t, filepath.Join(frontend, "package-lock.json"), "{}")

	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "npm"), []byte("#!/bin/sh\nexit 42\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	engine, tasks := newVerifyChecksEngine(t, wt, []string{"true"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", ti.Status)
	}
	if got := tasks.Reason("t1"); !strings.Contains(got, "could not repair corrupted node_modules") {
		t.Fatalf("reason = %q, want node_modules repair failure", got)
	}
}

func TestVerifyTaskNow_NodeModulesRepairFailureIsNotVerified(t *testing.T) {
	wt := t.TempDir()
	frontend := filepath.Join(wt, "frontend")
	nm := filepath.Join(frontend, "node_modules")
	if err := os.MkdirAll(filepath.Join(nm, "vite"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(frontend, "package.json"), "{}")
	writeTestFile(t, filepath.Join(frontend, "package-lock.json"), "{}")

	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "npm"), []byte("#!/bin/sh\nexit 42\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	engine, _ := newVerifyChecksEngine(t, wt, []string{"true"})
	verified, passed, _, output, err := engine.VerifyTaskNow(t.Context(), "t1")
	if err == nil {
		t.Fatal("expected repair error")
	}
	if verified || passed {
		t.Fatalf("verified=%v passed=%v, want verified=false passed=false", verified, passed)
	}
	if output != "node_modules repair failed" {
		t.Fatalf("output = %q, want node_modules repair failed", output)
	}
}

func TestRepairCorruptedNodeModules_UsesSubdirMiseConfig(t *testing.T) {
	wt := t.TempDir()
	frontend := filepath.Join(wt, "frontend")
	nm := filepath.Join(frontend, "node_modules")
	if err := os.MkdirAll(filepath.Join(nm, "vite"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(frontend, "package.json"), "{}")
	writeTestFile(t, filepath.Join(frontend, "package-lock.json"), "{}")
	writeTestFile(t, filepath.Join(frontend, "mise.toml"), "[tools]\nnode = \"24\"\n")

	fakeBin := t.TempDir()
	miseScript := "#!/bin/sh\n" +
		"test \"$1\" = exec || exit 41\n" +
		"test \"$2\" = -- || exit 42\n" +
		"shift 2\n" +
		"FROM_MISE=1 exec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "mise"), []byte(miseScript), 0o755); err != nil {
		t.Fatal(err)
	}
	npmScript := "#!/bin/sh\n" +
		"test \"$FROM_MISE\" = 1 || exit 43\n" +
		"mkdir -p node_modules/.bin\n" +
		"touch node_modules/.package-lock.json\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "npm"), []byte(npmScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	if failed := engine.repairCorruptedNodeModules(context.Background(), "t1", wt); failed {
		t.Fatal("repairCorruptedNodeModules reported failure; want subdir mise config to drive npm repair")
	}
	if _, err := os.Stat(filepath.Join(frontend, "node_modules", ".bin")); err != nil {
		t.Fatalf("node_modules/.bin missing after repair: %v", err)
	}
	if _, err := os.Stat(filepath.Join(frontend, "node_modules", ".package-lock.json")); err != nil {
		t.Fatalf("node_modules/.package-lock.json missing after repair: %v", err)
	}
}

func TestRepairCorruptedNodeModules_LeavesHealthyInstallAlone(t *testing.T) {
	wt := t.TempDir()
	frontend := filepath.Join(wt, "frontend")
	nm := filepath.Join(frontend, "node_modules")
	if err := os.MkdirAll(filepath.Join(nm, ".bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nm, ".package-lock.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(frontend, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	// No fake npm on PATH — if the engine tried to repair a healthy install
	// it would fail loudly (npm ci would error or hit the network); a
	// successful, silent no-op proves the healthy install was left alone.
	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.repairCorruptedNodeModules(context.Background(), "t1", wt)
}

func TestRepairCorruptedNodeModules_SkipsPnpmWorkspace(t *testing.T) {
	wt := t.TempDir()
	frontend := filepath.Join(wt, "frontend")
	nm := filepath.Join(frontend, "node_modules")
	// A pnpm workspace: non-empty node_modules missing .package-lock.json —
	// which looks "corrupted" to the npm heuristic but is legitimate here.
	if err := os.MkdirAll(filepath.Join(nm, "vite"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(frontend, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(frontend, "pnpm-lock.yaml"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fake `npm` that fails loudly if invoked — the repair must not run `npm ci`
	// against a pnpm-owned install.
	binDir := t.TempDir()
	npmScript := "#!/bin/sh\ntouch invoked\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "npm"), []byte(npmScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.repairCorruptedNodeModules(context.Background(), "t1", wt)

	if _, err := os.Stat(filepath.Join(frontend, "invoked")); err == nil {
		t.Error("npm ci was invoked against a pnpm workspace")
	}
}

func TestExecVerifyChecks_RepairsCorruptedNodeModulesBeforeRunning(t *testing.T) {
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	frontend := filepath.Join(wt, "frontend")
	nm := filepath.Join(frontend, "node_modules")
	if err := os.MkdirAll(filepath.Join(nm, "vite"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(frontend, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(frontend, "package-lock.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	npmScript := "#!/bin/sh\nmkdir -p node_modules/.bin\ntouch node_modules/.package-lock.json\n"
	if err := os.WriteFile(filepath.Join(binDir, "npm"), []byte(npmScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The verify command only passes once node_modules/.bin exists — proving
	// the repair ran before the command, not after.
	cmd := "test -d frontend/node_modules/.bin"
	engine, tasks := newVerifyChecksEngine(t, wt, []string{cmd})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("Output = %q, want clean (repair should have fixed node_modules first)", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged", ti.Status)
	}
}

// TestBuiltinSimpleTaskImplement_VerifyChecksWiring pins verify_checks'
// contribution to simple-task-implement now that it runs inside the
// parallel_gates coordinator (see execParallelGates,
// engine_steps_parallel_gates.go) alongside detect_tampering and
// focused_checks rather than as its own serial step: a blocked or flagged
// outcome still ends the workflow, and a clean one still proceeds to
// set_ready_review.
func TestBuiltinSimpleTaskImplement_VerifyChecksWiring(t *testing.T) {
	t.Parallel()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var impl *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-implement" {
			impl = &defs[i]
			break
		}
	}
	if impl == nil {
		t.Fatal("simple-task-implement builtin not found")
		return
	}
	gates := impl.StepByID("parallel_gates")
	if gates == nil {
		t.Fatal("parallel_gates step missing from simple-task-implement")
		return
	}
	if gates.Type != StepParallelGates {
		t.Errorf("parallel_gates type = %q, want %q", gates.Type, StepParallelGates)
	}
	codegen := impl.StepByID("codegen_gate")
	if codegen == nil {
		t.Fatal("codegen_gate step missing")
		return
	}
	if got, _ := ResolveTransition(codegen.Next, map[string]string{"task.status": "in-progress"}); got != "parallel_gates" {
		t.Errorf("codegen_gate clean goto = %q, want parallel_gates", got)
	}
	if got, _ := ResolveTransition(gates.Next, map[string]string{"task.status": "blocked"}); got != "" {
		t.Errorf("blocked parallel_gates goto = %q, want end", got)
	}
	if got, _ := ResolveTransition(gates.Next, map[string]string{"task.status": "human-required"}); got != "" {
		t.Errorf("flagged parallel_gates goto = %q, want end", got)
	}
	if got, _ := ResolveTransition(gates.Next, map[string]string{"task.status": "in-progress"}); got != "set_ready_review" {
		t.Errorf("clean parallel_gates goto = %q, want set_ready_review", got)
	}
}

func TestNodeModulesTorn(t *testing.T) {
	t.Parallel()

	newProject := func(t *testing.T, withLock bool) (dir, nodeModules string) {
		t.Helper()
		dir = t.TempDir()
		if withLock {
			if err := os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		nodeModules = filepath.Join(dir, "node_modules")
		if err := os.MkdirAll(nodeModules, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir, nodeModules
	}

	t.Run("missing .bin is torn", func(t *testing.T) {
		t.Parallel()
		dir, nm := newProject(t, false)
		if !nodeModulesTorn(dir, nm) {
			t.Error("want torn: node_modules/.bin missing")
		}
	})

	t.Run(".bin file is torn", func(t *testing.T) {
		t.Parallel()
		dir, nm := newProject(t, false)
		if err := os.WriteFile(filepath.Join(nm, ".bin"), []byte("not-a-dir"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !nodeModulesTorn(dir, nm) {
			t.Error("want torn: node_modules/.bin exists but is not a directory")
		}
	})

	t.Run("no lockfile trusts .bin presence", func(t *testing.T) {
		t.Parallel()
		dir, nm := newProject(t, false)
		if err := os.MkdirAll(filepath.Join(nm, ".bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if nodeModulesTorn(dir, nm) {
			t.Error("want healthy: .bin present, no lockfile to compare stamp against")
		}
	})

	t.Run("missing npm stamp is torn", func(t *testing.T) {
		t.Parallel()
		dir, nm := newProject(t, true)
		if err := os.MkdirAll(filepath.Join(nm, ".bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if !nodeModulesTorn(dir, nm) {
			t.Error("want torn: npm never finished writing node_modules/.package-lock.json")
		}
	})

	t.Run("stamp older than lockfile is torn", func(t *testing.T) {
		t.Parallel()
		dir, nm := newProject(t, true)
		if err := os.MkdirAll(filepath.Join(nm, ".bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		stamp := filepath.Join(nm, ".package-lock.json")
		if err := os.WriteFile(stamp, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(stamp, old, old); err != nil {
			t.Fatal(err)
		}
		if !nodeModulesTorn(dir, nm) {
			t.Error("want torn: stamp predates the lockfile it should have installed")
		}
	})

	t.Run("fresh stamp is healthy", func(t *testing.T) {
		t.Parallel()
		dir, nm := newProject(t, true)
		if err := os.MkdirAll(filepath.Join(nm, ".bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(nm, ".package-lock.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if nodeModulesTorn(dir, nm) {
			t.Error("want healthy: stamp is newer than the lockfile")
		}
	})
}

// fakeNpmOnPath prepends a directory containing a fake `npm` script to PATH
// for the duration of the test, so repairTornNodeModules's npm call runs the
// fake instead of a real install. The fake writes a marker file recording its
// working directory and arguments so the test can assert whether/where/how it
// ran.
func fakeNpmOnPath(t *testing.T, markerPath string) {
	t.Helper()
	binDir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\npwd > %q\nprintf '%%s\\n' \"$*\" >> %q\n", markerPath, markerPath)
	npmPath := filepath.Join(binDir, "npm")
	if err := os.WriteFile(npmPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRepairTornNodeModules_RepairsWhenTorn(t *testing.T) {
	wt := t.TempDir()
	frontend := filepath.Join(wt, "frontend")
	if err := os.MkdirAll(frontend, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(frontend, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(frontend, "package-lock.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// node_modules present but missing .bin — the torn signature.
	if err := os.MkdirAll(filepath.Join(frontend, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(t.TempDir(), "marker")
	fakeNpmOnPath(t, marker)

	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.repairTornNodeModules(t.Context(), "t1", wt)

	out, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("expected npm ci to run and write a marker, got: %v", err)
	}
	if got := string(out); got != frontend+"\nci --ignore-scripts\n" {
		t.Errorf("npm repair marker = %q, want dir and safe args", got)
	}
}

func TestRepairTornNodeModules_RepairsRootProject(t *testing.T) {
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "package-lock.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// node_modules present but missing .bin — the torn signature.
	if err := os.MkdirAll(filepath.Join(wt, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(t.TempDir(), "marker")
	fakeNpmOnPath(t, marker)

	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.repairTornNodeModules(t.Context(), "t1", wt)

	out, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("expected npm ci to run for root package and write a marker, got: %v", err)
	}
	if got := string(out); got != wt+"\nci --ignore-scripts\n" {
		t.Errorf("npm repair marker = %q, want root dir and safe args", got)
	}
}

func TestRepairTornNodeModules_SkipsWithoutLockfile(t *testing.T) {
	wt := t.TempDir()
	frontend := filepath.Join(wt, "frontend")
	if err := os.MkdirAll(filepath.Join(frontend, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(frontend, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(t.TempDir(), "marker")
	fakeNpmOnPath(t, marker)

	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.repairTornNodeModules(t.Context(), "t1", wt)

	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected npm ci to be skipped without lockfile, got err=%v", err)
	}
}

func TestRepairTornNodeModules_LogsRepairFailure(t *testing.T) {
	wt := t.TempDir()
	frontend := filepath.Join(wt, "frontend")
	if err := os.MkdirAll(filepath.Join(frontend, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(frontend, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(frontend, "package-lock.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	npmPath := filepath.Join(binDir, "npm")
	if err := os.WriteFile(npmPath, []byte("#!/bin/sh\nexit 17\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), logger)
	engine.repairTornNodeModules(t.Context(), "t1", wt)

	if !strings.Contains(logBuf.String(), "workflow.verify-checks.npm-repair-failed") {
		t.Fatalf("expected failed npm repair to be logged, got %q", logBuf.String())
	}
}

func TestRepairTornNodeModules_DisablesLifecycleScripts(t *testing.T) {
	wt := t.TempDir()
	frontend := filepath.Join(wt, "frontend")
	if err := os.MkdirAll(filepath.Join(frontend, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(frontend, "package.json"), []byte(`{
  "scripts": {
    "preinstall": "touch lifecycle-ran"
  }
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(frontend, "package-lock.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(t.TempDir(), "marker")
	lifecycleMarker := filepath.Join(frontend, "lifecycle-ran")
	binDir := t.TempDir()
	script := `#!/bin/sh
pwd > ` + strconv.Quote(marker) + `
printf '%s\n' "$*" >> ` + strconv.Quote(marker) + `
case " $* " in
  *" --ignore-scripts "*) exit 0 ;;
  *) touch ` + strconv.Quote(lifecycleMarker) + `; exit 0 ;;
esac
`
	npmPath := filepath.Join(binDir, "npm")
	if err := os.WriteFile(npmPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.repairTornNodeModules(t.Context(), "t1", wt)

	if _, err := os.Stat(lifecycleMarker); !os.IsNotExist(err) {
		t.Fatalf("repair ran package lifecycle script marker; err = %v", err)
	}
	out, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("expected npm repair marker: %v", err)
	}
	if got := string(out); got != frontend+"\nci --ignore-scripts\n" {
		t.Errorf("npm repair marker = %q, want dir and safe args", got)
	}
}

func TestRepairTornNodeModules_SkipsWhenHealthy(t *testing.T) {
	wt := t.TempDir()
	frontend := filepath.Join(wt, "frontend")
	if err := os.MkdirAll(filepath.Join(frontend, "node_modules", ".bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(frontend, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(t.TempDir(), "marker")
	fakeNpmOnPath(t, marker)

	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.repairTornNodeModules(t.Context(), "t1", wt)

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("expected npm ci NOT to run for a healthy install (no lockfile to compare), marker err = %v", err)
	}
}
