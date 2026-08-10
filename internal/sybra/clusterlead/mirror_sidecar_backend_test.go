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

// TestApplyFollowerTaskWritesPlanDraftFilesOnFileBackend is #3297's
// regression test: a follower's plan drafts must reach the leader on first
// adoption, both in the returned Task's PlanDrafts map (Merge) and as
// sidecar files on the file backend (writeSidecars) — before the fix,
// Merge never copied the field at all, so the leader's map stayed empty
// regardless of what writeSidecars did.
func TestApplyFollowerTaskWritesPlanDraftFilesOnFileBackend(t *testing.T) {
	cfg := leaderConfig("http://unused", []string{"owner/pet"})
	roster, _ := NewRoster(cfg, nil)
	mgr := newManager(t)
	mirror := NewMirror(cfg, mgr, roster, nil, time.Second)

	if !mirror.applyFollowerTask("pet-box", task.Task{
		ID: "task-abc123", Title: "legit", Status: task.StatusTodo,
		ProjectID: "owner/pet", UpdatedAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
		PlanDrafts: map[string]string{"alpha": "draft content"},
	}) {
		t.Fatal("a valid follower task must still adopt")
	}

	got, err := mgr.Get("task-abc123")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.PlanDrafts["alpha"] != "draft content" {
		t.Fatalf("PlanDrafts = %+v, want alpha=draft content", got.PlanDrafts)
	}

	draftFile := filepath.Join(mgr.Store().Dir(), "task-abc123.plan-draft-alpha.md")
	if data, err := os.ReadFile(draftFile); err != nil {
		t.Fatalf("plan draft sidecar file not written: %v", err)
	} else if string(data) != "draft content" {
		t.Fatalf("plan draft sidecar content = %q, want %q", data, "draft content")
	}
}

// TestApplyFollowerTaskMergeDropsPlanDraftsRemovedByFollower proves a later
// merge that reports a smaller draft set (e.g. the follower re-planned and
// deleted its old drafts) removes the sidecar file the leader no longer
// should be serving, not just add/update the ones still present.
func TestApplyFollowerTaskMergeDropsPlanDraftsRemovedByFollower(t *testing.T) {
	cfg := leaderConfig("http://unused", []string{"owner/pet"})
	roster, _ := NewRoster(cfg, nil)
	mgr := newManager(t)
	mirror := NewMirror(cfg, mgr, roster, nil, time.Second)

	if !mirror.applyFollowerTask("pet-box", task.Task{
		ID: "task-abc123", Title: "legit", Status: task.StatusTodo,
		ProjectID: "owner/pet", UpdatedAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
		PlanDrafts: map[string]string{"alpha": "first attempt", "beta": "first attempt"},
	}) {
		t.Fatal("first adopt must succeed")
	}
	dir := mgr.Store().Dir()
	if _, err := os.Stat(filepath.Join(dir, "task-abc123.plan-draft-beta.md")); err != nil {
		t.Fatalf("test setup: beta sidecar file missing: %v", err)
	}

	if !mirror.applyFollowerTask("pet-box", task.Task{
		ID: "task-abc123", Title: "legit", Status: task.StatusTodo,
		ProjectID: "owner/pet", UpdatedAt: time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC),
		PlanDrafts: map[string]string{"alpha": "second attempt"},
	}) {
		t.Fatal("later merge must succeed")
	}

	got, err := mgr.Get("task-abc123")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.PlanDrafts["alpha"] != "second attempt" {
		t.Fatalf("PlanDrafts[alpha] = %q, want %q", got.PlanDrafts["alpha"], "second attempt")
	}
	if _, ok := got.PlanDrafts["beta"]; ok {
		t.Fatalf("PlanDrafts = %+v, want no beta entry after the follower dropped it", got.PlanDrafts)
	}
	if _, err := os.Stat(filepath.Join(dir, "task-abc123.plan-draft-beta.md")); !os.IsNotExist(err) {
		t.Fatalf("beta sidecar file still exists after the follower dropped it: err=%v", err)
	}
}

// TestApplyFollowerTaskPlanDraftsRoundTripOnDatabaseBackend mirrors
// TestApplyFollowerTaskSkipsSidecarFilesOnDatabaseBackend for PlanDrafts:
// the map must reach the database via Merge's own field copy (PutBy then
// persists it through SidecarsFromTask) with no stray sidecar file written
// to the local tasks directory writeSidecars is skipped for.
func TestApplyFollowerTaskPlanDraftsRoundTripOnDatabaseBackend(t *testing.T) {
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
			PlanDrafts: map[string]string{"alpha": "draft content"},
		}) {
			t.Fatal("a valid follower task must still adopt")
		}

		got, err := mgr.Get("task-abc123")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.PlanDrafts["alpha"] != "draft content" {
			t.Fatalf("PlanDrafts = %+v, want alpha=draft content", got.PlanDrafts)
		}

		entries, err := os.ReadDir(fileStore.Dir())
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
