package config

// DefaultServerPort is the control plane's listen port when neither env nor
// cluster.bind_addr says otherwise.
const DefaultServerPort = "8080"

// ServerConfig controls sybra-server's HTTP control-plane authentication and
// CORS policy. AuthToken gates every request except GET /health behind a
// shared-secret bearer token: callers must send
// `Authorization: Bearer <token>`, or, for the SSE endpoints only (browser
// EventSource cannot set request headers), a `?token=<token>` query param.
// AuthToken is auto-generated and persisted to config.yaml on first run if
// left empty — see applyServerDefaults.
type ServerConfig struct {
	AuthToken      string   `yaml:"auth_token" json:"authToken"`
	AllowedOrigins []string `yaml:"allowed_origins" json:"allowedOrigins"`
}
