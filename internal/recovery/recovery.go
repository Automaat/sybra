// Package recovery handles boot-time and periodic recovery of tasks and
// agents whose state survived a crash or restart. The startup pass runs
// once from App.Startup; RestartStaleInProgress is also called from the
// orchestrator loop every minute to catch agents that died after boot.
package recovery

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

// Orchestrator is the subset of *agentorch.Orchestrator that recovery
// uses. Defined here so the package does not import internal/sybra/agentorch
// directly, keeping recovery a low-level leaf package.
type Orchestrator interface {
	StartAgent(taskID, mode, prompt string, includeTaskDescription, oneShot bool) (*agent.Agent, error)
	StartPRFixAgent(taskID string) error
}

// ProjectGetter is the subset of *project.Store needed to decide whether
// a respawned agent should create a draft PR (work) or a regular PR (pet).
type ProjectGetter interface {
	Get(id string) (project.Project, error)
}

// WorkflowRestarter is the subset of *workflow.Engine that recovery needs.
// Defined as an interface so tests can stub it without wiring the full engine.
type WorkflowRestarter interface {
	StartWorkflow(taskID, workflowID string) error
	HandleAgentComplete(taskID string, completion workflow.AgentCompletion)
}

// Recovery owns the dependencies needed by the boot-time cleanup pass and
// the periodic restart-stale sweep. Construct once during App.Startup,
// reuse from the orchestrator loop.
type Recovery struct {
	Tasks          *task.Manager
	Agents         *agent.Manager
	Worktrees      *worktree.Manager
	WorkflowEngine WorkflowRestarter // optional; nil-safe
	Orchestrator   Orchestrator
	Projects       ProjectGetter
	Logger         *slog.Logger
	Throttle       *logging.ErrorThrottle
	WG             *sync.WaitGroup

	LogDir       string
	LogRetention time.Duration // 0 disables age-based pruning

	// TrashRetentionDays bounds how long a soft-deleted task (see
	// task.Store.Delete) survives under the trash dir before
	// pruneTrash permanently removes it. A negative value disables
	// pruning.
	TrashRetentionDays int
}

// RunStartupCleanup sequences boot-time maintenance in the order that lets
// each step see the output of the previous one: chats first so their
// worktrees show up as orphans to the subsequent sweep; stale run state
// next so restart-stale sees a clean slate.
func (r *Recovery) RunStartupCleanup(ctx context.Context) {
	// Reattach to surviving agent subprocesses FIRST so the sweeps below —
	// which all key off HasRunningAgentForTask — see them as live and do
	// not remove their worktrees, gc their chat tasks, mark their runs
	// stale, or restart them.
	// ReattachAll derives its per-agent contexts from agent.Manager's own
	// m.ctx field (bound once at Manager construction from this same App
	// root context), not from a threaded parameter — see the Engine.SetContext
	// / e.ctx pattern for why re-plumbing that across ReattachAll's whole
	// reattach fan-out is out of scope here.
	if reattached := r.Agents.ReattachAll(); len(reattached) > 0 { //nolint:contextcheck // agent.Manager uses its own m.ctx field, see comment above
		r.Logger.Info("recovery.reattach", "count", len(reattached))
	}
	r.Worktrees.RepairAll(ctx)
	r.gcOrphanChats(ctx)
	r.pruneTrash()
	r.Worktrees.CleanupOrphaned(ctx)
	r.cleanStaleRuns()
	r.pruneAgentLogs()
	r.RestartStaleInProgress(ctx)
}

// pruneAgentLogs removes stale per-agent NDJSON files. Safe to call with
// an empty LogDir (test setups) — the logging helper no-ops.
func (r *Recovery) pruneAgentLogs() {
	rep := logging.PruneAgentLogs(r.LogDir, r.LogRetention, time.Now())
	logging.LogPruneReport(r.Logger, rep)
}

// pruneTrash permanently removes trash generations older than
// TrashRetentionDays, run right after gcOrphanChats (whose Delete calls just
// trashed any orphaned chat tasks) and before Worktrees.CleanupOrphaned.
// Logs the resulting count and every generation removed.
func (r *Recovery) pruneTrash() {
	rep, err := r.Tasks.PruneTrash(r.TrashRetentionDays)
	if err != nil {
		r.Logger.Warn("recovery.trash.prune_failed", "err", err)
		return
	}
	for _, entry := range rep.Entries {
		r.Logger.Info("recovery.trash.pruned", "id", entry.ID, "generation", entry.Generation, "deleted_date", entry.DeletedDate, "title", entry.Title)
	}
	for _, err := range rep.Errors {
		r.Logger.Warn("recovery.trash.prune_error", "err", err)
	}
	r.Logger.Info("recovery.trash.prune", "scanned", rep.Scanned, "removed", rep.Removed, "errors", len(rep.Errors))
}
