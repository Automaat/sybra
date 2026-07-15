package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSignalTarget_GroupLeaderVsMember(t *testing.T) {
	leader := exec.Command("sleep", "30")
	leader.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := leader.Start(); err != nil {
		t.Fatalf("start leader: %v", err)
	}
	t.Cleanup(func() { _ = leader.Process.Kill() })
	go func() { _ = leader.Wait() }()

	member := exec.Command("sleep", "30")
	if err := member.Start(); err != nil {
		t.Fatalf("start member: %v", err)
	}
	t.Cleanup(func() { _ = member.Process.Kill() })
	go func() { _ = member.Wait() }()

	if got := signalTarget(leader.Process.Pid); got != -leader.Process.Pid {
		t.Errorf("session leader: signalTarget=%d, want %d (whole group)", got, -leader.Process.Pid)
	}
	if got := signalTarget(member.Process.Pid); got != member.Process.Pid {
		t.Errorf("group member: signalTarget=%d, want %d (pid only, never a group it merely joined)", got, member.Process.Pid)
	}
}

func TestReapProcessGroup_KillsDescendants(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	leader := exec.Command("sh", "-c", "sleep 60 & echo $! > "+pidFile+"; wait")
	leader.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := leader.Start(); err != nil {
		t.Fatalf("start leader: %v", err)
	}
	go func() { _ = leader.Wait() }()

	var childPID int
	deadline := time.After(3 * time.Second)
	for childPID == 0 {
		select {
		case <-deadline:
			_ = syscall.Kill(-leader.Process.Pid, syscall.SIGKILL)
			t.Fatal("child pid file never written")
		case <-time.After(20 * time.Millisecond):
		}
		if data, err := os.ReadFile(pidFile); err == nil {
			if p, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				childPID = p
			}
		}
	}

	reapProcessGroup(leader.Process.Pid)

	gone := time.After(3 * time.Second)
	for processAlive(childPID) {
		select {
		case <-gone:
			t.Fatalf("descendant pid %d survived process-group reap", childPID)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
