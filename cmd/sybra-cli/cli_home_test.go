package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

// TestHomeFlag_OverridesEverything pins --home as the top of the precedence
// order: it wins even when SYBRA_CONTROL_HOME and SYBRA_HOME both point
// elsewhere. Deliberately leaves SYBRA_TASKS_DIR unset (unlike setupStore) so
// TasksDir is derived from the resolved home — the thing under test here.
func TestHomeFlag_OverridesEverything(t *testing.T) {
	realHome := t.TempDir()
	sandboxHome := t.TempDir()
	t.Setenv("SYBRA_HOME", sandboxHome)
	t.Setenv("SYBRA_CONTROL_HOME", filepath.Join(t.TempDir(), "unused"))

	code, out := runCLI(t, "--json", "--home", realHome, "create", "--title", "in real home")
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, out)
	}

	// The task must land under --home, not SYBRA_HOME or SYBRA_CONTROL_HOME.
	code, out = runCLI(t, "--json", "--home", realHome, "list")
	if code != 0 {
		t.Fatalf("list exit %d: %s", code, out)
	}
	if !strings.Contains(out, "in real home") {
		t.Fatalf("list under --home = %q, want the created task", out)
	}

	// The sandbox home (bare SYBRA_HOME, no --home) must not see it.
	code, out = runCLI(t, "--json", "--home", sandboxHome, "list")
	if code != 0 {
		t.Fatalf("list exit %d: %s", code, out)
	}
	if strings.Contains(out, "in real home") {
		t.Fatalf("sandbox home list leaked the --home task: %s", out)
	}
}

// TestHomeFlag_EqualsForm pins the --home=DIR form alongside --home DIR.
func TestHomeFlag_EqualsForm(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("SYBRA_HOME", t.TempDir())

	code, out := runCLI(t, "--json", "--home="+realHome, "create", "--title", "equals form")
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, out)
	}
	code, out = runCLI(t, "--json", "--home", realHome, "list")
	if code != 0 {
		t.Fatalf("list exit %d: %s", code, out)
	}
	if !strings.Contains(out, "equals form") {
		t.Fatalf("list under --home = %q, want the created task", out)
	}
}

// TestControlHomeEnv_WinsOverSybraHome pins the second precedence tier: bare
// sybra-cli (no --home) inside a task-scoped agent, where SYBRA_HOME points at
// the sandbox but SYBRA_CONTROL_HOME points at the real operator store, must
// reach the real store.
func TestControlHomeEnv_WinsOverSybraHome(t *testing.T) {
	realHome := t.TempDir()
	sandboxHome := t.TempDir()
	t.Setenv("SYBRA_HOME", sandboxHome)
	t.Setenv("SYBRA_CONTROL_HOME", realHome)

	code, out := runCLI(t, "--json", "create", "--title", "via control home")
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, out)
	}

	code, out = runCLI(t, "--json", "--home", realHome, "list")
	if code != 0 {
		t.Fatalf("list exit %d: %s", code, out)
	}
	if !strings.Contains(out, "via control home") {
		t.Fatalf("real home list = %q, want the task created via SYBRA_CONTROL_HOME", out)
	}

	code, out = runCLI(t, "--json", "--home", sandboxHome, "list")
	if code != 0 {
		t.Fatalf("list exit %d: %s", code, out)
	}
	if strings.Contains(out, "via control home") {
		t.Fatalf("sandbox home leaked the SYBRA_CONTROL_HOME task: %s", out)
	}
}

func TestControlHomeEnv_ForcesFilesystemModeEvenWithServerRunning(t *testing.T) {
	realHome := t.TempDir()
	sandboxHome := t.TempDir()
	tasksDir := filepath.Join(realHome, "tasks")
	if err := os.MkdirAll(tasksDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYBRA_HOME", sandboxHome)
	t.Setenv("SYBRA_CONTROL_HOME", realHome)

	code, out := runCLI(t, "--json", "create", "--title", "control home target")
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, out)
	}

	tasks, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	list, err := tasks.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("expected exactly one seeded task, got %v (err=%v)", list, err)
	}
	id := list[0].ID

	port := startFakeAPIServer(t, tasksDir)
	t.Setenv("SYBRA_PORT", port)

	lockdownDir(t, tasksDir)

	code, _ = runCLI(t, "--json", "update", id, "--status", "todo")
	if code == 0 {
		t.Fatal("SYBRA_CONTROL_HOME must force filesystem mode even when a server is reachable; update against a read-only dir should fail")
	}
}

// TestHomeFlag_MalformedMissingValue_NonHookIsFatal pins that a dangling
// --home with no value is a hard usage error for ordinary commands.
func TestHomeFlag_MalformedMissingValue_NonHookIsFatal(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "--json", "list", "--home")
	if code == 0 {
		t.Fatal("expected non-zero exit for a dangling --home with no value")
	}
}

// TestHomeFlag_MalformedMissingValue_HookFailsOpen pins that the same
// malformed --home never blocks a codex hook invocation — hook must always
// exit 0 so a bad flag can't stall an agent run.
func TestHomeFlag_MalformedMissingValue_HookFailsOpen(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "hook", "--home")
	if code != 0 {
		t.Fatalf("hook exit = %d, want 0 (fail-open)", code)
	}
}
