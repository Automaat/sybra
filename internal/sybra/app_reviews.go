package sybra

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/experience"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/scrub"
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
	cfg            *config.Config
	experience     *experience.Store
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
	// fetchPRStateFn is used by closeFinishedReviewTasks to check whether a
	// review-task's PR is still open. Overridable in tests; nil falls back to
	// github.FetchPRState.
	fetchPRStateFn func(repo string, number int) (github.PRState, error)
	// fetchReviewsFn fetches the PR review summary. Overridable in tests; nil
	// falls back to github.FetchReviews.
	fetchReviewsFn func() (github.ReviewSummary, error)
	// viewerLoginFn returns the authenticated GitHub login (the identity the fix
	// agent posts as), used to tell the agent's own thread replies from a human
	// collaborator's. Overridable in tests; nil falls back to github.ViewerLogin.
	viewerLoginFn func() string
	// findMergedPRFn looks up a merged PR by head branch in a project repo.
	// Returns the PR number (0 = none or ambiguous). nil falls back to gh-based implementation.
	// Overridable in tests.
	findMergedPRFn func(repo, branch string) (int, error)
	// lastRevertScan rate-limits the default-branch revert scan (revertScanInterval).
	lastRevertScan time.Time
}

// agentLogin returns the GitHub login the fix agent posts as.
func (r *ReviewHandler) agentLogin() string {
	if r.viewerLoginFn != nil {
		return r.viewerLoginFn()
	}
	return github.ViewerLogin()
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
	cfg *config.Config,
	experienceStore *experience.Store,
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
		cfg:           cfg,
		experience:    experienceStore,
	}
}

func (r *ReviewHandler) Name() string { return "reviews" }

func (r *ReviewHandler) Poll(_ context.Context) time.Duration {
	return r.pollAndMonitorPRs()
}

func (r *ReviewHandler) pollAndMonitorPRs() time.Duration {
	// Load tasks up-front so stale review-task reconciliation runs even
	// when FetchReviews fails (transient errors, rate limits, etc.).
	tasks, err := r.tasks.List()
	if err != nil {
		return prPollSlow
	}

	fetchFn := github.FetchReviews
	if r.fetchReviewsFn != nil {
		fetchFn = r.fetchReviewsFn
	}
	summary, fetchErr := fetchFn()
	if fetchErr != nil {
		if github.IsTransientError(fetchErr) {
			r.transientFetchFails++
			if r.transientFetchFails < transientFetchWarnThreshold {
				r.logger.Info("pr-monitor.fetch", "err", fetchErr)
			} else {
				r.logger.Warn("pr-monitor.fetch", "err", fetchErr, "consecutive", r.transientFetchFails)
			}
			// Reconcile stale review tasks on transient failures only. Non-transient
			// errors (auth, 4xx) mean the per-task FetchPRState calls will also fail
			// or be wasted, compounding backoff under an already-throttled API.
			r.closeFinishedReviewTasks(tasks, nil)
		} else {
			r.transientFetchFails = 0
			r.logger.Warn("pr-monitor.fetch", "err", fetchErr)
		}
		return prPollSlow
	}
	r.transientFetchFails = 0

	r.emit("reviews:updated", summary)

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
			// Only the comments kind carries a feedback fingerprint; conflict,
			// ci_failure, and ready_to_merge fall back to SHA-only gating.
			var sig string
			if issues[i].Kind == github.PRIssueComments {
				sig = issues[i].PR.FeedbackSig
			}
			switch r.prTracker.Decide(issues[i].TaskID, issues[i].Kind, issues[i].PR.HeadSHA, sig) {
			case github.DispatchHandle:
				if issues[i].Kind == github.PRIssueReadyToMerge {
					r.handleAutoMerge(issues[i])
					continue
				}
				r.handlePRIssue(issues[i])
			case github.DispatchExhausted:
				r.escalateExhaustedFix(issues[i])
			case github.DispatchSkip:
			}
		}
	}

	if len(closedMatchers) > 0 {
		r.advanceClosedTaskPRs(monitoredPRs, closedMatchers)
	}

	r.scanForReverts(tasks)
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
		// Flip to done immediately with the base outcome — the status transition
		// must never wait on GitHub enrichment.
		base := classifyLandingOutcome(c.State)
		if _, err := r.tasks.Update(c.TaskID, task.Update{
			Status:  task.Ptr(task.StatusDone),
			Outcome: task.Ptr(base),
		}); err != nil {
			r.logger.Error("pr-monitor.closed-update", "task_id", c.TaskID, "err", err)
			continue
		}
		eventType := audit.EventPRMerged
		if c.State == "CLOSED" {
			eventType = audit.EventPRClosed
		}
		r.logAudit(eventType, c.TaskID, "", map[string]any{"pr": c.PRNumber, "state": c.State})
		// Enrich (bounded GitHub I/O); refine the outcome if a human edited the PR
		// and persist the merge commit for later revert detection.
		outcome, landData := r.computeLanding(c.TaskID, c.PRNumber, c.State, base)
		upd := task.Update{}
		refine := false
		if outcome != base {
			upd.Outcome = task.Ptr(outcome)
			refine = true
		}
		if mc, ok := landData["merge_commit"].(string); ok && mc != "" {
			upd.MergeCommit = task.Ptr(mc)
			refine = true
		}
		if refine {
			if _, err := r.tasks.Update(c.TaskID, upd); err != nil {
				r.logger.Warn("pr-monitor.outcome-refine", "task_id", c.TaskID, "err", err)
			}
		}
		if t, err := r.tasks.Get(c.TaskID); err == nil {
			r.recordExperienceOnLanding(t)
		}
		r.logAudit(audit.EventTaskLanded, c.TaskID, "", landData)
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

// landingEnrichTimeout bounds the GitHub enrichment so a slow gh never stalls
// the PR poll loop.
const landingEnrichTimeout = 20 * time.Second

// computeLanding builds the refined outcome and the task.landed data for a closed
// PR, starting from the base outcome already recorded. Local timing (created/work
// → land) and the agent's last pushed SHA are always added; for a merge it adds
// PR size and human-edit signals via GitHub, bounded so the poll can't hang.
func (r *ReviewHandler) computeLanding(taskID string, prNumber int, state, base string) (outcome string, data map[string]any) {
	outcome = base
	data = map[string]any{"pr": prNumber, "state": state}
	t, err := r.tasks.Get(taskID)
	if err != nil {
		data["outcome"] = outcome
		return outcome, data
	}
	if !t.CreatedAt.IsZero() {
		data["created_to_land_h"] = time.Since(t.CreatedAt).Hours()
	}
	if started := earliestRunStart(t.AgentRuns); !started.IsZero() {
		data["work_to_land_h"] = time.Since(started).Hours()
	}
	agentSHA := lastAgentHeadSHA(t.AgentRuns)
	if agentSHA != "" {
		data["agent_head_sha"] = agentSHA
	}
	if t.ProjectID != "" && prNumber > 0 {
		enr := r.enrichLanding(t.ProjectID, prNumber, agentSHA, base)
		outcome = enr.outcome
		maps.Copy(data, enr.data)
	}
	data["outcome"] = outcome
	return outcome, data
}

type landingEnrich struct {
	outcome string
	data    map[string]any
}

// enrichLanding records PR size for any landing and, for a merge, detects human
// edits made after the agent's last push. All gh calls run under a single
// context deadline so a stalled call is killed (releasing the global gh gate)
// rather than blocking the poll. A merge is classified merged_with_edits only
// when the compare is strictly ahead of the agent's SHA (status=="ahead",
// commits > 0) — so a rebase/force-push (diverged) or unpushed-local divergence
// doesn't produce a false positive.
func (r *ReviewHandler) enrichLanding(repo string, prNumber int, agentSHA, base string) landingEnrich {
	ctx, cancel := context.WithTimeout(context.Background(), landingEnrichTimeout)
	defer cancel()

	enr := landingEnrich{outcome: base, data: map[string]any{}}
	if s, err := github.FetchPRStatsContext(ctx, repo, prNumber); err == nil {
		enr.data["additions"] = s.Additions
		enr.data["deletions"] = s.Deletions
		enr.data["changed_files"] = s.ChangedFiles
	}
	if base != "merged" {
		return enr // closed (unmerged): size only, no edit detection
	}
	// Record the merge commit so the revert scanner can later detect a revert.
	if mc, err := github.FetchPRMergeCommitContext(ctx, repo, prNumber); err == nil && mc != "" {
		enr.data["merge_commit"] = mc
	}
	head, err := github.FetchPRHeadSHAContext(ctx, repo, prNumber)
	if err != nil {
		return enr // couldn't read the merged head; keep base outcome + size
	}
	if landingOutcome("MERGED", agentSHA, head) != "merged_with_edits" {
		return enr
	}
	if cmp, err := github.FetchPRCompare(ctx, repo, agentSHA, head); err == nil && cmp.Status == "ahead" && cmp.Commits > 0 {
		enr.outcome = "merged_with_edits"
		enr.data["human_edit_lines"] = cmp.Additions + cmp.Deletions
		enr.data["human_edit_commits"] = cmp.Commits
	}
	return enr
}

// landingOutcome is the cheap pre-check: a merge whose head moved past the
// agent's last push *may* have human edits (confirmed by enrichLanding's
// compare). CLOSED is terminal; an unknown/missing SHA stays "merged".
func landingOutcome(state, agentSHA, mergedHeadSHA string) string {
	if state == "CLOSED" {
		return "closed"
	}
	if agentSHA != "" && mergedHeadSHA != "" && agentSHA != mergedHeadSHA {
		return "merged_with_edits"
	}
	return "merged"
}

func (r *ReviewHandler) recordExperienceOnLanding(t task.Task) {
	defer func() {
		if rec := recover(); rec != nil {
			r.logger.Warn("experience.record.panic", "task_id", t.ID, "panic", rec)
		}
	}()
	if r == nil || r.experience == nil || r.cfg == nil || !r.cfg.Experience.Enabled {
		return
	}
	if t.ProjectID == "" || (t.Outcome != "merged" && t.Outcome != "merged_with_edits") {
		return
	}
	proj, err := r.projects.Get(t.ProjectID)
	if err != nil || proj.ID == "" {
		r.logAudit(audit.EventExperienceSkipped, t.ID, "", map[string]any{
			"reason": "project_unresolved",
		})
		return
	}
	if !r.cfg.AllowsProjectType(string(proj.Type)) {
		return
	}

	rec := experience.FromTask(t, proj)
	projectKey := experience.ProjectKey(proj)
	if proj.Type == project.ProjectTypeWork {
		scrubExperienceRecord(&rec, experienceBlocklist(proj))
	}
	if err := r.experience.Put(projectKey, rec); err != nil {
		r.logAudit(audit.EventExperienceSkipped, t.ID, "", map[string]any{
			"reason": "write_failed",
		})
		r.logger.Warn("experience.record.write", "task_id", t.ID, "err", err)
		return
	}
	data := map[string]any{"record_id": rec.TaskID}
	if proj.Type == project.ProjectTypeWork {
		data["project_key"] = projectKey
	} else {
		data["project_id"] = t.ProjectID
	}
	r.logAudit(audit.EventExperienceRecorded, t.ID, "", data)
}

func experienceBlocklist(proj project.Project) []string {
	blocklist := []string{proj.ID, proj.Owner, proj.Repo}
	if proj.URL != "" {
		blocklist = append(blocklist, proj.URL)
	}
	return blocklist
}

func scrubExperienceRecord(rec *experience.Record, blocklist []string) {
	rawTaskID := rec.TaskID
	if scrubbed, redactions := scrub.Scrub(rec.TaskID, blocklist); redactions > 0 {
		rec.TaskID = experience.WorkRecordID(rawTaskID)
	} else {
		rec.TaskID = scrubbed
	}
	rec.ProjectID, _ = scrub.Scrub(rec.ProjectID, blocklist)
	rec.ProjectType, _ = scrub.Scrub(rec.ProjectType, blocklist)
	rec.Title, _ = scrub.Scrub(rec.Title, blocklist)
	rec.Size, _ = scrub.Scrub(rec.Size, blocklist)
	rec.Type, _ = scrub.Scrub(rec.Type, blocklist)
	rec.AgentMode, _ = scrub.Scrub(rec.AgentMode, blocklist)
	rec.Provider, _ = scrub.Scrub(rec.Provider, blocklist)
	rec.Outcome, _ = scrub.Scrub(rec.Outcome, blocklist)
	rec.Strategy, _ = scrub.Scrub(rec.Strategy, blocklist)
	rec.Caution, _ = scrub.Scrub(rec.Caution, blocklist)
	for i := range rec.Tags {
		rec.Tags[i], _ = scrub.Scrub(rec.Tags[i], blocklist)
	}
	for i := range rec.FailureModes {
		rec.FailureModes[i], _ = scrub.Scrub(rec.FailureModes[i], blocklist)
	}
	for i := range rec.VerifyCommands {
		rec.VerifyCommands[i], _ = scrub.Scrub(rec.VerifyCommands[i], blocklist)
	}
}

// lastAgentHeadSHA returns the HeadSHA of the most recent agent run that recorded
// one, or "" — the commit the fleet last left on the branch.
func lastAgentHeadSHA(runs []task.AgentRun) string {
	for i := range slices.Backward(runs) {
		if runs[i].HeadSHA != "" {
			return runs[i].HeadSHA
		}
	}
	return ""
}

const (
	// revertScanInterval rate-limits the revert scan so it doesn't run every poll.
	revertScanInterval = 30 * time.Minute
	// revertScanMaxAge bounds how far back a merged task is still checked for reverts.
	revertScanMaxAge = 30 * 24 * time.Hour
	// revertScanCommits is how many recent default-branch commits to fetch per repo.
	revertScanCommits = 100
)

// scanForReverts detects when a landed task's merge commit was later reverted on
// the default branch, flips the task outcome to "reverted", and emits
// pr.reverted (the change-failure signal). Rate-limited and bounded: one gh call
// per repo with eligible tasks, every revertScanInterval, with each call killed
// after landingEnrichTimeout. Reuses the already-listed tasks (no extra read).
func (r *ReviewHandler) scanForReverts(tasks []task.Task) {
	now := time.Now()
	if !r.lastRevertScan.IsZero() && now.Sub(r.lastRevertScan) < revertScanInterval {
		return
	}

	byRepo := map[string][]task.Task{}
	for i := range tasks {
		t := tasks[i]
		// A PR number is enough to detect a revert (by the "Reverts #n" form), so
		// a task whose merge-commit capture failed at landing is still checked.
		if t.ProjectID == "" || t.PRNumber == 0 {
			continue
		}
		if t.Outcome != "merged" && t.Outcome != "merged_with_edits" {
			// Skip closed / already-reverted. A reverted task stays reverted even
			// if the revert is later re-applied — the change-failure still occurred.
			continue
		}
		if t.ClosedAt != nil && now.Sub(*t.ClosedAt) > revertScanMaxAge {
			continue
		}
		byRepo[t.ProjectID] = append(byRepo[t.ProjectID], t)
	}
	if len(byRepo) == 0 {
		r.lastRevertScan = now // nothing to check; hold off until the next interval
		return
	}

	scannedAny := false
	for repo, cands := range byRepo {
		ctx, cancel := context.WithTimeout(context.Background(), landingEnrichTimeout)
		msgs, err := github.FetchRecentCommitMessages(ctx, repo, revertScanCommits)
		cancel()
		if err != nil {
			r.logger.Warn("pr-monitor.revert-scan", "repo", repo, "err", err)
			continue
		}
		scannedAny = true
		for j := range cands {
			t := &cands[j]
			if !isReverted(t.MergeCommit, t.ProjectID, t.PRNumber, msgs) {
				continue
			}
			if _, err := r.tasks.Update(t.ID, task.Update{Outcome: task.Ptr("reverted")}); err != nil {
				r.logger.Warn("pr-monitor.revert-update", "task_id", t.ID, "err", err)
				continue
			}
			r.logAudit(audit.EventPRReverted, t.ID, "", map[string]any{"pr": t.PRNumber, "merge_commit": t.MergeCommit})
			r.logger.Info("pr-monitor.reverted", "task_id", t.ID, "pr", t.PRNumber, "merge_commit", t.MergeCommit)
		}
	}
	// Only start the rate-limit clock once at least one repo was actually scanned,
	// so a transient gh outage retries on the next poll instead of waiting 30m.
	if scannedAny {
		r.lastRevertScan = now
	}
}

// isReverted reports whether any default-branch commit message reverts the task.
// It matches two GitHub revert forms so it works regardless of squash config:
//   - the git footer "This reverts commit <full-sha>" (default COMMIT_MESSAGES)
//   - the auto-generated body "Reverts #<n>" / "Reverts owner/repo#<n>" (which
//     is what survives when the squash commit message is PR_BODY, as this repo's)
//
// Both anchor on the original PR's full SHA or number, so a passing mention of
// the PR isn't a false positive.
func isReverted(mergeCommit, repo string, prNumber int, commitMessages []string) bool {
	var needles []string
	if mergeCommit != "" {
		needles = append(needles, "This reverts commit "+mergeCommit)
	}
	if prNumber > 0 {
		needles = append(needles, fmt.Sprintf("Reverts #%d", prNumber))
		if repo != "" {
			needles = append(needles, fmt.Sprintf("Reverts %s#%d", repo, prNumber))
		}
	}
	if len(needles) == 0 {
		return false
	}
	for _, m := range commitMessages {
		for _, n := range needles {
			if strings.Contains(m, n) {
				return true
			}
		}
	}
	return false
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
		status := task.StatusInReview
		reason := "pr-fix cancelled: " + kind + " resolved"
		if _, updErr := r.tasks.Update(t.ID, task.Update{
			Status:       &status,
			StatusReason: &reason,
		}); updErr != nil {
			r.logger.Error("pr-monitor.cancel-resolved.status", "task_id", t.ID, "kind", kind, "err", updErr)
			continue
		}
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
// adoption: a task with a branch but no linked PR that was either placed in
// in-review without a PR being recorded, or stranded in human-required by the
// implement/verify/evaluate path before a late-finishing agent opened the PR.
// For human-required tasks the strand-reason gate prevents resurrecting a task
// that a human or the watchdog deliberately stopped. For in-review tasks no
// reason gate is needed — a task already in review with no PR is unambiguously
// an orphan regardless of how it got there. Chat tasks and inbound review tasks
// are never own-PR tasks.
func orphanPRAdoptionEligible(t *task.Task) bool {
	if t.TaskType == task.TaskTypeChat || slices.Contains(t.Tags, "review") {
		return false
	}
	if t.PRNumber != 0 || t.Branch == "" || t.ProjectID == "" {
		return false
	}
	// In-review tasks with no linked PR are always eligible: they've been placed
	// in review state but the PR number was never recorded (e.g. PR opened
	// manually without going through simple-task-pr).
	if t.Status == task.StatusInReview {
		return true
	}
	// Human-required tasks: only adopt if stranded by implement/verify/evaluate
	// (never resurrect deliberate watchdog stops or dwell escalations).
	return t.Status == task.StatusHumanRequired && hasOrphanStrandReason(t.StatusReason)
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
//
// When no open PR is found, falls through to adoptOrphanMergedPR to handle the
// race where the PR was opened and merged between two poll cycles.
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
		if ambiguous {
			continue
		}
		if match != nil {
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
			continue
		}
		// No open PR found. Check for a recently merged PR: handles the race
		// where the PR was opened and merged between poll cycles.
		r.adoptOrphanMergedPR(t)
	}
}

// adoptOrphanMergedPR checks whether a merged PR exists for an orphan task's
// branch and, if exactly one is found, links it and advances the task to done.
// Handles the race where a manually-opened PR is merged before the poll loop
// finds it as an open PR (so adoptOrphanPRs never had a chance to link it).
func (r *ReviewHandler) adoptOrphanMergedPR(t *task.Task) {
	fn := r.findMergedPRFn
	if fn == nil {
		fn = findMergedPRByBranch
	}
	prNum, err := fn(t.ProjectID, t.Branch)
	if err != nil {
		r.logger.Warn("pr-monitor.orphan-merged-check", "task_id", t.ID, "err", err)
		return
	}
	if prNum == 0 {
		return
	}
	taskID, repo, branch := t.ID, t.ProjectID, t.Branch
	const state = "MERGED"
	base := classifyLandingOutcome(state)
	updated, err := r.tasks.Update(taskID, task.Update{
		PRNumber:     task.Ptr(prNum),
		Status:       task.Ptr(task.StatusDone),
		Outcome:      task.Ptr(base),
		StatusReason: task.Ptr(""),
	})
	if err != nil {
		r.logger.Error("pr-monitor.orphan-merged-adopt", "task_id", taskID, "pr", prNum, "err", err)
		return
	}
	*t = updated
	r.logAudit(audit.EventPROrphanAdopted, taskID, "", map[string]any{
		"pr": prNum, "repo": repo, "branch": branch, "state": state,
	})
	r.logAudit(audit.EventPRMerged, taskID, "", map[string]any{"pr": prNum, "state": state})
	// Enrich with PR size, timing, human-edit data, and merge_commit for revert
	// detection — same side effects as the normal advanceClosedTaskPRs path.
	outcome, landData := r.computeLanding(taskID, prNum, state, base)
	upd := task.Update{}
	refine := false
	if outcome != base {
		upd.Outcome = task.Ptr(outcome)
		refine = true
	}
	if mc, ok := landData["merge_commit"].(string); ok && mc != "" {
		upd.MergeCommit = task.Ptr(mc)
		refine = true
	}
	if refine {
		if _, err := r.tasks.Update(taskID, upd); err != nil {
			r.logger.Warn("pr-monitor.orphan-merged-refine", "task_id", taskID, "err", err)
		}
	}
	if current, err := r.tasks.Get(taskID); err == nil {
		r.recordExperienceOnLanding(current)
	}
	r.logAudit(audit.EventTaskLanded, taskID, "", landData)
	r.logger.Info("pr-monitor.orphan-merged-adopted",
		"task_id", taskID, "pr", prNum, "branch", branch)
}

// findMergedPRByBranch queries GitHub for a merged PR matching the given head
// branch in the repository. Returns the PR number, or 0 if none or ambiguous.
func findMergedPRByBranch(repo, branch string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "list",
		"--repo", repo, "--head", branch, "--state", "merged", "--json", "number", "--limit", "2")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("%w: %s", err, out)
	}
	var prs []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return 0, err
	}
	if len(prs) != 1 {
		return 0, nil
	}
	return prs[0].Number, nil
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
