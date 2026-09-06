package audit

// WithRelease attributes newly recorded events to this running leader build.
// Install it after importing historical records; Read never relabels history.
func WithRelease(store Store, revision string) Store {
	return &releaseStore{Store: store, revision: revision}
}

type releaseStore struct {
	Store
	revision string
}

func (s *releaseStore) Log(e Event) error {
	if e.Release == "" {
		e.Release = s.revision
	}
	return s.Store.Log(e)
}
