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
	t, err := r.tasks.CreateFull(title, body, "headless", task.Update{
		Tags:      &tags,
		ProjectID: task.Ptr(projectID),
		PRNumber:  task.Ptr(pr.Number),
	})
	if err != nil {
		r.logger.Error("review.create-task", "pr", pr.Number, "err", err)
		return
	}
	r.logger.Info("review.task-created", "task_id", t.ID, "pr", pr.Number, "project", projectID)
	go triage(t)
}

// triageReviewSmall returns true when the PR is below both size thresholds and
// should be routed to human-required rather than dispatched to a review agent.
func triageReviewSmall(additions, changedFiles int) bool {
	return additions < reviewSmallAdditions && changedFiles < reviewSmallFiles
}

func (r *Handler) triageReview(t task.Task) {
	stats, err := github.FetchPRStats(t.ProjectID, t.PRNumber)
	if err != nil {
		r.logger.Warn("review.triage.stats", "task_id", t.ID, "err", err)
		// fallback: start agent when we can't determine size
		if _, err := r.tasks.Update(t.ID, task.Update{Status: task.Ptr(task.StatusInReview)}); err != nil {
			r.logger.Error("review.triage.status", "task_id", t.ID, "err", err)
		}
		if err := r.StartReviewAgent(t, false); err != nil {
			r.logger.Error("review.triage.start", "task_id", t.ID, "err", err)
		}
		return
	}

	r.logger.Info("review.triage", "task_id", t.ID, "additions", stats.Additions, "files", stats.ChangedFiles)

	if triageReviewSmall(stats.Additions, stats.ChangedFiles) {
		reason := fmt.Sprintf("PR too small for agent review (%d additions, %d files)", stats.Additions, stats.ChangedFiles)
		if _, err := r.tasks.Update(t.ID, task.Update{
			Status:       task.Ptr(task.StatusHumanRequired),
			StatusReason: &reason,
		}); err != nil {
			r.logger.Error("review.triage.human", "task_id", t.ID, "err", err)
		}
		r.logger.Info("review.triage.small", "task_id", t.ID, "additions", stats.Additions, "files", stats.ChangedFiles)
		return
	}

	if _, err := r.tasks.Update(t.ID, task.Update{Status: task.Ptr(task.StatusInReview)}); err != nil {
		r.logger.Error("review.triage.status", "task_id", t.ID, "err", err)
	}
	if err := r.StartReviewAgent(t, false); err != nil {
		r.logger.Error("review.triage.start", "task_id", t.ID, "err", err)
	}
}

func (r *Handler) StartFixReviewAgent(t task.Task) error {
	if t.ProjectID == "" || t.PRNumber == 0 {
		return fmt.Errorf("task %s has no linked PR", t.ID)
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
		TaskID:                 t.ID,
		Name:                   agent.RoleFixReview.AgentName(t.Title),
		Mode:                   "headless",
		Prompt:                 prompt,
		Dir:                    dir,
		Model:                  "opus",
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
	r.logAudit(audit.EventFixReviewStarted, t.ID, ag.ID, map[string]any{"pr": t.PRNumber})
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

	prompt := fmt.Sprintf("Run /staff-code-review on https://github.com/%s/pull/%d", current.ProjectID, current.PRNumber)

	cfg := r.agents.ApplyABVariant(StaffCodeReviewRunConfig(current, prompt, dir, posture), r.cfg.ABTesting, current.ID, string(agent.RoleReview))
	ag, err := r.agents.Run(cfg)
	if err != nil {
		return err
	}
	if err := r.tasks.AddRun(current.ID, task.AgentRun{
		AgentID: ag.ID, Role: string(agent.RoleReview), Mode: "headless", State: string(agent.StateRunning), StartedAt: ag.StartedAt,
		Prompt: cfg.Prompt,
	}); err != nil {
		r.logger.Error("task.add-run", "task_id", current.ID, "err", err)
		if stopErr := r.agents.StopAgent(ag.ID); stopErr != nil {
			return fmt.Errorf("record review run: %w; stop started agent %s: %w", err, ag.ID, stopErr)
		}
		return fmt.Errorf("record review run: %w", err)
	}
	r.logAudit(audit.EventReviewStarted, current.ID, ag.ID, map[string]any{"pr": current.PRNumber})
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
		TaskID:                 t.ID,
		Name:                   agent.RoleReview.AgentName(t.Title),
		Mode:                   "headless",
		Prompt:                 prompt,
		Dir:                    dir,
		Model:                  "opus",
		HeadlessPermissionMode: posture,
	}
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
		if r.hasReviewTask(tasks, matches[i].ProjectID, matches[i].PR.Number) {
			continue
		}
		r.createReviewTask(matches[i].PR, matches[i].ProjectID)
	}
}

func (r *Handler) hasReviewTask(tasks []task.Task, projectID string, prNumber int) bool {
	for i := range tasks {
		// PR numbers are per-repo, so a review task only suppresses another
		// PR with the same number when they belong to the same project.
		if tasks[i].ProjectID == projectID &&
			tasks[i].PRNumber == prNumber &&
			slices.Contains(tasks[i].Tags, "review") {
			return true
		}
	}
	return false
}

// reviewPRKey identifies a PR within a repo for summary lookups.
func reviewPRKey(projectID string, prNumber int) string {
	return projectID + "#" + strconv.Itoa(prNumber)
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
		if t.PRNumber == 0 || t.ProjectID == "" {
			continue
		}
		r.reconcileReviewTask(t, requested, approved)
	}
}

// reconcileReviewTask computes and applies the phase for a single review task.
func (r *Handler) reconcileReviewTask(t *task.Task, requested, approved map[string]github.PullRequest) {
	// An agent owning the PR short-circuits: surface "reviewing" without the
	// extra GitHub round-trips.
	if r.agents.HasRunningAgentForTask(t.ID) {
		r.applyReviewPhase(t, computeReviewPhase(reviewSignals{AgentRunning: true}))
		return
	}

	key := reviewPRKey(t.ProjectID, t.PRNumber)
	reqPR, inReq := requested[key]
	apPR, inApproved := approved[key]

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
		r.applyReviewPhase(t, res)
		return
	}

	myState, err := github.FetchMyReviewState(t.ProjectID, t.PRNumber)
	if err != nil {
		r.logger.Warn("review.my-state", "task_id", t.ID, "err", err)
		return
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
		ViewerApproved:            myState.Approved || inApproved,
		Submitted:                 submitted,
		ReRequested:               inReq,
		HeadSHA:                   headSHA,
		ReviewedSHA:               myState.ReviewedSHA,
		HeadLineageUnknown:        headLineageUnknown,
		BaseOnlyMergeFromReviewed: baseOnlyMergeFromReviewed,
	}))
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

// applyReviewPhase persists only the fields that changed. Status is set only
// when the result names one and it differs (so an unchanged status never
// clears a triage-authored reason); the reason follows a status or phase change.
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
	if statusChanged {
		u.Status = task.Ptr(res.Status)
	}
	if res.Reason != "" && (statusChanged || phaseChanged) {
		u.StatusReason = task.Ptr(res.Reason)
	}

	prev := t.ReviewPhase
	if _, err := r.tasks.Update(t.ID, u); err != nil {
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
	return t.TaskType != task.TaskTypeChat &&
		!task.IsTerminalStatus(t.Status) &&
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
		if _, err := r.tasks.Update(c.TaskID, task.Update{
			Status:       task.Ptr(task.StatusDone),
			StatusReason: &reason,
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
