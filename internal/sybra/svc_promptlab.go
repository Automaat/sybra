package sybra

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/promptlab"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

// rejectedNoFeedbackReason is recorded as StatusReason when a reviewer
// rejects a prompt-lab proposal without typing feedback, so the reject is
// still auditable from the task's status alone.
const rejectedNoFeedbackReason = "rejected (no feedback provided)"

const (
	approvedProgressNote     = "Approved: started Prompt Lab variant authoring + offline eval workflow"
	autoApprovedProgressNote = "Auto-approved (prompt_lab.auto_approve): started Prompt Lab variant authoring + offline eval workflow"
)

const (
	promptLabAlreadyActiveWait = 500 * time.Millisecond
	promptLabAlreadyActivePoll = 25 * time.Millisecond
)

// PromptLabService exposes Prompt Lab proposal approve/reject operations as
// Wails-bound methods. Proposals are plain tasks (tagged
// "prompt-lab-proposal" + "requires-human", status human-required) until
// approval, which starts the dedicated prompt-lab authoring workflow.
//
// Approval is not necessarily a human: under prompt_lab.auto_approve
// (default on) promptLabCoordinator approves each proposal as it files it,
// making these methods the manual override rather than the only way
// through. What gates production either way is
// prompteval.Gate.AllowEnrollment inside the authoring workflow — approval
// only buys a candidate the right to be authored and screened.
type PromptLabService struct {
	tasks          *task.Manager
	artifacts      *artifact.Store
	projects       *project.Store
	workflowEngine *workflow.Engine
}

// ApproveProposal greenlights a pending prompt-lab proposal: moves it to
// in-progress, drops the requires-human tag, and starts the dedicated
// prompt-lab authoring workflow. Errors without mutating if the task is not a
// pending proposal (wrong status, or missing the prompt-lab-proposal tag).
//
// promptLabCoordinator drives the same path unattended via autoApprove when
// prompt_lab.auto_approve is set, so every guard, dispatch, and
// revert-on-failure below must hold for a caller that is not a human; the
// two differ only in the progress note left in the task's decision log.
func (s *PromptLabService) ApproveProposal(id string) (task.Task, error) {
	return s.approveProposal(id, approvedProgressNote)
}

func (s *PromptLabService) autoApprove(id string) error {
	_, err := s.approveProposal(id, autoApprovedProgressNote)
	return err
}

func (s *PromptLabService) approveProposal(id, progressNote string) (task.Task, error) {
	if s.workflowEngine == nil {
		return task.Task{}, errors.New("prompt-lab approval unavailable: no workflow engine to start the authoring workflow")
	}
	t, err := s.tasks.UpdateFn(id, func(cur task.Task) (task.Update, error) {
		if err := requirePendingProposal(cur); err != nil {
			return task.Update{}, err
		}
		status := task.StatusInProgress
		reason := ""
		tags := removeTag(cur.Tags, "requires-human")
		update := task.Update{
			Status:       &status,
			StatusReason: &reason,
			Tags:         &tags,
		}
		if cur.ProjectID == "" {
			if projectID := promptLabTargetProjectID(s.projects); projectID != "" {
				update.ProjectID = &projectID
			}
		}
		return update, nil
	})
	if err != nil {
		return task.Task{}, err
	}

	matched, dispatchErr := s.workflowEngine.DispatchEvent(
		id,
		"task.status_changed",
		map[string]string{"task.status": string(task.StatusInProgress)},
		nil,
	)
	if !s.promptLabDispatchStarted(id, matched, dispatchErr) {
		failure := "no prompt-lab workflow matched"
		if dispatchErr != nil {
			failure = dispatchErr.Error()
		}
		revertReason := "Prompt Lab approval failed to start authoring workflow: " + failure
		status := task.StatusHumanRequired
		tags := mergeTag(t.Tags, "requires-human")
		reverted, revertErr := s.tasks.Update(id, task.Update{
			Status:       &status,
			StatusReason: &revertReason,
			Tags:         tags,
		})
		if revertErr != nil {
			return task.Task{}, fmt.Errorf("%s; additionally failed to restore human-required: %w", revertReason, revertErr)
		}
		return reverted, errors.New(revertReason)
	}

	s.appendProgress(id, artifact.ProgressKindDecision, progressNote)
	return t, nil
}

func (s *PromptLabService) promptLabDispatchStarted(id, matched string, err error) bool {
	if err == nil {
		return matched != ""
	}
	if !errors.Is(err, workflow.ErrWorkflowAlreadyActive) {
		return false
	}
	deadline := time.Now().Add(promptLabAlreadyActiveWait)
	for {
		if s.promptLabTaskHasActiveWorkflow(id) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(promptLabAlreadyActivePoll)
	}
}

func (s *PromptLabService) promptLabTaskHasActiveWorkflow(id string) bool {
	t, getErr := s.tasks.Get(id)
	if getErr != nil || t.Workflow == nil {
		return false
	}
	return t.Workflow.State != workflow.ExecCompleted && t.Workflow.State != workflow.ExecFailed
}

// RejectProposal declines a pending prompt-lab proposal: moves it to
// cancelled and records the reviewer's feedback (or a fixed sentinel when
// no feedback was given) as the status reason. Errors without mutating if
// the task is not a pending proposal.
func (s *PromptLabService) RejectProposal(id, feedback string) (task.Task, error) {
	reason := strings.TrimSpace(feedback)
	if reason == "" {
		reason = rejectedNoFeedbackReason
	}

	t, err := s.tasks.UpdateFn(id, func(cur task.Task) (task.Update, error) {
		if err := requirePendingProposal(cur); err != nil {
			return task.Update{}, err
		}
		status := task.StatusCancelled
		tags := removeTag(cur.Tags, "requires-human")
		return task.Update{
			Status:       &status,
			StatusReason: &reason,
			Tags:         &tags,
		}, nil
	})
	if err != nil {
		return task.Task{}, err
	}

	s.appendProgress(id, artifact.ProgressKindBlocker, reason)
	return t, nil
}

// requirePendingProposal guards ApproveProposal/RejectProposal against
// acting on anything but a currently-pending prompt-lab proposal, closing
// the race a stale browser tab could otherwise re-fire (double approve,
// approve-after-reject, etc).
func requirePendingProposal(cur task.Task) error {
	if !slices.Contains(cur.Tags, promptlab.ProposalTag) || !isPendingProposalStatus(cur.Status) {
		return fmt.Errorf("task %s is not a pending prompt-lab proposal (status=%s)", cur.ID, cur.Status)
	}
	return nil
}

func isPendingProposalStatus(s task.Status) bool {
	return s == task.StatusHumanRequired || s == task.StatusTodo
}

// removeTag returns tags with every occurrence of target removed.
func removeTag(tags []string, target string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t != target {
			out = append(out, t)
		}
	}
	return out
}

func mergeTag(tags []string, target string) *[]string {
	out := append([]string(nil), tags...)
	if !slices.Contains(out, target) {
		out = append(out, target)
	}
	return &out
}

// appendProgress records the approve/reject decision in the task's progress
// log, best-effort: a failure is logged and swallowed, never rolling back
// the already-committed status change.
func (s *PromptLabService) appendProgress(taskID, kind, message string) {
	if s.artifacts == nil {
		return
	}
	err := s.artifacts.AppendProgress(taskID, artifact.ProgressEntry{
		Kind:    kind,
		Message: message,
	})
	if err != nil {
		slog.Warn("promptlab.progress.append_failed", "task_id", taskID, "kind", kind, "err", err)
	}
}
