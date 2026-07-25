package config

// AdmissionConfig gates the workflow engine's admission_preflight step (see
// internal/workflow/engine_steps_admission.go), a deterministic pre-dispatch
// check that rejects a task's plan contract for missing admission facts
// (objective, unrecognized required_capabilities), oversized scope, or a
// failing push-credential preflight — before any code-author agent is
// dispatched.
type AdmissionConfig struct {
	// Enabled turns the admission_preflight step's checks on. Defaults true
	// (see applyAdmissionDefaults) — safe because a contract with no
	// schema_version (every contract generated before this feature shipped)
	// validates exactly as it did before this migration, and the size limits
	// below default to 0 (disabled), so enabling this does not retroactively
	// block any in-flight task.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// MaxAcceptanceCriteria caps a plan contract's acceptance_criteria count
	// before admission_preflight flags the task as oversized (an
	// operator_decision human-required, not an automatic split — auto-split
	// is deferred, see #2466 Fix point 5). Zero disables the limit.
	MaxAcceptanceCriteria int `yaml:"max_acceptance_criteria" json:"maxAcceptanceCriteria"`
	// MaxChangeSurfaceFiles caps a plan contract's files count the same way.
	// Zero disables the limit.
	MaxChangeSurfaceFiles int `yaml:"max_change_surface_files" json:"maxChangeSurfaceFiles"`
}
