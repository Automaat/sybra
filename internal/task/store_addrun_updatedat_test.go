package task

import (
	"testing"
	"time"
)

func TestAddRunBumpsUpdatedAtConsistently(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create("t", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	before := created.UpdatedAt

	time.Sleep(2 * time.Millisecond)
	inProg := StatusInProgress
	if err := store.AddRunWithStatus(created.ID, AgentRun{AgentID: "a1", State: "running", StartedAt: time.Now()}, &inProg); err != nil {
		t.Fatal(err)
	}

	fromDisk, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !fromDisk.UpdatedAt.After(before) {
		t.Errorf("AddRunWithStatus did not advance UpdatedAt on disk: before=%v after=%v", before, fromDisk.UpdatedAt)
	}
	if fromDisk.Status != StatusInProgress {
		t.Errorf("status = %s, want in-progress", fromDisk.Status)
	}

	listed, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	var cached Task
	for i := range listed {
		if listed[i].ID == created.ID {
			cached = listed[i]
		}
	}
	if !cached.UpdatedAt.Equal(fromDisk.UpdatedAt) {
		t.Errorf("cache (List) UpdatedAt %v disagrees with disk (Get) %v — the mirror clock would be non-monotonic across paths", cached.UpdatedAt, fromDisk.UpdatedAt)
	}
}

func TestUpdateRunBumpsUpdatedAt(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create("t", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddRun(created.ID, AgentRun{AgentID: "a1", State: "running", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	mid, _ := store.Get(created.ID)

	time.Sleep(2 * time.Millisecond)
	if err := store.UpdateRun(created.ID, "a1", RunPatch{HeadSHA: Ptr("abc123")}); err != nil {
		t.Fatal(err)
	}
	after, _ := store.Get(created.ID)
	if !after.UpdatedAt.After(mid.UpdatedAt) {
		t.Errorf("UpdateRun did not advance UpdatedAt: mid=%v after=%v", mid.UpdatedAt, after.UpdatedAt)
	}
}
