package stats

import "time"

// Repository is the run-history surface reports read through. Two implementations exist: the line-per-record Store and the database-backed SQLStore, selected by config.Database.Backend.
//
// No context: runs are recorded from agent completion and read from the stats UI and the evaluation service, none of which carries one. SQLStore bounds each statement with its own deadline.
type Repository interface {
	Record(r RunRecord) error
	Len() int
	All() []RunRecord
	AllForTask(taskID string) []RunRecord
	Query() StatsResponse
	QueryAt(now time.Time) StatsResponse
}

var (
	_ Repository = (*Store)(nil)
	_ Repository = (*SQLStore)(nil)
)
