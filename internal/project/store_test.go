package project

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestNewStore(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Get("nonexistent/repo")
	if err == nil {
		t.Fatal("expected error for nonexistent project")
	}
	if !errors.Is(err, ErrProjectNotRegistered) {
		t.Fatalf("missing project: got %v, want errors.Is(ErrProjectNotRegistered)", err)
	}
}

func TestStoreListMultiple(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Delete("nonexistent/repo"); err == nil {
		t.Fatal("expected error for nonexistent project")
	}
}

func TestStoreFilePath(t *testing.T) {
	t.Parallel()
	store := &Store{dir: "/tmp/projects"}
	path := store.filePath("owner/repo")
	if filepath.Base(path) != "owner--repo.yaml" {
		t.Errorf("filePath = %q, want owner--repo.yaml basename", path)
	}
}

func TestStoreCreateInvalidURL(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Create("https://gitlab.com/owner/repo", ProjectTypePet)
	if err == nil {
		t.Fatal("expected error for non-github URL")
	}
}

func TestStoreCreateDuplicate(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Write a project manually to simulate existing
	p := Project{ID: "owner/repo", Owner: "owner", Repo: "repo"}
	if err := store.writeFile(p); err != nil {
		t.Fatal(err)
	}

	_, err = store.Create("https://github.com/owner/repo", ProjectTypePet)
	if err == nil {
		t.Fatal("expected error for duplicate project")
	}
}

func TestStoreDefaultTypeOnRead(t *testing.T) {
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	p := Project{ID: "owner/repo", Owner: "owner", Repo: "repo"}
	if err := store.writeFile(p); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get("owner/repo")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Type != ProjectTypePet {
		t.Errorf("Type = %q, want %q", got.Type, ProjectTypePet)
	}
}

func TestStoreUpdate(t *testing.T) {
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	p := Project{ID: "owner/repo", Owner: "owner", Repo: "repo", Type: ProjectTypePet}
	if err := store.writeFile(p); err != nil {
		t.Fatal(err)
	}

	got, err := store.Update("owner/repo", ProjectTypeWork)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Type != ProjectTypeWork {
		t.Errorf("Type = %q, want %q", got.Type, ProjectTypeWork)
	}

	persisted, err := store.Get("owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Type != ProjectTypeWork {
		t.Errorf("persisted Type = %q, want %q", persisted.Type, ProjectTypeWork)
	}
}

func TestStoreUpdateInvalidType(t *testing.T) {
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	p := Project{ID: "owner/repo", Owner: "owner", Repo: "repo"}
	if err := store.writeFile(p); err != nil {
		t.Fatal(err)
	}

	_, err = store.Update("owner/repo", "enterprise")
	if err == nil {
		t.Fatal("expected error for invalid project type")
	}
}

func TestStoreSetSetupCommands(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	p := Project{ID: "owner/repo", Owner: "owner", Repo: "repo", Type: ProjectTypePet}
	if err := store.writeFile(p); err != nil {
		t.Fatal(err)
	}

	cmds := []string{"npm install", "mkdir -p dist"}
	got, err := store.SetSetupCommands("owner/repo", cmds)
	if err != nil {
		t.Fatalf("SetSetupCommands: %v", err)
	}
	if len(got.SetupCommands) != 2 || got.SetupCommands[0] != "npm install" {
		t.Errorf("SetupCommands = %v, want %v", got.SetupCommands, cmds)
	}

	persisted, err := store.Get("owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.SetupCommands) != 2 {
		t.Errorf("persisted SetupCommands = %v, want %v", persisted.SetupCommands, cmds)
	}
}

// TestStoreConcurrentUpdatesDontDropWrites exercises the exact race the
// mutex closes: MarkReady simulating an async clone completion racing
// SetSetupCommands from a UI edit, both doing Get→mutate→writeFile against
// the same project. Without per-id locking one goroutine's write can
// silently overwrite the other's read-stale copy, dropping a field.
func TestStoreConcurrentUpdatesDontDropWrites(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	p := Project{ID: "owner/repo", Owner: "owner", Repo: "repo", Type: ProjectTypePet, Status: ProjectStatusCloning}
	if err := store.writeFile(p); err != nil {
		t.Fatal(err)
	}

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n * 2)
	for range n {
		go func() {
			defer wg.Done()
			if err := store.MarkReady("owner/repo"); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := store.SetSetupCommands("owner/repo", []string{"npm install"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	got, err := store.Get("owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ProjectStatusReady {
		t.Errorf("Status = %q, want %q (a racing SetSetupCommands write dropped MarkReady's update)", got.Status, ProjectStatusReady)
	}
	if len(got.SetupCommands) != 1 {
		t.Errorf("SetupCommands = %v, want 1 entry (a racing MarkReady write dropped SetSetupCommands's update)", got.SetupCommands)
	}
}

func TestStoreDeleteCleansClone(t *testing.T) {
	t.Parallel()
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
