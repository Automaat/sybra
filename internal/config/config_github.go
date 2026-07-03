package config

type GitHubConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// PollerRole splits GitHub search polling (reviews/issues/renovate) across
	// machines sharing one token. "primary" (or empty) runs the search pollers;
	// "secondary" skips them so a sibling instance owns the searches and the
	// shared token isn't billed twice. On-demand per-PR/issue calls still run on
	// every machine — only the periodic searches are gated.
	PollerRole string `yaml:"poller_role" json:"pollerRole"`
	// Poll-interval overrides in seconds. Zero falls back to the built-in
	// default. Raised defaults (vs. the original 1m/5m) cut steady-state request
	// volume; lower them only on a high-limit (App-token) instance.
	ReviewsFastSeconds  int `yaml:"reviews_fast_seconds" json:"reviewsFastSeconds"`
	ReviewsSlowSeconds  int `yaml:"reviews_slow_seconds" json:"reviewsSlowSeconds"`
	IssuesSeconds       int `yaml:"issues_seconds" json:"issuesSeconds"`
	RenovateFastSeconds int `yaml:"renovate_fast_seconds" json:"renovateFastSeconds"`
	RenovateSlowSeconds int `yaml:"renovate_slow_seconds" json:"renovateSlowSeconds"`
	// App configures GitHub App installation-token auth. When enabled, Sybra
	// mints a short-lived installation token and injects it into the gh
	// subprocess (GH_TOKEN), raising the REST ceiling to 15k/hr. Unset = fall
	// back to gh's own auth.
	App GitHubAppConfig `yaml:"app" json:"app"`
	// NativeAutoMerge is a kill-switch for arming GitHub's native
	// `gh pr merge --auto` on pet-project PRs once Sybra's own review/fix
	// cycle is done and the base branch's protection supports it. It is an
	// accelerator on top of the existing green-gated MergePR path, not a
	// replacement — when unsupported or disabled the legacy merge stays the
	// fallback. Default off (zero value = false).
	NativeAutoMerge bool `yaml:"native_auto_merge" json:"nativeAutoMerge"`
	// AutoResolveCleanMerges is a kill-switch for the deterministic
	// clean-merge fast-path: before dispatching a conflict-recovery agent,
	// Sybra attempts a plain `git merge` of the PR's base branch in Go. When
	// that merge creates a commit with no conflicting hunks, it is pushed and
	// no agent is spawned; conflicts, no-op merges, and errors still fall
	// through to the agent-assisted path. Default off (zero value = false).
	AutoResolveCleanMerges bool `yaml:"auto_resolve_clean_merges" json:"autoResolveCleanMerges"`
}

// GitHubAppConfig holds GitHub App installation-token credentials. The private
// key never leaves disk as plaintext config — only its path is stored.
type GitHubAppConfig struct {
	Enabled        bool   `yaml:"enabled" json:"enabled"`
	AppID          int64  `yaml:"app_id" json:"appId"`
	InstallationID int64  `yaml:"installation_id" json:"installationId"`
	PrivateKeyPath string `yaml:"private_key_path" json:"privateKeyPath"`
}
