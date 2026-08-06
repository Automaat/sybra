package sybra

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/config"
)

// A one-leg failover chain has no failover at all, and that is how one weekly
// limit plus one usage limit turned into a dead board. Startup is where an
// operator can still act on it.
func TestWarnThinFailoverChain(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		enabled []string
		wantMsg string
	}{
		{name: "two providers are silent", enabled: []string{"claude", "codex"}},
		{name: "one provider warns", enabled: []string{"claude"}, wantMsg: "app.providers.no-failover"},
		// Distinct from one provider: there is no "single rate limit" to blame
		// when nothing is enabled, and the instance cannot dispatch at all.
		{name: "none enabled is its own error", wantMsg: "app.providers.none-enabled"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{}
			for _, name := range tc.enabled {
				switch name {
				case "claude":
					cfg.Providers.Claude.Enabled = true
				case "codex":
					cfg.Providers.Codex.Enabled = true
				}
			}
			records := make([]slog.Record, 0)
			a := &App{cfg: cfg, logger: slog.New(&recordHandler{records: &records})}

			a.warnThinFailoverChain()

			var got string
			for _, r := range records {
				switch r.Message {
				case "app.providers.no-failover", "app.providers.none-enabled":
					got = r.Message
				}
			}
			if got != tc.wantMsg {
				t.Errorf("logged %q, want %q", got, tc.wantMsg)
			}
		})
	}
}

// The warning only helps if startup reaches it, and startup reaches it through
// the automations summary — the line an operator already reads to see what a
// machine is running. Capacity belongs next to it.
func TestLogAutomationsSummary_CarriesProvidersAndWarning(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	cfg.Providers.Claude.Enabled = true

	records := make([]slog.Record, 0)
	a := &App{cfg: cfg, logger: slog.New(&recordHandler{records: &records})}

	a.logAutomationsSummary()

	var summary, warned bool
	providers := "<absent>"
	for _, r := range records {
		switch r.Message {
		case "app.automations":
			summary = true
			r.Attrs(func(attr slog.Attr) bool {
				if attr.Key == "providers" {
					providers = attr.Value.String()
				}
				return true
			})
		case "app.providers.no-failover", "app.providers.none-enabled":
			warned = true
		}
	}
	if !summary {
		t.Fatal("no app.automations line")
	}
	if !strings.Contains(providers, "claude") {
		t.Errorf("providers attr = %s, want the enabled chain", providers)
	}
	if !warned {
		t.Error("summary did not reach the failover warning")
	}
}
