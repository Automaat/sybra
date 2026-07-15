package config

// TriageConfig controls the background auto-triage worker. When Enabled,
// sybra periodically classifies tasks in status=new via claude -p and
// atomically applies the verdict (title, tags, size/type, mode, project).
type TriageConfig struct {
	Enabled     bool   `yaml:"enabled" json:"enabled"`
	PollSeconds int    `yaml:"poll_seconds" json:"pollSeconds"`
	Model       string `yaml:"model" json:"model"`
}
