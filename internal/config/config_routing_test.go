package config

import (
	"testing"
	"time"
)

// TestRoutingEvaluationMaxAge covers the tri-state resolution: unset fills the
// 72h default, while an explicit non-positive value disables the freshness
// check (0 duration) and survives applyRoutingDefaults instead of being
// rewritten back to 72.
func TestRoutingEvaluationMaxAge(t *testing.T) {
	zero := 0.0
	twelve := 12.0

	tests := []struct {
		name  string
		field *float64
		want  time.Duration
	}{
		{"unset defaults to 72h", nil, 72 * time.Hour},
		{"explicit zero disables", &zero, 0},
		{"explicit positive honored", &twelve, 12 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Routing: RoutingConfig{EvaluationMaxAgeHours: tc.field}}
			applyRoutingDefaults(cfg)
			if got := cfg.Routing.EvaluationMaxAge(); got != tc.want {
				t.Fatalf("EvaluationMaxAge() = %s, want %s", got, tc.want)
			}
		})
	}

	// applyRoutingDefaults must not clobber an explicit 0 back to the default.
	cfg := &Config{Routing: RoutingConfig{EvaluationMaxAgeHours: &zero}}
	applyRoutingDefaults(cfg)
	if cfg.Routing.EvaluationMaxAgeHours == nil || *cfg.Routing.EvaluationMaxAgeHours != 0 {
		t.Fatalf("explicit 0 was rewritten to %v, want preserved 0", cfg.Routing.EvaluationMaxAgeHours)
	}
}
