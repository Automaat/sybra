package sybra

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Automaat/sybra/internal/monitor"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/triage"
	"github.com/Automaat/sybra/internal/umbrella"
)

// Whole-operation endpoints for sybra-cli. Umbrella expansion, triage classification, and a monitor scan each touch many tasks under locks the server owns, so exposing their pieces separately would let a client interleave with the server mid-operation.

// UmbrellaExpandDTO is the wire form of umbrella.Result.
type UmbrellaExpandDTO struct {
	UmbrellaURL string `json:"umbrellaUrl"`
	Created     int    `json:"created"`
	Skipped     int    `json:"skipped"`
	Degraded    bool   `json:"degraded"`
	ChildCount  int    `json:"childCount"`
	MaxParallel int    `json:"maxParallel"`
}

// ExpandUmbrella expands a ☂️ umbrella issue into a gated child DAG. An empty model uses the instance's configured planner.
func (s *TaskService) ExpandUmbrella(issueURL, model string) (UmbrellaExpandDTO, error) {
	if s.umbrellaExpand == nil {
		return UmbrellaExpandDTO{}, unavailableError("umbrella expansion is not enabled on this instance")
	}
	res, err := s.umbrellaExpand(issueURL, model)
	if err != nil {
		return UmbrellaExpandDTO{}, err
	}
	return umbrellaResultDTO(res), nil
}

func umbrellaResultDTO(res umbrella.Result) UmbrellaExpandDTO {
	return UmbrellaExpandDTO{
		UmbrellaURL: res.UmbrellaURL,
		Created:     res.Created,
		Skipped:     res.Skipped,
		Degraded:    res.Degraded,
		ChildCount:  res.ChildCount,
		MaxParallel: res.MaxParallel,
	}
}

// TriageResultDTO pairs a classifier verdict with the task the verdict was applied to.
type TriageResultDTO struct {
	Verdict triage.Verdict `json:"verdict"`
	Task    task.Task      `json:"task"`
}

// ClassifyTask runs the triage classifier over one task and applies its verdict atomically.
func (s *TaskService) ClassifyTask(id, model string) (TriageResultDTO, error) {
	if s.tasks == nil || s.projects == nil {
		return TriageResultDTO{}, unavailableError("task store unavailable")
	}
	t, err := s.tasks.Get(id)
	if err != nil {
		return TriageResultDTO{}, err
	}
	// The guard belongs here, not on the caller: a client that checked the
	// status against its own board would be checking one board and mutating
	// another. Reclassifying a task another subsystem owns rewrites gating
	// tags it must not touch (see internal/triage/apply.go).
	if t.Status != task.StatusNew {
		return TriageResultDTO{}, conflictError(fmt.Sprintf(
			"task %s has status %q, not %q — triage classify only reclassifies fresh tasks",
			id, t.Status, task.StatusNew))
	}
	projects, err := s.projects.List()
	if err != nil {
		return TriageResultDTO{}, fmt.Errorf("list projects: %w", err)
	}
	cfg := s.config()
	if model == "" && cfg != nil {
		model = cfg.Triage.Model
	}
	classifier := &triage.FallbackClassifier{Model: model, Logger: slog.New(slog.DiscardHandler)}
	verdict, updated, err := triage.ClassifyAndApply(s.recoveryCtx(), classifier, s.tasks, s.audit, t, projects)
	if err != nil {
		// The stamp has to land on the board that owns the task. A client
		// stamping its own copy would leave the owning instance's task with
		// no retryable marker, and the workflow engine parks it.
		reason := triage.RetryableStatusReason(err)
		if _, markErr := s.tasks.Update(id, task.Update{StatusReason: &reason}); markErr != nil {
			return TriageResultDTO{}, errors.Join(err, fmt.Errorf("mark retryable triage failure: %w", markErr))
		}
		return TriageResultDTO{}, err
	}
	return TriageResultDTO{Verdict: verdict, Task: updated}, nil
}

// ScanMonitor runs one anomaly-detector pass and returns its report.
func (s *TaskService) ScanMonitor() (monitor.Report, error) {
	if s.monitorScan == nil {
		return monitor.Report{}, unavailableError("monitor is not running on this instance")
	}
	return s.monitorScan(context.Background())
}
