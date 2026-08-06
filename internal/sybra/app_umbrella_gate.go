package sybra

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
	"github.com/Automaat/sybra/internal/watchdogreason"
)

// umbrellaGatedTag is the tag the expander sets on a gate-blocked child; the
// gate requires it to release and strips it on release. Aliased from the
// umbrella package so the CLI expander and this gate cannot drift.
const umbrellaGatedTag = umbrella.GatedTag

// umbrellaSettleDelay is how long after creation a childless umbrella tracker
// must persist before the gate treats it as complete. Comfortably exceeds the
// 1-minute orchestrator tick so a tracker whose children are still being
// materialized in the same expansion is never closed prematurely.
const umbrellaSettleDelay = 2 * time.Minute

// umbrellaState aggregates one umbrella's tracker and children for a gate tick.
type umbrellaState struct {
	tracker    *task.Task
	cap        int // max children running at once
	total      int // child task count
	doneCount  int
	active     int // children occupying a parallelism slot
	anyHR      bool
	anyBlocked bool // non-gated child stuck in `blocked` (e.g. human-review flip)
	expanding  bool
	released   int // children released so far this tick (counts toward the cap)
	children   []umbrellaProgressChild
}

type umbrellaProgressChild struct {
	id     string
	title  string
	issue  string
	status task.Status
}

const (
	umbrellaProgressStart = "<!-- sybra:umbrella-progress:start -->"
	umbrellaProgressEnd   = "<!-- sybra:umbrella-progress:end -->"
)

// releaseUnblockedChildren is the umbrella gate, run every orchestrator tick.
// It releases gate-blocked children whose dependencies are done — up to each
// umbrella's max-parallel cap — and rolls each umbrella tracker's status up
// from its children (cycle or a stuck child → human-required; all done →
// done + close the umbrella issue). No-op when no umbrella tasks exist.
func (a *App) releaseUnblockedChildren(ctx context.Context) {
	a.recoverDegradedUmbrellas()

	tasks, err := a.tasks.List()
	if err != nil {
		return
	}
	inFlight := a.umbrellaRecoveryInFlightSnapshot()

	byID := make(map[string]*task.Task, len(tasks))
	states := map[string]*umbrellaState{}
	nodes := make([]umbrella.Node, len(tasks))
	hasUmbrella := false

	stateFor := func(ref string) *umbrellaState {
		key := umbrella.NormalizeIssueRef(ref)
		s := states[key]
		if s == nil {
			s = &umbrellaState{cap: umbrella.DefaultMaxParallel}
			states[key] = s
		}
		return s
	}

	for i := range tasks {
		t := &tasks[i]
		byID[t.ID] = t
		if t.TaskType == task.TaskTypeUmbrella {
			hasUmbrella = true
			st := stateFor(t.Issue)
			st.tracker = t
			st.cap = umbrella.ParseMaxParallel(t.Tags)
			st.expanding = umbrella.HasActiveExpandPhase(t.Tags) || slices.Contains(t.Tags, umbrella.ExpandingTag)
		}
		if t.UmbrellaIssue != "" {
			hasUmbrella = true
			accumulateChild(stateFor(t.UmbrellaIssue), t)
		}
	}
	if !hasUmbrella {
		return
	}

	crossProgramRefs := map[string][]string{} // task ID -> free-text external blockers found in its body
	// mergedDeps mirrors the DependsOn set the gate's own release decision runs
	// against — t.DependsOn folded together with body-derived cross-program refs
	// (umbrella.ExternalBlockers). holdScopeVerdictBlocked matches a scope
	// verdict against this same merged set, not the raw t.DependsOn, so a verdict
	// naming a body-referenced external ref still holds the child (see finding on
	// namesScopeVerdictDep reading t.DependsOn directly).
	mergedDeps := make(map[string][]string, len(tasks))

	for i := range tasks {
		t := &tasks[i]
		expanding := false
		if t.UmbrellaIssue != "" {
			expanding = stateFor(t.UmbrellaIssue).expanding
		}
		dependsOn := t.DependsOn
		if t.UmbrellaIssue != "" {
			if refs := umbrella.ExternalBlockers(t.Body, t.Issue); len(refs) > 0 {
				crossProgramRefs[t.ID] = refs
				dependsOn = mergeIssueRefs(dependsOn, refs)
				if !slices.Equal(dependsOn, t.DependsOn) {
					a.persistCrossProgramDeps(ctx, t, dependsOn)
				}
			}
		}
		mergedDeps[t.ID] = dependsOn
		nodes[i] = umbrella.Node{
			ID:        t.ID,
			Issue:     t.Issue,
			Umbrella:  t.UmbrellaIssue,
			DependsOn: dependsOn,
			Done:      t.Status == task.StatusDone,
			// Gate-marked todo children (current model) and legacy
			// blocked+gated children (tasks created before this change)
			// are both eligible for release. Never release a task that is
			// blocked without the gating tag (contained Sybra bug), one the
			// workflow engine itself parked blocked (e.g. a watchdog-exhausted
			// retry, see isWorkflowOwnedBlock), or one that already ran an
			// implementation agent — none of these holds is the umbrella
			// gate's own, and releasing one would discard the child's
			// in-flight workflow (#2538), even if it still carries the gating
			// tag. A child whose umbrella is currently mid-recovery is held
			// regardless — RecoverDegraded may be mutating this same
			// umbrella's children concurrently, so this tick must not
			// release from a partial graph.
			Awaiting: t.UmbrellaIssue != "" &&
				!inFlight[umbrella.NormalizeIssueRef(t.UmbrellaIssue)] &&
				!expanding &&
				(t.Status == task.StatusTodo || t.Status == task.StatusBlocked) &&
				!isWorkflowOwnedBlock(t) &&
				!hasStartedImplementation(t) &&
				slices.Contains(t.Tags, umbrellaGatedTag),
		}
	}

	g := umbrella.Build(nodes)
	cyclic := map[string]bool{}
	for _, umb := range g.CyclicUmbrellas() {
		cyclic[umbrella.NormalizeIssueRef(umb)] = true
	}

	a.flagCrossProgramBlockers(crossProgramRefs, nodes, byID, g)

	for ref, st := range states {
		if st.expanding {
			delete(states, ref)
		}
	}
	for ref := range inFlight {
		delete(states, ref)
	}

	// A blocked tracker pauses only tracker rollup/issue close; dependency-ready
	// children still release so independent work under the umbrella can proceed.
	// Expanding/recovering trackers are removed from states above, so release
	// and rollup both hold until the complete DAG is visible.
	ready := a.holdUnmetConditions(g.ReadyToRelease(), byID, states, mergedDeps)
	ready = a.holdScopeVerdictBlocked(ready, byID, states, mergedDeps)
	a.releaseCapped(ctx, ready, byID, states)

	// An in-flight recovery run owns this umbrella's tracker/children this
	// tick; skip its rollup entirely rather than computing status from a
	// snapshot RecoverDegraded may be mutating underneath. Unrelated
	// umbrellas continue rolling up normally.
	a.rollupTrackers(states, cyclic)
}

// hasStartedImplementation reports whether t has ever run an implementation
// agent. A dependency-gated child that never left blocked-awaiting-deps has
// none; the umbrella gate must never release one that does, since a blocked
// status on a child that already ran implementation means a watchdog
// exhaustion, not an unmet dependency (sybra#2538).
func hasStartedImplementation(t *task.Task) bool {
	for i := range slices.Backward(t.AgentRuns) {
		role := t.AgentRuns[i].Role
		if role == "" || role == string(agent.RoleImplementation) {
			return true
		}
	}
	return false
}

// persistCrossProgramDeps writes a body-derived external dependency ref (see
// umbrella.ExternalBlockers) into t's own DependsOn field so the precondition
// survives as structured state a human or the next agent run can read
// directly, instead of being re-derived from prose on every gate tick and
// every dispatch decision (the exact churn behind sybra#2640: task aa8a3956
// re-confirmed the same unmet #2464 precondition across 5+ runs because
// nothing durable recorded it). Kept alongside the ephemeral merge used for
// this tick's graph — that merge is discarded at function return, so without
// this write the field a human inspects via `sybra-cli get`/the GUI would
// never show the dependency the gate is actually enforcing. On success,
// updates t in place so the rest of this tick's pass (nodes[i] below, byID)
// observes the new value immediately, and best-effort forwards the edit to a
// follower-homed task's home node the same way TaskService.UpdateTask does
// for any other Tags/DependsOn edit — a missed push just leaves Mirror's
// drift backstop to repair it on the next reconcile.
func (a *App) persistCrossProgramDeps(ctx context.Context, t *task.Task, merged []string) {
	updated, err := a.tasks.Update(t.ID, task.Update{DependsOn: task.Ptr(merged)})
	if err != nil {
		a.logger.Error("umbrella.gate.cross_program_blocker.persist_failed", "task_id", t.ID, "err", err)
		return
	}
	t.DependsOn = updated.DependsOn
	if a.assigner == nil {
		return
	}
	pushCtx, cancel := context.WithTimeout(ctx, pushReleaseTimeout)
	defer cancel()
	if _, err := a.assigner.PushFieldUpdate(pushCtx, updated); err != nil {
		a.logger.Warn("umbrella.gate.cross_program_blocker.push_failed", "task_id", t.ID, "err", err)
	}
}

// mergeIssueRefs unions extra into existing, de-duplicated by
// NormalizeIssueRef and preserving existing's entries (and their original
// spelling) first.
func mergeIssueRefs(existing, extra []string) []string {
	seen := make(map[string]bool, len(existing)+len(extra))
	out := make([]string, 0, len(existing)+len(extra))
	for _, r := range append(slices.Clone(existing), extra...) {
		key := umbrella.NormalizeIssueRef(r)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

// flagCrossProgramBlockers stamps a distinct StatusReason on a still-gated
// (Awaiting) child whose body names a free-text "after #N"/"strictly after
// #N" dependency (see umbrella.ExternalBlockers) that has not resolved to a
// done task — almost always a cross-program issue no Sybra task tracks at
// all, since the planner's DependsOn can only ever reference the umbrella's
// own sub-issues (buildPlanSchema). Without this, the child's board card
// looks indistinguishable from any other dependency-satisfied child, and a
// human — or an automated review cycle that got as far as dispatching it
// before catching the mismatch — rediscovers the same unmet dependency from
// scratch every time instead of seeing it named up front (real incident:
// umbrella #2493's child #2503 named "strictly after #2464" in its body and
// was released anyway, burning 4 review runs re-confirming the same gap;
// sybra#2616). Only touches tasks that are actually held back by one of
// these refs, so it never fights releaseCapped for a child ready to go.
func (a *App) flagCrossProgramBlockers(refsByTask map[string][]string, nodes []umbrella.Node, byID map[string]*task.Task, g *umbrella.Graph) {
	if len(refsByTask) == 0 {
		return
	}
	awaiting := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		awaiting[n.ID] = n.Awaiting
	}
	for id, refs := range refsByTask {
		if !awaiting[id] {
			continue
		}
		unresolved := g.UnresolvedRefs(refs)
		if len(unresolved) == 0 {
			continue
		}
		t := byID[id]
		if t == nil {
			continue
		}
		reason := externalBlockerReason(unresolved)
		if t.StatusReason == reason {
			continue
		}
		if _, err := a.tasks.Update(id, task.Update{StatusReason: task.Ptr(reason)}); err != nil {
			a.logger.Error("umbrella.gate.cross_program_blocker.flag_failed", "task_id", id, "err", err)
			continue
		}
		t.StatusReason = reason
	}
}

// externalBlockerReason renders a deterministic, human-facing status reason
// naming the specific unresolved cross-program ref(s) still holding a child
// back, distinct from the generic reasons trackerRollup/releaseCapped use.
func externalBlockerReason(refs []string) string {
	sorted := slices.Clone(refs)
	slices.Sort(sorted)
	return "held: body names external dependency " + strings.Join(sorted, ", ") + " not tracked as done"
}

// dependencyConditionReasonPrefix marks a human-required status_reason as
// having been written by holdUnmetConditions for a "note"-kind
// task.DepCondition, mirroring dependencyScopeVerdictReasonPrefix's pattern
// for the (deliberately distinct) scope-verdict gate.
const dependencyConditionReasonPrefix = "held: unmet depends_on condition for"

// holdUnmetConditions filters ready — the children ReadyToRelease has
// confirmed have every depends_on ref resolved to Done — removing any child
// whose task.DependsOnConditions names a condition on one of those very refs
// that is not yet satisfied. Runs before holdScopeVerdictBlocked so a fresh
// note/label condition is enforced on the same tick a dependency first
// closes, not one tick later.
//
// A child with no DependsOnConditions returns immediately with no I/O and no
// task.Update — every existing no-condition child (and its tests) sees zero
// behavior change. For a child that does carry conditions, only the first
// condition whose Ref matches a current dependency is evaluated per tick
// (validateDepConditions enforces at most one condition per ref at write
// time); a Ref that no longer names a current dependency is inert, exactly
// like a stale blocker.KindDependencyScopeUnmet verdict.
func (a *App) holdUnmetConditions(ready []string, byID map[string]*task.Task, states map[string]*umbrellaState, mergedDeps map[string][]string) []string {
	if len(ready) == 0 {
		return ready
	}
	kept := ready[:0]
	for _, id := range ready {
		t := byID[id]
		if t == nil || len(t.DependsOnConditions) == 0 {
			kept = append(kept, id)
			continue
		}
		if a.holdChildUnmetCondition(t, mergedDeps[id], states) {
			continue
		}
		kept = append(kept, id)
	}
	return kept
}

// holdChildUnmetCondition evaluates t's DependsOnConditions against its
// current dependency set, applying at most the first matching (non-inert)
// condition per tick. Returns held=true when the child must not release this
// tick — the caller drops it from ready without further checks.
func (a *App) holdChildUnmetCondition(t *task.Task, dependsOn []string, states map[string]*umbrellaState) (held bool) {
	for _, cond := range t.DependsOnConditions {
		if !matchesDepRef(cond.Ref, dependsOn) {
			continue // inert: Ref is not among this task's current dependencies
		}
		switch cond.Kind {
		case task.DepConditionKindNote:
			a.escalateUnmetCondition(t, cond, states)
			return true
		case task.DepConditionKindLabel:
			met, checked := a.labelConditionMet(cond)
			if !checked {
				// FetchIssue failed or the ref is unresolvable — fail closed
				// and retry next tick rather than releasing on unverifiable
				// input.
				return true
			}
			if !met {
				a.holdSelfHealingCondition(t, unmetLabelConditionReason(cond))
				return true
			}
		default:
			// Unrecognized Kind (a hand-edited task file — CLI/API input is
			// validated at write time by task.applyDependsOnConditionsField).
			// Fail closed: hold without escalating.
			a.holdSelfHealingCondition(t, unknownConditionKindReason(cond))
			return true
		}
	}
	return false
}

// labelConditionMet checks a "label" DepCondition against its referenced
// closing issue's current GitHub labels. checked=false means the check could
// not be performed (unresolvable ref or a FetchIssue error) and the caller
// must fail closed rather than read met's zero value as "absent".
func (a *App) labelConditionMet(cond task.DepCondition) (met, checked bool) {
	repo, number, ok := umbrella.ParseRef(cond.Ref)
	if !ok {
		a.logger.Warn("umbrella.gate.condition.unresolvable_ref", "ref", cond.Ref)
		return false, false
	}
	fetch := a.umbrellaFetchIssue
	if fetch == nil {
		fetch = github.FetchIssue
	}
	issue, err := fetch(repo, number)
	if err != nil {
		a.logger.Warn("umbrella.gate.condition.fetch_issue_failed", "ref", cond.Ref, "err", err)
		return false, false
	}
	return slices.Contains(issue.Labels, cond.Value), true
}

// holdSelfHealingCondition records reason on t without changing Status — the
// child stays gated/blocked (or todo+tagged) exactly as before, so a later
// tick re-evaluates it once the underlying condition changes (a label is
// applied, or the task file is corrected). No-ops when reason already
// matches, so a condition that stays unmet across many ticks does not write
// (or push to a follower) every single tick.
func (a *App) holdSelfHealingCondition(t *task.Task, reason string) {
	if t.StatusReason == reason {
		return
	}
	if _, err := a.tasks.Update(t.ID, task.Update{StatusReason: task.Ptr(reason)}); err != nil {
		a.logger.Error("umbrella.gate.condition.hold_failed", "task_id", t.ID, "err", err)
		return
	}
	t.StatusReason = reason
}

func unmetLabelConditionReason(cond task.DepCondition) string {
	return "held: depends_on " + cond.Ref + " missing required label " + cond.Value
}

func unknownConditionKindReason(cond task.DepCondition) string {
	return "held: depends_on " + cond.Ref + " has unrecognized condition kind " + cond.Kind
}

// escalateUnmetCondition holds t at human-required for a "note"-kind
// condition, naming the ref and the free-text acceptance note a human must
// confirm before this can release. Sets blocker.KindDependencyConditionUnmet
// — deliberately not blocker.KindDependencyScopeUnmet, so a human clearing
// this blocker is never misread as having confirmed an unrelated scope
// verdict (see blocker.KindDependencyConditionUnmet's doc comment).
//
// Clearing the blocker alone does not release the child: as long as this
// condition still names a current dependency, holdChildUnmetCondition
// re-escalates the next tick it becomes ready again. A human must remove or
// edit the condition itself (via a --depends-on-condition update) once the
// note's scope is confirmed to exist — an accepted limitation matching
// blocker.KindDependencyScopeUnmet's existing require-explicit-human-edit
// design, not an oversight.
func (a *App) escalateUnmetCondition(t *task.Task, cond task.DepCondition, states map[string]*umbrellaState) {
	reason := dependencyConditionReasonPrefix + " " + cond.Ref + ": " + cond.Value
	if _, err := a.tasks.Apply(task.TransitionIntent{
		TaskID:   t.ID,
		ToStatus: task.StatusHumanRequired,
		Actor:    "umbrella.gate.condition.escalate",
		Extra: task.Update{
			StatusReason: task.Ptr(reason),
			Blocker: task.Ptr(blocker.State{
				Kind:       blocker.KindDependencyConditionUnmet,
				Code:       cond.Ref,
				NextAction: cond.Value,
			}),
		},
	}); err != nil {
		a.logger.Error("umbrella.gate.condition.escalate_failed", "task_id", t.ID, "err", err)
		return
	}
	if st := states[umbrella.NormalizeIssueRef(t.UmbrellaIssue)]; st != nil {
		st.setChildStatus(t.ID, task.StatusHumanRequired)
		st.anyHR = true
	}
	a.logger.Info("umbrella.gate.condition.held", "task_id", t.ID, "dep", cond.Ref)
}

// dependencyScopeVerdictReasonPrefix marks a human-required status_reason as
// having been written by holdScopeVerdictBlocked, mirroring
// externalBlockerReason/admissionPreflightReasonPrefix's pattern for other
// mechanical gates.
const dependencyScopeVerdictReasonPrefix = "held: prior verdict flagged unmet scope for"

// holdScopeVerdictBlocked filters ready — the children ReadyToRelease just
// confirmed have every depends_on ref resolved to Done — removing any child
// whose Blocker already carries an explicit prior verdict
// (blocker.KindDependencyScopeUnmet) that one of those very refs did not
// actually satisfy the scope this task needs, and escalating it to
// human-required instead. depsSatisfied only ever asks "is the referenced
// task Done?" — it cannot tell a scope-complete closure from one that merely
// closed the same issue number (sybra#2637: umbrella #2493's child #2503
// cycled blocked -> todo -> human-required 8 times because PR #2620 closed
// #2464 without implementing the permutation-contract scope #2503 actually
// depended on, burning a fresh implementation-agent run each cycle). Once a
// prior run recorded that verdict, trust it over the raw Done flag: require a
// human to clear the blocker (or record a fresh one) after confirming the
// scope now genuinely exists, rather than silently re-dispatching into the
// same already-known-negative cycle. A blocker whose Code no longer names a
// current depends_on ref (e.g. DependsOn was edited since) is stale and must
// not hold release.
func (a *App) holdScopeVerdictBlocked(ready []string, byID map[string]*task.Task, states map[string]*umbrellaState, mergedDeps map[string][]string) []string {
	if len(ready) == 0 {
		return ready
	}
	kept := ready[:0]
	for _, id := range ready {
		t := byID[id]
		if t == nil || !namesScopeVerdictDep(t, mergedDeps[id]) {
			kept = append(kept, id)
			continue
		}
		reason := dependencyScopeVerdictReasonPrefix + " " + t.Blocker.Code +
			" — clear the blocker once the required scope is confirmed to exist"
		if _, err := a.tasks.Apply(task.TransitionIntent{
			TaskID:   id,
			ToStatus: task.StatusHumanRequired,
			Actor:    "umbrella.gate.scope_verdict.hold",
			Extra: task.Update{
				StatusReason: task.Ptr(reason),
				Blocker:      task.Ptr(t.Blocker),
			},
		}); err != nil {
			a.logger.Error("umbrella.gate.scope_verdict.hold_failed", "task_id", id, "err", err)
			kept = append(kept, id) // don't silently strand it on our own write error
			continue
		}
		if st := states[umbrella.NormalizeIssueRef(t.UmbrellaIssue)]; st != nil {
			st.setChildStatus(id, task.StatusHumanRequired)
			st.anyHR = true
		}
		a.logger.Info("umbrella.gate.scope_verdict.held", "task_id", id, "dep", t.Blocker.Code)
	}
	return kept
}

// namesScopeVerdictDep reports whether t carries an explicit prior
// dependency-scope-unmet verdict naming one of its own current dependency refs.
// dependsOn is the same merged set the gate's release decision runs against
// (t.DependsOn plus body-derived cross-program refs), not the raw t.DependsOn,
// so a verdict naming a body-referenced external ref is still honored.
func namesScopeVerdictDep(t *task.Task, dependsOn []string) bool {
	if t.Blocker.Kind != blocker.KindDependencyScopeUnmet || t.Blocker.Code == "" {
		return false
	}
	if dependsOn == nil {
		dependsOn = t.DependsOn
	}
	return matchesDepRef(t.Blocker.Code, dependsOn)
}

// matchesDepRef reports whether code names one of the dependsOn refs. It first
// matches on the canonical "owner/repo#n" form (via NormalizeIssueRef, which
// collapses github.com URLs and lowercases shorthand). If code is a bare "#n"
// (or plain "n") with no owner/repo — the spelling a human copies straight off
// a GitHub issue, which NormalizeIssueRef cannot canonicalize without a repo —
// it falls back to matching by issue number alone. That errs toward holding the
// child rather than releasing it into the known-negative re-dispatch cycle this
// gate exists to stop (sybra#2637).
func matchesDepRef(code string, dependsOn []string) bool {
	target := umbrella.NormalizeIssueRef(code)
	if slices.ContainsFunc(dependsOn, func(ref string) bool {
		return umbrella.NormalizeIssueRef(ref) == target
	}) {
		return true
	}
	if n, ok := bareIssueNumber(code); ok {
		return slices.ContainsFunc(dependsOn, func(ref string) bool {
			_, rn, rok := umbrella.ParseRef(ref)
			return rok && rn == n
		})
	}
	return false
}

// bareIssueNumber parses a repo-less issue reference ("#42" or "42") into its
// number. It reports ok=false for anything carrying an owner/repo (those are
// handled by the canonical NormalizeIssueRef path) or that is not a plain
// number.
func bareIssueNumber(s string) (int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if s == "" || strings.ContainsAny(s, "/#") {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// accumulateChild folds one child task's status into its umbrella's tally.
// A todo child carrying the gating tag is still waiting for deps — it must not
// count as active or it would consume a parallelism slot before being released.
func accumulateChild(st *umbrellaState, t *task.Task) {
	st.total++
	st.children = append(st.children, umbrellaProgressChild{
		id:     t.ID,
		title:  t.Title,
		issue:  t.Issue,
		status: t.Status,
	})
	switch t.Status {
	case task.StatusDone:
		st.doneCount++
	case task.StatusCancelled:
		// No side effect here: whether a cancelled child blocks rollup depends
		// on whether a sibling still covers its issue (a cancelled duplicate
		// from an umbrella-expansion race vs. a genuine abandonment), which
		// needs the full child set — see unresolvedCancellation, called from
		// trackerRollup once every child has been folded in.
	case task.StatusHumanRequired:
		if watchdogreason.IsRetryableStop(t.StatusReason) {
			st.active++
			return
		}
		st.anyHR = true
	case task.StatusBlocked:
		if isWorkflowOwnedBlock(t) {
			// The workflow engine parked this itself (e.g. a watchdog-exhausted
			// retry) — never the umbrella gate's own hold, even if it still
			// carries the gating tag. Surface it like any other stuck child
			// below rather than silently treating it as dependency-awaiting.
			st.anyBlocked = true
			return
		}
		if slices.Contains(t.Tags, umbrellaGatedTag) {
			// Gate-blocked, awaiting its dependencies — not stuck, handled by
			// ReadyToRelease/depsSatisfied.
			return
		}
		// Blocked without the gating tag: not the umbrella gate's own hold (e.g.
		// a human-review flip for a contained Sybra bug). depsSatisfied requires
		// a dependency to reach Done, so a dependent chain waiting on this child
		// would otherwise stall forever with no escalation. Surface it like any
		// other stuck child, but do not count it against the parallelism cap:
		// blocked work cannot make progress, and consuming all slots would freeze
		// unrelated ready children under the same umbrella.
		st.anyBlocked = true
	default:
		if isRunningChild(t.Status) && !slices.Contains(t.Tags, umbrellaGatedTag) {
			st.active++
		}
	}
}

// isWorkflowOwnedBlock reports whether t's `blocked` status is held by the
// workflow engine itself — e.g. handleWatchdogRateLimitRetry's zero-output
// exhaustion path, or canRetryWorktreeRepair's disk/rebase repair hold —
// rather than the umbrella dependency gate. The gate never sets Blocker on a
// child it releases or holds, so a non-zero blocker.ActorWorkflow is an
// authoritative, structured signal that this `blocked` predates (and is
// unrelated to) any umbrella-gated tag the task happens to still carry.
// Checked ahead of the tag in both Awaiting and accumulateChild so a tag that
// reappears through any means (stale client round-trip, future bug) can never
// again cause the gate to mistake a workflow-owned hold for its own and
// re-release a child mid-implementation into a fresh triage cycle (#2538).
func isWorkflowOwnedBlock(t *task.Task) bool {
	return t.Blocker.Actor == blocker.ActorWorkflow
}

// clearGateTagOnHandedOffChildren strips the gating tag from a child that has
// already run an implementation agent.
//
// Once implementation starts, the umbrella gate has handed the child to its
// own workflow: Awaiting excludes it via hasStartedImplementation, so the gate
// will never release it again. But the tag it left behind still makes
// skipTaskCreatedWorkflow refuse the child, and ResumeStalled skips a terminal
// workflow — so nothing owns the task and it sits in todo forever. Measured on
// the server: six children stranded this way since 2026-08-01, none of them
// waiting on an unmet dependency.
//
// Clearing the tag is the narrow fix: it hands the child back to the normal
// dispatcher at exactly the point the gate stopped owning it, and is a no-op
// for children the gate is still legitimately holding or the workflow parked.
func (a *App) clearGateTagOnHandedOffChildren(tasks []task.Task) {
	for i := range tasks {
		t := &tasks[i]
		if t.UmbrellaIssue == "" || !slices.Contains(t.Tags, umbrellaGatedTag) {
			continue
		}
		// todo only. A child parked `blocked` with implementation history was
		// put there deliberately by the workflow (watchdog exhaustion,
		// sybra#2538) and its tag is what marks it as not the gate's to
		// release — clearing it there would re-release work that was stopped
		// on purpose. A gated child sitting in todo has no such owner.
		if t.Status != task.StatusTodo || !hasStartedImplementation(t) {
			continue
		}
		newTags := slices.DeleteFunc(slices.Clone(t.Tags), func(s string) bool {
			return s == umbrellaGatedTag
		})
		if _, err := a.tasks.Update(t.ID, task.Update{Tags: &newTags}); err != nil {
			a.logger.Warn("umbrella.gate.stale-tag-clear", "task_id", t.ID, "err", err)
			continue
		}
		a.logger.Info("umbrella.gate.stale-tag-cleared", "task_id", t.ID, "umbrella", t.UmbrellaIssue)
	}
}

// isRunningChild reports whether a child status occupies a parallelism slot —
// i.e. it has been released and is somewhere in the pipeline but not finished.
func isRunningChild(s task.Status) bool {
	switch s {
	case task.StatusBlocked, task.StatusNew, task.StatusHumanRequired, task.StatusDone, task.StatusCancelled:
		return false
	default:
		return true
	}
}

// releaseCapped releases ready children to `todo`, but no more than each
// umbrella's remaining parallelism budget (cap - active). Strips the gating
// marker on release so a later re-block cannot retrigger it.
func (a *App) releaseCapped(ctx context.Context, ready []string, byID map[string]*task.Task, states map[string]*umbrellaState) {
	for _, id := range ready {
		t, ok := byID[id]
		if !ok || t == nil {
			continue
		}
		st := states[umbrella.NormalizeIssueRef(t.UmbrellaIssue)]
		if st == nil || st.active+st.released >= st.cap {
			continue // at the umbrella's parallelism cap for this tick
		}
		prevTags, prevStatus, prevReason := t.Tags, t.Status, t.StatusReason
		newTags := slices.DeleteFunc(slices.Clone(t.Tags), func(s string) bool {
			return s == umbrellaGatedTag
		})
		result, err := a.tasks.Apply(task.TransitionIntent{
			TaskID:   id,
			ToStatus: task.StatusTodo,
			Actor:    "umbrella.gate.release",
			Extra: task.Update{
				Tags:         &newTags,
				StatusReason: task.Ptr("umbrella dependencies satisfied"),
			},
		})
		if err != nil {
			a.logger.Error("umbrella.release.failed", "task_id", id, "err", err)
			continue
		}
		updated := result.Task
		pushed, err := a.pushReleaseToHomeNode(ctx, updated)
		if err != nil {
			a.logger.Error("umbrella.release.push_failed", "task_id", id, "err", err)
			// The follower never saw the release, so it must not look released
			// here either — restore the pre-release state so ReadyToRelease
			// picks this child up again next tick instead of leaving the
			// leader's board silently diverged from the follower forever.
			if _, rerr := a.tasks.Apply(task.TransitionIntent{
				TaskID:   id,
				ToStatus: prevStatus,
				Actor:    "umbrella.gate.release.rollback",
				Extra: task.Update{
					Tags:         &prevTags,
					StatusReason: task.Ptr(prevReason),
				},
			}); rerr != nil {
				a.logger.Error("umbrella.release.rollback_failed", "task_id", id, "err", rerr)
			}
			continue
		}
		if !pushed {
			// No transport error, but the release did not reach the follower
			// and, unlike a failed push, retrying it would not help — e.g.
			// Assigner declined for confidentiality and has already moved the
			// task to its own terminal blocked state with an operator-facing
			// reason (clusterlead.blockForConfidentiality). Rolling back here
			// would stomp that reason with a generic "gated" state and retry
			// forever against a follower that will never accept the push.
			// Leave whatever state the push path already applied alone and
			// just decline to report a release that never happened.
			a.logger.Warn("umbrella.release.not_pushed", "task_id", id)
			continue
		}
		st.setChildStatus(id, updated.Status)
		st.released++
		a.logger.Info("umbrella.child.released", "task_id", id)
	}
}

// pushReleaseTimeout bounds pushReleaseToHomeNode's remote call well under the
// cluster client's own 30s default so one unreachable follower can delay the
// single-threaded dispatch tick — and every other umbrella's local-only
// releases queued behind it in the same releaseCapped loop — by at most this
// long rather than up to 30s per stuck release.
const pushReleaseTimeout = 5 * time.Second

const blockedTrackerChildrenCompleteReason = "children complete, tracker blocked — needs release"

// pushReleaseToHomeNode forwards a just-released child's new state to its
// home follower when the task isn't homed locally. The local a.tasks.Update
// above only ever touches this leader's own canonical copy; Mirror only pulls
// follower state up to the leader; and the assigner's normal Route/Tick push
// a task to its follower once, at first assignment, then never again. Without
// this forward, a follower-homed task released by this leader's umbrella gate
// would carry the release in the leader's mirror forever while the follower —
// the node that actually dispatches it — stays stuck on its pre-release copy.
// Released children are always still-gated/todo, so pushing them verbatim can
// never roll back a follower's in-progress execution state (see
// Assigner.PushUpdate).
//
// Returns pushed=true, err=nil when the task is homed locally (nothing to
// push), the assigner/config aren't wired up (test doubles), or the push
// reached the follower — PushUpdate's own routed/pushed return is the
// authority on that even when it also reports an error: AssignTask lands
// before PushUpdate's trailing local bookkeeping write (re-stamping
// AssignedNode, already-correct here), so a failure in that last step must
// not be read as "the follower never got it" — the caller must not roll back
// a release the follower already holds, or leader and follower go split
// brain (leader thinks gated, follower thinks released).
//
// Returns pushed=false, err=nil when PushUpdate declined to push without any
// transport error — currently only the confidentiality gate
// (clusterlead.blockForConfidentiality), which already moves the task to its
// own terminal blocked state — so the caller must not treat that as either a
// release or a transient failure to retry.
//
// Returns pushed=false, err!=nil only when the push itself never reached the
// follower (network/timeout/remote error); the caller should roll back and
// let the next tick retry.
func (a *App) pushReleaseToHomeNode(ctx context.Context, t task.Task) (pushed bool, err error) {
	if a.assigner == nil || a.cfg == nil {
		return true, nil
	}
	home := a.cfg.HomeNodeForTask(t.ProjectID, t.NodeOverride)
	if home.Local {
		return true, nil
	}
	pushCtx, cancel := context.WithTimeout(ctx, pushReleaseTimeout)
	defer cancel()
	ok, err := a.assigner.PushUpdate(pushCtx, t)
	if ok {
		if err != nil {
			a.logger.Warn("umbrella.release.push_bookkeeping_failed", "task_id", t.ID, "node", home.Name, "err", err)
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("push release to %q: %w", home.Name, err)
	}
	return false, nil
}

func (st *umbrellaState) setChildStatus(id string, status task.Status) {
	for i := range st.children {
		if st.children[i].id == id {
			st.children[i].status = status
			return
		}
	}
}

// rollupTrackers advances each umbrella tracker's status to reflect its
// children and closes the umbrella issue when everything is done.
func (a *App) rollupTrackers(states map[string]*umbrellaState, cyclic map[string]bool) {
	for key, st := range states {
		if st.tracker == nil {
			continue
		}
		// A tracker is "settled" once it has outlived the creation window, so a
		// childless tally that just reflects children still being materialized
		// is not mistaken for a completed umbrella. A zero CreatedAt (e.g. a
		// task file missing created_at) is treated as not settled rather than
		// infinitely old, so it never bypasses the guard.
		settled := !st.tracker.CreatedAt.IsZero() &&
			time.Since(st.tracker.CreatedAt) > umbrellaSettleDelay
		if st.tracker.Status == task.StatusBlocked {
			// A blocked tracker can be owned by an operator or another workflow
			// path. Preserve that ownership while work remains, but once every
			// materialized child is done stamp an explicit, actionable reason
			// instead of leaving an invisible permanent dead end. Do not close
			// the umbrella or override the blocked status: release remains an
			// operator decision.
			desired, _, doClose := trackerRollup(st, cyclic[key], settled)
			if desired == task.StatusDone && doClose &&
				st.tracker.StatusReason != blockedTrackerChildrenCompleteReason {
				if _, err := a.tasks.Update(st.tracker.ID, task.Update{
					StatusReason: task.Ptr(blockedTrackerChildrenCompleteReason),
				}); err != nil {
					a.logger.Error("umbrella.tracker.blocked_complete.update.failed", "task_id", st.tracker.ID, "err", err)
				}
			}
			continue
		}
		desired, reason, doClose := trackerRollup(st, cyclic[key], settled)
		if body := umbrellaTrackerBody(st.tracker.Body, st.children); body != st.tracker.Body {
			if _, err := a.tasks.Update(st.tracker.ID, task.Update{
				Body: task.Ptr(body),
			}); err != nil {
				a.logger.Error("umbrella.tracker.progress.update.failed", "task_id", st.tracker.ID, "err", err)
				continue
			}
		}
		if desired == st.tracker.Status {
			continue
		}
		// Close the umbrella issue BEFORE flipping the tracker to done, so a
		// transient close failure is retried next tick rather than leaving the
		// issue open under a done tracker that never re-attempts the close.
		if doClose && a.closeUmbrellaIssue(st.tracker.Issue) {
			continue
		}
		if _, err := a.tasks.Apply(task.TransitionIntent{
			TaskID:   st.tracker.ID,
			ToStatus: desired,
			Actor:    "umbrella.gate.tracker_rollup",
			Extra: task.Update{
				StatusReason: task.Ptr(reason),
			},
		}); err != nil {
			a.logger.Error("umbrella.tracker.update.failed", "task_id", st.tracker.ID, "err", err)
			continue
		}
		a.logger.Info("umbrella.tracker.rollup", "task_id", st.tracker.ID, "status", desired)
	}
}

func umbrellaTrackerBody(body string, children []umbrellaProgressChild) string {
	block := renderUmbrellaProgressBlock(children)
	start := strings.Index(body, umbrellaProgressStart)
	if start >= 0 {
		searchFrom := start + len(umbrellaProgressStart)
		if relEnd := strings.Index(body[searchFrom:], umbrellaProgressEnd); relEnd >= 0 {
			end := searchFrom + relEnd + len(umbrellaProgressEnd)
			return body[:start] + block + body[end:]
		}
		return body[:start] + block
	}
	if strings.TrimSpace(body) == "" {
		return block
	}
	sep := "\n\n"
	if strings.HasSuffix(body, "\n\n") {
		sep = ""
	} else if strings.HasSuffix(body, "\n") {
		sep = "\n"
	}
	return body + sep + block
}

func renderUmbrellaProgressBlock(children []umbrellaProgressChild) string {
	children = slices.Clone(children)
	slices.SortFunc(children, func(a, b umbrellaProgressChild) int {
		aIssue := umbrella.NormalizeIssueRef(a.issue)
		bIssue := umbrella.NormalizeIssueRef(b.issue)
		if aIssue != bIssue {
			return strings.Compare(aIssue, bIssue)
		}
		return strings.Compare(a.title, b.title)
	})

	var b strings.Builder
	b.WriteString(umbrellaProgressStart)
	b.WriteString("\n## Subissues\n\n")
	if len(children) == 0 {
		b.WriteString("_No materialized subissues._\n")
	} else {
		for _, child := range children {
			box := " "
			if child.status == task.StatusDone {
				box = "x"
			}
			fmt.Fprintf(&b, "- [%s] %s%s — %s\n",
				box,
				strings.ReplaceAll(child.title, "\n", " "),
				umbrellaProgressIssueSuffix(child.issue),
				child.status,
			)
		}
	}
	b.WriteString(umbrellaProgressEnd)
	return b.String()
}

func umbrellaProgressIssueSuffix(ref string) string {
	_, n, ok := umbrella.ParseRef(ref)
	if ok {
		return fmt.Sprintf(" (#%d)", n)
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	return " (" + ref + ")"
}

// resolveCancellations classifies every cancelled child in children against
// its siblings. An umbrella-expansion race can materialize the same sub-issue
// as two tasks; cleanup cancels the loser in favor of its live twin (see
// releaseUnblockedChildren's dedup path and #2294's post-mortem). That
// cancellation is not an abandonment — the issue is still covered — and must
// not permanently gate the umbrella to human-required. Only a cancellation
// with no live (non-cancelled) sibling for its issue is a genuine deliberate
// abandonment worth surfacing (dependents on that issue would otherwise stall
// forever with no escalation; see depsSatisfied): unresolved reports whether
// any such abandonment exists. resolved counts the opposite case — a
// cancelled duplicate whose issue is covered by a live sibling — which the
// caller must exclude from a total/doneCount completion check, or an umbrella
// with a resolved duplicate could never satisfy doneCount == total (a
// cancelled task never becomes done) and would sit at in-progress forever
// with no signal at all once it stops being surfaced as human-required.
func resolveCancellations(children []umbrellaProgressChild) (unresolved bool, resolved int) {
	liveIssue := make(map[string]bool, len(children))
	for _, c := range children {
		if c.status == task.StatusCancelled {
			continue
		}
		if key := umbrella.NormalizeIssueRef(c.issue); key != "" {
			liveIssue[key] = true
		}
	}
	for _, c := range children {
		if c.status != task.StatusCancelled {
			continue
		}
		key := umbrella.NormalizeIssueRef(c.issue)
		if key == "" || !liveIssue[key] {
			unresolved = true
			continue
		}
		resolved++
	}
	return unresolved, resolved
}

// trackerRollup decides an umbrella tracker's status from its children. A
// cycle, a stuck (human-required) child, a non-gated blocked child, or an
// unresolved cancellation (see resolveCancellations) surfaces as
// human-required (halting only that chain); all-done closes the umbrella. A
// resolved cancellation (a cancelled duplicate whose issue has a live
// sibling) counts against neither total nor doneCount, so it can never block
// completion the way a cancelled child that never reaches Done otherwise
// would. A tracker with no children (every sub-issue was already closed at
// expansion) is vacuously complete, but only once `settled` (so a tracker
// observed while its children are still being materialized is not closed
// prematurely) and only when expansion isn't currently failing — a tracker
// carrying umbrella.ExpandFailTagPrefix (see
// internal/umbrella.recordExpandFailure) never had a chance to materialize
// its children in the first place, and closing it would silently drop the
// umbrella issue while sub-issues remain open on GitHub (#1570).
func trackerRollup(st *umbrellaState, cyclic, settled bool) (status task.Status, reason string, doClose bool) {
	expandFailing := st.tracker != nil && umbrella.ParseExpandFailCount(st.tracker.Tags) > 0
	expandActive := st.tracker != nil && umbrella.HasActiveExpandPhase(st.tracker.Tags)
	unresolvedCancellation, resolvedCancellations := resolveCancellations(st.children)
	effectiveTotal := st.total - resolvedCancellations
	switch {
	case cyclic:
		return task.StatusHumanRequired, "umbrella dependency cycle detected", false
	case st.anyHR:
		return task.StatusHumanRequired, "umbrella child needs attention", false
	case st.anyBlocked:
		return task.StatusHumanRequired, "umbrella child is blocked", false
	case unresolvedCancellation:
		return task.StatusHumanRequired, "umbrella child was cancelled", false
	case expandFailing:
		// Defer entirely to internal/umbrella.recordExpandFailure, which owns
		// this tracker's status/reason while expansion keeps failing. Rollup
		// must not overwrite that state just because the tracker already has
		// children or all currently-materialized children happen to be done.
		return st.tracker.Status, st.tracker.StatusReason, false
	case effectiveTotal > 0 && st.doneCount == effectiveTotal:
		return task.StatusDone, "all umbrella children complete", true
	case effectiveTotal == 0 && settled && !expandActive:
		return task.StatusDone, "umbrella has no open sub-issues", true
	default:
		return task.StatusInProgress, "umbrella in progress", false
	}
}

// closeUmbrellaIssue closes the umbrella's GitHub issue with a generic comment
// (no work content). It returns retry=true on any close failure so the caller
// holds off flipping the tracker to done and tries again next tick (gh issue
// close is idempotent, so re-attempts are safe); a persistently failing close
// leaves the tracker in-progress for the operator to notice. An unparseable
// ref is permanent (retry=false): the tracker still completes, the issue is
// just left for manual closing.
func (a *App) closeUmbrellaIssue(umbRef string) (retry bool) {
	repo, number, ok := umbrella.ParseRef(umbRef)
	if !ok {
		a.logger.Warn("umbrella.close.skip", "reason", "unparseable ref", "ref", umbRef)
		return false
	}
	closeFn := a.umbrellaCloseIssue
	if closeFn == nil {
		closeFn = github.CloseIssue
	}
	if err := closeFn(repo, number, "All umbrella sub-tasks completed."); err != nil {
		a.logger.Error("umbrella.close.failed", "repo", repo, "number", number, "err", err)
		return true
	}
	a.logger.Info("umbrella.closed", "repo", repo, "number", number)
	return false
}

// buildGroundLister returns a TrackedFilesFunc backed by projStore's existing
// bare clones: it resolves repo -> registered project -> clone's default
// branch -> tracked files at that branch, with no network fetch. Any
// resolution failure (unregistered repo, unreadable clone) is returned to the
// caller so grounding fails open rather than silently skipping.
func buildGroundLister(projStore *project.Store) umbrella.TrackedFilesFunc {
	return func(ctx context.Context, repo string) ([]string, error) {
		p, err := projStore.Get(repo)
		if err != nil {
			return nil, fmt.Errorf("ground: project %s: %w", repo, err)
		}
		files, err := project.TrackedFilesAtDefaultBranch(ctx, p.ClonePath)
		if err != nil {
			return nil, fmt.Errorf("ground: tracked files for %s: %w", repo, err)
		}
		return files, nil
	}
}
