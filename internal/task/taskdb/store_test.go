package taskdb

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func sqlTask(id, title string) task.Task {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	return task.Task{
		ID: id, Title: title, Status: task.StatusTodo, AgentMode: task.AgentModeHeadless,
		Body: "## Description\n\nwork\n", CreatedAt: now, UpdatedAt: now,
	}
}

// TestSQLStore_TaskAndSidecarsWriteTogether is the issue's "a task and its
// sidecars update together or not at all".
//
// As files this was several separate writes, so a crash between them left a
// task disagreeing with its own plan and nothing afterwards could tell which
// was current.
func TestSQLStore_TaskAndSidecarsWriteTogether(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		record := sqlTask("abc12345", "first")
		sidecars := []Sidecar{
			{Kind: SidecarPlan, Content: "# Plan\n"},
			{Kind: SidecarCodeReview, Content: "# Review\n"},
			{Kind: SidecarPlanDraft, Name: "a", Content: "draft a\n"},
			{Kind: SidecarPlanDraft, Name: "b", Content: "draft b\n"},
		}
		if err := store.Put(t.Context(), record, sidecars); err != nil {
			t.Fatalf("Put: %v", err)
		}

		got, gotSidecars, err := store.Get(t.Context(), "abc12345")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Title != "first" || got.Status != task.StatusTodo {
			t.Fatalf("task round-tripped as %+v", got)
		}
		if len(gotSidecars) != 4 {
			t.Fatalf("read %d sidecars, want 4", len(gotSidecars))
		}

		// A rewrite replaces the set: a sidecar dropped from the task is gone
		// with it, in the same transaction.
		onlyPlan := []Sidecar{{Kind: SidecarPlan, Content: "# Plan\n"}}
		if err := store.Put(t.Context(), record, onlyPlan); err != nil {
			t.Fatalf("second Put: %v", err)
		}
		_, gotSidecars, err = store.Get(t.Context(), "abc12345")
		if err != nil {
			t.Fatalf("Get after rewrite: %v", err)
		}
		if len(gotSidecars) != 1 || gotSidecars[0].Kind != SidecarPlan {
			t.Fatalf("rewrite left %+v, want only the plan", gotSidecars)
		}
	})
}

func TestSQLStore_BoardProjectionOmitsRunTextAndNodeListFiltersBeforeRead(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatal(err)
		}
		active := sqlTask("aaa11111", "active")
		active.AssignedNode = "home-nas"
		active.AgentRuns = []task.AgentRun{{AgentID: "agent-1", Mode: "headless", Prompt: "SECRET PROMPT", Result: "SECRET RESULT"}}
		if err := store.Put(t.Context(), active, nil); err != nil {
			t.Fatal(err)
		}
		other := sqlTask("bbb22222", "other node")
		other.AssignedNode = "laptop"
		if err := store.Put(t.Context(), other, nil); err != nil {
			t.Fatal(err)
		}
		closed := sqlTask("ccc33333", "old closed")
		closed.AssignedNode = "home-nas"
		closed.Status = task.StatusDone
		old := time.Now().Add(-time.Hour)
		closed.ClosedAt = &old
		if err := store.Put(t.Context(), closed, nil); err != nil {
			t.Fatal(err)
		}

		board, err := store.ListBoard(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(board) != 3 || board[0].AgentRuns[0].Prompt != "" || board[0].AgentRuns[0].Result != "" {
			t.Fatalf("board projection retained heavy run text: %+v", board)
		}
		var boardDoc string
		if err := d.QueryRowContext(t.Context(), `SELECT board_doc FROM tasks WHERE id = 'aaa11111'`).Scan(&boardDoc); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(boardDoc, "SECRET") {
			t.Fatalf("persisted board projection contains transcript text: %q", boardDoc)
		}

		forNode, err := store.ListForNode(t.Context(), "home-nas", time.Now().Add(-10*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if len(forNode) != 1 || forNode[0].ID != active.ID || forNode[0].AgentRuns[0].Prompt != "SECRET PROMPT" {
			t.Fatalf("ListForNode = %+v, want only full active home-nas task", forNode)
		}
	})
}

func TestSQLStore_ListActiveSkipsTerminalDocumentsBeforeParsing(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatal(err)
		}
		active := sqlTask("aaa11111", "active")
		if err := store.Put(t.Context(), active, nil); err != nil {
			t.Fatal(err)
		}
		for i, status := range []task.Status{task.StatusDone, task.StatusCancelled} {
			closed := sqlTask(fmt.Sprintf("closed%02d", i), "closed")
			closed.Status = status
			when := time.Now().Add(-time.Hour)
			closed.ClosedAt = &when
			if err := store.Put(t.Context(), closed, nil); err != nil {
				t.Fatal(err)
			}
		}

		got, err := store.ListActive(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != active.ID {
			t.Fatalf("ListActive = %+v, want only %s", got, active.ID)
		}
	})
}

func TestSQLStore_BackfillsLegacyBoardProjection(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		for i := range 26 { // Cross the 25-row backfill page boundary.
			legacy := sqlTask(fmt.Sprintf("%08x", i+1), "legacy")
			legacy.AssignedNode = "home-nas"
			legacy.AgentRuns = []task.AgentRun{{AgentID: "agent-1", Mode: "headless", Prompt: "OLD SECRET"}}
			doc, err := task.MarshalStored(legacy)
			if err != nil {
				t.Fatal(err)
			}
			_, err = d.ExecContext(t.Context(), d.Rebind(`INSERT INTO tasks
				(id, status, project_id, title, created_at, updated_at, deleted_at, doc)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`), legacy.ID, string(legacy.Status), legacy.ProjectID,
				legacy.Title, db.TimeValue(legacy.CreatedAt), db.TimeValue(legacy.UpdatedAt), int64(0), string(doc))
			if err != nil {
				t.Fatal(err)
			}
		}
		_, err := d.ExecContext(t.Context(), d.Rebind(`INSERT INTO tasks
			(id, status, project_id, title, created_at, updated_at, deleted_at, doc)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`), "0000001b", string(task.StatusTodo), "", "malformed",
			db.TimeValue(time.Now()), db.TimeValue(time.Now()), int64(0), "MALFORMED SECRET TRANSCRIPT")
		if err != nil {
			t.Fatal(err)
		}
		deleted := sqlTask("0000001c", "deleted legacy")
		deleted.AssignedNode = "home-nas"
		deletedDoc, err := task.MarshalStored(deleted)
		if err != nil {
			t.Fatal(err)
		}
		_, err = d.ExecContext(t.Context(), d.Rebind(`INSERT INTO tasks
			(id, status, project_id, title, created_at, updated_at, deleted_at, doc)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`), deleted.ID, string(deleted.Status), deleted.ProjectID, deleted.Title,
			db.TimeValue(deleted.CreatedAt), db.TimeValue(deleted.UpdatedAt), db.TimeValue(time.Now()), string(deletedDoc))
		if err != nil {
			t.Fatal(err)
		}
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.BackfillBoardProjections(t.Context()); err != nil {
			t.Fatal(err)
		}
		board, err := store.ListBoard(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(board) != 26 || board[0].AgentRuns[0].Prompt != "" || board[25].AgentRuns[0].Prompt != "" {
			t.Fatalf("backfilled board projection = %+v", board)
		}
		var boardDoc, assignedNode string
		if err := d.QueryRowContext(t.Context(), `SELECT board_doc, assigned_node FROM tasks WHERE id = '0000001a'`).Scan(&boardDoc, &assignedNode); err != nil {
			t.Fatal(err)
		}
		if boardDoc == "" || strings.Contains(boardDoc, "OLD SECRET") || assignedNode != "home-nas" {
			t.Fatalf("board_doc=%q assigned_node=%q", boardDoc, assignedNode)
		}
		if err := d.QueryRowContext(t.Context(), `SELECT board_doc FROM tasks WHERE id = '0000001b'`).Scan(&boardDoc); err != nil {
			t.Fatal(err)
		}
		if boardDoc != "\n" {
			t.Fatalf("malformed board_doc = %q, want tiny sentinel", boardDoc)
		}
		if err := d.QueryRowContext(t.Context(), `SELECT board_doc FROM tasks WHERE id = '0000001c'`).Scan(&boardDoc); err != nil {
			t.Fatal(err)
		}
		if boardDoc != "" {
			t.Fatalf("deleted task was unnecessarily backfilled: %q", boardDoc)
		}
		if err := store.Restore(t.Context(), deleted.ID); err != nil {
			t.Fatal(err)
		}
		if err := d.QueryRowContext(t.Context(), `SELECT board_doc, assigned_node FROM tasks WHERE id = '0000001c'`).Scan(&boardDoc, &assignedNode); err != nil {
			t.Fatal(err)
		}
		if boardDoc == "" || assignedNode != "home-nas" {
			t.Fatalf("restored board_doc=%q assigned_node=%q", boardDoc, assignedNode)
		}
	})
}

// TestSQLStore_PutFnByRoundTripsSidecarsAndRecordsHistory proves PutFnBy
// hands fn a fully sidecar-populated Task (matching Get), writes back both
// the row and the sidecar set fn changed, and records the actor and changed
// fields in the same transaction.
func TestSQLStore_PutFnByRoundTripsSidecarsAndRecordsHistory(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		record := sqlTask("abc12345", "first")
		if err := store.Put(t.Context(), record, []Sidecar{{Kind: SidecarPlan, Content: "# Plan\n"}}); err != nil {
			t.Fatalf("Put: %v", err)
		}

		var sawPlan string
		saved, err := store.PutFnBy(t.Context(), "abc12345", "operator", func(cur task.Task) (task.Task, []string, error) {
			sawPlan = cur.Plan
			cur.Title = "renamed"
			cur.CodeReview = "# Review\n"
			return cur, []string{"title", "code_review"}, nil
		})
		if err != nil {
			t.Fatalf("PutFnBy: %v", err)
		}
		if sawPlan != "# Plan\n" {
			t.Fatalf("fn saw Plan = %q, want the sidecar Put wrote", sawPlan)
		}
		if saved.Title != "renamed" || saved.CodeReview != "# Review\n" {
			t.Fatalf("returned task = %+v, want the fn's edits applied", saved)
		}

		gotTask, gotSidecars, err := store.Get(t.Context(), "abc12345")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if gotTask.Title != "renamed" {
			t.Fatalf("Title = %q, want renamed", gotTask.Title)
		}
		byKind := map[string]string{}
		for _, sc := range gotSidecars {
			byKind[sc.Kind] = sc.Content
		}
		if byKind[SidecarPlan] != "# Plan\n" {
			t.Fatalf("plan sidecar = %q, want it kept", byKind[SidecarPlan])
		}
		if byKind[SidecarCodeReview] != "# Review\n" {
			t.Fatalf("code_review sidecar = %q, want the fn's new value", byKind[SidecarCodeReview])
		}

		entries, err := store.History(t.Context(), HistoryQuery{TaskID: "abc12345"})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		last := entries[len(entries)-1]
		if last.Actor != "operator" {
			t.Errorf("history actor = %q, want operator", last.Actor)
		}
		if len(last.Fields) != 2 || last.Fields[0] != "title" || last.Fields[1] != "code_review" {
			t.Errorf("history fields = %v, want [title code_review]", last.Fields)
		}
	})
}

// TestSQLStore_PutFnByMissingTaskErrors proves PutFnBy refuses to invent a
// task that was never written, the same as UpdateFn against a nonexistent id.
func TestSQLStore_PutFnByMissingTaskErrors(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		_, err = store.PutFnBy(t.Context(), "missing0001", "operator", func(cur task.Task) (task.Task, []string, error) {
			t.Fatal("fn must not run for a task that does not exist")
			return cur, nil, nil
		})
		if err == nil {
			t.Fatal("expected an error for a missing task")
		}
	})
}

// TestSQLStore_DeletedTaskStaysRecoverable is the issue's "a deleted task stays
// recoverable until the retention window passes".
func TestSQLStore_DeletedTaskStaysRecoverable(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		if err := store.Put(t.Context(), sqlTask("abc12345", "doomed"), nil); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := store.Delete(t.Context(), "abc12345"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, _, err := store.Get(t.Context(), "abc12345"); err == nil {
			t.Fatal("a deleted task still reads back from the board")
		}
		list, err := store.List(t.Context())
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 0 {
			t.Fatalf("a deleted task still lists: %d", len(list))
		}

		if err := store.Restore(t.Context(), "abc12345"); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		back, _, err := store.Get(t.Context(), "abc12345")
		if err != nil {
			t.Fatalf("Get after restore: %v", err)
		}
		if back.Title != "doomed" {
			t.Errorf("restored task is %+v", back)
		}

		// Past retention it goes for good.
		if err := store.Delete(t.Context(), "abc12345"); err != nil {
			t.Fatalf("second Delete: %v", err)
		}
		if err := store.PurgeDeleted(t.Context(), -time.Hour+time.Hour); err != nil {
			t.Fatalf("PurgeDeleted(0): %v", err)
		}
		if err := store.Restore(t.Context(), "abc12345"); err != nil {
			t.Fatalf("Restore within retention: %v", err)
		}
		if _, _, err := store.Get(t.Context(), "abc12345"); err != nil {
			t.Fatalf("a zero retention purged a task that was still within it: %v", err)
		}
	})
}

// TestImport_ReportsUnparseableAndStillCompletes is the issue's "a file that
// fails to parse during import is reported and skipped, and the import still
// completes".
func TestImport_ReportsUnparseableAndStillCompletes(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		dir := t.TempDir()
		for _, id := range []string{"aaa11111", "bbb22222"} {
			data, err := task.MarshalStored(sqlTask(id, "task "+id))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, id+".md"), data, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
		// A file no parser accepts, sitting between the two good ones.
		if err := os.WriteFile(filepath.Join(dir, "abb00000.md"), []byte("---\nnot: [valid\n"), 0o600); err != nil {
			t.Fatalf("write bad: %v", err)
		}
		// Sidecars belonging to the first task.
		for suffix, want := range map[string]string{".plan.md": "# Plan\n", ".review.md": "# Review\n"} {
			if err := os.WriteFile(filepath.Join(dir, "aaa11111"+suffix), []byte(want), 0o600); err != nil {
				t.Fatalf("write sidecar: %v", err)
			}
		}

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
		for range 2 {
			if err := Import(t.Context(), d, dir, "home-a", logger); err != nil {
				t.Fatalf("import: %v", err)
			}
		}

		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		list, err := store.List(t.Context())
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 2 {
			t.Fatalf("after two imports the board holds %d tasks, want the 2 that parse", len(list))
		}
		if !strings.Contains(buf.String(), "task.import.unparseable") {
			t.Fatalf("the unreadable task was skipped silently; log was %q", buf.String())
		}

		_, sidecars, err := store.Get(t.Context(), "aaa11111")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if len(sidecars) != 2 {
			t.Fatalf("imported %d sidecars for the first task, want 2", len(sidecars))
		}
		if _, err := os.Stat(filepath.Join(dir, "abb00000.md")); err != nil {
			t.Errorf("the unreadable file was removed: %v", err)
		}
	})
}

// TestImport_MatchesEveryRealSidecarSuffix proves sidecarsOnDisk's suffix
// map matches every actual on-disk filename the file store writes, not a
// guessed convention. Three entries previously didn't: .plan-contract.md
// (the file store writes .json), .code-review.md (the file store writes
// .review.md), and .spec-decisions.md (the file store writes the singular
// .spec-decision.md) — each one meant that sidecar kind silently never
// imported into the database for any existing file-backed install.
//
// Suffixes come from task.SidecarFileSuffixes, the file store's own single source of truth for what a fixed-name sidecar file is named (internal/task/store.go), not a second hand-maintained list here — a future sidecar kind added there is covered by this test automatically instead of silently passing while still missing from taskdb's own suffix map.
func TestImport_MatchesEveryRealSidecarSuffix(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		dir := t.TempDir()
		data, err := task.MarshalStored(sqlTask("aaa11111", "task with every sidecar"))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "aaa11111.md"), data, 0o600); err != nil {
			t.Fatalf("write task: %v", err)
		}
		for _, suffix := range task.SidecarFileSuffixes {
			content := "content for " + suffix
			if suffix == ".comments.json" {
				content = "[]"
			} else if strings.HasSuffix(suffix, ".json") {
				content = `{"content":"` + suffix + `"}`
			}
			if err := os.WriteFile(filepath.Join(dir, "aaa11111"+suffix), []byte(content), 0o600); err != nil {
				t.Fatalf("write sidecar %s: %v", suffix, err)
			}
		}

		if err := Import(t.Context(), d, dir, "home-a", slog.New(slog.DiscardHandler)); err != nil {
			t.Fatalf("import: %v", err)
		}

		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		_, sidecars, err := store.Get(t.Context(), "aaa11111")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if len(sidecars) != len(task.SidecarFileSuffixes) {
			t.Fatalf("imported %d sidecars, want %d (one per suffix on disk) — a suffix mismatch silently drops its sidecar kind", len(sidecars), len(task.SidecarFileSuffixes))
		}
	})
}

// TestSQLStore_CreateByRefusesCollision proves CreateBy never silently
// overwrites an existing row the way PutBy's upsert would, so a caller can
// tell "the candidate ID is taken, try another" from a real write failure.
func TestSQLStore_CreateByRefusesCollision(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		first := sqlTask("abc12345", "first")
		if _, err := store.CreateBy(t.Context(), first, nil, "operator"); err != nil {
			t.Fatalf("first CreateBy: %v", err)
		}
		second := sqlTask("abc12345", "second")
		_, err = store.CreateBy(t.Context(), second, nil, "operator")
		if !errors.Is(err, ErrIDCollision) {
			t.Fatalf("second CreateBy err = %v, want ErrIDCollision", err)
		}
		got, _, err := store.Get(t.Context(), "abc12345")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Title != "first" {
			t.Fatalf("Title = %q, the collision must not have overwritten the original", got.Title)
		}
	})
}

// TestSQLStore_NotFoundErrorsSatisfyOsErrNotExist proves Get and PutFnBy
// answer a missing id the same way the file backend's Store.read does —
// wrapping os.ErrNotExist — because internal/sybra/svc_tasks_board.go's
// boardRejectionFor maps only errors.Is(err, os.ErrNotExist) to a clean 404;
// an unwrapped "not found" string previously fell through to a generic 500,
// hiding the real reason from every API client.
func TestSQLStore_NotFoundErrorsSatisfyOsErrNotExist(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}

		_, _, err = store.Get(t.Context(), "missing99")
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Get err = %v, want it to satisfy errors.Is(err, os.ErrNotExist)", err)
		}

		_, err = store.PutFnBy(t.Context(), "missing99", "operator", func(cur task.Task) (task.Task, []string, error) {
			return cur, nil, nil
		})
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("PutFnBy err = %v, want it to satisfy errors.Is(err, os.ErrNotExist)", err)
		}
	})
}
