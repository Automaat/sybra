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
	// MaxAttempts caps how many times a task may fail testing and bounce back
	// to in-progress for re-implementation before it is escalated to
	// human-required instead. 0 falls back to DefaultTestingMaxAttempts.
	MaxAttempts int `yaml:"max_attempts" json:"maxAttempts"`
}
