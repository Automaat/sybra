package evaluation

import (
	"math"
	"slices"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/autonomy"
	"github.com/Automaat/sybra/internal/taskstatus"
)

type EvidenceState string

const (
	EvidenceKnown   EvidenceState = "known"
	EvidenceUnknown EvidenceState = "unknown"
)

type RateEvidence struct {
	State   EvidenceState `json:"state"`
	Rate    float64       `json:"rate"`
	Success int           `json:"success"`
	Known   int           `json:"known"`
	Unknown int           `json:"unknown"`
}

type DurationEvidence struct {
	State   EvidenceState `json:"state"`
	Samples int           `json:"samples"`
	MeanSec float64       `json:"meanSec"`
	P90Sec  float64       `json:"p90Sec"`
	Unknown int           `json:"unknown"`
}

type IncidentFanout struct {
	State   EvidenceState `json:"state"`
	Count   int           `json:"count"`
	Unknown int           `json:"unknown"`
	Mean    float64       `json:"mean"`
	P90     float64       `json:"p90"`
	Max     int           `json:"max"`
}

type PreflightWaste struct {
	State         EvidenceState `json:"state"`
	Failures      int           `json:"failures"`
	CostUSD       float64       `json:"costUsd"`
	Tokens        int           `json:"tokens"`
	UnknownUsage  int           `json:"unknownUsage"`
	UnknownLegacy int           `json:"unknownLegacy"`
}

// AutonomySLOs exposes evidence-aware autonomy outcomes. Unknown legacy or
// unprovable records are counted explicitly and never guessed into a rate.
type AutonomySLOs struct {
	AutonomousCompletion     RateEvidence     `json:"autonomousCompletion"`
	ValidHumanEscalation     RateEvidence     `json:"validHumanEscalation"`
	MachineHumanRequired     int              `json:"machineHumanRequiredInvariantViolations"`
	RecoverySuccess          RateEvidence     `json:"recoverySuccess"`
	RepeatRepair             RateEvidence     `json:"repeatRepair"`
	IncidentFanout           IncidentFanout   `json:"incidentFanout"`
	TimeToContainment        DurationEvidence `json:"timeToContainment"`
	TimeToRecovery           DurationEvidence `json:"timeToRecovery"`
	PreflightDetectableWaste PreflightWaste   `json:"preflightDetectableWaste"`
}

func ComputeAutonomySLOs(sc Scorecard, events []audit.Event, since, until time.Time) AutonomySLOs {
	out := AutonomySLOs{}
	out.AutonomousCompletion = rateEvidence(sc.AutonomousLandings, sc.AutonomousLandings+sc.HumanTouchedLandings, sc.AutonomyUnknownLandings)

	incidents := map[string]int{}
	unknownFanout := map[string]bool{}
	var containment, recovery []float64
	repairs := map[string]int{}
	resolvedWithRepair := map[string]bool{}
	for i := range events {
		e := events[i]
		if e.Timestamp.Before(since) || e.Timestamp.After(until) {
			continue
		}
		switch e.Type {
		case audit.EventTaskStatusChanged:
			if strVal(e.Data, "to") != string(taskstatus.HumanRequired) {
				continue
			}
			owner := autonomy.FailureOwner(strVal(e.Data, "failure_owner"))
			switch {
			case owner.AllowsHumanRequired():
				out.ValidHumanEscalation.Success++
				out.ValidHumanEscalation.Known++
			case owner == autonomy.FailureOwnerMachine:
				out.ValidHumanEscalation.Known++
				out.MachineHumanRequired++
			case owner == autonomy.FailureOwnerExternalTransient:
				out.ValidHumanEscalation.Known++
			default:
				out.ValidHumanEscalation.Unknown++
			}
		case audit.EventMonitorIncidentObserved:
			recordIncidentFanout(e.Data, incidents, unknownFanout)
		case audit.EventMonitorIncidentRemediation:
			result := strVal(e.Data, "remediation_result")
			if fp := strVal(e.Data, "fingerprint"); fp != "" && result != "held" && result != "failed" {
				repairs[fp]++
			}
		case audit.EventMonitorIncidentResolved:
			fp := strVal(e.Data, "fingerprint")
			if repairs[fp] > 0 {
				resolvedWithRepair[fp] = true
			}
			if v, ok := floatVal(e.Data, "containment_s"); ok {
				containment = append(containment, v)
			} else {
				out.TimeToContainment.Unknown++
			}
			if v, ok := floatVal(e.Data, "recovery_s"); ok {
				recovery = append(recovery, v)
			} else {
				out.TimeToRecovery.Unknown++
			}
		case audit.EventAdmissionDecided:
			if !boolVal(e.Data, "preflight_detectable") {
				if strVal(e.Data, "outcome") == string(taskstatus.Blocked) {
					out.PreflightDetectableWaste.UnknownLegacy++
				}
				continue
			}
			out.PreflightDetectableWaste.Failures++
			if !boolVal(e.Data, "usage_known") {
				out.PreflightDetectableWaste.UnknownUsage++
				continue
			}
			if v, ok := floatVal(e.Data, "cost_usd"); ok {
				out.PreflightDetectableWaste.CostUSD += v
			}
			out.PreflightDetectableWaste.Tokens += intVal(e.Data, "tokens")
		}
	}
	out.ValidHumanEscalation = finishRate(out.ValidHumanEscalation)
	matured, successful, repeated := 0, 0, 0
	for fp, attempts := range repairs {
		if resolvedWithRepair[fp] {
			matured++
			successful++
			if attempts > 1 {
				repeated++
			}
		}
	}
	out.RecoverySuccess = rateEvidence(successful, matured, len(repairs)-matured)
	out.RepeatRepair = rateEvidence(repeated, matured, len(repairs)-matured)
	out.IncidentFanout = fanoutEvidence(incidents)
	out.IncidentFanout.Unknown = len(unknownFanout)
	out.TimeToContainment, out.TimeToRecovery = incidentDurations(containment, recovery, out.TimeToContainment.Unknown, out.TimeToRecovery.Unknown)
	finishPreflightWaste(&out.PreflightDetectableWaste)
	return out
}

func incidentDurations(containment, recovery []float64, containmentUnknown, recoveryUnknown int) (contained, recovered DurationEvidence) {
	contained = durationEvidence(containment)
	contained.Unknown = containmentUnknown
	recovered = durationEvidence(recovery)
	recovered.Unknown = recoveryUnknown
	return contained, recovered
}

func finishPreflightWaste(waste *PreflightWaste) {
	if waste.Failures > 0 && waste.UnknownUsage < waste.Failures {
		waste.State = EvidenceKnown
	} else {
		waste.State = EvidenceUnknown
	}
}

func recordIncidentFanout(data map[string]any, incidents map[string]int, unknown map[string]bool) {
	fp := strVal(data, "fingerprint")
	if fp == "" {
		return
	}
	if known, present := data["affected_task_count_known"].(bool); present && !known {
		delete(incidents, fp)
		unknown[fp] = true
		return
	}
	if unknown[fp] {
		return
	}
	if n := intVal(data, "affected_task_count"); n >= incidents[fp] {
		incidents[fp] = n
	}
}

func rateEvidence(success, known, unknown int) RateEvidence {
	return finishRate(RateEvidence{Success: success, Known: known, Unknown: unknown})
}

func finishRate(r RateEvidence) RateEvidence {
	if r.Known == 0 {
		r.State = EvidenceUnknown
		return r
	}
	r.State = EvidenceKnown
	r.Rate = float64(r.Success) / float64(r.Known)
	return r
}

func fanoutEvidence(values map[string]int) IncidentFanout {
	if len(values) == 0 {
		return IncidentFanout{State: EvidenceUnknown}
	}
	all := make([]int, 0, len(values))
	total := 0
	for _, value := range values {
		all = append(all, value)
		total += value
	}
	slices.Sort(all)
	return IncidentFanout{State: EvidenceKnown, Count: len(all), Mean: float64(total) / float64(len(all)), P90: percentileInts(all, .9), Max: all[len(all)-1]}
}

func durationEvidence(values []float64) DurationEvidence {
	if len(values) == 0 {
		return DurationEvidence{State: EvidenceUnknown}
	}
	slices.Sort(values)
	total := 0.0
	for _, value := range values {
		total += value
	}
	return DurationEvidence{State: EvidenceKnown, Samples: len(values), MeanSec: total / float64(len(values)), P90Sec: percentileFloats(values, .9)}
}

func percentileInts(values []int, p float64) float64 {
	idx := int(math.Ceil(float64(len(values))*p)) - 1
	return float64(values[max(0, idx)])
}

func percentileFloats(values []float64, p float64) float64 {
	idx := int(math.Ceil(float64(len(values))*p)) - 1
	return values[max(0, idx)]
}

func intVal(data map[string]any, key string) int {
	switch value := data[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}
