package config

type RenovateConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Author  string `yaml:"author" json:"author"`
}
