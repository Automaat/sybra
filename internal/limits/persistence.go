package limits

// Persistence is where quota snapshots and usage events survive a restart. Two implementations exist: the single JSON document and the database-backed SQLPersistence, selected by config.Database.Backend.
//
// Load and Save carry the whole set because that is what the file store always wrote: a snapshot dropped for age disappears by not being in the next Save, and the in-memory state is the authority between them.
type Persistence interface {
	Load() (map[string]Snapshot, []UsageEvent, error)
	Save(snapshots map[string]Snapshot, events []UsageEvent) error
}
