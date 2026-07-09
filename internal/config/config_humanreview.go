package config

// HumanReviewConfig controls the in-process automation that spawns a
// headless review agent every time a task transitions to human-required.
// The agent inspects the task, its agent runs, recent logs and the Sybra
// source tree, decides whether the transition is genuine or a Sybra bug,
// and (on bug) files a deduplicated GitHub issue + flips the task to
// blocked. Per-machine toggle: enable on the laptop with the source
// checkout, leave disabled on the server.
type HumanReviewConfig struct {
	Enabled      bool   `yaml:"enabled" json:"enabled"`
	SybraRepoDir string `yaml:"sybra_repo_dir" json:"sybraRepoDir"`
	// Repo is the owner/name where bug issues are filed. Defaults to
	// "Automaat/sybra" when empty.
	Repo string `yaml:"repo" json:"repo"`
	// Model is the Claude model name or alias (e.g. "sonnet",
	// "claude-haiku-4-5-20251001"). Defaults to
	// "claude-haiku-4-5-20251001" when empty — diagnosis, not authoring.
	Model string `yaml:"model" json:"model"`
	// MaxPerHour caps how many review agents may be spawned in any rolling
	// 60-minute window across all tasks on this machine. Zero falls back
	// to DefaultHumanReviewMaxPerHour.
	MaxPerHour int `yaml:"max_per_hour" json:"maxPerHour"`
	// IssueLabel is the label applied to filed issues (in addition to
	// "bug"). Defaults to "sybra-bug".
	IssueLabel string `yaml:"issue_label" json:"issueLabel"`
	// SybraBugAction controls the side-effect for sybra_bug verdicts:
	// file_issue (default), local_task, block_only, or note_only.
	SybraBugAction string `yaml:"sybra_bug_action" json:"sybraBugAction"`
}
