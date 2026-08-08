package workflow

import (
	"cmp"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/clock"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/metrics"
	providerpkg "github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/taskstatus"
	"github.com/Automaat/sybra/internal/watchdogreason"
	"github.com/Automaat/sybra/internal/worktreeerr"
)

func TestResumeStalled_RecordsFallbackMetric(t *testing.T) {
	if !metrics.Enabled() {
		if err := metrics.Init(config.MetricsConfig{Enabled: true}); err != nil {
			t.Fatalf("metrics.Init: %v", err)
		}
	}

	before := workflowMetricValue(t, "sybra_orchestrator_resume_stalled_fallbacks_total", nil)

	store := newTestStore(t)
	tasks := &memTasks{tasks: map[string]*TaskInfo{
		"t1": {
			ID:         "t1",
			Generation: 1,
			Status:     "in-progress",
			Workflow: &Execution{
				WorkflowID:  "test-simple",
				CurrentStep: "implement",
				State:       ExecWaiting,
			},
		},
	}, gets: map[string]int{}}
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	engine.ResumeStalled()

	if len(agents.calls) != 1 {
		t.Fatalf("StartAgent calls = %d, want 1", len(agents.calls))
	}
	after := workflowMetricValue(t, "sybra_orchestrator_resume_stalled_fallbacks_total", nil)
	if after != before+1 {
		t.Fatalf("fallback metric delta = %.0f, want 1 (before=%.0f after=%.0f)", after-before, before, after)
	}
}

func TestResumeStalled_WatchdogHangRetriesThenEscalates(t *testing.T) {
	tests := []struct {
		name       string
		retries    string
		wantStarts int
		wantStatus taskstatus.Status
		wantReason string
		wantRetry  string
		baseline   string
		wantClean  string
	}{
		{
			name:       "first hang retries",
			wantStarts: 1,
			wantStatus: "in-progress",
			wantRetry:  "1",
			baseline:   "abc123",
			wantClean:  "abc123",
		},
		{
			name:       "budget exhausted escalates",
			retries:    "2",
			wantStarts: 0,
			wantStatus: "human-required",
			wantReason: "watchdog hang: retry budget exhausted after 2 clean re-dispatches",
			wantRetry:  "2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewTestEngine(store, tasks, agents, discardLogger())
			vars := map[string]string{}
			if tc.retries != "" {
				vars[watchdogHangRetryKey("implement")] = tc.retries
			}
			if tc.baseline != "" {
				vars[tamperBaselineVar("implement")] = tc.baseline
			}
			tasks.Put(TaskInfo{
				ID:           "t1",
				Status:       "in-progress",
				StatusReason: "watchdog hang: no stream activity",
				AgentMode:    "headless",
				Workflow: &Execution{
					WorkflowID:  "test-simple",
					CurrentStep: "implement",
					State:       ExecWaiting,
					Variables:   vars,
					StartedAt:   time.Now().UTC(),
				},
			})

			engine.ResumeStalled()

			if got := agents.CallCount(); got != tc.wantStarts {
				t.Fatalf("StartAgent calls = %d, want %d", got, tc.wantStarts)
			}
			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.StatusReason != tc.wantReason {
				t.Fatalf("status_reason = %q, want %q", got.StatusReason, tc.wantReason)
			}
			if got.Workflow.Variables[watchdogHangRetryKey("implement")] != tc.wantRetry {
				t.Fatalf("hang retry var = %q, want %q", got.Workflow.Variables[watchdogHangRetryKey("implement")], tc.wantRetry)
			}
			if tc.wantStatus == "human-required" && got.Workflow.State != ExecFailed {
				t.Fatalf("workflow state = %q, want ExecFailed so the operator can re-dispatch", got.Workflow.State)
			}
			if tc.wantStarts > 0 {
				if got := agents.calls[0].CleanRetryRef; got != tc.wantClean {
					t.Fatalf("clean retry ref = %q, want %q", got, tc.wantClean)
				}
				if got.Workflow.Variables[watchdogHangCleanRetryKey("implement")] != "" {
					t.Fatalf("clean retry marker = %q, want cleared after dispatch", got.Workflow.Variables[watchdogHangCleanRetryKey("implement")])
				}
			}
		})
	}
}

func TestResumeStalled_WatchdogStopImplementationRetriesThenEscalates(t *testing.T) {
	tests := []struct {
		name       string
		reason     string
		retries    string
		wantStarts int
		wantStatus taskstatus.Status
		wantReason string
		wantRetry  string
		baseline   string
		wantClean  string
	}{
		{
			name:       "structured first retry requeues implementation",
			reason:     "watchdog: loop stop: looping on toolchain setup",
			wantStarts: 1,
			wantStatus: "in-progress",
			wantRetry:  "1",
			baseline:   "abc123",
			wantClean:  "abc123",
		},
		{
			name:       "legacy first retry requeues implementation",
			reason:     "watchdog: looping on toolchain setup",
			wantStarts: 1,
			wantStatus: "in-progress",
			wantRetry:  "1",
			wantClean:  "HEAD",
		},
		{
			name:       "budget exhausted stays human required",
			reason:     "watchdog: loop stop: looping on toolchain setup",
			retries:    "2",
			wantStarts: 0,
			wantStatus: "human-required",
			wantReason: "watchdog stop: retry budget exhausted after 2 clean re-dispatches",
			wantRetry:  "2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewTestEngine(store, tasks, agents, discardLogger())
			vars := map[string]string{}
			if tc.retries != "" {
				vars[watchdogStopRetryKey("implement")] = tc.retries
			}
			if tc.baseline != "" {
				vars[tamperBaselineVar("implement")] = tc.baseline
			}
			tasks.Put(TaskInfo{
				ID:           "t1",
				Status:       "human-required",
				StatusReason: tc.reason,
				AgentMode:    "headless",
				Workflow: &Execution{
					WorkflowID:  "test-simple",
					CurrentStep: "implement",
					State:       ExecWaiting,
					Variables:   vars,
					StartedAt:   time.Now().UTC(),
				},
			})

			engine.ResumeStalled()

			if got := agents.CallCount(); got != tc.wantStarts {
				t.Fatalf("StartAgent calls = %d, want %d", got, tc.wantStarts)
			}
			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.StatusReason != tc.wantReason {
				t.Fatalf("status_reason = %q, want %q", got.StatusReason, tc.wantReason)
			}
			if got.Workflow.Variables[watchdogStopRetryKey("implement")] != tc.wantRetry {
				t.Fatalf("stop retry var = %q, want %q", got.Workflow.Variables[watchdogStopRetryKey("implement")], tc.wantRetry)
			}
			if tc.wantStatus == "human-required" && got.Workflow.State != ExecFailed {
				t.Fatalf("workflow state = %q, want ExecFailed after retry exhaustion", got.Workflow.State)
			}
			if tc.wantStarts > 0 {
				if got := agents.calls[0].CleanRetryRef; got != tc.wantClean {
					t.Fatalf("clean retry ref = %q, want %q", got, tc.wantClean)
				}
				if got.Workflow.Variables[watchdogHangCleanRetryKey("implement")] != "" {
					t.Fatalf("clean retry marker = %q, want cleared after dispatch", got.Workflow.Variables[watchdogHangCleanRetryKey("implement")])
				}
			}
		})
	}
}

func TestResumeStalled_WatchdogRewardHackingRetriesThenEscalates(t *testing.T) {
	tests := []struct {
		name       string
		role       string
		status     taskstatus.Status
		stepID     string
		workflowID string
		retries    string
		plan       string
		wantStarts int
		wantStatus taskstatus.Status
		wantReason string
		wantRetry  string
		wantClean  string
	}{
		{
			name:       "implementation retries from same worktree",
			role:       "implementation",
			status:     "in-progress",
			stepID:     "implement",
			workflowID: "test-simple",
			wantStarts: 1,
			wantStatus: "in-progress",
			wantRetry:  "1",
			wantClean:  "HEAD",
		},
		{
			name:       "planning retries when plan artifacts exist",
			role:       "plan",
			status:     "planning",
			stepID:     "plan",
			workflowID: "test-simple",
			plan:       "# Execution Plan\n\n- retry from existing artifacts",
			wantStarts: 1,
			wantStatus: "planning",
			wantRetry:  "1",
			wantClean:  "HEAD",
		},
		{
			name:       "implementation exhaustion escalates",
			role:       "implementation",
			status:     "in-progress",
			stepID:     "implement",
			workflowID: "test-simple",
			retries:    "2",
			wantStarts: 0,
			wantStatus: "human-required",
			wantReason: "watchdog: reward-hacking retry budget exhausted after 2 clean re-dispatches",
			wantRetry:  "2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewTestEngine(store, tasks, agents, discardLogger())
			vars := map[string]string{}
			if tc.retries != "" {
				vars[watchdogRewardHackingRetryKey(tc.stepID)] = tc.retries
			}
			tasks.Put(TaskInfo{
				ID:           "t1",
				Role:         tc.role,
				Status:       tc.status,
				StatusReason: "watchdog: reward-hacking retry: repeated search without editing",
				AgentMode:    "headless",
				Plan:         tc.plan,
				Workflow: &Execution{
					WorkflowID:  tc.workflowID,
					CurrentStep: tc.stepID,
					State:       ExecWaiting,
					Variables:   vars,
					StartedAt:   time.Now().UTC(),
				},
			})

			engine.ResumeStalled()

			if got := agents.CallCount(); got != tc.wantStarts {
				t.Fatalf("StartAgent calls = %d, want %d", got, tc.wantStarts)
			}
			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.StatusReason != tc.wantReason {
				t.Fatalf("status_reason = %q, want %q", got.StatusReason, tc.wantReason)
			}
			if got.Workflow.Variables[watchdogRewardHackingRetryKey(tc.stepID)] != tc.wantRetry {
				t.Fatalf("reward-hacking retry var = %q, want %q", got.Workflow.Variables[watchdogRewardHackingRetryKey(tc.stepID)], tc.wantRetry)
			}
			if tc.wantStatus == "human-required" && got.Workflow.State != ExecFailed {
				t.Fatalf("workflow state = %q, want ExecFailed after retry exhaustion", got.Workflow.State)
			}
			if tc.wantStarts > 0 {
				if got := agents.calls[0].CleanRetryRef; got != tc.wantClean {
					t.Fatalf("clean retry ref = %q, want %q", got, tc.wantClean)
				}
				if got.Workflow.Variables[watchdogHangCleanRetryKey(tc.stepID)] != "" {
					t.Fatalf("clean retry marker = %q, want cleared after dispatch", got.Workflow.Variables[watchdogHangCleanRetryKey(tc.stepID)])
				}
			}
		})
	}
}

func TestResumeStalled_WorktreeRepairRetriesThenExhausts(t *testing.T) {
	tests := []struct {
		name       string
		attempts   string
		wantStarts int
		wantStatus taskstatus.Status
		wantRetry  string
		wantExh    bool
	}{
		{
			name:       "first attempt retries in place",
			wantStarts: 1,
			wantStatus: "in-progress",
			wantRetry:  "1",
		},
		{
			name:       "budget exhausted stays blocked and marks exhausted",
			attempts:   "2",
			wantStarts: 0,
			wantStatus: "blocked",
			wantRetry:  "2",
			wantExh:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewTestEngine(store, tasks, agents, discardLogger())
			vars := map[string]string{}
			if tc.attempts != "" {
				vars[worktreeRepairRetryKey("implement")] = tc.attempts
			}
			tasks.Put(TaskInfo{
				ID:           "t1",
				Status:       "blocked",
				StatusReason: worktreeerr.RebaseBlockedReason,
				AgentMode:    "headless",
				Blocker: blocker.State{
					Kind:       blocker.KindWorktreeRepair,
					Actor:      blocker.ActorWorkflow,
					Code:       "rebase_failed",
					NextAction: "repair_worktree",
				},
				Workflow: &Execution{
					WorkflowID:  "test-simple",
					CurrentStep: "implement",
					State:       ExecWaiting,
					Variables:   vars,
					StartedAt:   time.Now().UTC(),
				},
			})

			engine.ResumeStalled()

			if got := agents.CallCount(); got != tc.wantStarts {
				t.Fatalf("StartAgent calls = %d, want %d", got, tc.wantStarts)
			}
			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.Workflow.Variables[worktreeRepairRetryKey("implement")] != tc.wantRetry {
				t.Fatalf("retry var = %q, want %q", got.Workflow.Variables[worktreeRepairRetryKey("implement")], tc.wantRetry)
			}
			if got.Blocker.Exhausted != tc.wantExh {
				t.Fatalf("blocker.Exhausted = %v, want %v", got.Blocker.Exhausted, tc.wantExh)
			}
			if !tc.wantExh && !got.Blocker.IsZero() {
				t.Fatalf("blocker state should be cleared on successful retry, got %+v", got.Blocker)
			}
		})
	}
}

func TestResumeStalled_WatchdogStopVerifiedFailureDoesNotRetry(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "human-required",
		StatusReason: "watchdog: verify suite still fails after loop stop: go test ./cmd/sybra-cli\nFAIL",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			StartedAt:   time.Now().UTC(),
		},
	})

	engine.ResumeStalled()

	if got := agents.CallCount(); got != 0 {
		t.Fatalf("StartAgent calls = %d, want 0 for verified blocker", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if got.StatusReason == "" || !strings.Contains(got.StatusReason, "verify suite still fails") {
		t.Fatalf("status_reason = %q, want verified-failure reason preserved", got.StatusReason)
	}
}

func TestResumeStalled_WatchdogStopRateLimitedProviderKeepsHumanRequired(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetProviderRateLimited(true)
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "human-required",
		StatusReason: "watchdog: loop stop: looping on toolchain setup",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			StartedAt:   time.Now().UTC(),
		},
	})

	engine.ResumeStalled()

	if got := agents.CallCount(); got != 0 {
		t.Fatalf("StartAgent calls = %d, want 0 while provider is rate-limited", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if got.StatusReason != "watchdog: loop stop: looping on toolchain setup" {
		t.Fatalf("status_reason = %q, want retryable stop marker preserved", got.StatusReason)
	}
	if got.Workflow.Variables[watchdogStopRetryKey("implement")] != "" {
		t.Fatalf("stop retry var = %q, want empty when dispatch is skipped", got.Workflow.Variables[watchdogStopRetryKey("implement")])
	}
}

func TestResumeStalled_WatchdogHangExhaustedRunTestOpensPR(t *testing.T) {
	store := newInlineTestStore(t, "testing-task", `
id: testing-task
steps:
  - id: run_test
    type: run_agent
    config:
      role: test-runner
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	var completed []CompletionInfo
	engine.SetOnComplete(func(info CompletionInfo) { completed = append(completed, info) })
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "testing",
		StatusReason: "watchdog hang: no stream activity",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "testing-task",
			CurrentStep: "run_test",
			State:       ExecWaiting,
			Variables: map[string]string{
				watchdogHangRetryKey("run_test"): strconv.Itoa(maxWatchdogHangRetries),
			},
			StartedAt: time.Now().UTC(),
		},
	})

	engine.ResumeStalled()

	if got := agents.CallCount(); got != 0 {
		t.Fatalf("StartAgent calls = %d, want 0", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "ready-pr" {
		t.Fatalf("status = %q, want ready-pr", got.Status)
	}
	if got.StatusReason != "manual testing gate could not be run after auto-retries (harness/infra limitation, not a product defect) — opening PR for CI and human review" {
		t.Fatalf("status_reason = %q", got.StatusReason)
	}
	if got.Workflow.State != ExecCompleted {
		t.Fatalf("workflow state = %q, want %q", got.Workflow.State, ExecCompleted)
	}
	if got.Workflow.CurrentStep != "" {
		t.Fatalf("workflow current_step = %q, want empty", got.Workflow.CurrentStep)
	}
	if len(completed) != 1 {
		t.Fatalf("workflow completions = %d, want 1", len(completed))
	}
	if completed[0].WorkflowID != "testing-task" {
		t.Fatalf("completion workflow = %q, want testing-task", completed[0].WorkflowID)
	}
}

func TestResumeStalled_WatchdogHangExhaustedRunTestHumanRequiredWhenOpenPRDisabled(t *testing.T) {
	store := newInlineTestStore(t, "testing-task", `
id: testing-task
steps:
  - id: run_test
    type: run_agent
    config:
      role: test-runner
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	engine.SetOpenPROnUnrunnableGate(false)
	var completed []CompletionInfo
	engine.SetOnComplete(func(info CompletionInfo) { completed = append(completed, info) })
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "testing",
		StatusReason: "watchdog hang: no stream activity",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "testing-task",
			CurrentStep: "run_test",
			State:       ExecWaiting,
			Variables: map[string]string{
				watchdogHangRetryKey("run_test"): strconv.Itoa(maxWatchdogHangRetries),
			},
			StartedAt: time.Now().UTC(),
		},
	})

	engine.ResumeStalled()

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if got.Workflow.State != ExecFailed {
		t.Fatalf("workflow state = %q, want %q", got.Workflow.State, ExecFailed)
	}
	if len(completed) != 0 {
		t.Fatalf("workflow completions = %d, want 0", len(completed))
	}
}

func TestResumeStalled_TransientFetchRetriesThenEscalates(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetFailSpawn(worktreeerr.ErrTransientFetch)
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: transientFetchStatusReason,
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   map[string]string{},
			StartedAt:   time.Now().UTC(),
		},
	})

	for attempt := 1; attempt <= maxTransientFetchRetries; attempt++ {
		engine.ResumeStalled()
		got, err := tasks.GetTask("t1")
		if err != nil {
			t.Fatalf("get task after attempt %d: %v", attempt, err)
		}
		if got.Status != "in-progress" {
			t.Fatalf("attempt %d status = %q, want in-progress", attempt, got.Status)
		}
		if got.Workflow.Variables[transientFetchRetryKey("implement")] != strconv.Itoa(attempt) {
			t.Fatalf("attempt %d retry var = %q, want %q", attempt, got.Workflow.Variables[transientFetchRetryKey("implement")], strconv.Itoa(attempt))
		}
		if got.StatusReason != transientFetchStatusReason {
			t.Fatalf("attempt %d status_reason = %q, want %q", attempt, got.StatusReason, transientFetchStatusReason)
		}
	}

	engine.ResumeStalled()

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required after retry exhaustion", got.Status)
	}
	wantReason := "agent start blocked: transient network retry budget exhausted after 2 attempts reconciling worktree with remote"
	if got.StatusReason != wantReason {
		t.Fatalf("status_reason = %q, want %q", got.StatusReason, wantReason)
	}
	if got.Workflow.Variables[transientFetchRetryKey("implement")] != strconv.Itoa(maxTransientFetchRetries) {
		t.Fatalf("retry var = %q, want %d", got.Workflow.Variables[transientFetchRetryKey("implement")], maxTransientFetchRetries)
	}
}

func TestResumeStalled_WatchdogHangDoesNotBurnBudgetWhileAgentRunning(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "watchdog hang: no stream activity",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   map[string]string{},
			StartedAt:   time.Now().UTC(),
		},
	})
	if _, _, _, err := agents.StartAgent("t1", "implementation", "headless", "sonnet", "", "p", "", nil, false, false, "", "", AgentAssignment{}); err != nil {
		t.Fatal(err)
	}

	engine.ResumeStalled()

	if got := agents.CallCount(); got != 1 {
		t.Fatalf("StartAgent calls = %d, want only the pre-existing running agent", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Workflow.Variables[watchdogHangRetryKey("implement")] != "" {
		t.Fatalf("hang retry var = %q, want empty while agent is still running", got.Workflow.Variables[watchdogHangRetryKey("implement")])
	}
	if got.StatusReason != "watchdog hang: no stream activity" {
		t.Fatalf("status_reason = %q, want marker preserved until no agent is running", got.StatusReason)
	}
}

func TestResumeStalled_PrioritizesReviewOverNewWork(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	parked := func(id string, status taskstatus.Status) TaskInfo {
		return TaskInfo{
			ID:        id,
			Status:    status,
			AgentMode: "headless",
			Workflow: &Execution{
				WorkflowID:  "test-simple",
				CurrentStep: "implement",
				State:       ExecWaiting,
				Variables:   make(map[string]string),
			},
		}
	}
	tasks.Put(parked("t-new", "todo"))
	tasks.Put(parked("t-mid", "in-progress"))
	tasks.Put(parked("t-review", "in-review"))

	engine.ResumeStalled()

	var order []string
	for _, c := range agents.calls {
		order = append(order, c.TaskID)
	}
	want := []string{"t-review", "t-mid", "t-new"}
	if !slices.Equal(order, want) {
		t.Fatalf("dispatch order = %v, want %v", order, want)
	}
}

// TestResumeStalled_DispatchComparatorOverridesDefaultOrder pins that an
// injected SetDispatchComparator replaces the built-in
// dispatchorder.Rank(status)-only sort entirely, so app wiring (agentqueue.Less)
// controls ordering without internal/workflow importing internal/agentqueue.
func TestResumeStalled_DispatchComparatorOverridesDefaultOrder(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	parked := func(id string, status taskstatus.Status) TaskInfo {
		return TaskInfo{
			ID:        id,
			Status:    status,
			AgentMode: "headless",
			Workflow: &Execution{
				WorkflowID:  "test-simple",
				CurrentStep: "implement",
				State:       ExecWaiting,
				Variables:   make(map[string]string),
			},
		}
	}
	tasks.Put(parked("t-new", "todo"))
	tasks.Put(parked("t-review", "in-review"))

	// Reverse alphabetical order by ID — deliberately nothing like the
	// built-in status-rank sort, so a passing test proves the comparator
	// actually drove the ordering rather than coincidentally matching it.
	engine.SetDispatchComparator(func() func(a, b TaskInfo) int {
		return func(a, b TaskInfo) int {
			return cmp.Compare(b.ID, a.ID)
		}
	})

	engine.ResumeStalled()

	var order []string
	for _, c := range agents.calls {
		order = append(order, c.TaskID)
	}
	want := []string{"t-review", "t-new"}
	if !slices.Equal(order, want) {
		t.Fatalf("dispatch order = %v, want %v (injected comparator must win over the default status-rank sort)", order, want)
	}
}

// TestResumeStalled_QueueReconcilerInvokedBeforeDispatch pins that a wired
// SetQueueReconciler runs once per ResumeStalled tick, before the dispatch
// scan (see the doc comment on Engine.queueReconciler).
func TestResumeStalled_QueueReconcilerInvokedBeforeDispatch(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "todo",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   make(map[string]string),
		},
	})

	var calls int
	var calledBeforeDispatch bool
	engine.SetQueueReconciler(func() {
		calls++
		calledBeforeDispatch = agents.CallCount() == 0
	})

	engine.ResumeStalled()

	if calls != 1 {
		t.Fatalf("queue reconciler called %d times, want 1", calls)
	}
	if !calledBeforeDispatch {
		t.Fatal("queue reconciler must run before the dispatch scan, not after")
	}

	engine.ResumeStalled()
	if calls != 2 {
		t.Fatalf("queue reconciler called %d times across two ticks, want 2", calls)
	}
}

func TestResumeStalled_ReconcilesWaitHumanStatus(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(Definition{
		ID:   "wait-human-wf",
		Name: "wait human wf",
		Steps: []Step{
			{
				ID:     "review_plan",
				Name:   "Review Plan",
				Type:   StepWaitHuman,
				Config: StepConfig{Status: "plan-review", HumanActions: []string{"approve", "reject"}},
				Next:   []Transition{{GoTo: ""}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "todo",
		Workflow: &Execution{
			WorkflowID:  "wait-human-wf",
			CurrentStep: "review_plan",
			State:       ExecWaiting,
			Variables:   make(map[string]string),
		},
	})

	engine.ResumeStalled()

	ti, _ := tasks.GetTask("t1")
	if ti.Status != "plan-review" {
		t.Fatalf("status = %q, want plan-review (wait_human status must be reconciled)", ti.Status)
	}
	if agents.CallCount() != 0 {
		t.Fatalf("wait_human step must not spawn an agent, got %d", agents.CallCount())
	}
}

func TestResumeStalled_WaitHumanStatusRespectsSkipStatuses(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(Definition{
		ID:   "wait-human-wf",
		Name: "wait human wf",
		Steps: []Step{
			{
				ID:     "review_plan",
				Name:   "Review Plan",
				Type:   StepWaitHuman,
				Config: StepConfig{Status: "plan-review", HumanActions: []string{"approve", "reject"}},
				Next:   []Transition{{GoTo: ""}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	for _, keep := range []taskstatus.Status{"cancelled", "done", "human-required"} {
		t.Run(string(keep), func(t *testing.T) {
			tasks := newMemTasks()
			engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())
			tasks.Put(TaskInfo{
				ID:     "t1",
				Status: keep,
				Workflow: &Execution{
					WorkflowID:  "wait-human-wf",
					CurrentStep: "review_plan",
					State:       ExecWaiting,
					Variables:   make(map[string]string),
				},
			})

			engine.ResumeStalled()

			ti, _ := tasks.GetTask("t1")
			if ti.Status != keep {
				t.Fatalf("status = %q, want %q unchanged (skip status must not be reconciled)", ti.Status, keep)
			}
		})
	}
}

func TestResumeStalled_ReroutesStaleConditionBranch(t *testing.T) {
	store := newInlineTestStore(t, "condition-reroute", `
id: condition-reroute
steps:
  - id: maybe_critique
    type: condition
    next:
      - when:
          field: task.tags
          operator: not_contains
          value: nocritic
        goto: critique_plan
      - goto: review_plan
  - id: critique_plan
    type: run_agent
    config:
      role: plan-critic
      mode: headless
      prompt: critique
    next:
      - goto: review_plan
  - id: review_plan
    type: wait_human
    config:
      status: plan-review
      human_actions: [approve, reject]
    next:
      - goto: ""
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	now := time.Now().UTC()
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "planning",
		Tags:   []string{"nocritic"},
		Workflow: &Execution{
			WorkflowID:  "condition-reroute",
			CurrentStep: "critique_plan",
			State:       ExecWaiting,
			Variables:   make(map[string]string),
			StepHistory: []StepRecord{{
				StepID:    "maybe_critique",
				Status:    "completed",
				StartedAt: now.Add(-time.Minute),
				EndedAt:   now.Add(-time.Minute),
			}},
			EffectLog: []EffectRecord{
				{
					ID:          EffectID{Generation: 1, StepSeq: 0, StepID: "maybe_critique", Pos: effectPosStepAction},
					IntentAt:    now.Add(-time.Minute),
					CompletedAt: &now,
				},
				{
					ID:       EffectID{Generation: 1, StepSeq: 1, StepID: "critique_plan", Pos: effectPosStepAction},
					IntentAt: now.Add(-time.Minute),
				},
			},
		},
	})
	def, err := store.Get("condition-reroute")
	if err != nil {
		t.Fatal(err)
	}
	step := def.StepByID("critique_plan")
	if step == nil {
		t.Fatal("critique_plan step missing")
	}
	pre, _ := tasks.GetTask("t1")
	condition := latestConditionPredecessor(&def, pre.Workflow, step.ID)
	if condition == nil {
		t.Fatal("precondition: condition predecessor not detected")
	}
	nextID, err := ResolveTransition(condition.Next, engine.transitionFields(pre, pre.Workflow))
	if err != nil {
		t.Fatal(err)
	}
	if nextID != "review_plan" {
		t.Fatalf("precondition: condition routes to %q, want review_plan", nextID)
	}

	engine.ResumeStalled()

	ti, _ := tasks.GetTask("t1")
	if ti.Workflow == nil {
		t.Fatal("workflow missing")
	}
	if ti.Workflow.CurrentStep != "review_plan" {
		t.Fatalf("CurrentStep = %q, want review_plan", ti.Workflow.CurrentStep)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Fatalf("State = %q, want %q", ti.Workflow.State, ExecWaiting)
	}
	if ti.Status != "plan-review" {
		t.Fatalf("Status = %q, want plan-review", ti.Status)
	}
	if agents.CallCount() != 0 {
		t.Fatalf("critique agent dispatched %d times, want 0", agents.CallCount())
	}
	for _, rec := range ti.Workflow.EffectLog {
		if rec.ID.StepID == "critique_plan" {
			t.Fatalf("stale critique_plan effect was not cleared: %+v", ti.Workflow.EffectLog)
		}
	}
}

func TestResumeStalled_DoesNotRerouteConditionWhileDispatchClaimed(t *testing.T) {
	store := newInlineTestStore(t, "condition-reroute-claimed", `
id: condition-reroute-claimed
steps:
  - id: maybe_critique
    type: condition
    next:
      - when:
          field: task.tags
          operator: not_contains
          value: nocritic
        goto: critique_plan
      - goto: review_plan
  - id: critique_plan
    type: run_agent
    config:
      role: plan-critic
      mode: headless
      prompt: critique
    next:
      - goto: review_plan
  - id: review_plan
    type: wait_human
    config:
      status: plan-review
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	now := time.Now().UTC()
	tasks.Put(TaskInfo{
		ID: "t1", Status: "planning", Tags: []string{"nocritic"},
		Workflow: &Execution{
			WorkflowID: "condition-reroute-claimed", CurrentStep: "critique_plan", State: ExecWaiting,
			StepHistory: []StepRecord{{StepID: "maybe_critique", Status: "completed", StartedAt: now, EndedAt: now}},
			AgentRoutes: map[string]string{"agent-1": "critique_plan"},
		},
	})

	// This models recovery's lost-callback bridge: the manager no longer sees
	// the agent, but HandleAgentComplete has already claimed the task while it
	// advances the persisted route.
	engine.mu.Lock()
	engine.dispatching["t1"] = struct{}{}
	engine.mu.Unlock()
	defer engine.clearResumeDispatching("t1")

	engine.ResumeStalled()

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.Workflow.CurrentStep != "critique_plan" {
		t.Fatalf("CurrentStep = %q, want critique_plan while completion owns dispatch", ti.Workflow.CurrentStep)
	}
	if _, ok := ti.Workflow.AgentRoute("agent-1"); !ok {
		t.Fatal("condition reroute cleared the completing agent route")
	}
	if agents.CallCount() != 0 {
		t.Fatalf("agent starts = %d, want 0", agents.CallCount())
	}
}

func TestResumeStalled_ConditionRerouteDoesNotOverwriteFreshCompletion(t *testing.T) {
	store := newInlineTestStore(t, "condition-reroute-fresh", `
id: condition-reroute-fresh
steps:
  - id: maybe_critique
    type: condition
    next:
      - when:
          field: task.tags
          operator: not_contains
          value: nocritic
        goto: critique_plan
      - goto: review_plan
  - id: critique_plan
    type: run_agent
    config:
      role: plan-critic
      mode: headless
      prompt: critique
    next:
      - goto: review_plan
  - id: review_plan
    type: wait_human
    config:
      status: plan-review
`)
	tasks := newMemTasks()
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())
	now := time.Now().UTC()
	stale := TaskInfo{
		ID: "t1", Status: "planning", Tags: []string{"nocritic"},
		Workflow: &Execution{
			WorkflowID: "condition-reroute-fresh", CurrentStep: "critique_plan", State: ExecWaiting,
			StepHistory: []StepRecord{{StepID: "maybe_critique", Status: "completed", StartedAt: now, EndedAt: now}},
			AgentRoutes: map[string]string{"agent-1": "critique_plan"},
		},
	}
	tasks.Put(stale)

	// A recovered completion advances after ResumeStalled read stale but before
	// its reroute claim. The reroute must re-read rather than write stale back.
	completed := stale
	completed.Workflow = stale.Workflow.Clone()
	if completed.Workflow == nil {
		t.Fatal("stale workflow clone is nil")
	}
	completed.Workflow.CurrentStep = "review_plan"
	completed.Workflow.ClearAgentRoute("agent-1")
	tasks.Put(completed)

	def, err := store.Get("condition-reroute-fresh")
	if err != nil {
		t.Fatal(err)
	}
	step := def.StepByID("critique_plan")
	if step == nil {
		t.Fatal("critique_plan step missing")
	}
	if !engine.resumeStalledRerouteStaleConditionBranch(&stale, &def, step) {
		t.Fatal("stale reroute was not consumed")
	}

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow.CurrentStep != "review_plan" {
		t.Fatalf("CurrentStep = %q, want completion's review_plan", got.Workflow.CurrentStep)
	}
}

func TestResumeStalled_RunAgent(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	// Simulate a task stuck at "implement" step with no running agent.
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecRunning,
			Variables:   make(map[string]string),
		},
	})

	engine.ResumeStalled()

	// Should have started an agent for the implement step.
	if agents.CallCount() != 1 {
		t.Fatalf("expected 1 agent start, got %d", agents.CallCount())
	}
	if agents.LastCall().Role != "implementation" {
		t.Fatalf("expected implementation, got %q", agents.LastCall().Role)
	}
}

func TestResumeStalled_RunAgentAfterCompletedDispatchEffect(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	completedAt := time.Now().UTC()
	tasks.Put(TaskInfo{
		ID:         "t1",
		Generation: 1,
		Status:     "in-progress",
		AgentMode:  "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecRunning,
			Variables:   make(map[string]string),
			EffectLog: []EffectRecord{{
				ID:          EffectID{Generation: 1, StepSeq: 0, StepID: "implement", Pos: effectPosStepAction},
				IntentAt:    completedAt.Add(-time.Second),
				Owner:       engine.ownerID,
				CompletedAt: &completedAt,
			}},
		},
	})

	engine.ResumeStalled()

	if agents.CallCount() != 1 {
		t.Fatalf("expected 1 agent start, got %d", agents.CallCount())
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if len(got.Workflow.EffectLog) != 2 {
		t.Fatalf("effect log len = %d, want 2: %+v", len(got.Workflow.EffectLog), got.Workflow.EffectLog)
	}
	if got.Workflow.EffectLog[1].ID.StepSeq <= got.Workflow.EffectLog[0].ID.StepSeq {
		t.Fatalf("new effect StepSeq = %d, want greater than prior completed StepSeq %d",
			got.Workflow.EffectLog[1].ID.StepSeq, got.Workflow.EffectLog[0].ID.StepSeq)
	}
	if got.Workflow.EffectLog[1].CompletedAt == nil {
		t.Fatalf("new dispatch effect was not completed: %+v", got.Workflow.EffectLog[1])
	}
}

func TestResumeStalled_DispatchesRateLimitedProviderForFailover(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetProviderRateLimited(true)
	agents.SetProviderFailover(true)
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecRunning,
			Variables:   make(map[string]string),
		},
	})

	engine.ResumeStalled()

	if agents.CallCount() != 1 {
		t.Fatalf("expected workflow to dispatch so agent manager can fail over, got %d starts", agents.CallCount())
	}
}

func TestRescheduleRateLimitedAgent_RerunsCurrentStep(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   make(map[string]string),
		},
	})
	setWorkflowAgentRoute(t, tasks, "t1", "limited-agent", "implement")

	engine.RescheduleRateLimitedAgent("t1", "limited-agent")

	if agents.CallCount() != 1 {
		t.Fatalf("expected replacement agent start, got %d", agents.CallCount())
	}
	if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "limited-agent"); tracked {
		t.Fatal("rate-limited agent step mapping was not cleared")
	}
}

func TestRescheduleInterruptedAgent_UnparksHumanRequiredCurrentStep(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "human-required",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   make(map[string]string),
		},
	})
	if err := tasks.UpdateTaskStatus("t1", "human-required", "provider run aborted after tool use was rejected"); err != nil {
		t.Fatalf("UpdateTaskStatus: %v", err)
	}
	setWorkflowAgentRoute(t, tasks, "t1", "interrupted-agent", "implement")

	engine.RescheduleInterruptedAgent("t1", "interrupted-agent")

	if got := agents.CallCount(); got != 1 {
		t.Fatalf("expected replacement agent start, got %d", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "in-progress" || got.StatusReason != "" {
		t.Fatalf("status/reason = %q/%q, want in-progress with empty reason", got.Status, got.StatusReason)
	}
	if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "interrupted-agent"); tracked {
		t.Fatal("interrupted agent step mapping was not cleared")
	}
}

func TestRescheduleInterruptedAgent_SkipsBlockedAndTerminalStatuses(t *testing.T) {
	for _, status := range []taskstatus.Status{"blocked", "done", "cancelled"} {
		t.Run(string(status), func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewTestEngine(store, tasks, agents, discardLogger())

			tasks.Put(TaskInfo{
				ID:        "t1",
				Status:    status,
				AgentMode: "headless",
				Workflow: &Execution{
					WorkflowID:  "test-simple",
					CurrentStep: "implement",
					State:       ExecWaiting,
					Variables:   make(map[string]string),
				},
			})
			setWorkflowAgentRoute(t, tasks, "t1", "interrupted-agent", "implement")

			engine.RescheduleInterruptedAgent("t1", "interrupted-agent")

			if got := agents.CallCount(); got != 0 {
				t.Fatalf("replacement agent starts = %d, want 0", got)
			}
			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != status {
				t.Fatalf("status = %q, want %q", got.Status, status)
			}
			if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "interrupted-agent"); tracked {
				t.Fatal("interrupted agent step mapping was not cleared")
			}
		})
	}
}

func TestInterruptedRecoveryStatus_PlanningWorkflow(t *testing.T) {
	t.Parallel()

	if got := interruptedRecoveryStatus("simple-task-plan"); got != "planning" {
		t.Fatalf("interruptedRecoveryStatus(simple-task-plan) = %q, want planning", got)
	}
	if got := interruptedRecoveryStatus("testing-task"); got != "testing" {
		t.Fatalf("interruptedRecoveryStatus(testing-task) = %q, want testing", got)
	}
	if got := interruptedRecoveryStatus("simple-task-implement"); got != "in-progress" {
		t.Fatalf("interruptedRecoveryStatus(simple-task-implement) = %q, want in-progress", got)
	}
}

func TestRescheduleRateLimitedAgent_WatchdogRetriesThenEscalates(t *testing.T) {
	tests := []struct {
		name       string
		retries    string
		wantStarts int
		wantStatus taskstatus.Status
		wantReason string
		wantRetry  string
	}{
		{
			name:       "first watchdog rate limit reruns",
			wantStarts: 1,
			wantStatus: "in-progress",
			wantReason: "",
			wantRetry:  "1",
		},
		{
			name:       "budget exhausted escalates",
			retries:    "2",
			wantStarts: 0,
			wantStatus: "human-required",
			wantReason: "watchdog: rate limit retry budget exhausted after 2 clean re-dispatches",
			wantRetry:  "2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewTestEngine(store, tasks, agents, discardLogger())

			vars := map[string]string{}
			if tc.retries != "" {
				vars[watchdogRateLimitRetryKey("implement")] = tc.retries
			}
			tasks.Put(TaskInfo{
				ID:           "t1",
				Status:       "in-progress",
				StatusReason: "watchdog: rate limit: org-level quota exhausted",
				AgentMode:    "headless",
				Workflow: &Execution{
					WorkflowID:  "test-simple",
					CurrentStep: "implement",
					State:       ExecWaiting,
					Variables:   vars,
				},
			})
			setWorkflowAgentRoute(t, tasks, "t1", "limited-agent", "implement")

			engine.RescheduleRateLimitedAgent("t1", "limited-agent")

			if got := agents.CallCount(); got != tc.wantStarts {
				t.Fatalf("expected replacement agent starts = %d, got %d", tc.wantStarts, got)
			}
			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.StatusReason != tc.wantReason {
				t.Fatalf("status_reason = %q, want %q", got.StatusReason, tc.wantReason)
			}
			if got.Workflow.Variables[watchdogRateLimitRetryKey("implement")] != tc.wantRetry {
				t.Fatalf("rate-limit retry var = %q, want %q", got.Workflow.Variables[watchdogRateLimitRetryKey("implement")], tc.wantRetry)
			}
			if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "limited-agent"); tracked {
				t.Fatal("rate-limited agent step mapping was not cleared")
			}
		})
	}
}

func TestResumeStalled_WatchdogZeroOutputUsesSharedRetryBudget(t *testing.T) {
	oldStartedAt := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name             string
		retries          string
		freshUsed        bool
		since            time.Time
		wantStarts       int
		wantStatus       taskstatus.Status
		wantReason       string
		wantRetry        string
		wantFresh        string
		wantSessionFence bool
	}{
		{
			name:       "resume consumes persisted retry and reruns",
			retries:    "1",
			wantStarts: 1,
			wantStatus: "in-progress",
			wantReason: "",
			wantRetry:  "2",
		},
		{
			// Exhausting the *resume* budget is not terminal: the poisoned
			// session is fenced with a reset budget so the next tick dispatches a
			// clean session, instead of latching a permanent blocked deadlock
			// (2026-07-23). This tick is consumed (no dispatch yet).
			name:             "resume budget exhausted grants one fresh-session round",
			retries:          strconv.Itoa(maxWatchdogRateLimitRetries),
			wantStarts:       0,
			wantStatus:       "in-progress",
			wantReason:       watchdogreason.RateLimit(watchdogreason.ZeroOutputBeforeStartup),
			wantRetry:        "0",
			wantFresh:        "1",
			wantSessionFence: true,
		},
		{
			// A second exhaustion still inside maxSilentHangWait of the first
			// one is another provider-capacity blip, not a latch: it keeps
			// granting fresh rounds instead of blocking (778422ef, 2026-08-06 —
			// a task blocked permanently after exactly two rounds while claude
			// flapped healthy/unhealthy for six hours with no peer to fail
			// over to).
			name:             "fresh round exhausts again within the wait ceiling, grants another",
			retries:          strconv.Itoa(maxWatchdogRateLimitRetries),
			freshUsed:        true,
			since:            time.Now().Add(-1 * time.Hour),
			wantStarts:       0,
			wantStatus:       taskstatus.InProgress,
			wantReason:       watchdogreason.RateLimit(watchdogreason.ZeroOutputBeforeStartup),
			wantRetry:        "0",
			wantFresh:        "1",
			wantSessionFence: true,
		},
		{
			name:             "wait ceiling exceeded, blocks and fences off the poisoned session",
			retries:          strconv.Itoa(maxWatchdogRateLimitRetries),
			freshUsed:        true,
			since:            time.Now().Add(-(maxSilentHangWait + time.Hour)),
			wantStarts:       0,
			wantStatus:       "blocked",
			wantReason:       "watchdog: zero-output startup retry budget exhausted after 3 identical attempts",
			wantRetry:        strconv.Itoa(maxWatchdogRateLimitRetries),
			wantFresh:        "1",
			wantSessionFence: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewTestEngine(store, tasks, agents, discardLogger())

			vars := map[string]string{}
			if tc.retries != "" {
				vars[watchdogRateLimitRetryKey("implement")] = tc.retries
			}
			if tc.freshUsed {
				vars[watchdogZeroOutputFreshRetryKey("implement")] = "1"
			}
			if !tc.since.IsZero() {
				vars[watchdogSilentHangSinceKey("implement")] = tc.since.UTC().Format(time.RFC3339)
			}
			tasks.Put(TaskInfo{
				ID:           "t1",
				Status:       "in-progress",
				StatusReason: watchdogreason.RateLimit(watchdogreason.ZeroOutputBeforeStartup),
				AgentMode:    "headless",
				Workflow: &Execution{
					WorkflowID:  "test-simple",
					CurrentStep: "implement",
					State:       ExecWaiting,
					Variables:   vars,
					StartedAt:   oldStartedAt,
				},
			})

			engine.ResumeStalled()

			if got := agents.CallCount(); got != tc.wantStarts {
				t.Fatalf("replacement agent starts = %d, want %d", got, tc.wantStarts)
			}
			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.StatusReason != tc.wantReason {
				t.Fatalf("status_reason = %q, want %q", got.StatusReason, tc.wantReason)
			}
			if got.Workflow.Variables[watchdogRateLimitRetryKey("implement")] != tc.wantRetry {
				t.Fatalf("rate-limit retry var = %q, want %q", got.Workflow.Variables[watchdogRateLimitRetryKey("implement")], tc.wantRetry)
			}
			if got.Workflow.Variables[watchdogZeroOutputFreshRetryKey("implement")] != tc.wantFresh {
				t.Fatalf("fresh-retry var = %q, want %q", got.Workflow.Variables[watchdogZeroOutputFreshRetryKey("implement")], tc.wantFresh)
			}
			// sybra#2542: exhausting the zero-output-stall budget must bump
			// Workflow.StartedAt past every prior agent run, so
			// PickImplementationResumeSession's StartedAt fence rejects the
			// poisoned session and the next dispatch starts fresh instead of
			// resuming the same session that just hung.
			if tc.wantSessionFence && !got.Workflow.StartedAt.After(oldStartedAt) {
				t.Fatalf("workflow.started_at = %v, want it bumped past %v to fence off the poisoned resume session", got.Workflow.StartedAt, oldStartedAt)
			}
			if !tc.wantSessionFence && !got.Workflow.StartedAt.Equal(oldStartedAt) {
				t.Fatalf("workflow.started_at = %v, want unchanged %v", got.Workflow.StartedAt, oldStartedAt)
			}
		})
	}
}

// TestResumeStalled_SilentHangReasonRecovers pins the reason rename (#3154):
// the watchdog now parks a zero-output run under the silent-hang reason instead
// of a borrowed rate-limit one, and the shared retry policy has to keep picking
// it up. Without this the fix would trade a parked provider for a parked task
// that nothing re-dispatches. The sibling tests above still drive the legacy
// wrapped reason, which is what tasks parked before the upgrade carry on disk.
func TestResumeStalled_SilentHangReasonRecovers(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: watchdogreason.SilentHang(watchdogreason.ZeroOutputBeforeStartup),
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   map[string]string{watchdogRateLimitRetryKey("implement"): "1"},
		},
	})

	engine.ResumeStalled()

	if got := agents.CallCount(); got != 1 {
		t.Fatalf("replacement agent starts = %d, want 1", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "in-progress" || got.StatusReason != "" {
		t.Fatalf("status/reason = %q/%q, want in-progress with the reason cleared", got.Status, got.StatusReason)
	}
	if got.Workflow.Variables[watchdogRateLimitRetryKey("implement")] != "2" {
		t.Fatalf("retry var = %q, want 2", got.Workflow.Variables[watchdogRateLimitRetryKey("implement")])
	}
}

// TestRescheduleRateLimitedAgent_SilentHangRoutesAroundTheProvider pins the
// failover the health-gate park used to buy for the hung run itself. Parking
// the provider is what pushed the retry onto a peer; now that a silent child
// leaves provider health alone (so other tasks keep dispatching), this run has
// to route around the provider that produced nothing, or it just hands the
// same wedged CLI another 15 minutes.
func TestRescheduleRateLimitedAgent_SilentHangRoutesAroundTheProvider(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.defaultProvider = "claude"
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: watchdogreason.SilentHang(watchdogreason.ZeroOutputBeforeStartup),
		AgentMode:    "headless",
		AgentRuns: []AgentRunInfo{
			{AgentID: "hung-agent", Role: "implementation", Provider: "claude"},
		},
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})
	setWorkflowAgentRoute(t, tasks, "t1", "hung-agent", "implement")

	engine.RescheduleRateLimitedAgent("t1", "hung-agent")

	if got := agents.CallCount(); got != 1 {
		t.Fatalf("replacement agent starts = %d, want 1", got)
	}
	if p := agents.LastCall().Provider; p == "claude" || p == "" {
		t.Fatalf("re-dispatched provider = %q, want a peer of the provider that went silent", p)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if v := got.Workflow.Variables[watchdogSilentHangAvoidKey("implement")]; v != "" {
		t.Fatalf("avoid var = %q, want it consumed by the dispatch it steered", v)
	}
}

// TestRescheduleRateLimitedAgent_RealRateLimitKeepsItsProvider guards the other
// side: a genuine rate limit is already handled by the health gate's failover,
// so it must not pick up the silent-hang reroute and start crossing providers
// on its own.
func TestRescheduleRateLimitedAgent_RealRateLimitKeepsItsProvider(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.defaultProvider = "claude"
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: watchdogreason.RateLimit("org-level quota exhausted"),
		AgentMode:    "headless",
		AgentRuns: []AgentRunInfo{
			{AgentID: "limited-agent", Role: "implementation", Provider: "claude"},
		},
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})
	setWorkflowAgentRoute(t, tasks, "t1", "limited-agent", "implement")

	engine.RescheduleRateLimitedAgent("t1", "limited-agent")

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if v := got.Workflow.Variables[watchdogSilentHangAvoidKey("implement")]; v != "" {
		t.Fatalf("avoid var = %q, want empty for a real rate limit", v)
	}
}

func TestIsWatchdogRateLimitReason_CoversBothParkedForms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		reason string
		want   bool
	}{
		{"silent hang", watchdogreason.SilentHang(watchdogreason.ZeroOutputBeforeStartup), true},
		{"legacy wrapped zero output", watchdogreason.RateLimit(watchdogreason.ZeroOutputBeforeStartup), true},
		{"real rate limit", watchdogreason.RateLimit("org-level quota exhausted"), true},
		{"hang", watchdogreason.Hang("no stream activity"), false},
		{"empty", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWatchdogRateLimitReason(tc.reason); got != tc.want {
				t.Fatalf("isWatchdogRateLimitReason(%q) = %v, want %v", tc.reason, got, tc.want)
			}
		})
	}
}

// TestResumeStalled_WatchdogZeroOutputFreshRoundDispatches proves the deadlock
// break end-to-end: a poisoned resume that has exhausted its resume budget is
// granted a fresh round (tick 1 consumed, fenced) and then actually
// re-dispatches a clean session on the next tick — rather than latching a
// permanent blocked state as it did during the 2026-07-23 board freeze.
func TestResumeStalled_WatchdogZeroOutputFreshRoundDispatches(t *testing.T) {
	oldStartedAt := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: watchdogreason.RateLimit(watchdogreason.ZeroOutputBeforeStartup),
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   map[string]string{watchdogRateLimitRetryKey("implement"): strconv.Itoa(maxWatchdogRateLimitRetries)},
			StartedAt:   oldStartedAt,
		},
	})

	// Tick 1: resume budget already exhausted -> fence + reset, no dispatch yet.
	engine.ResumeStalled()
	if got := agents.CallCount(); got != 0 {
		t.Fatalf("tick 1 dispatched %d agents, want 0 (fence-and-wait)", got)
	}
	afterFence, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if afterFence.Status != "in-progress" {
		t.Fatalf("tick 1 status = %q, want in-progress (not blocked)", afterFence.Status)
	}
	if !afterFence.Workflow.StartedAt.After(oldStartedAt) {
		t.Fatalf("tick 1 did not fence the poisoned session (started_at %v)", afterFence.Workflow.StartedAt)
	}

	// Tick 2: fenced clean session actually dispatches.
	engine.ResumeStalled()
	if got := agents.CallCount(); got != 1 {
		t.Fatalf("tick 2 dispatched %d agents total, want 1 (fresh session ran)", got)
	}
	final, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if final.Status == "blocked" {
		t.Fatalf("task latched blocked despite a fresh-session recovery being available")
	}
}

func TestResumeStalled_WatchdogRateLimitPoolBusyDoesNotBurnRetryBudget(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetFailSpawn(ErrAgentPoolBusy)
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "watchdog: rate limit: org-level quota exhausted",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})

	// The first retry is armed then hits a benign capacity park. A later tick
	// must retry dispatch normally instead of re-arming the rate-limit policy.
	for range maxWatchdogRateLimitRetries + 1 {
		engine.ResumeStalled()
	}

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "in-progress" {
		t.Fatalf("status = %q, want in-progress", got.Status)
	}
	if got.StatusReason != "" {
		t.Fatalf("status_reason = %q, want cleared after arming retry", got.StatusReason)
	}
	if retry := got.Workflow.Variables[watchdogRateLimitRetryKey("implement")]; retry != "1" {
		t.Fatalf("rate-limit retry var = %q, want 1 after repeated pool-busy parks", retry)
	}
	if got.Workflow.State == ExecFailed {
		t.Fatal("pool-busy parks must not exhaust the watchdog retry budget")
	}
}

func TestResumeStalled_WatchdogHangPoolBusyRetainsReaskNote(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetFailSpawn(ErrAgentPoolBusy)
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{
		ID: "t1", Status: "in-progress", StatusReason: "watchdog hang: no stream activity", AgentMode: "headless",
		Workflow: &Execution{WorkflowID: "test-simple", CurrentStep: "implement", State: ExecWaiting, Variables: map[string]string{}, StartedAt: time.Now().UTC()},
	})

	engine.ResumeStalled()

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if note := got.Workflow.Variables[watchdogReaskNoteVar]; note == "" {
		t.Fatal("pool-busy park cleared watchdog guidance before an agent started")
	}
	if retry := got.Workflow.Variables[watchdogHangRetryKey("implement")]; retry != "1" {
		t.Fatalf("hang retry = %q, want 1", retry)
	}
}

func TestResumeStalled_WatchdogRateLimitDoesNotClearConcurrentFailure(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "watchdog: rate limit: org-level quota exhausted",
		AgentMode:    "headless",
		Workflow:     &Execution{WorkflowID: "test-simple", CurrentStep: "implement", State: ExecWaiting, Variables: map[string]string{}},
	})
	var once sync.Once
	tasks.onSetWorkflow = func(id string) {
		once.Do(func() {
			if err := tasks.UpdateTaskStatus(id, "human-required", "concurrent permanent failure"); err != nil {
				t.Errorf("set concurrent failure: %v", err)
			}
		})
	}

	engine.ResumeStalled()

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "human-required" || got.StatusReason != "concurrent permanent failure" {
		t.Fatalf("concurrent failure was overwritten: status=%q reason=%q", got.Status, got.StatusReason)
	}
	if starts := agents.CallCount(); starts != 0 {
		t.Fatalf("concurrent failure must fence dispatch, started %d agents", starts)
	}
}

// TestRescheduleRateLimitedAgent_ParksWhileProviderRateLimitedNoFailover is the
// regression guard for sybra#1585: a reschedule that fires while the step's
// provider is still inside its rate-limit cooldown (and no healthy peer exists
// to fail over to) must PARK the task — not consume a watchdog retry budget and
// not escalate to human-required. Even with the retry budget already exhausted,
// the task waits for the cooldown to expire instead of bothering a human.
func TestRescheduleRateLimitedAgent_ParksWhileProviderRateLimitedNoFailover(t *testing.T) {
	tests := []struct {
		name    string
		retries string
	}{
		{name: "fresh budget parks", retries: ""},
		{name: "exhausted budget still parks", retries: strconv.Itoa(maxWatchdogRateLimitRetries)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			// Provider throttled, no peer to fail over to → must park and wait.
			agents.SetProviderRateLimited(true)
			engine := NewTestEngine(store, tasks, agents, discardLogger())

			vars := map[string]string{}
			if tc.retries != "" {
				vars[watchdogRateLimitRetryKey("implement")] = tc.retries
			}
			tasks.Put(TaskInfo{
				ID:           "t1",
				Status:       "in-progress",
				StatusReason: "watchdog: rate limit: org-level quota exhausted",
				AgentMode:    "headless",
				Workflow: &Execution{
					WorkflowID:  "test-simple",
					CurrentStep: "implement",
					State:       ExecWaiting,
					Variables:   vars,
				},
			})
			setWorkflowAgentRoute(t, tasks, "t1", "limited-agent", "implement")

			engine.RescheduleRateLimitedAgent("t1", "limited-agent")

			if got := agents.CallCount(); got != 0 {
				t.Fatalf("expected no replacement agent starts while parked, got %d", got)
			}
			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != "in-progress" {
				t.Fatalf("status = %q, want in-progress (parked, not escalated)", got.Status)
			}
			// Budget untouched: the parked attempt never counted.
			if got := got.Workflow.Variables[watchdogRateLimitRetryKey("implement")]; got != tc.retries {
				t.Fatalf("rate-limit retry var = %q, want unchanged %q", got, tc.retries)
			}
			// Breaker untouched: a transient throttle is not a dispatch failure.
			if _, tripped := got.Workflow.Variables[circuitBreakerFailureKey("implement")]; tripped {
				t.Fatalf("circuit breaker recorded a failure for a transient rate limit: %v", got.Workflow.Variables)
			}
			if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "limited-agent"); tracked {
				t.Fatal("rate-limited agent step mapping was not cleared")
			}
		})
	}
}

func TestRescheduleRateLimitedAgent_EmptyTaskIDClearsTrackedAgent(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})
	setWorkflowAgentRoute(t, tasks, "t1", "limited-agent", "implement")

	engine.RescheduleRateLimitedAgent("", "limited-agent")

	if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "limited-agent"); tracked {
		t.Fatal("tracked agent step mapping should be cleared for empty task IDs")
	}
	if got := agents.CallCount(); got != 0 {
		t.Fatalf("unexpected replacement agent start count = %d, want 0", got)
	}
}

func TestRescheduleRateLimitedAgent_SkippedInflightDoesNotConsumeWatchdogRetry(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "watchdog: rate limit: org-level quota exhausted",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables: map[string]string{
				watchdogRateLimitRetryKey("implement"): "1",
			},
		},
	})
	setWorkflowAgentRoute(t, tasks, "t1", "limited-agent", "implement")

	unlockInflight := engine.acquireInflight("t1")
	engine.RescheduleRateLimitedAgent("t1", "limited-agent")
	unlockInflight()

	if got := agents.CallCount(); got != 0 {
		t.Fatalf("unexpected replacement agent starts = %d, want 0", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "in-progress" {
		t.Fatalf("status = %q, want in-progress", got.Status)
	}
	if got.StatusReason != "watchdog: rate limit: org-level quota exhausted" {
		t.Fatalf("status_reason = %q", got.StatusReason)
	}
	if got.Workflow.Variables[watchdogRateLimitRetryKey("implement")] != "1" {
		t.Fatalf("rate-limit retry var = %q, want 1", got.Workflow.Variables[watchdogRateLimitRetryKey("implement")])
	}
}

func TestRescheduleRateLimitedAgent_SkippedSharedClaimDoesNotConsumeWatchdogRetry(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "watchdog: rate limit: org-level quota exhausted",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables: map[string]string{
				watchdogRateLimitRetryKey("implement"): "1",
			},
		},
	})
	setWorkflowAgentRoute(t, tasks, "t1", "limited-agent", "implement")

	agents.SetDispatchClaimed("t1", true)
	engine.RescheduleRateLimitedAgent("t1", "limited-agent")

	if got := agents.CallCount(); got != 0 {
		t.Fatalf("unexpected replacement agent starts = %d, want 0", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "in-progress" {
		t.Fatalf("status = %q, want in-progress", got.Status)
	}
	if got.StatusReason != "watchdog: rate limit: org-level quota exhausted" {
		t.Fatalf("status_reason = %q", got.StatusReason)
	}
	if got.Workflow.Variables[watchdogRateLimitRetryKey("implement")] != "1" {
		t.Fatalf("rate-limit retry var = %q, want 1", got.Workflow.Variables[watchdogRateLimitRetryKey("implement")])
	}
}

func TestRescheduleRateLimitedAgent_SkipsBlockedStatus(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "blocked",
		StatusReason: "watchdog: zero-output startup retry budget exhausted after 3 identical attempts",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecFailed,
			Variables: map[string]string{
				watchdogRateLimitRetryKey("implement"): strconv.Itoa(maxWatchdogRateLimitRetries),
			},
		},
	})
	setWorkflowAgentRoute(t, tasks, "t1", "limited-agent", "implement")

	engine.RescheduleRateLimitedAgent("t1", "limited-agent")

	if got := agents.CallCount(); got != 0 {
		t.Fatalf("unexpected replacement agent starts = %d, want 0", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", got.Status)
	}
	if got.StatusReason != "watchdog: zero-output startup retry budget exhausted after 3 identical attempts" {
		t.Fatalf("status_reason = %q", got.StatusReason)
	}
	if got.Workflow.Variables[watchdogRateLimitRetryKey("implement")] != strconv.Itoa(maxWatchdogRateLimitRetries) {
		t.Fatalf("rate-limit retry var = %q, want %d", got.Workflow.Variables[watchdogRateLimitRetryKey("implement")], maxWatchdogRateLimitRetries)
	}
	if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "limited-agent"); tracked {
		t.Fatal("rate-limited agent step mapping was not cleared")
	}
}

func TestRescheduleRateLimitedAgent_HoldsDispatchClaimAcrossRescheduleAttempt(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.startEntered = make(chan struct{}, 1)
	agents.startGate = make(chan struct{})
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "watchdog: rate limit: org-level quota exhausted",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})

	setWorkflowAgentRoute(t, tasks, "t1", "limited-agent-1", "implement")
	setWorkflowAgentRoute(t, tasks, "t1", "limited-agent-2", "implement")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		engine.RescheduleRateLimitedAgent("t1", "limited-agent-1")
	}()

	<-agents.startEntered

	go func() {
		defer wg.Done()
		engine.RescheduleRateLimitedAgent("t1", "limited-agent-2")
	}()

	close(agents.startGate)
	wg.Wait()

	if got := agents.CallCount(); got != 1 {
		t.Fatalf("replacement agent starts = %d, want 1", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Workflow.Variables[watchdogRateLimitRetryKey("implement")] != "1" {
		t.Fatalf("rate-limit retry var = %q, want 1", got.Workflow.Variables[watchdogRateLimitRetryKey("implement")])
	}
}

func TestRescheduleCheckpointedAgent_RerunsCurrentStepSameWorktree(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.claimInsideStart = true
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	engine.SetMaxCheckpoints(3)

	const worktreeDir = "/tmp/checkpoint-worktree"
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables: map[string]string{
				WorkflowVarDir:                    worktreeDir,
				"step.implement.checkpoint_count": "1",
			},
		},
	})
	setWorkflowAgentRoute(t, tasks, "t1", "checkpoint-agent", "implement")

	engine.RescheduleCheckpointedAgent("t1", "checkpoint-agent")

	if got := agents.CallCount(); got != 1 {
		t.Fatalf("expected replacement agent start, got %d", got)
	}
	agents.mu.Lock()
	call := agents.calls[len(agents.calls)-1]
	agents.mu.Unlock()
	if call.Dir != worktreeDir {
		t.Fatalf("replacement dir = %q, want %q", call.Dir, worktreeDir)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Workflow.Variables["step.implement.checkpoint_count"] != "2" {
		t.Fatalf("checkpoint count = %q, want 2", got.Workflow.Variables["step.implement.checkpoint_count"])
	}
	if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "checkpoint-agent"); tracked {
		t.Fatal("checkpointed agent step mapping was not cleared")
	}
}

func TestRescheduleCheckpointedAgent_RetriesGhostDispatchPark(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.failSpawnOnce = ErrDispatchInFlight
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	engine.SetMaxCheckpoints(3)

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables: map[string]string{
				WorkflowVarDir:                    "/tmp/checkpoint-worktree",
				"step.implement.checkpoint_count": "1",
			},
		},
	})
	setWorkflowAgentRoute(t, tasks, "t1", "checkpoint-agent", "implement")

	engine.RescheduleCheckpointedAgent("t1", "checkpoint-agent")

	if got := agents.CallCount(); got != 1 {
		t.Fatalf("replacement agent starts = %d, want 1 after retrying ghost park", got)
	}
}

func TestRescheduleCheckpointedAgent_ParksAtCap(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	engine.SetMaxCheckpoints(2)

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables: map[string]string{
				"step.implement.checkpoint_count": "2",
			},
		},
	})
	setWorkflowAgentRoute(t, tasks, "t1", "checkpoint-agent", "implement")

	engine.RescheduleCheckpointedAgent("t1", "checkpoint-agent")

	if got := agents.CallCount(); got != 0 {
		t.Fatalf("unexpected replacement agent starts = %d, want 0", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if got.Workflow.Variables["step.implement.checkpoint_count"] != "2" {
		t.Fatalf("checkpoint count = %q, want unchanged 2", got.Workflow.Variables["step.implement.checkpoint_count"])
	}
}

func TestRescheduleRateLimitedAgent_RerunsParallelChild(t *testing.T) {
	store := newTestStoreWith(t, "test-parallel.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-parallel",
			CurrentStep: "plan",
			State:       ExecWaiting,
			Variables:   make(map[string]string),
			ParallelInflight: map[string]*ParallelChildren{
				"plan": {
					ParentStepID: "plan",
					Children: map[string]*ChildStatus{
						"plan_a": {Status: "pending", AgentID: "limited-agent", Provider: "claude"},
						"plan_b": {Status: "pending", AgentID: "other-agent", Provider: "codex"},
					},
				},
			},
		},
	})
	setWorkflowAgentRoute(t, tasks, "t1", "limited-agent", "plan_a")

	engine.RescheduleRateLimitedAgent("t1", "limited-agent")

	if agents.CallCount() != 1 {
		t.Fatalf("expected replacement parallel child start, got %d", agents.CallCount())
	}
	if got := agents.LastCall(); got.Prompt != "Plan A t1" {
		t.Fatalf("unexpected replacement call: %+v", got)
	}
	if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "limited-agent"); tracked {
		t.Fatal("rate-limited child step mapping was not cleared")
	}
	if spawnedStep, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "agent-1"); !tracked || spawnedStep != "plan_a" {
		t.Fatalf("replacement child mapping = (%q, %v), want plan_a/true", spawnedStep, tracked)
	}
	updated, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Workflow == nil || updated.Workflow.ParallelInflight == nil {
		t.Fatalf("workflow parallel state missing: %+v", updated.Workflow)
	}
	rec := updated.Workflow.ParallelInflight["plan"]
	if rec == nil || rec.Children == nil {
		t.Fatalf("parallel record missing: %+v", rec)
	}
	child := rec.Children["plan_a"]
	if child == nil {
		t.Fatal("child plan_a missing")
		return
	}
	if child.AgentID != "agent-1" || child.Status != "pending" {
		t.Fatalf("child status = %+v, want pending agent-1", child)
	}
}

func TestRescheduleRateLimitedAgent_ParallelChildWatchdogRetriesThenEscalates(t *testing.T) {
	tests := []struct {
		name       string
		retries    string
		wantStarts int
		wantStatus taskstatus.Status
		wantReason string
		wantRetry  string
	}{
		{
			name:       "first watchdog rate limit reruns child",
			wantStarts: 1,
			wantStatus: "in-progress",
			wantReason: "",
			wantRetry:  "1",
		},
		{
			name:       "budget exhausted escalates instead of looping forever",
			retries:    "2",
			wantStarts: 0,
			wantStatus: "human-required",
			wantReason: "watchdog: rate limit retry budget exhausted after 2 clean re-dispatches",
			wantRetry:  "2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStoreWith(t, "test-parallel.yaml")
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewTestEngine(store, tasks, agents, discardLogger())

			vars := map[string]string{}
			if tc.retries != "" {
				vars[watchdogRateLimitRetryKey("plan_a")] = tc.retries
			}
			tasks.Put(TaskInfo{
				ID:           "t1",
				Status:       "in-progress",
				StatusReason: "watchdog: rate limit: org-level quota exhausted",
				AgentMode:    "headless",
				Workflow: &Execution{
					WorkflowID:  "test-parallel",
					CurrentStep: "plan",
					State:       ExecWaiting,
					Variables:   vars,
					ParallelInflight: map[string]*ParallelChildren{
						"plan": {
							ParentStepID: "plan",
							Children: map[string]*ChildStatus{
								"plan_a": {Status: "pending", AgentID: "limited-agent", Provider: "claude"},
								"plan_b": {Status: "pending", AgentID: "other-agent", Provider: "codex"},
							},
						},
					},
				},
			})
			setWorkflowAgentRoute(t, tasks, "t1", "limited-agent", "plan_a")

			engine.RescheduleRateLimitedAgent("t1", "limited-agent")

			if got := agents.CallCount(); got != tc.wantStarts {
				t.Fatalf("expected replacement agent starts = %d, got %d", tc.wantStarts, got)
			}
			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.StatusReason != tc.wantReason {
				t.Fatalf("status_reason = %q, want %q", got.StatusReason, tc.wantReason)
			}
			if got.Workflow.Variables[watchdogRateLimitRetryKey("plan_a")] != tc.wantRetry {
				t.Fatalf("rate-limit retry var = %q, want %q", got.Workflow.Variables[watchdogRateLimitRetryKey("plan_a")], tc.wantRetry)
			}
			if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "limited-agent"); tracked {
				t.Fatal("rate-limited child step mapping was not cleared")
			}
		})
	}
}

func TestResumeStalled_ParallelProviderUnhealthyLeavesChildPending(t *testing.T) {
	store := newTestStoreWith(t, "test-parallel.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetFailSpawn(&providerpkg.UnhealthyError{Provider: "claude", Reason: "rate_limited"})
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-parallel",
			CurrentStep: "plan",
			State:       ExecWaiting,
			Variables:   make(map[string]string),
			ParallelInflight: map[string]*ParallelChildren{
				"plan": {
					ParentStepID: "plan",
					Children: map[string]*ChildStatus{
						"plan_a": {Status: "pending"},
						"plan_b": {Status: "pending"},
					},
				},
			},
		},
	})

	engine.ResumeStalled()

	updated, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Workflow == nil || updated.Workflow.ParallelInflight == nil {
		t.Fatalf("workflow parallel state missing: %+v", updated.Workflow)
	}
	rec := updated.Workflow.ParallelInflight["plan"]
	if rec == nil || rec.Children == nil {
		t.Fatalf("parallel record missing: %+v", rec)
	}
	for id, child := range rec.Children {
		if child == nil {
			t.Fatalf("child %s missing", id)
			return
		}
		if child.Status != "pending" {
			t.Fatalf("child %s status = %q, want pending after transient provider block", id, child.Status)
		}
	}
	if updated.Status != "in-progress" {
		t.Fatalf("task status = %q, want in-progress", updated.Status)
	}
	if got := tasks.Reason("t1"); !strings.Contains(got, "provider claude unhealthy") {
		t.Fatalf("reason = %q, want provider unhealthy reason", got)
	}
}

func TestResumeStalled_FailoversRateLimitedProvider(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetProviderRateLimited(true)
	agents.SetProviderFailover(true)
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecRunning,
			Variables:   make(map[string]string),
		},
	})

	engine.ResumeStalled()

	if agents.CallCount() != 1 {
		t.Fatalf("expected 1 agent start when failover is available, got %d", agents.CallCount())
	}
}

func TestResumeStalled_SkipsWorkflowRetryUntil(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	fakeClock := clock.NewFake(time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC))
	engine.SetClock(fakeClock)

	wf := &Execution{
		WorkflowID:  "test-simple",
		CurrentStep: "implement",
		State:       ExecWaiting,
		Variables: map[string]string{
			workflowRetryAfterVar: fakeClock.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow:  wf,
	})

	engine.ResumeStalled()
	if agents.CallCount() != 0 {
		t.Fatalf("agent starts before retry window = %d, want 0", agents.CallCount())
	}

	fakeClock.Advance(time.Hour + time.Second)
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow:  wf,
	})

	engine.ResumeStalled()
	if agents.CallCount() != 1 {
		t.Fatalf("agent starts after retry window = %d, want 1", agents.CallCount())
	}
}

// TestResumeStalled_SkipLogsPromotedToThrottledInfo proves ResumeStalled's
// skip-reason logs are visible at the default (INFO) log level instead of
// being silently swallowed at Debug: a dropped/skipped resume was previously
// invisible in production logs. The first occurrence of a given skip under a
// task logs at INFO; identical repeats on later ticks are dropped, so a
// long-lived cooldown (a multi-hour retry_after window, or a multi-day
// provider park) does not write a line per maintenance tick at any level.
func TestResumeStalled_SkipLogsPromotedToThrottledInfo(t *testing.T) {
	var records []slog.Record
	logger := slog.New(&demotionRecordHandler{records: &records})

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, logger)

	retryAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	wf := &Execution{
		WorkflowID:  "test-simple",
		CurrentStep: "implement",
		State:       ExecWaiting,
		Variables: map[string]string{
			workflowRetryAfterVar: retryAt,
		},
	}
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow:  wf,
	})

	skipRecords := func() []slog.Record {
		var out []slog.Record
		for _, r := range records {
			if r.Message == "workflow.resume-stalled.skip" {
				out = append(out, r)
			}
		}
		return out
	}

	engine.ResumeStalled()
	first := skipRecords()
	if len(first) != 1 {
		t.Fatalf("got %d skip records after first tick, want 1: %+v", len(first), first)
	}
	if first[0].Level != slog.LevelInfo {
		t.Fatalf("first skip level = %v, want Info", first[0].Level)
	}
	if got := recordAttr(first[0], "reason"); got != "retry_after" {
		t.Fatalf("reason = %q, want retry_after", got)
	}

	// A repeat inside logging.InfoRepeatInterval is dropped outright, not
	// downgraded: a park that lasts days would otherwise write one line per
	// maintenance tick at debug level. Re-emission after the interval is
	// covered by the logging package's own clock-seamed test.
	records = nil
	for range 5 {
		engine.ResumeStalled()
	}
	if repeat := skipRecords(); len(repeat) != 0 {
		t.Fatalf("got %d skip records from repeat ticks, want 0: %+v", len(repeat), repeat)
	}
}

// A rate-limited provider is the one skip reason that can hold for days, and
// it was the one reason logged straight to Debug instead of through the
// throttle — invisible at the default level, and one line per tick at debug.
func TestResumeStalled_RateLimitedProviderSkipIsThrottled(t *testing.T) {
	var records []slog.Record
	logger := slog.New(&demotionRecordHandler{records: &records})

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetProviderRateLimited(true)
	engine := NewTestEngine(store, tasks, agents, logger)

	for _, id := range []string{"t1", "t2"} {
		tasks.Put(TaskInfo{
			ID:        id,
			Status:    "in-progress",
			AgentMode: "headless",
			Workflow: &Execution{
				WorkflowID:  "test-simple",
				CurrentStep: "implement",
				State:       ExecWaiting,
				StartedAt:   time.Now().UTC(),
			},
		})
	}

	skips := func() []slog.Record {
		var out []slog.Record
		for _, r := range records {
			if r.Message == "workflow.resume-stalled.skip" &&
				recordAttr(r, "reason") == "provider_rate_limited" {
				out = append(out, r)
			}
		}
		return out
	}

	engine.ResumeStalled()
	first := skips()
	// One per task: keying the throttle on anything coarser would hide every
	// parked task in the fleet behind whichever one parked first.
	if len(first) != 2 {
		t.Fatalf("got %d park records on the first tick, want one per task: %+v", len(first), first)
	}
	for _, r := range first {
		if r.Level != slog.LevelInfo {
			t.Errorf("park logged at %v, want Info so it is visible at the default level", r.Level)
		}
		// test-simple's implement step declares no provider, and 9 of the 14
		// builtin run_agent steps are the same. Reporting an empty provider
		// defeats the whole point of promoting this line out of Debug.
		if got := recordAttr(r, "provider"); got != "claude" {
			t.Errorf("provider = %q, want the default the park is actually about", got)
		}
	}

	records = nil
	for range 20 {
		engine.ResumeStalled()
	}
	if repeat := skips(); len(repeat) != 0 {
		t.Errorf("got %d park records from repeat ticks, want 0: %+v", len(repeat), repeat)
	}
}

// A park that ends and recurs must be logged again. The throttle only re-arms
// on Clear, and nothing called it — so a second park was invisible at INFO and
// suppressed at DEBUG, which is strictly worse than the per-tick Debug line
// this replaced.
func TestResumeStalled_LaterParkIsLoggedAgain(t *testing.T) {
	var records []slog.Record
	logger := slog.New(&demotionRecordHandler{records: &records})

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetProviderRateLimitedFor("claude", true)
	engine := NewTestEngine(store, tasks, agents, logger)

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			StartedAt:   time.Now().UTC(),
		},
	})

	parkRecords := func() int {
		n := 0
		for _, r := range records {
			if recordAttr(r, "reason") == "provider_rate_limited" {
				n++
			}
		}
		return n
	}

	engine.ResumeStalled()
	if got := parkRecords(); got != 1 {
		t.Fatalf("first park logged %d times, want 1", got)
	}

	// The park ends, the resume proceeds, and that run finishes — the state a
	// task is in when a later park hits it.
	agents.SetProviderRateLimitedFor("claude", false)
	engine.ResumeStalled()
	agents.SimulateComplete("t1")
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			StartedAt:   time.Now().UTC(),
		},
	})

	records = nil
	agents.SetProviderRateLimitedFor("claude", true)
	engine.ResumeStalled()
	if got := parkRecords(); got != 1 {
		t.Errorf("second park logged %d times, want 1: the throttle never re-armed", got)
	}
}

func TestResumeStalled_SkipWaitHuman(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	// Task at wait_human step.
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "plan-review",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "review_plan",
			State:       ExecWaiting,
			Variables:   make(map[string]string),
		},
	})

	engine.ResumeStalled()

	// Should NOT start any agent.
	if agents.CallCount() != 0 {
		t.Fatalf("expected 0 agent starts for wait_human, got %d", agents.CallCount())
	}
}

// TestResumeStalled_SkipsHumanRequired reproduces the post-restart dispatch bug
// where a review task was parked on human-required but ResumeStalled
// re-dispatched its workflow's run_agent step after restart, overriding the
// triage verdict.
func TestResumeStalled_SkipsHumanRequired(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	// Simulate a review task whose pr-review workflow is at the implement
	// (run_agent) step but whose status was flipped to human-required before
	// the restart.
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "human-required",
		AgentMode: "headless",
		Tags:      []string{"review"},
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecRunning,
			Variables:   make(map[string]string),
		},
	})

	engine.ResumeStalled()

	// Must NOT dispatch an agent: human-required overrides the workflow.
	if agents.CallCount() != 0 {
		t.Fatalf("expected 0 agent starts for human-required task, got %d", agents.CallCount())
	}
}

func TestResumeStalled_SkipsBlockedStatus(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "blocked",
		StatusReason: "watchdog: zero-output startup retry budget exhausted after 3 identical attempts",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecFailed,
			Variables: map[string]string{
				watchdogRateLimitRetryKey("implement"): strconv.Itoa(maxWatchdogRateLimitRetries),
			},
		},
	})

	engine.ResumeStalled()

	if agents.CallCount() != 0 {
		t.Fatalf("expected 0 agent starts for blocked task, got %d", agents.CallCount())
	}
}

func TestResumeStalled_SkipsDoneStatus(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	// Simulate a task whose PR merged (flipping Status to done) while its
	// workflow was still paused Running/Waiting at an earlier step — e.g.
	// advanceClosedTaskPRsWithFetch racing ResumeStalled before the workflow
	// is cancelled. Resuming would rebase the already-merged branch against
	// origin/main and self-conflict, flipping the done task back to
	// human-required.
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "done",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecRunning,
			Variables:   make(map[string]string),
		},
	})

	engine.ResumeStalled()

	if agents.CallCount() != 0 {
		t.Fatalf("expected 0 agent starts for done task, got %d", agents.CallCount())
	}
}

func TestResumeStalled_SkipsTerminalStatusAfterFreshRead(t *testing.T) {
	tests := []struct {
		name   string
		status taskstatus.Status
	}{
		{name: "done", status: "done"},
		{name: "cancelled", status: "cancelled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewTestEngine(store, tasks, agents, discardLogger())

			tasks.Put(TaskInfo{
				ID:        "t1",
				Status:    "in-review",
				AgentMode: "headless",
				Workflow: &Execution{
					WorkflowID:  "test-simple",
					CurrentStep: "implement",
					State:       ExecRunning,
					Variables:   make(map[string]string),
				},
			})
			tasks.SetGetTaskHook(func(id string, task *TaskInfo, count int) {
				if id == "t1" && count == 1 {
					task.Status = tc.status
				}
			})

			engine.ResumeStalled()

			if got := agents.CallCount(); got != 0 {
				t.Fatalf("expected 0 agent starts after status flipped to %s, got %d", tc.status, got)
			}
			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != tc.status {
				t.Fatalf("status = %q, want %q", got.Status, tc.status)
			}
		})
	}
}

func TestRescheduleRateLimitedAgent_ParallelChildSkipsTerminalStatusAfterFreshRead(t *testing.T) {
	tests := []struct {
		name   string
		status taskstatus.Status
	}{
		{name: "done", status: "done"},
		{name: "cancelled", status: "cancelled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStoreWith(t, "test-parallel.yaml")
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewTestEngine(store, tasks, agents, discardLogger())

			tasks.Put(TaskInfo{
				ID:        "t1",
				Status:    "in-progress",
				AgentMode: "headless",
				Workflow: &Execution{
					WorkflowID:  "test-parallel",
					CurrentStep: "plan",
					State:       ExecWaiting,
					Variables:   make(map[string]string),
					ParallelInflight: map[string]*ParallelChildren{
						"plan": {
							ParentStepID: "plan",
							Children: map[string]*ChildStatus{
								"plan_a": {Status: "pending", AgentID: "limited-agent", Provider: "claude"},
								"plan_b": {Status: "pending", AgentID: "other-agent", Provider: "codex"},
							},
						},
					},
				},
			})
			setWorkflowAgentRoute(t, tasks, "t1", "limited-agent", "plan_a")
			tasks.SetGetTaskHook(func(id string, task *TaskInfo, count int) {
				if id == "t1" && count == 2 {
					task.Status = tc.status
				}
			})

			engine.RescheduleRateLimitedAgent("t1", "limited-agent")

			if got := agents.CallCount(); got != 0 {
				t.Fatalf("expected 0 replacement agent starts after status flipped to %s, got %d", tc.status, got)
			}
			if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "limited-agent"); tracked {
				t.Fatal("rate-limited child step mapping was not cleared")
			}
			got, err := tasks.GetTask("t1")
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != tc.status {
				t.Fatalf("status = %q, want %q", got.Status, tc.status)
			}
		})
	}
}

func TestResumeStalled_SkipsTaskWithRunningAgent(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecRunning,
			Variables:   make(map[string]string),
		},
	})
	// Simulate an agent already running.
	_, _, _, _ = agents.StartAgent("t1", "implementation", "headless", "sonnet", "", "test", "", nil, false, false, "", "", AgentAssignment{})

	initialCalls := agents.CallCount()
	engine.ResumeStalled()

	if agents.CallCount() != initialCalls {
		t.Fatal("ResumeStalled should not start another agent when one is already running")
	}
}

func TestResumeStalled_SkipsCompletedWorkflow(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	now := time.Now().UTC()
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "done",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "",
			State:       ExecCompleted,
			CompletedAt: &now,
			Variables:   make(map[string]string),
		},
	})

	engine.ResumeStalled()

	if agents.CallCount() != 0 {
		t.Fatal("ResumeStalled should skip completed workflows")
	}
}

// TestResumeStalled_SkipsInflightDispatch exercises the ResumeStalled → race
// that actually produced the duplicate spawn in prod. The ResumeStalled
// ticker fires during the 1-3s window while an interactive plan step is
// still preparing its worktree and starting the claude process — at that
// point no agent is registered yet so HasRunningAgent returns false.
// Without the inflight guard the ticker would call executeSteps → execRunAgent
// and spawn a second agent for the same step. With the guard it must skip.
func TestResumeStalled_SkipsInflightDispatch(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	// Task sitting at an interactive run_agent step with no running agent —
	// the shape ResumeStalled normally resumes.
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "planning",
		AgentMode: "interactive",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "plan",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})

	// Simulate the original dispatch being mid-flight inside AdvanceStep —
	// the per-task advance mutex is held, no agent registered yet (worktree
	// still being created in the real system, fake-claude hasn't started).
	unlockInflight := engine.acquireInflight("t1")

	before := agents.CallCount()
	engine.ResumeStalled()
	if got := agents.CallCount(); got != before {
		t.Errorf("ResumeStalled spawned a duplicate agent: calls %d → %d (expected no change while inflight)",
			before, got)
	}

	// Once the original dispatch finishes and releases the advance mutex,
	// a subsequent tick is allowed to resume — that's the real recovery path.
	unlockInflight()

	engine.ResumeStalled()
	if got := agents.CallCount(); got != before+1 {
		t.Errorf("ResumeStalled after inflight cleared: calls %d → %d (want +1)", before, got)
	}
}

// TestResumeStalled_SkipsClaimHeldByOutOfBandDispatcher verifies ResumeStalled
// consults the shared agent.Manager dispatch-claim coordinator (IsDispatching)
// in addition to workflow completion-routing bookkeeping. A claim
// held by a dispatcher the engine has no local visibility into — e.g.
// recovery.RestartStaleInProgress launching via agentorch.Orchestrator
// outside the workflow engine entirely — must still block a concurrent
// ResumeStalled redispatch, closing the exact split-ownership race the
// engine-local route tracking alone cannot see.
func TestResumeStalled_SkipsClaimHeldByOutOfBandDispatcher(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "planning",
		AgentMode: "interactive",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "plan",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})

	// Simulate an out-of-band dispatcher (recovery.RestartStaleInProgress)
	// holding the shared agent.Manager claim for t1 — invisible to the
	// engine's own completion-routing state.
	agents.SetDispatchClaimed("t1", true)

	before := agents.CallCount()
	engine.ResumeStalled()
	if got := agents.CallCount(); got != before {
		t.Errorf("ResumeStalled spawned a duplicate agent while the shared claim was held elsewhere: calls %d → %d",
			before, got)
	}

	// Once the out-of-band dispatcher releases its claim, ResumeStalled may
	// proceed as normal.
	agents.SetDispatchClaimed("t1", false)
	engine.ResumeStalled()
	if got := agents.CallCount(); got != before+1 {
		t.Errorf("ResumeStalled after shared claim released: calls %d → %d (want +1)", before, got)
	}
}

// TestResumeStalled_ConcurrentCallsReserveDispatchWindow verifies the engine
// still keeps its own short-lived per-task reservation between ResumeStalled's
// preflight and the eventual StartAgent call. The shared agent.Manager claim is
// only acquired inside StartAgent, so without this earlier reservation two
// concurrent ResumeStalled loops can both enter executeSteps before any claim
// exists and race a duplicate start.
func TestResumeStalled_ConcurrentCallsReserveDispatchWindow(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.startGate = make(chan struct{})
	agents.startEntered = make(chan struct{}, 2)
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "planning",
		AgentMode: "interactive",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "plan",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})

	var wg sync.WaitGroup
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			engine.ResumeStalled()
		}()
	}

	select {
	case <-agents.startEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first ResumeStalled never reached StartAgent")
	}

	select {
	case <-agents.startEntered:
		t.Fatal("second ResumeStalled reached StartAgent while the first dispatch window was still reserved")
	case <-time.After(100 * time.Millisecond):
	}

	close(agents.startGate)
	wg.Wait()

	if got := agents.CallCount(); got != 1 {
		t.Fatalf("StartAgent call count = %d, want 1", got)
	}
}

// The throttle is keyed on the task id, and several skip reasons share that
// key. Clearing on a merely-passing preflight re-armed all of them, so a task
// stuck behind an out-of-band dispatch claim logged a fresh INFO every tick —
// the flood this change exists to remove, moved up a level.
func TestResumeStalled_ClaimedElsewhereDoesNotReArmEveryTick(t *testing.T) {
	var records []slog.Record
	logger := slog.New(&demotionRecordHandler{records: &records})

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetDispatchClaimed("t1", true)
	engine := NewTestEngine(store, tasks, agents, logger)

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			StartedAt:   time.Now().UTC(),
		},
	})

	for range 10 {
		engine.ResumeStalled()
	}

	info := 0
	for _, r := range records {
		if r.Message == "workflow.resume-stalled.skip" && r.Level == slog.LevelInfo {
			info++
		}
	}
	if info != 1 {
		t.Errorf("got %d INFO skip records across 10 identical ticks, want 1", info)
	}
}

// A park that moves to a different provider is a state change, and the whole
// reason the provider is in the throttle value is that it must re-arm INFO
// rather than wait out the interval.
func TestResumeStalled_ParkMovingProviderReArms(t *testing.T) {
	var records []slog.Record
	logger := slog.New(&demotionRecordHandler{records: &records})

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetProviderRateLimitedFor("claude", true)
	engine := NewTestEngine(store, tasks, agents, logger)

	put := func() {
		tasks.Put(TaskInfo{
			ID: "t1", Status: "in-progress", AgentMode: "headless",
			Workflow: &Execution{
				WorkflowID:  "test-simple",
				CurrentStep: "implement",
				State:       ExecWaiting,
				StartedAt:   time.Now().UTC(),
			},
		})
	}

	parks := func() int {
		n := 0
		for _, r := range records {
			if recordAttr(r, "reason") == "provider_rate_limited" && r.Level == slog.LevelInfo {
				n++
			}
		}
		return n
	}

	put()
	engine.ResumeStalled()
	if got := parks(); got != 1 {
		t.Fatalf("claude park logged %d times at INFO, want 1", got)
	}

	records = nil
	agents.SetProviderRateLimitedFor("claude", false)
	agents.SetProviderRateLimitedFor("codex", true)
	agents.SetDefaultProvider("codex")
	put()
	engine.ResumeStalled()
	if got := parks(); got != 1 {
		t.Errorf("park moving to codex logged %d times at INFO, want 1", got)
	}
}

// The reschedule route logs its own park and fires before ResumeStalled ever
// sees the task, so it needs its own re-arm. Without one, a park that ends and
// recurs here is dropped entirely rather than logged.
func TestRescheduleRateLimitedAgent_LaterParkIsLoggedAgain(t *testing.T) {
	var records []slog.Record
	logger := slog.New(&demotionRecordHandler{records: &records})

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetProviderRateLimitedFor("claude", true)
	engine := NewTestEngine(store, tasks, agents, logger)

	park := func(agentID string) {
		tasks.Put(TaskInfo{
			ID: "t1", Status: "in-progress", AgentMode: "headless",
			Workflow: &Execution{
				WorkflowID:  "test-simple",
				CurrentStep: "implement",
				State:       ExecWaiting,
				Variables:   make(map[string]string),
			},
		})
		setWorkflowAgentRoute(t, tasks, "t1", agentID, "implement")
		engine.RescheduleRateLimitedAgent("t1", agentID)
	}

	parks := func() int {
		n := 0
		for _, r := range records {
			if r.Message == "workflow.rate-limit-reschedule.park" &&
				recordAttr(r, "reason") == "provider_rate_limited" {
				n++
			}
		}
		return n
	}

	park("limited-agent-1")
	if got := parks(); got != 1 {
		t.Fatalf("first park logged %d times, want 1", got)
	}

	// The limit lifts and the reschedule dispatches, ending the park.
	agents.SetProviderRateLimitedFor("claude", false)
	park("limited-agent-2")
	if agents.CallCount() == 0 {
		t.Fatal("reschedule never dispatched, so the park never ended")
	}

	records = nil
	agents.SetProviderRateLimitedFor("claude", true)
	park("limited-agent-3")
	if got := parks(); got != 1 {
		t.Errorf("second park logged %d times, want 1: the throttle never re-armed", got)
	}
}

// The park value has to discriminate by step, or a park on a later step is
// suppressed as a repeat of an earlier one under the same provider.
func TestResumeStalled_ParkOnAnotherStepIsLogged(t *testing.T) {
	var records []slog.Record
	logger := slog.New(&demotionRecordHandler{records: &records})

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetProviderRateLimited(true)
	engine := NewTestEngine(store, tasks, agents, logger)

	put := func(step string) {
		tasks.Put(TaskInfo{
			ID: "t1", Status: "in-progress", AgentMode: "headless",
			Workflow: &Execution{
				WorkflowID: "test-simple", CurrentStep: step,
				State: ExecWaiting, StartedAt: time.Now().UTC(),
			},
		})
	}

	parks := func() int {
		n := 0
		for _, r := range records {
			if recordAttr(r, "reason") == "provider_rate_limited" && r.Level == slog.LevelInfo {
				n++
			}
		}
		return n
	}

	put("implement")
	engine.ResumeStalled()
	if got := parks(); got != 1 {
		t.Fatalf("park on implement logged %d times, want 1", got)
	}
	var step string
	for _, r := range records {
		if recordAttr(r, "reason") == "provider_rate_limited" {
			step = recordAttr(r, "step")
		}
	}
	if step != "implement" {
		t.Errorf("step attr = %q, want implement — an operator cannot tell which step is parked", step)
	}

	records = nil
	put("plan")
	engine.ResumeStalled()
	if got := parks(); got != 1 {
		t.Errorf("park on a second step logged %d times, want 1", got)
	}
}
