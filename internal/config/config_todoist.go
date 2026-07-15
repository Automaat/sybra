package config

type TodoistConfig struct {
	Enabled          bool   `yaml:"enabled" json:"enabled"`
	APIToken         string `yaml:"api_token" json:"apiToken"`
	ProjectID        string `yaml:"project_id" json:"projectId"`
	DefaultProjectID string `yaml:"default_project_id" json:"defaultProjectId"`
	PollSeconds      int    `yaml:"poll_seconds" json:"pollSeconds"`
}
