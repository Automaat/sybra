package monitor

import (
	"testing"
	"time"

	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/task"
)

func defaultCfg() config.MonitorConfig {
	return config.MonitorConfig{
		Enabled:              true,
		IntervalSeconds:      300,
		Model:                "sonnet",
		IssueCooldownMinutes: 30,
		DispatchLimit:        3,
		StuckHumanHours:      8,
		LostAgentMinutes:     15,
		FailureRateThreshold: 0.3,
		BottleneckHours: map[string]float64{
			"plan-review":    4,
			"human-required": 8,
			"in-progress":    6,
			"default":        12,
		},
	}
}

func mkTask(id string, status task.Status, opts ...func(*task.Task)) task.Task {
	t := task.Task{
		ID:        id,
		Title:     "task " + id,
		Status:    status,
		AgentMode: task.AgentModeHeadless,
		Tags:      []string{"medium"},
		UpdatedAt: time.Now().Add(-time.Hour).UTC(),
	}
	for _, o := range opts {
		o(&t)
	}
	return t
}

func TestDetect(t *testing.T) {
	now := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	cfg := defaultCfg()

	cases := []struct {
		name string
		in   DetectInput
		want []AnomalyKind
	}{
		{
			name: "clean board",
			in: DetectInput{
				Now: now,
				Tasks: []task.Task{
					mkTask("a", task.StatusInProgress),
					mkTask("b", task.StatusDone),
				},
				LiveAgents: []liveAgent{{TaskID: "a", Running: true}},
				Cfg:        cfg,
			},
			want: nil,
		},
		{
			name: "over_dispatch_limit triggers when in_progress > limit",
			in: DetectInput{
				Now: now,
				Tasks: []task.Task{
					mkTask("a", task.StatusInProgress),
					mkTask("b", task.StatusInProgress),
					mkTask("c", task.StatusInProgress),
					mkTask("d", task.StatusInProgress),
				},
				LiveAgents: []liveAgent{
					{TaskID: "a", Running: true},
					{TaskID: "b", Running: true},
					{TaskID: "c", Running: true},
					{TaskID: "d", Running: true},
				},
				Cfg: cfg,
			},
			want: []AnomalyKind{KindOverDispatchLimit},
		},
		{
			name: "untriaged on todo missing agent_mode",
			in: DetectInput{
				Now:   now,
				Tasks: []task.Task{mkTask("a", task.StatusTodo, func(t *task.Task) { t.AgentMode = "" })},
				Cfg:   cfg,
			},
			want: []AnomalyKind{KindUntriaged},
		},
		{
			name: "untriaged on todo missing tags",
			in: DetectInput{
				Now:   now,
				Tasks: []task.Task{mkTask("a", task.StatusTodo, func(t *task.Task) { t.Tags = nil })},
				Cfg:   cfg,
			},
			want: []AnomalyKind{KindUntriaged},
		},
		{
			name: "untriaged not flagged on done tasks",
			in: DetectInput{
				Now: now,
				Tasks: []task.Task{mkTask("a", task.StatusDone, func(t *task.Task) {
					t.Tags = nil
					t.AgentMode = ""
				})},
				Cfg: cfg,
			},
			want: nil,
		},
		{
			name: "stuck_human_blocked on plan-review past budget",
			in: DetectInput{
				Now: now,
				Tasks: []task.Task{mkTask("a", task.StatusPlanReview, func(t *task.Task) {
					t.UpdatedAt = now.Add(-9 * time.Hour)
				})},
				Cfg: cfg,
			},
			want: []AnomalyKind{KindStuckHumanBlocked},
		},
		{
			name: "stuck_human_blocked not flagged below budget",
			in: DetectInput{
				Now: now,
				Tasks: []task.Task{mkTask("a", task.StatusHumanRequired, func(t *task.Task) {
					t.UpdatedAt = now.Add(-2 * time.Hour)
				})},
				Cfg: cfg,
			},
			want: nil,
		},
		{
			name: "pr_gap on in-review with project but no PR",
			in: DetectInput{
				Now: now,
				Tasks: []task.Task{mkTask("a", task.StatusInReview, func(t *task.Task) {
					t.ProjectID = "owner/repo"
				})},
				Cfg: cfg,
			},
			want: []AnomalyKind{KindPRGap},
		},
		{
			name: "pr_gap suppressed when PR is set",
			in: DetectInput{
				Now: now,
				Tasks: []task.Task{mkTask("a", task.StatusInReview, func(t *task.Task) {
					t.ProjectID = "owner/repo"
					t.PRNumber = 412
				})},
				Cfg: cfg,
			},
			want: nil,
		},
		{
			name: "pr_gap suppressed when project missing",
			in: DetectInput{
				Now:   now,
				Tasks: []task.Task{mkTask("a", task.StatusInReview)},
				Cfg:   cfg,
			},
			want: nil,
		},
		{
			name: "lost_agent when in-progress without recent agent event",
			in: DetectInput{
				Now:        now,
				Tasks:      []task.Task{mkTask("a", task.StatusInProgress)},
				LiveAgents: []liveAgent{},
				Cfg:        cfg,
			},
			want: []AnomalyKind{KindLostAgent},
		},
		{
			name: "lost_agent suppressed by recent agent.* event",
			in: DetectInput{
				Now:   now,
				Tasks: []task.Task{mkTask("a", task.StatusInProgress)},
				Events15m: []audit.Event{
					{Type: "agent.started", TaskID: "a", Timestamp: now.Add(-2 * time.Minute)},
				},
				Cfg: cfg,
			},
			want: nil,
		},
		{
			name: "lost_agent suppressed by live agent without audit yet",
			in: DetectInput{
				Now:        now,
				Tasks:      []task.Task{mkTask("a", task.StatusInProgress)},
				LiveAgents: []liveAgent{{TaskID: "a", Running: true}},
				Cfg:        cfg,
			},
			want: nil,
		},
		{
			name: "failure_spike when failure_rate > threshold",
			in: DetectInput{
				Now: now,
				HourSummary: audit.Summary{
					FailureRate: 0.45,
					AgentRuns:   10,
					Period:      "test",
				},
				Cfg: cfg,
			},
			want: []AnomalyKind{KindFailureSpike},
		},
		{
			name: "failure_spike suppressed below threshold",
			in: DetectInput{
				Now: now,
				HourSummary: audit.Summary{
					FailureRate: 0.1,
					AgentRuns:   10,
				},
				Cfg: cfg,
			},
			want: nil,
		},
		{
			name: "bottleneck on plan-review dwell over threshold",
			in: DetectInput{
				Now: now,
				HourSummary: audit.Summary{
					StatusBottlenecks: map[string]float64{"plan-review": 5.5},
				},
				Cfg: cfg,
			},
			want: []AnomalyKind{KindBottleneck},
		},
		{
			name: "lost_agent + failure_spike independent",
			in: DetectInput{
				Now:        now,
				Tasks:      []task.Task{mkTask("a", task.StatusInProgress)},
				LiveAgents: []liveAgent{},
				HourSummary: audit.Summary{
					FailureRate: 0.5,
					AgentRuns:   4,
				},
				Cfg: cfg,
			},
			want: []AnomalyKind{KindFailureSpike, KindLostAgent},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := Detect(tc.in)
			report.Anomalies = SortAnomalies(report.Anomalies)
			got := make([]AnomalyKind, 0, len(report.Anomalies))
			for _, a := range report.Anomalies {
				got = append(got, a.Kind)
			}
			if !sameKinds(got, tc.want) {
				t.Fatalf("want kinds %v, got %v", tc.want, got)
			}
		})
	}
}

func TestCounts(t *testing.T) {
	tasks := []task.Task{
		mkTask("a", task.StatusTodo),
		mkTask("b", task.StatusTodo),
		mkTask("c", task.StatusInProgress),
		mkTask("d", task.StatusDone),
	}
	c := countByStatus(tasks)
	if c.Todo != 2 {
		t.Errorf("Todo: want 2, got %d", c.Todo)
	}
	if c.InProgress != 1 {
		t.Errorf("InProgress: want 1, got %d", c.InProgress)
	}
	if c.Done != 1 {
		t.Errorf("Done: want 1, got %d", c.Done)
	}
}

func TestFingerprintStability(t *testing.T) {
	a := Anomaly{Kind: KindLostAgent, TaskID: "abc"}
	b := Anomaly{Kind: KindLostAgent, TaskID: "abc"}
	if Fingerprint(a.Kind, a.TaskID, nil) != Fingerprint(b.Kind, b.TaskID, nil) {
		t.Fatal("fingerprint not stable for identical anomalies")
	}
	if Fingerprint(KindBottleneck, "", map[string]any{"status": "plan-review"}) ==
		Fingerprint(KindBottleneck, "", map[string]any{"status": "in-progress"}) {
		t.Fatal("bottleneck fingerprints should differ by status")
	}
}

func sameKinds(got, want []AnomalyKind) bool {
	if len(got) != len(want) {
		return false
	}
	wantCopy := append([]AnomalyKind(nil), want...)
	SortAnomalyKinds(wantCopy)
	gotCopy := append([]AnomalyKind(nil), got...)
	SortAnomalyKinds(gotCopy)
	for i := range gotCopy {
		if gotCopy[i] != wantCopy[i] {
			return false
		}
	}
	return true
}

// SortAnomalyKinds is a small test helper kept package-private.
func SortAnomalyKinds(k []AnomalyKind) {
	for i := 1; i < len(k); i++ {
		for j := i; j > 0 && k[j] < k[j-1]; j-- {
			k[j], k[j-1] = k[j-1], k[j]
		}
	}
}
