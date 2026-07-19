package attachment

import "time"

// Attachment is the persisted metadata for a task-owned local blob.
type Attachment struct {
	ID          string    `json:"id" yaml:"id"`
	FileName    string    `json:"fileName" yaml:"file_name"`
	ContentType string    `json:"contentType" yaml:"content_type"`
	SizeBytes   int64     `json:"sizeBytes" yaml:"size_bytes"`
	Path        string    `json:"path" yaml:"path"`
	CreatedAt   time.Time `json:"createdAt" yaml:"created_at"`
}

// UploadRequest is the validated input for storing an attachment blob.
type UploadRequest struct {
	FileName    string
	ContentType string
	Data        []byte
}
