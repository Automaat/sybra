package clusterlead

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Automaat/sybra/internal/cluster"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
)

// Assigner routes a leader's canonical tasks to the follower that homes their
// project: it pushes the task via the follower control plane and stamps
// AssignedNode on the canonical copy so the local dispatch paths (gated on
// HomeNodeFor) leave it to the follower. Inert on a non-leader node.
type Assigner struct {
	cfg    *config.Config
	tasks  *task.Manager
	roster *cluster.Roster
	logger *slog.Logger
}

// NewAssigner constructs an Assigner. A nil logger falls back to slog.Default().
func NewAssigner(cfg *config.Config, tasks *task.Manager, roster *cluster.Roster, logger *slog.Logger) *Assigner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Assigner{cfg: cfg, tasks: tasks, roster: roster, logger: logger}
}

// Tick scans the canonical store and routes every non-terminal task whose
// project homes on a remote follower and that has not been routed there yet. It
// is idempotent: a task already stamped with its home node is skipped, so
// re-running the tick never re-pushes. Inert on a non-leader node.
func (a *Assigner) Tick(ctx context.Context) {
	if a.cfg == nil || !a.cfg.IsLeader() || a.tasks == nil || a.roster == nil {
		return
	}
	tasks, err := a.tasks.List()
	if err != nil {
		a.logger.Warn("cluster.assign.list.failed", "err", err)
		return
	}
	for i := range tasks {
		t := tasks[i]
		if task.IsTerminalStatus(t.Status) || t.TaskType == task.TaskTypeChat {
			continue
		}
		home := a.cfg.HomeNodeFor(t.ProjectID)
		if home.Local || t.AssignedNode == home.Name {
			continue
		}
		if _, err := a.Route(ctx, t); err != nil {
			a.logger.Warn("cluster.assign.failed", "task", t.ID, "node", home.Name, "err", err)
			continue
		}
		a.logger.Info("cluster.assign.routed", "task", t.ID, "node", home.Name)
	}
}

// Route pushes task t to its home follower and stamps AssignedNode on the
// canonical copy. It returns routed=false with no error when the task homes
// locally (the caller keeps the existing local path). Routing is best-effort
// idempotent: the follower's AssignTask upserts by id, so a repeated push is
// harmless.
func (a *Assigner) Route(ctx context.Context, t task.Task) (routed bool, err error) {
	home := a.cfg.HomeNodeFor(t.ProjectID)
	if home.Local {
		return false, nil
	}
	client, ok := a.roster.Client(home.Name)
	if !ok || client == nil {
		return false, fmt.Errorf("clusterlead: no follower client for node %q", home.Name)
	}
	push := t
	push.AssignedNode = home.Name
	if err := client.AssignTask(ctx, push); err != nil {
		return false, fmt.Errorf("clusterlead: assign %s to %s: %w", t.ID, home.Name, err)
	}
	cur, err := a.tasks.Get(t.ID)
	if err != nil {
		return true, fmt.Errorf("clusterlead: reload %s after assign: %w", t.ID, err)
	}
	cur.AssignedNode = home.Name
	if _, _, err := a.tasks.Put(cur); err != nil {
		return true, fmt.Errorf("clusterlead: stamp assigned_node on %s: %w", t.ID, err)
	}
	return true, nil
}
