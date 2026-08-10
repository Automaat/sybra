package clusterlead

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/attachment"
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

// missingConfirmThreshold is how many consecutive reconcile ticks a task must
// 404 on its assigned node before reconcileMissing trusts that as "the
// follower confirms this task is gone" and trashes the leader's canonical
// copy. Assigner.Reassign stamps canonical.AssignedNode to the new node
// *before* pushing the task there (reassign.go's stampNode doc comment: this
// ordering is deliberate, so a revived dead follower cannot clobber a task
// that has moved) — so a single 404 on a freshly assigned node is exactly as
// consistent with "not pushed yet" as with "genuinely deleted," and a
// follower client call can legitimately take up to defaultCallTimeout (30s)
// before even failing. Requiring the same node to 404 across
// missingConfirmThreshold ticks (each spaced by the reconcile interval)
// gives a slow AssignTask/attachment-transfer push comfortably longer than
// one RPC timeout to land before the leader trusts the absence and trashes
// its own copy — while a genuinely deleted task, which will 404 forever,
// still gets cleaned up in bounded time instead of staying a ghost.
const missingConfirmThreshold = 3

// errFollowerUpdateStale stops an apply after its preliminary work when a
// newer follower update already reached the canonical task before the final
// lock-protected merge.
var errFollowerUpdateStale = errors.New("cluster follower update is stale")

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

	// missingMu guards missingStreak, written from each node's independent
	// reconcileLoop goroutine.
	missingMu     sync.Mutex
	missingStreak map[string]int // "node|taskID" -> consecutive confirmed-404 ticks
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
	return &Mirror{
		cfg: cfg, tasks: tasks, roster: roster, logger: logger, interval: interval,
		missingStreak: make(map[string]int),
	}
}

// SetAttachments supplies the leader-local blob store used to mirror follower
// attachment uploads back into the canonical task view.
func (m *Mirror) SetAttachments(store *attachment.Store) {
	m.attachments = store
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

// reconcileMissing closes the gap ListTasksForNode's staleness filter opens: a
// non-terminal task the leader still considers live and assigned to node, but
// that this node's response omitted. Fetches only the handful of affected
// tasks directly (GetTask) instead of falling back to a full list, and
// branches on the result:
//   - The follower 404s (os.ErrNotExist surfaced as http.StatusNotFound — see
//     httpapi.stripErrorResult) on missingConfirmThreshold consecutive ticks:
//     the follower closed the task more than mirrorStaleTerminalWindow ago
//     while unreachable (an outage or restart spanning the whole window, so
//     the closing update never applied), or it was trashed outright (e.g. a
//     duplicate cleanup, #2294's post-mortem). Either way the follower will
//     never offer it again, so the leader trashes its own stale copy instead
//     of leaving it stuck at its last non-terminal state permanently — which
//     would otherwise keep poisoning downstream rollup logic that scans all
//     children, like trackerRollup's cancelled-child check, forever. A
//     single 404 is not enough on its own — see missingConfirmThreshold's
//     doc comment for why a freshly reassigned task looks identical for one
//     tick.
//   - Any other error (network failure, follower down): leave the canonical
//     copy untouched and retry next tick — the follower may still have it.
func (m *Mirror) reconcileMissing(ctx context.Context, node string, client *cluster.Client, tasks []task.Task) {
	seen := make(map[string]struct{}, len(tasks))
	for i := range tasks {
		seen[tasks[i].ID] = struct{}{}
	}
	// A task the follower's snapshot accounts for is not missing this tick,
	// whatever it was on a prior tick — drop any confirmed-404 streak so a
	// task that legitimately reappeared (e.g. a slow Reassign push that
	// finally landed) starts from zero if it ever goes missing again, rather
	// than resuming a stale count left over from an unrelated earlier gap.
	m.clearMissingStreaks(node, tasks)

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
			var apiErr *cluster.APIError
			if errors.As(gerr, &apiErr) && apiErr.Status == http.StatusNotFound {
				streak := m.bumpMissingStreak(node, t.ID)
				if streak < missingConfirmThreshold {
					m.logger.Info("cluster.mirror.reconcile_missing.confirming",
						"node", node, "task", t.ID, "streak", streak, "threshold", missingConfirmThreshold)
					continue
				}
				// The follower has confirmed this task gone across
				// missingConfirmThreshold consecutive ticks (e.g. trashed as
				// a duplicate cleanup, see #2294's post-mortem) — long enough
				// to rule out a task freshly reassigned to node whose push
				// just hasn't landed yet (Assigner.Reassign stamps
				// AssignedNode before pushing; see missingConfirmThreshold's
				// doc comment). Trash the leader's stale mirror so it stops
				// permanently gating downstream rollup logic (like
				// trackerRollup's cancelled-child check) on a ghost task the
				// follower will never offer again.
				// A move can race the old node's delayed 404. Take the same
				// ownership lock as Reassign and confirm this is still the exact
				// canonical generation we checked before deleting it.
				unlock := lockTaskOwnership(t.ID)
				current, cerr := m.tasks.Get(t.ID)
				if cerr != nil {
					unlock()
					m.logger.Warn("cluster.mirror.reconcile_missing.refresh_failed", "node", node, "task", t.ID, "err", cerr)
					continue
				}
				if current.AssignedNode != node || current.AssignmentRev != t.AssignmentRev {
					unlock()
					m.clearMissingStreak(node, t.ID)
					continue
				}
				if derr := m.tasks.DeleteBy(t.ID, "clusterlead.mirror.reconcile_missing"); derr != nil {
					unlock()
					m.logger.Warn("cluster.mirror.reconcile_missing.trash_failed", "node", node, "task", t.ID, "err", derr)
					continue
				}
				unlock()
				m.clearMissingStreak(node, t.ID)
				m.logger.Info("cluster.mirror.reconcile_missing.trashed", "node", node, "task", t.ID, "confirmations", streak)
				continue
			}
			m.logger.Debug("cluster.mirror.reconcile_missing.failed", "node", node, "task", t.ID, "err", gerr)
			continue
		}
		// Found despite this tick's snapshot omitting it — a later absence
		// is unrelated and must not inherit a stale streak.
		m.clearMissingStreak(node, t.ID)
		m.applyFollowerTaskWithContext(ctx, node, follower)
	}
}

func missingStreakKey(node, taskID string) string { return node + "|" + taskID }

// bumpMissingStreak increments and returns node/taskID's consecutive
// confirmed-404 count.
func (m *Mirror) bumpMissingStreak(node, taskID string) int {
	m.missingMu.Lock()
	defer m.missingMu.Unlock()
	key := missingStreakKey(node, taskID)
	m.missingStreak[key]++
	return m.missingStreak[key]
}

func (m *Mirror) clearMissingStreak(node, taskID string) {
	m.missingMu.Lock()
	defer m.missingMu.Unlock()
	delete(m.missingStreak, missingStreakKey(node, taskID))
}

// clearMissingStreaks drops the confirmed-404 streak for every task node's
// fresh ListTasks snapshot accounts for.
func (m *Mirror) clearMissingStreaks(node string, tasks []task.Task) {
	if len(tasks) == 0 {
		return
	}
	m.missingMu.Lock()
	defer m.missingMu.Unlock()
	for i := range tasks {
		delete(m.missingStreak, missingStreakKey(node, tasks[i].ID))
	}
}

func (m *Mirror) applyFollowerTask(node string, follower task.Task) bool {
	return m.applyFollowerTaskWithContext(context.Background(), node, follower)
}

func (m *Mirror) applyFollowerTaskWithContext(ctx context.Context, node string, follower task.Task) bool {
	// Validate on ingest, not at Put: the id arrives from a peer over HTTP and
	// reaches writeSidecars — which builds eight paths from it — before Put's
	// own ValidateID runs. Rejecting here means a hostile id writes nothing.
	if err := task.ValidateID(follower.ID); err != nil {
		m.logger.Warn("cluster.mirror.reject.invalid_task_id", "node", node, "task", follower.ID, "err", err)
		return false
	}

	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	canonical, err := m.tasks.Get(follower.ID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return m.adoptFollowerTask(ctx, node, follower)
		}
		// A genuine local-store fault (I/O, corrupt frontmatter), not
		// "doesn't exist yet" — the discarded bool at the call site leaves
		// this as the only trace convergence stalled for this task.
		m.logger.Warn("cluster.mirror.apply.get_failed", "node", node, "task", follower.ID, "err", err)
		return false
	}
	if canonical.AssignedNode != node {
		// Silent before this diff too, but now reachable by a new class of
		// cause: adoptFollowerTask assigns a never-seen ID to the first node
		// whose reconcile tick claims it, so a same-ID collision between two
		// followers' independently self-originated tasks (task IDs are only
		// 32 bits of randomness, internal/task/store.go) would permanently
		// strand the loser here with no prior trace at all. Whatever the
		// cause, a canonical/reporting-node mismatch deserves a log line.
		m.logger.Warn("cluster.mirror.apply.assigned_elsewhere", "node", node, "task", follower.ID, "assigned_node", canonical.AssignedNode)
		return false
	}
	m.detectAndRepairDrift(ctx, node, canonical, follower)
	follower, ok := m.localizeFollowerAttachments(ctx, node, canonical, follower)
	if !ok {
		return false
	}
	_, ok = Merge(canonical, follower)
	if !ok {
		return false
	}
	saved, _, err := m.tasks.PutFnBy(follower.ID, "clusterlead.mirror.apply_follower_task", func(cur task.Task) (task.Task, error) {
		latest, err := mergeFollowerForNode(cur, node, follower)
		if err != nil {
			return task.Task{}, err
		}
		return latest, nil
	})
	if err != nil {
		if errors.Is(err, errFollowerUpdateStale) {
			return false
		}
		m.logger.Warn("cluster.mirror.apply.failed", "node", node, "task", follower.ID, "err", err)
		return false
	}
	if err := m.writeSidecars(saved); err != nil {
		m.logger.Warn("cluster.mirror.sidecar.failed", "node", node, "task", follower.ID, "err", err)
		return false
	}
	m.logger.Debug("cluster.mirror.applied", "node", node, "task", follower.ID, "status", string(saved.Status), "rev", saved.MirrorRev)
	return true
}

// mergeFollowerForNode makes the final, lock-protected acceptance decision.
// A reassign can stamp a new owner while the old follower's RPCs are in
// flight, so the reporting node must still own the freshly-read task here,
// not only at the beginning of applyFollowerTaskWithContext.
func mergeFollowerForNode(cur task.Task, node string, follower task.Task) (task.Task, error) {
	if cur.AssignedNode != node {
		return task.Task{}, errFollowerUpdateStale
	}
	latest, ok := Merge(cur, follower)
	if !ok {
		return task.Task{}, errFollowerUpdateStale
	}
	return latest, nil
}

// adoptFollowerTask creates the leader's first canonical record for a task
// that exists only on node — e.g. a follower's own local umbrella expansion
// or triage, created without leader involvement and so never assigned a
// node (ListTasksForNode returns these to the node that actually holds
// them; see its doc comment). There is no existing canonical copy to merge
// against, so unlike applyFollowerTaskWithContext's steady-state path, this
// takes follower's own fields as the whole record rather than preserving
// leader-authoritative identity fields from a prior version — there is no
// prior version.
func (m *Mirror) adoptFollowerTask(ctx context.Context, node string, follower task.Task) bool {
	adopted, ok := m.localizeFollowerAttachments(ctx, node, task.Task{}, follower)
	if !ok {
		return false
	}
	adopted.AssignedNode = node
	adopted.MirrorRev = 1
	followerUpdated := adopted.UpdatedAt
	adopted.MirrorUpdatedAt = &followerUpdated
	adopted.UpdatedAt = time.Now().UTC()
	if _, _, err := m.tasks.PutBy(adopted, "clusterlead.mirror.adopt"); err != nil {
		m.logger.Warn("cluster.mirror.adopt.failed", "node", node, "task", adopted.ID, "err", err)
		return false
	}
	if err := m.writeSidecars(adopted); err != nil {
		m.logger.Warn("cluster.mirror.adopt.sidecar_failed", "node", node, "task", adopted.ID, "err", err)
		return false
	}
	m.logger.Info("cluster.mirror.adopted", "node", node, "task", adopted.ID, "status", string(adopted.Status))
	return true
}

// detectAndRepairDrift compares the two leader-authoritative fields Merge
// never touches — Tags and DependsOn — between the leader's canonical copy
// and what the follower actually reports. Every other field either flows
// follower-authoritative through Merge every reconcile tick (Status,
// Workflow, AgentRuns, ...) or is immutable identity (ID, ProjectID,
// CreatedAt), so an ordinary disagreement there is expected and self-heals
// on its own. Tags/DependsOn only ever change on the leader (umbrella-gate
// release, a manual sybra-cli edit, ...); if a leader-side write to one of
// them never reached the follower — the #2349 class of bug, from any cause,
// not just the one fixed there — nothing else in the reconcile loop will
// ever notice, because Merge doesn't carry them. This is the backstop:
// detected within one reconcile interval, logged, and repaired by pushing
// just the drifted fields onto the follower's own current state (never a
// stale full-task overwrite that could roll back follower-side execution
// progress).
//
// Purely self-healing, no alerting: every case this backstop hits — a
// follower restart mid-push, a transient network blip, a repair that fails
// outright — either fixes itself within one tick or keeps retrying every
// tick after, so there is nothing here a human needs paged for. (#2495
// narrowed alerting to repair-failure/re-drift only, and it still produced
// 42 `cluster_drift` issues in 2 weeks for cases the backstop had already
// fixed — not worth a GitHub issue at any threshold.)
func (m *Mirror) detectAndRepairDrift(ctx context.Context, node string, canonical, follower task.Task) {
	tagsDrift := !slices.Equal(sortedCopy(canonical.Tags), sortedCopy(follower.Tags))
	depsDrift := !slices.Equal(sortedCopy(canonical.DependsOn), sortedCopy(follower.DependsOn))
	condsDrift := !slices.Equal(sortedConditions(canonical.DependsOnConditions), sortedConditions(follower.DependsOnConditions))
	if !tagsDrift && !depsDrift && !condsDrift {
		return
	}
	m.logger.Error("cluster.mirror.drift_detected",
		"task", canonical.ID, "node", node,
		"tags_drift", tagsDrift, "deps_drift", depsDrift, "conds_drift", condsDrift,
		"canonical_tags", canonical.Tags, "follower_tags", follower.Tags,
		"canonical_depends_on", canonical.DependsOn, "follower_depends_on", follower.DependsOn,
		"canonical_depends_on_conditions", canonical.DependsOnConditions, "follower_depends_on_conditions", follower.DependsOnConditions)
	m.repairDrift(ctx, node, canonical)
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
// driftRepairTimeout first. Every failure path logs its own Warn; a failed
// repair simply retries on the next reconcile tick.
func (m *Mirror) repairDrift(ctx context.Context, node string, canonical task.Task) {
	// Serialize against ownership transfer before contacting the follower. A
	// stale repair of the former owner must never resurrect its task after a
	// reassignment has completed.
	unlock := lockTaskOwnership(canonical.ID)
	defer unlock()
	current, err := m.tasks.Get(canonical.ID)
	if err != nil {
		m.logger.Warn("cluster.mirror.drift_repair.refresh_failed", "task", canonical.ID, "node", node, "err", err)
		return
	}
	if current.AssignedNode != node || current.AssignmentRev != canonical.AssignmentRev {
		return
	}
	canonical = current
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
	repaired.DependsOnConditions = canonical.DependsOnConditions
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

// sortedConditions returns a copy of conds sorted by (Ref, Kind, Value) so
// DependsOnConditions — a set, not an ordered list — compares equal
// regardless of authoring order, mirroring sortedCopy's treatment of
// Tags/DependsOn.
func sortedConditions(conds []task.DepCondition) []task.DepCondition {
	out := slices.Clone(conds)
	slices.SortFunc(out, func(a, b task.DepCondition) int {
		if c := strings.Compare(a.Ref, b.Ref); c != 0 {
			return c
		}
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		return strings.Compare(a.Value, b.Value)
	})
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

// writeSidecars compensates for PutBy/PutFnBy's file-backend gap: the file
// Store's plain whole-task write never touches the sidecar files, so the
// caller writes them separately here. On the database backend this write
// would be pure waste — taskdb's PutBy/PutFnBy already persist every
// sidecar field in the same transaction as the task write via
// SidecarsFromTask — and worse, it would leave stray, never-read files on
// disk under a backend that no longer treats the tasks directory as
// authoritative for this content, so it is skipped entirely there.
func (m *Mirror) writeSidecars(t task.Task) error {
	if m.tasks == nil || !m.tasks.PersistsToFile() {
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
	if err := store.CurrentTestFailures().Write(t.ID, t.CurrentTestFailures); err != nil {
		return err
	}
	if err := store.AcceptanceLedgers().Write(t.ID, t.AcceptanceLedger); err != nil {
		return err
	}
	if err := store.SpecDecisions().Write(t.ID, t.SpecDecision); err != nil {
		return err
	}
	return writePlanDraftSidecars(store, t)
}

// writePlanDraftSidecars mirrors t.PlanDrafts onto the leader's local
// PlanDraftStore files: unlike every other sidecar field above, a plan
// draft is one of N entries in a map rather than a single string, so
// bringing the leader's files in line with the follower's reported state
// needs both writing what the follower has and deleting what it no longer
// does — a name dropped by the follower (e.g. a re-plan's DeleteAll) must
// not linger as a stale file the leader keeps serving.
func writePlanDraftSidecars(store *task.Store, t task.Task) error {
	existing, err := store.PlanDrafts().List(t.ID)
	if err != nil {
		return err
	}
	for name, content := range t.PlanDrafts {
		if err := store.PlanDrafts().Write(t.ID, name, content); err != nil {
			return err
		}
	}
	for name := range existing {
		if _, ok := t.PlanDrafts[name]; ok {
			continue
		}
		if err := store.PlanDrafts().Delete(t.ID, name); err != nil {
			return err
		}
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
	out.Escalation = follower.Escalation
	out.AutonomyOutcome = follower.AutonomyOutcome
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
	out.CurrentTestFailures = follower.CurrentTestFailures
	out.AcceptanceLedger = follower.AcceptanceLedger
	out.SpecDecision = follower.SpecDecision
	out.PlanDrafts = follower.PlanDrafts
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
