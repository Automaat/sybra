package sandbox

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/project"
	"gopkg.in/yaml.v3"
)

// --- SandboxConfig mode detection ---

func TestSandboxConfigModeDetection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		cfg        *project.SandboxConfig
		wantK8s    bool
		wantDocker bool
	}{
		{"nil", nil, false, false},
		{"k8s cluster set", &project.SandboxConfig{Cluster: "k3d"}, true, false},
		{"docker image", &project.SandboxConfig{Image: "nginx:alpine", Port: 80}, false, true},
		{"docker build", &project.SandboxConfig{Build: ".", Port: 8080}, false, true},
		{"docker compose_file", &project.SandboxConfig{ComposeFile: "docker-compose.yml", Port: 80}, false, true},
		{"empty", &project.SandboxConfig{}, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cfg.IsK8s(); got != tc.wantK8s {
				t.Errorf("IsK8s() = %v, want %v", got, tc.wantK8s)
			}
			if got := tc.cfg.IsDocker(); got != tc.wantDocker {
				t.Errorf("IsDocker() = %v, want %v", got, tc.wantDocker)
			}
		})
	}
}

// --- Compose YAML generation ---

func TestGenerateComposeYAML_ImageOnly(t *testing.T) {
	t.Parallel()
	cfg := &project.SandboxConfig{Image: "nginx:alpine", Port: 80}
	data, _, err := generateComposeYAML("/some/worktree", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("invalid yaml: %v", err)
	}
	services, ok := parsed["services"].(map[string]any)
	if !ok {
		t.Fatal("missing services key")
	}
	app, ok := services["app"].(map[string]any)
	if !ok {
		t.Fatal("missing app service")
	}
	if app["image"] != "nginx:alpine" {
		t.Errorf("image = %v, want nginx:alpine", app["image"])
	}
	ports, ok := app["ports"].([]any)
	if !ok || len(ports) == 0 {
		t.Fatal("missing ports")
	}
	if ports[0] != "0:80" {
		t.Errorf("port = %v, want 0:80", ports[0])
	}
	if _, hasSidecars := services["postgres"]; hasSidecars {
		t.Error("unexpected sidecar services")
	}
}

func TestGenerateComposeYAML_BuildMode(t *testing.T) {
	t.Parallel()
	cfg := &project.SandboxConfig{Build: ".", Port: 8080}
	data, _, err := generateComposeYAML("/my/worktree", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("invalid yaml: %v", err)
	}
	services := parsed["services"].(map[string]any)
	app := services["app"].(map[string]any)
	build, ok := app["build"].(string)
	if !ok {
		t.Fatalf("build field missing or wrong type: %v", app["build"])
	}
	if build != "/my/worktree/." && build != "/my/worktree" {
		// filepath.Join normalizes "." so accept both forms
		if !strings.HasPrefix(build, "/my/worktree") {
			t.Errorf("build context = %q, want prefix /my/worktree", build)
		}
	}
	if app["image"] != nil {
		t.Errorf("image should be unset in build mode, got %v", app["image"])
	}
}

func TestGenerateComposeYAML_WithSidecars(t *testing.T) {
	t.Parallel()
	cfg := &project.SandboxConfig{
		Image: "myapp:latest",
		Port:  8080,
		With:  []string{"postgres:16", "redis:7"},
	}
	data, appEnv, err := generateComposeYAML("/worktree", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("invalid yaml: %v", err)
	}
	services := parsed["services"].(map[string]any)
	if _, ok := services["postgres"]; !ok {
		t.Error("postgres sidecar missing")
	}
	if _, ok := services["redis"]; !ok {
		t.Error("redis sidecar missing")
	}
	app := services["app"].(map[string]any)
	dependsOn, ok := app["depends_on"].([]any)
	if !ok || len(dependsOn) != 2 {
		t.Errorf("depends_on = %v, want 2 entries", app["depends_on"])
	}
	if _, ok := appEnv["DATABASE_URL"]; !ok {
		t.Error("DATABASE_URL missing from appEnv")
	}
	if _, ok := appEnv["REDIS_URL"]; !ok {
		t.Error("REDIS_URL missing from appEnv")
	}
}

func TestGenerateComposeYAML_EnvInterpolation(t *testing.T) {
	t.Parallel()
	// ${VAR} should be preserved as-is; docker compose expands at runtime.
	cfg := &project.SandboxConfig{
		Image: "myapp:latest",
		Port:  8080,
		Env:   map[string]string{"API_KEY": "${MY_API_KEY}"},
	}
	data, appEnv, err := generateComposeYAML("/worktree", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The ${} value must survive YAML round-trip unchanged.
	var parsed map[string]any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("invalid yaml: %v", err)
	}
	if appEnv["API_KEY"] != "${MY_API_KEY}" {
		t.Errorf("API_KEY = %q, want ${MY_API_KEY}", appEnv["API_KEY"])
	}
}

// --- LoadEnvFile ---

func TestLoadEnvFile_Happy(t *testing.T) {
	t.Parallel()
	f := writeTempEnvFile(t, "KEY1=val1\n# comment\n\nKEY2=val2\n")
	entries, err := LoadEnvFile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %v", len(entries), entries)
	}
	if entries[0] != "KEY1=val1" {
		t.Errorf("entries[0] = %q", entries[0])
	}
	if entries[1] != "KEY2=val2" {
		t.Errorf("entries[1] = %q", entries[1])
	}
}

func TestLoadEnvFile_EmptyPath(t *testing.T) {
	t.Parallel()
	entries, err := LoadEnvFile("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entries != nil {
		t.Errorf("expected nil, got %v", entries)
	}
}

func TestLoadEnvFile_NotFound(t *testing.T) {
	t.Parallel()
	_, err := LoadEnvFile("/nonexistent/path/.env")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestLoadEnvFile_Malformed(t *testing.T) {
	t.Parallel()
	f := writeTempEnvFile(t, "GOOD=value\nbadline\nANOTHER=ok\n")
	entries, err := LoadEnvFile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// "badline" skipped, two valid entries returned.
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %v", len(entries), entries)
	}
}

func TestLoadEnvFile_TildeExpansion(t *testing.T) {
	t.Parallel()
	// Just verify ~ expansion doesn't panic; file won't exist → error expected.
	_, err := LoadEnvFile("~/nonexistent-sybra-test-file.env")
	if err == nil {
		t.Error("expected error for nonexistent expanded path")
	}
	// Error must not mention "~" (expansion happened).
	if strings.Contains(err.Error(), "~/") {
		t.Errorf("~ was not expanded in error: %v", err)
	}
}

// --- Instance.EnvVars ---

func TestInstanceEnvVars_Docker(t *testing.T) {
	t.Parallel()
	inst := &Instance{TaskID: "t1", URL: "http://localhost:54321"}
	vars := inst.EnvVars()
	if len(vars) != 1 {
		t.Fatalf("got %d vars, want 1: %v", len(vars), vars)
	}
	if vars[0] != "SANDBOX_URL=http://localhost:54321" {
		t.Errorf("vars[0] = %q", vars[0])
	}
}

func TestInstanceEnvVars_K8s(t *testing.T) {
	t.Parallel()
	inst := &Instance{
		TaskID:     "t1",
		URL:        "http://localhost:54321",
		Kubeconfig: "/tmp/sybra-t1/kubeconfig",
	}
	vars := inst.EnvVars()
	if len(vars) != 2 {
		t.Fatalf("got %d vars, want 2: %v", len(vars), vars)
	}
	hasURL := false
	hasKube := false
	for _, v := range vars {
		if v == "SANDBOX_URL=http://localhost:54321" {
			hasURL = true
		}
		if v == "KUBECONFIG=/tmp/sybra-t1/kubeconfig" {
			hasKube = true
		}
	}
	if !hasURL {
		t.Error("SANDBOX_URL missing")
	}
	if !hasKube {
		t.Error("KUBECONFIG missing")
	}
}

func TestInstanceEnvVars_Nil(t *testing.T) {
	t.Parallel()
	var inst *Instance
	if vars := inst.EnvVars(); vars != nil {
		t.Errorf("nil instance should return nil, got %v", vars)
	}
}

// --- Manager ---

func TestManager_GetBeforeStart(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)
	if inst := m.Get("unknown-task"); inst != nil {
		t.Errorf("expected nil, got %v", inst)
	}
}

func TestManager_StopNotStarted(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)
	// Should not panic.
	m.Stop("nonexistent")
}

func TestManager_StartIdempotent(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)
	// Manually insert a fake instance.
	fake := &Instance{TaskID: "task-1", URL: "http://localhost:9999"}
	m.mu.Lock()
	m.instances["task-1"] = fake
	m.mu.Unlock()

	// Start should return the existing instance without creating a new one.
	inst, err := m.Start(context.TODO(), "task-1", "/worktree", &project.SandboxConfig{Image: "nginx", Port: 80})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inst != fake {
		t.Error("Start should return existing instance, not a new one")
	}
}

func TestManager_StartNilConfig(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)
	_, err := m.Start(context.TODO(), "task-1", "/worktree", nil)
	if err == nil {
		t.Fatal("expected error for nil config")
	}
}

// --- helpers ---

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	return NewManager(dir, slog.New(slog.NewTextHandler(os.Stderr, nil)))
}

func writeTempEnvFile(t *testing.T, content string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp env file: %v", err)
	}
	return f
}
