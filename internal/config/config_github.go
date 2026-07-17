package config

type GitHubConfig struct {
	// Enabled is the top-level kill-switch: false forces every GitHub
	// automation off regardless of the sub-toggles below. Fresh generated
	// configs set this to false so first-run GitHub polling is opt-in. Legacy
	// configs that omit this key keep the old enabled behavior during load.
	// true defers to IssuesEnabled/ReviewsEnabled.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// IssuesEnabled gates the GitHub Issues fetcher specifically. Defaults to
	// true (see DefaultConfig). Effective state is Enabled && IssuesEnabled —
	// use RunsIssuesFetcher() rather than reading this field directly.
	IssuesEnabled bool `yaml:"issues_enabled" json:"issuesEnabled"`
	// ReviewsEnabled gates PR reviewer poll registration specifically.
	// Defaults to true (see DefaultConfig). Effective state is
	// Enabled && ReviewsEnabled — use RunsReviewer() rather than reading this
	// field directly.
	ReviewsEnabled bool `yaml:"reviews_enabled" json:"reviewsEnabled"`
	// PollerRole splits GitHub search polling (reviews/issues/renovate) across
	// machines sharing one token. "primary" (or empty) runs the search pollers;
	// "secondary" skips them so a sibling instance owns the searches and the
	// shared token isn't billed twice. On-demand per-PR/issue calls still run on
	// every machine — only the periodic searches are gated.
	PollerRole string `yaml:"poller_role" json:"pollerRole"`
	// Poll-interval overrides in seconds. Zero falls back to the built-in
	// default. Raised defaults (vs. the original 1m/5m) cut steady-state request
	// volume; lower them only on a high-limit (App-token) instance.
	ReviewsFastSeconds int `yaml:"reviews_fast_seconds" json:"reviewsFastSeconds"`
	// ReviewRoundsPerHour caps automated review runs one PR may receive in a
	// rolling hour before the task is parked for a human. 0 uses the default;
	// negative disables the cap. Rate-based rather than a lifetime total so a
	// long-lived PR that is legitimately re-reviewed after each push is never
	// blocked, while a runaway loop is stopped within the hour (#2164 sustained
	// ~5/hour for 23 hours).
	ReviewRoundsPerHour int `yaml:"review_rounds_per_hour" json:"reviewRoundsPerHour"`
	ReviewsSlowSeconds  int `yaml:"reviews_slow_seconds" json:"reviewsSlowSeconds"`
	// ReviewsMaxPRsPerTick caps how many non-active linked PRs the known-PR
	// poller fetches in one tick. Zero falls back to the built-in default;
	// resolved non-positive values mean "unlimited".
	ReviewsMaxPRsPerTick int `yaml:"reviews_max_prs_per_tick" json:"reviewsMaxPRsPerTick"`
	// ReviewsStableBackoffMaxTicks caps the exponential skip window for linked
	// PRs whose head SHA and updatedAt stay unchanged across polls. Zero falls
	// back to the built-in default; resolved non-positive values disable the
	// backoff entirely.
	ReviewsStableBackoffMaxTicks int `yaml:"reviews_stable_backoff_max_ticks" json:"reviewsStableBackoffMaxTicks"`
	IssuesSeconds                int `yaml:"issues_seconds" json:"issuesSeconds"`
	RenovateFastSeconds          int `yaml:"renovate_fast_seconds" json:"renovateFastSeconds"`
	RenovateSlowSeconds          int `yaml:"renovate_slow_seconds" json:"renovateSlowSeconds"`
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
