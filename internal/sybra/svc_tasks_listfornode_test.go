package sybra

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

// TestListTasksForNodeFiltersByAssignedNode verifies the cluster-mirror-only
// listing (see internal/cluster.Client.ListTasks and #2258) only returns
// tasks assigned to the requested node, never tasks assigned elsewhere or
// unassigned to this node's own board.
func TestListTasksForNodeFiltersByAssignedNode(t *testing.T) {
	svc, a := setupTaskService(t)

	mustPut := func(tk task.Task) {
		t.Helper()
		if _, _, err := a.tasks.Put(tk); err != nil {
			t.Fatalf("Put(%s): %v", tk.ID, err)
		}
	}

	mustPut(task.Task{ID: "mine", Title: "mine", Status: task.StatusTodo, AssignedNode: "home-nas", UpdatedAt: time.Now()})
	mustPut(task.Task{ID: "elsewhere", Title: "elsewhere", Status: task.StatusTodo, AssignedNode: "other-box", UpdatedAt: time.Now()})
	mustPut(task.Task{ID: "unassigned", Title: "unassigned", Status: task.StatusTodo, UpdatedAt: time.Now()})

	got, err := svc.ListTasksForNode("home-nas")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "mine" {
		t.Fatalf("ListTasksForNode(home-nas) = %+v, want only [mine]", got)
	}
}

// TestListTasksForNodeDropsStaleTerminalTasks verifies a terminal (done)
// task assigned to the node stops being re-serialized once it has been
// closed for longer than mirrorStaleTerminalWindow -- the leader's Mirror
// converged on it long ago, so repeating its full body/plan sidecars on
// every 30s reconcile forever is pure waste. A task closed inside the window
// still appears, giving the leader multiple chances to apply the final state
// first.
func TestListTasksForNodeDropsStaleTerminalTasks(t *testing.T) {
	svc, a := setupTaskService(t)

	old := time.Now().Add(-mirrorStaleTerminalWindow - time.Minute)
	fresh := time.Now().Add(-time.Minute)

	mustPut := func(tk task.Task) {
		t.Helper()
		if _, _, err := a.tasks.Put(tk); err != nil {
			t.Fatalf("Put(%s): %v", tk.ID, err)
		}
	}

	mustPut(task.Task{
		ID: "stale-done", Title: "stale-done", Status: task.StatusDone,
		AssignedNode: "home-nas", UpdatedAt: old, ClosedAt: &old,
	})
	mustPut(task.Task{
		ID: "fresh-done", Title: "fresh-done", Status: task.StatusDone,
		AssignedNode: "home-nas", UpdatedAt: fresh, ClosedAt: &fresh,
	})
	mustPut(task.Task{
		ID: "in-progress", Title: "in-progress", Status: task.StatusInProgress,
		AssignedNode: "home-nas", UpdatedAt: old,
	})

	got, err := svc.ListTasksForNode("home-nas")
	if err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]bool, len(got))
	for _, tk := range got {
		ids[tk.ID] = true
	}
	if ids["stale-done"] {
		t.Errorf("ListTasksForNode returned stale-done, want it dropped (closed %v ago)", mirrorStaleTerminalWindow+time.Minute)
	}
	if !ids["fresh-done"] {
		t.Error("ListTasksForNode dropped fresh-done, want it kept (closed within the window)")
	}
	if !ids["in-progress"] {
		t.Error("ListTasksForNode dropped in-progress, want non-terminal tasks always kept")
	}
}
