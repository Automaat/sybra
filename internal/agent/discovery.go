package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type claudeSession struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	StartedAt int64  `json:"startedAt"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
}

func (m *Manager) DiscoverAgents() []*Agent {
	sessions := readClaudeSessions()

	m.mu.RLock()
	trackedPIDs := make(map[int]bool)
	for _, a := range m.agents {
		if a.cmd != nil && a.cmd.Process != nil {
			trackedPIDs[a.cmd.Process.Pid] = true
		}
		if a.PID != 0 {
			trackedPIDs[a.PID] = true
		}
	}
	m.mu.RUnlock()

	var discovered []*Agent
	for _, s := range sessions {
		if trackedPIDs[s.PID] {
			continue
		}

		if !processAlive(s.PID) {
			continue
		}

		a := &Agent{
			ID:        fmt.Sprintf("ext-%d", s.PID),
			Mode:      sessionKind(s.Kind),
			State:     StateRunning,
			External:  true,
			PID:       s.PID,
			SessionID: s.SessionID,
			StartedAt: time.UnixMilli(s.StartedAt).UTC(),
			Name:      agentName(s),
			Project:   projectName(s.CWD),
		}

		m.mu.Lock()
		if _, exists := m.agents[a.ID]; !exists {
			m.agents[a.ID] = a
		}
		m.mu.Unlock()

		discovered = append(discovered, a)
	}
	return discovered
}

func readClaudeSessions() []claudeSession {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	dir := filepath.Join(home, ".claude", "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var sessions []claudeSession
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}

		var s claudeSession
		if err := json.Unmarshal(data, &s); err != nil {
			continue
		}
		if s.PID == 0 {
			continue
		}

		sessions = append(sessions, s)
	}
	return sessions
}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func agentName(s claudeSession) string {
	if s.Name != "" {
		return s.Name
	}
	return projectName(s.CWD)
}

func projectName(cwd string) string {
	if cwd == "" {
		return ""
	}
	return filepath.Base(cwd)
}

func sessionKind(kind string) string {
	if kind == "headless" {
		return "headless"
	}
	return "interactive"
}
