package logging

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRotationTriggersAtMaxSize(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	w, err := NewRotatingWriter(path, 100, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	// Write 60 bytes
	if _, err := w.Write(make([]byte, 60)); err != nil {
		t.Fatal(err)
	}

	// Write 50 more — triggers rotation (60+50 > 100)
	if _, err := w.Write(make([]byte, 50)); err != nil {
		t.Fatal(err)
	}

	// Rotated file should exist
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Error("expected rotated file .1 to exist")
	}

	// Current file should have 50 bytes
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 50 {
		t.Errorf("current file size = %d, want 50", info.Size())
	}
}

func TestMaxFilesCleanup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	w, err := NewRotatingWriter(path, 50, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	// Force 3 rotations
	for range 3 {
		if _, err := w.Write(make([]byte, 60)); err != nil {
			t.Fatal(err)
		}
	}

	// .1 should exist
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Error("expected .1 to exist")
	}

	// .2 should NOT exist (maxFiles=2 means keep current + .1)
	if _, err := os.Stat(path + ".2"); err == nil {
		t.Error("expected .2 to be deleted")
	}
}

func TestConcurrentWrites(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	w, err := NewRotatingWriter(path, 1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			for range 100 {
				if _, err := w.Write([]byte("hello\n")); err != nil {
					return
				}
			}
		})
	}
	wg.Wait()
}

func TestRestartPicksUpExistingSize(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	// Write some data
	if err := os.WriteFile(path, make([]byte, 40), 0o644); err != nil {
		t.Fatal(err)
	}

	w, err := NewRotatingWriter(path, 100, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	// Write 70 more — should trigger rotation (40+70 > 100)
	if _, err := w.Write(make([]byte, 70)); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Error("expected rotation after restart with existing data")
	}
}

func TestAgentOutputFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	f, err := NewAgentOutputFile(dir, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	// Verify file is in agents/ subdir
	rel, err := filepath.Rel(dir, f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(rel) != "agents" {
		t.Errorf("file dir = %q, want agents/", filepath.Dir(rel))
	}
}
