package sybra

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/sybra/agentorch"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/worktree"
)

type reviewDispatchHarness struct {
	aa     *agentAdapter
	agents *agent.Manager
	task   task.Task
	src    string
	branch string
}

func gitOrFatal(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func setupReviewDispatchHarness(t *testing.T) reviewDispatchHarness {
	t.Helper()
	const prNumber = 42
	const prBranch = "feat/theirs"

	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	gitOrFatal(t, "init", "-b", "main", src)
	gitOrFatal(t, "-C", src, "config", "user.email", "test@test.com")
	gitOrFatal(t, "-C", src, "config", "user.name", "Test")
	gitOrFatal(t, "-C", src, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOrFatal(t, "-C", src, "add", ".")
	gitOrFatal(t, "-C", src, "commit", "-m", "init")
	gitOrFatal(t, "-C", src, "checkout", "-b", prBranch)
	if err := os.WriteFile(filepath.Join(src, "pr.txt"), []byte("their work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOrFatal(t, "-C", src, "add", ".")
	gitOrFatal(t, "-C", src, "commit", "-m", "their work")
	head := gitOrFatal(t, "-C", src, "rev-parse", "HEAD")
	gitOrFatal(t, "-C", src, "update-ref", "refs/pull/42/head", head)
	gitOrFatal(t, "-C", src, "checkout", "main")
	gitOrFatal(t, "-C", src, "branch", "-D", prBranch)

	bare := filepath.Join(tmp, "origin.git")
	gitOrFatal(t, "clone", "--bare", src, bare)
	gitOrFatal(t, "-c", "safe.bareRepository=all", "-C", bare, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	gitOrFatal(t, "-c", "safe.bareRepository=all", "-C", bare, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")

	projects, err := project.NewStore(filepath.Join(tmp, "projects"), filepath.Join(tmp, "clones"))
	if err != nil {
		t.Fatal(err)
	}
	projYAML := "id: owner/repo\nname: repo\nowner: owner\nrepo: repo\nurl: " + bare +
		"\nclone_path: " + bare + "\ntype: pet\n"
	if err := os.WriteFile(filepath.Join(tmp, "projects", "owner--repo.yaml"), []byte(projYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	taskStore, err := task.NewStore(filepath.Join(tmp, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	taskMgr := task.NewManager(taskStore, nil)
	tk, err := taskMgr.Create("Review: their work", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	tags := []string{task.TagReview}
	tk, err = taskMgr.Update(tk.ID, task.Update{
		ProjectID: task.Ptr("owner/repo"),
		PRNumber:  task.Ptr(prNumber),
		Branch:    task.Ptr(prBranch),
		Tags:      &tags,
	})
	if err != nil {
		t.Fatal(err)
	}

	logger := discardLogger()
	agents := newTestAgentManager(t, t.Context(), func(string, any) {}, logger, filepath.Join(tmp, "logs"))
	t.Cleanup(func() { agents.ShutdownWithGrace(2 * time.Second) })
	wm := worktree.New(worktree.Config{
		WorktreesDir:     filepath.Join(tmp, "worktrees"),
		Projects:         projects,
		Tasks:            taskMgr,
		Logger:           logger,
		AgentChecker:     agents.HasRunningAgentForTask,
		LiveAgentChecker: agents.HasLiveRegisteredAgentForTask,
		PRBranchResolver: func(_ string, _ int) (string, error) { return prBranch, nil },
	})
	agentOrch := agentorch.New(taskMgr, projects, agents, nil, logger, wm, nil)
	aa := &agentAdapter{agents: agents, agentOrch: agentOrch, tasks: taskMgr, projects: projects}

	return reviewDispatchHarness{aa: aa, agents: agents, task: tk, src: src, branch: prBranch}
}

func TestResolveWorktreeDirReviewRoleChecksOutPRHeadDetached(t *testing.T) {
	// Given a review task carrying another author's PR branch
	h := setupReviewDispatchHarness(t)
	claim, ok := h.agents.TryClaimDispatch(h.task.ID)
	if !ok {
		t.Fatal("dispatch claim not granted")
	}
	defer claim.Release()

	// When the review agent's worktree is resolved
	_, dir, _, err := h.aa.resolveWorktreeDir(h.task, h.task.ID, agent.RoleReview, "", claim)
	if err != nil {
		t.Fatalf("resolveWorktreeDir: %v", err)
	}

	// Then it is a detached read-only checkout of the PR head, never pushed
	if out, symErr := exec.Command("git", "-C", dir, "symbolic-ref", "-q", "HEAD").CombinedOutput(); symErr == nil {
		t.Fatalf("review worktree is on branch %q, want detached PR head", strings.TrimSpace(string(out)))
	}
	if exec.Command("git", "-C", h.src, "rev-parse", "--verify", "refs/heads/"+h.branch).Run() == nil {
		t.Fatal("review dispatch pushed the branch it was only asked to review")
	}
}
