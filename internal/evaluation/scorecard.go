// Package evaluation computes a periodic scorecard measuring how well the agent
// fleet performs — autonomy, throughput/stability, reliability, and efficiency —
// from the stats and audit data Sybra already records. It is read-only: it never
// dispatches agents or files issues. The scorecard is the ground-truth signal the
// dashboard (and future LLM-judge) build on.
package evaluation

import (
	"encoding/base64"
	"math"
	"sort"
	"time"

	"github.com/Automaat/sybra/internal/abtest"
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
type ComparisonAttributionMode string

const (
	ComparisonAttributionLatestAuthor    ComparisonAttributionMode = "latest_author"
	ComparisonAttributionAnyContribution ComparisonAttributionMode = "any_author_contribution"
)

type ComparisonBreakdown struct {
	Key                       string                `json:"key"`
	AttributionMode           string                `json:"attributionMode"`
	Provider                  string                `json:"provider,omitempty"`
	Model                     string                `json:"model,omitempty"`
	Role                      string                `json:"role,omitempty"`
	ReasoningEffort           string                `json:"reasoningEffort,omitempty"`
	ExperimentID              string                `json:"experimentId,omitempty"`
	VariantID                 string                `json:"variantId,omitempty"`
	Runs                      int                   `json:"runs"`
	Failures                  int                   `json:"failures"`
	FailureRate               float64               `json:"failureRate"`
	FailureEstimate           RateEstimate          `json:"failureEstimate"`
	Landed                    int                   `json:"landed"`
	LandedEstimate            RateEstimate          `json:"landedEstimate"`
	Merged                    int                   `json:"merged"`
	MergedWithEdits           int                   `json:"mergedWithEdits"`
	Closed                    int                   `json:"closed"`
	MergeRate                 float64               `json:"mergeRate"`
	MergedWithEditsRate       float64               `json:"mergedWithEditsRate"`
	CIFirstPassRate           float64               `json:"ciFirstPassRate"`
	ReworkRate                float64               `json:"reworkRate"`
	RevertRate                float64               `json:"revertRate"`
	MergeEstimate             RateEstimate          `json:"mergeEstimate"`
	CIFirstPassEstimate       RateEstimate          `json:"ciFirstPassEstimate"`
	MergedWithEditsEstimate   RateEstimate          `json:"mergedWithEditsEstimate"`
	ReworkEstimate            RateEstimate          `json:"reworkEstimate"`
	RevertEstimate            RateEstimate          `json:"revertEstimate"`
	DurationP50S              float64               `json:"durationP50S"`
	DurationP90S              float64               `json:"durationP90S"`
	TotalCostUSD              float64               `json:"totalCostUsd"`
	CostPerLanded             float64               `json:"costPerLanded"`
	PremiumRequests           float64               `json:"premiumRequests"`
	PremiumRequestsPerLanded  float64               `json:"premiumRequestsPerLanded"`
	TurnsPerLanded            float64               `json:"turnsPerLanded"`
	ToolsPerLanded            float64               `json:"toolsPerLanded"`
	InsufficientData          bool                  `json:"insufficientData"`
	QualityAttributionLimited bool                  `json:"qualityAttributionLimited"`
	Baseline                  bool                  `json:"baseline"`
	BaselineVariantID         string                `json:"baselineVariantId,omitempty"`
	SampleStatus              string                `json:"sampleStatus,omitempty"`
	MinSamplesPerVariant      int                   `json:"minSamplesPerVariant,omitempty"`
	RoleBreakdowns            []ComparisonBreakdown `json:"roleBreakdowns,omitempty"`
}

// RateEstimate is a binomial rate with fixed 95% Wilson uncertainty and an
// optional effect delta relative to an A/B baseline row.
type RateEstimate struct {
	Numerator         int     `json:"numerator"`
	Denominator       int     `json:"denominator"`
	Point             float64 `json:"point"`
	WilsonLower       float64 `json:"wilsonLower"`
	WilsonUpper       float64 `json:"wilsonUpper"`
	DeltaFromBaseline float64 `json:"deltaFromBaseline"`
	HasDelta          bool    `json:"hasDelta"`
	HasData           bool    `json:"hasData"`
}

// VariantSampleStatus describes whether one configured or observed A/B variant
// has enough samples for the configured minimum.
type VariantSampleStatus struct {
	VariantID    string `json:"variantId"`
	Runs         int    `json:"runs"`
	Ready        bool   `json:"ready"`
	Configured   bool   `json:"configured"`
	Observed     bool   `json:"observed"`
	SampleStatus string `json:"sampleStatus"`
}

// ExperimentSampleStatus summarizes sample readiness for an experiment/role.
type ExperimentSampleStatus struct {
	Key                  string                `json:"key"`
	ExperimentID         string                `json:"experimentId"`
	Role                 string                `json:"role"`
	BaselineVariantID    string                `json:"baselineVariantId,omitempty"`
	MinSamplesPerVariant int                   `json:"minSamplesPerVariant"`
	Variants             []VariantSampleStatus `json:"variants"`
	ReadyVariants        int                   `json:"readyVariants"`
	TotalRuns            int                   `json:"totalRuns"`
	Status               string                `json:"status"`
}

// Report is the persisted, emitted, and CLI-printed output of one evaluation tick.
type Report struct {
	GeneratedAt              time.Time                `json:"generatedAt"`
	Since                    time.Time                `json:"since"`
	Until                    time.Time                `json:"until"`
	Overall                  Scorecard                `json:"overall"`
	ByProvider               []Breakdown              `json:"byProvider,omitempty"`
	ByRole                   []Breakdown              `json:"byRole,omitempty"`
	ByAgentModel             []ComparisonBreakdown    `json:"byAgentModel,omitempty"`
	ByAgentModelContribution []ComparisonBreakdown    `json:"byAgentModelContribution,omitempty"`
	ByVariant                []ComparisonBreakdown    `json:"byVariant,omitempty"`
	ByVariantContribution    []ComparisonBreakdown    `json:"byVariantContribution,omitempty"`
	VariantExperiments       []ExperimentSampleStatus `json:"variantExperiments,omitempty"`
	Weaknesses               []Weakness               `json:"weaknesses,omitempty"`
	Notes                    []string                 `json:"notes,omitempty"`
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

// CompareOptions controls optional comparison semantics. MinSamples controls the
// InsufficientData flag only; rows are still emitted so users can see early data.
type CompareOptions struct {
	MinSamples  int
	Experiments []abtest.Experiment
}

// CompareResult is the row set plus optional experiment readiness summaries.
type CompareResult struct {
	Rows        []ComparisonBreakdown
	Experiments []ExperimentSampleStatus
}

// CompareByLatestAuthor and CompareByContribution group run records by key and
// attribute landed-task outcomes using explicit final-stage or contribution
// semantics. minSamples controls the InsufficientData flag only; rows are still
// emitted so users can see early data.
type comparisonAcc struct {
	row             ComparisonBreakdown
	durations       []float64
	turns, tools    int
	ciClean, rework int
	reverted        int
}

func CompareBy(records []stats.RunRecord, events []audit.Event, since, until time.Time, opts CompareOptions, key func(stats.RunRecord) string) CompareResult {
	return compareByAttribution(records, events, since, until, opts, key, ComparisonAttributionLatestAuthor)
}

func CompareByLatestAuthor(records []stats.RunRecord, events []audit.Event, since, until time.Time, minSamples int, key func(stats.RunRecord) string) []ComparisonBreakdown {
	return compareByAttribution(records, events, since, until, CompareOptions{MinSamples: minSamples}, key, ComparisonAttributionLatestAuthor).Rows
}

func CompareByContribution(records []stats.RunRecord, events []audit.Event, since, until time.Time, minSamples int, key func(stats.RunRecord) string) []ComparisonBreakdown {
	return compareByAttribution(records, events, since, until, CompareOptions{MinSamples: minSamples}, key, ComparisonAttributionAnyContribution).Rows
}

func compareByAttribution(records []stats.RunRecord, events []audit.Event, since, until time.Time, opts CompareOptions, key func(stats.RunRecord) string, mode ComparisonAttributionMode) CompareResult {
	groups := map[string]*comparisonAcc{}
	ensure := func(r stats.RunRecord, k string) *comparisonAcc {
		a := groups[k]
		if a == nil {
			a = &comparisonAcc{row: ComparisonBreakdown{
				Key:             k,
				AttributionMode: string(mode),
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

	applyComparisonLandings(ensure, records, events, since, until, key, mode)
	rows := comparisonRows(groups, opts.MinSamples)
	experiments := applyVariantSemantics(rows, opts)
	return CompareResult{Rows: rows, Experiments: experiments}
}

// CompareVariants returns A/B rows at experiment/variant granularity, with
// role-specific rows attached as drilldowns. It uses latest-author attribution.
func CompareVariants(records []stats.RunRecord, events []audit.Event, since, until time.Time, minSamples int) []ComparisonBreakdown {
	return compareVariantsByAttribution(records, events, since, until, CompareOptions{MinSamples: minSamples}, ComparisonAttributionLatestAuthor).Rows
}

func CompareVariantsByContribution(records []stats.RunRecord, events []audit.Event, since, until time.Time, minSamples int) []ComparisonBreakdown {
	return compareVariantsByAttribution(records, events, since, until, CompareOptions{MinSamples: minSamples}, ComparisonAttributionAnyContribution).Rows
}

func compareVariantsByAttribution(records []stats.RunRecord, events []audit.Event, since, until time.Time, opts CompareOptions, mode ComparisonAttributionMode) CompareResult {
	parentResult := compareByAttribution(records, events, since, until, CompareOptions{MinSamples: opts.MinSamples}, variantKey, mode)
	childResult := compareByAttribution(records, events, since, until, opts, variantRoleKey, mode)
	parents := parentResult.Rows
	children := childResult.Rows
	normalizeVariantParents(parents, records, since, until)

	for i := range parents {
		for j := range children {
			if children[j].ExperimentID == parents[i].ExperimentID && children[j].VariantID == parents[i].VariantID {
				parents[i].RoleBreakdowns = append(parents[i].RoleBreakdowns, children[j])
			}
		}
		sort.Slice(parents[i].RoleBreakdowns, func(a, b int) bool {
			return parents[i].RoleBreakdowns[a].Key < parents[i].RoleBreakdowns[b].Key
		})
	}
	return CompareResult{Rows: parents, Experiments: childResult.Experiments}
}

func variantKey(r stats.RunRecord) string {
	if r.ExperimentID == "" || r.VariantID == "" {
		return ""
	}
	return encodedComparisonKey(r.ExperimentID, r.VariantID)
}

func variantRoleKey(r stats.RunRecord) string {
	if r.ExperimentID == "" || r.VariantID == "" {
		return ""
	}
	return encodedComparisonKey(r.ExperimentID, r.VariantID, normalizedRole(r.Role))
}

func encodedComparisonKey(parts ...string) string {
	out := make([]byte, 0, len(parts)*12)
	for i, part := range parts {
		if i > 0 {
			out = append(out, ':')
		}
		out = base64.RawURLEncoding.AppendEncode(out, []byte(part))
	}
	return string(out)
}

func normalizeVariantParents(rows []ComparisonBreakdown, records []stats.RunRecord, since, until time.Time) {
	type meta struct {
		provider, model, reasoning valueConsensus
	}
	metas := map[string]*meta{}
	for i := range records {
		r := records[i]
		if r.Timestamp.Before(since) || r.Timestamp.After(until) {
			continue
		}
		k := variantKey(r)
		if k == "" {
			continue
		}
		m := metas[k]
		if m == nil {
			m = &meta{}
			metas[k] = m
		}
		m.provider.add(r.Provider)
		m.model.add(r.Model)
		m.reasoning.add(r.ReasoningEffort)
	}
	for i := range rows {
		rows[i].Role = ""
		if m := metas[rows[i].Key]; m != nil {
			rows[i].Provider = m.provider.value()
			rows[i].Model = m.model.value()
			rows[i].ReasoningEffort = m.reasoning.value()
		}
	}
}

type valueConsensus struct {
	seen   bool
	mixed  bool
	chosen string
}

func (v *valueConsensus) add(next string) {
	if !v.seen {
		v.seen = true
		v.chosen = next
		return
	}
	if v.chosen != next {
		v.mixed = true
	}
}

func (v *valueConsensus) value() string {
	if v.mixed {
		return ""
	}
	return v.chosen
}

func applyComparisonLandings(ensure func(stats.RunRecord, string) *comparisonAcc, records []stats.RunRecord, events []audit.Event, since, until time.Time, key func(stats.RunRecord) string, mode ComparisonAttributionMode) {
	landed := scanLandings(events, func(t time.Time) bool { return !t.Before(since) && !t.After(until) })
	sigs := scanTaskSignals(events)
	credited := map[string]map[string]stats.RunRecord{}
	for taskID := range landed.tasks {
		landedAt := landingTimestamp(events, taskID, since, until)
		if landedAt.IsZero() {
			continue
		}
		taskCredits := creditedAuthorRuns(records, taskID, since, landedAt, key, mode)
		if len(taskCredits) == 0 {
			continue
		}
		credited[taskID] = taskCredits
		for k := range taskCredits {
			r := taskCredits[k]
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
	}
	for i := range events {
		e := events[i]
		if e.Type != audit.EventPRReverted || e.TaskID == "" || e.Timestamp.Before(since) || e.Timestamp.After(until) {
			continue
		}
		for k := range credited[e.TaskID] {
			r := credited[e.TaskID][k]
			ensure(r, k).reverted++
		}
	}
}

func creditedAuthorRuns(records []stats.RunRecord, taskID string, since, landedAt time.Time, key func(stats.RunRecord) string, mode ComparisonAttributionMode) map[string]stats.RunRecord {
	switch mode {
	case ComparisonAttributionAnyContribution:
		return latestAuthorRunsByKey(records, taskID, since, landedAt, key)
	default:
		r, ok := latestAuthorRun(records, taskID, since, landedAt, key)
		if !ok {
			return nil
		}
		return map[string]stats.RunRecord{key(r): r}
	}
}

func latestAuthorRunsByKey(records []stats.RunRecord, taskID string, since, until time.Time, key func(stats.RunRecord) string) map[string]stats.RunRecord {
	best := map[string]stats.RunRecord{}
	for i := range records {
		r := records[i]
		if r.TaskID != taskID || r.Timestamp.Before(since) || r.Timestamp.After(until) || !isAuthorRole(r.Role) {
			continue
		}
		k := key(r)
		if k == "" {
			continue
		}
		if prev, ok := best[k]; !ok || r.Timestamp.After(prev.Timestamp) {
			best[k] = r
		}
	}
	return best
}

func comparisonRows(groups map[string]*comparisonAcc, minSamples int) []ComparisonBreakdown {
	out := make([]ComparisonBreakdown, 0, len(groups))
	for _, a := range groups {
		row := a.row
		row.FailureEstimate = wilson95(row.Failures, row.Runs)
		row.LandedEstimate = wilson95(row.Landed, row.Runs)
		row.MergeEstimate = wilson95(row.Merged, row.Landed)
		row.MergedWithEditsEstimate = wilson95(row.MergedWithEdits, row.Landed)
		row.CIFirstPassEstimate = wilson95(a.ciClean, row.Landed)
		row.ReworkEstimate = wilson95(a.rework, row.Landed)
		row.RevertEstimate = wilson95(a.reverted, row.Landed)
		if row.FailureEstimate.HasData {
			row.FailureRate = row.FailureEstimate.Point
		}
		if row.MergeEstimate.HasData {
			row.MergeRate = row.MergeEstimate.Point
		}
		if row.MergedWithEditsEstimate.HasData {
			row.MergedWithEditsRate = row.MergedWithEditsEstimate.Point
		}
		if row.CIFirstPassEstimate.HasData {
			row.CIFirstPassRate = row.CIFirstPassEstimate.Point
		}
		if row.ReworkEstimate.HasData {
			row.ReworkRate = row.ReworkEstimate.Point
		}
		if row.RevertEstimate.HasData {
			row.RevertRate = row.RevertEstimate.Point
		}
		if row.Landed > 0 {
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

func wilson95(numerator, denominator int) RateEstimate {
	est := RateEstimate{Numerator: numerator, Denominator: denominator}
	if denominator <= 0 {
		return est
	}
	if numerator < 0 {
		numerator = 0
	}
	if numerator > denominator {
		numerator = denominator
	}
	const z = 1.959963984540054
	n := float64(denominator)
	p := float64(numerator) / n
	z2 := z * z
	center := p + z2/(2*n)
	margin := z * math.Sqrt((p*(1-p)+z2/(4*n))/n)
	denom := 1 + z2/n
	est.Point = finiteOrZero(p)
	est.WilsonLower = finiteOrZero((center - margin) / denom)
	est.WilsonUpper = finiteOrZero((center + margin) / denom)
	est.HasData = true
	return est
}

func finiteOrZero(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

func applyVariantSemantics(rows []ComparisonBreakdown, opts CompareOptions) []ExperimentSampleStatus {
	if len(opts.Experiments) == 0 {
		return nil
	}
	configured := configuredExperimentRoles(opts.Experiments)
	if len(configured) == 0 {
		return nil
	}
	byGroup := map[string][]int{}
	for i := range rows {
		row := &rows[i]
		if row.ExperimentID == "" || row.Role == "" || row.VariantID == "" {
			continue
		}
		gk := experimentRoleKey(row.ExperimentID, row.Role)
		byGroup[gk] = append(byGroup[gk], i)
		if _, ok := configured[gk]; !ok {
			configured[gk] = experimentRoleConfig{experimentID: row.ExperimentID, role: row.Role}
		}
	}
	keys := make([]string, 0, len(configured))
	for k := range configured {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	statuses := make([]ExperimentSampleStatus, 0, len(keys))
	for _, gk := range keys {
		cfg := configured[gk]
		indexes := byGroup[gk]
		rowByVariant := map[string]*ComparisonBreakdown{}
		for _, idx := range indexes {
			row := &rows[idx]
			row.MinSamplesPerVariant = opts.MinSamples
			row.BaselineVariantID = cfg.baselineVariantID
			if opts.MinSamples > 0 && row.Runs < opts.MinSamples {
				row.SampleStatus = "low-sample"
			} else {
				row.SampleStatus = "actionable"
			}
			rowByVariant[row.VariantID] = row
		}
		applyBaselineDeltas(rowByVariant, cfg.baselineVariantID)
		statuses = append(statuses, experimentSampleStatus(gk, cfg, rowByVariant, opts.MinSamples))
	}
	return statuses
}

type experimentRoleConfig struct {
	experimentID       string
	role               string
	baselineVariantID  string
	configuredVariants []string
}

func configuredExperimentRoles(experiments []abtest.Experiment) map[string]experimentRoleConfig {
	out := map[string]experimentRoleConfig{}
	for _, exp := range experiments {
		if exp.ID == "" || !exp.EnabledValue() || len(exp.Variants) == 0 {
			continue
		}
		baseline := exp.Variants[0].ID
		variants := make([]string, 0, len(exp.Variants))
		for _, v := range exp.Variants {
			if v.ID != "" {
				variants = append(variants, v.ID)
			}
		}
		for _, role := range exp.Roles {
			role = normalizedRole(role)
			out[experimentRoleKey(exp.ID, role)] = experimentRoleConfig{
				experimentID:       exp.ID,
				role:               role,
				baselineVariantID:  baseline,
				configuredVariants: variants,
			}
		}
	}
	return out
}

func experimentRoleKey(experimentID, role string) string {
	return experimentID + "|" + role
}

func applyBaselineDeltas(rows map[string]*ComparisonBreakdown, baselineVariantID string) {
	if baselineVariantID == "" {
		return
	}
	baseline := rows[baselineVariantID]
	if baseline == nil {
		return
	}
	baseline.Baseline = true
	applyDelta := func(est *RateEstimate, base RateEstimate) {
		if !est.HasData || !base.HasData {
			return
		}
		est.DeltaFromBaseline = est.Point - base.Point
		est.HasDelta = true
	}
	for _, row := range rows {
		applyDelta(&row.FailureEstimate, baseline.FailureEstimate)
		applyDelta(&row.LandedEstimate, baseline.LandedEstimate)
		applyDelta(&row.MergeEstimate, baseline.MergeEstimate)
		applyDelta(&row.CIFirstPassEstimate, baseline.CIFirstPassEstimate)
		applyDelta(&row.MergedWithEditsEstimate, baseline.MergedWithEditsEstimate)
		applyDelta(&row.ReworkEstimate, baseline.ReworkEstimate)
		applyDelta(&row.RevertEstimate, baseline.RevertEstimate)
	}
}

func experimentSampleStatus(key string, cfg experimentRoleConfig, rows map[string]*ComparisonBreakdown, minSamples int) ExperimentSampleStatus {
	variantIDs := orderedVariantIDs(cfg.configuredVariants, rows)
	status := ExperimentSampleStatus{
		Key:                  key,
		ExperimentID:         cfg.experimentID,
		Role:                 cfg.role,
		BaselineVariantID:    cfg.baselineVariantID,
		MinSamplesPerVariant: minSamples,
		Variants:             make([]VariantSampleStatus, 0, len(variantIDs)),
	}
	configured := map[string]bool{}
	for _, id := range cfg.configuredVariants {
		configured[id] = true
	}
	for _, id := range variantIDs {
		row := rows[id]
		runs := 0
		observed := false
		if row != nil {
			runs = row.Runs
			observed = true
		}
		ready := minSamples <= 0 || runs >= minSamples
		if ready {
			status.ReadyVariants++
		}
		status.TotalRuns += runs
		sampleStatus := "low-sample"
		if ready {
			sampleStatus = "actionable"
		}
		status.Variants = append(status.Variants, VariantSampleStatus{
			VariantID:    id,
			Runs:         runs,
			Ready:        ready,
			Configured:   configured[id],
			Observed:     observed,
			SampleStatus: sampleStatus,
		})
	}
	switch {
	case len(status.Variants) == 0 || status.TotalRuns == 0:
		status.Status = "no-data"
	case status.ReadyVariants == len(status.Variants):
		status.Status = "actionable"
	case status.ReadyVariants == 0:
		status.Status = "low-sample"
	default:
		status.Status = "directional"
	}
	return status
}

func orderedVariantIDs(configured []string, rows map[string]*ComparisonBreakdown) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(configured)+len(rows))
	for _, id := range configured {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	observed := make([]string, 0, len(rows))
	for id := range rows {
		if id != "" && !seen[id] {
			observed = append(observed, id)
		}
	}
	sort.Strings(observed)
	out = append(out, observed...)
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
