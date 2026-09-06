package selfmonitor

import (
	"testing"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/health"
	"github.com/Automaat/sybra/internal/task"
)

type forbiddenUpdater struct{ calls int }

func (s *forbiddenUpdater) Apply(task.TransitionIntent) (task.TransitionResult, error) {
	s.calls++
	return task.TransitionResult{}, nil
}

func TestActorNeverRequeuesAnUnrepairedCause(t *testing.T) {
	for _, cause := range []health.TransitionCause{"", health.CauseUnknown, health.CauseIncidentRecurrence,
		health.CauseInfrastructure, health.CausePlanning, health.CauseSpecification, health.CauseOperator} {
		for _, dry := range []bool{true, false} {
			updater := &forbiddenUpdater{}
			actor := &Actor{Tasks: updater, DryRun: dry}
			inv := InvestigatedFinding{Finding: health.Finding{Category: health.CatTriageMismatch, Cause: cause, TaskID: "fixture"},
				Verdict: Verdict{Classification: VerdictConfirmed}}
			if rec := actor.Act(t.Context(), inv); rec.Kind != "" || updater.calls != 0 {
				t.Fatalf("cause %q dry=%v spent another run without repairing its cause", cause, dry)
			}
		}
	}
}

func TestActorPipelineKeepsFindingsWithoutModeFlip(t *testing.T) {
	updater := &forbiddenUpdater{}
	findings := []health.Finding{
		{Category: health.CatTriageMismatch, Cause: health.CausePlanning, TaskID: "planning-fixture", Fingerprint: "planning-fixture"},
		{Category: health.CatInfrastructureEscalation, Cause: health.CauseInfrastructure, TaskID: "infra", Fingerprint: "infra"},
		{Category: health.CatEscalationUnknown, Cause: health.CauseUnknown, TaskID: "legacy", Fingerprint: "legacy"},
		{Category: health.CatIncidentRecovery, Cause: health.CauseIncidentRecurrence, TaskID: "incident", Fingerprint: "incident"},
	}
	path := writeFixture(t, fixtureLines())
	for i := range findings {
		findings[i].LogFile = path
	}
	service := NewService(Deps{
		Cfg:    config.SelfMonitorConfig{Enabled: true, AutoActCategories: []string{string(health.CatTriageMismatch), string(health.CatInfrastructureEscalation), string(health.CatEscalationUnknown), string(health.CatIncidentRecovery)}},
		Health: &stubHealth{Report: &health.Report{Findings: findings}},
		Judge:  &stubJudge{verdict: Verdict{Classification: VerdictConfirmed}},
		Actor:  &Actor{Tasks: updater},
	})
	report, err := service.Scan(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != len(findings) {
		t.Fatal("classification globally suppressed findings")
	}
	if len(report.ActionsTaken) != 0 || updater.calls != 0 {
		t.Fatal("confirmed finding bypassed deterministic remediation boundary")
	}
}
