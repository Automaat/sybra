package config

import (
	"slices"
	"testing"
)

// Callers reporting capacity need the same ordered chain the dispatcher walks,
// or a one-leg chain is not recognisable as one.
func TestProvidersConfig_EnabledNames(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  ProvidersConfig
		want []string
	}{
		{name: "none enabled", want: nil},
		{
			name: "preference order, not declaration order",
			cfg: ProvidersConfig{
				OpenCode: ProviderEntryConfig{Enabled: true},
				Copilot:  ProviderEntryConfig{Enabled: true},
				Codex:    ProviderEntryConfig{Enabled: true},
				Claude:   ProviderEntryConfig{Enabled: true},
			},
			want: []string{"claude", "codex", "copilot", "opencode"},
		},
		{
			name: "gaps close without reordering",
			cfg: ProvidersConfig{
				Claude:   ProviderEntryConfig{Enabled: true},
				OpenCode: ProviderEntryConfig{Enabled: true},
			},
			want: []string{"claude", "opencode"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cfg.EnabledNames(); !slices.Equal(got, tc.want) {
				t.Errorf("EnabledNames() = %v, want %v", got, tc.want)
			}
		})
	}
}
