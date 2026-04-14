package selfmonitor

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/health"
	"github.com/Automaat/synapse/internal/task"
)

// configWithAutoAct returns a SelfMonitorConfig that auto-acts on the given categories.
func configWithAutoAct(categories ...string) config.SelfMonitorConfig {
	return config.SelfMonitorConfig{
		Enabled:           true,
		IntervalHours:     6,
		AutoActCategories: categories,
	}
}

// stubTaskUpdater satisfies taskUpdater for actor tests.
type stubTaskUpdater struct {
	updated map[string]task.Update
	err     error
}

func newStubUpdater() *stubTaskUpdater {
	return &stubTaskUpdater{updated: map[string]task.Update{}}
}

func (s *stubTaskUpdater) Update(id string, u task.Update) (task.Task, error) {
	if s.err != nil {
		return task.Task{}, s.err
	}
	s.updated[id] = u
	return task.Task{ID: id}, nil
}

func confirmedTriageMismatch(taskID string) InvestigatedFinding {
	return InvestigatedFinding{
		Fingerprint: "triage_mismatch:" + taskID,
		Finding: health.Finding{
			Category: health.CatTriageMismatch,
			TaskID:   taskID,
		},
		Verdict: Verdict{Classification: VerdictConfirmed},
	}
}

func TestActor_FlipsAgentMode_ConfirmedTriageMismatch(t *testing.T) {
	updater := newStubUpdater()
	actor := &Actor{Tasks: updater, DryRun: false, Logger: slog.Default()}

	inv := confirmedTriageMismatch("task-flip")
	rec := actor.Act(context.Background(), inv)

	if rec.Kind != "flip_agent_mode" {
		t.Errorf("Kind = %q, want flip_agent_mode", rec.Kind)
	}
	if rec.DryRun {
		t.Error("DryRun = true, want false")
	}
	if rec.Error != "" {
		t.Errorf("Error = %q, want empty", rec.Error)
	}
	if rec.Reference != "task-flip" {
		t.Errorf("Reference = %q, want task-flip", rec.Reference)
	}

	u, ok := updater.updated["task-flip"]
	if !ok {
		t.Fatal("task-flip not updated")
	}
	if u.AgentMode == nil || *u.AgentMode != task.AgentModeInteractive {
		t.Errorf("AgentMode = %v, want interactive", u.AgentMode)
	}
}

func TestActor_DryRun_DoesNotUpdate(t *testing.T) {
	updater := newStubUpdater()
	actor := &Actor{Tasks: updater, DryRun: true, Logger: slog.Default()}

	inv := confirmedTriageMismatch("task-dry")
	rec := actor.Act(context.Background(), inv)

	if rec.Kind != "flip_agent_mode" {
		t.Errorf("Kind = %q, want flip_agent_mode", rec.Kind)
	}
	if !rec.DryRun {
		t.Error("DryRun = false, want true")
	}
	if len(updater.updated) != 0 {
		t.Errorf("task updated in dry run: %v", updater.updated)
	}
}

func TestActor_SkipsNonConfirmedVerdicts(t *testing.T) {
	tests := []struct {
		classification string
	}{
		{VerdictPending},
		{VerdictFalsePositive},
		{"needs_human"},
	}
	for _, tt := range tests {
		t.Run(tt.classification, func(t *testing.T) {
			updater := newStubUpdater()
			actor := &Actor{Tasks: updater, DryRun: false, Logger: slog.Default()}

			inv := InvestigatedFinding{
				Fingerprint: "triage_mismatch:t1",
				Finding: health.Finding{
					Category: health.CatTriageMismatch,
					TaskID:   "t1",
				},
				Verdict: Verdict{Classification: tt.classification},
			}
			rec := actor.Act(context.Background(), inv)

			if rec.Kind != "" {
				t.Errorf("Kind = %q, want empty for non-confirmed verdict %q", rec.Kind, tt.classification)
			}
			if len(updater.updated) != 0 {
				t.Errorf("task updated for non-confirmed verdict: %v", updater.updated)
			}
		})
	}
}

func TestActor_SkipsUnhandledCategories(t *testing.T) {
	categories := []health.Category{
		health.CatCostOutlier,
		health.CatStuckTask,
		health.CatAgentRetryLoop,
		health.CatFailureRate,
	}
	for _, cat := range categories {
		t.Run(string(cat), func(t *testing.T) {
			updater := newStubUpdater()
			actor := &Actor{Tasks: updater, DryRun: false, Logger: slog.Default()}

			inv := InvestigatedFinding{
				Fingerprint: string(cat) + ":t1",
				Finding: health.Finding{
					Category: cat,
					TaskID:   "t1",
				},
				Verdict: Verdict{Classification: VerdictConfirmed},
			}
			rec := actor.Act(context.Background(), inv)

			if rec.Kind != "" {
				t.Errorf("Kind = %q, want empty for category %q", rec.Kind, cat)
			}
		})
	}
}

func TestActor_RecordsErrorWhenUpdateFails(t *testing.T) {
	updater := &stubTaskUpdater{
		updated: map[string]task.Update{},
		err:     os.ErrPermission,
	}
	actor := &Actor{Tasks: updater, DryRun: false, Logger: slog.Default()}

	inv := confirmedTriageMismatch("task-fail")
	rec := actor.Act(context.Background(), inv)

	if rec.Kind != "flip_agent_mode" {
		t.Errorf("Kind = %q, want flip_agent_mode even on failure", rec.Kind)
	}
	if rec.Error == "" {
		t.Error("Error = empty, want non-empty on update failure")
	}
}

func TestActor_FlipsAgentModeViaServicePipeline(t *testing.T) {
	// Integration: service wires Actor into tick loop for AutoActCategories.
	updater := newStubUpdater()

	logPath := writeFixture(t, fixtureLines())

	rep := &health.Report{
		Findings: []health.Finding{{
			Category:    health.CatTriageMismatch,
			Fingerprint: "triage_mismatch:task-wired",
			TaskID:      "task-wired",
			LogFile:     logPath,
		}},
	}

	svc := NewService(Deps{
		Health: &stubHealth{Report: rep},
		Judge:  &stubJudge{verdict: Verdict{Classification: VerdictConfirmed}},
		Actor:  &Actor{Tasks: updater, DryRun: false, Logger: slog.Default()},
		Cfg:    configWithAutoAct(string(health.CatTriageMismatch)),
	})

	r, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if len(r.ActionsTaken) != 1 {
		t.Fatalf("ActionsTaken = %d, want 1", len(r.ActionsTaken))
	}
	if r.ActionsTaken[0].Kind != "flip_agent_mode" {
		t.Errorf("Kind = %q, want flip_agent_mode", r.ActionsTaken[0].Kind)
	}
	if _, ok := updater.updated["task-wired"]; !ok {
		t.Error("task-wired not updated via pipeline")
	}
}

func TestActor_RespectsMaxAutoActionsPerDay(t *testing.T) {
	updater := newStubUpdater()

	ledgerPath := filepath.Join(t.TempDir(), "ledger.jsonl")
	ledger, err := Open(ledgerPath)
	if err != nil {
		t.Fatalf("Open ledger: %v", err)
	}
	if err := ledger.Append(LedgerEntry{
		Fingerprint: "fp-old",
		Verdict:     VerdictConfirmed,
		Action:      "flip_agent_mode",
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	logPath := writeFixture(t, fixtureLines())
	rep := &health.Report{
		Findings: []health.Finding{{
			Category:    health.CatTriageMismatch,
			Fingerprint: "triage_mismatch:task-capped",
			TaskID:      "task-capped",
			LogFile:     logPath,
		}},
	}

	cfg := configWithAutoAct(string(health.CatTriageMismatch))
	cfg.MaxAutoActionsPerDay = 1

	svc := NewService(Deps{
		Health: &stubHealth{Report: rep},
		Ledger: ledger,
		Judge:  &stubJudge{verdict: Verdict{Classification: VerdictConfirmed}},
		Actor:  &Actor{Tasks: updater, DryRun: false, Logger: slog.Default()},
		Cfg:    cfg,
	})

	r, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.ActionsTaken) != 0 {
		t.Fatalf("ActionsTaken = %d, want 0 when budget exhausted", len(r.ActionsTaken))
	}
	if _, ok := updater.updated["task-capped"]; ok {
		t.Error("task-capped updated despite exhausted budget")
	}
}

func TestActor_RecordsActionToLedger(t *testing.T) {
	updater := newStubUpdater()

	ledgerPath := filepath.Join(t.TempDir(), "ledger.jsonl")
	ledger, err := Open(ledgerPath)
	if err != nil {
		t.Fatalf("Open ledger: %v", err)
	}

	logPath := writeFixture(t, fixtureLines())
	rep := &health.Report{
		Findings: []health.Finding{{
			Category:    health.CatTriageMismatch,
			Fingerprint: "triage_mismatch:task-ledger",
			TaskID:      "task-ledger",
			LogFile:     logPath,
		}},
	}

	cfg := configWithAutoAct(string(health.CatTriageMismatch))
	cfg.MaxAutoActionsPerDay = 3

	svc := NewService(Deps{
		Health: &stubHealth{Report: rep},
		Ledger: ledger,
		Judge:  &stubJudge{verdict: Verdict{Classification: VerdictConfirmed}},
		Actor:  &Actor{Tasks: updater, DryRun: false, Logger: slog.Default()},
		Cfg:    cfg,
	})

	r, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.ActionsTaken) != 1 {
		t.Fatalf("ActionsTaken = %d, want 1", len(r.ActionsTaken))
	}
	if got := ledger.ActionsInWindow(24 * time.Hour); got != 1 {
		t.Fatalf("ActionsInWindow = %d, want 1", got)
	}
}
