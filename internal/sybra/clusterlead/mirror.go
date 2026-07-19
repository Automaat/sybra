package clusterlead

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/attachment"
	"github.com/Automaat/sybra/internal/cluster"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/monitor"
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

	attachments *attachment.Store
	anomalySink monitor.IssueSink
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

// SetAttachments supplies the leader-local blob store used to mirror follower
// attachment uploads back into the canonical task view.
func (m *Mirror) SetAttachments(store *attachment.Store) {
	m.attachments = store
}

// SetAnomalySink supplies the sink drift detection reports through — the same
// routing sink monitor.Service uses, so a cluster-drift finding gets the
// existing dedup, work-project scrubbing, and durable GitHub-issue-outbox
// behavior for free. A nil sink (monitor disabled, or not yet wired at
// construction time) disables alerting only; detection and repair still run.
func (m *Mirror) SetAnomalySink(sink monitor.IssueSink) {
	m.anomalySink = sink
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
		m.applyFollowerTaskWithContext(ctx, node, tasks[i])
	}
	m.reconcileMissing(ctx, node, client, tasks)
}

// reconcileMissing closes the gap ListTasksForNode's staleness filter opens:
// a task the leader still considers live and assigned to node, but that this
// node's response omitted. That only happens when the follower closed it
// more than mirrorStaleTerminalWindow ago while the leader could not reach
// this node at all (an outage or restart spanning the entire window) -- the
// closing update never got a chance to apply, and the follower will never
// offer that task again, so without this the canonical copy would stay stuck
// at its last non-terminal state permanently. Fetches only the handful of
// affected tasks directly (GetTask) instead of falling back to a full list.
func (m *Mirror) reconcileMissing(ctx context.Context, node string, client *cluster.Client, tasks []task.Task) {
	seen := make(map[string]struct{}, len(tasks))
	for i := range tasks {
		seen[tasks[i].ID] = struct{}{}
	}
	canonical, err := m.tasks.List()
	if err != nil {
		m.logger.Warn("cluster.mirror.reconcile_missing.list_failed", "node", node, "err", err)
		return
	}
	for i := range canonical {
		t := canonical[i]
		if t.AssignedNode != node || task.IsTerminalStatus(t.Status) {
			continue
		}
		if _, ok := seen[t.ID]; ok {
			continue
		}
		follower, gerr := client.GetTask(ctx, t.ID)
		if gerr != nil {
			m.logger.Debug("cluster.mirror.reconcile_missing.failed", "node", node, "task", t.ID, "err", gerr)
			continue
		}
		m.applyFollowerTaskWithContext(ctx, node, follower)
	}
}

func (m *Mirror) applyFollowerTask(node string, follower task.Task) bool {
	return m.applyFollowerTaskWithContext(context.Background(), node, follower)
}

func (m *Mirror) applyFollowerTaskWithContext(ctx context.Context, node string, follower task.Task) bool {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	canonical, err := m.tasks.Get(follower.ID)
	if err != nil {
		return false
	}
	if canonical.AssignedNode != node {
		return false
	}
	m.detectAndRepairDrift(ctx, node, canonical, follower)
	follower, ok := m.localizeFollowerAttachments(ctx, node, canonical, follower)
	if !ok {
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

// detectAndRepairDrift compares the two leader-authoritative fields Merge
// never touches — Tags and DependsOn — between the leader's canonical copy
// and what the follower actually reports. Every other field either flows
// follower-authoritative through Merge every reconcile tick (Status,
// Workflow, AgentRuns, ...) or is immutable identity (ID, ProjectID,
// CreatedAt), so an ordinary disagreement there is expected and self-heals
// on its own — alerting on it would just be noise. Tags/DependsOn only ever
// change on the leader (umbrella-gate release, a manual sybra-cli edit, ...);
// if a leader-side write to one of them never reached the follower — the
// #2349 class of bug, from any cause, not just the one fixed there — nothing
// else in the reconcile loop will ever notice, because Merge doesn't carry
// them and Assigner only pushes a task to its follower once, at first
// assignment. This is the backstop: detected within one reconcile interval,
// logged, alerted through the same anomaly sink as everything else, and
// repaired by pushing just the drifted fields onto the follower's own
// current state (never a stale full-task overwrite that could roll back
// follower-side execution progress).
func (m *Mirror) detectAndRepairDrift(ctx context.Context, node string, canonical, follower task.Task) {
	tagsDrift := !slices.Equal(sortedCopy(canonical.Tags), sortedCopy(follower.Tags))
	depsDrift := !slices.Equal(sortedCopy(canonical.DependsOn), sortedCopy(follower.DependsOn))
	if !tagsDrift && !depsDrift {
		return
	}
	m.logger.Error("cluster.mirror.drift_detected",
		"task", canonical.ID, "node", node,
		"tags_drift", tagsDrift, "deps_drift", depsDrift,
		"canonical_tags", canonical.Tags, "follower_tags", follower.Tags,
		"canonical_depends_on", canonical.DependsOn, "follower_depends_on", follower.DependsOn)
	m.alertDrift(ctx, node, canonical, tagsDrift, depsDrift)
	m.repairDrift(ctx, node, canonical)
}

func (m *Mirror) alertDrift(ctx context.Context, node string, canonical task.Task, tagsDrift, depsDrift bool) {
	if m.anomalySink == nil {
		return
	}
	ev := map[string]any{
		"node":       node,
		"tags_drift": tagsDrift,
		"deps_drift": depsDrift,
		"tags":       canonical.Tags,
		"depends_on": canonical.DependsOn,
	}
	a := monitor.Anomaly{
		Kind:        monitor.KindClusterDrift,
		TaskID:      canonical.ID,
		Severity:    monitor.SeverityError,
		RequiresLLM: false,
		Fingerprint: monitor.Fingerprint(monitor.KindClusterDrift, canonical.ID, ev),
		Evidence:    ev,
		DetectedAt:  time.Now().UTC(),
	}
	if _, err := m.anomalySink.Submit(ctx, a, monitor.DeterministicIssueBody(a)); err != nil {
		m.logger.Warn("cluster.mirror.drift_alert.failed", "task", canonical.ID, "node", node, "err", err)
	}
}

// driftRepairTimeout bounds repairDrift's two follower round trips (a
// re-fetch plus the repair push) so one unreachable follower can't hold
// applyMu — shared across every node's reconcile goroutine — for the full
// 30s cluster client default per drifted task.
const driftRepairTimeout = 5 * time.Second

// repairDrift pushes only the drifted fields onto the follower's own current
// task state (not the leader's canonical copy verbatim) — the follower's
// Status/Workflow/AgentRuns/etc. may be more current than what the leader
// last pulled, and overwriting those would roll back real execution
// progress, exactly the split-brain risk PushUpdate exists to avoid. It
// re-fetches via GetTask rather than reusing this tick's ListTasks snapshot,
// which can already be stale by the time this repair runs — earlier tasks in
// the same batch, under the same applyMu lock, each take up to
// driftRepairTimeout first.
func (m *Mirror) repairDrift(ctx context.Context, node string, canonical task.Task) {
	client, ok := m.roster.Client(node)
	if !ok || client == nil {
		m.logger.Warn("cluster.mirror.drift_repair.no_client", "task", canonical.ID, "node", node)
		return
	}
	repairCtx, cancel := context.WithTimeout(ctx, driftRepairTimeout)
	defer cancel()
	live, err := client.GetTask(repairCtx, canonical.ID)
	if err != nil {
		m.logger.Warn("cluster.mirror.drift_repair.refetch_failed", "task", canonical.ID, "node", node, "err", err)
		return
	}
	repaired := live
	repaired.Tags = canonical.Tags
	repaired.DependsOn = canonical.DependsOn
	repaired.AssignedNode = node
	if err := client.AssignTask(repairCtx, repaired); err != nil {
		m.logger.Warn("cluster.mirror.drift_repair.failed", "task", canonical.ID, "node", node, "err", err)
		return
	}
	m.logger.Info("cluster.mirror.drift_repaired", "task", canonical.ID, "node", node)
}

// sortedCopy returns a sorted copy of ss so set-like fields (Tags,
// DependsOn) compare equal regardless of element order.
func sortedCopy(ss []string) []string {
	out := slices.Clone(ss)
	slices.Sort(out)
	return out
}

func (m *Mirror) localizeFollowerAttachments(ctx context.Context, node string, canonical, follower task.Task) (task.Task, bool) {
	if len(follower.Attachments) == 0 {
		return follower, true
	}
	if m.attachments == nil || m.roster == nil {
		m.logger.Warn("cluster.mirror.attachments.unavailable", "node", node, "task", follower.ID)
		return follower, false
	}
	client, ok := m.roster.Client(node)
	if !ok || client == nil {
		m.logger.Warn("cluster.mirror.attachments.no_client", "node", node, "task", follower.ID)
		return follower, false
	}

	canonicalByID := make(map[string]task.Attachment, len(canonical.Attachments))
	for i := range canonical.Attachments {
		canonicalByID[canonical.Attachments[i].ID] = canonical.Attachments[i]
	}
	local := make([]task.Attachment, 0, len(follower.Attachments))
	for i := range follower.Attachments {
		att := follower.Attachments[i]
		if existing, ok := canonicalByID[att.ID]; ok && attachmentEquivalent(existing, att) {
			if _, err := m.attachments.Path(canonical.ID, existing.ID); err == nil {
				local = append(local, existing)
				continue
			}
		}
		data, err := client.ExportAttachment(ctx, follower.ID, att.ID)
		if err != nil {
			m.logger.Warn("cluster.mirror.attachment.export.failed", "node", node, "task", follower.ID, "attachment_id", att.ID, "err", err)
			return follower, false
		}
		imported, err := m.attachments.Import(follower.ID, att, data)
		if err != nil {
			m.logger.Warn("cluster.mirror.attachment.import.failed", "node", node, "task", follower.ID, "attachment_id", att.ID, "err", err)
			return follower, false
		}
		local = append(local, imported)
	}
	follower.Attachments = local
	return follower, true
}

func attachmentEquivalent(a, b task.Attachment) bool {
	return a.ID == b.ID &&
		a.FileName == b.FileName &&
		a.ContentType == b.ContentType &&
		a.SizeBytes == b.SizeBytes &&
		a.CreatedAt.Equal(b.CreatedAt)
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
	out.Attachments = follower.Attachments

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
