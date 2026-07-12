package task

import (
	"testing"
	"time"
)

func TestStorePutVerbatimAndUpsert(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	in := Task{
		ID:           "mirror-1",
		Title:        "pushed",
		Status:       StatusInProgress,
		AgentMode:    AgentModeHeadless,
		AssignedNode: "box",
		MirrorRev:    3,
		CreatedAt:    created,
		UpdatedAt:    created,
		Body:         "body text",
	}
	saved, err := store.Put(in)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if saved.FilePath == "" {
		t.Error("Put should set FilePath")
	}

	got, err := store.Get("mirror-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != "mirror-1" || got.Status != StatusInProgress || got.AssignedNode != "box" || got.MirrorRev != 3 {
		t.Fatalf("Put did not persist verbatim: %+v", got)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("Put overwrote CreatedAt: %v", got.CreatedAt)
	}

	in.Status = StatusReadyPR
	if _, err := store.Put(in); err != nil {
		t.Fatalf("second Put (upsert): %v", err)
	}
	got, _ = store.Get("mirror-1")
	if got.Status != StatusReadyPR {
		t.Errorf("upsert status = %s, want ready-pr", got.Status)
	}
	all, _ := store.List()
	n := 0
	for i := range all {
		if all[i].ID == "mirror-1" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("upsert produced %d copies, want 1", n)
	}
}

func TestStorePutRequiresID(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(Task{Title: "no id"}); err == nil {
		t.Fatal("Put must reject a task with no ID")
	}
}
