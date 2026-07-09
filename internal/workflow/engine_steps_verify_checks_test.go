package workflow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeCheckGetter struct{ cmds, setup []string }

func (f *fakeCheckGetter) VerifyCommands(context.Context, string) []string { return f.cmds }

func (f *fakeCheckGetter) SetupCommands(context.Context, string) []string { return f.setup }

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

func newVerifyChecksEngineWithSetup(t *testing.T, wt string, cmds, setup []string) (*Engine, *memTasks) {
	t.Helper()
	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wt, ok: true})
	engine.SetCheckConfigGetter(&fakeCheckGetter{cmds: cmds, setup: setup})
	return engine, engine.tasks.(*memTasks)
}

func TestExecVerifyChecks_ToolchainHealRepairs(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	cmds := []string{`test -f .healed || { echo "vite: command not found" >&2; exit 1; }`}
	engine, tasks := newVerifyChecksEngineWithSetup(t, wt, cmds, []string{"touch .healed"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), TaskInfo{ID: "t1"})
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

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), TaskInfo{ID: "t1"})
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

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), TaskInfo{ID: "t1"})
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

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged (non-toolchain failure must not trigger setup heal)", out.Output)
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
	failed, _, _, err := engine.runVerifyCommands(ctx, "t1", wt, []string{"sleep 20"})
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

func writeFakeNPMScript(t *testing.T, script string) {
	t.Helper()
	bin := t.TempDir()
	npmPath := filepath.Join(bin, "npm")
	if err := os.WriteFile(npmPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake npm: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// writeFakeNPM installs a stub `npm` on PATH that only understands `npm ci`:
// it recreates node_modules/.bin and node_modules/.package-lock.json in the
// current directory, mirroring what a real `npm ci` does for the purposes of
// this gate's corruption check. succeed=false makes it fail every time, to
// exercise the "repair itself fails" path.
func writeFakeNPM(t *testing.T, succeed bool) {
	t.Helper()
	script := "#!/bin/sh\n"
	if succeed {
		script += "if [ \"$1\" = \"ci\" ]; then mkdir -p node_modules/.bin && touch node_modules/.package-lock.json && exit 0; fi\nexit 1\n"
	} else {
		script += "exit 1\n"
	}
	writeFakeNPMScript(t, script)
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
	if err := os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write package-lock.json: %v", err)
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
	if err := os.WriteFile(filepath.Join(healthyDir, "package-lock.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// pnpm/yarn-style project: package.json and broken node_modules are not
	// enough to run an npm-specific repair without package-lock.json.
	nonNpmDir := filepath.Join(root, "pnpm-app")
	if err := os.MkdirAll(filepath.Join(nonNpmDir, "node_modules", "some-pkg"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nonNpmDir, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nonNpmDir, "pnpm-lock.yaml"), []byte("lockfileVersion: 9\n"), 0o644); err != nil {
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

	// Symlinked directories must be skipped so the scan cannot escape the
	// worktree or hand npm a symlink target to rewrite.
	symlinkTarget := t.TempDir()
	makeCorruptedNodeModules(t, symlinkTarget)
	if err := os.Symlink(symlinkTarget, filepath.Join(root, "linked")); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}
	symlinkedNodeModulesDir := filepath.Join(root, "symlinked-node-modules")
	if err := os.MkdirAll(symlinkedNodeModulesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(symlinkedNodeModulesDir, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(symlinkedNodeModulesDir, "package-lock.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(filepath.Join(symlinkTarget, "node_modules"), filepath.Join(symlinkedNodeModulesDir, "node_modules")); err != nil {
		t.Fatalf("symlink node_modules: %v", err)
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

func TestExecVerifyChecks_NodeModulesRepairCancelSkips(t *testing.T) {
	// Not t.Parallel(): mutates PATH and installs a fake npm.
	writeFakeNPMScript(t, "#!/bin/sh\nif [ \"$1\" = \"ci\" ]; then touch npm-started; while :; do sleep 0.1; done; fi\nexit 1\n")
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	frontend := filepath.Join(wt, "frontend")
	makeCorruptedNodeModules(t, frontend)

	cmd := "cd frontend && test -d node_modules/.bin"
	engine, tasks := newVerifyChecksEngine(t, wt, []string{cmd})
	parentCtx, cancel := context.WithCancel(context.Background())
	engine.SetContext(parentCtx)
	t.Cleanup(cancel)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	type result struct {
		out StepOutput
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), TaskInfo{ID: "t1"})
		done <- result{out: out, err: err}
	}()

	started := filepath.Join(frontend, "npm-started")
	deadline := time.After(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("fake npm ci did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("unexpected error: %v", res.err)
		}
		if res.out.Output != "skipped: context canceled" {
			t.Fatalf("Output = %q, want skipped: context canceled", res.out.Output)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("execVerifyChecks did not return after engine context cancellation")
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged after engine cancellation", ti.Status)
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

func TestExecVerifyChecks_NodeModulesRepairScopesToFailedCommandDir(t *testing.T) {
	// Not t.Parallel(): writeFakeNPM uses t.Setenv, which forbids it.
	writeFakeNPM(t, true)
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	frontend := filepath.Join(wt, "frontend")
	docs := filepath.Join(wt, "docs")
	makeCorruptedNodeModules(t, frontend)
	makeCorruptedNodeModules(t, docs)

	cmd := "cd frontend && test -d node_modules/.bin"
	engine, tasks := newVerifyChecksEngine(t, wt, []string{cmd})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("Output = %q, want clean", out.Output)
	}
	if _, err := os.Stat(filepath.Join(docs, "node_modules", ".bin")); err == nil {
		t.Fatal("unrelated docs/node_modules was repaired")
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

	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.repairTornNodeModules("t1", wt)

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

	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.repairTornNodeModules("t1", wt)

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

	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.repairTornNodeModules("t1", wt)

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

	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), logger)
	engine.repairTornNodeModules("t1", wt)

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

	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.repairTornNodeModules("t1", wt)

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

	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.repairTornNodeModules("t1", wt)

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("expected npm ci NOT to run for a healthy install (no lockfile to compare), marker err = %v", err)
	}
}
