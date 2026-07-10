package config

type ExperienceConfig struct {
	Enabled    bool `yaml:"enabled" json:"enabled"`
	MaxRecords int  `yaml:"max_records" json:"maxRecords"`
	// TTLDays expires records older than this many days out of injection —
	// a stale record can otherwise poison a prompt with advice that no
	// longer matches the current codebase. 0 (default) disables expiry, so
	// existing deployments are unaffected until an operator opts in.
	TTLDays int `yaml:"ttl_days" json:"ttlDays"`
}
