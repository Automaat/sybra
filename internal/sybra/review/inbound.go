package review

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/sybra/agentorch"
	"github.com/Automaat/sybra/internal/task"
)

func (r *Handler) createReviewTask(pr github.PullRequest, projectID string) {
	r.createReviewTaskWithTriage(pr, projectID, r.triageReview)
}

func (r *Handler) createReviewTaskWithTriage(pr github.PullRequest, projectID string, triage func(task.Task)) {
	title := "Review: " + pr.Title
	body := fmt.Sprintf("%s\n\nAuthor: @%s", pr.URL, pr.Author)

	// Use CreateFull so the review tag, projectID, and PRNumber are visible to
	// file-watchers from the very first write. A two-step Create + Update leaves
	// a window where the initial file has no "review" tag, which lets
	// simple-task-plan claim the task.created workflow slot before pr-review
	// can match — causing triage loops and incorrect status transitions.
	tags := []string{"review"}
	u := task.Update{
		Tags:      &tags,
		ProjectID: task.Ptr(projectID),
		PRNumber:  task.Ptr(pr.Number),
	}
	if projectHeadRepoMatches(projectID, pr.HeadRepo) && pr.HeadRefName != "" {
		u.Branch = task.Ptr(pr.HeadRefName)
	}
	t, err := r.tasks.CreateFull(title, body, "headless", u)
	if err != nil {
		r.logger.Error("review.create-task", "pr", pr.Number, "err", err)
		return
	}
	r.logger.Info("review.task-created", "task_id", t.ID, "pr", pr.Number, "project", projectID)
	go triage(t)
}

func (r *Handler) triageReview(t task.Task) {
	start := r.startReviewAgentFn
	if start == nil {
		start = r.StartReviewAgent
	}
	statsFn := r.fetchPRStatsFn
	if statsFn == nil {
		statsFn = github.FetchPRStats
	}
	stats, err := statsFn(t.ProjectID, t.PRNumber)
	if err != nil {
		r.logger.Warn("review.triage.stats", "task_id", t.ID, "err", err)
		// fallback: start agent when we can't determine size
		if _, err := r.tasks.Apply(task.TransitionIntent{
			TaskID:   t.ID,
			ToStatus: task.StatusInReview,
			Actor:    "review.triage",
		}); err != nil {
			r.logger.Error("review.triage.status", "task_id", t.ID, "err", err)
		}
		if err := start(t, false); err != nil {
			r.logger.Error("review.triage.start", "task_id", t.ID, "err", err)
		}
		return
	}

	r.logger.Info("review.triage", "task_id", t.ID, "additions", stats.Additions, "files", stats.ChangedFiles)

	if _, err := r.tasks.Apply(task.TransitionIntent{
		TaskID:   t.ID,
		ToStatus: task.StatusInReview,
		Actor:    "review.triage",
	}); err != nil {
		r.logger.Error("review.triage.status", "task_id", t.ID, "err", err)
	}
	if err := start(t, false); err != nil {
		r.logger.Error("review.triage.start", "task_id", t.ID, "err", err)
	}
}

func (r *Handler) StartFixReviewAgent(t task.Task) error {
	if t.ProjectID == "" || t.PRNumber == 0 {
		return fmt.Errorf("task %s has no linked PR", t.ID)
	}

	// Claim sole dispatch rights before touching anything else. This is the
	// one caller of agent.Manager.Run in this package that used to skip the
	// claim StartReviewAgent takes just below — reachable only from the
	// Wails-bound ReviewService.StartFixReview (manual "Fix Review" click),
	// it could race an automated fix-review dispatch already in flight for
	// the same task via WorkflowEngine.DispatchEvent (which claims through
	// agentAdapter.TryClaimDispatch, the same underlying agent.Manager).
	// agents.Run itself performs no per-task guard, so without this claim
	// both callers could start a second headless agent against the same
	// worktree/branch concurrently.
	if r.agents != nil {
		claim, ok := r.agents.TryClaimDispatch(t.ID)
		if !ok {
			r.logger.Info("fix-review.agent-skip", "task_id", t.ID, "pr", t.PRNumber, "reason", "dispatch_in_progress")
			return nil
		}
		defer claim.Release()
	}

	posture, postureErr := agentorch.ResolveHeadlessPermissionMode(t, r.cfg)
	if postureErr != nil {
		return postureErr
	}

	// context.Background(): StartFixReviewAgent is reached both from a Wails-bound
	// ReviewService method (no ctx) and from an async triage goroutine spawned
	// with a fixed func(task.Task) signature — no ctx to thread from either path.
	dir, err := r.worktrees.PrepareForFix(context.Background(), t, t.PRNumber)
	if err != nil {
		return fmt.Errorf("prepare worktree: %w", err)
	}

	prompt := fmt.Sprintf(
		"Run /fix-review https://github.com/%s/pull/%d --auto\n\n"+
			"IMPORTANT: when committing, use conventional commit format "+
			"`fix(review): address PR review comments` (type(scope) required by repo hooks). "+
			"Sign the commit with `git commit %s`. Push the branch when done.",
		t.ProjectID, t.PRNumber, project.CommitSignFlags(context.Background()),
	) + reviewHoldFixSuffix(r.cfg)

	ag, err := r.agents.Run(agent.RunConfig{
		TaskID: t.ID,
		Name:   agent.RoleFixReview.AgentName(t.Title),
		Role:   agent.RoleFixReview,
		Mode:   "headless",
		Prompt: prompt,
		Dir:    dir,
		Model:  "opus",
		// An effort the operator pinned on the task outranks the role
		// baseline the Manager would otherwise resolve; empty stays empty so
		// the Manager applies agent.role_effort and then the baseline.
		ReasoningEffort:        t.ReasoningEffort,
		HeadlessPermissionMode: posture,
		// MaxTurns intentionally not inherited: fix-review agents need
		// enough turns to fetch the PR, apply fixes, and commit.
	})
	if err != nil {
		return err
	}
	if err := r.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Role: string(agent.RoleFixReview), Mode: "headless", State: string(agent.StateRunning), StartedAt: ag.StartedAt,
		Prompt: prompt,
	}); err != nil {
		r.logger.Error("task.add-run", "task_id", t.ID, "err", err)
	}
	r.logAudit(audit.EventFixReviewStarted, t.ID, ag.ID, map[string]any{"pr": t.PRNumber, "prompt_hash": ag.GetPromptHash()})
	r.logger.Info("fix-review.agent-started", "task_id", t.ID, "agent_id", ag.ID, "pr", t.PRNumber)
	return nil
}

func (r *Handler) StartReviewAgent(t task.Task, force bool) error {
	current := t
	if force && r.tasks != nil {
		if latest, err := r.tasks.Get(t.ID); err == nil {
			current = latest
		}
	}
	if r.agents != nil {
		claim, ok := r.agents.TryClaimDispatch(current.ID)
		if !ok {
			r.logger.Info("review.agent-skip", "task_id", current.ID, "pr", current.PRNumber, "reason", "dispatch_in_progress")
			return nil
		}
		defer claim.Release()
	}
	if !force && r.tasks != nil {
		if latest, err := r.tasks.Get(current.ID); err == nil {
			current = latest
		}
	}
	if current.ProjectID == "" || current.PRNumber == 0 {
		return fmt.Errorf("task %s has no linked PR", current.ID)
	}
	if !force && reviewAgentAlreadyRan(current) {
		r.logger.Info("review.agent-skip", "task_id", current.ID, "pr", current.PRNumber, "reason", "already_reviewed")
		return nil
	}

	posture, postureErr := agentorch.ResolveHeadlessPermissionMode(current, r.cfg)
	if postureErr != nil {
		return postureErr
	}

	dir := config.HomeDir()
	if current.ProjectID != "" {
		// context.Background(): same dead end as StartFixReviewAgent above —
		// reached from a Wails-bound service method with no ctx.
		d, err := r.worktrees.PrepareForReview(context.Background(), current)
		if err != nil {
			r.logger.Error("review.worktree", "task_id", current.ID, "err", err)
		} else {
			dir = d
		}
	}

	prompt := StaffCodeReviewPrompt(current.ProjectID, current.PRNumber)

	cfg := r.agents.ApplyABVariant(StaffCodeReviewRunConfig(current, prompt, dir, posture), r.abTestingConfig(), current.ID, string(agent.RoleReview))
	ag, err := r.agents.Run(cfg)
	if err != nil {
		return err
	}
	if err := r.tasks.AddRun(current.ID, task.AgentRun{
		AgentID:         ag.ID,
		Role:            string(agent.RoleReview),
		Mode:            "headless",
		Provider:        ag.Provider,
		Model:           ag.Model,
		ExperimentID:    ag.ExperimentID,
		VariantID:       ag.VariantID,
		RoutingReason:   ag.RoutingReason,
		AssignmentUnit:  ag.AssignmentUnit,
		AssignmentKey:   ag.AssignmentKey,
		DecisionVersion: ag.DecisionVersion,
		State:           string(agent.StateRunning),
		StartedAt:       ag.StartedAt,
		Prompt:          cfg.Prompt,
	}); err != nil {
		r.logger.Error("task.add-run", "task_id", current.ID, "err", err)
		if stopErr := r.agents.StopAgent(ag.ID); stopErr != nil {
			return fmt.Errorf("record review run: %w; stop started agent %s: %w", err, ag.ID, stopErr)
		}
		return fmt.Errorf("record review run: %w", err)
	}
	r.logAudit(audit.EventReviewStarted, current.ID, ag.ID, map[string]any{"pr": current.PRNumber, "prompt_hash": ag.GetPromptHash()})
	r.logger.Info("review.agent-started", "task_id", current.ID, "agent_id", ag.ID, "pr", current.PRNumber)
	return nil
}

// StaffCodeReviewRunConfig builds the base run config for the GitHub-PR-triggered
// staff review. Provider is left unset so the caller's Manager.ApplyABVariant
// resolves it through the A/B suite (with failover on) or, when A/B is disabled,
// the manager default. Model defaults to opus so the claude variant and any
// A/B-disabled fallback keep their high-scrutiny model; an A/B pick overrides it.
// MaxTurns is intentionally not inherited: review agents need enough turns to
// fetch the PR, run the skill, and write findings.
func StaffCodeReviewRunConfig(t task.Task, prompt, dir, posture string) agent.RunConfig {
	return agent.RunConfig{
		TaskID: t.ID,
		Name:   agent.RoleReview.AgentName(t.Title),
		Role:   agent.RoleReview,
		Mode:   "headless",
		Prompt: prompt,
		Dir:    dir,
		Model:  "opus",
		// An effort the operator pinned on the task outranks the role
		// baseline the Manager would otherwise resolve; empty stays empty so
		// the Manager applies agent.role_effort and then the baseline.
		ReasoningEffort:        t.ReasoningEffort,
		HeadlessPermissionMode: posture,
	}
}

// StaffCodeReviewPrompt returns the direct PR-review prompt shared by inbound
// review automation and task enrichment. It withholds only approval
// authority: these PRs are other people's work, and an approval from the
// operator's account can satisfy a required-reviewer gate. REQUEST_CHANGES
// and COMMENT are feedback, not authority, so the prompt authorizes
// submitting those directly instead of parking every review as an unsubmitted
// pending draft. Kept in lockstep with the pr-review builtin workflow prompts
// and backed by the gh PATH shim (agent.writeGhShim), which refuses submitted
// APPROVE events if this instruction ever drifts.
func StaffCodeReviewPrompt(projectID string, prNumber int) string {
	return fmt.Sprintf(`Run /staff-code-review on https://github.com/%s/pull/%d

This task is an authorized Sybra PR review for the linked project. Do not ask the operator for confirmation before posting your review.

You have no approval authority: NEVER submit an APPROVE review event. Do not run `+"`gh pr review --approve`"+` and do not submit `+"`event=APPROVE`"+` via `+"`gh api`"+`. REQUEST_CHANGES and COMMENT are feedback, not approval authority, so submit those directly instead of leaving them pending.

Before posting, fetch the PR head SHA and existing reviews; if a review for that head already contains the Sybra harness footer, do not create another review.

Use GitHub's review API so findings become inline comments, not one aggregated comment. Add each blocking correctness issue as a `+"`comments`"+` entry on the changed line it applies to. Put only the short verdict and summary in the review body.

Post via `+"`gh api repos/%s/pulls/%d/reviews -X POST ...`"+` with explicit `+"`-f`"+`/`+"`-F`"+` fields such as `+"`comments[][path]`"+`, `+"`comments[][line]`"+`, `+"`comments[][side]=RIGHT`"+`, and `+"`comments[][body]`"+`, then choose the event by verdict: at least one blocking finding, submit with `+"`-f event=REQUEST_CHANGES`"+`; no blocking finding but the review has notes worth surfacing, submit with `+"`-f event=COMMENT`"+`; nothing to say at all (fully clean, no findings), omit `+"`event`"+` so GitHub leaves the review PENDING for a human to approve.

End the review summary and every review comment you post with a blank line then exactly this standalone harness attribution footer:

_Generated by Sybra harness_`, projectID, prNumber, projectID, prNumber)
}

func reviewAgentAlreadyRan(t task.Task) bool {
	if t.Reviewed {
		return true
	}
	return slices.ContainsFunc(t.AgentRuns, func(r task.AgentRun) bool {
		return r.Role == string(agent.RoleReview)
	})
}

func (r *Handler) maybeCreateReviewTasks(tasks []task.Task, reviewPRs []github.PullRequest) {
	projects, err := r.projects.List()
	if err != nil || len(projects) == 0 {
		return
	}

	projectMatchers := make([]github.ProjectMatcher, 0, len(projects))
	for i := range projects {
		projectMatchers = append(projectMatchers, github.ProjectMatcher{
			ID:         projects[i].Owner + "/" + projects[i].Repo,
			Repository: projects[i].Owner + "/" + projects[i].Repo,
		})
	}

	matches := github.MatchReviewPRs(reviewPRs, projectMatchers)
	for i := range matches {
		if matches[i].PR.IsDraft {
			continue
		}
		if matches[i].PR.ReviewDecision == "APPROVED" {
			continue
		}
		if r.hasActiveLocalPROwner(tasks, matches[i].ProjectID, matches[i].PR.Number, matches[i].PR.HeadRefName, matches[i].PR.HeadRepo) {
			continue
		}
		r.createReviewTask(matches[i].PR, matches[i].ProjectID)
	}
}

func (r *Handler) hasActiveLocalPROwner(tasks []task.Task, projectID string, prNumber int, branch, headRepo string) bool {
	for i := range tasks {
		// PR numbers are per-repo, so an existing local owner only suppresses
		// another PR with the same number when they belong to the same project.
		// This is intentionally broader than "has review tag": self-authored
		// Sybra PRs are already owned by their implementation task. Branch is a
		// secondary key for the window before create-pr/link-pr has stamped the
		// implementation task's PR number, but only for same-repo heads; forked PR
		// branch names are not unique.
		if taskOwnsPR(tasks[i], "", projectID, prNumber, branch, headRepo, false) {
			return true
		}
	}
	return false
}

func activeNonReviewPROwner(tasks []task.Task, reviewTaskID, projectID string, prNumber int, branch, headRepo string) (string, bool) {
	for i := range tasks {
		if !taskOwnsPR(tasks[i], reviewTaskID, projectID, prNumber, branch, headRepo, true) {
			continue
		}
		return tasks[i].ID, true
	}
	return "", false
}

func taskOwnsPR(t task.Task, reviewTaskID, projectID string, prNumber int, branch, headRepo string, excludeReviewTasks bool) bool {
	if (reviewTaskID != "" && t.ID == reviewTaskID) ||
		t.ProjectID != projectID ||
		task.IsTerminalStatus(t.Status) ||
		(excludeReviewTasks && slices.Contains(t.Tags, "review")) {
		return false
	}
	if prNumber != 0 && t.PRNumber == prNumber {
		return true
	}
	if t.PRNumber != 0 {
		return false
	}
	return branch != "" && t.Branch == branch && projectHeadRepoMatches(projectID, headRepo)
}

func projectHeadRepoMatches(projectID, headRepo string) bool {
	if projectID == "" || headRepo == "" {
		return false
	}
	return strings.EqualFold(projectID, headRepo)
}

// reviewPRKey identifies a PR within a repo for summary lookups.
func reviewPRKey(projectID string, prNumber int) string {
	return projectID + "#" + strconv.Itoa(prNumber)
}

// reconcileFailureLimit is how many consecutive non-transient reconcile
// failures a review task tolerates before escalating to a human.
//
// The reconcile read decides whether a review task still needs an agent. The
// old code logged a warning and left the phase untouched on failure, which
// sounds conservative but is not: `needs-approval` is a *dispatchable* phase,
// so a permanently-failing read parked the task in the one state that re-fires
// every cooldown. #2164 warned every ~2 minutes for 23 hours while re-reviewing
// a stranger's PR 112 times. A warn-log is not an alarm.
const reconcileFailureLimit = 5

// reconcileEscalationReason prefixes the StatusReason this circuit writes.
const reconcileEscalationReason = "review reconcile failed"

// recordReconcileFailure counts consecutive reconcile failures and escalates
// once they look permanent. Transient blips (5xx, timeouts, budget backoff) are
// expected and never count; only a read that keeps failing is a defect.
//
// The count lives on the task itself (Task.ReconcileFailures), not an
// in-memory map (#2199): a process restart must not hand a permanently-failing
// task a fresh free budget, silently doubling how long #2164-style breakage
// can run undetected across a redeploy.
func (r *Handler) recordReconcileFailure(t *task.Task, err error) {
	if github.IsTransientError(err) {
		r.logger.Warn("review.my-state", "task_id", t.ID, "err", err, "transient", true)
		return
	}

	// Already parked on a human: escalating again achieves nothing and actively
	// harms — human-required is not terminal, so the poller keeps feeding this
	// task back, and each pass would overwrite the operator's own triage note
	// and rewrite updated_at on work nobody is doing. Deliberately keyed on
	// status alone, not on our own reason string: an operator who replaces the
	// note must not thereby re-arm the clobber.
	if t.Status == task.StatusHumanRequired {
		// Drop the count too: it measures progress toward an escalation that has
		// already happened, and keeping it would pin an entry for every parked
		// task for the life of the process.
		r.clearReconcileFailure(t)
		return
	}

	attempts := t.ReconcileFailures + 1
	if attempts < reconcileFailureLimit {
		if _, uerr := r.tasks.Update(t.ID, task.Update{ReconcileFailures: task.Ptr(attempts)}); uerr != nil {
			r.logger.Error("review.reconcile.count", "task_id", t.ID, "err", uerr)
		}
		r.logger.Warn("review.my-state", "task_id", t.ID, "err", err, "attempts", attempts)
		return
	}

	r.logger.Error("review.reconcile.circuit-open",
		"task_id", t.ID, "failures", reconcileFailureLimit, "err", err)
	// human-required is not dispatchable, so escalating both surfaces the defect
	// and starves the re-review a frozen phase would keep feeding. Reset the
	// count in this same write (rather than leaving it for the next
	// clearReconcileFailure call) so an already-parked task never takes a
	// second write — and a second updated_at bump — on the very next poll.
	if _, uerr := r.tasks.Apply(task.TransitionIntent{
		TaskID:   t.ID,
		ToStatus: task.StatusHumanRequired,
		Actor:    "review.reconcile.escalate",
		Extra: task.Update{
			StatusReason:      task.Ptr(fmt.Sprintf("%s %d times: %v", reconcileEscalationReason, reconcileFailureLimit, err)),
			ReconcileFailures: task.Ptr(0),
		},
	}); uerr != nil {
		r.logger.Error("review.reconcile.escalate", "task_id", t.ID, "err", uerr)
	}
}

// clearReconcileFailure resets t's durable failure count once a reconcile
// succeeds. A no-op write when the count is already zero, so a healthy task
// reconciling every cooldown doesn't touch the task file on every pass.
func (r *Handler) clearReconcileFailure(t *task.Task) {
	if t == nil || t.ReconcileFailures == 0 {
		return
	}
	if _, err := r.tasks.Update(t.ID, task.Update{ReconcileFailures: task.Ptr(0)}); err != nil {
		r.logger.Error("review.reconcile.clear", "task_id", t.ID, "err", err)
		return
	}
	t.ReconcileFailures = 0
}

// RateLimitParkReason prefixes the StatusReason written when the review rate
// breaker trips (#2168). The reconciler honours it as a latch.
const RateLimitParkReason = "automated review rate limit"

// circuitParked reports whether t was parked by an automation breaker rather
// than by ordinary review flow.
//
// human-required is NOT a latch here: reconcileReviewPhases skips only
// done/cancelled, and computeReviewPhase names Status=in-review for the
// needs-approval state, so a parked task is dragged back to in-review on the
// next poll and re-dispatched within the cooldown. The breaker would self-heal
// into the next burst and its reason would be overwritten before a human read it.
func circuitParked(t *task.Task) bool {
	return t.Status == task.StatusHumanRequired &&
		strings.HasPrefix(t.StatusReason, RateLimitParkReason)
}

// reconcileReviewPhases recomputes the lifecycle phase of every inbound
// PR-review task (tag `review`) from live GitHub signals and persists any
// delta. It supersedes the old human-required→in-review "published" detector,
// folding that transition into the phase machine.
func (r *Handler) reconcileReviewPhases(tasks []task.Task, summary github.ReviewSummary) {
	requested := indexPRsByKey(summary.ReviewRequested)
	approved := indexPRsByKey(summary.ReviewedByMe) // reviewed-by:@me is approvals-only

	for i := range tasks {
		t := &tasks[i]
		if !slices.Contains(t.Tags, "review") || task.IsTerminalStatus(t.Status) {
			continue
		}
		if circuitParked(t) {
			continue
		}
		if t.PRNumber == 0 || t.ProjectID == "" {
			continue
		}
		key := reviewPRKey(t.ProjectID, t.PRNumber)
		branch := t.Branch
		headRepo := ""
		if pr, ok := requested[key]; ok {
			headRepo = pr.HeadRepo
			if branch == "" {
				branch = pr.HeadRefName
			}
		} else if pr, ok := approved[key]; ok {
			headRepo = pr.HeadRepo
			if branch == "" {
				branch = pr.HeadRefName
			}
		}
		if ownerID, ok := activeNonReviewPROwner(tasks, t.ID, t.ProjectID, t.PRNumber, branch, headRepo); ok {
			reason := fmt.Sprintf("Duplicate: PR is already tracked by active task %s", ownerID)
			if _, err := r.tasks.Apply(task.TransitionIntent{
				TaskID:   t.ID,
				ToStatus: task.StatusCancelled,
				Actor:    "review.duplicate-owner.cancel",
				Extra: task.Update{
					StatusReason: task.Ptr(reason),
				},
			}); err != nil {
				r.logger.Error("review.duplicate-owner.cancel", "task_id", t.ID, "owner_task_id", ownerID, "err", err)
			}
			continue
		}
		r.reconcileReviewTask(t, requested, approved)
	}
}

// reconcileReviewTask computes and applies the phase for a single review task.
func (r *Handler) reconcileReviewTask(t *task.Task, requested, approved map[string]github.PullRequest) {
	key := reviewPRKey(t.ProjectID, t.PRNumber)
	reqPR, inReq := requested[key]
	apPR, inApproved := approved[key]

	// An agent owning the PR short-circuits: surface "reviewing" without the
	// extra GitHub round-trips.
	if r.agents.HasRunningAgentForTask(t.ID) {
		// Reaching this proves the task is healthy, so any earlier failures are
		// stale. Without clearing here the count never decays and a single fresh
		// failure hours later can trip a circuit meant to catch a persistent one.
		r.clearReconcileFailure(t)
		// A running (possibly stuck/looping) review agent that already submitted a
		// bogus approval would otherwise leave it live on GitHub for the whole run,
		// since this branch skips the full self-approval path below (#2198).
		r.reverseLiveSelfApproval(t, inApproved)
		r.applyReviewPhase(t, computeReviewPhase(reviewSignals{AgentRunning: true}))
		return
	}

	// A conflicting PR is blocked on the author rebasing — surface "conflict" and
	// sink it to the bottom of the lane, whatever the viewer's review state (the
	// conflict outranks every other review phase). Prefer the mergeability already
	// carried by the review summary; only spend a PR-state call when the PR is in
	// neither leg (e.g. a submitted, non-re-requested review) or GitHub hasn't
	// computed mergeability yet.
	mergeable := ""
	switch {
	case inReq:
		mergeable = reqPR.Mergeable
	case inApproved:
		mergeable = apPR.Mergeable
	}
	if mergeable == "" || mergeable == "UNKNOWN" {
		if st, err := github.FetchPRState(t.ProjectID, t.PRNumber); err != nil {
			r.logger.Warn("review.pr-state", "task_id", t.ID, "err", err)
		} else {
			mergeable = st.Mergeable
		}
	}
	if res, decided := stickyConflictPhase(mergeable, t.ReviewPhase); decided {
		r.clearReconcileFailure(t)
		// The conflict phase outranks review state, but a self-approval is still a
		// live green light on GitHub that native auto-merge would count the moment
		// the conflict clears — reverse it now rather than waiting for the one poll
		// where mergeability flips back and this branch stops short-circuiting.
		r.reverseLiveSelfApproval(t, inApproved)
		r.applyReviewPhase(t, res)
		return
	}

	myStateFn := github.FetchMyReviewState
	if r.fetchMyReviewStateFn != nil {
		myStateFn = r.fetchMyReviewStateFn
	}
	myState, err := myStateFn(t.ProjectID, t.PRNumber)
	if err != nil {
		r.recordReconcileFailure(t, err)
		return
	}
	r.clearReconcileFailure(t)

	viewerApproved := myState.Approved || (!myState.Submitted && inApproved)
	selfApproved := myState.ViewerIsBot && viewerApproved
	if selfApproved {
		r.dismissSelfApproval(t, myState)
	}

	submitted := myState.Submitted || inApproved
	headSHA := ""
	baseOnlyMergeFromReviewed := false
	headLineageUnknown := false
	switch {
	case inReq:
		headSHA = reqPR.HeadSHA
	case inApproved:
		headSHA = apPR.HeadSHA
	case submitted:
		// A submitted (non-approval) review that wasn't re-requested leaves the
		// PR in neither summary leg; fetch the head so a silent push past the
		// reviewed commit still flips us to needs-approval.
		if sha, herr := github.FetchPRHeadSHA(t.ProjectID, t.PRNumber); herr != nil {
			r.logger.Warn("review.head-sha", "task_id", t.ID, "err", herr)
		} else {
			headSHA = sha
		}
	}
	if submitted && !inReq && !myState.Approved && !inApproved && headSHA != "" && myState.ReviewedSHA != "" && headSHA != myState.ReviewedSHA {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		baseOnly, lerr := github.FetchBaseOnlyMergeFromReviewed(ctx, t.ProjectID, t.PRNumber, headSHA, myState.ReviewedSHA)
		cancel()
		if lerr != nil {
			headLineageUnknown = true
			r.logger.Warn(
				"review.head-lineage",
				"task_id", t.ID,
				"repo", t.ProjectID,
				"pr", t.PRNumber,
				"head_sha", headSHA,
				"reviewed_sha", myState.ReviewedSHA,
				"err", lerr,
			)
		} else {
			baseOnlyMergeFromReviewed = baseOnly
		}
	}

	r.applyReviewPhase(t, computeReviewPhase(reviewSignals{
		CostGuardrailStopped:      latestReviewRunStoppedByCostGuardrail(t),
		HasDraft:                  myState.Pending,
		SelfApproved:              selfApproved,
		Approved:                  viewerApproved,
		Submitted:                 submitted,
		ReRequested:               inReq,
		HeadSHA:                   headSHA,
		ReviewedSHA:               myState.ReviewedSHA,
		HeadLineageUnknown:        headLineageUnknown,
		BaseOnlyMergeFromReviewed: baseOnlyMergeFromReviewed,
	}))
}

// selfApprovalDismissMessage is left on GitHub's audit trail explaining why
// an approval was reversed the moment it was detected.
const selfApprovalDismissMessage = "Dismissed automatically by Sybra: our own bot identity approved its own review task, which can never stand in for a human review."

// reverseLiveSelfApproval dismisses a live bot self-approval before the
// agent-running and conflict short-circuits in reconcileReviewTask return,
// which would otherwise skip the full self-approval path and leave a bogus
// APPROVED review standing on GitHub for the whole agent run / conflict window.
//
// The REST round-trip is spent only when the cheap approvals-only summary leg
// (inApproved) already flags an approval, so the common no-approval path keeps
// the short-circuit's zero-round-trip cost. Escalation to human-required is not
// attempted here — the short-circuit phases (reviewing/conflict) stand; the
// full self-approval escalation runs on the next fall-through poll.
func (r *Handler) reverseLiveSelfApproval(t *task.Task, inApproved bool) {
	if !inApproved {
		return
	}
	myStateFn := github.FetchMyReviewState
	if r.fetchMyReviewStateFn != nil {
		myStateFn = r.fetchMyReviewStateFn
	}
	myState, err := myStateFn(t.ProjectID, t.PRNumber)
	if err != nil {
		r.logger.Warn("review.self-approval.probe", "task_id", t.ID, "pr", t.PRNumber, "err", err)
		return
	}
	r.dismissSelfApproval(t, myState)
}

// dismissSelfApproval reverses an approval our own bot identity submitted on
// a PR it is reviewing. This is never a legitimate green light — it means an
// approval escaped the gh shim's PreToolUse-adjacent floor (internal/agent's
// ghshim.go) or was otherwise submitted under the bot's own credentials
// rather than a human's — so the outcome, not just the attempt, is caught and
// reversed here (#2198).
//
// Best-effort: myState is only populated with a review ID when the REST
// fetch itself observed the approval (myState.Approved), so an inApproved-only
// detection (stale search-leg signal, REST already shows something else) has
// nothing to dismiss yet — computeReviewPhase still keeps the task out of the
// "approved" phase regardless. A dismissal failure is logged, never fatal:
// escalating the task to human-required does not depend on it succeeding.
func (r *Handler) dismissSelfApproval(t *task.Task, myState github.MyReviewState) {
	if !myState.ViewerIsBot || !myState.Approved || myState.ReviewID == 0 {
		return
	}
	dismissFn := github.DismissReview
	if r.dismissReviewFn != nil {
		dismissFn = r.dismissReviewFn
	}
	if err := dismissFn(t.ProjectID, t.PRNumber, myState.ReviewID, selfApprovalDismissMessage); err != nil {
		r.logger.Error("review.self-approval.dismiss-failed", "task_id", t.ID, "pr", t.PRNumber, "err", err)
		return
	}
	r.logAudit(audit.EventReviewSelfApprovalDismissed, t.ID, "", map[string]any{"pr": t.PRNumber, "repo": t.ProjectID})
}

func latestReviewRunStoppedByCostGuardrail(t *task.Task) bool {
	for i := range slices.Backward(t.AgentRuns) {
		run := t.AgentRuns[i]
		if run.Role != string(agent.RoleReview) {
			continue
		}
		return run.EscalationReason == "cost"
	}
	return false
}

// defaultManualReviewReason explains the manual review phase on the board.
// Without it a task parked by computeReviewPhase carries no status reason at
// all, so the operator sees a needs-you badge with no stated cause.
const defaultManualReviewReason = "no automated review exists for this PR yet — review it, or reopen the task to let an agent draft one"

// applyReviewPhase persists only the fields that changed. Status is set only
// when the result names one and it differs (so an unchanged status never
// clears a triage-authored reason); the reason follows a status or phase change.
//
// This is the single write site for a review task's ReviewPhase/Status
// (#2499): computeReviewPhase (review_phase.go) is a pure function of
// reviewSignals — it returns a reviewPhaseResult rather than mutating
// anything — and every caller routes the result through here rather than
// writing task.Update directly. The outbound (own-PR) equivalent is
// applyPRPhase in outbound.go, following the same pattern for PRPhase.
func (r *Handler) applyReviewPhase(t *task.Task, res reviewPhaseResult) {
	statusChanged := res.Status != "" && res.Status != t.Status
	phaseChanged := res.Phase != t.ReviewPhase
	if !statusChanged && !phaseChanged {
		return
	}

	u := task.Update{}
	if phaseChanged {
		u.ReviewPhase = task.Ptr(res.Phase)
	}
	reason := res.Reason
	// A phase that asserts human-required but names no reason (manual) leaves
	// the board showing "needs you" with nothing said, which is
	// indistinguishable from a bug.
	//
	// The empty reason exists so an existing triage/reconciliation blocker
	// survives — but that only holds while the status is unchanged, since a
	// transition clears the reason regardless. So fill on a transition (where
	// nothing is preserved either way) or when the reason is genuinely blank,
	// and leave an existing reason alone on a phase-only update.
	if reason == "" && res.Status == task.StatusHumanRequired &&
		(statusChanged || strings.TrimSpace(t.StatusReason) == "") {
		reason = defaultManualReviewReason
	}
	if reason != "" && (statusChanged || phaseChanged) {
		u.StatusReason = task.Ptr(reason)
	}

	prev := t.ReviewPhase
	if statusChanged {
		if _, err := r.tasks.Apply(task.TransitionIntent{
			TaskID:   t.ID,
			ToStatus: res.Status,
			Actor:    "review.phase-update",
			Extra:    u,
		}); err != nil {
			r.logger.Error("review.phase-update", "task_id", t.ID, "phase", res.Phase, "err", err)
			return
		}
	} else if _, err := r.tasks.Update(t.ID, u); err != nil {
		r.logger.Error("review.phase-update", "task_id", t.ID, "phase", res.Phase, "err", err)
		return
	}
	if !phaseChanged {
		return
	}
	r.logger.Info("review.phase", "task_id", t.ID, "pr", t.PRNumber, "from", prev, "to", res.Phase)
	if reviewPhasePublished(prev, res.Phase) {
		r.logAudit(audit.EventReviewPublished, t.ID, "", map[string]any{"pr": t.PRNumber})
	}
}

// reviewPhasePublished reports whether a transition represents the human
// publishing their review — moving from a pre-submit phase into a submitted
// one. Drives the EventReviewPublished audit log.
func reviewPhasePublished(prev, next string) bool {
	if next != ReviewPhaseAwaitingAuthor && next != ReviewPhaseNeedsApproval {
		return false
	}
	switch prev {
	case ReviewPhaseDrafted, ReviewPhaseManual, ReviewPhaseReviewing, "":
		return true
	default:
		return false
	}
}

// indexPRsByKey maps PRs by "owner/repo#number" for O(1) summary lookups.
func indexPRsByKey(prs []github.PullRequest) map[string]github.PullRequest {
	m := make(map[string]github.PullRequest, len(prs))
	for i := range prs {
		m[reviewPRKey(prs[i].Repository, prs[i].Number)] = prs[i]
	}
	return m
}

func reviewClosedPREligible(t *task.Task) bool {
	return !task.IsTerminalStatus(t.Status) &&
		slices.Contains(t.Tags, "review") &&
		t.ProjectID != "" &&
		t.PRNumber != 0
}

func reviewTaskMatchers(tasks []task.Task) []github.TaskMatcher {
	matchers := make([]github.TaskMatcher, 0, len(tasks))
	for i := range tasks {
		if !reviewClosedPREligible(&tasks[i]) {
			continue
		}
		matchers = append(matchers, github.TaskMatcher{
			ID:        tasks[i].ID,
			PRNumber:  tasks[i].PRNumber,
			ProjectID: tasks[i].ProjectID,
		})
	}
	return matchers
}

func openReviewPRs(summary github.ReviewSummary) []github.PullRequest {
	if len(summary.ReviewedByMe) == 0 {
		return summary.ReviewRequested
	}
	prs := make([]github.PullRequest, 0, len(summary.ReviewRequested)+len(summary.ReviewedByMe))
	prs = append(prs, summary.ReviewRequested...)
	prs = append(prs, summary.ReviewedByMe...)
	return prs
}

func (r *Handler) closeFinishedReviewTasks(tasks []task.Task, openReviewPRs []github.PullRequest) {
	matchers := reviewTaskMatchers(tasks)
	if len(matchers) == 0 {
		return
	}
	fetchFn := github.FetchPRState
	if r.fetchPRStateFn != nil {
		fetchFn = r.fetchPRStateFn
	}
	r.closeFinishedReviewTasksWithFetch(tasks, openReviewPRs, fetchFn)
}

func (r *Handler) closeFinishedReviewTasksWithFetch(tasks []task.Task, openReviewPRs []github.PullRequest, fetchFn func(repo string, number int) (github.PRState, error)) {
	matchers := reviewTaskMatchers(tasks)
	if len(matchers) == 0 {
		return
	}
	closedPRs := github.DetectClosedTaskPRs(openReviewPRs, matchers, fetchFn)
	for _, c := range closedPRs {
		if r.agents.HasRunningAgentForTask(c.TaskID) {
			r.logger.Info("review.closed-skip-running-agent", "task_id", c.TaskID, "pr", c.PRNumber)
			continue
		}
		reason := fmt.Sprintf("review PR %s", strings.ToLower(c.State))
		if _, err := r.tasks.Apply(task.TransitionIntent{
			TaskID:   c.TaskID,
			ToStatus: task.StatusDone,
			Actor:    "review.closed-update",
			Extra: task.Update{
				StatusReason: &reason,
			},
		}); err != nil {
			r.logger.Error("review.closed-update", "task_id", c.TaskID, "err", err)
			continue
		}
		eventType := audit.EventPRMerged
		if c.State == "CLOSED" {
			eventType = audit.EventPRClosed
		}
		r.logAudit(eventType, c.TaskID, "", map[string]any{"pr": c.PRNumber, "state": c.State, "review_task": true})
		r.logger.Info("review.auto-done", "task_id", c.TaskID, "pr", c.PRNumber, "state", c.State)
	}
}
