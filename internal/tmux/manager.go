package tmux

import (
	"fmt"
	"os"
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

func (m *Manager) CreateSessionInDir(name, cmd, dir string) error {
	return run("new-session", "-d", "-s", name, "-x", "200", "-y", "50", "-c", dir, cmd)
}

// SendKeys pastes text into a tmux pane via load-buffer + paste-buffer.
// Does NOT send Enter — caller is responsible for submitting.
func (m *Manager) SendKeys(name, keys string) error {
	f, err := os.CreateTemp("", "synapse-prompt-*.txt")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()

	if _, err := f.WriteString(keys); err != nil {
		_ = f.Close()
		return fmt.Errorf("write prompt: %w", err)
	}
	_ = f.Close()

	if err := run("load-buffer", f.Name()); err != nil {
		return fmt.Errorf("load-buffer: %w", err)
	}
	return run("paste-buffer", "-t", name)
}

// SendRawKeys sends tmux key names (e.g. "Down", "Enter") directly via send-keys.
func (m *Manager) SendRawKeys(name string, keys ...string) error {
	args := append([]string{"send-keys", "-t", name}, keys...)
	return run(args...)
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
		if strings.Contains(err.Error(), "no server running") || strings.Contains(err.Error(), "no sessions") || strings.Contains(err.Error(), "No such file or directory") {
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

func (m *Manager) PanePID(name string) (string, error) {
	return output("list-panes", "-t", name, "-F", "#{pane_pid}")
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
