package evaluation

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/taskstatus"
)

func TestComputeAutonomySLOsTypedAndUnknownEvidence(t *testing.T) {
	base := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	events := []audit.Event{
		{Timestamp: base.Add(time.Minute), Type: audit.EventTaskStatusChanged, Data: map[string]any{"to": string(taskstatus.HumanRequired), "failure_owner": "operator_decision"}},
		{Timestamp: base.Add(2 * time.Minute), Type: audit.EventTaskStatusChanged, Data: map[string]any{"to": string(taskstatus.HumanRequired), "failure_owner": "machine"}},
		{Timestamp: base.Add(3 * time.Minute), Type: audit.EventTaskStatusChanged, Data: map[string]any{"to": string(taskstatus.HumanRequired)}},
		{Timestamp: base.Add(3500 * time.Millisecond), Type: audit.EventTaskStatusChanged, Data: map[string]any{"to": string(taskstatus.HumanRequired), "failure_owner": "external_transient"}},
		{Timestamp: base.Add(4 * time.Minute), Type: audit.EventMonitorIncidentObserved, Data: map[string]any{"fingerprint": "i1", "affected_task_count": 4}},
		{Timestamp: base.Add(4500 * time.Millisecond), Type: audit.EventMonitorIncidentObserved, Data: map[string]any{"fingerprint": "fleet", "affected_task_count": 0}},
		{Timestamp: base.Add(5 * time.Minute), Type: audit.EventMonitorIncidentRemediation, Data: map[string]any{"fingerprint": "i1", "remediation_id": "r1"}},
		{Timestamp: base.Add(6 * time.Minute), Type: audit.EventMonitorIncidentRemediation, Data: map[string]any{"fingerprint": "i1", "remediation_id": "r2"}},
		{Timestamp: base.Add(7 * time.Minute), Type: audit.EventMonitorIncidentResolved, Data: map[string]any{"fingerprint": "i1", "containment_s": 60.0, "recovery_s": 180.0}},
		{Timestamp: base.Add(8 * time.Minute), Type: audit.EventAdmissionDecided, Data: map[string]any{"outcome": string(taskstatus.Blocked), "preflight_detectable": true, "usage_known": true, "cost_usd": .5, "tokens": 200}},
		{Timestamp: base.Add(9 * time.Minute), Type: audit.EventAdmissionDecided, Data: map[string]any{"outcome": string(taskstatus.Blocked)}},
	}
	got := ComputeAutonomySLOs(Scorecard{AutonomousLandings: 3, HumanTouchedLandings: 1, AutonomyUnknownLandings: 2}, events, base, base.Add(time.Hour))
	if got.AutonomousCompletion.State != EvidenceKnown || got.AutonomousCompletion.Rate != .75 || got.AutonomousCompletion.Unknown != 2 {
		t.Fatalf("autonomous completion = %+v", got.AutonomousCompletion)
	}
	if got.ValidHumanEscalation.Success != 1 || got.ValidHumanEscalation.Known != 3 || got.ValidHumanEscalation.Unknown != 1 || got.MachineHumanRequired != 1 {
		t.Fatalf("human escalation metrics = %+v violations=%d", got.ValidHumanEscalation, got.MachineHumanRequired)
	}
	if got.RecoverySuccess.Rate != 1 || got.RepeatRepair.Rate != 1 || got.IncidentFanout.Max != 4 || got.IncidentFanout.Count != 2 {
		t.Fatalf("incident metrics = recovery:%+v repeat:%+v fanout:%+v", got.RecoverySuccess, got.RepeatRepair, got.IncidentFanout)
	}
	if got.TimeToContainment.MeanSec != 60 || got.TimeToRecovery.MeanSec != 180 {
		t.Fatalf("duration metrics = containment:%+v recovery:%+v", got.TimeToContainment, got.TimeToRecovery)
	}
	if got.PreflightDetectableWaste.CostUSD != .5 || got.PreflightDetectableWaste.Tokens != 200 || got.PreflightDetectableWaste.UnknownLegacy != 1 {
		t.Fatalf("preflight waste = %+v", got.PreflightDetectableWaste)
	}
}

func TestComputeAutonomySLOsCountsMissingDurationAndOverflowAsUnknown(t *testing.T) {
	base := time.Now().UTC()
	events := []audit.Event{
		{Timestamp: base, Type: audit.EventMonitorIncidentObserved, Data: map[string]any{"fingerprint": "i", "affected_task_count": 256, "affected_task_count_known": false}},
		{Timestamp: base, Type: audit.EventMonitorIncidentResolved, Data: map[string]any{"fingerprint": "i"}},
	}
	got := ComputeAutonomySLOs(Scorecard{}, events, base.Add(-time.Second), base.Add(time.Second))
	if got.IncidentFanout.Unknown != 1 || got.TimeToContainment.Unknown != 1 || got.TimeToRecovery.Unknown != 1 {
		t.Fatalf("unknown evidence omitted: %+v", got)
	}
}

func TestComputeAutonomySLOsExcludesHeldAndFailedRemediationFromRecoverySuccess(t *testing.T) {
	base := time.Now().UTC()
	events := []audit.Event{
		{Timestamp: base, Type: audit.EventMonitorIncidentRemediation, Data: map[string]any{"fingerprint": "i1", "remediation_result": "held"}},
		{Timestamp: base, Type: audit.EventMonitorIncidentRemediation, Data: map[string]any{"fingerprint": "i1", "remediation_result": "failed"}},
		{Timestamp: base, Type: audit.EventMonitorIncidentResolved, Data: map[string]any{"fingerprint": "i1"}},
		{Timestamp: base, Type: audit.EventMonitorIncidentRemediation, Data: map[string]any{"fingerprint": "i2", "remediation_result": "started"}},
		{Timestamp: base, Type: audit.EventMonitorIncidentResolved, Data: map[string]any{"fingerprint": "i2"}},
	}
	got := ComputeAutonomySLOs(Scorecard{}, events, base.Add(-time.Minute), base.Add(time.Minute))
	if got.RecoverySuccess.Success != 1 || got.RecoverySuccess.Known != 1 {
		t.Fatalf("recovery success should only credit the genuine repair attempt: %+v", got.RecoverySuccess)
	}
}

func TestComputeAutonomySLOsLegacyOnlyIsUnknown(t *testing.T) {
	base := time.Now().UTC()
	got := ComputeAutonomySLOs(Scorecard{AutonomyUnknownLandings: 3}, []audit.Event{{Timestamp: base, Type: audit.EventTaskStatusChanged, Data: map[string]any{"to": string(taskstatus.HumanRequired)}}}, base.Add(-time.Minute), base.Add(time.Minute))
	if got.AutonomousCompletion.State != EvidenceUnknown || got.ValidHumanEscalation.State != EvidenceUnknown || got.ValidHumanEscalation.Unknown != 1 || got.RecoverySuccess.State != EvidenceUnknown {
		t.Fatalf("legacy evidence was guessed: %+v", got)
	}
}
