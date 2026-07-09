package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeCheckGetter struct{ cmds []string }

func (f *fakeCheckGetter) VerifyCommands(context.Context, string) []string { return f.cmds }

func newVerifyChecksStep() *Step { return &Step{ID: "verify_checks", Type: StepVerifyChecks} }

func newVerifyChecksEngine(t *testing.T, wt string, cmds []string) (*Engine, *memTasks) {
	t.Helper()
	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wt, ok: true})
	engine.SetCheckConfigGetter(&fakeCheckGetter{cmds: cmds})
	return engine, engine.tasks.(*memTasks)
}

func TestExecVerifyChecks_NoGetterSkips(t *testing.T) {
	t.Parallel()
	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: t.TempDir(), ok: true})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "skipped: no check config getter" {
		t.Errorf("Output = %q, want skip", out.Output)
	}
}

func TestExecVerifyChecks_NoCommandsSkips(t *testing.T) {
	t.Parallel()
	engine, tasks := newVerifyChecksEngine(t, t.TempDir(), nil)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "skipped: no verify commands configured" {
		t.Errorf("Output = %q, want skip", out.Output)
	}
}

func TestExecVerifyChecks_PassIsClean(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, tasks := newVerifyChecksEngine(t, wt, []string{"true", "echo ok"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), TaskInfo{ID: "t1"})
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

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), TaskInfo{ID: "t1"})
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

func TestExecVerifyChecks_BlessedTagSkips(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, tasks := newVerifyChecksEngine(t, wt, []string{"exit 1"}) // would fail
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(),
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

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), TaskInfo{ID: "t1"})
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

func TestExecVerifyChecks_TimeoutRetryAbsorbsLoadSpike(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	slowOnce := "test -f .timeout-marker || { touch .timeout-marker; sleep 30; }"
	engine, tasks := newVerifyChecksEngine(t, wt, []string{slowOnce})
	engine.SetVerifyTimeout(200 * time.Millisecond)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), TaskInfo{ID: "t1"})
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

func TestExecVerifyChecks_FlakeRetryPasses(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	// Fails the first run, succeeds on retry (the marker persists in the
	// worktree between attempts) — a nondeterministic flake must not block.
	flaky := "test -f .flake-marker || { touch .flake-marker; exit 1; }"
	engine, tasks := newVerifyChecksEngine(t, wt, []string{flaky})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), TaskInfo{ID: "t1"})
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
	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
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

// writeFakeNPM installs a stub `npm` on PATH that only understands `npm ci`:
// it recreates node_modules/.bin and node_modules/.package-lock.json in the
// current directory, mirroring what a real `npm ci` does for the purposes of
// this gate's corruption check. succeed=false makes it fail every time, to
// exercise the "repair itself fails" path.
func writeFakeNPM(t *testing.T, succeed bool) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\n"
	if succeed {
		script += "if [ \"$1\" = \"ci\" ]; then mkdir -p node_modules/.bin && touch node_modules/.package-lock.json && exit 0; fi\nexit 1\n"
	} else {
		script += "exit 1\n"
	}
	npmPath := filepath.Join(bin, "npm")
	if err := os.WriteFile(npmPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake npm: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// makeCorruptedNodeModules creates dir/node_modules missing .bin and
// .package-lock.json alongside a package.json — the shape a killed `npm ci`
// leaves behind.
func makeCorruptedNodeModules(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "some-pkg"), 0o755); err != nil {
		t.Fatalf("mkdir node_modules: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
}

func TestFindCorruptedNodeModules(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Corrupted: node_modules present but missing .bin/.package-lock.json.
	corruptedDir := filepath.Join(root, "frontend")
	makeCorruptedNodeModules(t, corruptedDir)

	// Healthy: node_modules with both markers present — not corrupted.
	healthyDir := filepath.Join(root, "healthy")
	if err := os.MkdirAll(filepath.Join(healthyDir, "node_modules", ".bin"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(healthyDir, "node_modules", ".package-lock.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(healthyDir, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Never installed: no node_modules at all — not corruption, leave alone.
	neverInstalledDir := filepath.Join(root, "never-installed")
	if err := os.MkdirAll(neverInstalledDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(neverInstalledDir, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := findCorruptedNodeModules(root)
	if len(got) != 1 || got[0] != corruptedDir {
		t.Fatalf("findCorruptedNodeModules = %v, want [%s]", got, corruptedDir)
	}
}

func TestExecVerifyChecks_NodeModulesRepairRecoversFromCorruption(t *testing.T) {
	// Not t.Parallel(): writeFakeNPM uses t.Setenv, which forbids it.
	writeFakeNPM(t, true)
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	makeCorruptedNodeModules(t, filepath.Join(wt, "frontend"))

	// Fails while node_modules/.bin is missing; passes once the fake `npm ci`
	// repair recreates it.
	cmd := "cd frontend && test -d node_modules/.bin"
	engine, tasks := newVerifyChecksEngine(t, wt, []string{cmd})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("Output = %q, want clean (corrupted node_modules repaired then retried)", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged (repair recovered the check)", ti.Status)
	}
}

func TestExecVerifyChecks_NodeModulesRepairFailureStillBlocks(t *testing.T) {
	// Not t.Parallel(): writeFakeNPM uses t.Setenv, which forbids it.
	writeFakeNPM(t, false) // repair itself fails every time
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	makeCorruptedNodeModules(t, filepath.Join(wt, "frontend"))

	cmd := "cd frontend && test -d node_modules/.bin"
	engine, tasks := newVerifyChecksEngine(t, wt, []string{cmd})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged (repair itself failed, must not mask a real problem)", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required", ti.Status)
	}
}

func TestExecVerifyChecks_UnrelatedFailureSkipsRepair(t *testing.T) {
	t.Parallel()
	// No corrupted node_modules anywhere in the worktree — a genuine code
	// failure must still block without attempting any repair.
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, tasks := newVerifyChecksEngine(t, wt, []string{"exit 1"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required", ti.Status)
	}
}

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
	vc := impl.StepByID("verify_checks")
	if vc == nil {
		t.Fatal("verify_checks step missing from simple-task-implement")
		return
	}
	dt := impl.StepByID("detect_tampering")
	if dt == nil {
		t.Fatal("detect_tampering step missing")
		return
	}
	if got, _ := ResolveTransition(dt.Next, map[string]string{"task.status": "in-progress"}); got != "verify_checks" {
		t.Errorf("detect_tampering clean goto = %q, want verify_checks", got)
	}
	if got, _ := ResolveTransition(vc.Next, map[string]string{"task.status": "human-required"}); got != "" {
		t.Errorf("flagged verify_checks goto = %q, want end", got)
	}
	if got, _ := ResolveTransition(vc.Next, map[string]string{"task.status": "in-progress"}); got != "set_ready_review" {
		t.Errorf("clean verify_checks goto = %q, want set_ready_review", got)
	}
}
