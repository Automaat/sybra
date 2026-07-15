package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
)

// makeDoctorWorktree creates a real git repo at path with one committed
// file; dirty additionally leaves an uncommitted change.
func makeDoctorWorktree(t *testing.T, path string, dirty bool) {
	t.Helper()
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@test.local")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@test.local")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	runGit(t, path, "init")
	if err := os.WriteFile(filepath.Join(path, "f.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	runGit(t, path, "add", "-A")
	runGit(t, path, "commit", "-m", "init")
	if dirty {
		if err := os.WriteFile(filepath.Join(path, "f.txt"), []byte("b"), 0o644); err != nil {
			t.Fatalf("dirty file: %v", err)
		}
	}
}

func writeDoctorFile(t *testing.T, path string, size int, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// putDoctorTask writes a fully-formed task straight to the store, bypassing
// the CLI, so tests can set an arbitrary StatusChangedAt without waiting out
// a real retention window.
func putDoctorTask(t *testing.T, cfg *config.Config, tk task.Task) {
	t.Helper()
	store, err := task.NewStore(cfg.TasksDir)
	if err != nil {
		t.Fatalf("open task store: %v", err)
	}
	if _, err := store.Put(tk); err != nil {
		t.Fatalf("put task %s: %v", tk.ID, err)
	}
}

// setupDoctorHome sets up an isolated SYBRA_HOME with a 1-hour sandbox
// retention window (config default is 24h — too slow for tests) and returns
// the resolved config.
func setupDoctorHome(t *testing.T) *config.Config {
	t.Helper()
	setupStore(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Sandbox.RetentionHours = 1
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
	cfg, err = config.Load()
	if err != nil {
		t.Fatalf("config.Load (reload): %v", err)
	}
	return cfg
}

func TestDoctorCleanupDryRunReportsWithoutDeleting(t *testing.T) {
	setupDoctorHome(t)
	now := time.Now()

	orphanSandbox := filepath.Join(config.HomeDir(), "sandboxes", "orphan01")
	writeDoctorFile(t, filepath.Join(orphanSandbox, "f"), 10, now)

	code, out := runCLI(t, "doctor", "cleanup")
	if code != 0 {
		t.Fatalf("doctor cleanup exit = %d, want 0 (output: %q)", code, out)
	}
	if !strings.Contains(out, "sandboxes") {
		t.Fatalf("expected the sandboxes bucket in the human report, got %q", out)
	}
	if !strings.Contains(out, "Dry run") {
		t.Fatalf("expected a dry-run notice, got %q", out)
	}
	if _, err := os.Stat(orphanSandbox); err != nil {
		t.Fatalf("dry-run must not delete anything: %v", err)
	}
}

func TestDoctorCleanupJSONShape(t *testing.T) {
	setupDoctorHome(t)

	code, out := runCLI(t, "--json", "doctor", "cleanup")
	if code != 0 {
		t.Fatalf("doctor cleanup exit = %d, want 0 (output: %q)", code, out)
	}
	var report doctorCleanupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, out)
	}
	if report.Applied {
		t.Fatal("Applied must be false for a dry run")
	}
	if report.Buckets == nil {
		t.Fatal("Buckets must be present (possibly empty) in the JSON shape")
	}
	if len(report.Results) != 0 {
		t.Fatalf("Results must be empty for a dry run, got %+v", report.Results)
	}
}

func TestDoctorCleanupApplyCleansOnlySafeBucketsByDefault(t *testing.T) {
	cfg := setupDoctorHome(t)
	now := time.Now()

	orphanSandbox := filepath.Join(config.HomeDir(), "sandboxes", "orphan02")
	writeDoctorFile(t, filepath.Join(orphanSandbox, "f"), 10, now)

	doneWorktree := filepath.Join(cfg.WorktreesDir, "deadbeef")
	makeDoctorWorktree(t, doneWorktree, false)
	putDoctorTask(t, cfg, task.Task{ID: "deadbeef", Title: "t", Status: task.StatusDone, StatusChangedAt: now.Add(-100 * time.Hour)})

	code, out := runCLI(t, "doctor", "cleanup", "--apply")
	if code != 0 {
		t.Fatalf("doctor cleanup --apply exit = %d, want 0 (output: %q)", code, out)
	}

	if _, err := os.Stat(orphanSandbox); !os.IsNotExist(err) {
		t.Fatalf("expected the orphaned sandbox (safe bucket) to be removed, stat err = %v", err)
	}
	if _, err := os.Stat(doneWorktree); err != nil {
		t.Fatalf("expected the done task's worktree (destructive, ungated) to survive: %v", err)
	}
}

func TestDoctorCleanupApplyForceWorktreesSurvivesActiveTask(t *testing.T) {
	cfg := setupDoctorHome(t)
	now := time.Now()

	activeWorktree := filepath.Join(cfg.WorktreesDir, "aaaaaaaa")
	makeDoctorWorktree(t, activeWorktree, true) // dirty, but irrelevant: task is active
	putDoctorTask(t, cfg, task.Task{ID: "aaaaaaaa", Title: "active", Status: task.StatusInProgress, StatusChangedAt: now})

	doneWorktree := filepath.Join(cfg.WorktreesDir, "bbbbbbbb")
	makeDoctorWorktree(t, doneWorktree, false)
	putDoctorTask(t, cfg, task.Task{ID: "bbbbbbbb", Title: "done", Status: task.StatusDone, StatusChangedAt: now.Add(-100 * time.Hour)})

	code, out := runCLI(t, "doctor", "cleanup", "--apply", "--force", "--worktrees")
	if code != 0 {
		t.Fatalf("doctor cleanup --apply --force --worktrees exit = %d, want 0 (output: %q)", code, out)
	}

	if _, err := os.Stat(activeWorktree); err != nil {
		t.Fatalf("expected the active task's worktree to survive: %v", err)
	}
	if _, err := os.Stat(doneWorktree); !os.IsNotExist(err) {
		t.Fatalf("expected the done task's worktree to be removed, stat err = %v", err)
	}
}

func TestDoctorCleanupUnknownOnlyExitsTwo(t *testing.T) {
	setupDoctorHome(t)

	code, out := runCLI(t, "doctor", "cleanup", "--only", "not-a-real-bucket")
	if code != 2 {
		t.Fatalf("doctor cleanup --only <bogus> exit = %d, want 2 (output: %q)", code, out)
	}
}

func TestDoctorCleanupInvalidOlderThanExitsTwo(t *testing.T) {
	setupDoctorHome(t)

	code, out := runCLI(t, "doctor", "cleanup", "--older-than", "not-a-duration")
	if code != 2 {
		t.Fatalf("doctor cleanup --older-than <bogus> exit = %d, want 2 (output: %q)", code, out)
	}
}

func TestDoctorCleanupWorktreesBucketRequiresGateFlag(t *testing.T) {
	cfg := setupDoctorHome(t)
	now := time.Now()

	doneWorktree := filepath.Join(cfg.WorktreesDir, "cccccccc")
	makeDoctorWorktree(t, doneWorktree, false)
	putDoctorTask(t, cfg, task.Task{ID: "cccccccc", Title: "done", Status: task.StatusDone, StatusChangedAt: now.Add(-100 * time.Hour)})

	code, out := runCLI(t, "--json", "doctor", "cleanup", "--only", "worktrees")
	if code != 0 {
		t.Fatalf("doctor cleanup exit = %d, want 0 (output: %q)", code, out)
	}
	var report doctorCleanupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, out)
	}
	if len(report.Buckets) != 0 {
		t.Fatalf("worktrees bucket must not appear without --worktrees, got %+v", report.Buckets)
	}
}

func TestDoctorCleanupSharedCacheRequiresExternal(t *testing.T) {
	setupDoctorHome(t)
	now := time.Now()

	goModCache := filepath.Join(config.HomeDir(), "shared-cache", "go-mod", "cache.txt")
	npmCache := filepath.Join(config.HomeDir(), "shared-cache", "npm", "cache.txt")
	writeDoctorFile(t, goModCache, 10, now)
	writeDoctorFile(t, npmCache, 12, now)

	code, out := runCLI(t, "--json", "doctor", "cleanup", "--only", "shared-cache")
	if code != 0 {
		t.Fatalf("doctor cleanup exit = %d, want 0 (output: %q)", code, out)
	}
	var report doctorCleanupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, out)
	}
	if len(report.Buckets) != 0 {
		t.Fatalf("shared-cache bucket must not appear without --external, got %+v", report.Buckets)
	}

	code, out = runCLI(t, "--json", "doctor", "cleanup", "--apply", "--only", "shared-cache", "--external")
	if code != 0 {
		t.Fatalf("doctor cleanup --apply --only shared-cache --external exit = %d, want 0 (output: %q)", code, out)
	}
	for _, path := range []string{goModCache, npmCache} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected shared-cache file to be removed with --external: %s stat err = %v", path, err)
		}
	}
}

func TestDoctorCleanupApplyPartialFailureExitsOne(t *testing.T) {
	setupDoctorHome(t)
	now := time.Now()

	sandboxRoot := filepath.Join(config.HomeDir(), "sandboxes")
	orphan := filepath.Join(sandboxRoot, "orphan03")
	writeDoctorFile(t, filepath.Join(orphan, "f"), 10, now)

	// Deny write on the parent dir so os.RemoveAll cannot unlink the orphan
	// entry itself, forcing a genuine delete error (not merely a skip).
	if err := os.Chmod(sandboxRoot, 0o555); err != nil {
		t.Fatalf("chmod sandboxes dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(sandboxRoot, 0o755) })

	code, out := runCLI(t, "--json", "doctor", "cleanup", "--apply", "--only", "sandboxes")
	if code != 1 {
		t.Fatalf("doctor cleanup --apply exit = %d, want 1 on partial failure (output: %q)", code, out)
	}
	var report doctorCleanupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, out)
	}
	if len(report.Results) != 1 || len(report.Results[0].Errors) == 0 {
		t.Fatalf("expected a recorded delete error, got %+v", report.Results)
	}
}
