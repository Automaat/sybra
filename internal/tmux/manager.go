package tmux

import (
	"fmt"
	"os/exec"
	"strings"
)

type Manager struct{}

func NewManager() *Manager {
	return &Manager{}
}

func (m *Manager) CreateSession(name, cmd string) error {
	return run("new-session", "-d", "-s", name, "-x", "200", "-y", "50", cmd)
}

func (m *Manager) SendKeys(name, keys string) error {
	return run("send-keys", "-t", name, keys, "Enter")
}

func (m *Manager) CapturePaneOutput(name string) (string, error) {
	return output("capture-pane", "-t", name, "-p")
}

func (m *Manager) KillSession(name string) error {
	return run("kill-session", "-t", name)
}

func (m *Manager) SessionExists(name string) bool {
	return run("has-session", "-t", name) == nil
}

func run(args ...string) error {
	cmd := exec.Command("tmux", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tmux %s: %s: %w", args[0], strings.TrimSpace(string(out)), err)
	}
	return nil
}

func output(args ...string) (string, error) {
	cmd := exec.Command("tmux", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %s: %w", args[0], strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}
