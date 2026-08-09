package cleanup

// ProtectedPersistence holds the protected-findings ledger. Two
// implementations exist: the single JSON document and the database-backed
// SQLProtectedStore, selected by config.Database.Backend.
//
// Lock acquires the exclusive hold a read-modify-write needs and returns its
// release, which is the shape the store already had. Every mutating method is
// one such cycle, and losing one loses the record that a path is protected —
// after which a cleanup pass is free to delete it.
type ProtectedPersistence interface {
	Lock() (release func(), err error)
	Read() (protectedFile, error)
	Write(rec protectedFile) error
}

var _ ProtectedPersistence = (*protectedFiles)(nil)
