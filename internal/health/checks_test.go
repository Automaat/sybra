package health

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/stats"
	"github.com/Automaat/sybra/internal/task"
)

func TestCheckFailureRate(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name      string
		completed int
		failed    int
		wantN     int
	}{
		{"below threshold", 8, 2, 0},
		{"above threshold", 5, 5, 1},
		{"too few runs", 2, 2, 0},
		{"no failures", 10, 0, 0},
		{"all failures", 0, 6, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var events []audit.Event
			for range tt.completed {
				events = append(events, audit.Event{Type: audit.EventAgentCompleted, Timestamp: now})
			}
			for range tt.failed {
				events = append(events, audit.Event{Type: audit.EventAgentFailed, Timestamp: now})
			}
			got := checkFailureRate(events, now)
			if len(got) != tt.wantN {
				t.Errorf("got %d findings, want %d", len(got), tt.wantN)
			}
		})
	}
}

func TestCheckFailureRateDedupesLegacyFailedCompatibilityEvent(t *testing.T) {
	now := time.Now().UTC()
	events := []audit.Event{
		{Type: audit.EventAgentCompleted, AgentID: "a1", Timestamp: now, Data: map[string]any{"state": "stopped"}},
		{Type: audit.EventAgentCompleted, AgentID: "a2", Timestamp: now, Data: map[string]any{"state": "stopped"}},
		{Type: audit.EventAgentCompleted, AgentID: "a3", Timestamp: now, Data: map[string]any{"state": "stopped"}},
		{Type: audit.EventAgentCompleted, AgentID: "a4", Timestamp: now, Data: map[string]any{"state": "stopped"}},
		{Type: audit.EventAgentCompleted, AgentID: "a5", Timestamp: now, Data: map[string]any{"state": "error"}},
		{Type: audit.EventAgentFailed, AgentID: "a5", Timestamp: now.Add(time.Second)},
	}

	got := checkFailureRate(events, now)
	if len(got) != 0 {
		t.Fatalf("deduped failure rate should stay at 20%%, got findings=%+v", got)
	}
}

func TestCheckCostOutliers(t *testing.T) {
	now := time.Now().UTC()

	tests := []struct {
		name string
		runs []struct {
			role string
			cost float64
		}
		wantN int
	}{
		{
			"no outliers",
			[]struct {
				role string
				cost float64
			}{{"eval", 0.20}, {"", 5.0}},
			0,
		},
		{
			"eval outlier",
			[]struct {
				role string
				cost float64
			}{{"eval", 1.50}},
			1,
		},
		{
			"impl outlier",
			[]struct {
				role string
				cost float64
			}{{"", 20.0}},
			1,
		},
		{
			"daily total exceeded",
			[]struct {
				role string
				cost float64
			}{{"", 14.0}, {"", 14.0}, {"", 14.0}, {"", 14.0}, {"", 14.0}, {"", 14.0}, {"", 14.0}, {"", 14.0}, {"", 14.0}, {"", 14.0}, {"", 14.0}, {"", 14.0}, {"", 14.0}, {"", 14.0}, {"", 14.0}},
			1, // daily total only (individual runs are under $15)
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var events []audit.Event
			for _, r := range tt.runs {
				events = append(events, audit.Event{
					Type:      audit.EventAgentCompleted,
					Timestamp: now,
					Data:      map[string]any{"cost_usd": r.cost, "role": r.role},
				})
			}
			got := checkCostOutliers(events, now)
			if len(got) != tt.wantN {
				t.Errorf("got %d findings, want %d", len(got), tt.wantN)
			}
		})
	}
}

func TestCheckStuckTasks(t *testing.T) {
	now := time.Now().UTC()

	tests := []struct {
		name     string
		tasks    []task.Task
		resolved []string
		wantN    int
	}{
		{
			"task stuck over 6h",
			[]task.Task{{ID: "t1", Status: task.StatusInProgress, UpdatedAt: now.Add(-7 * time.Hour)}},
			nil,
			1,
		},
		{
			"task in-progress but recently updated",
			[]task.Task{{ID: "t1", Status: task.StatusInProgress, UpdatedAt: now.Add(-1 * time.Hour)}},
			nil,
			0,
		},
		{
			"task stuck but has completion event",
			[]task.Task{{ID: "t1", Status: task.StatusInProgress, UpdatedAt: now.Add(-7 * time.Hour)}},
			[]string{"t1"},
			0,
		},
		{
			"task not in-progress",
			[]task.Task{{ID: "t1", Status: task.StatusTodo, UpdatedAt: now.Add(-7 * time.Hour)}},
			nil,
			0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var events []audit.Event
			for _, id := range tt.resolved {
				events = append(events, audit.Event{
					Type:   audit.EventAgentCompleted,
					TaskID: id,
				})
			}
			got := checkStuckTasks(events, tt.tasks, now)
			if len(got) != tt.wantN {
				t.Errorf("got %d findings, want %d", len(got), tt.wantN)
			}
		})
	}
}

func TestCheckWorkflowLoops(t *testing.T) {
	now := time.Now().UTC()

	tests := []struct {
		name   string
		counts map[string]int
		wantN  int
	}{
		{"no loops", map[string]int{"t1": 1}, 0},
		{"below threshold", map[string]int{"t1": 2}, 0},
		{"at threshold", map[string]int{"t1": 3}, 1},
		{"multiple tasks", map[string]int{"t1": 3, "t2": 4}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var events []audit.Event
			for taskID, n := range tt.counts {
				for range n {
					events = append(events, audit.Event{
						Type:      audit.EventPlanRejected,
						TaskID:    taskID,
						Timestamp: now,
					})
				}
			}
			got := checkWorkflowLoops(events, now)
			if len(got) != tt.wantN {
				t.Errorf("got %d findings, want %d", len(got), tt.wantN)
			}
		})
	}
}

func TestCheckStatusBounce(t *testing.T) {
	now := time.Now().UTC()

	tests := []struct {
		name        string
		transitions []struct{ taskID, from, to string }
		wantN       int
	}{
		{
			"no bounce",
			[]struct{ taskID, from, to string }{
				{"t1", "todo", "in-progress"},
				{"t1", "in-progress", "in-review"},
			},
			0,
		},
		{
			"bounce detected",
			[]struct{ taskID, from, to string }{
				{"t1", "todo", "in-progress"},
				{"t1", "in-progress", "todo"},
				{"t1", "todo", "in-progress"},
				{"t1", "in-progress", "todo"},
			},
			2, // todo→in-progress x2 and in-progress→todo x2
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var events []audit.Event
			for _, tr := range tt.transitions {
				events = append(events, audit.Event{
					Type:      audit.EventTaskStatusChanged,
					TaskID:    tr.taskID,
					Timestamp: now,
					Data:      map[string]any{"from": tr.from, "to": tr.to},
				})
			}
			got := checkStatusBounce(events, now)
			if len(got) != tt.wantN {
				t.Errorf("got %d findings, want %d", len(got), tt.wantN)
			}
		})
	}
}

func TestCheckStatusBounceSkipsExpectedHumanReviewHandoffs(t *testing.T) {
	now := time.Now().UTC()
	events := []audit.Event{
		{Type: audit.EventTaskStatusChanged, TaskID: "t1", Timestamp: now, Data: map[string]any{"from": "todo", "to": "human-required", "human_kind": "review_manual"}},
		{Type: audit.EventTaskStatusChanged, TaskID: "t1", Timestamp: now, Data: map[string]any{"from": "todo", "to": "human-required", "human_kind": "review_manual"}},
	}
	if got := checkStatusBounce(events, now); len(got) != 0 {
		t.Fatalf("got %d findings, want 0", len(got))
	}
}

func TestCheckCostDrift(t *testing.T) {
	now := time.Now().UTC()

	mkEvents := func(n int, cost float64) []audit.Event {
		events := make([]audit.Event, n)
		for i := range n {
			events[i] = audit.Event{
				Type:      audit.EventAgentCompleted,
				Timestamp: now,
				Data:      map[string]any{"cost_usd": cost},
			}
		}
		return events
	}

	tests := []struct {
		name      string
		todayN    int
		todayCost float64
		weekN     int
		weekCost  float64
		wantN     int
	}{
		{"no drift", 10, 1.0, 50, 1.0, 0},
		{"significant drift", 10, 3.0, 50, 1.0, 1},
		{"too few today runs", 3, 5.0, 50, 1.0, 0},
		{"too few week runs", 10, 3.0, 5, 1.0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			today := mkEvents(tt.todayN, tt.todayCost)
			week := mkEvents(tt.weekN, tt.weekCost)
			got := checkCostDrift(today, week, now)
			if len(got) != tt.wantN {
				t.Errorf("got %d findings, want %d", len(got), tt.wantN)
			}
		})
	}
}

func TestBuildStats(t *testing.T) {
	now := time.Now().UTC()
	events := []audit.Event{
		{Type: audit.EventAgentCompleted, AgentID: "a1", Timestamp: now, Data: map[string]any{"cost_usd": 1.5, "role": "eval", "state": "stopped"}},
		{Type: audit.EventAgentCompleted, AgentID: "a2", Timestamp: now, Data: map[string]any{"cost_usd": 3.0, "role": "", "state": "error"}},
		{Type: audit.EventAgentFailed, AgentID: "a2", Timestamp: now, Data: map[string]any{"cost_usd": 99.0}},
		{Type: audit.EventTaskCreated, Timestamp: now}, // should be ignored
	}

	s := buildStats(events)

	if s.TotalAgentRuns != 2 {
		t.Errorf("TotalAgentRuns = %d, want 2", s.TotalAgentRuns)
	}
	if s.FailedAgentRuns != 1 {
		t.Errorf("FailedAgentRuns = %d, want 1", s.FailedAgentRuns)
	}
	if s.TotalCostUSD != 4.5 {
		t.Errorf("TotalCostUSD = %.2f, want 4.50", s.TotalCostUSD)
	}
	if s.CostByRole["eval"] != 1.5 {
		t.Errorf("CostByRole[eval] = %.2f, want 1.50", s.CostByRole["eval"])
	}
	if s.CostByRole["implementation"] != 3.0 {
		t.Errorf("CostByRole[implementation] = %.2f, want 3.00", s.CostByRole["implementation"])
	}
}

func TestRunAccountingAgreesAcrossHealthAuditAndBackfill(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	events := []audit.Event{
		{Timestamp: now, Type: audit.EventAgentStarted, TaskID: "t1", AgentID: "a1"},
		{Timestamp: now.Add(time.Minute), Type: audit.EventAgentCompleted, TaskID: "t1", AgentID: "a1", Data: map[string]any{"state": "stopped", "cost_usd": 0.1}},

		{Timestamp: now.Add(2 * time.Minute), Type: audit.EventAgentStarted, TaskID: "t2", AgentID: "a2"},
		{Timestamp: now.Add(3 * time.Minute), Type: audit.EventAgentCompleted, TaskID: "t2", AgentID: "a2", Data: map[string]any{"state": "error", "cost_usd": 0.2}},
		{Timestamp: now.Add(4 * time.Minute), Type: audit.EventAgentFailed, TaskID: "t2", AgentID: "a2", Data: map[string]any{"cost_usd": 9.9}},

		{Timestamp: now.Add(5 * time.Minute), Type: audit.EventAgentStarted, TaskID: "t3", AgentID: "a3"},

		{Timestamp: now.Add(6 * time.Minute), Type: audit.EventAgentCompleted, TaskID: "t4", AgentID: "a4", Data: map[string]any{"state": "stopped", "cost_usd": 0.3}},
	}

	healthStats := buildStats(events)
	if healthStats.TotalAgentRuns != 3 || healthStats.FailedAgentRuns != 1 {
		t.Fatalf("health stats = %+v, want 3 runs / 1 failure", healthStats)
	}

	summary := audit.Summarize(events, now.Add(-time.Hour), now.Add(time.Hour))
	if summary.AgentRuns != 3 {
		t.Fatalf("audit summary agent runs = %d, want 3", summary.AgentRuns)
	}
	if summary.FailureRate != 0.33 {
		t.Fatalf("audit summary failure rate = %.2f, want 0.33", summary.FailureRate)
	}

	dir := t.TempDir()
	auditDir := filepath.Join(dir, "audit")
	if err := os.MkdirAll(auditDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []byte
	for i := range events {
		b, err := json.Marshal(events[i])
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, b...)
		lines = append(lines, '\n')
	}
	if err := os.WriteFile(filepath.Join(auditDir, now.Format(time.DateOnly)+".ndjson"), lines, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := stats.NewStore(filepath.Join(dir, "stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Backfill(auditDir); err != nil {
		t.Fatal(err)
	}
	resp := store.QueryAt(now.Add(2 * time.Hour))
	if resp.AllTime.TotalRuns != 3 {
		t.Fatalf("stats total runs = %d, want 3", resp.AllTime.TotalRuns)
	}
	var failed int
	for i := range resp.RecentRuns {
		if resp.RecentRuns[i].Outcome == "failed" {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("stats failed runs = %d, want 1", failed)
	}
}
