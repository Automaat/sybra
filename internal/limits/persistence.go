package limits

// Persistence is where quota snapshots and usage events survive a restart. Two implementations exist: the single JSON document and the database-backed SQLPersistence, selected by config.Database.Backend.
//
// Load and Save carry the whole set because that is what the file store always wrote: a snapshot dropped for age disappears by not being in the next Save, and the in-memory state is the authority between them.
type Persistence interface {
	Load() (map[string]Snapshot, []UsageEvent, error)
	Save(snapshots map[string]Snapshot, events []UsageEvent) error
	// Critical runs a read-modify-write as one atomic unit against every other
	// writer, in this process and in any other sharing the board.
	//
	// It replaces the cross-process file lock, which existed for exactly this:
	// the desktop app and sybra-server each hold their own Store over one
	// board, and without it two updates interleave as load/load/save/save and
	// the second silently discards the first. Load and Save called inside fn
	// see the same transaction.
	Critical(fn func() error) error
}
