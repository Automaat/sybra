package config

import (
	"strings"
	"testing"
)

func TestValidateResolvedConfig_SLOTargetBounds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*SLOTargets)
		wantMsg string
	}{
		{
			name:    "negative retry cap",
			mutate:  func(s *SLOTargets) { s.MaxIdenticalRetryCap = -1 },
			wantMsg: "evaluation.slo.max_identical_retry_cap must be 0 or greater",
		},
		{
			name:    "negative restarts per hour",
			mutate:  func(s *SLOTargets) { s.MaxRestartsPerHour = -0.5 },
			wantMsg: "evaluation.slo.max_restarts_per_hour must be 0 or greater",
		},
		{
			name:    "autonomy rate above 1",
			mutate:  func(s *SLOTargets) { s.MinAutonomyRate = 1.5 },
			wantMsg: "evaluation.slo.min_autonomy_rate must be between 0 and 1",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			c.mutate(&cfg.Evaluation.SLO)
			err := ValidateResolvedConfig(cfg)
			if err == nil {
				t.Fatalf("ValidateResolvedConfig() err = nil, want error containing %q", c.wantMsg)
				panic("unreachable")
			}
			if msgs := ValidationMessages(err); !containsMsg(msgs, c.wantMsg) {
				t.Fatalf("messages = %v, want one containing %q", msgs, c.wantMsg)
			}
		})
	}
}

func containsMsg(msgs []string, want string) bool {
	for _, m := range msgs {
		if strings.Contains(m, want) {
			return true
		}
	}
	return false
}
