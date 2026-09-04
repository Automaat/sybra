package config

// DefaultWebhookPort is the inbound GitHub webhook listener's default port.
const DefaultWebhookPort = 8081

// WebhookConfig is the deprecated top-level webhook shape. Resolve maps its
// fields into GitHubWebhookConfig so existing installations keep working.
type WebhookConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Port    int    `yaml:"port" json:"port"`
	Secret  string `yaml:"secret" json:"secret" secret:"true"`
}
