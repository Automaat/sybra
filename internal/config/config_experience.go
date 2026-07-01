package config

type ExperienceConfig struct {
	Enabled    bool `yaml:"enabled" json:"enabled"`
	MaxRecords int  `yaml:"max_records" json:"maxRecords"`
}
