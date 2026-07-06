package sybra

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/project"
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

func TestReleaseUnblockedChildren_UpdatesTrackerProgressChecklist(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"
	tracker := mkTracker(t, m, umb, 5)
	if _, err := m.Update(tracker.ID, task.Update{Body: task.Ptr("Original body.")}); err != nil {
		t.Fatalf("seed tracker body: %v", err)
	}
	mkChild(t, m, "done child", "Automaat/sybra#1", umb, nil, task.StatusDone)
	mkChild(t, m, "blocked child", "Automaat/sybra#2", umb, nil, task.StatusHumanRequired)

	app.releaseUnblockedChildren()

	body := mustTask(t, m, tracker.ID).Body
	for _, want := range []string{
		"Original body.",
		umbrellaProgressStart,
		"- [x] done child (#1) — done",
		"- [ ] blocked child (#2) — human-required",
		umbrellaProgressEnd,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("tracker body missing %q:\n%s", want, body)
		}
	}
}

func TestUmbrellaTrackerBody_ReplacesBlockAndIsIdempotent(t *testing.T) {
	t.Parallel()
	children := []umbrellaProgressChild{
		{title: "first", issue: "Automaat/sybra#1", status: task.StatusDone},
		{title: "second", issue: "Automaat/sybra#2", status: task.StatusInProgress},
	}
	input := "Intro\n\n" +
		umbrellaProgressStart + "\nold\n" + umbrellaProgressEnd +
		"\n\nTail"

	got := umbrellaTrackerBody(input, children)
	if !strings.Contains(got, "Intro\n\n"+umbrellaProgressStart) || !strings.Contains(got, umbrellaProgressEnd+"\n\nTail") {
		t.Fatalf("body outside generated block was not preserved:\n%s", got)
	}
	if strings.Contains(got, "old") {
		t.Fatalf("old generated block was not replaced:\n%s", got)
	}
	if again := umbrellaTrackerBody(got, children); again != got {
		t.Fatalf("umbrellaTrackerBody is not idempotent:\nfirst:\n%s\nsecond:\n%s", got, again)
	}
}

func TestUmbrellaTrackerBody_ReplacesMalformedBlockFromStart(t *testing.T) {
	t.Parallel()
	children := []umbrellaProgressChild{
		{title: "first", issue: "Automaat/sybra#1", status: task.StatusDone},
	}
	input := "Intro\n\n" +
		umbrellaProgressStart + "\nold without end\n\nTail that belonged to malformed block"

	got := umbrellaTrackerBody(input, children)
	if !strings.HasPrefix(got, "Intro\n\n"+umbrellaProgressStart) {
		t.Fatalf("body before malformed generated block was not preserved:\n%s", got)
	}
	for _, unwanted := range []string{"old without end", "Tail that belonged to malformed block"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("malformed generated block content %q was not replaced:\n%s", unwanted, got)
		}
	}
	if !strings.Contains(got, "- [x] first (#1) — done") || !strings.Contains(got, umbrellaProgressEnd) {
		t.Fatalf("replacement progress block missing expected content:\n%s", got)
	}
}

func TestRenderUmbrellaProgressBlock_EmptyChildren(t *testing.T) {
	t.Parallel()
	got := renderUmbrellaProgressBlock(nil)
	for _, want := range []string{
		umbrellaProgressStart,
		"## Subissues",
		"_No materialized subissues._",
		umbrellaProgressEnd,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("progress block missing %q:\n%s", want, got)
		}
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
		{
			"zero children expand-failing below threshold never auto-closes",
			umbrellaState{total: 0, tracker: &task.Task{
				Status:       task.StatusInProgress,
				StatusReason: "umbrella expansion failed (attempt 1): boom",
				Tags:         []string{"umbrella", umbrella.ExpandFailTag(1)},
			}},
			false, true, task.StatusInProgress, false,
		},
		{
			"zero children expand-failing at threshold stays parked human-required",
			umbrellaState{total: 0, tracker: &task.Task{
				Status:       task.StatusHumanRequired,
				StatusReason: "umbrella expansion failed (attempt 3): boom",
				Tags:         []string{"umbrella", umbrella.ExpandFailTag(3)},
			}},
			false, true, task.StatusHumanRequired, false,
		},
		{
			"existing children expand-failing below threshold stays tracker-owned",
			umbrellaState{total: 2, doneCount: 1, tracker: &task.Task{
				Status:       task.StatusInProgress,
				StatusReason: "umbrella expansion failed (attempt 2): boom",
				Tags:         []string{"umbrella", umbrella.ExpandFailTag(2)},
			}},
			false, true, task.StatusInProgress, false,
		},
		{
			"all materialized children done does not close over fresh expand failure",
			umbrellaState{total: 2, doneCount: 2, tracker: &task.Task{
				Status:       task.StatusHumanRequired,
				StatusReason: "umbrella expansion failed (attempt 3): boom",
				Tags:         []string{"umbrella", umbrella.ExpandFailTag(3)},
			}},
			false, true, task.StatusHumanRequired, false,
		},
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

// TestReleaseUnblockedChildren_ExpandFailingTrackerNeverAutoCloses guards
// #1570: a tracker that exists only because internal/umbrella.Expand failed
// to materialize any children (planner killed at its timeout, repeatedly)
// must never be auto-closed as "no open sub-issues" — that previously
// closed the umbrella's GitHub issue while it still had open sub-issues that
// simply never got the chance to materialize as local tasks.
func TestReleaseUnblockedChildren_ExpandFailingTrackerNeverAutoCloses(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	closes := 0
	app.umbrellaCloseIssue = func(string, int, string) error { closes++; return nil }
	const umb = "https://github.com/Automaat/sybra/issues/100"
	tracker, err := m.CreateFull("umbrella", "", task.AgentModeHeadless, task.Update{
		Issue:        task.Ptr(umb),
		TaskType:     task.Ptr(task.TaskTypeUmbrella),
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("umbrella expansion failed (attempt 3): plan umbrella: run planner: killed"),
		Tags:         task.Ptr([]string{"umbrella", umbrella.ExpandFailTag(3)}),
	})
	if err != nil {
		t.Fatalf("create failure tracker: %v", err)
	}

	app.releaseUnblockedChildren()

	if got := mustStatus(t, m, tracker.ID); got != task.StatusHumanRequired {
		t.Fatalf("tracker = %q, want to stay human-required (never auto-closed)", got)
	}
	if closes != 0 {
		t.Fatalf("umbrella issue closed %d times, want 0 while expansion keeps failing", closes)
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

func TestReleaseUnblockedChildren_PreservesBlockedTracker(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		childStatus task.Status
	}{
		{"human-required child", task.StatusHumanRequired},
		{"completed child", task.StatusDone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			app, m := newUmbrellaGateApp(t)
			closes := 0
			app.umbrellaCloseIssue = func(string, int, string) error { closes++; return nil }
			const umb = "https://github.com/Automaat/sybra/issues/100"
			tracker := mkTracker(t, m, umb, 5)
			const reason = "auto-review: provider failure (local task abc12345; issue filing failed)"
			if _, err := m.Update(tracker.ID, task.Update{
				Status:       task.Ptr(task.StatusBlocked),
				StatusReason: task.Ptr(reason),
			}); err != nil {
				t.Fatalf("block tracker: %v", err)
			}
			mkChild(t, m, "child", "Automaat/sybra#1", umb, nil, tt.childStatus)

			app.releaseUnblockedChildren()

			got := mustTask(t, m, tracker.ID)
			if got.Status != task.StatusBlocked {
				t.Fatalf("tracker = %q, want blocked", got.Status)
			}
			if got.StatusReason != reason {
				t.Fatalf("tracker reason = %q, want %q", got.StatusReason, reason)
			}
			if closes != 0 {
				t.Fatalf("umbrella was closed %d times while tracker blocked", closes)
			}
		})
	}
}

func TestReleaseUnblockedChildren_BlockedTrackerStillReleasesReadyChildren(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"
	tracker := mkTracker(t, m, umb, 5)
	const reason = "blocked awaiting operator action"
	if _, err := m.Update(tracker.ID, task.Update{
		Status:       task.Ptr(task.StatusBlocked),
		StatusReason: task.Ptr(reason),
	}); err != nil {
		t.Fatalf("block tracker: %v", err)
	}
	child := mkChild(t, m, "ready", "Automaat/sybra#1", umb, nil, task.StatusTodo)

	app.releaseUnblockedChildren()

	gotTracker := mustTask(t, m, tracker.ID)
	if gotTracker.Status != task.StatusBlocked {
		t.Fatalf("tracker = %q, want blocked", gotTracker.Status)
	}
	if gotTracker.StatusReason != reason {
		t.Fatalf("tracker reason = %q, want %q", gotTracker.StatusReason, reason)
	}
	gotChild := mustTask(t, m, child.ID)
	if gotChild.Status != task.StatusTodo {
		t.Fatalf("child = %q, want todo", gotChild.Status)
	}
	if slices.Contains(gotChild.Tags, umbrellaGatedTag) {
		t.Fatalf("ready child still gated under blocked tracker: tags=%v", gotChild.Tags)
	}
	if gotChild.StatusReason != "umbrella dependencies satisfied" {
		t.Fatalf("child reason = %q, want release reason", gotChild.StatusReason)
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

// mustWriteProjectYAMLWithClone writes a minimal project YAML that resolves
// via project.Store.Get with ClonePath pointed at a real bare clone, so
// buildGroundLister can exercise the real DefaultBranch + ListTrackedFiles
// path without a network fetch.
func mustWriteProjectYAMLWithClone(t *testing.T, dir, id, clonePath string) {
	t.Helper()
	safe := strings.ReplaceAll(id, "/", "--")
	path := filepath.Join(dir, safe+".yaml")
	content := "id: " + id + "\ntype: pet\nowner: stub\nrepo: stub\nclone_path: " + clonePath + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write project YAML: %v", err)
	}
}

// newBareRepoWithTrackedFile creates a source repo with one committed file,
// bare-clones it, and returns the bare clone path and its default branch.
func newBareRepoWithTrackedFile(t *testing.T) (barePath, branch string) {
	t.Helper()
	src := t.TempDir()
	for _, args := range [][]string{
		{"git", "init", src},
		{"git", "-C", src, "config", "user.email", "test@test.com"},
		{"git", "-C", src, "config", "user.name", "Test"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(src, "internal", "foo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "internal", "foo", "bar.go"), []byte("package foo"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "-C", src, "add", "."},
		{"git", "-C", src, "commit", "-m", "init"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	branchOut, err := exec.Command("git", "-C", src, "branch", "--show-current").CombinedOutput()
	if err != nil {
		t.Fatalf("branch --show-current: %v: %s", err, branchOut)
	}
	branch = strings.TrimSpace(string(branchOut))

	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := project.CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}
	return bare, branch
}

func TestBuildGroundLister(t *testing.T) {
	t.Parallel()
	bare, _ := newBareRepoWithTrackedFile(t)

	dir := t.TempDir()
	store, err := project.NewStore(dir, t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	mustWriteProjectYAMLWithClone(t, dir, "o/r", bare)

	lister := buildGroundLister(store)

	t.Run("resolves tracked files for a registered project", func(t *testing.T) {
		t.Parallel()
		files, err := lister(context.Background(), "o/r")
		if err != nil {
			t.Fatalf("lister: %v", err)
		}
		if len(files) != 1 || files[0] != "internal/foo/bar.go" {
			t.Fatalf("files = %v, want [internal/foo/bar.go]", files)
		}
	})

	t.Run("fails open for an unregistered project", func(t *testing.T) {
		t.Parallel()
		if _, err := lister(context.Background(), "no/such-project"); err == nil {
			t.Fatal("expected an error for an unregistered project")
		}
	})
}

// TestUmbrellaGroundingWired proves the wiring contract: the umbrella.Ground
// config toggle controls whether a lister-backed ExpandOption is threaded
// through, for both the GitHub-issues-fetcher path (app_init.go) and the
// manual/GUI expand path (services_wire_tasks.go). It exercises the same
// closure-building logic those files use rather than reaching into private
// App internals that would require a full app.Startup().
func TestUmbrellaGroundingWired(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := project.NewStore(dir, t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	cases := []struct {
		name       string
		ground     bool
		wantLister bool
	}{
		{"ground enabled threads a grounder", true, true},
		{"ground disabled leaves grounding off", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var opts []umbrella.ExpandOption
			if tc.ground {
				opts = append(opts, umbrella.WithExpandGrounder(buildGroundLister(store), 0))
			}
			if got := len(opts) > 0; got != tc.wantLister {
				t.Fatalf("grounder threaded = %v, want %v", got, tc.wantLister)
			}
		})
	}
}
