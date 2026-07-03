package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/abtest"
	"gopkg.in/yaml.v3"
)

// AllowsProjectType reports whether automations on this machine should act on
// projects of the given type. An empty ProjectTypes list means "all types".
func (c *Config) AllowsProjectType(t string) bool {
	if c == nil || len(c.ProjectTypes) == 0 {
		return true
	}
	return slices.Contains(c.ProjectTypes, t)
}

// DefaultBashTimeoutSeconds is the per-bash-tool-call timeout used when
// BashTimeoutSeconds is not set in config.
const DefaultBashTimeoutSeconds = 300

// DefaultRetryWatchdog is the CLAUDE_CODE_RETRY_WATCHDOG value used when
// RetryWatchdog is not set in config. Exceeds the old CLAUDE_CODE_MAX_RETRIES
// cap of 15, appropriate for unattended server runs.
const DefaultRetryWatchdog = 30

// BashTimeoutMs returns the bash tool timeout in milliseconds, exported into
// the claude subprocess as BASH_DEFAULT_TIMEOUT_MS / BASH_MAX_TIMEOUT_MS.
func (c *Config) BashTimeoutMs() int {
	if c != nil && c.Agent.BashTimeoutSeconds > 0 {
		return c.Agent.BashTimeoutSeconds * 1000
	}
	return DefaultBashTimeoutSeconds * 1000
}

// RetryWatchdog returns the configured watchdog value, DefaultRetryWatchdog
// when unset (0), or 0 when explicitly disabled (negative value).
func (c *Config) RetryWatchdog() int {
	if c != nil && c.Agent.RetryWatchdog < 0 {
		return 0
	}
	if c != nil && c.Agent.RetryWatchdog > 0 {
		return c.Agent.RetryWatchdog
	}
	return DefaultRetryWatchdog
}

// DefaultMaxLogEvents returns the configured cap or 500 if unset.
func (c *Config) DefaultMaxLogEvents() int {
	if c != nil && c.Agent.MaxLogEvents > 0 {
		return c.Agent.MaxLogEvents
	}
	return 500
}

// DefaultLogRetentionDays returns the configured retention window for
// per-agent NDJSON logs, or 14 days if unset. A negative value disables
// age-based pruning (0-byte files are still removed).
func (c *Config) DefaultLogRetentionDays() int {
	if c == nil {
		return 14
	}
	if c.Agent.LogRetentionDays < 0 {
		return c.Agent.LogRetentionDays
	}
	if c.Agent.LogRetentionDays == 0 {
		return 14
	}
	return c.Agent.LogRetentionDays
}

// DefaultRequirePermissions returns the configured default, or true if unset.
func (c *Config) DefaultRequirePermissions() bool {
	if c != nil && c.Agent.RequirePermissions != nil {
		return *c.Agent.RequirePermissions
	}
	return true
}

// NormalizeHeadlessPermissionMode canonicalizes a headless permission mode value.
// Empty string maps to "bypass". "bypass" and "auto" pass through unchanged.
// Any other value is rejected with an error.
func NormalizeHeadlessPermissionMode(s string) (string, error) {
	// Trim + lowercase first: a formatting slip (`auto `, `Auto`) must not
	// silently fall through to the permissive bypass default and disable the
	// guardrail this feature exists to provide.
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "bypass":
		return "bypass", nil
	case "auto":
		return "auto", nil
	default:
		return "", fmt.Errorf("invalid headless_permission_mode %q (valid: bypass, auto)", s)
	}
}

// DefaultHeadlessPermissionMode returns the configured default headless permission
// mode, or "bypass" if unset. An invalid config value is logged and treated as
// "bypass" so a misconfigured server never silently switches posture.
func (c *Config) DefaultHeadlessPermissionMode() string {
	if c == nil || c.Agent.HeadlessPermissionMode == "" {
		return "bypass"
	}
	mode, err := NormalizeHeadlessPermissionMode(c.Agent.HeadlessPermissionMode)
	if err != nil {
		slog.Warn("config: invalid agent.headless_permission_mode; falling back to bypass", "value", c.Agent.HeadlessPermissionMode)
		return "bypass"
	}
	return mode
}

// SurviveRestartEnabled reports whether agent subprocesses should be
// detached to survive an app restart and reattached on the next startup.
// Defaults to true when unset.
func (c *Config) SurviveRestartEnabled() bool {
	if c != nil && c.Agent.SurviveRestart != nil {
		return *c.Agent.SurviveRestart
	}
	return true
}

// DefaultTestingMaxConcurrent bounds concurrent test-runner agents (each owns
// an isolated sandbox) when TestingConfig.MaxConcurrent is unset.
const DefaultTestingMaxConcurrent = 3

// DefaultTestingMaxAttempts bounds the testing → in-progress re-implementation
// loop when TestingConfig.MaxAttempts is unset.
const DefaultTestingMaxAttempts = 3

// TestingMaxConcurrent returns the configured cap or DefaultTestingMaxConcurrent.
func (c *Config) TestingMaxConcurrent() int {
	if c != nil && c.Testing.MaxConcurrent > 0 {
		return c.Testing.MaxConcurrent
	}
	return DefaultTestingMaxConcurrent
}

// TestingMaxAttempts returns the configured cap or DefaultTestingMaxAttempts.
func (c *Config) TestingMaxAttempts() int {
	if c != nil && c.Testing.MaxAttempts > 0 {
		return c.Testing.MaxAttempts
	}
	return DefaultTestingMaxAttempts
}

// PollDefaults exposes the resolved poll intervals (override-or-default) so the
// poll handlers don't each re-implement the fallback logic.
const (
	DefaultReviewsFastSeconds  = 120 // was 60
	DefaultReviewsSlowSeconds  = 600 // was 300
	DefaultIssuesSeconds       = 600 // was 300
	DefaultRenovateFastSeconds = 120 // was 60
	DefaultRenovateSlowSeconds = 600 // was 300
)

// RunsSearchPollers reports whether this machine owns the periodic GitHub
// search pollers. Secondary instances skip them to avoid double-billing a
// shared token.
func (c GitHubConfig) RunsSearchPollers() bool {
	return !strings.EqualFold(strings.TrimSpace(c.PollerRole), "secondary")
}

func (c GitHubConfig) reviewsFast() time.Duration {
	return secsOr(c.ReviewsFastSeconds, DefaultReviewsFastSeconds)
}

func (c GitHubConfig) reviewsSlow() time.Duration {
	return secsOr(c.ReviewsSlowSeconds, DefaultReviewsSlowSeconds)
}

// ReviewsFast/ReviewsSlow/Issues/RenovateFast/RenovateSlow return resolved poll
// intervals (override or raised default).
func (c GitHubConfig) ReviewsFast() time.Duration { return c.reviewsFast() }
func (c GitHubConfig) ReviewsSlow() time.Duration { return c.reviewsSlow() }
func (c GitHubConfig) Issues() time.Duration {
	return secsOr(c.IssuesSeconds, DefaultIssuesSeconds)
}
func (c GitHubConfig) RenovateFast() time.Duration {
	return secsOr(c.RenovateFastSeconds, DefaultRenovateFastSeconds)
}
func (c GitHubConfig) RenovateSlow() time.Duration {
	return secsOr(c.RenovateSlowSeconds, DefaultRenovateSlowSeconds)
}

func secsOr(v, def int) time.Duration {
	if v <= 0 {
		v = def
	}
	return time.Duration(v) * time.Second
}

// DefaultHumanReviewMaxPerHour is the fallback rate-limit cap used when
// HumanReviewConfig.MaxPerHour is zero.
const DefaultHumanReviewMaxPerHour = 6

const (
	HumanReviewSybraBugActionFileIssue = "file_issue"
	HumanReviewSybraBugActionLocalTask = "local_task"
	HumanReviewSybraBugActionBlockOnly = "block_only"
	HumanReviewSybraBugActionNoteOnly  = "note_only"
)

// HumanReviewMaxPerHour returns the configured cap or the package default.
func (c *Config) HumanReviewMaxPerHour() int {
	if c != nil && c.HumanReview.MaxPerHour > 0 {
		return c.HumanReview.MaxPerHour
	}
	return DefaultHumanReviewMaxPerHour
}

// HumanReviewRepo returns the configured target repo or "Automaat/sybra".
func (c *Config) HumanReviewRepo() string {
	if c != nil && c.HumanReview.Repo != "" {
		return c.HumanReview.Repo
	}
	return "Automaat/sybra"
}

// HumanReviewModel returns the configured model alias or "sonnet".
func (c *Config) HumanReviewModel() string {
	if c != nil && c.HumanReview.Model != "" {
		return c.HumanReview.Model
	}
	return "sonnet"
}

// HumanReviewIssueLabel returns the configured label or "sybra-bug".
func (c *Config) HumanReviewIssueLabel() string {
	if c != nil && c.HumanReview.IssueLabel != "" {
		return c.HumanReview.IssueLabel
	}
	return "sybra-bug"
}

func (c *Config) HumanReviewSybraBugAction() string {
	if c == nil {
		return HumanReviewSybraBugActionFileIssue
	}
	switch strings.ToLower(strings.TrimSpace(c.HumanReview.SybraBugAction)) {
	case "", HumanReviewSybraBugActionFileIssue:
		return HumanReviewSybraBugActionFileIssue
	case HumanReviewSybraBugActionLocalTask:
		return HumanReviewSybraBugActionLocalTask
	case HumanReviewSybraBugActionBlockOnly:
		return HumanReviewSybraBugActionBlockOnly
	case HumanReviewSybraBugActionNoteOnly:
		return HumanReviewSybraBugActionNoteOnly
	default:
		slog.Warn("config: invalid human_review.sybra_bug_action; falling back to file_issue", "value", c.HumanReview.SybraBugAction)
		return HumanReviewSybraBugActionFileIssue
	}
}

func HomeDir() string {
	if dir := os.Getenv("SYBRA_HOME"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".sybra")
}

func DefaultConfig() *Config {
	return &Config{
		Logging: LoggingConfig{
			Level:     "info",
			Dir:       defaultLogDir(),
			MaxSizeMB: 50,
			MaxFiles:  5,
		},
		Audit: AuditConfig{
			Enabled:       true,
			RetentionDays: 30,
		},
		Agent: AgentDefaults{
			Provider:         "claude",
			MaxConcurrent:    100,
			MaxCostUSD:       5.0,
			MaxTurns:         150,
			DispatchJitterMs: 0,
		},
		Notification: NotificationConfig{
			Desktop: true,
		},
		Renovate: RenovateConfig{
			Enabled: true,
			Author:  "app/renovate",
		},
		GitHub: GitHubConfig{
			Enabled: true,
		},
		Monitor: MonitorConfig{
			Enabled: true,
		},
		HarnessEvolve: HarnessEvolveConfig{
			Enabled: true,
		},
		Watchdog: WatchdogConfig{
			Enabled:       true,
			LoopThreshold: 6,
		},
		ABTesting: abtest.DefaultConfig(),
		AutoUpdate: AutoUpdateConfig{
			Remote:              "origin",
			Branch:              "main",
			Mode:                "notify",
			PollSeconds:         300,
			RestartDelaySeconds: 2,
		},
		Providers: ProvidersConfig{
			HealthCheck: ProviderHealthCheckConfig{
				Enabled:         true,
				IntervalSeconds: 300,
			},
			Claude:  ProviderEntryConfig{Enabled: true, RateLimitCooldownSeconds: 900},
			Codex:   ProviderEntryConfig{Enabled: true, RateLimitCooldownSeconds: 900},
			Copilot: ProviderEntryConfig{Enabled: true, RateLimitCooldownSeconds: 900},
			Limits: ProviderLimitsConfig{
				Enabled:                 true,
				SessionThresholdPercent: 85,
				WeeklyThresholdPercent:  90,
				PreferUnderused:         true,
				BackfillDays:            14,
				MaxInFlightPerProvider:  0,
			},
			AutoFailover: true,
		},
		TasksDir: defaultTasksDir(),
	}
}

func (c *Config) AuditDir() string {
	return filepath.Join(c.Logging.Dir, "audit")
}

// Save writes the current config to disk.
func (c *Config) Save() error {
	path := configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// Directories returns the resolved paths for all sybra data directories.
func (c *Config) Directories() map[string]string {
	return map[string]string{
		"tasks":       c.TasksDir,
		"skills":      c.SkillsDir,
		"projects":    c.ProjectsDir,
		"clones":      c.ClonesDir,
		"worktrees":   c.WorktreesDir,
		"logs":        c.Logging.Dir,
		"audit":       c.AuditDir(),
		"loop_agents": c.LoopAgentsDir,
		"artifacts":   ArtifactsDir(),
		"experiences": c.ExperiencesDir(),
		"learning":    LearningDir(),
	}
}

// LearningDir is the directory under ~/.sybra that holds persisted Learning
// Digests (see internal/learning). Layout: <dir>/<hash>.json + latest.json.
func LearningDir() string {
	return filepath.Join(HomeDir(), "learning")
}

func (c *Config) ExperiencesDir() string {
	return filepath.Join(HomeDir(), "experience")
}

func Load() (*Config, error) {
	cfg := DefaultConfig()

	path := configPath()
	data, err := os.ReadFile(path)
	if err == nil {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	} else if os.IsNotExist(err) {
		if writeErr := writeDefaultConfig(path); writeErr != nil {
			return nil, writeErr
		}
	}

	if v := os.Getenv("SYBRA_LOG_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
	if v := os.Getenv("SYBRA_LOG_DIR"); v != "" {
		cfg.Logging.Dir = v
	}

	if cfg.Logging.Dir == "" {
		cfg.Logging.Dir = defaultLogDir()
	}
	if cfg.TasksDir == "" {
		cfg.TasksDir = defaultTasksDir()
	}
	if v := os.Getenv("SYBRA_TASKS_DIR"); v != "" {
		cfg.TasksDir = v
	}

	if cfg.SkillsDir == "" {
		cfg.SkillsDir = defaultSkillsDir()
	}

	// Migration: previous releases defaulted to ~/.sybra/skills which Claude
	// Code never reads. Silently retarget the old default so users with stale
	// configs get the fix without manual intervention. cdb6dc5 changed the
	// default but did not migrate persisted overrides.
	if cfg.SkillsDir == filepath.Join(HomeDir(), "skills") {
		cfg.SkillsDir = defaultSkillsDir()
	}
	if cfg.ProjectsDir == "" {
		cfg.ProjectsDir = defaultProjectsDir()
	}
	if cfg.ClonesDir == "" {
		cfg.ClonesDir = defaultClonesDir()
	}
	if cfg.WorktreesDir == "" {
		cfg.WorktreesDir = defaultWorktreesDir()
	}
	if cfg.LoopAgentsDir == "" {
		cfg.LoopAgentsDir = defaultLoopAgentsDir()
	}

	if v := os.Getenv("SYBRA_TODOIST_TOKEN"); v != "" {
		cfg.Todoist.APIToken = v
	}
	if cfg.Todoist.PollSeconds <= 0 {
		cfg.Todoist.PollSeconds = 120
	}

	if cfg.Renovate.Author == "" {
		cfg.Renovate.Author = "app/renovate"
	}
	if cfg.Triage.PollSeconds <= 0 {
		cfg.Triage.PollSeconds = 60
	}
	if cfg.Triage.Model == "" {
		cfg.Triage.Model = "sonnet"
	}
	if cfg.Agent.Provider == "" {
		cfg.Agent.Provider = "claude"
	}

	applyProvidersDefaults(cfg)
	applyMonitorDefaults(cfg)
	applyWatchdogDefaults(cfg)
	applySelfMonitorDefaults(cfg)
	applyEvaluationDefaults(cfg)
	applyLearningDigestDefaults(cfg)
	applyHarnessEvolveDefaults(cfg)
	applyPromptLabDefaults(cfg)
	applyExperienceDefaults(cfg)
	applyABTestingDefaults(cfg)
	applyOrchestratorDefaults(cfg)
	applyAutoUpdateDefaults(cfg)
	applyReviewHoldDefaults(cfg)

	return cfg, nil
}

// Review-hold modes control how far the hold extends to the fix-review agent's
// own code changes once its replies are drafted into a pending review.
const (
	ReviewHoldModePush     = "push"      // reply held; code still pushed
	ReviewHoldModePushNits = "push_nits" // reply held; code pushed only when a nit
	ReviewHoldModeHold     = "hold"      // reply held; code held too (no push)

	DefaultReviewHoldMode        = ReviewHoldModePush
	DefaultReviewHoldNitMaxLines = 10
)

// applyReviewHoldDefaults fills the mode/threshold only when the hold is
// enabled, so a disabled block stays at its zero value (and off).
func applyReviewHoldDefaults(cfg *Config) {
	if !cfg.ReviewHold.Enabled {
		return
	}
	if !validReviewHoldMode(cfg.ReviewHold.Mode) {
		cfg.ReviewHold.Mode = DefaultReviewHoldMode
	}
	if cfg.ReviewHold.NitMaxLines <= 0 {
		cfg.ReviewHold.NitMaxLines = DefaultReviewHoldNitMaxLines
	}
}

func validReviewHoldMode(mode string) bool {
	switch mode {
	case ReviewHoldModePush, ReviewHoldModePushNits, ReviewHoldModeHold:
		return true
	default:
		return false
	}
}

// ReviewHoldEnabled reports whether Sybra must draft PR comment replies as a
// pending review instead of posting them live. Nil-safe for test construction.
func (c *Config) ReviewHoldEnabled() bool {
	return c != nil && c.ReviewHold.Enabled
}

// ReviewHoldMode returns the resolved hold mode, falling back to the default for
// empty/unknown values so callers never branch on an invalid mode.
func (c *Config) ReviewHoldMode() string {
	if c == nil || !validReviewHoldMode(c.ReviewHold.Mode) {
		return DefaultReviewHoldMode
	}
	return c.ReviewHold.Mode
}

// ReviewHoldNitMaxLines returns the resolved nit ceiling for push_nits mode.
func (c *Config) ReviewHoldNitMaxLines() int {
	if c == nil || c.ReviewHold.NitMaxLines <= 0 {
		return DefaultReviewHoldNitMaxLines
	}
	return c.ReviewHold.NitMaxLines
}

func applyExperienceDefaults(cfg *Config) {
	if cfg.Experience.MaxRecords <= 0 {
		cfg.Experience.MaxRecords = 5
	}
}

func applyAutoUpdateDefaults(cfg *Config) {
	if cfg.AutoUpdate.Remote == "" {
		cfg.AutoUpdate.Remote = "origin"
	}
	if cfg.AutoUpdate.Branch == "" {
		cfg.AutoUpdate.Branch = "main"
	}
	if cfg.AutoUpdate.Mode == "" {
		cfg.AutoUpdate.Mode = "notify"
	}
	if cfg.AutoUpdate.PollSeconds <= 0 {
		cfg.AutoUpdate.PollSeconds = 300
	}
	if cfg.AutoUpdate.RestartDelaySeconds <= 0 {
		cfg.AutoUpdate.RestartDelaySeconds = 2
	}
}

func applyABTestingDefaults(cfg *Config) {
	if cfg == nil {
		return
	}
	def := abtest.DefaultConfig()
	if cfg.ABTesting.Enabled == nil {
		cfg.ABTesting.Enabled = def.Enabled
	}
	if cfg.ABTesting.MinSamplesPerVariant <= 0 {
		cfg.ABTesting.MinSamplesPerVariant = def.MinSamplesPerVariant
	}
	if len(cfg.ABTesting.Experiments) == 0 {
		cfg.ABTesting.Experiments = def.Experiments
	}
}

// applyWatchdogDefaults fills the Watchdog model default. Enabled and
// LoopThreshold are seeded by DefaultConfig (true / 6), so a config missing the
// watchdog block keeps the always-on default while an explicit `enabled: false`
// or `loop_threshold: 0` survives — the latter being the documented way to keep
// the watchdog running with the real-time loop trigger off. LoopThreshold is
// deliberately NOT defaulted here so an explicit 0 is not clobbered back to 6.
func applyWatchdogDefaults(cfg *Config) {
	w := &cfg.Watchdog
	if w.Model == "" {
		w.Model = "claude-haiku-4-5-20251001"
	}
}

// applyEvaluationDefaults fills zero values for the Evaluation block so older
// configs behave deterministically. Enabled stays false until operators opt in.
func applyEvaluationDefaults(cfg *Config) {
	e := &cfg.Evaluation
	if e.IntervalHours < 1 {
		e.IntervalHours = 24
	}
	if e.WindowDays <= 0 {
		e.WindowDays = 30
	}
	if e.Offline.Runner == "" {
		e.Offline.Runner = "auto"
	}
	if e.Offline.MinScore <= 0 {
		e.Offline.MinScore = 1.0
	}
	if e.Offline.UnavailablePolicy == "" {
		e.Offline.UnavailablePolicy = "fail"
	}
}

// applyLearningDigestDefaults fills zero values for the LearningDigest block.
// Enabled stays false until operators opt in.
func applyLearningDigestDefaults(cfg *Config) {
	d := &cfg.LearningDigest
	if d.IntervalHours < 1 {
		d.IntervalHours = 24
	}
	if d.WindowDays <= 0 {
		d.WindowDays = 7
	}
	if d.MaxWindowDays <= 0 {
		d.MaxWindowDays = 30
	}
	if d.Model == "" {
		d.Model = "sonnet"
	}
	if d.MinRuns <= 0 {
		d.MinRuns = 20
	}
	if d.MinLandings <= 0 {
		d.MinLandings = 3
	}
}

// applyHarnessEvolveDefaults fills zero values while preserving Enabled from
// DefaultConfig or an explicit YAML override.
func applyHarnessEvolveDefaults(cfg *Config) {
	h := &cfg.HarnessEvolve
	if h.IntervalHours < 1 {
		h.IntervalHours = 24
	}
	if h.LookbackHours <= 0 {
		h.LookbackHours = 168
	}
	if h.MinClusterSize <= 0 {
		h.MinClusterSize = 2
	}
	if h.Sink == "" {
		h.Sink = "local-task"
	}
}

// applyPromptLabDefaults fills zero values while preserving Enabled from an
// explicit YAML override — the zero value (false) is exactly the desired
// default, so no DefaultConfig entry is needed.
func applyPromptLabDefaults(cfg *Config) {
	p := &cfg.PromptLab
	if p.IntervalHours < 1 {
		p.IntervalHours = 24
	}
	if p.LookbackHours <= 0 {
		p.LookbackHours = 168
	}
	if p.MinSamples <= 0 {
		p.MinSamples = 5
	}
	if p.MinEffectSize <= 0 {
		p.MinEffectSize = 0.15
	}
	if p.MaxProposalsPerRun <= 0 {
		p.MaxProposalsPerRun = 3
	}
}

// applySelfMonitorDefaults fills zero values for the SelfMonitor block so
// older configs behave deterministically and the service can rely on every
// field. Enabled stays false until operators opt in.
func applySelfMonitorDefaults(cfg *Config) {
	s := &cfg.SelfMonitor
	if s.IntervalHours < 1 {
		s.IntervalHours = 6
	}
	if s.JudgeModel == "" {
		s.JudgeModel = "claude-haiku-4-5-20251001"
	}
	if s.SynthesizerModel == "" {
		s.SynthesizerModel = "claude-sonnet-4-6"
	}
	if s.MaxIssuesPerRun <= 0 {
		s.MaxIssuesPerRun = 5
	}
	if s.MaxAutoActionsPerDay <= 0 {
		s.MaxAutoActionsPerDay = 3
	}
	if len(s.AutoActCategories) == 0 {
		s.AutoActCategories = []string{
			"stuck_task",
			"workflow_loop",
			"cost_outlier",
			"triage_mismatch",
		}
	}

	// DryRun defaults to true as the first-week safety net. Operators flip
	// it to false once the ledger shows clean ActionRecords. Because bool
	// zero-values are indistinguishable from explicit false, we only flip
	// to true when the whole SelfMonitor block is freshly populated — i.e.
	// when none of the user-facing knobs were set. This avoids silently
	// re-enabling DryRun on an operator who explicitly disabled it.
	//
	// Proxy for "freshly populated": IssueLabel is the last field the
	// operator typically edits; if it's empty after the above defaults
	// ran, we know nothing in the block was user-specified.
	if s.IssueCooldownHours <= 0 {
		s.IssueCooldownHours = 24
	}
	if s.IssueLabel == "" {
		s.IssueLabel = "selfmonitor"
		s.DryRun = true
	}
	if s.MaxCostPerTickUSD <= 0 {
		s.MaxCostPerTickUSD = 2.0
	}
	if s.JudgeParallelism <= 0 {
		s.JudgeParallelism = 4
	}
	if s.SuppressionDays <= 0 {
		s.SuppressionDays = 7
	}
	if s.SuppressionThreshold <= 0 {
		s.SuppressionThreshold = 3
	}
}

// applyOrchestratorDefaults fills zero values for the Orchestrator block so the
// dispatch loop has a fast scheduling cadence and a slower maintenance cadence
// even on configs that predate the split.
func applyOrchestratorDefaults(cfg *Config) {
	if cfg.Orchestrator.DispatchIntervalSeconds <= 0 {
		cfg.Orchestrator.DispatchIntervalSeconds = 10
	}
	if cfg.Orchestrator.MaintenanceIntervalSeconds <= 0 {
		cfg.Orchestrator.MaintenanceIntervalSeconds = 60
	}
}

// applyMonitorDefaults fills zero values for the Monitor block so older
// configs behave deterministically and the service can rely on every field.
// Enabled stays false until users opt in.
func applyMonitorDefaults(cfg *Config) {
	if cfg.Monitor.IntervalSeconds < 60 {
		cfg.Monitor.IntervalSeconds = 300
	}
	if cfg.Monitor.Model == "" {
		cfg.Monitor.Model = "sonnet"
	}
	if cfg.Monitor.IssueCooldownMinutes <= 0 {
		cfg.Monitor.IssueCooldownMinutes = 30
	}
	if cfg.Monitor.DispatchLimit <= 0 {
		cfg.Monitor.DispatchLimit = cfg.Agent.MaxConcurrent
	}
	if cfg.Monitor.StuckHumanHours <= 0 {
		cfg.Monitor.StuckHumanHours = 8
	}
	if cfg.Monitor.LostAgentMinutes <= 0 {
		cfg.Monitor.LostAgentMinutes = 15
	}
	if cfg.Monitor.PRGapGraceMinutes <= 0 {
		cfg.Monitor.PRGapGraceMinutes = 15
	}
	if cfg.Monitor.FailureRateThreshold <= 0 {
		cfg.Monitor.FailureRateThreshold = 0.3
	}
	if cfg.Monitor.IssueLabel == "" {
		cfg.Monitor.IssueLabel = "monitor"
	}
	if cfg.Monitor.IssueRepo == "" {
		cfg.Monitor.IssueRepo = "Automaat/sybra"
	}
	if cfg.Monitor.BottleneckHours == nil {
		cfg.Monitor.BottleneckHours = map[string]float64{}
	}
	if _, ok := cfg.Monitor.BottleneckHours["plan-review"]; !ok {
		cfg.Monitor.BottleneckHours["plan-review"] = 4
	}
	if _, ok := cfg.Monitor.BottleneckHours["human-required"]; !ok {
		cfg.Monitor.BottleneckHours["human-required"] = 8
	}
	if _, ok := cfg.Monitor.BottleneckHours["in-progress"]; !ok {
		cfg.Monitor.BottleneckHours["in-progress"] = 6
	}
	if _, ok := cfg.Monitor.BottleneckHours["default"]; !ok {
		cfg.Monitor.BottleneckHours["default"] = 12
	}
}

// applyProvidersDefaults fills zero values for the Providers block so older
// configs (which predate the block entirely) behave identically to the
// DefaultConfig factory.
func applyProvidersDefaults(cfg *Config) {
	if cfg.Providers.HealthCheck.IntervalSeconds <= 0 {
		cfg.Providers.HealthCheck.IntervalSeconds = 300
	}
	if cfg.Providers.HealthCheck.IntervalSeconds < 60 {
		cfg.Providers.HealthCheck.IntervalSeconds = 60
	}
	if cfg.Providers.Claude.RateLimitCooldownSeconds <= 0 {
		cfg.Providers.Claude.RateLimitCooldownSeconds = 900
	}
	if cfg.Providers.Codex.RateLimitCooldownSeconds <= 0 {
		cfg.Providers.Codex.RateLimitCooldownSeconds = 900
	}
	if cfg.Providers.Copilot.RateLimitCooldownSeconds <= 0 {
		cfg.Providers.Copilot.RateLimitCooldownSeconds = 900
	}
	if cfg.Providers.Limits.SessionThresholdPercent <= 0 {
		cfg.Providers.Limits.SessionThresholdPercent = 85
	}
	if cfg.Providers.Limits.WeeklyThresholdPercent <= 0 {
		cfg.Providers.Limits.WeeklyThresholdPercent = 90
	}
	if cfg.Providers.Limits.BackfillDays <= 0 {
		cfg.Providers.Limits.BackfillDays = 14
	}
}

func (c *LoggingConfig) SlogLevel() slog.Level {
	switch c.Level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func writeDefaultConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("# Sybra configuration\n# All values are optional — defaults apply when omitted.\n"), 0o644)
}

func configPath() string {
	return filepath.Join(HomeDir(), "config.yaml")
}

func defaultLogDir() string {
	return filepath.Join(HomeDir(), "logs")
}

func defaultTasksDir() string {
	return filepath.Join(HomeDir(), "tasks")
}

func defaultSkillsDir() string {
	return filepath.Join(HomeDir(), ".claude", "skills")
}

func defaultProjectsDir() string {
	return filepath.Join(HomeDir(), "projects")
}

func defaultClonesDir() string {
	return filepath.Join(HomeDir(), "clones")
}

func defaultWorktreesDir() string {
	return filepath.Join(HomeDir(), "worktrees")
}

func defaultLoopAgentsDir() string {
	return filepath.Join(HomeDir(), "loop-agents")
}

func WorkflowsDir() string {
	return filepath.Join(HomeDir(), "workflows")
}

// AgentsDir is the directory under ~/.sybra that holds the live-agent
// registry (one YAML file per running agent) used to reattach to
// subprocesses that survived an app restart.
func AgentsDir() string {
	return filepath.Join(HomeDir(), "agents")
}

func StatsFile() string {
	return filepath.Join(HomeDir(), "stats.json")
}

func LimitsFile() string {
	return filepath.Join(HomeDir(), "limits.json")
}

// ArtifactsDir is the directory under ~/.sybra that holds per-task harness
// artifacts (plan snapshots, trace events, generic intermediate outputs).
// Layout: <dir>/<task-id>/<name> + <name>.meta.json.
func ArtifactsDir() string {
	return filepath.Join(HomeDir(), "artifacts")
}

// SelfMonitorDir is the directory under ~/.sybra that holds the
// selfmonitor ledger, last-report snapshot, and any other persisted state
// the service owns.
func SelfMonitorDir() string {
	return filepath.Join(HomeDir(), "selfmonitor")
}

// HarnessEvolveDir is the local store for governed harness-evolution proposal
// records and run snapshots.
func HarnessEvolveDir() string {
	return filepath.Join(HomeDir(), "harness-evolution")
}

// PromptLabDir is the local store for promptlab run snapshots and scaffolded
// proposal records. It never holds authored prompt/skill text — see
// internal/promptlab.
func PromptLabDir() string {
	return filepath.Join(HomeDir(), "prompt-lab")
}

// PromptEvalDir is the local store for offline prompt/skill eval verdicts
// (internal/prompteval). Layout: <dir>/<variantID>/<digest>.json.
func PromptEvalDir() string {
	return filepath.Join(HomeDir(), "prompteval")
}

// SelfMonitorLedgerPath is the append-only ledger file selfmonitor.Open uses.
func SelfMonitorLedgerPath() string {
	return filepath.Join(SelfMonitorDir(), "ledger.jsonl")
}

// EvaluationReportPath is where the background evaluation service persists its
// most recent scorecard as JSON for the dashboard and inspection. The CLI
// `sybra-cli evaluation scan` recomputes a fresh report rather than reading it.
func EvaluationReportPath() string {
	return filepath.Join(HomeDir(), "evaluation-report.json")
}

// SelfMonitorLastReportPath is where the service writes the most recent
// Report as JSON. The CLI `sybra-cli selfmonitor scan` reads from here.
func SelfMonitorLastReportPath() string {
	return filepath.Join(SelfMonitorDir(), "last-report.json")
}

// HealthReportPath is the canonical path the health.Checker persists its
// rollup report to. Exposed here so CLI commands (and the selfmonitor
// service) can read it without hardcoding the layout.
func HealthReportPath() string {
	return filepath.Join(HomeDir(), "health-report.json")
}
