// Package recovery handles boot-time and periodic recovery of tasks and
// agents whose state survived a crash or restart. The startup pass runs
// once from App.Startup; RestartStaleInProgress is also called from the
// orchestrator loop every minute to catch agents that died after boot.
package recovery

import (
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

// Orchestrator is the subset of *sybra.AgentOrchestrator that recovery
// uses. Defined here so the package does not import internal/sybra (which
// would form a cycle: sybra → recovery → sybra).
type Orchestrator interface {
	StartAgent(taskID, mode, prompt string, oneShot bool) (*agent.Agent, error)
	StartPRFixAgent(taskID string) error
}

// ProjectGetter is the subset of *project.Store needed to decide whether
// a respawned agent should create a draft PR (work) or a regular PR (pet).
type ProjectGetter interface {
	Get(id string) (project.Project, error)
}

// Recovery owns the dependencies needed by the boot-time cleanup pass and
// the periodic restart-stale sweep. Construct once during App.Startup,
// reuse from the orchestrator loop.
type Recovery struct {
	Tasks          *task.Manager
	Agents         *agent.Manager
	Worktrees      *worktree.Manager
	WorkflowEngine *workflow.Engine // optional; nil-safe
	Orchestrator   Orchestrator
	Projects       ProjectGetter
	Logger         *slog.Logger
	Throttle       *logging.ErrorThrottle
	WG             *sync.WaitGroup

	LogDir       string
	LogRetention time.Duration // 0 disables age-based pruning
}

// RunStartupCleanup sequences boot-time maintenance in the order that lets
// each step see the output of the previous one: chats first so their
// worktrees show up as orphans to the subsequent sweep; stale run state
// next so restart-stale sees a clean slate.
func (r *Recovery) RunStartupCleanup() {
	r.Worktrees.RepairAll()
	r.gcOrphanChats()
	r.Worktrees.CleanupOrphaned()
	r.cleanStaleRuns()
	r.pruneAgentLogs()
	r.RestartStaleInProgress()
}

// pruneAgentLogs removes stale per-agent NDJSON files. Safe to call with
// an empty LogDir (test setups) — the logging helper no-ops.
func (r *Recovery) pruneAgentLogs() {
	rep := logging.PruneAgentLogs(r.LogDir, r.LogRetention, time.Now())
	logging.LogPruneReport(r.Logger, rep)
}
