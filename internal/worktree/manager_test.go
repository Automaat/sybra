package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/notes"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

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
	if out, err := exec.Command("git", "-c", "safe.bareRepository=all", "-C", bare, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*").CombinedOutput(); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
	// Ensure the bare has the tracking refs populated (origin/main).
	if out, err := exec.Command("git", "-c", "safe.bareRepository=all", "-C", bare, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*").CombinedOutput(); err != nil {
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
	return slog.New(slog.DiscardHandler)
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

// TestPathFor_ContainedForValidSlug verifies that PathFor always stays inside
// the worktrees directory when the task has a Slugify-safe slug.
func TestPathFor_ContainedForValidSlug(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m := &Manager{dir: dir}

	slugs := []string{"task", "my-task", "implement-auth", "fix-42", "a"}
	for _, slug := range slugs {
		tk := task.Task{ID: "abc12345", Slug: slug}
		got := m.PathFor(tk)
		rel, err := filepath.Rel(dir, got)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Errorf("PathFor(slug=%q) = %q escapes worktrees dir %q", slug, got, dir)
		}
	}
}

// TestPathFor_TraversalSlugWouldEscape documents why slug validation is
// required: without validation a persisted slug with path separators or
// dot-dot segments would make PathFor produce a path outside the worktrees
// directory. ValidateSlug must reject every slug that would escape.
func TestPathFor_TraversalSlugWouldEscape(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m := &Manager{dir: dir}

	malicious := []string{
		"../../etc/passwd",
		"../sibling",
		"..",
		"/absolute",
		"a/b",
	}
	for _, slug := range malicious {
		tk := task.Task{ID: "abc12345", Slug: slug}
		got := m.PathFor(tk)
		rel, err := filepath.Rel(dir, got)
		escapesDir := err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
		if escapesDir {
			// Path escapes: confirm ValidateSlug catches it.
			if verr := task.ValidateSlug(slug); verr == nil {
				t.Errorf("ValidateSlug(%q) = nil but PathFor produces escaping path %q", slug, got)
			}
		}
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

	m.CleanupOrphaned(context.Background())

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

	if err := m.runSetup(context.Background(), "task-empty", wtDir, nil); err != nil {
		t.Fatalf("runSetup(nil): %v", err)
	}
	if err := m.runSetup(context.Background(), "task-empty", wtDir, []string{}); err != nil {
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
	if err := m.runSetup(context.Background(), "task-ok", wtDir, []string{
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
	err := m.runSetup(context.Background(), "task-fail", wtDir, []string{
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

// TestRunSetupNonGating_FailureDoesNotBlock proves the fix-role setup path
// (issue #1454) never returns an error on a failing setup command — a fixer
// worktree must always be creatable even when the PR under repair broke the
// project's build step, since that's exactly what the fixer exists to
// repair. The failure must instead be captured into NOTES.md.
func TestRunSetupNonGating_FailureDoesNotBlock(t *testing.T) {
	t.Parallel()
	logsDir := t.TempDir()
	wtDir := t.TempDir()
	mustRunInDir(t, wtDir, "git", "init", "-b", "main")
	m := New(Config{WorktreesDir: wtDir, LogsDir: logsDir, Logger: discardLogger()})

	err := m.runSetupNonGating(context.Background(), "task-fix-fail", wtDir, []string{
		"echo broken build >&2 && exit 1",
	})
	if err != nil {
		t.Fatalf("runSetupNonGating must not fail worktree creation: %v", err)
	}

	content, readErr := os.ReadFile(filepath.Join(wtDir, notes.FileName))
	if readErr != nil {
		t.Fatalf("read scratchpad: %v", readErr)
	}
	if !strings.Contains(string(content), "Setup failure") {
		t.Errorf("scratchpad missing setup failure note: %q", content)
	}
}

// TestRunSetupNonGating_SuccessLeavesNoNote confirms a clean setup run does
// not create a spurious NOTES.md — the scratchpad is only seeded on failure
// for fix-role worktrees (implementation worktrees seed it separately via
// seedWorktree).
func TestRunSetupNonGating_SuccessLeavesNoNote(t *testing.T) {
	t.Parallel()
	logsDir := t.TempDir()
	wtDir := t.TempDir()
	mustRunInDir(t, wtDir, "git", "init", "-b", "main")
	m := New(Config{WorktreesDir: wtDir, LogsDir: logsDir, Logger: discardLogger()})

	if err := m.runSetupNonGating(context.Background(), "task-fix-ok", wtDir, []string{"true"}); err != nil {
		t.Fatalf("runSetupNonGating: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(wtDir, notes.FileName)); !os.IsNotExist(statErr) {
		t.Errorf("expected no NOTES.md on successful setup, stat err: %v", statErr)
	}
}

// TestRunSetupNonGating_PersistFailureReturnsError proves the prepare still
// fails closed when Sybra cannot persist the non-fatal setup failure context.
// Without this, fix-role worktrees would silently start with neither the note
// nor the marker that downstream circuit breakers rely on.
func TestRunSetupNonGating_PersistFailureReturnsError(t *testing.T) {
	t.Parallel()
	logsDir := t.TempDir()
	wtDir := t.TempDir()
	mustRunInDir(t, wtDir, "git", "init", "-b", "main")
	if err := os.Mkdir(filepath.Join(wtDir, notes.FileName), 0o755); err != nil {
		t.Fatalf("mkdir fake NOTES.md: %v", err)
	}
	m := New(Config{WorktreesDir: wtDir, LogsDir: logsDir, Logger: discardLogger()})

	err := m.runSetupNonGating(context.Background(), "task-fix-persist-fail", wtDir, []string{
		"echo broken build >&2 && exit 1",
	})
	if err == nil {
		t.Fatal("expected error when failure context cannot be persisted")
	}
	if !strings.Contains(err.Error(), "persist setup failure context") {
		t.Fatalf("error = %v, want persistence failure context", err)
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

	if err := m.runSetup(context.Background(), "task-cwd", wtDir, []string{"pwd > .cwd"}); err != nil {
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
	err := m.runSetup(context.Background(), "task-timeout", wtDir, []string{"sleep 5"})
	dur := time.Since(start)

	if err == nil {
		t.Fatal("expected error on timeout")
	}
	if dur > 3*time.Second {
		t.Errorf("timeout did not fire quickly: took %s", dur)
	}
}

// TestRunSetup_ParentCancellationKillsProcess proves runSetup derives its
// working context from the caller-supplied parent instead of context.Background():
// cancelling the parent must abort a long-running setup command even though
// SetupTimeout alone would not have fired yet.
func TestRunSetup_ParentCancellationKillsProcess(t *testing.T) {
	t.Parallel()
	logsDir := t.TempDir()
	wtDir := t.TempDir()
	m := New(Config{
		WorktreesDir: wtDir,
		LogsDir:      logsDir,
		Logger:       discardLogger(),
		SetupTimeout: time.Minute,
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := m.runSetup(ctx, "task-parent-cancel", wtDir, []string{"sleep 5"})
	dur := time.Since(start)

	if err == nil {
		t.Fatal("expected error when parent context is cancelled")
	}
	if dur > 3*time.Second {
		t.Errorf("parent cancellation did not abort setup quickly: took %s", dur)
	}
}

// TestRunSetup_TimeoutKillsProcessGroup proves a grandchild spawned by a
// setup command (e.g. a backgrounded daemon started by npm install) is
// killed along with the shell when the batch timeout fires, not left
// running as an orphan (issue #1538).
func TestRunSetup_TimeoutKillsProcessGroup(t *testing.T) {
	t.Parallel()
	logsDir := t.TempDir()
	wtDir := t.TempDir()
	m := New(Config{
		WorktreesDir: wtDir,
		LogsDir:      logsDir,
		Logger:       discardLogger(),
		SetupTimeout: 5 * time.Second,
	})

	pidFile := filepath.Join(wtDir, "child.pid")
	// Background a grandchild that writes its pid immediately (well before
	// the setup timeout fires) then outlives `sh`, staying alive for the
	// rest of the test unless the whole process group is killed.
	cmd := fmt.Sprintf("nohup sh -c 'echo $$ > %q; sleep 10' >/dev/null 2>&1 & sleep 10", pidFile)

	err := m.runSetup(context.Background(), "task-timeout-pg", wtDir, []string{cmd})
	if err == nil {
		t.Fatal("expected error on timeout")
	}

	// Give the grandchild a moment to have written its pid before we assert
	// it's gone — the write happens near-instantly after it forks, well
	// before the setup timeout above ever fires.
	deadline := time.Now().Add(3 * time.Second)
	var pidRaw []byte
	for time.Now().Before(deadline) {
		pidRaw, err = os.ReadFile(pidFile)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("grandchild never wrote pid file: %v", err)
	}

	pid, convErr := strconv.Atoi(strings.TrimSpace(string(pidRaw)))
	if convErr != nil {
		t.Fatalf("parse pid: %v", convErr)
	}

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if processGoneOrZombie(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("grandchild pid %d still alive after setup timeout", pid)
}

func processGoneOrZombie(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return true
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	stat := string(data)
	afterComm := strings.LastIndexByte(stat, ')')
	if afterComm < 0 || afterComm+2 >= len(stat) {
		return false
	}
	fields := strings.Fields(stat[afterComm+2:])
	return len(fields) > 0 && fields[0] == "Z"
}

// TestRunSetup_NoLogsDir — when no LogsDir is configured the hook still
// runs and returns errors correctly, only skipping file-based logging.
// This protects test harnesses that skip log configuration.
func TestRunSetup_NoLogsDir(t *testing.T) {
	t.Parallel()
	wtDir := t.TempDir()
	m := New(Config{WorktreesDir: wtDir, Logger: discardLogger()})

	marker := filepath.Join(wtDir, "ran")
	if err := m.runSetup(context.Background(), "task-nologs", wtDir, []string{"touch " + marker}); err != nil {
		t.Fatalf("runSetup: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("marker missing: %v", err)
	}

	if err := m.runSetup(context.Background(), "task-nologs", wtDir, []string{"exit 1"}); err == nil {
		t.Fatal("expected failure to propagate even without log dir")
	}
}

// --- mise trust preflight -----------------------------------------------
//
// installFakeMise writes a tiny mise shim that logs invocation args to a file
// and exits with exitCode. Returns (shimPath, logPath). Tests pass shimPath via
// Config.MisePath so parallel tests don't race on os.Setenv(PATH).
func installFakeMise(t *testing.T, exitCode int) (shimPath, logPath string) {
	t.Helper()
	binDir := t.TempDir()
	logPath = filepath.Join(binDir, "invocations.log")
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\nexit " + strconv.Itoa(exitCode) + "\n"
	shimPath = filepath.Join(binDir, "mise")
	if err := os.WriteFile(shimPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	return shimPath, logPath
}

// TestRunSetup_TrustsMiseConfig guards the fix for the 2026-04-16 server
// wave of "mise ERROR Config files ... are not trusted" failures. When a
// mise.toml is present in the worktree, runSetup must call `mise trust
// --yes` before any user setup command so first-run setup does not
// fail.
func TestRunSetup_TrustsMiseConfig(t *testing.T) {
	t.Parallel()
	logsDir := t.TempDir()
	wtDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wtDir, "mise.toml"), []byte("[tools]\n"), 0o644); err != nil {
		t.Fatalf("seed mise.toml: %v", err)
	}
	miseBin, miseLog := installFakeMise(t, 0)

	m := New(Config{WorktreesDir: wtDir, LogsDir: logsDir, Logger: discardLogger(), MisePath: miseBin})
	if err := m.runSetup(context.Background(), "task-trust", wtDir, []string{"true"}); err != nil {
		t.Fatalf("runSetup: %v", err)
	}

	data, err := os.ReadFile(miseLog)
	if err != nil {
		t.Fatalf("mise shim was never invoked: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "trust --yes" {
		t.Errorf("mise invoked with %q, want %q", got, "trust --yes")
	}

	setupLog, err := os.ReadFile(filepath.Join(logsDir, "worktrees", "task-trust-setup.log"))
	if err != nil {
		t.Fatalf("read setup log: %v", err)
	}
	if !strings.Contains(string(setupLog), "mise trust --yes") {
		t.Errorf("setup log missing trust entry:\n%s", setupLog)
	}
}

// TestRunSetup_SkipsTrustWithoutMiseConfig prevents regressions that would
// spend a subprocess call on every worktree regardless of whether mise is
// actually in play.
func TestRunSetup_SkipsTrustWithoutMiseConfig(t *testing.T) {
	t.Parallel()
	logsDir := t.TempDir()
	wtDir := t.TempDir()
	miseBin, miseLog := installFakeMise(t, 0)

	m := New(Config{WorktreesDir: wtDir, LogsDir: logsDir, Logger: discardLogger(), MisePath: miseBin})
	if err := m.runSetup(context.Background(), "task-no-mise", wtDir, []string{"true"}); err != nil {
		t.Fatalf("runSetup: %v", err)
	}

	if _, err := os.Stat(miseLog); !os.IsNotExist(err) {
		t.Errorf("mise shim was invoked despite absent mise.toml (stat err=%v)", err)
	}
}

// TestRunSetup_TrustFailureIsNonFatal: if `mise trust` exits non-zero
// (e.g. mise is installed but the config has a parse error that trust
// itself cannot accept), the setup commands still run. The clearer error
// surfaces from the real command that needs mise, not from the preflight.
func TestRunSetup_TrustFailureIsNonFatal(t *testing.T) {
	t.Parallel()
	logsDir := t.TempDir()
	wtDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wtDir, ".mise.toml"), []byte(""), 0o644); err != nil {
		t.Fatalf("seed .mise.toml: %v", err)
	}
	miseBin, _ := installFakeMise(t, 3)

	m := New(Config{WorktreesDir: wtDir, LogsDir: logsDir, Logger: discardLogger(), MisePath: miseBin})
	marker := filepath.Join(wtDir, "did-run")
	if err := m.runSetup(context.Background(), "task-trust-fail", wtDir, []string{"touch " + marker}); err != nil {
		t.Fatalf("runSetup should succeed despite trust failure: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("marker missing — setup command did not execute: %v", err)
	}
}

func TestHasMiseConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		file string
		want bool
	}{
		{"mise.toml", "mise.toml", true},
		{"dotfile variant", ".mise.toml", true},
		{"local variant", "mise.local.toml", true},
		{"dotfile local", ".mise.local.toml", true},
		{"unrelated toml is not a mise config", "foo.toml", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tc.file)
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			if got := hasMiseConfig(dir); got != tc.want {
				t.Errorf("hasMiseConfig(%s)=%v want %v", tc.file, got, tc.want)
			}
		})
	}
}

// TestPrepareForTask_RunsBootstrap is the end-to-end integration: a project
// configured with SetupCommands must execute them on every PrepareForTask
// invocation, with the worktree root as cwd and failures propagated as
// errors (not silent logs). This is the regression guard for the aa9ba123
// class of failure where agents start on a worktree missing required
// toolchain.
func TestPrepareForTask_RunsBootstrap(t *testing.T) {
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

	path, err := h.m.PrepareForTask(context.Background(), tk, nil)
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

func TestPrepareForTask_RebranchesOnBranchCollision(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)
	ctx := context.Background()

	mk := func(title string) task.Task {
		tk, err := h.tasks.Store().Create(title, "", "headless")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(h.proj.ID)}); err != nil {
			t.Fatal(err)
		}
		got, err := h.tasks.Get(tk.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	t1 := mk("fix: first task")
	if _, err := h.m.PrepareForTask(ctx, t1, nil); err != nil {
		t.Fatalf("PrepareForTask t1: %v", err)
	}
	t1, err := h.tasks.Get(t1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if t1.Branch == "" {
		t.Fatal("t1 branch not set after prepare")
	}

	t2 := mk("fix: second task")
	if _, err := h.tasks.Update(t2.ID, task.Update{Branch: task.Ptr(t1.Branch)}); err != nil {
		t.Fatal(err)
	}
	t2, err = h.tasks.Get(t2.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := h.m.PrepareForTask(ctx, t2, nil); err != nil {
		t.Fatalf("PrepareForTask t2 with colliding branch: %v", err)
	}
	t2, err = h.tasks.Get(t2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if t2.Branch == t1.Branch {
		t.Fatalf("t2 branch %q still collides with t1 — should have re-derived a unique branch", t2.Branch)
	}
}

func TestPrepareForTask_RerunsBootstrapOnExistingWorktreeReuse(t *testing.T) {
	counterPath := filepath.Join(t.TempDir(), "setup-count")
	h := prepareHarness(t, []string{fmt.Sprintf("printf x >> %s", strconv.Quote(counterPath))}, 30*time.Second)

	tk, err := h.tasks.Store().Create("reuse bootstrap task", "", "headless")
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

	firstPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("initial PrepareForTask: %v", err)
	}
	secondPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("reused PrepareForTask: %v", err)
	}
	if secondPath != firstPath {
		t.Fatalf("reused PrepareForTask path = %q, want %q", secondPath, firstPath)
	}

	count, err := os.ReadFile(counterPath)
	if err != nil {
		t.Fatalf("read setup counter: %v", err)
	}
	if got, want := string(count), "xx"; got != want {
		t.Fatalf("setup command ran %d times, want 2 (counter %q)", len(got), got)
	}
}

// TestPrepareForTask_MergesRepoAndAppSetup confirms that .sybra.yaml's
// `setup:` block runs first (canonical repo bootstrap) and then any
// machine-local SetupCommands are appended. Both must execute and the
// order must be repo → app so per-machine additions can depend on
// repo-installed tools.
func TestPrepareForTask_MergesRepoAndAppSetup(t *testing.T) {
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

	path, err := h.m.PrepareForTask(context.Background(), tk, nil)
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

// TestPrepareForTask_WorktreeBaseRefHead asserts that "head" mode branches from
// the current remote state, not the stale clone-time commit. Before the fix,
// refs/heads/<branch> in a bare clone was never updated after FetchOrigin, so
// selecting "head" silently produced worktrees rooted at the original clone SHA.
func TestPrepareForTask_WorktreeBaseRefHead(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	// Switch the project to head mode.
	if _, err := h.store.SetWorktreeBaseRef(h.proj.ID, project.WorktreeBaseRefHead); err != nil {
		t.Fatalf("SetWorktreeBaseRef: %v", err)
	}

	// Add a commit to the upstream src after the bare was cloned.
	if err := os.WriteFile(filepath.Join(h.src, "upstream.txt"), []byte("upstream\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunInDir(t, h.src, "git", "add", "upstream.txt")
	mustRunInDir(t, h.src, "git", "commit", "-m", "upstream progress")

	// Capture the new upstream SHA.
	rawSHA, err := exec.Command("git", "-C", h.src, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse src HEAD: %v", err)
	}
	wantSHA := strings.TrimSpace(string(rawSHA))

	tk, err := h.tasks.Store().Create("head-mode task", "", "headless")
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

	wtPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("PrepareForTask: %v", err)
	}

	// The upstream commit must be an ancestor of the worktree HEAD — confirming
	// that SyncLocalBranch carried the new commit into refs/heads/main before
	// the worktree branch was created.
	out, err := exec.Command("git", "-C", wtPath, "merge-base", wantSHA, "HEAD").Output()
	if err != nil {
		t.Fatalf("merge-base: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != wantSHA {
		t.Errorf("worktree does not contain upstream commit %s; merge-base=%s", wantSHA, got)
	}
}

func TestPrepareForTask_RebaseConflictFailsClosed(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	tk, err := h.tasks.Store().Create("conflicting task", "", "headless")
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

	wtPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("initial PrepareForTask: %v", err)
	}

	mustRunInDir(t, wtPath, "git", "config", "user.email", "test@test.com")
	mustRunInDir(t, wtPath, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("branch edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunInDir(t, wtPath, "git", "add", "README.md")
	mustRunInDir(t, wtPath, "git", "commit", "-m", "branch edit")

	if err := os.WriteFile(filepath.Join(h.src, "README.md"), []byte("upstream edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunInDir(t, h.src, "git", "add", "README.md")
	mustRunInDir(t, h.src, "git", "commit", "-m", "upstream edit")

	gotPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if !errors.Is(err, ErrRebaseFailed) {
		t.Fatalf("PrepareForTask error = %v, want ErrRebaseFailed", err)
	}
	if gotPath != "" {
		t.Fatalf("PrepareForTask path = %q, want empty path on rebase failure", gotPath)
	}
}

// TestPrepareForTask_RebaseSkipsWhenBaseAlreadyMerged reproduces the
// branch-conflict-fix recovery loop from task bdcc90a4: recoverBranchConflictNoPR
// resolves a rebase-block by merging base into the task's own branch (never a
// rebase) and pushing, then resumes the task's original interrupted step. That
// next step calls PrepareForTask again, which reuses the worktree and rebases
// it onto base via reconcileAndRebase. A plain `git rebase` linearizes
// history — it drops the merge commit and replays the branch's own pre-merge
// commit individually onto base — so it re-hits the exact same content
// conflict the merge just resolved, even though base's tip is fully contained
// in the branch. RebaseOnto must recognize base is already merged in (an
// ancestor of HEAD) and skip rebasing instead of looping forever.
func TestPrepareForTask_RebaseSkipsWhenBaseAlreadyMerged(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	tk, err := h.tasks.Store().Create("merged-then-rebased task", "", "headless")
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

	wtPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("initial PrepareForTask: %v", err)
	}

	mustRunInDir(t, wtPath, "git", "config", "user.email", "test@test.com")
	mustRunInDir(t, wtPath, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("branch edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunInDir(t, wtPath, "git", "add", "README.md")
	mustRunInDir(t, wtPath, "git", "commit", "-m", "branch edit")

	if err := os.WriteFile(filepath.Join(h.src, "README.md"), []byte("upstream edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunInDir(t, h.src, "git", "add", "README.md")
	mustRunInDir(t, h.src, "git", "commit", "-m", "upstream edit")

	// First re-prepare hits the genuine, unresolvable-via-rebase conflict —
	// same as TestPrepareForTask_RebaseConflictFailsClosed.
	if _, err := h.m.PrepareForTask(context.Background(), tk, nil); !errors.Is(err, ErrRebaseFailed) {
		t.Fatalf("PrepareForTask error = %v, want ErrRebaseFailed", err)
	}

	// Simulate recoverBranchConflictNoPR: fetch base, merge it into the
	// branch (never a rebase), resolve the conflict, commit, and push — this
	// is exactly what dispatchBranchConflictRecovery's pr-fix agent does.
	mustRunInDir(t, wtPath, "git", "fetch", "origin")
	mergeErr := exec.Command("git", "-C", wtPath, "merge", "--no-edit", "origin/main").Run()
	if mergeErr == nil {
		t.Fatal("expected the merge to stop for conflicts, got a clean merge")
	}
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("branch edit\nupstream edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunInDir(t, wtPath, "git", "add", "README.md")
	mustRunInDir(t, wtPath, "git", "commit", "--no-edit")
	branch := mustOutputInDir(t, wtPath, "git", "branch", "--show-current")
	mustRunInDir(t, wtPath, "git", "push", "origin", "HEAD:"+strings.TrimSpace(branch))

	// The resumed original step's dispatch re-prepares the same worktree. Before
	// the fix this hits the identical conflict again and returns
	// ErrRebaseFailed forever; it must now succeed since base is already merged in.
	gotPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("PrepareForTask after merge recovery should succeed, got err: %v", err)
	}
	if gotPath == "" {
		t.Fatal("PrepareForTask path is empty, want the recovered worktree path")
	}

	got, err := os.ReadFile(filepath.Join(gotPath, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "branch edit\nupstream edit\n"; string(got) != want {
		t.Fatalf("README.md = %q, want %q (the merge-resolved content, untouched by a skipped rebase)", got, want)
	}
}

// TestRecreateFromBase_DeletesPublishedBranch proves the exhausted-conflict
// recovery path discards the published task branch as well as the local one.
// Without the remote delete, the recreated fresh-base branch hits a non-fast-
// forward rejection on its next push and loops back into branch-conflict
// recovery forever.
func TestRecreateFromBase_DeletesPublishedBranch(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	tk, err := h.tasks.Store().Create("recreate branch from base", "", "headless")
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

	wtPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("initial PrepareForTask: %v", err)
	}
	branch := strings.TrimSpace(mustOutputInDir(t, wtPath, "git", "branch", "--show-current"))

	mustRunInDir(t, wtPath, "git", "config", "user.email", "test@test.com")
	mustRunInDir(t, wtPath, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(wtPath, "recreate.txt"), []byte("stale remote tip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunInDir(t, wtPath, "git", "add", "recreate.txt")
	mustRunInDir(t, wtPath, "git", "commit", "-m", "stale remote tip")
	mustRunInDir(t, wtPath, "git", "push", "origin", "HEAD:"+branch)
	staleSHA := strings.TrimSpace(mustOutputInDir(t, h.src, "git", "rev-parse", "--verify", "refs/heads/"+branch))

	if err := project.FetchOrigin(context.Background(), h.proj.ClonePath); err != nil {
		t.Fatalf("FetchOrigin after push: %v", err)
	}
	trackingRef := "refs/remotes/origin/" + branch
	if !project.RefExists(context.Background(), h.proj.ClonePath, trackingRef) {
		t.Fatalf("expected fetched tracking ref %s to exist before recreate", trackingRef)
	}

	if err := h.m.RecreateFromBase(context.Background(), tk); err != nil {
		t.Fatalf("RecreateFromBase: %v", err)
	}
	if project.BranchExists(context.Background(), h.proj.ClonePath, branch) {
		t.Fatalf("local branch %s still exists after recreate", branch)
	}
	if project.RefExists(context.Background(), h.proj.ClonePath, trackingRef) {
		t.Fatalf("tracking ref %s still exists after recreate", trackingRef)
	}
	if out, err := exec.Command("git", "-C", h.src, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).CombinedOutput(); err == nil {
		t.Fatalf("remote branch %s still exists after recreate", branch)
	} else {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Fatalf("verify remote branch deletion: %v: %s", err, out)
		}
	}
	backupSHA, ok := project.ResolveBareRef(context.Background(), h.proj.ClonePath, "refs/sybra-backup/"+branch)
	if !ok {
		t.Fatalf("backup ref for %s missing after recreate", branch)
	}
	if backupSHA != staleSHA {
		t.Fatalf("backup ref SHA = %s, want stale remote tip %s", backupSHA, staleSHA)
	}

	wtPath, err = h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("recreated PrepareForTask: %v", err)
	}
	mustRunInDir(t, wtPath, "git", "config", "user.email", "test@test.com")
	mustRunInDir(t, wtPath, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(wtPath, "recreate.txt"), []byte("fresh implementation\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunInDir(t, wtPath, "git", "add", "recreate.txt")
	mustRunInDir(t, wtPath, "git", "commit", "-m", "fresh implementation")
	mustRunInDir(t, wtPath, "git", "push", "origin", "HEAD:"+branch)

	pushedSHA := strings.TrimSpace(mustOutputInDir(t, wtPath, "git", "rev-parse", "HEAD"))
	remoteSHA := strings.TrimSpace(mustOutputInDir(t, h.src, "git", "rev-parse", "--verify", "refs/heads/"+branch))
	if remoteSHA != pushedSHA {
		t.Fatalf("remote branch SHA = %s, want pushed HEAD %s", remoteSHA, pushedSHA)
	}
	if remoteSHA == staleSHA {
		t.Fatalf("remote branch stayed on stale tip %s after fresh push", staleSHA)
	}
}

// TestRecreateFromBase_UnreachableForkWithoutTrackingRefDoesNotFail proves
// recreate still succeeds when the configured push remote is transiently
// unreachable but there is no fetched tracking ref for the task branch yet.
// In that shape there is no evidence of a published stale branch to clean up,
// so failing the entire recreate path would strand the task on a local-only
// cleanup problem.
func TestRecreateFromBase_UnreachableForkWithoutTrackingRefDoesNotFail(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	tk, err := h.tasks.Store().Create("recreate without reachable remote", "", "headless")
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

	wtPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("initial PrepareForTask: %v", err)
	}
	branch := strings.TrimSpace(mustOutputInDir(t, wtPath, "git", "branch", "--show-current"))

	if out, err := exec.Command(
		"git", "-c", "safe.bareRepository=all", "-C", h.proj.ClonePath,
		"remote", "add", "fork", "ssh://127.0.0.1:1/unreachable.git",
	).CombinedOutput(); err != nil {
		t.Fatalf("add unreachable fork remote: %v: %s", err, out)
	}

	if err := h.m.RecreateFromBase(context.Background(), tk); err != nil {
		t.Fatalf("RecreateFromBase with unreachable fork: %v", err)
	}
	if project.BranchExists(context.Background(), h.proj.ClonePath, branch) {
		t.Fatalf("local branch %s still exists after recreate", branch)
	}
	if _, statErr := os.Stat(wtPath); !os.IsNotExist(statErr) {
		t.Fatalf("worktree path %s still exists after recreate: %v", wtPath, statErr)
	}
	if _, ok := project.ResolveBareRef(context.Background(), h.proj.ClonePath, "refs/sybra-backup/"+branch); !ok {
		t.Fatalf("backup ref for %s missing after recreate", branch)
	}
}

// TestPrepareForTask_TransientFetchFailureIsNotRebaseFailed proves the fix for
// the bug where a network blip during reconcileAndRebase's remote fetch was
// indistinguishable from a genuine content conflict: both wrapped
// ErrRebaseFailed, so a transient SSH/DNS outage falsely parked a task
// human-required on an otherwise clean branch.
//
// reconcileAndRebase's fetch/ls-remote target PushRemote's chosen remote
// ("fork" when configured, else "origin"). Configuring an unreachable "fork"
// remote isolates the failure to that step alone: PrepareForTask's earlier
// "Fetching origin…" step (against the real local bare clone) keeps
// succeeding, and only the reconcile step hits a real, deterministic
// "Connection refused" transport error — reproducing the reported outage
// without any external network dependency.
func TestPrepareForTask_TransientFetchFailureIsNotRebaseFailed(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	tk, err := h.tasks.Store().Create("transient network task", "", "headless")
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

	wtPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("initial PrepareForTask: %v", err)
	}

	// Point the push remote at a port nothing listens on: fetch fails fast
	// with a real "Connection refused" transport error, exactly like the
	// reported outage, while origin's own fetch remains healthy.
	mustRunInDir(t, wtPath, "git", "remote", "add", "fork", "ssh://127.0.0.1:1/unreachable.git")

	gotPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if !errors.Is(err, ErrTransientFetch) {
		t.Fatalf("PrepareForTask error = %v, want ErrTransientFetch", err)
	}
	if errors.Is(err, ErrRebaseFailed) {
		t.Fatalf("PrepareForTask error = %v must not also classify as ErrRebaseFailed (would wrongly escalate to human-required)", err)
	}
	if gotPath != "" {
		t.Fatalf("PrepareForTask path = %q, want empty path on transient fetch failure", gotPath)
	}
}

// TestPrepareForTask_RebaseFailureRecoversViaMerge proves the merge fallback
// in reconcileAndRebase: a task branch whose commits, replayed individually,
// hit an intermediate patch-apply conflict against the new base — even though
// the branch's *net* content change doesn't actually overlap with upstream's
// edit — fails a plain rebase but succeeds via merge. The branch adds then
// reverts a line (net no-op) while upstream edits a different line the
// now-stale intermediate commit's patch context no longer matches; rebasing
// commit-by-commit conflicts on that mismatched context, but a single
// three-way merge of final states has nothing to reconcile.
func TestPrepareForTask_RebaseFailureRecoversViaMerge(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	tk, err := h.tasks.Store().Create("recoverable task", "", "headless")
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

	wtPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("initial PrepareForTask: %v", err)
	}

	mustRunInDir(t, wtPath, "git", "config", "user.email", "test@test.com")
	mustRunInDir(t, wtPath, "git", "config", "user.name", "Test")

	base, err := os.ReadFile(filepath.Join(wtPath, "README.md"))
	if err != nil {
		t.Fatal(err)
	}

	// Commit A: append a line — this is the commit whose patch context will
	// no longer match once upstream edits the same file. Clone base before
	// appending: append can mutate base's backing array in place if it has
	// spare capacity, which would corrupt commit B's revert below.
	withLine2 := append(append([]byte{}, base...), []byte("line2\n")...)
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), withLine2, 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunInDir(t, wtPath, "git", "add", "README.md")
	mustRunInDir(t, wtPath, "git", "commit", "-m", "add line2")

	// Commit B: revert it — net effect on the branch is byte-identical to
	// base, so a three-way merge against upstream has nothing to reconcile.
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), base, 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunInDir(t, wtPath, "git", "add", "README.md")
	mustRunInDir(t, wtPath, "git", "commit", "-m", "revert line2")

	// Upstream edits the same file commit A touched — unrelated to the
	// branch's net (no-op) change, but enough to break commit A's patch
	// context during a commit-by-commit rebase replay.
	upstream := append(append([]byte{}, base...), []byte("upstream addition\n")...)
	if err := os.WriteFile(filepath.Join(h.src, "README.md"), upstream, 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunInDir(t, h.src, "git", "add", "README.md")
	mustRunInDir(t, h.src, "git", "commit", "-m", "upstream edit")

	gotPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("PrepareForTask should recover via merge fallback, got err: %v", err)
	}
	if gotPath == "" {
		t.Fatal("PrepareForTask path is empty, want a recovered worktree path")
	}

	got, err := os.ReadFile(filepath.Join(gotPath, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, upstream) {
		t.Fatalf("README.md = %q, want upstream's content %q (branch's net change was a no-op)", got, upstream)
	}

	// The recovery must be a real merge commit, not a rebase — a subsequent
	// push must never need to force.
	out, err := exec.Command("git", "-C", gotPath, "log", "--merges", "-1", "--format=%H").Output()
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		t.Fatal("expected a merge commit on the recovered branch, found none")
	}
}

// TestPrepareForTask_ReuseRecreatesOnBranchMismatch reproduces the
// contributing factor from issue #1477: a reused worktree directory can be
// left checked out on a stale HEAD (e.g. a leftover detached HEAD from an
// interrupted rebase/heal attempt in a prior run) while still passing
// WorktreeHealthy. Reusing it as-is would let that stale HEAD get captured
// downstream as the tamper-detection baseline. PrepareForTask must instead
// detect the branch mismatch and recreate the worktree on the expected
// branch.
func TestPrepareForTask_ReuseRecreatesOnBranchMismatch(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	tk, err := h.tasks.Store().Create("branch mismatch task", "", "headless")
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

	wtPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("initial PrepareForTask: %v", err)
	}

	// Simulate a leftover detached HEAD from an interrupted prior run — still
	// a perfectly healthy git worktree, just not on the task's branch.
	mustRunInDir(t, wtPath, "git", "checkout", "--detach", "HEAD")
	if branch, err := project.CurrentBranch(context.Background(), wtPath); err != nil || branch != "" {
		t.Fatalf("precondition: expected detached HEAD, got branch=%q err=%v", branch, err)
	}

	gotPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("PrepareForTask after branch mismatch: %v", err)
	}

	got, err := project.CurrentBranch(context.Background(), gotPath)
	if err != nil {
		t.Fatalf("resolve branch: %v", err)
	}
	want := branchNameForTask(tk)
	if got != want {
		t.Fatalf("branch = %q, want %q — reused worktree must not stay on a stale HEAD", got, want)
	}
}

// TestPrepareForTask_RefusesReuseWhileAgentRunning proves the sybra#1495
// regression guard: a dispatcher that reaches PrepareForTask for a task
// whose worktree a tracked agent is still live in (e.g. a stale "no agent
// running" read racing ResumeStalled) must not rebase that worktree out from
// under the agent's in-flight edits. It must instead see ErrAgentRunning and
// leave the worktree untouched.
func TestPrepareForTask_RefusesReuseWhileAgentRunning(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	tk, err := h.tasks.Store().Create("live agent task", "", "headless")
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

	wtPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("initial PrepareForTask: %v", err)
	}
	preHEAD, err := project.CurrentCommit(context.Background(), wtPath)
	if err != nil {
		t.Fatalf("resolve pre-attempt HEAD: %v", err)
	}

	// Advance the upstream default branch so a rebase, if it ran, would be
	// observable (HEAD would move).
	if err := os.WriteFile(filepath.Join(h.src, "README.md"), []byte("upstream edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunInDir(t, h.src, "git", "add", "README.md")
	mustRunInDir(t, h.src, "git", "commit", "-m", "upstream edit")

	mWithLiveAgent := New(Config{
		WorktreesDir: h.wtDir,
		Projects:     h.store,
		Tasks:        h.tasks,
		Logger:       discardLogger(),
		LogsDir:      h.logsDir,
		AgentChecker: func(taskID string) bool { return taskID == tk.ID },
	})

	gotPath, err := mWithLiveAgent.PrepareForTask(context.Background(), tk, nil)
	if !errors.Is(err, ErrAgentRunning) {
		t.Fatalf("PrepareForTask error = %v, want ErrAgentRunning", err)
	}
	if gotPath != "" {
		t.Fatalf("PrepareForTask path = %q, want empty path when refusing reuse", gotPath)
	}

	postHEAD, err := project.CurrentCommit(context.Background(), wtPath)
	if err != nil {
		t.Fatalf("resolve post-attempt HEAD: %v", err)
	}
	if postHEAD != preHEAD {
		t.Fatalf("worktree HEAD changed from %q to %q — PrepareForTask must not touch a worktree with a live agent", preHEAD, postHEAD)
	}
}

// TestPrepareForTask_BadSlugRejected proves the use-site guard: even if a
// Task struct is constructed directly (bypassing the store and ParseBytes),
// PrepareForTask rejects a path-traversal slug before calling PathFor.
// This is defense-in-depth on top of the parse-time guard in ParseBytes.
func TestPrepareForTask_BadSlugRejected(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	badTask := task.Task{
		ID:        "abc12345678",
		Slug:      "../../etc/passwd",
		ProjectID: h.proj.ID,
		Status:    task.StatusTodo,
		AgentMode: "headless",
	}
	_, err := h.m.PrepareForTask(context.Background(), badTask, nil)
	if err == nil {
		t.Fatal("PrepareForTask with path-traversal slug: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "slug") {
		t.Errorf("error should mention slug; got: %v", err)
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

func mustOutputInDir(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
	return string(out)
}

// TestPrepareForTask_BootstrapFailureBlocks confirms a failing setup
// command aborts worktree preparation and surfaces the error to callers.
// Without this, an agent would start on a broken worktree and waste tokens
// hitting missing-tool errors.
func TestPrepareForTask_BootstrapFailureBlocks(t *testing.T) {
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

	_, err = h.m.PrepareForTask(context.Background(), tk, nil)
	if err == nil {
		t.Fatal("expected PrepareForTask to fail when bootstrap exits non-zero")
	}
	if !strings.Contains(err.Error(), "exit 42") {
		t.Errorf("error does not carry bootstrap command text: %v", err)
	}
}
