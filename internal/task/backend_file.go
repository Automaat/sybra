package task

// fileBackend adapts *Store to Persistence. actor and changed are accepted and discarded: the file backend has no history table to record them in — its audit trail is the tasks-dir git snapshot (internal/tasksnapshot), a separate mechanism this does not touch. Selecting the file backend must therefore keep every existing behavior exactly as it was before Persistence existed, which is why this delegates straight to Store's own tested methods rather than reimplementing anything.
type fileBackend struct {
	store *Store
}

func newFileBackend(store *Store) *fileBackend { return &fileBackend{store: store} }

func (f *fileBackend) Get(id string) (Task, error) { return f.store.Get(id) }

func (f *fileBackend) List() ([]Task, error) { return f.store.List() }

func (f *fileBackend) PutBy(t Task, actor string, changed []string) (Task, error) {
	return f.store.Put(t)
}

// CreateBy ignores t.ID: createNewTask (via CreatePrebuilt) mints and
// collision-retries its own the same way Store.CreateFull always has.
func (f *fileBackend) CreateBy(t Task, actor string) (Task, error) {
	return f.store.CreatePrebuilt(t, sidecarUpdateFromTask(t))
}

// UpdateFieldsBy delegates straight to Store.UpdateWithPrev, the only file
// path that writes the ten planning/review sidecar files: PutFnBy's generic
// whole-task write (via Store.PutFn) does not, so routing an Update-shaped
// mutation through it here would silently drop sidecar-field edits.
func (f *fileBackend) UpdateFieldsBy(id, actor string, compute func(cur Task) (Update, error)) (Task, Status, error) {
	cur, err := f.store.Get(id)
	if err != nil {
		return Task{}, "", err
	}
	u, err := compute(cur)
	if err != nil {
		return Task{}, "", err
	}
	saved, prevStatus, err := f.store.UpdateWithPrev(id, u)
	return saved, prevStatus, err
}

func (f *fileBackend) PutFnBy(id, actor string, fn func(cur Task) (Task, []string, error)) (Task, error) {
	saved, _, err := f.store.PutFn(id, func(cur Task) (Task, error) {
		next, _, ferr := fn(cur)
		return next, ferr
	})
	return saved, err
}

func (f *fileBackend) DeleteBy(id, actor string) error { return f.store.Delete(id) }

func (f *fileBackend) RestoreBy(id, actor string) error {
	_, err := f.store.RestoreFromTrash(id)
	return err
}

var _ Persistence = (*fileBackend)(nil)
