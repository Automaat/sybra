package loopagent

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

// backend is one Repository implementation under test. reopen returns a
// second Repository over the same durable storage, which is how the suite
// proves a record survives a restart.
type backend struct {
	name   string
	open   func(t *testing.T) Repository
	reopen func(t *testing.T) Repository
}

func backends(t *testing.T, run func(t *testing.T, b backend)) {
	t.Helper()
	t.Run("file", func(t *testing.T) {
		t.Helper()
		dir := t.TempDir()
		open := func(t *testing.T) Repository {
			t.Helper()
			s, err := NewStore(dir)
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			return s
		}
		run(t, backend{name: "file", open: open, reopen: open})
	})
	dbtest.Each(t, func(t *testing.T, e dbtest.Engine) {
		t.Helper()
		open := func(t *testing.T) Repository {
			t.Helper()
			s, err := NewSQLStore(e.Open(t))
			if err != nil {
				t.Fatalf("NewSQLStore: %v", err)
			}
			return s
		}
		run(t, backend{name: e.Name, open: open, reopen: open})
	})
}

func sampleAgent() LoopAgent {
	return LoopAgent{
		Name:         "self-monitor",
		Prompt:       "/sybra-self-monitor",
		IntervalSec:  3600,
		AllowedTools: []string{"Bash", "Read"},
		Model:        "opus",
		Enabled:      true,
	}
}

func TestRepository_CreateAssignsIdentityAndDefaults(t *testing.T) {
	backends(t, func(t *testing.T, b backend) {
		t.Helper()
		repo := b.open(t)
		created, err := repo.Create(t.Context(), sampleAgent())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if created.ID == "" {
			t.Error("Create did not assign an ID")
		}
		if created.Provider != providerid.Claude {
			t.Errorf("Provider = %q, want the claude default", created.Provider)
		}
		if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
			t.Error("Create did not stamp timestamps")
		}

		got, err := repo.Get(t.Context(), created.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		assertSameAgent(t, got, created)
	})
}

func TestRepository_CreateRejectsInvalidRecords(t *testing.T) {
	backends(t, func(t *testing.T, b backend) {
		t.Helper()
		repo := b.open(t)
		if _, err := repo.Create(t.Context(), LoopAgent{Prompt: "/x", IntervalSec: 60}); err == nil {
			t.Fatal("expected a validation error for a nameless loop agent")
		}
		all, err := repo.List(t.Context())
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(all) != 0 {
			t.Errorf("rejected record was persisted anyway: %+v", all)
		}
	})
}

func TestRepository_ListIsSortedByName(t *testing.T) {
	backends(t, func(t *testing.T, b backend) {
		t.Helper()
		repo := b.open(t)
		for _, name := range []string{"zulu", "alpha", "mike"} {
			la := sampleAgent()
			la.Name = name
			if _, err := repo.Create(t.Context(), la); err != nil {
				t.Fatalf("Create %s: %v", name, err)
			}
		}
		all, err := repo.List(t.Context())
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		got := make([]string, 0, len(all))
		for _, la := range all {
			got = append(got, la.Name)
		}
		if strings.Join(got, ",") != "alpha,mike,zulu" {
			t.Errorf("List order = %v, want alpha,mike,zulu", got)
		}
	})
}

func TestRepository_FindByName(t *testing.T) {
	backends(t, func(t *testing.T, b backend) {
		t.Helper()
		repo := b.open(t)
		created, err := repo.Create(t.Context(), sampleAgent())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		found, ok := repo.FindByName(t.Context(), created.Name)
		if !ok {
			t.Fatal("FindByName did not find the record it just created")
		}
		if found.ID != created.ID {
			t.Errorf("FindByName returned %s, want %s", found.ID, created.ID)
		}
		if _, ok := repo.FindByName(t.Context(), "nothing-here"); ok {
			t.Error("FindByName reported a match for an unknown name")
		}
	})
}

func TestRepository_UpdatePreservesCreatedAtAndBumpsUpdatedAt(t *testing.T) {
	backends(t, func(t *testing.T, b backend) {
		t.Helper()
		repo := b.open(t)
		created, err := repo.Create(t.Context(), sampleAgent())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		edit := created
		edit.CreatedAt = time.Unix(0, 0).UTC()
		edit.Enabled = false
		edit.Provider = ""
		edit.AllowedTools = []string{"Read"}
		updated, err := repo.Update(t.Context(), edit)
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if !updated.CreatedAt.Equal(created.CreatedAt) {
			t.Errorf("CreatedAt = %v, want the stored %v", updated.CreatedAt, created.CreatedAt)
		}
		if updated.UpdatedAt.Before(created.UpdatedAt) {
			t.Errorf("UpdatedAt went backwards: %v < %v", updated.UpdatedAt, created.UpdatedAt)
		}
		if updated.Provider != created.Provider {
			t.Errorf("Provider = %q, want the stored %q", updated.Provider, created.Provider)
		}
		if updated.Enabled {
			t.Error("Update did not persist Enabled=false")
		}

		got, err := repo.Get(t.Context(), created.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		assertSameAgent(t, got, updated)
	})
}

func TestRepository_UpdateRunMetadataLeavesUpdatedAtAlone(t *testing.T) {
	backends(t, func(t *testing.T, b backend) {
		t.Helper()
		repo := b.open(t)
		created, err := repo.Create(t.Context(), sampleAgent())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ran := time.Now().UTC().Truncate(time.Millisecond)
		updated, err := repo.UpdateRunMetadata(t.Context(), created.ID, func(la *LoopAgent) {
			la.LastRunAt = ran
			la.LastRunID = "agent-42"
			la.LastRunCost = 1.25
		})
		if err != nil {
			t.Fatalf("UpdateRunMetadata: %v", err)
		}
		if !updated.UpdatedAt.Equal(created.UpdatedAt) {
			t.Errorf("UpdatedAt = %v, want it untouched at %v", updated.UpdatedAt, created.UpdatedAt)
		}
		got, err := repo.Get(t.Context(), created.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !got.LastRunAt.Equal(ran) || got.LastRunID != "agent-42" || got.LastRunCost != 1.25 {
			t.Errorf("run metadata did not persist: %+v", got)
		}
	})
}

func TestRepository_Delete(t *testing.T) {
	backends(t, func(t *testing.T, b backend) {
		t.Helper()
		repo := b.open(t)
		created, err := repo.Create(t.Context(), sampleAgent())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := repo.Delete(t.Context(), created.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := repo.Get(t.Context(), created.ID); err == nil {
			t.Error("Get returned a deleted record")
		}
		if err := repo.Delete(t.Context(), created.ID); err != nil {
			t.Errorf("deleting a missing record should be a no-op, got %v", err)
		}
	})
}

func TestRepository_DataSurvivesRestart(t *testing.T) {
	backends(t, func(t *testing.T, b backend) {
		t.Helper()
		created, err := b.open(t).Create(t.Context(), sampleAgent())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := b.reopen(t).Get(t.Context(), created.ID)
		if err != nil {
			t.Fatalf("Get after restart: %v", err)
		}
		assertSameAgent(t, got, created)
	})
}

func TestRepository_ConcurrentUpdatesAllLand(t *testing.T) {
	backends(t, func(t *testing.T, b backend) {
		t.Helper()
		repo := b.open(t)
		created, err := repo.Create(t.Context(), sampleAgent())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Assert every writer succeeded and that its edit is the one that
		// survived, not merely that CreatedAt held — that stays true even when
		// 7 of 8 writes vanish.
		const writers = 8
		errs := make([]error, writers)
		models := make([]string, writers)
		var wg sync.WaitGroup
		for i := range writers {
			models[i] = "model-" + strconv.Itoa(i)
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				edit := created
				edit.CreatedAt = time.Time{}
				edit.Model = models[i]
				_, errs[i] = repo.Update(t.Context(), edit)
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("writer %d: %v", i, err)
			}
		}

		got, err := repo.Get(t.Context(), created.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !got.CreatedAt.Equal(created.CreatedAt) {
			t.Errorf("CreatedAt = %v, want the original %v", got.CreatedAt, created.CreatedAt)
		}
		if !slices.Contains(models, got.Model) {
			t.Errorf("Model = %q, want one of %v", got.Model, models)
		}
	})
}

func TestRepository_NilAllowedToolsReadsBackEmpty(t *testing.T) {
	backends(t, func(t *testing.T, b backend) {
		t.Helper()
		repo := b.open(t)
		la := sampleAgent()
		la.AllowedTools = nil
		created, err := repo.Create(t.Context(), la)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := repo.Get(t.Context(), created.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		// A nil here serializes to allowedTools:null on one backend and [] on
		// the other, so the API contract would vary by engine.
		if got.AllowedTools == nil {
			t.Error("AllowedTools read back nil, want an empty slice on every backend")
		}
		if len(got.AllowedTools) != 0 {
			t.Errorf("AllowedTools = %v, want empty", got.AllowedTools)
		}
	})
}

func TestRepository_StampedTimestampsMatchWhatWasStored(t *testing.T) {
	backends(t, func(t *testing.T, b backend) {
		t.Helper()
		repo := b.open(t)
		created, err := repo.Create(t.Context(), sampleAgent())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// time.Now() is nanosecond-granular on Linux and the database keeps
		// microseconds, so a returned record that was not rounded to the
		// stored precision never compares equal to its own read-back.
		got, err := repo.Get(t.Context(), created.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !got.CreatedAt.Equal(created.CreatedAt) {
			t.Errorf("CreatedAt read back as %v, want the returned %v", got.CreatedAt, created.CreatedAt)
		}
		if !got.UpdatedAt.Equal(created.UpdatedAt) {
			t.Errorf("UpdatedAt read back as %v, want the returned %v", got.UpdatedAt, created.UpdatedAt)
		}

		updated, err := repo.Update(t.Context(), created)
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		reread, err := repo.Get(t.Context(), created.ID)
		if err != nil {
			t.Fatalf("Get after update: %v", err)
		}
		if !reread.UpdatedAt.Equal(updated.UpdatedAt) {
			t.Errorf("UpdatedAt read back as %v, want the returned %v", reread.UpdatedAt, updated.UpdatedAt)
		}
	})
}

func TestSQLStore_GetReportsMissingRecords(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		repo, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		_, err = repo.Get(t.Context(), "nope")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get(unknown) = %v, want ErrNotFound", err)
		}
	})
}

func TestSQLStore_NeedsAnOpenDatabase(t *testing.T) {
	if _, err := NewSQLStore(nil); err == nil {
		t.Fatal("expected an error when constructed without a database")
	}
}

// assertSameAgent compares the fields a caller can observe. Timestamps are
// compared with Equal because the database round-trips them through UTC
// microseconds while YAML keeps a monotonic-free wall clock.
func assertSameAgent(t *testing.T, got, want LoopAgent) {
	t.Helper()
	if got.ID != want.ID || got.Name != want.Name || got.Prompt != want.Prompt {
		t.Errorf("identity mismatch: got %+v, want %+v", got, want)
	}
	if got.IntervalSec != want.IntervalSec || got.Provider != want.Provider || got.Model != want.Model {
		t.Errorf("settings mismatch: got %+v, want %+v", got, want)
	}
	if got.Enabled != want.Enabled {
		t.Errorf("Enabled = %v, want %v", got.Enabled, want.Enabled)
	}
	if strings.Join(got.AllowedTools, ",") != strings.Join(want.AllowedTools, ",") {
		t.Errorf("AllowedTools = %v, want %v", got.AllowedTools, want.AllowedTools)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want.CreatedAt)
	}
	if !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, want.UpdatedAt)
	}
}

func TestRepository_TwoProcessesDoNotLoseAnUpdate(t *testing.T) {
	backends(t, func(t *testing.T, b backend) {
		t.Helper()
		// Two independent handles stand in for the desktop app and the CLI writing the same record. UpdateRunMetadata is a read-modify-write of one field, so a read that does not take the row's write lock loses increments: postgres kept the last committer's value with no error, and sqlite failed the second writer with SQLITE_BUSY_SNAPSHOT.
		writer := b.open(t)
		created, err := writer.Create(t.Context(), sampleAgent())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		first, second := b.reopen(t), b.reopen(t)

		const roundsPerHandle = 10
		errs := make([]error, 2*roundsPerHandle)
		var wg sync.WaitGroup
		for i := range roundsPerHandle {
			for h, repo := range []Repository{first, second} {
				wg.Add(1)
				go func(slot int, repo Repository) {
					defer wg.Done()
					_, errs[slot] = repo.UpdateRunMetadata(t.Context(), created.ID, func(la *LoopAgent) {
						la.LastRunID = "run-" + strconv.Itoa(slot)
						la.LastRunCost++
					})
				}(2*i+h, repo)
			}
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("writer %d: %v", i, err)
			}
		}

		got, err := writer.Get(t.Context(), created.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !strings.HasPrefix(got.LastRunID, "run-") {
			t.Errorf("LastRunID = %q, want a value written by one of the handles", got.LastRunID)
		}
		if got.LastRunCost != float64(2*roundsPerHandle) {
			t.Errorf("LastRunCost = %v, want %d — every increment must land", got.LastRunCost, 2*roundsPerHandle)
		}
	})
}
