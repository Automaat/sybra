package monitor

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

// A fleet with nowhere to run and an idle fleet both report dispatched:0, so
// the only way to tell them apart was grepping provider.health.flip out of the
// app log. The rule has to distinguish "nothing to do" from "cannot do
// anything" — raising on an idle board would be noise, not a signal.
func TestDetectNoProviderCapacity(t *testing.T) {
	now := time.Date(2026, 8, 5, 19, 0, 0, 0, time.UTC)
	until := now.Add(60 * time.Hour)

	todo := []task.Task{{ID: "t1", Status: task.StatusTodo, StatusChangedAt: now.Add(-time.Minute)}}

	tests := []struct {
		name      string
		tasks     []task.Task
		providers []ProviderHealth
		want      bool
	}{
		{
			name:  "every enabled provider down with work pending",
			tasks: todo,
			providers: []ProviderHealth{
				{Name: "claude", Enabled: true, Reason: "rate_limited", Until: until},
				{Name: "codex", Enabled: true, Reason: "rate_limited", Until: until},
				{Name: "copilot", Enabled: false},
			},
			want: true,
		},
		{
			// Not degraded: there is nothing to run.
			name:  "no capacity but an idle board",
			tasks: nil,
			providers: []ProviderHealth{
				{Name: "claude", Enabled: true, Reason: "rate_limited"},
			},
			want: false,
		},
		{
			name:  "one healthy provider is capacity",
			tasks: todo,
			providers: []ProviderHealth{
				{Name: "claude", Enabled: true, Reason: "rate_limited"},
				{Name: "codex", Enabled: true, Healthy: true, Reason: "ok"},
			},
			want: false,
		},
		{
			// Deliberately off is a configuration choice, not an outage.
			name:      "only disabled providers configured",
			tasks:     todo,
			providers: []ProviderHealth{{Name: "copilot", Enabled: false}},
			want:      false,
		},
		{
			name:      "provider health not wired stays silent",
			tasks:     todo,
			providers: nil,
			want:      false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := Detect(DetectInput{Now: now, Tasks: tc.tasks, Providers: tc.providers})
			var found *Anomaly
			for i := range report.Anomalies {
				if report.Anomalies[i].Kind == KindNoProviderCapacity {
					found = &report.Anomalies[i]
				}
			}
			if (found != nil) != tc.want {
				t.Fatalf("no_provider_capacity raised = %v, want %v", found != nil, tc.want)
				panic("unreachable")
			}
			if found == nil {
				return
			}
			// The expected recovery time has to be visible, or diagnosing it
			// still means reading the log.
			if got := found.Evidence["rate_limited_until"]; got == nil {
				t.Errorf("evidence missing rate_limited_until: %+v", found.Evidence)
			}
			if got := found.Evidence["reasons"]; got == nil {
				t.Errorf("evidence missing per-provider reasons: %+v", found.Evidence)
			}
		})
	}
}
