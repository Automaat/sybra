package sybra

import (
	"context"
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
//
// A SourcedViaREST PR always returns false here — REST fetches leave
// CopilotReviewed/UnresolvedCount/ReviewDecision zero (thread and review data
// are GraphQL-only), so this gate can never be honestly satisfied over REST;
// readyForRESTAutoMerge is the REST-sourced equivalent.
func readyForCopilotAutoMerge(pr github.PullRequest) bool {
	return !pr.SourcedViaREST &&
		!pr.IsDraft &&
		pr.Mergeable == "MERGEABLE" &&
		(pr.CIStatus == "SUCCESS" || pr.CIStatus == "") &&
		pr.CopilotReviewed &&
		pr.UnresolvedCount == 0 &&
		pr.ReviewDecision != "CHANGES_REQUESTED"
}

// readyToArmNativeAutoMerge reports whether a PR is ready to have GitHub's
// native auto-merge armed: the same review-cycle gate as
// readyForCopilotAutoMerge MINUS the CI-green requirement (native auto-merge
// itself waits for CI to go green) PLUS excluding a PR whose CI is already
// FAILURE — native auto-merge won't retry a hard failure, so arming on red CI
// would just strand it — and PRs already armed or bot-authored by Renovate
// (its own bypass path already merges without this gate).
func readyToArmNativeAutoMerge(pr github.PullRequest) bool {
	return !pr.IsDraft &&
		pr.Mergeable == "MERGEABLE" &&
		pr.CIStatus != "FAILURE" &&
		pr.CopilotReviewed &&
		pr.UnresolvedCount == 0 &&
		pr.ReviewDecision != "CHANGES_REQUESTED" &&
		!pr.AutoMergeEnabled &&
		pr.Author != "renovate[bot]"
}

// readyForRESTAutoMerge reports whether a REST-sourced PR satisfies the
// REST auto-merge policy: not draft, GitHub's raw mergeable_state is exactly
// "clean" (blocked/behind/unstable/unknown do NOT authorize it), both REST CI
// legs were fetched successfully (an unfetched CI status must never read as
// green), CI is green or genuinely absent, and RESTApproved — an explicit,
// current-head approval computed over REST review data. Uses only
// RESTApproved/RESTMergeableState/RESTCIFetched, never the thread-derived
// UnresolvedCount/CopilotReviewed which REST never populates.
func readyForRESTAutoMerge(pr github.PullRequest) bool {
	return !pr.IsDraft &&
		pr.RESTMergeableState == "clean" &&
		pr.RESTCIFetched &&
		(pr.CIStatus == "SUCCESS" || pr.CIStatus == "") &&
		pr.RESTApproved
}

// restRenovateGreen reports whether a REST-sourced renovate-fix PR is
// verified green over REST: strictly clean mergeable state and both CI legs
// fetched successfully and passing. Mirrors the GraphQL renovate-fix bypass
// (ReadyToMerge already implies green + mergeable + !draft) but re-derives it
// from REST-only fields since a REST-sourced PR carries none of the
// GraphQL-only review/thread data the Copilot gate would otherwise check.
func restRenovateGreen(pr github.PullRequest) bool {
	return !pr.IsDraft &&
		pr.RESTMergeableState == "clean" &&
		pr.RESTCIFetched &&
		(pr.CIStatus == "SUCCESS" || pr.CIStatus == "")
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

	// Prefer arming GitHub's native auto-merge over Sybra's own squash merge
	// when it's available and the PR is otherwise ready — it's cheaper (REST
	// poll on GitHub's side) than Sybra's GraphQL merge-gate polling. Only
	// tried once the CI-green-gated legacy path would otherwise fire, so this
	// never delays a merge; it just lets GitHub finish the last mile.
	if r.cfg != nil && r.cfg.GitHub.NativeAutoMerge && readyToArmNativeAutoMerge(issue.PR) {
		supportsFn := r.supportsAutoMergeFn
		if supportsFn == nil {
			supportsFn = github.SupportsNativeAutoMerge
		}
		ok, serr := supportsFn(issue.PR.Repository, issue.PR.BaseRefName)
		if serr != nil {
			r.logger.Error("auto-merge.native-support-check-failed", "task_id", t.ID, "pr", issue.PR.Number, "err", serr)
		}
		if serr == nil && ok {
			enableFn := r.enableAutoMergeFn
			if enableFn == nil {
				enableFn = github.EnableAutoMerge
			}
			if aerr := enableFn(issue.PR.Repository, issue.PR.Number); aerr != nil {
				r.logger.Error("auto-merge.native-arm-failed", "task_id", t.ID, "pr", issue.PR.Number, "err", aerr)
			} else {
				r.evictReadyPRCache(issue.PR.Repository, issue.PR.Number)
				r.prTracker.MarkHandled(t.ID, issue.Kind, issue.PR.HeadSHA)
				r.logAudit(audit.EventAutoMergeEnabled, t.ID, "", map[string]any{
					"pr": issue.PR.Number, "repo": issue.PR.Repository,
				})
				r.logger.Info("auto-merge.native-armed", "task_id", t.ID, "pr", issue.PR.Number)
				return
			}
		}
	}

	renovateFix := slices.Contains(t.Tags, "renovate-fix")

	var ready bool
	var gateEvidence string
	if issue.PR.SourcedViaREST {
		if renovateFix {
			ready = restRenovateGreen(issue.PR)
			gateEvidence = "renovate_green"
		} else {
			ready = readyForRESTAutoMerge(issue.PR)
			gateEvidence = "approved"
		}
	} else {
		// Hold the merge until Copilot has reviewed and its threads are resolved.
		// Without this, a green PR merges on the first poll after CI passes —
		// before Copilot's (asynchronous) review lands — and its feedback is
		// skipped.
		//
		// Renovate dependency-bump PRs (surfaced via the "Fix CI" flow) are bot-
		// authored and never receive a Copilot review, so the Copilot gate would
		// strand them. The ReadyToMerge issue already implies green + mergeable +
		// !draft, so preserve their prior green auto-merge.
		ready = renovateFix || readyForCopilotAutoMerge(issue.PR)
	}
	if !ready {
		return
	}

	var mergeErr error
	if issue.PR.SourcedViaREST {
		merge := r.mergePRViaREST
		if merge == nil {
			merge = github.MergePRViaREST
		}
		mergeErr = merge(issue.PR.Repository, issue.PR.Number, issue.PR.HeadSHA)
	} else {
		merge := r.mergePR
		if merge == nil {
			merge = github.MergePR
		}
		mergeErr = merge(issue.PR.Repository, issue.PR.Number)
	}
	r.evictReadyPRCache(issue.PR.Repository, issue.PR.Number)
	if mergeErr != nil {
		r.logger.Error("auto-merge.failed", "task_id", t.ID, "pr", issue.PR.Number, "err", mergeErr)
		return
	}

	r.prTracker.MarkHandled(t.ID, issue.Kind, issue.PR.HeadSHA)
	auditData := map[string]any{
		"pr": issue.PR.Number, "repo": issue.PR.Repository,
	}
	if issue.PR.SourcedViaREST {
		auditData["sourced_via_rest"] = true
		auditData["gate_evidence"] = gateEvidence
		auditData["head_sha"] = issue.PR.HeadSHA
	}
	r.logAudit(audit.EventPRAutoMerged, t.ID, "", auditData)
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

// ciFailurePrompt is the pr-fix agent prompt for a failing-CI issue.
func ciFailurePrompt(pr github.PullRequest) string {
	return fmt.Sprintf(
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
		pr.HeadRefName, pr.Number, pr.Number, pr.HeadRefName,
	)
}

// prIssueBody returns the pr-fix agent prompt for one fixable issue kind. ok is
// false for kinds with no agent prompt (ready_to_merge), which never reach the
// dispatch path.
func prIssueBody(issue github.PRIssue) (string, bool) {
	switch issue.Kind {
	case github.PRIssueConflict:
		return conflictPrompt(issue.PR), true
	case github.PRIssueCIFailure:
		return ciFailurePrompt(issue.PR), true
	case github.PRIssueComments:
		return commentsPrompt(issue.PR), true
	default:
		return "", false
	}
}

// fixKindPriority orders fixable kinds for coalesced dispatch. The kind with
// the lowest numeric priority (i.e. highest priority) becomes the "primary":
// it drives worktree prep, the workflow's pr_issue_kind var, and cancel/phase
// reconciliation. Conflicts sort first so a
// conflicting PR checks out its branch WITHOUT rebasing (PrepareForFix), then
// comments so any pair that includes review feedback also prefers the
// branch-preserving checkout; a lone ci_failure keeps its rebasing PrepareForTask
// path unchanged.
func fixKindPriority(kind github.PRIssueKind) int {
	switch kind {
	case github.PRIssueConflict:
		return 0
	case github.PRIssueComments:
		return 1
	case github.PRIssueCIFailure:
		return 2
	default:
		return 3
	}
}

func fixKindLabel(kind github.PRIssueKind) string {
	switch kind {
	case github.PRIssueConflict:
		return "Merge conflicts"
	case github.PRIssueCIFailure:
		return "Failing CI"
	case github.PRIssueComments:
		return "Review comments"
	default:
		return string(kind)
	}
}

func (r *ReviewHandler) logPRIssueDetected(taskID string, issue github.PRIssue) {
	var event string
	switch issue.Kind {
	case github.PRIssueConflict:
		event = audit.EventPRConflictDetected
	case github.PRIssueCIFailure:
		event = audit.EventPRCIFailureDetected
	case github.PRIssueComments:
		event = audit.EventPRCommentsDetected
	default:
		return
	}
	r.logAudit(event, taskID, "", map[string]any{
		"pr": issue.PR.Number, "repo": issue.PR.Repository,
	})
}

// coalescedFixPrompt composes a single pr-fix agent prompt covering every issue
// the monitor wants fixed on a PR this cycle. A single issue yields its bare
// per-kind prompt (behavior unchanged); multiple issues — e.g. a push that both
// fails CI and drew review comments — are stitched into one pass so exactly one
// agent runs per review round instead of one agent per kind across cycles.
func coalescedFixPrompt(issues []github.PRIssue) string {
	if len(issues) == 1 {
		body, _ := prIssueBody(issues[0])
		return body
	}
	var b strings.Builder
	b.WriteString("This PR has multiple open issues from the same push. Address " +
		"ALL of them in one pass, then push once at the end (the per-section push " +
		"commands are equivalent — run it a single time).\n\n")
	for i := range issues {
		body, ok := prIssueBody(issues[i])
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "=== Issue %d: %s ===\n%s\n\n", i+1, fixKindLabel(issues[i].Kind), body)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (r *ReviewHandler) handlePRIssue(ctx context.Context, issue github.PRIssue) bool {
	return r.dispatchFixIssues(ctx, issue.TaskID, []github.PRIssue{issue})
}

// dispatchFixIssues spawns a single pr-fix agent that addresses every handled
// issue for a task. handle must be non-empty and contain only fixable kinds
// (conflict, ci_failure, comments); the caller (handleTaskPRIssues) filters out
// ready_to_merge and applies the retry/cooldown gate. Coalescing avoids the
// double-dispatch where a CI failure and review comments from the same push each
// spawned their own sequential agent.
func (r *ReviewHandler) dispatchFixIssues(ctx context.Context, taskID string, handle []github.PRIssue) bool {
	if len(handle) == 0 {
		return false
	}
	t, err := r.tasks.Get(taskID)
	if err != nil {
		return false
	}
	// Stable-sort by fix priority so the primary (index 0) drives worktree prep
	// and the prompt reads in execution order (conflicts → comments → CI).
	slices.SortStableFunc(handle, func(a, b github.PRIssue) int {
		return fixKindPriority(a.Kind) - fixKindPriority(b.Kind)
	})
	for i := range handle {
		r.logPRIssueDetected(t.ID, handle[i])
	}
	primary := handle[0]
	dir, ok := r.prepareWorktree(ctx, t, primary)
	if !ok {
		return false
	}
	// dispatchPRIssue -> workflowEngine.DispatchEvent eventually reaches
	// execShell, which derives its context from workflow.Engine's own e.ctx
	// field (Engine.SetContext), not an explicit parameter threaded here.
	return r.dispatchPRIssue(t, primary, handle, coalescedFixPrompt(handle), dir) //nolint:contextcheck // Engine uses its own e.ctx field, see comment above
}

// dispatchPRIssue starts the pr-fix workflow for primary and, on success, marks
// every coalesced issue in handle as handled so none re-fires next cycle. The
// primary kind is authoritative for the workflow's pr_issue_kind var (cancel and
// phase reconciliation key on it); handle carries the full set for the retry
// tracker.
func (r *ReviewHandler) dispatchPRIssue(t task.Task, primary github.PRIssue, handle []github.PRIssue, prompt, dir string) bool {
	if r.workflowEngine == nil {
		r.logger.Error("pr-monitor.no-workflow-engine", "task_id", t.ID)
		return false
	}

	// Dispatch pr.event through the engine so trigger conditions in the
	// workflow YAML stay authoritative. StartWorkflow would bypass them.
	fullPrompt := fmt.Sprintf("# Task: %s\n\n%s%s", t.Title, prompt, prFixResultContract)
	kinds := make([]string, 0, len(handle))
	for i := range handle {
		kinds = append(kinds, string(handle[i].Kind))
	}
	vars := map[string]string{
		"prompt":                fullPrompt,
		"pr_issue_kind":         string(primary.Kind),
		"pr_issue_kinds":        strings.Join(kinds, ","),
		workflow.WorkflowVarDir: dir,
	}
	wfID, err := r.workflowEngine.DispatchEvent(t.ID, "pr.event",
		map[string]string{"pr.issue_kind": string(primary.Kind)}, vars)
	if err != nil {
		if errors.Is(err, workflow.ErrWorkflowAlreadyActive) {
			r.logger.Info("pr-monitor.workflow-already-active",
				"task_id", t.ID, "kind", string(primary.Kind))
			return false
		}
		r.logger.Error("pr-monitor.workflow-dispatch", "task_id", t.ID, "err", err)
		return false
	}
	if wfID == "" {
		r.logger.Warn("pr-monitor.no-matching-workflow",
			"task_id", t.ID, "kind", string(primary.Kind))
		return false
	}

	for i := range handle {
		r.prTracker.MarkHandled(t.ID, handle[i].Kind, handle[i].PR.HeadSHA)
	}
	r.logAudit(audit.EventPRFixAgentStarted, t.ID, "", map[string]any{
		"issue": string(primary.Kind), "kinds": strings.Join(kinds, ","),
		"pr": primary.PR.Number, "workflow": wfID,
	})

	r.logger.Info("pr-monitor.fix-started",
		"task_id", t.ID, "issue", string(primary.Kind), "kinds", strings.Join(kinds, ","),
		"pr", primary.PR.Number, "workflow", wfID,
	)
	return true
}

// recoverStaleBranchConflict turns a worktree-prep rebase failure into
// autonomous conflict resolution instead of a human escalation. The CI-fix and
// implement/review/test prepare paths rebase the task branch onto base before
// the agent starts; when the branch also conflicts with base — common when
// GitHub still reports UNKNOWN mergeability, so the monitor only emitted the CI
// failure, not a conflict — that rebase aborts. Rather than stranding a human,
// dispatch the conflict pr-fix, which checks out the PR head WITHOUT rebasing
// and has the agent resolve conflicts itself.
//
// Returns false (caller escalates to human as before) when there is no linked
// PR to fix, the PR is closed/unfetchable, or the conflict-fix retry budget is
// already spent. handlePRIssue's conflict branch prepares via PrepareForFix (no
// rebase), so this never re-enters the rebasing path that called it.
func (r *ReviewHandler) recoverStaleBranchConflict(taskID string) bool {
	if r == nil || r.workflowEngine == nil || r.prTracker == nil {
		return false
	}
	t, err := r.tasks.Get(taskID)
	if err != nil || t.PRNumber == 0 || t.ProjectID == "" {
		return false
	}
	// Don't loop forever on a genuinely unresolvable conflict — once the
	// conflict-fix budget is spent the normal exhaustion path escalates.
	if r.prTracker.AtCap(taskID, github.PRIssueConflict) {
		return false
	}
	fetchFn := github.FetchPRForMonitor
	if r.fetchKnownPRFn != nil {
		fetchFn = r.fetchKnownPRFn
	}
	pr, open, ferr := fetchFn(t.ProjectID, t.PRNumber)
	if ferr != nil || !open {
		return false
	}
	r.logger.Info("pr-monitor.rebase-block.recover-as-conflict",
		"task_id", taskID, "pr", t.PRNumber)
	if r.workflowEngine.HasActiveWorkflow(taskID) {
		if priorStep, cancelErr := r.workflowEngine.CancelWorkflow(taskID, "rebase conflict recovery"); cancelErr != nil {
			r.logger.Error("pr-monitor.rebase-block.cancel-active-workflow",
				"task_id", taskID, "err", cancelErr)
			return false
		} else {
			r.logger.Info("pr-monitor.rebase-block.cancelled-active-workflow",
				"task_id", taskID, "step", priorStep)
		}
	}
	// context.Background() is a dead end here: recoverStaleBranchConflict is
	// wired as AgentOrchestrator.conflictRecovery, a fixed func(taskID string)
	// bool callback (see app_agents.go) with no ctx parameter to thread from.
	return r.handlePRIssue(context.Background(), github.PRIssue{TaskID: taskID, Kind: github.PRIssueConflict, PR: pr})
}

// prepareWorktree sets up the fix worktree for the given task and PR issue.
// Returns ("", false) on error, with circuit-breaker escalation after wtFailureLimit
// consecutive failures. Returns ("", true) when no worktree is needed.
func (r *ReviewHandler) prepareWorktree(ctx context.Context, t task.Task, issue github.PRIssue) (string, bool) {
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
		d, wtErr = r.worktrees.PrepareForFix(ctx, t, issue.PR.Number)
	} else {
		d, wtErr = r.worktrees.PrepareForTask(ctx, t, nil)
	}
	if wtErr != nil {
		// A conflict fix already operates on the non-rebasing PrepareForFix path,
		// so a rebase failure here can only come from the CI-fix PrepareForTask
		// branch. Recover by re-routing to the conflict fix unless this already
		// IS a conflict fix (avoid re-entering ourselves).
		var recoverFn func(string) bool
		if issue.Kind != github.PRIssueConflict {
			recoverFn = r.recoverStaleBranchConflict
		}
		if markRebaseBlocked(r.tasks, t.ID, wtErr, r.logger, recoverFn) {
			return "", false
		}
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
			"End every thread reply and any PR comment you post with a blank line "+
			"then the harness attribution footer, exactly: `_Generated by Sybra "+
			"harness_`.\n\n"+
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

	return buildConflictPrompt(pr, filesCtx)
}

func buildConflictPrompt(pr github.PullRequest, filesCtx string) string {
	return fmt.Sprintf(
		"Fix merge conflicts on branch `%s` (PR #%d). "+
			"Use the task body, PR diff, changed-file list, and current code as context, then rebase.\n\n"+
			"Steps:\n"+
			"```bash\n"+
			"git fetch origin\n"+
			"git rebase refs/remotes/origin/main\n"+
			"# resolve every conflict preserving the PR intent and upstream changes\n"+
			"# run targeted tests for touched code, then git add and git rebase --continue\n"+
			"PUSH_REMOTE=origin\n"+
			"if git config --get remote.fork.url >/dev/null; then PUSH_REMOTE=fork; fi\n"+
			"git push --force-with-lease \"$PUSH_REMOTE\" HEAD:%s\n"+
			"```\n\n"+
			"Rules:\n"+
			"- Use `refs/remotes/origin/main` (not `origin/main`) to avoid ambiguous refs\n"+
			"- Push to `fork` (not `origin`) when a `fork` remote exists — the PR was opened from the fork\n"+
			"- Resolve conflicts keeping BOTH sides' intent\n"+
			"- Do not stop just because the conflict count is high — split by file and resolve all conflicts autonomously\n"+
			"- Stop only for a concrete blocker: binary conflict, missing secret/credential, deleted context you cannot reconstruct, or a semantic decision that the task/PR context does not answer\n"+
			"- No investigation, no extra commits, no unrelated changes"+
			"%s",
		pr.HeadRefName, pr.Number, pr.HeadRefName, filesCtx,
	)
}
