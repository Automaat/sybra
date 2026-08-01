package health

import (
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

// checkQuarantinedTasks reports one finding per task file that failed to
// parse and was moved to the store's quarantine dir (see
// task.Store.QuarantinedTasks) — each one is a task that silently vanished
// from every List-based sweep until quarantined, so this check is critical
// severity rather than a warning.
func checkQuarantinedTasks(entries []task.QuarantineEntry, now time.Time) []Finding {
	findings := make([]Finding, 0, len(entries))
	for _, e := range entries {
		findings = append(findings, Finding{
			Category:    CatTaskQuarantine,
			Severity:    SeverityCritical,
			Title:       fmt.Sprintf("task file %s quarantined (failed to parse)", e.File),
			Description: fmt.Sprintf("%s could not be parsed and was moved to the quarantine dir: %s", e.File, e.Reason),
			TaskID:      e.File,
			Evidence: map[string]any{
				"file":           e.File,
				"reason":         e.Reason,
				"quarantined_at": e.QuarantinedAt,
			},
			DetectedAt: now,
		})
	}
	return findings
}
