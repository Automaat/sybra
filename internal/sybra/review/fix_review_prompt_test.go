package review

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/attribution"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/skillinvoke"
)

// The dispatch prompt carries the whole procedure, so a run must not depend on
// a skill resolving: a slash invocation only runs natively on claude, and it
// also sets RunConfig.RequestedSkill, which arms a receipt gate for a skill this
// path no longer uses.
func TestFixReviewPromptsAreSelfContained(t *testing.T) {
	t.Parallel()

	pr := github.PullRequest{Number: 7, Repository: "o/r", HeadRefName: "feat", URL: "https://github.com/o/r/pull/7"}
	prompts := map[string]string{
		"automatic": commentsPrompt(context.Background(), pr, project.SigningAuto, reviewThreadBrief{}),
		"manual":    manualFixReviewPrompt(project.SigningAuto),
	}

	for name, prompt := range prompts {
		if names := skillinvoke.InvokedNames(prompt); len(names) != 0 {
			t.Errorf("%s prompt invokes skills %v, which arms the skill-receipt gate", name, names)
		}
		for _, want := range []string{
			"reviewThreads(first: 100)",
			"isResolved: false",
			"addPullRequestReviewThreadReply",
			"comments/$FIRST_COMMENT_DBID/replies",
			attribution.Footer,
			"fix(review): address PR review comments",
			"git push",
			reviewFixPolicy,
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("%s prompt missing %q", name, want)
			}
		}
		if strings.Contains(prompt, "{{") {
			t.Errorf("%s prompt has an unfilled placeholder:\n%s", name, prompt)
		}
	}
}

// Each provider shell call is a fresh process, so a fenced block that reads
// OWNER/REPO/PR must also set them. A block that inherits them from an earlier
// fence sends gh an empty -F pr and the agent's own enumeration fails — the
// exact state the thread brief exists to bound.
func TestFixReviewRunbookRepeatsShellStateInEveryBlock(t *testing.T) {
	t.Parallel()

	reads := regexp.MustCompile(`\$(?:OWNER|REPO|PR)\b`)
	prompt := commentsPrompt(context.Background(), github.PullRequest{Number: 7, Repository: "o/r", HeadRefName: "feat"}, project.SigningAuto, reviewThreadBrief{})
	for _, block := range shellBlocks(prompt) {
		if !reads.MatchString(block) {
			continue
		}
		for _, want := range []string{"OWNER=", "REPO=", "PR="} {
			if !strings.Contains(block, want) {
				t.Errorf("shell block reads $OWNER/$PR without setting %q:\n%s", want, block)
			}
		}
	}
}

// The operator path has no dispatcher-resolved branch, and a detached HEAD would
// make `git rev-parse --abbrev-ref HEAD` render `HEAD:HEAD`, which git refuses
// after a preflight that used a different refspec and passed.
func TestManualFixReviewPromptPushesThePRBranch(t *testing.T) {
	t.Parallel()

	manual := manualFixReviewPrompt(project.SigningAuto)
	if !strings.Contains(manual, "BRANCH=$(gh pr view --json headRefName -q .headRefName)") {
		t.Errorf("manual prompt does not resolve the PR branch:\n%s", manual)
	}
	if strings.Contains(manual, "HEAD:$(git rev-parse --abbrev-ref HEAD)") {
		t.Errorf("manual prompt pushes the symbolic HEAD, which is empty on a detached checkout:\n%s", manual)
	}
}

func shellBlocks(prompt string) []string {
	var blocks []string
	for i, fenced := range strings.Split(prompt, "```sh\n") {
		if i == 0 {
			continue
		}
		block, _, _ := strings.Cut(fenced, "```")
		blocks = append(blocks, block)
	}
	return blocks
}

// The automatic path knows the PR; only the operator path has to find it.
func TestFixReviewPromptTargets(t *testing.T) {
	t.Parallel()

	auto := commentsPrompt(context.Background(), github.PullRequest{Number: 7, Repository: "o/r", HeadRefName: "feat"}, project.SigningAuto, reviewThreadBrief{})
	for _, want := range []string{"OWNER=o", "REPO=r", "PR=7"} {
		if !strings.Contains(auto, want) {
			t.Errorf("automatic prompt missing %q", want)
		}
	}

	manual := manualFixReviewPrompt(project.SigningAuto)
	if !strings.Contains(manual, "gh pr view --json number") {
		t.Errorf("manual prompt cannot resolve the worktree's PR:\n%s", manual)
	}
	if strings.Contains(manual, "PR=7") {
		t.Errorf("manual prompt must not carry a dispatcher-resolved PR:\n%s", manual)
	}
}
