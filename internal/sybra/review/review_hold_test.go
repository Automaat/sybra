package review

import (
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/config"
)

func TestReviewHoldFixSuffix(t *testing.T) {
	t.Parallel()

	// Common assertions for any enabled suffix: never post live, draft a pending
	// review, and note that Sybra parks the task (routing is deterministic — the
	// suffix must not itself instruct a sentinel that the appended contract can
	// override).
	assertEnabled := func(t *testing.T, s string) {
		t.Helper()
		for _, want := range []string{
			"REVIEW HOLD",
			"Do NOT post live thread replies",
			"PENDING",
			"human-required",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("suffix missing %q:\n%s", want, s)
			}
		}
		if strings.Contains(s, "SYBRA_PR_FIX_RESULT") {
			t.Errorf("suffix must not instruct the result sentinel (routing is deterministic):\n%s", s)
		}
	}

	tests := []struct {
		name    string
		cfg     *config.Config
		enabled bool
		wantIn  []string
		notIn   []string
	}{
		{
			name:    "nil config is disabled",
			cfg:     nil,
			enabled: false,
		},
		{
			name:    "disabled block yields empty suffix",
			cfg:     &config.Config{},
			enabled: false,
		},
		{
			name:    "push mode still pushes",
			cfg:     &config.Config{ReviewHold: config.ReviewHoldConfig{Enabled: true, Mode: config.ReviewHoldModePush}},
			enabled: true,
			wantIn:  []string{"Push your code fixes to the PR branch as usual"},
		},
		{
			name:    "hold mode blocks the push",
			cfg:     &config.Config{ReviewHold: config.ReviewHoldConfig{Enabled: true, Mode: config.ReviewHoldModeHold}},
			enabled: true,
			wantIn:  []string{"Do NOT push"},
		},
		{
			name:    "push_nits mode names the resolved threshold",
			cfg:     &config.Config{ReviewHold: config.ReviewHoldConfig{Enabled: true, Mode: config.ReviewHoldModePushNits, NitMaxLines: 25}},
			enabled: true,
			wantIn:  []string{"at most 25", "ONLY if"},
		},
		{
			name:    "empty mode falls back to push",
			cfg:     &config.Config{ReviewHold: config.ReviewHoldConfig{Enabled: true}},
			enabled: true,
			wantIn:  []string{"as usual"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := reviewHoldFixSuffix(tt.cfg)
			if !tt.enabled {
				if got != "" {
					t.Fatalf("disabled hold must yield empty suffix, got:\n%s", got)
				}
				return
			}
			assertEnabled(t, got)
			for _, want := range tt.wantIn {
				if !strings.Contains(got, want) {
					t.Errorf("suffix missing %q:\n%s", want, got)
				}
			}
			for _, no := range tt.notIn {
				if strings.Contains(got, no) {
					t.Errorf("suffix unexpectedly contains %q:\n%s", no, got)
				}
			}
		})
	}
}
