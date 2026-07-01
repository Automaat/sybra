package config

type AuditConfig struct {
	Enabled       bool `yaml:"enabled" json:"enabled"`
	RetentionDays int  `yaml:"retention_days" json:"retentionDays"`
}
