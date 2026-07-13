package sybra

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

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

// PromptLabService exposes Prompt Lab proposal approve/reject operations as
// Wails-bound methods. Proposals are plain tasks (tagged
// "prompt-lab-proposal" + "requires-human", status human-required) until
// approval, which starts the dedicated prompt-lab authoring workflow.
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
func (s *PromptLabService) ApproveProposal(id string) (task.Task, error) {
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

	if s.workflowEngine != nil {
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
	}

	s.appendProgress(id, artifact.ProgressKindDecision, "Approved: started Prompt Lab variant authoring + offline eval workflow")
	return t, nil
}

func (s *PromptLabService) promptLabDispatchStarted(id, matched string, err error) bool {
	if err == nil {
		return matched != ""
	}
	if !errors.Is(err, workflow.ErrWorkflowAlreadyActive) {
		return false
	}
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
	if !slices.Contains(cur.Tags, promptlab.ProposalTag) || cur.Status != task.StatusHumanRequired {
		return fmt.Errorf("task %s is not a pending prompt-lab proposal (status=%s)", cur.ID, cur.Status)
	}
	return nil
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
