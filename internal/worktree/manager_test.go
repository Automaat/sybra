package worktree

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Automaat/synapse/internal/task"
)

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
