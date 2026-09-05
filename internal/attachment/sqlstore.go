package attachment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/fsutil"
)

// attachmentQueryTimeout bounds every statement. Uploads and downloads run from
// request handlers that hold no context of their own here.
const attachmentQueryTimeout = 60 * time.Second

// SQLStore keeps attachment content in the configured database backend.
//
// The bytes live in the row. That is the point of the move: a path is only
// meaningful on the machine holding it, so an attachment stored as a file is
// unreachable from a board reached over the network.
type SQLStore struct {
	db           *db.DB
	maxSizeBytes int64
}

// NewSQLStore returns the database-backed attachment store. maxSizeBytes of
// zero or less means no limit, matching the file store.
func NewSQLStore(database *db.DB, maxSizeBytes int64) (*SQLStore, error) {
	if database == nil {
		return nil, errors.New("attachment store needs an open database")
	}
	return &SQLStore{db: database, maxSizeBytes: maxSizeBytes}, nil
}

const (
	upsertAttachment = `INSERT INTO task_attachments (task_id, id, file_name, content_type, size_bytes, created_at, content)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (task_id, id) DO UPDATE SET
			file_name = excluded.file_name, content_type = excluded.content_type,
			size_bytes = excluded.size_bytes, created_at = excluded.created_at,
			content = excluded.content`

	selectAttachment = `SELECT file_name, content_type, size_bytes, created_at, content
		FROM task_attachments WHERE task_id = ? AND id = ?`

	selectAttachments = `SELECT id, file_name, content_type, size_bytes, created_at
		FROM task_attachments WHERE task_id = ? ORDER BY created_at, `

	deleteAttachment     = `DELETE FROM task_attachments WHERE task_id = ? AND id = ?`
	deleteTaskAttachment = `DELETE FROM task_attachments WHERE task_id = ?`
)

// Put stores one attachment's content and metadata.
func (s *SQLStore) Put(taskID string, meta Attachment, data []byte) (Attachment, error) {
	if s == nil {
		return Attachment{}, errors.New("attachment store is not configured")
	}
	// The same guards the file store applies. The primary key is (task_id, id), so a blank id is not rejected by the database — it collides with every other blank one and silently overwrites another task's attachment. The filename is sanitized because it is handed back to callers that build a download name from it.
	if err := fsutil.ValidateKey(taskID); err != nil {
		return Attachment{}, fmt.Errorf("task id: %w", err)
	}
	if err := fsutil.ValidateKey(meta.ID); err != nil {
		return Attachment{}, fmt.Errorf("attachment id: %w", err)
	}
	if err := s.validateSize(data); err != nil {
		return Attachment{}, err
	}
	meta.FileName = sanitizeFileName(meta.FileName)
	if meta.FileName == "" {
		return Attachment{}, errors.New("attachment filename is required")
	}
	meta.SizeBytes = int64(len(data))
	// Emptied deliberately: nothing on disk backs this record, and a stale path
	// would invite a caller to read a file that is not there.
	meta.Path = ""
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = time.Now().UTC()
	}

	ctx, cancel := context.WithTimeout(context.Background(), attachmentQueryTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, upsertAttachment,
		taskID, meta.ID, meta.FileName, meta.ContentType,
		meta.SizeBytes, db.TimeValue(meta.CreatedAt), data); err != nil {
		return Attachment{}, fmt.Errorf("write attachment: %w", err)
	}
	return meta, nil
}

// Content returns one attachment's bytes and metadata.
func (s *SQLStore) Content(taskID, attachmentID string) ([]byte, Attachment, error) {
	if s == nil {
		return nil, Attachment{}, errors.New("attachment store is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), attachmentQueryTimeout)
	defer cancel()

	var (
		meta      Attachment
		createdAt int64
		content   []byte
	)
	meta.ID = attachmentID
	err := s.db.QueryRowContext(ctx, selectAttachment, taskID, attachmentID).
		Scan(&meta.FileName, &meta.ContentType, &meta.SizeBytes, &createdAt, &content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Attachment{}, fmt.Errorf("attachment %q not found", attachmentID)
	}
	if err != nil {
		return nil, Attachment{}, fmt.Errorf("read attachment: %w", err)
	}
	meta.CreatedAt = db.TimeFrom(createdAt)
	return content, meta, nil
}

// List returns a task's attachments oldest first, which is the order the
// directory listing produced.
func (s *SQLStore) List(taskID string) ([]Attachment, error) {
	if s == nil {
		return nil, errors.New("attachment store is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), attachmentQueryTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, selectAttachments+s.db.OrderText("id"), taskID)
	if err != nil {
		return nil, fmt.Errorf("list attachments: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Attachment
	for rows.Next() {
		var (
			a         Attachment
			createdAt int64
		)
		if err := rows.Scan(&a.ID, &a.FileName, &a.ContentType, &a.SizeBytes, &createdAt); err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		a.CreatedAt = db.TimeFrom(createdAt)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attachments: %w", err)
	}
	return out, nil
}

// Delete removes one attachment.
func (s *SQLStore) Delete(taskID, attachmentID string) error {
	if s == nil {
		return errors.New("attachment store is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), attachmentQueryTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, deleteAttachment, taskID, attachmentID); err != nil {
		return fmt.Errorf("delete attachment: %w", err)
	}
	return nil
}

// DeleteTask removes every attachment belonging to a task.
func (s *SQLStore) DeleteTask(taskID string) error {
	if s == nil {
		return errors.New("attachment store is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), attachmentQueryTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, deleteTaskAttachment, taskID); err != nil {
		return fmt.Errorf("delete task attachments: %w", err)
	}
	return nil
}

// validateSize names the limit, so an operator reading the refusal knows what
// to change rather than only that the upload was too big.
func (s *SQLStore) validateSize(data []byte) error {
	if s.maxSizeBytes <= 0 {
		return nil
	}
	if int64(len(data)) > s.maxSizeBytes {
		return fmt.Errorf("attachment is %d bytes, which exceeds the configured limit of %d bytes",
			len(data), s.maxSizeBytes)
	}
	return nil
}

var _ Persistence = (*SQLStore)(nil)
