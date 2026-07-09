package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
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

	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
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

	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	tail := &boundedTail{max: 4096}
	engine.ensureNodeToolchain(context.Background(), "t1", dir, "npm run build:web", tail)

	if _, err := os.Stat(filepath.Join(dir, "marker-npm-ci-ran")); err == nil {
		t.Error("npm ci should not have run — toolchain looked intact")
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

	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
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

	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
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

	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
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

	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
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
	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	tail := &boundedTail{max: 4096}
	engine.ensureNodeToolchain(context.Background(), "t1", wt, "cd ../outside && npm run build", tail)

	if _, err := os.Stat(filepath.Join(outside, "marker-npm-ci-ran")); err == nil {
		t.Error("npm ci must not run outside the worktree via a `..` traversal")
	}
}

func TestEnsureNodeToolchain_NonNodeDirSkips(t *testing.T) {
	dir := t.TempDir() // no package.json
	fakeNPM(t, "marker-npm-ci-ran")

	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
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

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
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
