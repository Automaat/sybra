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

	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.repairCorruptedNodeModules(context.Background(), "t1", wt)

	if _, err := os.Stat(filepath.Join(nm, ".bin")); err != nil {
		t.Errorf("expected node_modules/.bin to be repaired: %v", err)
	}
	if _, err := os.Stat(filepath.Join(nm, ".package-lock.json")); err != nil {
		t.Errorf("expected node_modules/.package-lock.json to be repaired: %v", err)
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
	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
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

	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
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

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), TaskInfo{ID: "t1"})
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
