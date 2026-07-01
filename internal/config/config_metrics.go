package config

// MetricsConfig controls the OpenTelemetry metrics pipeline. When Enabled is
// true, sybra-server mounts /metrics on its existing mux and emits
// Prometheus-format output for external scrapers.
type MetricsConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}
