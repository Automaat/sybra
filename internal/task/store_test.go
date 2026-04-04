package task

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
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

	updated, err := store.Update(created.ID, map[string]any{
		"title":  "Updated",
		"status": "done",
		"body":   "new body",
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

	updated, err := store.Update(created.ID, map[string]any{
		"tags": []string{"backend", "auth"},
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

	updated, err := store.Update(created.ID, map[string]any{
		"agent_mode": "interactive",
	})
	if err != nil {
		t.Fatalf("update agent_mode: %v", err)
	}
	if updated.AgentMode != "interactive" {
		t.Errorf("AgentMode = %q, want %q", updated.AgentMode, "interactive")
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

	updated, err := store.Update(created.ID, map[string]any{
		"project_id": "owner/repo",
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

	updated, err := store.Update(created.ID, map[string]any{
		"status":        "human-required",
		"status_reason": "agent failed with errors",
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
	updated2, err := store.Update(created.ID, map[string]any{"status": "in-progress"})
	if err != nil {
		t.Fatalf("update2: %v", err)
	}
	if updated2.StatusReason != "" {
		t.Errorf("StatusReason after status change = %q, want empty", updated2.StatusReason)
	}
}

func TestStoreUpdateNotFound(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Update("nonexistent", map[string]any{"title": "x"})
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

	err = store.UpdateRun(created.ID, "agent-upd", map[string]any{
		"state":    "done",
		"cost_usd": 0.42,
		"result":   "success",
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
	if r.Result != "success" {
		t.Errorf("Result = %q, want %q", r.Result, "success")
	}
}

func TestStoreUpdateRunNotFound(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	err = store.UpdateRun("nonexistent", "agent-x", map[string]any{"state": "done"})
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

	// Update with wrong agent ID — should not error but should not change anything
	err = store.UpdateRun(created.ID, "agent-wrong", map[string]any{"state": "done"})
	if err != nil {
		t.Fatalf("UpdateRun with wrong agent: %v", err)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentRuns[0].State != "running" {
		t.Errorf("State should be unchanged, got %q", got.AgentRuns[0].State)
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
