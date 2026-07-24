package reviewbudget

import (
	"testing"
	"time"
)

func TestBudget_HourlyExceeded(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	runs := []Run{
		{Role: ReviewRole, StartedAt: now.Add(-90 * time.Minute)}, // outside the window
		{Role: ReviewRole, StartedAt: now.Add(-30 * time.Minute)},
		{Role: ReviewRole, StartedAt: now.Add(-10 * time.Minute)},
		{Role: "fix-review", StartedAt: now.Add(-5 * time.Minute)}, // wrong role
	}

	tests := []struct {
		name   string
		budget Budget
		runs   []Run
		want   bool
		wantN  int
	}{
		{"disabled when PerHour <= 0", Budget{PerHour: 0}, runs, false, 2},
		{"under limit", Budget{PerHour: 3}, runs, false, 2},
		{"at limit", Budget{PerHour: 2}, runs, true, 2},
		{"over limit", Budget{PerHour: 1}, runs, true, 2},
		{"negative disables", Budget{PerHour: -1}, runs, false, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.budget.HourlySpent(tt.runs, now); got != tt.wantN {
				t.Errorf("HourlySpent() = %d, want %d", got, tt.wantN)
			}
			if got := tt.budget.HourlyExceeded(tt.runs, now); got != tt.want {
				t.Errorf("HourlyExceeded() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBudget_HeadCovered(t *testing.T) {
	b := Budget{PerHead: 2}
	tests := []struct {
		name        string
		reviewedSHA string
		attempts    int
		head        string
		want        bool
	}{
		{"empty head always covered", "sha1", 5, "", true},
		{"new head not covered", "sha1", 5, "sha2", false},
		{"under budget", "sha1", 1, "sha1", false},
		{"at budget", "sha1", 2, "sha1", true},
		{"over budget", "sha1", 3, "sha1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := b.HeadCovered(tt.reviewedSHA, tt.attempts, tt.head); got != tt.want {
				t.Errorf("HeadCovered() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBudget_HeadCovered_DisabledPerHead(t *testing.T) {
	b := Budget{PerHead: 0}
	if b.HeadCovered("sha1", 100, "sha1") {
		t.Fatal("HeadCovered() = true with PerHead disabled, want false (never covered)")
	}
}

func TestBudget_NextAttempt(t *testing.T) {
	tests := []struct {
		name        string
		reviewedSHA string
		attempts    int
		head        string
		want        int
	}{
		{"new head restarts budget", "sha1", 5, "sha2", 1},
		{"same head increments", "sha1", 1, "sha1", 2},
		{"empty reviewed sha with matching head", "", 0, "", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (Budget{}).NextAttempt(tt.reviewedSHA, tt.attempts, tt.head); got != tt.want {
				t.Errorf("NextAttempt() = %d, want %d", got, tt.want)
			}
		})
	}
}
