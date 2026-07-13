package sandbox

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
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

func TestManager_StartPanicCleansStartingAndWakesWaiters(t *testing.T) {
	m := newTestManager(t)
	cfg := &project.SandboxConfig{Image: "nginx", Port: 80}

	leaderEntered := make(chan struct{})
	releaseLeader := make(chan struct{})
	var calls atomic.Int32
	m.startSandbox = func(context.Context, string, string, *project.SandboxConfig) (*Instance, error) {
		if calls.Add(1) == 1 {
			close(leaderEntered)
			<-releaseLeader
		}
		panic("boom")
	}

	leaderDone := make(chan error, 1)
	go func() {
		_, err := m.Start(context.Background(), "task-panic", "/worktree", cfg)
		leaderDone <- err
	}()

	select {
	case <-leaderEntered:
	case <-time.After(time.Second):
		t.Fatal("leader did not enter sandbox starter")
	}

	waiterCtx, cancelWaiter := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelWaiter()
	waiterDone := make(chan error, 1)
	go func() {
		_, err := m.Start(waiterCtx, "task-panic", "/worktree", cfg)
		waiterDone <- err
	}()

	time.Sleep(50 * time.Millisecond)
	close(releaseLeader)

	leaderErr := mustReceiveStartErr(t, leaderDone)
	if leaderErr == nil || !strings.Contains(leaderErr.Error(), "sandbox start panic: boom") {
		t.Fatalf("leader error = %v, want panic error", leaderErr)
	}

	waiterErr := mustReceiveStartErr(t, waiterDone)
	if waiterErr == nil || !strings.Contains(waiterErr.Error(), "sandbox start panic: boom") {
		t.Fatalf("waiter error = %v, want panic error", waiterErr)
	}

	m.mu.Lock()
	_, stillStarting := m.starting["task-panic"]
	inst := m.instances["task-panic"]
	m.mu.Unlock()
	if stillStarting {
		t.Fatal("panic left task in starting map")
	}
	if inst != nil {
		t.Fatalf("panic registered an instance: %+v", inst)
	}
}

func TestManager_SybraHomeDir(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)

	dir, err := m.SybraHomeDir("task-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
		t.Fatalf("SybraHomeDir did not create a directory at %q: %v", dir, statErr)
	}
	if !strings.HasSuffix(dir, filepath.Join("task-1", "sybra-home")) {
		t.Fatalf("unexpected dir layout: %q", dir)
	}

	dir2, err := m.SybraHomeDir("task-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dir == dir2 {
		t.Fatal("different tasks must not share a SYBRA_HOME")
	}

	// Idempotent — calling again for the same task returns the same path and
	// does not fail even though the directory already exists.
	dirAgain, err := m.SybraHomeDir("task-1")
	if err != nil {
		t.Fatalf("unexpected error on second call: %v", err)
	}
	if dirAgain != dir {
		t.Fatalf("expected stable path, got %q then %q", dir, dirAgain)
	}
}

func TestManager_RemoveDeletesTaskDirWithoutRunningInstance(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)

	dir, err := m.SybraHomeDir("task-remove")
	if err != nil {
		t.Fatalf("SybraHomeDir: %v", err)
	}
	taskDir := filepath.Dir(dir)
	if _, err := os.Stat(taskDir); err != nil {
		t.Fatalf("task dir missing before Remove: %v", err)
	}

	m.Remove("task-remove")

	if _, err := os.Stat(taskDir); !os.IsNotExist(err) {
		t.Fatalf("task dir %q still exists after Remove: %v", taskDir, err)
	}
}

func TestManager_CleanupOrphaned(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)

	keepDir, err := m.SybraHomeDir("task-active")
	if err != nil {
		t.Fatalf("SybraHomeDir(active): %v", err)
	}
	doneDir, err := m.SybraHomeDir("task-done")
	if err != nil {
		t.Fatalf("SybraHomeDir(done): %v", err)
	}
	liveDir, err := m.SybraHomeDir("task-live-terminal")
	if err != nil {
		t.Fatalf("SybraHomeDir(live): %v", err)
	}
	missingRoot, err := m.taskDir("task-missing")
	if err != nil {
		t.Fatalf("taskDir(missing): %v", err)
	}
	if err := os.MkdirAll(missingRoot, 0o755); err != nil {
		t.Fatalf("mkdir missing task dir: %v", err)
	}

	tasks := []task.Task{
		{ID: "task-active", Status: task.StatusTesting},
		{ID: "task-done", Status: task.StatusDone},
		{ID: "task-live-terminal", Status: task.StatusCancelled},
	}
	m.CleanupOrphaned(context.Background(), tasks, func(taskID string) bool {
		return taskID == "task-live-terminal"
	})

	if _, err := os.Stat(filepath.Dir(keepDir)); err != nil {
		t.Fatalf("active task dir removed unexpectedly: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(doneDir)); !os.IsNotExist(err) {
		t.Fatalf("terminal task dir still exists after cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(liveDir)); err != nil {
		t.Fatalf("live terminal task dir removed unexpectedly: %v", err)
	}
	if _, err := os.Stat(missingRoot); !os.IsNotExist(err) {
		t.Fatalf("missing-task dir still exists after cleanup: %v", err)
	}
}

func TestManager_CleanupOrphaned_Retention(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)
	m.SetRetentionWindow(time.Hour)

	now := time.Now()

	agedDoneDir, err := m.SybraHomeDir("task-aged-done")
	if err != nil {
		t.Fatalf("SybraHomeDir(aged-done): %v", err)
	}
	agedCancelledDir, err := m.SybraHomeDir("task-aged-cancelled")
	if err != nil {
		t.Fatalf("SybraHomeDir(aged-cancelled): %v", err)
	}
	agedBlockedDir, err := m.SybraHomeDir("task-aged-blocked")
	if err != nil {
		t.Fatalf("SybraHomeDir(aged-blocked): %v", err)
	}
	recentBlockedDir, err := m.SybraHomeDir("task-recent-blocked")
	if err != nil {
		t.Fatalf("SybraHomeDir(recent-blocked): %v", err)
	}
	inReviewDir, err := m.SybraHomeDir("task-in-review")
	if err != nil {
		t.Fatalf("SybraHomeDir(in-review): %v", err)
	}

	tasks := []task.Task{
		{ID: "task-aged-done", Status: task.StatusDone, StatusChangedAt: now.Add(-2 * time.Hour)},
		{ID: "task-aged-cancelled", Status: task.StatusCancelled, StatusChangedAt: now.Add(-2 * time.Hour)},
		{ID: "task-aged-blocked", Status: task.StatusBlocked, StatusChangedAt: now.Add(-2 * time.Hour)},
		{ID: "task-recent-blocked", Status: task.StatusBlocked, StatusChangedAt: now},
		{ID: "task-in-review", Status: task.StatusInReview, StatusChangedAt: now.Add(-2 * time.Hour)},
	}
	m.CleanupOrphaned(context.Background(), tasks, nil)

	for _, dir := range []string{agedDoneDir, agedCancelledDir, agedBlockedDir} {
		if _, err := os.Stat(filepath.Dir(dir)); !os.IsNotExist(err) {
			t.Errorf("aged eligible dir %q still exists after cleanup: %v", dir, err)
		}
	}
	for _, dir := range []string{recentBlockedDir, inReviewDir} {
		if _, err := os.Stat(filepath.Dir(dir)); err != nil {
			t.Errorf("preserved dir %q removed unexpectedly: %v", dir, err)
		}
	}
}

func TestManager_CleanupOrphaned_RetentionDisabled(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)
	m.SetRetentionWindow(-1)

	dir, err := m.SybraHomeDir("task-aged-done")
	if err != nil {
		t.Fatalf("SybraHomeDir: %v", err)
	}

	tasks := []task.Task{
		{ID: "task-aged-done", Status: task.StatusDone, StatusChangedAt: time.Now().Add(-1000 * time.Hour)},
	}
	m.CleanupOrphaned(context.Background(), tasks, nil)

	if _, err := os.Stat(filepath.Dir(dir)); err != nil {
		t.Fatalf("eligible dir removed despite disabled retention: %v", err)
	}
}

// TestRunCmd_ContextCancellationKillsProcess proves runCmd's exec.Command is
// built with the caller's context (exec.CommandContext): cancelling ctx must
// kill an in-flight subprocess instead of leaving it to run to completion.
func TestRunCmd_ContextCancellationKillsProcess(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := runCmd(ctx, "", nil, "sleep", "5")
	dur := time.Since(start)

	if err == nil {
		t.Fatal("expected error when context is cancelled mid-run")
	}
	if dur > 3*time.Second {
		t.Errorf("cancellation did not kill the subprocess quickly: took %s", dur)
	}
}

// --- helpers ---

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	return NewManager(dir, slog.New(slog.NewTextHandler(os.Stderr, nil)))
}

func mustReceiveStartErr(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return")
		return nil
	}
}

func writeTempEnvFile(t *testing.T, content string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp env file: %v", err)
	}
	return f
}
