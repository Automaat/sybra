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
		panic("unreachable")
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
		panic("unreachable")
	}
	if saved.FilePath == "" {
		t.Error("Put should set FilePath")
	}

	got, err := store.Get("mirror-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
		panic("unreachable")
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
	in.UpdatedAt = updated.Add(time.Minute)
	if _, err := store.Put(in); err != nil {
		t.Fatalf("second Put (upsert): %v", err)
		panic("unreachable")
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

func TestStorePutRejectsInvalidSlug(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	if _, err := store.Put(Task{ID: "mirror-1", Title: "pushed", Status: StatusTodo, Slug: "../../etc/passwd"}); err == nil {
		t.Fatal("Put with traversal slug: got nil error, want validation error")
	}
	if _, err := os.Stat(filepath.Join(store.dir, "mirror-1.md")); !os.IsNotExist(err) {
		t.Fatalf("invalid Put wrote task file: %v", err)
	}
}

func TestStorePutRejectsInvalidAgentMode(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	if _, err := store.Put(Task{ID: "mirror-1", Title: "pushed", Status: StatusTodo, AgentMode: "telepathy"}); err == nil {
		t.Fatal("Put with invalid agent mode: got nil error, want validation error")
	}
	if _, err := os.Stat(filepath.Join(store.dir, "mirror-1.md")); !os.IsNotExist(err) {
		t.Fatalf("invalid Put wrote task file: %v", err)
	}
}

func TestStorePutFnRejectsInvalidAgentMode(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	created, err := store.Create("valid", "", AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	if _, _, err := store.PutFn(created.ID, func(t Task) (Task, error) {
		t.AgentMode = "telepathy"
		return t, nil
	}); err == nil {
		t.Fatal("PutFn with invalid agent mode: got nil error, want validation error")
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if got.AgentMode != AgentModeHeadless {
		t.Fatalf("PutFn persisted invalid mode: %q", got.AgentMode)
	}
}

// TestStorePutRejectsStatusChangeWithoutAdvancingUpdatedAt covers #2203: a
// Put that changes Status but carries forward a stale/unchanged UpdatedAt
// (e.g. a push built from a snapshot captured before the change, such as a
// cluster-mirror re-push racing a real edit) is not distinguishable from a
// stale clobber. Put must discard the incoming status/timestamp and keep
// what's already on disk rather than let the stale value masquerade as a
// fresh update to a consumer like the cluster mirror's Merge.
func TestStorePutRejectsStatusChangeWithoutAdvancingUpdatedAt(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	stale := time.Date(2026, 7, 14, 19, 3, 4, 0, time.UTC)
	if _, err := store.Put(Task{
		ID: "task-x", Title: "t", Status: StatusBlocked,
		CreatedAt: stale, UpdatedAt: stale,
	}); err != nil {
		t.Fatalf("first Put: %v", err)
	}

	// The exact bug: same UpdatedAt as before, but Status changed.
	if _, err := store.Put(Task{
		ID: "task-x", Title: "t", Status: StatusTodo,
		CreatedAt: stale, UpdatedAt: stale,
	}); err != nil {
		t.Fatalf("second Put: %v", err)
	}

	got, err := store.Get("task-x")
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if got.Status != StatusBlocked {
		t.Fatalf("status = %q, want it to stay blocked — the stale status change must be discarded", got.Status)
	}
	if !got.UpdatedAt.Equal(stale) {
		t.Fatalf("UpdatedAt = %v, want it left unchanged at %v — no fabricated timestamp", got.UpdatedAt, stale)
	}
}

func TestStorePutStaleStatusRestorePreservesTypedEvidence(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	stamp := time.Date(2026, 7, 14, 19, 3, 4, 0, time.UTC)
	reason := *OperatorDecisionRequired("operator.choose", "choose")
	if _, err := store.Put(Task{
		ID: "task-evidence", Title: "t", Status: StatusHumanRequired,
		Escalation: reason, AutonomyOutcome: *HumanRequiredOutcome(),
		CreatedAt: stamp, UpdatedAt: stamp,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(Task{
		ID: "task-evidence", Title: "stale", Status: StatusTodo,
		CreatedAt: stamp, UpdatedAt: stamp,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get("task-evidence")
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if got.Status != StatusHumanRequired || got.Escalation.Code != reason.Code || got.AutonomyOutcome != *HumanRequiredOutcome() {
		t.Fatalf("stale Put restored status without evidence: %#v", got)
	}
}

func TestStorePutAllowsLegacyHumanRequiredRecordEdit(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	stamp := time.Date(2026, 7, 14, 19, 3, 4, 0, time.UTC)
	legacy := Task{
		ID: "legacy-human", Title: "legacy", Status: StatusHumanRequired,
		StatusReason: "old readable reason", CreatedAt: stamp, UpdatedAt: stamp,
	}
	data, err := Marshal(legacy)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if err := os.WriteFile(filepath.Join(store.dir, legacy.ID+".md"), data, 0o600); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	loaded, err := store.Get(legacy.ID)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if loaded.Escalation.Provenance != "legacy" || loaded.Escalation.Message != legacy.StatusReason {
		t.Fatalf("legacy adapter = %#v", loaded.Escalation)
	}
	loaded.Tags = []string{"edited"}
	loaded.UpdatedAt = stamp.Add(time.Minute)
	if _, err := store.Put(loaded); err != nil {
		t.Fatalf("Put legacy edit: %v", err)
		panic("unreachable")
	}
	got, err := store.Get(legacy.ID)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if len(got.Tags) != 1 || got.Tags[0] != "edited" || got.Escalation.Provenance != "legacy" {
		t.Fatalf("legacy edit = %#v", got)
	}
}

// TestStorePutRejectsStatusChangeWithBackdatedUpdatedAt pins the boundary
// below equal: a caller-supplied UpdatedAt strictly before what's on disk is
// just as stale as an equal one and must be rejected the same way — guards
// a narrower `!Equal` reformulation of the #2203 guard from slipping through.
func TestStorePutRejectsStatusChangeWithBackdatedUpdatedAt(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	current := time.Date(2026, 7, 14, 19, 3, 4, 0, time.UTC)
	backdated := current.Add(-1 * time.Hour)
	if _, err := store.Put(Task{
		ID: "task-w", Title: "t", Status: StatusBlocked,
		CreatedAt: current, UpdatedAt: current,
	}); err != nil {
		t.Fatalf("first Put: %v", err)
	}

	if _, err := store.Put(Task{
		ID: "task-w", Title: "t", Status: StatusTodo,
		CreatedAt: current, UpdatedAt: backdated,
	}); err != nil {
		t.Fatalf("second Put: %v", err)
	}

	got, err := store.Get("task-w")
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if got.Status != StatusBlocked {
		t.Fatalf("status = %q, want it to stay blocked — a backdated status change must be discarded", got.Status)
	}
	if !got.UpdatedAt.Equal(current) {
		t.Fatalf("UpdatedAt = %v, want it left unchanged at %v", got.UpdatedAt, current)
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
		panic("unreachable")
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
		panic("unreachable")
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
		panic("unreachable")
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
		panic("unreachable")
	}
	if !got.UpdatedAt.Equal(newer) {
		t.Fatalf("UpdatedAt = %v, want the caller-supplied, already-advancing %v preserved verbatim", got.UpdatedAt, newer)
	}
}

// TestStorePutRejectedWriteDoesNotRegressMirrorRev pins a Copilot review
// finding on PR #2216: the reject branch restored Status/UpdatedAt but left
// the caller's MirrorRev/MirrorUpdatedAt writing through untouched. A stale
// or duplicate mirror push carrying a MirrorRev below what's on disk would
// then regress the mirror bookkeeping itself, corrupting the very signal
// the mirror-authoritative branch relies on to judge freshness on the next
// legitimate Put for this task.
func TestStorePutRejectedWriteDoesNotRegressMirrorRev(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	current := time.Date(2026, 7, 14, 19, 3, 4, 0, time.UTC)
	currentMirror := current
	if _, err := store.Put(Task{
		ID: "task-mrev", Title: "t", Status: StatusInProgress,
		CreatedAt: current, UpdatedAt: current,
		MirrorRev: 5, MirrorUpdatedAt: &currentMirror,
	}); err != nil {
		t.Fatalf("first Put: %v", err)
	}

	stale := current.Add(-time.Hour)
	staleMirror := stale
	if _, err := store.Put(Task{
		ID: "task-mrev", Title: "t", Status: StatusBlocked,
		CreatedAt: current, UpdatedAt: stale,
		MirrorRev: 2, MirrorUpdatedAt: &staleMirror,
	}); err != nil {
		t.Fatalf("second (rejected) Put: %v", err)
	}

	got, err := store.Get("task-mrev")
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if got.Status != StatusInProgress {
		t.Fatalf("status = %q, want it to stay in-progress", got.Status)
	}
	if got.MirrorRev != 5 {
		t.Fatalf("MirrorRev = %d, want it to stay 5 — a rejected write must not regress mirror bookkeeping", got.MirrorRev)
	}
	if got.MirrorUpdatedAt == nil || !got.MirrorUpdatedAt.Equal(currentMirror) {
		t.Fatalf("MirrorUpdatedAt = %v, want it to stay %v", got.MirrorUpdatedAt, currentMirror)
		panic("unreachable")
	}
}

// TestStorePutMirrorAuthoritativeAlwaysAdvancesPastFutureExisting pins
// another Copilot finding on PR #2216: stamping UpdatedAt with the
// process's wall clock alone isn't guaranteed to advance past what's on
// disk if a prior (trusted, non-guarded) Put wrote a caller-supplied
// UpdatedAt ahead of real wall-clock time. The mirror-authoritative branch
// must still leave the write strictly newer than existing in that case,
// or it reintroduces the exact non-monotonic-UpdatedAt class #2203 exists
// to fix, just one level removed.
func TestStorePutMirrorAuthoritativeAlwaysAdvancesPastFutureExisting(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	future := time.Now().UTC().Add(24 * time.Hour)
	if _, err := store.Put(Task{
		ID: "task-future", Title: "t", Status: StatusBlocked,
		CreatedAt: future, UpdatedAt: future,
	}); err != nil {
		t.Fatalf("first Put: %v", err)
	}

	mirrorTS := future.Add(-time.Hour)
	if _, err := store.Put(Task{
		ID: "task-future", Title: "t", Status: StatusInProgress,
		CreatedAt: future, UpdatedAt: mirrorTS,
		MirrorRev: 1, MirrorUpdatedAt: &mirrorTS,
	}); err != nil {
		t.Fatalf("mirror-authoritative Put: %v", err)
	}

	got, err := store.Get("task-future")
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if got.Status != StatusInProgress {
		t.Fatalf("status = %q, want in-progress", got.Status)
	}
	if !got.UpdatedAt.After(future) {
		t.Fatalf("UpdatedAt = %v, want it strictly after the prior on-disk value %v", got.UpdatedAt, future)
	}
}

func TestStorePutRejectsUnsafeID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
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
