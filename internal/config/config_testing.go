package config

// TestingConfig controls the autonomous manual-testing phase. A task entering
// status=testing spawns a single adversarial test-runner agent that starts the
// real app/cluster in an isolated per-task sandbox and tries to prove the
// implementation does not satisfy the task. Each test-runner holds its own
// sandbox (Docker compose project / k3d cluster), so MaxConcurrent bounds
// real-app/cluster load independently of Agent.MaxConcurrent.
type TestingConfig struct {
	// MaxConcurrent caps simultaneously-running test-runner agents on this
	// machine. 0 falls back to DefaultTestingMaxConcurrent.
	MaxConcurrent int `yaml:"max_concurrent" json:"maxConcurrent"`
	// MaxAttempts is the generous absolute backstop for the testing →
	// re-implementation loop. Recurring grounded failure fingerprints escalate
	// independently of this count; this limit only parks a task human-required
	// when distinct grounded defects keep surfacing without convergence. 0
	// falls back to DefaultTestingMaxAttempts.
	MaxAttempts int `yaml:"max_attempts" json:"maxAttempts"`
	// OpenPROnUnrunnableGate controls what happens when a testing cycle
	// exhausts its auto-retry budget on an infra_failure outcome — the manual
	// gate itself could not be run (harness/tooling limitation), not a
	// product defect (see classifyTestOutcome). nil means not configured
	// (defaults to true): the task opens a PR (ready-pr) so CI and a human
	// reviewer see the real diff, instead of parking at human-required with
	// no PR for anyone to act on. Set false to restore the legacy
	// human-required escalation.
	OpenPROnUnrunnableGate *bool `yaml:"open_pr_on_unrunnable_gate" json:"openPrOnUnrunnableGate"`
}
