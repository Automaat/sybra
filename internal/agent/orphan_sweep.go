package agent

import (
	"context"
	"path/filepath"
	"strings"
)

type providerProcess struct {
	PID     int
	Command string
	CWD     string
}

func (m *Manager) ReapOrphanProviderProcesses(ctx context.Context, roots []string) int {
	roots = canonicalProcessRoots(roots)
	if len(roots) == 0 {
		return 0
	}
	procs := listProviderProcessesUnderRoots(ctx, roots)
	if len(procs) == 0 {
		return 0
	}
	tracked := m.trackedPIDs()
	reaped := 0
	for _, proc := range procs {
		if proc.PID <= 0 {
			continue
		}
		if _, ok := tracked[proc.PID]; ok {
			continue
		}
		m.logger.Warn("agent.orphan.reap", "pid", proc.PID, "cwd", proc.CWD, "command", proc.Command)
		signalPID(proc.PID, stopSIGINTGrace)
		reaped++
	}
	return reaped
}

func (m *Manager) trackedPIDs() map[int]struct{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	tracked := make(map[int]struct{}, len(m.agents))
	for _, a := range m.agents {
		if pid := a.GetPID(); pid > 0 {
			tracked[pid] = struct{}{}
		}
		if cmd := a.GetCmd(); cmd != nil && cmd.Process != nil && cmd.Process.Pid > 0 {
			tracked[cmd.Process.Pid] = struct{}{}
		}
	}
	return tracked
}

func canonicalProcessRoots(roots []string) []string {
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		} else {
			root = filepath.Clean(root)
		}
		out = append(out, root)
	}
	return out
}

func pathWithinRoots(path string, roots []string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	} else {
		path = filepath.Clean(path)
	}
	for _, root := range roots {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func isProviderProcessName(name string) bool {
	name = strings.TrimSpace(strings.ToLower(filepath.Base(name)))
	switch name {
	case "claude", "codex", "copilot":
		return true
	default:
		return false
	}
}
