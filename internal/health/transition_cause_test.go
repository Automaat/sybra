package health

import (
	"slices"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
)

func transitionFixture(from, to, actor, code, owner, provenance string) audit.Event {
	return audit.Event{Type: audit.EventTaskStatusChanged, TaskID: "task-synthetic", Data: map[string]any{
		"from": from, "to": to, "actor": actor, "escalation_code": code,
		"failure_owner": owner, "evidence_provenance": provenance,
	}}
}

func TestClassifyTransitionRequiresStructuredEvidence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event audit.Event
		want  TransitionCause
	}{
		{"incident", transitionFixture("done", "todo", "monitor.incident.reopen", "", "", ""), CauseIncidentRecurrence},
		{"cancelled incident", transitionFixture("cancelled", "todo", "monitor.incident.reopen", "", "", ""), CauseIncidentRecurrence},
		{"ordinary reopen", transitionFixture("done", "todo", "cli.reopen", "", "", ""), CauseUnknown},
		{"nonterminal incident actor", transitionFixture("in-review", "todo", "monitor.incident.reopen", "", "", ""), CauseUnknown},
		{"machine", transitionFixture("testing", "blocked", "workflow", "runenv.unavailable", "machine", "control_plane"), CauseInfrastructure},
		{"external transient", transitionFixture("testing", "blocked", "workflow", "github.unavailable", "external_transient", "github"), CauseInfrastructure},
		{"planning", transitionFixture("planning", "human-required", "workflow", "planning.retry_exhausted", "specification", "control_plane"), CausePlanning},
		{"specification", transitionFixture("testing", "human-required", "workflow", "requirements.missing", "specification", "operator"), CauseSpecification},
		{"operator", transitionFixture("testing", "human-required", "workflow", "approval.required", "operator_authority", "control_plane"), CauseOperator},
		{"legacy", transitionFixture("testing", "human-required", "workflow", "planning.retry_exhausted", "specification", "legacy"), CauseUnknown},
		{"provider assertion", transitionFixture("testing", "human-required", "workflow", "planning.retry_exhausted", "specification", "provider"), CauseUnknown},
		{"missing code", transitionFixture("testing", "human-required", "workflow", "", "specification", "control_plane"), CauseUnknown},
		{"missing metadata", transitionFixture("testing", "human-required", "", "", "", ""), CauseUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyTransition(tc.event); got != tc.want {
				t.Fatalf("cause = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEscalationFindingsDoNotInferCauseFromHeadless(t *testing.T) {
	events := []audit.Event{
		{Type: audit.EventTriageCompleted, TaskID: "task-synthetic", Data: map[string]any{"agent_mode": "headless"}},
		transitionFixture("planning", "human-required", "workflow", "planning.retry_exhausted", "specification", "control_plane"),
		transitionFixture("testing", "blocked", "workflow", "runenv.unavailable", "machine", "control_plane"),
		transitionFixture("testing", "human-required", "", "", "", ""),
	}
	got := checkTriageMismatch(events, time.Now())
	if len(got) != 3 {
		t.Fatalf("findings = %+v", got)
	}
	for _, want := range []struct {
		category Category
		cause    TransitionCause
	}{
		{CatTriageMismatch, CausePlanning}, {CatInfrastructureEscalation, CauseInfrastructure}, {CatEscalationUnknown, CauseUnknown},
	} {
		if !slices.ContainsFunc(got, func(f Finding) bool {
			return f.Category == want.category && f.Cause == want.cause && f.NextAction == nextAction(want.cause)
		}) {
			t.Errorf("missing %+v in %+v", want, got)
		}
	}
}

func TestStatusBounceSeparatesIncidentEpisodes(t *testing.T) {
	now := time.Now()
	var events []audit.Event
	for range 4 {
		events = append(events, transitionFixture("todo", "done", "workflow", "", "", ""), transitionFixture("done", "todo", "monitor.incident.reopen", "", "", ""))
	}
	for i := range events {
		events[i].Timestamp = now.Add(time.Duration(i) * time.Minute)
	}
	slices.Reverse(events) // audit stores need not return chronological order.
	got := checkStatusBounce(events, now)
	if len(got) != 1 || got[0].Category != CatIncidentRecovery || got[0].Severity != SeverityInfo || RollupScore(got) != ScoreGood {
		t.Fatalf("incident lifecycle treated as broken planning/bounce: %+v", got)
	}
	for i := range events {
		if events[i].Data["actor"] == "monitor.incident.reopen" {
			events[i].Data["actor"] = ""
		}
	}
	got = checkStatusBounce(events, now)
	if len(got) != 2 || RollupScore(got) != ScoreWarning {
		t.Fatalf("historical unknown repetition hidden: %+v", got)
	}
	for _, finding := range got {
		if finding.Category != CatStatusBounce || finding.Cause != CauseUnknown {
			t.Fatalf("invented cause: %+v", finding)
		}
	}
}

func TestIncidentDoesNotHideBounceWithinOneEpisode(t *testing.T) {
	now := time.Now()
	events := []audit.Event{
		transitionFixture("done", "todo", "monitor.incident.reopen", "", "", ""),
		transitionFixture("in-review", "in-progress", "fixer", "", "", ""),
		transitionFixture("in-progress", "in-review", "reviewer", "", "", ""),
		transitionFixture("in-review", "in-progress", "fixer", "", "", ""),
	}
	for i := range events {
		events[i].Timestamp = now.Add(time.Duration(i) * time.Minute)
	}
	got := checkStatusBounce(events, now)
	if len(got) != 2 || RollupScore(got) != ScoreWarning {
		t.Fatalf("real bounce lost: %+v", got)
	}
}
