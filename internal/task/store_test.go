package task

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/workflow"
)

func TestNewStore(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "tasks")
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store == nil {
		t.Fatal("store is nil")
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("not a directory")
	}
}

func TestStoreCreate(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	task, err := store.Create("Test task", "Body content", "headless")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if task.ID == "" {
		t.Error("ID is empty")
	}
	if task.Title != "Test task" {
		t.Errorf("Title = %q, want %q", task.Title, "Test task")
	}
	if task.Body != "Body content" {
		t.Errorf("Body = %q, want %q", task.Body, "Body content")
	}
	if task.AgentMode != "headless" {
		t.Errorf("AgentMode = %q, want %q", task.AgentMode, "headless")
	}
	if task.Status != StatusTodo {
		t.Errorf("Status = %q, want %q", task.Status, StatusTodo)
	}
	if task.FilePath == "" {
		t.Error("FilePath is empty")
	}

	if _, err := os.Stat(task.FilePath); err != nil {
		t.Errorf("file not written: %v", err)
	}
}

func TestStoreListEmpty(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	tasks, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected empty list, got %d", len(tasks))
	}
}

func TestStoreListMultiple(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for _, title := range []string{"Task A", "Task B", "Task C"} {
		if _, err := store.Create(title, "", "headless"); err != nil {
			t.Fatal(err)
		}
	}

	tasks, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 3 {
		t.Errorf("got %d tasks, want 3", len(tasks))
	}
}

func TestStoreListIgnoresNonMarkdown(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.Create("Real task", "", "headless"); err != nil {
		t.Fatal(err)
	}
	// Write a non-markdown file
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a task"), 0o644); err != nil {
		t.Fatal(err)
	}

	tasks, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Errorf("got %d tasks, want 1", len(tasks))
	}
}

func TestStoreGet(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Find me", "body", "interactive")
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("ID = %q, want %q", got.ID, created.ID)
	}
	if got.Title != "Find me" {
		t.Errorf("Title = %q, want %q", got.Title, "Find me")
	}
}

func TestStoreGetNotFound(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Get("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
}

func TestStoreUpdate(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Original", "original body", "headless")
	if err != nil {
		t.Fatal(err)
	}

	updated, err := store.Update(created.ID, Update{
		Title:  Ptr("Updated"),
		Status: Ptr(StatusDone),
		Body:   Ptr("new body"),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	if updated.Title != "Updated" {
		t.Errorf("Title = %q, want %q", updated.Title, "Updated")
	}
	if updated.Status != StatusDone {
		t.Errorf("Status = %q, want %q", updated.Status, StatusDone)
	}
	if updated.Body != "new body" {
		t.Errorf("Body = %q, want %q", updated.Body, "new body")
	}
	if !updated.UpdatedAt.After(created.UpdatedAt) {
		t.Errorf("UpdatedAt = %v, want after create time %v", updated.UpdatedAt, created.UpdatedAt)
	}

	// Verify persisted
	reloaded, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Title != "Updated" {
		t.Errorf("persisted Title = %q, want %q", reloaded.Title, "Updated")
	}
}

func TestStoreDelete(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Delete me", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Delete(created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := os.Stat(created.FilePath); !os.IsNotExist(err) {
		t.Error("file should be removed after delete")
	}

	_, err = store.Get(created.ID)
	if err == nil {
		t.Fatal("expected error after deleting task")
	}
}

func TestStoreWriteLocksAreReclaimed(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Lock lifecycle", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(created.ID, Update{Body: Ptr("updated")}); err != nil {
		t.Fatalf("update: %v", err)
	}

	if lockCount := store.locker.Len(); lockCount != 0 {
		t.Fatalf("locker entries after update = %d, want 0", lockCount)
	}

	if err := store.Delete(created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if lockCount := store.locker.Len(); lockCount != 0 {
		t.Fatalf("locker entries after delete = %d, want 0", lockCount)
	}
}

func TestStoreDeleteNotFound(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Delete("nonexistent"); err == nil {
		t.Fatal("expected error for nonexistent task")
	}
}

func TestStoreUpdateTags(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Tagged task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	updated, err := store.Update(created.ID, Update{
		Tags: Ptr([]string{"backend", "auth"}),
	})
	if err != nil {
		t.Fatalf("update tags: %v", err)
	}

	if len(updated.Tags) != 2 {
		t.Fatalf("Tags len = %d, want 2", len(updated.Tags))
	}
	if updated.Tags[0] != "backend" || updated.Tags[1] != "auth" {
		t.Errorf("Tags = %v, want [backend auth]", updated.Tags)
	}

	// Verify persisted
	reloaded, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Tags) != 2 {
		t.Errorf("persisted Tags len = %d, want 2", len(reloaded.Tags))
	}
}

func TestStoreUpdateAgentMode(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Mode task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	updated, err := store.Update(created.ID, Update{
		AgentMode: Ptr("interactive"),
	})
	if err != nil {
		t.Fatalf("update agent_mode: %v", err)
	}
	if updated.AgentMode != "interactive" {
		t.Errorf("AgentMode = %q, want %q", updated.AgentMode, "interactive")
	}
}

func TestStoreCreate_InvalidAgentMode(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("Bad mode", "", "supervised"); err == nil {
		t.Fatal("expected error for invalid agent_mode")
	}
}

func TestStoreUpdate_InvalidAgentMode(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create("Mode task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(created.ID, Update{AgentMode: Ptr("supervised")}); err == nil {
		t.Fatal("expected error for invalid agent_mode update")
	}
	if _, err := store.UpdateMap(created.ID, map[string]any{"agent_mode": "supervised"}); err == nil {
		t.Fatal("expected error for invalid agent_mode UpdateMap")
	}
}

func TestStoreUpdateProjectID(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Project task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	updated, err := store.Update(created.ID, Update{
		ProjectID: Ptr("owner/repo"),
	})
	if err != nil {
		t.Fatalf("update project_id: %v", err)
	}
	if updated.ProjectID != "owner/repo" {
		t.Errorf("ProjectID = %q, want %q", updated.ProjectID, "owner/repo")
	}

	reloaded, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ProjectID != "owner/repo" {
		t.Errorf("persisted ProjectID = %q, want %q", reloaded.ProjectID, "owner/repo")
	}
}

func TestStoreUpdateStatusHumanRequired(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Blocked task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	updated, err := store.Update(created.ID, Update{
		Status:       Ptr(StatusHumanRequired),
		StatusReason: Ptr("agent failed with errors"),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Status != StatusHumanRequired {
		t.Errorf("Status = %q, want %q", updated.Status, StatusHumanRequired)
	}
	if updated.StatusReason != "agent failed with errors" {
		t.Errorf("StatusReason = %q, want %q", updated.StatusReason, "agent failed with errors")
	}

	reloaded, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != StatusHumanRequired {
		t.Errorf("persisted Status = %q, want %q", reloaded.Status, StatusHumanRequired)
	}
	if reloaded.StatusReason != "agent failed with errors" {
		t.Errorf("persisted StatusReason = %q, want %q", reloaded.StatusReason, "agent failed with errors")
	}

	// Verify reason clears when status changes without explicit reason
	updated2, err := store.Update(created.ID, Update{Status: Ptr(StatusInProgress)})
	if err != nil {
		t.Fatalf("update2: %v", err)
	}
	if updated2.StatusReason != "" {
		t.Errorf("StatusReason after status change = %q, want empty", updated2.StatusReason)
	}
}

// TestStoreUpdateTestingCycleStartedAt_AutoStamp verifies that UpdateWithPrev
// automatically stamps TestingCycleStartedAt when a task moves out of
// human-required. This covers all callers (CLI, GUI, engine) uniformly.
func TestStoreUpdateTestingCycleStartedAt_AutoStamp(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Redispatch task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	// Move to human-required (no cycle stamp set yet).
	_, err = store.Update(created.ID, Update{Status: Ptr(StatusHumanRequired)})
	if err != nil {
		t.Fatalf("set human-required: %v", err)
	}
	before, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.TestingCycleStartedAt != nil {
		t.Error("expected TestingCycleStartedAt to be nil before re-dispatch")
	}

	// Human re-dispatches: move out of human-required.
	after, err := store.Update(created.ID, Update{Status: Ptr(StatusTesting)})
	if err != nil {
		t.Fatalf("set testing: %v", err)
	}
	if after.TestingCycleStartedAt == nil {
		t.Fatal("expected TestingCycleStartedAt to be set after human-required → testing transition")
	}

	// Reload from disk to confirm persistence.
	reloaded, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.TestingCycleStartedAt == nil {
		t.Error("TestingCycleStartedAt not persisted after human-required → testing transition")
	}
	if reloaded.TestingCycleStartedAt.IsZero() {
		t.Error("TestingCycleStartedAt is zero time")
	}
}

// TestStoreUpdateTestingCycleStartedAt_NoAutoStampOnNonHumanRequired verifies
// that the auto-stamp only fires for human-required → other transitions, not
// for normal workflow-internal transitions (e.g. in-progress → testing).
func TestStoreUpdateTestingCycleStartedAt_NoAutoStampOnNonHumanRequired(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Normal cycle task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	// Workflow-internal transition: in-progress → testing (auto-loop, not human re-dispatch).
	_, err = store.Update(created.ID, Update{Status: Ptr(StatusInProgress)})
	if err != nil {
		t.Fatalf("set in-progress: %v", err)
	}
	after, err := store.Update(created.ID, Update{Status: Ptr(StatusTesting)})
	if err != nil {
		t.Fatalf("set testing: %v", err)
	}
	if after.TestingCycleStartedAt != nil {
		t.Error("TestingCycleStartedAt must not be set for non-human-required → testing transitions")
	}
}

func TestStoreUpdateNotFound(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Update("nonexistent", Update{Title: Ptr("x")})
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
}

func TestStoreAddRun(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Run task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	run := AgentRun{
		AgentID:   "agent-001",
		Mode:      "headless",
		State:     "running",
		StartedAt: time.Now().UTC(),
		Prompt:    "# Task: Foo\n\nDo the thing\n\n---\n\nuser prompt",
	}

	if err := store.AddRun(created.ID, run); err != nil {
		t.Fatalf("AddRun: %v", err)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.AgentRuns) != 1 {
		t.Fatalf("AgentRuns len = %d, want 1", len(got.AgentRuns))
	}
	if got.AgentRuns[0].AgentID != "agent-001" {
		t.Errorf("AgentID = %q, want %q", got.AgentRuns[0].AgentID, "agent-001")
	}
	if got.AgentRuns[0].State != "running" {
		t.Errorf("State = %q, want %q", got.AgentRuns[0].State, "running")
	}
	if got.AgentRuns[0].Prompt != run.Prompt {
		t.Errorf("Prompt = %q, want %q", got.AgentRuns[0].Prompt, run.Prompt)
	}
}

func TestStoreAddRunMultiple(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Multi run", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	for i := range 3 {
		run := AgentRun{
			AgentID: fmt.Sprintf("agent-%d", i),
			Mode:    "headless",
			State:   "done",
		}
		if err := store.AddRun(created.ID, run); err != nil {
			t.Fatalf("AddRun %d: %v", i, err)
		}
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.AgentRuns) != 3 {
		t.Fatalf("AgentRuns len = %d, want 3", len(got.AgentRuns))
	}
}

func TestStoreAddRunNotFound(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	err = store.AddRun("nonexistent", AgentRun{AgentID: "x"})
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
}

func TestStoreAddRunWithStatus(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Run task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := store.AddRunWithStatus(created.ID, AgentRun{
		AgentID: "agent-001",
		Mode:    "headless",
		State:   "running",
	}, Ptr(StatusInProgress)); err != nil {
		t.Fatalf("AddRunWithStatus: %v", err)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusInProgress {
		t.Fatalf("Status = %q, want %q", got.Status, StatusInProgress)
	}
	if got.StatusChangedAt.Before(created.StatusChangedAt) {
		t.Fatalf("StatusChangedAt = %s, want at or after create timestamp %s", got.StatusChangedAt, created.StatusChangedAt)
	}
	if len(got.AgentRuns) != 1 {
		t.Fatalf("AgentRuns len = %d, want 1", len(got.AgentRuns))
	}
}

func TestStoreUpdateBackfillsLegacyStatusChangedAt(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Legacy", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	legacyUpdatedAt := time.Date(2026, 7, 9, 8, 30, 0, 0, time.UTC)
	writeLegacyTaskWithoutStatusChangedAt(t, created.FilePath, created.ID, StatusInProgress, legacyUpdatedAt)

	tags := []string{"medium", "touched"}
	got, prev, err := store.UpdateWithPrev(created.ID, Update{Tags: &tags})
	if err != nil {
		t.Fatalf("UpdateWithPrev: %v", err)
	}
	if prev != StatusInProgress {
		t.Fatalf("prev status = %q, want %q", prev, StatusInProgress)
	}
	if !got.StatusChangedAt.Equal(legacyUpdatedAt) {
		t.Fatalf("StatusChangedAt = %s, want legacy UpdatedAt %s", got.StatusChangedAt, legacyUpdatedAt)
	}
	if !got.UpdatedAt.After(legacyUpdatedAt) {
		t.Fatalf("UpdatedAt = %s, want refreshed after %s", got.UpdatedAt, legacyUpdatedAt)
	}
}

func TestStoreAddRunBackfillsLegacyStatusChangedAt(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Legacy run", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	legacyUpdatedAt := time.Date(2026, 7, 9, 8, 45, 0, 0, time.UTC)
	writeLegacyTaskWithoutStatusChangedAt(t, created.FilePath, created.ID, StatusInProgress, legacyUpdatedAt)

	if err := store.AddRun(created.ID, AgentRun{
		AgentID: "agent-legacy",
		Mode:    "headless",
		State:   "running",
	}); err != nil {
		t.Fatalf("AddRun: %v", err)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.StatusChangedAt.Equal(legacyUpdatedAt) {
		t.Fatalf("StatusChangedAt = %s, want legacy UpdatedAt %s", got.StatusChangedAt, legacyUpdatedAt)
	}
	if len(got.AgentRuns) != 1 {
		t.Fatalf("AgentRuns len = %d, want 1", len(got.AgentRuns))
	}
}

// TestStoreListBackfillsLegacyStatusChangedAtForNonTerminalStatus guards
// against the false-positive lost_agent regression: a legacy in-progress
// task with no StatusChangedAt and no intervening Update/AddRun call is
// only ever observed through List() (the read path monitor scan uses), so
// List must backfill it there rather than leaving it permanently zero.
func TestStoreListBackfillsLegacyStatusChangedAtForNonTerminalStatus(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Legacy in-progress", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	legacyUpdatedAt := time.Date(2026, 7, 9, 8, 45, 0, 0, time.UTC)
	writeLegacyTaskWithoutStatusChangedAt(t, created.FilePath, created.ID, StatusInProgress, legacyUpdatedAt)

	tasks, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var got *Task
	for i := range tasks {
		if tasks[i].ID == created.ID {
			got = &tasks[i]
		}
	}
	if got == nil {
		t.Fatalf("task %s not found in List() result", created.ID)
	}
	if got.StatusChangedAt.IsZero() {
		t.Fatal("StatusChangedAt is still zero after List(), want backfilled from legacy UpdatedAt")
	}
	if !got.StatusChangedAt.Equal(legacyUpdatedAt) {
		t.Fatalf("StatusChangedAt = %s, want legacy UpdatedAt %s", got.StatusChangedAt, legacyUpdatedAt)
	}
	if got.ClosedAt != nil {
		t.Fatalf("ClosedAt = %v, want nil for non-terminal status", got.ClosedAt)
	}

	// Re-read from a fresh store (cache bypassed) to confirm the backfill
	// was persisted to disk, not just returned in-memory.
	reopened, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := reopened.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.StatusChangedAt.Equal(legacyUpdatedAt) {
		t.Fatalf("persisted StatusChangedAt = %s, want %s", persisted.StatusChangedAt, legacyUpdatedAt)
	}
	if !persisted.UpdatedAt.Equal(legacyUpdatedAt) {
		t.Fatalf("persisted UpdatedAt = %s, want legacy UpdatedAt %s", persisted.UpdatedAt, legacyUpdatedAt)
	}
}

func TestStoreUpdateRun(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Update run", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	run := AgentRun{
		AgentID: "agent-upd",
		Mode:    "headless",
		State:   "running",
	}
	if err := store.AddRun(created.ID, run); err != nil {
		t.Fatal(err)
	}

	err = store.UpdateRun(created.ID, "agent-upd", RunPatch{
		State:                  Ptr("done"),
		CostUSD:                Ptr(0.42),
		PremiumRequests:        Ptr(1.5),
		Result:                 Ptr("success"),
		TestOutcome:            Ptr("product_bug"),
		TestFailureFingerprint: Ptr("abc123"),
	})
	if err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.AgentRuns) != 1 {
		t.Fatalf("AgentRuns len = %d, want 1", len(got.AgentRuns))
	}
	r := got.AgentRuns[0]
	if r.State != "done" {
		t.Errorf("State = %q, want %q", r.State, "done")
	}
	if r.CostUSD != 0.42 {
		t.Errorf("CostUSD = %f, want 0.42", r.CostUSD)
	}
	if r.PremiumRequests != 1.5 {
		t.Errorf("PremiumRequests = %f, want 1.5", r.PremiumRequests)
	}
	if r.Result != "success" {
		t.Errorf("Result = %q, want %q", r.Result, "success")
	}
	if r.TestOutcome != "product_bug" {
		t.Errorf("TestOutcome = %q, want product_bug", r.TestOutcome)
	}
	if r.TestFailureFingerprint != "abc123" {
		t.Errorf("TestFailureFingerprint = %q, want abc123", r.TestFailureFingerprint)
	}
}

func writeLegacyTaskWithoutStatusChangedAt(t *testing.T, path, id string, status Status, updatedAt time.Time) {
	t.Helper()
	createdAt := updatedAt.Add(-1 * time.Hour)
	body := fmt.Sprintf(`---
id: %s
title: Legacy task
status: %s
agent_mode: headless
tags: [medium]
created_at: %s
updated_at: %s
---
legacy body
`, id, status, createdAt.Format(time.RFC3339), updatedAt.Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write legacy task: %v", err)
	}
}

func TestStoreUpdateRunPayloadRoundTrip(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Update run payload", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	run := AgentRun{
		AgentID:  "agent-payload",
		Role:     "implementation",
		Mode:     "headless",
		State:    "running",
		Provider: "claude",
		Model:    "old-model",
	}
	if err := store.AddRun(created.ID, run); err != nil {
		t.Fatal(err)
	}

	patch := RunPatch{
		State:                  Ptr("done"),
		Outcome:                Ptr(RunOutcomeSuccess),
		CostUSD:                Ptr(1.23),
		PremiumRequests:        Ptr(2.5),
		Result:                 Ptr("completed with result"),
		Verdict:                Ptr("sybra_bug"),
		VerdictRendered:        Ptr(true),
		LogFile:                Ptr("/tmp/sybra/agent-payload.ndjson"),
		Provider:               Ptr("codex"),
		Model:                  Ptr("gpt-5"),
		ExperimentID:           Ptr("exp-123"),
		VariantID:              Ptr("variant-b"),
		AssignmentUnit:         Ptr("task"),
		AssignmentKey:          Ptr("task-abc123"),
		ReasoningEffort:        Ptr("high"),
		SessionID:              Ptr("session-123"),
		ProtocolViolation:      Ptr("missing-json"),
		TestOutcome:            Ptr("product_bug"),
		TestFailureFingerprint: Ptr("fingerprint-123"),
		HeadSHA:                Ptr("0123456789abcdef0123456789abcdef01234567"),
	}
	assertRunPatchCoversEveryField(t, patch)

	if err := store.UpdateRun(created.ID, "agent-payload", patch); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := Parse(got.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.AgentRuns) != 1 {
		t.Fatalf("AgentRuns len = %d, want 1", len(reloaded.AgentRuns))
	}

	assertAgentRunPayload(t, reloaded.AgentRuns[0], AgentRun{
		AgentID:                "agent-payload",
		Role:                   "implementation",
		Mode:                   "headless",
		Provider:               "codex",
		Model:                  "gpt-5",
		ExperimentID:           "exp-123",
		VariantID:              "variant-b",
		AssignmentUnit:         "task",
		AssignmentKey:          "task-abc123",
		ReasoningEffort:        "high",
		State:                  "done",
		Outcome:                RunOutcomeSuccess,
		CostUSD:                1.23,
		PremiumRequests:        2.5,
		Result:                 "completed with result",
		Verdict:                "sybra_bug",
		VerdictRendered:        true,
		LogFile:                "/tmp/sybra/agent-payload.ndjson",
		SessionID:              "session-123",
		ProtocolViolation:      "missing-json",
		TestOutcome:            "product_bug",
		TestFailureFingerprint: "fingerprint-123",
		HeadSHA:                "0123456789abcdef0123456789abcdef01234567",
	})
}

func assertRunPatchCoversEveryField(t *testing.T, patch RunPatch) {
	t.Helper()

	value := reflect.ValueOf(patch)
	for _, field := range reflect.VisibleFields(value.Type()) {
		if value.FieldByIndex(field.Index).IsNil() {
			t.Errorf("RunPatch field %s is not covered by payload round-trip test", field.Name)
		}
	}
}

func assertAgentRunPayload(t *testing.T, got, want AgentRun) {
	t.Helper()

	if got.AgentID != want.AgentID {
		t.Errorf("AgentID = %q, want %q", got.AgentID, want.AgentID)
	}
	if got.Role != want.Role {
		t.Errorf("Role = %q, want %q", got.Role, want.Role)
	}
	if got.Mode != want.Mode {
		t.Errorf("Mode = %q, want %q", got.Mode, want.Mode)
	}
	if got.Provider != want.Provider {
		t.Errorf("Provider = %q, want %q", got.Provider, want.Provider)
	}
	if got.Model != want.Model {
		t.Errorf("Model = %q, want %q", got.Model, want.Model)
	}
	if got.ExperimentID != want.ExperimentID {
		t.Errorf("ExperimentID = %q, want %q", got.ExperimentID, want.ExperimentID)
	}
	if got.VariantID != want.VariantID {
		t.Errorf("VariantID = %q, want %q", got.VariantID, want.VariantID)
	}
	if got.AssignmentUnit != want.AssignmentUnit {
		t.Errorf("AssignmentUnit = %q, want %q", got.AssignmentUnit, want.AssignmentUnit)
	}
	if got.AssignmentKey != want.AssignmentKey {
		t.Errorf("AssignmentKey = %q, want %q", got.AssignmentKey, want.AssignmentKey)
	}
	if got.ReasoningEffort != want.ReasoningEffort {
		t.Errorf("ReasoningEffort = %q, want %q", got.ReasoningEffort, want.ReasoningEffort)
	}
	if got.State != want.State {
		t.Errorf("State = %q, want %q", got.State, want.State)
	}
	if got.CostUSD != want.CostUSD {
		t.Errorf("CostUSD = %f, want %f", got.CostUSD, want.CostUSD)
	}
	if got.PremiumRequests != want.PremiumRequests {
		t.Errorf("PremiumRequests = %f, want %f", got.PremiumRequests, want.PremiumRequests)
	}
	if got.Result != want.Result {
		t.Errorf("Result = %q, want %q", got.Result, want.Result)
	}
	if got.Verdict != want.Verdict {
		t.Errorf("Verdict = %q, want %q", got.Verdict, want.Verdict)
	}
	if got.VerdictRendered != want.VerdictRendered {
		t.Errorf("VerdictRendered = %t, want %t", got.VerdictRendered, want.VerdictRendered)
	}
	if got.LogFile != want.LogFile {
		t.Errorf("LogFile = %q, want %q", got.LogFile, want.LogFile)
	}
	if got.SessionID != want.SessionID {
		t.Errorf("SessionID = %q, want %q", got.SessionID, want.SessionID)
	}
	if got.ProtocolViolation != want.ProtocolViolation {
		t.Errorf("ProtocolViolation = %q, want %q", got.ProtocolViolation, want.ProtocolViolation)
	}
	if got.TestOutcome != want.TestOutcome {
		t.Errorf("TestOutcome = %q, want %q", got.TestOutcome, want.TestOutcome)
	}
	if got.TestFailureFingerprint != want.TestFailureFingerprint {
		t.Errorf("TestFailureFingerprint = %q, want %q", got.TestFailureFingerprint, want.TestFailureFingerprint)
	}
	if got.HeadSHA != want.HeadSHA {
		t.Errorf("HeadSHA = %q, want %q", got.HeadSHA, want.HeadSHA)
	}
}

func TestStoreUpdateRunNotFound(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	err = store.UpdateRun("nonexistent", "agent-x", RunPatch{State: Ptr("done")})
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
}

func TestStoreUpdateRunNoMatchingAgent(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("No match", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddRun(created.ID, AgentRun{AgentID: "agent-a", State: "running"}); err != nil {
		t.Fatal(err)
	}

	err = store.UpdateRun(created.ID, "agent-wrong", RunPatch{State: Ptr("done")})
	if err == nil {
		t.Fatal("expected error for wrong agent")
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentRuns[0].State != "running" {
		t.Errorf("State should be unchanged, got %q", got.AgentRuns[0].State)
	}
}

func TestStoreUpdateRunSessionID(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Session ID task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddRun(created.ID, AgentRun{AgentID: "agent-s", Mode: "headless", State: "running"}); err != nil {
		t.Fatal(err)
	}

	err = store.UpdateRun(created.ID, "agent-s", RunPatch{
		State:     Ptr("done"),
		SessionID: Ptr("ses-abc123"),
	})
	if err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentRuns[0].SessionID != "ses-abc123" {
		t.Errorf("SessionID = %q, want %q", got.AgentRuns[0].SessionID, "ses-abc123")
	}

	// Verify YAML round-trip persists session_id
	reloaded, err := Parse(got.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.AgentRuns[0].SessionID != "ses-abc123" {
		t.Errorf("reloaded SessionID = %q, want %q", reloaded.AgentRuns[0].SessionID, "ses-abc123")
	}
}

func TestStoreUpdateRunSessionIDEmpty(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Session ID empty task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddRun(created.ID, AgentRun{
		AgentID:   "agent-e",
		Mode:      "headless",
		State:     "running",
		SessionID: "existing-id",
		HeadSHA:   "existing-sha",
	}); err != nil {
		t.Fatal(err)
	}

	// Empty durable values should not overwrite existing values.
	err = store.UpdateRun(created.ID, "agent-e", RunPatch{
		State:     Ptr("done"),
		SessionID: Ptr(""),
		HeadSHA:   Ptr(""),
	})
	if err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentRuns[0].SessionID != "existing-id" {
		t.Errorf("SessionID = %q, want existing-id preserved", got.AgentRuns[0].SessionID)
	}
	if got.AgentRuns[0].HeadSHA != "existing-sha" {
		t.Errorf("HeadSHA = %q, want existing-sha preserved", got.AgentRuns[0].HeadSHA)
	}
}

func TestStoreListSkipsPlanSidecars(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	task, err := store.Create("Real task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	// Write plan, plan-critique, and code-review sidecars
	for _, name := range []string{
		task.ID + ".plan.md",
		task.ID + ".plan-critique.md",
		task.ID + ".plan-research.md",
		task.ID + ".plan-decisions.md",
		task.ID + ".plan-brief.md",
		task.ID + ".review.md",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("# plan content"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tasks, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Errorf("got %d tasks, want 1 (sidecars must be skipped)", len(tasks))
	}
	if tasks[0].ID != task.ID {
		t.Errorf("task ID = %q, want %q", tasks[0].ID, task.ID)
	}
}

func TestStoreCreateFullAppliesInitialWorkflowFields(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	tags := []string{"handoff", "handoff-review"}
	status := StatusReadyReview
	plan := "# Plan\n\nReview the existing branch."
	created, err := store.CreateFull("Review task", "Body", "headless", Update{
		Status:      &status,
		Tags:        &tags,
		ProjectID:   Ptr("owner/repo"),
		WorktreeDir: Ptr("/tmp/worktree"),
		Plan:        &plan,
	})
	if err != nil {
		t.Fatalf("CreateFull: %v", err)
	}

	if created.Status != StatusReadyReview {
		t.Errorf("Status = %q, want %q", created.Status, StatusReadyReview)
	}
	if created.ProjectID != "owner/repo" {
		t.Errorf("ProjectID = %q, want owner/repo", created.ProjectID)
	}
	if created.WorktreeDir != "/tmp/worktree" {
		t.Errorf("WorktreeDir = %q, want /tmp/worktree", created.WorktreeDir)
	}
	if created.Plan != plan {
		t.Errorf("Plan = %q, want %q", created.Plan, plan)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Plan != plan {
		t.Errorf("Get Plan = %q, want %q", got.Plan, plan)
	}
	if got.WorktreeDir != "/tmp/worktree" {
		t.Errorf("Get WorktreeDir = %q, want /tmp/worktree", got.WorktreeDir)
	}
}

func TestStoreCreateDefaultMode(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Default mode", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if created.AgentMode != "interactive" {
		t.Errorf("AgentMode = %q, want %q", created.AgentMode, "interactive")
	}
}

func TestStoreListSkipsMalformed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Create a valid task
	if _, err := store.Create("Valid", "", "headless"); err != nil {
		t.Fatal(err)
	}
	// Write a malformed markdown file
	if err := os.WriteFile(filepath.Join(dir, "bad.md"), []byte("not valid frontmatter"), 0o644); err != nil {
		t.Fatal(err)
	}

	tasks, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Errorf("got %d tasks, want 1 (should skip malformed)", len(tasks))
	}
}

func TestStoreGetInvalidatePathRefreshesExternalEdit(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Original", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.Get(created.ID); err != nil {
		t.Fatalf("prime cache: %v", err)
	}

	created.Title = "Edited on disk"
	data, err := Marshal(created)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(created.FilePath, data, 0o644); err != nil {
		t.Fatalf("write edited task: %v", err)
	}

	store.InvalidatePath(created.FilePath)

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("get after invalidate: %v", err)
	}
	if got.Title != "Edited on disk" {
		t.Fatalf("Title = %q, want %q", got.Title, "Edited on disk")
	}
}

func TestStoreListInvalidatePathRefreshesSidecar(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.List(); err != nil {
		t.Fatalf("prime cache: %v", err)
	}

	planPath := filepath.Join(dir, created.ID+".plan.md")
	if err := os.WriteFile(planPath, []byte("# refreshed plan"), 0o644); err != nil {
		t.Fatalf("write plan: %v", err)
	}

	store.InvalidatePath(planPath)

	tasks, err := store.List()
	if err != nil {
		t.Fatalf("list after invalidate: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(tasks))
	}
	if tasks[0].Plan != "# refreshed plan" {
		t.Fatalf("Plan = %q, want %q", tasks[0].Plan, "# refreshed plan")
	}
}

func TestStoreListInvalidatePathRefreshesJSONSidecar(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.List(); err != nil {
		t.Fatalf("prime cache: %v", err)
	}

	contractPath := filepath.Join(dir, created.ID+".plan-contract.json")
	contract := `{"task_id":"` + created.ID + `"}`
	if err := os.WriteFile(contractPath, []byte(contract), 0o644); err != nil {
		t.Fatalf("write contract: %v", err)
	}

	store.InvalidatePath(contractPath)

	tasks, err := store.List()
	if err != nil {
		t.Fatalf("list after invalidate: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(tasks))
	}
	if tasks[0].PlanContract != contract {
		t.Fatalf("PlanContract = %q, want %q", tasks[0].PlanContract, contract)
	}
}

func TestStoreListReturnedSliceDoesNotMutateCache(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Original", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	listed, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	listed[0].Title = "Mutated caller copy"
	listed[0].Tags = append(listed[0].Tags, "mutated")

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "Original" {
		t.Fatalf("Title = %q, want %q", got.Title, "Original")
	}
	if len(got.Tags) != 0 {
		t.Fatalf("Tags = %v, want empty", got.Tags)
	}
}

// TestStoreConcurrentCreate verifies that N goroutines calling Create in
// parallel each produce a distinct, readable task — no ID collision,
// no lost writes, no race in the list cache. Run with -race to catch
// data races on s.listCache and s.listValid.
func TestStoreConcurrentCreate(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	const n = 50
	var wg sync.WaitGroup
	ids := make(chan string, n)
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			tk, cerr := store.Create(fmt.Sprintf("task-%d", i), "body", "headless")
			if cerr != nil {
				errs <- cerr
				return
			}
			ids <- tk.ID
		})
	}
	wg.Wait()
	close(ids)
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent create: %v", err)
	}

	seen := map[string]bool{}
	for id := range ids {
		if id == "" {
			t.Error("empty ID returned from Create")
			continue
		}
		if seen[id] {
			t.Errorf("ID collision: %q", id)
		}
		seen[id] = true
	}
	if len(seen) != n {
		t.Errorf("got %d unique ids, want %d", len(seen), n)
	}

	tasks, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != n {
		t.Errorf("List returned %d tasks, want %d", len(tasks), n)
	}

	// Every created ID must be retrievable via Get — proves the file write
	// survived the concurrent cache updates.
	for id := range seen {
		if _, err := store.Get(id); err != nil {
			t.Errorf("Get(%q) after concurrent create: %v", id, err)
		}
	}
}

// TestStoreSafePathRejectsTraversal verifies that Get/Delete/Update reject
// task IDs that would escape the store directory. The CLI passes raw user
// input as task IDs, and agents (which call sybra-cli with IDs scraped from
// prompts) form an untrusted-input edge — without this check, an ID like
// `../../etc/passwd` would resolve to `/etc/passwd.md` and Get could read /
// Delete could remove arbitrary `.md` files outside the tasks dir.
func TestStoreSafePathRejectsTraversal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Plant a real task file outside the store dir to prove the safePath
	// check protects it.
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("---\nid: outside\ntitle: outside\n---\nsensitive"), 0o644); err != nil {
		t.Fatal(err)
	}
	relative := "../" + filepath.Base(filepath.Dir(outside)) + "/outside"

	for _, badID := range []string{"../../etc/passwd", "/etc/passwd", "../escape", relative} {
		t.Run(badID, func(t *testing.T) {
			t.Parallel()
			if _, err := store.Get(badID); err == nil {
				t.Errorf("Get(%q) accepted traversal id; should reject", badID)
			}
			if err := store.Delete(badID); err == nil {
				t.Errorf("Delete(%q) accepted traversal id; should reject", badID)
			}
		})
	}
	// Confirm the planted outside file is intact — the test setup itself
	// shouldn't have touched it, but a regression that broke safePath would
	// have removed it via the Delete attempts above.
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("outside file was touched by the safePath bypass: %v", err)
	}
}

// TestStoreConcurrentUpdateSameTask verifies that two goroutines updating the
// same task concurrently never leave the file corrupted (unparseable) or lose
// independently updated fields. The test also checks that the on-disk file
// parses successfully after the race.
func TestStoreConcurrentUpdateSameTask(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("orig", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}

	const rounds = 100
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range rounds {
			if _, err := store.Update(created.ID, Update{Title: Ptr(fmt.Sprintf("A-%d", i))}); err != nil {
				t.Errorf("writer A: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := range rounds {
			if _, err := store.Update(created.ID, Update{Body: Ptr(fmt.Sprintf("B-%d", i))}); err != nil {
				t.Errorf("writer B: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	// The file must parse cleanly — atomic-write guarantees no half-written
	// content. A regression that dropped the rename-over-write would leave
	// a torn file here.
	final, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after concurrent updates: %v", err)
	}
	if final.ID != created.ID {
		t.Errorf("ID changed across updates: got %q, want %q", final.ID, created.ID)
	}
	if final.Title != "A-99" {
		t.Errorf("Title = %q, want A-99", final.Title)
	}
	if final.Body != "B-99" {
		t.Errorf("Body = %q, want B-99", final.Body)
	}

	// Parse the raw file to confirm on-disk consistency independent of cache.
	reloaded, err := Parse(final.FilePath)
	if err != nil {
		t.Fatalf("raw parse: %v — file is torn", err)
	}
	if reloaded.ID != created.ID {
		t.Errorf("reloaded ID = %q, want %q", reloaded.ID, created.ID)
	}
}

func TestStoreConcurrentUpdateAndUpdateRunSameTask(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("orig", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddRun(created.ID, AgentRun{AgentID: "agent-1", State: "queued"}); err != nil {
		t.Fatalf("AddRun: %v", err)
	}

	const rounds = 100
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range rounds {
			if _, err := store.Update(created.ID, Update{Body: Ptr(fmt.Sprintf("body-%d", i))}); err != nil {
				t.Errorf("Update body: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := range rounds {
			state := fmt.Sprintf("state-%d", i)
			if err := store.UpdateRun(created.ID, "agent-1", RunPatch{State: &state}); err != nil {
				t.Errorf("UpdateRun state: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	final, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after concurrent updates: %v", err)
	}
	if final.Body != "body-99" {
		t.Errorf("Body = %q, want body-99", final.Body)
	}
	if len(final.AgentRuns) != 1 {
		t.Fatalf("AgentRuns length = %d, want 1", len(final.AgentRuns))
	}
	if final.AgentRuns[0].State != "state-99" {
		t.Errorf("AgentRuns[0].State = %q, want state-99", final.AgentRuns[0].State)
	}
}

func TestClosedAtTransitions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		from       Status
		to         Status
		wantNil    bool
		wantChange bool // whether ClosedAt pointer value should change
	}{
		{name: "todo→done stamps", from: StatusTodo, to: StatusDone, wantNil: false, wantChange: true},
		{name: "todo→cancelled stamps", from: StatusTodo, to: StatusCancelled, wantNil: false, wantChange: true},
		{name: "done→in-progress clears", from: StatusDone, to: StatusInProgress, wantNil: true, wantChange: true},
		{name: "done→cancelled preserves", from: StatusDone, to: StatusCancelled, wantNil: false, wantChange: false},
		{name: "title edit preserves", from: StatusDone, to: StatusDone, wantNil: false, wantChange: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			task, err := store.Create("t", "", "headless")
			if err != nil {
				t.Fatal(err)
			}
			// Move to 'from' status first.
			if tc.from != StatusTodo {
				task, err = store.Update(task.ID, Update{Status: Ptr(tc.from)})
				if err != nil {
					t.Fatalf("setup to %q: %v", tc.from, err)
				}
			}
			origClosedAt := task.ClosedAt

			if tc.to == tc.from {
				// Title-only edit — status unchanged.
				task, err = store.Update(task.ID, Update{Title: Ptr("updated title")})
			} else {
				task, err = store.Update(task.ID, Update{Status: Ptr(tc.to)})
			}
			if err != nil {
				t.Fatalf("update to %q: %v", tc.to, err)
			}

			if tc.wantNil && task.ClosedAt != nil {
				t.Errorf("ClosedAt = %v, want nil", task.ClosedAt)
			}
			if !tc.wantNil && task.ClosedAt == nil {
				t.Error("ClosedAt is nil, want non-nil")
			}
			switch {
			case tc.wantChange:
				if origClosedAt == task.ClosedAt && (origClosedAt != nil && origClosedAt.Equal(*task.ClosedAt)) {
					t.Error("ClosedAt pointer unchanged, expected change")
				}
			case IsTerminalStatus(tc.from) && task.ClosedAt == nil:
				t.Error("ClosedAt cleared unexpectedly on both-terminal transition")
			case IsTerminalStatus(tc.from) && origClosedAt != nil && task.ClosedAt != nil &&
				!origClosedAt.Equal(*task.ClosedAt):
				t.Errorf("ClosedAt changed on both-terminal transition: %v → %v", origClosedAt, task.ClosedAt)
			}
		})
	}
}

func TestLegacyMigrationStampsClosedAt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Create task and force status=done without ClosedAt (simulate legacy file).
	tk, err := store.Create("legacy", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	// Directly write a done-status file with no closed_at field.
	tk.Status = StatusDone
	tk.ClosedAt = nil
	data, err := Marshal(tk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tk.FilePath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	store.invalidateListCache()

	tasks, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.ID == tk.ID {
			if task.ClosedAt == nil {
				t.Error("legacy migration: ClosedAt still nil after List()")
			}
			if !task.ClosedAt.Equal(task.UpdatedAt) {
				t.Errorf("legacy migration: ClosedAt=%v, want UpdatedAt=%v", task.ClosedAt, task.UpdatedAt)
			}
			return
		}
	}
	t.Fatal("task not found in List()")
}

func TestCloneTaskClosedAtNonAliased(t *testing.T) {
	t.Parallel()
	ts := time.Now().UTC()
	orig := Task{
		ID:        "x",
		ClosedAt:  &ts,
		CreatedAt: ts,
		UpdatedAt: ts,
	}
	clone := cloneTask(orig)
	if clone.ClosedAt == orig.ClosedAt {
		t.Error("clone.ClosedAt shares pointer with original")
	}
	if !clone.ClosedAt.Equal(*orig.ClosedAt) {
		t.Errorf("clone.ClosedAt=%v, want %v", clone.ClosedAt, orig.ClosedAt)
	}
	// Mutating clone must not affect original.
	newTs := ts.Add(time.Hour)
	*clone.ClosedAt = newTs
	if orig.ClosedAt.Equal(newTs) {
		t.Error("mutating clone.ClosedAt affected original")
	}
}

func TestClosedAtYAMLRoundTrip(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.Create("roundtrip", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	done, err := store.Update(task.ID, Update{Status: Ptr(StatusDone)})
	if err != nil {
		t.Fatal(err)
	}
	if done.ClosedAt == nil {
		t.Fatal("ClosedAt not set")
	}
	reloaded, err := Parse(done.FilePath)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if reloaded.ClosedAt == nil {
		t.Fatal("ClosedAt lost on parse")
	}
	if !reloaded.ClosedAt.Equal(*done.ClosedAt) {
		t.Errorf("ClosedAt mismatch: got %v, want %v", *reloaded.ClosedAt, *done.ClosedAt)
	}
	// Nil case: active task has no ClosedAt.
	active, err := store.Create("active", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	reloaded2, err := Parse(active.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded2.ClosedAt != nil {
		t.Errorf("active task should have nil ClosedAt, got %v", reloaded2.ClosedAt)
	}
}

// TestCloneWorkflowDoesNotAliasParallelInflight guards against a regression
// where cloneWorkflow shallow-copied the Execution struct and left the
// ParallelInflight map (and its nested *ChildStatus values) shared between
// the cache entry and the caller-visible clone. The shallow copy meant a
// caller that mutated tasks[i].Workflow.ParallelInflight on a List() result
// silently corrupted listCache, and the next List() observed the torn map.
func TestCloneWorkflowDoesNotAliasParallelInflight(t *testing.T) {
	t.Parallel()
	orig := workflow.Execution{
		WorkflowID:  "wf",
		CurrentStep: "parent",
		State:       workflow.ExecRunning,
		ParallelInflight: map[string]*workflow.ParallelChildren{
			"parent": {
				ParentStepID: "parent",
				Children: map[string]*workflow.ChildStatus{
					"a": {Status: "pending", Output: "untouched"},
					"b": {Status: "pending"},
				},
			},
		},
	}

	clone := cloneWorkflow(orig)

	if &clone.ParallelInflight == &orig.ParallelInflight {
		t.Fatal("clone.ParallelInflight aliases the original map header (impossible: maps are reference types but headers differ across struct copies)")
	}

	// Mutate the clone's outer map: must not affect the original.
	cloneParent := clone.ParallelInflight["parent"]
	if cloneParent == nil {
		t.Fatal("clone: parent entry missing")
		return
	}
	origParent := orig.ParallelInflight["parent"]
	if origParent == nil {
		t.Fatal("orig: parent entry missing")
		return
	}
	cloneChildA := cloneParent.Children["a"]
	if cloneChildA == nil {
		t.Fatal("clone: child 'a' missing")
		return
	}
	cloneChildA.Status = "completed"
	origChildA := origParent.Children["a"]
	if origChildA == nil {
		t.Fatal("orig: child 'a' missing after clone mutation")
		return
	}
	if origChildA.Status != "pending" {
		t.Errorf("clone mutation of ChildStatus leaked to original: status=%q", origChildA.Status)
	}

	// Replace a child entry on the clone — must not appear on the original.
	cloneParent.Children["c"] = &workflow.ChildStatus{Status: "completed"}
	if _, ok := origParent.Children["c"]; ok {
		t.Error("clone added child key 'c' that leaked to original Children map")
	}

	// Replace the parent entry on the clone — must not appear on the original.
	clone.ParallelInflight["other"] = &workflow.ParallelChildren{ParentStepID: "other"}
	if _, ok := orig.ParallelInflight["other"]; ok {
		t.Error("clone added parent key 'other' that leaked to original ParallelInflight map")
	}
}

// TestCloneWorkflowDoesNotAliasStepCounts guards against cloneWorkflow
// sharing the StepCounts map (persistent replan/retry counters, see
// workflow.Execution.StepCounts) between the cache entry and the
// caller-visible clone — the same aliasing class as ParallelInflight above.
func TestCloneWorkflowDoesNotAliasStepCounts(t *testing.T) {
	t.Parallel()
	orig := workflow.Execution{
		WorkflowID:  "wf",
		CurrentStep: "start_replan",
		State:       workflow.ExecRunning,
		StepCounts:  map[string]int{"start_replan": 2},
	}

	clone := cloneWorkflow(orig)

	clone.StepCounts["start_replan"] = 99
	clone.StepCounts["other"] = 1
	if orig.StepCounts["start_replan"] != 2 {
		t.Errorf("clone mutation leaked to original: start_replan=%d", orig.StepCounts["start_replan"])
	}
	if _, ok := orig.StepCounts["other"]; ok {
		t.Error("clone added key 'other' that leaked to original StepCounts map")
	}
}

// TestStoreListMutateClonedParallelInflightDoesNotAffectCache exercises the
// real path: List() returns clones whose ParallelInflight is independent
// of the Store's cache. Mutating a returned clone must not change what a
// subsequent List() observes.
func TestStoreListMutateClonedParallelInflightDoesNotAffectCache(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tk, err := store.Create("p", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	wf := workflow.Execution{
		WorkflowID:  "wf",
		CurrentStep: "parent",
		State:       workflow.ExecRunning,
		ParallelInflight: map[string]*workflow.ParallelChildren{
			"parent": {
				ParentStepID: "parent",
				Children: map[string]*workflow.ChildStatus{
					"a": {Status: "pending"},
				},
			},
		},
	}
	wfPtr := &wf
	if _, err := store.Update(tk.ID, Update{Workflow: &wfPtr}); err != nil {
		t.Fatal(err)
	}

	first, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Workflow == nil {
		t.Fatalf("unexpected list result: %+v", first)
	}
	// Mutate the returned clone's ChildStatus directly.
	firstParent := first[0].Workflow.ParallelInflight["parent"]
	if firstParent == nil {
		t.Fatal("first: parent entry missing")
		return
	}
	firstChildA := firstParent.Children["a"]
	if firstChildA == nil {
		t.Fatal("first: child 'a' missing")
		return
	}
	firstChildA.Status = "MUTATED-BY-CALLER"
	firstParent.Children["b"] = &workflow.ChildStatus{Status: "MUTATED"}

	second, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].Workflow == nil {
		t.Fatalf("unexpected second list: %+v", second)
	}
	secondParent := second[0].Workflow.ParallelInflight["parent"]
	if secondParent == nil {
		t.Fatal("second: parent entry missing")
		return
	}
	secondChildA := secondParent.Children["a"]
	if secondChildA == nil {
		t.Fatal("second: child 'a' missing")
		return
	}
	if secondChildA.Status != "pending" {
		t.Errorf("cache corrupted: ChildStatus.Status = %q, want %q", secondChildA.Status, "pending")
	}
	if _, ok := secondParent.Children["b"]; ok {
		t.Error("cache corrupted: caller-added child key 'b' visible on second List()")
	}
}

func TestClosedAtLegacyMigration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Manually write a task file with status done but no closed_at field.
	legacyContent := `---
id: legacy01
title: Legacy done task
status: done
agent_mode: headless
allowed_tools: []
tags: []
created_at: 2025-01-01T10:00:00Z
updated_at: 2025-06-01T12:00:00Z
---
body
`
	if err := os.WriteFile(filepath.Join(dir, "legacy01.md"), []byte(legacyContent), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].ClosedAt == nil {
		t.Fatal("legacy task ClosedAt should be migrated")
	}
	want := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	if !tasks[0].ClosedAt.Equal(want) {
		t.Errorf("ClosedAt = %v, want %v", *tasks[0].ClosedAt, want)
	}
}

// TestStoreGet_PathTraversalSlugRejected is the regression guard for the
// attack path: a task file edited outside Sybra with slug: ../../etc/passwd
// must be rejected by the store on load, before the slug can propagate to
// worktree path construction. Without the ParseBytes guard, Store.Get returns
// the task and PrepareForTask computes a path outside the worktrees dir.
func TestStoreGet_PathTraversalSlugRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	tk, err := store.Create("test task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	const badSlug = "../../etc/passwd"
	malicious := "---\nid: " + tk.ID + "\ntitle: test task\nstatus: todo\nslug: " + badSlug + "\nagent_mode: headless\n---\n"
	taskPath := filepath.Join(store.dir, tk.ID+".md")
	if err := os.WriteFile(taskPath, []byte(malicious), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = store.Get(tk.ID)
	if err == nil {
		t.Fatal("Store.Get with path-traversal slug: expected error, got nil")
	}

	// List must also skip the malformed file (it logs and continues).
	tasks, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, task := range tasks {
		if task.ID == tk.ID {
			t.Errorf("malformed-slug task appeared in List: slug=%q", task.Slug)
		}
	}
}

// TestStoreListInvalidatePathTargetedRefresh verifies that an external edit to
// one task file patches just that task in the warm list cache while leaving
// the other cached tasks intact, and that a vanished file is dropped.
func TestStoreListInvalidatePathTargetedRefresh(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := store.Create("Alpha", "a", "headless")
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Create("Bravo", "b", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); err != nil { // prime cache
		t.Fatalf("prime: %v", err)
	}

	// External edit of Alpha's title.
	edited := "---\nid: " + a.ID + "\ntitle: Alpha Edited\nstatus: todo\nagent_mode: headless\ncreated_at: " +
		a.CreatedAt.Format("2006-01-02T15:04:05Z07:00") + "\nupdated_at: " +
		a.UpdatedAt.Format("2006-01-02T15:04:05Z07:00") + "\n---\na\n"
	if err := os.WriteFile(a.FilePath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	store.InvalidatePath(a.FilePath)

	tasks, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Task{}
	for _, tk := range tasks {
		byID[tk.ID] = tk
	}
	if byID[a.ID].Title != "Alpha Edited" {
		t.Fatalf("Alpha title = %q, want refreshed", byID[a.ID].Title)
	}
	if byID[b.ID].Title != "Bravo" {
		t.Fatalf("Bravo missing/changed: %q", byID[b.ID].Title)
	}

	// Vanished file is dropped from the cache.
	if err := os.Remove(a.FilePath); err != nil {
		t.Fatal(err)
	}
	store.InvalidatePath(a.FilePath)
	tasks, err = store.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range tasks {
		if tk.ID == a.ID {
			t.Fatalf("Alpha should be gone after file removal")
		}
	}
	if len(tasks) != 1 {
		t.Fatalf("want 1 task remaining, got %d", len(tasks))
	}
}

func TestReasoningEffortRoundTrip(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	tk, err := store.Create("effort test", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	// Set reasoning effort via UpdateMap.
	updated, err := store.UpdateMap(tk.ID, map[string]any{"reasoning_effort": "high"})
	if err != nil {
		t.Fatalf("UpdateMap: %v", err)
	}
	if updated.ReasoningEffort != "high" {
		t.Errorf("after update: ReasoningEffort = %q, want %q", updated.ReasoningEffort, "high")
	}

	// Re-read from disk to verify persistence.
	got, err := store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReasoningEffort != "high" {
		t.Errorf("after re-read: ReasoningEffort = %q, want %q", got.ReasoningEffort, "high")
	}

	// Clear via "default" sentinel (empty string).
	cleared, err := store.UpdateMap(tk.ID, map[string]any{"reasoning_effort": ""})
	if err != nil {
		t.Fatalf("UpdateMap clear: %v", err)
	}
	if cleared.ReasoningEffort != "" {
		t.Errorf("after clear: ReasoningEffort = %q, want empty", cleared.ReasoningEffort)
	}

	// Invalid value must error.
	if _, err := store.UpdateMap(tk.ID, map[string]any{"reasoning_effort": "ultra"}); err == nil {
		t.Error("expected error for invalid reasoning_effort, got nil")
	}
}

// crossProcessHelperEnv triggers TestStoreCrossProcessUpdate to run as a
// worker process instead of the top-level test. Set by
// runCrossProcessHelper; unset (and thus falsy) in a normal `go test` run.
const crossProcessHelperEnv = "SYBRA_TASK_CROSS_PROCESS_DIR"

// TestStoreCrossProcessUpdate spawns two real OS processes — each with its
// own *Store over the same tasks dir, exactly how the GUI server and
// sybra-cli run in production — and has one flip Status while the other
// appends an AgentRun on the same task concurrently. Both do an internal
// read-modify-write (UpdateWithPrev / addRun); without a cross-process lock
// held across that whole section, whichever process writes last does so from
// its own stale read and silently drops the other's change. Regression test
// for the flock added to lockTask.
func TestStoreCrossProcessUpdate(t *testing.T) {
	if dir := os.Getenv(crossProcessHelperEnv); dir != "" {
		runCrossProcessUpdateWorker(t, dir)
		return
	}
	t.Parallel()

	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tk, err := store.Create("cross-process update", "body", "headless")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	const rounds = 8
	for i := range rounds {
		statusCmd := crossProcessUpdateCmd(t, dir, "status", tk.ID)
		runCmd := crossProcessUpdateCmd(t, dir, "run", tk.ID)

		if err := statusCmd.Start(); err != nil {
			t.Fatalf("round %d: start status worker: %v", i, err)
		}
		if err := runCmd.Start(); err != nil {
			t.Fatalf("round %d: start run worker: %v", i, err)
		}
		if err := statusCmd.Wait(); err != nil {
			t.Fatalf("round %d: status worker: %v", i, err)
		}
		if err := runCmd.Wait(); err != nil {
			t.Fatalf("round %d: run worker: %v", i, err)
		}

		got, err := store.Get(tk.ID)
		if err != nil {
			t.Fatalf("round %d: get: %v", i, err)
		}
		if got.Status != StatusInProgress {
			t.Fatalf("round %d: Status = %q, want %q — dropped by concurrent cross-process write", i, got.Status, StatusInProgress)
		}
		if len(got.AgentRuns) != i+1 {
			t.Fatalf("round %d: len(AgentRuns) = %d, want %d — dropped by concurrent cross-process write", i, len(got.AgentRuns), i+1)
		}

		// Reset status so the next round can observe a fresh flip.
		if _, err := store.Update(tk.ID, Update{Status: new(StatusTodo)}); err != nil {
			t.Fatalf("round %d: reset status: %v", i, err)
		}
	}
}

func crossProcessUpdateCmd(t *testing.T, dir, mode, taskID string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStoreCrossProcessUpdate$", "-test.v")
	cmd.Env = append(os.Environ(),
		crossProcessHelperEnv+"="+dir,
		"SYBRA_TASK_CROSS_PROCESS_MODE="+mode,
		"SYBRA_TASK_CROSS_PROCESS_TASK_ID="+taskID,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("worker[%s] output:\n%s", mode, out.String())
		}
	})
	return cmd
}

// runCrossProcessUpdateWorker is the body executed inside the spawned
// worker process. It performs exactly one store operation, selected by
// SYBRA_TASK_CROSS_PROCESS_MODE, against the shared tasks dir passed via
// crossProcessHelperEnv.
func runCrossProcessUpdateWorker(t *testing.T, dir string) {
	t.Helper()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("worker: new store: %v", err)
	}
	taskID := os.Getenv("SYBRA_TASK_CROSS_PROCESS_TASK_ID")
	switch os.Getenv("SYBRA_TASK_CROSS_PROCESS_MODE") {
	case "status":
		if _, err := store.Update(taskID, Update{Status: new(StatusInProgress)}); err != nil {
			t.Fatalf("worker: update status: %v", err)
		}
	case "run":
		if err := store.AddRun(taskID, AgentRun{AgentID: fmt.Sprintf("agent-%d", os.Getpid())}); err != nil {
			t.Fatalf("worker: add run: %v", err)
		}
	default:
		t.Fatal("worker: unknown SYBRA_TASK_CROSS_PROCESS_MODE")
	}
}

// --- Trash (soft-delete) tests ---

func TestStoreDeleteMovesFileAndSidecarsToTrash(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Trash me", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.plans.Write(created.ID, "the plan"); err != nil {
		t.Fatal(err)
	}
	if err := store.planDrafts.Write(created.ID, "claude", "draft content"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.comments.Add(created.ID, 1, "a comment"); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete(created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Primary file and every sidecar are gone from the live tasks dir.
	if _, err := os.Stat(created.FilePath); !os.IsNotExist(err) {
		t.Error("task file should be removed from tasks dir after delete")
	}
	liveEntries, err := os.ReadDir(store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range liveEntries {
		if strings.HasPrefix(e.Name(), created.ID+".") && !strings.HasSuffix(e.Name(), ".lock") {
			t.Errorf("unexpected leftover file in tasks dir: %s", e.Name())
		}
	}

	// The lock sidecar stays behind — it's lock plumbing, not task data.
	if _, err := os.Stat(filepath.Join(store.Dir(), created.ID+".md.lock")); err != nil {
		t.Errorf("lock file should remain in tasks dir: %v", err)
	}

	entries, err := store.ListTrash()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListTrash() = %d entries, want 1", len(entries))
	}
	if entries[0].ID != created.ID {
		t.Errorf("entry ID = %q, want %q", entries[0].ID, created.ID)
	}
	if entries[0].Title != created.Title {
		t.Errorf("entry Title = %q, want %q", entries[0].Title, created.Title)
	}

	genDir := filepath.Join(store.TrashDir(), entries[0].DeletedDate, entries[0].Generation)
	genEntries, err := os.ReadDir(genDir)
	if err != nil {
		t.Fatalf("read generation dir: %v", err)
	}
	names := map[string]bool{}
	for _, e := range genEntries {
		names[e.Name()] = true
	}
	for _, want := range []string{
		created.ID + ".md",
		created.ID + ".plan.md",
		created.ID + ".comments.json",
		created.ID + ".plan-draft-claude.md",
	} {
		if !names[want] {
			t.Errorf("trash generation missing %s (have %v)", want, names)
		}
	}
	if names[created.ID+".md.lock"] {
		t.Error("trash generation should not contain the .lock file")
	}
}

func TestStoreRestoreFromTrash(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Restore me", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.plans.Write(created.ID, "the plan"); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete(created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(created.ID); err == nil {
		t.Fatal("task should not be gettable while trashed")
	}

	restored, err := store.RestoreFromTrash(created.ID)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.ID != created.ID || restored.Title != created.Title {
		t.Errorf("restored task = %+v, want id/title matching %+v", restored, created)
	}
	if restored.Plan != "the plan" {
		t.Errorf("restored.Plan = %q, want %q", restored.Plan, "the plan")
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("get after restore: %v", err)
	}
	if got.Title != created.Title {
		t.Errorf("got.Title = %q, want %q", got.Title, created.Title)
	}

	// Trash generation should be cleaned up after a successful restore.
	remaining, err := store.ListTrash()
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Errorf("ListTrash() after restore = %d entries, want 0", len(remaining))
	}
}

func TestStoreRestoreFromTrash_RefusesLiveCollision(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Collide", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(created.ID); err != nil {
		t.Fatal(err)
	}

	// A new live task happens to reuse the same id (e.g. an external tool
	// recreated the file directly).
	if err := os.WriteFile(filepath.Join(store.Dir(), created.ID+".md"), []byte("---\nid: "+created.ID+"\n---\nnew task"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := store.RestoreFromTrash(created.ID); err == nil {
		t.Fatal("expected restore to refuse overwriting a live task")
	}
}

func TestStoreRestoreFromTrash_NotFound(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.RestoreFromTrash("nonexistent"); err == nil {
		t.Fatal("expected error for id with no trashed generation")
	}
}

func TestStoreTrash_DuplicateGenerationsAndNewestRestore(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Delete twice", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}
	primaryContent, err := os.ReadFile(created.FilePath)
	if err != nil {
		t.Fatal(err)
	}

	// A restore always cleans up the generation directory it consumes, so
	// two same-day generations for one id can't coexist through the public
	// Delete/RestoreFromTrash API alone (the freed bare-id slot gets reused
	// on the next delete). Drive the internal generation-naming helper
	// directly to produce the two-generations-for-one-id state that
	// ListTrash/RestoreFromTrash must handle — e.g. a crash between
	// restoring a task's files and removing the now-empty generation dir,
	// or a second delete racing in from another process.
	genA, err := store.newTrashGeneration(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(genA, created.ID+".md"), primaryContent, 0o644); err != nil {
		t.Fatal(err)
	}
	older := time.Now().Add(-time.Hour)
	if err := os.Chtimes(genA, older, older); err != nil {
		t.Fatal(err)
	}

	genB, err := store.newTrashGeneration(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if genA == genB {
		t.Fatalf("newTrashGeneration returned the same dir twice: %s", genA)
	}
	if err := os.WriteFile(filepath.Join(genB, created.ID+".md"), primaryContent, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(created.FilePath); err != nil {
		t.Fatal(err)
	}

	entries, err := store.ListTrash()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("ListTrash() = %d entries, want 2 distinct generations for %s", len(entries), created.ID)
	}

	restored, err := store.RestoreFromTrash(created.ID)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.ID != created.ID {
		t.Fatalf("restored.ID = %q, want %q", restored.ID, created.ID)
	}

	// The newer generation (genB) was consumed by restore; the older one
	// (genA) remains trashed.
	remaining, err := store.ListTrash()
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 {
		t.Fatalf("remaining = %+v, want exactly one leftover generation", remaining)
	}
	if remaining[0].Generation != filepath.Base(genA) {
		t.Fatalf("remaining generation = %q, want the older generation %q to survive", remaining[0].Generation, filepath.Base(genA))
	}
}

func TestStorePruneTrash(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	oldTask, err := store.Create("Old", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(oldTask.ID); err != nil {
		t.Fatal(err)
	}
	recentTask, err := store.Create("Recent", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(recentTask.ID); err != nil {
		t.Fatal(err)
	}

	// Both tasks were deleted today, so they share one date directory.
	// Backdate only the "old" task's own generation dir by moving it into a
	// separate, expired date directory — moving the whole shared date
	// directory would backdate (and later prune) the recent task too.
	oldEntries, err := store.ListTrash()
	if err != nil {
		t.Fatal(err)
	}
	var oldGenDir string
	for _, e := range oldEntries {
		if e.ID == oldTask.ID {
			oldGenDir = filepath.Join(store.TrashDir(), e.DeletedDate, e.Generation)
		}
	}
	if oldGenDir == "" {
		t.Fatal("could not find old task's trash generation")
	}
	backdated := filepath.Join(store.TrashDir(), time.Now().UTC().AddDate(0, 0, -30).Format(time.DateOnly))
	if err := os.MkdirAll(backdated, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(oldGenDir, filepath.Join(backdated, filepath.Base(oldGenDir))); err != nil {
		t.Fatal(err)
	}

	rep, err := store.PruneTrash(14)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if rep.Removed != 1 {
		t.Fatalf("rep.Removed = %d, want 1", rep.Removed)
	}
	if len(rep.Entries) != 1 || rep.Entries[0].ID != oldTask.ID {
		t.Fatalf("rep.Entries = %+v, want one entry for %s", rep.Entries, oldTask.ID)
	}

	remaining, err := store.ListTrash()
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ID != recentTask.ID {
		t.Fatalf("remaining = %+v, want only the recent task", remaining)
	}

	// The now-empty backdated date directory should have been removed.
	if _, err := os.Stat(backdated); !os.IsNotExist(err) {
		t.Error("expired, now-empty date directory should be removed")
	}
}

func TestStorePruneTrash_NegativeDisables(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Never pruned", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(created.ID); err != nil {
		t.Fatal(err)
	}

	entries, err := store.ListTrash()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListTrash() = %d entries, want 1", len(entries))
	}
	backdated := filepath.Join(store.TrashDir(), time.Now().UTC().AddDate(0, 0, -3650).Format(time.DateOnly))
	if err := os.Rename(filepath.Join(store.TrashDir(), entries[0].DeletedDate), backdated); err != nil {
		t.Fatal(err)
	}

	rep, err := store.PruneTrash(-1)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if rep.Scanned != 0 || rep.Removed != 0 {
		t.Fatalf("negative retention should no-op, got %+v", rep)
	}

	remaining, err := store.ListTrash()
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 {
		t.Fatalf("remaining = %d, want 1 (nothing pruned)", len(remaining))
	}
}

func TestStoreTrash_PruneVsRestoreRace(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Race", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(created.ID); err != nil {
		t.Fatal(err)
	}

	// Backdate so PruneTrash(1) treats this generation as expired.
	entries, err := store.ListTrash()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListTrash() = %d entries, want 1", len(entries))
	}
	backdated := filepath.Join(store.TrashDir(), time.Now().UTC().AddDate(0, 0, -30).Format(time.DateOnly))
	if err := os.Rename(filepath.Join(store.TrashDir(), entries[0].DeletedDate), backdated); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var restoreErr error
	var pruneRep TrashPruneReport
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, restoreErr = store.RestoreFromTrash(created.ID)
	}()
	go func() {
		defer wg.Done()
		pruneRep, _ = store.PruneTrash(1)
	}()
	wg.Wait()

	// Per-task locking (shared between Delete/RestoreFromTrash/pruneGeneration)
	// guarantees exactly one side wins: either the task is restored and prune
	// removed nothing, or prune won and the task stays gone.
	if restoreErr == nil {
		if pruneRep.Removed != 0 {
			t.Fatalf("prune must not remove a generation restore just consumed, got %+v", pruneRep)
		}
		if _, err := store.Get(created.ID); err != nil {
			t.Fatalf("restored task should be live: %v", err)
		}
	} else {
		if pruneRep.Removed != 1 {
			t.Fatalf("expected prune to have removed the generation when restore lost the race, got %+v", pruneRep)
		}
		if _, err := store.Get(created.ID); err == nil {
			t.Fatal("task should not be live when restore lost the race")
		}
	}
}

func TestStoreDelete_RenameFailureLeavesTaskIntact(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Undeletable", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}

	// Pre-create a regular file where the trash dir needs to be a
	// directory, so MkdirAll(dateDir) fails before any rename is attempted.
	if err := os.WriteFile(store.TrashDir(), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete(created.ID); err == nil {
		t.Fatal("expected delete to fail when the trash dir cannot be created")
	}

	if _, err := os.Stat(created.FilePath); err != nil {
		t.Errorf("task file should remain in place after a failed delete: %v", err)
	}
	if _, err := store.Get(created.ID); err != nil {
		t.Errorf("task should still be gettable after a failed delete: %v", err)
	}
}

func TestStoreGcOrphanChatInteraction_TrashesInsteadOfUnlinking(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	chat, err := store.CreateChat("owner/repo")
	if err != nil {
		t.Fatal(err)
	}

	// gcOrphanChats (internal/recovery) deletes orphaned chat tasks through
	// Manager.Delete → Store.Delete; assert that path now trashes the chat
	// task rather than unlinking it outright, so a wrongly-GC'd chat is
	// still recoverable.
	if err := store.Delete(chat.ID); err != nil {
		t.Fatal(err)
	}
	entries, err := store.ListTrash()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != chat.ID {
		t.Fatalf("ListTrash() = %+v, want the trashed chat task", entries)
	}
}

func TestTrashGenerationID_ReadError(t *testing.T) {
	t.Parallel()
	genDir := filepath.Join(t.TempDir(), "missing")
	if _, _, err := trashGenerationID(genDir); err == nil {
		t.Fatal("expected read error for missing generation dir")
	}
}
