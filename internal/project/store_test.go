package project

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/gitexec"
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

// newBareRepoUnder inits a bare git repo at dir/name and returns its path,
// for tests that need a clone Adopt will accept (under the store's clonesDir).
func newBareRepoUnder(t *testing.T, dir, name string) string {
	t.Helper()
	clonePath := filepath.Join(dir, name)
	if err := gitexec.Run(context.Background(), gitexec.Options{}, "init", "--bare", clonePath); err != nil {
		t.Fatalf("init bare repo: %v", err)
	}
	return clonePath
}

func TestStoreAdopt(t *testing.T) {
	t.Parallel()
	clonesDir := t.TempDir()
	store, err := NewStore(t.TempDir(), clonesDir)
	if err != nil {
		t.Fatal(err)
	}

	clonePath := newBareRepoUnder(t, clonesDir, "existing.git")
	// EvalSymlinks: macOS resolves TMPDIR through /var -> /private/var, and
	// Adopt now stores the resolved path, so the expectation must too.
	wantClonePath, err := filepath.EvalSymlinks(clonePath)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	p, err := store.Adopt("https://github.com/owner/repo", ProjectTypePet, clonePath)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if p.ID != "owner/repo" || p.Owner != "owner" || p.Repo != "repo" {
		t.Fatalf("unexpected identity: %+v", p)
	}
	if p.ClonePath != wantClonePath {
		t.Fatalf("ClonePath = %q, want %q", p.ClonePath, wantClonePath)
	}
	if p.Status != ProjectStatusReady {
		t.Fatalf("Status = %q, want ready", p.Status)
	}

	got, err := store.Get("owner/repo")
	if err != nil {
		t.Fatalf("Get after Adopt: %v", err)
	}
	if got.ClonePath != wantClonePath {
		t.Fatalf("persisted ClonePath = %q, want %q", got.ClonePath, wantClonePath)
	}

	// Spot-check two of CloneBare's setup steps landed too, so an adopted
	// clone is reachable by agent dispatch exactly as a Create'd one is.
	fetchspec, err := gitexec.Output(context.Background(), gitexec.Options{Dir: clonePath}, "config", "remote.origin.fetch")
	if err != nil {
		t.Fatalf("read remote.origin.fetch: %v", err)
	}
	if strings.TrimSpace(fetchspec) != "+refs/heads/*:refs/remotes/origin/*" {
		t.Fatalf("remote.origin.fetch = %q, want the standard refspec", fetchspec)
	}
	maintAuto, err := gitexec.Output(context.Background(), gitexec.Options{Dir: clonePath}, "config", "maintenance.auto")
	if err != nil {
		t.Fatalf("read maintenance.auto: %v", err)
	}
	if strings.TrimSpace(maintAuto) != "false" {
		t.Fatalf("maintenance.auto = %q, want false", maintAuto)
	}
}

func TestStoreAdoptInvalidURL(t *testing.T) {
	t.Parallel()
	clonesDir := t.TempDir()
	store, err := NewStore(t.TempDir(), clonesDir)
	if err != nil {
		t.Fatal(err)
	}

	clonePath := newBareRepoUnder(t, clonesDir, "existing.git")

	_, err = store.Adopt("https://gitlab.com/owner/repo", ProjectTypePet, clonePath)
	if err == nil {
		t.Fatal("expected error for non-github URL")
	}
}

func TestStoreAdoptNotAGitDir(t *testing.T) {
	t.Parallel()
	clonesDir := t.TempDir()
	store, err := NewStore(t.TempDir(), clonesDir)
	if err != nil {
		t.Fatal(err)
	}

	notAGitDir := filepath.Join(clonesDir, "not-a-repo")
	if err := os.MkdirAll(notAGitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = store.Adopt("https://github.com/owner/repo", ProjectTypePet, notAGitDir)
	if err == nil {
		t.Fatal("expected error for a clone path that is not a git repository")
	}
}

// TestStoreAdoptNonBareRejected proves Adopt refuses a normal working-tree
// checkout. Every downstream worktree operation treats ClonePath as the
// shared canonical repo; adopting someone's working copy would let Sybra
// mutate its refs as ordinary task lifecycle churn.
func TestStoreAdoptNonBareRejected(t *testing.T) {
	t.Parallel()
	clonesDir := t.TempDir()
	store, err := NewStore(t.TempDir(), clonesDir)
	if err != nil {
		t.Fatal(err)
	}

	nonBare := filepath.Join(clonesDir, "working-copy")
	if err := gitexec.Run(context.Background(), gitexec.Options{}, "init", nonBare); err != nil {
		t.Fatalf("init non-bare repo: %v", err)
	}

	_, err = store.Adopt("https://github.com/owner/repo", ProjectTypePet, nonBare)
	if err == nil {
		t.Fatal("expected error for a non-bare clone path")
	}
}

// TestStoreAdoptOutsideClonesDirRejected proves Adopt refuses a clone path
// outside clonesDir. Delete runs an unconditional os.RemoveAll(ClonePath);
// every other registration path only ever writes a clonesDir-rooted
// ClonePath, which is what makes that safe. Accepting an arbitrary path here
// would let a caller adopt a project pointing anywhere on the serving host
// and then delete it.
func TestStoreAdoptOutsideClonesDirRejected(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	outside := newBareRepoUnder(t, t.TempDir(), "existing.git")

	_, err = store.Adopt("https://github.com/owner/repo", ProjectTypePet, outside)
	if err == nil {
		t.Fatal("expected error for a clone path outside clonesDir")
	}
}

// TestStoreAdoptSymlinkEscapeRejected proves Adopt refuses a clone path that
// only lexically sits under clonesDir: a symlink inside clonesDir pointing
// at a bare repo outside it must not pass the containment check, because
// every git command run with Dir: clonePath — and Delete's os.RemoveAll —
// follows the link to wherever it actually points.
func TestStoreAdoptSymlinkEscapeRejected(t *testing.T) {
	t.Parallel()
	clonesDir := t.TempDir()
	store, err := NewStore(t.TempDir(), clonesDir)
	if err != nil {
		t.Fatal(err)
	}

	outside := newBareRepoUnder(t, t.TempDir(), "existing.git")
	linkPath := filepath.Join(clonesDir, "escape.git")
	if err := os.Symlink(outside, linkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err = store.Adopt("https://github.com/owner/repo", ProjectTypePet, linkPath)
	if err == nil {
		t.Fatal("expected error for a clone path that escapes clonesDir via a symlink")
	}
}

func TestStoreAdoptDuplicate(t *testing.T) {
	t.Parallel()
	clonesDir := t.TempDir()
	store, err := NewStore(t.TempDir(), clonesDir)
	if err != nil {
		t.Fatal(err)
	}

	clonePath := newBareRepoUnder(t, clonesDir, "existing.git")

	p := Project{ID: "owner/repo", Owner: "owner", Repo: "repo"}
	if err := store.writeFile(p); err != nil {
		t.Fatal(err)
	}

	_, err = store.Adopt("https://github.com/owner/repo", ProjectTypePet, clonePath)
	if err == nil {
		t.Fatal("expected error for duplicate project")
	}
}

// TestStoreAdoptClonePathAlreadyInUseRejected proves two project IDs can
// never alias one physical repo — cleanup for either would then run against
// the other's refs and worktrees with nothing aware of the aliasing.
func TestStoreAdoptClonePathAlreadyInUseRejected(t *testing.T) {
	t.Parallel()
	clonesDir := t.TempDir()
	store, err := NewStore(t.TempDir(), clonesDir)
	if err != nil {
		t.Fatal(err)
	}

	clonePath := newBareRepoUnder(t, clonesDir, "shared.git")

	if _, err := store.Adopt("https://github.com/owner/repo", ProjectTypePet, clonePath); err != nil {
		t.Fatalf("first Adopt: %v", err)
	}

	_, err = store.Adopt("https://github.com/other/repo", ProjectTypePet, clonePath)
	if err == nil {
		t.Fatal("expected error for a clone path already registered to another project")
	}
}

// TestStoreAdoptConcurrentDifferentIDsSameClonePathSerializes proves the
// clonePath-keyed lock, not just the id-keyed one, guards
// rejectClonePathInUse: two different owner/repo IDs racing to Adopt the
// same clonePath acquire disjoint id locks, so without a second lock keyed
// on clonePath both could pass the List-based check before either writes.
func TestStoreAdoptConcurrentDifferentIDsSameClonePathSerializes(t *testing.T) {
	t.Parallel()
	clonesDir := t.TempDir()
	store, err := NewStore(t.TempDir(), clonesDir)
	if err != nil {
		t.Fatal(err)
	}

	clonePath := newBareRepoUnder(t, clonesDir, "shared.git")

	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	successes := make([]bool, n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			url := fmt.Sprintf("https://github.com/owner%d/repo", i)
			_, err := store.Adopt(url, ProjectTypePet, clonePath)
			successes[i] = err == nil
		}(i)
	}
	wg.Wait()

	count := 0
	for _, ok := range successes {
		if ok {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%d of %d concurrent Adopt calls for the same clonePath succeeded, want exactly 1", count, n)
	}
}

// TestStoreCreateDoesNotHoldMetadataLockDuringClone proves synchronous Create
// uses the same register-clone-complete lifecycle as the asynchronous path.
// A blocked clone must not prevent a user from editing the pending project.
func TestStoreCreateDoesNotHoldMetadataLockDuringClone(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	cloneStarted := make(chan struct{})
	releaseClone := make(chan struct{})
	store.cloneBare = func(ctx context.Context, _ string, dest string) error {
		close(cloneStarted)
		<-releaseClone
		// A real bare repo, not a bare directory: Create configures commit
		// signing on the clone before publishing it.
		return gitexec.Run(ctx, gitexec.Options{}, "init", "--bare", dest)
	}

	created := make(chan error, 1)
	go func() {
		_, err := store.Create("https://github.com/owner/repo", ProjectTypePet)
		created <- err
	}()
	<-cloneStarted

	updated := make(chan error, 1)
	go func() {
		_, err := store.SetSetupCommands("owner/repo", []string{"npm ci"})
		updated <- err
	}()
	select {
	case err := <-updated:
		if err != nil {
			t.Fatalf("update while cloning: %v", err)
		}
	case <-time.After(time.Second):
		close(releaseClone)
		t.Fatal("metadata update was blocked by the clone")
	}

	close(releaseClone)
	if err := <-created; err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.Get("owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ProjectStatusReady {
		t.Errorf("Status = %q, want %q", got.Status, ProjectStatusReady)
	}
	if len(got.SetupCommands) != 1 || got.SetupCommands[0] != "npm ci" {
		t.Errorf("SetupCommands = %v, want [npm ci]", got.SetupCommands)
	}
}

func TestStoreCreateDeleteDuringCloneLeavesNoClone(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cloneStarted := make(chan struct{})
	releaseClone := make(chan struct{})
	store.cloneBare = func(ctx context.Context, _ string, dest string) error {
		close(cloneStarted)
		<-releaseClone
		// A real bare repo, not a bare directory: Create configures commit
		// signing on the clone before publishing it.
		return gitexec.Run(ctx, gitexec.Options{}, "init", "--bare", dest)
	}

	created := make(chan error, 1)
	go func() {
		_, err := store.Create("https://github.com/owner/repo", ProjectTypePet)
		created <- err
	}()
	<-cloneStarted
	if err := store.Delete("owner/repo"); err != nil {
		t.Fatalf("delete while cloning: %v", err)
	}
	close(releaseClone)
	if err := <-created; err == nil {
		t.Fatal("Create succeeded after its project was deleted")
	}
	if _, err := store.Get("owner/repo"); !errors.Is(err, ErrProjectNotRegistered) {
		t.Fatalf("deleted metadata = %v, want ErrProjectNotRegistered", err)
	}
	clonePath := filepath.Join(store.clonesDir, "owner", "repo.git")
	if _, err := os.Stat(clonePath); !os.IsNotExist(err) {
		t.Fatalf("clone path remains after delete: %v", err)
	}
}

func TestStoreMarkReadyForDoesNotCompleteRecreatedProject(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started, err := store.CreateMeta("https://github.com/owner/repo", ProjectTypePet)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(started.ID); err != nil {
		t.Fatal(err)
	}
	recreated, err := store.CreateMeta("https://github.com/owner/repo", ProjectTypePet)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReadyFor(started); err == nil {
		t.Fatal("stale completion marked the re-created project ready")
	}
	got, err := store.Get(recreated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ProjectStatusCloning || got.CloneGeneration != recreated.CloneGeneration {
		t.Errorf("re-created project changed after stale completion: %+v", got)
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

// A project registered before DisableAutoMaintenance existed in CloneBare
// never got maintenance.auto=false, leaving its clone exposed to the same
// cross-worktree repack race a fresh clone is now protected against.
// MigrateDisableAutoMaintenance must retrofit it without a re-clone.
func TestStoreMigrateDisableAutoMaintenance_RetrofitsExistingClone(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "pre-fix-bare.git")
	if out, err := exec.Command("git", "clone", "-q", "--bare", src, bare).CombinedOutput(); err != nil {
		t.Fatalf("git clone --bare: %v: %s", err, out)
	}
	// Simulate the pre-fix state: no CloneBare-set config at all, so
	// maintenance.auto falls back to git's own default (true). --unset exits
	// non-zero when the key was never set, which is the expected outcome
	// here — plain `git clone --bare` sets no such key.
	_ = exec.Command("git", "-C", bare, "config", "--unset", "maintenance.auto").Run()

	p := Project{ID: "org/pre-fix", Owner: "org", Repo: "pre-fix", ClonePath: bare}
	if err := store.writeFile(p); err != nil {
		t.Fatal(err)
	}

	if err := store.MigrateDisableAutoMaintenance(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	raw, err := outputBare(context.Background(), bare, "config", "maintenance.auto")
	if err != nil {
		t.Fatalf("read maintenance.auto: %v", err)
	}
	if got := strings.TrimSpace(raw); got != "false" {
		t.Errorf("maintenance.auto = %q, want %q", got, "false")
	}

	gcAuto, err := outputBare(context.Background(), bare, "config", "gc.auto")
	if err != nil {
		t.Fatalf("read gc.auto: %v", err)
	}
	if got := strings.TrimSpace(gcAuto); got != "0" {
		t.Errorf("gc.auto = %q, want %q", got, "0")
	}
	name, err := outputBare(context.Background(), bare, "config", "user.name")
	if err != nil {
		t.Fatalf("read user.name: %v", err)
	}
	email, err := outputBare(context.Background(), bare, "config", "user.email")
	if err != nil {
		t.Fatalf("read user.email: %v", err)
	}
	if strings.TrimSpace(name) != "Sybra Test" || strings.TrimSpace(email) != "test@test.com" {
		t.Errorf("migrated identity = %q <%q>", strings.TrimSpace(name), strings.TrimSpace(email))
	}
}

// disableAutoMaintenanceLocked's re-check must only swallow the two expected
// races (project deleted, clone directory removed) — a genuine read/parse
// failure has to surface in the joined error, not be silently dropped, or a
// real filesystem problem would be indistinguishable from a healthy startup.
func TestStoreDisableAutoMaintenanceLocked_PropagatesRealReadError(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	p := Project{ID: "org/broken", Owner: "org", Repo: "broken", ClonePath: t.TempDir()}
	if err := store.writeFile(p); err != nil {
		t.Fatal(err)
	}
	// Corrupt the record after it's written, simulating a genuine read/parse
	// failure distinct from "file does not exist" (ErrProjectNotRegistered).
	if err := os.WriteFile(store.filePath(p.ID), []byte("not: [valid yaml"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = store.disableAutoMaintenanceLocked(context.Background(), p.ID, p.ClonePath)
	if err == nil {
		t.Fatal("expected the parse error to propagate, got nil")
	}
	if errors.Is(err, ErrProjectNotRegistered) {
		t.Fatalf("a real parse error must not be reported as ErrProjectNotRegistered: %v", err)
	}
}

// A project mid-clone (ClonePath set but the directory not yet created) or
// mid-delete (directory already removed) must not fail the whole migration
// pass for every other registered project.
func TestStoreMigrateDisableAutoMaintenance_SkipsMissingClone(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	missing := Project{ID: "org/missing", Owner: "org", Repo: "missing", ClonePath: filepath.Join(t.TempDir(), "never-created.git")}
	if err := store.writeFile(missing); err != nil {
		t.Fatal(err)
	}
	empty := Project{ID: "org/empty", Owner: "org", Repo: "empty"}
	if err := store.writeFile(empty); err != nil {
		t.Fatal(err)
	}

	if err := store.MigrateDisableAutoMaintenance(context.Background()); err != nil {
		t.Fatalf("migrate should skip missing/empty clones rather than error: %v", err)
	}
}

// CreateProject's async clone path writes directly into the final ClonePath
// (no temp-path-plus-atomic-rename like Store.Create uses), so a project
// genuinely mid-clone has a directory that already exists and os.Stat
// succeeds against — Status=cloning, not a missing directory, is the real
// signal MigrateDisableAutoMaintenance must key off to avoid racing
// CloneBare's own .git/config writes.
func TestStoreMigrateDisableAutoMaintenance_SkipsInProgressClone(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "mid-clone-bare.git")
	if out, err := exec.Command("git", "clone", "-q", "--bare", src, bare).CombinedOutput(); err != nil {
		t.Fatalf("git clone --bare: %v: %s", err, out)
	}
	_ = exec.Command("git", "-C", bare, "config", "--unset", "maintenance.auto").Run()

	p := Project{ID: "org/mid-clone", Owner: "org", Repo: "mid-clone", ClonePath: bare, Status: ProjectStatusCloning}
	if err := store.writeFile(p); err != nil {
		t.Fatal(err)
	}

	if err := store.MigrateDisableAutoMaintenance(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	raw, err := outputBare(context.Background(), bare, "config", "maintenance.auto")
	if err == nil && strings.TrimSpace(raw) == "false" {
		t.Fatal("migrate touched a clone still marked Status=cloning; must wait for it to reach ready")
	}
}
