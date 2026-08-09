package task

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

// TestHistory_RecordsWhatChangedWhoAndWhen is the issue's "every change to a
// task records what changed, when, and which actor made it".
func TestHistory_RecordsWhatChangedWhoAndWhen(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		task := sqlTask("abc12345", "first")
		if err := store.PutBy(task, nil, "operator", []string{"title"}); err != nil {
			t.Fatalf("PutBy: %v", err)
		}
		task.Status = StatusInProgress
		if err := store.PutBy(task, nil, "orchestrator", []string{"status"}); err != nil {
			t.Fatalf("PutBy update: %v", err)
		}

		entries, err := store.History(HistoryQuery{TaskID: "abc12345"})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("recorded %d changes, want 2", len(entries))
		}
		if entries[0].Kind != ChangeCreated || entries[1].Kind != ChangeUpdated {
			t.Fatalf("kinds are %q then %q, want created then updated", entries[0].Kind, entries[1].Kind)
		}
		if entries[1].Actor != "orchestrator" {
			t.Errorf("actor = %q, want orchestrator", entries[1].Actor)
		}
		if len(entries[1].Fields) != 1 || entries[1].Fields[0] != "status" {
			t.Errorf("changed fields = %v, want [status]", entries[1].Fields)
		}
		if entries[1].ChangedAt.IsZero() {
			t.Error("the change has no timestamp")
		}
	})
}

// TestHistory_FiltersByActorAndWindow is the issue's "queryable by task, actor,
// and time range".
func TestHistory_FiltersByActorAndWindow(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		for _, actor := range []string{"operator", "orchestrator", "operator"} {
			task := sqlTask("abc12345", "t")
			if err := store.PutBy(task, nil, actor, nil); err != nil {
				t.Fatalf("PutBy: %v", err)
			}
		}
		if err := store.PutBy(sqlTask("bbb22222", "other"), nil, "operator", nil); err != nil {
			t.Fatalf("PutBy other: %v", err)
		}

		byActor, err := store.History(HistoryQuery{Actor: "orchestrator"})
		if err != nil {
			t.Fatalf("History by actor: %v", err)
		}
		if len(byActor) != 1 {
			t.Fatalf("actor filter returned %d entries, want 1", len(byActor))
		}
		byTask, err := store.History(HistoryQuery{TaskID: "abc12345"})
		if err != nil {
			t.Fatalf("History by task: %v", err)
		}
		if len(byTask) != 3 {
			t.Fatalf("task filter returned %d entries, want 3", len(byTask))
		}
		future, err := store.History(HistoryQuery{Since: time.Now().UTC().Add(time.Hour)})
		if err != nil {
			t.Fatalf("History by window: %v", err)
		}
		if len(future) != 0 {
			t.Fatalf("a future window returned %d entries", len(future))
		}
	})
}

// TestHistory_ReconstructsAPastState is the issue's "the state of any task at a
// past moment can be reconstructed".
func TestHistory_ReconstructsAPastState(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		task := sqlTask("abc12345", "before")
		if err := store.PutBy(task, nil, "operator", nil); err != nil {
			t.Fatalf("PutBy: %v", err)
		}
		afterFirst := time.Now().UTC()
		time.Sleep(5 * time.Millisecond)

		task.Title = "after an automation overwrote it"
		task.Status = StatusInReview
		if err := store.PutBy(task, nil, "automation", []string{"title", "status"}); err != nil {
			t.Fatalf("PutBy update: %v", err)
		}

		was, err := store.TaskAt("abc12345", afterFirst)
		if err != nil {
			t.Fatalf("TaskAt: %v", err)
		}
		if was.Title != "before" || was.Status != StatusTodo {
			t.Fatalf("reconstructed %q/%q, want the state before the overwrite", was.Title, was.Status)
		}
		now, _, err := store.Get("abc12345")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if now.Title == was.Title {
			t.Fatal("current state and the past one are the same; nothing was overwritten")
		}
	})
}

// TestHistory_SurvivesDeleteAndRestore is the issue's "a task deleted and
// restored keeps the history recorded before the deletion".
func TestHistory_SurvivesDeleteAndRestore(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		if err := store.PutBy(sqlTask("abc12345", "kept"), nil, "operator", nil); err != nil {
			t.Fatalf("PutBy: %v", err)
		}
		if err := store.Delete("abc12345"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if err := store.Restore("abc12345"); err != nil {
			t.Fatalf("Restore: %v", err)
		}

		entries, err := store.History(HistoryQuery{TaskID: "abc12345"})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		if len(entries) != 3 {
			t.Fatalf("history holds %d entries, want create, delete and restore", len(entries))
		}
		if entries[0].Kind != ChangeCreated {
			t.Errorf("the change from before the deletion is %q, want it kept", entries[0].Kind)
		}
		if entries[1].Kind != ChangeDeleted || entries[2].Kind != ChangeRestored {
			t.Errorf("kinds are %q then %q", entries[1].Kind, entries[2].Kind)
		}
	})
}

// TestHistory_TrimLeavesCurrentStateAlone is the issue's "retention trims
// history past its configured age without affecting current state".
func TestHistory_TrimLeavesCurrentStateAlone(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		if err := store.PutBy(sqlTask("abc12345", "kept"), nil, "operator", nil); err != nil {
			t.Fatalf("PutBy: %v", err)
		}
		if err := store.TrimHistory(time.Nanosecond); err != nil {
			t.Fatalf("TrimHistory: %v", err)
		}
		entries, err := store.History(HistoryQuery{TaskID: "abc12345"})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		if len(entries) != 0 {
			t.Fatalf("trim left %d entries", len(entries))
		}
		back, _, err := store.Get("abc12345")
		if err != nil {
			t.Fatalf("trimming history removed the task itself: %v", err)
		}
		if back.Title != "kept" {
			t.Errorf("task is %+v after a trim", back)
		}
	})
}
