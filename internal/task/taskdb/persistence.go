package taskdb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

// taskCallTimeout bounds a single Persistence call. Manager's callers carry
// no context of their own — the same reasoning taskQueryTimeout documents
// for every other unbounded caller into this package.
const taskCallTimeout = taskQueryTimeout

// Persistence adapts SQLStore to task.Persistence, so Manager can run
// against either backend through one interface. Defined here rather than in
// package task to avoid an import cycle: SQLStore already depends on task
// for Task/MarshalStored/ParseBytes, so task cannot import taskdb back.
type Persistence struct {
	store *SQLStore
}

// NewPersistence wraps store as a task.Persistence.
func NewPersistence(store *SQLStore) *Persistence {
	return &Persistence{store: store}
}

func (p *Persistence) Get(id string) (task.Task, error) {
	ctx, cancel := context.WithTimeout(context.Background(), taskCallTimeout)
	defer cancel()
	t, sidecars, err := p.store.Get(ctx, id)
	if err != nil {
		return task.Task{}, err
	}
	ApplySidecars(&t, sidecars)
	return t, nil
}

func (p *Persistence) List() ([]task.Task, error) {
	ctx, cancel := context.WithTimeout(context.Background(), taskCallTimeout)
	defer cancel()
	return p.store.List(ctx)
}

func (p *Persistence) ListBoard() ([]task.Task, error) {
	ctx, cancel := context.WithTimeout(context.Background(), taskCallTimeout)
	defer cancel()
	return p.store.ListBoard(ctx)
}

func (p *Persistence) ListForNode(node string, closedSince time.Time) ([]task.Task, error) {
	ctx, cancel := context.WithTimeout(context.Background(), taskCallTimeout)
	defer cancel()
	return p.store.ListForNode(ctx, node, closedSince)
}

func (p *Persistence) PutBy(t task.Task, actor string, changed []string) (task.Task, error) {
	ctx, cancel := context.WithTimeout(context.Background(), taskCallTimeout)
	defer cancel()
	if err := p.store.PutBy(ctx, t, SidecarsFromTask(t), actor, changed); err != nil {
		return task.Task{}, err
	}
	return t, nil
}

func (p *Persistence) CreateBy(t task.Task, actor string) (task.Task, error) {
	ctx, cancel := context.WithTimeout(context.Background(), taskCallTimeout)
	defer cancel()
	saved, err := p.store.CreateBy(ctx, t, SidecarsFromTask(t), actor)
	if errors.Is(err, ErrIDCollision) {
		return task.Task{}, fmt.Errorf("%w: %w", task.ErrCreateIDCollision, err)
	}
	return saved, err
}

func (p *Persistence) UpdateFieldsBy(id, actor string, compute func(cur task.Task) (task.Update, error)) (task.Task, task.Status, error) {
	var prevStatus task.Status
	saved, err := p.PutFnBy(id, actor, func(cur task.Task) (task.Task, []string, error) {
		prevStatus = cur.Status
		u, err := compute(cur)
		if err != nil {
			return task.Task{}, nil, err
		}
		return task.ApplyUpdate(cur, u)
	})
	return saved, prevStatus, err
}

func (p *Persistence) PutFnBy(id, actor string, fn func(cur task.Task) (task.Task, []string, error)) (task.Task, error) {
	ctx, cancel := context.WithTimeout(context.Background(), taskCallTimeout)
	defer cancel()
	return p.store.PutFnBy(ctx, id, actor, fn)
}

func (p *Persistence) DeleteBy(id, actor string) error {
	ctx, cancel := context.WithTimeout(context.Background(), taskCallTimeout)
	defer cancel()
	return p.store.DeleteBy(ctx, id, actor)
}

func (p *Persistence) RestoreBy(id, actor string) error {
	ctx, cancel := context.WithTimeout(context.Background(), taskCallTimeout)
	defer cancel()
	return p.store.RestoreBy(ctx, id, actor)
}

var _ task.Persistence = (*Persistence)(nil)
