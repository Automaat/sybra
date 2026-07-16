package agent

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/task"
)

const orphanSweepTimeout = 2 * time.Second

var orphanSweepDefaultContext = context.Background()

type providerProcess struct {
	PID     int
	Command string
	CWD     string
	Owner   processOwner
}

type trackedAgentSnapshot struct {
	State State
}

func (m *Manager) ReapOrphanProviderProcesses(ctx context.Context, roots []string) int { //nolint:contextcheck // Nil is a legacy caller contract; normalize it before deriving the bounded sweep context.
	roots = canonicalProcessRoots(roots)
	if len(roots) == 0 {
		return 0
	}
	if ctx == nil {
		ctx = orphanSweepDefaultContext
	}
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, orphanSweepTimeout)
	defer cancel()

	procs := listProviderProcessesUnderRoots(ctx, roots)
	if len(procs) == 0 {
		return 0
	}
	trackedPIDs, trackedAgents := m.trackedProcessOwners()
	reaped := 0
	for _, proc := range procs {
		if proc.PID <= 0 {
			continue
		}
		if _, ok := trackedPIDs[proc.PID]; ok {
			continue
		}
		if proc.Owner.AgentID != "" {
			if !shouldReapOwnedProcess(proc, trackedAgents) {
				continue
			}
			m.logger.Warn("agent.orphan.owned_reap", "pid", proc.PID, "cwd", proc.CWD, "command", proc.Command, "agent_id", proc.Owner.AgentID, "task_id", proc.Owner.TaskID, "mode", proc.Owner.Mode)
			signalPID(proc.PID, stopSIGINTGrace)
			reaped++
			continue
		}
		m.logger.Warn("agent.orphan.reap", "pid", proc.PID, "cwd", proc.CWD, "command", proc.Command)
		signalPID(proc.PID, stopSIGINTGrace)
		reaped++
	}
	return reaped
}

func shouldReapOwnedProcess(proc providerProcess, tracked map[string]trackedAgentSnapshot) bool {
	owner := proc.Owner
	if owner.AgentID == "" || owner.Mode != task.AgentModeHeadless {
		return false
	}
	if live, ok := tracked[owner.AgentID]; ok && live.State != StateStopped {
		return false
	}
	return true
}

func (m *Manager) trackedProcessOwners() (tracked map[int]struct{}, owners map[string]trackedAgentSnapshot) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	tracked = make(map[int]struct{}, len(m.agents))
	owners = make(map[string]trackedAgentSnapshot, len(m.agents))
	for _, a := range m.agents {
		if pid := a.GetPID(); pid > 0 {
			tracked[pid] = struct{}{}
		}
		if cmd := a.GetCmd(); cmd != nil && cmd.Process != nil && cmd.Process.Pid > 0 {
			tracked[cmd.Process.Pid] = struct{}{}
		}
		owners[a.ID] = trackedAgentSnapshot{State: a.GetState()}
	}
	return tracked, owners
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
	path = normalizeObservedProcessPath(path)
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

func normalizeObservedProcessPath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.TrimSuffix(path, " (deleted)")
	return strings.TrimSpace(path)
}

func isProviderProcessName(name string) bool {
	name = strings.TrimSpace(strings.ToLower(filepath.Base(name)))
	return providerid.IsKnown(name)
}

func orphanSweepRootsForAgent(a *Agent) []string {
	if a == nil || a.Mode != task.AgentModeHeadless {
		return nil
	}
	var roots []string
	if cwd := strings.TrimSpace(a.sessionCWD); cwd != "" {
		roots = append(roots, cwd)
	}
	if dir := strings.TrimSpace(a.sandboxHomeDir); dir != "" {
		roots = append(roots, dir)
	}
	return canonicalProcessRoots(roots)
}
