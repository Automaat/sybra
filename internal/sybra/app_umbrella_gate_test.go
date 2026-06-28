package sybra

import (
	"errors"
	"log/slog"
	"slices"
	"testing"

	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
)

var errTestClose = errors.New("close failed")

// mkTracker creates an umbrella tracker task carrying the given parallelism cap.
func mkTracker(t *testing.T, m *task.Manager, umb string, maxPar int) task.Task {
	t.Helper()
	tk, err := m.CreateFull("umbrella", "", task.AgentModeHeadless, task.Update{
		Issue:    task.Ptr(umb),
		TaskType: task.Ptr(task.TaskTypeUmbrella),
		Status:   task.Ptr(task.StatusInProgress),
		Tags:     task.Ptr([]string{"umbrella", umbrella.MaxParallelTag(maxPar)}),
	})
	if err != nil {
		t.Fatalf("create tracker: %v", err)
	}
	return tk
}

func TestReleaseUnblockedChildren_RespectsMaxParallel(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"
	mkTracker(t, m, umb, 2)

	c1 := mkChild(t, m, "c1", "Automaat/sybra#1", umb, nil, task.StatusTodo)
	c2 := mkChild(t, m, "c2", "Automaat/sybra#2", umb, nil, task.StatusTodo)
	c3 := mkChild(t, m, "c3", "Automaat/sybra#3", umb, nil, task.StatusTodo)

	app.releaseUnblockedChildren()

	released, held := 0, 0
	for _, id := range []string{c1.ID, c2.ID, c3.ID} {
		tk := mustTask(t, m, id)
		if tk.Status != task.StatusTodo {
			continue
		}
		if slices.Contains(tk.Tags, umbrellaGatedTag) {
			held++
		} else {
			released++
		}
	}
	if released != 2 || held != 1 {
		t.Fatalf("cap=2 should release 2 and hold 1, got released=%d held=%d", released, held)
	}
}

func TestReleaseUnblockedChildren_HaltChainFlagsTracker(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"
	tracker := mkTracker(t, m, umb, 5)

	// One child is stuck needing a human; an unrelated child is ready.
	stuck := mkChild(t, m, "stuck", "Automaat/sybra#1", umb, nil, task.StatusHumanRequired)
	indep := mkChild(t, m, "indep", "Automaat/sybra#2", umb, nil, task.StatusTodo)

	app.releaseUnblockedChildren()

	if got := mustStatus(t, m, tracker.ID); got != task.StatusHumanRequired {
		t.Fatalf("tracker = %q, want human-required on stuck child", got)
	}
	if got := mustStatus(t, m, stuck.ID); got != task.StatusHumanRequired {
		t.Fatalf("stuck child = %q, want human-required", got)
	}
	// Independent chains keep running.
	if got := mustStatus(t, m, indep.ID); got != task.StatusTodo {
		t.Fatalf("independent child = %q, want released to todo", got)
	}
}

func TestReleaseUnblockedChildren_PreservesBlockedTracker(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"
	tracker := mkTracker(t, m, umb, 5)
	reason := "auto-review: Sybra bug isolated (local task abc123; issue filing failed)"
	if _, err := m.Update(tracker.ID, task.Update{
		Status:       task.Ptr(task.StatusBlocked),
		StatusReason: task.Ptr(reason),
	}); err != nil {
		t.Fatalf("block tracker: %v", err)
	}
	mkChild(t, m, "stuck", "Automaat/sybra#1", umb, nil, task.StatusHumanRequired)

	app.releaseUnblockedChildren()

	got := mustTask(t, m, tracker.ID)
	if got.Status != task.StatusBlocked {
		t.Fatalf("tracker status = %q, want blocked", got.Status)
	}
	if got.StatusReason != reason {
		t.Fatalf("tracker status_reason = %q, want %q", got.StatusReason, reason)
	}
}

func TestReleaseUnblockedChildren_RollupClosesUmbrella(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	var gotRepo string
	var gotNum, closes int
	app.umbrellaCloseIssue = func(repo string, number int, _ string) error {
		gotRepo, gotNum, closes = repo, number, closes+1
		return nil
	}
	const umb = "https://github.com/Automaat/sybra/issues/100"
	tracker := mkTracker(t, m, umb, 5)
	mkChild(t, m, "c1", "Automaat/sybra#1", umb, nil, task.StatusDone)
	mkChild(t, m, "c2", "Automaat/sybra#2", umb, nil, task.StatusDone)

	app.releaseUnblockedChildren()

	if got := mustStatus(t, m, tracker.ID); got != task.StatusDone {
		t.Fatalf("tracker = %q, want done when all children done", got)
	}
	if closes != 1 || gotRepo != "Automaat/sybra" || gotNum != 100 {
		t.Fatalf("close = %d times repo=%q num=%d, want 1 Automaat/sybra 100", closes, gotRepo, gotNum)
	}
}

func TestTrackerRollup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		st              umbrellaState
		cyclic, settled bool
		want            task.Status
		wantClose       bool
	}{
		{"cycle", umbrellaState{total: 2}, true, true, task.StatusHumanRequired, false},
		{"stuck child", umbrellaState{total: 2, anyHR: true}, false, true, task.StatusHumanRequired, false},
		{"cancelled child", umbrellaState{total: 2, anyCancelled: true}, false, true, task.StatusHumanRequired, false},
		{"all done", umbrellaState{total: 2, doneCount: 2}, false, true, task.StatusDone, true},
		{"in progress", umbrellaState{total: 2, doneCount: 1}, false, true, task.StatusInProgress, false},
		{"zero children settled completes", umbrellaState{total: 0}, false, true, task.StatusDone, true},
		{"zero children not settled holds", umbrellaState{total: 0}, false, false, task.StatusInProgress, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := tt.st
			got, _, doClose := trackerRollup(&st, tt.cyclic, tt.settled)
			if got != tt.want || doClose != tt.wantClose {
				t.Fatalf("trackerRollup = (%q, close=%v), want (%q, close=%v)", got, doClose, tt.want, tt.wantClose)
			}
		})
	}
}

func TestReleaseUnblockedChildren_EmptyTrackerHeldUntilSettled(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	closes := 0
	app.umbrellaCloseIssue = func(string, int, string) error { closes++; return nil }
	const umb = "https://github.com/Automaat/sybra/issues/100"
	// Tracker with no children, created just now → not yet settled.
	tracker := mkTracker(t, m, umb, 5)

	app.releaseUnblockedChildren()

	// A freshly-created childless tracker must not be closed — its children may
	// still be materializing in the same expansion.
	if got := mustStatus(t, m, tracker.ID); got != task.StatusInProgress {
		t.Fatalf("tracker = %q, want in-progress until settled", got)
	}
	if closes != 0 {
		t.Fatalf("childless tracker closed %d times before settling", closes)
	}
}

func TestReleaseUnblockedChildren_CancelledChildSurfaces(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	closes := 0
	app.umbrellaCloseIssue = func(string, int, string) error { closes++; return nil }
	const umb = "https://github.com/Automaat/sybra/issues/100"
	tracker := mkTracker(t, m, umb, 5)
	mkChild(t, m, "c1", "Automaat/sybra#1", umb, nil, task.StatusDone)
	mkChild(t, m, "c2", "Automaat/sybra#2", umb, nil, task.StatusCancelled)

	app.releaseUnblockedChildren()

	// A cancelled child must surface for a human, never silently complete/close.
	if got := mustStatus(t, m, tracker.ID); got != task.StatusHumanRequired {
		t.Fatalf("tracker = %q, want human-required on cancelled child", got)
	}
	if closes != 0 {
		t.Fatalf("umbrella was closed %d times despite a cancelled child", closes)
	}
}

func TestReleaseUnblockedChildren_CloseFailureDefersDone(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	app.umbrellaCloseIssue = func(string, int, string) error { return errTestClose }
	const umb = "https://github.com/Automaat/sybra/issues/100"
	tracker := mkTracker(t, m, umb, 5)
	mkChild(t, m, "c1", "Automaat/sybra#1", umb, nil, task.StatusDone)

	app.releaseUnblockedChildren()

	// Close failed transiently → tracker must NOT flip to done (so the close
	// retries next tick rather than orphaning the open issue).
	if got := mustStatus(t, m, tracker.ID); got != task.StatusInProgress {
		t.Fatalf("tracker = %q, want in-progress held for close retry", got)
	}
}

func newUmbrellaGateApp(t *testing.T) (*App, *task.Manager) {
	t.Helper()
	dir := t.TempDir()
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tasks := task.NewManager(store, task.EmitterFunc(func(string, any) {}))
	app := &App{tasks: tasks, logger: slog.New(slog.DiscardHandler)}
	return app, tasks
}

// mkChild creates a gate-managed umbrella child task in the given status,
// carrying the gating marker tag the expander would set.
func mkChild(t *testing.T, m *task.Manager, title, issue, umb string, deps []string, status task.Status) task.Task {
	t.Helper()
	tk, err := m.CreateFull(title, "", task.AgentModeHeadless, task.Update{
		Issue:         task.Ptr(issue),
		UmbrellaIssue: task.Ptr(umb),
		DependsOn:     task.Ptr(deps),
		Status:        task.Ptr(status),
		Tags:          task.Ptr([]string{umbrellaGatedTag}),
	})
	if err != nil {
		t.Fatalf("CreateFull(%s): %v", title, err)
	}
	return tk
}

func mustStatus(t *testing.T, m *task.Manager, id string) task.Status {
	t.Helper()
	return mustTask(t, m, id).Status
}

func mustTask(t *testing.T, m *task.Manager, id string) task.Task {
	t.Helper()
	tk, err := m.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return tk
}

func TestReleaseUnblockedChildren_ReleasesRootWithNoDeps(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"

	root := mkChild(t, m, "root", "Automaat/sybra#1", umb, nil, task.StatusTodo)

	app.releaseUnblockedChildren()

	released, err := m.Get(root.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if released.Status != task.StatusTodo {
		t.Fatalf("root status = %q, want %q", released.Status, task.StatusTodo)
	}
	// The gating marker must be stripped on release so a later re-block cannot
	// retrigger a release.
	if slices.Contains(released.Tags, umbrellaGatedTag) {
		t.Fatalf("gating tag not stripped on release: tags=%v", released.Tags)
	}
}

func TestReleaseUnblockedChildren_IgnoresSybraBugBlock(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"

	// A dependency that has merged.
	mkChild(t, m, "dep", "Automaat/sybra#1", umb, nil, task.StatusDone)
	// An umbrella child that ran, hit a Sybra bug, and was parked in `blocked`
	// by human-review WITHOUT the gating marker. Its dep is done, but the gate
	// must not resurrect it.
	bug, err := m.CreateFull("buggy", "", task.AgentModeHeadless, task.Update{
		Issue:          task.Ptr("Automaat/sybra#2"),
		UmbrellaIssue:  task.Ptr(umb),
		DependsOn:      task.Ptr([]string{"Automaat/sybra#1"}),
		Status:         task.Ptr(task.StatusBlocked),
		BlockedByIssue: task.Ptr("https://github.com/Automaat/sybra/issues/500"),
	})
	if err != nil {
		t.Fatalf("create buggy: %v", err)
	}

	app.releaseUnblockedChildren()

	if got := mustStatus(t, m, bug.ID); got != task.StatusBlocked {
		t.Fatalf("sybra-bug-blocked child was released to %q, want blocked", got)
	}
}

func TestReleaseUnblockedChildren_HoldsThenReleasesChain(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"

	// dep is still in progress; child depends on it.
	dep := mkChild(t, m, "dep", "Automaat/sybra#1", umb, nil, task.StatusInProgress)
	child := mkChild(t, m, "child", "Automaat/sybra#2", umb, []string{"Automaat/sybra#1"}, task.StatusTodo)

	app.releaseUnblockedChildren()
	childTask := mustTask(t, m, child.ID)
	if childTask.Status != task.StatusTodo || !slices.Contains(childTask.Tags, umbrellaGatedTag) {
		t.Fatalf("child released early: status = %q tags = %v, want todo+%s", childTask.Status, childTask.Tags, umbrellaGatedTag)
	}

	// Finish the dependency, then the child must release.
	if _, err := m.Update(dep.ID, task.Update{Status: task.Ptr(task.StatusDone)}); err != nil {
		t.Fatalf("finish dep: %v", err)
	}
	app.releaseUnblockedChildren()
	if got := mustStatus(t, m, child.ID); got != task.StatusTodo {
		t.Fatalf("child status = %q, want %q after dep done", got, task.StatusTodo)
	}
}

func TestReleaseUnblockedChildren_CrossFormDependencyResolves(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"

	// Dependency recorded as a full URL; the child references it in shorthand.
	mkChild(t, m, "dep", "https://github.com/Automaat/sybra/issues/1", umb, nil, task.StatusDone)
	child := mkChild(t, m, "child", "Automaat/sybra#2", umb, []string{"Automaat/sybra#1"}, task.StatusTodo)

	app.releaseUnblockedChildren()
	if got := mustStatus(t, m, child.ID); got != task.StatusTodo {
		t.Fatalf("child status = %q, want %q (cross-form dep should resolve)", got, task.StatusTodo)
	}
}

func TestReleaseUnblockedChildren_CycleFlagsTracker(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"

	// Tracker task for the umbrella.
	tracker, err := m.CreateFull("umbrella", "", task.AgentModeHeadless, task.Update{
		Issue:    task.Ptr(umb),
		TaskType: task.Ptr(task.TaskTypeUmbrella),
		Status:   task.Ptr(task.StatusInProgress),
	})
	if err != nil {
		t.Fatalf("create tracker: %v", err)
	}

	// x <-> y mutual dependency.
	x := mkChild(t, m, "x", "Automaat/sybra#1", umb, []string{"Automaat/sybra#2"}, task.StatusTodo)
	y := mkChild(t, m, "y", "Automaat/sybra#2", umb, []string{"Automaat/sybra#1"}, task.StatusTodo)

	app.releaseUnblockedChildren()

	if got := mustStatus(t, m, tracker.ID); got != task.StatusHumanRequired {
		t.Fatalf("tracker status = %q, want %q on cycle", got, task.StatusHumanRequired)
	}
	// Cyclic children stay held (todo + gated tag, not released).
	if got := mustStatus(t, m, x.ID); got != task.StatusTodo {
		t.Fatalf("x status = %q, want todo (held gated)", got)
	}
	if got := mustStatus(t, m, y.ID); got != task.StatusTodo {
		t.Fatalf("y status = %q, want todo (held gated)", got)
	}
}

func TestReleaseUnblockedChildren_NoUmbrellaNoOp(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)

	// A plain blocked task with no umbrella must be left untouched.
	tk, err := m.CreateFull("plain", "", task.AgentModeHeadless, task.Update{
		Status:         task.Ptr(task.StatusBlocked),
		BlockedByIssue: task.Ptr("https://github.com/Automaat/sybra/issues/9"),
	})
	if err != nil {
		t.Fatalf("create plain: %v", err)
	}

	app.releaseUnblockedChildren()
	if got := mustStatus(t, m, tk.ID); got != task.StatusBlocked {
		t.Fatalf("plain blocked task changed to %q, want blocked", got)
	}
}
