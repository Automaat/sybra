package monitor

import (
	"strings"
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
		PRGapGraceMinutes:    15,
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
	return mkTaskAt(time.Now().UTC(), id, status, opts...)
}

func mkTaskAt(now time.Time, id string, status task.Status, opts ...func(*task.Task)) task.Task {
	t := task.Task{
		ID:        id,
		Title:     "task " + id,
		Status:    status,
		AgentMode: task.AgentModeHeadless,
		Tags:      []string{"medium"},
		UpdatedAt: now.Add(-1 * time.Hour),
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
					mkTaskAt(now, "a", task.StatusInProgress),
					mkTaskAt(now, "b", task.StatusDone),
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
					mkTaskAt(now, "a", task.StatusInProgress),
					mkTaskAt(now, "b", task.StatusInProgress),
					mkTaskAt(now, "c", task.StatusInProgress),
					mkTaskAt(now, "d", task.StatusInProgress),
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
				Tasks: []task.Task{mkTaskAt(now, "a", task.StatusTodo, func(t *task.Task) { t.AgentMode = "" })},
				Cfg:   cfg,
			},
			want: []AnomalyKind{KindUntriaged},
		},
		{
			name: "untriaged on todo missing tags",
			in: DetectInput{
				Now:   now,
				Tasks: []task.Task{mkTaskAt(now, "a", task.StatusTodo, func(t *task.Task) { t.Tags = nil })},
				Cfg:   cfg,
			},
			want: []AnomalyKind{KindUntriaged},
		},
		{
			name: "untriaged not flagged on done tasks",
			in: DetectInput{
				Now: now,
				Tasks: []task.Task{mkTaskAt(now, "a", task.StatusDone, func(t *task.Task) {
					t.Tags = nil
					t.AgentMode = ""
				})},
				Cfg: cfg,
			},
			want: nil,
		},
		{
			name: "untriaged not flagged within grace period",
			in: DetectInput{
				Now: now,
				Tasks: []task.Task{mkTaskAt(now, "a", task.StatusTodo, func(t *task.Task) {
					t.Tags = nil
					t.CreatedAt = now
				})},
				Cfg: cfg,
			},
			want: nil,
		},
		{
			name: "untriaged flagged after grace period",
			in: DetectInput{
				Now: now,
				Tasks: []task.Task{mkTaskAt(now, "a", task.StatusTodo, func(t *task.Task) {
					t.Tags = nil
					t.CreatedAt = now.Add(-20 * time.Minute)
				})},
				Cfg: cfg,
			},
			want: []AnomalyKind{KindUntriaged},
		},
		{
			name: "plan-review past budget is NOT flagged — waits indefinitely for human",
			in: DetectInput{
				Now: now,
				Tasks: []task.Task{mkTaskAt(now, "a", task.StatusPlanReview, func(t *task.Task) {
					t.UpdatedAt = now.Add(-9 * time.Hour)
				})},
				Cfg: cfg,
			},
			want: nil,
		},
		{
			name: "stuck_human_blocked on human-required past budget",
			in: DetectInput{
				Now: now,
				Tasks: []task.Task{mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
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
				Tasks: []task.Task{mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
					t.UpdatedAt = now.Add(-2 * time.Hour)
				})},
				Cfg: cfg,
			},
			want: nil,
		},
		{
			name: "pr_gap on stale in-review with project but no PR",
			in: DetectInput{
				Now: now,
				Tasks: []task.Task{mkTaskAt(now, "a", task.StatusInReview, func(t *task.Task) {
					t.ProjectID = "owner/repo"
					t.UpdatedAt = now.Add(-20 * time.Minute)
				})},
				Cfg: cfg,
			},
			want: []AnomalyKind{KindPRGap},
		},
		{
			name: "pr_gap suppressed during grace period",
			in: DetectInput{
				Now: now,
				Tasks: []task.Task{mkTaskAt(now, "a", task.StatusInReview, func(t *task.Task) {
					t.ProjectID = "owner/repo"
					t.UpdatedAt = now.Add(-5 * time.Minute)
				})},
				Cfg: cfg,
			},
			want: nil,
		},
		{
			name: "pr_gap suppressed when PR is set",
			in: DetectInput{
				Now: now,
				Tasks: []task.Task{mkTaskAt(now, "a", task.StatusInReview, func(t *task.Task) {
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
				Tasks: []task.Task{mkTaskAt(now, "a", task.StatusInReview)},
				Cfg:   cfg,
			},
			want: nil,
		},
		{
			name: "lost_agent when in-progress without recent agent event",
			in: DetectInput{
				Now:        now,
				Tasks:      []task.Task{mkTaskAt(now, "a", task.StatusInProgress)},
				LiveAgents: []liveAgent{},
				Cfg:        cfg,
			},
			want: []AnomalyKind{KindLostAgent},
		},
		{
			name: "lost_agent suppressed for umbrella tracker (rollup status, no agent)",
			in: DetectInput{
				Now: now,
				Tasks: []task.Task{mkTaskAt(now, "a", task.StatusInProgress, func(t *task.Task) {
					t.TaskType = task.TaskTypeUmbrella
				})},
				LiveAgents: []liveAgent{},
				Cfg:        cfg,
			},
			want: nil,
		},
		{
			name: "stuck_human_blocked still fires for a stalled umbrella tracker",
			in: DetectInput{
				Now: now,
				Tasks: []task.Task{mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
					t.TaskType = task.TaskTypeUmbrella
					t.UpdatedAt = now.Add(-24 * time.Hour)
				})},
				Cfg: cfg,
			},
			want: []AnomalyKind{KindStuckHumanBlocked},
		},
		{
			name: "lost_agent suppressed by recent agent.* event",
			in: DetectInput{
				Now:   now,
				Tasks: []task.Task{mkTaskAt(now, "a", task.StatusInProgress)},
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
				Tasks:      []task.Task{mkTaskAt(now, "a", task.StatusInProgress)},
				LiveAgents: []liveAgent{{TaskID: "a", Running: true}},
				Cfg:        cfg,
			},
			want: nil,
		},
		{
			name: "lost_agent suppressed when running run started recently",
			in: DetectInput{
				Now: now,
				Tasks: []task.Task{mkTaskAt(now, "a", task.StatusInProgress, func(t *task.Task) {
					t.AgentRuns = []task.AgentRun{
						{AgentID: "x", State: "running", StartedAt: now.Add(-2 * time.Minute)},
					}
				})},
				LiveAgents: []liveAgent{},
				Cfg:        cfg,
			},
			want: nil,
		},
		{
			name: "lost_agent fires when running run started outside window",
			in: DetectInput{
				Now: now,
				Tasks: []task.Task{mkTaskAt(now, "a", task.StatusInProgress, func(t *task.Task) {
					t.AgentRuns = []task.AgentRun{
						{AgentID: "x", State: "running", StartedAt: now.Add(-20 * time.Minute)},
					}
				})},
				LiveAgents: []liveAgent{},
				Cfg:        cfg,
			},
			want: []AnomalyKind{KindLostAgent},
		},
		{
			name: "lost_agent suppressed when task just transitioned to in-progress and dispatch hasn't recorded a run yet",
			in: DetectInput{
				Now: now,
				Tasks: []task.Task{mkTaskAt(now, "a", task.StatusInProgress, func(t *task.Task) {
					// Prior completed run from an earlier lifecycle, neither
					// running nor recent — only the status-transition
					// timestamp is fresh, mirroring dispatch latency between
					// the status flip and AddRun/the first agent.* audit
					// event landing.
					t.AgentRuns = []task.AgentRun{
						{AgentID: "x", State: "stopped", StartedAt: now.Add(-4 * time.Hour)},
					}
					t.StatusChangedAt = now.Add(-2 * time.Minute)
				})},
				LiveAgents: []liveAgent{},
				Cfg:        cfg,
			},
			want: nil,
		},
		{
			name: "lost_agent fires for legacy task with no StatusChangedAt and recent UpdatedAt",
			in: DetectInput{
				Now: now,
				Tasks: []task.Task{mkTaskAt(now, "a", task.StatusInProgress, func(t *task.Task) {
					// Legacy tasks are migrated by the store on write; the
					// detector must not keep treating UpdatedAt as status
					// transition evidence.
					t.AgentRuns = []task.AgentRun{
						{AgentID: "x", State: "stopped", StartedAt: now.Add(-4 * time.Hour)},
					}
					t.UpdatedAt = now.Add(-2 * time.Minute)
				})},
				LiveAgents: []liveAgent{},
				Cfg:        cfg,
			},
			want: []AnomalyKind{KindLostAgent},
		},
		{
			name: "lost_agent still fires when only UpdatedAt (not StatusChangedAt) is recent",
			in: DetectInput{
				Now: now,
				Tasks: []task.Task{mkTaskAt(now, "a", task.StatusInProgress, func(t *task.Task) {
					// The task transitioned to in-progress long ago (outside
					// the window) but something unrelated (a tag edit, an
					// audit sidecar write) touched it moments ago. UpdatedAt
					// alone must never suppress detection — only a real
					// status transition may.
					t.StatusChangedAt = now.Add(-1 * time.Hour)
					t.UpdatedAt = now.Add(-2 * time.Minute)
				})},
				LiveAgents: []liveAgent{},
				Cfg:        cfg,
			},
			want: []AnomalyKind{KindLostAgent},
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
				Tasks:      []task.Task{mkTaskAt(now, "a", task.StatusInProgress)},
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

func TestDetectPRGap_EvidenceIncludesTiming(t *testing.T) {
	now := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	cfg := defaultCfg()
	updatedAt := now.Add(-20 * time.Minute)
	tk := mkTaskAt(now, "a", task.StatusInReview, func(t *task.Task) {
		t.ProjectID = "owner/repo"
		t.Branch = "task-a"
		t.UpdatedAt = updatedAt
	})

	report := Detect(DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg})
	if len(report.Anomalies) != 1 {
		t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
	}
	ev := report.Anomalies[0].Evidence
	if ev["updated_at"] != updatedAt.Format(time.RFC3339) {
		t.Errorf("updated_at = %v, want %s", ev["updated_at"], updatedAt.Format(time.RFC3339))
	}
	if ev["dwell_minutes"] != 20.0 {
		t.Errorf("dwell_minutes = %v, want 20", ev["dwell_minutes"])
	}
	if ev["grace_minutes"] != 15.0 {
		t.Errorf("grace_minutes = %v, want 15", ev["grace_minutes"])
	}
}

func TestDetectStuckHumanBlocked_Evidence(t *testing.T) {
	now := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	cfg := defaultCfg()

	t.Run("includes status_reason when set", func(t *testing.T) {
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
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
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
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
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
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
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
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
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
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
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
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
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
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

func TestDetectStuckHumanBlocked_HumanReviewVerdict(t *testing.T) {
	now := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	cfg := defaultCfg()

	t.Run("still fires anomaly when result has human decision", func(t *testing.T) {
		// Human-review confirmed genuine human-required: operator reminder must still fire.
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
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
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
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
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{
				{
					AgentID: "rev1", Role: "human-review", State: "stopped",
					Result: "```sybra-verdict\n{\"decision\":\"sybra_bug\",\"summary\":\"workflow misfire\"}\n```",
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
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
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
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
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
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
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
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
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
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
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

	t.Run("omits verdict when running human-review follows stopped human verdict", func(t *testing.T) {
		// History: [stopped verdict=human, running human-review].
		// Older verdict must not be surfaced while a newer review is still in flight.
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{
				{AgentID: "rev1", Role: "human-review", State: "stopped", Verdict: "human"},
				{AgentID: "rev2", Role: "human-review", State: "running"},
			}
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
		}
		if _, ok := report.Anomalies[0].Evidence["human_review_verdict"]; ok {
			t.Error("evidence must not contain human_review_verdict while a newer human-review is running")
		}
	})

	t.Run("uses earlier verdict when last run has no parseable result", func(t *testing.T) {
		// Simulates: human-review ran and returned "human", then a second
		// human-review attempt was dispatched but failed with an API error
		// (result has no sybra-verdict block). The earlier verdict must win.
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
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

	t.Run("plan-review is never flagged, regardless of dwell", func(t *testing.T) {
		tk := mkTaskAt(now, "a", task.StatusPlanReview, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		for _, a := range report.Anomalies {
			if a.Kind == KindStuckHumanBlocked {
				t.Fatal("plan-review must not produce a stuck_human_blocked anomaly — it waits indefinitely")
			}
		}
	})

	t.Run("RequiresLLM=false when human-review verdict is human", func(t *testing.T) {
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
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

	t.Run("RequiresLLM=false when Verdict field empty but Result has human verdict", func(t *testing.T) {
		// Simulates the real-world case for task af1213a4: budget-exhausted task
		// escalated to human-required, single human-review run completed with the
		// decision embedded in the Result text (Verdict field was not yet
		// populated in this era). Detector must fall back to parsing Result.
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{
				{
					AgentID: "rev1", Role: "human-review", State: "stopped",
					Result: "Analysis.\n\n```sybra-verdict\n{\"decision\":\"human\",\"summary\":\"budget exhausted\"}\n```",
				},
			}
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
		}
		if report.Anomalies[0].RequiresLLM {
			t.Error("RequiresLLM must be false when Result contains a human verdict (Verdict field fallback)")
		}
		if v, _ := report.Anomalies[0].Evidence["human_review_verdict"].(string); v != "human" {
			t.Errorf("human_review_verdict = %q, want human", v)
		}
	})

	t.Run("RequiresLLM=true when human-review verdict is sybra_bug", func(t *testing.T) {
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
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
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
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
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
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

	t.Run("RequiresLLM=true when running human-review follows stopped human verdict", func(t *testing.T) {
		// History: [stopped verdict=human, running human-review].
		// Running review blocks stale verdict — treat as unknown until it completes.
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{
				{AgentID: "rev1", Role: "human-review", State: "stopped", Verdict: "human"},
				{AgentID: "rev2", Role: "human-review", State: "running"},
			}
		})
		in := DetectInput{Now: now, Tasks: []task.Task{tk}, Cfg: cfg}
		report := Detect(in)
		if len(report.Anomalies) != 1 {
			t.Fatalf("want 1 anomaly, got %d", len(report.Anomalies))
		}
		if !report.Anomalies[0].RequiresLLM {
			t.Error("RequiresLLM must be true while a newer human-review is still running")
		}
	})

	t.Run("RequiresLLM=false when earlier run has human verdict and last run failed", func(t *testing.T) {
		// Simulates the real-world case: first human-review returned "human",
		// second attempt hit API 529 with no parseable result. The task is still
		// genuinely human-required — should not spawn an LLM investigation.
		tk := mkTaskAt(now, "a", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{
				{AgentID: "rev1", Role: "human-review", State: "stopped", Verdict: "human"},
				{
					AgentID: "rev2", Role: "human-review", State: "stopped",
					Result: "API Error: 529 Overloaded.",
				},
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

// lostAgentInvestigationTask builds a local investigation task the way
// monitorRoutingSink.Submit would file it for a KindLostAgent anomaly on
// originID, using the same fingerprint/body shape DeterministicIssueBody
// produces (see internal/monitor/prompts.go).
func lostAgentInvestigationTask(now time.Time, id, originID string, status task.Status) task.Task {
	fp := Fingerprint(KindLostAgent, originID, nil)
	body := "## Detection\n- Kind: `lost_agent`\n- Fingerprint: `" + fp + "`\n\n" +
		"## Affected task\n- `" + originID + "`\n\n"
	return mkTaskAt(now, id, status, func(t *task.Task) {
		t.Tags = []string{lostAgentInvestigationTag}
		t.Body = body
	})
}

func TestDetectStuckHumanBlocked_KnownLostAgentCause(t *testing.T) {
	now := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	cfg := defaultCfg()
	findStuck := func(t *testing.T, report Report) Anomaly {
		t.Helper()
		for i := range report.Anomalies {
			if report.Anomalies[i].Kind == KindStuckHumanBlocked {
				return report.Anomalies[i]
			}
		}
		t.Fatal("want a stuck_human_blocked anomaly for the stalled task")
		return Anomaly{}
	}

	t.Run("RequiresLLM=false and evidence set when an open lost_agent investigation tracks this task", func(t *testing.T) {
		stuck := mkTaskAt(now, "orig", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
		})
		investigation := lostAgentInvestigationTask(now, "inv", "orig", task.StatusTodo)
		in := DetectInput{Now: now, Tasks: []task.Task{stuck, investigation}, Cfg: cfg}
		got := findStuck(t, Detect(in))
		if got.RequiresLLM {
			t.Error("RequiresLLM must be false when an open lost_agent investigation already tracks this task")
		}
		if known, _ := got.Evidence["known_lost_agent_investigation"].(bool); !known {
			t.Error("evidence missing known_lost_agent_investigation=true")
		}
	})

	t.Run("RequiresLLM=true when the investigation task is terminal", func(t *testing.T) {
		stuck := mkTaskAt(now, "orig", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
		})
		investigation := lostAgentInvestigationTask(now, "inv", "orig", task.StatusDone)
		in := DetectInput{Now: now, Tasks: []task.Task{stuck, investigation}, Cfg: cfg}
		got := findStuck(t, Detect(in))
		if !got.RequiresLLM {
			t.Error("a closed/done investigation task must not suppress the LLM re-investigation")
		}
	})

	t.Run("RequiresLLM=true once the task already carries the auto-retried tag", func(t *testing.T) {
		stuck := mkTaskAt(now, "orig", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.Tags = []string{"medium", monitorAutoRetriedTag}
		})
		investigation := lostAgentInvestigationTask(now, "inv", "orig", task.StatusTodo)
		in := DetectInput{Now: now, Tasks: []task.Task{stuck, investigation}, Cfg: cfg}
		got := findStuck(t, Detect(in))
		if !got.RequiresLLM {
			t.Error("a task already auto-retried once must fall back to the normal LLM path on a second stall")
		}
	})
	t.Run("RequiresLLM=true when the investigation predates the current agent run", func(t *testing.T) {
		stuck := mkTaskAt(now, "orig", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{
				{AgentID: "new-run", State: "stopped", StartedAt: now.Add(-2 * time.Hour)},
			}
		})
		investigation := lostAgentInvestigationTask(now, "inv", "orig", task.StatusTodo)
		investigation.UpdatedAt = now.Add(-4 * time.Hour)
		in := DetectInput{Now: now, Tasks: []task.Task{stuck, investigation}, Cfg: cfg}
		got := findStuck(t, Detect(in))
		if !got.RequiresLLM {
			t.Error("a stale investigation from an earlier run must not suppress the LLM re-investigation")
		}
		if known, _ := got.Evidence["known_lost_agent_investigation"].(bool); known {
			t.Error("stale investigation must not set known_lost_agent_investigation=true")
		}
	})

	t.Run("RequiresLLM=true when the investigation fingerprint does not match the affected task", func(t *testing.T) {
		stuck := mkTaskAt(now, "orig", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
		})
		investigation := lostAgentInvestigationTask(now, "inv", "orig", task.StatusTodo)
		investigation.Body = strings.ReplaceAll(
			investigation.Body,
			"- Fingerprint: `"+Fingerprint(KindLostAgent, "orig", nil)+"`",
			"- Fingerprint: `"+Fingerprint(KindLostAgent, "other", nil)+"`",
		)
		in := DetectInput{Now: now, Tasks: []task.Task{stuck, investigation}, Cfg: cfg}
		got := findStuck(t, Detect(in))
		if !got.RequiresLLM {
			t.Error("a mismatched investigation fingerprint must not suppress the LLM re-investigation")
		}
	})
}

func TestCounts(t *testing.T) {
	now := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	tasks := []task.Task{
		mkTaskAt(now, "a", task.StatusTodo),
		mkTaskAt(now, "b", task.StatusTodo),
		mkTaskAt(now, "c", task.StatusInProgress),
		mkTaskAt(now, "d", task.StatusDone),
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
