package config

import (
	"strings"
	"testing"
)

func TestValidateResolvedConfigGitHubWebhookCommandPrefix(t *testing.T) {
	tests := []struct {
		name    string
		prefix  string
		wantErr bool
	}{
		{name: "default", prefix: "/sybra"},
		{name: "custom", prefix: "/review-agent"},
		{name: "missing slash", prefix: "sybra", wantErr: true},
		{name: "slash only", prefix: "/", wantErr: true},
		{name: "whitespace", prefix: "/review agent", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.GitHub.Webhook.CommandPrefix = tc.prefix
			err := ValidateResolvedConfig(cfg)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "github.webhook.command_prefix") {
					t.Fatalf("ValidateResolvedConfig() err = %v, want command-prefix error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateResolvedConfig() err = %v", err)
			}
		})
	}
}
