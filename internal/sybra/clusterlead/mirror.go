package clusterlead

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/cluster"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
)

// DefaultReconcileInterval is how often the mirror re-lists each follower's
// tasks — the sole sync mechanism as of #2188. Every follower update, not
// only the filesystem-direct-write case that motivated the change, now lags
// the leader's canonical store by up to this interval; previously most
// updates arrived near-instantly over an SSE stream. Traded deliberately for
// reliability: SSE is leader-initiated (not a reverse-connectivity problem)
// but was confirmed live to never fire for a write applied outside the
// follower's own API process — e.g. sybra-cli run over plain SSH with no
// SYBRA_PORT/SYBRA_AUTH_TOKEN in its environment, which is how every
// operator fix applied directly on a follower host reaches it — so it left
// exactly that class of update permanently stale with no self-healing path.
// Whether the follower's own filesystem watcher could be made to re-emit
// task:updated for such writes is unexplored; until it is, one dependable
// polling path beats one broken push path plus one that only partly works.
const DefaultReconcileInterval = 30 * time.Second

// reconcileFailureEscalateThreshold is how many consecutive reconcile
// failures for one node before the log level escalates from Warn to Error —
// a transient blip logs a warning; a stuck node (auth drift, a payload that
// still exceeds the response cap, network loss) becomes impossible to miss.
const reconcileFailureEscalateThreshold = 5

// Mirror keeps the leader's canonical store in sync with follower execution
// state via a reconcile ticker, applying every update through one authority
// merge: execution fields are follower-authoritative, identity/assignment
// fields stay leader-authoritative, and a per-task monotonic clock drops stale
// or out-of-order updates.
type Mirror struct {
	cfg      *config.Config
	tasks    *task.Manager
	roster   *cluster.Roster
	logger   *slog.Logger
	interval time.Duration
	applyMu  sync.Mutex
}

// NewMirror constructs a Mirror. A nil logger falls back to slog.Default(); a
// non-positive interval falls back to DefaultReconcileInterval.
func NewMirror(cfg *config.Config, tasks *task.Manager, roster *cluster.Roster, logger *slog.Logger, interval time.Duration) *Mirror {
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = DefaultReconcileInterval
	}
	return &Mirror{cfg: cfg, tasks: tasks, roster: roster, logger: logger, interval: interval}
}

// Run starts, per follower, a reconcile ticker, and blocks until ctx is
// cancelled. Inert on a non-leader node or with no roster.
func (m *Mirror) Run(ctx context.Context) {
	if m.cfg == nil || !m.cfg.IsLeader() || m.roster == nil {
		return
	}
	var wg sync.WaitGroup
	for _, name := range m.roster.Names() {
		wg.Add(1)
		go func(name string) { defer wg.Done(); m.reconcileLoop(ctx, name) }(name)
	}
	wg.Wait()
}

func (m *Mirror) reconcileLoop(ctx context.Context, node string) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	var consecutiveFailures int
	m.reconcileNode(ctx, node, &consecutiveFailures)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reconcileNode(ctx, node, &consecutiveFailures)
		}
	}
}

func (m *Mirror) reconcileNode(ctx context.Context, node string, consecutiveFailures *int) {
	client, ok := m.roster.Client(node)
	if !ok || client == nil {
		return
	}
	tasks, err := client.ListTasks(ctx)
	if err != nil {
		*consecutiveFailures++
		level := slog.LevelWarn
		if *consecutiveFailures >= reconcileFailureEscalateThreshold {
			level = slog.LevelError
		}
		m.logger.Log(ctx, level, "cluster.mirror.reconcile.failed",
			"node", node, "err", err, "consecutive_failures", *consecutiveFailures)
		return
	}
	*consecutiveFailures = 0
	for i := range tasks {
		m.applyFollowerTask(node, tasks[i])
	}
}

func (m *Mirror) applyFollowerTask(node string, follower task.Task) bool {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	canonical, err := m.tasks.Get(follower.ID)
	if err != nil {
		return false
	}
	if canonical.AssignedNode != node {
		return false
	}
	merged, ok := Merge(canonical, follower)
	if !ok {
		return false
	}
	if err := m.writeSidecars(merged); err != nil {
		m.logger.Warn("cluster.mirror.sidecar.failed", "node", node, "task", follower.ID, "err", err)
		return false
	}
	if _, _, err := m.tasks.Put(merged); err != nil {
		m.logger.Warn("cluster.mirror.apply.failed", "node", node, "task", follower.ID, "err", err)
		return false
	}
	m.logger.Debug("cluster.mirror.applied", "node", node, "task", follower.ID, "status", string(merged.Status), "rev", merged.MirrorRev)
	return true
}

func (m *Mirror) writeSidecars(t task.Task) error {
	if m.tasks == nil {
		return nil
	}
	store := m.tasks.Store()
	if err := store.Plans().Write(t.ID, t.Plan); err != nil {
		return err
	}
	if err := store.PlanContracts().Write(t.ID, t.PlanContract); err != nil {
		return err
	}
	if err := store.PlanCritiques().Write(t.ID, t.PlanCritique); err != nil {
		return err
	}
	if err := store.PlanResearch().Write(t.ID, t.PlanResearch); err != nil {
		return err
	}
	if err := store.PlanDecisions().Write(t.ID, t.PlanDecisions); err != nil {
		return err
	}
	if err := store.PlanBrief().Write(t.ID, t.PlanBrief); err != nil {
		return err
	}
	if err := store.CodeReviews().Write(t.ID, t.CodeReview); err != nil {
		return err
	}
	return nil
}

// Merge produces the canonical task's next state from a follower's reported
// state. Execution fields (status, agent runs, worktree/branch/PR, workflow,
// review phases, sidecars) are follower-authoritative; identity and assignment
// fields (id, project, assigned_node, ingested title, issue refs, created_at)
// stay leader-authoritative. It returns ok=false when the follower state is not
// newer than the last applied (stale or out-of-order), so the caller drops it.
// MirrorRev increments per applied update and MirrorUpdatedAt tracks the
// follower clock of the applied state.
func Merge(canonical, follower task.Task) (task.Task, bool) {
	if canonical.MirrorUpdatedAt != nil && !follower.UpdatedAt.After(*canonical.MirrorUpdatedAt) {
		return canonical, false
	}
	out := canonical

	out.Status = follower.Status
	out.StatusReason = follower.StatusReason
	out.StatusChangedAt = follower.StatusChangedAt
	out.AgentRuns = follower.AgentRuns
	out.Workflow = follower.Workflow
	out.WorktreeDir = follower.WorktreeDir
	out.Branch = follower.Branch
	out.PRNumber = follower.PRNumber
	out.PRPhase = follower.PRPhase
	out.ReviewPhase = follower.ReviewPhase
	out.ReviewedHeadSHA = follower.ReviewedHeadSHA
	out.ReviewedHeadAttempts = follower.ReviewedHeadAttempts
	out.Reviewed = follower.Reviewed
	out.RunRole = follower.RunRole
	out.Outcome = follower.Outcome
	out.MergeCommit = follower.MergeCommit
	out.Body = follower.Body
	out.Plan = follower.Plan
	out.PlanContract = follower.PlanContract
	out.PlanCritique = follower.PlanCritique
	out.PlanResearch = follower.PlanResearch
	out.PlanDecisions = follower.PlanDecisions
	out.PlanBrief = follower.PlanBrief
	out.CodeReview = follower.CodeReview

	out.MirrorRev = canonical.MirrorRev + 1
	followerUpdated := follower.UpdatedAt
	out.MirrorUpdatedAt = &followerUpdated
	// UpdatedAt is the leader-local write clock every other Store writer
	// (UpdateWithPrev) stamps with time.Now() — borrowing the follower's
	// clock here instead let an unrelated leader-side edit's fresher
	// UpdatedAt outrun a genuinely newer, already-validated (via
	// MirrorUpdatedAt above) follower update, tripping Put's #2203 stale
	// guard on legitimate mirror traffic.
	out.UpdatedAt = time.Now().UTC()
	return out, true
}

func nodesFromConfig(cfg *config.Config) []cluster.Node {
	if cfg == nil {
		return nil
	}
	nodes := make([]cluster.Node, 0, len(cfg.Cluster.Followers))
	for i := range cfg.Cluster.Followers {
		f := cfg.Cluster.Followers[i]
		nodes = append(nodes, cluster.Node{
			Name:      f.Name,
			Endpoints: f.Endpoints,
			Token:     f.ResolveToken(),
			TLSPin:    f.TLSPin,
		})
	}
	return nodes
}

// NewRoster builds a cluster.Roster from the leader's configured followers. The
// returned roster is empty (Names() is nil) when there are no followers; callers
// check that rather than a nil roster.
func NewRoster(cfg *config.Config, logger *slog.Logger) (*cluster.Roster, error) {
	return cluster.NewRoster(nodesFromConfig(cfg), logger)
}
