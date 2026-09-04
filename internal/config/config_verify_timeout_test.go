package config

import (
	"testing"
	"time"
)

// TestVerifyTimeout covers the budget an operator can now name. Before this
// key existed a repo whose suite outgrew the compiled-in 10 minutes had no way
// to say so: the task blocked on a timeout that blamed slow tests, and the
// only documented escape skipped verification for that task entirely.
func TestVerifyTimeout(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  *Config
		want time.Duration
	}{
		{name: "nil config", cfg: nil, want: DefaultVerifyTimeoutMinutes * time.Minute},
		{name: "unset falls back", cfg: &Config{}, want: DefaultVerifyTimeoutMinutes * time.Minute},
		{name: "configured wins", cfg: &Config{Testing: TestingConfig{VerifyTimeoutMinutes: 25}}, want: 25 * time.Minute},
		// Negative is meaningless for a budget and must not disable the gate.
		{name: "negative falls back", cfg: &Config{Testing: TestingConfig{VerifyTimeoutMinutes: -5}}, want: DefaultVerifyTimeoutMinutes * time.Minute},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cfg.VerifyTimeout(); got != tc.want {
				t.Errorf("VerifyTimeout() = %s, want %s", got, tc.want)
			}
		})
	}
}
