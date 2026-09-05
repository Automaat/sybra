package experience

// Repository is the advisory-memory surface triage and planning read through. Two implementations exist: the per-project-directory Store and the database-backed SQLStore, selected by config.Database.Backend.
//
// These methods take no context. Records are read while assembling an agent's prompt and written from the review handler's landing path, neither of which carries one, and threading a context through those callers is a change to them rather than to where records are stored. SQLStore bounds each statement with its own deadline instead, so a stalled backend cannot hold a dispatch open indefinitely.
type Repository interface {
	Put(projectID string, rec Record) error
	Query(projectID string, limit int) ([]Record, error)
	Delete(projectID string) error
}

var (
	_ Repository = (*Store)(nil)
	_ Repository = (*SQLStore)(nil)
)
