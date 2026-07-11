package agent

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestReapOrphanProviderProcesses(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only process enumeration test")
	}

	prevGrace := stopSIGINTGrace
	stopSIGINTGrace = 20 * time.Millisecond
	t.Cleanup(func() { stopSIGINTGrace = prevGrace })

	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir(), ManagerConfig{})
	root := t.TempDir()
	proc := spawnProviderProcess(t, root)

	if got := m.ReapOrphanProviderProcesses([]string{root}); got != 1 {
		t.Fatalf("reaped = %d, want 1", got)
	}
	waitForProcessExit(t, proc.Process.Pid)
}

func TestReapOrphanProviderProcesses_SkipsTrackedPID(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only process enumeration test")
	}

	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir(), ManagerConfig{})
	root := t.TempDir()
	proc := spawnProviderProcess(t, root)

	m.mu.Lock()
	m.agents["tracked"] = &Agent{ID: "tracked", TaskID: "task-1", PID: proc.Process.Pid}
	m.mu.Unlock()

	if got := m.ReapOrphanProviderProcesses([]string{root}); got != 0 {
		t.Fatalf("reaped = %d, want 0 for tracked pid", got)
	}
	if !processAlive(proc.Process.Pid) {
		t.Fatal("tracked process was killed")
	}
}

func spawnProviderProcess(t *testing.T, root string) *exec.Cmd {
	t.Helper()

	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("sleep not found: %v", err)
	}
	binDir := t.TempDir()
	link := filepath.Join(binDir, "claude")
	if err := os.Symlink(sleepPath, link); err != nil {
		t.Fatalf("symlink sleep -> claude: %v", err)
	}
	cwd := filepath.Join(root, "sandboxes", "task-1", "sybra-home")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir cwd: %v", err)
	}
	cmd := exec.Command(link, "30")
	cmd.Dir = cwd
	if err := cmd.Start(); err != nil {
		t.Fatalf("start provider-shaped process: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	go func() { _ = cmd.Wait() }()
	return cmd
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive after reap", pid)
}
