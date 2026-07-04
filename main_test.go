//go:build darwin

package main

import (
	"testing"

	"github.com/Automaat/sybra/internal/config"
)

func TestDesktopBrowserOptions(t *testing.T) {
	t.Parallel()

	truthy, falsy := true, false

	cases := []struct {
		name    string
		cfg     *config.Config
		wantLen int
	}{
		{"default/nil config wires the opener", nil, 1},
		{"nil field wires the opener", &config.Config{}, 1},
		{"explicit true wires the opener", &config.Config{Browser: config.BrowserConfig{InApp: &truthy}}, 1},
		{"explicit false omits the opener", &config.Config{Browser: config.BrowserConfig{InApp: &falsy}}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := desktopBrowserOptions(tc.cfg, func(string) {})
			if len(opts) != tc.wantLen {
				t.Errorf("desktopBrowserOptions() returned %d options, want %d", len(opts), tc.wantLen)
			}
		})
	}
}
