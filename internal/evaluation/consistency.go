package evaluation

import "github.com/Automaat/sybra/internal/health"

// Discrepancy is one field where a Scorecard and a health.Stats disagree
// despite both being derived from the same audit event stream via the same
// runacct.Count(..., CountsTowardCodeAuthorFailureRate) accounting — see
// scanReliability (this package) and health.buildStats. A non-empty
// Discrepancy slice means the two subsystems' run/failure accounting has
// drifted apart: either a bug in one of the two call sites, or a real
// inconsistency an operator needs to see before trusting either report.
type Discrepancy struct {
	Field     string  `json:"field"`
	Scorecard float64 `json:"scorecard"`
	Health    float64 `json:"health"`
}

// ReconcileReports compares run totals and failure/stall outcomes between a
// Scorecard and a health.Stats computed over the same audit event window.
// Pure: no I/O. Returns nil when the two agree exactly on every field.
func ReconcileReports(sc Scorecard, hs health.Stats) []Discrepancy {
	var out []Discrepancy
	check := func(field string, scVal, hsVal int) {
		if scVal != hsVal {
			out = append(out, Discrepancy{Field: field, Scorecard: float64(scVal), Health: float64(hsVal)})
		}
	}
	check("agent_runs", sc.AgentRuns, hs.TotalAgentRuns)
	check("agent_failures", sc.AgentFailures, hs.FailedAgentRuns)
	check("agent_resolved_runs", sc.AgentResolvedRuns, hs.ResolvedRuns)
	check("agent_stalls", sc.AgentStalls, hs.StalledRuns)
	return out
}
