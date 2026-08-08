package sybra

import (
	"errors"
	"strings"
	"time"

	"os"

	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/reject"
	"github.com/Automaat/sybra/internal/scrub"
	"github.com/Automaat/sybra/internal/task"
)

// The board operations below exist for sybra-cli, not the GUI.
//
// A CLI command that edits a task on another machine's board by opening a
// local file reports success and changes nothing, because the owning instance
// never reads that file. Every board command therefore has to be expressible
// as a server call; these are the ones the GUI never needed.

// boardRejection re-labels a refusal so the operator reads its reason.
//
// The HTTP handler sanitizes an unrecognized error to "internal error" on
// purpose: the store's failures wrap an *fs.PathError or format an absolute
// path into the message, and neither may reach a client. Only the errors the
// store marks as a refusal of the request, plus the two transition sentinels,
// are known to be path-free, so only those are relabelled. Anything else is
// returned untouched and stays a 500.
func boardRejection(err error) error {
	switch {
	case err == nil:
		return nil
	case reject.Is(err):
		return validationError(err.Error())
	case errors.Is(err, task.ErrTransitionConflict), errors.Is(err, task.ErrIllegalTransition):
		return conflictError(err.Error())
	}
	return err
}

// boardRejectionFor is boardRejection plus the missing-record case, which
// needs the identifier the caller already holds.
//
// The stores answer a miss with an error naming the absolute path they looked
// at, and the handler therefore flattens it to a bare "not found". That leaves
// an operator who mistyped an id with no way to tell which of their arguments
// was wrong, so the reason is rebuilt from the id instead of the path.
func boardRejectionFor(kind, id string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrNotExist),
		errors.Is(err, artifact.ErrNotFound),
		errors.Is(err, project.ErrProjectNotRegistered):
		return notFoundError(kind, id)
	}
	return boardRejection(err)
}

// CreateTaskFull creates a task with initial field values, optionally landing it directly in a status other than the default. An empty status uses the default.
func (s *TaskService) CreateTaskFull(title, body, mode, status string, init task.Update) (task.Task, error) {
	if status == "" {
		created, err := s.CreateTaskWithInit(title, body, mode, init)
		return created, boardRejection(err)
	}
	validated, err := task.ValidateStatus(status)
	if err != nil {
		return task.Task{}, validationError(err.Error())
	}
	created, err := s.tasks.CreateWithStatus(title, body, mode, validated, init)
	if err != nil {
		return task.Task{}, boardRejection(err)
	}
	s.startCreatedWorkflow(created)
	return created, nil
}

// UpdateTaskFields applies a typed field update. It is the struct-shaped
// counterpart to UpdateTask's map form, which sybra-cli cannot use for the
// fields it edits: task.Update has no JSON tags, so a map round-trip would
// have to re-derive every key name by hand.
func (s *TaskService) UpdateTaskFields(id string, u task.Update) (task.Task, error) {
	if s.tasks == nil {
		return task.Task{}, unavailableError("task store unavailable")
	}
	updated, err := s.tasks.Update(id, u)
	return updated, boardRejectionFor("task", id, err)
}

// ApplyTransition runs a status transition through the same gate the GUI and
// the workflow engine use, so a CLI status change is admitted, audited, and
// dispatched identically wherever it was typed.
func (s *TaskService) ApplyTransition(intent task.TransitionIntent) (task.TransitionResult, error) {
	if s.tasks == nil {
		return task.TransitionResult{}, unavailableError("task store unavailable")
	}
	result, err := s.tasks.Apply(intent)
	return result, boardRejection(err)
}

// TouchTask bumps a task's updated-at without changing any field. Used to surface out-of-band edits, such as an appended progress entry.
func (s *TaskService) TouchTask(id string) (task.Task, error) {
	if s.tasks == nil {
		return task.Task{}, unavailableError("task store unavailable")
	}
	touched, err := s.tasks.Touch(id)
	return touched, boardRejectionFor("task", id, err)
}

// AppendTaskProgress records one progress entry against a task and bumps its
// updated-at.
//
// sybra-cli cannot do this half locally: the artifact store lives beside the
// board it belongs to, so a client appending to its own disk while touching
// another machine's task writes the entry where the owning instance will never
// read it. Scrubbing happens here for the same reason — the work blocklist
// comes from the project record on this side of the wire.
func (s *TaskService) AppendTaskProgress(taskID, kind, role, message string) (artifact.ProgressEntry, error) {
	if s.tasks == nil || s.artifacts == nil {
		return artifact.ProgressEntry{}, unavailableError("progress log unavailable")
	}
	if !artifact.ValidProgressKind(kind) {
		return artifact.ProgressEntry{}, validationError("invalid progress kind " + kind)
	}
	if strings.TrimSpace(message) == "" {
		return artifact.ProgressEntry{}, validationError("progress message is required")
	}
	t, err := s.tasks.Get(taskID)
	if err != nil {
		return artifact.ProgressEntry{}, boardRejectionFor("task", taskID, err)
	}
	if t.ProjectID != "" && s.projects != nil {
		if p, pErr := s.projects.Get(t.ProjectID); pErr == nil {
			if blocklist := p.WorkBlocklist(); blocklist != nil {
				message, _ = scrub.Scrub(message, blocklist)
			}
		}
	}
	entry := artifact.ProgressEntry{Ts: time.Now().UTC(), Kind: kind, Role: role, Message: message}
	if err := s.artifacts.AppendProgress(taskID, entry); err != nil {
		return artifact.ProgressEntry{}, err
	}
	if _, err := s.tasks.Touch(taskID); err != nil && s.logger != nil {
		s.logger.Warn("task.progress.touch_failed", "task_id", taskID, "err", err)
	}
	return entry, nil
}

// ListTrash returns every retained generation of every soft-deleted task.
func (s *TaskService) ListTrash() ([]task.TrashEntry, error) {
	if s.tasks == nil {
		return nil, unavailableError("task store unavailable")
	}
	return s.tasks.ListTrash()
}

// RestoreFromTrash brings the newest retained generation of a deleted task back onto the board.
func (s *TaskService) RestoreFromTrash(id string) (task.Task, error) {
	if s.tasks == nil {
		return task.Task{}, unavailableError("task store unavailable")
	}
	restored, err := s.tasks.RestoreFromTrash(id)
	return restored, boardRejectionFor("trashed task", id, err)
}

// DeleteTrashedGeneration permanently removes one retained generation, reporting whether it existed.
func (s *TaskService) DeleteTrashedGeneration(id string) (bool, error) {
	if s.tasks == nil {
		return false, unavailableError("task store unavailable")
	}
	removed, err := s.tasks.DeleteTrashedGeneration(id)
	return removed, boardRejectionFor("trashed task", id, err)
}

// TrashPruneReportDTO is the wire form of task.TrashPruneReport. The domain
// type carries []error, which JSON renders as a list of empty objects, so the
// failures would reach the operator as `{}` without this.
type TrashPruneReportDTO struct {
	Scanned int               `json:"scanned"`
	Removed int               `json:"removed"`
	Entries []task.TrashEntry `json:"entries"`
	Errors  []string          `json:"errors,omitempty"`
}

// PruneAllTrash removes every retained generation past its retention window.
func (s *TaskService) PruneAllTrash() (TrashPruneReportDTO, error) {
	if s.tasks == nil {
		return TrashPruneReportDTO{}, unavailableError("task store unavailable")
	}
	report, err := s.tasks.PruneAllTrash()
	if err != nil {
		return TrashPruneReportDTO{}, err
	}
	dto := TrashPruneReportDTO{Scanned: report.Scanned, Removed: report.Removed, Entries: report.Entries}
	for _, e := range report.Errors {
		dto.Errors = append(dto.Errors, e.Error())
	}
	return dto, nil
}
