package workflow

// Repository is the definition and snapshot surface the engine reads through.
// Two implementations exist: the per-file Store and the database-backed
// SQLStore, selected by config.Database.Backend.
//
// Unlike the other repositories moved to the database, these methods take no
// context. The engine calls them from step evaluation and dispatch, neither of
// which carries one, and threading a context through the engine is a change to
// the engine rather than to where definitions are stored. SQLStore therefore
// bounds each query with its own deadline — see NewSQLStore — so a stalled
// backend cannot hold shutdown open indefinitely. Give this interface a
// context the day the engine has one to give.
type Repository interface {
	List() ([]Definition, error)
	Get(id string) (Definition, error)
	Save(def Definition) error
	Delete(id string) error
	SaveSnapshot(def Definition) (string, error)
	GetSnapshot(workflowID, hash string) (Definition, error)
	// Dir is the directory definitions live in, or "" for a backend that has
	// none. Callers that show an operator where to edit a workflow by hand use
	// it, and must handle the empty case rather than printing a bare path.
	Dir() string
}

var (
	_ Repository = (*Store)(nil)
	_ Repository = (*SQLStore)(nil)
)
