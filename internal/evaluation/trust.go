package evaluation

import (
	"fmt"
	"time"
)

// TrustworthyResult explains whether a Report is safe to drive automated
// experiment promotion/expansion from (internal/routing's adaptive weight
// service). A report can exist (Service.LastReport ok=true) while still
// being untrustworthy: computed under a schema version this build no longer
// agrees with, or stale enough that it no longer reflects current behavior.
type TrustworthyResult struct {
	Trustworthy bool
	// Reason is empty when Trustworthy, otherwise a short, log-safe
	// explanation (no prompts or task content).
	Reason string
}

// Trustworthy reports whether rep is safe to promote/expand experiment
// traffic from. Two checks, both fail-open on an unset/zero signal (mirrors
// minStatusOrNoEvidence in slo.go: absence of a signal is not proof of a
// breach) so a hand-built Report fixture with no SchemaVersion/GeneratedAt
// set is treated as compatible rather than spuriously rejected:
//
//   - SchemaVersion, when set, must match ScorecardSchemaVersion — a report
//     computed under a different metric semantics (see ScorecardSchemaVersion's
//     doc comment) must never drive a weight decision.
//   - GeneratedAt, when set and maxAge > 0, must be no older than maxAge —
//     an evaluation service that stopped ticking must not let routing keep
//     acting on an increasingly outdated signal.
func Trustworthy(rep Report, now time.Time, maxAge time.Duration) TrustworthyResult {
	if rep.SchemaVersion != 0 && rep.SchemaVersion != ScorecardSchemaVersion {
		return TrustworthyResult{Reason: fmt.Sprintf(
			"report schema_version %d does not match current schema_version %d",
			rep.SchemaVersion, ScorecardSchemaVersion,
		)}
	}
	if maxAge > 0 && !rep.GeneratedAt.IsZero() {
		if age := now.Sub(rep.GeneratedAt); age > maxAge {
			return TrustworthyResult{Reason: fmt.Sprintf(
				"report is stale: generated %s ago, max age %s",
				age.Round(time.Minute), maxAge,
			)}
		}
	}
	return TrustworthyResult{Trustworthy: true}
}
