package toolledger

// Store is where tool-call records are kept. Two implementations exist: the per-day NDJSON Logger and the database-backed SQLStore, selected by config.Database.Backend.
//
// No context: records are written from the provider stream and the approval hook, neither of which carries one. SQLStore bounds each statement with its own deadline.
type Store interface {
	Log(r Record) error
	Close() error
}

var _ Store = (*Logger)(nil)
