package tmux

import (
	"os/exec"
	"testing"
)

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
}

func TestNewManager(t *testing.T) {
	m := NewManager()
	if m == nil {
		t.Fatal("manager is nil")
	}
}

func TestSessionExistsNonexistent(t *testing.T) {
	requireTmux(t)
	m := NewManager()
	if m.SessionExists("synapse-nonexistent-test-xyz") {
		t.Error("nonexistent session should return false")
	}
}

func TestKillSessionNonexistent(t *testing.T) {
	requireTmux(t)
	m := NewManager()
	err := m.KillSession("synapse-nonexistent-test-xyz")
	if err == nil {
		t.Error("expected error when killing nonexistent session")
	}
}

func TestCapturePaneNonexistent(t *testing.T) {
	requireTmux(t)
	m := NewManager()
	_, err := m.CapturePaneOutput("synapse-nonexistent-test-xyz")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestPaneCommandNonexistent(t *testing.T) {
	requireTmux(t)
	m := NewManager()
	_, err := m.PaneCommand("synapse-nonexistent-test-xyz")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestListSessions(t *testing.T) {
	requireTmux(t)
	m := NewManager()
	sessions, err := m.ListSessions()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = sessions
}

func TestSendKeysNonexistent(t *testing.T) {
	requireTmux(t)
	m := NewManager()
	err := m.SendKeys("synapse-nonexistent-test-xyz", "echo hello")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestCreateAndCleanupSession(t *testing.T) {
	requireTmux(t)
	m := NewManager()

	name := "synapse-test-unit"
	_ = m.KillSession(name)

	err := m.CreateSession(name, "sleep 60")
	if err != nil {
		t.Skipf("tmux not available: %v", err)
	}
	t.Cleanup(func() { _ = m.KillSession(name) })

	if !m.SessionExists(name) {
		t.Error("session should exist after creation")
	}

	output, err := m.CapturePaneOutput(name)
	if err != nil {
		t.Errorf("capture pane: %v", err)
	}
	_ = output

	paneCmd, err := m.PaneCommand(name)
	if err != nil {
		t.Errorf("pane command: %v", err)
	}
	if paneCmd == "" {
		t.Error("pane command should not be empty")
	}

	sessions, err := m.ListSessions()
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	found := false
	for _, s := range sessions {
		if s.Name == name {
			found = true
		}
	}
	if !found {
		t.Error("created session not found in list")
	}

	if err := m.KillSession(name); err != nil {
		t.Errorf("kill session: %v", err)
	}
	if m.SessionExists(name) {
		t.Error("session should not exist after kill")
	}
}
