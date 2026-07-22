package config

// AutoUpdateConfig controls source update checks. In "auto" mode a clean
// fast-forward update is applied and Sybra requests a supervisor restart.
type AutoUpdateConfig struct {
	Enabled        bool     `yaml:"enabled" json:"enabled"`
	RepoDir        string   `yaml:"repo_dir" json:"repoDir"`
	Remote         string   `yaml:"remote" json:"remote"`
	Branch         string   `yaml:"branch" json:"branch"`
	Mode           string   `yaml:"mode" json:"mode"`
	// RequiredChecks is the exact set of status/check names that must report
	// SUCCESS on the candidate commit before auto mode may apply it. Empty is
	// allowed in notify mode; auto mode fails closed when this list is empty.
	RequiredChecks []string `yaml:"required_checks" json:"requiredChecks"`
	PollSeconds    int      `yaml:"poll_seconds" json:"pollSeconds"`
	// Deprecated: ignored. Kept so existing config files continue to load.
	RestartDelaySeconds int `yaml:"restart_delay_seconds" json:"restartDelaySeconds"`
}
