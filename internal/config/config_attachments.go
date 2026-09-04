package config

// AttachmentConfig controls local task attachment uploads.
type AttachmentConfig struct {
	// MaxSizeMB caps a single uploaded attachment's raw byte size before it is
	// written to disk. 0 falls back to DefaultAttachmentMaxSizeMB.
	MaxSizeMB int `yaml:"max_size_mb" json:"maxSizeMB"`
}
