package config

import (
	"testing"
	"time"
)

func TestDefaultSandboxRetention(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		cfg          *Config
		wantWindow   time.Duration
		wantDisabled bool
	}{
		{"nil config → 24h", nil, 24 * time.Hour, false},
		{"unset → 24h", &Config{}, 24 * time.Hour, false},
		{"explicit 6h → 6h", &Config{Sandbox: SandboxConfig{RetentionHours: 6}}, 6 * time.Hour, false},
		{"negative disables", &Config{Sandbox: SandboxConfig{RetentionHours: -1}}, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotWindow, gotDisabled := tc.cfg.DefaultSandboxRetention()
			if gotWindow != tc.wantWindow || gotDisabled != tc.wantDisabled {
				t.Errorf("got (%v, %v), want (%v, %v)", gotWindow, gotDisabled, tc.wantWindow, tc.wantDisabled)
			}
		})
	}
}
