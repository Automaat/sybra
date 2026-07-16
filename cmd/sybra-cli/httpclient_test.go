package main

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/httpapi"
	"github.com/Automaat/sybra/internal/task"
)

type fakeTaskService struct {
	tasks *task.Manager
}

func (f *fakeTaskService) GetTask(id string) (task.Task, error) {
	return f.tasks.Get(id)
}

func (f *fakeTaskService) UpdateTask(id string, updates map[string]any) (task.Task, error) {
	return f.tasks.UpdateMap(id, updates)
}

func (f *fakeTaskService) CreateTask(title, body, mode string) (task.Task, error) {
	return f.tasks.Create(title, body, mode)
}

func (f *fakeTaskService) DeleteTask(id string) error {
	return f.tasks.Delete(id)
}

func startFakeAPIServer(t *testing.T, tasksDir string) string {
	t.Helper()
	rawStore, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	svc := &fakeTaskService{tasks: task.NewManager(rawStore, nil)}
	mux := http.NewServeMux()
	httpapi.Mount(mux, map[string]httpapi.Service{
		"TaskService": httpapi.NewService(svc, "GetTask", "UpdateTask", "CreateTask", "DeleteTask"),
	}, slog.Default())
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
}

type failingTaskService struct {
	fakeTaskService
}

func (f *failingTaskService) UpdateTask(string, map[string]any) (task.Task, error) {
	return task.Task{}, errors.New("simulated internal server failure")
}

func startFailingAPIServer(t *testing.T, tasksDir string) string {
	t.Helper()
	rawStore, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	svc := &failingTaskService{fakeTaskService{tasks: task.NewManager(rawStore, nil)}}
	mux := http.NewServeMux()
	httpapi.Mount(mux, map[string]httpapi.Service{
		"TaskService": httpapi.NewService(svc, "GetTask", "UpdateTask", "CreateTask", "DeleteTask"),
	}, slog.Default())
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
}

func lockdownDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

func TestUpdate_UsesHTTPModeWhenFilesystemIsReadOnly(t *testing.T) {
	home := t.TempDir()
	tasksDir := filepath.Join(home, ".sybra", "tasks")
	if err := os.MkdirAll(tasksDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("SYBRA_HOME", "")
	t.Setenv("SYBRA_CONTROL_HOME", "")
	t.Setenv("SYBRA_PORT", "1")

	code, out := runCLI(t, "--json", "create", "--title", "http mode target")
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, out)
	}

	tasks, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	list, err := tasks.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("expected exactly one seeded task, got %v (err=%v)", list, err)
	}
	id := list[0].ID

	serverTasksDir := t.TempDir()
	taskFile := id + ".md"
	seeded, err := os.ReadFile(filepath.Join(tasksDir, taskFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverTasksDir, taskFile), seeded, 0o600); err != nil {
		t.Fatal(err)
	}

	port := startFakeAPIServer(t, serverTasksDir)
	t.Setenv("SYBRA_PORT", port)

	lockdownDir(t, tasksDir)

	code, out = runCLI(t, "--json", "update", id, "--status", "todo", "--status-reason", "via http")
	if code != 0 {
		t.Fatalf("update over HTTP mode should succeed against a read-only task dir, got exit %d: %s", code, out)
	}
	if !strings.Contains(out, "via http") {
		t.Fatalf("update output = %q, want it to reflect the applied status_reason", out)
	}

	served, err := task.NewStore(serverTasksDir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := served.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusTodo || got.StatusReason != "via http" {
		t.Fatalf("server-side task = %+v, want the update to have landed via HTTP", got)
	}
}

func TestUpdate_FailsClosedWhenNoServerAndFilesystemReadOnly(t *testing.T) {
	home := t.TempDir()
	tasksDir := filepath.Join(home, ".sybra", "tasks")
	if err := os.MkdirAll(tasksDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("SYBRA_HOME", "")
	t.Setenv("SYBRA_CONTROL_HOME", "")
	t.Setenv("SYBRA_PORT", "1")

	code, out := runCLI(t, "--json", "create", "--title", "no server target")
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, out)
	}

	tasks, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	list, err := tasks.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("expected exactly one seeded task, got %v (err=%v)", list, err)
	}
	id := list[0].ID

	lockdownDir(t, tasksDir)

	code, _ = runCLI(t, "--json", "update", id, "--status", "todo")
	if code == 0 {
		t.Fatal("update against a read-only task dir with no server reachable should fail, not silently succeed")
	}
}

func TestUpdate_ServerErrorNeverFallsBackToFilesystem(t *testing.T) {
	home := t.TempDir()
	tasksDir := filepath.Join(home, ".sybra", "tasks")
	if err := os.MkdirAll(tasksDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("SYBRA_HOME", "")
	t.Setenv("SYBRA_CONTROL_HOME", "")
	t.Setenv("SYBRA_PORT", "1")

	code, out := runCLI(t, "--json", "create", "--title", "server error target")
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, out)
	}

	tasks, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	list, err := tasks.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("expected exactly one seeded task, got %v (err=%v)", list, err)
	}
	id := list[0].ID
	before, err := tasks.Get(id)
	if err != nil {
		t.Fatal(err)
	}

	port := startFailingAPIServer(t, t.TempDir())
	t.Setenv("SYBRA_PORT", port)

	code, out = runCLI(t, "--json", "update", id, "--status", "todo")
	if code == 0 {
		t.Fatalf("update must surface a real server error, not silently fall back to filesystem: exit 0, out=%s", out)
	}

	after, err := tasks.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != before.Status {
		t.Fatalf("filesystem task status changed to %q despite the server call failing — HTTP 5xx must not fall back to a direct write", after.Status)
	}
}

func TestUpdate_HomeFlagForcesFilesystemModeEvenWithServerRunning(t *testing.T) {
	home := t.TempDir()
	tasksDir := filepath.Join(home, "tasks")
	if err := os.MkdirAll(tasksDir, 0o700); err != nil {
		t.Fatal(err)
	}

	code, out := runCLI(t, "--json", "--home", home, "create", "--title", "home flag target")
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, out)
	}

	tasks, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	list, err := tasks.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("expected exactly one seeded task, got %v (err=%v)", list, err)
	}
	id := list[0].ID

	port := startFakeAPIServer(t, tasksDir)
	t.Setenv("SYBRA_PORT", port)

	lockdownDir(t, tasksDir)

	code, _ = runCLI(t, "--json", "--home", home, "update", id, "--status", "todo")
	if code == 0 {
		t.Fatal("--home must force filesystem mode even when a server is reachable; update against a read-only dir should fail")
	}
}
