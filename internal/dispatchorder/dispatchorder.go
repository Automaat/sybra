// Package dispatchorder ranks a task's pipeline status for dispatch ordering.
// It is a dependency-free leaf shared by internal/workflow and
// internal/agentqueue so the mapping cannot drift between two hand-copied
// versions. It keys on a raw string so it stays dependency-free; callers
// holding a taskstatus.Status convert at the call. The cycle that once forced
// this is gone — internal/taskstatus imports only fmt — so a future change may
// take the typed value directly.
package dispatchorder

import "github.com/Automaat/sybra/internal/taskstatus"

// Rank ranks a task's pipeline status for dispatch ordering: lower ranks are
// dispatched sooner. Unknown statuses rank last (4).
func Rank(status string) int {
	switch status {
	case string(taskstatus.InReview), string(taskstatus.ReadyPR):
		return 0
	case string(taskstatus.ReadyReview), string(taskstatus.Testing):
		return 1
	case string(taskstatus.InProgress):
		return 2
	case string(taskstatus.Planning), string(taskstatus.PlanReview):
		return 3
	default:
		return 4
	}
}
