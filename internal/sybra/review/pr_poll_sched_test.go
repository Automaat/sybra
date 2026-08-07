package review

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
)

func TestExpBackoff(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		streak   int
		maxTicks int
		wantMin  int
		wantMax  int
	}{
		{name: "non-positive streak", streak: 0, maxTicks: 8},
		{name: "first stable poll", streak: 1, maxTicks: 8, wantMin: 1, wantMax: 1},
		{name: "second stable poll", streak: 2, maxTicks: 8, wantMin: 1, wantMax: 2},
		{name: "third stable poll", streak: 3, maxTicks: 8, wantMin: 2, wantMax: 4},
		{name: "clamps to max", streak: 5, maxTicks: 8, wantMin: 4, wantMax: 8},
		{name: "disabled when max non-positive", streak: 3, maxTicks: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := expBackoff(tt.streak, tt.maxTicks); got < tt.wantMin || got > tt.wantMax {
				t.Fatalf("expBackoff(%d, %d) = %d, want [%d, %d]", tt.streak, tt.maxTicks, got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestSelectKnownPRPoll(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	newTask := func(id string, pr int, updatedAt time.Time) task.Task {
		return task.Task{
			ID:        id,
			ProjectID: "owner/repo",
			PRNumber:  pr,
			Status:    task.StatusInReview,
			UpdatedAt: updatedAt,
		}
	}

	t.Run("active bypasses cap and recent tasks win", func(t *testing.T) {
		t.Parallel()

		agents := newTestAgentManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())
		claim, ok := agents.TryClaimDispatch("active")
		if !ok {
			t.Fatal("TryClaimDispatch(active) = false, want true")
		}
		defer claim.Release()

		r := &Handler{
			agents: agents,
			cfg: &config.Config{
				GitHub: config.GitHubConfig{ReviewsMaxPRsPerTick: 2},
			},
		}

		sel := r.selectKnownPRPoll(context.Background(), []task.Task{
			newTask("older", 3, base.Add(-2*time.Hour)),
			newTask("active", 1, base.Add(-4*time.Hour)),
			newTask("newest", 2, base),
		})

		if got := taskIDs(sel.tasks); len(got) != 2 || got[0] != "active" || got[1] != "newest" {
			t.Fatalf("selected ids = %v, want [active newest]", got)
		}
		if sel.selectedPRs != 2 || sel.deferredPRs != 0 || sel.cappedPRs != 1 {
			t.Fatalf("selection stats = %+v, want selected=2 deferred=0 capped=1", sel)
		}
	})

	t.Run("priority ordering and cap truncation", func(t *testing.T) {
		t.Parallel()

		r := &Handler{
			cfg: &config.Config{
				GitHub: config.GitHubConfig{ReviewsMaxPRsPerTick: 2},
			},
		}

		sel := r.selectKnownPRPoll(context.Background(), []task.Task{
			newTask("mid", 2, base.Add(-time.Minute)),
			newTask("old", 3, base.Add(-2*time.Minute)),
			newTask("new", 1, base),
		})

		if got := taskIDs(sel.tasks); len(got) != 2 || got[0] != "new" || got[1] != "mid" {
			t.Fatalf("selected ids = %v, want [new mid]", got)
		}
		if sel.cappedPRs != 1 {
			t.Fatalf("cappedPRs = %d, want 1", sel.cappedPRs)
		}
	})

	t.Run("ineligible PR-linked tasks do not consume cap", func(t *testing.T) {
		t.Parallel()

		r := &Handler{
			cfg: &config.Config{
				GitHub: config.GitHubConfig{ReviewsMaxPRsPerTick: 2},
			},
		}

		done := newTask("done", 1, base)
		done.Status = task.StatusDone
		reviewTask := newTask("review-task", 3, base.Add(-2*time.Second))
		reviewTask.Tags = []string{"review"}

		sel := r.selectKnownPRPoll(context.Background(), []task.Task{
			done,
			reviewTask,
			newTask("eligible-new", 4, base.Add(-time.Minute)),
			newTask("eligible-mid", 5, base.Add(-2*time.Minute)),
			newTask("eligible-old", 6, base.Add(-3*time.Minute)),
		})

		if got := eligibleTaskIDs(sel.tasks); len(got) != 2 || got[0] != "eligible-new" || got[1] != "eligible-mid" {
			t.Fatalf("eligible selected ids = %v, want [eligible-new eligible-mid]", got)
		}
		if sel.selectedPRs != 2 || sel.cappedPRs != 1 {
			t.Fatalf("selection stats = %+v, want selected=2 capped=1", sel)
		}
	})

	t.Run("deferred countdown becomes eligible again", func(t *testing.T) {
		t.Parallel()

		r := &Handler{
			prSnapshots: PRSnapshotStore{entries: map[string]prSnapshot{
				"owner/repo#7": {skipTicks: 2},
			}},
		}
		tk := newTask("deferred", 7, base)

		for i, wantSkip := range []int{1, 0} {
			sel := r.selectKnownPRPoll(context.Background(), []task.Task{tk})
			if len(sel.tasks) != 0 {
				t.Fatalf("poll %d selected ids = %v, want none while deferred", i+1, taskIDs(sel.tasks))
			}
			if sel.deferredPRs != 1 {
				t.Fatalf("poll %d deferredPRs = %d, want 1", i+1, sel.deferredPRs)
			}
			if _, _, got, _ := r.prSnapshots.Backoff("owner/repo#7"); got != wantSkip {
				t.Fatalf("poll %d skipTicks = %d, want %d", i+1, got, wantSkip)
			}
		}

		sel := r.selectKnownPRPoll(context.Background(), []task.Task{tk})
		if got := taskIDs(sel.tasks); len(got) != 1 || got[0] != "deferred" {
			t.Fatalf("selected ids = %v, want [deferred] after countdown", got)
		}
	})

	// Backoff is keyed on the PR, so a task unblocked back into a fixable lane
	// would otherwise serve out a streak the PR earned while it was parked.
	t.Run("task status change breaks stable backoff", func(t *testing.T) {
		t.Parallel()

		unblockedAt := base.Add(time.Hour)
		probes := 0
		r := &Handler{
			prSnapshots: PRSnapshotStore{entries: map[string]prSnapshot{
				"owner/repo#7": {
					headSHA:         "sha-1",
					updatedAt:       "2026-07-13T12:00:00Z",
					stableStreak:    3,
					skipTicks:       4,
					statusChangedAt: base,
				},
			}},
			fetchHeadStateFn: func(string, int) (string, bool, string, error) {
				probes++
				return "sha-1", true, "2026-07-13T12:00:00Z", nil
			},
		}
		tk := newTask("unblocked", 7, unblockedAt)
		tk.StatusChangedAt = unblockedAt

		sel := r.selectKnownPRPoll(context.Background(), []task.Task{tk})
		if probes != 0 {
			t.Fatalf("fetchHeadStateFn calls = %d, want 0 — a status change selects without probing", probes)
		}
		if got := taskIDs(sel.tasks); len(got) != 1 || got[0] != "unblocked" {
			t.Fatalf("selected ids = %v, want [unblocked] on the tick after a status change", got)
		}
		if sel.deferredPRs != 0 {
			t.Fatalf("deferredPRs = %d, want 0", sel.deferredPRs)
		}
		if _, _, got, _ := r.prSnapshots.Backoff("owner/repo#7"); got != 0 {
			t.Fatalf("skipTicks = %d, want reset to 0", got)
		}

		// The same status must not keep bypassing the backoff on later ticks.
		r.prSnapshots.set("owner/repo#7", prSnapshot{
			headSHA:         "sha-1",
			updatedAt:       "2026-07-13T12:00:00Z",
			skipTicks:       2,
			statusChangedAt: unblockedAt,
		})
		sel = r.selectKnownPRPoll(context.Background(), []task.Task{tk})
		if len(sel.tasks) != 0 || sel.deferredPRs != 1 {
			t.Fatalf("second tick selected=%v deferred=%d, want none selected and 1 deferred", taskIDs(sel.tasks), sel.deferredPRs)
		}
	})

	// A deferred PR is absent from the selection, so it is also absent from the
	// `seen` set Prune keeps — its skip counters must be retained explicitly or
	// the backoff can never survive past the tick that armed it.
	t.Run("deferred and capped keys are retained for pruning", func(t *testing.T) {
		t.Parallel()

		r := &Handler{
			cfg:         &config.Config{GitHub: config.GitHubConfig{ReviewsMaxPRsPerTick: 1}},
			prSnapshots: PRSnapshotStore{entries: map[string]prSnapshot{"owner/repo#7": {skipTicks: 2}}},
		}
		deferredTask := newTask("deferred", 7, base)
		cappedA := newTask("capped-a", 8, base.Add(time.Hour))
		cappedB := newTask("capped-b", 9, base)

		sel := r.selectKnownPRPoll(context.Background(), []task.Task{deferredTask, cappedA, cappedB})
		if sel.deferredPRs != 1 || sel.cappedPRs != 1 {
			t.Fatalf("selection stats = %+v, want deferred=1 capped=1", sel)
		}
		want := map[string]bool{"owner/repo#7": true, "owner/repo#9": true}
		if len(sel.retainKeys) != len(want) {
			t.Fatalf("retainKeys = %v, want the deferred and capped keys", sel.retainKeys)
		}
		for _, key := range sel.retainKeys {
			if !want[key] {
				t.Fatalf("retainKeys = %v, unexpected key %q", sel.retainKeys, key)
			}
		}
	})

	// An older sibling task must not rewind the PR-keyed stamp, or the newer
	// one reads as "advanced" forever and the PR loses its request pacing.
	t.Run("older sibling task cannot rewind the status stamp", func(t *testing.T) {
		t.Parallel()

		probes := 0
		r := &Handler{
			prSnapshots: PRSnapshotStore{entries: map[string]prSnapshot{
				"owner/repo#7": {
					headSHA:         "sha-1",
					updatedAt:       "2026-07-13T12:00:00Z",
					skipTicks:       2,
					statusChangedAt: base,
				},
			}},
			fetchHeadStateFn: func(string, int) (string, bool, string, error) {
				probes++
				return "sha-1", true, "2026-07-13T12:00:00Z", nil
			},
		}
		newer := newTask("newer", 7, base.Add(2*time.Hour))
		newer.StatusChangedAt = base.Add(2 * time.Hour)
		older := newTask("older", 7, base.Add(time.Hour))
		older.StatusChangedAt = base.Add(time.Hour)

		if sel := r.selectKnownPRPoll(context.Background(), []task.Task{newer, older}); len(sel.tasks) != 2 {
			t.Fatalf("first tick selected = %v, want both tasks after the status change", taskIDs(sel.tasks))
		}
		if got := r.prSnapshots.entries["owner/repo#7"].statusChangedAt; !got.Equal(newer.StatusChangedAt) {
			t.Fatalf("stamp after tick 1 = %v, want the newer task's %v", got, newer.StatusChangedAt)
		}

		// Re-arm only the skip counter, leaving whatever stamp tick 1 wrote, so
		// a rewound stamp shows up as a PR that never backs off again.
		armed := r.prSnapshots.entries["owner/repo#7"]
		armed.skipTicks = 2
		r.prSnapshots.set("owner/repo#7", armed)

		sel := r.selectKnownPRPoll(context.Background(), []task.Task{newer, older})
		if len(sel.tasks) != 0 {
			t.Fatalf("second tick selected = %v, want none — the stamp must not rewind", taskIDs(sel.tasks))
		}
	})

	t.Run("updatedAt change breaks stable backoff", func(t *testing.T) {
		t.Parallel()

		tk := newTask("reviewed", 7, base)
		r := &Handler{
			prSnapshots: PRSnapshotStore{entries: map[string]prSnapshot{
				"owner/repo#7": {
					headSHA:      "sha-1",
					updatedAt:    "2026-07-13T12:00:00Z",
					stableStreak: 3,
					skipTicks:    4,
				},
			}},
			fetchHeadStateFn: func(repo string, number int) (string, bool, string, error) {
				if repo != "owner/repo" || number != 7 {
					t.Fatalf("fetchHeadStateFn(%q, %d), want owner/repo#7", repo, number)
				}
				return "sha-1", true, "2026-07-13T12:05:00Z", nil
			},
		}

		sel := r.selectKnownPRPoll(context.Background(), []task.Task{tk})
		if got := taskIDs(sel.tasks); len(got) != 1 || got[0] != "reviewed" {
			t.Fatalf("selected ids = %v, want [reviewed] when updatedAt changes", got)
		}
		if sel.deferredPRs != 0 {
			t.Fatalf("deferredPRs = %d, want 0", sel.deferredPRs)
		}
		if _, _, got, _ := r.prSnapshots.Backoff("owner/repo#7"); got != 0 {
			t.Fatalf("skipTicks = %d, want reset to 0", got)
		}
	})

	t.Run("head-state probe error preserves backoff", func(t *testing.T) {
		t.Parallel()

		tk := newTask("rate-limited", 7, base)
		r := &Handler{
			prSnapshots: PRSnapshotStore{entries: map[string]prSnapshot{
				"owner/repo#7": {
					headSHA:      "sha-1",
					updatedAt:    "2026-07-13T12:00:00Z",
					stableStreak: 3,
					skipTicks:    4,
				},
			}},
			fetchHeadStateFn: func(string, int) (string, bool, string, error) {
				return "", false, "", fmt.Errorf("rate limited")
			},
		}

		sel := r.selectKnownPRPoll(context.Background(), []task.Task{tk})
		if got := taskIDs(sel.tasks); len(got) != 0 {
			t.Fatalf("selected ids = %v, want none while probe error preserves backoff", got)
		}
		if sel.deferredPRs != 1 {
			t.Fatalf("deferredPRs = %d, want 1", sel.deferredPRs)
		}
		if _, _, got, _ := r.prSnapshots.Backoff("owner/repo#7"); got != 3 {
			t.Fatalf("skipTicks = %d, want decremented to 3", got)
		}
	})

	t.Run("prunes unlinked PRs", func(t *testing.T) {
		t.Parallel()

		r := &Handler{
			prSnapshots: PRSnapshotStore{entries: map[string]prSnapshot{
				"owner/repo#1": {},
				"owner/repo#2": {},
			}},
		}

		r.pruneKnownPRState(map[string]struct{}{"owner/repo#1": {}})
		if got := r.prSnapshots.Len(); got != 1 {
			t.Fatalf("len(prSnapshots) = %d, want 1", got)
		}
		if _, ok := r.prSnapshots.entries["owner/repo#1"]; !ok {
			t.Fatal("owner/repo#1 pruned, want retained")
		}
		if _, ok := r.prSnapshots.entries["owner/repo#2"]; ok {
			t.Fatal("owner/repo#2 retained, want pruned")
		}
	})

	t.Run("50 PR board simulation", func(t *testing.T) {
		t.Parallel()

		agents := newTestAgentManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())
		claim, ok := agents.TryClaimDispatch("active-1")
		if !ok {
			t.Fatal("TryClaimDispatch(active-1) = false, want true")
		}
		defer claim.Release()

		r := &Handler{
			agents: agents,
			cfg: &config.Config{
				GitHub: config.GitHubConfig{
					ReviewsMaxPRsPerTick:         25,
					ReviewsStableBackoffMaxTicks: 8,
				},
			},
		}

		tasks := make([]task.Task, 0, 50)
		for i := range 50 {
			tasks = append(tasks, newTask(
				taskIDForBoard(i),
				i+1,
				base.Add(time.Duration(-i)*time.Minute),
			))
		}
		tasks[0].ID = "active-1"

		sel := r.selectKnownPRPoll(context.Background(), tasks)
		if sel.selectedPRs != 25 {
			t.Fatalf("selectedPRs = %d, want 25", sel.selectedPRs)
		}
		if sel.cappedPRs != 25 {
			t.Fatalf("cappedPRs = %d, want 25", sel.cappedPRs)
		}
		if got := taskIDs(sel.tasks); len(got) != 25 || got[0] != "active-1" {
			t.Fatalf("selected ids len/head = %d/%q, want 25/active-1", len(got), firstID(got))
		}

		stablePR := github.PullRequest{HeadSHA: "sha-1", UpdatedAt: "2026-07-13T12:00:00Z"}
		r.noteKnownPRResult("owner/repo", 42, stablePR)
		wantSkips := [][2]int{{1, 1}, {1, 2}, {2, 4}, {4, 8}}
		for i, want := range wantSkips {
			r.noteKnownPRResult("owner/repo", 42, stablePR)
			if _, _, got, _ := r.prSnapshots.Backoff("owner/repo#42"); got < want[0] || got > want[1] {
				t.Fatalf("stable round %d skipTicks = %d, want [%d, %d]", i+1, got, want[0], want[1])
			}
		}

		changedPR := stablePR
		changedPR.HeadSHA = "sha-2"
		r.noteKnownPRResult("owner/repo", 42, changedPR)
		if _, _, got, _ := r.prSnapshots.Backoff("owner/repo#42"); got != 0 {
			t.Fatalf("skipTicks after head change = %d, want 0", got)
		}
	})
}

func taskIDs(tasks []task.Task) []string {
	ids := make([]string, len(tasks))
	for i := range tasks {
		ids[i] = tasks[i].ID
	}
	return ids
}

func eligibleTaskIDs(tasks []task.Task) []string {
	ids := make([]string, 0, len(tasks))
	for i := range tasks {
		if knownPRPollEligible(&tasks[i]) {
			ids = append(ids, tasks[i].ID)
		}
	}
	return ids
}

func taskIDForBoard(i int) string {
	return fmt.Sprintf("task-%02d", i)
}

func firstID(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}
