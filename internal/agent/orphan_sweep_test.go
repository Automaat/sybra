package agent

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
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

	if got := m.ReapOrphanProviderProcesses(context.Background(), []string{root}); got != 1 {
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

	if got := m.ReapOrphanProviderProcesses(context.Background(), []string{root}); got != 0 {
		t.Fatalf("reaped = %d, want 0 for tracked pid", got)
	}
	if !processAlive(proc.Process.Pid) {
		t.Fatal("tracked process was killed")
	}
}

func TestReapOrphanProviderProcesses_ReapsOwnedHeadlessMCPHelper(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only process enumeration test")
	}

	prevGrace := stopSIGINTGrace
	stopSIGINTGrace = 20 * time.Millisecond
	t.Cleanup(func() { stopSIGINTGrace = prevGrace })

	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir(), ManagerConfig{})
	root := t.TempDir()
	proc := spawnOwnedMCPHelperProcess(t, root, "chrome-devtools-mcp", mcpOwner{
		AgentID: "agent-1",
		TaskID:  "task-1",
		Mode:    "headless",
	})

	if got := m.ReapOrphanProviderProcesses(context.Background(), []string{root}); got != 1 {
		t.Fatalf("reaped = %d, want 1", got)
	}
	waitForProcessExit(t, proc.Process.Pid)
}

func TestReapOrphanProviderProcesses_ReapsOwnedNonProviderDescendantAfterProviderExit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only process enumeration test")
	}

	prevGrace := stopSIGINTGrace
	stopSIGINTGrace = 20 * time.Millisecond
	t.Cleanup(func() { stopSIGINTGrace = prevGrace })

	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir(), ManagerConfig{})
	root := t.TempDir()
	childPID := spawnOwnedProviderDescendantProcess(t, root, processOwner{
		AgentID: "agent-1",
		TaskID:  "task-1",
		Mode:    "headless",
	})

	if got := m.ReapOrphanProviderProcesses(context.Background(), []string{root}); got != 1 {
		t.Fatalf("reaped = %d, want 1", got)
	}
	waitForProcessExit(t, childPID)
}

func TestReapOrphanProviderProcesses_SkipsTrackedOwnedHeadlessMCPHelper(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only process enumeration test")
	}

	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir(), ManagerConfig{})
	root := t.TempDir()
	proc := spawnOwnedMCPHelperProcess(t, root, "chrome-devtools-mcp", mcpOwner{
		AgentID: "agent-1",
		TaskID:  "task-1",
		Mode:    "headless",
	})

	m.mu.Lock()
	m.agents["agent-1"] = &Agent{ID: "agent-1", TaskID: "task-1", Mode: "headless", State: StateRunning}
	m.mu.Unlock()

	if got := m.ReapOrphanProviderProcesses(context.Background(), []string{root}); got != 0 {
		t.Fatalf("reaped = %d, want 0 for tracked helper owner", got)
	}
	if !processAlive(proc.Process.Pid) {
		t.Fatal("tracked helper was killed")
	}
}

func TestReapOrphanProviderProcesses_SkipsInteractiveOwnedMCPHelper(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only process enumeration test")
	}

	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir(), ManagerConfig{})
	root := t.TempDir()
	proc := spawnOwnedMCPHelperProcess(t, root, "chrome-devtools-mcp", mcpOwner{
		AgentID: "agent-1",
		TaskID:  "task-1",
		Mode:    "interactive",
	})

	if got := m.ReapOrphanProviderProcesses(context.Background(), []string{root}); got != 0 {
		t.Fatalf("reaped = %d, want 0 for interactive-owned helper", got)
	}
	if !processAlive(proc.Process.Pid) {
		t.Fatal("interactive helper was killed")
	}
}

func TestReapOrphanProviderProcesses_ReapsDeletedCWDOrphan(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only process enumeration test")
	}

	prevGrace := stopSIGINTGrace
	stopSIGINTGrace = 20 * time.Millisecond
	t.Cleanup(func() { stopSIGINTGrace = prevGrace })

	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir(), ManagerConfig{})
	root := t.TempDir()
	proc, cwd := spawnGenericProcess(t, root, "sleep")
	if err := os.RemoveAll(cwd); err != nil {
		t.Fatalf("remove cwd: %v", err)
	}
	waitForDeletedCWD(t, proc.Process.Pid)

	if got := m.ReapOrphanProviderProcesses(context.Background(), []string{root}); got != 1 {
		t.Fatalf("reaped = %d, want 1", got)
	}
	waitForProcessExit(t, proc.Process.Pid)
}

func TestReapOrphanProviderProcesses_CanceledContextLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only process enumeration test")
	}
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir(), ManagerConfig{})

	if got := m.ReapOrphanProviderProcesses(ctx, []string{t.TempDir()}); got != 0 {
		t.Fatalf("reaped = %d, want 0 for canceled scan", got)
	}
}

func TestOrphanSweepRootsForAgent_UsesResolvedSandboxHome(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	sandboxHome := t.TempDir()
	got := orphanSweepRootsForAgent(&Agent{
		TaskID:         "task-1",
		Mode:           "headless",
		sessionCWD:     cwd,
		sandboxHomeDir: sandboxHome,
	})
	want := canonicalProcessRoots([]string{cwd, sandboxHome})
	if len(got) != len(want) {
		t.Fatalf("roots len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("roots[%d] = %q, want %q (all=%v)", i, got[i], want[i], got)
		}
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

func spawnOwnedProviderDescendantProcess(t *testing.T, root string, owner processOwner) int {
	t.Helper()

	binDir := t.TempDir()
	script := filepath.Join(binDir, "claude")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30 >/dev/null 2>&1 &\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write provider script: %v", err)
	}
	cwd := filepath.Join(root, "worktrees", "task-1")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir cwd: %v", err)
	}
	cmd := exec.Command(script)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), processOwnerAssignments(owner)...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start provider script: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait provider script: %v", err)
	}
	childPID := waitForOwnedProcessUnderRoot(t, []string{cwd}, owner, "sleep")
	t.Cleanup(func() {
		if processAlive(childPID) {
			_ = syscall.Kill(childPID, syscall.SIGKILL)
		}
	})
	return childPID
}

func spawnOwnedMCPHelperProcess(t *testing.T, root, name string, owner mcpOwner) *exec.Cmd {
	t.Helper()

	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("sleep not found: %v", err)
	}
	binDir := t.TempDir()
	link := filepath.Join(binDir, name)
	if err := os.Symlink(sleepPath, link); err != nil {
		t.Fatalf("symlink sleep -> %s: %v", name, err)
	}
	cwd := filepath.Join(root, "sandboxes", "task-1", "sybra-home")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir cwd: %v", err)
	}
	cmd := exec.Command(link, "30")
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), mcpOwnerAssignments(owner)...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start owned helper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	go func() { _ = cmd.Wait() }()
	return cmd
}

func spawnGenericProcess(t *testing.T, root, name string) (*exec.Cmd, string) {
	t.Helper()

	bin, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("%s not found: %v", name, err)
	}
	cwd := filepath.Join(root, "worktrees", "task-1")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir cwd: %v", err)
	}
	cmd := exec.Command(bin, "30")
	cmd.Dir = cwd
	if err := cmd.Start(); err != nil {
		t.Fatalf("start generic process: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	go func() { _ = cmd.Wait() }()
	return cmd, cwd
}

func waitForOwnedProcessUnderRoot(t *testing.T, roots []string, owner processOwner, wantCommand string) int {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		procs := listProviderProcessesUnderRoots(context.Background(), roots)
		for _, proc := range procs {
			if proc.Owner == owner && proc.Command == wantCommand {
				return proc.PID
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("owned process %q under %v not observed", wantCommand, roots)
	return 0
}

func waitForDeletedCWD(t *testing.T, pid int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	link := filepath.Join("/proc", strconv.Itoa(pid), "cwd")
	for time.Now().Before(deadline) {
		cwd, err := os.Readlink(link)
		if err == nil && normalizeObservedProcessPath(cwd) != cwd {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d cwd never showed deleted suffix", pid)
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
