package clusterlead

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
)

const drainOldNodeTimeout = 10 * time.Second

// ErrUnknownNode is an operator-facing refusal: its text is the operator's own
// input, so a caller may surface it. Any other Reassign failure may wrap a
// follower's response and must be sanitized before it reaches a client.
var ErrUnknownNode = errors.New("unknown cluster node")

// ErrConfidentiality reports that a work task was withheld from a node that is
// not both trusted and encrypted. Safe for a caller to surface.
var ErrConfidentiality = errors.New("work task withheld from untrusted node")

// Reassign moves a task to an explicitly named node, overriding its project's
// configured home. This is the manual escape hatch for a dead or degraded
// follower: the new node re-provisions its own worktree from the branch, since
// a worktree is not transferable between machines.
//
// The confidentiality guard still applies — a manual reassignment must not be a
// way to route a work task onto an untrusted or cleartext node.
//
// Ordering matters. The canonical AssignedNode is written BEFORE the task is
// pushed, because the mirror ignores any follower whose name no longer matches
// canonical.AssignedNode: that is what makes a late update from the old node —
// including one from a dead follower that comes back — unable to clobber the
// task after it has moved.
func (a *Assigner) Reassign(ctx context.Context, taskID, node string) error {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return fmt.Errorf("clusterlead: load %s: %w", taskID, err)
	}
	home, ok := a.cfg.HomeNodeByName(node)
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownNode, node)
	}
	if !home.Local && a.isWorkProject(t.ProjectID) && (!home.Trusted || !home.Encrypted) {
		if a.auditBlock != nil {
			a.auditBlock(t.ID, home.Name, "manual reassignment to an untrusted or cleartext node")
		}
		return fmt.Errorf("%w: %q (trusted=%v encrypted=%v)", ErrConfidentiality, home.Name, home.Trusted, home.Encrypted)
	}

	previous := t.AssignedNode
	if previous == home.Name {
		return nil
	}
	a.drainNode(ctx, previous, t)

	t.AssignedNode = home.Name
	t.WorktreeDir = ""
	t.MirrorRev = 0
	t.MirrorUpdatedAt = nil
	if _, _, err := a.tasks.Put(t); err != nil {
		return fmt.Errorf("clusterlead: stamp node on %s: %w", t.ID, err)
	}

	if home.Local {
		a.logger.Info("cluster.reassign", "task", t.ID, "from", previous, "to", config.LocalNodeName)
		return nil
	}

	client, ok := a.roster.Client(home.Name)
	if !ok || client == nil {
		return fmt.Errorf("clusterlead: no follower client for node %q", home.Name)
	}
	if err := client.AssignTask(ctx, t); err != nil {
		return fmt.Errorf("clusterlead: assign %s to %s: %w", t.ID, home.Name, err)
	}
	a.logger.Info("cluster.reassign", "task", t.ID, "from", previous, "to", home.Name)
	return nil
}

func (a *Assigner) drainNode(ctx context.Context, node string, t task.Task) {
	if node == "" {
		return
	}
	client, ok := a.roster.Client(node)
	if !ok || client == nil {
		return
	}
	drainCtx, cancel := context.WithTimeout(ctx, drainOldNodeTimeout)
	defer cancel()

	agents, err := client.ListAgents(drainCtx)
	if err != nil {
		a.logger.Warn("cluster.reassign.drain.unreachable", "task", t.ID, "node", node, "err", err)
		return
	}
	for _, ag := range agents {
		if ag == nil || ag.TaskID != t.ID {
			continue
		}
		if err := client.StopAgent(drainCtx, ag.ID); err != nil {
			a.logger.Warn("cluster.reassign.drain.stop_failed", "task", t.ID, "node", node, "agent", ag.ID, "err", err)
			continue
		}
		a.logger.Info("cluster.reassign.drain.stopped", "task", t.ID, "node", node, "agent", ag.ID)
	}
}
