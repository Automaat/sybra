package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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
	script := "#!/bin/sh\npwd > " + markerPath + "\nprintf '%s\\n' \"$*\" >> " + markerPath + "\n"
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
