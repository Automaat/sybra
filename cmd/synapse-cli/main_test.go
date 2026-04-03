package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Automaat/synapse/internal/task"
)

func mustUnmarshal(t *testing.T, data string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(data), v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
}

func setupStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SYNAPSE_HOME", dir)
	t.Setenv("SYNAPSE_TASKS_DIR", filepath.Join(dir, "tasks"))
	return dir
}

func runCLI(t *testing.T, args ...string) (exitCode int, output string) {
	t.Helper()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	code := run(args)

	_ = w.Close()
	os.Stdout = old

	buf := make([]byte, 64*1024)
	n, _ := r.Read(buf)
	return code, string(buf[:n])
}

func TestListEmpty(t *testing.T) {
	setupStore(t)
	code, out := runCLI(t, "--json", "list")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var tasks []task.Task
	if err := json.Unmarshal([]byte(out), &tasks); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("expected 0 tasks, got %d", len(tasks))
	}
}

func TestCreateAndGet(t *testing.T) {
	setupStore(t)

	code, out := runCLI(t, "--json", "create", "--title", "test task", "--body", "body text", "--tags", "a,b")
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, out)
	}

	var created task.Task
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.Title != "test task" {
		t.Errorf("title = %q", created.Title)
	}
	if created.Body != "body text" {
		t.Errorf("body = %q", created.Body)
	}
	if len(created.Tags) != 2 || created.Tags[0] != "a" || created.Tags[1] != "b" {
		t.Errorf("tags = %v", created.Tags)
	}

	code, out = runCLI(t, "--json", "get", created.ID)
	if code != 0 {
		t.Fatalf("get exit %d: %s", code, out)
	}
	var got task.Task
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("get returned id %q, want %q", got.ID, created.ID)
	}
}

func TestUpdateStatus(t *testing.T) {
	setupStore(t)

	code, out := runCLI(t, "--json", "create", "--title", "update me")
	if code != 0 {
		t.Fatalf("create exit %d", code)
	}
	var created task.Task
	mustUnmarshal(t, out, &created)

	code, out = runCLI(t, "--json", "update", created.ID, "--status", "in-progress")
	if code != 0 {
		t.Fatalf("update exit %d: %s", code, out)
	}
	var updated task.Task
	mustUnmarshal(t, out, &updated)
	if updated.Status != "in-progress" {
		t.Errorf("status = %q", updated.Status)
	}
}

func TestDelete(t *testing.T) {
	setupStore(t)

	code, out := runCLI(t, "--json", "create", "--title", "delete me")
	if code != 0 {
		t.Fatalf("create exit %d", code)
	}
	var created task.Task
	mustUnmarshal(t, out, &created)

	code, _ = runCLI(t, "--json", "delete", created.ID)
	if code != 0 {
		t.Fatalf("delete exit %d", code)
	}

	code, out = runCLI(t, "--json", "list")
	if code != 0 {
		t.Fatalf("list exit %d", code)
	}
	var tasks []task.Task
	mustUnmarshal(t, out, &tasks)
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks after delete, got %d", len(tasks))
	}
}

func TestListFilterStatus(t *testing.T) {
	setupStore(t)

	runCLI(t, "--json", "create", "--title", "task1")
	_, out := runCLI(t, "--json", "create", "--title", "task2")
	var t2 task.Task
	mustUnmarshal(t, out, &t2)
	runCLI(t, "--json", "update", t2.ID, "--status", "in-progress")

	code, out := runCLI(t, "--json", "list", "--status", "new")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var tasks []task.Task
	mustUnmarshal(t, out, &tasks)
	if len(tasks) != 1 {
		t.Errorf("expected 1 new task, got %d", len(tasks))
	}
}

func TestListFilterTag(t *testing.T) {
	setupStore(t)

	runCLI(t, "--json", "create", "--title", "tagged", "--tags", "api,backend")
	runCLI(t, "--json", "create", "--title", "untagged")

	code, out := runCLI(t, "--json", "list", "--tag", "api")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var tasks []task.Task
	mustUnmarshal(t, out, &tasks)
	if len(tasks) != 1 {
		t.Errorf("expected 1 tagged task, got %d", len(tasks))
	}
}

func TestUnknownCommand(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "bogus")
	if code == 0 {
		t.Error("expected non-zero exit for unknown command")
	}
}

func TestNoArgs(t *testing.T) {
	code, _ := runCLI(t)
	if code == 0 {
		t.Error("expected non-zero exit for no args")
	}
}

// Tests from PR branch (coverage boost)

func TestOnlyJSONFlag(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "--json")
	if code == 0 {
		t.Error("expected non-zero exit for --json with no command")
	}
}

func TestGetNoID(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "--json", "get")
	if code == 0 {
		t.Error("expected non-zero exit for get without ID")
	}
}

func TestGetNotFound(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "--json", "get", "nonexistent")
	if code == 0 {
		t.Error("expected non-zero exit for nonexistent task")
	}
}

func TestDeleteNoID(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "--json", "delete")
	if code == 0 {
		t.Error("expected non-zero exit for delete without ID")
	}
}

func TestDeleteNotFound(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "--json", "delete", "nonexistent")
	if code == 0 {
		t.Error("expected non-zero exit for deleting nonexistent task")
	}
}

func TestCreateNoTitle(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "--json", "create")
	if code == 0 {
		t.Error("expected non-zero exit for create without title")
	}
}

func TestUpdateNoID(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "--json", "update")
	if code == 0 {
		t.Error("expected non-zero exit for update without ID")
	}
}

func TestUpdateNoFlags(t *testing.T) {
	setupStore(t)
	code, out := runCLI(t, "--json", "create", "--title", "no flags test")
	if code != 0 {
		t.Fatalf("create exit %d", code)
	}
	var created task.Task
	mustUnmarshal(t, out, &created)

	code, _ = runCLI(t, "--json", "update", created.ID)
	if code == 0 {
		t.Error("expected non-zero exit for update with no flags")
	}
}

func TestUpdateMultipleFields(t *testing.T) {
	setupStore(t)
	code, out := runCLI(t, "--json", "create", "--title", "multi update")
	if code != 0 {
		t.Fatalf("create exit %d", code)
	}
	var created task.Task
	mustUnmarshal(t, out, &created)

	code, out = runCLI(t, "--json", "update", created.ID,
		"--title", "new title",
		"--status", "done",
		"--body", "new body",
		"--mode", "interactive",
		"--tags", "x,y,z")
	if code != 0 {
		t.Fatalf("update exit %d: %s", code, out)
	}

	var updated task.Task
	mustUnmarshal(t, out, &updated)
	if updated.Title != "new title" {
		t.Errorf("Title = %q, want %q", updated.Title, "new title")
	}
	if updated.Status != "done" {
		t.Errorf("Status = %q, want %q", updated.Status, "done")
	}
	if updated.Body != "new body" {
		t.Errorf("Body = %q, want %q", updated.Body, "new body")
	}
	if updated.AgentMode != "interactive" {
		t.Errorf("AgentMode = %q, want %q", updated.AgentMode, "interactive")
	}
	if len(updated.Tags) != 3 {
		t.Fatalf("Tags len = %d, want 3", len(updated.Tags))
	}
}

func TestUpdateNotFound(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "--json", "update", "nonexistent", "--title", "x")
	if code == 0 {
		t.Error("expected non-zero exit for updating nonexistent task")
	}
}

func TestListBothFilters(t *testing.T) {
	setupStore(t)

	// Create tasks with different statuses and tags
	runCLI(t, "--json", "create", "--title", "match", "--tags", "api")
	_, out := runCLI(t, "--json", "create", "--title", "match2", "--tags", "api")
	var t2 task.Task
	mustUnmarshal(t, out, &t2)
	runCLI(t, "--json", "update", t2.ID, "--status", "in-progress")
	runCLI(t, "--json", "create", "--title", "no match tag", "--tags", "web")

	code, out := runCLI(t, "--json", "list", "--status", "new", "--tag", "api")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var tasks []task.Task
	mustUnmarshal(t, out, &tasks)
	if len(tasks) != 1 {
		t.Errorf("expected 1 task matching both filters, got %d", len(tasks))
	}
}

func TestCreateWithMode(t *testing.T) {
	setupStore(t)
	code, out := runCLI(t, "--json", "create", "--title", "interactive task", "--mode", "interactive")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var created task.Task
	mustUnmarshal(t, out, &created)
	if created.AgentMode != "interactive" {
		t.Errorf("AgentMode = %q, want %q", created.AgentMode, "interactive")
	}
}

// Tests from main (project support)

func TestCreateWithProject(t *testing.T) {
	setupStore(t)

	code, out := runCLI(t, "--json", "create", "--title", "proj task", "--project", "owner/repo")
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, out)
	}
	var created task.Task
	mustUnmarshal(t, out, &created)
	if created.ProjectID != "owner/repo" {
		t.Errorf("projectId = %q, want %q", created.ProjectID, "owner/repo")
	}
}

func TestUpdateProject(t *testing.T) {
	setupStore(t)

	code, out := runCLI(t, "--json", "create", "--title", "no proj")
	if code != 0 {
		t.Fatalf("create exit %d", code)
	}
	var created task.Task
	mustUnmarshal(t, out, &created)

	code, out = runCLI(t, "--json", "update", created.ID, "--project", "org/myrepo")
	if code != 0 {
		t.Fatalf("update exit %d: %s", code, out)
	}
	var updated task.Task
	mustUnmarshal(t, out, &updated)
	if updated.ProjectID != "org/myrepo" {
		t.Errorf("projectId = %q, want %q", updated.ProjectID, "org/myrepo")
	}
}

func TestListFilterProject(t *testing.T) {
	setupStore(t)

	runCLI(t, "--json", "create", "--title", "proj task", "--project", "owner/repo")
	runCLI(t, "--json", "create", "--title", "other task")

	code, out := runCLI(t, "--json", "list", "--project", "owner/repo")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var tasks []task.Task
	mustUnmarshal(t, out, &tasks)
	if len(tasks) != 1 {
		t.Errorf("expected 1 project task, got %d", len(tasks))
	}
}

func TestProjectListEmpty(t *testing.T) {
	setupStore(t)
	code, out := runCLI(t, "--json", "project", "list")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var projects []map[string]any
	mustUnmarshal(t, out, &projects)
	if len(projects) != 0 {
		t.Errorf("expected 0 projects, got %d", len(projects))
	}
}

func TestProjectNoSubcommand(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "--json", "project")
	if code == 0 {
		t.Error("expected non-zero exit for no subcommand")
	}
}

func TestProjectUnknownSubcommand(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "--json", "project", "bogus")
	if code == 0 {
		t.Error("expected non-zero exit for unknown subcommand")
	}
}

func TestProjectGetNotFound(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "--json", "project", "get", "nonexistent/repo")
	if code == 0 {
		t.Error("expected non-zero exit for nonexistent project")
	}
}

func TestProjectDeleteNotFound(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "--json", "project", "delete", "nonexistent/repo")
	if code == 0 {
		t.Error("expected non-zero exit for nonexistent project")
	}
}

func TestProjectCreateNoURL(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "--json", "project", "create")
	if code == 0 {
		t.Error("expected non-zero exit for missing url")
	}
}

func TestProjectGetNoID(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "--json", "project", "get")
	if code == 0 {
		t.Error("expected non-zero exit for missing id")
	}
}

func TestProjectDeleteNoID(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "--json", "project", "delete")
	if code == 0 {
		t.Error("expected non-zero exit for missing id")
	}
}
