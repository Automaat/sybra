package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/Automaat/sybra/internal/runacct"
	"github.com/Automaat/sybra/internal/runoutcome"
	"github.com/Automaat/sybra/internal/taskstatus"
	"github.com/Automaat/sybra/internal/version"
)

const (
	EventFactoryQueue          = "factory.queue"
	EventFactoryCIWait         = "factory.ci.waiting"
	EventFactoryCIVerified     = "factory.ci.verified"
	EventFactoryReleaseStarted = "factory.release.started"
	FactoryMaxWindow           = 31 * 24 * time.Hour
	FactoryMaxEvents           = 250000
	FactoryMaxBytes            = 32 << 20
)

type FactoryQuery struct {
	Since   time.Time `json:"since"`
	Until   time.Time `json:"until"`
	Release string    `json:"release,omitempty"`
}

func (q FactoryQuery) Validate() error {
	if q.Since.IsZero() || q.Until.IsZero() || !q.Until.After(q.Since) || q.Until.Sub(q.Since) > FactoryMaxWindow {
		return fmt.Errorf("factory report requires a positive window of at most 31 days")
	}
	if q.Release != "" && q.Release != "unknown" && q.Release != "mixed" && !version.ValidRevision(q.Release) {
		return fmt.Errorf("release must be a full revision, unknown, or mixed")
	}
	return nil
}

type FactoryPhase struct {
	Samples       int      `json:"samples"`
	MedianSeconds *float64 `json:"median_seconds"`
	P95Seconds    *float64 `json:"p95_seconds"`
	Open          int      `json:"open"`
	Unknown       int      `json:"unknown"`
	Censored      int      `json:"censored"`
	Unavailable   bool     `json:"unavailable"`
}

type FactoryReport struct {
	Window                       FactoryQuery            `json:"window"`
	Events                       int                     `json:"events"`
	Releases                     map[string]int          `json:"release_event_counts"`
	Phases                       map[string]FactoryPhase `json:"phases"`
	AgentRuns                    int                     `json:"agent_runs"`
	ResolvedRuns                 int                     `json:"resolved_runs"`
	FailedRuns                   int                     `json:"failed_runs"`
	StalledRuns                  int                     `json:"stalled_runs"`
	UnknownRuns                  int                     `json:"unknown_runs"`
	RetriesAfterFailure          int                     `json:"retries_after_failure"`
	UniqueCompletedTasks         int                     `json:"unique_completed_tasks"`
	ReopenedTasks                int                     `json:"reopened_tasks"`
	ObservedCostUSD              float64                 `json:"observed_cost_usd"`
	UnknownCostRuns              int                     `json:"unknown_cost_runs"`
	CompletedTaskWindowCostUSD   float64                 `json:"completed_task_window_cost_usd"`
	CompletedTaskUnknownCostRuns int                     `json:"completed_task_unknown_cost_runs"`
	Notes                        []string                `json:"notes"`
}

// FactoryIntervalKey binds a telemetry interval without recording repository
// identifiers, work commit SHAs, prompts, or task bodies in its payload.
func FactoryIntervalKey(identity, generation string) string {
	digest := sha256.Sum256([]byte(identity + "\x00" + generation))
	return hex.EncodeToString(digest[:])
}

// SummarizeFactory reduces the canonical audit stream, not provider log text.
// Windows are half-open [since, until). Partial intervals never become zeroes.
func SummarizeFactory(events []Event, q FactoryQuery) (FactoryReport, error) {
	if err := q.Validate(); err != nil {
		return FactoryReport{}, err
	}
	selected := make([]Event, 0, min(len(events), FactoryMaxEvents+1))
	for _, e := range events {
		if !e.Timestamp.Before(q.Since) && e.Timestamp.Before(q.Until) {
			selected = append(selected, e)
		}
		if len(selected) > FactoryMaxEvents {
			break
		}
	}
	if len(selected) > FactoryMaxEvents {
		return FactoryReport{}, fmt.Errorf("factory report exceeds %d events; narrow the window", FactoryMaxEvents)
	}
	ordered := factoryEvents(selected, q)
	r := FactoryReport{Window: q, Events: len(ordered), Releases: map[string]int{}, Phases: map[string]FactoryPhase{}, Notes: []string{
		"Nearest-rank p95; median averages the two middle observations. Null means no complete samples, not zero latency.",
		"Queue is observed admission-queue residence; removals without dequeue are censored. CI is observed PR-monitor blocking wait, not GitHub job runtime; early CI may finish during review.",
		"Deployment is candidate first seen to the target leader build starting. Worker activation coverage is unavailable in this stream. Release labels are leader builds, never inferred historical worker versions.",
		"Costs cover canonical terminal runs observed inside this window only, not lifetime task costs. Reopened tasks are unique IDs; completed counts require the final observed status to be done.",
		"Open means no observed end in this window, not proof of a currently running process. Missing starts and old telemetry remain unknown. No task/project content is included.",
	}}
	for _, e := range ordered {
		r.Releases[factoryRelease(e.Release)]++
	}
	if len(r.Releases) > 128 {
		return FactoryReport{}, fmt.Errorf("too many releases; narrow the window")
	}
	for _, phase := range []string{"queue", "ci", "deploy"} {
		r.Phases[phase] = factoryIntervals(ordered, phase, q.Release)
	}
	completed := factoryCompleted(ordered, q.Release, &r)
	factoryRuns(ordered, q.Release, completed, &r)
	return r, nil
}

func factoryEvents(events []Event, q FactoryQuery) []Event {
	out := make([]Event, 0, len(events))
	seen := map[string]bool{}
	for _, e := range events {
		if e.Timestamp.Before(q.Since) || !e.Timestamp.Before(q.Until) {
			continue
		}
		id := EventID(e)
		if !seen[id] {
			out = append(out, e)
			seen[id] = true
		}
	}
	slices.SortStableFunc(out, func(a, b Event) int { return a.Timestamp.Compare(b.Timestamp) })
	return out
}

func factoryRelease(release string) string {
	if version.ValidRevision(release) {
		return release
	}
	return "unknown"
}

func pairRelease(start, end Event) string {
	if start.Type == "" {
		return factoryRelease(end.Release)
	}
	if end.Type == "" {
		return factoryRelease(start.Release)
	}
	a, b := factoryRelease(start.Release), factoryRelease(end.Release)
	if a == "unknown" || b == "unknown" {
		return "unknown"
	}
	if a != b {
		return "mixed"
	}
	return a
}

func releaseMatches(filter, release string) bool { return filter == "" || filter == release }

type factorySamples struct {
	values                  []float64
	open, unknown, censored int
}

func (s *factorySamples) add(start, end time.Time, censored bool) {
	switch {
	case censored:
		s.censored++
	case start.IsZero():
		s.unknown++
	case end.IsZero():
		s.open++
	case end.Before(start):
		s.unknown++
	default:
		s.values = append(s.values, end.Sub(start).Seconds())
	}
}

func (s *factorySamples) report() FactoryPhase {
	r := FactoryPhase{Samples: len(s.values), Open: s.open, Unknown: s.unknown, Censored: s.censored,
		Unavailable: len(s.values)+s.open+s.unknown+s.censored == 0}
	if len(s.values) == 0 {
		return r
	}
	slices.Sort(s.values)
	n := len(s.values)
	median := s.values[n/2]
	if n%2 == 0 {
		median = (s.values[n/2-1] + median) / 2
	}
	p95 := s.values[int(math.Ceil(float64(n)*0.95))-1]
	r.MedianSeconds, r.P95Seconds = &median, &p95
	return r
}

func factoryCompleted(events []Event, release string, report *FactoryReport) map[string]bool {
	last := map[string]Event{}
	reopened := map[string]bool{}
	for _, e := range events {
		if e.Type != EventTaskStatusChanged || e.TaskID == "" {
			continue
		}
		from, _ := e.Data["from"].(string)
		to, _ := e.Data["to"].(string)
		if from == to || to == "" {
			continue
		}
		last[e.TaskID] = e
		if from == string(taskstatus.Done) && releaseMatches(release, factoryRelease(e.Release)) {
			reopened[e.TaskID] = true
		}
	}
	done := map[string]bool{}
	for id, e := range last {
		if e.Data["to"] == string(taskstatus.Done) && releaseMatches(release, factoryRelease(e.Release)) {
			done[id] = true
		}
	}
	report.UniqueCompletedTasks, report.ReopenedTasks = len(done), len(reopened)
	return done
}

func factoryRuns(events []Event, release string, completed map[string]bool, report *FactoryReport) {
	starts := map[string]Event{}
	for _, e := range events {
		if e.Type == EventAgentStarted {
			if _, ok := starts[e.AgentID]; !ok {
				starts[e.AgentID] = e
			}
		}
	}
	runs := NormalizeAgentRuns(events)
	slices.SortStableFunc(runs, func(a, b RunLifecycle) int { return a.StartedAt.Compare(b.StartedAt) })
	var samples factorySamples
	var selected []Event
	for i := range runs {
		run := &runs[i]
		start := starts[run.AgentID]
		matched := releaseMatches(release, pairRelease(start, run.TerminalEvent))
		if matched {
			report.AgentRuns++
			samples.add(run.StartedAt, run.TerminalAt, false)
			if run.Terminal {
				selected = append(selected, run.TerminalEvent)
				factoryCost(run.TerminalEvent, completed, report)
			}
		}
	}
	report.RetriesAfterFailure = factoryRetries(runs, starts, release)
	counts := runacct.Count(RunRecords(selected), nil, runacct.CountConfig{})
	report.ResolvedRuns, report.FailedRuns = counts.Resolved, counts.Failures
	report.StalledRuns, report.UnknownRuns = counts.Stalled, counts.Unknown
	report.Phases["agent"] = samples.report()
}

func factoryRetries(runs []RunLifecycle, starts map[string]Event, release string) int {
	type boundary struct {
		at    time.Time
		run   *RunLifecycle
		start bool
	}
	var boundaries []boundary
	for i := range runs {
		run := &runs[i]
		if run.TaskID == "" || run.AgentID == "" {
			continue
		}
		if run.Started {
			boundaries = append(boundaries, boundary{run.StartedAt, run, true})
		}
		if run.Terminal {
			boundaries = append(boundaries, boundary{run.TerminalAt, run, false})
		}
	}
	slices.SortStableFunc(boundaries, func(a, b boundary) int { return a.at.Compare(b.at) })
	lastOutcome := map[string]string{}
	count := 0
	for i := range boundaries {
		b := &boundaries[i]
		start := starts[b.run.AgentID]
		role, _ := b.run.TerminalEvent.Data["role"].(string)
		if role == "" {
			role, _ = start.Data["role"].(string)
		}
		key := b.run.TaskID + "\x00" + role
		if b.start {
			if lastOutcome[key] == runoutcome.Failed && releaseMatches(release, pairRelease(start, b.run.TerminalEvent)) {
				count++
			}
		} else {
			lastOutcome[key] = b.run.Outcome
		}
	}
	return count
}

func factoryCost(e Event, completed map[string]bool, report *FactoryReport) {
	cost, ok := e.Data["cost_usd"].(float64)
	if !ok || cost < 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
		report.UnknownCostRuns++
		if completed[e.TaskID] {
			report.CompletedTaskUnknownCostRuns++
		}
		return
	}
	report.ObservedCostUSD += cost
	if completed[e.TaskID] {
		report.CompletedTaskWindowCostUSD += cost
	}
}
