package config

// EvidenceConfig gates the workflow engine's require_evidence step (see
// internal/workflow/engine_steps_evidence.go), the final deterministic
// completion gate that blocks a task from landing until every criterion
// applicable to it (verify_checks, detect_tampering, the test-runner
// verdict, review) has fresh, passing evidence recorded for the task's
// current HEAD. Lives under AgentDefaults — resolved config key
// agent.evidence.enabled.
type EvidenceConfig struct {
	// Enabled turns the require_evidence gate on. Defaults false (see
	// applyEvidenceDefaults) — the underlying producers (verify_checks,
	// detect_tampering, codegen_gate, focused_checks, the test-runner, and
	// review) always record evidence regardless of this flag, so enabling it
	// later gates against history that was already being collected rather
	// than starting cold.
	Enabled bool `yaml:"enabled" json:"enabled"`
}
