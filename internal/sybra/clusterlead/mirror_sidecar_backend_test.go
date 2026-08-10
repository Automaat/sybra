package clusterlead

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/task/taskdb"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

// TestApplyFollowerTaskWritesSidecarFilesOnFileBackend pins writeSidecars'
// existing behavior for the file backend: PutFn's plain whole-task write
// never touches sidecar files, so the mirror's compensating direct write is
// still required and must still happen.
func TestApplyFollowerTaskWritesSidecarFilesOnFileBackend(t *testing.T) {
	cfg := leaderConfig("http://unused", []string{"owner/pet"})
	roster, _ := NewRoster(cfg, nil)
	mgr := newManager(t)
	mirror := NewMirror(cfg, mgr, roster, nil, time.Second)

	if !mirror.applyFollowerTask("pet-box", task.Task{
		ID: "task-abc123", Title: "legit", Status: task.StatusTodo,
		ProjectID: "owner/pet", UpdatedAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
		Plan: "plan body", CodeReview: "review body",
	}) {
		t.Fatal("a valid follower task must still adopt")
	}

	dir := mgr.Store().Dir()
	planFile := filepath.Join(dir, "task-abc123.plan.md")
	if data, err := os.ReadFile(planFile); err != nil {
		t.Fatalf("plan sidecar file not written: %v", err)
	} else if string(data) != "plan body" {
		t.Fatalf("plan sidecar content = %q, want %q", data, "plan body")
	}
	reviewFile := filepath.Join(dir, "task-abc123.review.md")
	if data, err := os.ReadFile(reviewFile); err != nil {
		t.Fatalf("code-review sidecar file not written: %v", err)
	} else if string(data) != "review body" {
		t.Fatalf("code-review sidecar content = %q, want %q", data, "review body")
	}
}

// TestApplyFollowerTaskSkipsSidecarFilesOnDatabaseBackend proves the DB
// backend both gets the sidecar content (already persisted by PutBy's own
// SidecarsFromTask, in the same transaction as the task write) and does NOT
// also get a stray, never-read sidecar file written to the local tasks
// directory the way it would before this fix.
func TestApplyFollowerTaskSkipsSidecarFilesOnDatabaseBackend(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		sqlStore, err := taskdb.NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		fileStore, err := task.NewStore(t.TempDir())
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		mgr := task.NewManagerWithPersistence(fileStore, taskdb.NewPersistence(sqlStore), nil)
		if mgr.PersistsToFile() {
			t.Fatal("test setup: expected a DB-backed Manager")
		}

		cfg := leaderConfig("http://unused", []string{"owner/pet"})
		roster, _ := NewRoster(cfg, nil)
		mirror := NewMirror(cfg, mgr, roster, nil, time.Second)

		if !mirror.applyFollowerTask("pet-box", task.Task{
			ID: "task-abc123", Title: "legit", Status: task.StatusTodo,
			ProjectID: "owner/pet", UpdatedAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
			Plan: "plan body", CodeReview: "review body",
		}) {
			t.Fatal("a valid follower task must still adopt")
		}

		got, err := mgr.Get("task-abc123")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Plan != "plan body" {
			t.Fatalf("Plan = %q, want %q (must already be persisted by PutBy, not the skipped writeSidecars)", got.Plan, "plan body")
		}
		if got.CodeReview != "review body" {
			t.Fatalf("CodeReview = %q, want %q", got.CodeReview, "review body")
		}

		dir := fileStore.Dir()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read tasks dir: %v", err)
		}
		if len(entries) != 0 {
			names := make([]string, len(entries))
			for i, e := range entries {
				names[i] = e.Name()
			}
			t.Fatalf("tasks dir has stray files on the DB backend: %v", names)
		}
	})
}
