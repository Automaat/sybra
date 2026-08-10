package clusterlead

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/task/taskdb"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

// TestApplyFollowerTaskWritesSidecarFilesOnFileBackend pins writeSidecars'
// existing behavior for the file backend: PutFn's plain whole-task write
// never touches sidecar files, so the mirror's compensating direct write is
// still required and must still happen.
func TestApplyFollowerTaskWritesSidecarFilesOnFileBackend(t *testing.T) {
	cfg := leaderConfig("http://unused", []string{"owner/pet"})
	roster, _ := NewRoster(cfg, nil)
	mgr := newManager(t)
	mirror := NewMirror(cfg, mgr, roster, nil, time.Second)

	if !mirror.applyFollowerTask("pet-box", task.Task{
		ID: "task-abc123", Title: "legit", Status: task.StatusTodo,
		ProjectID: "owner/pet", UpdatedAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
		Plan: "plan body", CodeReview: "review body",
	}) {
		t.Fatal("a valid follower task must still adopt")
	}

	dir := mgr.Store().Dir()
	planFile := filepath.Join(dir, "task-abc123.plan.md")
	if data, err := os.ReadFile(planFile); err != nil {
		t.Fatalf("plan sidecar file not written: %v", err)
	} else if string(data) != "plan body" {
		t.Fatalf("plan sidecar content = %q, want %q", data, "plan body")
	}
	reviewFile := filepath.Join(dir, "task-abc123.review.md")
	if data, err := os.ReadFile(reviewFile); err != nil {
		t.Fatalf("code-review sidecar file not written: %v", err)
	} else if string(data) != "review body" {
		t.Fatalf("code-review sidecar content = %q, want %q", data, "review body")
	}
}

// TestApplyFollowerTaskSkipsSidecarFilesOnDatabaseBackend proves the DB
// backend both gets the sidecar content (already persisted by PutBy's own
// SidecarsFromTask, in the same transaction as the task write) and does NOT
// also get a stray, never-read sidecar file written to the local tasks
// directory the way it would before this fix.
func TestApplyFollowerTaskSkipsSidecarFilesOnDatabaseBackend(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		sqlStore, err := taskdb.NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		fileStore, err := task.NewStore(t.TempDir())
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		mgr := task.NewManagerWithPersistence(fileStore, taskdb.NewPersistence(sqlStore), nil)
		if mgr.PersistsToFile() {
			t.Fatal("test setup: expected a DB-backed Manager")
		}

		cfg := leaderConfig("http://unused", []string{"owner/pet"})
		roster, _ := NewRoster(cfg, nil)
		mirror := NewMirror(cfg, mgr, roster, nil, time.Second)

		if !mirror.applyFollowerTask("pet-box", task.Task{
			ID: "task-abc123", Title: "legit", Status: task.StatusTodo,
			ProjectID: "owner/pet", UpdatedAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
			Plan: "plan body", CodeReview: "review body",
		}) {
			t.Fatal("a valid follower task must still adopt")
		}

		got, err := mgr.Get("task-abc123")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Plan != "plan body" {
			t.Fatalf("Plan = %q, want %q (must already be persisted by PutBy, not the skipped writeSidecars)", got.Plan, "plan body")
		}
		if got.CodeReview != "review body" {
			t.Fatalf("CodeReview = %q, want %q", got.CodeReview, "review body")
		}

		dir := fileStore.Dir()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read tasks dir: %v", err)
		}
		if len(entries) != 0 {
			names := make([]string, len(entries))
			for i, e := range entries {
				names[i] = e.Name()
			}
			t.Fatalf("tasks dir has stray files on the DB backend: %v", names)
		}
	})
}

// TestApplyFollowerTaskWritesPlanDraftFilesOnFileBackend is #3297's
// regression test: a follower's plan drafts must reach the leader on first
// adoption, both in the returned Task's PlanDrafts map (Merge) and as
// sidecar files on the file backend (writeSidecars) — before the fix,
// Merge never copied the field at all, so the leader's map stayed empty
// regardless of what writeSidecars did.
func TestApplyFollowerTaskWritesPlanDraftFilesOnFileBackend(t *testing.T) {
	cfg := leaderConfig("http://unused", []string{"owner/pet"})
	roster, _ := NewRoster(cfg, nil)
	mgr := newManager(t)
	mirror := NewMirror(cfg, mgr, roster, nil, time.Second)

	if !mirror.applyFollowerTask("pet-box", task.Task{
		ID: "task-abc123", Title: "legit", Status: task.StatusTodo,
		ProjectID: "owner/pet", UpdatedAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
		PlanDrafts: map[string]string{"alpha": "draft content"},
	}) {
		t.Fatal("a valid follower task must still adopt")
	}

	got, err := mgr.Get("task-abc123")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.PlanDrafts["alpha"] != "draft content" {
		t.Fatalf("PlanDrafts = %+v, want alpha=draft content", got.PlanDrafts)
	}

	draftFile := filepath.Join(mgr.Store().Dir(), "task-abc123.plan-draft-alpha.md")
	if data, err := os.ReadFile(draftFile); err != nil {
		t.Fatalf("plan draft sidecar file not written: %v", err)
	} else if string(data) != "draft content" {
		t.Fatalf("plan draft sidecar content = %q, want %q", data, "draft content")
	}
}

// TestApplyFollowerTaskMergeDropsPlanDraftsRemovedByFollower proves a later
// merge that reports a smaller draft set (e.g. the follower re-planned and
// deleted its old drafts) removes the sidecar file the leader no longer
// should be serving, not just add/update the ones still present.
func TestApplyFollowerTaskMergeDropsPlanDraftsRemovedByFollower(t *testing.T) {
	cfg := leaderConfig("http://unused", []string{"owner/pet"})
	roster, _ := NewRoster(cfg, nil)
	mgr := newManager(t)
	mirror := NewMirror(cfg, mgr, roster, nil, time.Second)

	if !mirror.applyFollowerTask("pet-box", task.Task{
		ID: "task-abc123", Title: "legit", Status: task.StatusTodo,
		ProjectID: "owner/pet", UpdatedAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
		PlanDrafts: map[string]string{"alpha": "first attempt", "beta": "first attempt"},
	}) {
		t.Fatal("first adopt must succeed")
	}
	dir := mgr.Store().Dir()
	if _, err := os.Stat(filepath.Join(dir, "task-abc123.plan-draft-beta.md")); err != nil {
		t.Fatalf("test setup: beta sidecar file missing: %v", err)
	}

	if !mirror.applyFollowerTask("pet-box", task.Task{
		ID: "task-abc123", Title: "legit", Status: task.StatusTodo,
		ProjectID: "owner/pet", UpdatedAt: time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC),
		PlanDrafts: map[string]string{"alpha": "second attempt"},
	}) {
		t.Fatal("later merge must succeed")
	}

	got, err := mgr.Get("task-abc123")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.PlanDrafts["alpha"] != "second attempt" {
		t.Fatalf("PlanDrafts[alpha] = %q, want %q", got.PlanDrafts["alpha"], "second attempt")
	}
	if _, ok := got.PlanDrafts["beta"]; ok {
		t.Fatalf("PlanDrafts = %+v, want no beta entry after the follower dropped it", got.PlanDrafts)
	}
	if _, err := os.Stat(filepath.Join(dir, "task-abc123.plan-draft-beta.md")); !os.IsNotExist(err) {
		t.Fatalf("beta sidecar file still exists after the follower dropped it: err=%v", err)
	}
}

// TestApplyFollowerTaskPlanDraftsRoundTripOnDatabaseBackend mirrors
// TestApplyFollowerTaskSkipsSidecarFilesOnDatabaseBackend for PlanDrafts:
// the map must reach the database via Merge's own field copy (PutBy then
// persists it through SidecarsFromTask) with no stray sidecar file written
// to the local tasks directory writeSidecars is skipped for.
func TestApplyFollowerTaskPlanDraftsRoundTripOnDatabaseBackend(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		sqlStore, err := taskdb.NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		fileStore, err := task.NewStore(t.TempDir())
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		mgr := task.NewManagerWithPersistence(fileStore, taskdb.NewPersistence(sqlStore), nil)
		if mgr.PersistsToFile() {
			t.Fatal("test setup: expected a DB-backed Manager")
		}

		cfg := leaderConfig("http://unused", []string{"owner/pet"})
		roster, _ := NewRoster(cfg, nil)
		mirror := NewMirror(cfg, mgr, roster, nil, time.Second)

		if !mirror.applyFollowerTask("pet-box", task.Task{
			ID: "task-abc123", Title: "legit", Status: task.StatusTodo,
			ProjectID: "owner/pet", UpdatedAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
			PlanDrafts: map[string]string{"alpha": "draft content"},
		}) {
			t.Fatal("a valid follower task must still adopt")
		}

		got, err := mgr.Get("task-abc123")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.PlanDrafts["alpha"] != "draft content" {
			t.Fatalf("PlanDrafts = %+v, want alpha=draft content", got.PlanDrafts)
		}

		entries, err := os.ReadDir(fileStore.Dir())
		if err != nil {
			t.Fatalf("read tasks dir: %v", err)
		}
		if len(entries) != 0 {
			names := make([]string, len(entries))
			for i, e := range entries {
				names[i] = e.Name()
			}
			t.Fatalf("tasks dir has stray files on the DB backend: %v", names)
		}
	})
}

// TestWriteMergedSidecarsSkipsStaleSupersededMerge is #3308's regression
// test for the ordering half of the gap: two writers merging the same task
// concurrently (the periodic mirror and a request-triggered forward, or two
// request-triggered forwards) have no lock ordering their sidecar writes
// relative to each other, only Merge's staleness guard ordering which one
// wins the primary record. A write carrying an older MirrorUpdatedAt than
// what's already on the task must not clobber a newer write's content.
func TestWriteMergedSidecarsSkipsStaleSupersededMerge(t *testing.T) {
	tasks := newManager(t)
	older := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC)

	newerTask := task.Task{ID: "task-stale-merge", Plan: "newer content", MirrorUpdatedAt: &newer}
	if _, _, err := tasks.Put(newerTask); err != nil {
		t.Fatalf("seed newer task: %v", err)
	}
	if err := WriteMergedSidecars(tasks, newerTask); err != nil {
		t.Fatalf("WriteMergedSidecars(newer): %v", err)
	}

	staleTask := task.Task{ID: "task-stale-merge", Plan: "stale content", MirrorUpdatedAt: &older}
	if err := WriteMergedSidecars(tasks, staleTask); err != nil {
		t.Fatalf("WriteMergedSidecars(stale): %v", err)
	}

	planFile := filepath.Join(tasks.Store().Dir(), "task-stale-merge.plan.md")
	data, err := os.ReadFile(planFile)
	if err != nil {
		t.Fatalf("plan sidecar file missing: %v", err)
	}
	if string(data) != "newer content" {
		t.Fatalf("plan sidecar content = %q, want %q — the stale write must not have overwritten it", data, "newer content")
	}
}

// stubSidecarWriteSeams overrides WriteMergedSidecars' sleep and
// once-write seams for the duration of the test, restoring both on
// cleanup. Using injected function values instead of a real filesystem
// permission error and a real timer makes attempt counts and mid-retry
// interleaving assertable directly, with no wall-clock race.
func stubSidecarWriteSeams(t *testing.T, sleep func(time.Duration), once func(*task.Store, task.Task) error) {
	t.Helper()
	origSleep, origOnce := sidecarWriteSleep, doWriteMergedSidecarsOnce
	sidecarWriteSleep, doWriteMergedSidecarsOnce = sleep, once
	t.Cleanup(func() { sidecarWriteSleep, doWriteMergedSidecarsOnce = origSleep, origOnce })
}

// TestWriteMergedSidecarsRetriesTransientFailure is #3308's regression test
// for the failure half of the gap: a write that fails once must not leave
// the sidecar content permanently missing — WriteMergedSidecars retries a
// bounded number of times before giving up, and every write it does is
// idempotent so a retry cannot double-apply anything. The once-write seam
// fails exactly the first call and succeeds after, so the assertion on
// attempt count is load-bearing rather than a guess about real I/O timing.
func TestWriteMergedSidecarsRetriesTransientFailure(t *testing.T) {
	tasks := newManager(t)
	tsk := task.Task{ID: "task-retry", Plan: "plan body"}
	if _, _, err := tasks.Put(tsk); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	var attempts, sleeps int
	stubSidecarWriteSeams(t,
		func(time.Duration) { sleeps++ },
		func(store *task.Store, tk task.Task) error {
			attempts++
			if attempts == 1 {
				return errors.New("injected transient failure")
			}
			return writeMergedSidecarsOnce(store, tk)
		})

	if err := WriteMergedSidecars(tasks, tsk); err != nil {
		t.Fatalf("WriteMergedSidecars did not recover from a transient failure: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("write attempts = %d, want 2 (one failure, one success)", attempts)
	}
	if sleeps != 1 {
		t.Fatalf("sleeps = %d, want 1 (between attempt 1 and attempt 2)", sleeps)
	}

	planFile := filepath.Join(tasks.Store().Dir(), "task-retry.plan.md")
	data, err := os.ReadFile(planFile)
	if err != nil {
		t.Fatalf("plan sidecar file not written after retry: %v", err)
	}
	if string(data) != "plan body" {
		t.Fatalf("plan sidecar content = %q, want %q", data, "plan body")
	}
}

// TestWriteMergedSidecarsExhaustsRetriesOnPermanentFailure proves the retry
// loop is bounded — a write that never recovers must still return an error
// rather than retrying forever or silently succeeding.
func TestWriteMergedSidecarsExhaustsRetriesOnPermanentFailure(t *testing.T) {
	tasks := newManager(t)
	tsk := task.Task{ID: "task-permanent-fail", Plan: "plan body"}
	if _, _, err := tasks.Put(tsk); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	var attempts, sleeps int
	injected := errors.New("permanent failure")
	stubSidecarWriteSeams(t,
		func(time.Duration) { sleeps++ },
		func(*task.Store, task.Task) error {
			attempts++
			return injected
		})

	err := WriteMergedSidecars(tasks, tsk)
	if !errors.Is(err, injected) {
		t.Fatalf("err = %v, want it to wrap the injected failure", err)
	}
	if attempts != sidecarWriteMaxAttempts {
		t.Fatalf("write attempts = %d, want %d", attempts, sidecarWriteMaxAttempts)
	}
	if sleeps != sidecarWriteMaxAttempts-1 {
		t.Fatalf("sleeps = %d, want %d", sleeps, sidecarWriteMaxAttempts-1)
	}
}

// TestWriteMergedSidecarsRetryDetectsSupersedingMergeMidRetry is #3308's
// regression test for the gap adversarial review found in the first version
// of this fix: checking freshness once before the retry loop, rather than
// before every attempt, left the loop free to write stale content anyway if
// a genuinely newer merge landed during a retry's sleep. The sleep seam
// simulates exactly that: while writer A (stale) is "asleep" between its
// failed first attempt and its retry, writer B (fresh) completes its own
// merge and sidecar write. A's retry must then detect it has been
// superseded and back off instead of overwriting B's content.
func TestWriteMergedSidecarsRetryDetectsSupersedingMergeMidRetry(t *testing.T) {
	tasks := newManager(t)
	older := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC)

	staleTask := task.Task{ID: "task-mid-retry-race", Plan: "stale content", MirrorUpdatedAt: &older}
	if _, _, err := tasks.Put(staleTask); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	var attempts int
	stubSidecarWriteSeams(t,
		func(time.Duration) {
			// Writer B's entire merge-and-write lands here, mid-sleep.
			freshTask := task.Task{ID: "task-mid-retry-race", Plan: "fresh content", MirrorUpdatedAt: &newer}
			if _, _, err := tasks.Put(freshTask); err != nil {
				t.Fatalf("writer B primary commit: %v", err)
			}
			if err := writeMergedSidecarsOnce(tasks.Store(), freshTask); err != nil {
				t.Fatalf("writer B sidecar write: %v", err)
			}
		},
		func(store *task.Store, tk task.Task) error {
			attempts++
			if attempts == 1 {
				return errors.New("injected transient failure")
			}
			return writeMergedSidecarsOnce(store, tk)
		})

	if err := WriteMergedSidecars(tasks, staleTask); err != nil {
		t.Fatalf("WriteMergedSidecars(stale): %v", err)
	}
	// The freshness check must catch the supersession before the retry's
	// actual write runs, so the write function itself is called at most
	// once (the initial failing attempt) — never a second time with stale
	// content.
	if attempts != 1 {
		t.Fatalf("write attempts = %d, want 1 — the retry must have been skipped once superseded", attempts)
	}

	planFile := filepath.Join(tasks.Store().Dir(), "task-mid-retry-race.plan.md")
	data, err := os.ReadFile(planFile)
	if err != nil {
		t.Fatalf("plan sidecar file missing: %v", err)
	}
	if string(data) != "fresh content" {
		t.Fatalf("plan sidecar content = %q, want %q — writer A's stale retry must not have won", data, "fresh content")
	}
}
