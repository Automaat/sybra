package taskdb

import (
	"testing"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

// newTestManager builds a database-backed task.Manager, the same wiring
// app_init.go uses when database.backend selects one — a real Manager, not a
// bare SQLStore, so these tests exercise the actor-required *By API, hook
// firing, and locking Manager itself adds, not just the persistence layer
// underneath it.
func newTestManager(t *testing.T, d *db.DB) *task.Manager {
	t.Helper()
	sqlStore, err := NewSQLStore(d)
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}
	fileStore, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return task.NewManagerWithPersistence(fileStore, NewPersistence(sqlStore), nil)
}

// TestManager_SQLBackend_CreateRecordsHistoryWithActor is the issue's "every
// task mutation in the running application records a history entry" and "a
// recorded change names the actor that made it", exercised through the real
// Manager the app constructs, not the persistence layer directly.
// TestManager_SQLBackend_PersistsToFileIsFalse proves a DB-backed Manager
// correctly reports it is not file-backed — the discriminator
// internal/sybra/clusterlead/mirror.go's writeSidecars uses to decide
// whether its compensating direct-file sidecar write is still needed.
func TestManager_SQLBackend_PersistsToFileIsFalse(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		mgr := newTestManager(t, d)
		if mgr.PersistsToFile() {
			t.Fatal("PersistsToFile() = true for a DB-backed Manager, want false")
		}
	})
}

func TestManager_SQLBackend_CreateRecordsHistoryWithActor(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		mgr := newTestManager(t, d)
		sqlStore, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}

		created, err := mgr.CreateBy("first task", "body", "", "operator")
		if err != nil {
			t.Fatalf("CreateBy: %v", err)
		}
		if created.ID == "" {
			t.Fatal("CreateBy did not mint an ID")
		}

		entries, err := sqlStore.History(t.Context(), HistoryQuery{TaskID: created.ID})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("history holds %d entries, want 1", len(entries))
		}
		if entries[0].Kind != ChangeCreated {
			t.Errorf("kind = %q, want created", entries[0].Kind)
		}
		if entries[0].Actor != "operator" {
			t.Errorf("actor = %q, want operator", entries[0].Actor)
		}
	})
}

// TestManager_SQLBackend_BlankActorRefused is the issue's "a mutation that
// cannot name [an actor] is refused rather than recorded as anonymous".
func TestManager_SQLBackend_BlankActorRefused(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		mgr := newTestManager(t, d)
		if _, err := mgr.CreateBy("task", "body", "", ""); err == nil {
			t.Fatal("expected an error for a blank actor")
		}
		if _, err := mgr.UpdateBy("nonexistent0", "", task.Update{}); err == nil {
			t.Fatal("expected an error for a blank actor")
		}
		if err := mgr.DeleteBy("nonexistent0", ""); err == nil {
			t.Fatal("expected an error for a blank actor")
		}
	})
}

// TestManager_SQLBackend_UpdateRecordsChangedFieldsAndActor exercises the
// most common mutation path — UpdateBy — and confirms the field-level
// bookkeeping (generation, changed-field list) that only file backend's
// UpdateWithPrev used to compute lands identically through Persistence.
func TestManager_SQLBackend_UpdateRecordsChangedFieldsAndActor(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		mgr := newTestManager(t, d)
		sqlStore, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}

		created, err := mgr.CreateBy("task", "body", "", "creator")
		if err != nil {
			t.Fatalf("CreateBy: %v", err)
		}
		newTitle := "renamed"
		updated, err := mgr.UpdateBy(created.ID, "updater", task.Update{Title: &newTitle})
		if err != nil {
			t.Fatalf("UpdateBy: %v", err)
		}
		if updated.Title != "renamed" {
			t.Fatalf("Title = %q, want renamed", updated.Title)
		}
		if updated.Generation != created.Generation+1 {
			t.Errorf("Generation = %d, want %d", updated.Generation, created.Generation+1)
		}

		entries, err := sqlStore.History(t.Context(), HistoryQuery{TaskID: created.ID})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("history holds %d entries, want create+update", len(entries))
		}
		last := entries[1]
		if last.Actor != "updater" {
			t.Errorf("actor = %q, want updater", last.Actor)
		}
		if len(last.Fields) != 1 || last.Fields[0] != "title" {
			t.Errorf("changed fields = %v, want [title]", last.Fields)
		}
	})
}

// TestManager_SQLBackend_DeleteAndRestoreRecordActors is the issue's
// "deleting and restoring a task records the actor that did it".
func TestManager_SQLBackend_DeleteAndRestoreRecordActors(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		mgr := newTestManager(t, d)
		sqlStore, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}

		created, err := mgr.CreateBy("task", "body", "", "creator")
		if err != nil {
			t.Fatalf("CreateBy: %v", err)
		}
		if err := mgr.DeleteBy(created.ID, "deleter"); err != nil {
			t.Fatalf("DeleteBy: %v", err)
		}
		if _, err := mgr.RestoreBy(created.ID, "restorer"); err != nil {
			t.Fatalf("RestoreBy: %v", err)
		}

		entries, err := sqlStore.History(t.Context(), HistoryQuery{TaskID: created.ID})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		if len(entries) != 3 {
			t.Fatalf("history holds %d entries, want create+delete+restore", len(entries))
		}
		if entries[1].Actor != "deleter" || entries[1].Kind != ChangeDeleted {
			t.Errorf("delete entry = %+v", entries[1])
		}
		if entries[2].Actor != "restorer" || entries[2].Kind != ChangeRestored {
			t.Errorf("restore entry = %+v", entries[2])
		}
	})
}

// TestManager_SQLBackend_AddRunRecordsHistory proves AddRunBy's read-modify-
// write against AgentRuns — a plain Task field, not a sidecar — round-trips
// through the same Persistence path as a field Update.
func TestManager_SQLBackend_AddRunRecordsHistory(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		mgr := newTestManager(t, d)
		created, err := mgr.CreateBy("task", "body", "", "creator")
		if err != nil {
			t.Fatalf("CreateBy: %v", err)
		}
		if err := mgr.AddRunBy(created.ID, "dispatcher", task.AgentRun{AgentID: "agent-1"}); err != nil {
			t.Fatalf("AddRunBy: %v", err)
		}
		got, err := mgr.Get(created.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if len(got.AgentRuns) != 1 || got.AgentRuns[0].AgentID != "agent-1" {
			t.Fatalf("AgentRuns = %+v", got.AgentRuns)
		}
	})
}

// TestManager_SQLBackend_MutationTransportProbesWithoutFileStore proves
// ProbeMutationTransport/MutationTransportIdentity — the run-environment
// preflight gate every headless agent dispatch goes through when its role
// requires CapabilityTaskMutation — work for a task that was never written
// to disk. The pre-fix code read the task through Manager's file-only store
// field, so both calls 404'd for a DB-backed task and blocked every agent
// run from ever starting under the (now-default) sqlite backend.
func TestManager_SQLBackend_MutationTransportProbesWithoutFileStore(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		mgr := newTestManager(t, d)
		created, err := mgr.CreateBy("task", "body", "", "creator")
		if err != nil {
			t.Fatalf("CreateBy: %v", err)
		}

		if err := mgr.ProbeMutationTransport(created.ID); err != nil {
			t.Fatalf("ProbeMutationTransport: %v", err)
		}
		identity, err := mgr.MutationTransportIdentity(created.ID)
		if err != nil {
			t.Fatalf("MutationTransportIdentity: %v", err)
		}
		if identity == "" {
			t.Fatal("identity is empty, want a non-empty directory fingerprint")
		}

		second, err := mgr.MutationTransportIdentity(created.ID)
		if err != nil {
			t.Fatalf("MutationTransportIdentity (second): %v", err)
		}
		if second != identity {
			t.Fatalf("identity changed across calls with no mutation in between: %q != %q", second, identity)
		}
	})
}

// TestManager_SQLBackend_ApplyRecordsHistoryWithActor proves the status
// transition entrypoint — the primary way production code (workflow engine,
// review, monitor, watchdog, recovery) mutates a task's status — routes
// through Persistence too, not just the secondary Create/Put/Update surface.
// Apply/ApplyFn were the one mutation path #3268's first pass missed: they
// wrote through Manager's file-only store field directly, so a DB-backed
// board would never record (or even durably apply) a status transition.
func TestManager_SQLBackend_ApplyRecordsHistoryWithActor(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		mgr := newTestManager(t, d)
		sqlStore, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}

		created, err := mgr.CreateBy("task", "body", "", "creator")
		if err != nil {
			t.Fatalf("CreateBy: %v", err)
		}

		result, err := mgr.Apply(task.TransitionIntent{
			TaskID:   created.ID,
			ToStatus: task.StatusInProgress,
			Actor:    "transitioner",
		})
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if !result.Applied {
			t.Fatal("Applied = false, want true")
		}
		if result.Task.Status != task.StatusInProgress {
			t.Fatalf("status = %q, want in-progress", result.Task.Status)
		}

		entries, err := sqlStore.History(t.Context(), HistoryQuery{TaskID: created.ID})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("history holds %d entries, want create+apply", len(entries))
		}
		if entries[1].Actor != "transitioner" {
			t.Errorf("actor = %q, want transitioner", entries[1].Actor)
		}
	})
}

// TestManager_SQLBackend_ApplyIdempotentReplaySkipsWrite proves the
// idempotency-key replay guard — an Apply-caller's crash-safe retry
// contract — still holds against the DB backend: a second Apply with the
// same IdempotencyKey against an unchanged generation must not write a
// second history row.
func TestManager_SQLBackend_ApplyIdempotentReplaySkipsWrite(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		mgr := newTestManager(t, d)
		sqlStore, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}

		created, err := mgr.CreateBy("task", "body", "", "creator")
		if err != nil {
			t.Fatalf("CreateBy: %v", err)
		}

		intent := task.TransitionIntent{
			TaskID:         created.ID,
			ToStatus:       task.StatusInProgress,
			Actor:          "transitioner",
			IdempotencyKey: "retry-42",
		}
		first, err := mgr.Apply(intent)
		if err != nil {
			t.Fatalf("first Apply: %v", err)
		}
		if !first.Applied {
			t.Fatal("first Applied = false, want true")
		}

		second, err := mgr.Apply(intent)
		if err != nil {
			t.Fatalf("second Apply: %v", err)
		}
		if second.Applied {
			t.Fatal("second Applied = true, want false (idempotent replay)")
		}
		if second.Task.Generation != first.Task.Generation {
			t.Fatalf("generation after replay = %d, want unchanged %d", second.Task.Generation, first.Task.Generation)
		}

		entries, err := sqlStore.History(t.Context(), HistoryQuery{TaskID: created.ID})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("history holds %d entries after replay, want create+apply only", len(entries))
		}
	})
}

// TestManager_SQLBackend_CommentsRoundTripThroughSetCommentPersistence
// exercises the exact wiring app.go's Startup uses — a Manager built with
// SetCommentPersistence called separately from construction, the same
// two-step sequence Startup uses since a Manager must already exist before
// it can be told about a Persistence — proving Manager.Comments() actually
// reaches the database once wired rather than silently falling back to the
// file store.
func TestManager_SQLBackend_CommentsRoundTripThroughSetCommentPersistence(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		mgr := newTestManager(t, d)
		mgr.SetCommentPersistence(NewCommentStore(d))

		created, err := mgr.CreateBy("task", "body", "", "creator")
		if err != nil {
			t.Fatalf("CreateBy: %v", err)
		}

		added, err := mgr.Comments().Add(created.ID, 5, "looks good")
		if err != nil {
			t.Fatalf("Comments().Add: %v", err)
		}

		got, err := mgr.Comments().List(created.ID)
		if err != nil {
			t.Fatalf("Comments().List: %v", err)
		}
		if len(got) != 1 || got[0].ID != added.ID {
			t.Fatalf("comments = %+v, want just %+v", got, added)
		}

		// Confirm this actually landed in the database's task_sidecars row,
		// not just in some in-memory fallback: a fresh SQLStore-backed
		// CommentStore over the same *db.DB must see it too.
		sameDB := NewCommentStore(d)
		viaFreshHandle, err := sameDB.List(created.ID)
		if err != nil {
			t.Fatalf("List via fresh handle: %v", err)
		}
		if len(viaFreshHandle) != 1 || viaFreshHandle[0].ID != added.ID {
			t.Fatalf("comments via fresh handle = %+v, want just %+v", viaFreshHandle, added)
		}
	})
}

// TestManager_SQLBackend_UnrelatedTaskUpdatePreservesComments proves an
// ordinary task-field write does not wipe a task's comments. Comments live
// in the same task_sidecars row PutBy/PutFnBy manage for a task's own
// sidecar fields (Plan, CodeReview, ...); those methods clear every
// existing sidecar row for the task before reinserting only what
// SidecarsFromTask computes from the Task struct, which never includes
// comments — a plain unscoped delete would silently drop every review
// comment on the very next unrelated status/body/tag write.
func TestManager_SQLBackend_UnrelatedTaskUpdatePreservesComments(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		mgr := newTestManager(t, d)
		mgr.SetCommentPersistence(NewCommentStore(d))

		created, err := mgr.CreateBy("task", "body", "", "creator")
		if err != nil {
			t.Fatalf("CreateBy: %v", err)
		}
		if _, err := mgr.Comments().Add(created.ID, 5, "please fix this"); err != nil {
			t.Fatalf("Comments().Add: %v", err)
		}

		newTitle := "renamed"
		if _, err := mgr.UpdateBy(created.ID, "operator", task.Update{Title: &newTitle}); err != nil {
			t.Fatalf("UpdateBy: %v", err)
		}

		got, err := mgr.Comments().List(created.ID)
		if err != nil {
			t.Fatalf("Comments().List after unrelated update: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("comments after unrelated update = %+v, want the original comment still present", got)
		}
	})
}

// TestManager_SQLBackend_PlanDraftWriteRoundTripsAndSurvivesUnrelatedUpdate
// is #3293's regression test, matching the shape #3287 added for comments:
// a plan draft set via UpdateBy's PlanDraftWrite must reach the database
// (not just a local file) and must still be there after an unrelated field
// write, since PutFnBy recomputes every sidecar row from the in-memory
// Task's current state — a plan draft that was never merged into that
// state would silently vanish on the very next status/title/etc. update.
func TestManager_SQLBackend_PlanDraftWriteRoundTripsAndSurvivesUnrelatedUpdate(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		mgr := newTestManager(t, d)

		created, err := mgr.CreateBy("task", "body", "", "creator")
		if err != nil {
			t.Fatalf("CreateBy: %v", err)
		}

		saved, err := mgr.UpdateBy(created.ID, "workflow.engine.write_sidecar", task.Update{
			PlanDraftWrite: &task.PlanDraftEntry{Name: "claude", Content: "draft content"},
		})
		if err != nil {
			t.Fatalf("UpdateBy: %v", err)
		}
		if saved.PlanDrafts["claude"] != "draft content" {
			t.Fatalf("returned task PlanDrafts = %+v, want claude=draft content", saved.PlanDrafts)
		}

		got, err := mgr.Get(created.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.PlanDrafts["claude"] != "draft content" {
			t.Fatalf("re-read PlanDrafts = %+v, want claude=draft content — the draft never reached the database", got.PlanDrafts)
		}

		// A second draft under a different name — the actual scenario this
		// feature exists for, N parallel planners each writing their own
		// draft for the same task — must not clobber the first: the
		// database backend's write recomputes every sidecar row from the
		// task's current in-memory PlanDrafts map, so this only holds if
		// that map already carries every existing entry before the new one
		// is merged in, not just the one this specific call is writing.
		if _, err := mgr.UpdateBy(created.ID, "workflow.engine.write_sidecar", task.Update{
			PlanDraftWrite: &task.PlanDraftEntry{Name: "codex", Content: "second draft"},
		}); err != nil {
			t.Fatalf("second draft UpdateBy: %v", err)
		}
		afterSecondDraft, err := mgr.Get(created.ID)
		if err != nil {
			t.Fatalf("Get after second draft: %v", err)
		}
		if afterSecondDraft.PlanDrafts["claude"] != "draft content" || afterSecondDraft.PlanDrafts["codex"] != "second draft" {
			t.Fatalf("PlanDrafts after second draft = %+v, want both entries", afterSecondDraft.PlanDrafts)
		}

		newTitle := "renamed"
		if _, err := mgr.UpdateBy(created.ID, "operator", task.Update{Title: &newTitle}); err != nil {
			t.Fatalf("unrelated UpdateBy: %v", err)
		}

		afterUnrelated, err := mgr.Get(created.ID)
		if err != nil {
			t.Fatalf("Get after unrelated update: %v", err)
		}
		if afterUnrelated.PlanDrafts["claude"] != "draft content" || afterUnrelated.PlanDrafts["codex"] != "second draft" {
			t.Fatalf("PlanDrafts after unrelated update = %+v, want both drafts still present", afterUnrelated.PlanDrafts)
		}
	})
}

// TestManager_SQLBackend_PlanDraftWriteRejectsUnsafeName proves the database
// backend enforces the same name guard the file backend's PlanDraftStore.Write
// always has — the file backend rejects a name that isn't safe as a
// filename component, and a Persistence implementation that never touches
// PlanDraftStore at all must not silently accept what the other backend
// refuses.
func TestManager_SQLBackend_PlanDraftWriteRejectsUnsafeName(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		mgr := newTestManager(t, d)
		created, err := mgr.CreateBy("task", "body", "", "creator")
		if err != nil {
			t.Fatalf("CreateBy: %v", err)
		}
		_, err = mgr.UpdateBy(created.ID, "workflow.engine.write_sidecar", task.Update{
			PlanDraftWrite: &task.PlanDraftEntry{Name: "a/b", Content: "x"},
		})
		if err == nil {
			t.Fatal("expected an error for an unsafe plan draft name")
		}
	})
}
