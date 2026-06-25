// Package evaluation computes a periodic scorecard measuring how well the agent
// fleet performs — autonomy, throughput/stability, reliability, and efficiency —
// from the stats and audit data Sybra already records. It is read-only: it never
// dispatches agents or files issues. The scorecard is the ground-truth signal the
// dashboard (and future LLM-judge) build on.
package evaluation

import (
	"sort"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/stats"
)

// Scorecard holds the aggregate metrics over one time window.
//
// Landing-derived metrics read task.landed audit events; reliability and
// efficiency read stats run records (the run Outcome carries the accurate
// success/failure outcome). Metrics that require signals not yet captured
// (merge-without-edit, change-failure rate, review density) are deferred —
// see Report.Notes.
type Scorecard struct {
	WindowDays float64 `json:"windowDays"`

	// Throughput & outcomes (from task.landed events).
	TasksLanded   int     `json:"tasksLanded"`
	Merged        int     `json:"merged"`
	Closed        int     `json:"closed"`
	MergeRate     float64 `json:"mergeRate"`     // merged / landed
	LeadTimeP50H  float64 `json:"leadTimeP50H"`  // created_to_land_h
	LeadTimeP90H  float64 `json:"leadTimeP90H"`  //
	CycleTimeP50H float64 `json:"cycleTimeP50H"` // work_to_land_h (first agent run → land)
	CycleTimeP90H float64 `json:"cycleTimeP90H"` //

	// Autonomy: did landed work reach done without a human in the loop?
	AutonomousLandings   int     `json:"autonomousLandings"`
	HumanTouchedLandings int     `json:"humanTouchedLandings"`
	AutonomyRate         float64 `json:"autonomyRate"` // autonomous / landed

	// Reliability (from stats run outcomes).
	AgentRuns       int     `json:"agentRuns"`
	AgentFailures   int     `json:"agentFailures"`
	FailureRate     float64 `json:"failureRate"`
	CIFirstPassRate float64 `json:"ciFirstPassRate"` // landed without a CI-fix / landed

	// Efficiency: window spend and effort per landed PR.
	TotalCostUSD   float64 `json:"totalCostUsd"`
	CostPerLanded  float64 `json:"costPerLanded"`
	TurnsPerLanded float64 `json:"turnsPerLanded"`
	ToolsPerLanded float64 `json:"toolsPerLanded"`
	ReworkTasks    int     `json:"reworkTasks"` // tasks with a repeated status transition
}

// Report is the persisted, emitted, and CLI-printed output of one evaluation tick.
type Report struct {
	GeneratedAt time.Time `json:"generatedAt"`
	Since       time.Time `json:"since"`
	Until       time.Time `json:"until"`
	Overall     Scorecard `json:"overall"`
	Notes       []string  `json:"notes,omitempty"`
}

// deferredNotes documents metrics that need signals not yet captured, so the
// report never silently presents a partial picture as complete.
var deferredNotes = []string{
	"merge-without-edit rate and human-edit distance pending #1082",
	"change-failure rate and MTTR pending revert detection (#1082)",
	"review-finding density pending review-count capture (#1082)",
}

// taskSignals accumulates per-task lifecycle facts used for autonomy,
// CI-first-pass, and rework detection.
type taskSignals struct {
	humanTouched bool
	ciFixNeeded  bool
	transitions  map[string]int
}

// Compute builds a scorecard from run records and audit events bounded by
// [since, until]. Records and events outside the window are ignored, so callers
// may pass a superset. Pure: no I/O, deterministic for a given input.
func Compute(records []stats.RunRecord, events []audit.Event, since, until time.Time) Scorecard {
	sc := Scorecard{WindowDays: until.Sub(since).Hours() / 24}
	win := func(t time.Time) bool { return !t.Before(since) && !t.After(until) }

	lg := scanLandings(events, win)
	sigs := scanTaskSignals(events) // not window-bound: capture full task history
	runs, fails := scanReliability(records, win)
	cost, turns, tools := scanEfficiency(records, win)

	sc.TasksLanded, sc.Merged, sc.Closed = lg.count, lg.merged, lg.closed
	sc.AgentRuns, sc.AgentFailures = runs, fails
	sc.TotalCostUSD = cost
	autonomous, humanTouched, ciClean := classifyLanded(lg.tasks, sigs)
	sc.AutonomousLandings, sc.HumanTouchedLandings = autonomous, humanTouched
	sc.ReworkTasks = countRework(sigs)

	if n := float64(sc.TasksLanded); n > 0 {
		sc.MergeRate = float64(sc.Merged) / n
		sc.AutonomyRate = float64(autonomous) / n
		sc.CIFirstPassRate = float64(ciClean) / n
		sc.CostPerLanded = cost / n
		sc.TurnsPerLanded = float64(turns) / n
		sc.ToolsPerLanded = float64(tools) / n
	}
	if sc.AgentRuns > 0 {
		sc.FailureRate = float64(sc.AgentFailures) / float64(sc.AgentRuns)
	}
	sc.LeadTimeP50H = percentile(lg.leadTimes, 50)
	sc.LeadTimeP90H = percentile(lg.leadTimes, 90)
	sc.CycleTimeP50H = percentile(lg.cycleTimes, 50)
	sc.CycleTimeP90H = percentile(lg.cycleTimes, 90)
	return sc
}

// landingAgg holds the outcome counts and timing samples from task.landed events.
type landingAgg struct {
	count, merged, closed int
	leadTimes, cycleTimes []float64
	tasks                 map[string]bool
}

func scanLandings(events []audit.Event, win func(time.Time) bool) landingAgg {
	lg := landingAgg{tasks: map[string]bool{}}
	for i := range events {
		e := events[i]
		if e.Type != audit.EventTaskLanded || !win(e.Timestamp) {
			continue
		}
		lg.count++
		if e.TaskID != "" {
			lg.tasks[e.TaskID] = true
		}
		if strVal(e.Data, "outcome") == "closed" {
			lg.closed++
		} else {
			lg.merged++
		}
		if v, ok := floatVal(e.Data, "created_to_land_h"); ok {
			lg.leadTimes = append(lg.leadTimes, v)
		}
		if v, ok := floatVal(e.Data, "work_to_land_h"); ok {
			lg.cycleTimes = append(lg.cycleTimes, v)
		}
	}
	return lg
}

// scanTaskSignals is intentionally not window-bound: a task that lands inside
// the window may have been escalated to human-required or had its CI fixed
// before the window opened. Callers pass a wider event range so those signals
// are not lost (which would over-report autonomy and CI-first-pass).
func scanTaskSignals(events []audit.Event) map[string]*taskSignals {
	sigs := map[string]*taskSignals{}
	sig := func(id string) *taskSignals {
		s := sigs[id]
		if s == nil {
			s = &taskSignals{transitions: map[string]int{}}
			sigs[id] = s
		}
		return s
	}
	for i := range events {
		e := events[i]
		if e.TaskID == "" {
			continue
		}
		switch e.Type {
		case audit.EventTaskStatusChanged:
			s := sig(e.TaskID)
			if strVal(e.Data, "to") == "human-required" {
				s.humanTouched = true
			}
			s.transitions[strVal(e.Data, "from")+"->"+strVal(e.Data, "to")]++
		case audit.EventHumanReviewSpawned:
			sig(e.TaskID).humanTouched = true
		case audit.EventPRCIFailureDetected, audit.EventPRFixAgentStarted, audit.EventRenovateCIFix:
			sig(e.TaskID).ciFixNeeded = true
		}
	}
	return sigs
}

// scanReliability derives runs and failures from stats run records, not audit
// events: the failure outcome is recorded on every run record (set from the
// process exit), whereas a distinct agent.failed audit event is never emitted.
func scanReliability(records []stats.RunRecord, win func(time.Time) bool) (runs, failures int) {
	for i := range records {
		r := records[i]
		if !win(r.Timestamp) {
			continue
		}
		runs++
		if r.Outcome == "failed" {
			failures++
		}
	}
	return runs, failures
}

func scanEfficiency(records []stats.RunRecord, win func(time.Time) bool) (cost float64, turns, tools int) {
	for i := range records {
		r := records[i]
		if !win(r.Timestamp) {
			continue
		}
		cost += r.CostUSD
		turns += r.TurnCount
		tools += r.ToolCalls
	}
	return cost, turns, tools
}

// classifyLanded splits landed tasks into autonomous vs human-touched and counts
// those that landed without a CI fix. A task with no recorded signals counts as
// autonomous and CI-clean.
func classifyLanded(landed map[string]bool, sigs map[string]*taskSignals) (autonomous, humanTouched, ciClean int) {
	for id := range landed {
		s := sigs[id]
		if s == nil || !s.humanTouched {
			autonomous++
		} else {
			humanTouched++
		}
		if s == nil || !s.ciFixNeeded {
			ciClean++
		}
	}
	return autonomous, humanTouched, ciClean
}

func countRework(sigs map[string]*taskSignals) int {
	n := 0
	for _, s := range sigs {
		for _, c := range s.transitions {
			if c >= 2 {
				n++
				break
			}
		}
	}
	return n
}

// percentile returns the p-th percentile (0–100) using nearest-rank. Empty → 0.
func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	if p <= 0 {
		return s[0]
	}
	if p >= 100 {
		return s[len(s)-1]
	}
	rank := int(p/100*float64(len(s)) + 0.999999) // ceil
	rank = max(1, min(rank, len(s)))
	return s[rank-1]
}

func strVal(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}

// floatVal reads a numeric field, tolerating the float64 that JSON round-trips
// produce as well as in-process int values.
func floatVal(m map[string]any, k string) (float64, bool) {
	if m == nil {
		return 0, false
	}
	switch v := m[k].(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
}
