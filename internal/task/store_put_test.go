package task

import (
	"os"
	"path/filepath"
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
	updated := time.Date(2026, 7, 2, 15, 30, 0, 0, time.UTC)
	in := Task{
		ID:           "mirror-1",
		Title:        "pushed",
		Status:       StatusInProgress,
		AgentMode:    AgentModeHeadless,
		AssignedNode: "box",
		MirrorRev:    3,
		CreatedAt:    created,
		UpdatedAt:    updated,
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
	if !got.UpdatedAt.Equal(updated) {
		t.Errorf("Put overwrote leader-supplied UpdatedAt: got %v, want %v", got.UpdatedAt, updated)
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

func TestStorePutRejectsUnsafeID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(dir, "..", "escaped.md")
	for _, id := range []string{"", "..", "../escaped", "a/b", "a\\b", ".hidden", "../../etc/passwd"} {
		if _, err := store.Put(Task{ID: id, Title: "x", Status: StatusTodo}); err == nil {
			t.Errorf("Put must reject unsafe id %q", id)
		}
	}
	if _, err := os.Stat(canary); !os.IsNotExist(err) {
		t.Fatalf("a traversal id escaped the tasks dir: %s exists", canary)
	}
}
