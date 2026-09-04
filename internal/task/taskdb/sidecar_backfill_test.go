package taskdb

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

// TestBackfillMissingSidecarKinds_FillsGapsFromOldImport reproduces the
// exact state a board left in by the pre-fix sidecarsOnDisk suffix bug: a
// task record already imported (the tasks table has a row, so a naive
// re-import would be a no-op via dbimport.Once's marker), with its
// PlanContract/CodeReview/SpecDecision files still on disk but no matching
// task_sidecars rows, because the old suffix map never found them.
func TestBackfillMissingSidecarKinds_FillsGapsFromOldImport(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		dir := t.TempDir()
		data, err := task.MarshalStored(sqlTask("aaa11111", "already imported"))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "aaa11111.md"), data, 0o600); err != nil {
			t.Fatalf("write task: %v", err)
		}
		writeSidecarFile(t, dir, "aaa11111.plan-contract.json", `{"contract":true}`)
		writeSidecarFile(t, dir, "aaa11111.review.md", "review content")
		writeSidecarFile(t, dir, "aaa11111.spec-decision.md", "spec decision content")

		// Simulate the task already having been imported by the main pass,
		// so only the backfill — not a fresh Import — can be what fills the
		// gap this test checks for.
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		if _, err := store.CreateBy(t.Context(), sqlTask("aaa11111", "already imported"), nil, "test"); err != nil {
			t.Fatalf("seed task: %v", err)
		}

		if err := BackfillMissingSidecarKinds(t.Context(), d, dir, "home-a", slog.New(slog.DiscardHandler)); err != nil {
			t.Fatalf("BackfillMissingSidecarKinds: %v", err)
		}

		_, sidecars, err := store.Get(t.Context(), "aaa11111")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		byKind := make(map[string]string)
		for _, sc := range sidecars {
			byKind[sc.Kind] = sc.Content
		}
		if byKind[SidecarPlanContract] != `{"contract":true}` {
			t.Errorf("PlanContract = %q, want the on-disk content", byKind[SidecarPlanContract])
		}
		if byKind[SidecarCodeReview] != "review content" {
			t.Errorf("CodeReview = %q, want the on-disk content", byKind[SidecarCodeReview])
		}
		if byKind[SidecarSpecDecision] != "spec decision content" {
			t.Errorf("SpecDecision = %q, want the on-disk content", byKind[SidecarSpecDecision])
		}
	})
}

// TestBackfillMissingSidecarKinds_NeverOverwritesExistingRow is the
// safety property the whole function exists for: a task that already has a
// CodeReview row — written correctly by ordinary live DB-backend usage
// after the original (buggy) import ran — must keep that row exactly as it
// is, even though a different, older value sits in the on-disk file. The
// on-disk file is stale by definition once the database is authoritative;
// overwriting a live row with it would be a regression, not a recovery.
func TestBackfillMissingSidecarKinds_NeverOverwritesExistingRow(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		dir := t.TempDir()
		data, err := task.MarshalStored(sqlTask("aaa11111", "live task"))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "aaa11111.md"), data, 0o600); err != nil {
			t.Fatalf("write task: %v", err)
		}
		writeSidecarFile(t, dir, "aaa11111.review.md", "STALE on-disk review")

		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		liveTask := sqlTask("aaa11111", "live task")
		liveTask.CodeReview = "LIVE current review, written after migration"
		if _, err := store.CreateBy(t.Context(), liveTask, SidecarsFromTask(liveTask), "test"); err != nil {
			t.Fatalf("seed task with live review: %v", err)
		}

		if err := BackfillMissingSidecarKinds(t.Context(), d, dir, "home-a", slog.New(slog.DiscardHandler)); err != nil {
			t.Fatalf("BackfillMissingSidecarKinds: %v", err)
		}

		got, err := NewPersistence(store).Get("aaa11111")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.CodeReview != "LIVE current review, written after migration" {
			t.Fatalf("CodeReview = %q, backfill must not overwrite an existing row with stale disk content", got.CodeReview)
		}
	})
}

// TestBackfillMissingSidecarKinds_RunsOnlyOnce proves the backfill is
// itself exactly-once, the same guarantee the main import gives: running it
// twice does not re-scan or duplicate anything the second time.
func TestBackfillMissingSidecarKinds_RunsOnlyOnce(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		dir := t.TempDir()
		data, err := task.MarshalStored(sqlTask("aaa11111", "task"))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "aaa11111.md"), data, 0o600); err != nil {
			t.Fatalf("write task: %v", err)
		}
		writeSidecarFile(t, dir, "aaa11111.review.md", "review content")

		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		if _, err := store.CreateBy(t.Context(), sqlTask("aaa11111", "task"), nil, "test"); err != nil {
			t.Fatalf("seed task: %v", err)
		}

		for i := range 2 {
			if err := BackfillMissingSidecarKinds(t.Context(), d, dir, "home-a", slog.New(slog.DiscardHandler)); err != nil {
				t.Fatalf("BackfillMissingSidecarKinds run %d: %v", i, err)
			}
		}

		_, sidecars, err := store.Get(t.Context(), "aaa11111")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		count := 0
		for _, sc := range sidecars {
			if sc.Kind == SidecarCodeReview {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("CodeReview sidecar rows = %d after two backfill runs, want exactly 1", count)
		}
	})
}

// TestBackfillMissingSidecarKinds_ConcurrentLiveWriteAlwaysWins drives an
// actual race between the backfill and a concurrent PutBy for the same
// (task_id, kind), the case TestBackfillMissingSidecarKinds_NeverOverwritesExistingRow
// cannot exercise since it writes the live value sequentially, before
// calling the backfill. insertSidecarIfAbsent's ON CONFLICT DO NOTHING
// against upsertSidecar's ON CONFLICT DO UPDATE (what a normal PutBy write
// uses) makes the outcome order-independent: whichever one commits the row
// first, PutBy's own write always ends up as the final content, because
// the backfill never overwrites an existing row and PutBy always
// overwrites — so the live value wins regardless of which transaction the
// database happens to serialize first.
//
// The sqlite leg cannot demonstrate genuine interleaving, since dbtest's sqlite pool caps at one connection with an immediate write lock taken at BeginTx, so one goroutine's transaction always fully completes before the other's can even open — it runs here only as a same-engine sanity check that the statement itself is well-formed, not as evidence for the fix. The postgres leg is what actually proves the atomicity claim, since only it can hold two open transactions racing for the same row at once.
func TestBackfillMissingSidecarKinds_ConcurrentLiveWriteAlwaysWins(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		const n = 12
		for i := range n {
			id := fmt.Sprintf("task%04d", i)
			dir := t.TempDir()
			data, err := task.MarshalStored(sqlTask(id, "race target"))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, id+".md"), data, 0o600); err != nil {
				t.Fatalf("write task: %v", err)
			}
			writeSidecarFile(t, dir, id+".review.md", "STALE on-disk review")

			store, err := NewSQLStore(d)
			if err != nil {
				t.Fatalf("NewSQLStore: %v", err)
			}
			if _, err := store.CreateBy(t.Context(), sqlTask(id, "race target"), nil, "test"); err != nil {
				t.Fatalf("seed task: %v", err)
			}

			liveTask := sqlTask(id, "race target")
			liveTask.CodeReview = "LIVE review written by a concurrent PutBy"

			var wg sync.WaitGroup
			errs := make([]error, 2)
			wg.Add(2)
			go func() {
				defer wg.Done()
				errs[0] = store.PutBy(t.Context(), liveTask, SidecarsFromTask(liveTask), "test", []string{"code_review"})
			}()
			go func() {
				defer wg.Done()
				errs[1] = BackfillMissingSidecarKinds(t.Context(), d, dir, "scope-"+id, slog.New(slog.DiscardHandler))
			}()
			wg.Wait()
			for _, err := range errs {
				if err != nil {
					t.Fatalf("race participant failed: %v", err)
				}
			}

			got, err := NewPersistence(store).Get(id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.CodeReview != "LIVE review written by a concurrent PutBy" {
				t.Fatalf("task %s: CodeReview = %q, want the live value regardless of race order", id, got.CodeReview)
			}
		}
	})
}

func writeSidecarFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
