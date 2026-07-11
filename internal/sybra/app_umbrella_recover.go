package sybra

import (
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
)

// recoverDegradedUmbrellas scans for umbrella trackers tagged
// umbrella.FallbackTag and schedules an async umbrella.RecoverDegraded run
// for each eligible, due tracker — one goroutine per distinct umbrella ref,
// single-flighted against App's own in-flight set. Ineligible trackers
// (disabled config, wrong project type, terminal/frozen status, exhausted,
// cooling down, invalid ref, or duplicate tracker groups) are skipped without
// ever calling the planner. Cheap to call every gate tick: once nothing is
// due it does one task list scan and returns.
func (a *App) recoverDegradedUmbrellas() {
	if a.cfg == nil || !a.cfg.Umbrella.Enabled {
		return
	}
	tasks, err := a.tasks.List()
	if err != nil {
		return
	}

	byRef := map[string][]task.Task{}
	for i := range tasks {
		t := &tasks[i]
		if t.TaskType != task.TaskTypeUmbrella || !slices.Contains(t.Tags, umbrella.FallbackTag) {
			continue
		}
		key := umbrella.NormalizeIssueRef(t.Issue)
		byRef[key] = append(byRef[key], *t)
	}

	now := time.Now()
	for key, trackers := range byRef {
		if len(trackers) > 1 {
			a.logger.Warn("umbrella.recover.skip", "reason", "duplicate degraded trackers", "ref", key, "count", len(trackers))
			continue
		}
		tracker := trackers[0]
		if !a.umbrellaRecoveryEligible(tracker, now) {
			continue
		}
		if !a.markUmbrellaRecoveryInFlight(key) {
			continue // already recovering
		}
		recoverFn := a.runUmbrellaRecovery
		if a.umbrellaRecoverFn != nil {
			recoverFn = a.umbrellaRecoverFn
		}
		a.wg.Go(func() {
			defer a.clearUmbrellaRecoveryInFlight(key)
			recoverFn(tracker)
		})
	}
}

// umbrellaRecoveryEligible reports whether tracker should have a
// RecoverDegraded run scheduled for it. Every check here is a cheap,
// planner-free filter — the goal is that an ineligible tracker never causes
// a goroutine spawn or a planner call.
func (a *App) umbrellaRecoveryEligible(tracker task.Task, now time.Time) bool {
	if _, _, ok := umbrella.ParseRef(tracker.Issue); !ok {
		a.logger.Warn("umbrella.recover.skip", "reason", "invalid tracker issue ref", "task_id", tracker.ID, "issue", tracker.Issue)
		return false
	}
	if tracker.Status == task.StatusDone || tracker.Status == task.StatusCancelled {
		return false
	}
	if tracker.Status == task.StatusHumanRequired || tracker.Status == task.StatusBlocked {
		// A human/block hold for a reason unrelated to recovery (e.g. a
		// dependency cycle, a stuck child) must not be disturbed by an async
		// re-plan. An empty or recovery-owned reason means the hold (if any)
		// came from recovery itself, so it's safe to keep retrying.
		if tracker.StatusReason != "" && !strings.HasPrefix(tracker.StatusReason, umbrella.RecoveryFailureReasonPrefix) {
			return false
		}
	}
	if umbrella.HasRecoverExhaustedTag(tracker.Tags) {
		return false
	}
	if !umbrella.RecoverDue(tracker.Tags, now) {
		return false
	}
	if !a.umbrellaRecoveryProjectAllowed(tracker) {
		return false
	}
	return true
}

// umbrellaRecoveryProjectAllowed reports whether the tracker's project (if
// any) is registered and its type is allowed to run automations on this
// machine. A tracker with no project ID is not filtered (nothing to check
// against). A missing/unreadable project record fails closed (skip) — an
// automation must never guess a stale project's routing.
func (a *App) umbrellaRecoveryProjectAllowed(tracker task.Task) bool {
	if tracker.ProjectID == "" {
		return true
	}
	p, err := a.projects.Get(tracker.ProjectID)
	if err != nil {
		a.logger.Warn("umbrella.recover.skip", "reason", "unknown project", "task_id", tracker.ID, "project_id", tracker.ProjectID)
		return false
	}
	return a.allowsProjectType(p.Type)
}

// markUmbrellaRecoveryInFlight marks ref as recovering, returning false if it
// already was (single-flight).
func (a *App) markUmbrellaRecoveryInFlight(ref string) bool {
	a.umbrellaRecoveryMu.Lock()
	defer a.umbrellaRecoveryMu.Unlock()
	if a.umbrellaRecoveryInFlight[ref] {
		return false
	}
	a.umbrellaRecoveryInFlight[ref] = true
	return true
}

func (a *App) clearUmbrellaRecoveryInFlight(ref string) {
	a.umbrellaRecoveryMu.Lock()
	defer a.umbrellaRecoveryMu.Unlock()
	delete(a.umbrellaRecoveryInFlight, ref)
}

// umbrellaRecoveryInFlightSnapshot returns a point-in-time copy of the
// in-flight set for a gate tick to filter release/rollup against, without
// holding the recovery lock for the whole tick.
func (a *App) umbrellaRecoveryInFlightSnapshot() map[string]bool {
	a.umbrellaRecoveryMu.Lock()
	defer a.umbrellaRecoveryMu.Unlock()
	out := make(map[string]bool, len(a.umbrellaRecoveryInFlight))
	for k := range a.umbrellaRecoveryInFlight {
		out[k] = true
	}
	return out
}

// umbrellaRecoveryExpandOptions mirrors the grounding-toggle wiring used by
// the other umbrella.Expand call sites (initIssuesFetcher, wireTaskService):
// threads a grounder ExpandOption only when cfg.Umbrella.Ground is set.
// Reads a.cfg fresh on every call so a config reload changes grounding
// without re-wiring.
func (a *App) umbrellaRecoveryExpandOptions() []umbrella.ExpandOption {
	if !a.cfg.Umbrella.Ground {
		return nil
	}
	return []umbrella.ExpandOption{umbrella.WithExpandGrounder(buildGroundLister(a.projects), a.cfg.Umbrella.GroundMinSubIssues)}
}

// runUmbrellaRecovery calls umbrella.RecoverDegraded for one tracker and
// logs/audits the outcome. Runs off the gate path in its own goroutine (see
// recoverDegradedUmbrellas), so a slow or failing planner call for one
// umbrella never delays dispatch for the rest of the board.
func (a *App) runUmbrellaRecovery(tracker task.Task) {
	a.logger.Info("umbrella.recover.attempt", "task_id", tracker.ID, "issue", tracker.Issue)
	a.logAudit(audit.EventUmbrellaRecovery, tracker.ID, "", map[string]any{"outcome": "attempted"})

	opts := a.umbrellaRecoveryExpandOptions()
	run := umbrella.FallbackPlannerRunner(a.cfg.Umbrella.Model, a.providerHealth)

	// a.ctx is App's root context, bound once at Startup — same pattern as
	// initIssuesFetcher's expander closure.
	result, err := umbrella.RecoverDegraded(a.ctx, a.tasks, run, tracker.Issue, opts...)
	if err != nil {
		a.logger.Error("umbrella.recover.error", "task_id", tracker.ID, "err", err)
		a.logAudit(audit.EventUmbrellaRecovery, tracker.ID, "", map[string]any{"outcome": "error", "reason": err.Error()})
		return
	}
	a.logger.Info("umbrella.recover.result", "task_id", tracker.ID,
		"outcome", result.Outcome, "reason", result.Reason,
		"children_created", result.ChildrenCreated, "children_updated", result.ChildrenUpdated,
		"fail_count", result.FailCount, "exhausted", result.Exhausted)
	a.logAudit(audit.EventUmbrellaRecovery, tracker.ID, "", map[string]any{
		"outcome":   string(result.Outcome),
		"reason":    result.Reason,
		"exhausted": result.Exhausted,
	})
}
