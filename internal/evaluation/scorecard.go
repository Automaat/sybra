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
// (change-failure rate, review density) are deferred — see Report.Notes.
type Scorecard struct {
	WindowDays float64 `json:"windowDays"`

	// Throughput & outcomes (from task.landed events).
	TasksLanded     int     `json:"tasksLanded"`
	Merged          int     `json:"merged"`          // clean merges (no human edits)
	MergedWithEdits int     `json:"mergedWithEdits"` // merged after a human edited the PR
	Closed          int     `json:"closed"`
	MergeRate       float64 `json:"mergeRate"`     // clean merged / landed
	LeadTimeP50H    float64 `json:"leadTimeP50H"`  // created_to_land_h
	LeadTimeP90H    float64 `json:"leadTimeP90H"`  //
	CycleTimeP50H   float64 `json:"cycleTimeP50H"` // work_to_land_h (first agent run → land)
	CycleTimeP90H   float64 `json:"cycleTimeP90H"` //

	// Autonomy: did landed work reach done without a human in the loop?
	AutonomousLandings   int     `json:"autonomousLandings"`
	HumanTouchedLandings int     `json:"humanTouchedLandings"`
	AutonomyRate         float64 `json:"autonomyRate"` // autonomous / landed

	// Reliability (from stats run outcomes).
	AgentRuns         int     `json:"agentRuns"`
	AgentFailures     int     `json:"agentFailures"`
	FailureRate       float64 `json:"failureRate"`
	CIFirstPassRate   float64 `json:"ciFirstPassRate"`   // landed without a CI-fix / landed
	Reverted          int     `json:"reverted"`          // merged landings later reverted on the default branch
	ChangeFailureRate float64 `json:"changeFailureRate"` // reverted / merged landings (DORA)

	// Efficiency: window spend and effort per landed PR.
	TotalCostUSD   float64 `json:"totalCostUsd"`
	CostPerLanded  float64 `json:"costPerLanded"`
	TurnsPerLanded float64 `json:"turnsPerLanded"`
	ToolsPerLanded float64 `json:"toolsPerLanded"`
	ReworkTasks    int     `json:"reworkTasks"` // tasks with a repeated status transition
}

// Breakdown is the per-dimension (provider, role) slice of the effort and
// reliability metrics derivable from stats run records. Landing-derived metrics
// (autonomy, throughput) are not broken down because task.landed events don't
// carry provider/role/project yet — see Report.Notes.
type Breakdown struct {
	Key          string  `json:"key"`
	Runs         int     `json:"runs"`
	Failures     int     `json:"failures"`
	FailureRate  float64 `json:"failureRate"`
	TotalCostUSD float64 `json:"totalCostUsd"`
	Turns        int     `json:"turns"`
	Tools        int     `json:"tools"`
}

// ComparisonBreakdown compares agent/model or experiment variants on the
// speed, quality, and cost signals Sybra already records.
type ComparisonBreakdown struct {
	Key                       string  `json:"key"`
	Provider                  string  `json:"provider,omitempty"`
	Model                     string  `json:"model,omitempty"`
	Role                      string  `json:"role,omitempty"`
	ReasoningEffort           string  `json:"reasoningEffort,omitempty"`
	ExperimentID              string  `json:"experimentId,omitempty"`
	VariantID                 string  `json:"variantId,omitempty"`
	Runs                      int     `json:"runs"`
	Failures                  int     `json:"failures"`
	FailureRate               float64 `json:"failureRate"`
	Landed                    int     `json:"landed"`
	Merged                    int     `json:"merged"`
	MergedWithEdits           int     `json:"mergedWithEdits"`
	Closed                    int     `json:"closed"`
	MergeRate                 float64 `json:"mergeRate"`
	MergedWithEditsRate       float64 `json:"mergedWithEditsRate"`
	CIFirstPassRate           float64 `json:"ciFirstPassRate"`
	ReworkRate                float64 `json:"reworkRate"`
	RevertRate                float64 `json:"revertRate"`
	DurationP50S              float64 `json:"durationP50S"`
	DurationP90S              float64 `json:"durationP90S"`
	TotalCostUSD              float64 `json:"totalCostUsd"`
	CostPerLanded             float64 `json:"costPerLanded"`
	PremiumRequests           float64 `json:"premiumRequests"`
	PremiumRequestsPerLanded  float64 `json:"premiumRequestsPerLanded"`
	TurnsPerLanded            float64 `json:"turnsPerLanded"`
	ToolsPerLanded            float64 `json:"toolsPerLanded"`
	InsufficientData          bool    `json:"insufficientData"`
	QualityAttributionLimited bool    `json:"qualityAttributionLimited"`
}

// Report is the persisted, emitted, and CLI-printed output of one evaluation tick.
type Report struct {
	GeneratedAt  time.Time             `json:"generatedAt"`
	Since        time.Time             `json:"since"`
	Until        time.Time             `json:"until"`
	Overall      Scorecard             `json:"overall"`
	ByProvider   []Breakdown           `json:"byProvider,omitempty"`
	ByRole       []Breakdown           `json:"byRole,omitempty"`
	ByAgentModel []ComparisonBreakdown `json:"byAgentModel,omitempty"`
	ByVariant    []ComparisonBreakdown `json:"byVariant,omitempty"`
	Weaknesses   []Weakness            `json:"weaknesses,omitempty"`
	Notes        []string              `json:"notes,omitempty"`
}

// deferredNotes documents metrics that need signals not yet captured, so the
// report never silently presents a partial picture as complete.
var deferredNotes = []string{
	"MTTR (time to restore after a revert) pending revert-to-fix timing",
	"review-finding density pending review-count capture",
	"per-project/provider autonomy + throughput breakdowns pending project/provider on task.landed",
	"revert detection scans the latest 100 default-branch commits per repo; a revert beyond that on a very busy repo can be missed",
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
	sc.MergedWithEdits = lg.mergedWithEdits
	sc.AgentRuns, sc.AgentFailures = runs, fails
	sc.Reverted = countReverts(events, win, lg.tasks)
	sc.TotalCostUSD = cost
	autonomous, humanTouched, ciClean := classifyLanded(lg.tasks, lg.edited, sigs)
	sc.AutonomousLandings, sc.HumanTouchedLandings = autonomous, humanTouched
	sc.ReworkTasks = countRework(sigs, lg.tasks)

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
	if mergedLandings := sc.Merged + sc.MergedWithEdits; mergedLandings > 0 {
		sc.ChangeFailureRate = float64(sc.Reverted) / float64(mergedLandings)
	}
	sc.LeadTimeP50H = percentile(lg.leadTimes, 50)
	sc.LeadTimeP90H = percentile(lg.leadTimes, 90)
	sc.CycleTimeP50H = percentile(lg.cycleTimes, 50)
	sc.CycleTimeP90H = percentile(lg.cycleTimes, 90)
	return sc
}

// landingAgg holds the outcome counts and timing samples from task.landed events.
type landingAgg struct {
	count, merged, mergedWithEdits, closed int
	leadTimes, cycleTimes                  []float64
	tasks                                  map[string]bool
	edited                                 map[string]bool // landed but a human edited the PR
}

func scanLandings(events []audit.Event, win func(time.Time) bool) landingAgg {
	lg := landingAgg{tasks: map[string]bool{}, edited: map[string]bool{}}
	for i := range events {
		e := events[i]
		if e.Type != audit.EventTaskLanded || !win(e.Timestamp) {
			continue
		}
		lg.count++
		if e.TaskID != "" {
			lg.tasks[e.TaskID] = true
		}
		switch strVal(e.Data, "outcome") {
		case "closed":
			lg.closed++
		case "merged_with_edits":
			lg.mergedWithEdits++
			if e.TaskID != "" {
				lg.edited[e.TaskID] = true // a human edited the PR → not autonomous
			}
		default:
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
// countReverts counts pr.reverted events in the window — but only for tasks that
// also landed in the window, so the change-failure numerator and denominator
// share a cohort (a revert of a PR that merged before the window doesn't push
// the rate above 100%).
func countReverts(events []audit.Event, win func(time.Time) bool, landed map[string]bool) int {
	n := 0
	for i := range events {
		e := events[i]
		if e.Type == audit.EventPRReverted && win(e.Timestamp) && landed[e.TaskID] {
			n++
		}
	}
	return n
}

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
func classifyLanded(landed, edited map[string]bool, sigs map[string]*taskSignals) (autonomous, humanTouched, ciClean int) {
	for id := range landed {
		s := sigs[id]
		// A task is human-touched if it went human-required / spawned a human
		// review (sigs) OR a human edited its PR before merge (merged_with_edits).
		if (s == nil || !s.humanTouched) && !edited[id] {
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

// countRework counts how many landed tasks bounced (a status transition seen
// 2+ times). Scoped to landed tasks so signals from tasks that never landed (or
// landed outside the window) — visible because the signal read is wider than the
// landing window — don't inflate the count.
func countRework(sigs map[string]*taskSignals, landed map[string]bool) int {
	n := 0
	for id := range landed {
		s := sigs[id]
		if s == nil {
			continue
		}
		for _, c := range s.transitions {
			if c >= 2 {
				n++
				break
			}
		}
	}
	return n
}

// BreakdownBy groups in-window run records by key and computes effort and
// reliability per group, sorted by key. Records with an empty key are skipped.
func BreakdownBy(records []stats.RunRecord, since, until time.Time, key func(stats.RunRecord) string) []Breakdown {
	type acc struct {
		runs, fails, turns, tools int
		cost                      float64
	}
	groups := map[string]*acc{}
	for i := range records {
		r := records[i]
		if r.Timestamp.Before(since) || r.Timestamp.After(until) {
			continue
		}
		k := key(r)
		if k == "" {
			continue
		}
		a := groups[k]
		if a == nil {
			a = &acc{}
			groups[k] = a
		}
		a.runs++
		if r.Outcome == "failed" {
			a.fails++
		}
		a.cost += r.CostUSD
		a.turns += r.TurnCount
		a.tools += r.ToolCalls
	}
	out := make([]Breakdown, 0, len(groups))
	for k, a := range groups {
		b := Breakdown{Key: k, Runs: a.runs, Failures: a.fails, TotalCostUSD: a.cost, Turns: a.turns, Tools: a.tools}
		if a.runs > 0 {
			b.FailureRate = float64(a.fails) / float64(a.runs)
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// CompareBy groups run records by key and attributes landed-task outcomes to
// the latest code-author run for that task. minSamples controls the
// InsufficientData flag only; rows are still emitted so users can see early data.
type comparisonAcc struct {
	row             ComparisonBreakdown
	durations       []float64
	turns, tools    int
	ciClean, rework int
	reverted        int
}

func CompareBy(records []stats.RunRecord, events []audit.Event, since, until time.Time, minSamples int, key func(stats.RunRecord) string) []ComparisonBreakdown {
	groups := map[string]*comparisonAcc{}
	ensure := func(r stats.RunRecord, k string) *comparisonAcc {
		a := groups[k]
		if a == nil {
			a = &comparisonAcc{row: ComparisonBreakdown{
				Key:             k,
				Provider:        r.Provider,
				Model:           r.Model,
				Role:            normalizedRole(r.Role),
				ReasoningEffort: r.ReasoningEffort,
				ExperimentID:    r.ExperimentID,
				VariantID:       r.VariantID,
			}}
			groups[k] = a
		}
		return a
	}
	for i := range records {
		r := records[i]
		if r.Timestamp.Before(since) || r.Timestamp.After(until) {
			continue
		}
		k := key(r)
		if k == "" {
			continue
		}
		a := ensure(r, k)
		a.row.Runs++
		if r.Outcome == "failed" {
			a.row.Failures++
		}
		a.row.TotalCostUSD += r.CostUSD
		a.row.PremiumRequests += r.PremiumRequests
		a.durations = append(a.durations, r.DurationS)
		a.turns += r.TurnCount
		a.tools += r.ToolCalls
	}

	applyComparisonLandings(ensure, records, events, since, until, key)
	return comparisonRows(groups, minSamples)
}

func applyComparisonLandings(ensure func(stats.RunRecord, string) *comparisonAcc, records []stats.RunRecord, events []audit.Event, since, until time.Time, key func(stats.RunRecord) string) {
	landed := scanLandings(events, func(t time.Time) bool { return !t.Before(since) && !t.After(until) })
	sigs := scanTaskSignals(events)
	for taskID := range landed.tasks {
		landedAt := landingTimestamp(events, taskID, since, until)
		if landedAt.IsZero() {
			continue
		}
		r, ok := latestAuthorRun(records, taskID, since, landedAt, key)
		if !ok {
			continue
		}
		k := key(r)
		if k == "" {
			continue
		}
		a := ensure(r, k)
		a.row.Landed++
		if landed.edited[taskID] {
			a.row.MergedWithEdits++
		} else {
			// scanLandings tracks closed only as aggregate; read the per-task
			// outcome directly so variant rows preserve merged vs closed.
			switch landingOutcome(events, taskID, since, until) {
			case "closed":
				a.row.Closed++
			default:
				a.row.Merged++
			}
		}
		if s := sigs[taskID]; s == nil || !s.ciFixNeeded {
			a.ciClean++
		}
		if taskHasRework(sigs[taskID]) {
			a.rework++
		}
	}
	for i := range events {
		e := events[i]
		if e.Type != audit.EventPRReverted || e.TaskID == "" || e.Timestamp.Before(since) || e.Timestamp.After(until) {
			continue
		}
		r, ok := latestAuthorRun(records, e.TaskID, since, e.Timestamp, key)
		if !ok {
			continue
		}
		if k := key(r); k != "" {
			ensure(r, k).reverted++
		}
	}
}

func comparisonRows(groups map[string]*comparisonAcc, minSamples int) []ComparisonBreakdown {
	out := make([]ComparisonBreakdown, 0, len(groups))
	for _, a := range groups {
		row := a.row
		if row.Runs > 0 {
			row.FailureRate = float64(row.Failures) / float64(row.Runs)
		}
		if row.Landed > 0 {
			row.MergeRate = float64(row.Merged) / float64(row.Landed)
			row.MergedWithEditsRate = float64(row.MergedWithEdits) / float64(row.Landed)
			row.CIFirstPassRate = float64(a.ciClean) / float64(row.Landed)
			row.ReworkRate = float64(a.rework) / float64(row.Landed)
			row.RevertRate = float64(a.reverted) / float64(row.Landed)
			row.CostPerLanded = row.TotalCostUSD / float64(row.Landed)
			row.PremiumRequestsPerLanded = row.PremiumRequests / float64(row.Landed)
			row.TurnsPerLanded = float64(a.turns) / float64(row.Landed)
			row.ToolsPerLanded = float64(a.tools) / float64(row.Landed)
		}
		row.DurationP50S = percentile(a.durations, 50)
		row.DurationP90S = percentile(a.durations, 90)
		row.InsufficientData = minSamples > 0 && row.Runs < minSamples
		row.QualityAttributionLimited = row.Landed == 0 && row.Runs > 0
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func latestAuthorRun(records []stats.RunRecord, taskID string, since, until time.Time, key func(stats.RunRecord) string) (stats.RunRecord, bool) {
	var best stats.RunRecord
	ok := false
	for i := range records {
		r := records[i]
		if r.TaskID != taskID || r.Timestamp.Before(since) || r.Timestamp.After(until) || !isAuthorRole(r.Role) || key(r) == "" {
			continue
		}
		if !ok || r.Timestamp.After(best.Timestamp) {
			best = r
			ok = true
		}
	}
	return best, ok
}

func landingOutcome(events []audit.Event, taskID string, since, until time.Time) string {
	for i := range events {
		e := events[i]
		if e.Type == audit.EventTaskLanded && e.TaskID == taskID && !e.Timestamp.Before(since) && !e.Timestamp.After(until) {
			return strVal(e.Data, "outcome")
		}
	}
	return ""
}

func landingTimestamp(events []audit.Event, taskID string, since, until time.Time) time.Time {
	var out time.Time
	for i := range events {
		e := events[i]
		if e.Type != audit.EventTaskLanded || e.TaskID != taskID || e.Timestamp.Before(since) || e.Timestamp.After(until) {
			continue
		}
		if out.IsZero() || e.Timestamp.Before(out) {
			out = e.Timestamp
		}
	}
	return out
}

func taskHasRework(s *taskSignals) bool {
	if s == nil {
		return false
	}
	for _, c := range s.transitions {
		if c >= 2 {
			return true
		}
	}
	return false
}

func isAuthorRole(role string) bool {
	switch normalizedRole(role) {
	case "implementation", "fix-review", "pr-fix":
		return true
	default:
		return false
	}
}

func normalizedRole(role string) string {
	if role == "" {
		return "implementation"
	}
	return role
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
