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
	unlock := a.lockOwnership(taskID)
	defer unlock()
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

	previousNode := t.AssignedNode
	if previousNode == home.Name {
		return a.pinOverride(taskID, node)
	}
	if previousNode == "" && a.stopLocalAgents == nil && mayHaveLiveAgent(t) {
		return fmt.Errorf("%w: %s is running on the leader and this caller cannot stop its agents; reassign it from the board",
			ErrCannotDrainLocal, taskID)
	}
	a.drainTask(ctx, previousNode, t)

	moved, err := a.stampNode(taskID, node, home.Name)
	if err != nil {
		return err
	}
	if home.Local {
		a.logger.Info("cluster.reassign", "task", taskID, "from", previousNode, "to", config.LocalNodeName)
		return nil
	}

	client, ok := a.roster.Client(home.Name)
	if !ok || client == nil {
		a.rollbackNode(taskID, t, moved)
		return fmt.Errorf("clusterlead: no follower client for node %q", home.Name)
	}
	push, err := a.transferAttachments(ctx, client, moved)
	if err != nil {
		a.rollbackNode(taskID, t, moved)
		return fmt.Errorf("clusterlead: transfer attachments for %s to %s: %w", taskID, home.Name, err)
	}
	if err := client.AssignTask(ctx, push); err != nil {
		a.rollbackNode(taskID, t, moved)
		return fmt.Errorf("clusterlead: assign %s to %s: %w", taskID, home.Name, err)
	}
	a.logger.Info("cluster.reassign", "task", taskID, "from", previousNode, "to", home.Name)
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
	if _, _, err := a.tasks.PutBy(cur, "clusterlead.assigner.pin_override"); err != nil {
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
	// Advance the persisted ownership revision even when an ABA move returns
	// to the same node name. UpdatedAt records the ordinary task write.
	cur.AssignmentRev++
	cur.UpdatedAt = time.Now().UTC()
	cur.MirrorRev = 0
	cur.MirrorUpdatedAt = nil
	saved, _, err := a.tasks.PutBy(cur, "clusterlead.assigner.stamp_node")
	if err != nil {
		return task.Task{}, fmt.Errorf("clusterlead: stamp node on %s: %w", taskID, err)
	}
	return saved, nil
}

func (a *Assigner) rollbackNode(taskID string, previous, moved task.Task) {
	cur, err := a.tasks.Get(taskID)
	if err != nil {
		a.logger.Error("cluster.reassign.rollback.failed", "task", taskID, "err", err)
		return
	}
	if cur.AssignedNode != moved.AssignedNode || cur.AssignmentRev != moved.AssignmentRev {
		return
	}
	cur.AssignedNode = previous.AssignedNode
	// A rollback is still an ownership change. Advance the revision so an
	// RPC response from the failed assignment cannot apply afterwards.
	cur.AssignmentRev++
	cur.UpdatedAt = time.Now().UTC()
	cur.NodeOverride = previous.NodeOverride
	cur.WorktreeDir = previous.WorktreeDir
	cur.MirrorRev = previous.MirrorRev
	cur.MirrorUpdatedAt = previous.MirrorUpdatedAt
	if _, _, err := a.tasks.PutBy(cur, "clusterlead.assigner.rollback_node"); err != nil {
		a.logger.Error("cluster.reassign.rollback.failed", "task", taskID, "err", err)
		return
	}
	a.logger.Warn("cluster.reassign.rolled_back", "task", taskID, "node", previous.AssignedNode)
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
