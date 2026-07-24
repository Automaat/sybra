package config

// DefaultWebhookPort is the inbound webhook listener's default port when the
// feature is enabled and webhook.port is unset or invalid.
const DefaultWebhookPort = 8081

// WebhookConfig controls the optional inbound HTTP webhook used for external
// task creation. When Enabled is true, sybra-server starts a separate listener
// on Port and serves POST /webhook/task. Secret optionally enables HMAC-SHA256
// request signing via X-Sybra-Signature: sha256=<hex>.
type WebhookConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Port    int    `yaml:"port" json:"port"`
	Secret  string `yaml:"secret" json:"secret" secret:"true"`
}
