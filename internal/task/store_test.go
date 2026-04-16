package task

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	if len(got.AgentRuns) != 1 {
		t.Fatalf("AgentRuns len = %d, want 1", len(got.AgentRuns))
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

	err = store.UpdateRun(created.ID, "agent-s", map[string]any{
		"state":      "done",
		"session_id": "ses-abc123",
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
	if err := store.AddRun(created.ID, AgentRun{AgentID: "agent-e", Mode: "headless", State: "running", SessionID: "existing-id"}); err != nil {
		t.Fatal(err)
	}

	// Empty session_id should not overwrite existing value
	err = store.UpdateRun(created.ID, "agent-e", map[string]any{
		"state":      "done",
		"session_id": "",
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
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tk, cerr := store.Create(fmt.Sprintf("task-%d", i), "body", "headless")
			if cerr != nil {
				errs <- cerr
				return
			}
			ids <- tk.ID
		}(i)
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
// same task concurrently never leave the file corrupted (unparseable) or with
// a lost StatusReason/Title state — the last writer must win cleanly. The test
// also checks that the on-disk file parses successfully after the race.
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

	// Parse the raw file to confirm on-disk consistency independent of cache.
	reloaded, err := Parse(final.FilePath)
	if err != nil {
		t.Fatalf("raw parse: %v — file is torn", err)
	}
	if reloaded.ID != created.ID {
		t.Errorf("reloaded ID = %q, want %q", reloaded.ID, created.ID)
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
