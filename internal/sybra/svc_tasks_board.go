package sybra

import (
	"fmt"

	"github.com/Automaat/sybra/internal/task"
)

// The board operations below exist for sybra-cli, not the GUI.
//
// A CLI command that edits a task on another machine's board by opening a
// local file reports success and changes nothing, because the owning instance
// never reads that file. Every board command therefore has to be expressible
// as a server call; these are the ones the GUI never needed.

// CreateTaskFull creates a task with initial field values, optionally landing it directly in a status other than the default. An empty status uses the default.
func (s *TaskService) CreateTaskFull(title, body, mode, status string, init task.Update) (task.Task, error) {
	if status == "" {
		return s.CreateTaskWithInit(title, body, mode, init)
	}
	validated, err := task.ValidateStatus(status)
	if err != nil {
		return task.Task{}, err
	}
	created, err := s.tasks.CreateWithStatus(title, body, mode, validated, init)
	if err != nil {
		return task.Task{}, err
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
		return task.Task{}, fmt.Errorf("task store unavailable")
	}
	return s.tasks.Update(id, u)
}

// ApplyTransition runs a status transition through the same gate the GUI and
// the workflow engine use, so a CLI status change is admitted, audited, and
// dispatched identically wherever it was typed.
func (s *TaskService) ApplyTransition(intent task.TransitionIntent) (task.TransitionResult, error) {
	if s.tasks == nil {
		return task.TransitionResult{}, fmt.Errorf("task store unavailable")
	}
	return s.tasks.Apply(intent)
}

// TouchTask bumps a task's updated-at without changing any field. Used to surface out-of-band edits, such as an appended progress entry.
func (s *TaskService) TouchTask(id string) (task.Task, error) {
	if s.tasks == nil {
		return task.Task{}, fmt.Errorf("task store unavailable")
	}
	return s.tasks.Touch(id)
}

// ListTrash returns every retained generation of every soft-deleted task.
func (s *TaskService) ListTrash() ([]task.TrashEntry, error) {
	if s.tasks == nil {
		return nil, fmt.Errorf("task store unavailable")
	}
	return s.tasks.ListTrash()
}

// RestoreFromTrash brings the newest retained generation of a deleted task back onto the board.
func (s *TaskService) RestoreFromTrash(id string) (task.Task, error) {
	if s.tasks == nil {
		return task.Task{}, fmt.Errorf("task store unavailable")
	}
	return s.tasks.RestoreFromTrash(id)
}

// DeleteTrashedGeneration permanently removes one retained generation, reporting whether it existed.
func (s *TaskService) DeleteTrashedGeneration(id string) (bool, error) {
	if s.tasks == nil {
		return false, fmt.Errorf("task store unavailable")
	}
	return s.tasks.DeleteTrashedGeneration(id)
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
		return TrashPruneReportDTO{}, fmt.Errorf("task store unavailable")
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
