package evaluation

import (
	"sort"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/task"
)

// Canonical lifecycle phases, in order. A phase groups one or more task statuses
// so a landed task's wall-clock can be decomposed into where the time went.
// "other" catches any status not mapped below, so a newly-added status is never
// silently dropped from the accounting.
const (
	PhaseQueued       = "queued"
	PhasePlanning     = "planning"
	PhaseImplementing = "implementing"
	PhaseTesting      = "testing"
	PhaseReview       = "review"
	PhaseWaiting      = "waiting"
	PhaseOther        = "other"
)

var phaseOrder = []string{
	PhaseQueued, PhasePlanning, PhaseImplementing,
	PhaseTesting, PhaseReview, PhaseWaiting, PhaseOther,
}

// phaseOf maps a task status (as recorded in task.status_changed audit events)
// to its lifecycle phase. Terminal statuses return "" — no duration accrues to
// them.
func phaseOf(status string) string {
	switch task.Status(status) {
	case "":
		return "" // missing status (e.g. first-observation from=="") — not a phase
	case task.StatusNew, task.StatusTodo:
		return PhaseQueued
	case task.StatusPlanning, task.StatusPlanReview:
		return PhasePlanning
	case task.StatusInProgress:
		return PhaseImplementing
	case task.StatusTesting:
		return PhaseTesting
	case task.StatusReadyReview, task.StatusInReview, task.StatusReadyPR:
		return PhaseReview
	case task.StatusBlocked, task.StatusHumanRequired:
		return PhaseWaiting
	case task.StatusDone, task.StatusCancelled:
		return ""
	default:
		return PhaseOther
	}
}

// PhaseStat aggregates one phase across the landed-task cohort. Percentiles and
// mean are over the tasks that actually entered the phase (Count), so a phase
// many tasks skip is not dragged toward zero; TotalH sums across the whole
// cohort for share-of-lead-time.
type PhaseStat struct {
	Phase  string  `json:"phase"`
	Count  int     `json:"count"`
	P50H   float64 `json:"p50h"`
	P90H   float64 `json:"p90h"`
	MeanH  float64 `json:"meanH"`
	TotalH float64 `json:"totalH"`
}

// TaskPhases is one landed task's lifecycle decomposition (≈ its lead time split
// by phase).
type TaskPhases struct {
	TaskID  string             `json:"taskId"`
	TotalH  float64            `json:"totalH"`
	ByPhase map[string]float64 `json:"byPhase"`
}

// PhaseReport is the per-phase lifecycle-duration breakdown over a window.
type PhaseReport struct {
	Since   time.Time    `json:"since"`
	Until   time.Time    `json:"until"`
	Cohort  int          `json:"cohort"` // landed tasks analyzed
	Phases  []PhaseStat  `json:"phases"`
	Slowest []TaskPhases `json:"slowest,omitempty"`
}

type statusEdge struct {
	ts   time.Time
	from string
	to   string
}

// ComputePhaseDurations decomposes the wall-clock of each task that landed in
// [since, until] into per-phase durations, reconstructed from its full
// task.status_changed history. events should span wider than [since, until] so a
// task that landed in-window but started earlier is fully reconstructed (the
// caller is expected to read a wider audit range). slowestN bounds the per-task
// detail list (0 = none).
//
// Pure and deterministic for a given input — no I/O.
func ComputePhaseDurations(events []audit.Event, since, until time.Time, slowestN int) PhaseReport {
	landed := landedInWindow(events, since, until)
	rep := PhaseReport{Since: since, Until: until, Cohort: len(landed)}

	// Gather creation + status-change timelines for cohort tasks only.
	created := map[string]time.Time{}
	transitions := map[string][]statusEdge{}
	for i := range events {
		e := events[i]
		if e.TaskID == "" {
			continue
		}
		if _, ok := landed[e.TaskID]; !ok {
			continue
		}
		switch e.Type {
		case audit.EventTaskCreated:
			if t, ok := created[e.TaskID]; !ok || e.Timestamp.Before(t) {
				created[e.TaskID] = e.Timestamp
			}
		case audit.EventTaskStatusChanged:
			transitions[e.TaskID] = append(transitions[e.TaskID], statusEdge{
				ts:   e.Timestamp,
				from: strVal(e.Data, "from"),
				to:   strVal(e.Data, "to"),
			})
		}
	}

	perPhaseSamples := map[string][]float64{}
	perPhaseTotal := map[string]float64{}
	details := make([]TaskPhases, 0, len(landed))
	for id, landedAt := range landed {
		byPhase := taskPhaseDurations(created[id], transitions[id], landedAt)
		if len(byPhase) == 0 {
			continue
		}
		var total float64
		for ph, h := range byPhase {
			perPhaseSamples[ph] = append(perPhaseSamples[ph], h)
			perPhaseTotal[ph] += h
			total += h
		}
		details = append(details, TaskPhases{TaskID: id, TotalH: total, ByPhase: byPhase})
	}

	for _, ph := range phaseOrder {
		s := perPhaseSamples[ph]
		if len(s) == 0 {
			continue
		}
		rep.Phases = append(rep.Phases, PhaseStat{
			Phase:  ph,
			Count:  len(s),
			P50H:   percentile(s, 50),
			P90H:   percentile(s, 90),
			MeanH:  mean(s),
			TotalH: perPhaseTotal[ph],
		})
	}

	sort.Slice(details, func(i, j int) bool {
		if details[i].TotalH != details[j].TotalH {
			return details[i].TotalH > details[j].TotalH
		}
		return details[i].TaskID < details[j].TaskID
	})
	switch {
	case slowestN <= 0:
		details = nil // contract: 0 (or negative) = no per-task detail
	case len(details) > slowestN:
		details = details[:slowestN]
	}
	rep.Slowest = details
	return rep
}

func landedInWindow(events []audit.Event, since, until time.Time) map[string]time.Time {
	landed := map[string]time.Time{}
	for i := range events {
		e := events[i]
		if e.Type != audit.EventTaskLanded || e.TaskID == "" {
			continue
		}
		if e.Timestamp.Before(since) || e.Timestamp.After(until) {
			continue
		}
		if t, ok := landed[e.TaskID]; !ok || e.Timestamp.Before(t) {
			landed[e.TaskID] = e.Timestamp // earliest landing wins (guard dup events)
		}
	}
	return landed
}

// taskPhaseDurations reconstructs how long one task spent in each phase, from
// creation through each status transition to landing. Segments with a
// non-positive span (clock skew, or transitions recorded after landing) are
// skipped rather than counted negative. Returns nil when there are no
// transitions to anchor a timeline.
func taskPhaseDurations(created time.Time, edges []statusEdge, landedAt time.Time) map[string]float64 {
	if len(edges) == 0 {
		return nil
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].ts.Before(edges[j].ts) })

	out := map[string]float64{}
	add := func(status string, start, end time.Time) {
		ph := phaseOf(status)
		if ph == "" {
			return
		}
		if d := end.Sub(start).Hours(); d > 0 {
			out[ph] += d
		}
	}

	// Anchor the first segment at creation when known (counts the initial dwell
	// in edges[0].from); otherwise start at the first transition (initial dwell
	// is unknown, not zero).
	prevStatus := edges[0].from
	prevTime := edges[0].ts
	if !created.IsZero() && !created.After(edges[0].ts) {
		prevTime = created
	}
	for i := range edges {
		add(prevStatus, prevTime, edges[i].ts)
		prevStatus = edges[i].to
		prevTime = edges[i].ts
	}
	add(prevStatus, prevTime, landedAt) // final open segment runs to landing
	return out
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}
