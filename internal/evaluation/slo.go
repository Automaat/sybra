package evaluation

import (
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/runoutcome"
)

// SLOTargets is the rolling autonomy/reliability target set (#2441). Defined
// in internal/config (config cannot import evaluation, since evaluation
// already imports config for EvaluationConfig) and aliased here so
// evaluation code reads naturally as evaluation.SLOTargets.
type SLOTargets = config.SLOTargets

// DefaultSLOTargets re-exports config.DefaultSLOTargets for callers (tests,
// the CI gate script) that only import evaluation.
func DefaultSLOTargets() SLOTargets { return config.DefaultSLOTargets() }

// SLOSignals are the audit-derived signals EvaluateSLOs needs beyond what a
// Scorecard already carries — Compute has no notion of "identical retry" or
// "restart", so these are scanned separately over the same event window.
type SLOSignals struct {
	// IdenticalRetryMax is the largest number of failed agent runs recorded
	// for any single task in the window (see scanIdenticalRetryCap).
	IdenticalRetryMax int `json:"identicalRetryMax"`
	// RestartsPerHour is the rate of automatic human-required recovery
	// restarts in the window (see scanRestartCadence).
	RestartsPerHour float64 `json:"restartsPerHour"`
}

// ComputeSLOSignals scans events bounded by [since, until] for the two
// audit-derived SLO signals. Pure: no I/O, deterministic for a given input.
func ComputeSLOSignals(events []audit.Event, since, until time.Time) SLOSignals {
	return SLOSignals{
		IdenticalRetryMax: scanIdenticalRetryCap(events, since, until),
		RestartsPerHour:   scanRestartCadence(events, since, until),
	}
}

// SLOStatus is one target's compliance verdict.
type SLOStatus struct {
	Name   string  `json:"name"`
	Actual float64 `json:"actual"`
	Target float64 `json:"target"`
	Met    bool    `json:"met"`
	// BudgetRemaining is 0 (breached) .. 1 (at least a full safety margin of
	// headroom above/below target), normalized so five heterogeneous SLOs
	// (rates in [0,1], integer counts, an hourly cadence) can be compared on
	// one scale and the minimum taken as the fleet-wide error budget.
	BudgetRemaining float64 `json:"budgetRemaining"`
}

// SLOReport is the compliance verdict for one Scorecard + SLOSignals pair
// against a target set.
type SLOReport struct {
	Targets SLOTargets `json:"targets"`
	// Statuses is fixed order: autonomy, ci_first_pass, rework,
	// identical_retry_cap, restart_cadence.
	Statuses  []SLOStatus `json:"statuses"`
	Compliant bool        `json:"compliant"`
	// ErrorBudgetRemaining is min(Statuses[i].BudgetRemaining), floored at 0.
	// 0 means at least one SLO is breached; the throttle in
	// internal/sybra/agentorch keys off this reaching 0.
	ErrorBudgetRemaining float64  `json:"errorBudgetRemaining"`
	Breaches             []string `json:"breaches,omitempty"`
}

const (
	sloNameAutonomy       = "autonomy"
	sloNameCIFirstPass    = "ci_first_pass"
	sloNameRework         = "rework"
	sloNameIdenticalRetry = "identical_retry_cap"
	sloNameRestartCadence = "restart_cadence"
)

// EvaluateSLOs grades a Scorecard + SLOSignals against targets. Pure: no I/O,
// no clock reads — callers stamp GeneratedAt on the containing Report, not
// here. A Scorecard with TasksLanded == 0 still produces a report (every
// rate-based status reads 0 vs its target, i.e. compliant) since an empty
// window represents no evidence of a breach, not a breach itself.
func EvaluateSLOs(sc Scorecard, sig SLOSignals, targets SLOTargets) SLOReport {
	reworkRate := 0.0
	if sc.TasksLanded > 0 {
		reworkRate = float64(sc.ReworkTasks) / float64(sc.TasksLanded)
	}

	statuses := []SLOStatus{
		minStatusOrNoEvidence(sloNameAutonomy, sc.TasksLanded > 0, sc.AutonomyRate, targets.MinAutonomyRate),
		minStatusOrNoEvidence(sloNameCIFirstPass, sc.TasksLanded > 0, sc.CIFirstPassRate, targets.MinCIFirstPassRate),
		maxStatus(sloNameRework, reworkRate, targets.MaxReworkRate),
		maxStatus(sloNameIdenticalRetry, float64(sig.IdenticalRetryMax), float64(targets.MaxIdenticalRetryCap)),
		maxStatus(sloNameRestartCadence, sig.RestartsPerHour, targets.MaxRestartsPerHour),
	}

	rep := SLOReport{Targets: targets, Statuses: statuses, Compliant: true}
	budget := 1.0
	for i := range statuses {
		s := &statuses[i]
		if !s.Met {
			rep.Compliant = false
			rep.Breaches = append(rep.Breaches, s.Name)
		}
		if s.BudgetRemaining < budget {
			budget = s.BudgetRemaining
		}
	}
	rep.ErrorBudgetRemaining = budget
	return rep
}

// minStatusOrNoEvidence grades a "must be at least target" SLO but treats a
// window with no evidence (hasEvidence=false, e.g. TasksLanded == 0) as
// compliant rather than reading Scorecard's zero-value rate as a 0% breach —
// an empty window is the absence of a signal, not proof of one. Mirrors the
// same judgment call Weaknesses.minLandedForSignal makes for the identical
// autonomy/ci_first_pass ratios.
func minStatusOrNoEvidence(name string, hasEvidence bool, actual, target float64) SLOStatus {
	if !hasEvidence {
		return SLOStatus{Name: name, Actual: 0, Target: target, Met: true, BudgetRemaining: 1}
	}
	return minStatus(name, actual, target)
}

// minStatus grades a "must be at least target" SLO (autonomy, CI-first-pass).
// Headroom is the distance from target up to a perfect 1.0, so a metric
// sitting exactly on target reads BudgetRemaining=0 (on the edge of breach)
// and one sitting at 1.0 reads 1 (maximum headroom).
func minStatus(name string, actual, target float64) SLOStatus {
	met := actual >= target
	headroom := 1 - target
	budget := 0.0
	if headroom > 0 {
		budget = clamp01((actual - target) / headroom)
	} else if met {
		budget = 1
	}
	return SLOStatus{Name: name, Actual: actual, Target: target, Met: met, BudgetRemaining: budget}
}

// maxStatus grades a "must be at most target" SLO (rework rate, identical
// retry cap, restart cadence). Headroom is target itself (distance from
// target down to zero), so a metric at target reads BudgetRemaining=0 and one
// at zero reads 1.
func maxStatus(name string, actual, target float64) SLOStatus {
	met := actual <= target
	budget := 0.0
	if target > 0 {
		budget = clamp01((target - actual) / target)
	} else if met {
		budget = 1
	}
	return SLOStatus{Name: name, Actual: actual, Target: target, Met: met, BudgetRemaining: budget}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// scanIdenticalRetryCap returns the largest number of failed agent runs
// recorded for any single task inside [since, until] — the identical-retry
// signal for MaxIdenticalRetryCap. Mirrors internal/health's
// checkAgentRetryLoops fingerprint (same task + failed outcome), the
// existing definition of a retry loop, via the same audit.RunRecords
// normalization (agent.completed with a failed state, or the legacy
// agent.failed event type).
func scanIdenticalRetryCap(events []audit.Event, since, until time.Time) int {
	records := audit.RunRecords(events)
	perTask := map[string]int{}
	maxCount := 0
	for i := range records {
		r := records[i]
		if r.TaskID == "" || r.Timestamp.Before(since) || r.Timestamp.After(until) {
			continue
		}
		if runoutcome.Normalize(r.Outcome) != runoutcome.Failed {
			continue
		}
		perTask[r.TaskID]++
		if perTask[r.TaskID] > maxCount {
			maxCount = perTask[r.TaskID]
		}
	}
	return maxCount
}

// automaticRestartTolerance bounds how close a task.dispatched event must
// land to a human-required exit for scanRestartCadence to treat the exit as
// human-initiated rather than automatic. A manual dispatch
// (TaskService.DispatchFromHumanRequired) logs both events from the same
// call; the monitor's own auto-retry (internal/monitor/remediator.go) and
// the PR-monitor's blocker reconciliation (internal/sybra/review/outbound.go)
// update task status directly and never log task.dispatched.
const automaticRestartTolerance = 60 * time.Second

// scanRestartCadence returns the rate (per hour) of automatic human-required
// recovery restarts inside [since, until]: a task.status_changed event with
// from="human-required" and to in {"in-progress","in-review"} that has no
// paired task.dispatched event for the same task within
// automaticRestartTolerance. These are the two automatic-recovery
// transitions documented in orchestrator/CLAUDE.md's Status Transitions
// table; a human clicking "Dispatch" in the GUI produces the identical
// from/to pair but always logs task.dispatched alongside it, which is what
// distinguishes the two without a dedicated "automatic" flag on the event.
func scanRestartCadence(events []audit.Event, since, until time.Time) float64 {
	hours := until.Sub(since).Hours()
	if hours <= 0 {
		return 0
	}
	dispatchedAt := map[string][]time.Time{}
	for i := range events {
		e := events[i]
		if e.Type == audit.EventTaskDispatched && e.TaskID != "" {
			dispatchedAt[e.TaskID] = append(dispatchedAt[e.TaskID], e.Timestamp)
		}
	}
	manuallyDispatchedNear := func(taskID string, at time.Time) bool {
		for _, ts := range dispatchedAt[taskID] {
			d := ts.Sub(at)
			if d < 0 {
				d = -d
			}
			if d <= automaticRestartTolerance {
				return true
			}
		}
		return false
	}

	restarts := 0
	for i := range events {
		e := events[i]
		if e.Type != audit.EventTaskStatusChanged || e.Timestamp.Before(since) || e.Timestamp.After(until) {
			continue
		}
		if strVal(e.Data, "from") != "human-required" {
			continue
		}
		to := strVal(e.Data, "to")
		if to != "in-progress" && to != "in-review" {
			continue
		}
		if manuallyDispatchedNear(e.TaskID, e.Timestamp) {
			continue
		}
		restarts++
	}
	return float64(restarts) / hours
}
