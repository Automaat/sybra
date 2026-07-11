package umbrella

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
)

// mkTracker creates a degraded umbrella tracker (FallbackTag present by
// default) carrying maxParallel, plus any extraTags (e.g. recovery state).
func mkTracker(t *testing.T, tasks *task.Manager, umb github.Issue, maxParallel int, extraTags ...string) task.Task {
	t.Helper()
	tags := append([]string{"umbrella", MaxParallelTag(maxParallel), FallbackTag}, extraTags...)
	tk, err := tasks.CreateFull(umb.Title, umb.Body, task.AgentModeHeadless, task.Update{
		Issue:     task.Ptr(umb.URL),
		TaskType:  task.Ptr(task.TaskTypeUmbrella),
		ProjectID: task.Ptr(umb.Repository),
		Status:    task.Ptr(task.StatusInProgress),
		Tags:      task.Ptr(tags),
	})
	if err != nil {
		t.Fatalf("create tracker: %v", err)
	}
	return tk
}

// mkGatedChild creates a gate-mutable child (todo or blocked, carrying
// GatedTag) — the shape RecoverDegraded is allowed to rewrite.
func mkGatedChild(t *testing.T, tasks *task.Manager, umb github.Issue, issue string, deps []string, status task.Status) task.Task {
	t.Helper()
	tk, err := tasks.CreateFull("child "+issue, "", task.AgentModeHeadless, task.Update{
		Issue:         task.Ptr(issue),
		UmbrellaIssue: task.Ptr(umb.URL),
		DependsOn:     task.Ptr(deps),
		Status:        task.Ptr(status),
		Tags:          task.Ptr([]string{GatedTag}),
	})
	if err != nil {
		t.Fatalf("create gated child: %v", err)
	}
	return tk
}

// mkActiveChild creates a released/active child (no GatedTag) — frozen from
// RecoverDegraded's perspective.
func mkActiveChild(t *testing.T, tasks *task.Manager, umb github.Issue, issue string, deps []string) task.Task {
	t.Helper()
	tk, err := tasks.CreateFull("active "+issue, "", task.AgentModeHeadless, task.Update{
		Issue:         task.Ptr(issue),
		UmbrellaIssue: task.Ptr(umb.URL),
		DependsOn:     task.Ptr(deps),
		Status:        task.Ptr(task.StatusInProgress),
	})
	if err != nil {
		t.Fatalf("create active child: %v", err)
	}
	return tk
}

// planJSON renders a minimal valid planner response covering refs with no
// explicit metadata — deriveEdges' serial-default layer still produces a
// real (non-flat) dependency chain from this.
func planJSON(t *testing.T, maxParallel int, refs ...string) string {
	t.Helper()
	children := make([]map[string]any, len(refs))
	for i, r := range refs {
		children[i] = map[string]any{
			"issue":     r,
			"dependsOn": []string{},
			"touches":   []string{},
			"produces":  []string{},
			"requires":  []string{},
		}
	}
	b, err := json.Marshal(map[string]any{"children": children, "maxParallel": maxParallel})
	if err != nil {
		t.Fatalf("marshal plan json: %v", err)
	}
	return string(b)
}

func newCountingRunner(planResponse string) (run Runner, calls func() int) {
	var n int
	run = func(context.Context, string, string) (string, error) {
		n++
		return planResponse, nil
	}
	calls = func() int { return n }
	return run, calls
}

func erroringRunner(cause error) Runner {
	return func(context.Context, string, string) (string, error) {
		return "", cause
	}
}

func invalidRunner() Runner {
	return func(context.Context, string, string) (string, error) {
		return "not json", nil
	}
}

func TestRecoverDegraded_Success(t *testing.T) {
	subs := makeTestIssues(3)
	umb := github.Issue{Title: "umbrella", URL: "https://github.com/o/r/issues/100", Repository: "o/r", Body: "body"}
	restore := githubFetchUmbrellaForTest(t, umb, subs)
	defer restore()
	tasks := newTestTaskManager(t)
	mkTracker(t, tasks, umb, 1)
	mkGatedChild(t, tasks, umb, subs[0].URL, nil, task.StatusTodo)
	mkGatedChild(t, tasks, umb, subs[1].URL, []string{subs[0].URL}, task.StatusBlocked)
	mkGatedChild(t, tasks, umb, subs[2].URL, []string{subs[1].URL}, task.StatusBlocked)
	run, calls := newCountingRunner(planJSON(t, 2, subs[0].URL, subs[1].URL, subs[2].URL))

	res, err := RecoverDegraded(context.Background(), tasks, run, umb.URL)
	if err != nil {
		t.Fatalf("RecoverDegraded: %v", err)
	}
	if res.Outcome != RecoveryRecovered {
		t.Fatalf("Outcome = %q, want %q (reason=%q)", res.Outcome, RecoveryRecovered, res.Reason)
	}
	if calls() != 1 {
		t.Fatalf("planner called %d times, want 1", calls())
	}
	if res.ChildrenCreated != 0 {
		t.Fatalf("ChildrenCreated = %d, want 0 (all already materialized)", res.ChildrenCreated)
	}
	// c3 previously depended only on c2; the recovered plan's serial default
	// makes it depend on every earlier sibling (c1 and c2), so only c3 changes.
	if res.ChildrenUpdated != 1 {
		t.Fatalf("ChildrenUpdated = %d, want 1", res.ChildrenUpdated)
	}

	tracker := mustGetByIssue(t, tasks, umb.URL, task.TaskTypeUmbrella)
	if slices.Contains(tracker.Tags, FallbackTag) {
		t.Fatalf("tracker tags = %v, want FallbackTag removed", tracker.Tags)
	}
	if ParseMaxParallel(tracker.Tags) != 2 || !HasMaxParallelTag(tracker.Tags) {
		t.Fatalf("tracker tags = %v, want MaxParallelTag(2)", tracker.Tags)
	}

	c3 := mustGetByIssue(t, tasks, subs[2].URL, "")
	want := []string{subs[0].URL, subs[1].URL}
	if !slices.Equal(c3.DependsOn, want) {
		t.Fatalf("c3 DependsOn = %v, want %v", c3.DependsOn, want)
	}
}

func TestRecoverDegraded_MissingChildIsCreated(t *testing.T) {
	subs := makeTestIssues(2)
	umb := github.Issue{Title: "umbrella", URL: "https://github.com/o/r/issues/100", Repository: "o/r", Body: "body"}
	restore := githubFetchUmbrellaForTest(t, umb, subs)
	defer restore()
	tasks := newTestTaskManager(t)
	mkTracker(t, tasks, umb, 5)
	mkGatedChild(t, tasks, umb, subs[0].URL, nil, task.StatusTodo)
	// subs[1] never materialized as a child — simulates a partial earlier expansion.
	run, _ := newCountingRunner(planJSON(t, 5, subs[0].URL, subs[1].URL))

	res, err := RecoverDegraded(context.Background(), tasks, run, umb.URL)
	if err != nil {
		t.Fatalf("RecoverDegraded: %v", err)
	}
	if res.Outcome != RecoveryRecovered {
		t.Fatalf("Outcome = %q, want %q (reason=%q)", res.Outcome, RecoveryRecovered, res.Reason)
	}
	if res.ChildrenCreated != 1 {
		t.Fatalf("ChildrenCreated = %d, want 1", res.ChildrenCreated)
	}

	created := mustGetByIssue(t, tasks, subs[1].URL, "")
	if created.UmbrellaIssue != umb.URL || created.Status != task.StatusTodo || !slices.Contains(created.Tags, GatedTag) {
		t.Fatalf("created child = %+v, want gated todo child of the umbrella", created)
	}
}

func TestRecoverDegraded_PlannerErrorRecordsFailure(t *testing.T) {
	subs := makeTestIssues(2)
	umb := github.Issue{Title: "umbrella", URL: "https://github.com/o/r/issues/100", Repository: "o/r", Body: "body"}
	restore := githubFetchUmbrellaForTest(t, umb, subs)
	defer restore()
	tasks := newTestTaskManager(t)
	mkTracker(t, tasks, umb, 5)
	mkGatedChild(t, tasks, umb, subs[0].URL, nil, task.StatusTodo)
	mkGatedChild(t, tasks, umb, subs[1].URL, []string{subs[0].URL}, task.StatusBlocked)
	run := erroringRunner(errors.New("provider unavailable"))

	res, err := RecoverDegraded(context.Background(), tasks, run, umb.URL)
	if err != nil {
		t.Fatalf("RecoverDegraded: %v", err)
	}
	if res.Outcome != RecoveryFailed || res.FailCount != 1 || res.Exhausted {
		t.Fatalf("res = %+v, want Failed/FailCount=1/Exhausted=false", res)
	}

	tracker := mustGetByIssue(t, tasks, umb.URL, task.TaskTypeUmbrella)
	if !slices.Contains(tracker.Tags, FallbackTag) {
		t.Fatalf("tracker tags = %v, want FallbackTag kept on failure", tracker.Tags)
	}
	if ParseRecoverFailCount(tracker.Tags) != 1 {
		t.Fatalf("recover fail count = %d, want 1: %v", ParseRecoverFailCount(tracker.Tags), tracker.Tags)
	}
	if !strings.Contains(tracker.StatusReason, "provider unavailable") {
		t.Fatalf("StatusReason = %q, want to mention the cause", tracker.StatusReason)
	}
}

// TestRecoverDegraded_ConcurrentOperatorReasonNotClobberedOnFailure exercises
// the race recordRecoveryFailure guards against: an operator (or another
// automation) moves the tracker to blocked with an unrelated reason while
// the planner call is in flight. The runner stub mutates the tracker
// mid-call to simulate that window; the failure path must leave the
// operator's reason alone rather than overwriting it with a recovery-owned
// one.
func TestRecoverDegraded_ConcurrentOperatorReasonNotClobberedOnFailure(t *testing.T) {
	subs := makeTestIssues(2)
	umb := github.Issue{Title: "umbrella", URL: "https://github.com/o/r/issues/100", Repository: "o/r", Body: "body"}
	restore := githubFetchUmbrellaForTest(t, umb, subs)
	defer restore()
	tasks := newTestTaskManager(t)
	tracker := mkTracker(t, tasks, umb, 5)
	mkGatedChild(t, tasks, umb, subs[0].URL, nil, task.StatusTodo)
	mkGatedChild(t, tasks, umb, subs[1].URL, []string{subs[0].URL}, task.StatusBlocked)

	const operatorReason = "blocked: waiting on an unrelated dependency"
	run := func(context.Context, string, string) (string, error) {
		if _, err := tasks.UpdateFn(tracker.ID, func(cur task.Task) (task.Update, error) {
			return task.Update{
				Status:       task.Ptr(task.StatusBlocked),
				StatusReason: task.Ptr(operatorReason),
			}, nil
		}); err != nil {
			t.Fatalf("simulate concurrent operator edit: %v", err)
		}
		return "", errors.New("provider unavailable")
	}

	res, err := RecoverDegraded(context.Background(), tasks, run, umb.URL)
	if err != nil {
		t.Fatalf("RecoverDegraded: %v", err)
	}
	if res.Outcome != RecoveryFailed {
		t.Fatalf("res = %+v, want Failed", res)
	}

	got := mustGetByIssue(t, tasks, umb.URL, task.TaskTypeUmbrella)
	if got.StatusReason != operatorReason {
		t.Fatalf("StatusReason = %q, want operator's reason %q left untouched", got.StatusReason, operatorReason)
	}
	if ParseRecoverFailCount(got.Tags) != 1 {
		t.Fatalf("recover fail count = %d, want 1 (retry bookkeeping still advances): %v", ParseRecoverFailCount(got.Tags), got.Tags)
	}
}

func TestRecoverDegraded_PlannerErrorReasonIsSafeAndUTF8(t *testing.T) {
	subs := makeTestIssues(2)
	umb := github.Issue{Title: "umbrella", URL: "https://github.com/o/r/issues/100", Repository: "o/r", Body: "body"}
	restore := githubFetchUmbrellaForTest(t, umb, subs)
	defer restore()
	tasks := newTestTaskManager(t)
	mkTracker(t, tasks, umb, 5)
	mkGatedChild(t, tasks, umb, subs[0].URL, nil, task.StatusTodo)
	mkGatedChild(t, tasks, umb, subs[1].URL, []string{subs[0].URL}, task.StatusBlocked)
	rawPayload := "INTERNAL-CUSTOMER " + strings.Repeat("界", 300)
	run := erroringRunner(errors.New(`provider unavailable: "` + rawPayload + `" https://github.com/work/repo/issues/7 ABC-123 user@example.com`))

	res, err := RecoverDegraded(context.Background(), tasks, run, umb.URL)
	if err != nil {
		t.Fatalf("RecoverDegraded: %v", err)
	}
	if !utf8.ValidString(res.Reason) || len(res.Reason) > 160 {
		t.Fatalf("RecoveryResult.Reason invalid or too long: len=%d valid=%v %q", len(res.Reason), utf8.ValidString(res.Reason), res.Reason)
	}
	for _, leak := range []string{"INTERNAL-CUSTOMER", "github.com/work/repo", "ABC-123", "user@example.com"} {
		if strings.Contains(res.Reason, leak) {
			t.Fatalf("RecoveryResult.Reason leaked %q: %q", leak, res.Reason)
		}
	}

	tracker := mustGetByIssue(t, tasks, umb.URL, task.TaskTypeUmbrella)
	if !utf8.ValidString(tracker.StatusReason) || len(tracker.StatusReason) > 200 {
		t.Fatalf("StatusReason invalid or too long: len=%d valid=%v %q", len(tracker.StatusReason), utf8.ValidString(tracker.StatusReason), tracker.StatusReason)
	}
	for _, leak := range []string{"INTERNAL-CUSTOMER", "github.com/work/repo", "ABC-123", "user@example.com"} {
		if strings.Contains(tracker.StatusReason, leak) {
			t.Fatalf("StatusReason leaked %q: %q", leak, tracker.StatusReason)
		}
	}
}

// TestRecoverDegraded_FallbackPlanIsRefused guards the "recovery must not
// accept a degraded plan" rule: a planner that never produces a valid DAG
// makes Generate itself fall back to a linear chain (Fallback=true).
// RecoverDegraded must refuse to apply that, record a failure, and leave
// every existing child untouched.
func TestRecoverDegraded_FallbackPlanIsRefused(t *testing.T) {
	subs := makeTestIssues(2)
	umb := github.Issue{Title: "umbrella", URL: "https://github.com/o/r/issues/100", Repository: "o/r", Body: "body"}
	restore := githubFetchUmbrellaForTest(t, umb, subs)
	defer restore()
	tasks := newTestTaskManager(t)
	mkTracker(t, tasks, umb, 5)
	c1 := mkGatedChild(t, tasks, umb, subs[0].URL, nil, task.StatusTodo)
	c2 := mkGatedChild(t, tasks, umb, subs[1].URL, []string{subs[0].URL}, task.StatusBlocked)

	res, err := RecoverDegraded(context.Background(), tasks, invalidRunner(), umb.URL)
	if err != nil {
		t.Fatalf("RecoverDegraded: %v", err)
	}
	if res.Outcome != RecoveryFailed {
		t.Fatalf("Outcome = %q, want %q (reason=%q)", res.Outcome, RecoveryFailed, res.Reason)
	}
	if !strings.Contains(res.Reason, "fallback") {
		t.Fatalf("Reason = %q, want to mention the fallback plan", res.Reason)
	}

	tracker := mustGetByIssue(t, tasks, umb.URL, task.TaskTypeUmbrella)
	if !slices.Contains(tracker.Tags, FallbackTag) {
		t.Fatalf("tracker tags = %v, want FallbackTag kept", tracker.Tags)
	}
	got1 := mustTaskByID(t, tasks, c1.ID)
	got2 := mustTaskByID(t, tasks, c2.ID)
	if !slices.Equal(got1.DependsOn, c1.DependsOn) || !slices.Equal(got2.DependsOn, []string{subs[0].URL}) {
		t.Fatalf("children mutated on a refused plan: c1=%v c2=%v", got1.DependsOn, got2.DependsOn)
	}
}

func TestRecoverDegraded_CooldownSkipsPlanner(t *testing.T) {
	subs := makeTestIssues(1)
	umb := github.Issue{Title: "umbrella", URL: "https://github.com/o/r/issues/100", Repository: "o/r", Body: "body"}
	restore := githubFetchUmbrellaForTest(t, umb, subs)
	defer restore()
	tasks := newTestTaskManager(t)
	mkTracker(t, tasks, umb, 5, RecoverAfterTag(time.Now().Add(time.Hour)))
	run, calls := newCountingRunner(planJSON(t, 5, subs[0].URL))

	res, err := RecoverDegraded(context.Background(), tasks, run, umb.URL)
	if err != nil {
		t.Fatalf("RecoverDegraded: %v", err)
	}
	if res.Outcome != RecoverySkipped {
		t.Fatalf("Outcome = %q, want %q", res.Outcome, RecoverySkipped)
	}
	if calls() != 0 {
		t.Fatalf("planner called %d times, want 0 while cooling down", calls())
	}
}

func TestRecoverDegraded_ExhaustedSkipsPlanner(t *testing.T) {
	subs := makeTestIssues(1)
	umb := github.Issue{Title: "umbrella", URL: "https://github.com/o/r/issues/100", Repository: "o/r", Body: "body"}
	restore := githubFetchUmbrellaForTest(t, umb, subs)
	defer restore()
	tasks := newTestTaskManager(t)
	mkTracker(t, tasks, umb, 5, RecoverExhaustedTag)
	run, calls := newCountingRunner(planJSON(t, 5, subs[0].URL))

	res, err := RecoverDegraded(context.Background(), tasks, run, umb.URL)
	if err != nil {
		t.Fatalf("RecoverDegraded: %v", err)
	}
	if res.Outcome != RecoverySkipped {
		t.Fatalf("Outcome = %q, want %q", res.Outcome, RecoverySkipped)
	}
	if calls() != 0 {
		t.Fatalf("planner called %d times, want 0 once exhausted", calls())
	}
}

// TestRecoverDegraded_DuplicateChildRefsRefused guards the safety refusal
// path: two children claiming the same sub-issue ref is a state
// RecoverDegraded cannot safely reconcile, so it must refuse before ever
// calling the planner.
func TestRecoverDegraded_DuplicateChildRefsRefused(t *testing.T) {
	subs := makeTestIssues(2)
	umb := github.Issue{Title: "umbrella", URL: "https://github.com/o/r/issues/100", Repository: "o/r", Body: "body"}
	restore := githubFetchUmbrellaForTest(t, umb, subs)
	defer restore()
	tasks := newTestTaskManager(t)
	mkTracker(t, tasks, umb, 5)
	mkGatedChild(t, tasks, umb, subs[0].URL, nil, task.StatusTodo)
	mkGatedChild(t, tasks, umb, subs[0].URL, nil, task.StatusTodo) // duplicate ref
	run, calls := newCountingRunner(planJSON(t, 5, subs[0].URL, subs[1].URL))

	res, err := RecoverDegraded(context.Background(), tasks, run, umb.URL)
	if err != nil {
		t.Fatalf("RecoverDegraded: %v", err)
	}
	if res.Outcome != RecoverySafetyRefused {
		t.Fatalf("Outcome = %q, want %q (reason=%q)", res.Outcome, RecoverySafetyRefused, res.Reason)
	}
	if calls() != 0 {
		t.Fatalf("planner called %d times, want 0 on unsafe children", calls())
	}
	tracker := mustGetByIssue(t, tasks, umb.URL, task.TaskTypeUmbrella)
	if !slices.Contains(tracker.Tags, FallbackTag) || ParseRecoverFailCount(tracker.Tags) != 1 {
		t.Fatalf("tracker tags = %v, want FallbackTag kept and fail count 1", tracker.Tags)
	}
}

// TestRecoverDegraded_ActiveChildDependenciesUntouched guards child
// mutability: a released/active child's dependencies must never be rewritten
// by recovery, even though the recovered plan computes different canonical
// dependencies for its ref.
func TestRecoverDegraded_ActiveChildDependenciesUntouched(t *testing.T) {
	subs := makeTestIssues(2)
	umb := github.Issue{Title: "umbrella", URL: "https://github.com/o/r/issues/100", Repository: "o/r", Body: "body"}
	restore := githubFetchUmbrellaForTest(t, umb, subs)
	defer restore()
	tasks := newTestTaskManager(t)
	mkTracker(t, tasks, umb, 1)
	mkGatedChild(t, tasks, umb, subs[0].URL, nil, task.StatusTodo)
	active := mkActiveChild(t, tasks, umb, subs[1].URL, nil) // frozen: no GatedTag, in-progress
	run, _ := newCountingRunner(planJSON(t, 1, subs[0].URL, subs[1].URL))

	res, err := RecoverDegraded(context.Background(), tasks, run, umb.URL)
	if err != nil {
		t.Fatalf("RecoverDegraded: %v", err)
	}
	if res.Outcome != RecoveryRecovered {
		t.Fatalf("Outcome = %q, want %q (reason=%q)", res.Outcome, RecoveryRecovered, res.Reason)
	}
	if res.ChildrenUpdated != 0 {
		t.Fatalf("ChildrenUpdated = %d, want 0 (only child needing a change is frozen)", res.ChildrenUpdated)
	}

	got := mustTaskByID(t, tasks, active.ID)
	if len(got.DependsOn) != 0 {
		t.Fatalf("active child DependsOn = %v, want untouched (empty)", got.DependsOn)
	}
	tracker := mustGetByIssue(t, tasks, umb.URL, task.TaskTypeUmbrella)
	if slices.Contains(tracker.Tags, FallbackTag) {
		t.Fatalf("tracker tags = %v, want FallbackTag removed despite the frozen child", tracker.Tags)
	}
}

func TestRecoverDegraded_RechecksTrackerStatusBeforeMutating(t *testing.T) {
	subs := makeTestIssues(2)
	umb := github.Issue{Title: "umbrella", URL: "https://github.com/o/r/issues/100", Repository: "o/r", Body: "body"}
	restore := githubFetchUmbrellaForTest(t, umb, subs)
	defer restore()
	tasks := newTestTaskManager(t)
	tracker := mkTracker(t, tasks, umb, 1)
	c1 := mkGatedChild(t, tasks, umb, subs[0].URL, nil, task.StatusTodo)
	c2 := mkGatedChild(t, tasks, umb, subs[1].URL, nil, task.StatusBlocked)
	run := func(context.Context, string, string) (string, error) {
		reason := "operator cancelled during recovery"
		if _, err := tasks.Update(tracker.ID, task.Update{Status: task.Ptr(task.StatusCancelled), StatusReason: task.Ptr(reason)}); err != nil {
			t.Fatalf("cancel tracker during planner run: %v", err)
		}
		return planJSON(t, 2, subs[0].URL, subs[1].URL), nil
	}

	res, err := RecoverDegraded(context.Background(), tasks, run, umb.URL)
	if err != nil {
		t.Fatalf("RecoverDegraded: %v", err)
	}
	if res.Outcome != RecoverySkipped {
		t.Fatalf("Outcome = %q, want %q (reason=%q)", res.Outcome, RecoverySkipped, res.Reason)
	}
	if res.Reason != "tracker status is not recoverable" {
		t.Fatalf("Reason = %q, want tracker status skip", res.Reason)
	}

	gotTracker := mustTaskByID(t, tasks, tracker.ID)
	if !slices.Contains(gotTracker.Tags, FallbackTag) {
		t.Fatalf("tracker tags = %v, want FallbackTag kept after cancelled recheck", gotTracker.Tags)
	}
	got1 := mustTaskByID(t, tasks, c1.ID)
	got2 := mustTaskByID(t, tasks, c2.ID)
	if !slices.Equal(got1.DependsOn, c1.DependsOn) || !slices.Equal(got2.DependsOn, c2.DependsOn) {
		t.Fatalf("children mutated after tracker cancellation: c1=%v c2=%v", got1.DependsOn, got2.DependsOn)
	}
}

func TestRecoverDegraded_MalformedRecoveryTagsReplacedCanonically(t *testing.T) {
	subs := makeTestIssues(1)
	umb := github.Issue{Title: "umbrella", URL: "https://github.com/o/r/issues/100", Repository: "o/r", Body: "body"}
	restore := githubFetchUmbrellaForTest(t, umb, subs)
	defer restore()
	tasks := newTestTaskManager(t)
	mkTracker(t, tasks, umb, 5, "umbrella-recover-fail:abc", "umbrella-recover-after:not-a-number")
	run := erroringRunner(errors.New("boom"))

	res, err := RecoverDegraded(context.Background(), tasks, run, umb.URL)
	if err != nil {
		t.Fatalf("RecoverDegraded: %v", err)
	}
	if res.FailCount != 1 {
		t.Fatalf("FailCount = %d, want 1 (malformed tag parses as 0)", res.FailCount)
	}

	tracker := mustGetByIssue(t, tasks, umb.URL, task.TaskTypeUmbrella)
	var failTags, afterTags []string
	for _, tag := range tracker.Tags {
		if strings.HasPrefix(tag, RecoverFailTagPrefix) {
			failTags = append(failTags, tag)
		}
		if strings.HasPrefix(tag, RecoverAfterTagPrefix) {
			afterTags = append(afterTags, tag)
		}
	}
	if !slices.Equal(failTags, []string{RecoverFailTag(1)}) {
		t.Fatalf("recover-fail tags = %v, want exactly [%s]", failTags, RecoverFailTag(1))
	}
	if len(afterTags) != 1 || afterTags[0] == "umbrella-recover-after:not-a-number" {
		t.Fatalf("recover-after tags = %v, want exactly one canonical tag", afterTags)
	}
}

func TestRecoverDegraded_DuplicateMaxParallelTagsCollapsed(t *testing.T) {
	subs := makeTestIssues(1)
	umb := github.Issue{Title: "umbrella", URL: "https://github.com/o/r/issues/100", Repository: "o/r", Body: "body"}
	restore := githubFetchUmbrellaForTest(t, umb, subs)
	defer restore()
	tasks := newTestTaskManager(t)
	tracker, err := tasks.CreateFull(umb.Title, umb.Body, task.AgentModeHeadless, task.Update{
		Issue:     task.Ptr(umb.URL),
		TaskType:  task.Ptr(task.TaskTypeUmbrella),
		ProjectID: task.Ptr(umb.Repository),
		Status:    task.Ptr(task.StatusInProgress),
		Tags:      task.Ptr([]string{"umbrella", MaxParallelTag(1), MaxParallelTag(9), FallbackTag}),
	})
	if err != nil {
		t.Fatalf("create tracker: %v", err)
	}
	mkGatedChild(t, tasks, umb, subs[0].URL, nil, task.StatusTodo)
	run, _ := newCountingRunner(planJSON(t, 3, subs[0].URL))

	res, err := RecoverDegraded(context.Background(), tasks, run, umb.URL)
	if err != nil {
		t.Fatalf("RecoverDegraded: %v", err)
	}
	if res.Outcome != RecoveryRecovered {
		t.Fatalf("Outcome = %q, want %q (reason=%q)", res.Outcome, RecoveryRecovered, res.Reason)
	}

	got := mustTaskByID(t, tasks, tracker.ID)
	var maxParallelTags []string
	for _, tag := range got.Tags {
		if strings.HasPrefix(tag, MaxParallelTagPrefix) {
			maxParallelTags = append(maxParallelTags, tag)
		}
	}
	if !slices.Equal(maxParallelTags, []string{MaxParallelTag(3)}) {
		t.Fatalf("max-parallel tags = %v, want exactly [%s]", maxParallelTags, MaxParallelTag(3))
	}
}

// TestRecoverDegraded_PartialWriteReruns proves each mutation checkpoint is
// idempotent: injecting a failure right after a checkpoint's write, then
// rerunning, must converge to success (or remain safely cooled-down) without
// duplicating child tasks or control tags.
func TestRecoverDegraded_PartialWriteReruns(t *testing.T) {
	checkpoints := []string{checkpointChildrenCreated, checkpointDepsUpdated, checkpointMaxParallel, checkpointBeforeFinalClear}
	for _, cp := range checkpoints {
		t.Run(cp, func(t *testing.T) {
			subs := makeTestIssues(2)
			umb := github.Issue{Title: "umbrella", URL: "https://github.com/o/r/issues/100", Repository: "o/r", Body: "body"}
			restore := githubFetchUmbrellaForTest(t, umb, subs)
			defer restore()
			tasks := newTestTaskManager(t)
			mkTracker(t, tasks, umb, 1)
			mkGatedChild(t, tasks, umb, subs[0].URL, nil, task.StatusTodo)
			mkGatedChild(t, tasks, umb, subs[1].URL, []string{subs[0].URL}, task.StatusBlocked)
			run, _ := newCountingRunner(planJSON(t, 2, subs[0].URL, subs[1].URL))

			recoveryFailAfter = cp
			res1, err := RecoverDegraded(context.Background(), tasks, run, umb.URL)
			recoveryFailAfter = ""
			if err != nil {
				t.Fatalf("first (injected-failure) run: %v", err)
			}
			if res1.Outcome != RecoveryFailed {
				t.Fatalf("first run outcome = %q, want %q (injected at %s)", res1.Outcome, RecoveryFailed, cp)
			}

			res2, err := RecoverDegraded(context.Background(), tasks, run, umb.URL)
			if err != nil {
				t.Fatalf("rerun: %v", err)
			}
			if res2.Outcome != RecoveryRecovered && res2.Outcome != RecoverySkipped {
				t.Fatalf("rerun outcome = %q, want %q or %q (safely backed off)", res2.Outcome, RecoveryRecovered, RecoverySkipped)
			}
			assertNoDuplicateChildrenOrTags(t, tasks, umb.URL)
		})
	}
}

func assertNoDuplicateChildrenOrTags(t *testing.T, tasks *task.Manager, umbURL string) {
	t.Helper()
	all, err := tasks.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	umbKey := NormalizeIssueRef(umbURL)
	seenChild := map[string]int{}
	var tracker *task.Task
	for i := range all {
		tt := &all[i]
		if tt.TaskType == task.TaskTypeUmbrella && NormalizeIssueRef(tt.Issue) == umbKey {
			tracker = tt
		}
		if tt.UmbrellaIssue != "" && NormalizeIssueRef(tt.UmbrellaIssue) == umbKey {
			seenChild[NormalizeIssueRef(tt.Issue)]++
		}
	}
	for ref, n := range seenChild {
		if n > 1 {
			t.Fatalf("duplicate child task for ref %s: %d tasks", ref, n)
		}
	}
	if tracker == nil {
		t.Fatal("tracker task missing after rerun")
	}
	maxParallelCount, recoverFailCount, recoverAfterCount, fallbackCount, exhaustedCount := 0, 0, 0, 0, 0
	for _, tag := range tracker.Tags {
		switch {
		case strings.HasPrefix(tag, MaxParallelTagPrefix):
			maxParallelCount++
		case strings.HasPrefix(tag, RecoverFailTagPrefix):
			recoverFailCount++
		case strings.HasPrefix(tag, RecoverAfterTagPrefix):
			recoverAfterCount++
		case tag == FallbackTag:
			fallbackCount++
		case tag == RecoverExhaustedTag:
			exhaustedCount++
		}
	}
	if maxParallelCount > 1 || recoverFailCount > 1 || recoverAfterCount > 1 || fallbackCount > 1 || exhaustedCount > 1 {
		t.Fatalf("duplicate control tags after rerun: %v", tracker.Tags)
	}
}

func mustGetByIssue(t *testing.T, tasks *task.Manager, issue string, taskType task.TaskType) task.Task {
	t.Helper()
	all, err := tasks.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for i := range all {
		if NormalizeIssueRef(all[i].Issue) == NormalizeIssueRef(issue) && (taskType == "" || all[i].TaskType == taskType) {
			return all[i]
		}
	}
	t.Fatalf("no task found for issue %s (type=%q)", issue, taskType)
	return task.Task{}
}

func mustTaskByID(t *testing.T, tasks *task.Manager, id string) task.Task {
	t.Helper()
	tk, err := tasks.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return tk
}
