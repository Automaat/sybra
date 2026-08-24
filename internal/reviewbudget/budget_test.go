package reviewbudget

import (
	"testing"
	"time"
)

func TestBudget_HourlyExceeded(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	runs := []Run{
		{Role: ReviewRole, StartedAt: now.Add(-90 * time.Minute), Outcome: SuccessOutcome}, // outside the window
		{Role: ReviewRole, StartedAt: now.Add(-30 * time.Minute), Outcome: SuccessOutcome},
		{Role: ReviewRole, StartedAt: now.Add(-10 * time.Minute), Outcome: SuccessOutcome},
		{Role: ReviewRole, StartedAt: now.Add(-5 * time.Minute), Outcome: "failure", TurnCount: 0}, // provider never started
		{Role: "fix-review", StartedAt: now.Add(-5 * time.Minute)},                                 // wrong role
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

func TestBudget_LifetimeExceeded(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	runs := []Run{
		{Role: ReviewRole, StartedAt: now.Add(-48 * time.Hour), Outcome: SuccessOutcome},
		{Role: ReviewRole, StartedAt: now.Add(-24 * time.Hour), Outcome: SuccessOutcome},
		{Role: ReviewRole, StartedAt: now.Add(-time.Hour), Outcome: SuccessOutcome},
		{Role: ReviewRole, StartedAt: now.Add(-30 * time.Minute), Outcome: "failure", TurnCount: 0},
		{Role: "fix-review", StartedAt: now.Add(-30 * time.Minute), Outcome: SuccessOutcome},
	}

	tests := []struct {
		name  string
		limit int
		want  bool
	}{
		{"disabled when PerTask <= 0", 0, false},
		{"under limit", 4, false},
		{"at limit", 3, true},
		{"over limit", 2, true},
		{"negative disables", -1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := Budget{PerTask: tt.limit}
			if got := b.LifetimeSpent(runs); got != 3 {
				t.Fatalf("LifetimeSpent() = %d, want 3", got)
			}
			if got := b.LifetimeExceeded(runs); got != tt.want {
				t.Errorf("LifetimeExceeded(limit=%d) = %v, want %v", tt.limit, got, tt.want)
			}
		})
	}
}

func TestBudget_Exhausted(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	runs := []Run{
		{Role: ReviewRole, StartedAt: now.Add(-50 * time.Minute), Outcome: SuccessOutcome},
		{Role: ReviewRole, StartedAt: now.Add(-10 * time.Minute), Outcome: SuccessOutcome},
		{Role: ReviewRole, StartedAt: now.Add(-2 * time.Hour), Outcome: SuccessOutcome},
	}

	if !(Budget{PerHour: 2}).Exhausted(runs, now) {
		t.Fatal("Exhausted() = false, want true when hourly budget is spent")
	}
	if !(Budget{PerTask: 3}).Exhausted(runs, now) {
		t.Fatal("Exhausted() = false, want true when lifetime budget is spent")
	}
	if (Budget{PerHour: 3, PerTask: 4}).Exhausted(runs, now) {
		t.Fatal("Exhausted() = true, want false when neither budget is spent")
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

func TestLifetimeSpent_IgnoresRunsThatReviewedNothing(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	b := Budget{PerHour: 3, PerTask: 6}
	runs := []Run{
		{Role: ReviewRole, StartedAt: now.Add(-5 * time.Hour), Outcome: SuccessOutcome, TurnCount: 4},
		{Role: ReviewRole, StartedAt: now.Add(-4 * time.Hour), Outcome: SuccessOutcome, TurnCount: 9},
		{Role: ReviewRole, StartedAt: now.Add(-3 * time.Hour), Outcome: "failure", TurnCount: 1},
		{Role: ReviewRole, StartedAt: now.Add(-2 * time.Hour), Outcome: "failure", TurnCount: 0},
		{Role: ReviewRole, StartedAt: now.Add(-time.Hour), Outcome: "", TurnCount: 6},
	}
	if got := b.LifetimeSpent(runs); got != 2 {
		t.Fatalf("LifetimeSpent = %d, want 2 — only the runs that produced a review", got)
	}
	if b.LifetimeExceeded(runs) {
		t.Fatal("a task was held at its lifetime ceiling by rounds that reviewed nothing")
	}
}

func TestHourlySpent_StillCountsAFailedDispatch(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	b := Budget{PerHour: 3, PerTask: 6}
	runs := []Run{
		{Role: ReviewRole, StartedAt: now.Add(-10 * time.Minute), Outcome: "failure", TurnCount: 1},
		{Role: ReviewRole, StartedAt: now.Add(-8 * time.Minute), Outcome: "failure", TurnCount: 1},
		{Role: ReviewRole, StartedAt: now.Add(-6 * time.Minute), Outcome: "failure", TurnCount: 1},
	}
	if got := b.HourlySpent(runs, now); got != 3 {
		t.Fatalf("HourlySpent = %d, want 3 — a failing loop is still a loop", got)
	}
	if !b.HourlyExceeded(runs, now) {
		t.Fatal("a provider failing in a tight loop was not caught by the rolling-hour breaker")
	}
}

func TestBudget_ARunThatNeverStartedCountsNowhere(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	b := Budget{PerHour: 3, PerTask: 6}
	runs := []Run{{Role: ReviewRole, StartedAt: now.Add(-time.Minute), Outcome: "failure", TurnCount: 0}}
	if got := b.HourlySpent(runs, now); got != 0 {
		t.Fatalf("HourlySpent = %d, want 0 for a run that never saw its instructions", got)
	}
	if got := b.LifetimeSpent(runs); got != 0 {
		t.Fatalf("LifetimeSpent = %d, want 0 for a run that never saw its instructions", got)
	}
}

func TestLifetimeSpent_CountsASalvagedReview(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	b := Budget{PerHour: 3, PerTask: 6}
	runs := []Run{
		{Role: ReviewRole, StartedAt: now.Add(-4 * time.Hour), Outcome: SuccessOutcome, TurnCount: 5},
		{Role: ReviewRole, StartedAt: now.Add(-3 * time.Hour), Outcome: "failure", TurnCount: 9, Salvaged: true},
		{Role: ReviewRole, StartedAt: now.Add(-2 * time.Hour), Outcome: "failure", TurnCount: 3},
		{Role: ReviewRole, StartedAt: now.Add(-time.Hour), Outcome: "", TurnCount: 4},
	}
	if got := b.LifetimeSpent(runs); got != 2 {
		t.Fatalf("LifetimeSpent = %d, want 2 — the successful round and the salvaged one", got)
	}
}

func TestLifetimeSpent_ASalvageThatNeverStartedStillCountsNowhere(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	b := Budget{PerHour: 3, PerTask: 6}
	runs := []Run{{Role: ReviewRole, StartedAt: now, Outcome: "failure", TurnCount: 0, Salvaged: true}}
	if got := b.LifetimeSpent(runs); got != 0 {
		t.Fatalf("LifetimeSpent = %d, want 0 — a run that never saw its instructions reviewed nothing to salvage", got)
	}
}
