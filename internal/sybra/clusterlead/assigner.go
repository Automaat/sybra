package clusterlead

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/Automaat/sybra/internal/attachment"
	"github.com/Automaat/sybra/internal/cluster"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
)

// Assigner routes a leader's canonical tasks to the follower that homes their
// project: it pushes the task via the follower control plane and stamps
// AssignedNode on the canonical copy so the local dispatch paths (gated on
// HomeNodeFor) leave it to the follower. Inert on a non-leader node.
//
// isWorkProject reports whether a project holds confidential work content; it
// must fail safe (return true when a project's type cannot be determined) so an
// unclassifiable project is never leaked to a follower. auditBlock, when set,
// records a confidentiality refusal.
type Assigner struct {
	cfg           *config.Config
	tasks         *task.Manager
	roster        *cluster.Roster
	isWorkProject func(projectID string) bool
	auditBlock    func(taskID, node, reason string)
	logger        *slog.Logger
	attachments   *attachment.Store

	stopLocalAgents func(taskID string)
}

// SetStopLocalAgents supplies the hook Reassign uses to stop the leader's own
// agents for a task before it is moved onto a follower. Without it, a task
// running locally would keep its local agent alive while the new node starts a
// second one on the same branch.
func (a *Assigner) SetStopLocalAgents(stop func(taskID string)) {
	a.stopLocalAgents = stop
}

// SetAttachments supplies the local blob store used to replicate attachments
// before a task is assigned to a follower.
func (a *Assigner) SetAttachments(store *attachment.Store) {
	a.attachments = store
}

// NewAssigner constructs an Assigner. isWorkProject classifies a project as
// confidential work (must fail safe); auditBlock records a confidentiality
// refusal and may be nil. A nil logger falls back to slog.Default().
func NewAssigner(cfg *config.Config, tasks *task.Manager, roster *cluster.Roster, isWorkProject func(string) bool, auditBlock func(taskID, node, reason string), logger *slog.Logger) *Assigner {
	if logger == nil {
		logger = slog.Default()
	}
	if isWorkProject == nil {
		isWorkProject = func(string) bool { return true }
	}
	return &Assigner{cfg: cfg, tasks: tasks, roster: roster, isWorkProject: isWorkProject, auditBlock: auditBlock, logger: logger}
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
		if task.IsTerminalStatus(t.Status) || t.Status == task.StatusBlocked {
			continue
		}
		home := a.cfg.HomeNodeForTask(t.ProjectID, t.NodeOverride)
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
	return a.route(ctx, t, false)
}

// PushUpdate re-pushes a task that is already assigned to its home follower,
// forwarding a leader-side canonical edit (e.g. the umbrella gate clearing a
// dependency block) that would otherwise never reach the follower: Mirror
// only pulls follower state up to the leader (internal/sybra/clusterlead/mirror.go),
// it never pushes leader edits back down, and Route/Tick push only once, at
// first assignment.
//
// Callers must only use this for a task that has not started executing
// anywhere (e.g. still gated/todo) — AssignTask writes the pushed copy
// verbatim on the follower, so pushing a stale leader-side snapshot over a
// follower's own in-progress execution state (AgentRuns, Workflow, ...) would
// roll it back.
func (a *Assigner) PushUpdate(ctx context.Context, t task.Task) (pushed bool, err error) {
	return a.route(ctx, t, true)
}

// PushFieldUpdate forwards a leader-side Tags/DependsOn edit to a task's home
// follower without touching any other field — the write-time counterpart to
// mirror.Mirror's detect-and-repair backstop, called on every leader-side
// Tags/DependsOn edit (see TaskService.UpdateTask) so that backstop mostly
// has nothing left to do.
//
// Unlike PushUpdate/Route, which push the leader's whole task snapshot and
// are documented safe only for a task that hasn't started executing anywhere
// (AssignTask writes verbatim, and the leader's snapshot can already be
// stale), this re-fetches the follower's own current live state via GetTask
// and patches only Tags/DependsOn onto it before pushing back — identical to
// mirror.Mirror's repairDrift repair pattern. That makes it safe to call
// regardless of the task's execution state: it can never roll back the
// follower's own Status/Workflow/AgentRuns progress.
//
// Returns pushed=false, err=nil when the task doesn't home on a follower, or
// isn't assigned to one yet — Route/Tick's own first push already carries
// the task's current Tags/DependsOn, so there is nothing to forward.
func (a *Assigner) PushFieldUpdate(ctx context.Context, t task.Task) (pushed bool, err error) {
	home := a.cfg.HomeNodeForTask(t.ProjectID, t.NodeOverride)
	if home.Local || t.AssignedNode != home.Name {
		return false, nil
	}
	client, ok := a.roster.Client(home.Name)
	if !ok || client == nil {
		return false, fmt.Errorf("clusterlead: no follower client for node %q", home.Name)
	}
	live, err := client.GetTask(ctx, t.ID)
	if err != nil {
		return false, fmt.Errorf("clusterlead: refetch %s from %s: %w", t.ID, home.Name, err)
	}
	live.Tags = t.Tags
	live.DependsOn = t.DependsOn
	live.AssignedNode = home.Name
	if err := client.AssignTask(ctx, live); err != nil {
		return false, fmt.Errorf("clusterlead: push field update for %s to %s: %w", t.ID, home.Name, err)
	}
	return true, nil
}

func (a *Assigner) route(ctx context.Context, t task.Task, force bool) (routed bool, err error) {
	home := a.cfg.HomeNodeForTask(t.ProjectID, t.NodeOverride)
	if home.Local {
		return false, nil
	}
	if !force && t.AssignedNode == home.Name {
		return false, nil
	}
	if a.isWorkProject(t.ProjectID) && (!home.Trusted || !home.Encrypted) {
		return false, a.blockForConfidentiality(t, home)
	}
	client, ok := a.roster.Client(home.Name)
	if !ok || client == nil {
		return false, fmt.Errorf("clusterlead: no follower client for node %q", home.Name)
	}
	push := t
	push.AssignedNode = home.Name
	push, err = a.transferAttachments(ctx, client, push)
	if err != nil {
		return false, fmt.Errorf("clusterlead: transfer attachments for %s to %s: %w", t.ID, home.Name, err)
	}
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

func (a *Assigner) transferAttachments(ctx context.Context, client *cluster.Client, t task.Task) (task.Task, error) {
	if len(t.Attachments) == 0 {
		return t, nil
	}
	if a.attachments == nil {
		return t, fmt.Errorf("attachment store unavailable")
	}
	next := t
	next.Attachments = make([]task.Attachment, len(t.Attachments))
	for i := range t.Attachments {
		att := t.Attachments[i]
		path, err := a.attachments.Path(t.ID, att.ID)
		if err != nil {
			return t, fmt.Errorf("read attachment %s metadata: %w", att.ID, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return t, fmt.Errorf("read attachment %s blob: %w", att.ID, err)
		}
		imported, err := client.ImportAttachment(ctx, t.ID, att, data)
		if err != nil {
			return t, fmt.Errorf("import attachment %s: %w", att.ID, err)
		}
		next.Attachments[i] = imported
	}
	return next, nil
}

func (a *Assigner) blockForConfidentiality(t task.Task, home config.HomeNode) error {
	reason := fmt.Sprintf("cluster: work task withheld from %q (trusted=%v encrypted=%v); assign a trusted, encrypted follower", home.Name, home.Trusted, home.Encrypted)
	if t.Status == task.StatusBlocked && t.StatusReason == reason {
		return nil
	}
	blocked := task.StatusBlocked
	if _, err := a.tasks.Update(t.ID, task.Update{Status: &blocked, StatusReason: &reason}); err != nil {
		return fmt.Errorf("clusterlead: block work task %s: %w", t.ID, err)
	}
	if a.auditBlock != nil {
		a.auditBlock(t.ID, home.Name, reason)
	}
	a.logger.Warn("cluster.assign.blocked_confidential", "task", t.ID, "node", home.Name, "trusted", home.Trusted, "encrypted", home.Encrypted)
	return nil
}
