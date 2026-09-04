package config

// HumanReviewConfig controls the in-process automation that spawns a
// headless review agent every time a task transitions to human-required.
// The agent inspects the task, its agent runs, recent logs and the Sybra
// source tree, decides whether the transition is genuine or a Sybra bug,
// and records the diagnosis on the task. Per-machine toggle: enable on the
// laptop with the source checkout, leave disabled on the server.
type HumanReviewConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// SybraRepoDir is the fallback working directory used only when a task
	// has no worktree of its own (e.g. project-less, or the recorded
	// worktree was cleaned up) — see humanReviewDispatchDir. It must be a
	// dedicated checkout, distinct from auto_update.repo_dir: the review
	// agent is dispatched with RunConfig.ReadOnlyDir=true in that fallback
	// case, so the OS process sandbox denies writes to it under enforce
	// regardless, but pointing it at the live deploy/build checkout is still
	// wrong on principle — this diagnostic agent has no business sharing a
	// directory that autoupdate concurrently ff-merges and builds from.
	SybraRepoDir string `yaml:"sybra_repo_dir" json:"sybraRepoDir"`
	// Repo is the owner/name project_id assigned to local sybra-bug tasks.
	// Defaults to "Automaat/sybra" when empty.
	Repo string `yaml:"repo" json:"repo"`
	// Model is a provider-neutral alias ("sonnet", "haiku", "opus") — never a
	// concrete provider-specific model ID, since the spawned provider follows
	// the machine's configured agent.provider (claude, codex, ...) and a
	// literal like "claude-haiku-4-5-20251001" is rejected outright by a
	// non-Claude provider's CLI (see #2639). Defaults to "haiku" when empty —
	// diagnosis, not authoring.
	Model string `yaml:"model" json:"model"`
	// MaxPerHour caps how many review agents may be spawned in any rolling
	// 60-minute window across all tasks on this machine. Zero falls back
	// to DefaultHumanReviewMaxPerHour.
	MaxPerHour int `yaml:"max_per_hour" json:"maxPerHour"`
	// IssueLabel is a legacy setting from the removed GitHub issue filing
	// path. Defaults to "sybra-bug".
	IssueLabel string `yaml:"issue_label" json:"issueLabel"`
	// SybraBugAction controls the side-effect for sybra_bug verdicts:
	// note_only (default), local_task, or block_only. The legacy file_issue
	// value is accepted but treated as note_only.
	SybraBugAction string `yaml:"sybra_bug_action" json:"sybraBugAction"`
}
