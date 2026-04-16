package health

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
)

func TestIsAgentFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		evt  audit.Event
		want bool
	}{
		{"legacy failed event", audit.Event{Type: audit.EventAgentFailed}, true},
		{"completed stopped is success", audit.Event{Type: audit.EventAgentCompleted, Data: map[string]any{"state": "stopped"}}, false},
		{"completed error is failure", audit.Event{Type: audit.EventAgentCompleted, Data: map[string]any{"state": "error"}}, true},
		{"completed without state is success", audit.Event{Type: audit.EventAgentCompleted, Data: map[string]any{}}, false},
		{"unrelated event", audit.Event{Type: audit.EventTaskCreated}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isAgentFailure(tt.evt); got != tt.want {
				t.Errorf("isAgentFailure(%s) = %v, want %v", tt.evt.Type, got, tt.want)
			}
		})
	}
}

func TestCheckAgentRetryLoops(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()

	tests := []struct {
		name     string
		failures map[string]int
		wantN    int
	}{
		{"no failures", map[string]int{}, 0},
		{"single failure ignored", map[string]int{"t1": 1}, 0},
		{"two failures flagged", map[string]int{"t1": 2}, 1},
		{"multiple tasks", map[string]int{"t1": 3, "t2": 1, "t3": 2}, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var events []audit.Event
			for taskID, n := range tt.failures {
				for range n {
					events = append(events, audit.Event{
						Type:      audit.EventAgentCompleted,
						TaskID:    taskID,
						Timestamp: now,
						Data:      map[string]any{"state": "error", "role": ""},
					})
				}
			}
			got := checkAgentRetryLoops(events, now)
			if len(got) != tt.wantN {
				t.Errorf("got %d findings, want %d", len(got), tt.wantN)
			}
		})
	}
}

func TestCheckAgentRetryLoopsIgnoresSuccesses(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	events := []audit.Event{
		{Type: audit.EventAgentCompleted, TaskID: "t1", Timestamp: now, Data: map[string]any{"state": "stopped"}},
		{Type: audit.EventAgentCompleted, TaskID: "t1", Timestamp: now, Data: map[string]any{"state": "stopped"}},
	}
	if got := checkAgentRetryLoops(events, now); len(got) != 0 {
		t.Errorf("got %d findings on all-success input, want 0", len(got))
	}
}

func TestCheckTriageMismatch(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()

	tests := []struct {
		name   string
		events []audit.Event
		wantN  int
	}{
		{
			"headless then human-required",
			[]audit.Event{
				{Type: audit.EventTriageClassified, TaskID: "t1", Data: map[string]any{"mode": "headless"}},
				{Type: audit.EventTaskStatusChanged, TaskID: "t1", Data: map[string]any{"from": "in-progress", "to": "human-required"}},
			},
			1,
		},
		{
			"interactive then human-required (not a mismatch)",
			[]audit.Event{
				{Type: audit.EventTriageClassified, TaskID: "t1", Data: map[string]any{"mode": "interactive"}},
				{Type: audit.EventTaskStatusChanged, TaskID: "t1", Data: map[string]any{"from": "in-progress", "to": "human-required"}},
			},
			0,
		},
		{
			"headless without escalation",
			[]audit.Event{
				{Type: audit.EventTriageClassified, TaskID: "t1", Data: map[string]any{"mode": "headless"}},
				{Type: audit.EventTaskStatusChanged, TaskID: "t1", Data: map[string]any{"from": "in-progress", "to": "done"}},
			},
			0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := checkTriageMismatch(tt.events, now)
			if len(got) != tt.wantN {
				t.Errorf("got %d findings, want %d", len(got), tt.wantN)
			}
		})
	}
}

func TestCheckStatusBottleneck(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()

	t.Run("plan-review dwell exceeds threshold", func(t *testing.T) {
		t.Parallel()
		events := []audit.Event{
			{Type: audit.EventTaskStatusChanged, TaskID: "t1", Timestamp: now.Add(-30 * time.Hour), Data: map[string]any{"from": "planning", "to": "plan-review"}},
			{Type: audit.EventTaskStatusChanged, TaskID: "t1", Timestamp: now, Data: map[string]any{"from": "plan-review", "to": "in-progress"}},
		}
		got := checkStatusBottleneck(events, now)
		if len(got) != 1 || got[0].Evidence["status"] != "plan-review" {
			t.Errorf("expected one plan-review bottleneck, got %+v", got)
		}
	})

	t.Run("dwell within threshold", func(t *testing.T) {
		t.Parallel()
		events := []audit.Event{
			{Type: audit.EventTaskStatusChanged, TaskID: "t1", Timestamp: now.Add(-2 * time.Hour), Data: map[string]any{"from": "planning", "to": "plan-review"}},
			{Type: audit.EventTaskStatusChanged, TaskID: "t1", Timestamp: now, Data: map[string]any{"from": "plan-review", "to": "in-progress"}},
		}
		if got := checkStatusBottleneck(events, now); len(got) != 0 {
			t.Errorf("expected no findings, got %d", len(got))
		}
	})

	t.Run("status not in threshold map ignored", func(t *testing.T) {
		t.Parallel()
		events := []audit.Event{
			{Type: audit.EventTaskStatusChanged, TaskID: "t1", Timestamp: now.Add(-100 * time.Hour), Data: map[string]any{"from": "new", "to": "in-progress"}},
			{Type: audit.EventTaskStatusChanged, TaskID: "t1", Timestamp: now, Data: map[string]any{"from": "in-progress", "to": "done"}},
		}
		if got := checkStatusBottleneck(events, now); len(got) != 0 {
			t.Errorf("expected no findings for in-progress, got %d", len(got))
		}
	})
}

func TestRollupScore(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		findings []Finding
		want     Score
	}{
		{"empty is good", nil, ScoreGood},
		{"only warnings", []Finding{{Severity: SeverityWarning}, {Severity: SeverityWarning}}, ScoreWarning},
		{"any critical wins", []Finding{{Severity: SeverityWarning}, {Severity: SeverityCritical}}, ScoreCritical},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := RollupScore(tt.findings); got != tt.want {
				t.Errorf("RollupScore = %s, want %s", got, tt.want)
			}
		})
	}
}
