package health

import (
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/sandbox"
)

// checkSandboxCleanupFailures flags each currently quarantined sandbox
// cleanup failure (see sandbox.Manager.QuarantinedEntries) — a removal that
// survived ownership normalization and the transient-busy retry budget, and
// is therefore genuinely unsafe rather than transient. One finding per task
// id, deduplicated across ticks by FingerprintFor since it keys per-task
// findings on (Category, TaskID).
func checkSandboxCleanupFailures(entries []sandbox.QuarantineEntry, now time.Time) []Finding {
	findings := make([]Finding, 0, len(entries))
	for _, e := range entries {
		findings = append(findings, Finding{
			Category:    CatSandboxCleanup,
			Severity:    SeverityCritical,
			Title:       fmt.Sprintf("sandbox cleanup failed for task %s (%d bytes retained)", e.TaskID, e.BytesRetained),
			Description: fmt.Sprintf("Sandbox dir %s for task %s could not be removed after %d attempt(s): %s", e.Path, e.TaskID, e.Attempts, e.LastError),
			TaskID:      e.TaskID,
			Evidence: map[string]any{
				"path":            e.Path,
				"bytes_retained":  e.BytesRetained,
				"attempts":        e.Attempts,
				"last_error":      e.LastError,
				"first_failed_at": e.FirstFailedAt,
				"last_failed_at":  e.LastFailedAt,
			},
			DetectedAt: now,
		})
	}
	return findings
}
