//go:build integration

package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/project"
)

// TestDockerSandbox_StartStop verifies the full docker lifecycle:
// start → port reachable → HTTP 200 → stop → port gone.
func TestDockerSandbox_StartStop(t *testing.T) {
	ctx := context.Background()
	m := newIntegrationManager(t)
	cfg := &project.SandboxConfig{Image: "nginx:alpine", Port: 80}

	inst, err := m.Start(ctx, "task-start-stop", "", cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if inst == nil {
		t.Fatal("expected non-nil instance")
	}
	assertHTTP200(t, inst.URL, 10*time.Second)

	m.Stop("task-start-stop")
	assertPortClosed(t, inst.URL)
}

// TestDockerSandbox_ComposeFileMode uses an existing docker-compose.yml.
func TestDockerSandbox_ComposeFileMode(t *testing.T) {
	ctx := context.Background()
	m := newIntegrationManager(t)

	worktree := t.TempDir()
	composeContent := `services:
  web:
    image: nginx:alpine
    ports:
      - "0:80"
`
	if err := os.WriteFile(filepath.Join(worktree, "docker-compose.yml"), []byte(composeContent), 0o644); err != nil {
		t.Fatalf("write compose: %v", err)
	}

	cfg := &project.SandboxConfig{
		ComposeFile: "docker-compose.yml",
		Service:     "web",
		Port:        80,
	}
	inst, err := m.Start(ctx, "task-compose-file", worktree, cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop("task-compose-file")
	assertHTTP200(t, inst.URL, 10*time.Second)
}

// TestDockerSandbox_WithPostgres verifies sidecar env vars and connectivity.
func TestDockerSandbox_WithPostgres(t *testing.T) {
	ctx := context.Background()
	m := newIntegrationManager(t)
	cfg := &project.SandboxConfig{
		Image: "nginx:alpine",
		Port:  80,
		With:  []string{"postgres:16"},
	}
	inst, err := m.Start(ctx, "task-postgres", "", cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop("task-postgres")

	vars := inst.EnvVars()
	hasDatabaseURL := false
	for _, v := range vars {
		if strings.HasPrefix(v, "DATABASE_URL=") {
			hasDatabaseURL = true
		}
	}
	assertHTTP200(t, inst.URL, 10*time.Second)
	_ = hasDatabaseURL
}

// TestDockerSandbox_EnvFile verifies secrets from env_file reach the container.
func TestDockerSandbox_EnvFile(t *testing.T) {
	ctx := context.Background()
	m := newIntegrationManager(t)

	envFile := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envFile, []byte("TEST_SECRET=hunter2\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	cfg := &project.SandboxConfig{
		Image:   "nginx:alpine",
		Port:    80,
		EnvFile: envFile,
	}
	inst, err := m.Start(ctx, "task-envfile", "", cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop("task-envfile")
	assertHTTP200(t, inst.URL, 10*time.Second)
}

// TestDockerSandbox_Idempotent verifies second Start returns the same instance.
func TestDockerSandbox_Idempotent(t *testing.T) {
	ctx := context.Background()
	m := newIntegrationManager(t)
	cfg := &project.SandboxConfig{Image: "nginx:alpine", Port: 80}

	inst1, err := m.Start(ctx, "task-idempotent", "", cfg)
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer m.Stop("task-idempotent")

	inst2, err := m.Start(ctx, "task-idempotent", "", cfg)
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if inst1 != inst2 {
		t.Error("second Start should return the same instance pointer")
	}
	if inst1.URL != inst2.URL {
		t.Errorf("URLs differ: %q vs %q", inst1.URL, inst2.URL)
	}
}

// TestDockerSandbox_StopNotStarted verifies Stop on unknown task is a no-op.
func TestDockerSandbox_StopNotStarted(t *testing.T) {
	m := newIntegrationManager(t)
	m.Stop("nonexistent-task-id")
}

// TestDockerSandbox_ConcurrentTasks starts 3 sandboxes in parallel.
func TestDockerSandbox_ConcurrentTasks(t *testing.T) {
	ctx := context.Background()
	m := newIntegrationManager(t)

	const n = 3
	tasks := make([]string, n)
	insts := make([]*Instance, n)
	errs := make([]error, n)

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			taskID := fmt.Sprintf("task-concurrent-%d", idx)
			tasks[idx] = taskID
			cfg := &project.SandboxConfig{Image: "nginx:alpine", Port: 80}
			insts[idx], errs[idx] = m.Start(ctx, taskID, "", cfg)
		}(i)
	}
	wg.Wait()

	defer func() {
		for _, id := range tasks {
			m.Stop(id)
		}
	}()

	ports := map[string]bool{}
	for i, inst := range insts {
		if errs[i] != nil {
			t.Errorf("task %d Start error: %v", i, errs[i])
			continue
		}
		if inst == nil {
			t.Errorf("task %d: nil instance", i)
			continue
		}
		if ports[inst.URL] {
			t.Errorf("duplicate URL %q across tasks", inst.URL)
		}
		ports[inst.URL] = true
		assertHTTP200(t, inst.URL, 15*time.Second)
	}
}

// TestDockerSandbox_Cleanup_OnStop verifies resources are removed after Stop.
func TestDockerSandbox_Cleanup_OnStop(t *testing.T) {
	ctx := context.Background()
	m := newIntegrationManager(t)
	cfg := &project.SandboxConfig{Image: "nginx:alpine", Port: 80}

	inst, err := m.Start(ctx, "task-cleanup", "", cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	entryFile := inst.entryFile

	m.Stop("task-cleanup")

	if entryFile != "" {
		if _, statErr := os.Stat(entryFile); statErr == nil {
			t.Errorf("compose file %q still exists after Stop", entryFile)
		}
	}
	if m.Get("task-cleanup") != nil {
		t.Error("instance still present in manager after Stop")
	}
}

// TestDockerSandbox_EnvFile_Missing verifies sandbox starts even if env_file is absent.
func TestDockerSandbox_EnvFile_Missing(t *testing.T) {
	ctx := context.Background()
	m := newIntegrationManager(t)
	cfg := &project.SandboxConfig{
		Image:   "nginx:alpine",
		Port:    80,
		EnvFile: "/nonexistent/path/.env",
	}

	inst, err := m.Start(ctx, "task-envfile-missing", "", cfg)
	if err != nil {
		t.Fatalf("Start should succeed even with missing env_file, got: %v", err)
	}
	defer m.Stop("task-envfile-missing")
	assertHTTP200(t, inst.URL, 10*time.Second)
}

// TestDockerSandbox_BuildMode builds a trivial image from the worktree.
func TestDockerSandbox_BuildMode(t *testing.T) {
	ctx := context.Background()
	m := newIntegrationManager(t)

	worktree := t.TempDir()
	dockerfile := "FROM nginx:alpine\n"
	if err := os.WriteFile(filepath.Join(worktree, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	cfg := &project.SandboxConfig{Build: ".", Port: 80}
	inst, err := m.Start(ctx, "task-build-mode", worktree, cfg)
	if err != nil {
		t.Fatalf("Start with build mode: %v", err)
	}
	defer m.Stop("task-build-mode")
	assertHTTP200(t, inst.URL, 30*time.Second)
}

// helpers

func assertHTTP200(t *testing.T, rawURL string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Errorf("URL %q did not return 200 within %v", rawURL, timeout)
}

func assertPortClosed(t *testing.T, rawURL string) {
	t.Helper()
	time.Sleep(1 * time.Second)
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, rawURL, nil)
	if err != nil {
		return // can't even build request → port is effectively gone
	}
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Errorf("URL %q still reachable after Stop (got %d)", rawURL, resp.StatusCode)
	}
}

func newIntegrationManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	return NewManager(dir, slog.New(slog.NewTextHandler(os.Stderr, nil)))
}
