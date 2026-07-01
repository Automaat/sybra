package config

type LoggingConfig struct {
	Level     string `yaml:"level" json:"level"`
	Dir       string `yaml:"dir" json:"dir"`
	MaxSizeMB int    `yaml:"max_size_mb" json:"maxSizeMB"`
	MaxFiles  int    `yaml:"max_files" json:"maxFiles"`
}
