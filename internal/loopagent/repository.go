package loopagent

// Repository is the persistence surface the scheduler and the GUI service use.
// Two implementations exist: the per-file Store and the database-backed
// SQLStore, selected by config.Database.Backend.
type Repository interface {
	List() ([]LoopAgent, error)
	Get(id string) (LoopAgent, error)
	FindByName(name string) (LoopAgent, bool)
	Create(la LoopAgent) (LoopAgent, error)
	Update(la LoopAgent) (LoopAgent, error)
	UpdateRunMetadata(id string, mutate func(*LoopAgent)) (LoopAgent, error)
	Delete(id string) error
}

var (
	_ Repository = (*Store)(nil)
	_ Repository = (*SQLStore)(nil)
)
