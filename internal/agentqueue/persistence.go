package agentqueue

import "log/slog"

// Persistence is the queue's durability mirror. Two implementations exist: the
// per-item YAML files and the database-backed SQLStore, selected by
// config.Database.Backend.
//
// Every method is best-effort by contract: the in-memory queue is the
// authority and a store failure is logged rather than failing the operation it
// mirrors. Dispatch ordering does not depend on the order load returns — the
// queue sorts by the persisted fields — so what has to survive a restart is
// the item, not the sequence it was read in.
type Persistence interface {
	put(it Item) error
	del(taskID string) error
	load(log *slog.Logger) []Item
}

var _ Persistence = (*store)(nil)
