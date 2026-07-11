package sybra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/sandbox"
	"github.com/Automaat/sybra/internal/stats"
	"github.com/Automaat/sybra/internal/sybra/agentorch"
	"github.com/Automaat/sybra/internal/sybra/review"
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
	artifacts           *artifact.Store
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
	// deleteTask allows tests to force DeleteTask failures on cleanup branches
	// without mutating the real task store or broadening the public API.
	deleteTask func(id string) error
	// enrichReconciling holds task IDs with an in-flight enrichment goroutine —
	// either the original CreateTask async fetch or a reconcile-pass retry — so
	// a maintenance tick never stacks a second concurrent gh fetch on the same
	// stub. Zero value ready.
	enrichReconciling sync.Map
	// enrichRetryCooldown holds the next maintenance retry time per task ID.
	// Initial async enrichment is not gated; this only prevents a permanently
	// broken stub from spending GitHub calls on every reconcile tick.
	enrichRetryCooldown sync.Map
}

const enrichPendingRetryCooldown = time.Hour

type TamperReportDTO struct {
	TaskID          string             `json:"taskId"`
	ReportAvailable bool               `json:"reportAvailable"`
	Base            string             `json:"base"`
	Range           string             `json:"range"`
	Files           []string           `json:"files"`
	Findings        []TamperFindingDTO `json:"findings"`
}

type TamperFindingDTO struct {
	File     string `json:"file"`
	Category string `json:"category"`
	Severity string `json:"severity"`
	Rule     string `json:"rule"`
	Detail   string `json:"detail"`
}

type TaskArtifactDTO struct {
	artifact.Meta
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

type TaskSetupLogDTO struct {
	TaskID    string `json:"taskId"`
	Path      string `json:"path,omitempty"`
	Exists    bool   `json:"exists"`
	Content   string `json:"content,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type TaskAuditEventDTO struct {
	Timestamp time.Time      `json:"ts"`
	Type      string         `json:"type"`
	TaskID    string         `json:"taskId,omitempty"`
	AgentID   string         `json:"agentId,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

const taskDiagnosticReadLimit = 256 * 1024

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

func (s *TaskService) ListTaskArtifacts(taskID string) ([]TaskArtifactDTO, error) {
	if s.artifacts == nil {
		return []TaskArtifactDTO{}, nil
	}
	metas, err := s.artifacts.List(taskID)
	if err != nil {
		return nil, err
	}
	out := make([]TaskArtifactDTO, 0, len(metas))
	for i := range metas {
		meta := metas[i]
		meta.SourcePath = ""
		dto := TaskArtifactDTO{Meta: meta}
		data, _, readErr := s.artifacts.Read(taskID, meta.Name)
		if readErr != nil {
			dto.Error = readErr.Error()
			out = append(out, dto)
			continue
		}
		if len(data) > taskDiagnosticReadLimit {
			if meta.Stream {
				data = data[len(data)-taskDiagnosticReadLimit:]
			} else {
				data = data[:taskDiagnosticReadLimit]
			}
			dto.Error = fmt.Sprintf("truncated to %d bytes", taskDiagnosticReadLimit)
		}
		dto.Content = string(data)
		out = append(out, dto)
	}
	return out, nil
}

func (s *TaskService) GetTaskSetupLog(taskID string) (TaskSetupLogDTO, error) {
	dto := TaskSetupLogDTO{TaskID: taskID}
	if s.cfg == nil || s.cfg.Logging.Dir == "" {
		return dto, nil
	}
	path := filepath.Join(s.cfg.Logging.Dir, "worktrees", taskID+"-setup.log")
	root := filepath.Join(s.cfg.Logging.Dir, "worktrees")
	cleanRoot := filepath.Clean(root) + string(filepath.Separator)
	cleanPath := filepath.Clean(path)
	if !strings.HasPrefix(cleanPath, cleanRoot) {
		return dto, fmt.Errorf("setup log path escapes log root")
	}
	dto.Path = cleanPath
	data, err := os.ReadFile(cleanPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return dto, nil
		}
		return dto, fmt.Errorf("read setup log: %w", err)
	}
	dto.Exists = true
	if len(data) > taskDiagnosticReadLimit {
		data = data[len(data)-taskDiagnosticReadLimit:]
		dto.Truncated = true
	}
	dto.Content = string(data)
	return dto, nil
}

func (s *TaskService) ListTaskAuditEvents(taskID string, days int) ([]TaskAuditEventDTO, error) {
	if s.cfg == nil || s.cfg.Logging.Dir == "" {
		return []TaskAuditEventDTO{}, nil
	}
	if days <= 0 || days > 90 {
		days = 14
	}
	until := time.Now().UTC().Add(time.Minute)
	since := until.AddDate(0, 0, -days)
	events, err := audit.Read(s.cfg.AuditDir(), audit.Query{
		Since:  since,
		Until:  until,
		TaskID: taskID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]TaskAuditEventDTO, 0, len(events))
	for _, ev := range events {
		out = append(out, TaskAuditEventDTO{
			Timestamp: ev.Timestamp,
			Type:      ev.Type,
			TaskID:    ev.TaskID,
			AgentID:   ev.AgentID,
			Data:      ev.Data,
		})
	}
	slices.Reverse(out)
	return out, nil
}

// GetTamperReport returns the detector report artifact for a tamper-flagged task.
func (s *TaskService) GetTamperReport(taskID string) (TamperReportDTO, error) {
	if s.artifacts == nil {
		return emptyTamperReport(taskID), nil
	}
	data, _, err := s.artifacts.Read(taskID, "tamper-report.json")
	if err != nil {
		if isMissingArtifactError(err) {
			return emptyTamperReport(taskID), nil
		}
		return TamperReportDTO{}, err
	}
	var report TamperReportDTO
	if err := json.Unmarshal(data, &report); err != nil {
		return TamperReportDTO{}, fmt.Errorf("parse tamper report: %w", err)
	}
	report.TaskID = taskID
	report.ReportAvailable = true
	if report.Files == nil {
		report.Files = []string{}
	}
	if report.Findings == nil {
		report.Findings = []TamperFindingDTO{}
	}
	return report, nil
}

// ListTaskProgress returns the agent-authored progress entries for a task.
// Empty (not an error) when the task has no progress log yet.
func (s *TaskService) ListTaskProgress(taskID string) ([]artifact.ProgressEntry, error) {
	if s.artifacts == nil {
		return []artifact.ProgressEntry{}, nil
	}
	entries, err := s.artifacts.ReadProgress(taskID)
	if err != nil {
		return nil, err
	}
	if entries == nil {
		return []artifact.ProgressEntry{}, nil
	}
	return entries, nil
}

func emptyTamperReport(taskID string) TamperReportDTO {
	return TamperReportDTO{
		TaskID:          taskID,
		ReportAvailable: false,
		Files:           []string{},
		Findings:        []TamperFindingDTO{},
	}
}

func isMissingArtifactError(err error) bool {
	return errors.Is(err, artifact.ErrNotFound)
}

// BlessTampering records a human bless for a tamper-flagged task and sends it
// back to the review workflow.
func (s *TaskService) BlessTampering(taskID string) (task.Task, error) {
	var (
		cur      task.Task
		tagAdded bool
	)
	updated, err := s.tasks.UpdateFn(taskID, func(t task.Task) (task.Update, error) {
		if !t.TamperFlagged {
			return task.Update{}, conflictError(
				"task is not tamper-flagged: bless requires status=human-required with a " +
					"status_reason starting with " + strconv.Quote(workflow.TamperFlaggedReasonPrefix),
			)
		}
		cur = t

		merged := append([]string(nil), t.Tags...)
		if !slices.Contains(merged, workflow.TamperBlessedTag) {
			merged = append(merged, workflow.TamperBlessedTag)
			tagAdded = true
		}
		return task.Update{
			Tags:   task.Ptr(merged),
			Status: task.Ptr(task.StatusReadyReview),
		}, nil
	})
	if err != nil {
		return updated, err
	}

	report, reportErr := s.GetTamperReport(taskID)
	reportAvailable := reportErr == nil && report.ReportAvailable
	findingCount := 0
	highSeverityFindingCount := 0
	if reportErr == nil {
		findingCount = len(report.Findings)
		for i := range report.Findings {
			if report.Findings[i].Severity == "high" {
				highSeverityFindingCount++
			}
		}
	} else if s.logger != nil {
		s.logger.Warn("task.tamper-report.audit-skipped", "task_id", taskID, "err", reportErr)
	}

	if s.audit != nil {
		if logErr := s.audit.Log(audit.Event{
			Type:   audit.EventTaskTamperBlessed,
			TaskID: taskID,
			Data: map[string]any{
				"previousStatus":           string(cur.Status),
				"previousStatusReason":     cur.StatusReason,
				"reportAvailable":          reportAvailable,
				"findingCount":             findingCount,
				"highSeverityFindingCount": highSeverityFindingCount,
				"tagAdded":                 tagAdded,
			},
		}); logErr != nil && s.logger != nil {
			s.logger.Warn("task.tamper-bless.audit", "task_id", taskID, "err", logErr)
		}
	}
	return updated, nil
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
			patch := task.RunPatch{CostUSD: task.Ptr(estimate.CostUSD)}
			if estimate.PremiumRequests > 0 {
				patch.PremiumRequests = task.Ptr(estimate.PremiumRequests)
			}
			if run.Provider == "" && estimate.Provider != "" {
				patch.Provider = task.Ptr(estimate.Provider)
				run.Provider = estimate.Provider
			}
			if err := s.tasks.UpdateRun(t.ID, run.AgentID, patch); err != nil && s.logger != nil {
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
		estimate, ok := estimateUsageFromEvents(run.Model, provider, events, run.StartedAt)
		if ok {
			return estimate, true
		}
	}
	return agentRunUsageEstimate{}, false
}

func estimateUsageFromEvents(model, provider string, events []agent.StreamEvent, startedAt time.Time) (agentRunUsageEstimate, bool) {
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
			cost = stats.EstimateCostDetailed(model, input, output, cacheCreate, cacheRead, reasoning, startedAt)
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
	return s.CreateTaskWithInit(title, body, mode, task.Update{})
}

// CreateTaskWithInit is CreateTask plus caller-supplied initial field
// overrides (e.g. TodoistID) applied atomically in the same first-write as
// task creation. Callers that need a dedupe key persisted alongside the task
// — so a crash between create and a second update can never re-import the
// same source item — should use this instead of CreateTask followed by a
// separate Update.
func (s *TaskService) CreateTaskWithInit(title, body, mode string, init task.Update) (task.Task, error) {
	prRepo, prNumber := github.ParsePRURL(title)
	issueRepo, issueNumber := github.ParseIssueURL(title)
	isURLStub := prRepo != "" || issueRepo != ""

	// A URL stub is created with the enrich-pending marker so the emit-path
	// task.created dispatch can't race async enrichment and start a flat
	// workflow on the un-enriched stub (CreateFull persists the tag before
	// emitting TaskCreated). Non-URL tasks take the plain create path.
	createInit := init
	if isURLStub {
		tags := []string{enrichPendingTag}
		if init.Tags != nil {
			tags = append(tags, (*init.Tags)...)
		}
		createInit.Tags = task.Ptr(tags)
	}
	t, err := s.tasks.CreateFull(title, body, mode, createInit)
	if err != nil {
		return t, err
	}

	if prRepo != "" {
		s.enrichReconciling.Store(t.ID, struct{}{})
		s.wg.Go(func() {
			defer s.enrichReconciling.Delete(t.ID)
			s.enrichFromPR(t.ID, prRepo, prNumber)
		})
	} else if issueRepo != "" {
		s.enrichReconciling.Store(t.ID, struct{}{})
		s.wg.Go(func() {
			defer s.enrichReconciling.Delete(t.ID)
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
			// context.Background(): UpdateTask is a Wails-bound method; this
			// runs in a detached background goroutine with no ctx to thread.
			s.worktrees.Remove(context.Background(), t.ID)
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
				dispatched, wfErr := s.redispatchStatusChanged(id, newStatus)
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

// redispatchStatusChanged dispatches task.status_changed for the given task
// and status, so trigger conditions in workflow YAML select the workflow
// matching the new status rather than replaying a stale saved workflow ID.
// Shared by UpdateTask's async in-progress restart goroutine and
// DispatchFromHumanRequired's synchronous dispatch.
func (s *TaskService) redispatchStatusChanged(id, status string) (string, error) {
	if s.workflowEngine == nil {
		return "", errors.New("workflow engine not initialized")
	}
	return s.workflowEngine.DispatchEvent(
		id,
		"task.status_changed",
		map[string]string{"task.status": status},
		nil,
	)
}

// dispatchTargetSpec describes one status an operator can dispatch a
// human-required task to.
type dispatchTargetSpec struct {
	// requiresPR gates the target on the task already having a linked PR
	// (only in-review needs this — dispatching in-review without a PR would
	// flip the task into PR-monitoring with nothing to monitor).
	requiresPR bool
	// dispatches selects whether redispatchStatusChanged runs after the
	// status write. in-review has no task.status_changed trigger — it is a
	// plain PR-guarded status flip into the existing PR-monitoring state.
	dispatches bool
}

var dispatchTargets = map[string]dispatchTargetSpec{
	string(task.StatusInProgress): {dispatches: true},
	string(task.StatusTesting):    {dispatches: true},
	string(task.StatusReadyPR):    {dispatches: true},
	string(task.StatusInReview):   {requiresPR: true, dispatches: false},
}

// DispatchFromHumanRequired flips a task parked in human-required to target
// (one of in-progress/testing/ready-pr/in-review), recording reason as the
// audit-visible status_reason. For dispatching targets it synchronously
// re-enters the workflow via task.status_changed; on any failure to do so it
// fails closed, reverting the task to human-required with an explanatory
// status_reason so the operator is never left with a task silently stuck in
// a target status with no workflow driving it.
//
// The whole check-then-write sequence runs under the workflow engine's
// per-task human-action lock (shared with plan-review's
// HandleHumanActionRecovering), so a double-click or a second operator cannot
// race the same stuck task between the guard reads and the status write.
func (s *TaskService) DispatchFromHumanRequired(id, target, reason string) (task.Task, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return task.Task{}, conflictError("a decision reason is required")
	}
	if _, ok := dispatchTargets[target]; !ok {
		return task.Task{}, conflictError(fmt.Sprintf("unsupported dispatch target %q", target))
	}

	var result task.Task
	run := func() error {
		out, err := s.dispatchFromHumanRequiredLocked(id, target, reason)
		result = out
		return err
	}
	// s.workflowEngine is only nil in narrow tests; fall back to running
	// unlocked there. dispatchFromHumanRequiredLocked's dispatching targets
	// still go through redispatchStatusChanged, which itself guards against
	// a nil engine and fails closed rather than nil-panicking.
	if s.workflowEngine != nil {
		if err := s.workflowEngine.WithHumanActionLock(id, run); err != nil {
			return task.Task{}, err
		}
		return result, nil
	}
	if err := run(); err != nil {
		return task.Task{}, err
	}
	return result, nil
}

func (s *TaskService) dispatchFromHumanRequiredLocked(id, target, reason string) (task.Task, error) {
	cur, err := s.tasks.Get(id)
	if err != nil {
		return task.Task{}, err
	}
	if cur.Status != task.StatusHumanRequired {
		return task.Task{}, conflictError(fmt.Sprintf("task is not human-required (status=%q)", cur.Status))
	}
	spec := dispatchTargets[target]
	if spec.requiresPR && cur.PRNumber == 0 {
		return task.Task{}, conflictError(fmt.Sprintf("cannot dispatch to %q: task has no linked PR", target))
	}
	if s.agents.HasRunningAgentForTask(id) {
		return task.Task{}, conflictError("cannot dispatch: an agent is already running for this task")
	}
	if cur.Workflow != nil && cur.Workflow.State != workflow.ExecCompleted && cur.Workflow.State != workflow.ExecFailed {
		return task.Task{}, conflictError(fmt.Sprintf("cannot dispatch: task has active workflow %q (state=%s)",
			cur.Workflow.WorkflowID, cur.Workflow.State))
	}

	if _, err := s.tasks.UpdateMap(id, map[string]any{
		"status":        target,
		"status_reason": reason,
	}); err != nil {
		return task.Task{}, err
	}

	if spec.dispatches {
		matched, dispatchErr := s.redispatchStatusChanged(id, target)
		// The status write above synchronously fires App.initStatusHook, which
		// for testing/ready-pr already dispatches the workflow via
		// dispatchStatusWorkflow before UpdateMap returns. Our own dispatch then
		// observes that active workflow and returns ErrWorkflowAlreadyActive —
		// that is the hook having succeeded, not a conflict. Mirror
		// dispatchStatusWorkflow and treat it as benign; a genuine agent-start
		// failure surfaces as a different error and still fails closed.
		hookAlreadyStarted := errors.Is(dispatchErr, workflow.ErrWorkflowAlreadyActive)
		dispatchStarted := dispatchErr == nil && matched != ""
		if !dispatchStarted && !hookAlreadyStarted {
			failure := "no workflow matched"
			if dispatchErr != nil {
				failure = dispatchErr.Error()
			}
			s.logger.Error("task.dispatch.failed", "task_id", id, "target", target, "err", failure)
			revertReason := fmt.Sprintf("%s (dispatch to %s failed: %s)", reason, target, failure)
			if _, revertErr := s.tasks.UpdateMap(id, map[string]any{
				"status":        string(task.StatusHumanRequired),
				"status_reason": revertReason,
			}); revertErr != nil {
				s.logger.Error("task.dispatch.revert-failed", "task_id", id, "target", target, "err", revertErr)
				s.logDispatchAudit(id, target, string(cur.Status), reason, "revert-failed")
				return task.Task{}, fmt.Errorf("dispatch to %s failed (%s) and revert to human-required also failed: %w", target, failure, revertErr)
			}
			// The bounce back to human-required is the event an operator most
			// needs a durable record of — log it, not just the success path.
			s.logDispatchAudit(id, target, string(cur.Status), reason, "reverted")
			return task.Task{}, conflictError(fmt.Sprintf("dispatch to %q failed: %s", target, failure))
		}
	}

	s.logDispatchAudit(id, target, string(cur.Status), reason, "dispatched")
	return s.tasks.Get(id)
}

// logDispatchAudit records a human-required dispatch attempt and its outcome
// ("dispatched"/"reverted"/"revert-failed"). The operator's reason is included
// so the durable audit record carries the rationale even after status_reason is
// later overwritten.
func (s *TaskService) logDispatchAudit(id, target, previousStatus, reason, outcome string) {
	if s.audit == nil {
		return
	}
	if logErr := s.audit.Log(audit.Event{
		Type:   audit.EventTaskDispatched,
		TaskID: id,
		Data: map[string]any{
			"target":         target,
			"previousStatus": previousStatus,
			"reason":         reason,
			"outcome":        outcome,
		},
	}); logErr != nil && s.logger != nil {
		s.logger.Warn("task.dispatch.audit", "task_id", id, "err", logErr)
	}
}

// DeleteTask removes a task file from disk and cleans up its worktree.
func (s *TaskService) DeleteTask(id string) error {
	s.logger.Info("task.delete", "task_id", id)
	s.agents.KillAgentsForTask(id, 10*time.Second)
	if s.sandboxes != nil {
		s.sandboxes.Stop(id)
	}
	// context.Background(): DeleteTask is a Wails-bound method with no ctx.
	s.worktrees.Remove(context.Background(), id)
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
		Slug:      task.Ptr(slug),
	}
	if branch, ok := s.claimIngestBranch(repo, pr.HeadRefName, taskID); ok {
		u.Branch = task.Ptr(branch)
	} else {
		s.logger.Warn("enrich-pr.branch-collision", "task_id", taskID, "pr", number, "branch", pr.HeadRefName)
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
		s.enrichRetryCooldown.Delete(taskID)
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
		return
	}
	s.enrichRetryCooldown.Delete(taskID)
}

// startPRReviewAgent starts a headless agent that runs /staff-code-review on the PR.
func (s *TaskService) startPRReviewAgent(t task.Task) error {
	posture, postureErr := agentorch.ResolveHeadlessPermissionMode(t, s.cfg)
	if postureErr != nil {
		return postureErr
	}

	dir := config.HomeDir()
	if t.ProjectID != "" {
		// context.Background(): reached from CreateTask's async enrich-from-PR
		// goroutine (and the enrich-reconcile maintenance retry), both fired
		// from a Wails-bound entry point with no ctx to thread through.
		d, err := s.worktrees.PrepareForReview(context.Background(), t)
		if err != nil {
			s.logger.Warn("enrich-pr.worktree", "task_id", t.ID, "err", err)
		} else {
			dir = d
		}
	}

	prompt := fmt.Sprintf("Run /staff-code-review on https://github.com/%s/pull/%d", t.ProjectID, t.PRNumber)
	ag, err := s.agents.Run(review.StaffCodeReviewRunConfig(t, prompt, dir, posture))
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
	linkedPRs, linkedErr := s.fetchIssueLinkedPRsFunc()(repo, number)
	if linkedErr != nil {
		s.logger.Warn("enrich-issue.linked-prs", "task_id", taskID, "repo", repo, "number", number, "err", linkedErr)
		// Keep the enrich-pending marker on a warn-only linked-PR fetch
		// failure. Clearing it here would leave the task in todo with no
		// marker, no workflow dispatch, and no way for
		// ReconcilePendingEnrichment to find and retry it — inert forever.
		labels = append(labels, enrichPendingTag)
	} else if linked, ok := s.singleViewerLinkedPR(linkedPRs); ok {
		if branch, ok := s.claimIngestBranch(repo, linked.HeadRefName, taskID); ok {
			u.PRNumber = task.Ptr(linked.Number)
			u.Branch = task.Ptr(branch)
			u.Status = task.Ptr(task.StatusInReview)
		} else {
			// This PR's branch already belongs to a different task — e.g. one
			// PR closes more than one GitHub issue, and another issue's task
			// already claimed it. Leave this task's own PR link unclaimed
			// rather than have two tasks race to own the same branch/worktree.
			s.logger.Warn("enrich-issue.linked-pr.branch-collision", "task_id", taskID, "issue", issue.URL, "pr", linked.Number)
		}
	} else if len(linkedPRs) > 0 {
		if viewerPRs := s.viewerLinkedPRCount(linkedPRs); viewerPRs > 1 {
			s.logger.Warn("enrich-issue.linked-prs.ambiguous", "task_id", taskID, "count", viewerPRs)
		}
	}
	u.Tags = &labels
	updated, err := s.tasks.Update(taskID, u)
	if err != nil {
		s.logger.Error("enrich-issue.update", "task_id", taskID, "err", err)
		return
	}
	if linkedErr == nil && len(linkedPRs) == 0 {
		s.enrichRetryCooldown.Delete(taskID)
		s.startCreatedWorkflow(updated)
	} else if linkedErr == nil {
		s.enrichRetryCooldown.Delete(taskID)
	}
	s.logger.Info("enrich-issue.done", "task_id", taskID, "title", issue.Title)
}

// ReconcilePendingEnrichment re-attempts GitHub enrichment for URL stubs whose
// initial async enrichment never completed. A stub is created with the raw URL
// as its title and the enrich-pending marker; enrichment then runs in a
// fire-and-forget goroutine that, on any failure (a transient GitHub rate
// limit / gateway blip, or the app exiting before it finished), simply logs and
// returns. The marker is never cleared, so the stub keeps its raw-URL title
// forever and — because URL stubs only dispatch a workflow once the marker
// clears — never starts one. This is the recovery path for that orphaned state:
// the still-present marker is the retry signal, and the enrich functions are
// idempotent, so re-running them either succeeds now or fails the same benign
// way. Driven from the maintenance pass so recovery is continuous rather than a
// one-shot at startup.
func (s *TaskService) ReconcilePendingEnrichment() {
	all, err := s.tasks.List()
	if err != nil {
		s.logger.Error("enrich-reconcile.list", "err", err)
		return
	}
	for i := range all {
		t := all[i]
		if !slices.Contains(t.Tags, enrichPendingTag) {
			continue
		}
		// The user took the task out of the queue (e.g. cancelled/done); don't
		// spend a GitHub fetch reviving it.
		if task.IsTerminalStatus(t.Status) {
			continue
		}
		// An in-flight agent means the stub is already being worked; leave it be.
		if s.agents.HasRunningAgentForTask(t.ID) {
			continue
		}
		// The stub still carries its raw URL as the title; re-derive the target.
		prRepo, prNumber := github.ParsePRURL(t.Title)
		issueRepo, issueNumber := github.ParseIssueURL(t.Title)
		if prRepo == "" && issueRepo == "" && t.Issue != "" {
			// Title was already rewritten to the real issue title by a prior
			// enrich attempt that got the content but failed the linked-PR
			// fetch (marker deliberately kept — see enrichFromIssue). Fall
			// back to the persisted issue URL so the retry isn't stranded.
			issueRepo, issueNumber = github.ParseIssueURL(t.Issue)
		}
		if prRepo == "" && issueRepo == "" {
			// Title no longer parses as a GitHub URL (manually edited) yet the
			// marker lingers — nothing to fetch. Warn once per tick; leave the
			// task for the user to resolve.
			s.logger.Warn("enrich-reconcile.unparseable", "task_id", t.ID, "title", t.Title)
			continue
		}
		if s.enrichPendingRetryCoolingDown(t.ID) {
			continue
		}
		// Dedupe across ticks: skip if a reconcile enrichment is already running
		// for this task (a slow gh call can span more than one maintenance tick).
		if _, inFlight := s.enrichReconciling.LoadOrStore(t.ID, struct{}{}); inFlight {
			continue
		}
		id := t.ID
		s.enrichRetryCooldown.Store(id, time.Now().Add(enrichPendingRetryCooldown))
		s.logger.Info("enrich-reconcile.retry", "task_id", id, "title", t.Title)
		if prRepo != "" {
			repo, number := prRepo, prNumber
			s.wg.Go(func() {
				defer s.enrichReconciling.Delete(id)
				s.enrichFromPR(id, repo, number)
			})
			continue
		}
		repo, number := issueRepo, issueNumber
		s.wg.Go(func() {
			defer s.enrichReconciling.Delete(id)
			s.enrichFromIssue(id, repo, number)
		})
	}
}

func (s *TaskService) enrichPendingRetryCoolingDown(taskID string) bool {
	untilValue, ok := s.enrichRetryCooldown.Load(taskID)
	if !ok {
		return false
	}
	until, ok := untilValue.(time.Time)
	if !ok || until.IsZero() {
		s.enrichRetryCooldown.Delete(taskID)
		return false
	}
	if time.Now().Before(until) {
		return true
	}
	s.enrichRetryCooldown.Delete(taskID)
	return false
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
// leave a duplicate flat task for the same issue. On expansion failure, a
// planner failure (internal/umbrella.recordExpandFailure, see #1570) already
// created or updated a separate, durable tracker task carrying the failure
// detail — in that case the stub is marked a duplicate (mirroring the success
// path) rather than also claiming TaskTypeUmbrella, which would leave two
// tracker tasks for the same issue. A failure before any tracker existed
// (bad URL, GitHub fetch failure) has nothing else to defer to, so the stub
// itself becomes the identifiable (but inert) record instead.
func (s *TaskService) expandUmbrellaStub(taskID, repo string, issue github.Issue) {
	res, err := s.umbrellaExpand(issue.URL)
	if err != nil {
		s.logger.Error("enrich-issue.umbrella-expand", "task_id", taskID, "issue", issue.URL, "err", err)
		if s.umbrellaTrackerExistsElsewhere(taskID, issue.URL) {
			s.enrichDuplicateUmbrellaStub(taskID, repo, issue,
				"umbrella expansion failed; see the umbrella tracker task for details")
		} else {
			s.enrichInertUmbrellaStub(taskID, repo, issue,
				"umbrella expansion failed; retry with `sybra-cli umbrella <url>`")
		}
		return
	}
	// Expand created the real tracker + children; the stub is now a duplicate.
	// Use DeleteTask (not the raw store Delete) so any agent/sandbox/worktree
	// that started on the stub — e.g. if a flat workflow won the create race
	// before the enrich-pending marker took effect — is torn down, not leaked.
	if delErr := s.deleteTaskFunc()(taskID); delErr != nil {
		s.logger.Error("enrich-issue.umbrella-stub-delete", "task_id", taskID, "err", delErr)
		// Cleanup failed: enrich the stub so it is an identifiable,
		// user-deletable duplicate rather than a raw-URL task with no metadata.
		s.enrichDuplicateUmbrellaStub(taskID, repo, issue,
			"umbrella expanded to a separate tracker; this duplicate can be deleted")
		return
	}
	s.logger.Info("enrich-issue.umbrella-expanded", "issue", issue.URL, "created", res.Created, "stub", taskID)
}

// umbrellaTrackerExistsElsewhere reports whether some other task already
// carries TaskTypeUmbrella for issueURL. Used to decide, after a failed
// expandUmbrellaStub, whether that failure already has a durable tracker to
// point at (see recordExpandFailure) or whether this stub is the only record.
func (s *TaskService) umbrellaTrackerExistsElsewhere(taskID, issueURL string) bool {
	all, err := s.tasks.List()
	if err != nil {
		// Unreadable store: assume a tracker exists. Claiming this stub as the
		// only record would mint a second TaskTypeUmbrella task for the same
		// issue — the exact duplication this helper prevents; a mislabeled
		// duplicate stub is the cheaper failure.
		s.logger.Error("enrich-issue.umbrella-tracker-scan", "task_id", taskID, "err", err)
		return true
	}
	key := umbrella.NormalizeIssueRef(issueURL)
	for i := range all {
		t := &all[i]
		if t.ID != taskID && t.TaskType == task.TaskTypeUmbrella && umbrella.NormalizeIssueRef(t.Issue) == key {
			return true
		}
	}
	return false
}

// enrichInertUmbrellaStub turns the stub into an identifiable, inert task: real
// title/body/issue plus the issue's labels as tags (mirroring the normal
// enrichFromIssue path so tag-driven routing and the UI still recognize it),
// and a StatusReason explaining why no workflow started. No flat workflow is
// started — the task is a known umbrella.
//
// On the true expansion-failure path, TaskType is set to umbrella so the fix
// holds durably: every write to the task file re-fires the emit-path
// task.created dispatch, and the enrich-pending tag this call clears was the
// only guard skipTaskCreatedWorkflow had left to skip on. Duplicate-stub
// cleanup failures must not set TaskTypeUmbrella because a real tracker
// already exists for the same issue.
func (s *TaskService) enrichInertUmbrellaStub(taskID, repo string, issue github.Issue, reason string) {
	s.enrichUmbrellaStub(taskID, repo, issue, reason, true)
}

func (s *TaskService) enrichDuplicateUmbrellaStub(taskID, repo string, issue github.Issue, reason string) {
	s.enrichUmbrellaStub(taskID, repo, issue, reason, false)
}

func (s *TaskService) enrichUmbrellaStub(taskID, repo string, issue github.Issue, reason string, markUmbrella bool) {
	u := task.Update{
		Title:        task.Ptr(issue.Title),
		Issue:        task.Ptr(issue.URL),
		ProjectID:    task.Ptr(repo),
		Slug:         task.Ptr(task.Slugify(issue.Title)),
		StatusReason: task.Ptr(reason),
	}
	// Replace tags with the issue's labels (possibly empty), preserving them for
	// identification/routing while clearing the enrich-pending marker.
	labels := slices.Clone(issue.Labels)
	if markUmbrella {
		u.TaskType = task.Ptr(task.TaskTypeUmbrella)
	} else {
		// Not the tracker (a real one already exists elsewhere) — mark this
		// duplicate durably so skipTaskCreatedWorkflow keeps blocking dispatch
		// on it without also claiming TaskTypeUmbrella.
		labels = append(labels, umbrellaDuplicateTag)
	}
	if issue.Body != "" {
		u.Body = task.Ptr(issue.Body)
	}
	u.Tags = &labels
	if _, err := s.tasks.Update(taskID, u); err != nil {
		s.logger.Error("enrich-issue.umbrella-stub-enrich", "task_id", taskID, "err", err)
	}
}

func (s *TaskService) deleteTaskFunc() func(id string) error {
	if s.deleteTask != nil {
		return s.deleteTask
	}
	return s.DeleteTask
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

// claimIngestBranch returns (branch, true) unless branch is already the
// Branch of a different live task in projectID, in which case it returns
// ("", false) so the caller leaves the task's Branch unset — branchNameForTask
// then derives a task-unique fallback from the task's own id instead of
// cross-assigning a sibling's branch. A List failure fails closed (treated as
// a collision): losing a real branch link on a transient read error is
// strictly safer than risking two tasks racing to own the same worktree.
func (s *TaskService) claimIngestBranch(projectID, branch, excludeTaskID string) (string, bool) {
	if branch == "" {
		return "", false
	}
	all, err := s.tasks.List()
	if err != nil {
		s.logger.Warn("ingest.branch-guard.list", "err", err)
		return "", false
	}
	if owner, taken := task.BranchOwnedByOther(all, projectID, branch, excludeTaskID); taken {
		s.logger.Warn("ingest.branch-guard.collision",
			"task_id", excludeTaskID, "owner_task_id", owner, "branch", branch, "project_id", projectID)
		return "", false
	}
	return branch, true
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
