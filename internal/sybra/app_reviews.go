package sybra

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

const (
	prPollFast = 1 * time.Minute
	prPollSlow = 5 * time.Minute
)

const (
	reviewSmallAdditions = 40
	reviewSmallFiles     = 5
)

const transientFetchWarnThreshold = 3

// ReviewHandler manages PR review task creation, agent dispatch, and status tracking.
type ReviewHandler struct {
	DomainHandler
	tasks          *task.Manager
	projects       *project.Store
	agents         *agent.Manager
	prTracker      *github.IssueTracker
	worktrees      *worktree.Manager
	workflowEngine *workflow.Engine
	// renovatePRsFn returns Renovate-bot PRs to fold into the monitor pass.
	// FetchReviews uses author:@me which excludes bot-authored PRs, so without
	// this hook a Renovate PR linked to a task by pr_number/branch never gets
	// re-dispatched to pr-fix when CI fails. nil = renovate disabled.
	renovatePRsFn       func() []github.PullRequest
	transientFetchFails int
	// wtFailures tracks consecutive worktree-creation failures per task ID.
	// Once a task hits wtFailureLimit, it is escalated to human-required.
	wtFailures map[string]int
	// mergePR performs the actual squash-merge; overridable in tests.
	// nil falls back to github.MergePR.
	mergePR func(repo string, number int) error
	// fetchThreads / resolveThread back the Copilot-thread auto-resolver;
	// overridable in tests. nil falls back to the github package functions.
	fetchThreads  func(repo string, number int) ([]github.ReviewThread, error)
	resolveThread func(threadID string) error
}

func newReviewHandler(
	tasks *task.Manager,
	projects *project.Store,
	agents *agent.Manager,
	al *audit.Logger,
	logger *slog.Logger,
	prTracker *github.IssueTracker,
	emit func(string, any),
	worktrees *worktree.Manager,
	renovatePRsFn func() []github.PullRequest,
) *ReviewHandler {
	return &ReviewHandler{
		DomainHandler: DomainHandler{audit: al, logger: logger, emit: emit},
		tasks:         tasks,
		projects:      projects,
		agents:        agents,
		prTracker:     prTracker,
		worktrees:     worktrees,
		renovatePRsFn: renovatePRsFn,
		wtFailures:    make(map[string]int),
		mergePR:       github.MergePR,
		fetchThreads:  github.FetchReviewThreads,
		resolveThread: github.ResolveReviewThread,
	}
}

func (r *ReviewHandler) Name() string { return "reviews" }

func (r *ReviewHandler) Poll(_ context.Context) time.Duration {
	return r.pollAndMonitorPRs()
}

func (r *ReviewHandler) pollAndMonitorPRs() time.Duration {
	summary, err := github.FetchReviews()
	if err != nil {
		if github.IsTransientError(err) {
			r.transientFetchFails++
			if r.transientFetchFails < transientFetchWarnThreshold {
				r.logger.Info("pr-monitor.fetch", "err", err)
			} else {
				r.logger.Warn("pr-monitor.fetch", "err", err, "consecutive", r.transientFetchFails)
			}
		} else {
			r.transientFetchFails = 0
			r.logger.Warn("pr-monitor.fetch", "err", err)
		}
		return prPollSlow
	}
	r.transientFetchFails = 0

	r.emit("reviews:updated", summary)

	tasks, err := r.tasks.List()
	if err != nil {
		return prPollSlow
	}

	monitoredPRs := r.monitoredPRs(summary)

	// Recover PRs orphaned by a workflow that exited before linking — e.g. a
	// task stranded in human-required while a late-finishing agent opened the
	// PR (PRNumber never recorded). Re-link by branch and re-activate so the
	// normal pr-fix/auto-merge path resumes. Runs before matcher assembly so an
	// adopted task is monitored in this same poll.
	r.adoptOrphanPRs(tasks, monitoredPRs)

	var (
		matchers       []github.TaskMatcher
		closedMatchers []github.TaskMatcher
	)
	for i := range tasks {
		m := github.TaskMatcher{
			ID:        tasks[i].ID,
			PRNumber:  tasks[i].PRNumber,
			Branch:    tasks[i].Branch,
			ProjectID: tasks[i].ProjectID,
		}
		if prMonitorEligible(&tasks[i]) {
			matchers = append(matchers, m)
			closedMatchers = append(closedMatchers, m)
		} else if prClosedEligible(&tasks[i]) {
			// human-required tasks are excluded from pr-fix dispatch but still
			// advance to done when their PR is merged.
			closedMatchers = append(closedMatchers, m)
		}
	}

	if len(matchers) > 0 {
		issues := github.MatchTaskPRs(monitoredPRs, matchers)
		r.prTracker.Cleanup()
		r.cancelResolvedPRFixWorkflows(tasks, issues)

		for i := range issues {
			if r.agents.HasRunningAgentForTask(issues[i].TaskID) {
				continue
			}
			// Gate dispatch on workflow state too: an agent may have just
			// exited while the workflow is still in verify_commits /
			// link_pr_and_review. Without this, a fresh issue (e.g.
			// kind=conflict appearing because main moved during the agent
			// run) races the in-flight workflow's tail steps and triggers
			// a layered re-dispatch that DispatchEvent later rejects, but
			// only after we've prepped a worktree and emitted audit
			// noise.
			if r.workflowEngine != nil && r.workflowEngine.HasActiveWorkflow(issues[i].TaskID) {
				continue
			}
			if !r.prTracker.ShouldHandle(issues[i].TaskID, issues[i].Kind, issues[i].PR.HeadSHA) {
				continue
			}
			if issues[i].Kind == github.PRIssueReadyToMerge {
				r.handleAutoMerge(issues[i])
				continue
			}
			r.handlePRIssue(issues[i])
		}
	}

	if len(closedMatchers) > 0 {
		r.advanceClosedTaskPRs(monitoredPRs, closedMatchers)
	}

	r.resolveAddressedCopilotThreads(tasks, monitoredPRs)
	r.maybeCreateReviewTasks(tasks, summary.ReviewRequested)
	r.reconcileReviewPhases(tasks, summary)
	r.reconcilePRPhases(tasks, monitoredPRs)
	r.closeFinishedReviewTasks(tasks, openReviewPRs(summary))

	if prNeedsAttention(monitoredPRs) {
		return prPollFast
	}
	return prPollSlow
}

// advanceClosedTaskPRs moves tasks whose linked PR is no longer open to done,
// stamping the terminal outcome and emitting the audit + task.landed events the
// evaluation scorecard reads. Skips tasks with a still-running agent.
func (r *ReviewHandler) advanceClosedTaskPRs(monitoredPRs []github.PullRequest, closedMatchers []github.TaskMatcher) {
	closedPRs := github.DetectClosedTaskPRs(monitoredPRs, closedMatchers, github.FetchPRState)
	for _, c := range closedPRs {
		if r.agents.HasRunningAgentForTask(c.TaskID) {
			r.logger.Info("pr-monitor.closed-skip-running-agent", "task_id", c.TaskID, "pr", c.PRNumber)
			continue
		}
		outcome := classifyLandingOutcome(c.State)
		if _, err := r.tasks.Update(c.TaskID, task.Update{
			Status:  task.Ptr(task.StatusDone),
			Outcome: task.Ptr(outcome),
		}); err != nil {
			r.logger.Error("pr-monitor.closed-update", "task_id", c.TaskID, "err", err)
			continue
		}
		eventType := audit.EventPRMerged
		if c.State == "CLOSED" {
			eventType = audit.EventPRClosed
		}
		r.logAudit(eventType, c.TaskID, "", map[string]any{"pr": c.PRNumber, "state": c.State})
		r.recordLanding(c.TaskID, c.PRNumber, c.State)
		r.logger.Info("pr-monitor.auto-done", "task_id", c.TaskID, "pr", c.PRNumber, "state", c.State, "outcome", outcome)
	}
}

// classifyLandingOutcome maps a terminal PR state to a task outcome label.
// Explicit "CLOSED" (closed unmerged) → "closed"; everything else
// ("MERGED" and the eligible default) → "merged".
func classifyLandingOutcome(state string) string {
	if state == "CLOSED" {
		return "closed"
	}
	return "merged"
}

// recordLanding emits a task.landed audit event capturing the terminal outcome
// and timing — the ground-truth signal the evaluation scorecard reads. Kept
// fully local (no network) so it never stalls the PR poll loop. Two timings are
// recorded distinctly: created_to_land_h is queue-inclusive (task filed → land),
// work_to_land_h starts from the first agent run (closer to DORA cycle time).
func (r *ReviewHandler) recordLanding(taskID string, prNumber int, state string) {
	data := map[string]any{
		"pr":      prNumber,
		"state":   state,
		"outcome": classifyLandingOutcome(state),
	}
	if t, err := r.tasks.Get(taskID); err == nil {
		if !t.CreatedAt.IsZero() {
			data["created_to_land_h"] = time.Since(t.CreatedAt).Hours()
		}
		if started := earliestRunStart(t.AgentRuns); !started.IsZero() {
			data["work_to_land_h"] = time.Since(started).Hours()
		}
	}
	r.logAudit(audit.EventTaskLanded, taskID, "", data)
}

// earliestRunStart returns the start time of the first agent run, or the zero
// time when there are no runs with a start timestamp.
func earliestRunStart(runs []task.AgentRun) time.Time {
	var earliest time.Time
	for i := range runs {
		s := runs[i].StartedAt
		if s.IsZero() {
			continue
		}
		if earliest.IsZero() || s.Before(earliest) {
			earliest = s
		}
	}
	return earliest
}

// cancelResolvedPRFixWorkflows terminates any in-flight pr-fix workflow
// whose originating PR issue (`pr_issue_kind` in workflow vars) is no
// longer present on the live PR. Prevents ResumeStalled from re-spawning
// fix agents forever when the underlying CI failure or conflict has
// since been resolved on a newer push.
//
// Without this, a pr-fix workflow remains in state=waiting on the `fix`
// step until its agent succeeds or the task is deleted — there is no
// trigger-re-evaluation between dispatch and completion. The orchestrator
// loop then re-dispatches the step every minute, spawning a fresh agent
// each time even though the PR is now green.
func (r *ReviewHandler) cancelResolvedPRFixWorkflows(tasks []task.Task, issues []github.PRIssue) {
	if r.workflowEngine == nil {
		return
	}
	// Index live issues per task so we can answer "kind K still present
	// for task T?" in O(1).
	liveByTask := make(map[string]map[string]bool, len(tasks))
	for i := range issues {
		set := liveByTask[issues[i].TaskID]
		if set == nil {
			set = make(map[string]bool, 2)
			liveByTask[issues[i].TaskID] = set
		}
		set[string(issues[i].Kind)] = true
	}

	for i := range tasks {
		t := &tasks[i]
		if t.Workflow == nil || t.Workflow.WorkflowID != "pr-fix" {
			continue
		}
		if t.Workflow.State == workflow.ExecCompleted || t.Workflow.State == workflow.ExecFailed {
			continue
		}
		kind := t.Workflow.Variables["pr_issue_kind"]
		if kind == "" {
			continue
		}
		if liveByTask[t.ID][kind] {
			continue // condition still holds — let the workflow proceed
		}
		step, err := r.workflowEngine.CancelWorkflow(t.ID, "pr-monitor: "+kind+" resolved")
		if err != nil {
			r.logger.Error("pr-monitor.cancel-resolved", "task_id", t.ID, "kind", kind, "err", err)
			continue
		}
		// Clear cooldown so a future failure of the same kind on a new
		// SHA re-triggers fresh (the closed-PR path does the same via
		// prTracker.Cleanup; we need the explicit clear here because
		// the PR is still open).
		r.prTracker.Clear(t.ID, github.PRIssueKind(kind))
		r.logger.Info("pr-monitor.cancel-resolved",
			"task_id", t.ID, "kind", kind, "step", step, "pr", t.PRNumber)
	}
}

// monitoredPRs returns the union of user-authored PRs (from FetchReviews) and
// Renovate-bot PRs (from renovatePRsFn). Renovate's PRs aren't in author:@me,
// so without folding them in here the pr-fix monitor would never re-spawn an
// agent on a Renovate PR whose CI keeps failing.
func (r *ReviewHandler) monitoredPRs(summary github.ReviewSummary) []github.PullRequest {
	if r.renovatePRsFn == nil {
		return summary.CreatedByMe
	}
	renovatePRs := r.renovatePRsFn()
	if len(renovatePRs) == 0 {
		return summary.CreatedByMe
	}
	prs := make([]github.PullRequest, 0, len(summary.CreatedByMe)+len(renovatePRs))
	prs = append(prs, summary.CreatedByMe...)
	prs = append(prs, renovatePRs...)
	return prs
}

// prMonitorEligible decides whether the PR monitor should consider a task
// when scanning for CI failures, conflicts, and ready-to-merge state.
//
// Historical behavior was "in-review only" — which silently stranded tasks
// whose workflow exited to `in-progress` with a live PR (e.g. an evaluate
// step that crashed before flipping to in-review, or a manually-spawned
// agent that opened a PR outside of any workflow). Those tasks would render
// a red ✗ in the kanban UI forever and never get picked up for pr-fix.
//
// Now we also include in-progress tasks that carry an explicit PR number.
// Branch-only matching stays gated on in-review to avoid false positives
// from tasks that pushed a WIP branch without opening a PR yet.
func prMonitorEligible(t *task.Task) bool {
	if t.TaskType == task.TaskTypeChat {
		// Chat tasks are ephemeral and never have PRs — exclude from PR monitoring.
		return false
	}
	if slices.Contains(t.Tags, "review") {
		// Review tasks are inbound (reviewing someone else's PR), not tasks
		// whose own PR is being tracked. They're handled separately.
		return false
	}
	switch t.Status {
	case task.StatusInReview:
		return t.PRNumber != 0 || t.Branch != ""
	case task.StatusInProgress:
		// Only in-progress tasks that already have a PR — a branch alone
		// isn't enough, we don't want to treat mid-implementation tasks
		// as candidates for pr-fix dispatch.
		return t.PRNumber != 0
	default:
		return false
	}
}

// prClosedEligible is a superset of prMonitorEligible: it additionally includes
// human-required tasks that carry a PR number. Those tasks are excluded from
// pr-fix dispatch and auto-merge (they need operator attention) but should
// still advance to done when their PR is merged or closed.
func prClosedEligible(t *task.Task) bool {
	if prMonitorEligible(t) {
		return true
	}
	if t.TaskType == task.TaskTypeChat || slices.Contains(t.Tags, "review") {
		return false
	}
	return t.Status == task.StatusHumanRequired && t.PRNumber != 0
}

// orphanStrandReasons are the status_reason fragments the implement / verify /
// evaluate workflow steps write when they park a task without linking a PR.
// Adoption is gated to these (FAIL CLOSED): a task parked for any other reason
// — notably a deliberate watchdog stop ("watchdog: …") or a dwell escalation —
// must never be auto-resurrected, even if a PR happens to match its branch.
// Keep in sync with internal/workflow/engine_steps_verify.go (no-commits
// verdicts) and engine_steps_link.go (evaluate).
var orphanStrandReasons = []string{
	"no commits",               // verify_commits: empty branch / agent crashed before commit
	"commits pushed but no PR", // evaluate: commits exist but the PR was never linked
}

func hasOrphanStrandReason(reason string) bool {
	for _, frag := range orphanStrandReasons {
		if strings.Contains(reason, frag) {
			return true
		}
	}
	return false
}

// orphanPRAdoptionEligible reports whether a task is a candidate for orphan-PR
// adoption: parked in human-required with a branch but no linked PR, *and* with
// a status_reason that marks it as stranded by the implement/verify/evaluate
// path before a late-finishing agent opened the PR. The reason gate is what
// keeps adoption from resurrecting a task a human or the watchdog deliberately
// stopped. Chat tasks and inbound review tasks are never own-PR tasks.
func orphanPRAdoptionEligible(t *task.Task) bool {
	if t.TaskType == task.TaskTypeChat || slices.Contains(t.Tags, "review") {
		return false
	}
	return t.Status == task.StatusHumanRequired &&
		t.PRNumber == 0 &&
		t.Branch != "" &&
		t.ProjectID != "" &&
		hasOrphanStrandReason(t.StatusReason)
}

// adoptOrphanPRs re-links tasks stranded in human-required without a PR number
// to a matching open PR discovered by head branch *within the task's own
// project*, then flips them to in-review so the monitor's normal pr-fix/
// auto-merge path resumes. Re-activation is non-destructive: a pet PR still
// passes through the full auto-merge gate (Copilot review + green CI), and a
// work PR is merged by a human. The repo guard (prs[j].Repository ==
// t.ProjectID) is essential: monitoredPRs spans every repo the user authors PRs
// in, so a same-named branch in another repo must not be linked. A branch
// matching more than one open PR in the project is left untouched (ambiguous).
// Matched entries of `tasks` are mutated in place so the caller's matcher
// assembly observes the new state in the same poll.
func (r *ReviewHandler) adoptOrphanPRs(tasks []task.Task, prs []github.PullRequest) {
	for i := range tasks {
		t := &tasks[i]
		if !orphanPRAdoptionEligible(t) {
			continue
		}
		var match *github.PullRequest
		ambiguous := false
		for j := range prs {
			if prs[j].HeadRefName != t.Branch || prs[j].Repository != t.ProjectID {
				continue
			}
			if match != nil {
				ambiguous = true
				break
			}
			match = &prs[j]
		}
		if ambiguous || match == nil {
			continue
		}
		updated, err := r.tasks.Update(t.ID, task.Update{
			PRNumber:     task.Ptr(match.Number),
			Status:       task.Ptr(task.StatusInReview),
			StatusReason: task.Ptr(""),
		})
		if err != nil {
			r.logger.Error("pr-monitor.orphan-adopt", "task_id", t.ID, "pr", match.Number, "err", err)
			continue
		}
		tasks[i] = updated
		r.logAudit(audit.EventPROrphanAdopted, t.ID, "", map[string]any{
			"pr": match.Number, "repo": match.Repository, "branch": t.Branch,
		})
		r.logger.Info("pr-monitor.orphan-adopted",
			"task_id", t.ID, "pr", match.Number, "branch", t.Branch)
	}
}

func prNeedsAttention(prs []github.PullRequest) bool {
	for i := range prs {
		if prs[i].CIStatus == "PENDING" || prs[i].CIStatus == "FAILURE" {
			return true
		}
		if prs[i].Mergeable == "CONFLICTING" || prs[i].Mergeable == "UNKNOWN" {
			return true
		}
		// Review comments just landed — poll fast so the fix agent dispatches
		// and the In Review card flips to "fixing" without a 5-minute lag.
		if !prs[i].IsDraft && (prs[i].ReviewDecision == "CHANGES_REQUESTED" || prs[i].UnresolvedCount > 0) {
			return true
		}
		if !prs[i].IsDraft && prs[i].Mergeable == "MERGEABLE" && (prs[i].CIStatus == "SUCCESS" || prs[i].CIStatus == "") {
			return true
		}
	}
	return false
}
