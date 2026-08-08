package loopagent

import "context"

// Repository is the persistence surface the scheduler and the GUI service use.
// Two implementations exist: the per-file Store and the database-backed
// SQLStore, selected by config.Database.Backend.
//
// Every method takes a context because the database backend can be a server on
// another host: without one, a stalled connection has no deadline and no
// cancellation path, and shutdown blocks on it.
type Repository interface {
	List(ctx context.Context) ([]LoopAgent, error)
	Get(ctx context.Context, id string) (LoopAgent, error)
	FindByName(ctx context.Context, name string) (LoopAgent, bool)
	Create(ctx context.Context, la LoopAgent) (LoopAgent, error)
	CreateIfAbsentByName(ctx context.Context, la LoopAgent) (LoopAgent, bool, error)
	Update(ctx context.Context, la LoopAgent) (LoopAgent, error)
	UpdateRunMetadata(ctx context.Context, id string, mutate func(*LoopAgent)) (LoopAgent, error)
	Delete(ctx context.Context, id string) error
}

var (
	_ Repository = (*Store)(nil)
	_ Repository = (*SQLStore)(nil)
)
