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

// TestStorePutBumpsUpdatedAtWhenStatusChangesWithoutAdvancing covers #2203: a
// Put that changes Status but carries forward a stale/unchanged UpdatedAt
// (e.g. a push built from a snapshot captured before the change) silently
// defeats any consumer gating on strictly-newer to detect the update — the
// cluster mirror's own staleness guard among them. Put must correct the
// timestamp rather than trust the caller in this one specific case.
func TestStorePutBumpsUpdatedAtWhenStatusChangesWithoutAdvancing(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	stale := time.Date(2026, 7, 14, 19, 3, 4, 0, time.UTC)
	if _, err := store.Put(Task{
		ID: "task-x", Title: "t", Status: StatusBlocked,
		CreatedAt: stale, UpdatedAt: stale,
	}); err != nil {
		t.Fatalf("first Put: %v", err)
	}

	// Same UpdatedAt as before, but Status changed — the exact bug: a caller
	// that forgot to advance the timestamp on a real status change.
	if _, err := store.Put(Task{
		ID: "task-x", Title: "t", Status: StatusTodo,
		CreatedAt: stale, UpdatedAt: stale,
	}); err != nil {
		t.Fatalf("second Put: %v", err)
	}

	got, err := store.Get("task-x")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusTodo {
		t.Fatalf("status = %q, want todo", got.Status)
	}
	if !got.UpdatedAt.After(stale) {
		t.Fatalf("UpdatedAt = %v, want it bumped past the stale caller-supplied value %v", got.UpdatedAt, stale)
	}
}

// TestStorePutKeepsVerbatimUpdatedAtWhenStatusUnchanged pins that the #2203
// guard is scoped to an actual status change — a same-status Put (the common
// idempotent-repush case) must still write UpdatedAt exactly as supplied.
func TestStorePutKeepsVerbatimUpdatedAtWhenStatusUnchanged(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	stale := time.Date(2026, 7, 14, 19, 3, 4, 0, time.UTC)
	if _, err := store.Put(Task{
		ID: "task-y", Title: "t", Status: StatusTodo,
		CreatedAt: stale, UpdatedAt: stale,
	}); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if _, err := store.Put(Task{
		ID: "task-y", Title: "t retitled", Status: StatusTodo,
		CreatedAt: stale, UpdatedAt: stale,
	}); err != nil {
		t.Fatalf("second Put: %v", err)
	}

	got, err := store.Get("task-y")
	if err != nil {
		t.Fatal(err)
	}
	if !got.UpdatedAt.Equal(stale) {
		t.Fatalf("UpdatedAt = %v, want unchanged verbatim %v — status did not change, no bump should apply", got.UpdatedAt, stale)
	}
}

// TestStorePutTrustsGenuinelyAdvancingUpdatedAt pins the normal, correct
// case unaffected: a status change whose caller-supplied UpdatedAt already
// advances past what's on disk (the cluster mirror applying a real, newer
// follower update) is written verbatim, no defensive bump needed or applied.
func TestStorePutTrustsGenuinelyAdvancingUpdatedAt(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	older := time.Date(2026, 7, 14, 19, 3, 4, 0, time.UTC)
	newer := time.Date(2026, 7, 16, 7, 3, 50, 0, time.UTC)
	if _, err := store.Put(Task{
		ID: "task-z", Title: "t", Status: StatusInProgress,
		CreatedAt: older, UpdatedAt: older,
	}); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if _, err := store.Put(Task{
		ID: "task-z", Title: "t", Status: StatusDone,
		CreatedAt: older, UpdatedAt: newer,
	}); err != nil {
		t.Fatalf("second Put: %v", err)
	}

	got, err := store.Get("task-z")
	if err != nil {
		t.Fatal(err)
	}
	if !got.UpdatedAt.Equal(newer) {
		t.Fatalf("UpdatedAt = %v, want the caller-supplied, already-advancing %v preserved verbatim", got.UpdatedAt, newer)
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
