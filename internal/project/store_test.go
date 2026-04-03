package project

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "projects")
	clonesDir := filepath.Join(t.TempDir(), "clones")
	store, err := NewStore(dir, clonesDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store == nil {
		t.Fatal("store is nil")
	}

	for _, d := range []string{dir, clonesDir} {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatalf("dir not created: %v", err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", d)
		}
	}
}

func TestStoreListEmpty(t *testing.T) {
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	projects, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(projects) != 0 {
		t.Errorf("expected empty list, got %d", len(projects))
	}
}

func TestStoreWriteAndGet(t *testing.T) {
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	p := Project{
		ID:    "owner/repo",
		Name:  "repo",
		Owner: "owner",
		Repo:  "repo",
		URL:   "https://github.com/owner/repo",
	}
	if err := store.writeFile(p); err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	got, err := store.Get("owner/repo")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != "owner/repo" {
		t.Errorf("ID = %q, want %q", got.ID, "owner/repo")
	}
	if got.Owner != "owner" {
		t.Errorf("Owner = %q, want %q", got.Owner, "owner")
	}
	if got.Repo != "repo" {
		t.Errorf("Repo = %q, want %q", got.Repo, "repo")
	}
	if got.URL != "https://github.com/owner/repo" {
		t.Errorf("URL = %q", got.URL)
	}
}

func TestStoreGetNotFound(t *testing.T) {
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Get("nonexistent/repo")
	if err == nil {
		t.Fatal("expected error for nonexistent project")
	}
}

func TestStoreListMultiple(t *testing.T) {
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"org/repo-a", "org/repo-b"} {
		p := Project{ID: id, Owner: "org", Repo: id[4:]}
		if err := store.writeFile(p); err != nil {
			t.Fatal(err)
		}
	}

	projects, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 2 {
		t.Errorf("got %d projects, want 2", len(projects))
	}
}

func TestStoreListIgnoresNonYAML(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	p := Project{ID: "owner/repo", Owner: "owner", Repo: "repo"}
	if err := store.writeFile(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a project"), 0o644); err != nil {
		t.Fatal(err)
	}

	projects, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 {
		t.Errorf("got %d projects, want 1", len(projects))
	}
}

func TestStoreDeleteNotFound(t *testing.T) {
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Delete("nonexistent/repo"); err == nil {
		t.Fatal("expected error for nonexistent project")
	}
}

func TestStoreFilePath(t *testing.T) {
	store := &Store{dir: "/tmp/projects"}
	path := store.filePath("owner/repo")
	if filepath.Base(path) != "owner--repo.yaml" {
		t.Errorf("filePath = %q, want owner--repo.yaml basename", path)
	}
}

func TestStoreCreateInvalidURL(t *testing.T) {
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Create("https://gitlab.com/owner/repo")
	if err == nil {
		t.Fatal("expected error for non-github URL")
	}
}

func TestStoreCreateDuplicate(t *testing.T) {
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Write a project manually to simulate existing
	p := Project{ID: "owner/repo", Owner: "owner", Repo: "repo"}
	if err := store.writeFile(p); err != nil {
		t.Fatal(err)
	}

	_, err = store.Create("https://github.com/owner/repo")
	if err == nil {
		t.Fatal("expected error for duplicate project")
	}
}

func TestStoreDeleteCleansClone(t *testing.T) {
	dir := t.TempDir()
	clonesDir := t.TempDir()
	store, err := NewStore(dir, clonesDir)
	if err != nil {
		t.Fatal(err)
	}

	clonePath := filepath.Join(clonesDir, "test-clone")
	if err := os.MkdirAll(clonePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clonePath, "HEAD"), []byte("ref: refs/heads/main"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := Project{ID: "org/tool", Owner: "org", Repo: "tool", ClonePath: clonePath}
	if err := store.writeFile(p); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete("org/tool"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := os.Stat(clonePath); !os.IsNotExist(err) {
		t.Error("clone dir should be removed")
	}
	if _, err := os.Stat(store.filePath("org/tool")); !os.IsNotExist(err) {
		t.Error("YAML file should be removed")
	}
}
