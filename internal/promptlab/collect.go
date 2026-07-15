package promptlab

import (
	"fmt"
	"sort"
	"time"

	"github.com/Automaat/sybra/internal/evaluation"
	"github.com/Automaat/sybra/internal/stats"
)

// unboundedSince/unboundedUntil span effectively all time. CollectWeakSubjects
// receives records already scoped by the caller's lookback window (see
// withinLookback); passing an all-time window into evaluation.BreakdownBy and
// evaluation.Compute means the lookback trim is the only one that applies —
// there is no second, independent window to fall out of sync with it.
var (
	unboundedSince time.Time
	unboundedUntil = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
)

// CollectWeakSubjects derives per-role WeakSubjects directly from run
// records — not a persisted evaluation report — so the same lookback-
// filtered evidence backs both the gating metrics (samples, failure rate,
// effect size vs the fleet baseline) and the representative project/task
// attribution. A role is only surfaced when it clears BOTH gates —
// MinSamples (enough runs to trust the number) and MinEffectSize (the
// failure rate is actually worse than the fleet baseline, not noise) — so a
// single unlucky run never triggers a proposal. Results are sorted by effect
// size, worst first.
func CollectWeakSubjects(records []stats.RunRecord, minSamples int, minEffectSize float64) []WeakSubject {
	overall := evaluation.Compute(records, nil, unboundedSince, unboundedUntil)
	byRole := evaluation.BreakdownBy(records, unboundedSince, unboundedUntil, func(r stats.RunRecord) string {
		return normalizedRole(r.Role)
	})

	var out []WeakSubject
	for _, b := range byRole {
		if b.Runs < minSamples {
			continue
		}
		effect := b.FailureRate - overall.FailureRate
		if effect < minEffectSize {
			continue
		}
		out = append(out, WeakSubject{
			Subject: Subject{Role: b.Key},
			Metric:  "failure_rate",
			Detail: fmt.Sprintf("role %s fails %.0f%% vs %.0f%% overall over %d runs",
				b.Key, b.FailureRate*100, overall.FailureRate*100, b.Runs),
			Samples:    b.Runs,
			EffectSize: effect,
			ProjectIDs: projectIDsForRole(records, b.Key),
			TaskIDs:    taskIDsForRole(records, b.Key, 5),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].EffectSize > out[j].EffectSize })
	return out
}

// projectIDsForRole returns the distinct, sorted project IDs behind a role's
// run records — the originating context a filer needs to resolve a
// work-typed scrub blocklist before persisting a proposal derived from this
// evidence.
func projectIDsForRole(records []stats.RunRecord, role string) []string {
	seen := map[string]bool{}
	var out []string
	for i := range records {
		r := &records[i]
		if normalizedRole(r.Role) != role || r.ProjectID == "" || seen[r.ProjectID] {
			continue
		}
		seen[r.ProjectID] = true
		out = append(out, r.ProjectID)
	}
	sort.Strings(out)
	return out
}

// taskIDsForRole returns up to limit distinct, sorted task IDs so a reviewer
// can jump straight to representative failing runs.
func taskIDsForRole(records []stats.RunRecord, role string, limit int) []string {
	seen := map[string]bool{}
	var out []string
	for i := range records {
		r := &records[i]
		if normalizedRole(r.Role) != role || r.TaskID == "" || seen[r.TaskID] {
			continue
		}
		seen[r.TaskID] = true
		out = append(out, r.TaskID)
	}
	sort.Strings(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func normalizedRole(role string) string {
	if role == "" {
		return "implementation"
	}
	return role
}

// withinLookback filters records to those at or after now-lookback. Zero
// lookback disables the filter (all history considered).
func withinLookback(records []stats.RunRecord, lookback time.Duration, now time.Time) []stats.RunRecord {
	if lookback <= 0 {
		return records
	}
	since := now.Add(-lookback)
	out := make([]stats.RunRecord, 0, len(records))
	for i := range records {
		r := &records[i]
		if !r.Timestamp.Before(since) {
			out = append(out, *r)
		}
	}
	return out
}
