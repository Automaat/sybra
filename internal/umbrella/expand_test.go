package umbrella

import (
	"context"
	"slices"
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
	minBudget := PlannerAttemptTimeout * time.Duration(plannerJobAttempts*plannerGenerateSamples)
	if got := plannerTimeout(0); got < minBudget {
		t.Fatalf("plannerTimeout(0) = %v, want at least %v (room for %d planner samples x %d llmjob attempts)", got, minBudget, plannerGenerateSamples, plannerJobAttempts)
	}
	small := plannerTimeout(1)
	large := plannerTimeout(38)
	if large <= small {
		t.Fatalf("plannerTimeout(38) = %v, want greater than plannerTimeout(1) = %v", large, small)
	}
}

// TestPlannerAttemptTimeout_ScalesWithPromptSize guards #1570: a fixed
// per-attempt budget deadline-kills every attempt on a large umbrella, whose
// prompt (and per-child JSON answer) grow with sub-issue count. The budget
// must hold the floor for small prompts, grow with prompt size, and stay
// capped so one attempt cannot hold the expansion slot indefinitely.
func TestPlannerAttemptTimeout_ScalesWithPromptSize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		promptLen int
		want      time.Duration
	}{
		{"empty prompt keeps floor", 0, PlannerAttemptTimeout},
		{"below one chunk keeps floor", plannerAttemptPromptChunk - 1, PlannerAttemptTimeout},
		{"each chunk buys a minute", 4 * plannerAttemptPromptChunk, PlannerAttemptTimeout + 4*time.Minute},
		{"huge prompt is capped", 1 << 30, plannerAttemptTimeoutMax},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := plannerAttemptTimeout(tt.promptLen); got != tt.want {
				t.Fatalf("plannerAttemptTimeout(%d) = %v, want %v", tt.promptLen, got, tt.want)
			}
		})
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

	_, trackerExists, trackerID, err := scanExisting(tasks, umb)
	if err != nil {
		t.Fatalf("scanExisting: %v", err)
	}
	if !trackerExists {
		t.Fatal("trackerExists = false, want true")
	}
	if trackerID != tracker.ID {
		t.Fatalf("trackerID = %q, want %q", trackerID, tracker.ID)
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
