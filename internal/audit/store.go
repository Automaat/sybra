package audit

// Store is the audit trail's persistence surface. Two implementations exist: the per-day NDJSON Logger and the database-backed SQLStore, selected by config.Database.Backend.
//
// No context: events are logged from every corner of the app, most of which hold no context, and threading one through them is a change to those callers rather than to where the trail is kept. SQLStore bounds each statement with its own deadline.
type Store interface {
	Log(e Event) error
	Read(q Query) ([]Event, error)
	// Cleanup removes events past retentionDays. Zero or less keeps everything, matching the file trail.
	Cleanup(retentionDays int) error
	Close() error
}

var _ Store = (*Logger)(nil)
