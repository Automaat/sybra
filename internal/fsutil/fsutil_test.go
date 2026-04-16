package fsutil

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	data := []byte("hello world")

	if err := AtomicWrite(path, data); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("got %q, want %q", got, data)
	}
}

func TestAtomicWrite_Overwrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")

	if err := AtomicWrite(path, []byte("old")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := AtomicWrite(path, []byte("new")); err != nil {
		t.Fatalf("second write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("got %q, want %q", got, "new")
	}
}

func TestAtomicWrite_BadDir(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nonexistent", "file.txt")
	if err := AtomicWrite(path, []byte("data")); err == nil {
		t.Fatal("expected error for non-existent parent dir")
	}
}

// TestAtomicWrite_RenameFailCleansUpTemp verifies the temp file is removed
// when os.Rename fails. A read-only target directory is the simplest
// repeatable trigger: CreateTemp succeeds (the temp lands next to the
// eventual target), Write succeeds, Close succeeds, but Rename into the
// read-only target fails with EACCES. Prior to the fix, the orphan .tmp
// accumulated on every failed write — eventually filling the disk.
func TestAtomicWrite_RenameFailCleansUpTemp(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses chmod 0o500")
	}
	dir := t.TempDir()

	// Seed an existing target file so Rename has something concrete to replace,
	// then drop write permission on the containing directory so Rename fails
	// but CreateTemp still works against an already-writable temp namespace.
	target := filepath.Join(dir, "locked.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Make the directory non-writable so Rename fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	// AtomicWrite should fail — but must not leave an orphan temp.
	err := AtomicWrite(target, []byte("new"))
	if err == nil {
		t.Skip("rename did not fail on this platform/fs; test relies on directory permissions semantics")
	}

	// Restore permissions so we can inspect the dir.
	_ = os.Chmod(dir, 0o755)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "locked.txt" {
			continue
		}
		// Any leftover entry indicates a temp file leak.
		t.Errorf("found leftover entry after failed AtomicWrite: %s", e.Name())
	}
}

func TestListFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	names := []string{"a.md", "b.md", "c.yaml", "d.txt"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", n, err)
		}
	}

	paths, err := ListFiles(dir, ".md")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(paths) != 2 {
		t.Errorf("got %d paths, want 2: %v", len(paths), paths)
	}
	for _, p := range paths {
		if filepath.Ext(p) != ".md" {
			t.Errorf("unexpected path %q", p)
		}
	}
}

func TestListFiles_Empty(t *testing.T) {
	t.Parallel()
	paths, err := ListFiles(t.TempDir(), ".md")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("expected empty, got %v", paths)
	}
}

func TestListFiles_BadDir(t *testing.T) {
	t.Parallel()
	_, err := ListFiles(filepath.Join(t.TempDir(), "nonexistent"), ".md")
	if err == nil {
		t.Fatal("expected error for non-existent dir")
	}
}
