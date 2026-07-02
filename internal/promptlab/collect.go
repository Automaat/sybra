package promptlab

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/Automaat/sybra/internal/evaluation"
	"github.com/Automaat/sybra/internal/stats"
)

// loadEvaluationReport reads the persisted fleet scorecard. A missing file
// (evaluation service hasn't ticked yet) is treated as "no evidence yet" —
// not an error — so a fresh install can run the loop without crashing. A
// present-but-corrupt file is a genuine error: it would be wrong to treat
// unparsable data as "nothing wrong".
func loadEvaluationReport(path string) (evaluation.Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return evaluation.Report{}, nil
		}
		return evaluation.Report{}, err
	}
	var report evaluation.Report
	if err := json.Unmarshal(data, &report); err != nil {
		return evaluation.Report{}, fmt.Errorf("parse evaluation report: %w", err)
	}
	return report, nil
}

// CollectWeakSubjects derives per-role WeakSubjects from an evaluation report
// and supporting run records. A role is only surfaced when it clears BOTH
// gates — MinSamples (enough runs to trust the number) and MinEffectSize
// (the failure rate is actually worse than the fleet baseline, not noise) —
// so a single unlucky run never triggers a proposal. Results are sorted by
// effect size, worst first.
func CollectWeakSubjects(report evaluation.Report, records []stats.RunRecord, minSamples int, minEffectSize float64) []WeakSubject {
	var out []WeakSubject
	for _, b := range report.ByRole {
		if b.Key == "" || b.Runs < minSamples {
			continue
		}
		effect := b.FailureRate - report.Overall.FailureRate
		if effect < minEffectSize {
			continue
		}
		out = append(out, WeakSubject{
			Subject: Subject{Role: b.Key},
			Metric:  "failure_rate",
			Detail: fmt.Sprintf("role %s fails %.0f%% vs %.0f%% overall over %d runs",
				b.Key, b.FailureRate*100, report.Overall.FailureRate*100, b.Runs),
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

// taskIdsForRole returns up to limit distinct, sorted task IDs so a reviewer
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
