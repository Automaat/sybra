package config

// EvaluationConfig controls the in-process evaluation service, which periodically
// computes a fleet scorecard (autonomy, throughput, reliability, efficiency) from
// stats + audit data. Read-only: it never dispatches agents or files issues, so
// it needs no project-type routing — each machine scores its own local data.
type EvaluationConfig struct {
	Enabled       bool              `yaml:"enabled" json:"enabled"`
	IntervalHours float64           `yaml:"interval_hours" json:"intervalHours"`
	WindowDays    int               `yaml:"window_days" json:"windowDays"`
	Offline       OfflineEvalConfig `yaml:"offline" json:"offline"`
	// SLO is the rolling autonomy/reliability target set the evaluation
	// scorecard is graded against — see internal/evaluation.EvaluateSLOs.
	// Zero-value fields are filled by applyEvaluationDefaults so existing
	// configs need no migration.
	SLO SLOTargets `yaml:"slo" json:"slo"`
}

// SLOTargets are the rolling autonomy/reliability targets Sybra holds
// itself to (see #2441). Defined here rather than in internal/evaluation
// because config cannot import evaluation (evaluation already imports
// config); internal/evaluation/slo.go aliases this type so evaluation code
// reads naturally as evaluation.SLOTargets.
type SLOTargets struct {
	// MinAutonomyRate is the minimum fraction of landed tasks that must
	// reach done without a human in the loop.
	MinAutonomyRate float64 `yaml:"min_autonomy_rate" json:"minAutonomyRate"`
	// MinCIFirstPassRate is the minimum fraction of landed tasks that must
	// pass CI on the first push (no CI-fix agent needed).
	MinCIFirstPassRate float64 `yaml:"min_ci_first_pass_rate" json:"minCiFirstPassRate"`
	// MaxReworkRate is the maximum fraction of landed tasks allowed to
	// bounce between statuses (a repeated status transition).
	MaxReworkRate float64 `yaml:"max_rework_rate" json:"maxReworkRate"`
	// MaxIdenticalRetryCap is the maximum number of failed agent runs a
	// single task may accumulate in-window before it counts as a breach.
	MaxIdenticalRetryCap int `yaml:"max_identical_retry_cap" json:"maxIdenticalRetryCap"`
	// MaxRestartsPerHour is the maximum rate of automatic human-required
	// recovery restarts (monitor auto-retry, PR-monitor blocker
	// reconciliation) the fleet may sustain.
	MaxRestartsPerHour float64 `yaml:"max_restarts_per_hour" json:"maxRestartsPerHour"`
	// ThrottleOnBudgetExhausted enables a default-off dispatch clamp
	// (internal/sybra/agentorch.effectiveMaxConcurrent) that narrows the
	// concurrency ceiling for new workflow-driven implementation dispatch
	// while the SLO error budget is exhausted. Recovery and
	// interactive/operator dispatch are never throttled by this flag.
	ThrottleOnBudgetExhausted bool `yaml:"throttle_on_budget_exhausted" json:"throttleOnBudgetExhausted"`
}

// DefaultSLOTargets returns the task-default SLO targets (#2441): autonomy
// and CI-first-pass at 80%, rework under 40%, at most 3 identical retries,
// and at most 1 automatic restart per hour. Used by applyEvaluationDefaults
// to fill any zero-value field, so an existing config.yaml predating this
// field needs no migration.
func DefaultSLOTargets() SLOTargets {
	return SLOTargets{
		MinAutonomyRate:      0.80,
		MinCIFirstPassRate:   0.80,
		MaxReworkRate:        0.40,
		MaxIdenticalRetryCap: 3,
		MaxRestartsPerHour:   1,
	}
}

// OfflineEvalConfig controls the offline prompt/skill eval runner
// (internal/prompteval) that screens candidate variants before they are
// eligible for online A/B enrollment.
type OfflineEvalConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Runner selects the offline runner: "auto" (default, promptfoo if on
	// PATH else native), "promptfoo", or "native".
	Runner     string `yaml:"runner" json:"runner"`
	BinaryPath string `yaml:"binary_path,omitempty" json:"binaryPath,omitempty"`
	// MinScore is the minimum Result.Score (0..1) required for a verdict to
	// be scored StatusPass.
	MinScore float64 `yaml:"min_score" json:"minScore"`
	// UnavailablePolicy is "fail" (default, fail-closed) or "pass" — what
	// AllowEnrollment returns when no verdict is recorded or the runner
	// could not measure a result.
	UnavailablePolicy string `yaml:"unavailable_policy" json:"unavailablePolicy"`
	// Mode is reserved for future dry-run/enforce distinctions; currently
	// informational only.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
}
