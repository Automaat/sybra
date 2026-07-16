package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// init neutralizes the real removal backoff/sleep for every test in this
// package — TestManager_RemoveContext_RetriesTransientBusyThenSucceeds and
// friends exercise real retry loops and must not burn wall-clock time.
func init() {
	sandboxRemoveBackoffs = []time.Duration{0, 0, 0}
	sandboxRemoveSleep = func(time.Duration) {}
}

func TestOwnerRecordRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if _, ok := readOwnerRecord(dir); ok {
		t.Fatal("readOwnerRecord on a dir with no record: ok = true, want false")
	}

	if err := writeOwnerRecord(dir); err != nil {
		t.Fatalf("writeOwnerRecord: %v", err)
	}

	rec, ok := readOwnerRecord(dir)
	if !ok {
		t.Fatal("readOwnerRecord after write: ok = false, want true")
	}
	if rec.UID != os.Getuid() || rec.GID != os.Getgid() {
		t.Errorf("readOwnerRecord = %+v, want uid=%d gid=%d", rec, os.Getuid(), os.Getgid())
	}
}

func TestReadOwnerRecordMalformed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ownerFileName), []byte("not json"), 0o600); err != nil {
		t.Fatalf("write malformed record: %v", err)
	}
	if _, ok := readOwnerRecord(dir); ok {
		t.Fatal("readOwnerRecord on malformed JSON: ok = true, want false")
	}
}

func TestMismatchedOwnership(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("data"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "g"), []byte("data"), 0o600); err != nil {
		t.Fatalf("write nested file: %v", err)
	}

	real := ownerRecord{UID: os.Getuid(), GID: os.Getgid()}
	if mismatchedOwnership(dir, real) {
		t.Error("mismatchedOwnership with the real owner: true, want false")
	}

	fake := ownerRecord{UID: real.UID + 999999, GID: real.GID + 999999}
	if !mismatchedOwnership(dir, fake) {
		t.Error("mismatchedOwnership with a fake owner: false, want true")
	}
}

func TestMismatchedOwnershipMissingPath(t *testing.T) {
	t.Parallel()
	// filepath.WalkDir on a nonexistent root reports an error on the root
	// entry itself; mismatchedOwnership must not misreport that as a match.
	if mismatchedOwnership(filepath.Join(t.TempDir(), "does-not-exist"), ownerRecord{}) {
		t.Error("mismatchedOwnership on a missing path: true, want false")
	}
}

func TestIsTransientRemoveErr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ebusy", errors.New("remove /x: device or resource busy"), true},
		{"wrapped ebusy", fs.ErrClosed, false},
		{"permission denied", fs.ErrPermission, false},
		{"not exist", fs.ErrNotExist, false},
		{"generic", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isTransientRemoveErr(tt.err); got != tt.want {
				t.Errorf("isTransientRemoveErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestDirSize(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), make([]byte, 10), 0o600); err != nil {
		t.Fatalf("write a: %v", err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b"), make([]byte, 25), 0o600); err != nil {
		t.Fatalf("write b: %v", err)
	}

	size, err := dirSize(dir)
	if err != nil {
		t.Fatalf("dirSize: %v", err)
	}
	if size != 35 {
		t.Errorf("dirSize = %d, want 35", size)
	}
}

func TestQuarantinePersistence(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)

	if _, ok := m.loadQuarantine("task-x"); ok {
		t.Fatal("loadQuarantine before save: ok = true, want false")
	}

	entry := QuarantineEntry{
		TaskID:        "task-x",
		Path:          "/data/sandboxes/task-x",
		BytesRetained: 4096,
		Attempts:      1,
		LastError:     "boom",
		FirstFailedAt: time.Now().Add(-time.Hour).UTC().Truncate(time.Second),
		LastFailedAt:  time.Now().UTC().Truncate(time.Second),
	}
	if err := m.saveQuarantine(entry); err != nil {
		t.Fatalf("saveQuarantine: %v", err)
	}

	got, ok := m.loadQuarantine("task-x")
	if !ok {
		t.Fatal("loadQuarantine after save: ok = false, want true")
	}
	if got != entry {
		t.Errorf("loadQuarantine = %+v, want %+v", got, entry)
	}

	entries := m.QuarantinedEntries()
	if len(entries) != 1 || entries[0].TaskID != "task-x" {
		t.Errorf("QuarantinedEntries = %+v, want single task-x entry", entries)
	}

	m.clearQuarantine("task-x")
	if _, ok := m.loadQuarantine("task-x"); ok {
		t.Fatal("loadQuarantine after clear: ok = true, want false")
	}
	if entries := m.QuarantinedEntries(); len(entries) != 0 {
		t.Errorf("QuarantinedEntries after clear = %+v, want empty", entries)
	}
}
