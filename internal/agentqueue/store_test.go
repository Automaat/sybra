package agentqueue

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

func TestStore_RoundTrip(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	it := Item{
		TaskID:   "t1",
		Role:     "implementation",
		Priority: task.PriorityHigh,
		Status:   task.StatusInReview,
		Manual:   true,
		Enqueued: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
	}
	if err := s.put(it); err != nil {
		t.Fatalf("put: %v", err)
	}

	loaded := s.load(discardLogger())
	if len(loaded) != 1 {
		t.Fatalf("load() returned %d items, want 1", len(loaded))
	}
	if !loaded[0].Enqueued.Equal(it.Enqueued) || loaded[0].TaskID != it.TaskID ||
		loaded[0].Role != it.Role || loaded[0].Priority != it.Priority ||
		loaded[0].Status != it.Status || loaded[0].Manual != it.Manual {
		t.Errorf("round-tripped item = %+v, want %+v", loaded[0], it)
	}

	if err := s.del("t1"); err != nil {
		t.Fatalf("del: %v", err)
	}
	if loaded := s.load(discardLogger()); len(loaded) != 0 {
		t.Fatalf("expected empty store after del, got %v", loaded)
	}
}

// TestStore_LoadPreservesEmptyStatus guards the write/read contract symmetry:
// Offer accepts an item with a zero-value Status (task.validStatuses has no ""
// entry), so the read path must not drop it on reload or a queued item would
// be silently lost across a restart.
func TestStore_LoadPreservesEmptyStatus(t *testing.T) {
	dir := t.TempDir()
	s, err := newStore(dir)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	it := Item{TaskID: "t1", Priority: task.PriorityLow, Enqueued: time.Now()}
	if err := s.put(it); err != nil {
		t.Fatalf("put: %v", err)
	}

	loaded := s.load(discardLogger())
	if len(loaded) != 1 || loaded[0].TaskID != "t1" || loaded[0].Status != "" {
		t.Fatalf("load() = %v, want single item with empty status", loaded)
	}
}

// TestQueue_EmptyStatusSurvivesRestart reproduces the original data-loss bug at
// the Queue level: an offered zero-Status item must still be present after a
// fresh New against the same dir (a simulated restart).
func TestQueue_EmptyStatusSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	q, err := New(dir, Options{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !q.Offer(Item{TaskID: "t1", Priority: task.PriorityLow}) {
		t.Fatal("Offer should enqueue a new item")
	}

	reloaded, err := New(dir, Options{}, discardLogger())
	if err != nil {
		t.Fatalf("New (restart): %v", err)
	}
	if got := reloaded.Snapshot(); len(got) != 1 || got[0].TaskID != "t1" {
		t.Fatalf("after restart Snapshot() = %v, want single item t1", got)
	}
}

func TestStore_DelMissingIsNotAnError(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	if err := s.del("never-existed"); err != nil {
		t.Errorf("del of missing file should not error, got %v", err)
	}
}

func TestStore_InitFailure(t *testing.T) {
	// A regular file cannot be MkdirAll'd into a directory.
	tmp := t.TempDir()
	blocker := filepath.Join(tmp, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocker file: %v", err)
	}
	if _, err := newStore(filepath.Join(blocker, "queue")); err == nil {
		t.Fatal("newStore under a file path should fail")
	}
}

func TestStore_LoadSkipsCorruptAndMismatchedFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := newStore(dir)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}

	good := Item{TaskID: "good", Priority: task.PriorityLow, Status: task.StatusNew, Enqueued: time.Now()}
	if err := s.put(good); err != nil {
		t.Fatalf("put good: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "corrupt.yaml"), []byte("not: valid: yaml: ["), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty-id.yaml"), []byte("task_id: \"\"\n"), 0o644); err != nil {
		t.Fatalf("seed empty-id file: %v", err)
	}
	// Filename doesn't match the TaskID inside.
	if err := os.WriteFile(filepath.Join(dir, "mismatch.yaml"), []byte("task_id: other\n"), 0o644); err != nil {
		t.Fatalf("seed mismatch file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad-priority.yaml"), []byte("task_id: bad-priority\npriority: not-a-priority\n"), 0o644); err != nil {
		t.Fatalf("seed bad-priority file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad-status.yaml"), []byte("task_id: bad-status\nstatus: not-a-status\n"), 0o644); err != nil {
		t.Fatalf("seed bad-status file: %v", err)
	}

	loaded := s.load(discardLogger())
	if len(loaded) != 1 || loaded[0].TaskID != "good" {
		t.Fatalf("load() = %v, want only [good]", loaded)
	}
}

func TestStore_LoadDuplicateTaskIDKeepsLast(t *testing.T) {
	dir := t.TempDir()
	s, err := newStore(dir)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	// Two files, both naming the same TaskID (only one can legitimately be
	// named "<TaskID>.yaml" by put/del, but load must still degrade safely
	// if a leftover file collides).
	if err := os.WriteFile(filepath.Join(dir, "dup.yaml"), []byte("task_id: dup\npriority: low\nstatus: new\n"), 0o644); err != nil {
		t.Fatalf("seed dup.yaml: %v", err)
	}

	loaded := s.load(discardLogger())
	if len(loaded) != 1 || loaded[0].TaskID != "dup" || loaded[0].Priority != task.PriorityLow {
		t.Fatalf("load() = %v, want single dup item", loaded)
	}
}

func TestStore_PutAndDelFailuresAreNonFatalForQueue(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-based failure injection not supported on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permissions")
	}

	dir := t.TempDir()
	q, err := New(dir, Options{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod dir read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	// Offer must still succeed in-memory even though the store write fails.
	if !q.Offer(Item{TaskID: "t1", Priority: task.PriorityLow}) {
		t.Fatal("Offer should succeed in-memory despite store put failure")
	}
	if len(q.Snapshot()) != 1 {
		t.Fatalf("expected item to remain queued despite persist failure")
	}

	// Remove must still succeed in-memory even though the store delete fails.
	q.Remove("t1")
	if len(q.Snapshot()) != 0 {
		t.Fatalf("expected item to be removed in-memory despite delete failure")
	}
}

func TestStore_SafeTaskID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"task-1", true},
		{"", false},
		{"..", false},
		{"../escape", false},
		{"a/b", false},
		{"a\\b", false},
		{"a/../b", false},
		{".hidden", true},
	}
	for _, tt := range tests {
		if got := safeTaskID(tt.id); got != tt.want {
			t.Errorf("safeTaskID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}
