package clusterlead

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/cluster"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
)

// DefaultReconcileInterval is how often the mirror re-lists each follower's
// tasks, covering silent SSE drops and a leader restart.
const DefaultReconcileInterval = 30 * time.Second

// Mirror keeps the leader's canonical store in sync with follower execution
// state. Per follower it consumes the SSE event stream (for immediacy) and runs
// a reconcile ticker (for durability), applying both through one authority
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

// Run starts, per follower, an SSE consumer and a reconcile ticker, and blocks
// until ctx is cancelled. Inert on a non-leader node or with no roster.
func (m *Mirror) Run(ctx context.Context) {
	if m.cfg == nil || !m.cfg.IsLeader() || m.roster == nil {
		return
	}
	var wg sync.WaitGroup
	for _, name := range m.roster.Names() {
		wg.Add(2)
		go func(name string) { defer wg.Done(); m.streamLoop(ctx, name) }(name)
		go func(name string) { defer wg.Done(); m.reconcileLoop(ctx, name) }(name)
	}
	wg.Wait()
}

func (m *Mirror) reconcileLoop(ctx context.Context, node string) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	m.reconcileNode(ctx, node)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reconcileNode(ctx, node)
		}
	}
}

func (m *Mirror) reconcileNode(ctx context.Context, node string) {
	client, ok := m.roster.Client(node)
	if !ok || client == nil {
		return
	}
	tasks, err := client.ListTasks(ctx)
	if err != nil {
		m.logger.Debug("cluster.mirror.reconcile.failed", "node", node, "err", err)
		return
	}
	for i := range tasks {
		m.applyFollowerTask(node, tasks[i])
	}
}

func (m *Mirror) streamLoop(ctx context.Context, node string) {
	for {
		if ctx.Err() != nil {
			return
		}
		client, ok := m.roster.Client(node)
		if !ok || client == nil {
			return
		}
		ch, err := client.Subscribe(ctx)
		if err != nil {
			m.logger.Debug("cluster.mirror.subscribe.failed", "node", node, "err", err)
			if !sleepCtx(ctx, m.interval) {
				return
			}
			continue
		}
		m.consume(ctx, node, client, ch)
	}
}

func (m *Mirror) consume(ctx context.Context, node string, client *cluster.Client, ch <-chan cluster.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			id := taskIDFromEvent(ev)
			if id == "" {
				continue
			}
			follower, err := client.GetTask(ctx, id)
			if err != nil {
				m.logger.Debug("cluster.mirror.fetch.failed", "node", node, "task", id, "err", err)
				continue
			}
			m.applyFollowerTask(node, follower)
		}
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
	out.Reviewed = follower.Reviewed
	out.RunRole = follower.RunRole
	out.Outcome = follower.Outcome
	out.MergeCommit = follower.MergeCommit
	out.Body = follower.Body
	out.Plan = follower.Plan
	out.PlanContract = follower.PlanContract
	out.CodeReview = follower.CodeReview

	out.MirrorRev = canonical.MirrorRev + 1
	followerUpdated := follower.UpdatedAt
	out.MirrorUpdatedAt = &followerUpdated
	out.UpdatedAt = follower.UpdatedAt
	return out, true
}

func taskIDFromEvent(ev cluster.Event) string {
	if !strings.HasPrefix(ev.Name, "task:") {
		return ""
	}
	base := filepath.Base(strings.TrimSpace(ev.Data))
	base = strings.Trim(base, "\"")
	id := strings.TrimSuffix(base, ".md")
	if id == "" || id == "." || strings.ContainsAny(id, `/\`) {
		return ""
	}
	return id
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
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
