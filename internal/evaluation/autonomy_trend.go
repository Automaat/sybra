package evaluation

import (
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/stats"
)

// autonomyTrendWeeks is the default number of weekly buckets in AutonomyTrend
// — roughly a quarter, enough to see a trend without an unreadable chart.
const autonomyTrendWeeks = 12

// autonomyAllTimeAnchor is the "since the beginning of Sybra" bound for
// all-time autonomy, matching the existing all-history read convention (see
// internal/sybra/svc_stats.go's auditDoneTaskClosures).
var autonomyAllTimeAnchor = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// AutonomySnapshot is autonomy over one arbitrary [Since, Until] window.
type AutonomySnapshot struct {
	Since              time.Time `json:"since"`
	Until              time.Time `json:"until"`
	TasksLanded        int       `json:"tasksLanded"`
	AutonomousLandings int       `json:"autonomousLandings"`
	AutonomyRate       float64   `json:"autonomyRate"`
}

// AutonomyWeekPoint is autonomy over one 7-day bucket. WeekEnd is exclusive
// for every bucket except the newest (the one ending at "now"): consecutive
// buckets share a boundary instant, so treat [WeekStart, WeekEnd) as the
// bucket's true range rather than reading WeekEnd as an inclusive endpoint —
// see ComputeAutonomyTrend's nanosecond trim.
type AutonomyWeekPoint struct {
	WeekStart          time.Time `json:"weekStart"`
	WeekEnd            time.Time `json:"weekEnd"`
	TasksLanded        int       `json:"tasksLanded"`
	AutonomousLandings int       `json:"autonomousLandings"`
	AutonomyRate       float64   `json:"autonomyRate"`
}

// AutonomyTrend holds all-time / last-week / last-month autonomy snapshots
// plus a week-by-week series, so the Evaluation tab can show how autonomy has
// moved over time instead of only the current rolling scorecard window.
type AutonomyTrend struct {
	GeneratedAt time.Time           `json:"generatedAt"`
	Overall     AutonomySnapshot    `json:"overall"`
	LastWeek    AutonomySnapshot    `json:"lastWeek"`
	LastMonth   AutonomySnapshot    `json:"lastMonth"`
	Weekly      []AutonomyWeekPoint `json:"weekly"`
}

// ComputeAutonomyTrend is pure — no I/O, deterministic for given input — same
// contract as Compute, which it calls repeatedly over different windows.
// Records/events outside a given window are ignored by Compute itself, so
// callers pass the full retained history once and this slices it. Each
// snapshot and weekly bucket is computed independently (not a cumulative
// running rate), so every rate is honest for its own window.
//
// Scans task signals once and reuses them across every window (via
// computeWithSignals) rather than calling Compute directly, which would
// rescan the full event history — including the not-window-bound
// scanTaskSignals — on each of the (weeks+3) calls this makes.
func ComputeAutonomyTrend(records []stats.RunRecord, events []audit.Event, now time.Time, weeks int) AutonomyTrend {
	sigs := scanTaskSignals(events)
	snapshot := func(since time.Time) AutonomySnapshot {
		sc := computeWithSignals(records, events, sigs, since, now)
		return AutonomySnapshot{
			Since: since, Until: now,
			TasksLanded: sc.TasksLanded, AutonomousLandings: sc.AutonomousLandings,
			AutonomyRate: sc.AutonomyRate,
		}
	}
	if weeks <= 0 {
		weeks = autonomyTrendWeeks
	}
	weekly := make([]AutonomyWeekPoint, 0, weeks)
	for i := weeks - 1; i >= 0; i-- {
		weekEnd := now.AddDate(0, 0, -7*i)
		weekStart := weekEnd.AddDate(0, 0, -7)
		// weekEnd of this bucket equals weekStart of the next, and Compute's
		// window is inclusive on both ends — trim a nanosecond so a landing on
		// that exact instant isn't double-counted into both buckets.
		until := weekEnd
		if i > 0 {
			until = until.Add(-time.Nanosecond)
		}
		sc := computeWithSignals(records, events, sigs, weekStart, until)
		weekly = append(weekly, AutonomyWeekPoint{
			WeekStart: weekStart, WeekEnd: weekEnd,
			TasksLanded: sc.TasksLanded, AutonomousLandings: sc.AutonomousLandings,
			AutonomyRate: sc.AutonomyRate,
		})
	}
	return AutonomyTrend{
		GeneratedAt: now,
		Overall:     snapshot(autonomyAllTimeAnchor),
		LastWeek:    snapshot(now.AddDate(0, 0, -7)),
		LastMonth:   snapshot(now.AddDate(0, 0, -30)),
		Weekly:      weekly,
	}
}
