package clusterlead

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

// TestMergeSnapshotGapSurvivesInterveningLeaderEdit closes the gap the
// leader-edit-race regression test doesn't cover: applyFollowerTask calls
// Merge, then writeSidecars (several blocking file writes), then Put — an
// unrelated local edit can land in that window, after Merge's own
// time.Now() snapshot but before the write actually reaches Put's lock.
// Store.Put's MirrorRev-based freshness check must still accept the
// follower's status change in that case, since MirrorRev proves it
// regardless of what happened to UpdatedAt in the interim.
func TestMergeSnapshotGapSurvivesInterveningLeaderEdit(t *testing.T) {
	mgr := newManager(t)
	t0 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	if _, _, err := mgr.Put(task.Task{
		ID: "task-pet", Title: "leader title", Status: task.StatusTodo,
		ProjectID: "owner/pet", AssignedNode: "pet-box", CreatedAt: t0, UpdatedAt: t0,
	}); err != nil {
		t.Fatal(err)
	}
	canonical, _ := mgr.Get("task-pet")

	follower := task.Task{
		ID: "task-pet", Title: "follower title", Status: task.StatusInProgress,
		ProjectID: "attacker/x", AssignedNode: "elsewhere", UpdatedAt: t0.Add(90 * time.Minute),
	}
	merged, ok := Merge(canonical, follower)
	if !ok {
		t.Fatal("Merge should accept")
	}

	// Simulates the writeSidecars gap: an unrelated leader edit lands here,
	// after Merge's snapshot but before the mirror's own Put reaches disk.
	newTitle := "leader renamed it while mirror apply was in flight"
	if _, err := mgr.Update("task-pet", task.Update{Title: &newTitle}); err != nil {
		t.Fatal(err)
	}

	saved, _, err := mgr.Put(merged)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != task.StatusInProgress {
		t.Fatalf("got status=%q title=%q, want status=in-progress title=%q",
			saved.Status, saved.Title, follower.Title)
	}
}
