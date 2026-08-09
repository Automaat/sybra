package attachment

// Persistence is where attachment content lives. Two implementations exist: the
// per-task blob directories and the database-backed SQLStore, selected by
// config.Database.Backend.
//
// Content, not a path. A path is only meaningful to a process on the same
// machine, which is what kept an attachment unreachable from a board reached
// over the network; the caller receives the bytes and never learns where they
// came from.
type Persistence interface {
	Put(taskID string, meta Attachment, data []byte) (Attachment, error)
	Content(taskID, attachmentID string) ([]byte, Attachment, error)
	List(taskID string) ([]Attachment, error)
	Delete(taskID, attachmentID string) error
	DeleteTask(taskID string) error
}
