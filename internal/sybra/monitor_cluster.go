package sybra

import (
	"context"
	"log/slog"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/cluster"
)

// monitorAgentLister gives the leader's monitor a merged live-agent snapshot:
// local agents plus every follower's current agent list. Followers use the
// same type with a nil roster, so they stay local-only.
type monitorAgentLister struct {
	local  *agent.Manager
	roster *cluster.Roster
	logger *slog.Logger
}

func newMonitorAgentLister(local *agent.Manager, roster *cluster.Roster, logger *slog.Logger) monitorAgentLister {
	return monitorAgentLister{local: local, roster: roster, logger: logger}
}

func (l monitorAgentLister) ListAgents() []*agent.Agent {
	var out []*agent.Agent
	if l.local != nil {
		out = append(out, l.local.ListAgents()...)
	}
	if l.roster == nil {
		return out
	}
	for _, name := range l.roster.Names() {
		client, ok := l.roster.Client(name)
		if !ok || client == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), nodeListTimeout)
		agents, err := client.ListAgents(ctx)
		cancel()
		if err != nil {
			if l.logger != nil {
				l.logger.Debug("monitor.cluster.list_agents.failed", "node", name, "err", err)
			}
			continue
		}
		for _, ag := range agents {
			if ag == nil {
				continue
			}
			ag.Node = name
			out = append(out, ag)
		}
	}
	return out
}

// recoverLostAgentTask keeps lost-agent ownership with the canonical task
// owner. Local tasks recover through the local recovery loop; follower-homed
// tasks are recovered by an explicit RPC to the assigned node.
func (a *App) recoverLostAgentTask(ctx context.Context, taskID string) {
	if a == nil || a.tasks == nil || taskID == "" {
		return
	}
	t, err := a.tasks.Get(taskID)
	if err != nil {
		a.logger.Warn("monitor.recover.get.failed", "task_id", taskID, "err", err)
		return
	}
	if a.runsTaskLocally(t) {
		if a.recovery == nil {
			a.logger.Warn("monitor.recover.local.skipped", "task_id", taskID, "reason", "recovery_unavailable")
			return
		}
		if err := a.recovery.RestartTaskIfStale(ctx, taskID); err != nil {
			a.logger.Error("monitor.recover.local.failed", "task_id", taskID, "err", err)
		}
		return
	}
	if a.clusterRoster == nil {
		a.logger.Warn("monitor.recover.remote.skipped", "task_id", taskID, "reason", "cluster_roster_unavailable")
		return
	}
	node := t.AssignedNode
	if node == "" && a.cfg != nil {
		node = a.cfg.HomeNodeForTask(t.ProjectID, t.NodeOverride).Name
	}
	client, ok := a.clusterRoster.Client(node)
	if !ok || client == nil {
		a.logger.Warn("monitor.recover.remote.skipped", "task_id", taskID, "node", node, "reason", "cluster_node_unavailable")
		return
	}
	if err := client.RecoverLostAgent(ctx, taskID); err != nil {
		a.logger.Error("monitor.recover.remote.failed", "task_id", taskID, "node", node, "err", err)
	}
}
