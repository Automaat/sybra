package sybra

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/task"
)

func newDetachedCheckout(t *testing.T) (dir, sha string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "Test")
	run("config", "commit.gpgsign", "false")
	run("commit", "--allow-empty", "-m", "base")
	sha = run("rev-parse", "HEAD")
	run("checkout", "--detach", sha)
	return dir, sha
}

func TestRemoteBaseRef_DetachedReviewCheckoutNamesThePRHead(t *testing.T) {
	// Given a review task on a detached checkout of a pull request head
	dir, _ := newDetachedCheckout(t)
	review := task.Task{ID: "t1", PRNumber: 382}

	// When its remote workspace base ref is resolved
	ref, err := remoteBaseRef(context.Background(), review, dir)

	// Then the pull request head names it instead of failing on symbolic-ref
	if err != nil {
		t.Fatalf("remoteBaseRef: %v", err)
	}
	if ref != "refs/pull/382/head" {
		t.Fatalf("ref = %q, want refs/pull/382/head", ref)
	}
}

func TestRemoteBaseRef_PrefersTheTaskBranch(t *testing.T) {
	// Given a checkout attached to one branch and a task recording another
	dir, sha := newDetachedCheckout(t)
	cmd := exec.Command("git", "checkout", "-B", "feat/other", sha)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout: %v\n%s", err, out)
	}
	owned := task.Task{ID: "t1", Branch: "feat/mine", PRNumber: 7}

	// When the base ref is resolved
	ref, err := remoteBaseRef(context.Background(), owned, dir)

	// Then the task's branch wins over both the checkout and the pull request
	if err != nil {
		t.Fatalf("remoteBaseRef: %v", err)
	}
	if ref != "refs/heads/feat/mine" {
		t.Fatalf("ref = %q, want refs/heads/feat/mine", ref)
	}
}

func TestRemoteBaseRef_ReadsAnAttachedCheckout(t *testing.T) {
	// Given a branchless task on an ordinary attached checkout
	dir, sha := newDetachedCheckout(t)
	cmd := exec.Command("git", "checkout", "-B", "feat/attached", sha)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout: %v\n%s", err, out)
	}

	// When the base ref is resolved
	ref, err := remoteBaseRef(context.Background(), task.Task{ID: "t1"}, dir)

	// Then the checked-out branch names it
	if err != nil {
		t.Fatalf("remoteBaseRef: %v", err)
	}
	if ref != "refs/heads/feat/attached" {
		t.Fatalf("ref = %q, want refs/heads/feat/attached", ref)
	}
}

func TestRemoteBaseRef_DetachedWithoutAPullRequestSaysSo(t *testing.T) {
	// Given a detached checkout with neither a branch nor a pull request
	dir, _ := newDetachedCheckout(t)

	// When the base ref is resolved
	_, err := remoteBaseRef(context.Background(), task.Task{ID: "t1"}, dir)

	// Then the refusal names the real condition, not a git plumbing failure
	if err == nil {
		t.Fatal("remoteBaseRef accepted a checkout it cannot name")
	}
	if !strings.Contains(err.Error(), "no branch or pull request") {
		t.Fatalf("error = %v, want it to name the missing branch and pull request", err)
	}
	if strings.Contains(err.Error(), "symbolic-ref") {
		t.Fatalf("error = %v, want no git plumbing detail", err)
	}
}

func TestRemoteBaseRef_ResultPassesTheExecutionContract(t *testing.T) {
	// Given every shape the resolver can return
	dir, sha := newDetachedCheckout(t)
	cases := []task.Task{
		{ID: "t1", PRNumber: 382},
		{ID: "t2", Branch: "feat/mine"},
	}

	for _, tk := range cases {
		// When the base ref is resolved and carried into a run spec
		ref, err := remoteBaseRef(context.Background(), tk, dir)
		if err != nil {
			t.Fatalf("remoteBaseRef(%+v): %v", tk, err)
		}
		workspace := executioncontract.Workspace{
			RepositoryID: "owner/repo",
			BaseSHA:      sha,
			BaseRef:      ref,
			Roots:        []executioncontract.LogicalRoot{executioncontract.RootWorktree},
		}

		// Then the contract that gates every dispatch accepts it
		if err := specForWorkspace(workspace).Validate(); err != nil {
			t.Fatalf("ref %q rejected by the execution contract: %v", ref, err)
		}
	}
}

func specForWorkspace(workspace executioncontract.Workspace) executioncontract.RunSpec {
	return executioncontract.RunSpec{
		Version:        executioncontract.CurrentVersion(),
		BuildVersion:   "test-build",
		RunID:          "run-1",
		EffectID:       "effect-1",
		IdempotencyKey: "idem-1",
		Fence: executioncontract.GenerationFence{
			TaskID: "t1", TaskGeneration: 1, WorkflowID: "simple-task-implement", WorkflowGeneration: 1, StepID: "implement",
		},
		Role:      "implementation",
		Provider:  executioncontract.ProviderIntent{Provider: providerid.Codex, Model: "gpt-5.4"},
		Prompt:    executioncontract.Prompt{Text: "do the thing"},
		Deadline:  time.Now().UTC().Add(time.Hour),
		Workspace: workspace,
	}
}

func TestSpecForWorkspaceIsOtherwiseValid(t *testing.T) {
	// Given the fixture the ref assertions validate through
	_, sha := newDetachedCheckout(t)
	workspace := executioncontract.Workspace{
		RepositoryID: "owner/repo",
		BaseSHA:      sha,
		BaseRef:      "refs/heads/main",
		Roots:        []executioncontract.LogicalRoot{executioncontract.RootWorktree},
	}

	// When a spec carrying a sound workspace is validated
	err := specForWorkspace(workspace).Validate()

	// Then nothing outside the workspace makes it fail, so a ref verdict is observable
	if err != nil {
		t.Fatalf("fixture spec is invalid for reasons unrelated to the workspace: %v", err)
	}
}

func TestSpecForWorkspaceRejectsABadBaseRef(t *testing.T) {
	// Given a workspace whose base ref the contract must refuse
	_, sha := newDetachedCheckout(t)
	workspace := executioncontract.Workspace{
		RepositoryID: "owner/repo",
		BaseSHA:      sha,
		BaseRef:      "refs/heads/bad..ref",
		Roots:        []executioncontract.LogicalRoot{executioncontract.RootWorktree},
	}

	// When the spec is validated
	err := specForWorkspace(workspace).Validate()

	// Then the ref verdict reaches the caller, so the acceptance test can fail
	if err == nil {
		t.Fatal("contract accepted a base ref containing '..'")
	}
}

func TestFollowerResolvableBaseRef(t *testing.T) {
	tests := []struct {
		name   string
		ref    string
		usable bool
	}{
		{"branch ref", "refs/heads/main", true},
		{"pull head", "refs/pull/382/head", false},
		{"remote tracking", "refs/remotes/origin/main", false},
		{"tag", "refs/tags/v1", false},
		{"empty", "", false},
		{"branch prefix bolted onto a full ref", "refs/heads/refs/pull/382/head", false},
		{"bare prefix", "refs/heads/", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Given a base ref bound for a follower's run clone
			// When placement asks whether that clone can resolve it
			got := followerResolvableBaseRef(tc.ref)

			// Then only a plain branch ref skips shipping a base bundle
			if got != tc.usable {
				t.Fatalf("followerResolvableBaseRef(%q) = %v, want %v", tc.ref, got, tc.usable)
			}
		})
	}
}

func TestPlacementCarriesTheResolvedBaseRefVerbatim(t *testing.T) {
	// Given a review task's detached checkout of a pull request head
	dir, _ := newDetachedCheckout(t)
	review := task.Task{ID: "t1", PRNumber: 382}

	// When placement resolves the base ref it will put in the run spec
	ref, err := remoteBaseRef(context.Background(), review, dir)
	if err != nil {
		t.Fatalf("remoteBaseRef: %v", err)
	}

	// Then it travels unchanged, never re-prefixed into refs/heads/refs/pull/...
	metadata := agent.RemoteRunMetadata{WorkspaceBaseRef: ref}
	if metadata.WorkspaceBaseRef != "refs/pull/382/head" {
		t.Fatalf("WorkspaceBaseRef = %q, want the resolver's own ref", metadata.WorkspaceBaseRef)
	}
	if strings.HasPrefix(metadata.WorkspaceBaseRef, "refs/heads/refs/") {
		t.Fatalf("WorkspaceBaseRef = %q, want no double prefix", metadata.WorkspaceBaseRef)
	}
}

func TestPrepareRemoteWorkspaceBaseShipsABundleForANonBranchRef(t *testing.T) {
	// Given a leader whose base commit the follower's anchor already contains
	dir, base := remoteBackendRepository(t)

	// When placement prepares the base for a branch ref
	content, ref, err := prepareRemoteWorkspaceBase(context.Background(), dir, "run-branch", base, base, "refs/heads/main")
	if err != nil {
		t.Fatalf("prepareRemoteWorkspaceBase(branch): %v", err)
	}

	// Then the anchor shortcut applies and no bundle travels
	if len(content) != 0 || ref != nil {
		t.Fatalf("branch ref shipped a bundle it does not need: content=%d ref=%v", len(content), ref)
	}

	// When the same base is prepared for a detached pull-request ref
	content, ref, err = prepareRemoteWorkspaceBase(context.Background(), dir, "run-pull", base, base, "refs/pull/382/head")
	if err != nil {
		t.Fatalf("prepareRemoteWorkspaceBase(pull): %v", err)
	}

	// Then a bundle travels, since the follower's clone cannot resolve that ref
	if len(content) == 0 || ref == nil {
		t.Fatal("pull ref took the anchor shortcut; the follower would fail the ancestry gate")
	}
}
