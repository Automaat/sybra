package sybra

import (
	"time"

	"github.com/Automaat/sybra/internal/audit"
)

// The queue captures at under its lock, then calls us after unlocking. Preserve
// that boundary even if a slow audit store delays or reorders delivery.
func (a *App) observeQueueBoundary(taskID string, enqueued, at time.Time, state string) {
	if a.audit == nil {
		return
	}
	err := a.audit.Log(audit.Event{Type: audit.EventFactoryQueue, TaskID: taskID, Timestamp: at, Data: map[string]any{
		"interval_key": audit.FactoryIntervalKey(taskID, enqueued.UTC().Format(time.RFC3339Nano)), "state": state,
	}})
	if err != nil {
		a.logger.Error("audit.log", "type", audit.EventFactoryQueue, "err", err)
	}
}
