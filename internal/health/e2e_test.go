package health

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/task"
)

// TestE2E_NewChecksFireThroughChecker drives the full Checker.check pipeline
// against a freshly seeded audit dir and asserts that the three new detectors
// (agent_retry_loop, triage_mismatch, status_bottleneck) and the score rollup
// surface in the persisted health-report.json that the CLI consumes.
func TestE2E_NewChecksFireThroughChecker(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	auditDir := filepath.Join(home, "audit")
	tasksDir := filepath.Join(home, "tasks")

	logger, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatalf("audit.NewLogger: %v", err)
	}
	t.Cleanup(func() { _ = logger.Close() })

	now := time.Now().UTC()

	seed := []audit.Event{
		// task-retry: 3 failed agent runs → checkAgentRetryLoops (critical)
		{Timestamp: now.Add(-3 * time.Hour), Type: audit.EventAgentCompleted, TaskID: "task-retry", AgentID: "a1", Data: map[string]any{"state": "error", "role": "", "cost_usd": 0.40}},
		{Timestamp: now.Add(-2 * time.Hour), Type: audit.EventAgentCompleted, TaskID: "task-retry", AgentID: "a2", Data: map[string]any{"state": "error", "role": "", "cost_usd": 0.50}},
		{Timestamp: now.Add(-1 * time.Hour), Type: audit.EventAgentCompleted, TaskID: "task-retry", AgentID: "a3", Data: map[string]any{"state": "error", "role": "", "cost_usd": 0.60}},

		// task-mismatch: triaged headless then escalated to human-required
		// → checkTriageMismatch (warning).
		{Timestamp: now.Add(-26 * time.Hour), Type: audit.EventTriageClassified, TaskID: "task-mismatch", Data: map[string]any{"mode": "headless"}},
		{Timestamp: now.Add(-2 * time.Hour), Type: audit.EventTaskStatusChanged, TaskID: "task-mismatch", Data: map[string]any{"from": "in-progress", "to": "human-required"}},

		// task-bottleneck: lingered in plan-review for 30h before moving on
		// → checkStatusBottleneck (warning, threshold 12h).
		{Timestamp: now.Add(-50 * time.Hour), Type: audit.EventTaskStatusChanged, TaskID: "task-bottleneck", Data: map[string]any{"from": "planning", "to": "plan-review"}},
		{Timestamp: now.Add(-20 * time.Hour), Type: audit.EventTaskStatusChanged, TaskID: "task-bottleneck", Data: map[string]any{"from": "plan-review", "to": "in-progress"}},
	}
	for _, e := range seed {
		if err := logger.Log(e); err != nil {
			t.Fatalf("audit log: %v", err)
		}
	}

	store, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tasks := task.NewManager(store, nil)

	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := New(auditDir, tasks, home, silent, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Run(ctx)

	report := c.LatestReport()
	if report == nil {
		t.Fatal("LatestReport returned nil")
	}

	categories := map[Category]Finding{}
	for _, f := range report.Findings {
		categories[f.Category] = f
	}

	if _, ok := categories[CatAgentRetryLoop]; !ok {
		t.Errorf("expected CatAgentRetryLoop finding, got categories=%v", findingCategories(report.Findings))
	}
	if _, ok := categories[CatTriageMismatch]; !ok {
		t.Errorf("expected CatTriageMismatch finding, got categories=%v", findingCategories(report.Findings))
	}
	if _, ok := categories[CatStatusBottleneck]; !ok {
		t.Errorf("expected CatStatusBottleneck finding, got categories=%v", findingCategories(report.Findings))
	}

	if retry, ok := categories[CatAgentRetryLoop]; ok {
		if retry.TaskID != "task-retry" {
			t.Errorf("retry-loop TaskID = %q, want task-retry", retry.TaskID)
		}
		if retry.Severity != SeverityCritical {
			t.Errorf("retry-loop severity = %q, want critical", retry.Severity)
		}
	}

	if report.Score != ScoreCritical {
		t.Errorf("Score = %q, want critical (a critical finding fired)", report.Score)
	}

	// The persisted JSON is what sybra-cli health reads. Verify the score
	// and findings round-trip through the file the CLI consumes.
	data, err := os.ReadFile(filepath.Join(home, "health-report.json"))
	if err != nil {
		t.Fatalf("read persisted report: %v", err)
	}
	var persisted struct {
		Score    string `json:"score"`
		Findings []struct {
			Category string `json:"category"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("parse persisted report: %v", err)
	}
	if persisted.Score != string(ScoreCritical) {
		t.Errorf("persisted score = %q, want critical", persisted.Score)
	}
	gotPersisted := map[string]bool{}
	for _, f := range persisted.Findings {
		gotPersisted[f.Category] = true
	}
	for _, want := range []Category{CatAgentRetryLoop, CatTriageMismatch, CatStatusBottleneck} {
		if !gotPersisted[string(want)] {
			t.Errorf("persisted report missing category %q", want)
		}
	}
}

// TestE2E_GoodScoreWhenNothingFires ensures the rollup yields ScoreGood when
// the audit log only contains successful, well-behaved events.
func TestE2E_GoodScoreWhenNothingFires(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	auditDir := filepath.Join(home, "audit")
	tasksDir := filepath.Join(home, "tasks")

	logger, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatalf("audit.NewLogger: %v", err)
	}
	t.Cleanup(func() { _ = logger.Close() })

	now := time.Now().UTC()
	clean := []audit.Event{
		{Timestamp: now.Add(-2 * time.Hour), Type: audit.EventAgentCompleted, TaskID: "task-ok", AgentID: "a1", Data: map[string]any{"state": "stopped", "role": "", "cost_usd": 0.30}},
		{Timestamp: now.Add(-1 * time.Hour), Type: audit.EventTaskStatusChanged, TaskID: "task-ok", Data: map[string]any{"from": "in-progress", "to": "done"}},
	}
	for _, e := range clean {
		if err := logger.Log(e); err != nil {
			t.Fatalf("audit log: %v", err)
		}
	}

	store, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tasks := task.NewManager(store, nil)
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := New(auditDir, tasks, home, silent, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Run(ctx)

	report := c.LatestReport()
	if report == nil {
		t.Fatal("LatestReport returned nil")
	}
	if report.Score != ScoreGood {
		t.Errorf("Score = %q, want good (findings=%v)", report.Score, findingCategories(report.Findings))
	}
}

func findingCategories(findings []Finding) []string {
	out := make([]string, len(findings))
	for i := range findings {
		out[i] = string(findings[i].Category)
	}
	return out
}
