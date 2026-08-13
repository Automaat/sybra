package sybra

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/autonomy"
	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/evidence"
	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/experience"
	"github.com/Automaat/sybra/internal/pressure"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/sandbox"
	"github.com/Automaat/sybra/internal/skillinvoke"
	"github.com/Automaat/sybra/internal/sybra/agentorch"
	"github.com/Automaat/sybra/internal/sybra/runenv"
	"github.com/Automaat/sybra/internal/sybra/verification"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/taskstatus"
	"github.com/Automaat/sybra/internal/triage"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
	"github.com/Automaat/sybra/internal/worktreeerr"
)

var (
	errWorkflowEffectNoPersist             = errors.New("workflow effect claim requires no persistence")
	errWorkflowStatusReasonNoLongerMatches = errors.New("workflow status reason no longer matches")
)

// Compile-time interface checks.
var (
	_ workflow.TaskProvider                 = (*taskAdapter)(nil)
	_ workflow.TaskClassifier               = (*taskClassifierAdapter)(nil)
	_ workflow.AgentLauncher                = (*agentAdapter)(nil)
	_ workflow.WorktreeGetter               = (*worktreeGetterAdapter)(nil)
	_ workflow.AttemptNoteAppender          = (*attemptNoteAppenderAdapter)(nil)
	_ workflow.BranchSyncer                 = (*branchSyncerAdapter)(nil)
	_ workflow.CheckConfigGetter            = (*checkConfigGetterAdapter)(nil)
	_ workflow.ManualTestConfigGetter       = (*manualTestConfigGetterAdapter)(nil)
	_ workflow.ArtifactRecorder             = (*artifactRecorderAdapter)(nil)
	_ workflow.EvidenceRecorder             = (*evidenceRecorderAdapter)(nil)
	_ workflow.CostBudgetChecker            = (*agentAdapter)(nil)
	_ workflow.AttemptWorktreeManager       = (*attemptWorktreeAdapter)(nil)
	_ workflow.VerificationWorkspaceManager = (*verificationWorkspaceAdapter)(nil)
	_ workflow.VerificationCommandRunner    = (*agentAdapter)(nil)
)

type verificationWorkspaceAdapter struct{ mgr *verification.Manager }

func (a *verificationWorkspaceAdapter) PrepareVerification(ctx context.Context, taskID, purpose, canonicalDir string) (workflow.VerificationWorkspace, error) {
	lease, err := a.mgr.Prepare(ctx, taskID, purpose, canonicalDir)
	if err != nil {
		return workflow.VerificationWorkspace{}, err
	}
	return workflow.VerificationWorkspace{ID: lease.ID, Dir: lease.WorkspaceDir, SourceSHA: lease.SourceSHA}, nil
}

func (a *verificationWorkspaceAdapter) FinalizeVerification(ctx context.Context, workspace workflow.VerificationWorkspace, commands []string, output string) error {
	lease, err := a.mgr.Lease(workspace.ID)
	if err != nil {
		return err
	}
	return a.mgr.Finalize(ctx, lease, commands, output, "")
}

func (a *verificationWorkspaceAdapter) ValidateVerification(ctx context.Context, workspace workflow.VerificationWorkspace) error {
	lease, err := a.mgr.Lease(workspace.ID)
	if err != nil {
		return err
	}
	return a.mgr.ValidateSource(ctx, lease)
}

func (a *verificationWorkspaceAdapter) ReleaseVerification(workspace workflow.VerificationWorkspace) {
	lease, err := a.mgr.Lease(workspace.ID)
	if err == nil {
		a.mgr.Release(lease)
	}
}

// attemptWorktreeAdapter bridges worktree.Manager → workflow.AttemptWorktreeManager.
type attemptWorktreeAdapter struct {
	tasks *task.Manager
	mgr   *worktree.Manager
}

func (a *attemptWorktreeAdapter) PrepareAttempt(taskID, attemptID string) (dir, branch string, err error) {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return "", "", fmt.Errorf("get task: %w", err)
	}
	// context.Background(): AttemptWorktreeManager is a fixed interface
	// signature invoked from workflow step execution, which never threads a
	// caller ctx today (see the identical rationale on PrepareForTask calls
	// elsewhere in this file).
	return a.mgr.PrepareAttempt(context.Background(), t, attemptID)
}

func (a *attemptWorktreeAdapter) PromoteAttempt(taskID, winnerDir, winnerBranch string) (string, error) {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return "", fmt.Errorf("get task: %w", err)
	}
	return a.mgr.PromoteAttempt(context.Background(), t, winnerDir, winnerBranch)
}

func (a *attemptWorktreeAdapter) CleanupAttempts(taskID string, attemptIDs []string) {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return
	}
	a.mgr.CleanupAttempts(context.Background(), t, attemptIDs)
}

// artifactRecorderAdapter bridges artifact.Store → workflow.ArtifactRecorder.
type artifactRecorderAdapter struct {
	store *artifact.Store
}

func (a *artifactRecorderAdapter) RecordTrace(taskID string, ev any) error {
	return a.store.Append(taskID, artifact.KindTrace, ev)
}

func (a *artifactRecorderAdapter) PutPlanSnapshot(taskID, role, stepID, sourcePath, content string) error {
	name := ""
	if stepID != "" {
		name = "plan-" + stepID + ".md"
	}
	_, err := a.store.Put(taskID, artifact.Artifact{
		Kind:         artifact.KindPlan,
		Name:         name,
		ProducerRole: role,
		StepID:       stepID,
		SourcePath:   sourcePath,
		Content:      []byte(content),
	})
	return err
}

func (a *artifactRecorderAdapter) PutGeneric(taskID, name, stepID, content string) error {
	_, err := a.store.Put(taskID, artifact.Artifact{
		Kind:    artifact.KindGeneric,
		Name:    name,
		StepID:  stepID,
		Content: []byte(content),
	})
	return err
}

// evidenceRecorderAdapter bridges evidence.Store → workflow.EvidenceRecorder.
type evidenceRecorderAdapter struct {
	store *evidence.Store
}

func (a *evidenceRecorderAdapter) AppendCriterion(taskID string, entry evidence.CriterionEvidence) error {
	return a.store.Append(taskID, entry)
}

func (a *evidenceRecorderAdapter) Evidence(taskID string) (evidence.CompletionEvidence, error) {
	return a.store.Load(taskID)
}

// taskAdapter bridges task.Manager → workflow.TaskProvider.
type taskAdapter struct {
	tasks    *task.Manager
	projects *project.Store
}

func (a *taskAdapter) GetTask(id string) (workflow.TaskInfo, error) {
	t, err := a.tasks.Get(id)
	if err != nil {
		return workflow.TaskInfo{}, err
	}
	info := taskToInfo(t)
	if t.ProjectID != "" && a.projects != nil {
		if p, pErr := a.projects.Get(t.ProjectID); pErr == nil {
			info.ProjectType = string(p.Type)
		}
	}
	return info, nil
}

func (a *taskAdapter) ListTasks() ([]workflow.TaskInfo, error) {
	tasks, err := a.tasks.List()
	if err != nil {
		return nil, err
	}
	infos := make([]workflow.TaskInfo, 0, len(tasks))
	for i := range tasks {
		infos = append(infos, taskToInfo(tasks[i]))
	}
	return infos, nil
}

func (a *taskAdapter) UpdateTaskStatus(id string, status taskstatus.Status, reason string) error {
	st, err := task.ValidateStatus(string(status))
	if err != nil {
		return err
	}
	reason = a.normalizeHumanRequiredReason(id, st, reason)
	_, err = a.tasks.ApplyFn(id, func(cur task.Task) (task.TransitionIntent, error) {
		target := st
		extra := task.Update{}
		if reason != "" {
			extra.StatusReason = &reason
		} else if cur.Status != st && (st != task.StatusHumanRequired || cur.Status != task.StatusBlocked) {
			extra.ClearStatusReason = task.Ptr(true)
		}
		if st == task.StatusHumanRequired {
			target = task.StatusBlocked
			extra.Escalation = task.MachineFailure("workflow.untyped_escalation", reason)
			extra.AutonomyOutcome = task.QuarantinedOutcome()
		}
		return task.TransitionIntent{
			ToStatus: target,
			Actor:    "workflow.engine.update_status",
			Extra:    extra,
		}, nil
	})
	return err
}

func (a *taskAdapter) ClearTaskStatusReasonIf(id string, expectedStatus taskstatus.Status, expectedReason string) (bool, error) {
	cleared := false
	_, err := a.tasks.UpdateFnBy(id, "workflow.engine.clear_status_reason", func(cur task.Task) (task.Update, error) {
		if cur.Status != expectedStatus || cur.StatusReason != expectedReason {
			return task.Update{}, errWorkflowStatusReasonNoLongerMatches
		}
		cleared = true
		return task.Update{ClearStatusReason: task.Ptr(true)}, nil
	})
	if errors.Is(err, errWorkflowStatusReasonNoLongerMatches) {
		return false, nil
	}
	return cleared, err
}

// ClearTaskStatusReasonAndSetWorkflowIf clears a status reason and persists the
// workflow in a single store write, and only while the task still carries the
// expected status/reason. It is the atomic form of SetWorkflow followed by
// ClearTaskStatusReasonIf: on a mismatch nothing is written at all, so a
// superseded retry cannot leave an incremented counter banked against a budget
// it never spent, and no crash window can land the bumped counter without the
// cleared marker (see #2749).
func (a *taskAdapter) ClearTaskStatusReasonAndSetWorkflowIf(id, expectedStatus, expectedReason string, wf *workflow.Execution) (bool, error) {
	cleared := false
	_, err := a.tasks.ApplyFn(id, func(cur task.Task) (task.TransitionIntent, error) {
		if string(cur.Status) != expectedStatus || cur.StatusReason != expectedReason {
			return task.TransitionIntent{}, errWorkflowStatusReasonNoLongerMatches
		}
		cleared = true
		return task.TransitionIntent{
			ToStatus: cur.Status,
			Actor:    "workflow.engine.clear_reason_and_set_workflow",
			Extra:    task.Update{ClearStatusReason: task.Ptr(true), Workflow: &wf},
		}, nil
	})
	if errors.Is(err, errWorkflowStatusReasonNoLongerMatches) {
		return false, nil
	}
	return cleared, err
}
func (a *taskAdapter) UpdateTaskBlocker(id string, status taskstatus.Status, reason string, state blocker.State) error {
	st, err := task.ValidateStatus(string(status))
	if err != nil {
		return err
	}
	reason = a.normalizeHumanRequiredReason(id, st, reason)
	var reasonPtr *string
	if reason != "" {
		reasonPtr = &reason
	}
	escalation, outcome := typedBlockerEscalation(st, state, reason)
	if st == task.StatusHumanRequired && !blocker.AllowsHumanRequired(state.Kind) {
		st = task.StatusBlocked
	}
	_, err = a.tasks.Apply(task.TransitionIntent{
		TaskID:   id,
		ToStatus: st,
		Actor:    "workflow.engine.update_blocker",
		Extra: task.Update{
			Blocker:         &state,
			StatusReason:    reasonPtr,
			Escalation:      escalation,
			AutonomyOutcome: outcome,
		},
	})
	return err
}

func (a *taskAdapter) normalizeHumanRequiredReason(taskID string, status task.Status, reason string) string {
	if status != task.StatusHumanRequired {
		return reason
	}
	if strings.TrimSpace(reason) != "" {
		return reason
	}
	cur, err := a.tasks.Get(taskID)
	if err != nil {
		return reason
	}
	if strings.TrimSpace(cur.StatusReason) != "" {
		return cur.StatusReason
	}
	return fallbackHumanRequiredReason(cur)
}

func fallbackHumanRequiredReason(t task.Task) string {
	if len(t.AgentRuns) > 0 {
		if reason := fallbackHumanRequiredReasonFromRun(t, t.AgentRuns[len(t.AgentRuns)-1]); reason != "" {
			return reason
		}
	}
	if t.Workflow != nil && strings.TrimSpace(t.Workflow.CurrentStep) != "" {
		return fmt.Sprintf("workflow step %s escalated to human-required without recording a reason", t.Workflow.CurrentStep)
	}
	return "task escalated to human-required without recording a reason"
}

func fallbackHumanRequiredReasonFromRun(t task.Task, run task.AgentRun) string {
	role := strings.TrimSpace(run.Role)
	if role == "" {
		role = "agent"
	} else {
		role += " agent"
	}
	runLabel := role + " run"
	if agentID := strings.TrimSpace(run.AgentID); agentID != "" {
		runLabel += " " + agentID
	}
	step := ""
	if t.Workflow != nil && strings.TrimSpace(t.Workflow.CurrentStep) != "" {
		step = " at workflow step " + t.Workflow.CurrentStep
	}
	switch {
	case strings.TrimSpace(run.Outcome) == "" && strings.TrimSpace(run.Result) == "":
		return runLabel + step + " stopped without a recorded outcome or result"
	case strings.TrimSpace(run.Outcome) == "":
		return runLabel + step + " stopped without a recorded outcome"
	default:
		return ""
	}
}

func (a *taskAdapter) UpdateTaskPR(id string, prNumber int) error {
	_, err := a.tasks.UpdateBy(id, "workflow.engine.update_pr", task.Update{PRNumber: &prNumber})
	return err
}

func (a *taskAdapter) MarkTaskReviewed(id string) error {
	reviewed := true
	_, err := a.tasks.UpdateBy(id, "workflow.engine.mark_reviewed", task.Update{Reviewed: &reviewed})
	return err
}

func (a *taskAdapter) SetCodeReviewVerdict(id, verdict string) error {
	_, err := a.tasks.UpdateBy(id, "workflow.engine.set_code_review_verdict", task.Update{CodeReviewVerdict: &verdict})
	return err
}

func (a *taskAdapter) MarkAgentRunProtocolViolation(taskID, agentID, violation string) error {
	return a.tasks.UpdateRunBy(taskID, "workflow.engine.mark_protocol_violation", agentID, task.RunPatch{ProtocolViolation: task.Ptr(violation)})
}

func (a *taskAdapter) MarkAgentRunTestOutcome(taskID, agentID, outcome, fingerprint string) error {
	patch := task.RunPatch{TestOutcome: task.Ptr(outcome)}
	if fingerprint != "" {
		patch.TestFailureFingerprint = task.Ptr(fingerprint)
	}
	return a.tasks.UpdateRunBy(taskID, "workflow.engine.mark_test_outcome", agentID, patch)
}

// MarkAgentRunIncomplete corrects a run's recorded outcome after the fact. The
// completion handler derives success from a clean exit alone, which it must:
// whether a code-author run produced commits is only known later, once the
// branch is inspected.
func (a *taskAdapter) MarkAgentRunIncomplete(taskID, agentID string) error {
	return a.tasks.UpdateRunBy(taskID, "workflow.engine.mark_run_incomplete", agentID, task.RunPatch{Outcome: task.Ptr(task.RunOutcomeIncomplete)})
}

func (a *taskAdapter) RecordAgentRunFinalCommit(taskID, agentID, headSHA, source string) error {
	patch := task.RunPatch{}
	if headSHA != "" {
		patch.HeadSHA = task.Ptr(headSHA)
	}
	if source != "" {
		patch.FinalCommitSource = task.Ptr(source)
	}
	return a.tasks.UpdateRunBy(taskID, "workflow.engine.record_final_commit", agentID, patch)
}

func (a *taskAdapter) AppendTaskBody(id, content string) error {
	_, err := a.tasks.AppendBodyBy(id, "workflow.engine.append_body", content)
	return err
}

func (a *taskAdapter) ReplaceTaskBody(id, body string) error {
	_, err := a.tasks.UpdateBy(id, "workflow.engine.replace_body", task.Update{Body: &body})
	return err
}

func (a *taskAdapter) SetWorkflow(id string, wf *workflow.Execution) error {
	_, err := a.tasks.ApplyFn(id, func(cur task.Task) (task.TransitionIntent, error) {
		return task.TransitionIntent{
			ToStatus: cur.Status,
			Actor:    "workflow.engine.set_workflow",
			Extra:    task.Update{Workflow: &wf},
		}, nil
	})
	return err
}

// SetStatusAndWorkflow persists a task's Status/StatusReason and Workflow in
// a single store write, closing the crash window a paired
// UpdateTaskStatus+SetWorkflow call leaves open (a restart between the two
// calls could otherwise land a terminal task status with a still-running
// workflow, or vice versa — see #2749).
func (a *taskAdapter) SetStatusAndWorkflow(id, status, reason string, wf *workflow.Execution) error {
	st, err := task.ValidateStatus(status)
	if err != nil {
		return err
	}
	reason = a.normalizeHumanRequiredReason(id, st, reason)
	extra := task.Update{Workflow: &wf}
	if reason != "" {
		extra.StatusReason = &reason
	}
	if st == task.StatusHumanRequired {
		st = task.StatusBlocked
		extra.Escalation = task.MachineFailure("workflow.untyped_escalation", reason)
		extra.AutonomyOutcome = task.QuarantinedOutcome()
	}
	_, err = a.tasks.Apply(task.TransitionIntent{
		TaskID:   id,
		ToStatus: st,
		Actor:    "workflow.engine.set_status_and_workflow",
		Extra:    extra,
	})
	return err
}

func (a *taskAdapter) SetEscalationAndWorkflow(id, status, reason string, escalation autonomy.EscalationReason, outcome autonomy.Outcome, wf *workflow.Execution) error {
	st, err := task.ValidateStatus(status)
	if err != nil {
		return err
	}
	reason = a.normalizeHumanRequiredReason(id, st, reason)
	extra := task.Update{
		Workflow:        &wf,
		Escalation:      &escalation,
		AutonomyOutcome: &outcome,
	}
	if reason != "" {
		extra.StatusReason = &reason
	}
	_, err = a.tasks.Apply(task.TransitionIntent{
		TaskID:   id,
		ToStatus: st,
		Actor:    "workflow.engine.set_escalation_and_workflow",
		Extra:    extra,
	})
	return err
}

// SetBlockerAndWorkflow persists a task's Status/StatusReason/Blocker and
// Workflow in a single store write, the blocker-path counterpart to
// SetStatusAndWorkflow — closing the same crash window for callers that
// escalate to a blocked status (via a workflow-owned blocker.State) alongside
// a workflow mutation (see #2749).
func (a *taskAdapter) SetBlockerAndWorkflow(id, status, reason string, state blocker.State, wf *workflow.Execution) error {
	st, err := task.ValidateStatus(status)
	if err != nil {
		return err
	}
	reason = a.normalizeHumanRequiredReason(id, st, reason)
	var reasonPtr *string
	if reason != "" {
		reasonPtr = &reason
	}
	escalation, outcome := typedBlockerEscalation(st, state, reason)
	if st == task.StatusHumanRequired && !blocker.AllowsHumanRequired(state.Kind) {
		st = task.StatusBlocked
	}
	_, err = a.tasks.Apply(task.TransitionIntent{
		TaskID:   id,
		ToStatus: st,
		Actor:    "workflow.engine.set_blocker_and_workflow",
		Extra: task.Update{
			Blocker:         &state,
			StatusReason:    reasonPtr,
			Workflow:        &wf,
			Escalation:      escalation,
			AutonomyOutcome: outcome,
		},
	})
	return err
}

func typedBlockerEscalation(status task.Status, state blocker.State, reason string) (*autonomy.EscalationReason, *autonomy.Outcome) {
	if status != task.StatusHumanRequired {
		return nil, nil
	}
	owner := blocker.FailureOwner(state.Kind)
	if owner == autonomy.FailureOwnerUnknown {
		// An unclassified workflow blocker is not evidence that a human owns
		// the failure. Treat it as control-plane debt and quarantine it.
		owner = autonomy.FailureOwnerMachine
	}
	code := "workflow.blocker."
	if state.Kind == "" {
		code += "unknown"
	} else {
		code += string(state.Kind)
	}
	if owner.AllowsHumanRequired() {
		return task.ControlPlaneFailure(code, owner, reason), task.HumanRequiredOutcome()
	}
	return task.ControlPlaneFailure(code, owner, reason), task.QuarantinedOutcome()
}

var errWorkflowWriteFenceMismatch = errors.New("workflow write fence mismatch")

func (a *taskAdapter) SetWorkflowIf(id string, fence workflow.WorkflowWriteFence, wf *workflow.Execution) (bool, error) {
	_, err := a.tasks.ApplyFn(id, func(cur task.Task) (task.TransitionIntent, error) {
		if cur.Generation != fence.Generation || cur.Status != fence.Status ||
			cur.StatusReason != fence.StatusReason || cur.Workflow == nil ||
			cur.Workflow.WorkflowID != fence.WorkflowID || cur.Workflow.CurrentStep != fence.CurrentStep ||
			cur.Workflow.State != fence.State {
			return task.TransitionIntent{}, errWorkflowWriteFenceMismatch
		}
		expectedStatus := cur.Status
		return task.TransitionIntent{
			TaskID: id, ToStatus: cur.Status, Actor: "workflow.engine.set_workflow_if",
			ExpectedGeneration: &fence.Generation, ExpectedStatus: &expectedStatus,
			Extra: task.Update{Workflow: &wf},
		}, nil
	})
	if errors.Is(err, errWorkflowWriteFenceMismatch) {
		return false, nil
	}
	return err == nil, err
}

func (a *taskAdapter) SetStatusAndWorkflowIf(id string, fence workflow.WorkflowWriteFence, status taskstatus.Status, reason string, wf *workflow.Execution) (bool, error) {
	st, err := task.ValidateStatus(string(status))
	if err != nil {
		return false, err
	}
	_, err = a.tasks.ApplyFn(id, func(cur task.Task) (task.TransitionIntent, error) {
		if cur.Generation != fence.Generation || cur.Status != fence.Status ||
			cur.StatusReason != fence.StatusReason || cur.Workflow == nil ||
			cur.Workflow.WorkflowID != fence.WorkflowID || cur.Workflow.CurrentStep != fence.CurrentStep ||
			cur.Workflow.State != fence.State {
			return task.TransitionIntent{}, errWorkflowWriteFenceMismatch
		}
		expectedStatus := cur.Status
		return task.TransitionIntent{
			TaskID: id, ToStatus: st, Actor: "workflow.engine.set_status_and_workflow_if",
			ExpectedGeneration: &fence.Generation, ExpectedStatus: &expectedStatus,
			Extra: task.Update{StatusReason: task.Ptr(reason), Workflow: &wf},
		}, nil
	})
	if errors.Is(err, errWorkflowWriteFenceMismatch) {
		return false, nil
	}
	return err == nil, err
}

func (a *taskAdapter) ClaimWorkflowEffect(id string, claim workflow.EffectClaim) (workflow.EffectClaimResult, error) {
	var result workflow.EffectClaimResult
	var fenceErr error
	_, err := a.tasks.ApplyFn(id, func(cur task.Task) (task.TransitionIntent, error) {
		if cur.Workflow == nil {
			return task.TransitionIntent{}, fmt.Errorf("task %s has no workflow", id)
		}
		wf := cur.Workflow.Clone()
		result.Workflow = wf
		claimResult, claimErr := wf.ClaimEffect(claim)
		claimResult.Workflow = wf
		result = claimResult
		if claimErr != nil {
			if errors.Is(claimErr, workflow.ErrEffectClaimConflict) || errors.Is(claimErr, workflow.ErrEffectAlreadyComplete) {
				fenceErr = claimErr
				return task.TransitionIntent{}, errWorkflowEffectNoPersist
			}
			return task.TransitionIntent{}, claimErr
		}
		return task.TransitionIntent{
			ToStatus: cur.Status,
			Actor:    "workflow.engine.claim_effect",
			Extra:    task.Update{Workflow: &wf},
		}, nil
	})
	if err != nil {
		if errors.Is(err, errWorkflowEffectNoPersist) {
			return result, fenceErr
		}
		return workflow.EffectClaimResult{}, err
	}
	return result, nil
}

func (a *taskAdapter) CompleteWorkflowEffect(id string, claim workflow.EffectClaim) (workflow.EffectClaimResult, error) {
	var result workflow.EffectClaimResult
	var fenceErr error
	_, err := a.tasks.ApplyFn(id, func(cur task.Task) (task.TransitionIntent, error) {
		if cur.Workflow == nil {
			return task.TransitionIntent{}, fmt.Errorf("task %s has no workflow", id)
		}
		wf := cur.Workflow.Clone()
		result.Workflow = wf
		claimResult, claimErr := wf.CompleteEffect(claim)
		claimResult.Workflow = wf
		result = claimResult
		if claimErr != nil {
			if errors.Is(claimErr, workflow.ErrEffectClaimLost) || errors.Is(claimErr, workflow.ErrEffectAlreadyComplete) {
				fenceErr = claimErr
				return task.TransitionIntent{}, errWorkflowEffectNoPersist
			}
			return task.TransitionIntent{}, claimErr
		}
		return task.TransitionIntent{
			ToStatus: cur.Status,
			Actor:    "workflow.engine.complete_effect",
			Extra:    task.Update{Workflow: &wf},
		}, nil
	})
	if err != nil {
		if errors.Is(err, errWorkflowEffectNoPersist) {
			return result, fenceErr
		}
		return workflow.EffectClaimResult{}, err
	}
	return result, nil
}

func (a *taskAdapter) ReleaseWorkflowEffect(id string, claim workflow.EffectClaim) (workflow.EffectClaimResult, error) {
	var result workflow.EffectClaimResult
	var fenceErr error
	_, err := a.tasks.ApplyFn(id, func(cur task.Task) (task.TransitionIntent, error) {
		if cur.Workflow == nil {
			return task.TransitionIntent{}, fmt.Errorf("task %s has no workflow", id)
		}
		wf := cur.Workflow.Clone()
		result.Workflow = wf
		claimResult, claimErr := wf.ReleaseEffect(claim)
		claimResult.Workflow = wf
		result = claimResult
		if claimErr != nil {
			if errors.Is(claimErr, workflow.ErrEffectClaimLost) || errors.Is(claimErr, workflow.ErrEffectAlreadyComplete) {
				fenceErr = claimErr
				return task.TransitionIntent{}, errWorkflowEffectNoPersist
			}
			return task.TransitionIntent{}, claimErr
		}
		return task.TransitionIntent{
			ToStatus: cur.Status,
			Actor:    "workflow.engine.release_effect",
			Extra:    task.Update{Workflow: &wf},
		}, nil
	})
	if err != nil {
		if errors.Is(err, errWorkflowEffectNoPersist) {
			return result, fenceErr
		}
		return workflow.EffectClaimResult{}, err
	}
	return result, nil
}

func (a *taskAdapter) ConsumeSupervisorSteer(taskID, prompt string) (string, error) {
	return agentorch.PrependSupervisorSteer(a.tasks, taskID, prompt)
}

func (a *taskAdapter) WriteSidecar(id, kind, content string) error {
	var u task.Update
	// Plan drafts are namespaced "plan_draft.<name>" so the workflow can fan
	// out to N parallel planners without growing the static sidecar enum.
	// The engine derives <name> from the parallel child step ID.
	if name, ok := strings.CutPrefix(kind, "plan_draft."); ok {
		u.PlanDraftWrite = &task.PlanDraftEntry{Name: name, Content: content}
		_, err := a.tasks.UpdateBy(id, "workflow.engine.write_sidecar", u)
		return err
	}
	switch kind {
	case "plan":
		u.Plan = &content
	case "plan_contract":
		u.PlanContract = &content
	case "code_review":
		u.CodeReview = &content
	case "plan_critique":
		u.PlanCritique = &content
	case "plan_research":
		u.PlanResearch = &content
	case "plan_decisions":
		u.PlanDecisions = &content
	case "plan_brief":
		u.PlanBrief = &content
	case "current_test_failures":
		u.CurrentTestFailures = &content
	case "acceptance_ledger":
		u.AcceptanceLedger = &content
	case "spec_decision":
		u.SpecDecision = &content
	default:
		return fmt.Errorf("unknown sidecar kind %q (want plan|plan_contract|code_review|plan_critique|plan_research|plan_decisions|plan_brief|current_test_failures|acceptance_ledger|spec_decision|plan_draft.<name>)", kind)
	}
	_, err := a.tasks.UpdateBy(id, "workflow.engine.write_sidecar", u)
	return err
}

// taskClassifierAdapter bridges internal/triage's deterministic classifier to
// workflow.TaskClassifier for the `classify_task` step. It runs the same
// classify+apply pipeline as `sybra-cli triage classify <id>` and the
// poll-based auto-triage handler (internal/poll.TriageHandler), so the
// workflow step no longer needs a full agent session to reach it.
type taskClassifierAdapter struct {
	tasks             *task.Manager
	projects          *project.Store
	classifier        triage.Classifier
	audit             audit.Store
	sybraBugProjectID string
}

func (a *App) newTaskClassifierAdapter() *taskClassifierAdapter {
	return &taskClassifierAdapter{
		tasks:             a.tasks,
		projects:          a.projects,
		classifier:        &triage.FallbackClassifier{Model: a.cfg.Triage.Model, Logger: a.logger, Gate: a.providerHealth},
		audit:             a.audit,
		sybraBugProjectID: a.cfg.HumanReviewRepo(),
	}
}

func (a *taskClassifierAdapter) ClassifyTask(ctx context.Context, taskID string) error {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return err
	}
	var projects []project.Project
	if a.projects != nil {
		projects, err = a.projects.List()
		if err != nil {
			return err
		}
	}
	_, _, err = triage.ClassifyAndApplyWithOptions(ctx, a.classifier, a.tasks, a.audit, t, projects, triage.ApplyOptions{
		SybraBugProjectID: a.sybraBugProjectID,
	})
	return err
}

func taskToInfo(t task.Task) workflow.TaskInfo {
	return workflow.TaskInfo{
		ID:                    t.ID,
		Title:                 t.Title,
		Generation:            t.Generation,
		Status:                t.Status,
		StatusReason:          t.StatusReason,
		Blocker:               t.Blocker,
		Role:                  t.RunRole,
		Tags:                  t.Tags,
		AgentMode:             t.AgentMode,
		Priority:              string(t.Priority),
		ProjectID:             t.ProjectID,
		NodeOverride:          t.NodeOverride,
		HandoffSourceProvider: t.HandoffSourceProvider,
		PRNumber:              t.PRNumber,
		Branch:                t.Branch,
		Body:                  t.Body,
		Plan:                  t.Plan,
		PlanContract:          t.PlanContract,
		PlanCritique:          t.PlanCritique,
		PlanResearch:          t.PlanResearch,
		PlanDecisions:         t.PlanDecisions,
		PlanBrief:             t.PlanBrief,
		CodeReview:            t.CodeReview,
		CurrentTestFailures:   t.CurrentTestFailures,
		AcceptanceLedger:      t.AcceptanceLedger,
		SpecDecision:          t.SpecDecision,
		CodeReviewVerdict:     t.CodeReviewVerdict,
		PlanDrafts:            t.PlanDrafts,
		Attachments:           toAttachmentInfos(t.Attachments),
		Issue:                 t.Issue,
		Reviewed:              t.Reviewed,
		Workflow:              t.Workflow,
		AgentRuns:             toRunInfos(t.AgentRuns),
		TestingCycleStartedAt: t.TestingCycleStartedAt,
	}
}

func toAttachmentInfos(attachments []task.Attachment) []workflow.AttachmentInfo {
	if len(attachments) == 0 {
		return nil
	}
	out := make([]workflow.AttachmentInfo, len(attachments))
	for i := range attachments {
		out[i] = workflow.AttachmentInfo{
			ID:          attachments[i].ID,
			FileName:    attachments[i].FileName,
			ContentType: attachments[i].ContentType,
			SizeBytes:   attachments[i].SizeBytes,
			Path:        attachments[i].Path,
		}
	}
	return out
}

// toRunInfos projects a task's agent runs onto the engine-visible subset used
// by route_test_result and cross-provider provenance.
func toRunInfos(runs []task.AgentRun) []workflow.AgentRunInfo {
	if len(runs) == 0 {
		return nil
	}
	out := make([]workflow.AgentRunInfo, len(runs))
	for i := range runs {
		out[i] = workflow.AgentRunInfo{
			AgentID:                runs[i].AgentID,
			Role:                   runs[i].Role,
			Provider:               runs[i].Provider,
			RequestedSkill:         runs[i].RequestedSkill,
			SkillExecutionMode:     runs[i].SkillExecutionMode,
			SkillConformance:       runs[i].SkillConformance,
			StartedAt:              runs[i].StartedAt,
			Outcome:                runs[i].Outcome,
			ProtocolViolation:      runs[i].ProtocolViolation,
			TestOutcome:            runs[i].TestOutcome,
			TestFailureFingerprint: runs[i].TestFailureFingerprint,
			HeadSHA:                runs[i].HeadSHA,
			FinalCommitSource:      runs[i].FinalCommitSource,
			SubagentCallCount:      runs[i].SubagentCallCount,
			TurnCount:              runs[i].TurnCount,
		}
	}
	return out
}

// worktreeGetterAdapter bridges worktree.Manager + task.Manager → workflow.WorktreeGetter.
type worktreeGetterAdapter struct {
	tasks *task.Manager
	mgr   *worktree.Manager
}

type attemptNoteAppenderAdapter struct{}

// checkConfigGetterAdapter resolves a task's codegen/verify commands by merging
// the repo `.sybra.yaml` checks (read from the project's trusted default
// branch, never the checked-out worktree — see resolveTrustedSetupCommands
// and issue #1519) with the app-level project config.
type checkConfigGetterAdapter struct {
	tasks    *task.Manager
	projects *project.Store
	mgr      *worktree.Manager
}

// manualTestConfigGetterAdapter resolves repo .sybra.yaml manual_test hints.
type manualTestConfigGetterAdapter struct {
	tasks    *task.Manager
	projects *project.Store
	mgr      *worktree.Manager
}

func (a *manualTestConfigGetterAdapter) ManualTestConfig(taskID string) workflow.ManualTestInfo {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return workflow.ManualTestInfo{}
	}

	var repoManual *project.ManualTestConfig
	if a.mgr != nil {
		wtPath := a.mgr.PathFor(t)
		if _, statErr := os.Stat(wtPath); statErr == nil {
			if repoCfg, rErr := project.LoadRepoConfig(wtPath); rErr == nil && repoCfg != nil {
				repoManual = repoCfg.ManualTest
			}
		}
	}

	var appManual *project.ManualTestConfig
	if t.ProjectID != "" && a.projects != nil {
		if p, pErr := a.projects.Get(t.ProjectID); pErr == nil {
			appManual = p.ManualTest
		}
	}
	manual := project.MergeManualTest(repoManual, appManual)
	if manual == nil {
		return workflow.ManualTestInfo{}
	}
	return workflow.ManualTestInfo{
		Kind:          string(manual.Kind),
		Command:       manual.Command,
		HealthURL:     manual.HealthURL,
		ProbeCommands: manual.ProbeCommands,
	}
}

func (a *checkConfigGetterAdapter) CodegenCommands(ctx context.Context, taskID string) []string {
	merged := a.mergedChecks(ctx, taskID)
	if merged == nil {
		return nil
	}
	return merged.Codegen
}

func (a *checkConfigGetterAdapter) VerifyCommands(ctx context.Context, taskID string) []string {
	merged := a.mergedChecks(ctx, taskID)
	if merged == nil {
		return nil
	}
	return merged.Verify
}

func (a *checkConfigGetterAdapter) FocusedChecks(ctx context.Context, taskID string) []project.FocusedCheck {
	merged := a.mergedChecks(ctx, taskID)
	if merged == nil {
		return nil
	}
	return merged.Focused
}

func (a *checkConfigGetterAdapter) WorktreeBaseRef(ctx context.Context, taskID string) string {
	t, err := a.tasks.Get(taskID)
	if err != nil || t.ProjectID == "" || a.projects == nil {
		return project.WorktreeBaseRefFresh
	}
	p, err := a.projects.Get(t.ProjectID)
	if err != nil || p.WorktreeBaseRef != project.WorktreeBaseRefHead {
		return project.WorktreeBaseRefFresh
	}
	return project.WorktreeBaseRefHead
}

func (a *checkConfigGetterAdapter) mergedChecks(ctx context.Context, taskID string) *project.ChecksConfig {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return nil
	}
	wtPath := a.mgr.PathFor(t)
	if _, statErr := os.Stat(wtPath); statErr != nil {
		return nil
	}
	var repoChecks *project.ChecksConfig
	var appChecks *project.ChecksConfig
	if t.ProjectID != "" {
		if p, pErr := a.projects.Get(t.ProjectID); pErr == nil {
			appChecks = p.Checks
			// Read checks.{codegen,verify,focused} from the project's trusted default
			// branch, never the checked-out worktree: the worktree's own
			// .sybra.yaml may carry malicious commands planted by a
			// compromised or prompt-injected agent, and these commands run
			// unsandboxed
			// via `sh -c` (see resolveTrustedSetupCommands, issue #1519).
			// ctx carries the caller's step deadline so a hung
			// git show/symbolic-ref on the bare repo can't block indefinitely.
			if repoCfg, rErr := project.LoadRepoConfigAtDefaultBranch(ctx, p.ClonePath); rErr == nil && repoCfg != nil {
				repoChecks = repoCfg.Checks
			}
		}
	}
	return project.MergeChecks(repoChecks, appChecks)
}

func (a *checkConfigGetterAdapter) SetupCommands(ctx context.Context, taskID string) []string {
	t, err := a.tasks.Get(taskID)
	if err != nil || t.ProjectID == "" {
		return nil
	}
	wtPath := a.mgr.PathFor(t)
	if _, statErr := os.Stat(wtPath); statErr != nil {
		return nil
	}
	p, pErr := a.projects.Get(t.ProjectID)
	if pErr != nil {
		return nil
	}
	var repoSetup []string
	if repoCfg, rErr := project.LoadRepoConfigAtDefaultBranch(ctx, p.ClonePath); rErr == nil && repoCfg != nil {
		repoSetup = repoCfg.Setup
	}
	return project.MergeSetup(repoSetup, p.SetupCommands)
}

// ensurePRTailWorktree restores a missing worktree for deterministic PR-tail
// operations. Branch-conflict recovery runs while a task is in-progress, not
// ready-pr, so limiting this to ready-pr strands an otherwise recoverable
// branch just before push_branch can publish it.
func ensurePRTailWorktree(ctx context.Context, tasks *task.Manager, mgr *worktree.Manager, taskID string) (path string, ok bool, err error) {
	if tasks == nil || mgr == nil {
		return "", false, nil
	}
	t, err := tasks.Get(taskID)
	if err != nil {
		return "", false, err
	}
	path = mgr.PathFor(t)
	if _, err := os.Stat(path); err == nil {
		return path, true, nil
	}
	if !statusCanRunPRTail(t.Status) || strings.TrimSpace(t.ProjectID) == "" {
		return "", false, nil
	}

	var prepared string
	switch {
	case strings.TrimSpace(t.Branch) == "" && t.PRNumber > 0:
		prepared, err = mgr.PrepareForFix(ctx, t, t.PRNumber)
	default:
		prepared, err = mgr.PrepareForBranchFix(ctx, t)
	}
	if err != nil {
		return "", false, err
	}
	return prepared, true, nil
}

func statusCanRunPRTail(status task.Status) bool {
	switch status {
	case task.StatusInProgress, task.StatusReadyReview, task.StatusInReview, task.StatusTesting, task.StatusReadyPR:
		return true
	default:
		return false
	}
}

func (a *worktreeGetterAdapter) GetWorktreePath(taskID string) (string, bool) {
	path, ok, err := ensurePRTailWorktree(context.Background(), a.tasks, a.mgr, taskID)
	if err != nil {
		return "", false
	}
	return path, ok
}

func (a *worktreeGetterAdapter) ResolvePRWorktree(ctx context.Context, taskID string) (path string, found bool, err error) {
	return ensurePRTailWorktree(ctx, a.tasks, a.mgr, taskID)
}

func (*attemptNoteAppenderAdapter) AppendReimplementNote(ctx context.Context, _, wtPath, marker, note string) error {
	return worktree.AppendNote(ctx, wtPath, marker, note)
}

// branchSyncerAdapter bridges task.Manager + worktree.Manager → workflow.BranchSyncer.
type branchSyncerAdapter struct {
	tasks *task.Manager
	mgr   *worktree.Manager
}

func (a *branchSyncerAdapter) SyncTaskBranch(ctx context.Context, taskID string) (string, error) {
	if _, ok, err := ensurePRTailWorktree(ctx, a.tasks, a.mgr, taskID); err != nil {
		return worktree.SyncFailed.String(), fmt.Errorf("sync branch: ensure worktree: %w", err)
	} else if !ok {
		return worktree.SyncSkipped.String(), nil
	}
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return worktree.SyncFailed.String(), fmt.Errorf("sync branch: get task: %w", err)
	}
	result, err := a.mgr.SyncTaskBranch(ctx, t)
	return result.String(), err
}

// agentAdapter bridges agent.Manager + agentorch.Orchestrator → workflow.AgentLauncher.
type agentAdapter struct {
	agents       *agent.Manager
	agentOrch    *agentorch.Orchestrator
	tasks        *task.Manager
	projects     *project.Store
	sandboxes    *sandbox.Manager
	experience   experience.Repository
	pressure     *pressure.Gate
	runenv       *runenv.Service
	verification *verification.Manager
}

func (a *agentAdapter) RunVerificationCommand(ctx context.Context, taskID, dir, command string, env []string, output io.Writer) error {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return err
	}
	return a.agents.RunVerificationCommand(ctx, agent.RunConfig{
		TaskID: taskID, Role: agent.RoleTestRunner, Dir: dir, ExtraEnv: env,
		SandboxMode: agentorch.ResolveSandboxMode(t, a.agentOrch.Cfg()),
	}, "/bin/sh", []string{"-c", command}, output)
}

func translatePoolBusy(err error) error {
	if agent.IsCapacityError(err) {
		return fmt.Errorf("%w: %w", workflow.ErrAgentPoolBusy, err)
	}
	if agent.IsAttemptConflict(err) {
		return fmt.Errorf("%w: %w", workflow.ErrDispatchInFlight, err)
	}
	return err
}

func assignmentAttemptAccess(assignment workflow.AgentAssignment) agent.AttemptAccess {
	if assignment.AdmissionObserve {
		return agent.AttemptAccessObserve
	}
	return ""
}

func (a *agentAdapter) directRunInputs(taskID string, role agent.Role) (task.Task, string, error) {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return task.Task{}, "", err
	}
	// Each test runner starts an isolated real-app/cluster sandbox, so cap
	// that load independently of the general agent concurrency limit.
	if err := a.ensureTestRunnerCapacity(role); err != nil {
		return task.Task{}, "", err
	}
	posture, err := agentorch.ResolveHeadlessPermissionMode(t, a.agentOrch.Cfg())
	return t, posture, err
}

func (a *agentAdapter) StartAgent(taskID, role, mode, model, provider, prompt, dir string, allowedTools []string, needsWorktree, oneShot bool, outputSchema, cleanRetryRef string, assignment workflow.AgentAssignment) (agentID, startedDir, baselineRef string, err error) { //nolint:funlen // Admission, workspace prep, and certified launch share one claim lifetime.
	// "interactive" is no longer a dispatchable agent.RunConfig.Mode, but a
	// run_agent step whose mode templates off {{.Task.AgentMode}} (e.g.
	// simple-task-implement's implement step) echoes whatever legacy value
	// is on disk — task.AgentModeInteractive is still a valid, load-only
	// value on old task files (see task.validAgentModes). Coerce here, the
	// single entry point both branches below share, rather than stalling
	// the workflow on "unknown mode" for a pre-existing interactive task.
	if mode == "interactive" {
		mode = "headless"
	}
	// For implementation agents without a pre-staged dir, use the full
	// orchestrator (handles worktree, project assignment). A workflow that
	// seeds WorkflowVarDir (e.g. tests or flows that pre-stage via
	// PrepareForFix) bypasses the orchestrator's worktree path and uses the
	// caller-provided dir directly.
	if (role == "" || role == string(agent.RoleImplementation)) && dir == "" {
		// agentOrch.StartAgentWithAssignment already translates
		// agent.ErrMaxConcurrentReached into workflow.ErrAgentPoolBusy at the
		// source, so every caller (this adapter, recovery.Recovery) sees the
		// same benign sentinel without needing its own wrap here.
		ag, baselineRef, err := a.agentOrch.StartAgentWithAssignment(taskID, mode, prompt, false, oneShot, cleanRetryRef, outputSchema, assignment)
		if err != nil {
			return "", "", "", err
		}
		return ag.ID, "", baselineRef, nil
	}

	// For roles that don't go through StartAgentWithAssignment (triage, eval,
	// plan, pr-fix, fix-review, test-runner, ...), build RunConfig directly.
	r := agent.Role(role)
	// claimDirectDispatch serializes this path per task, closing the same
	// check-then-act race StartAgentWithAssignment closes for implementation
	// agents: without it, two dispatchers (e.g. a fast ResumeStalled retry)
	// can each observe no running agent and start a duplicate agent against
	// the same task/worktree. claim.Release is idempotent, so the
	// worktree-prep recovery path below can release it early (to unblock a
	// nested same-task recovery dispatch) without this deferred call
	// double-releasing on return.
	claim, ok := a.agents.TryClaimDispatch(taskID)
	if !ok {
		return "", "", "", workflow.ErrDispatchInFlight
	}
	defer claim.Release()

	t, posture, err := a.directRunInputs(taskID, r)
	if err != nil {
		return "", "", "", err
	}

	cfg := agent.RunConfig{
		TaskID:                  taskID,
		Name:                    r.AgentName(t.Title),
		Role:                    r,
		Mode:                    mode,
		Prompt:                  prompt,
		AllowedTools:            allowedTools,
		Model:                   model,
		Provider:                provider,
		IntentID:                assignment.IntentID,
		AdmissionTaskKey:        assignment.AdmissionTaskKey,
		AttemptAccess:           assignmentAttemptAccess(assignment),
		ExperimentID:            assignment.ExperimentID,
		VariantID:               assignment.VariantID,
		RoutingReason:           assignment.RoutingReason,
		AssignmentUnit:          assignment.AssignmentUnit,
		AssignmentKey:           assignment.AssignmentKey,
		DecisionVersion:         assignment.DecisionVersion,
		DisableProviderFailover: assignment.ExperimentID != "",
		Dir:                     dir,
		OneShot:                 oneShot,
		// mode is coerced to headless above, so legacy interactive tasks get
		// no concurrency bypass — they are treated as ordinary headless runs.
		RequestedSkill:         workflowRequestedSkill(prompt),
		ForceInjectedSkill:     assignment.ForceInjectedSkill,
		SkillRecoveryAttempt:   assignment.SkillRecoveryAttempt,
		MaxTurns:               t.MaxTurns,
		RequirePermissions:     agentorch.ResolvePermission(t, a.agentOrch.Cfg()),
		HeadlessPermissionMode: posture,
		SandboxMode:            agentorch.ResolveSandboxMode(t, a.agentOrch.Cfg()),
		ReasoningEffort:        agentorch.FirstNonEmpty(assignment.ReasoningEffort, t.ReasoningEffort, agentorch.ResolveRoleEffort(r, a.agentOrch.Cfg())),
		// Code-author roles (implementation/fix-review/pr-fix) are primed with
		// NOTES.md; verifier roles (review/test-runner/eval) share the same
		// worktree but must stay independent of the implementer's scratchpad.
		SeedWorkingMemory: r.AuthorsCode(),
		// Non-disposable judge roles remain read-only. Independent verifier roles
		// are switched below to writable disposable clones whose mutations are
		// captured and discarded.
		ReadOnlyDir:                   r.JudgesWithoutWriting(),
		ReadOnlyPaths:                 assignment.ReadOnlyPaths,
		RemoteExpectedOutputs:         append([]executioncontract.ExpectedOutput(nil), assignment.RemoteOutputs...),
		RemoteDiscardWorkspaceChanges: !r.AuthorsCode(),
		SidecarDir:                    assignment.RemoteSidecarDir,
		// fork_subagent is a task-level opt-in, but must never reach a
		// verifier role (review/test-runner/eval) — a forked subagent's own
		// token spend would multiply on every independent check, and a
		// verifier has no need for the parallelism it buys an implementer.
		ForkSubagent: t.ForkSubagent && r.AuthorsCode(),
		OutputSchema: outputSchema,
	}
	if len(assignment.RemoteOutputs) > 0 && cfg.SidecarDir == "" && a.sandboxes != nil {
		sidecarDir, sidecarErr := a.sandboxes.SybraHomeDir(taskID)
		if sidecarErr != nil {
			return "", "", "", fmt.Errorf("prepare remote sidecar directory: %w", sidecarErr)
		}
		cfg.SidecarDir = sidecarDir
	}
	a.withExperiencePrompt(&cfg, r, t)

	cleanRetryReset := false
	if needsWorktree {
		t, cfg.Dir, cleanRetryReset, err = a.ensureWorktreeDir(t, taskID, r, cfg.Dir, cleanRetryRef, claim)
		if err != nil {
			return "", "", "", err
		}
	}
	if cfg.Dir != "" && cleanRetryRef != "" && !cleanRetryReset {
		if _, resetErr := a.resetWorktreeForRetry(t, cfg.Dir, cleanRetryRef); resetErr != nil {
			return "", "", "", resetErr
		}
	}
	hasCanonicalWorktree := cfg.Dir != ""
	if cfg.Dir == "" {
		// A direct-dispatch role (notably a best-of-N judge) may deliberately
		// have no worktree: it reads the candidate worktrees by absolute path.
		// Give it an isolated temporary cwd rather than the operator's Sybra
		// home. Besides keeping relative writes away from the board, this is a
		// distinct ReadOnlyDir under enforce mode, so the sandbox can re-bind it
		// read-only without also re-binding all of os.TempDir read-only.
		cfg.Dir, err = fallbackAgentWorkingDir()
		if err != nil {
			return "", "", "", err
		}
	}

	canonicalDir := cfg.Dir
	ag, baselineRef, err := a.launchDirectWithVerification(t, r, &cfg, hasCanonicalWorktree)
	if err != nil {
		return "", "", "", err
	}

	if recErr := a.recordSystemAgentStart(taskID, role, mode, cfg, ag); recErr != nil {
		return "", "", "", recErr
	}

	return ag.ID, canonicalDir, baselineRef, nil
}

func (a *agentAdapter) launchDirectWithVerification(t task.Task, role agent.Role, cfg *agent.RunConfig, hasCanonicalWorktree bool) (ag *agent.Agent, baselineRef string, err error) {
	canonicalDir := cfg.Dir
	var lease verification.Lease
	if role.IsVerifier() && a.verification != nil {
		if hasCanonicalWorktree {
			lease, err = a.verification.Prepare(context.Background(), t.ID, string(role), canonicalDir)
		} else {
			lease, err = a.verification.PrepareScratch(t.ID, string(role))
		}
		if err != nil {
			return nil, "", fmt.Errorf("prepare disposable verification lease: %w", err)
		}
		defer func() {
			if err != nil && ag == nil {
				a.verification.Release(lease)
			}
		}()
		cfg.EphemeralSandboxHome = lease.ScratchDir
		if lease.WorkspaceDir != "" {
			// The verifier executes in a per-run clone, but durable admission
			// identifies the workflow effect against its canonical worktree.
			// Replaying the same effect with a fresh clone must not look like a
			// different intent to the attempt ledger.
			cfg.AdmissionWorktree = canonicalDir
			cfg.Dir = lease.WorkspaceDir
			// The clone is deliberately writable: its entire mutation footprint is
			// captured and discarded, while the canonical checkout stays outside
			// the process sandbox's write roots.
			cfg.ReadOnlyDir = false
		}
		cfg.BeforeStart = func(agentID string) error {
			return a.verification.BindAgent(lease.ID, agentID)
		}
	}
	ag, baselineRef, err = a.launchCertifiedDirect(t, role, cfg)
	if err != nil || lease.WorkspaceDir == "" {
		return ag, baselineRef, err
	}
	// Tamper baselines and workflow state are authoritative-branch data, not a
	// detached verifier clone's private HEAD.
	return ag, agentorch.CurrentWorktreeHead(context.Background(), canonicalDir), nil
}

func (a *agentAdapter) launchCertifiedDirect(t task.Task, role agent.Role, cfg *agent.RunConfig) (*agent.Agent, string, error) {
	baselineRef := agentorch.CurrentWorktreeHead(context.Background(), cfg.Dir)
	a.configureTestRunnerRun(cfg, t.ID, role, t)
	ag, err := a.agents.Run(*cfg)
	if err != nil {
		if a.runenv != nil && runenv.IsEnvironmentFailure(err) {
			a.runenv.InvalidateTask(t.ID)
		}
		return nil, "", translatePoolBusy(err)
	}
	return ag, baselineRef, nil
}

// fallbackAgentWorkingDir creates an otherwise empty, per-run cwd for a
// direct-dispatch agent that intentionally has no task worktree. It must be a
// child of the OS temp directory rather than os.TempDir itself: read-only
// judge runs re-bind their cwd after the sandbox's broad tmp write root, and
// locking the whole tmp root would break provider scratch files. The OS temp
// cleaner reclaims these directories if a stopped/crashed process leaves one
// behind.
func fallbackAgentWorkingDir() (string, error) {
	dir, err := os.MkdirTemp("", "sybra-agent-cwd-")
	if err != nil {
		return "", fmt.Errorf("create fallback agent working directory: %w", err)
	}
	return dir, nil
}

// ensureWorktreeDir resolves the cwd a direct-dispatch agent should run in.
// A provided dir is usually already prepared, but if a workflow retry carries
// a stale `_dir` pointing at a deleted Sybra-managed worktree, rebuild the
// role's expected worktree instead of spawning into a dead path and feeding
// the workflow's start-failure circuit breaker.
func (a *agentAdapter) ensureWorktreeDir(t task.Task, taskID string, role agent.Role, dir, cleanRetryRef string, claim *agent.DispatchClaim) (updated task.Task, resolvedDir string, cleanRetryReset bool, err error) {
	if dir == "" {
		return a.resolveWorktreeDir(t, taskID, role, cleanRetryRef, claim)
	}
	if _, statErr := os.Stat(dir); statErr == nil {
		if cleanRetryRef != "" {
			ready, readyErr := a.cleanRetryWorktreeReady(t, taskID, role, dir)
			if readyErr != nil {
				return t, "", false, readyErr
			}
			if ready {
				return t, dir, true, nil
			}
			if role.AuthorsCode() && strings.TrimSpace(t.Branch) != "" {
				updated, resolvedDir, err := a.recreateManagedWorktreeForRetry(t, taskID, role, dir, claim)
				return updated, resolvedDir, true, err
			}
		}
		repair, repairErr := a.providedWorktreeNeedsRepair(t, taskID, role, dir)
		if repairErr != nil {
			return t, "", false, repairErr
		}
		if !repair {
			return t, dir, false, nil
		}
		updated, resolvedDir, cleanRetryReset, err := a.reprepareProvidedWorktreeDir(t, taskID, role, dir, cleanRetryRef, claim)
		return updated, resolvedDir, cleanRetryReset, err
	} else if !os.IsNotExist(statErr) {
		return t, "", false, fmt.Errorf("stat provided worktree dir %s: %w", dir, statErr)
	}
	if cleanRetryRef != "" {
		updated, resolvedDir, _, err = a.reprepareProvidedWorktreeDir(t, taskID, role, dir, "", claim)
		return updated, resolvedDir, true, err
	}
	updated, resolvedDir, cleanRetryReset, err = a.reprepareProvidedWorktreeDir(t, taskID, role, dir, cleanRetryRef, claim)
	return updated, resolvedDir, cleanRetryReset, err
}

func (a *agentAdapter) cleanRetryWorktreeReady(t task.Task, taskID string, role agent.Role, dir string) (bool, error) {
	if !role.AuthorsCode() || strings.TrimSpace(t.Branch) == "" || strings.TrimSpace(dir) == "" {
		return false, nil
	}
	currentBranch, err := project.CurrentBranch(context.Background(), dir)
	if err != nil {
		return false, fmt.Errorf("resolve clean retry worktree branch %s: %w", dir, err)
	}
	if currentBranch != t.Branch {
		a.agentOrch.Logger().Warn("workflow.worktree.clean-retry.branch-mismatch",
			"task_id", taskID, "role", role, "path", dir, "branch", currentBranch, "want_branch", t.Branch)
		return false, nil
	}
	dirty, err := project.IsWorktreeDirty(context.Background(), dir)
	if err != nil {
		return false, fmt.Errorf("inspect clean retry worktree %s: %w", dir, err)
	}
	if dirty {
		a.agentOrch.Logger().Warn("workflow.worktree.clean-retry.dirty",
			"task_id", taskID, "role", role, "path", dir)
		return false, nil
	}
	if project.HasUnpushedCommits(context.Background(), dir) {
		a.agentOrch.Logger().Warn("workflow.worktree.clean-retry.unpushed",
			"task_id", taskID, "role", role, "path", dir)
		return false, nil
	}
	return true, nil
}

func (a *agentAdapter) providedWorktreeNeedsRepair(t task.Task, taskID string, role agent.Role, dir string) (bool, error) {
	// PR review retries carry the previous step's _dir. Even when that
	// checkout is healthy, the PR may have advanced since the prior attempt;
	// route it back through PrepareForReview so the detached HEAD is compared
	// with the freshly fetched refs/pull/<N>/head.
	if role == agent.RoleReview && t.PRNumber != 0 {
		return true, nil
	}
	if !role.AuthorsCode() || strings.TrimSpace(t.Branch) == "" {
		return false, nil
	}
	currentBranch, err := project.CurrentBranch(context.Background(), dir)
	if err != nil {
		if t.WorktreeDir != "" {
			return false, fmt.Errorf("resolve adopted worktree branch %s: %w", dir, err)
		}
		a.agentOrch.Logger().Warn("workflow.worktree.branch-unreadable",
			"task_id", taskID, "role", role, "path", dir, "err", err)
		return true, nil
	}
	if currentBranch == t.Branch {
		return false, nil
	}
	if t.WorktreeDir != "" {
		return false, fmt.Errorf("adopted worktree branch %q does not match task branch %q", currentBranch, t.Branch)
	}
	a.agentOrch.Logger().Warn("workflow.worktree.branch-mismatch",
		"task_id", taskID, "role", role, "path", dir, "branch", currentBranch, "want_branch", t.Branch)
	return true, nil
}

func (a *agentAdapter) reprepareProvidedWorktreeDir(t task.Task, taskID string, role agent.Role, dir, cleanRetryRef string, claim *agent.DispatchClaim) (updated task.Task, resolvedDir string, cleanRetryReset bool, err error) {
	if _, statErr := os.Stat(dir); statErr == nil {
		if t.WorktreeDir != "" {
			return t, "", false, fmt.Errorf("provided adopted worktree dir %s is stale for task branch %q", dir, t.Branch)
		}
		resolvedDir, cleanRetryReset, err = a.reprepareExistingWorktreeDir(t, taskID, role, dir, cleanRetryRef, claim)
		return t, resolvedDir, cleanRetryReset, err
	} else if !os.IsNotExist(statErr) {
		return t, "", false, fmt.Errorf("stat provided worktree dir %s: %w", dir, statErr)
	}
	t, err = a.agentOrch.AutoAssignProject(t)
	if err != nil {
		return t, "", false, err
	}
	if t.ProjectID == "" {
		return t, "", false, fmt.Errorf("task %s has no project_id: refusing to start %s agent without isolated worktree: %w", taskID, role, workflow.ErrNoProjectAssigned)
	}
	proj, err := a.projects.Get(t.ProjectID)
	if err != nil {
		return t, "", false, err
	}
	if rErr := a.agentOrch.Worktrees().PruneMissingWorktree(context.Background(), proj.ClonePath, dir); rErr != nil {
		return t, "", false, fmt.Errorf("reconcile missing worktree dir %s: %w", dir, rErr)
	}
	resolvedDir, cleanRetryReset, err = a.reprepareMissingWorktreeDir(t, taskID, role, dir, claim)
	return t, resolvedDir, cleanRetryReset, err
}

func (a *agentAdapter) recreateManagedWorktreeForRetry(t task.Task, taskID string, role agent.Role, dir string, claim *agent.DispatchClaim) (updated task.Task, resolvedDir string, err error) {
	if t.WorktreeDir != "" {
		return t, "", fmt.Errorf("adopted worktree %s is not reusable for clean retry on task branch %q", dir, t.Branch)
	}
	proj, err := a.projects.Get(t.ProjectID)
	if err != nil {
		return t, "", err
	}
	a.agentOrch.Logger().Warn("workflow.worktree.clean-retry.recreate",
		"task_id", taskID, "role", role, "path", dir, "branch", t.Branch)
	if fetchErr := project.FetchOrigin(context.Background(), proj.ClonePath); fetchErr != nil {
		a.agentOrch.Logger().Warn("workflow.worktree.clean-retry.fetch",
			"task_id", taskID, "role", role, "project", proj.ID, "branch", t.Branch, "err", fetchErr)
	}
	remoteRef := "refs/remotes/origin/" + t.Branch
	if remoteHead, ok := project.ResolveBareRef(context.Background(), proj.ClonePath, remoteRef); ok {
		if remoteHead != "" {
			if err := project.SetBranchTo(context.Background(), proj.ClonePath, t.Branch, remoteHead); err != nil {
				return t, "", fmt.Errorf("reset task branch %s to pushed tip %s: %w", t.Branch, remoteHead, err)
			}
		}
	} else if project.BranchExists(context.Background(), proj.ClonePath, t.Branch) {
		if err := project.DeleteBranch(context.Background(), proj.ClonePath, t.Branch); err != nil {
			return t, "", fmt.Errorf("drop local-only task branch %s before clean retry recreate: %w", t.Branch, err)
		}
	}
	if err := project.RemoveWorktreeReconcile(context.Background(), proj.ClonePath, dir); err != nil {
		return t, "", fmt.Errorf("reconcile clean retry worktree dir %s: %w", dir, err)
	}
	resolvedDir, _, err = a.reprepareMissingWorktreeDir(t, taskID, role, dir, claim)
	return t, resolvedDir, err
}

func (a *agentAdapter) reprepareExistingWorktreeDir(t task.Task, taskID string, role agent.Role, dir, cleanRetryRef string, claim *agent.DispatchClaim) (resolvedDir string, cleanRetryReset bool, err error) {
	resolvedDir, cleanRetryReset, err = a.reprepareProvidedWorktreeForRole(t, taskID, role, dir, claim)
	if err != nil {
		return "", false, err
	}
	if cleanRetryRef != "" {
		if _, resetErr := a.resetWorktreeForRetry(t, resolvedDir, cleanRetryRef); resetErr != nil {
			return "", false, resetErr
		}
		cleanRetryReset = true
	}
	return resolvedDir, cleanRetryReset, nil
}

func (a *agentAdapter) reprepareMissingWorktreeDir(t task.Task, taskID string, role agent.Role, missingDir string, claim *agent.DispatchClaim) (dir string, cleanRetryReset bool, err error) {
	if t.WorktreeDir != "" {
		return "", false, fmt.Errorf("provided adopted worktree dir %s missing: %w", missingDir, os.ErrNotExist)
	}
	a.agentOrch.Logger().Warn("workflow.worktree.dir-missing",
		"task_id", taskID, "role", role, "path", missingDir)
	return a.reprepareProvidedWorktreeForRole(t, taskID, role, missingDir, claim)
}

func (a *agentAdapter) reprepareProvidedWorktreeForRole(t task.Task, taskID string, role agent.Role, dir string, claim *agent.DispatchClaim) (resolvedDir string, cleanRetryReset bool, err error) {
	switch role {
	case agent.RolePRFix, agent.RoleTestFix:
		if t.PRNumber == 0 {
			return "", false, fmt.Errorf("prepare fix worktree: task has no pr_number")
		}
		d, wtErr := a.agentOrch.Worktrees().PrepareForFix(context.Background(), t, t.PRNumber)
		if wtErr != nil {
			claim.Release()
			return "", false, fmt.Errorf("prepare fix worktree: %w", wtErr)
		}
		return d, false, nil
	case agent.RoleReview:
		if t.PRNumber != 0 {
			d, wtErr := a.agentOrch.Worktrees().PrepareForReview(context.Background(), t)
			if wtErr != nil {
				claim.Release()
				return "", false, fmt.Errorf("prepare review worktree: %w", wtErr)
			}
			return d, false, nil
		}
		fallthrough
	default:
		d, wtErr := a.agentOrch.Worktrees().PrepareForTask(context.Background(), t, nil)
		if wtErr != nil {
			claim.Release()
			return "", false, a.classifyDirectDispatchWorktreeErr(taskID, wtErr)
		}
		return d, false, nil
	}
}

func workflowRequestedSkill(prompt string) string {
	names := skillinvoke.InvokedNames(prompt)
	if len(names) != 1 {
		return ""
	}
	return names[0]
}

func (a *agentAdapter) configureTestRunnerRun(cfg *agent.RunConfig, taskID string, role agent.Role, t task.Task) {
	if a.sandboxes != nil {
		if role == agent.RoleTestRunner {
			cfg.ExtraEnv = a.agentOrch.SandboxEnv(taskID, cfg.Dir, t)
		} else if inst := a.sandboxes.Get(taskID); inst != nil {
			cfg.ExtraEnv = inst.EnvVars()
		}
	}
	if role != agent.RoleTestRunner {
		return
	}
	cfg.PlaywrightMCPEligible = true
	cfg.PlaywrightMCPOutputDir = filepath.Join(cfg.Dir, worktree.EvidenceDirName)
}

// resolveWorktreeDir auto-assigns a project to t (if needed), optionally
// resets the worktree for a clean retry, and prepares the worktree dir for
// the direct-dispatch path. claim is the caller's held dispatch claim: on a
// worktree-prep failure this releases it early (see the claim.Release() call
// below) rather than the caller's own deferred release, which — since
// DispatchClaim.Release is idempotent — is then a safe no-op.
func (a *agentAdapter) resolveWorktreeDir(t task.Task, taskID string, role agent.Role, cleanRetryRef string, claim *agent.DispatchClaim) (updated task.Task, dir string, cleanRetryReset bool, err error) {
	t, err = a.agentOrch.AutoAssignProject(t)
	if err != nil {
		return t, "", false, err
	}
	if t.ProjectID == "" {
		return t, "", false, fmt.Errorf("task %s has no project_id: refusing to start %s agent without isolated worktree: %w", taskID, role, workflow.ErrNoProjectAssigned)
	}
	if cleanRetryRef != "" {
		target := a.agentOrch.Worktrees().PathFor(t)
		if _, statErr := os.Stat(target); statErr == nil {
			ready, readyErr := a.cleanRetryWorktreeReady(t, taskID, role, target)
			if readyErr != nil {
				return t, "", false, readyErr
			}
			if ready {
				cleanRetryReset = true
			} else if role.AuthorsCode() && strings.TrimSpace(t.Branch) != "" {
				updated, resolvedDir, recreateErr := a.recreateManagedWorktreeForRetry(t, taskID, role, target, claim)
				return updated, resolvedDir, true, recreateErr
			}
		} else if os.IsNotExist(statErr) {
			cleanRetryReset = true
		} else {
			return t, "", false, fmt.Errorf("stat clean retry worktree: %w", statErr)
		}
		if !cleanRetryReset {
			reset, resetErr := a.resetWorktreeForRetry(t, "", cleanRetryRef)
			if resetErr != nil {
				return t, "", false, resetErr
			}
			cleanRetryReset = reset
		}
	}
	// context.Background(): StartAgent implements workflow.AgentDispatcher,
	// a fixed interface signature with no ctx parameter (invoked from many
	// workflow step-execution call sites); see the Engine.SetContext /
	// e.ctx pattern for why threading ctx across that interface is out of
	// scope for this pass.
	d, wtErr := a.agentOrch.Worktrees().PrepareForTask(context.Background(), t, nil)
	if wtErr != nil {
		// Release our dispatch claim before classifying/recovering: a
		// rebase-blocked wtErr routes through RecoverFromWorktreePrepFailure
		// -> RecoverStaleBranchConflict, which synchronously starts the
		// branch-conflict-fix workflow and dispatches ITS OWN "fix" agent for
		// this same taskID. dispatchClaims is a non-reentrant per-task map
		// (agent.Manager.ClaimTaskDispatch), so if we still held the claim
		// here, that nested dispatch would collide with it and park on
		// ErrDispatchInFlight without ever starting the conflict-resolution
		// agent. We're bailing out of this dispatch
		// attempt regardless (wtErr != nil means we never call a.agents.Run
		// below), so releasing early is safe: it doesn't overlap with our own
		// (never-attempted) agent start.
		claim.Release()
		return t, "", cleanRetryReset, a.classifyDirectDispatchWorktreeErr(taskID, wtErr)
	}
	return t, d, cleanRetryReset, nil
}

// classifyDirectDispatchWorktreeErr translates a PrepareForTask failure from
// the direct-dispatch path into the error execRunAgent should see. A tracked
// agent still live in the worktree (worktree.ErrAgentRunning) is a benign
// timing collision with a stale "no agent running" read upstream, not a real
// worktree conflict — treat it like ErrDispatchInFlight so the step parks
// and retries once the agent is genuinely idle, instead of escalating.
func (a *agentAdapter) classifyDirectDispatchWorktreeErr(taskID string, wtErr error) error {
	if errors.Is(wtErr, worktreeerr.ErrAgentRunning) {
		return workflow.ErrDispatchInFlight
	}
	// handled=true covers every branch of RecoverFromWorktreePrepFailure that
	// already wrote the task's terminal status itself: an autonomous
	// conflict-fix redispatch (recovered=true), MarkRebaseBlocked parking the
	// task at human-required, or its already-resolved-on-remote downgrade to
	// in_review. Only checking recovered (as before) let an unhandled-but-not-
	// recovered rebase failure fall through to `return wtErr` below even
	// though the status was already resolved — the caller's surfaceStartFailure
	// would then reclassify the same wtErr and clobber that resolved status
	// (e.g. overwriting in_review back to human-required) using a stale
	// pre-dispatch status snapshot.
	if handled, _ := a.agentOrch.RecoverFromWorktreePrepFailure(a.tasks, taskID, wtErr); handled {
		return workflow.ErrDispatchInFlight
	}
	return wtErr
}

func (a *agentAdapter) resetWorktreeForRetry(t task.Task, dir, ref string) (bool, error) {
	// context.Background(): StartAgent implements workflow.AgentDispatcher,
	// a fixed interface signature with no ctx parameter (see the earlier
	// comment on the PrepareForTask call in this file).
	target, reset, err := a.agentOrch.Worktrees().ResetForRetry(context.Background(), t, dir, ref)
	if err != nil {
		a.agentOrch.Logger().Warn("worktree.clean-retry.reset", "task_id", t.ID, "path", target, "ref", ref, "err", err)
		return false, err
	}
	if reset {
		a.agentOrch.Logger().Info("worktree.clean-retry.reset", "task_id", t.ID, "path", target, "ref", ref)
	}
	return reset, nil
}

func (a *agentAdapter) recordSystemAgentStart(taskID, role, mode string, cfg agent.RunConfig, ag *agent.Agent) error {
	var nextStatus *task.Status
	if cur, rerr := a.tasks.Get(taskID); rerr == nil && cur.Status == task.StatusHumanRequired {
		nextStatus = task.Ptr(task.StatusInProgress)
	}
	if addErr := a.tasks.AddRunWithStatusBy(taskID, "workflow.engine.record_agent_start", task.AgentRun{
		AgentID:                 ag.ID,
		Role:                    role,
		Mode:                    mode,
		Provider:                ag.Provider,
		Model:                   ag.Model,
		ExperimentID:            ag.ExperimentID,
		VariantID:               ag.VariantID,
		RoutingReason:           ag.RoutingReason,
		AssignmentUnit:          ag.AssignmentUnit,
		AssignmentKey:           ag.AssignmentKey,
		DecisionVersion:         ag.DecisionVersion,
		ReasoningEffort:         ag.ReasoningEffort,
		RequestedSkill:          ag.RequestedSkill,
		SkillExecutionMode:      ag.SkillExecutionMode,
		ResolvedSkillSourceHash: ag.ResolvedSkillSourceHash,
		SkillConformance:        ag.SkillConformance,
		OneShot:                 cfg.OneShot,
		State:                   string(agent.StateRunning),
		StartedAt:               ag.StartedAt,
		Prompt:                  cfg.Prompt,
	}, nextStatus); addErr != nil {
		if errors.Is(addErr, os.ErrNotExist) {
			// A stale workflow dispatcher can lose the task underneath it
			// (delete/cleanup/terminal teardown) after StartAgent succeeded but
			// before the AgentRun write. Treat that as a silent no-op: the
			// workflow already no longer owns a task file to update.
			return nil
		}
		// Not just lost history: reviewbudget.Budget (#2499) counts Role=="review"
		// AgentRuns to bound the automated review loop, so a silently dropped
		// write here under-counts that durable budget for this task, same as
		// #2199 named. Fail closed like StartReviewAgent (review/inbound.go):
		// signal the agent that already started before surfacing the error
		// (best-effort — does not block for exit), so a workflow-level retry
		// targets an agent Sybra no longer considers live instead of piling a
		// second one on top of an unrecorded process still running.
		slog.Error("agent-adapter.add-run", "task_id", taskID, "agent_id", ag.ID, "err", addErr)
		if stopErr := a.agents.StopAgent(ag.ID); stopErr != nil {
			return fmt.Errorf("record agent run: %w; stop started agent %s: %w", addErr, ag.ID, stopErr)
		}
		return fmt.Errorf("record agent run: %w", addErr)
	}
	return nil
}

func (a *agentAdapter) withExperiencePrompt(cfg *agent.RunConfig, role agent.Role, t task.Task) {
	if a == nil || a.experience == nil || a.agentOrch == nil || a.agentOrch.Cfg() == nil {
		return
	}
	if !roleReceivesExperience(role) || !a.agentOrch.Cfg().Experience.Enabled || t.ProjectID == "" {
		return
	}
	projStore := a.projects
	if projStore == nil {
		projStore = a.agentOrch.Projects()
	}
	if projStore == nil {
		return
	}
	proj, err := projStore.Get(t.ProjectID)
	if err != nil || proj.ID == "" || !a.agentOrch.Cfg().AllowsProjectType(string(proj.Type)) {
		return
	}
	projectKey := experience.ProjectKey(proj)
	records, err := a.experience.Query(projectKey, a.agentOrch.Cfg().Experience.MaxRecords)
	if err != nil || len(records) == 0 {
		return
	}
	// Gate each candidate on TTL + tag-overlap trigger instead of injecting
	// every retained record unconditionally — see experience.Eligible.
	ttlDays := a.agentOrch.Cfg().Experience.TTLDays
	now := time.Now()
	records = slices.DeleteFunc(records, func(rec experience.Record) bool {
		return !experience.Eligible(rec, t.Tags, ttlDays, now)
	})
	if len(records) == 0 {
		return
	}
	appendix := experience.FormatForPrompt(records)
	if appendix == "" {
		return
	}
	cfg.Prompt += appendix
	ids := make([]string, 0, len(records))
	for i := range records {
		ids = append(ids, records[i].TaskID)
	}
	data := map[string]any{"record_ids": ids, "role": string(role)}
	if proj.Type == project.ProjectTypeWork {
		data["project_key"] = projectKey
	} else {
		data["project_id"] = t.ProjectID
	}
	a.agentOrch.LogAudit(audit.EventExperienceInjected, t.ID, "", data)
}

func roleReceivesExperience(role agent.Role) bool {
	return role == agent.RolePlan || role == agent.RoleTriage
}

func (a *agentAdapter) ensureTestRunnerCapacity(role agent.Role) error {
	if role == agent.RoleTestRunner && a.agents.CountLiveByRole(agent.RoleTestRunner) >= a.agentOrch.Cfg().TestingMaxConcurrent() {
		return workflow.ErrTestRunnerBusy
	}
	return nil
}

func (a *agentAdapter) HasRunningAgent(taskID string) bool {
	return a.agents.HasRunningAgentForTask(taskID)
}

func (a *agentAdapter) HasOtherRunningAgentForTask(taskID, exceptAgentID string) bool {
	return a.agents.HasOtherRunningAgentForTask(taskID, exceptAgentID)
}

func (a *agentAdapter) FindRunningAgentForRole(taskID, role string) (string, bool) {
	r := agent.Role(role)
	ag := a.agents.FindRunningAgentForTask(taskID, r)
	if ag == nil {
		return "", false
	}
	return ag.ID, true
}

func (a *agentAdapter) StopAgentsForTask(taskID, role string) {
	r := agent.Role(role)
	for _, ag := range a.agents.FindAllRunningAgentsForTask(taskID, r) {
		_ = a.agents.StopAgent(ag.ID)
	}
}

func (a *agentAdapter) SendPrompt(agentID, message string) error {
	return a.agents.SendPromptToAgent(agentID, message)
}

func (a *agentAdapter) DefaultProvider() string {
	return a.agents.DefaultProvider()
}

func (a *agentAdapter) ProviderRateLimited(provider string) bool {
	return a.agents.ProviderRateLimited(provider)
}

func (a *agentAdapter) ProviderCanFailover(provider string) bool {
	return a.agents.ProviderCanFailover(provider)
}

func (a *agentAdapter) ProviderHealthy(provider string) bool {
	return a.agents.ProviderHealthy(provider)
}

func (a *agentAdapter) TryClaimDispatch(taskID string) (workflow.DispatchClaim, bool) {
	return a.agents.TryClaimDispatch(taskID)
}

func (a *agentAdapter) IsDispatching(taskID string) bool {
	return a.agents.IsDispatching(taskID)
}

func (a *agentAdapter) AdmitDispatch(taskID, role, mode string) (admit bool, reason string) {
	// mode is coerced to headless before every AdmitDispatch call site
	// (resolveRunAgentMode, spawnParallelChild, spawnBestOfNAttempt) — there
	// is no dispatchable "interactive" mode left to bypass the gate for.
	if a.pressure == nil {
		return true, ""
	}
	admit, reason = a.pressure.Admit()
	if !admit {
		a.agentOrch.LogAudit(audit.EventAgentDeferredPressure, taskID, "", map[string]any{
			"role":   role,
			"mode":   mode,
			"reason": reason,
		})
		return false, reason
	}
	return true, ""
}

// CheckTaskCostBudget implements workflow.CostBudgetChecker for the
// best_of_n/judge preflight — see agentorch.Orchestrator.CheckTaskCostBudget.
func (a *agentAdapter) CheckTaskCostBudget(taskID string) error {
	return a.agentOrch.CheckTaskCostBudget(taskID)
}
