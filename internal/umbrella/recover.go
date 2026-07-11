package umbrella

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/scrub"
	"github.com/Automaat/sybra/internal/task"
)

// RecoveryOutcome classifies how a RecoverDegraded run concluded.
type RecoveryOutcome string

const (
	// RecoverySkipped means no planner call was made: the tracker was not
	// eligible (no tracker, not degraded, cooling down, or already exhausted).
	RecoverySkipped RecoveryOutcome = "skipped"
	// RecoverySafetyRefused means the tracker's existing children were found
	// in an unsafe state (duplicate or stale issue refs) before any planner
	// call, so recovery refused to touch them.
	RecoverySafetyRefused RecoveryOutcome = "safety_refused"
	// RecoveryFailed means the planner call, plan validation, or a task write
	// failed. The tracker keeps FallbackTag and durable retry state advances.
	RecoveryFailed RecoveryOutcome = "failed"
	// RecoveryRecovered means the tracker was successfully re-planned and
	// FallbackTag was removed.
	RecoveryRecovered RecoveryOutcome = "recovered"
)

// RecoveryResult summarizes one RecoverDegraded run.
type RecoveryResult struct {
	UmbrellaURL     string
	Outcome         RecoveryOutcome
	Reason          string
	ChildrenCreated int
	ChildrenUpdated int
	FailCount       int  // recovery failure count after this attempt (0 on skip/success)
	Exhausted       bool // true when this attempt pushed the tracker to RecoverExhaustedTag
}

// recoveryFailAfter is a test seam (recover_test.go only): when non-empty and
// matching a checkpoint name reached during a RecoverDegraded run, an
// injected failure is returned immediately after that checkpoint's mutation
// has been durably applied — letting tests prove a rerun converges from a
// partial-write state instead of duplicating tags or children. Empty by
// default (no injection). Never set outside tests; RecoverDegraded's callers
// run sequentially per umbrella under lockExpandIssue, so this single mutable
// var is safe as long as tests using it do not run in parallel with each
// other.
var recoveryFailAfter string

const (
	checkpointChildrenCreated  = "children_created"
	checkpointDepsUpdated      = "deps_updated"
	checkpointMaxParallel      = "maxparallel_replaced"
	checkpointBeforeFinalClear = "before_fallback_clear"
)

var errInjectedFailure = errors.New("injected test failure")

func injectedFailure(checkpoint string) error {
	if recoveryFailAfter == checkpoint {
		return fmt.Errorf("%s: %w", checkpoint, errInjectedFailure)
	}
	return nil
}

// RecoverDegraded re-plans a degraded umbrella tracker (one carrying
// FallbackTag): it fetches the umbrella's current sub-issues, re-runs the
// planner via Generate, and — on a valid, non-fallback plan — creates any
// still-missing open children, wholesale-replaces the DependsOn of every
// gate-mutable existing child with the recovered canonical dependencies,
// replaces the tracker's umbrella-max-parallel tag, and finally drops
// FallbackTag. It deliberately does not call Expand: Expand short-circuits
// once every open sub-issue already has a task, which is always true for an
// already-degraded (already fully-materialized) tracker — recovery instead
// always re-plans and rewrites dependencies.
//
// Ineligible trackers (missing, not degraded, cooling down, or exhausted) are
// reported as RecoverySkipped without a planner call. A planner error,
// fallback plan, invalid DAG, or write failure keeps FallbackTag and records
// durable retry state (RecoverFailTagPrefix / RecoverAfterTagPrefix /
// RecoverExhaustedTag at RecoverFailThreshold) via recordRecoveryFailure.
// Every mutation step is idempotent so a rerun after a partial failure
// converges instead of duplicating tags or children.
func RecoverDegraded(ctx context.Context, tasks *task.Manager, run Runner, umbrellaURL string, opts ...ExpandOption) (RecoveryResult, error) {
	var cfg expandConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	repo, number, ok := ParseRef(umbrellaURL)
	if !ok {
		return RecoveryResult{}, fmt.Errorf("not a GitHub issue URL: %s", umbrellaURL)
	}
	unlock, err := lockExpandIssue(tasks, umbrellaURL)
	if err != nil {
		return RecoveryResult{}, err
	}
	defer func() {
		if err := unlock(); err != nil {
			slog.Warn("umbrella.recover.unlock_failed", "issue", umbrellaURL, "err", err)
		}
	}()

	umb, subs, err := fetchUmbrellaBounded(ctx, repo, number)
	if err != nil {
		return RecoveryResult{}, fmt.Errorf("fetch umbrella: %w", err)
	}

	state, err := scanUmbrellaState(tasks, umb.URL)
	if err != nil {
		return RecoveryResult{}, fmt.Errorf("scan umbrella state: %w", err)
	}
	if skip, eligible := recoveryEligibility(state.tracker, time.Now()); !eligible {
		skip.UmbrellaURL = umb.URL
		return skip, nil
	}

	planSubs, byRef := buildPlanIndex(subs)

	if err := detectUnsafeChildren(state.children, byRef); err != nil {
		res, recErr := recordRecoveryFailure(tasks, state.tracker.id, RecoverySafetyRefused, err)
		res.UmbrellaURL = umb.URL
		return res, recErr
	}

	var genOpts []GenerateOption
	if cfg.lister != nil {
		genOpts = append(genOpts, WithGrounder(cfg.lister, cfg.minSubs))
	}

	pctx, cancel := context.WithTimeout(ctx, plannerTimeout(len(subs)))
	defer cancel()
	plan, err := Generate(pctx, run, umb.URL, umb.Body, planSubs, genOpts...)
	if err != nil {
		res, recErr := recordRecoveryFailure(tasks, state.tracker.id, RecoveryFailed, fmt.Errorf("plan umbrella: %w", err))
		res.UmbrellaURL = umb.URL
		return res, recErr
	}
	if plan.Fallback {
		res, recErr := recordRecoveryFailure(tasks, state.tracker.id, RecoveryFailed, errors.New("planner produced a fallback plan"))
		res.UmbrellaURL = umb.URL
		return res, recErr
	}
	// Defensive re-validation: Generate already ran this exact check
	// internally, but recovery treats it as a hard mutation gate rather than
	// trusting Generate's internal contract to never change out from under it.
	if err := plan.validate(planSubs); err != nil {
		res, recErr := recordRecoveryFailure(tasks, state.tracker.id, RecoveryFailed, fmt.Errorf("invalid recovered plan: %w", err))
		res.UmbrellaURL = umb.URL
		return res, recErr
	}

	state, err = scanUmbrellaState(tasks, umb.URL)
	if err != nil {
		return RecoveryResult{}, fmt.Errorf("rescan umbrella state before mutation: %w", err)
	}
	if skip, eligible := recoveryEligibility(state.tracker, time.Now()); !eligible {
		skip.UmbrellaURL = umb.URL
		return skip, nil
	}

	res, err := applyRecoveredPlan(tasks, umb, plan, planSubs, byRef, state)
	res.UmbrellaURL = umb.URL
	return res, err
}

// recoveryEligibility reports whether tracker qualifies for a recovery
// attempt at instant now, and — when it does not — the RecoverySkipped
// result to return (UmbrellaURL is filled in by the caller).
func recoveryEligibility(tracker existingTracker, now time.Time) (skip RecoveryResult, eligible bool) {
	switch {
	case !tracker.exists:
		return RecoveryResult{Outcome: RecoverySkipped, Reason: "no umbrella tracker"}, false
	case !slices.Contains(tracker.tags, FallbackTag):
		return RecoveryResult{Outcome: RecoverySkipped, Reason: "tracker is not degraded"}, false
	case !trackerStatusRecoveryEligible(tracker):
		return RecoveryResult{Outcome: RecoverySkipped, Reason: "tracker status is not recoverable"}, false
	case HasRecoverExhaustedTag(tracker.tags):
		return RecoveryResult{Outcome: RecoverySkipped, Reason: "recovery exhausted"}, false
	case !RecoverDue(tracker.tags, now):
		return RecoveryResult{Outcome: RecoverySkipped, Reason: "recovery cooling down"}, false
	default:
		return RecoveryResult{}, true
	}
}

func trackerStatusRecoveryEligible(tracker existingTracker) bool {
	switch tracker.status {
	case task.StatusDone, task.StatusCancelled:
		return false
	case task.StatusHumanRequired, task.StatusBlocked:
		return tracker.statusReason == "" || strings.HasPrefix(tracker.statusReason, RecoveryFailureReasonPrefix)
	default:
		return true
	}
}

// buildPlanIndex projects fetched GitHub sub-issues into the planner's
// SubIssue shape plus a normalized-ref lookup, mirroring Expand's own
// indexing so both entry points feed Generate identically.
func buildPlanIndex(subs []github.Issue) (planSubs []SubIssue, byRef map[string]github.Issue) {
	planSubs = make([]SubIssue, len(subs))
	byRef = make(map[string]github.Issue, len(subs))
	for i := range subs {
		planSubs[i] = SubIssue{
			Ref:    subs[i].URL,
			Title:  subs[i].Title,
			Body:   subs[i].Body,
			Closed: strings.EqualFold(subs[i].State, "CLOSED"),
		}
		byRef[NormalizeIssueRef(subs[i].URL)] = subs[i]
	}
	return planSubs, byRef
}

// applyRecoveredPlan runs the mutation phase of RecoverDegraded against an
// already-validated, non-fallback plan: materialize missing children,
// rewrite existing gate-mutable children's dependencies, replace the
// tracker's max-parallel tag, clear recovery retry state, and finally remove
// FallbackTag. Each step is checkpointed via injectedFailure (a test seam)
// and, on any failure, durably recorded via recordRecoveryFailure before
// returning — the caller only needs to stamp UmbrellaURL onto the result.
func applyRecoveredPlan(tasks *task.Manager, umb github.Issue, plan Plan, planSubs []SubIssue, byRef map[string]github.Issue, state umbrellaScanState) (RecoveryResult, error) {
	trackerID := state.tracker.id
	fail := func(cause error) (RecoveryResult, error) {
		return recordRecoveryFailure(tasks, trackerID, RecoveryFailed, cause)
	}

	specs := ChildSpecs(plan, planSubs, state.refs)
	created, err := createChildren(tasks, umb, specs, byRef)
	if err != nil {
		res, recErr := fail(fmt.Errorf("create missing children: %w", err))
		res.ChildrenCreated = created
		return res, recErr
	}
	if err := injectedFailure(checkpointChildrenCreated); err != nil {
		res, recErr := fail(err)
		res.ChildrenCreated = created
		return res, recErr
	}

	updated, err := applyChildDeps(tasks, plan, byRef, state.children)
	if err != nil {
		res, recErr := fail(fmt.Errorf("update child dependencies: %w", err))
		res.ChildrenCreated, res.ChildrenUpdated = created, updated
		return res, recErr
	}
	if err := injectedFailure(checkpointDepsUpdated); err != nil {
		res, recErr := fail(err)
		res.ChildrenCreated, res.ChildrenUpdated = created, updated
		return res, recErr
	}

	if err := replaceMaxParallelTag(tasks, trackerID, plan.MaxParallel); err != nil {
		res, recErr := fail(fmt.Errorf("replace max-parallel tag: %w", err))
		res.ChildrenCreated, res.ChildrenUpdated = created, updated
		return res, recErr
	}
	if err := injectedFailure(checkpointMaxParallel); err != nil {
		res, recErr := fail(err)
		res.ChildrenCreated, res.ChildrenUpdated = created, updated
		return res, recErr
	}

	if err := clearRecoveryState(tasks, trackerID); err != nil {
		res, recErr := fail(fmt.Errorf("clear recovery state: %w", err))
		res.ChildrenCreated, res.ChildrenUpdated = created, updated
		return res, recErr
	}
	if err := injectedFailure(checkpointBeforeFinalClear); err != nil {
		res, recErr := fail(err)
		res.ChildrenCreated, res.ChildrenUpdated = created, updated
		return res, recErr
	}
	// FallbackTag is removed last: every prior step is safe to observe with
	// FallbackTag still present (a rerun would simply re-recover), whereas
	// removing it first would make a crash mid-mutation invisible to both the
	// eligibility scan and a human scanning the board.
	if err := removeFallbackTag(tasks, trackerID); err != nil {
		res, recErr := fail(fmt.Errorf("remove fallback tag: %w", err))
		res.ChildrenCreated, res.ChildrenUpdated = created, updated
		return res, recErr
	}

	return RecoveryResult{Outcome: RecoveryRecovered, ChildrenCreated: created, ChildrenUpdated: updated}, nil
}

// umbrellaState is the recovery-focused counterpart to scanExisting: it also
// returns the umbrella's existing child tasks (not just a boolean-exists
// map), which RecoverDegraded needs to detect unsafe duplicate/stale refs and
// to rewrite existing children's dependencies in place.
type umbrellaScanState struct {
	tracker  existingTracker
	children []task.Task
	refs     map[string]bool // normalized issue ref -> a task already references it (mirrors scanExisting)
}

func scanUmbrellaState(tasks *task.Manager, umbrellaURL string) (umbrellaScanState, error) {
	all, err := tasks.List()
	if err != nil {
		return umbrellaScanState{}, err
	}
	umbKey := NormalizeIssueRef(umbrellaURL)
	state := umbrellaScanState{refs: make(map[string]bool, len(all))}
	for i := range all {
		t := &all[i]
		if t.Issue != "" {
			state.refs[NormalizeIssueRef(t.Issue)] = true
		}
		if t.TaskType == task.TaskTypeUmbrella && NormalizeIssueRef(t.Issue) == umbKey {
			state.tracker = existingTracker{exists: true, id: t.ID, tags: t.Tags, status: t.Status, statusReason: t.StatusReason}
		}
		if t.UmbrellaIssue != "" && NormalizeIssueRef(t.UmbrellaIssue) == umbKey {
			state.children = append(state.children, *t)
		}
	}
	return state, nil
}

// detectUnsafeChildren refuses to recover an umbrella whose existing children
// are in a state RecoverDegraded cannot safely reconcile: two children
// claiming the same sub-issue ref (which one is canonical?), or a child
// referencing a ref no longer among the umbrella's fetched sub-issues (the
// umbrella's sub-issue set changed underneath a materialized child).
func detectUnsafeChildren(children []task.Task, byRef map[string]github.Issue) error {
	seen := map[string]int{}
	var dup, stale []string
	for i := range children {
		key := NormalizeIssueRef(children[i].Issue)
		seen[key]++
		if seen[key] == 2 {
			dup = append(dup, key)
		}
		if _, ok := byRef[key]; !ok {
			stale = append(stale, key)
		}
	}
	if len(dup) > 0 || len(stale) > 0 {
		var parts []string
		if len(dup) > 0 {
			parts = append(parts, fmt.Sprintf("duplicate child refs: %s", strings.Join(dup, ", ")))
		}
		if len(stale) > 0 {
			parts = append(parts, fmt.Sprintf("stale child refs: %s", strings.Join(stale, ", ")))
		}
		return errors.New(strings.Join(parts, "; "))
	}
	return nil
}

// childMutable reports whether a child task is currently held by the
// umbrella gate (gate-marked todo, or the legacy gated-blocked shape) and so
// is safe for recovery to rewrite. Any other status — released/active,
// human-required, done, cancelled, or blocked without the gating tag — is
// "frozen": recovery must not touch its dependencies, since the gate no
// longer consults DependsOn for it (or a human/other automation now owns it).
func childMutable(t task.Task) bool {
	return (t.Status == task.StatusTodo || t.Status == task.StatusBlocked) && slices.Contains(t.Tags, GatedTag)
}

// canonicalChildDeps computes, for every non-closed sub-issue the plan
// covers, its canonical DependsOn list: closed-sub dependencies dropped
// (already satisfied, and never materialize into a task — mirrors
// ChildSpecs), remaining refs rewritten to their canonical issue URL.
func canonicalChildDeps(plan Plan, byRef map[string]github.Issue) map[string][]string {
	closed := make(map[string]bool, len(byRef))
	for key := range byRef {
		if strings.EqualFold(byRef[key].State, "CLOSED") {
			closed[key] = true
		}
	}
	out := make(map[string][]string, len(plan.Children))
	for i := range plan.Children {
		c := &plan.Children[i]
		key := NormalizeIssueRef(c.Ref)
		if closed[key] {
			continue
		}
		deps := make([]string, 0, len(c.DependsOn))
		for _, d := range c.DependsOn {
			if closed[NormalizeIssueRef(d)] {
				continue
			}
			deps = append(deps, d)
		}
		out[key] = canonicalizeDeps(deps, byRef)
	}
	return out
}

// applyChildDeps wholesale-replaces every gate-mutable existing child's
// DependsOn with the plan's canonical dependencies for its ref. Newly created
// children (via createChildren, above) already got the same canonical
// dependencies at creation time, so only pre-existing children need rewriting
// here. Revalidates mutability per child inside UpdateFn (not from the
// pre-fetched snapshot) so a child the gate released between the scan and
// this write is correctly skipped instead of having its dependencies
// clobbered out from under an in-flight run.
func applyChildDeps(tasks *task.Manager, plan Plan, byRef map[string]github.Issue, children []task.Task) (int, error) {
	canon := canonicalChildDeps(plan, byRef)
	updated := 0
	for i := range children {
		childID := children[i].ID
		want, ok := canon[NormalizeIssueRef(children[i].Issue)]
		if !ok {
			continue // closed sub-issue, or not covered by this plan — nothing to reconcile
		}
		_, err := tasks.UpdateFn(childID, func(cur task.Task) (task.Update, error) {
			if !childMutable(cur) {
				return task.Update{}, errSkipUpdate
			}
			if slices.Equal(cur.DependsOn, want) {
				return task.Update{}, errSkipUpdate
			}
			return task.Update{DependsOn: task.Ptr(want)}, nil
		})
		if err != nil {
			if errors.Is(err, errSkipUpdate) {
				continue
			}
			return updated, fmt.Errorf("update child %s dependencies: %w", childID, err)
		}
		updated++
	}
	return updated, nil
}

// hasSingleTag reports whether tags carries exactly one entry sharing prefix,
// and that entry equals want exactly — i.e. the tag set is already in its
// canonical single-tag form, so a caller can skip a no-op rewrite.
func hasSingleTag(tags []string, prefix, want string) bool {
	count := 0
	for _, t := range tags {
		if strings.HasPrefix(t, prefix) {
			if t != want {
				return false
			}
			count++
		}
	}
	return count == 1
}

// replaceMaxParallelTag collapses every umbrella-max-parallel:* tag on the
// tracker down to exactly MaxParallelTag(n), idempotently.
func replaceMaxParallelTag(tasks *task.Manager, trackerID string, n int) error {
	want := MaxParallelTag(n)
	_, err := tasks.UpdateFn(trackerID, func(cur task.Task) (task.Update, error) {
		if hasSingleTag(cur.Tags, MaxParallelTagPrefix, want) {
			return task.Update{}, errSkipUpdate
		}
		return task.Update{Tags: task.Ptr(ReplaceTagPrefix(cur.Tags, MaxParallelTagPrefix, want))}, nil
	})
	if err != nil && !errors.Is(err, errSkipUpdate) {
		return err
	}
	return nil
}

// clearRecoveryState strips every umbrella-recover-* tag from the tracker and
// blanks StatusReason only when it is owned by the recovery-failure path
// (RecoveryFailureReasonPrefix) — never a reason some other automation set.
// Idempotent: a no-op when the tracker already carries no recovery tags and
// no recovery-owned reason.
func clearRecoveryState(tasks *task.Manager, trackerID string) error {
	_, err := tasks.UpdateFn(trackerID, func(cur task.Task) (task.Update, error) {
		hasRecoveryTags := slices.ContainsFunc(cur.Tags, isRecoveryTag)
		ownsReason := strings.HasPrefix(cur.StatusReason, RecoveryFailureReasonPrefix)
		if !hasRecoveryTags && !ownsReason {
			return task.Update{}, errSkipUpdate
		}
		newTags := slices.DeleteFunc(slices.Clone(cur.Tags), isRecoveryTag)
		upd := task.Update{Tags: task.Ptr(newTags)}
		if ownsReason {
			upd.StatusReason = task.Ptr("")
		}
		return upd, nil
	})
	if err != nil && !errors.Is(err, errSkipUpdate) {
		return err
	}
	return nil
}

func isRecoveryTag(t string) bool {
	return strings.HasPrefix(t, RecoverFailTagPrefix) || strings.HasPrefix(t, RecoverAfterTagPrefix) || t == RecoverExhaustedTag
}

// removeFallbackTag drops FallbackTag from the tracker if present.
// Idempotent.
func removeFallbackTag(tasks *task.Manager, trackerID string) error {
	_, err := tasks.UpdateFn(trackerID, func(cur task.Task) (task.Update, error) {
		if !slices.Contains(cur.Tags, FallbackTag) {
			return task.Update{}, errSkipUpdate
		}
		newTags := slices.DeleteFunc(slices.Clone(cur.Tags), func(t string) bool { return t == FallbackTag })
		return task.Update{Tags: task.Ptr(newTags)}, nil
	})
	if err != nil && !errors.Is(err, errSkipUpdate) {
		return err
	}
	return nil
}

// recordRecoveryFailure durably records a failed recovery attempt: bumps the
// consecutive-failure tag, writes the next backoff instant, marks the
// tracker exhausted at RecoverFailThreshold, and sets a recovery-owned
// StatusReason. FallbackTag and any dependency writes already applied before
// the failure are left untouched — a recovery failure never clears existing
// child dependencies.
func recordRecoveryFailure(tasks *task.Manager, trackerID string, outcome RecoveryOutcome, cause error) (RecoveryResult, error) {
	var result RecoveryResult
	_, err := tasks.UpdateFn(trackerID, func(cur task.Task) (task.Update, error) {
		count := ParseRecoverFailCount(cur.Tags) + 1
		newTags := ReplaceTagPrefix(cur.Tags, RecoverFailTagPrefix, RecoverFailTag(count))
		after := time.Now().Add(RecoverBackoff(count))
		newTags = ReplaceTagPrefix(newTags, RecoverAfterTagPrefix, RecoverAfterTag(after))
		exhausted := count >= RecoverFailThreshold
		if exhausted && !slices.Contains(newTags, RecoverExhaustedTag) {
			newTags = append(newTags, RecoverExhaustedTag)
		}
		reason := safeRecoveryFailureReason(cause)
		result = RecoveryResult{Outcome: outcome, Reason: reason, FailCount: count, Exhausted: exhausted}
		return task.Update{
			Tags:         task.Ptr(newTags),
			StatusReason: task.Ptr(formatRecoveryFailureReason(count, reason)),
		}, nil
	})
	if err != nil {
		return RecoveryResult{}, fmt.Errorf("record recovery failure: %w", err)
	}
	return result, nil
}

// SafeRecoveryFailureReason returns a bounded, scrubbed recovery failure reason
// suitable for persisted task metadata or audit data. It deliberately strips
// quoted/delimited payloads because planner/provider errors can echo issue
// content or raw LLM output.
func SafeRecoveryFailureReason(cause error) string {
	return safeRecoveryFailureReason(cause)
}

func safeRecoveryFailureReason(cause error) string {
	if cause == nil {
		return ""
	}
	reason := cause.Error()
	reason, _ = scrub.Scrub(reason, nil)
	reason = stripRecoveryPayloads(reason)
	return truncateUTF8(reason, 160)
}

func stripRecoveryPayloads(reason string) string {
	reason = strings.ReplaceAll(reason, "\r", " ")
	reason = strings.ReplaceAll(reason, "\n", " ")
	reason = strings.Join(strings.Fields(reason), " ")
	reason = stripDelimitedPayloads(reason, '"', '"')
	reason = stripDelimitedPayloads(reason, '\'', '\'')
	reason = stripDelimitedPayloads(reason, '{', '}')
	reason = stripDelimitedPayloads(reason, '[', ']')
	return strings.TrimSpace(reason)
}

func stripDelimitedPayloads(s string, open, closing rune) string {
	var b strings.Builder
	depth := 0
	replaced := false
	for _, r := range s {
		switch {
		case r == open && depth == 0:
			depth = 1
			if !replaced {
				b.WriteString("[redacted]")
				replaced = true
			}
		case r == open && open != closing && depth > 0:
			depth++
		case r == closing && depth > 0:
			depth--
			if depth == 0 {
				replaced = false
			}
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func formatRecoveryFailureReason(count int, reason string) string {
	reason = fmt.Sprintf("%s%d): %s", RecoveryFailureReasonPrefix, count, reason)
	const maxLen = 200
	return truncateUTF8(reason, maxLen)
}

func truncateUTF8(s string, maxLen int) string {
	if maxLen <= 0 || len(s) <= maxLen {
		return s
	}
	const tail = "..."
	if maxLen <= len(tail) {
		return tail[:maxLen]
	}
	cut := maxLen - len(tail)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + tail
}
