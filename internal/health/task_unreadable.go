package health

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

// checkUnreadableTasks reports one finding per task file that failed to
// parse. Store.List surfaces those as degraded, non-dispatchable board
// entries (task.Task.Degraded) rather than dropping them, but a degraded
// entry is only visible to someone looking at the board — every List-based
// sweep (recovery, monitor, umbrella, mirror) still skips it, so the task
// stays stalled until a human repairs the file on disk. Critical severity
// rather than a warning for that reason.
//
// The finding's TaskID is the degraded entry's synthetic, filename-derived
// ID, so the fingerprint stays stable across ticks while the file is broken
// (and the parse error text keeps changing).
func checkUnreadableTasks(tasks []task.Task, now time.Time) []Finding {
	var findings []Finding
	for i := range tasks {
		t := &tasks[i]
		if !t.Degraded {
			continue
		}
		file := filepath.Base(t.FilePath)
		findings = append(findings, Finding{
			Category:    CatTaskUnreadable,
			Severity:    SeverityCritical,
			Title:       fmt.Sprintf("task file %s cannot be parsed", file),
			Description: fmt.Sprintf("%s failed to parse and is skipped by every task sweep until it is repaired on disk: %s", file, t.ParseError),
			TaskID:      t.ID,
			Evidence: map[string]any{
				"file":   file,
				"reason": t.ParseError,
			},
			DetectedAt: now,
		})
	}
	return findings
}
