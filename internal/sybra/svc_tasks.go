package sybra

import (
	"bytes"
	"context"
	"encoding/base64"
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

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/attachment"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/intervention"
	"github.com/Automaat/sybra/internal/monitor"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/sandbox"
	"github.com/Automaat/sybra/internal/stats"
	"github.com/Automaat/sybra/internal/sybra/clusterlead"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

// TaskService exposes task CRUD operations as Wails-bound methods.
type TaskService struct {
	tasks          *task.Manager
	agents         *agent.Manager
	workflowEngine *workflow.Engine
	worktrees      *worktree.Manager
	sandboxes      *sandbox.Manager
	artifacts      *artifact.Store
	attachments    *attachment.Store
	wg             *sync.WaitGroup
	logger         *slog.Logger
	audit          audit.Store
	cfg            *config.Config
	currentConfig  func() *config.Config
	// projects and intervention back recordInterventionOnUnblock only; nil in
	// tests that don't exercise the human-required unblock path (the method
	// guards on both being non-nil before doing anything).
	projects     *project.Store
	intervention *intervention.Store
	abTesting    func() abtest.Config
	// assigner forwards a Tags/DependsOn edit to a follower-homed task's home
	// node at write time (see UpdateTask) — the write-time counterpart to
	// clusterlead.Mirror's detect-and-repair drift backstop. nil on a
	// non-leader node or in tests that don't exercise clustering.
	assigner *clusterlead.Assigner
	// ctx is the app's root context (wireTaskService sets it from a.ctx), used
	// only where a Wails-bound method has no request-scoped context of its own
	// to thread through — see RecoverLostAgent. nil in tests that construct
	// TaskService directly; recoveryCtx falls back to context.Background().
	ctx                 context.Context
	recoverLostAgent    func(context.Context, string) error
	fetchPR             func(repo string, number int) (github.PullRequest, error)
	fetchIssue          func(repo string, number int) (github.Issue, error)
	fetchIssueLinkedPRs func(repo string, issueNumber int) ([]github.PullRequest, error)
	viewerLogin         func() string
	// umbrellaExpand expands a detected ☂️ umbrella issue into a gated child
	// DAG instead of a flat task. Wired in wireServices; gated at call time on
	// cfg.Umbrella.Enabled. nil in tests that don't exercise umbrellas.
	umbrellaExpand func(issueURL, model string) (umbrella.Result, error)
	// monitorScan runs one anomaly-detector pass on the server's own monitor
	// service, so sybra-cli's `monitor scan` reports what the running instance
	// sees rather than what a second reader of the same files would. Wired
	// unconditionally: monitor.enabled stops the background loop, not the
	// ability to read the board, so the closure falls back to a read-only
	// ad-hoc pass. nil only in tests that do not exercise a scan.
	monitorScan func(context.Context) (monitor.Report, error)
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
	// followerStatusMu serializes each follower RPC with the corresponding
	// leader mirror write, preventing concurrent UI transitions from applying
	// remote and local statuses in opposite orders.
	followerStatusMu sync.Mutex
}

func (s *TaskService) config() *config.Config {
	if s.currentConfig != nil {
		return s.currentConfig()
	}
	return s.cfg
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

// ListTasks returns the board projection of every task. Historical prompt and
// result text is intentionally omitted: cards need run metadata for cost/count
// badges, while TaskDetail immediately calls GetTask for the selected task.
// Returning that text here made every board refresh serialize the complete
// lifetime prompt history of every task (hundreds of MiB on a mature board).
func (s *TaskService) ListTasks() ([]task.Task, error) {
	return s.tasks.ListBoard()
}

// mirrorStaleTerminalWindow bounds how long a terminal (done/cancelled) task
// keeps appearing in ListTasksForNode after it closed. The leader's Mirror
// polls every DefaultReconcileInterval (30s); ten minutes is dozens of cycles
// of headroom to deliver the final state before the task drops out, so a
// large closed-task backlog stops being re-serialized (full body/plan/review
// sidecars) on every reconcile forever — see #2188/#2258.
const mirrorStaleTerminalWindow = 10 * time.Minute

// ListTasksForNode returns the subset of the board relevant to a cluster
// follower's mirror: tasks assigned to that node, excluding terminal tasks
// closed longer than mirrorStaleTerminalWindow ago. Unlike ListTasks, this is
// sized for repeated polling rather than a one-off full board read.
//
// Also includes tasks with no AssignedNode at all. This service call always
// runs against this instance's own store (the leader polls each follower's
// HTTP API individually), so an unassigned task here unambiguously lives on
// this node — e.g. created by this follower's own local umbrella expansion
// or triage, never routed by a leader. AssignedNode is leader-only metadata
// (see Assigner.route/stampNode); a follower has no way to stamp its own
// name onto a task it created itself, so without this, such a task is
// permanently invisible to the leader's mirror — see cluster.mirror.adopted.
func (s *TaskService) ListTasksForNode(node string) ([]task.Task, error) {
	all, err := s.tasks.ListForNode(node, time.Now().Add(-mirrorStaleTerminalWindow))
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for i := range all {
		t := all[i]
		// Degraded entries deliberately exist only on the local board. They do
		// not name a valid task payload and must not be replicated to a leader.
		if t.Degraded {
			continue
		}
		if t.AssignedNode != node && t.AssignedNode != "" {
			continue
		}
		if task.IsTerminalStatus(t.Status) && t.ClosedAt != nil && time.Since(*t.ClosedAt) > mirrorStaleTerminalWindow {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// GetTask returns a single task by ID.
func (s *TaskService) GetTask(id string) (task.Task, error) {
	t, err := s.tasks.Get(id)
	if err != nil {
		return t, boardRejectionFor("task", id, err)
	}
	return s.withEstimatedAgentRunCosts(t), nil
}

func (s *TaskService) UploadAttachment(taskID, fileName string, data []byte) (task.Attachment, error) {
	if s.attachments == nil {
		return task.Attachment{}, validationError("attachments are unavailable")
	}
	if strings.TrimSpace(fileName) == "" {
		return task.Attachment{}, validationError("attachment filename is required")
	}
	meta, err := s.attachments.Put(taskID, attachment.UploadRequest{FileName: fileName, Data: data})
	if err != nil {
		return task.Attachment{}, validationError(err.Error())
	}
	_, err = s.tasks.UpdateFnBy(taskID, "svc.tasks.upload_attachment", func(cur task.Task) (task.Update, error) {
		next := slices.Clone(cur.Attachments)
		next = append(next, meta)
		return task.Update{Attachments: &next}, nil
	})
	if err != nil {
		_ = s.attachments.Delete(taskID, meta.ID)
		return task.Attachment{}, err
	}
	return meta, nil
}

func (s *TaskService) ListAttachments(taskID string) ([]task.Attachment, error) {
	t, err := s.tasks.Get(taskID)
	if err != nil {
		return nil, boardRejectionFor("task", taskID, err)
	}
	if t.Attachments == nil {
		return []task.Attachment{}, nil
	}
	return slices.Clone(t.Attachments), nil
}

func (s *TaskService) DeleteAttachment(taskID, attachmentID string) error {
	if s.attachments == nil {
		return validationError("attachments are unavailable")
	}
	_, err := s.tasks.UpdateFnBy(taskID, "svc.tasks.delete_attachment", func(cur task.Task) (task.Update, error) {
		idx := slices.IndexFunc(cur.Attachments, func(att task.Attachment) bool { return att.ID == attachmentID })
		if idx < 0 {
			return task.Update{}, validationError(fmt.Sprintf("attachment %q not found", attachmentID))
		}
		next := make([]task.Attachment, 0, len(cur.Attachments)-1)
		next = append(next, cur.Attachments[:idx]...)
		next = append(next, cur.Attachments[idx+1:]...)
		return task.Update{Attachments: &next}, nil
	})
	if err != nil {
		return err
	}
	if err := s.attachments.Delete(taskID, attachmentID); err != nil && s.logger != nil {
		s.logger.Warn("attachments.delete", "task_id", taskID, "attachment_id", attachmentID, "err", err)
	}
	return nil
}

func (s *TaskService) GetAttachmentURL(taskID, attachmentID string) (string, error) {
	if s.attachments == nil {
		return "", validationError("attachments are unavailable")
	}
	t, err := s.tasks.Get(taskID)
	if err != nil {
		return "", boardRejectionFor("task", taskID, err)
	}
	idx := slices.IndexFunc(t.Attachments, func(att task.Attachment) bool { return att.ID == attachmentID })
	if idx < 0 {
		return "", validationError(fmt.Sprintf("attachment %q not found", attachmentID))
	}
	att := t.Attachments[idx]
	data, _, err := s.attachments.Content(taskID, attachmentID)
	if err != nil {
		// The task already lists this attachment, so a read that still fails is a backend fault rather than a bad request, and its message describes storage the caller cannot act on.
		return "", fmt.Errorf("read attachment %q: %w", attachmentID, err)
	}
	return "data:" + att.ContentType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func (s *TaskService) ListTaskArtifacts(taskID string) ([]TaskArtifactDTO, error) {
	if s.artifacts == nil {
		return []TaskArtifactDTO{}, nil
	}
	metas, err := s.artifacts.List(taskID)
	if err != nil {
		return nil, boardRejectionFor("task", taskID, err)
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
	cfg := s.config()
	if cfg == nil || cfg.Logging.Dir == "" {
		return dto, nil
	}
	path := filepath.Join(cfg.Logging.Dir, "worktrees", taskID+"-setup.log")
	root := filepath.Join(cfg.Logging.Dir, "worktrees")
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
	cfg := s.config()
	if cfg == nil || cfg.Logging.Dir == "" {
		return []TaskAuditEventDTO{}, nil
	}
	if days <= 0 || days > 90 {
		days = 14
	}
	until := time.Now().UTC().Add(time.Minute)
	since := until.AddDate(0, 0, -days)
	if s.audit == nil {
		return []TaskAuditEventDTO{}, nil
	}
	events, err := s.audit.Read(audit.Query{
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
		return TamperReportDTO{}, boardRejectionFor("task", taskID, err)
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
		return nil, boardRejectionFor("task", taskID, err)
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

func translateTaskLockTimeout(err error) error {
	if err == nil || !errors.Is(err, fsutil.ErrLockTimeout) {
		return err
	}
	// The reason, not the lock path: this error names an absolute path under
	// the server's home and a holder pid, and its caller is a client on another
	// machine. What that client acts on is the 503, which it retries.
	return unavailableError("resource is locked; retry")
}

// writeMergedSidecarsOrWarn writes t's sidecar content to the file backend after a request-triggered forward already merged and persisted it via PutFnBy — PutFnBy's plain whole-task write never touches sidecar files. A failure here must not fail the whole RPC, since the task's primary fields already committed; it only logs. WriteMergedSidecars itself retries a transient write failure and skips a stale write superseded by a newer merge (#3308); what a warning here still means is every retry inside that call was exhausted, which for a healthy disk should be rare.
func (s *TaskService) writeMergedSidecarsOrWarn(op, taskID, node string, t task.Task) {
	if err := clusterlead.WriteMergedSidecars(s.tasks, t); err != nil {
		s.logger.Warn("cluster.task."+op+".sidecar_failed", "task_id", taskID, "node", node, "err", err)
	}
}

// BlessTampering records a human bless for a tamper-flagged task and sends it
// back to the review workflow.
func (s *TaskService) BlessTampering(taskID string) (task.Task, error) {
	// A follower owns task execution. Ask it to bless first, before changing
	// the leader mirror, so the board cannot claim the task resumed when the
	// worker rejected or never received the transition.
	if s.assigner != nil {
		s.followerStatusMu.Lock()
		defer s.followerStatusMu.Unlock()
		current, err := s.tasks.Get(taskID)
		if err != nil {
			return task.Task{}, boardRejectionFor("task", taskID, err)
		}
		ctx, cancel := context.WithTimeout(s.recoveryCtx(), fieldPushTimeout)
		defer cancel()
		remote, forwarded, err := s.assigner.BlessTampering(ctx, current)
		if err != nil {
			return task.Task{}, err
		} else if forwarded {
			result, _, putErr := s.tasks.PutFnBy(taskID, "svc.tasks.bless_tampering", func(local task.Task) (task.Task, error) {
				if local.AssignedNode != current.AssignedNode || local.AssignmentRev != current.AssignmentRev {
					return task.Task{}, conflictError("task ownership changed while follower tamper blessing was in flight")
				}
				merged, ok := clusterlead.Merge(local, remote)
				if !ok {
					return local, nil
				}
				if !slices.Contains(merged.Tags, workflow.TamperBlessedTag) {
					merged.Tags = append(merged.Tags, workflow.TamperBlessedTag)
				}
				return merged, nil
			})
			if putErr != nil {
				return task.Task{}, translateTaskLockTimeout(putErr)
			}
			s.writeMergedSidecarsOrWarn("tamper_bless", taskID, current.AssignedNode, result)
			s.logger.Info("cluster.task.tamper_bless.forwarded", "task_id", taskID, "node", current.AssignedNode)
			return result, nil
		}
	}
	var (
		cur      task.Task
		tagAdded bool
	)
	result, err := s.tasks.ApplyFn(taskID, func(t task.Task) (task.TransitionIntent, error) {
		if !t.TamperFlagged {
			return task.TransitionIntent{}, conflictError(
				"task is not tamper-flagged: bless requires status=human-required with blocker.kind=" +
					strconv.Quote(string(blocker.KindTamperDetected)),
			)
		}
		cur = t

		merged := append([]string(nil), t.Tags...)
		if !slices.Contains(merged, workflow.TamperBlessedTag) {
			merged = append(merged, workflow.TamperBlessedTag)
			tagAdded = true
		}
		return task.TransitionIntent{
			ToStatus: task.StatusReadyReview,
			Actor:    "svc.tasks.bless_tampering",
			Extra: task.Update{
				Tags:              task.Ptr(merged),
				ClearStatusReason: task.Ptr(true),
				ClearBlocker:      task.Ptr(true),
			},
			OperatorOverride: true,
		}, nil
	})
	if err != nil {
		return result.Task, translateTaskLockTimeout(err)
	}
	updated := result.Task

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
			if err := s.tasks.UpdateRunBy(t.ID, "svc.tasks.estimate_run_cost", run.AgentID, patch); err != nil && s.logger != nil {
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
	cost = stats.EstimateAgentCost(stats.AgentUsage{
		Provider:        provider,
		Model:           model,
		CostUSD:         cost,
		InputTokens:     input,
		OutputTokens:    output,
		CacheCreate:     cacheCreate,
		CacheRead:       cacheRead,
		ReasoningTokens: reasoning,
		PremiumRequests: premiumRequests,
		StartedAt:       startedAt,
	})
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
	for _, provider := range []string{providerid.Codex, providerid.Copilot, providerid.Claude} {
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
		return providerid.Codex
	}
	if strings.HasPrefix(model, "claude-") || model == "sonnet" || model == "opus" || model == "haiku" {
		return providerid.Claude
	}
	return ""
}

// CreateTask creates a new task and starts a matching workflow.
// If the title is a GitHub issue URL, fetches real title/body from GitHub.
func (s *TaskService) CreateTask(title, body, mode string) (task.Task, error) {
	return s.CreateTaskWithInit(title, body, mode, task.Update{})
}

// AssignTask persists a task pushed from the cluster leader into this
// follower's local store (leader-follower execution mirror, umbrella #1803).
// The task is written verbatim — the leader owns its canonical ID, status, and
// workflow — and the file watcher then dispatches it through the normal
// workflow, so nothing downstream changes. The pushed DTO is validated before
// it touches disk; a malformed task is rejected with a 400 rather than
// corrupting the board.
func (s *TaskService) AssignTask(t task.Task) error {
	if t.Degraded {
		return validationError("assigned task must not be degraded")
	}
	if err := task.ValidateID(t.ID); err != nil {
		return validationError(err.Error())
	}
	if strings.TrimSpace(t.Title) == "" {
		return validationError("assigned task must have a title")
	}
	if _, err := task.ValidateStatus(string(t.Status)); err != nil {
		return validationError(err.Error())
	}
	if t.AgentMode != "" {
		if _, err := task.ValidateAgentMode(t.AgentMode); err != nil {
			return validationError(err.Error())
		}
	}
	if t.Slug != "" {
		if err := task.ValidateSlug(t.Slug); err != nil {
			return validationError(err.Error())
		}
	}
	if t.TaskType != "" {
		if _, err := task.ValidateTaskType(string(t.TaskType)); err != nil {
			return validationError(err.Error())
		}
	}
	if current, err := s.tasks.Get(t.ID); err == nil && assignedTaskNoOp(current, t) {
		s.logger.Debug("cluster.task.assign.noop", "task", current.ID, "assigned_node", current.AssignedNode, "status", string(current.Status))
		return nil
	}
	// Leader-owned mirror bookkeeping (see normalizeAssignedTaskForCompare)
	// must never reach the local store: Assigner.Route forwards the leader's
	// canonical copy as-is, so a task re-routed after prior mirror cycles
	// still carries a stale MirrorRev/MirrorUpdatedAt here. Store.Put reads
	// those fields as proof this Put came through Merge; left untouched,
	// this write would masquerade as mirror-authoritative and bypass its
	// plain staleness guard.
	t.MirrorRev = 0
	t.MirrorUpdatedAt = nil
	saved, created, err := s.tasks.PutBy(t, "cluster.task.assign")
	if err != nil {
		return translateTaskLockTimeout(fmt.Errorf("assign task: %w", err))
	}
	if s.audit != nil && created {
		_ = s.audit.Log(audit.Event{
			Type:   audit.EventTaskCreated,
			TaskID: saved.ID,
			Data:   map[string]any{"source": "cluster_assign", "assigned_node": saved.AssignedNode, "status": string(saved.Status)},
		})
	}
	s.logger.Info("cluster.task.assigned", "task", saved.ID, "created", created, "status", string(saved.Status), "assigned_node", saved.AssignedNode)
	return nil
}

// RecoverLostAgent is a follower RPC the leader uses after it has
// authoritatively detected a lost-agent anomaly on a follower-owned task.
// It mirrors the local monitor path: best-effort stop the stale running run,
// then hand the task to recovery for a targeted restart.
func (s *TaskService) RecoverLostAgent(taskID string) error {
	if strings.TrimSpace(taskID) == "" {
		return validationError("task id is required")
	}
	if s.recoverLostAgent == nil {
		return unavailableError("lost-agent recovery unavailable")
	}
	// Resolve the task first. The writes below log and continue on failure,
	// so without this an unusable id reached recovery as though it named a
	// real task, and the caller's mistake came back as a server fault.
	if _, err := s.tasks.Get(taskID); err != nil {
		return boardRejectionFor("task", taskID, err)
	}
	reason := "monitor: agent lost; recovery will resume"
	if _, err := s.tasks.UpdateBy(taskID, "cluster.task.recover", task.Update{StatusReason: &reason}); err != nil && s.logger != nil {
		s.logger.Warn("cluster.task.recover.status-reason.failed", "task_id", taskID, "err", err)
	}
	if t, err := s.tasks.Get(taskID); err == nil {
		for i := range slices.Backward(t.AgentRuns) {
			if t.AgentRuns[i].State != string(agent.StateRunning) {
				continue
			}
			if err := s.tasks.UpdateRunBy(taskID, "cluster.task.recover", t.AgentRuns[i].AgentID, task.RunPatch{
				State: task.Ptr(string(agent.StateStopped)),
			}); err != nil && s.logger != nil {
				s.logger.Warn("cluster.task.recover.update-run.failed", "task_id", taskID, "agent_id", t.AgentRuns[i].AgentID, "err", err)
			}
			break
		}
	}
	return s.recoverLostAgent(s.recoveryCtx(), taskID)
}

// recoveryCtx is the context RecoverLostAgent hands to recovery.
// RecoverLostAgent is a Wails-bound method with no request-scoped context of
// its own — s.ctx (the app's root context, set once by wireTaskService) lets
// a follower's shutdown-cancellation reach the same
// workflow.IsShutdownCancellation check Recovery.surfaceStartFailure applies
// on the local maintenance-sweep path (sybra#2291): without this, an
// in-flight leader→follower RecoverLostAgent RPC racing a follower's
// graceful shutdown would fall back to context.Background() (never done),
// so the check could never recognize the cancellation as shutdown-induced
// and would still surface a misleading status_reason / feed the breaker.
func (s *TaskService) recoveryCtx() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

func assignedTaskNoOp(current, pushed task.Task) bool {
	cur := normalizeAssignedTaskForCompare(current, &current)
	next := normalizeAssignedTaskForCompare(pushed, &current)
	curData, err := task.MarshalStored(cur)
	if err != nil {
		return false
	}
	nextData, err := task.MarshalStored(next)
	if err != nil {
		return false
	}
	return bytes.Equal(curData, nextData)
}

func normalizeAssignedTaskForCompare(t task.Task, fallback *task.Task) task.Task {
	if t.TaskType != task.TaskTypeUmbrella {
		t.TaskType = ""
	}
	// Ignore leader-owned mirror bookkeeping when deciding whether a pushed
	// follower task would change local semantics; otherwise a mirror-only bump
	// (timestamps / mirror rev) rewrites the file and re-triggers watchers.
	t.UpdatedAt = time.Time{}
	t.StatusChangedAt = time.Time{}
	t.MirrorRev = 0
	t.MirrorUpdatedAt = nil
	if t.CreatedAt.IsZero() && fallback != nil {
		t.CreatedAt = fallback.CreatedAt
	}
	if t.AgentRuns == nil {
		t.AgentRuns = []task.AgentRun{}
	}
	t.Plan = ""
	t.PlanContract = ""
	t.PlanCritique = ""
	t.PlanResearch = ""
	t.PlanDecisions = ""
	t.PlanBrief = ""
	t.CodeReview = ""
	t.PlanDrafts = nil
	t.FilePath = ""
	t.TamperFlagged = false
	return t
}

// CreateTaskWithInit is CreateTask plus caller-supplied initial field
// overrides (e.g. Issue) applied atomically in the same first-write as
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
		return t, boardRejection(translateTaskLockTimeout(err))
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
	if cfg := s.config(); cfg != nil && !cfg.HomeNodeForTask(t.ProjectID, t.NodeOverride).Local {
		return
	}
	// pr-fix / ordinary existing-PR tasks are driven outside task.created.
	// Explicit handoff entry points are the exception: they intentionally route
	// through task.created even when a PR number is already known.
	if skipTaskCreatedWorkflow(t) {
		return
	}
	// An agent-only instance refuses the start below, so bail before announcing
	// an auto-start that will not happen — the log line read like a leak.
	if !s.workflowEngine.AutoDispatchEnabled() {
		return
	}
	info := taskToInfo(t)
	if def := s.workflowEngine.MatchWorkflow(info, "task.created"); def != nil {
		s.logger.Info("workflow.auto-start", "task_id", t.ID, "workflow", def.ID)
		s.wg.Go(func() {
			if wfErr := s.workflowEngine.StartWorkflow(t.ID, def.ID); wfErr != nil &&
				!errors.Is(wfErr, workflow.ErrAutoDispatchDisabled) {
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
// Moving a task to a dispatching stage when its workflow is terminal (completed
// or failed) and no agent is running restarts the workflow — allowing the user
// to retry implementation, review, testing, or PR creation after a failed run.
//
//nolint:funlen // Field decoding and one atomic transition are intentionally audited together.
func (s *TaskService) UpdateTask(id string, updates map[string]any) (task.Task, error) {
	cur, _ := s.tasks.Get(id)
	decisionReason := ""
	if reason, ok := updates["status_reason"].(string); ok {
		decisionReason = reason
	}

	if status, ok := updates["status"].(string); ok {
		s.followerStatusMu.Lock()
		defer s.followerStatusMu.Unlock()
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
		// A follower owns its task's live agents and workflow. Validate this
		// local request first, then forward before changing the leader mirror so
		// a rejected remote transition cannot silently revert a success response.
		if s.assigner != nil {
			ctx, cancel := context.WithTimeout(context.Background(), fieldPushTimeout)
			defer cancel()
			remoteUpdates := map[string]any{"status": status}
			if statusReason, hasReason := updates["status_reason"]; hasReason {
				remoteUpdates["status_reason"] = statusReason
			}
			remote, forwarded, forwardErr := s.assigner.UpdateTaskStatus(ctx, cur, remoteUpdates)
			if forwardErr != nil {
				return cur, forwardErr
			} else if forwarded {
				// Merge the follower's returned execution snapshot, including its
				// clock watermark, instead of rebuilding just status locally. A
				// delayed mirror poll from before this RPC is then stale by
				// construction and cannot undo the accepted transition.
				t, _, putErr := s.tasks.PutFnBy(id, "svc.tasks.update", func(local task.Task) (task.Task, error) {
					if local.AssignedNode != cur.AssignedNode || local.AssignmentRev != cur.AssignmentRev {
						return task.Task{}, conflictError("task ownership changed while follower status update was in flight")
					}
					merged, ok := clusterlead.Merge(local, remote)
					if !ok {
						return local, nil
					}
					return merged, nil
				})
				if putErr != nil {
					return cur, translateTaskLockTimeout(putErr)
				}
				s.writeMergedSidecarsOrWarn("status_update", id, cur.AssignedNode, t)
				s.logger.Info("cluster.task.status_update.forwarded", "task_id", id, "node", cur.AssignedNode, "status", status)
				return t, nil
			}
		}
	}
	var t task.Task
	var err error
	if statusText, hasStatus := updates["status"].(string); hasStatus {
		localUpdates := make(map[string]any, len(updates)-1)
		for key, value := range updates {
			if key != "status" {
				localUpdates[key] = value
			}
		}
		extra, parseErr := task.UpdateFromMap(localUpdates)
		if parseErr != nil {
			return t, validationError(parseErr.Error())
		}
		status, statusErr := task.ValidateStatus(statusText)
		if statusErr != nil {
			return t, validationError(statusErr.Error())
		}
		if status == task.StatusHumanRequired {
			display := decisionReason
			if strings.TrimSpace(display) == "" {
				display = "operator moved task to human-required"
			}
			extra.Escalation = task.OperatorDecisionEvidence("operator.manual_status_change", display)
			extra.AutonomyOutcome = task.HumanRequiredOutcome()
		}
		result, applyErr := s.tasks.Apply(task.TransitionIntent{
			TaskID: id, ToStatus: status, Actor: "svc.tasks.update",
			Extra: extra, OperatorOverride: true,
		})
		t, err = result.Task, applyErr
	} else {
		t, err = s.tasks.UpdateMapBy(id, "svc.tasks.update", updates)
	}
	if err != nil {
		return t, boardRejectionFor("task", id, translateTaskLockTimeout(err))
	}
	s.appendManualHumanRequiredDecision(cur, t, decisionReason)
	s.wg.Go(func() {
		// UpdateTask is a Wails-bound method the frontend awaits synchronously;
		// the follower push carries a bounded remote round trip, so run it
		// detached to avoid blocking the IPC caller. It's best-effort anyway —
		// the Mirror drift backstop catches a missed push on the next tick.
		s.pushFieldEditToFollower(id, updates, t)
	})
	if task.IsTerminalStatus(t.Status) {
		s.wg.Go(func() {
			// context.Background(): UpdateTask is a Wails-bound method; this
			// runs in a detached background goroutine with no ctx to thread.
			s.worktrees.Remove(context.Background(), t.ID)
			if s.sandboxes != nil {
				s.sandboxes.Remove(t.ID)
			}
		})
	}

	// When manually moved to a dispatching stage with a terminal workflow and
	// no live agent, dispatch via task.status_changed so the trigger system
	// picks the right workflow for the new status. Naively restarting
	// cur.Workflow.WorkflowID would replay whatever flow ran before, rather
	// than the workflow appropriate for the stage the operator selected.
	// This backstop is needed when the local status hook was unavailable (for
	// example, an update forwarded from another board process).
	if s.workflowEngine != nil && s.workflowEngine.AutoDispatchEnabled() {
		if newStatus, ok := updates["status"].(string); ok &&
			isManualDispatchStatus(newStatus) &&
			cur.Workflow != nil &&
			(cur.Workflow.State == workflow.ExecCompleted || cur.Workflow.State == workflow.ExecFailed) &&
			!s.agents.HasRunningAgentForTask(id) {
			s.logger.Info("workflow.restart", "task_id", id, "from_workflow", cur.Workflow.WorkflowID, "status", newStatus)
			s.wg.Go(func() {
				dispatched, wfErr := s.redispatchStatusChanged(id, newStatus)
				if errors.Is(wfErr, workflow.ErrAutoDispatchDisabled) {
					return
				}
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

func isManualDispatchStatus(status string) bool {
	switch task.Status(status) {
	case task.StatusInProgress, task.StatusReadyReview, task.StatusTesting, task.StatusReadyPR:
		return true
	default:
		return false
	}
}

// fieldPushTimeout bounds pushFieldEditToFollower's remote round trip so an
// unreachable follower can't leave the detached push goroutine hanging.
const fieldPushTimeout = 5 * time.Second

// pushFieldEditToFollower forwards a Tags/DependsOn edit on an already
// follower-assigned task to its home node at write time
// (clusterlead.Assigner.PushFieldUpdate) — the write-time counterpart to
// clusterlead.Mirror's detect-and-repair drift backstop (Merge never carries
// either field, so without this a leader-side edit — e.g. a manual
// `sybra-cli update --tags` — would otherwise only reach the follower via
// that backstop, one reconcile interval later and after filing a drift
// alert). Best-effort: a failed push here just leaves the backstop to catch
// it on the next tick, same as any other transient cluster hiccup.
func (s *TaskService) pushFieldEditToFollower(id string, updates map[string]any, t task.Task) {
	if s.assigner == nil {
		return
	}
	_, tagsEdited := updates["tags"]
	_, depsEdited := updates["depends_on"]
	_, condsEdited := updates["depends_on_conditions"]
	if !tagsEdited && !depsEdited && !condsEdited {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), fieldPushTimeout)
	defer cancel()
	if _, err := s.assigner.PushFieldUpdate(ctx, t); err != nil {
		s.logger.Warn("task.update.field_push_failed", "task_id", id, "err", err)
	}
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

// dispatchTargetSpec describes one status an operator or human-review recovery
// can move a human-required task to.
type dispatchTargetSpec struct {
	// requiresPR gates the target on the task already having a linked PR
	// (only in-review needs this — dispatching in-review without a PR would
	// flip the task into PR-monitoring with nothing to monitor).
	requiresPR bool
	// dispatches selects whether redispatchStatusChanged runs after the
	// status write. in-review has no task.status_changed trigger — it is a
	// plain PR-guarded status flip into the existing PR-monitoring state.
	// done is also non-dispatching: it is a terminal close-out for an already
	// completed/merged task that human-review proved should not be retried.
	dispatches bool
	// clearWorkflow removes a stale terminal workflow when the target status
	// itself is the complete recovery. Without this, a task can be closed while
	// still displaying the failed workflow that caused the human-required park.
	clearWorkflow bool
}

var dispatchTargets = map[string]dispatchTargetSpec{
	string(task.StatusInProgress):  {dispatches: true},
	string(task.StatusReadyReview): {dispatches: true},
	string(task.StatusTesting):     {dispatches: true},
	string(task.StatusReadyPR):     {dispatches: true},
	string(task.StatusInReview):    {requiresPR: true, dispatches: false},
	string(task.StatusDone):        {dispatches: false, clearWorkflow: true},
}

func readyPRNoWorkflowAllowed(role, target string) bool {
	if target != string(task.StatusReadyPR) {
		return false
	}
	switch agent.Role(role) {
	case agent.RoleReview, agent.RoleTestRunner, agent.RoleHumanReview:
		return true
	default:
		return false
	}
}

// DispatchFromHumanRequired flips a task parked in human-required to target
// (one of in-progress/ready-review/testing/ready-pr/in-review), recording
// reason as the audit-visible status_reason. For dispatching targets it
// synchronously re-enters the workflow via task.status_changed; on any failure
// to do so it fails closed, reverting the task to human-required with an
// explanatory status_reason so the operator is never left with a task silently
// stuck in a target status with no workflow driving it.
//
// The whole check-then-write sequence runs under the workflow engine's
// per-task human-action lock (shared with plan-review's
// HandleHumanActionRecovering), so a double-click or a second operator cannot
// race the same stuck task between the guard reads and the status write.
func (s *TaskService) DispatchFromHumanRequired(id, target, reason string) (task.Task, error) {
	return s.dispatchFromHumanRequiredAllowingAgent(id, target, reason, "")
}

func (s *TaskService) dispatchFromHumanRequiredAllowingAgent(id, target, reason, exceptAgentID string) (task.Task, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return task.Task{}, conflictError("a decision reason is required")
	}
	if _, ok := dispatchTargets[target]; !ok {
		return task.Task{}, conflictError(fmt.Sprintf("unsupported dispatch target %q", target))
	}

	var result task.Task
	run := func() error {
		out, err := s.dispatchFromHumanRequiredLockedAllowingAgent(id, target, reason, exceptAgentID)
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

//nolint:funlen // Dispatch preconditions and the resulting atomic transition must stay contiguous.
func (s *TaskService) dispatchFromHumanRequiredLockedAllowingAgent(id, target, reason, exceptAgentID string) (task.Task, error) {
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
	if s.assigner != nil {
		s.followerStatusMu.Lock()
		ctx, cancel := context.WithTimeout(s.recoveryCtx(), fieldPushTimeout)
		remote, forwarded, forwardErr := s.assigner.DispatchFromHumanRequired(ctx, cur, target, reason)
		cancel()
		if forwardErr != nil {
			s.followerStatusMu.Unlock()
			return task.Task{}, forwardErr
		}
		if forwarded {
			local, _, localErr := s.tasks.PutFnBy(id, "svc.tasks.dispatch-human-required", func(current task.Task) (task.Task, error) {
				if current.AssignedNode != cur.AssignedNode || current.AssignmentRev != cur.AssignmentRev {
					return task.Task{}, conflictError("task ownership changed while follower dispatch was in flight")
				}
				merged, ok := clusterlead.Merge(current, remote)
				if !ok {
					return current, nil
				}
				return merged, nil
			})
			if localErr != nil {
				s.followerStatusMu.Unlock()
				return task.Task{}, translateTaskLockTimeout(localErr)
			}
			s.writeMergedSidecarsOrWarn("dispatch_human_required", id, cur.AssignedNode, local)
			s.followerStatusMu.Unlock()
			s.logDispatchAudit(id, target, string(cur.Status), reason, "forwarded")
			return local, nil
		}
		s.followerStatusMu.Unlock()
	}
	hasRunning := s.agents.HasRunningAgentForTask(id)
	if exceptAgentID != "" {
		hasRunning = s.agents.HasOtherRunningAgentForTask(id, exceptAgentID)
	}
	if hasRunning {
		return task.Task{}, conflictError("cannot dispatch: an agent is already running for this task")
	}
	if cur.Workflow != nil && cur.Workflow.State != workflow.ExecCompleted && cur.Workflow.State != workflow.ExecFailed {
		return task.Task{}, conflictError(fmt.Sprintf("cannot dispatch: task has active workflow %q (state=%s)",
			cur.Workflow.WorkflowID, cur.Workflow.State))
	}

	extra := task.Update{
		StatusReason: task.Ptr(reason),
		ClearBlocker: task.Ptr(true),
	}
	if spec.clearWorkflow {
		extra.ClearWorkflow = task.Ptr(true)
	}
	if _, err := s.tasks.Apply(task.TransitionIntent{
		TaskID:           id,
		ToStatus:         task.Status(target),
		Actor:            "svc.tasks.dispatch-human-required",
		ExpectedStatus:   task.Ptr(task.StatusHumanRequired),
		Extra:            extra,
		OperatorOverride: true,
	}); err != nil {
		return task.Task{}, translateTaskLockTimeout(err)
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
			if dispatchErr == nil && matched == "" && readyPRNoWorkflowAllowed(cur.RunRole, target) {
				s.logger.Info("task.dispatch.no-workflow-needed", "task_id", id, "target", target, "role", cur.RunRole)
			} else {
				s.logger.Error("task.dispatch.failed", "task_id", id, "target", target, "err", failure)
				revertReason := fmt.Sprintf("%s (dispatch to %s failed: %s)", reason, target, failure)
				if _, revertErr := s.tasks.Apply(task.TransitionIntent{
					TaskID: id, ToStatus: task.StatusBlocked, Actor: "svc.tasks.dispatch-failure",
					Extra: task.Update{
						StatusReason:    task.Ptr(revertReason),
						Escalation:      task.MachineFailure("dispatch.workflow_start_failed", revertReason),
						AutonomyOutcome: task.QuarantinedOutcome(),
					},
					OperatorOverride: true,
				}); revertErr != nil {
					s.logger.Error("task.dispatch.revert-failed", "task_id", id, "target", target, "err", revertErr)
					s.logDispatchAudit(id, target, string(cur.Status), reason, "revert-failed")
					return task.Task{}, translateTaskLockTimeout(fmt.Errorf("dispatch to %s failed (%s) and quarantine also failed: %w", target, failure, revertErr))
				}
				// A control-plane dispatch failure is machine-owned. Quarantine it
				// instead of manufacturing another request for human judgment.
				s.logDispatchAudit(id, target, string(cur.Status), reason, "quarantined")
				return task.Task{}, conflictError(fmt.Sprintf("dispatch to %q failed: %s", target, failure))
			}
		}
	}

	s.logDispatchAudit(id, target, string(cur.Status), reason, "dispatched")
	if exceptAgentID == "" {
		s.appendDecisionProgress(id, artifact.ManualDecisionMessage(string(cur.Status), target, reason))
	}
	s.recordInterventionOnUnblock(cur, target, reason, exceptAgentID)
	return s.tasks.Get(id)
}

func (s *TaskService) appendManualHumanRequiredDecision(before, after task.Task, reason string) {
	if before.Status != task.StatusHumanRequired || after.Status == task.StatusHumanRequired {
		return
	}
	s.appendDecisionProgress(after.ID, artifact.ManualDecisionMessage(string(before.Status), string(after.Status), reason))
	// UpdateTask is the generic GUI/CLI field-edit endpoint, reached only by an
	// operator (or a script run on their behalf) — never by an automated
	// recovery path, which exits human-required through
	// dispatchFromHumanRequiredLockedAllowingAgent or the review package's own
	// Capture calls instead. Route this exit through the same
	// guard+scrub+persist+audit pipeline so a plain status/field edit is as
	// durably attributed as a click on the Dispatch button (issue #2727).
	s.recordInterventionOnUnblock(before, string(after.Status), reason, "")
}

func (s *TaskService) appendDecisionProgress(taskID, message string) {
	if s.artifacts == nil || strings.TrimSpace(message) == "" {
		return
	}
	if err := s.artifacts.AppendProgress(taskID, artifact.ProgressEntry{
		Kind:    artifact.ProgressKindDecision,
		Message: message,
	}); err != nil && s.logger != nil {
		s.logger.Warn("task.progress.append_failed", "task_id", taskID, "kind", artifact.ProgressKindDecision, "err", err)
	}
}

// recordInterventionOnUnblock captures a genuine operator-initiated unblock
// of a human-required task through intervention.Capture (see
// internal/intervention) — advisory context for a future replay fixture
// (sybra#2454), never a routing/admission/completion gate. cur is the task as
// it stood immediately before this dispatch's status write, so its
// Status/Blocker/Workflow/StatusReason are exactly the system-state signal
// being captured.
//
// This is one of three real exit paths from human-required — the other two
// (automated PR-blocker reconciliation and automated PR-landing advance) are
// hooked the same way from internal/sybra/review; all three route through the
// same intervention.Capture so a fingerprint dedups identically regardless of
// which path produced it. exceptAgentID=="" means a human clicked Dispatch;
// non-empty means an automatic recovery path re-entered the workflow on the
// operator's behalf.
func (s *TaskService) recordInterventionOnUnblock(cur task.Task, target, reason, exceptAgentID string) {
	class := intervention.OperatorActionHuman
	if exceptAgentID != "" {
		class = intervention.OperatorActionAutoRecovery
	}
	intervention.Capture(s.intervention, s.config(), s.projects, s.audit, s.logger, cur, target, reason, class)
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
	deleted, err := s.tasks.Get(id)
	if err != nil {
		s.logger.Error("task.delete.failed", "task_id", id, "err", err)
		return boardRejectionFor("task", id, err)
	}
	if err := s.tasks.DeleteBy(id, "svc.tasks.delete"); err != nil {
		s.logger.Error("task.delete.failed", "task_id", id, "err", err)
		return boardRejectionFor("task", id, translateTaskLockTimeout(err))
	}
	s.agents.KillAgentsForTask(id, 10*time.Second)
	if s.sandboxes != nil {
		s.sandboxes.Remove(id)
	}
	// context.Background(): DeleteTask is a Wails-bound method with no ctx.
	s.worktrees.RemoveTask(context.Background(), deleted)
	if s.audit != nil {
		_ = s.audit.Log(audit.Event{
			Type:   audit.EventTaskDeleted,
			TaskID: id,
		})
	}
	return nil
}

// enrichFromPR fetches a GitHub PR and updates the task.
// If the PR was authored by the current viewer, moves to in-review for PR
// monitoring. Otherwise, it tags the task as an inbound review and hands it to
// the pr-review workflow. Keeping PR review dispatch behind the workflow engine
// gives it the same assignment, rate, and prompt-contract gates as review
// request polling.
func (s *TaskService) enrichFromPR(taskID, repo string, number int) {
	pr, err := s.fetchPRFunc()(repo, number)
	if err != nil {
		s.logger.Error("enrich-pr.fetch", "task_id", taskID, "repo", repo, "number", number, "err", err)
		return
	}
	viewer := s.viewerLoginFunc()()
	if viewer == "" {
		// Guessing "not mine" is irreversible: it points a review agent at our
		// own PR and, via u.Tags, drops the enrich-pending marker reconcile
		// retries on. Bail before any Update so the retry survives.
		s.logger.Warn("enrich-pr.no-viewer", "task_id", taskID, "repo", repo, "number", number)
		return
	}

	slug := task.Slugify(pr.Title)
	u := task.Update{
		Title:     task.Ptr(pr.Title),
		ProjectID: task.Ptr(repo),
		PRNumber:  task.Ptr(pr.Number),
		Slug:      task.Ptr(slug),
	}
	if branch, ok := s.claimIngestBranch(repo, pr.HeadRefName, taskID); ok {
		u.Branch = task.Ptr(branch)
	}
	// claimIngestBranch already logs the reason (list failure vs. collision)
	// on rejection; no need to duplicate/relabel it here.
	// Replace tags with the PR's labels (possibly empty), which also clears the
	// enrich-pending marker set on the URL stub at creation.
	labels := pr.Labels
	u.Tags = &labels

	isMyPR := strings.EqualFold(pr.Author, viewer)
	if isMyPR {
		if _, err := s.tasks.Apply(task.TransitionIntent{
			TaskID:   taskID,
			ToStatus: task.StatusInReview,
			Actor:    "svc.tasks.enrich_pr.my_pr",
			Extra:    u,
		}); err != nil {
			s.logger.Error("enrich-pr.update", "task_id", taskID, "err", err)
			return
		}
		s.enrichRetryCooldown.Delete(taskID)
		s.logger.Info("enrich-pr.my-pr", "task_id", taskID, "pr", number, "title", pr.Title)
		return
	}

	// Not my PR: add review tag and let the pr-review workflow own dispatch.
	labels = append(labels, "review")
	u.Tags = &labels
	if _, err := s.tasks.UpdateBy(taskID, "svc.tasks.enrich_pr.review", u); err != nil {
		s.logger.Error("enrich-pr.update", "task_id", taskID, "err", err)
		return
	}
	t, err := s.tasks.Get(taskID)
	if err != nil {
		s.logger.Error("enrich-pr.get", "task_id", taskID, "err", err)
		return
	}
	s.startCreatedWorkflow(t)
	s.enrichRetryCooldown.Delete(taskID)
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
		}
		// Else: this PR's branch already belongs to a different task — e.g.
		// one PR closes more than one GitHub issue, and another issue's task
		// already claimed it — or the ownership check itself failed to read.
		// Leave this task's own PR link unclaimed rather than have two tasks
		// race to own the same branch/worktree; claimIngestBranch already
		// logged the specific reason.
	} else if len(linkedPRs) > 0 {
		if viewerPRs := s.viewerLinkedPRCount(linkedPRs); viewerPRs > 1 {
			s.logger.Warn("enrich-issue.linked-prs.ambiguous", "task_id", taskID, "count", viewerPRs)
		}
	}
	u.Tags = &labels
	var updated task.Task
	if u.Status != nil {
		toStatus := *u.Status
		u.Status = nil
		result, applyErr := s.tasks.Apply(task.TransitionIntent{
			TaskID:   taskID,
			ToStatus: toStatus,
			Actor:    "svc.tasks.enrich_issue",
			Extra:    u,
		})
		updated, err = result.Task, applyErr
	} else {
		updated, err = s.tasks.UpdateBy(taskID, "svc.tasks.enrich_issue", u)
	}
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
	cfg := s.config()
	return cfg != nil && cfg.Umbrella.Enabled && s.umbrellaExpand != nil
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
	res, err := s.umbrellaExpand(issue.URL, "")
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
	if _, err := s.tasks.UpdateBy(taskID, "svc.tasks.enrich_umbrella_stub", u); err != nil {
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
