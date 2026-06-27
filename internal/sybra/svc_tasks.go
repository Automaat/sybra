package sybra

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/sandbox"
	"github.com/Automaat/sybra/internal/stats"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

// TaskService exposes task CRUD operations as Wails-bound methods.
type TaskService struct {
	tasks               *task.Manager
	agents              *agent.Manager
	workflowEngine      *workflow.Engine
	worktrees           *worktree.Manager
	sandboxes           *sandbox.Manager
	wg                  *sync.WaitGroup
	logger              *slog.Logger
	audit               *audit.Logger
	cfg                 *config.Config
	fetchPR             func(repo string, number int) (github.PullRequest, error)
	fetchIssue          func(repo string, number int) (github.Issue, error)
	fetchIssueLinkedPRs func(repo string, issueNumber int) ([]github.PullRequest, error)
	viewerLogin         func() string
	// umbrellaExpand expands a detected ☂️ umbrella issue into a gated child
	// DAG instead of a flat task. Wired in wireServices; gated at call time on
	// cfg.Umbrella.Enabled. nil in tests that don't exercise umbrellas.
	umbrellaExpand func(issueURL string) (umbrella.Result, error)
}

// ListTasks returns all tasks from the store, excluding ephemeral chat tasks.
// Chat tasks are surfaced exclusively through the Chats view.
func (s *TaskService) ListTasks() ([]task.Task, error) {
	all, err := s.tasks.List()
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for i := range all {
		if all[i].TaskType == task.TaskTypeChat {
			continue
		}
		out = append(out, all[i])
	}
	return out, nil
}

// GetTask returns a single task by ID.
func (s *TaskService) GetTask(id string) (task.Task, error) {
	t, err := s.tasks.Get(id)
	if err != nil {
		return t, err
	}
	return s.withEstimatedAgentRunCosts(t), nil
}

func (s *TaskService) withEstimatedAgentRunCosts(t task.Task) task.Task {
	if len(t.AgentRuns) == 0 {
		return t
	}
	for i := range t.AgentRuns {
		run := &t.AgentRuns[i]
		if run.CostUSD > 0 || run.LogFile == "" {
			continue
		}
		estimate, ok := estimateAgentRunUsage(*run)
		if !ok {
			if s.logger != nil {
				s.logger.Debug("task.agent-run-cost.estimate-skipped", "task_id", t.ID, "agent_id", run.AgentID, "log", run.LogFile)
			}
			continue
		}
		if estimate.PremiumRequests > 0 {
			run.PremiumRequests = estimate.PremiumRequests
		}
		run.CostUSD = estimate.CostUSD
		if estimate.CostUSD > 0 && s.tasks != nil {
			updates := map[string]any{"cost_usd": estimate.CostUSD}
			if estimate.PremiumRequests > 0 {
				updates["premium_requests"] = estimate.PremiumRequests
			}
			if run.Provider == "" && estimate.Provider != "" {
				updates["provider"] = estimate.Provider
				run.Provider = estimate.Provider
			}
			if err := s.tasks.UpdateRun(t.ID, run.AgentID, updates); err != nil && s.logger != nil {
				s.logger.Debug("task.agent-run-cost.persist-skipped", "task_id", t.ID, "agent_id", run.AgentID, "err", err)
			}
		}
	}
	return t
}

type agentRunUsageEstimate struct {
	CostUSD         float64
	PremiumRequests float64
	Provider        string
}

func estimateAgentRunUsage(run task.AgentRun) (agentRunUsageEstimate, bool) {
	for _, provider := range providersForRun(run) {
		events, err := agent.ParseLogFile(run.LogFile, 0, provider)
		if err != nil {
			continue
		}
		estimate, ok := estimateUsageFromEvents(run.Model, provider, events)
		if ok {
			return estimate, true
		}
	}
	return agentRunUsageEstimate{}, false
}

func estimateUsageFromEvents(model, provider string, events []agent.StreamEvent) (agentRunUsageEstimate, bool) {
	var input, output, cacheCreate, cacheRead, reasoning int
	var cost, premiumRequests float64
	var resultSeen bool
	for j := range events {
		if events[j].Type != "result" {
			continue
		}
		resultSeen = true
		cost += events[j].CostUSD
		premiumRequests += events[j].PremiumRequests
		input += events[j].InputTokens
		output += events[j].OutputTokens
		cacheCreate += events[j].CacheCreationInputTokens
		cacheRead += events[j].CacheReadInputTokens
		reasoning += events[j].ReasoningTokens
	}
	if !resultSeen {
		return agentRunUsageEstimate{}, false
	}
	if cost == 0 {
		switch provider {
		case "copilot":
			cost = stats.EstimateCopilotCost(premiumRequests)
		case "codex", "claude":
			cost = stats.EstimateCostDetailed(model, input, output, cacheCreate, cacheRead, reasoning)
		}
	}
	if cost == 0 && premiumRequests == 0 {
		return agentRunUsageEstimate{}, false
	}
	return agentRunUsageEstimate{CostUSD: cost, PremiumRequests: premiumRequests, Provider: provider}, true
}

func providersForRun(run task.AgentRun) []string {
	if run.Provider != "" {
		return []string{run.Provider}
	}
	preferred := providerForRun(run)
	providers := make([]string, 0, 3)
	if preferred != "" {
		providers = append(providers, preferred)
	}
	for _, provider := range []string{"codex", "copilot", "claude"} {
		if provider != preferred {
			providers = append(providers, provider)
		}
	}
	return providers
}

func providerForRun(run task.AgentRun) string {
	if run.Provider != "" {
		return run.Provider
	}
	model := run.Model
	if i := strings.LastIndexByte(model, '/'); i >= 0 {
		model = model[i+1:]
	}
	if strings.HasPrefix(model, "gpt-") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4") {
		return "codex"
	}
	if strings.HasPrefix(model, "claude-") || model == "sonnet" || model == "opus" || model == "haiku" {
		return "claude"
	}
	return ""
}

// CreateTask creates a new task and starts a matching workflow.
// If the title is a GitHub issue URL, fetches real title/body from GitHub.
func (s *TaskService) CreateTask(title, body, mode string) (task.Task, error) {
	prRepo, prNumber := github.ParsePRURL(title)
	issueRepo, issueNumber := github.ParseIssueURL(title)
	isURLStub := prRepo != "" || issueRepo != ""

	// A URL stub is created with the enrich-pending marker so the emit-path
	// task.created dispatch can't race async enrichment and start a flat
	// workflow on the un-enriched stub (CreateFull persists the tag before
	// emitting TaskCreated). Non-URL tasks take the plain create path.
	var t task.Task
	var err error
	if isURLStub {
		t, err = s.tasks.CreateFull(title, body, mode, task.Update{Tags: task.Ptr([]string{enrichPendingTag})})
	} else {
		t, err = s.tasks.Create(title, body, mode)
	}
	if err != nil {
		return t, err
	}

	if prRepo != "" {
		s.wg.Go(func() {
			s.enrichFromPR(t.ID, prRepo, prNumber)
		})
	} else if issueRepo != "" {
		s.wg.Go(func() {
			s.enrichFromIssue(t.ID, issueRepo, issueNumber)
		})
	}
	if s.audit != nil {
		_ = s.audit.Log(audit.Event{
			Type:   audit.EventTaskCreated,
			TaskID: t.ID,
			Data:   map[string]any{"title": title, "mode": mode},
		})
	}
	// A URL stub is dispatched by its enrich step (after the marker clears);
	// only plain tasks start their workflow here.
	if !isURLStub {
		s.startCreatedWorkflow(t)
	}
	return t, nil
}

func (s *TaskService) startCreatedWorkflow(t task.Task) {
	if s.workflowEngine == nil || t.Status != task.StatusTodo {
		return
	}
	// pr-fix / ordinary existing-PR tasks are driven outside task.created.
	// Explicit handoff entry points are the exception: they intentionally route
	// through task.created even when a PR number is already known.
	if skipTaskCreatedWorkflow(t) {
		return
	}
	info := taskToInfo(t)
	if def := s.workflowEngine.MatchWorkflow(info, "task.created"); def != nil {
		s.logger.Info("workflow.auto-start", "task_id", t.ID, "workflow", def.ID)
		s.wg.Go(func() {
			if wfErr := s.workflowEngine.StartWorkflow(t.ID, def.ID); wfErr != nil {
				s.logger.Error("workflow.auto-start.failed", "task_id", t.ID, "err", wfErr)
			}
		})
	}
}

// UpdateTask applies field updates to a task. The workflow engine drives
// all status-based transitions; this method only handles cleanup on done.
//
// Moving a task to "testing" is refused if another workflow is still active —
// the testing workflow needs a clean slate (no in-flight agents or pending
// human steps) so the user can't accidentally lose context by dragging.
//
// Moving a task to "in-progress" when its workflow is terminal (completed or
// failed) and no agent is running restarts the workflow — allowing the user to
// retry implementation after a human-required escalation.
func (s *TaskService) UpdateTask(id string, updates map[string]any) (task.Task, error) {
	cur, _ := s.tasks.Get(id)

	if status, ok := updates["status"].(string); ok {
		// Reject status regressions while an agent is running on this task.
		// Moving back to todo/new/done/cancelled while an agent is active loses in-flight work.
		agentBlockedStatuses := map[string]bool{
			string(task.StatusNew):       true,
			string(task.StatusTodo):      true,
			string(task.StatusDone):      true,
			string(task.StatusCancelled): true,
		}
		if agentBlockedStatuses[status] && s.agents.HasRunningAgentForTask(id) {
			return cur, conflictError(fmt.Sprintf("cannot move to %q: stop the running agent first", status))
		}

		if status == string(task.StatusTesting) {
			if cur.Workflow != nil &&
				cur.Workflow.State != workflow.ExecCompleted &&
				cur.Workflow.State != workflow.ExecFailed {
				return cur, conflictError(fmt.Sprintf("cannot move to testing: task has active workflow %q (state=%s)",
					cur.Workflow.WorkflowID, cur.Workflow.State))
			}
		}
	}
	t, err := s.tasks.UpdateMap(id, updates)
	if err != nil {
		return t, err
	}
	if task.IsTerminalStatus(t.Status) {
		s.wg.Go(func() {
			s.worktrees.Remove(t.ID)
			if s.sandboxes != nil {
				s.sandboxes.Stop(t.ID)
			}
		})
	}

	// When manually moved to in-progress with a terminal workflow and no live
	// agent, dispatch via task.status_changed so the trigger system picks the
	// right workflow for the new status. Naively restarting cur.Workflow.WorkflowID
	// would replay whatever flow ran before — for tasks created on the
	// pre-split monolithic `simple-task` (commit 3764ed9) this re-ran triage
	// and flipped status back to `planning` instead of running implement.
	// DispatchEvent matches against current trigger conditions, which is what
	// the user wants: in-progress → simple-task-implement.
	if s.workflowEngine != nil {
		if newStatus, ok := updates["status"].(string); ok &&
			newStatus == string(task.StatusInProgress) &&
			cur.Workflow != nil &&
			(cur.Workflow.State == workflow.ExecCompleted || cur.Workflow.State == workflow.ExecFailed) &&
			!s.agents.HasRunningAgentForTask(id) {
			s.logger.Info("workflow.restart", "task_id", id, "from_workflow", cur.Workflow.WorkflowID, "status", newStatus)
			s.wg.Go(func() {
				dispatched, wfErr := s.workflowEngine.DispatchEvent(
					id,
					"task.status_changed",
					map[string]string{"task.status": newStatus},
					nil,
				)
				if wfErr != nil {
					s.logger.Error("workflow.restart.failed", "task_id", id, "err", wfErr)
					return
				}
				if dispatched == "" {
					s.logger.Warn("workflow.restart.no-match", "task_id", id, "status", newStatus)
				}
			})
		}
	}

	return t, nil
}

// DeleteTask removes a task file from disk and cleans up its worktree.
func (s *TaskService) DeleteTask(id string) error {
	s.logger.Info("task.delete", "task_id", id)
	s.agents.KillAgentsForTask(id, 10*time.Second)
	if s.sandboxes != nil {
		s.sandboxes.Stop(id)
	}
	s.worktrees.Remove(id)
	if s.audit != nil {
		_ = s.audit.Log(audit.Event{
			Type:   audit.EventTaskDeleted,
			TaskID: id,
		})
	}
	if err := s.tasks.Delete(id); err != nil {
		s.logger.Error("task.delete.failed", "task_id", id, "err", err)
		return err
	}
	return nil
}

// enrichFromPR fetches a GitHub PR and updates the task.
// If the PR was authored by the current viewer, moves to in-review for PR monitoring.
// Otherwise, starts a headless review agent with /staff-code-review.
func (s *TaskService) enrichFromPR(taskID, repo string, number int) {
	pr, err := s.fetchPRFunc()(repo, number)
	if err != nil {
		s.logger.Error("enrich-pr.fetch", "task_id", taskID, "repo", repo, "number", number, "err", err)
		return
	}
	viewer := s.viewerLoginFunc()()

	slug := task.Slugify(pr.Title)
	u := task.Update{
		Title:     task.Ptr(pr.Title),
		ProjectID: task.Ptr(repo),
		PRNumber:  task.Ptr(pr.Number),
		Branch:    task.Ptr(pr.HeadRefName),
		Slug:      task.Ptr(slug),
	}
	// Replace tags with the PR's labels (possibly empty), which also clears the
	// enrich-pending marker set on the URL stub at creation.
	labels := pr.Labels
	u.Tags = &labels

	isMyPR := viewer != "" && strings.EqualFold(pr.Author, viewer)
	if isMyPR {
		u.Status = task.Ptr(task.StatusInReview)
		if _, err := s.tasks.Update(taskID, u); err != nil {
			s.logger.Error("enrich-pr.update", "task_id", taskID, "err", err)
			return
		}
		s.logger.Info("enrich-pr.my-pr", "task_id", taskID, "pr", number, "title", pr.Title)
		return
	}

	// Not my PR: add review tag and start review agent.
	labels = append(labels, "review")
	u.Tags = &labels
	if _, err := s.tasks.Update(taskID, u); err != nil {
		s.logger.Error("enrich-pr.update", "task_id", taskID, "err", err)
		return
	}
	t, err := s.tasks.Get(taskID)
	if err != nil {
		s.logger.Error("enrich-pr.get", "task_id", taskID, "err", err)
		return
	}
	if err := s.startPRReviewAgent(t); err != nil {
		s.logger.Error("enrich-pr.review-agent", "task_id", taskID, "err", err)
	}
}

// startPRReviewAgent starts a headless agent that runs /staff-code-review on the PR.
func (s *TaskService) startPRReviewAgent(t task.Task) error {
	posture, postureErr := resolveHeadlessPermissionMode(t, s.cfg)
	if postureErr != nil {
		return postureErr
	}

	dir := config.HomeDir()
	if t.ProjectID != "" {
		d, err := s.worktrees.PrepareForReview(t)
		if err != nil {
			s.logger.Warn("enrich-pr.worktree", "task_id", t.ID, "err", err)
		} else {
			dir = d
		}
	}

	prompt := fmt.Sprintf("Run /staff-code-review on https://github.com/%s/pull/%d", t.ProjectID, t.PRNumber)
	ag, err := s.agents.Run(agent.RunConfig{
		TaskID:                 t.ID,
		Name:                   agent.RoleReview.AgentName(t.Title),
		Mode:                   "headless",
		Prompt:                 prompt,
		Dir:                    dir,
		Model:                  "opus",
		HeadlessPermissionMode: posture,
		// MaxTurns intentionally not inherited: review agents need
		// enough turns to fetch the PR, run the skill, and write findings.
	})
	if err != nil {
		return err
	}
	if err := s.tasks.AddRun(t.ID, task.AgentRun{
		AgentID:   ag.ID,
		Role:      string(agent.RoleReview),
		Mode:      "headless",
		State:     string(agent.StateRunning),
		StartedAt: ag.StartedAt,
		Prompt:    prompt,
	}); err != nil {
		s.logger.Error("task.add-run", "task_id", t.ID, "err", err)
	}
	if _, err := s.tasks.Update(t.ID, task.Update{Status: task.Ptr(task.StatusInReview)}); err != nil {
		s.logger.Error("enrich-pr.status", "task_id", t.ID, "err", err)
	}
	s.logger.Info("enrich-pr.review-started", "task_id", t.ID, "agent_id", ag.ID, "pr", t.PRNumber)
	return nil
}

// enrichFromIssue fetches a GitHub issue and updates the task with real title/body.
func (s *TaskService) enrichFromIssue(taskID, repo string, number int) {
	issue, err := s.fetchIssueFunc()(repo, number)
	if err != nil {
		s.logger.Error("enrich-issue.fetch", "task_id", taskID, "repo", repo, "number", number, "err", err)
		return
	}

	// An umbrella issue must not become a flat implementation task. Expand it
	// into a gated child DAG (the expander builds its own tracker) and drop the
	// stub. This mirrors the poll fetcher's pass-1 detection so manual "paste
	// issue URL" creation and background polling converge on identical handling
	// — previously a manually-added umbrella was triaged and implemented as one
	// flat task, ignoring its sub-issues entirely.
	if s.umbrellaExpansionEnabled() && umbrella.IsUmbrellaIssue(issue.Title, issue.Labels) {
		s.expandUmbrellaStub(taskID, repo, issue)
		return
	}

	slug := task.Slugify(issue.Title)
	u := task.Update{
		Title:     task.Ptr(issue.Title),
		Issue:     task.Ptr(issue.URL),
		ProjectID: task.Ptr(repo),
		Slug:      task.Ptr(slug),
	}
	if issue.Body != "" {
		u.Body = task.Ptr(issue.Body)
	}
	// Replace tags with the issue's labels (possibly empty), which also clears
	// the enrich-pending marker so startCreatedWorkflow below can dispatch.
	labels := issue.Labels
	u.Tags = &labels
	linkedPRs, linkedErr := s.fetchIssueLinkedPRsFunc()(repo, number)
	if linkedErr != nil {
		s.logger.Warn("enrich-issue.linked-prs", "task_id", taskID, "repo", repo, "number", number, "err", linkedErr)
	} else if linked, ok := s.singleViewerLinkedPR(linkedPRs); ok {
		u.PRNumber = task.Ptr(linked.Number)
		u.Branch = task.Ptr(linked.HeadRefName)
		u.Status = task.Ptr(task.StatusInReview)
	} else if len(linkedPRs) > 0 {
		if viewerPRs := s.viewerLinkedPRCount(linkedPRs); viewerPRs > 1 {
			s.logger.Warn("enrich-issue.linked-prs.ambiguous", "task_id", taskID, "count", viewerPRs)
		}
	}
	updated, err := s.tasks.Update(taskID, u)
	if err != nil {
		s.logger.Error("enrich-issue.update", "task_id", taskID, "err", err)
		return
	}
	if linkedErr == nil && len(linkedPRs) == 0 {
		s.startCreatedWorkflow(updated)
	}
	s.logger.Info("enrich-issue.done", "task_id", taskID, "title", issue.Title)
}

// umbrellaExpansionEnabled reports whether a detected umbrella issue should be
// auto-expanded on the manual-create path. Read live (not wired-once) so a
// config reload toggling umbrella.enabled takes effect without re-wiring.
func (s *TaskService) umbrellaExpansionEnabled() bool {
	return s.cfg != nil && s.cfg.Umbrella.Enabled && s.umbrellaExpand != nil
}

// expandUmbrellaStub expands a manually-created stub whose URL resolved to a
// ☂️ umbrella issue into a gated child DAG, then deletes the stub — the
// expander creates its own umbrella-typed tracker, so keeping the stub would
// leave a duplicate flat task for the same issue. On expansion failure the stub
// is enriched into an identifiable (but inert) task so the user is not left
// empty-handed and can retry with `sybra-cli umbrella <url>`; crucially no flat
// workflow is started on a known umbrella, which is the bug this path fixes.
func (s *TaskService) expandUmbrellaStub(taskID, repo string, issue github.Issue) {
	res, err := s.umbrellaExpand(issue.URL)
	if err != nil {
		s.logger.Error("enrich-issue.umbrella-expand", "task_id", taskID, "issue", issue.URL, "err", err)
		s.enrichInertUmbrellaStub(taskID, repo, issue,
			"umbrella expansion failed; retry with `sybra-cli umbrella <url>`")
		return
	}
	// Expand created the real tracker + children; the stub is now a duplicate.
	// Use DeleteTask (not the raw store Delete) so any agent/sandbox/worktree
	// that started on the stub — e.g. if a flat workflow won the create race
	// before the enrich-pending marker took effect — is torn down, not leaked.
	if delErr := s.DeleteTask(taskID); delErr != nil {
		s.logger.Error("enrich-issue.umbrella-stub-delete", "task_id", taskID, "err", delErr)
		// Cleanup failed: enrich the stub so it is an identifiable,
		// user-deletable duplicate rather than a raw-URL task with no metadata.
		s.enrichInertUmbrellaStub(taskID, repo, issue,
			"umbrella expanded to a separate tracker; this duplicate can be deleted")
		return
	}
	s.logger.Info("enrich-issue.umbrella-expanded", "issue", issue.URL, "created", res.Created, "stub", taskID)
}

// enrichInertUmbrellaStub turns the stub into an identifiable, inert task: real
// title/body/issue plus the issue's labels as tags (mirroring the normal
// enrichFromIssue path so tag-driven routing and the UI still recognize it),
// and a StatusReason explaining why no workflow started. No flat workflow is
// started — the task is a known umbrella.
func (s *TaskService) enrichInertUmbrellaStub(taskID, repo string, issue github.Issue, reason string) {
	u := task.Update{
		Title:        task.Ptr(issue.Title),
		Issue:        task.Ptr(issue.URL),
		ProjectID:    task.Ptr(repo),
		Slug:         task.Ptr(task.Slugify(issue.Title)),
		StatusReason: task.Ptr(reason),
	}
	if issue.Body != "" {
		u.Body = task.Ptr(issue.Body)
	}
	// Replace tags with the issue's labels (possibly empty), preserving them for
	// identification/routing while clearing the enrich-pending marker.
	labels := issue.Labels
	u.Tags = &labels
	if _, err := s.tasks.Update(taskID, u); err != nil {
		s.logger.Error("enrich-issue.umbrella-stub-enrich", "task_id", taskID, "err", err)
	}
}

func (s *TaskService) fetchPRFunc() func(string, int) (github.PullRequest, error) {
	if s.fetchPR != nil {
		return s.fetchPR
	}
	return github.FetchPR
}

func (s *TaskService) fetchIssueFunc() func(string, int) (github.Issue, error) {
	if s.fetchIssue != nil {
		return s.fetchIssue
	}
	return github.FetchIssue
}

func (s *TaskService) fetchIssueLinkedPRsFunc() func(string, int) ([]github.PullRequest, error) {
	if s.fetchIssueLinkedPRs != nil {
		return s.fetchIssueLinkedPRs
	}
	return github.FetchIssueLinkedPRs
}

func (s *TaskService) viewerLoginFunc() func() string {
	if s.viewerLogin != nil {
		return s.viewerLogin
	}
	return github.ViewerLogin
}

func (s *TaskService) singleViewerLinkedPR(prs []github.PullRequest) (github.PullRequest, bool) {
	viewer := s.viewerLoginFunc()()
	if viewer == "" {
		return github.PullRequest{}, false
	}
	var mine []github.PullRequest
	for i := range prs {
		if strings.EqualFold(prs[i].Author, viewer) {
			mine = append(mine, prs[i])
		}
	}
	if len(mine) != 1 {
		return github.PullRequest{}, false
	}
	return mine[0], true
}

func (s *TaskService) viewerLinkedPRCount(prs []github.PullRequest) int {
	viewer := s.viewerLoginFunc()()
	if viewer == "" {
		return 0
	}
	var count int
	for i := range prs {
		if strings.EqualFold(prs[i].Author, viewer) {
			count++
		}
	}
	return count
}
