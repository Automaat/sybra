package clusterlead

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// ErrCannotDrainLocal reports that a task running on the leader cannot be moved
// by this caller, because it has no way to stop the leader's own agents — the
// CLI builds its own Assigner and does not own the running server's agent
// manager. Failing closed beats letting two agents drive one branch. Safe for a
// caller to surface.
var ErrCannotDrainLocal = errors.New("cannot stop the leader's agents from here")

func mayHaveLiveAgent(t task.Task) bool {
	if task.IsTerminalStatus(t.Status) {
		return false
	}
	return t.Status != task.StatusNew && t.Status != task.StatusTodo
}

// Reassign moves a task to an explicitly named node, overriding its project's
// configured home. This is the manual escape hatch for a dead or degraded
// follower: the new node re-provisions its own worktree from the branch, since
// a worktree is not transferable between machines.
//
// It records NodeOverride, not just AssignedNode. AssignedNode is where the task
// last ran; the routing authority is HomeNodeForTask, and without an override it
// recomputes the project's configured home on the next tick — which would drag
// the task straight back onto the node the operator just evacuated.
//
// The confidentiality guard still applies: a manual reassignment must not be a
// way to route a work task onto an untrusted or cleartext node.
//
// Reassigning a task to the node it already runs on only pins the override: it
// must not drain, clear the worktree, or re-push, since that run is live.
//
// The canonical record is written BEFORE the push, because the mirror ignores
// any follower whose name no longer matches canonical.AssignedNode — that is
// what stops a revived dead follower from clobbering a task that has moved. If
// the push then fails, the move is rolled back so the task is never left
// pointing at a node that never received it.
func (a *Assigner) Reassign(ctx context.Context, taskID, node string) error {
	node = strings.TrimSpace(node)
	if node == "" {
		return fmt.Errorf("%w: %q", ErrUnknownNode, node)
	}
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
	previousOverride := t.NodeOverride
	if previous == home.Name {
		return a.pinOverride(taskID, node)
	}
	if previous == "" && a.stopLocalAgents == nil && mayHaveLiveAgent(t) {
		return fmt.Errorf("%w: %s is running on the leader and this caller cannot stop its agents; reassign it from the board",
			ErrCannotDrainLocal, taskID)
	}
	a.drainTask(ctx, previous, t)

	moved, err := a.stampNode(taskID, node, home.Name)
	if err != nil {
		return err
	}
	if home.Local {
		a.logger.Info("cluster.reassign", "task", taskID, "from", previous, "to", config.LocalNodeName)
		return nil
	}

	client, ok := a.roster.Client(home.Name)
	if !ok || client == nil {
		a.rollbackNode(taskID, previous, previousOverride, moved)
		return fmt.Errorf("clusterlead: no follower client for node %q", home.Name)
	}
	if err := client.AssignTask(ctx, moved); err != nil {
		a.rollbackNode(taskID, previous, previousOverride, moved)
		return fmt.Errorf("clusterlead: assign %s to %s: %w", taskID, home.Name, err)
	}
	a.logger.Info("cluster.reassign", "task", taskID, "from", previous, "to", home.Name)
	return nil
}

func (a *Assigner) pinOverride(taskID, override string) error {
	cur, err := a.tasks.Get(taskID)
	if err != nil {
		return fmt.Errorf("clusterlead: reload %s: %w", taskID, err)
	}
	if cur.NodeOverride == override {
		return nil
	}
	cur.NodeOverride = override
	if _, _, err := a.tasks.Put(cur); err != nil {
		return fmt.Errorf("clusterlead: pin node on %s: %w", taskID, err)
	}
	return nil
}

func (a *Assigner) stampNode(taskID, override, assigned string) (task.Task, error) {
	cur, err := a.tasks.Get(taskID)
	if err != nil {
		return task.Task{}, fmt.Errorf("clusterlead: reload %s: %w", taskID, err)
	}
	cur.NodeOverride = override
	cur.AssignedNode = assigned
	cur.WorktreeDir = ""
	cur.MirrorRev = 0
	cur.MirrorUpdatedAt = nil
	saved, _, err := a.tasks.Put(cur)
	if err != nil {
		return task.Task{}, fmt.Errorf("clusterlead: stamp node on %s: %w", taskID, err)
	}
	return saved, nil
}

func (a *Assigner) rollbackNode(taskID, previous, previousOverride string, moved task.Task) {
	cur, err := a.tasks.Get(taskID)
	if err != nil {
		a.logger.Error("cluster.reassign.rollback.failed", "task", taskID, "err", err)
		return
	}
	if cur.AssignedNode != moved.AssignedNode {
		return
	}
	cur.AssignedNode = previous
	cur.NodeOverride = previousOverride
	if _, _, err := a.tasks.Put(cur); err != nil {
		a.logger.Error("cluster.reassign.rollback.failed", "task", taskID, "err", err)
		return
	}
	a.logger.Warn("cluster.reassign.rolled_back", "task", taskID, "node", previous)
}

func (a *Assigner) drainTask(ctx context.Context, node string, t task.Task) {
	if node == "" {
		a.drainLocal(t)
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

func (a *Assigner) drainLocal(t task.Task) {
	if a.stopLocalAgents == nil {
		return
	}
	a.stopLocalAgents(t.ID)
	a.logger.Info("cluster.reassign.drain.stopped_local", "task", t.ID)
}
