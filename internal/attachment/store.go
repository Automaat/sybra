package attachment

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/google/uuid"

	"github.com/Automaat/sybra/internal/fsutil"
)

const metaFileName = "meta.json"

// Store persists task attachment blobs under a local root directory.
type Store struct {
	root         string
	maxSizeBytes int64
	locks        sync.Map // taskID -> *sync.Mutex
}

// NewStore constructs a local attachment store rooted at dir.
func NewStore(dir string, maxSizeBytes int64) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create attachments dir: %w", err)
	}
	return &Store{root: dir, maxSizeBytes: maxSizeBytes}, nil
}

// Put stores a task attachment blob and its metadata atomically.
func (s *Store) Put(taskID string, req UploadRequest) (Attachment, error) {
	if s == nil {
		return Attachment{}, errors.New("attachment store is not configured")
	}
	if err := fsutil.ValidateKey(taskID); err != nil {
		return Attachment{}, fmt.Errorf("task id: %w", err)
	}
	if err := s.validateSize(req.Data); err != nil {
		return Attachment{}, err
	}
	name := sanitizeFileName(req.FileName)
	if name == "" {
		return Attachment{}, errors.New("attachment filename is required")
	}
	contentType := strings.TrimSpace(req.ContentType)
	if contentType == "" {
		contentType = http.DetectContentType(req.Data)
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	mu := s.lockFor(taskID)
	mu.Lock()
	defer mu.Unlock()

	id := "att_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	dir, err := s.attachmentDir(taskID, id)
	if err != nil {
		return Attachment{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Attachment{}, fmt.Errorf("create attachment dir: %w", err)
	}
	blobPath := filepath.Join(dir, name)
	if err := writeBlob(blobPath, req.Data, 0o600); err != nil {
		return Attachment{}, fmt.Errorf("write attachment blob: %w", err)
	}

	meta := Attachment{
		ID:          id,
		FileName:    name,
		ContentType: contentType,
		SizeBytes:   int64(len(req.Data)),
		Path:        blobPath,
		CreatedAt:   time.Now().UTC(),
	}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		_ = os.Remove(blobPath)
		return Attachment{}, fmt.Errorf("marshal attachment metadata: %w", err)
	}
	if err := writeBlob(filepath.Join(dir, metaFileName), metaBytes, 0o600); err != nil {
		_ = os.Remove(blobPath)
		return Attachment{}, fmt.Errorf("write attachment metadata: %w", err)
	}
	return meta, nil
}

// Import stores a replicated attachment blob with its existing attachment ID.
func (s *Store) Import(taskID string, meta Attachment, data []byte) (Attachment, error) {
	if s == nil {
		return Attachment{}, errors.New("attachment store is not configured")
	}
	if err := fsutil.ValidateKey(taskID); err != nil {
		return Attachment{}, fmt.Errorf("task id: %w", err)
	}
	if err := fsutil.ValidateKey(meta.ID); err != nil {
		return Attachment{}, fmt.Errorf("attachment id: %w", err)
	}
	if err := s.validateSize(data); err != nil {
		return Attachment{}, err
	}
	name := sanitizeFileName(meta.FileName)
	if name == "" {
		return Attachment{}, errors.New("attachment filename is required")
	}
	contentType := strings.TrimSpace(meta.ContentType)
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	createdAt := meta.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	mu := s.lockFor(taskID)
	mu.Lock()
	defer mu.Unlock()

	dir, err := s.attachmentDir(taskID, meta.ID)
	if err != nil {
		return Attachment{}, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return Attachment{}, fmt.Errorf("replace attachment dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Attachment{}, fmt.Errorf("create attachment dir: %w", err)
	}
	blobPath := filepath.Join(dir, name)
	if err := writeBlob(blobPath, data, 0o600); err != nil {
		return Attachment{}, fmt.Errorf("write attachment blob: %w", err)
	}

	local := Attachment{
		ID:          meta.ID,
		FileName:    name,
		ContentType: contentType,
		SizeBytes:   int64(len(data)),
		Path:        blobPath,
		CreatedAt:   createdAt,
	}
	metaBytes, err := json.Marshal(local)
	if err != nil {
		_ = os.RemoveAll(dir)
		return Attachment{}, fmt.Errorf("marshal attachment metadata: %w", err)
	}
	if err := writeBlob(filepath.Join(dir, metaFileName), metaBytes, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return Attachment{}, fmt.Errorf("write attachment metadata: %w", err)
	}
	return local, nil
}

// List returns every on-disk attachment metadata entry for a task.
func (s *Store) List(taskID string) ([]Attachment, error) {
	if s == nil {
		return nil, errors.New("attachment store is not configured")
	}
	if err := fsutil.ValidateKey(taskID); err != nil {
		return nil, fmt.Errorf("task id: %w", err)
	}
	taskDir, err := s.taskDir(taskID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Attachment{}, nil
		}
		return nil, fmt.Errorf("read attachment dir: %w", err)
	}
	out := make([]Attachment, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		meta, err := s.readMeta(filepath.Join(taskDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, meta)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].FileName < out[j].FileName
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// Path returns the validated blob path for a task attachment.
func (s *Store) Path(taskID, attachmentID string) (string, error) {
	if s == nil {
		return "", errors.New("attachment store is not configured")
	}
	dir, err := s.attachmentDir(taskID, attachmentID)
	if err != nil {
		return "", err
	}
	meta, err := s.readMeta(dir)
	if err != nil {
		return "", err
	}
	if !fsutil.Within(dir, meta.Path) {
		return "", fmt.Errorf("attachment path escapes task dir: %s", attachmentID)
	}
	if _, err := os.Stat(meta.Path); err != nil {
		return "", fmt.Errorf("stat attachment blob: %w", err)
	}
	return meta.Path, nil
}

// Delete removes one attachment blob and its metadata. Missing attachments are ignored.
func (s *Store) Delete(taskID, attachmentID string) error {
	if s == nil {
		return errors.New("attachment store is not configured")
	}
	if err := fsutil.ValidateKey(taskID); err != nil {
		return fmt.Errorf("task id: %w", err)
	}
	if err := fsutil.ValidateKey(attachmentID); err != nil {
		return fmt.Errorf("attachment id: %w", err)
	}
	mu := s.lockFor(taskID)
	mu.Lock()
	defer mu.Unlock()
	dir, err := s.attachmentDir(taskID, attachmentID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delete attachment: %w", err)
	}
	return nil
}

// DeleteTask removes all attachment blobs for a task. Missing tasks are ignored.
func (s *Store) DeleteTask(taskID string) error {
	if s == nil {
		return errors.New("attachment store is not configured")
	}
	if err := fsutil.ValidateKey(taskID); err != nil {
		return fmt.Errorf("task id: %w", err)
	}
	mu := s.lockFor(taskID)
	mu.Lock()
	defer mu.Unlock()
	dir, err := s.taskDir(taskID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delete task attachments: %w", err)
	}
	return nil
}

func (s *Store) validateSize(data []byte) error {
	if s.maxSizeBytes <= 0 {
		return nil
	}
	if int64(len(data)) > s.maxSizeBytes {
		return fmt.Errorf("attachment exceeds max size of %d bytes", s.maxSizeBytes)
	}
	return nil
}

func (s *Store) lockFor(taskID string) *sync.Mutex {
	existing, _ := s.locks.LoadOrStore(taskID, &sync.Mutex{})
	if mu, ok := existing.(*sync.Mutex); ok {
		return mu
	}
	mu := &sync.Mutex{}
	s.locks.Store(taskID, mu)
	return mu
}

func (s *Store) taskDir(taskID string) (string, error) {
	return fsutil.SafeJoin(s.root, taskID)
}

func (s *Store) attachmentDir(taskID, attachmentID string) (string, error) {
	if err := fsutil.ValidateKey(attachmentID); err != nil {
		return "", fmt.Errorf("attachment id: %w", err)
	}
	taskDir, err := s.taskDir(taskID)
	if err != nil {
		return "", err
	}
	return fsutil.SafeJoin(taskDir, attachmentID)
}

func (s *Store) readMeta(dir string) (Attachment, error) {
	data, err := os.ReadFile(filepath.Join(dir, metaFileName))
	if err != nil {
		return Attachment{}, fmt.Errorf("read attachment metadata: %w", err)
	}
	var meta Attachment
	if err := json.Unmarshal(data, &meta); err != nil {
		return Attachment{}, fmt.Errorf("decode attachment metadata: %w", err)
	}
	return meta, nil
}

func sanitizeFileName(name string) string {
	name = strings.TrimSpace(filepath.Base(strings.ReplaceAll(name, "\\", "/")))
	if name == "" || name == "." || name == ".." {
		return ""
	}
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case strings.ContainsRune("._-()[] ", r):
			b.WriteRune(r)
			lastDash = false
		case r < 32:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	sanitized := strings.Trim(b.String(), ". -")
	if sanitized == "" || sanitized == "." || sanitized == ".." {
		return ""
	}
	return sanitized
}

// writeBlob stages an attachment payload beside its target and publishes it
// with an explicit mode: blobs are served to the frontend, so their mode must
// not vary with the operator's umask.
func writeBlob(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsutil.AtomicWriteMode(path, data, perm)
}
