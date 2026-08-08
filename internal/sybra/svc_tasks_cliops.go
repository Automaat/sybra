package sybra

import (
	"context"
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

// ExpandUmbrella expands a ☂️ umbrella issue into a gated child DAG.
func (s *TaskService) ExpandUmbrella(issueURL string) (UmbrellaExpandDTO, error) {
	if s.umbrellaExpand == nil {
		return UmbrellaExpandDTO{}, fmt.Errorf("umbrella expansion is not enabled on this instance")
	}
	res, err := s.umbrellaExpand(issueURL)
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
		return TriageResultDTO{}, fmt.Errorf("task store unavailable")
	}
	t, err := s.tasks.Get(id)
	if err != nil {
		return TriageResultDTO{}, err
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
		return TriageResultDTO{}, err
	}
	return TriageResultDTO{Verdict: verdict, Task: updated}, nil
}

// ScanMonitor runs one anomaly-detector pass and returns its report.
func (s *TaskService) ScanMonitor() (monitor.Report, error) {
	if s.monitorScan == nil {
		return monitor.Report{}, fmt.Errorf("monitor is not running on this instance")
	}
	return s.monitorScan(context.Background())
}
