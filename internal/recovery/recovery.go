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
	"github.com/Automaat/sybra/internal/cleanup"
	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/reconcile"
	"github.com/Automaat/sybra/internal/sandbox"
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
	ReplayPersistedEffects()
	ReplayPersistedEffectsForTask(taskID string) bool
	// ReclaimOrphanedEffectLeases must run before the two replay paths above:
	// both claim effects, and a lease held by the previous engine instance
	// fences them for the rest of its TTL.
	ReclaimOrphanedEffectLeases() int
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
	Tasks             *task.Manager
	Agents            *agent.Manager
	Worktrees         *worktree.Manager
	Sandboxes         *sandbox.Manager
	WorkflowEngine    WorkflowRestarter // optional; nil-safe
	Orchestrator      Orchestrator
	Projects          ProjectGetter
	PRs               PRResolver
	Reconciler        reconcile.Runner
	ConflictRecovery  func(taskID string) bool
	Logger            *slog.Logger
	Throttle          *logging.ErrorThrottle
	WG                *sync.WaitGroup
	ProtectedFindings *cleanup.ProtectedStore

	LogDir       string
	LogRetention time.Duration // 0 disables age-based pruning
	// LogGzipAfter compresses retained per-agent NDJSON logs older than
	// this. 0 disables compression.
	LogGzipAfter time.Duration
	// LogMaxTotalBytes caps the total size of the per-agent log directory.
	// 0 disables size-based enforcement.
	LogMaxTotalBytes int64
	OrphanRoots      []string
	// OwnedOrphanRoots may be shared with operator-run provider processes.
	// Recovery only reaps processes carrying Sybra's explicit owner marker.
	OwnedOrphanRoots []string

	DispatchGate func(task.Task) bool

	// recoveryMu guards recoveryClaims.
	recoveryMu sync.Mutex
	// recoveryClaims tracks task IDs with an in-flight recovery decision. See
	// TryClaimRecovery.
	recoveryClaims map[string]struct{}

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

// RunStartupCleanup reconciles terminal/stale runs before any destructive
// worktree sweep. A completed-but-unpushed commit is still task work even when
// the task file already looks terminal; cleanup may only see it after the
// reconciler has preserved/adopted it or proved there is none.
func (r *Recovery) RunStartupCleanup(ctx context.Context) {
	// Reattach to surviving agent subprocesses FIRST so the sweeps below —
	// which all key off HasRunningAgentForTask — see them as live and do
	// not remove their worktrees, mark their runs stale, or restart them.
	if reattached := r.Agents.ReattachAllContext(ctx); len(reattached) > 0 {
		r.Logger.Info("recovery.reattach", "count", len(reattached))
	}
	reaped, dedicatedConfirmed := r.Agents.ReapOrphanProviderProcessesConfirmed(ctx, r.OrphanRoots)
	ownedReaped, ownedConfirmed := r.Agents.ReapOwnedOrphanProviderProcessesConfirmed(ctx, r.OwnedOrphanRoots)
	if reaped += ownedReaped; reaped > 0 {
		r.Logger.Info("recovery.orphan_reap", "count", reaped)
	}
	r.Worktrees.RepairAll(ctx)
	// Only after unregistered owned processes are gone and their worktrees have
	// been repaired is an expired ledger-only attempt safe to finalize.
	if dedicatedConfirmed && ownedConfirmed {
		r.Agents.ReconcileAttemptLeases(ctx)
	} else {
		r.Logger.Error("recovery.attempt_reconcile.deferred", "reason", "orphan termination unconfirmed")
	}
	r.cleanStaleRuns()
	if r.WorkflowEngine != nil {
		// Ordered ahead of the replay: reattach above has established which
		// steps are genuinely still running, and both replay paths below claim
		// effects that a dead instance's lease would otherwise fence.
		r.WorkflowEngine.ReclaimOrphanedEffectLeases()
		r.WorkflowEngine.ReplayPersistedEffects()
	}
	r.RestartStaleInProgress(ctx)
	r.pruneTrash(ctx)
	r.Worktrees.CleanupOrphaned(ctx)
	r.cleanupOrphanedSandboxes(ctx)
	r.pruneAgentLogs()
}

// pruneAgentLogs enforces retention (age/empty deletion, gzip compression,
// size cap) over per-agent NDJSON files. Safe to call with an empty LogDir
// (test setups) — the logging helper no-ops. Agent liveness is queried
// first so a live agent's own log — still being appended to — is excluded
// from every pass of the sweep.
func (r *Recovery) pruneAgentLogs() {
	var active map[string]bool
	if r.Agents != nil {
		active = r.Agents.ActiveLogPaths()
	}
	var protectedLogs map[string]bool
	if r.ProtectedFindings != nil && r.Tasks != nil {
		if findings, err := r.ProtectedFindings.List(); err == nil {
			if tasks, listErr := r.Tasks.List(); listErr == nil {
				protectedLogs = cleanup.ProtectedEvidenceLogPaths(r.LogDir, tasks, findings)
			}
		}
	}
	rep := logging.EnforceAgentLogRetention(r.LogDir, logging.RetentionOptions{
		MaxAge:            r.LogRetention,
		GzipAfter:         r.LogGzipAfter,
		MaxTotalBytes:     r.LogMaxTotalBytes,
		ActiveLogPaths:    active,
		ProtectedLogPaths: protectedLogs,
	}, time.Now())
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
// TrashRetentionDays, run right after the startup worktree repair sweep and
// before Worktrees.CleanupOrphaned. Logs the resulting count and every
// generation removed. Fires
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

func (r *Recovery) cleanupOrphanedSandboxes(ctx context.Context) {
	if r.Sandboxes == nil || r.Tasks == nil {
		return
	}
	tasks, err := r.Tasks.ListBoard()
	if err != nil {
		r.Logger.Warn("recovery.sandboxes.list", "err", err)
		return
	}
	var hasAgent func(string) bool
	if r.Agents != nil {
		hasAgent = r.Agents.HasRunningAgentForTask
	}
	var hasUnpushedCommits func(string) bool
	if r.Worktrees != nil {
		hasUnpushedCommits = func(taskID string) bool {
			return r.Worktrees.HasUnpushedCommits(ctx, taskID)
		}
	}
	r.Sandboxes.CleanupOrphaned(ctx, tasks, hasAgent, hasUnpushedCommits)
}
