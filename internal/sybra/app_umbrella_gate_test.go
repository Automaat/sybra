package sybra

import (
	"log/slog"
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

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

// mkChild creates an umbrella child task in the given status.
func mkChild(t *testing.T, m *task.Manager, title, issue, umb string, deps []string, status task.Status) task.Task {
	t.Helper()
	tk, err := m.CreateFull(title, "", task.AgentModeHeadless, task.Update{
		Issue:         task.Ptr(issue),
		UmbrellaIssue: task.Ptr(umb),
		DependsOn:     task.Ptr(deps),
		Status:        task.Ptr(status),
	})
	if err != nil {
		t.Fatalf("CreateFull(%s): %v", title, err)
	}
	return tk
}

func mustStatus(t *testing.T, m *task.Manager, id string) task.Status {
	t.Helper()
	tk, err := m.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return tk.Status
}

func TestReleaseUnblockedChildren_ReleasesRootWithNoDeps(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"

	root := mkChild(t, m, "root", "Automaat/sybra#1", umb, nil, task.StatusBlocked)

	app.releaseUnblockedChildren()

	if got := mustStatus(t, m, root.ID); got != task.StatusTodo {
		t.Fatalf("root status = %q, want %q", got, task.StatusTodo)
	}
}

func TestReleaseUnblockedChildren_HoldsThenReleasesChain(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"

	// dep is still in progress; child depends on it.
	dep := mkChild(t, m, "dep", "Automaat/sybra#1", umb, nil, task.StatusInProgress)
	child := mkChild(t, m, "child", "Automaat/sybra#2", umb, []string{"Automaat/sybra#1"}, task.StatusBlocked)

	app.releaseUnblockedChildren()
	if got := mustStatus(t, m, child.ID); got != task.StatusBlocked {
		t.Fatalf("child released early: status = %q, want %q", got, task.StatusBlocked)
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
	child := mkChild(t, m, "child", "Automaat/sybra#2", umb, []string{"Automaat/sybra#1"}, task.StatusBlocked)

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
	x := mkChild(t, m, "x", "Automaat/sybra#1", umb, []string{"Automaat/sybra#2"}, task.StatusBlocked)
	y := mkChild(t, m, "y", "Automaat/sybra#2", umb, []string{"Automaat/sybra#1"}, task.StatusBlocked)

	app.releaseUnblockedChildren()

	if got := mustStatus(t, m, tracker.ID); got != task.StatusHumanRequired {
		t.Fatalf("tracker status = %q, want %q on cycle", got, task.StatusHumanRequired)
	}
	// Cyclic children must stay blocked.
	if got := mustStatus(t, m, x.ID); got != task.StatusBlocked {
		t.Fatalf("x status = %q, want blocked", got)
	}
	if got := mustStatus(t, m, y.ID); got != task.StatusBlocked {
		t.Fatalf("y status = %q, want blocked", got)
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
