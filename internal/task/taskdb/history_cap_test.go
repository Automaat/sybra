package taskdb

import (
	"fmt"
	"testing"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func TestHistory_CapsEntriesPerTask(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		store.SetMaxHistoryPerTask(5)

		// Given a task rewritten far more often than the cap allows
		record := sqlTask("cap12345", "first")
		for i := range 40 {
			record.Title = fmt.Sprintf("title-%d", i)
			if err := store.PutBy(t.Context(), record, nil, "engine", []string{"title"}); err != nil {
				t.Fatalf("PutBy %d: %v", i, err)
			}
		}

		// When its history is read
		entries, err := store.History(t.Context(), HistoryQuery{TaskID: "cap12345"})
		if err != nil {
			t.Fatalf("History: %v", err)
		}

		// Then only the newest cap entries survive, in order
		if len(entries) != 5 {
			t.Fatalf("kept %d entries, want 5", len(entries))
		}
		if got := entries[len(entries)-1].Snapshot; got == "" {
			t.Fatal("newest entry has no snapshot")
		}
		want := fmt.Sprintf("title-%d", 39)
		newest, err := task.ParseBytes([]byte(entries[len(entries)-1].Snapshot))
		if err != nil {
			t.Fatalf("parse newest snapshot: %v", err)
		}
		if newest.Title != want {
			t.Errorf("newest snapshot title = %q, want %q", newest.Title, want)
		}
		oldest, err := task.ParseBytes([]byte(entries[0].Snapshot))
		if err != nil {
			t.Fatalf("parse oldest snapshot: %v", err)
		}
		if oldest.Title != fmt.Sprintf("title-%d", 35) {
			t.Errorf("oldest kept snapshot title = %q, want title-35", oldest.Title)
		}
	})
}

func TestHistory_CapIsPerTaskNotGlobal(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		store.SetMaxHistoryPerTask(3)

		// Given two tasks, one rewritten past the cap and one barely touched
		hot := sqlTask("hot12345", "hot")
		for i := range 20 {
			hot.Title = fmt.Sprintf("hot-%d", i)
			if err := store.PutBy(t.Context(), hot, nil, "engine", []string{"title"}); err != nil {
				t.Fatalf("PutBy hot %d: %v", i, err)
			}
		}
		quiet := sqlTask("cold1234", "quiet")
		if err := store.PutBy(t.Context(), quiet, nil, "operator", []string{"title"}); err != nil {
			t.Fatalf("PutBy quiet: %v", err)
		}

		// When each task's history is read
		hotEntries, err := store.History(t.Context(), HistoryQuery{TaskID: "hot12345"})
		if err != nil {
			t.Fatalf("History hot: %v", err)
		}
		quietEntries, err := store.History(t.Context(), HistoryQuery{TaskID: "cold1234"})
		if err != nil {
			t.Fatalf("History quiet: %v", err)
		}

		// Then trimming the hot task leaves the quiet one's single entry intact
		if len(hotEntries) != 3 {
			t.Errorf("hot task kept %d entries, want 3", len(hotEntries))
		}
		if len(quietEntries) != 1 {
			t.Errorf("quiet task kept %d entries, want 1", len(quietEntries))
		}
	})
}

func TestHistory_SweepBringsAnExistingBoardDownToTheCap(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}

		// Given tasks whose history accumulated before any cap was enforced
		store.SetMaxHistoryPerTask(-1)
		for _, id := range []string{"swp11111", "swp22222"} {
			record := sqlTask(id, "first")
			for i := range 30 {
				record.Title = fmt.Sprintf("%s-%d", id, i)
				if err := store.PutBy(t.Context(), record, nil, "engine", []string{"title"}); err != nil {
					t.Fatalf("PutBy %s %d: %v", id, i, err)
				}
			}
		}

		// When the sweep runs twice under a cap, those tasks never written again
		store.SetMaxHistoryPerTask(4)
		if err := store.TrimHistoryOverCap(t.Context()); err != nil {
			t.Fatalf("TrimHistoryOverCap: %v", err)
		}
		if err := store.TrimHistoryOverCap(t.Context()); err != nil {
			t.Fatalf("TrimHistoryOverCap rerun: %v", err)
		}

		// Then every task sits at the cap, keeping its newest entries
		for _, id := range []string{"swp11111", "swp22222"} {
			entries, err := store.History(t.Context(), HistoryQuery{TaskID: id})
			if err != nil {
				t.Fatalf("History %s: %v", id, err)
			}
			if len(entries) != 4 {
				t.Fatalf("%s kept %d entries, want 4", id, len(entries))
			}
			newest, err := task.ParseBytes([]byte(entries[len(entries)-1].Snapshot))
			if err != nil {
				t.Fatalf("parse newest snapshot: %v", err)
			}
			if want := fmt.Sprintf("%s-29", id); newest.Title != want {
				t.Errorf("%s newest title = %q, want %q", id, newest.Title, want)
			}
		}
	})
}

func TestHistory_NegativeCapKeepsEveryEntry(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}

		// Given the cap explicitly disabled
		store.SetMaxHistoryPerTask(-1)

		// When a task is rewritten many times
		record := sqlTask("keep1234", "first")
		for i := range 25 {
			record.Title = fmt.Sprintf("title-%d", i)
			if err := store.PutBy(t.Context(), record, nil, "engine", []string{"title"}); err != nil {
				t.Fatalf("PutBy %d: %v", i, err)
			}
		}

		// Then nothing is trimmed
		entries, err := store.History(t.Context(), HistoryQuery{TaskID: "keep1234"})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		if len(entries) != 25 {
			t.Errorf("kept %d entries, want 25", len(entries))
		}
	})
}

func TestHistory_SweepRunsUntilEveryBatchIsDone(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}

		// Given far more over-cap entries than one sweep statement removes
		store.SetMaxHistoryPerTask(-1)
		record := sqlTask("btch1234", "first")
		for i := range 60 {
			record.Title = fmt.Sprintf("title-%d", i)
			if err := store.PutBy(t.Context(), record, nil, "engine", []string{"title"}); err != nil {
				t.Fatalf("PutBy %d: %v", i, err)
			}
		}

		// When the sweep runs with a batch far smaller than the backlog
		store.SetMaxHistoryPerTask(5)
		store.setHistorySweepBatch(4)
		if err := store.TrimHistoryOverCap(t.Context()); err != nil {
			t.Fatalf("TrimHistoryOverCap: %v", err)
		}

		// Then it kept going instead of stopping after the first batch
		entries, err := store.History(t.Context(), HistoryQuery{TaskID: "btch1234"})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		if len(entries) != 5 {
			t.Fatalf("sweep left %d entries, want 5", len(entries))
		}
	})
}

func TestHistory_ShippedDefaultCapIsEnforced(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}

		// Given a shipped default that actually bounds a hot task
		if DefaultMaxHistoryPerTask <= 0 || DefaultMaxHistoryPerTask > 1000 {
			t.Fatalf("DefaultMaxHistoryPerTask = %d, want a bound that caps a hot task", DefaultMaxHistoryPerTask)
		}
		record := sqlTask("dflt1234", "first")
		for i := range DefaultMaxHistoryPerTask + 20 {
			record.Title = fmt.Sprintf("title-%d", i)
			if err := store.PutBy(t.Context(), record, nil, "engine", []string{"title"}); err != nil {
				t.Fatalf("PutBy %d: %v", i, err)
			}
		}

		// When its history is read
		entries, err := store.History(t.Context(), HistoryQuery{TaskID: "dflt1234"})
		if err != nil {
			t.Fatalf("History: %v", err)
		}

		// Then the default bounded it rather than letting it grow
		if len(entries) != DefaultMaxHistoryPerTask {
			t.Fatalf("kept %d entries, want the default %d", len(entries), DefaultMaxHistoryPerTask)
		}
	})
}
