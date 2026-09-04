package review

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"github.com/Automaat/sybra/internal/attribution"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
)

//go:embed prompts/fix-review.md
var fixReviewRunbook string

// reviewFixPolicy is the harness-side ruling on how a pr-fix agent must treat
// each review thread. The failure it exists to stop — a reply that concedes the
// point and files it as a follow-up instead of making the change — costs a whole
// review round and reads to the reviewer as work refused.
const reviewFixPolicy = "- Fix it in this PR. A reviewer's comment is work for this PR, never a follow-up. " +
	"Never reply that a fix is deferred, out of scope, left as-is for now, better in a separate PR, " +
	"or something you are happy to pick up separately.\n" +
	"- Size is not an exemption. Threading a new parameter through a constructor, widening a shared " +
	"test helper, or changing a signature across call sites is still this PR's work — make the change, " +
	"then run the tests it touches.\n" +
	"- Ties go to the reviewer. When the call is close, the request is ambiguous, or the trade-off has " +
	"no clear winner, implement the reviewer's version and state the trade-off in the reply.\n" +
	"- A question is a fix request with a polite face. Answer it, then apply the change the honest " +
	"answer calls for. Reply without a change only when the code is already right, and cite where.\n" +
	"- Leave the code alone only for a comment you can prove wrong, and put that evidence in the reply.\n" +
	"- If a change truly cannot be made here — it needs a human decision, a credential, or code outside " +
	"this worktree — leave that thread unanswered and report human-required naming the blocker. " +
	"A parked task keeps the feedback live; a deferral reply buries it.\n" +
	"- The harness re-reads your replies. A thread whose latest reply promises a follow-up counts as " +
	"unanswered and parks the task, exactly as ignoring the thread would."

// renderFixReviewRunbook fills the embedded procedure in. target is the shell
// block that sets OWNER/REPO/PR, push the block that publishes the fix.
//
// The runbook is spelled out here rather than delegated to `/fix-review`
// because a dispatch prompt is the one thing every provider reads the same
// way: a slash invocation only resolves natively on claude, and the review
// policy is too easy to lose in a skill body the model may skim. The
// fix-review-auto skill still ships for operators invoking it by hand.
func renderFixReviewRunbook(target, commitFlags, push string) string {
	return strings.NewReplacer(
		"{{TARGET}}", target,
		"{{POLICY}}", reviewFixPolicy,
		"{{COMMIT_FLAGS}}", commitFlags,
		"{{PUSH}}", push,
		"{{FOOTER}}", attribution.Footer,
	).Replace(fixReviewRunbook)
}

// fixReviewTargetLiteral names the PR the monitor already resolved.
func fixReviewTargetLiteral(repo string, number int) string {
	owner, name, _ := strings.Cut(repo, "/")
	return fmt.Sprintf("OWNER=%s\nREPO=%s\nPR=%d", owner, name, number)
}

// fixReviewTargetFromWorktree resolves the PR from the checkout itself, for the
// operator-triggered path where the dispatcher passes no PR.
const fixReviewTargetFromWorktree = "OWNER=$(gh repo view --json owner -q .owner.login)\n" +
	"REPO=$(gh repo view --json name -q .name)\n" +
	"PR=$(gh pr view --json number -q .number)"

// commentsPrompt is the pr-fix prompt for a PR whose reviewers left unresolved
// threads.
func commentsPrompt(ctx context.Context, pr github.PullRequest, signing project.SigningPolicy, brief reviewThreadBrief) string {
	push := prFixPushPrompt(pr.HeadRefName, "Push to the same remote create-pr would target for this worktree:", true, false)
	prompt := fmt.Sprintf(
		"This is your own PR (#%d) — reviewers left comments or unresolved threads. "+
			"Fix them in this PR, reply on every thread, and push. Work through the "+
			"procedure below yourself.\n\n%s",
		pr.Number, renderFixReviewRunbook(fixReviewTargetLiteral(pr.Repository, pr.Number), signing.CommitFlags(ctx), push),
	)
	if brief.prompt != "" {
		prompt += "\n\n" + brief.prompt
	}
	return prompt
}

// manualFixReviewPrompt is the operator-triggered counterpart of
// commentsPrompt. It names no PR — the worktree it runs in already does — but
// it carries the same procedure, since an operator-started run answers the same
// threads as an automatic one.
func manualFixReviewPrompt(signing project.SigningPolicy) string {
	push := prFixPushPrompt(`"$BRANCH"`, "Push to the same remote create-pr would target for this worktree:", true, false)
	push = strings.Replace(push, "```sh\n", "```sh\nBRANCH=$(gh pr view --json headRefName -q .headRefName)\n", 1)
	return "Reviewers left comments or unresolved threads on the pull request this " +
		"worktree belongs to. Fix them in that PR, reply on every thread, and push. " +
		"Work through the procedure below yourself.\n\n" +
		renderFixReviewRunbook(fixReviewTargetFromWorktree, signing.CommitFlags(context.Background()), push)
}
