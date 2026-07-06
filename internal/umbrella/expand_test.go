package umbrella

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
)

// TestPlannerTimeout_ScalesWithSubCountAndCoversEveryAttempt guards #1555: the
// overall Generate deadline must comfortably fit the full worst-case Generate
// path (initial attemptPlan plus critic re-ask, each with plannerAttempts
// planner samples, each sample with plannerJobAttempts full llmjob slices),
// and grow with subCount rather than staying a single fixed ceiling shared
// across every retry.
func TestPlannerTimeout_ScalesWithSubCountAndCoversEveryAttempt(t *testing.T) {
	t.Parallel()
	minBudget := plannerAttemptTimeout(0) * time.Duration(plannerJobAttempts*plannerGenerateSamples)
	if got := plannerTimeout(0); got < minBudget {
		t.Fatalf("plannerTimeout(0) = %v, want at least %v (room for %d planner samples x %d llmjob attempts)", got, minBudget, plannerGenerateSamples, plannerJobAttempts)
	}
	small := plannerTimeout(1)
	large := plannerTimeout(38)
	if large <= small {
		t.Fatalf("plannerTimeout(38) = %v, want greater than plannerTimeout(1) = %v", large, small)
	}
}

func TestPlannerAttemptTimeout_ScalesWithSubCount(t *testing.T) {
	t.Parallel()
	if got := plannerAttemptTimeout(0); got != PlannerAttemptTimeout {
		t.Fatalf("plannerAttemptTimeout(0) = %v, want base %v", got, PlannerAttemptTimeout)
	}
	small := plannerAttemptTimeout(1)
	large := plannerAttemptTimeout(38)
	if large <= small {
		t.Fatalf("plannerAttemptTimeout(38) = %v, want greater than plannerAttemptTimeout(1) = %v", large, small)
	}
	if large <= PlannerAttemptTimeout {
		t.Fatalf("plannerAttemptTimeout(38) = %v, want greater than fixed floor %v", large, PlannerAttemptTimeout)
	}
}

func newTestTaskManager(t *testing.T) *task.Manager {
	t.Helper()
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	return task.NewManager(store, task.EmitterFunc(func(string, any) {}))
}

func TestExpandThreadsScaledAttemptTimeoutToPlanner(t *testing.T) {
	restore := githubFetchUmbrellaForTest(t, github.Issue{
		Title:      "umbrella",
		URL:        "https://github.com/o/r/issues/100",
		Repository: "o/r",
	}, makeTestIssues(38))
	defer restore()

	tasks := newTestTaskManager(t)
	var got time.Duration
	run := func(ctx context.Context, _ string) (string, error) {
		got = plannerAttemptTimeoutFromContext(ctx)
		return "", errors.New("stop after observing context")
	}

	_, err := Expand(context.Background(), tasks, run, "https://github.com/o/r/issues/100")
	if err == nil {
		t.Fatal("Expand succeeded unexpectedly with an intentionally failing runner")
	}
	want := plannerAttemptTimeout(38)
	if got != want {
		t.Fatalf("planner attempt timeout from context = %v, want %v", got, want)
	}
}

func TestExpandPlannerDeadlineFallsBackToLinearChain(t *testing.T) {
	restore := githubFetchUmbrellaForTest(t, github.Issue{
		Title:      "umbrella",
		URL:        "https://github.com/o/r/issues/100",
		Repository: "o/r",
	}, makeTestIssues(3))
	defer restore()

	tasks := newTestTaskManager(t)
	run := func(context.Context, string) (string, error) {
		return "", context.DeadlineExceeded
	}

	res, err := Expand(context.Background(), tasks, run, "https://github.com/o/r/issues/100")
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if !res.Degraded {
		t.Fatalf("Degraded = false, want true after planner deadline fallback")
	}
	if res.Created != 3 {
		t.Fatalf("Created = %d, want 3", res.Created)
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var tracker *task.Task
	children := map[string]task.Task{}
	for i := range all {
		if all[i].TaskType == task.TaskTypeUmbrella {
			tracker = &all[i]
			continue
		}
		children[NormalizeIssueRef(all[i].Issue)] = all[i]
	}
	if tracker == nil {
		t.Fatal("no umbrella tracker created")
	}
	if !slices.Contains(tracker.Tags, FallbackTag) {
		t.Fatalf("tracker tags = %v, want %q", tracker.Tags, FallbackTag)
	}
	if got := ParseExpandFailCount(tracker.Tags); got != 0 {
		t.Fatalf("expand fail count = %d, want 0 for degraded fallback success", got)
	}
	if got := children["o/r#2"].DependsOn; len(got) != 1 || got[0] != "https://github.com/o/r/issues/1" {
		t.Fatalf("child #2 deps = %v, want issue #1", got)
	}
	if got := children["o/r#3"].DependsOn; len(got) != 1 || got[0] != "https://github.com/o/r/issues/2" {
		t.Fatalf("child #3 deps = %v, want issue #2", got)
	}
}

func githubFetchUmbrellaForTest(t *testing.T, umb github.Issue, subs []github.Issue) func() {
	t.Helper()
	old := fetchUmbrella
	fetchUmbrella = func(_ context.Context, _ string, _ int) (github.Issue, []github.Issue, error) {
		return umb, subs, nil
	}
	return func() {
		fetchUmbrella = old
	}
}

func makeTestIssues(n int) []github.Issue {
	out := make([]github.Issue, n)
	for i := range out {
		num := i + 1
		out[i] = github.Issue{
			Title:      "child",
			URL:        fmt.Sprintf("https://github.com/o/r/issues/%d", num),
			Repository: "o/r",
			State:      "OPEN",
		}
	}
	return out
}

func TestMaterialize_DegradedFreshTrackerCarriesFallbackTag(t *testing.T) {
	t.Parallel()
	tasks := newTestTaskManager(t)
	umb := github.Issue{Title: "umbrella", URL: "https://github.com/o/r/issues/100", Repository: "o/r"}
	specs := []ChildSpec{{Title: "c1", Issue: "o/r#1"}}

	if _, err := materialize(tasks, umb, specs, map[string]github.Issue{}, false, "", DefaultMaxParallel, true); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var tracker *task.Task
	for i := range all {
		if all[i].TaskType == task.TaskTypeUmbrella {
			tracker = &all[i]
		}
	}
	if tracker == nil {
		t.Fatal("no tracker task created")
	}
	if !slices.Contains(tracker.Tags, FallbackTag) {
		t.Errorf("fresh degraded tracker tags = %v, want to contain %q", tracker.Tags, FallbackTag)
	}
}

func TestMaterialize_DegradedExistingTrackerGetsFallbackTagIdempotently(t *testing.T) {
	t.Parallel()
	tasks := newTestTaskManager(t)
	umb := github.Issue{Title: "umbrella", URL: "https://github.com/o/r/issues/100", Repository: "o/r"}

	tracker, err := tasks.CreateFull(umb.Title, "", task.AgentModeHeadless, task.Update{
		Issue:    task.Ptr(umb.URL),
		TaskType: task.Ptr(task.TaskTypeUmbrella),
		Status:   task.Ptr(task.StatusInProgress),
		Tags:     task.Ptr([]string{"umbrella", MaxParallelTag(DefaultMaxParallel)}),
	})
	if err != nil {
		t.Fatalf("create tracker: %v", err)
	}

	specs := []ChildSpec{{Title: "c1", Issue: "o/r#1"}}
	if _, err := materialize(tasks, umb, specs, map[string]github.Issue{}, true, tracker.ID, DefaultMaxParallel, true); err != nil {
		t.Fatalf("materialize (first, degraded): %v", err)
	}
	got, err := tasks.Get(tracker.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !slices.Contains(got.Tags, FallbackTag) {
		t.Fatalf("existing tracker tags = %v, want to contain %q after degraded re-expansion", got.Tags, FallbackTag)
	}

	// A second degraded re-expansion against the same tracker must not
	// duplicate the tag.
	if _, err := materialize(tasks, umb, nil, map[string]github.Issue{}, true, tracker.ID, DefaultMaxParallel, true); err != nil {
		t.Fatalf("materialize (second, degraded): %v", err)
	}
	got, err = tasks.Get(tracker.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	count := 0
	for _, tag := range got.Tags {
		if tag == FallbackTag {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("FallbackTag count = %d, want exactly 1 (idempotent): %v", count, got.Tags)
	}
}

// TestRecordExpandFailure_NoTrackerCreatesVisibleFailureTracker guards #1570's
// board-visibility gap: previously a failed expansion with no prior tracker
// left zero trace on the board besides a log line. The first failure must
// materialize a real, inspectable task.
func TestRecordExpandFailure_NoTrackerCreatesVisibleFailureTracker(t *testing.T) {
	t.Parallel()
	tasks := newTestTaskManager(t)
	umb := github.Issue{Title: "umbrella", URL: "https://github.com/o/r/issues/100", Repository: "o/r", Body: "body"}

	if err := recordExpandFailure(tasks, umb, existingTracker{}, errors.New("planner killed")); err != nil {
		t.Fatalf("recordExpandFailure: %v", err)
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(all))
	}
	tracker := all[0]
	if tracker.TaskType != task.TaskTypeUmbrella || tracker.Issue != umb.URL {
		t.Fatalf("tracker = %+v, want TaskType=umbrella Issue=%q", tracker, umb.URL)
	}
	if ParseExpandFailCount(tracker.Tags) != 1 {
		t.Fatalf("fail count = %d, want 1: %v", ParseExpandFailCount(tracker.Tags), tracker.Tags)
	}
	if !strings.Contains(tracker.StatusReason, "planner killed") {
		t.Fatalf("StatusReason = %q, want to mention the failure cause", tracker.StatusReason)
	}
	if tracker.Status == task.StatusHumanRequired {
		t.Fatalf("status = human-required after a single failure, want to stay non-terminal below threshold")
	}
}

// TestRecordExpandFailure_EscalatesAtThreshold guards the "stop retrying"
// half of #1570: after ExpandFailThreshold consecutive failures against an
// existing tracker, the tracker must park human-required rather than keep
// silently retrying forever.
func TestRecordExpandFailure_EscalatesAtThreshold(t *testing.T) {
	t.Parallel()
	tasks := newTestTaskManager(t)
	umb := github.Issue{Title: "umbrella", URL: "https://github.com/o/r/issues/100", Repository: "o/r"}
	tracker, err := tasks.CreateFull(umb.Title, "", task.AgentModeHeadless, task.Update{
		Issue:    task.Ptr(umb.URL),
		TaskType: task.Ptr(task.TaskTypeUmbrella),
		Status:   task.Ptr(task.StatusInProgress),
		Tags:     task.Ptr([]string{"umbrella", ExpandFailTag(ExpandFailThreshold - 1)}),
	})
	if err != nil {
		t.Fatalf("create tracker: %v", err)
	}

	if err := recordExpandFailure(tasks, umb, existingTracker{exists: true, id: tracker.ID, tags: tracker.Tags}, errors.New("killed")); err != nil {
		t.Fatalf("recordExpandFailure: %v", err)
	}

	got, err := tasks.Get(tracker.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required at ExpandFailThreshold=%d", got.Status, ExpandFailThreshold)
	}
	if ParseExpandFailCount(got.Tags) != ExpandFailThreshold {
		t.Fatalf("fail count = %d, want %d", ParseExpandFailCount(got.Tags), ExpandFailThreshold)
	}
}

func TestRecordExpandFailure_RefreshesMissingTrackerSnapshot(t *testing.T) {
	t.Parallel()
	tasks := newTestTaskManager(t)
	umb := github.Issue{Title: "umbrella", URL: "https://github.com/o/r/issues/100", Repository: "o/r"}
	tracker, err := tasks.CreateFull(umb.Title, "", task.AgentModeHeadless, task.Update{
		Issue:    task.Ptr(umb.URL),
		TaskType: task.Ptr(task.TaskTypeUmbrella),
		Status:   task.Ptr(task.StatusInProgress),
		Tags:     task.Ptr([]string{"umbrella", ExpandFailTag(1)}),
	})
	if err != nil {
		t.Fatalf("create tracker: %v", err)
	}

	if err := recordExpandFailure(tasks, umb, existingTracker{}, errors.New("killed")); err != nil {
		t.Fatalf("recordExpandFailure: %v", err)
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("len(tasks) = %d, want 1 existing tracker updated in place", len(all))
	}
	got, err := tasks.Get(tracker.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ParseExpandFailCount(got.Tags) != 2 {
		t.Fatalf("fail count = %d, want 2 after updating the existing tracker", ParseExpandFailCount(got.Tags))
	}
}

func TestRecordExpandFailure_UsesLiveTagCount(t *testing.T) {
	t.Parallel()
	tasks := newTestTaskManager(t)
	umb := github.Issue{Title: "umbrella", URL: "https://github.com/o/r/issues/100", Repository: "o/r"}
	tracker, err := tasks.CreateFull(umb.Title, "", task.AgentModeHeadless, task.Update{
		Issue:    task.Ptr(umb.URL),
		TaskType: task.Ptr(task.TaskTypeUmbrella),
		Status:   task.Ptr(task.StatusInProgress),
		Tags:     task.Ptr([]string{"umbrella", ExpandFailTag(2)}),
	})
	if err != nil {
		t.Fatalf("create tracker: %v", err)
	}

	stale := existingTracker{exists: true, id: tracker.ID, tags: []string{"umbrella", ExpandFailTag(1)}}
	if err := recordExpandFailure(tasks, umb, stale, errors.New("killed")); err != nil {
		t.Fatalf("recordExpandFailure: %v", err)
	}

	got, err := tasks.Get(tracker.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ParseExpandFailCount(got.Tags) != 3 {
		t.Fatalf("fail count = %d, want 3 from the live tracker state, not stale input", ParseExpandFailCount(got.Tags))
	}
}

// TestClearExpandFailure_StripsTagOnSuccess guards the recovery path: once
// expansion succeeds again, the failure count must reset so a later, distinct
// failure streak starts counting from zero rather than resuming near the
// threshold.
func TestClearExpandFailure_StripsTagOnSuccess(t *testing.T) {
	t.Parallel()
	tasks := newTestTaskManager(t)
	tracker, err := tasks.CreateFull("umbrella", "", task.AgentModeHeadless, task.Update{
		Issue:        task.Ptr("https://github.com/o/r/issues/100"),
		TaskType:     task.Ptr(task.TaskTypeUmbrella),
		Status:       task.Ptr(task.StatusInProgress),
		StatusReason: task.Ptr("umbrella expansion failed (attempt 2): planner killed"),
		Tags:         task.Ptr([]string{"umbrella", ExpandFailTag(2)}),
	})
	if err != nil {
		t.Fatalf("create tracker: %v", err)
	}

	if err := clearExpandFailure(tasks, tracker.ID); err != nil {
		t.Fatalf("clearExpandFailure: %v", err)
	}

	got, err := tasks.Get(tracker.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ParseExpandFailCount(got.Tags) != 0 {
		t.Fatalf("fail count = %d, want 0 after clear", ParseExpandFailCount(got.Tags))
	}
	if got.StatusReason != "" {
		t.Fatalf("StatusReason = %q, want cleared with the failure tag", got.StatusReason)
	}

	// Idempotent: clearing an already-clean tracker is a no-op, not an error.
	if err := clearExpandFailure(tasks, tracker.ID); err != nil {
		t.Fatalf("clearExpandFailure (idempotent): %v", err)
	}
}

// TestMaterialize_BackfillsMaxParallelOnPlaceholderTracker guards the tracker
// created by recordExpandFailure (which has no MaxParallelTag, since it never
// ran through materialize's fresh-tracker branch): the first successful
// materialize against it must backfill the tag rather than leaving the
// tracker's parallelism cap silently pinned to DefaultMaxParallel forever.
func TestMaterialize_BackfillsMaxParallelOnPlaceholderTracker(t *testing.T) {
	t.Parallel()
	tasks := newTestTaskManager(t)
	umb := github.Issue{Title: "umbrella", URL: "https://github.com/o/r/issues/100", Repository: "o/r"}

	placeholder, err := tasks.CreateFull(umb.Title, umb.Body, task.AgentModeHeadless, task.Update{
		Issue:    task.Ptr(umb.URL),
		TaskType: task.Ptr(task.TaskTypeUmbrella),
		Status:   task.Ptr(task.StatusInProgress),
		Tags:     task.Ptr([]string{"umbrella", ExpandFailTag(1)}),
	})
	if err != nil {
		t.Fatalf("create placeholder tracker: %v", err)
	}
	if HasMaxParallelTag(placeholder.Tags) {
		t.Fatal("placeholder tracker unexpectedly already has a MaxParallelTag")
	}

	specs := []ChildSpec{{Title: "c1", Issue: "o/r#1"}}
	if _, err := materialize(tasks, umb, specs, map[string]github.Issue{}, true, placeholder.ID, 7, false); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	got, err := tasks.Get(placeholder.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !HasMaxParallelTag(got.Tags) || ParseMaxParallel(got.Tags) != 7 {
		t.Fatalf("tracker tags = %v, want a MaxParallelTag backfilled to 7", got.Tags)
	}
}

func TestScanExisting_ReturnsTrackerID(t *testing.T) {
	t.Parallel()
	tasks := newTestTaskManager(t)
	const umb = "https://github.com/o/r/issues/100"
	tracker, err := tasks.CreateFull("umbrella", "", task.AgentModeHeadless, task.Update{
		Issue:    task.Ptr(umb),
		TaskType: task.Ptr(task.TaskTypeUmbrella),
		Status:   task.Ptr(task.StatusInProgress),
		Tags:     task.Ptr([]string{"umbrella"}),
	})
	if err != nil {
		t.Fatalf("create tracker: %v", err)
	}

	_, got, err := scanExisting(tasks, umb)
	if err != nil {
		t.Fatalf("scanExisting: %v", err)
	}
	if !got.exists {
		t.Fatal("trackerExists = false, want true")
	}
	if got.id != tracker.ID {
		t.Fatalf("trackerID = %q, want %q", got.id, tracker.ID)
	}
}

func TestExpandThreadsGrounder(t *testing.T) {
	t.Parallel()
	t.Run("WithExpandGrounder sets the lister and threshold", func(t *testing.T) {
		t.Parallel()
		lister := func(_ context.Context, _ string) ([]string, error) { return nil, nil }
		var cfg expandConfig
		WithExpandGrounder(lister, 5)(&cfg)
		if cfg.lister == nil {
			t.Fatal("lister not set")
		}
		if cfg.minSubs != 5 {
			t.Fatalf("minSubs = %d, want 5", cfg.minSubs)
		}
	})

	t.Run("no option leaves the grounder unset", func(t *testing.T) {
		t.Parallel()
		var cfg expandConfig
		if cfg.lister != nil {
			t.Fatal("lister should be nil without WithExpandGrounder")
		}
	})
}

func TestIsUmbrellaIssue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		title  string
		labels []string
		want   bool
	}{
		{"umbrella emoji prefix", "☂️ feat(x): umbrella", nil, true},
		{"bare umbrella rune (no VS16)", "☂ feat(x): umbrella", nil, true},
		{"emoji with leading space", "  ☂️ leading space", nil, true},
		{"umbrella label", "ordinary title", []string{"bug", "umbrella"}, true},
		{"umbrella label mixed case + space", "ordinary title", []string{" Umbrella "}, true},
		{"emoji mid-title is not a prefix", "feat ☂️ inline", nil, false},
		{"plain issue", "ordinary title", []string{"bug"}, false},
		{"no title no labels", "", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := IsUmbrellaIssue(c.title, c.labels); got != c.want {
				t.Errorf("IsUmbrellaIssue(%q, %v) = %v, want %v", c.title, c.labels, got, c.want)
			}
		})
	}
}
