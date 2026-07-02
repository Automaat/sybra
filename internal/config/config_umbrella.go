package config

// UmbrellaConfig governs auto-expansion of ☂️ umbrella issues by the GitHub
// issue fetcher. Disabled by default; project-scoped via the top-level
// project_types allowlist so only one machine expands a given umbrella.
type UmbrellaConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Model overrides the planner model (empty = claude default).
	Model string `yaml:"model" json:"model"`
	// Ground gates the optional grounding step that confirms each
	// sub-issue's touches against the real repo's tracked files. Disabled
	// by default (extra git/tool calls); additive on top of planner output.
	Ground bool `yaml:"ground" json:"ground"`
	// GroundMinSubIssues gates grounding on umbrella size: grounding only
	// runs when the umbrella has at least this many sub-issues. Zero or
	// negative means always ground when Ground is enabled.
	GroundMinSubIssues int `yaml:"ground_min_sub_issues" json:"groundMinSubIssues"`
}
