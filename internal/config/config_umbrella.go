package config

// UmbrellaConfig governs auto-expansion of ☂️ umbrella issues by the GitHub
// issue fetcher. Disabled by default; project-scoped via the top-level
// project_types allowlist so only one machine expands a given umbrella.
type UmbrellaConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Model overrides the planner model (empty = claude default).
	Model string `yaml:"model" json:"model"`
}
