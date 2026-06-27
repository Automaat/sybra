package sybra

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

const wtFailureLimit = 5

const prFixResultContract = "\n\nBefore your final response, decide the outcome:\n" +
	"- If you completed and pushed the fix, end with `SYBRA_PR_FIX_RESULT: continue`.\n" +
	"- If you intentionally stopped because the PR needs a human, end with " +
	"`SYBRA_PR_FIX_RESULT: human-required` and `SYBRA_PR_FIX_REASON: <short reason>`."

// readyForCopilotAutoMerge reports whether a pet PR satisfies the auto-merge
// policy: mechanically mergeable, CI green (or no checks), GitHub Copilot has
// submitted a review, no unresolved review threads, and no outstanding change
// request. Human approval is intentionally NOT required — pet PRs never get one;
// Copilot's review is the gate. A repo without Copilot enabled stays parked in
// In Review until a human merges it.
func readyForCopilotAutoMerge(pr github.PullRequest) bool {
	return !pr.IsDraft &&
		pr.Mergeable == "MERGEABLE" &&
		(pr.CIStatus == "SUCCESS" || pr.CIStatus == "") &&
		pr.CopilotReviewed &&
		pr.UnresolvedCount == 0 &&
		pr.ReviewDecision != "CHANGES_REQUESTED"
}

func (r *ReviewHandler) handleAutoMerge(issue github.PRIssue) {
	t, err := r.tasks.Get(issue.TaskID)
	if err != nil {
		return
	}

	proj, err := r.projects.Get(t.ProjectID)
	if err != nil || proj.Type != project.ProjectTypePet {
		return
	}

	// Defense in depth: never merge a PR that lives outside the task's own
	// project. A mis-linked PR number (e.g. a branch-name collision) must not
	// be able to squash-merge an unrelated repo. proj.ID and PR.Repository are
	// both owner/repo.
	if issue.PR.Repository != proj.ID {
		r.logger.Warn("auto-merge.repo-mismatch",
			"task_id", t.ID, "task_project", proj.ID, "pr_repo", issue.PR.Repository, "pr", issue.PR.Number)
		return
	}

	// Hold the merge until Copilot has reviewed and its threads are resolved.
	// Without this, a green PR merges on the first poll after CI passes — before
	// Copilot's (asynchronous) review lands — and its feedback is skipped.
	//
	// Renovate dependency-bump PRs (surfaced via the "Fix CI" flow) are bot-
	// authored and never receive a Copilot review, so the Copilot gate would
	// strand them. The ReadyToMerge issue already implies green + mergeable +
	// !draft, so preserve their prior green auto-merge.
	if !slices.Contains(t.Tags, "renovate-fix") && !readyForCopilotAutoMerge(issue.PR) {
		return
	}

	merge := r.mergePR
	if merge == nil {
		merge = github.MergePR
	}
	if err := merge(issue.PR.Repository, issue.PR.Number); err != nil {
		r.logger.Error("auto-merge.failed", "task_id", t.ID, "pr", issue.PR.Number, "err", err)
		return
	}

	r.prTracker.MarkHandled(t.ID, issue.Kind, issue.PR.HeadSHA)
	r.logAudit(audit.EventPRAutoMerged, t.ID, "", map[string]any{
		"pr": issue.PR.Number, "repo": issue.PR.Repository,
	})
	r.logger.Info("auto-merge.merged", "task_id", t.ID, "pr", issue.PR.Number)
}

// escalateExhaustedFix parks a task whose pr-fix retry budget is spent. Trying
// the same fix MaxRetries times without clearing the issue means the agent
// can't resolve it on its own — flaky/unfixable CI, an unrebasable conflict, or
// review feedback that needs a human call — so stop looping and surface it.
// Mirrors the worktree circuit-breaker: own-PR tasks normally stay in In
// Review, but a hard, repeated failure escalates to human-required.
//
// Applies to every fixable kind (conflict, ci_failure, comments) — the durable
// retry cap keeps a capped entry across Cleanup, so a kind that did not escalate
// here would sit capped forever, never retried and never surfaced. Only the
// comments kind carries a feedback signature, so genuinely new reviewer feedback
// resets its budget (in Decide) before it ever reaches here. ready_to_merge
// never escalates — a green PR that simply hasn't merged is not a failure.
//
// Idempotent: a task already in human-required is left untouched. The tracker
// entry is cleared so a human un-parking the task starts from a fresh budget.
func (r *ReviewHandler) escalateExhaustedFix(issue github.PRIssue) {
	if issue.Kind == github.PRIssueReadyToMerge {
		return
	}
	t, err := r.tasks.Get(issue.TaskID)
	if err != nil || t.Status == task.StatusHumanRequired {
		return
	}
	reason := fmt.Sprintf(
		"pr-monitor: auto-fix exhausted after %d attempts (%s) — needs a human",
		github.MaxRetries, issue.Kind,
	)
	if _, err := r.tasks.Update(issue.TaskID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr(reason),
	}); err != nil {
		r.logger.Error("pr-monitor.fix-exhausted.escalate", "task_id", issue.TaskID, "err", err)
		return
	}
	r.prTracker.Clear(issue.TaskID, issue.Kind)
	r.logAudit(audit.EventPRFixExhausted, issue.TaskID, "", map[string]any{
		"pr": issue.PR.Number, "repo": issue.PR.Repository,
		"kind": string(issue.Kind), "attempts": github.MaxRetries,
	})
	r.logger.Warn("pr-monitor.fix-exhausted",
		"task_id", issue.TaskID, "pr", issue.PR.Number,
		"kind", string(issue.Kind), "attempts", github.MaxRetries)
}

func (r *ReviewHandler) handlePRIssue(issue github.PRIssue) {
	t, err := r.tasks.Get(issue.TaskID)
	if err != nil {
		return
	}

	var prompt string
	switch issue.Kind {
	case github.PRIssueConflict:
		prompt = conflictPrompt(issue.PR)
		r.logAudit(audit.EventPRConflictDetected, t.ID, "", map[string]any{
			"pr": issue.PR.Number, "repo": issue.PR.Repository,
		})

	case github.PRIssueCIFailure:
		prompt = fmt.Sprintf(
			"Fix failing CI on branch `%s` (PR #%d). "+
				"Check the failing run with `gh run view --log-failed`, "+
				"fix the code, commit and push. No unrelated changes.\n\n"+
				"Never weaken, skip, delete, or hardcode tests, snapshots, or "+
				"fixtures to make CI pass, and never edit CI config to neuter a "+
				"gate — fix the underlying code. Tampering is detected and blocks "+
				"the task.\n\n"+
				"Push to the remote that hosts the PR's head branch:\n"+
				"```sh\n"+
				"PUSH_REMOTE=origin\n"+
				"PR_HEAD=\"$(gh pr view %d --json headRepository --jq '.headRepository.nameWithOwner')\"\n"+
				"FORK_URL=\"$(git config --get remote.fork.url 2>/dev/null || true)\"\n"+
				"if [ -n \"$FORK_URL\" ] && echo \"$FORK_URL\" | grep -qF \"$PR_HEAD\"; then PUSH_REMOTE=fork; fi\n"+
				"git push \"$PUSH_REMOTE\" HEAD:%s\n"+
				"```",
			issue.PR.HeadRefName, issue.PR.Number, issue.PR.Number, issue.PR.HeadRefName,
		)
		r.logAudit(audit.EventPRCIFailureDetected, t.ID, "", map[string]any{
			"pr": issue.PR.Number, "repo": issue.PR.Repository,
		})

	case github.PRIssueComments:
		prompt = commentsPrompt(issue.PR)
		r.logAudit(audit.EventPRCommentsDetected, t.ID, "", map[string]any{
			"pr": issue.PR.Number, "repo": issue.PR.Repository,
		})

	case github.PRIssueReadyToMerge:
		// handled by handleAutoMerge, not by agent spawn
		return
	}

	dir, ok := r.prepareWorktree(t, issue)
	if !ok {
		return
	}

	if r.workflowEngine == nil {
		r.logger.Error("pr-monitor.no-workflow-engine", "task_id", t.ID)
		return
	}

	// Dispatch pr.event through the engine so trigger conditions in the
	// workflow YAML stay authoritative. StartWorkflow would bypass them.
	fullPrompt := fmt.Sprintf("# Task: %s\n\n%s%s", t.Title, prompt, prFixResultContract)
	vars := map[string]string{
		"prompt":                fullPrompt,
		"pr_issue_kind":         string(issue.Kind),
		workflow.WorkflowVarDir: dir,
	}
	wfID, err := r.workflowEngine.DispatchEvent(t.ID, "pr.event",
		map[string]string{"pr.issue_kind": string(issue.Kind)}, vars)
	if err != nil {
		if errors.Is(err, workflow.ErrWorkflowAlreadyActive) {
			r.logger.Info("pr-monitor.workflow-already-active",
				"task_id", t.ID, "kind", string(issue.Kind))
			return
		}
		r.logger.Error("pr-monitor.workflow-dispatch", "task_id", t.ID, "err", err)
		return
	}
	if wfID == "" {
		r.logger.Warn("pr-monitor.no-matching-workflow",
			"task_id", t.ID, "kind", string(issue.Kind))
		return
	}

	r.prTracker.MarkHandled(t.ID, issue.Kind, issue.PR.HeadSHA)
	r.logAudit(audit.EventPRFixAgentStarted, t.ID, "", map[string]any{
		"issue": string(issue.Kind), "pr": issue.PR.Number, "workflow": wfID,
	})

	r.logger.Info("pr-monitor.fix-started",
		"task_id", t.ID, "issue", string(issue.Kind),
		"pr", issue.PR.Number, "workflow", wfID,
	)
}

// prepareWorktree sets up the fix worktree for the given task and PR issue.
// Returns ("", false) on error, with circuit-breaker escalation after wtFailureLimit
// consecutive failures. Returns ("", true) when no worktree is needed.
func (r *ReviewHandler) prepareWorktree(t task.Task, issue github.PRIssue) (string, bool) {
	if t.ProjectID == "" {
		return "", true
	}
	var (
		d     string
		wtErr error
	)
	// Conflict and comment fixes operate on the PR's existing branch, so check
	// it out (PrepareForFix). A CI fix re-runs on a fresh worktree.
	if issue.Kind == github.PRIssueConflict || issue.Kind == github.PRIssueComments {
		d, wtErr = r.worktrees.PrepareForFix(t, issue.PR.Number)
	} else {
		d, wtErr = r.worktrees.PrepareForTask(t, nil)
	}
	if wtErr != nil {
		if r.wtFailures == nil {
			r.wtFailures = make(map[string]int)
		}
		r.wtFailures[t.ID]++
		if r.wtFailures[t.ID] >= wtFailureLimit {
			delete(r.wtFailures, t.ID)
			r.logger.Error("pr-monitor.worktree.circuit-open",
				"task_id", t.ID, "failures", wtFailureLimit, "err", wtErr)
			if _, uerr := r.tasks.Update(t.ID, task.Update{
				Status:       task.Ptr(task.StatusHumanRequired),
				StatusReason: task.Ptr(fmt.Sprintf("pr-monitor: worktree creation failed %d times", wtFailureLimit)),
			}); uerr != nil {
				r.logger.Error("pr-monitor.worktree.escalate", "task_id", t.ID, "err", uerr)
			}
			return "", false
		}
		r.logger.Error("pr-monitor.worktree", "task_id", t.ID, "err", wtErr)
		return "", false
	}
	delete(r.wtFailures, t.ID)
	return d, true
}

// commentsPrompt instructs the fix agent to address unresolved review comments
// on the user's own PR via the /fix-review skill (which replies on every
// thread), then push and re-request review.
func commentsPrompt(pr github.PullRequest) string {
	return fmt.Sprintf(
		"Run /fix-review %s --auto\n\n"+
			"This is your own PR (#%d) — reviewers left comments or unresolved "+
			"threads. Address the valid ones, reply on every thread, and push.\n\n"+
			"Never weaken, skip, delete, or hardcode tests to satisfy a comment "+
			"— fix the underlying code; tampering is detected and blocks the "+
			"task.\n\n"+
			"IMPORTANT: when committing, use conventional commit format "+
			"`fix(review): address PR review comments` (type(scope) required by "+
			"repo hooks). Sign the commit with `git commit -s -S`.\n\n"+
			"Push to the same remote the PR was opened from — never to `origin` "+
			"when a `fork` remote exists:\n"+
			"```sh\n"+
			"PUSH_REMOTE=origin\n"+
			"if git config --get remote.fork.url >/dev/null; then PUSH_REMOTE=fork; fi\n"+
			"git push \"$PUSH_REMOTE\" HEAD:%s\n"+
			"```",
		pr.URL, pr.Number, pr.HeadRefName,
	)
}

func conflictPrompt(pr github.PullRequest) string {
	var filesCtx string
	if files, err := github.FetchPRFiles(pr.Repository, pr.Number); err == nil && len(files) > 0 {
		var sb strings.Builder
		sb.WriteString("\n\nFiles changed in this PR:\n")
		for _, f := range files {
			sb.WriteString("- ")
			sb.WriteString(f)
			sb.WriteByte('\n')
		}
		filesCtx = sb.String()
	}

	return fmt.Sprintf(
		"Fix merge conflicts on branch `%s` (PR #%d). "+
			"Do NOT investigate git state — go straight to rebasing.\n\n"+
			"Steps:\n"+
			"```bash\n"+
			"git fetch origin\n"+
			"git rebase refs/remotes/origin/main\n"+
			"# resolve each conflict, git add, git rebase --continue\n"+
			"PUSH_REMOTE=origin\n"+
			"if git config --get remote.fork.url >/dev/null; then PUSH_REMOTE=fork; fi\n"+
			"git push --force-with-lease \"$PUSH_REMOTE\" HEAD:%s\n"+
			"```\n\n"+
			"Rules:\n"+
			"- Use `refs/remotes/origin/main` (not `origin/main`) to avoid ambiguous refs\n"+
			"- Push to `fork` (not `origin`) when a `fork` remote exists — the PR was opened from the fork\n"+
			"- Resolve conflicts keeping BOTH sides' intent\n"+
			"- If rebase produces more than 3 conflicting files, run `git rebase --abort` and stop — the task needs human review\n"+
			"- No investigation, no extra commits, no unrelated changes"+
			"%s",
		pr.HeadRefName, pr.Number, pr.HeadRefName, filesCtx,
	)
}
