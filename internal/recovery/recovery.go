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
	// DispatchEvent matches the task's CURRENT status against builtin trigger
	// conditions and starts whichever workflow matches — see
	// handleTerminalWorkflow for why this must be preferred over blindly
	// restarting a stale WorkflowID.
	DispatchEvent(taskID, event string, extraFields, vars map[string]string) (string, error)
	HandleAgentComplete(taskID string, completion workflow.AgentCompletion)
}

// PRResolver resolves the GitHub PR a task lost track of when its pr_number was
// cleared, so recovery can reconcile it. Implemented by the App over the repo's
// github abstraction; kept a narrow interface so recovery stays a leaf package.
type PRResolver interface {
	ResolvePRForTask(ctx context.Context, repo, branch, issue string) (PRRef, error)
}

// PRRef is the PR a PRResolver matched to a task. State is "OPEN" or "MERGED";
// a zero Number with an empty State means no unambiguous PR was found.
type PRRef struct {
	Number int
	State  string
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
	PRs            PRResolver
	Logger         *slog.Logger
	Throttle       *logging.ErrorThrottle
	WG             *sync.WaitGroup

	LogDir       string
	LogRetention time.Duration // 0 disables age-based pruning
	OrphanRoots  []string

	// TrashRetentionDays bounds how long a soft-deleted task (see
	// task.Store.Delete) survives under the trash dir before
	// pruneTrash permanently removes it. A negative value disables
	// pruning.
	TrashRetentionDays int

	// CommitBeforePrune, when set, is invoked at the top of pruneTrash so a
	// git snapshot of the tasks dir (see internal/tasksnapshot) is taken
	// immediately before the bulk-delete sweep — covering both the
	// boot-time RunStartupCleanup pass and the periodic PruneTrash loop.
	// nil-safe: recovery deliberately does not import internal/tasksnapshot
	// so it stays a leaf package; this func field is the only coupling.
	CommitBeforePrune func(context.Context)
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
	if reattached := r.Agents.ReattachAllContext(ctx); len(reattached) > 0 {
		r.Logger.Info("recovery.reattach", "count", len(reattached))
	}
	if reaped := r.Agents.ReapOrphanProviderProcesses(ctx, r.OrphanRoots); reaped > 0 {
		r.Logger.Info("recovery.orphan_reap", "count", reaped)
	}
	r.Worktrees.RepairAll(ctx)
	r.gcOrphanChats(ctx)
	r.pruneTrash(ctx)
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

// PruneTrash is the periodic entry point for a background ticker (see
// LifecycleManager.startTrashPruneLoop) that keeps the trash dir bounded
// between restarts on long-lived server deployments — RunStartupCleanup's
// call only covers the boot-time pass.
func (r *Recovery) PruneTrash(ctx context.Context) {
	r.pruneTrash(ctx)
}

func (r *Recovery) effectiveTrashRetentionDays() int {
	if r.TrashRetentionDays == 0 {
		return 14
	}
	return r.TrashRetentionDays
}

// pruneTrash permanently removes trash generations older than
// TrashRetentionDays, run right after gcOrphanChats (whose Delete calls just
// trashed any orphaned chat tasks) and before Worktrees.CleanupOrphaned.
// Logs the resulting count and every generation removed. Fires
// CommitBeforePrune first (nil-safe) so a git snapshot exists immediately
// before this bulk-delete sweep.
func (r *Recovery) pruneTrash(ctx context.Context) {
	if r.CommitBeforePrune != nil {
		r.CommitBeforePrune(ctx)
	}
	rep, err := r.Tasks.PruneTrash(r.effectiveTrashRetentionDays())
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
