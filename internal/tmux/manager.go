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

type SessionInfo struct {
	Name    string `json:"name"`
	Created string `json:"created"`
}

func (m *Manager) ListSessions() ([]SessionInfo, error) {
	out, err := output("list-sessions", "-F", "#{session_name}\t#{session_created}")
	if err != nil {
		if strings.Contains(err.Error(), "no server running") || strings.Contains(err.Error(), "no sessions") {
			return nil, nil
		}
		return nil, err
	}

	var sessions []SessionInfo
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		info := SessionInfo{Name: parts[0]}
		if len(parts) > 1 {
			info.Created = parts[1]
		}
		sessions = append(sessions, info)
	}
	return sessions, nil
}

func (m *Manager) PaneCommand(name string) (string, error) {
	return output("list-panes", "-t", name, "-F", "#{pane_current_command}")
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
