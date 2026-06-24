package task

import (
	"os"
	"path/filepath"
	"testing"
)

// mustListDrafts lists drafts for id, failing the test on an unexpected error
// so assertions never run against a map produced by a hidden failure.
func mustListDrafts(t *testing.T, s *PlanDraftStore, id string) map[string]string {
	t.Helper()
	got, err := s.List(id)
	if err != nil {
		t.Fatalf("List(%s): %v", id, err)
	}
	return got
}

func TestPlanDraftStore_WriteListReadDelete(t *testing.T) {
	s := NewPlanDraftStore(t.TempDir())

	if err := s.Write("task1", "plan_claude", "claude body"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.Write("task1", "plan_codex", "codex body"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := s.List("task1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 || got["plan_claude"] != "claude body" || got["plan_codex"] != "codex body" {
		t.Fatalf("List = %v, want both drafts", got)
	}

	if v, _ := s.Read("task1", "plan_claude"); v != "claude body" {
		t.Fatalf("Read = %q", v)
	}

	if err := s.Delete("task1", "plan_claude"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got = mustListDrafts(t, s, "task1")
	if len(got) != 1 || got["plan_codex"] != "codex body" {
		t.Fatalf("after Delete List = %v, want only plan_codex", got)
	}
}

// TestPlanDraftStore_NegativeCache is the correctness guard for the index:
// a List on a draft-less task must stay correct after drafts are written to
// OTHER tasks and after a draft is later written to the task itself.
func TestPlanDraftStore_NegativeCache(t *testing.T) {
	s := NewPlanDraftStore(t.TempDir())

	// Prime the index with a full scan that observes zero drafts.
	if got := mustListDrafts(t, s, "taskA"); len(got) != 0 {
		t.Fatalf("initial List = %v, want empty", got)
	}
	if !s.indexed {
		t.Fatal("index should be populated after a List scan")
	}

	// A write to a different task must invalidate the index so taskB is
	// discoverable rather than masked by the cached empty set.
	if err := s.Write("taskB", "plan_claude", "b"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if s.indexed {
		t.Fatal("write must invalidate the index")
	}
	if got := mustListDrafts(t, s, "taskB"); got["plan_claude"] != "b" {
		t.Fatalf("List(taskB) = %v, want the freshly written draft", got)
	}
	// taskA still has no drafts.
	if got := mustListDrafts(t, s, "taskA"); len(got) != 0 {
		t.Fatalf("List(taskA) = %v, want empty", got)
	}

	// A later write to taskA itself must surface immediately.
	if err := s.Write("taskA", "plan_codex", "a"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := mustListDrafts(t, s, "taskA"); got["plan_codex"] != "a" {
		t.Fatalf("List(taskA) = %v, want the new draft", got)
	}
}

// TestPlanDraftStore_ExternalWriteInvalidates simulates an out-of-process
// draft file appearing on disk (e.g. another Sybra instance). The Store-level
// watcher hook must drop the index so the new draft is not masked.
func TestPlanDraftStore_ExternalWriteInvalidates(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// Prime the negative cache: taskX has no drafts.
	if got := mustListDrafts(t, store.planDrafts, "taskX"); len(got) != 0 {
		t.Fatalf("initial List = %v, want empty", got)
	}

	// Simulate an external process dropping a draft file directly on disk.
	external := filepath.Join(dir, "taskX"+PlanDraftSidecarPrefix+"plan_ext.md")
	if err := os.WriteFile(external, []byte("ext body"), 0o644); err != nil {
		t.Fatalf("external write: %v", err)
	}

	// Watcher notifies the store of the sidecar change.
	store.InvalidatePath(external)

	got := mustListDrafts(t, store.planDrafts, "taskX")
	if got["plan_ext"] != "ext body" {
		t.Fatalf("List(taskX) = %v, want the externally written draft", got)
	}
}
