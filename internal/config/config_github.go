package config

type GitHubPollingConfig struct {
	Issues      GitHubPollingStreamConfig `yaml:"issues" json:"issues"`
	SybraPRs    GitHubPRPollingConfig     `yaml:"sybra_prs" json:"sybraPrs"`
	AssignedPRs GitHubPRPollingConfig     `yaml:"assigned_prs" json:"assignedPrs"`
}

type GitHubPollingStreamConfig struct {
	Enabled         bool `yaml:"enabled" json:"enabled"`
	IntervalSeconds int  `yaml:"interval_seconds" json:"intervalSeconds"`
}

type GitHubPRPollingConfig struct {
	Enabled               bool `yaml:"enabled" json:"enabled"`
	ActiveIntervalSeconds int  `yaml:"active_interval_seconds" json:"activeIntervalSeconds"`
	IdleIntervalSeconds   int  `yaml:"idle_interval_seconds" json:"idleIntervalSeconds"`
}

type GitHubConfig struct {
	// Enabled is the top-level kill-switch: false forces every GitHub
	// automation off regardless of the sub-toggles below. Fresh generated
	// configs set this to false so first-run GitHub polling is opt-in. Legacy
	// configs that omit this key keep the old enabled behavior during load.
	// true defers to IssuesEnabled/ReviewsEnabled.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Polling is the primary stream-level control surface for GitHub polling.
	// The legacy fields below remain as compatibility inputs during load but new
	// code should read this block through the effective helper methods.
	Polling GitHubPollingConfig `yaml:"polling" json:"polling"`
	// IssuesEnabled gates the GitHub Issues fetcher specifically. Defaults to
	// true for legacy configs. Deprecated compatibility input for
	// github.polling.issues.enabled.
	IssuesEnabled bool `yaml:"issues_enabled" json:"issuesEnabled"`
	// ReviewsEnabled gates PR reviewer poll registration specifically.
	// Deprecated compatibility input for both github.polling.sybra_prs.enabled
	// and github.polling.assigned_prs.enabled.
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
	// Deprecated compatibility input for both PR streams' active intervals.
	ReviewsFastSeconds int `yaml:"reviews_fast_seconds" json:"reviewsFastSeconds"`
	// Deprecated compatibility input for both PR streams' idle intervals.
	ReviewsSlowSeconds int `yaml:"reviews_slow_seconds" json:"reviewsSlowSeconds"`
	// ReviewsMaxPRsPerTick caps how many non-active linked PRs the known-PR
	// poller fetches in one tick. Zero falls back to the built-in default;
	// resolved non-positive values mean "unlimited".
	ReviewsMaxPRsPerTick int `yaml:"reviews_max_prs_per_tick" json:"reviewsMaxPRsPerTick"`
	// ReviewsStableBackoffMaxTicks caps the exponential skip window for linked
	// PRs whose head SHA and updatedAt stay unchanged across polls. Zero falls
	// back to the built-in default; resolved non-positive values disable the
	// backoff entirely.
	ReviewsStableBackoffMaxTicks int `yaml:"reviews_stable_backoff_max_ticks" json:"reviewsStableBackoffMaxTicks"`
	// Deprecated compatibility input for github.polling.issues.interval.
	IssuesSeconds int `yaml:"issues_seconds" json:"issuesSeconds"`
	// MentionTriggerPhrase, when set, gates a comment-mention search alongside
	// the existing assigned/labeled issue paths: an open issue whose comments
	// contain this phrase (e.g. "@sybra") gets a task via the same
	// dedup/creation path. Empty (default) disables the feature — existing
	// installs see no behavior change.
	MentionTriggerPhrase string `yaml:"mention_trigger_phrase" json:"mentionTriggerPhrase"`
	RenovateFastSeconds  int    `yaml:"renovate_fast_seconds" json:"renovateFastSeconds"`
	RenovateSlowSeconds  int    `yaml:"renovate_slow_seconds" json:"renovateSlowSeconds"`
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
	// clean-merge fast-path used before dispatching any review/fix-review
	// round on a PR: Sybra attempts a plain `git merge` of the PR's base
	// branch in Go. For a lone conflict issue, a merge that creates a commit
	// with no conflicting hunks is pushed and no agent is spawned; for every
	// other issue (comments, ci_failure, coalesced sets), the same clean
	// merge is pushed as a non-skipping pre-dispatch sync so the round never
	// wastes a pass re-diagnosing a stale-diff artifact. Conflicts, no-op
	// merges, and errors always fall through to the agent-assisted path.
	// Default off (zero value = false).
	AutoResolveCleanMerges bool `yaml:"auto_resolve_clean_merges" json:"autoResolveCleanMerges"`
	// FlakyDetection is a kill-switch for same-commit CI flakiness
	// classification. When true, a lone ci_failure issue is classified via
	// ClassifyCIFlakiness (the head commit's full check-run history, not just
	// the latest attempt) before it is escalated to a fix agent or a human: a
	// check that both passed and failed on the same SHA at or above
	// FlakySuccessThreshold is flaky, and gets a targeted rerun plus a
	// distinct audit event instead. Default off (zero value = false).
	FlakyDetection bool `yaml:"flaky_detection" json:"flakyDetection"`
	// FlakySuccessThreshold is the minimum same-check success rate (0-1) for
	// a currently-failing gating check to be classified flaky rather than
	// deterministic. Zero falls back to the built-in default; see
	// GitHubConfig.FlakyThreshold().
	FlakySuccessThreshold float64 `yaml:"flaky_success_threshold" json:"flakySuccessThreshold"`
}

// GitHubAppConfig holds GitHub App installation-token credentials. The private
// key never leaves disk as plaintext config — only its path is stored.
type GitHubAppConfig struct {
	Enabled        bool   `yaml:"enabled" json:"enabled"`
	AppID          int64  `yaml:"app_id" json:"appId"`
	InstallationID int64  `yaml:"installation_id" json:"installationId"`
	PrivateKeyPath string `yaml:"private_key_path" json:"privateKeyPath"`
}
