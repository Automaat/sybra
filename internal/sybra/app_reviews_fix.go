package sybra

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

const wtFailureLimit = 5

func (r *ReviewHandler) handleAutoMerge(issue github.PRIssue) {
	t, err := r.tasks.Get(issue.TaskID)
	if err != nil {
		return
	}

	proj, err := r.projects.Get(t.ProjectID)
	if err != nil || proj.Type != project.ProjectTypePet {
		return
	}

	if err := github.MergePR(issue.PR.Repository, issue.PR.Number); err != nil {
		r.logger.Error("auto-merge.failed", "task_id", t.ID, "pr", issue.PR.Number, "err", err)
		return
	}

	r.prTracker.MarkHandled(t.ID, issue.Kind, issue.PR.HeadSHA)
	r.logAudit(audit.EventPRAutoMerged, t.ID, "", map[string]any{
		"pr": issue.PR.Number, "repo": issue.PR.Repository,
	})
	r.logger.Info("auto-merge.merged", "task_id", t.ID, "pr", issue.PR.Number)
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
				"Push to the same remote the PR was opened from — never "+
				"to `origin` when a `fork` remote exists:\n"+
				"```sh\n"+
				"PUSH_REMOTE=origin\n"+
				"if git config --get remote.fork.url >/dev/null; then "+
				"PUSH_REMOTE=fork; fi\n"+
				"git push \"$PUSH_REMOTE\" HEAD:%s\n"+
				"```",
			issue.PR.HeadRefName, issue.PR.Number, issue.PR.HeadRefName,
		)
		r.logAudit(audit.EventPRCIFailureDetected, t.ID, "", map[string]any{
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
	fullPrompt := fmt.Sprintf("# Task: %s\n\n%s", t.Title, prompt)
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
	if issue.Kind == github.PRIssueConflict {
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
