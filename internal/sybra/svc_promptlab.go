package sybra

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/task"
)

// rejectedNoFeedbackReason is recorded as StatusReason when a reviewer
// rejects a prompt-lab proposal without typing feedback, so the reject is
// still auditable from the task's status alone.
const rejectedNoFeedbackReason = "rejected via GUI (no feedback provided)"

// PromptLabService exposes Prompt Lab proposal approve/reject operations as
// Wails-bound methods. Proposals are plain tasks (tagged
// "prompt-lab-proposal" + "requires-human", status human-required) with no
// workflow attached, so approve/reject are direct task.Manager transitions
// rather than workflow-engine actions.
type PromptLabService struct {
	tasks     *task.Manager
	artifacts *artifact.Store
}

// ApproveProposal greenlights a pending prompt-lab proposal: moves it to
// todo (ready for variant authoring + offline eval) and drops the
// requires-human tag. Errors without mutating if the task is not a pending
// proposal (wrong status, or missing the prompt-lab-proposal tag).
func (s *PromptLabService) ApproveProposal(id string) (task.Task, error) {
	t, err := s.tasks.UpdateFn(id, func(cur task.Task) (task.Update, error) {
		if err := requirePendingProposal(cur); err != nil {
			return task.Update{}, err
		}
		status := task.StatusTodo
		reason := ""
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

	s.appendProgress(id, artifact.ProgressKindDecision, "Approved: greenlight variant authoring + offline eval")
	return t, nil
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
	if !slices.Contains(cur.Tags, "prompt-lab-proposal") || cur.Status != task.StatusHumanRequired {
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
