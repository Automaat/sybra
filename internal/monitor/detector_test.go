package monitor

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
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

func TestDetectStuckHumanBlocked_Evidence(t *testing.T) {
	now := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	cfg := defaultCfg()

	t.Run("includes status_reason when set", func(t *testing.T) {
		tk := mkTask("a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.StatusReason = "watchdog: turn limit exceeded"
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
		}
		got, ok := report.Anomalies[0].Evidence["status_reason"]
		if !ok {
			t.Fatal("evidence missing status_reason")
		}
		if got != "watchdog: turn limit exceeded" {
			t.Errorf("status_reason = %q, want %q", got, "watchdog: turn limit exceeded")
		}
	})

	t.Run("omits status_reason when empty", func(t *testing.T) {
		tk := mkTask("a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
		}
		if _, ok := report.Anomalies[0].Evidence["status_reason"]; ok {
			t.Error("evidence should not contain status_reason when empty")
		}
	})

	t.Run("includes last_agent_role and last_agent_state", func(t *testing.T) {
		tk := mkTask("a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{
				{AgentID: "old", Role: "plan", State: "stopped"},
				{AgentID: "new", Role: "fix-review", State: "stopped"},
			}
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
		}
		ev := report.Anomalies[0].Evidence
		if ev["last_agent_role"] != "fix-review" {
			t.Errorf("last_agent_role = %q, want fix-review", ev["last_agent_role"])
		}
		if ev["last_agent_state"] != "stopped" {
			t.Errorf("last_agent_state = %q, want stopped", ev["last_agent_state"])
		}
	})

	t.Run("omits last_agent_role when role is empty", func(t *testing.T) {
		tk := mkTask("a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{
				{AgentID: "impl", Role: "", State: "stopped"},
			}
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
		}
		ev := report.Anomalies[0].Evidence
		if _, ok := ev["last_agent_role"]; ok {
			t.Error("last_agent_role should be omitted when empty")
		}
		if ev["last_agent_state"] != "stopped" {
			t.Errorf("last_agent_state = %q, want stopped", ev["last_agent_state"])
		}
	})

	t.Run("omits last_agent fields when no runs", func(t *testing.T) {
		tk := mkTask("a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
		}
		ev := report.Anomalies[0].Evidence
		if _, ok := ev["last_agent_role"]; ok {
			t.Error("last_agent_role should be absent when no runs")
		}
		if _, ok := ev["last_agent_state"]; ok {
			t.Error("last_agent_state should be absent when no runs")
		}
	})

	t.Run("includes pr_number when set", func(t *testing.T) {
		tk := mkTask("a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.PRNumber = 42
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
		}
		got, ok := report.Anomalies[0].Evidence["pr_number"]
		if !ok {
			t.Fatal("evidence missing pr_number")
		}
		if got != 42 {
			t.Errorf("pr_number = %v, want 42", got)
		}
	})

	t.Run("omits pr_number when zero", func(t *testing.T) {
		tk := mkTask("a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
		}
		if _, ok := report.Anomalies[0].Evidence["pr_number"]; ok {
			t.Error("evidence should not contain pr_number when zero")
		}
	})
}

func TestParseHumanReviewDecision(t *testing.T) {
	cases := []struct {
		name   string
		result string
		want   string
	}{
		{
			name:   "human verdict",
			result: "Analysis complete.\n\n```sybra-verdict\n{\"decision\":\"human\",\"summary\":\"needs human\"}\n```",
			want:   "human",
		},
		{
			name:   "sybra_bug verdict",
			result: "```sybra-verdict\n{\"decision\":\"sybra_bug\",\"summary\":\"workflow misfire\"}\n```",
			want:   "sybra_bug",
		},
		{
			name:   "case insensitive decision",
			result: "```sybra-verdict\n{\"decision\":\"HUMAN\"}\n```",
			want:   "human",
		},
		{
			name:   "no verdict block",
			result: "No structured verdict here.",
			want:   "",
		},
		{
			name:   "invalid decision value",
			result: "```sybra-verdict\n{\"decision\":\"maybe\"}\n```",
			want:   "",
		},
		{
			name:   "malformed json",
			result: "```sybra-verdict\n{broken\n```",
			want:   "",
		},
		{
			name:   "empty result",
			result: "",
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseHumanReviewDecision(tc.result)
			if got != tc.want {
				t.Errorf("parseHumanReviewDecision = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDetectStuckHumanBlocked_HumanReviewVerdict(t *testing.T) {
	now := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	cfg := defaultCfg()

	t.Run("still fires anomaly when result has human decision", func(t *testing.T) {
		// Human-review confirmed genuine human-required: operator reminder must still fire.
		tk := mkTask("a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{
				{
					AgentID: "rev1", Role: "human-review", State: "stopped",
					Result: "Analysis.\n\n```sybra-verdict\n{\"decision\":\"human\",\"summary\":\"needs direct work\"}\n```",
				},
			}
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly (confirmed human-blocked must not be suppressed), got %d", len(report.Anomalies))
		}
		if v, _ := report.Anomalies[0].Evidence["human_review_verdict"].(string); v != "human" {
			t.Fatalf("want human_review_verdict=human in evidence, got %q", v)
		}
	})

	t.Run("still fires anomaly when Verdict field is human", func(t *testing.T) {
		tk := mkTask("a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{
				{AgentID: "rev1", Role: "human-review", State: "stopped", Verdict: "human"},
			}
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly (confirmed human-blocked must not be suppressed), got %d", len(report.Anomalies))
		}
		if v, _ := report.Anomalies[0].Evidence["human_review_verdict"].(string); v != "human" {
			t.Fatalf("want human_review_verdict=human in evidence, got %q", v)
		}
	})

	t.Run("fires anomaly when result has sybra_bug decision", func(t *testing.T) {
		tk := mkTask("a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{
				{
					AgentID: "rev1", Role: "human-review", State: "stopped",
					Result: "```sybra-verdict\n{\"decision\":\"sybra_bug\"}\n```",
				},
			}
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly (sybra_bug fires), got %d", len(report.Anomalies))
		}
		got, ok := report.Anomalies[0].Evidence["human_review_verdict"]
		if !ok {
			t.Fatal("evidence missing human_review_verdict")
		}
		if got != "sybra_bug" {
			t.Errorf("human_review_verdict = %q, want sybra_bug", got)
		}
	})

	t.Run("omits human_review_verdict when result is empty", func(t *testing.T) {
		tk := mkTask("a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{
				{AgentID: "rev1", Role: "human-review", State: "stopped", Result: ""},
			}
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
		}
		if _, ok := report.Anomalies[0].Evidence["human_review_verdict"]; ok {
			t.Error("evidence should not contain human_review_verdict when result is empty")
		}
	})

	t.Run("omits human_review_verdict when agent still running", func(t *testing.T) {
		tk := mkTask("a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{
				{
					AgentID: "rev1", Role: "human-review", State: "running",
					Result: "```sybra-verdict\n{\"decision\":\"human\"}\n```",
				},
			}
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
		}
		if _, ok := report.Anomalies[0].Evidence["human_review_verdict"]; ok {
			t.Error("evidence should not contain human_review_verdict when agent is still running")
		}
	})

	t.Run("omits human_review_verdict when role is not human-review", func(t *testing.T) {
		tk := mkTask("a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{
				{
					AgentID: "fix1", Role: "fix-review", State: "stopped",
					Result: "```sybra-verdict\n{\"decision\":\"human\"}\n```",
				},
			}
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
		}
		if _, ok := report.Anomalies[0].Evidence["human_review_verdict"]; ok {
			t.Error("evidence should not contain human_review_verdict when role is not human-review")
		}
	})

	t.Run("uses Verdict field when set, even with truncated Result", func(t *testing.T) {
		// Simulates a long review response where the sybra-verdict block was
		// cut off during Result truncation but the Verdict field was populated
		// from the live agent output at completion time.
		tk := mkTask("a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{
				{
					AgentID: "rev1", Role: "human-review", State: "stopped",
					Result:  "Very long diagnostic writeup... (truncated)",
					Verdict: "sybra_bug",
				},
			}
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
		}
		got, ok := report.Anomalies[0].Evidence["human_review_verdict"]
		if !ok {
			t.Fatal("evidence missing human_review_verdict")
		}
		if got != "sybra_bug" {
			t.Errorf("human_review_verdict = %q, want %q", got, "sybra_bug")
		}
	})

	t.Run("Verdict field takes priority over Result parsing", func(t *testing.T) {
		// Verdict field wins even when Result also contains a parseable block.
		tk := mkTask("a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{
				{
					AgentID: "rev1", Role: "human-review", State: "stopped",
					Result:  "```sybra-verdict\n{\"decision\":\"human\"}\n```",
					Verdict: "sybra_bug",
				},
			}
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
		}
		got, ok := report.Anomalies[0].Evidence["human_review_verdict"]
		if !ok {
			t.Fatal("evidence missing human_review_verdict")
		}
		if got != "sybra_bug" {
			t.Errorf("human_review_verdict = %q, want sybra_bug (Verdict field wins)", got)
		}
	})

	t.Run("uses earlier verdict when last run has no parseable result", func(t *testing.T) {
		// Simulates: human-review ran and returned "human", then a second
		// human-review attempt was dispatched but failed with an API error
		// (result has no sybra-verdict block). The earlier verdict must win.
		tk := mkTask("a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{
				{
					AgentID: "rev1", Role: "human-review", State: "stopped",
					Result: "Analysis.\n\n```sybra-verdict\n{\"decision\":\"human\",\"summary\":\"project not registered\"}\n```",
				},
				{
					AgentID: "rev2", Role: "human-review", State: "stopped",
					Result: "API Error: 529 Overloaded. This is a server-side issue, usually temporary.",
				},
			}
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
		}
		got, ok := report.Anomalies[0].Evidence["human_review_verdict"]
		if !ok {
			t.Fatal("evidence missing human_review_verdict; failed last run should not mask earlier verdict")
		}
		if got != "human" {
			t.Errorf("human_review_verdict = %q, want human", got)
		}
	})
}

func TestDetectStuckHumanBlocked_RequiresLLM(t *testing.T) {
	now := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	cfg := defaultCfg()

	t.Run("RequiresLLM=false when status is plan-review", func(t *testing.T) {
		tk := mkTask("a", task.StatusPlanReview, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
		}
		if report.Anomalies[0].RequiresLLM {
			t.Error("RequiresLLM must be false for plan-review — action is deterministic")
		}
	})

	t.Run("RequiresLLM=false when human-review verdict is human", func(t *testing.T) {
		tk := mkTask("a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{
				{AgentID: "rev1", Role: "human-review", State: "stopped", Verdict: "human"},
			}
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
		}
		if report.Anomalies[0].RequiresLLM {
			t.Error("RequiresLLM must be false when human-review confirmed verdict=human")
		}
	})

	t.Run("RequiresLLM=true when human-review verdict is sybra_bug", func(t *testing.T) {
		tk := mkTask("a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{
				{AgentID: "rev1", Role: "human-review", State: "stopped", Verdict: "sybra_bug"},
			}
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
		}
		if !report.Anomalies[0].RequiresLLM {
			t.Error("RequiresLLM must be true when verdict is sybra_bug")
		}
	})

	t.Run("RequiresLLM=true when no human-review ran", func(t *testing.T) {
		tk := mkTask("a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
		}
		if !report.Anomalies[0].RequiresLLM {
			t.Error("RequiresLLM must be true when no human-review verdict is known")
		}
	})

	t.Run("RequiresLLM=true when human-review still running", func(t *testing.T) {
		tk := mkTask("a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{
				{AgentID: "rev1", Role: "human-review", State: "running", Verdict: "human"},
			}
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
		}
		// Verdict field on a running agent is not surfaced, so RequiresLLM stays true.
		if !report.Anomalies[0].RequiresLLM {
			t.Error("RequiresLLM must be true when human-review is still running")
		}
	})

	t.Run("RequiresLLM=false when earlier run has human verdict and last run failed", func(t *testing.T) {
		// Simulates the real-world case: first human-review returned "human",
		// second attempt hit API 529 with no parseable result. The task is still
		// genuinely human-required — should not spawn an LLM investigation.
		tk := mkTask("a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{
				{AgentID: "rev1", Role: "human-review", State: "stopped", Verdict: "human"},
				{AgentID: "rev2", Role: "human-review", State: "stopped",
					Result: "API Error: 529 Overloaded."},
			}
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
		}
		if report.Anomalies[0].RequiresLLM {
			t.Error("RequiresLLM must be false: earlier human verdict must not be masked by a failed last run")
		}
	})
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
