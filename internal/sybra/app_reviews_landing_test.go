package sybra

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/task"
)

func TestClassifyLandingOutcome(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		state string
		want  string
	}{
		{"merged", "MERGED", "merged"},
		{"closed unmerged", "CLOSED", "closed"},
		{"empty defaults to merged", "", "merged"},
		{"unknown defaults to merged", "OPEN", "merged"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyLandingOutcome(tt.state); got != tt.want {
				t.Errorf("classifyLandingOutcome(%q) = %q, want %q", tt.state, got, tt.want)
			}
		})
	}
}

// TestRecordLanding_EmitsTaskLanded verifies the landing helper records a
// structured task.landed event with the outcome, PR number, and queue-inclusive
// timing the evaluation scorecard reads. A freshly-created task has no agent
// runs, so work_to_land_h is absent; the helper makes no network calls.
func TestRecordLanding_EmitsTaskLanded(t *testing.T) {
	auditDir := t.TempDir()
	al, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatalf("audit.NewLogger: %v", err)
	}

	taskStore, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatalf("task NewStore: %v", err)
	}
	tasks := task.NewManager(taskStore, nil)
	created, err := tasks.Create("ship it", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler), audit: al},
		tasks:         tasks,
	}
	r.recordLanding(created.ID, 42, "MERGED")

	events, err := audit.Read(auditDir, audit.Query{
		Since: time.Now().Add(-time.Hour),
		Until: time.Now().Add(time.Hour),
		Type:  audit.EventTaskLanded,
	})
	if err != nil {
		t.Fatalf("audit.Read: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d task.landed events, want 1", len(events))
	}
	e := events[0]
	if e.TaskID != created.ID {
		t.Errorf("TaskID = %q, want %q", e.TaskID, created.ID)
	}
	if got := e.Data["outcome"]; got != "merged" {
		t.Errorf("outcome = %v, want merged", got)
	}
	// JSON round-trip decodes numbers as float64.
	if got, _ := e.Data["pr"].(float64); got != 42 {
		t.Errorf("pr = %v, want 42", e.Data["pr"])
	}
	if _, ok := e.Data["created_to_land_h"]; !ok {
		t.Errorf("missing created_to_land_h in %v", e.Data)
	}
	if _, ok := e.Data["work_to_land_h"]; ok {
		t.Errorf("unexpected work_to_land_h for task with no agent runs: %v", e.Data)
	}
}
