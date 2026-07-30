package cleanup

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newProtectedStore(t *testing.T) *ProtectedStore {
	t.Helper()
	store := NewProtectedStore(filepath.Join(t.TempDir(), "protected-findings.json"))
	store.now = func() time.Time {
		return time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	}
	store.reminderWindow = time.Hour
	return store
}

func TestProtectedStoreObserveDeduplicatesWithinReminderWindow(t *testing.T) {
	t.Parallel()
	store := newProtectedStore(t)
	obs := Observation{
		Kind:          ResourceWorktree,
		TaskID:        "task-1",
		Path:          "/tmp/worktree-1",
		Reason:        ReasonUnpushedCommits,
		ObservedHead:  "abc123",
		ObservedState: "dirty=false",
		BytesRetained: 128,
	}

	first, event, err := store.Observe(obs)
	if err != nil {
		t.Fatalf("Observe(first): %v", err)
	}
	if event != ObserveCreated {
		t.Fatalf("first event = %q, want %q", event, ObserveCreated)
	}

	second, event, err := store.Observe(obs)
	if err != nil {
		t.Fatalf("Observe(second): %v", err)
	}
	if event != ObserveUnchanged {
		t.Fatalf("second event = %q, want %q", event, ObserveUnchanged)
	}
	if second.LastLoggedAt != first.LastLoggedAt {
		t.Fatalf("LastLoggedAt changed unexpectedly: first=%s second=%s", first.LastLoggedAt, second.LastLoggedAt)
	}
}

func TestProtectedStoreObserveStateChangeReopensDiscardedFinding(t *testing.T) {
	t.Parallel()
	store := newProtectedStore(t)
	obs := Observation{
		Kind:          ResourceWorktree,
		TaskID:        "task-1",
		Path:          "/tmp/worktree-1",
		Reason:        ReasonUnpushedCommits,
		ObservedHead:  "abc123",
		ObservedState: "dirty=false",
		BytesRetained: 128,
	}
	first, _, err := store.Observe(obs)
	if err != nil {
		t.Fatalf("Observe(first): %v", err)
	}
	if _, err := store.Discard(first.ID); err != nil {
		t.Fatalf("Discard: %v", err)
	}

	store.now = func() time.Time {
		return time.Date(2026, 7, 30, 13, 0, 0, 0, time.UTC)
	}
	next := obs
	next.ObservedHead = "def456"
	next.ObservedState = "dirty=true"
	got, event, err := store.Observe(next)
	if err != nil {
		t.Fatalf("Observe(reopen): %v", err)
	}
	if event != ObserveReopened {
		t.Fatalf("event = %q, want %q", event, ObserveReopened)
	}
	if got.State != FindingOpen {
		t.Fatalf("State = %q, want %q", got.State, FindingOpen)
	}
	if got.ObservedHead != "def456" || got.ObservedState != "dirty=true" {
		t.Fatalf("updated finding = %+v", got)
	}
}

func TestProtectedStoreRescueFailureLeavesFindingOpen(t *testing.T) {
	t.Parallel()
	store := newProtectedStore(t)
	dir := filepath.Join(t.TempDir(), "not-a-git-repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got, _, err := store.Observe(Observation{
		Kind:          ResourceWorktree,
		TaskID:        "task-1",
		Path:          dir,
		Reason:        ReasonUnpushedCommits,
		ObservedHead:  "abc123",
		ObservedState: "dirty=false",
		BytesRetained: 64,
	})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}

	if _, err := store.Rescue(got.ID); err == nil {
		t.Fatal("Rescue() error = nil, want git rescue failure")
	}
	reloaded, ok, err := store.Get(got.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("Get() ok = false, want finding to remain present")
	}
	if reloaded.State != FindingOpen {
		t.Fatalf("State after failed rescue = %q, want %q", reloaded.State, FindingOpen)
	}
}

func TestProtectedStoreObserveConcurrentScansDeduplicates(t *testing.T) {
	t.Parallel()
	store := newProtectedStore(t)
	obs := Observation{
		Kind:          ResourceSandbox,
		TaskID:        "task-2",
		Path:          "/tmp/sandbox-2",
		Reason:        ReasonUnpushedCommits,
		ObservedState: "bytes=32",
		BytesRetained: 32,
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, _, err := store.Observe(obs); err != nil {
				t.Errorf("Observe: %v", err)
			}
		})
	}
	wg.Wait()

	findings, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("len(findings) = %d, want 1", len(findings))
	}
}
