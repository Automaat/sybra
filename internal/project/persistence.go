package project

// Persistence is where project records live. Two implementations exist: the
// per-project YAML files and the database-backed SQLStore, selected by
// config.Database.Backend.
//
// Read returns the record exactly as stored, with no defaulting applied. The
// store applies defaults on the paths that want them; RawType deliberately does
// not, because a work project whose type field is absent must never be read as
// pet and routed to an untrusted follower.
//
// Clones stay on disk either way. Only the record moves, and it carries the
// clone's path.
type Persistence interface {
	Lock(id string) (release func(), err error)
	Read(id string) (Project, error)
	Write(p Project) error
	List() ([]Project, error)
	Delete(id string) error
}
