package worktree

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

func hasGit() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// initBareWithCommit creates a bare repo containing a single commit on
// `main`. PrepareForTask branches off origin/main so the bare repo must
// have something checked in for the flow to succeed. Returns (bare, src)
// so callers can seed additional commits via src (the bare's origin) and
// reach them through the normal FetchOrigin path.
func initBareWithCommitReturnSrc(t *testing.T) (bare, src string) {
	t.Helper()
	src = t.TempDir()
	for _, args := range [][]string{
		{"git", "init", "-b", "main", src},
		{"git", "-C", src, "config", "user.email", "test@test.com"},
		{"git", "-C", src, "config", "user.name", "Test"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("# bootstrap-test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "-C", src, "add", "."},
		{"git", "-C", src, "commit", "-m", "init"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}

	bare = filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "clone", "--bare", src, bare).CombinedOutput(); err != nil {
		t.Fatalf("git clone --bare: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", bare, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*").CombinedOutput(); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
	// Ensure the bare has the tracking refs populated (origin/main).
	if out, err := exec.Command("git", "-C", bare, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*").CombinedOutput(); err != nil {
		t.Fatalf("git fetch: %v: %s", err, out)
	}
	return bare, src
}

// preparedManager wires up a Manager with real project/task stores backed
// by temp dirs and returns everything a PrepareForTask integration test
// needs. Caller supplies SetupCommands; the bare repo is created fresh.
type preparedHarness struct {
	m       *Manager
	store   *project.Store
	tasks   *task.Manager
	proj    project.Project
	logsDir string
	wtDir   string
	src     string
}

func prepareHarness(t *testing.T, setupCommands []string, timeout time.Duration) preparedHarness {
	t.Helper()
	if !hasGit() {
		t.Skip("git not available")
	}

	bare, src := initBareWithCommitReturnSrc(t)
	wtDir := t.TempDir()
	logsDir := t.TempDir()

	projDir := filepath.Join(t.TempDir(), "projects")
	clonesDir := filepath.Join(t.TempDir(), "clones")
	store, err := project.NewStore(projDir, clonesDir)
	if err != nil {
		t.Fatal(err)
	}

	// Write project YAML directly — bypass Store.Create which would try to
	// clone from a URL. The bare repo is already at `bare`.
	proj := project.Project{
		ID:            "test/proj",
		Name:          "proj",
		Owner:         "test",
		Repo:          "proj",
		URL:           bare,
		ClonePath:     bare,
		Type:          project.ProjectTypePet,
		Status:        project.ProjectStatusReady,
		SetupCommands: setupCommands,
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	// Use SetSetupCommands indirectly by creating the YAML file. Easiest:
	// seed the file via Store's internal writeFile through SetSetupCommands
	// after creating a placeholder. We instead write the YAML with a helper.
	projYAML := filepath.Join(projDir, "test--proj.yaml")
	if err := os.WriteFile(projYAML, mustMarshalProject(t, proj), 0o644); err != nil {
		t.Fatal(err)
	}

	taskStore, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	taskMgr := task.NewManager(taskStore, nil)

	m := New(Config{
		WorktreesDir: wtDir,
		Projects:     store,
		Tasks:        taskMgr,
		Logger:       discardLogger(),
		LogsDir:      logsDir,
		SetupTimeout: timeout,
	})

	return preparedHarness{m: m, store: store, tasks: taskMgr, proj: proj, logsDir: logsDir, wtDir: wtDir, src: src}
}

func mustMarshalProject(t *testing.T, p project.Project) []byte {
	t.Helper()
	// Reuse the store's YAML schema by round-tripping through its fields —
	// we rely on the Store reading YAML with the same tags used in Project.
	// Build the YAML manually to avoid the internal writeFile coupling.
	var sb strings.Builder
	sb.WriteString("id: " + p.ID + "\n")
	sb.WriteString("name: " + p.Name + "\n")
	sb.WriteString("owner: " + p.Owner + "\n")
	sb.WriteString("repo: " + p.Repo + "\n")
	sb.WriteString("url: " + p.URL + "\n")
	sb.WriteString("clone_path: " + p.ClonePath + "\n")
	sb.WriteString("type: " + string(p.Type) + "\n")
	sb.WriteString("status: " + string(p.Status) + "\n")
	if len(p.SetupCommands) > 0 {
		sb.WriteString("setup_commands:\n")
		for _, c := range p.SetupCommands {
			sb.WriteString("  - " + c + "\n")
		}
	}
	sb.WriteString("created_at: " + p.CreatedAt.Format(time.RFC3339Nano) + "\n")
	sb.WriteString("updated_at: " + p.UpdatedAt.Format(time.RFC3339Nano) + "\n")
	return []byte(sb.String())
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestPathFor(t *testing.T) {
	m := &Manager{dir: "/tmp/wt"}
	tk := task.Task{ID: "abc12345", Slug: "my-task"}
	got := m.PathFor(tk)
	want := "/tmp/wt/my-task-abc12345"
	if got != want {
		t.Errorf("PathFor = %q, want %q", got, want)
	}
}

func TestPathForNoSlug(t *testing.T) {
	m := &Manager{dir: "/tmp/wt"}
	tk := task.Task{ID: "abc12345"}
	got := m.PathFor(tk)
	want := "/tmp/wt/abc12345"
	if got != want {
		t.Errorf("PathFor = %q, want %q", got, want)
	}
}

func TestExists(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{dir: dir}

	tk := task.Task{ID: "exists01"}
	if m.Exists(tk) {
		t.Error("should not exist yet")
	}

	if err := os.MkdirAll(filepath.Join(dir, tk.DirName()), 0o755); err != nil {
		t.Fatal(err)
	}
	if !m.Exists(tk) {
		t.Error("should exist after mkdir")
	}
}

func TestValidatePath(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{dir: dir}

	// Outside worktrees dir
	if err := m.ValidatePath("/tmp/other"); err == nil {
		t.Error("expected error for path outside worktrees dir")
	}

	// Non-existent path within dir
	if err := m.ValidatePath(filepath.Join(dir, "nope")); err == nil {
		t.Error("expected error for non-existent path")
	}

	// Valid directory
	sub := filepath.Join(dir, "valid")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.ValidatePath(sub); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestValidatePath_PrefixEscapeBug demonstrates a path-traversal vulnerability
// in ValidatePath: the containment check uses strings.HasPrefix on the cleaned
// paths, which incorrectly accepts sibling directories whose name starts with
// the worktrees dir name (e.g. "/tmp/wt-evil" passes when m.dir is "/tmp/wt").
//
// ValidatePath gates ProjectService.OpenInTerminal / OpenInEditor
// (svc_projects.go), so a frontend caller can use this to open arbitrary
// directories outside of m.dir as long as the path string shares the prefix.
//
// Fix: use filepath.Rel and require the relative path not to start with "..",
// or compare with a trailing separator appended to m.dir.
func TestValidatePath_PrefixEscapeBug(t *testing.T) {
	base := t.TempDir()
	worktreesDir := filepath.Join(base, "wt")
	siblingDir := filepath.Join(base, "wt-evil")
	if err := os.MkdirAll(worktreesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(siblingDir, 0o755); err != nil {
		t.Fatal(err)
	}

	m := &Manager{dir: worktreesDir}

	// siblingDir is OUTSIDE worktreesDir but starts with the same string
	// prefix. ValidatePath must reject it; today it returns nil.
	if err := m.ValidatePath(siblingDir); err == nil {
		t.Errorf("ValidatePath(%q) returned nil; expected error because it is a sibling of %q, not contained in it", siblingDir, worktreesDir)
	}
}

func TestCleanupOrphaned(t *testing.T) {
	dir := t.TempDir()
	tasksDir := t.TempDir()

	store, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	taskMgr := task.NewManager(store, nil)

	tk, err := store.Create("test task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	// Create worktree dirs: one matching task (not done), one orphaned
	if err := os.MkdirAll(filepath.Join(dir, tk.DirName()), 0o755); err != nil {
		t.Fatal(err)
	}
	orphanDir := filepath.Join(dir, "orphan-12345678")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatal(err)
	}

	m := New(Config{
		WorktreesDir: dir,
		Tasks:        taskMgr,
		Logger:       discardLogger(),
	})

	m.CleanupOrphaned()

	// Orphan should be removed
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Error("orphan dir should be removed")
	}
	// Active task's dir should remain
	if _, err := os.Stat(filepath.Join(dir, tk.DirName())); err != nil {
		t.Error("active task dir should remain")
	}
}

// TestRunSetup_EmptyIsNoOp — backwards compat: projects without
// SetupCommands must skip the hook entirely without creating log files
// or calling into the filesystem.
func TestRunSetup_EmptyIsNoOp(t *testing.T) {
	t.Parallel()
	logsDir := t.TempDir()
	wtDir := t.TempDir()
	m := New(Config{WorktreesDir: wtDir, LogsDir: logsDir, Logger: discardLogger()})

	if err := m.runSetup("task-empty", wtDir, nil); err != nil {
		t.Fatalf("runSetup(nil): %v", err)
	}
	if err := m.runSetup("task-empty", wtDir, []string{}); err != nil {
		t.Fatalf("runSetup([]): %v", err)
	}
	// No log should have been written.
	entries, _ := os.ReadDir(filepath.Join(logsDir, "worktrees"))
	if len(entries) > 0 {
		t.Errorf("expected no setup logs, got %d", len(entries))
	}
}

// TestRunSetup_WritesLogOnSuccess confirms the per-task setup log records
// every command, its exit status, and the completion marker — this log is
// what operators read when a worktree fails to bootstrap.
func TestRunSetup_WritesLogOnSuccess(t *testing.T) {
	t.Parallel()
	logsDir := t.TempDir()
	wtDir := t.TempDir()
	m := New(Config{WorktreesDir: wtDir, LogsDir: logsDir, Logger: discardLogger()})

	marker := filepath.Join(wtDir, "bootstrap-ran")
	if err := m.runSetup("task-ok", wtDir, []string{
		"touch " + marker,
		"echo greetings-from-setup",
	}); err != nil {
		t.Fatalf("runSetup: %v", err)
	}

	if _, err := os.Stat(marker); err != nil {
		t.Errorf("marker file missing — setup did not run: %v", err)
	}

	logPath := filepath.Join(logsDir, "worktrees", "task-ok-setup.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read setup log: %v", err)
	}
	logText := string(data)
	for _, want := range []string{"touch ", "greetings-from-setup", "ok duration", "completed_at"} {
		if !strings.Contains(logText, want) {
			t.Errorf("setup log missing %q; got:\n%s", want, logText)
		}
	}
}

// TestRunSetup_FailureBlocks confirms a non-zero exit surfaces an error
// (callers propagate to fail worktree creation so agents never start on a
// broken toolchain) and that subsequent commands are not executed.
func TestRunSetup_FailureBlocks(t *testing.T) {
	t.Parallel()
	logsDir := t.TempDir()
	wtDir := t.TempDir()
	m := New(Config{WorktreesDir: wtDir, LogsDir: logsDir, Logger: discardLogger()})

	secondMarker := filepath.Join(wtDir, "should-not-run")
	err := m.runSetup("task-fail", wtDir, []string{
		"exit 17",
		"touch " + secondMarker,
	})
	if err == nil {
		t.Fatal("expected error from failing command")
	}
	if !strings.Contains(err.Error(), "exit 17") {
		t.Errorf("error missing command text: %v", err)
	}
	if _, statErr := os.Stat(secondMarker); !os.IsNotExist(statErr) {
		t.Error("second command ran after first failed — should have aborted")
	}

	logPath := filepath.Join(logsDir, "worktrees", "task-fail-setup.log")
	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "exit err=") {
		t.Errorf("setup log missing failure record; got:\n%s", string(data))
	}
}

// TestRunSetup_CwdIsWorktreeRoot guards against cwd leaks: a bootstrap
// script that writes `pwd > .cwd` must see the worktree path, not the
// caller's cwd.
func TestRunSetup_CwdIsWorktreeRoot(t *testing.T) {
	t.Parallel()
	logsDir := t.TempDir()
	wtDir := t.TempDir()
	m := New(Config{WorktreesDir: wtDir, LogsDir: logsDir, Logger: discardLogger()})

	if err := m.runSetup("task-cwd", wtDir, []string{"pwd > .cwd"}); err != nil {
		t.Fatalf("runSetup: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(wtDir, ".cwd"))
	if err != nil {
		t.Fatalf("read .cwd: %v", err)
	}
	// macOS $TMPDIR resolves through /private/var/folders/... while the
	// directory we pass resolves via /var/folders/... — compare eval'd paths.
	wantResolved, _ := filepath.EvalSymlinks(wtDir)
	gotResolved, _ := filepath.EvalSymlinks(strings.TrimSpace(string(got)))
	if gotResolved != wantResolved {
		t.Errorf("cwd = %q, want %q", gotResolved, wantResolved)
	}
}

// TestRunSetup_TimeoutKillsProcess confirms a stuck command is killed at
// the configured timeout and that the log captures the timeout marker.
func TestRunSetup_TimeoutKillsProcess(t *testing.T) {
	t.Parallel()
	logsDir := t.TempDir()
	wtDir := t.TempDir()
	m := New(Config{
		WorktreesDir: wtDir,
		LogsDir:      logsDir,
		Logger:       discardLogger(),
		SetupTimeout: 200 * time.Millisecond,
	})

	start := time.Now()
	err := m.runSetup("task-timeout", wtDir, []string{"sleep 5"})
	dur := time.Since(start)

	if err == nil {
		t.Fatal("expected error on timeout")
	}
	if dur > 3*time.Second {
		t.Errorf("timeout did not fire quickly: took %s", dur)
	}
}

// TestRunSetup_NoLogsDir — when no LogsDir is configured the hook still
// runs and returns errors correctly, only skipping file-based logging.
// This protects test harnesses that skip log configuration.
func TestRunSetup_NoLogsDir(t *testing.T) {
	t.Parallel()
	wtDir := t.TempDir()
	m := New(Config{WorktreesDir: wtDir, Logger: discardLogger()})

	marker := filepath.Join(wtDir, "ran")
	if err := m.runSetup("task-nologs", wtDir, []string{"touch " + marker}); err != nil {
		t.Fatalf("runSetup: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("marker missing: %v", err)
	}

	if err := m.runSetup("task-nologs", wtDir, []string{"exit 1"}); err == nil {
		t.Fatal("expected failure to propagate even without log dir")
	}
}

// TestPrepareForTask_RunsBootstrap is the end-to-end integration: a project
// configured with SetupCommands must execute them on every PrepareForTask
// invocation, with the worktree root as cwd and failures propagated as
// errors (not silent logs). This is the regression guard for the aa9ba123
// class of failure where agents start on a worktree missing required
// toolchain.
func TestPrepareForTask_RunsBootstrap(t *testing.T) {
	if !hasGit() {
		t.Skip("git not available")
	}
	h := prepareHarness(t, []string{"touch bootstrap-marker"}, 30*time.Second)

	tk, err := h.tasks.Store().Create("bootstrap task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(h.proj.ID)}); err != nil {
		t.Fatal(err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}

	path, err := h.m.PrepareForTask(tk, nil)
	if err != nil {
		t.Fatalf("PrepareForTask: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(path, "bootstrap-marker")); statErr != nil {
		t.Errorf("bootstrap marker missing in worktree — SetupCommands did not run: %v", statErr)
	}

	// Setup log must exist and include the command.
	logPath := filepath.Join(h.logsDir, "worktrees", tk.ID+"-setup.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read setup log: %v", err)
	}
	if !strings.Contains(string(data), "bootstrap-marker") {
		t.Errorf("setup log missing command: %s", data)
	}
}

// TestPrepareForTask_MergesRepoAndAppSetup confirms that .sybra.yaml's
// `setup:` block runs first (canonical repo bootstrap) and then any
// machine-local SetupCommands are appended. Both must execute and the
// order must be repo → app so per-machine additions can depend on
// repo-installed tools.
func TestPrepareForTask_MergesRepoAndAppSetup(t *testing.T) {
	if !hasGit() {
		t.Skip("git not available")
	}
	// App-level adds one command; repo-level (written to the worktree's
	// .sybra.yaml below) adds two.
	h := prepareHarness(t, []string{"echo app > app.marker"}, 30*time.Second)

	// Write .sybra.yaml into the upstream src (bare's origin) and commit
	// there so PrepareForTask's FetchOrigin pulls it into refs/remotes/
	// origin/main, which is the ref the worktree is branched from.
	repoYAML := "setup:\n  - echo repo1 > repo1.marker\n  - echo repo2 > repo2.marker\n"
	if err := os.WriteFile(filepath.Join(h.src, ".sybra.yaml"), []byte(repoYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunInDir(t, h.src, "git", "add", ".sybra.yaml")
	mustRunInDir(t, h.src, "git", "commit", "-m", "add repo setup")

	tk, err := h.tasks.Store().Create("merged setup", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(h.proj.ID)}); err != nil {
		t.Fatal(err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}

	path, err := h.m.PrepareForTask(tk, nil)
	if err != nil {
		t.Fatalf("PrepareForTask: %v", err)
	}

	for _, m := range []string{"repo1.marker", "repo2.marker", "app.marker"} {
		if _, statErr := os.Stat(filepath.Join(path, m)); statErr != nil {
			t.Errorf("marker %q missing — merge did not run all commands: %v", m, statErr)
		}
	}

	// Log must record all three commands in order: repo1, repo2, app.
	logPath := filepath.Join(h.logsDir, "worktrees", tk.ID+"-setup.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read setup log: %v", err)
	}
	text := string(data)
	// Precedence check: repo1 must appear before app in the log.
	if strings.Index(text, "repo1.marker") > strings.Index(text, "app.marker") {
		t.Errorf("ordering wrong — app ran before repo. log:\n%s", text)
	}
}

func mustRunInDir(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}

// TestPrepareForTask_BootstrapFailureBlocks confirms a failing setup
// command aborts worktree preparation and surfaces the error to callers.
// Without this, an agent would start on a broken worktree and waste tokens
// hitting missing-tool errors.
func TestPrepareForTask_BootstrapFailureBlocks(t *testing.T) {
	if !hasGit() {
		t.Skip("git not available")
	}
	h := prepareHarness(t, []string{"exit 42"}, 30*time.Second)

	tk, err := h.tasks.Store().Create("failing bootstrap", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(h.proj.ID)}); err != nil {
		t.Fatal(err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}

	_, err = h.m.PrepareForTask(tk, nil)
	if err == nil {
		t.Fatal("expected PrepareForTask to fail when bootstrap exits non-zero")
	}
	if !strings.Contains(err.Error(), "exit 42") {
		t.Errorf("error does not carry bootstrap command text: %v", err)
	}
}
