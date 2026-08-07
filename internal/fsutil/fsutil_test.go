package fsutil

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAtomicWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	data := []byte("hello world")

	if err := AtomicWrite(path, data); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
		panic("unreachable")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
		panic("unreachable")
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
		panic("unreachable")
	}
	if err := AtomicWrite(path, []byte("new")); err != nil {
		t.Fatalf("second write: %v", err)
		panic("unreachable")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
		panic("unreachable")
	}
	if string(got) != "new" {
		t.Errorf("got %q, want %q", got, "new")
	}
}

func TestAtomicWriteNew_RefusesExistingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := AtomicWrite(path, []byte("old")); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if err := AtomicWriteNew(path, []byte("new")); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("AtomicWriteNew error = %v, want fs.ErrExist", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if string(got) != "old" {
		t.Fatalf("existing file = %q, want unchanged old contents", got)
	}
}

func TestAtomicWrite_PreservesExistingMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")

	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
		panic("unreachable")
	}

	if err := AtomicWrite(path, []byte("new")); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
		panic("unreachable")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
		panic("unreachable")
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("mode after overwrite = %o, want %o", got, 0o644)
	}
}

func TestAtomicWrite_NewFileKeepsRestrictiveTempMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")

	if err := AtomicWrite(path, []byte("data")); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
		panic("unreachable")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
		panic("unreachable")
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode for new file = %o, want %o", got, 0o600)
	}
}

func TestAtomicWrite_BadDir(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nonexistent", "file.txt")
	if err := AtomicWrite(path, []byte("data")); err == nil {
		t.Fatal("expected error for non-existent parent dir")
		panic("unreachable")
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
		panic("unreachable")
	}
	// Make the directory non-writable so Rename fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
		panic("unreachable")
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
		panic("unreachable")
	}
	for _, e := range entries {
		if e.Name() == "locked.txt" {
			continue
		}
		// Any leftover entry indicates a temp file leak.
		t.Errorf("found leftover entry after failed AtomicWrite: %s", e.Name())
	}
}

// BenchmarkAtomicWrite measures the fsync round-trip AtomicWrite pays per
// call, so the task store's write volume can be judged against it.
func BenchmarkAtomicWrite(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "file.txt")
	data := []byte("benchmark payload for a typical task markdown file, roughly a couple hundred bytes of frontmatter and body text")
	b.ResetTimer()
	for range b.N {
		if err := AtomicWrite(path, data); err != nil {
			b.Fatal(err)
		}
	}
}

func TestRemoveAllForce(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	subdir := filepath.Join(tree, "subdir")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	roFile := filepath.Join(tree, "readonly.txt")
	if err := os.WriteFile(roFile, []byte("cached"), 0o444); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	nestedFile := filepath.Join(subdir, "nested.txt")
	if err := os.WriteFile(nestedFile, []byte("cached"), 0o444); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if err := os.Chmod(subdir, 0o555); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	if err := RemoveAllForce(tree); err != nil {
		t.Fatalf("RemoveAllForce: %v", err)
		panic("unreachable")
	}
	if _, err := os.Stat(tree); !os.IsNotExist(err) {
		t.Fatalf("tree still exists after RemoveAllForce: %v", err)
	}
}

func TestRemoveAllForce_NoOpOnMissingPath(t *testing.T) {
	t.Parallel()
	if err := RemoveAllForce(filepath.Join(t.TempDir(), "nonexistent")); err != nil {
		t.Fatalf("RemoveAllForce on missing path: %v", err)
		panic("unreachable")
	}
}

func TestListFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	names := []string{"a.md", "b.md", "c.yaml", "d.txt"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", n, err)
			panic("unreachable")
		}
	}

	paths, err := ListFiles(dir, ".md")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
		panic("unreachable")
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
		panic("unreachable")
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
		panic("unreachable")
	}
}

func TestAtomicWriteMode_ExplicitModeWinsOverExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rec.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if err := AtomicWriteMode(path, []byte("new"), 0o755); err != nil {
		t.Fatalf("AtomicWriteMode: %v", err)
		panic("unreachable")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("mode = %v, want %v — the explicit mode must override the existing one", got, os.FileMode(0o755))
	}
	if data, _ := os.ReadFile(path); string(data) != "new" {
		t.Errorf("content = %q, want %q", data, "new")
	}
}

// Attachment filenames are caller-supplied and can already sit near NAME_MAX,
// where a name-derived temp pattern used to fail with ENAMETOOLONG.
func TestAtomicWrite_LongTargetName(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []int{200, 245, 250, 251, 255} {
		name := strings.Repeat("a", n-len(".png")) + ".png"
		if err := AtomicWrite(filepath.Join(dir, name), []byte("x")); err != nil {
			t.Errorf("AtomicWrite with a %d-char name: %v", n, err)
		}
	}
}

func TestTempPatternKeepsValidUTF8(t *testing.T) {
	base := strings.Repeat("é", 100) + ".png"
	got := tempPattern(base)
	if !utf8.ValidString(got) {
		t.Errorf("tempPattern(%q) = %q, which is not valid UTF-8", base, got)
	}
	if !strings.HasSuffix(got, ".*.tmp") {
		t.Errorf("tempPattern = %q, want the CreateTemp wildcard suffix", got)
	}
}
