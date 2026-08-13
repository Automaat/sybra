package remotehandback_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agentworkspace"
	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/gitexec"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/remotehandback"
)

func TestImportGitReproducesCommittedDirtyAndUntrackedOutcome(t *testing.T) {
	source, base := repo(t)
	spec := spec(base)
	layout, err := agentworkspace.Prepare(t.Context(), filepath.Join(t.TempDir(), "runs"), source, spec)
	if err != nil {
		t.Fatal(err)
	}
	configure(t, layout.Worktree)
	if err := os.WriteFile(filepath.Join(layout.Worktree, "committed.txt"), []byte("commit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, layout.Worktree, "add", "committed.txt")
	git(t, layout.Worktree, "commit", "-m", "remote commit")
	if err := os.WriteFile(filepath.Join(layout.Worktree, "tracked.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.Worktree, "staged.txt"), []byte("staged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, layout.Worktree, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(layout.Worktree, "untracked.txt"), []byte("loose\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest, content, err := agentworkspace.Collect(t.Context(), layout, spec, "test")
	if err != nil {
		t.Fatal(err)
	}

	leader := filepath.Join(t.TempDir(), "leader")
	git(t, "", "clone", "--no-local", source, leader)
	git(t, leader, "checkout", "--detach", base)
	locked := false
	guard := func(ctx context.Context) (executioncontract.GenerationFence, string, error) {
		if !locked {
			return executioncontract.GenerationFence{}, "", errors.New("guard called outside canonical lock")
		}
		head, headErr := gitexec.Output(ctx, gitexec.Options{Dir: leader}, "rev-parse", "HEAD")
		return spec.Fence, head, headErr
	}
	lock := func(_ context.Context, _ string, fn func() error) error {
		locked = true
		defer func() { locked = false }()
		return fn()
	}
	if _, err := remotehandback.ImportGit(t.Context(), leader, spec, manifest, content, guard, lock); err != nil {
		t.Fatal(err)
	}
	if _, err := remotehandback.ImportGit(t.Context(), leader, spec, manifest, content, guard, lock); err != nil {
		t.Fatalf("recover exact already-published handback: %v", err)
	}
	head, _ := gitexec.Output(t.Context(), gitexec.Options{Dir: leader}, "rev-parse", "HEAD")
	if head != manifest.Workspace.FinalSHA {
		t.Fatalf("leader HEAD = %s, want %s", head, manifest.Workspace.FinalSHA)
	}
	for path, want := range map[string]string{"committed.txt": "commit\n", "tracked.txt": "dirty\n", "staged.txt": "staged\n", "untracked.txt": "loose\n"} {
		got, err := os.ReadFile(filepath.Join(leader, path))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v", path, got, err)
		}
	}
	status, _ := gitexec.Output(t.Context(), gitexec.Options{Dir: leader}, "status", "--porcelain")
	if !strings.Contains(status, "A  staged.txt") || !strings.Contains(status, " M tracked.txt") || !strings.Contains(status, "?? untracked.txt") {
		t.Fatalf("leader status = %q", status)
	}
	if err := os.WriteFile(filepath.Join(leader, "tracked.txt"), []byte("concurrent edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := remotehandback.ImportGit(t.Context(), leader, spec, manifest, content, guard, lock); !errors.Is(err, remotehandback.ErrStale) {
		t.Fatalf("tampered published state error = %v, want stale", err)
	}
	got, err := os.ReadFile(filepath.Join(leader, "tracked.txt"))
	if err != nil || string(got) != "concurrent edit\n" {
		t.Fatalf("stale recovery overwrote concurrent edit: %q, %v", got, err)
	}
}

func TestImportGitRejectsStaleAndCorruptWithoutMutation(t *testing.T) {
	source, base := repo(t)
	spec := spec(base)
	layout, err := agentworkspace.Prepare(t.Context(), filepath.Join(t.TempDir(), "runs"), source, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.Worktree, "tracked.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, content, err := agentworkspace.Collect(t.Context(), layout, spec, "test")
	if err != nil {
		t.Fatal(err)
	}
	leader := filepath.Join(t.TempDir(), "leader")
	git(t, "", "clone", "--no-local", source, leader)
	wrong := spec.Fence
	wrong.TaskGeneration++
	wrongGuard := func(context.Context) (executioncontract.GenerationFence, string, error) { return wrong, base, nil }
	lock := func(_ context.Context, _ string, fn func() error) error { return fn() }
	if _, err := remotehandback.ImportGit(t.Context(), leader, spec, manifest, content, wrongGuard, lock); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale error = %v", err)
	}
	guardCalls := 0
	changingGuard := func(context.Context) (executioncontract.GenerationFence, string, error) {
		guardCalls++
		if guardCalls == 1 {
			return spec.Fence, base, nil
		}
		return wrong, base, nil
	}
	if _, err := remotehandback.ImportGit(t.Context(), leader, spec, manifest, content, changingGuard, lock); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("generation changed during staging error = %v", err)
	}
	corrupt := append([]byte(nil), content...)
	corrupt[len(corrupt)/2] ^= 1
	guard := func(context.Context) (executioncontract.GenerationFence, string, error) { return spec.Fence, base, nil }
	if _, err := remotehandback.ImportGit(t.Context(), leader, spec, manifest, corrupt, guard, lock); err == nil {
		t.Fatal("corrupt package imported")
	}
	head, _ := gitexec.Output(t.Context(), gitexec.Options{Dir: leader}, "rev-parse", "HEAD")
	if head != base {
		t.Fatalf("rejected import mutated leader to %s", head)
	}
}

func repo(t *testing.T) (repoPath, baseSHA string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-b", "main")
	configure(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "tracked.txt")
	git(t, dir, "commit", "-m", "base")
	sha, err := gitexec.Output(t.Context(), gitexec.Options{Dir: dir}, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return dir, sha
}

func configure(t *testing.T, dir string) {
	t.Helper()
	git(t, dir, "config", "user.name", "Test")
	git(t, dir, "config", "user.email", "test@example.invalid")
	git(t, dir, "config", "commit.gpgsign", "false")
}
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	if err := gitexec.Run(t.Context(), gitexec.Options{Dir: dir}, args...); err != nil {
		t.Fatal(err)
	}
}
func spec(base string) executioncontract.RunSpec {
	return executioncontract.RunSpec{Version: executioncontract.CurrentVersion(), BuildVersion: "test", RunID: "run", EffectID: "effect", IdempotencyKey: "intent", Fence: executioncontract.GenerationFence{TaskID: "task", TaskGeneration: 1, WorkflowID: "ship", WorkflowGeneration: 1, StepID: "implement"}, Role: "implementation", Provider: executioncontract.ProviderIntent{Provider: providerid.Claude, Model: "sonnet"}, Prompt: executioncontract.Prompt{Text: "work"}, Deadline: time.Now().Add(time.Hour), Workspace: executioncontract.Workspace{RepositoryID: "repo", BaseSHA: base, BaseRef: "refs/heads/main", Roots: []executioncontract.LogicalRoot{executioncontract.RootWorktree, executioncontract.RootArtifact}}}
}
