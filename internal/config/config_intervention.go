package config

// InterventionConfig gates capture of genuine human-required unblocks into a
// normalized, fingerprint-deduplicated intervention record (see
// internal/intervention) — advisory memory for a future replay fixture
// (sybra#2454), never a deterministic gate.
type InterventionConfig struct {
	// Enabled turns capture on. Defaults true (see applyInterventionDefaults)
	// — safe because records are local-only, scrub-guarded for work
	// projects, and feed no routing/admission/completion decision.
	Enabled bool `yaml:"enabled" json:"enabled"`
}
