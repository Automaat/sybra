package review

import (
	"context"
	"strings"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

// resolveAddressedCopilotThreads breaks the pet auto-merge deadlock.
//
// The merge gate requires zero unresolved review threads, but the fix-review
// skill is deliberately told never to resolve threads ("the reviewer decides").
// GitHub Copilot is an automated reviewer that won't resolve its own threads, so
// a pet PR that Copilot commented on would stay parked forever. For a pet PR
// that is otherwise ready to merge and blocked only by unresolved threads, this
// resolves the Copilot-authored threads the fix agent has already addressed —
// detected via isOutdated (the anchored code changed since the comment). Human-
// authored threads and still-live (non-outdated) Copilot threads are never
// touched, so genuinely-unaddressed feedback still blocks the merge.
func (r *Handler) resolveAddressedCopilotThreads(ctx context.Context, tasks []task.Task, prs []github.PullRequest) {
	byNumber := make(map[int]string, len(tasks))
	byBranch := make(map[string]string, len(tasks))
	for i := range tasks {
		t := &tasks[i]
		if !ownPRColumnTask(t) {
			continue
		}
		proj, err := r.projects.Get(t.ProjectID)
		if err != nil || proj.Type != project.ProjectTypePet {
			continue
		}
		if t.PRNumber != 0 {
			byNumber[t.PRNumber] = t.ID
		}
		if t.Branch != "" {
			byBranch[t.Branch] = t.ID
		}
	}
	if len(byNumber) == 0 && len(byBranch) == 0 {
		return
	}

	for i := range prs {
		pr := &prs[i]
		if !blockedOnlyByThreads(*pr) {
			continue
		}
		taskID := byNumber[pr.Number]
		if taskID == "" {
			taskID = byBranch[pr.HeadRefName]
		}
		if taskID == "" {
			continue
		}
		// Don't fight an in-flight fix agent / workflow that may still be
		// pushing commits or replying on threads.
		if r.agents.HasRunningAgentForTask(taskID) {
			continue
		}
		if r.WorkflowEngine != nil && r.WorkflowEngine.HasActiveWorkflow(taskID) {
			continue
		}
		r.resolveCopilotThreadsForPR(taskID, *pr, r.agentLogin(ctx))
	}
}

// blockedOnlyByThreads reports whether a PR meets every auto-merge condition
// except the unresolved-threads check — the precise state in which resolving
// addressed Copilot threads can unblock a merge.
func blockedOnlyByThreads(pr github.PullRequest) bool {
	return !pr.IsDraft &&
		pr.Mergeable == "MERGEABLE" &&
		(pr.CIStatus == "SUCCESS" || pr.CIStatus == "") &&
		pr.CopilotReviewed &&
		pr.ReviewDecision != "CHANGES_REQUESTED" &&
		pr.UnresolvedCount > 0
}

func (r *Handler) resolveCopilotThreadsForPR(taskID string, pr github.PullRequest, agentLogin string) {
	// ViewerLogin() can fail (returns ""); fall back to the PR author so an
	// addressed thread is still detected. On own-PRs the author is the agent's
	// identity, mirroring convertCommonPR's fallback. Without this an empty
	// agentLogin would match nothing and re-park the pet PR on its threads.
	if agentLogin == "" {
		agentLogin = pr.Author
	}
	fetch := r.fetchThreads
	if fetch == nil {
		fetch = github.FetchReviewThreads
	}
	resolve := r.resolveThread
	if resolve == nil {
		resolve = github.ResolveReviewThread
	}

	threads, err := fetch(pr.Repository, pr.Number)
	if err != nil {
		r.logger.Warn("pr-monitor.threads.fetch", "task_id", taskID, "pr", pr.Number, "err", err)
		return
	}

	var resolved int
	for i := range threads {
		th := &threads[i]
		// Resolve only Copilot-authored threads the fix agent has addressed.
		// Leave human threads and live Copilot threads (no reply yet) alone.
		if th.IsResolved || !github.IsCopilotReviewer(th.AuthorLogin) {
			continue
		}
		// Addressed = the anchored code changed (outdated) OR the fix agent itself
		// posted the last reply. Copilot never resolves its own threads, so
		// without this an addressed-but-not-outdated thread would block the pet
		// merge forever. The reply must be the agent's own identity, not just
		// "not Copilot" — otherwise a human collaborator's reply on a Copilot
		// thread would be auto-dismissed, discarding live feedback.
		agentReplied := agentLogin != "" && strings.EqualFold(th.LastAuthorLogin, agentLogin)
		if !th.IsOutdated && !agentReplied {
			continue
		}
		if err := resolve(th.ID); err != nil {
			r.logger.Warn("pr-monitor.threads.resolve", "task_id", taskID, "pr", pr.Number, "thread", th.ID, "err", err)
			continue
		}
		resolved++
	}
	if resolved > 0 {
		r.logAudit(audit.EventPRCopilotThreadsResolved, taskID, "", map[string]any{
			"pr": pr.Number, "repo": pr.Repository, "count": resolved,
		})
		r.logger.Info("pr-monitor.threads.resolved", "task_id", taskID, "pr", pr.Number, "count", resolved)
	}
}
