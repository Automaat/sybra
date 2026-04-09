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
