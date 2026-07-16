package watchdog

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/task"
)

func TestStallLimit(t *testing.T) {
	tests := []struct {
		name string
		tags []string
		want time.Duration
	}{
		{"small", []string{"small"}, 10 * time.Minute},
		{"medium", []string{"medium"}, 15 * time.Minute},
		{"unset", nil, 15 * time.Minute},
		{"large", []string{"large"}, 45 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stallLimit(tc.tags)
			if got != tc.want {
				t.Fatalf("stallLimit(%v) = %v, want %v", tc.tags, got, tc.want)
			}
		})
	}
}

func TestInspectHeadless_DefersWhileForegroundCheckRunning(t *testing.T) {
	tasks, tk := newTestTasks(t)

	inspectCalled := false
	w := &Watchdog{
		tasks:  tasks,
		logger: slog.New(slog.DiscardHandler),
		wg:     &sync.WaitGroup{},
		inspectAgent: func(context.Context, *slog.Logger, agent.InspectInput) (agent.InspectorVerdict, error) {
			inspectCalled = true
			return agent.InspectorVerdict{Recommendation: "continue"}, nil
		},
	}

	now := time.Now().UTC()
	ag := &agent.Agent{
		ID:          "a1",
		TaskID:      tk.ID,
		StartedAt:   now.Add(-50 * time.Minute),
		LastEventAt: now.Add(-20 * time.Minute),
	}
	ag.SetLogPath("/tmp/watchdog-a1.ndjson")
	ag.AppendOutput(agent.StreamEvent{Type: "assistant", Content: "running"})
	ag.SetLastEventAt(now.Add(-20 * time.Minute))
	ag.SetForegroundCommand("tu-1", "mise run verify", now.Add(-20*time.Minute))

	s := newState()
	w.inspectHeadless(context.Background(), s, now, ag)

	if inspectCalled {
		t.Fatal("inspectAgent called while a foreground verify command is still running")
	}
	if s.ready(ag.ID, now.Add(4*time.Minute)) {
		t.Fatal("foreground-command defer expired too early")
	}
	if !s.ready(ag.ID, now.Add(6*time.Minute)) {
		t.Fatal("foreground-command defer did not expire after debounce window")
	}
}

func TestInspectHeadless_DefersAndBacksOffUnderPressure(t *testing.T) {
	tasks, tk := newTestTasks(t)

	inspectCalled := false
	w := &Watchdog{
		tasks:  tasks,
		logger: slog.New(slog.DiscardHandler),
		wg:     &sync.WaitGroup{},
		inspectAgent: func(context.Context, *slog.Logger, agent.InspectInput) (agent.InspectorVerdict, error) {
			inspectCalled = true
			return agent.InspectorVerdict{Recommendation: "continue"}, nil
		},
		admitPressure: func() (bool, string) {
			return false, "load per cpu 99.00 exceeds maximum 8.00"
		},
	}

	now := time.Now().UTC()
	ag := &agent.Agent{
		ID:          "a1",
		TaskID:      tk.ID,
		StartedAt:   now.Add(-50 * time.Minute),
		LastEventAt: now.Add(-20 * time.Minute),
	}
	ag.SetLogPath("/tmp/watchdog-a1.ndjson")
	ag.AppendOutput(agent.StreamEvent{Type: "assistant", Content: "running"})
	ag.SetLastEventAt(now.Add(-20 * time.Minute))

	s := newState()
	w.inspectHeadless(context.Background(), s, now, ag)

	if inspectCalled {
		t.Fatal("inspectAgent called under machine pressure")
	}
	if s.ready(ag.ID, now.Add(90*time.Second)) {
		t.Fatal("first pressure backoff expired too early")
	}

	secondAttempt := now.Add(2 * time.Minute)
	w.inspectHeadless(context.Background(), s, secondAttempt, ag)
	if inspectCalled {
		t.Fatal("inspectAgent called during second pressure defer")
	}
	if s.ready(ag.ID, secondAttempt.Add(3*time.Minute+30*time.Second)) {
		t.Fatal("second pressure backoff expired too early")
	}
	if !s.ready(ag.ID, secondAttempt.Add(4*time.Minute)) {
		t.Fatal("second pressure backoff did not expand to four minutes")
	}
}

func TestInspectHeadless_PassesSemanticLoopSummaryToInspector(t *testing.T) {
	tasks, tk := newTestTasks(t)

	var got agent.InspectInput
	w := &Watchdog{
		tasks:         tasks,
		logger:        slog.New(slog.DiscardHandler),
		wg:            &sync.WaitGroup{},
		loopThreshold: 6,
		emit:          func(string, any) {},
		stopAgent:     func(string) error { return nil },
		inspectAgent: func(_ context.Context, _ *slog.Logger, in agent.InspectInput) (agent.InspectorVerdict, error) {
			got = in
			return agent.InspectorVerdict{Recommendation: "continue"}, nil
		},
	}

	now := time.Now().UTC()
	ag := &agent.Agent{
		ID:          "a1",
		TaskID:      tk.ID,
		StartedAt:   now.Add(-10 * time.Minute),
		LastEventAt: now.Add(-time.Minute),
	}
	ag.SetLogPath("/tmp/watchdog-a1.ndjson")
	ag.AppendOutput(agent.StreamEvent{Type: "assistant", Content: "looping"})
	for _, step := range []struct {
		sig   string
		label string
	}{
		{"check:go test ./...", "check:go test ./..."},
		{"read:build.log", "read:build.log"},
		{"check:go test ./...", "check:go test ./..."},
		{"read:build.log", "read:build.log"},
		{"check:go test ./...", "check:go test ./..."},
		{"read:build.log", "read:build.log"},
	} {
		ag.NoteToolAction(step.sig, step.label)
	}

	w.inspectHeadless(context.Background(), newState(), now, ag)
	w.wg.Wait()

	if got.Trigger != "loop" {
		t.Fatalf("Trigger = %q, want loop", got.Trigger)
	}
	if !strings.Contains(got.LoopSummary, "cycled across 2 semantic families") {
		t.Fatalf("LoopSummary = %q, want semantic cycle summary", got.LoopSummary)
	}
	if !strings.Contains(got.LoopSummary, "check:go test ./...") || !strings.Contains(got.LoopSummary, "read:build.log") {
		t.Fatalf("LoopSummary = %q, want both repeated families", got.LoopSummary)
	}
}

func TestIsLongRunningCheckCommand(t *testing.T) {
	tests := []struct {
		command string
		want    bool
	}{
		{"mise run verify", true},
		{"sh -lc 'go test ./...'", true},
		{"npm ci", true},
		{"pwd", false},
		{"rg watchdog internal", false},
	}
	for _, tc := range tests {
		t.Run(tc.command, func(t *testing.T) {
			if got := isLongRunningCheckCommand(tc.command); got != tc.want {
				t.Fatalf("isLongRunningCheckCommand(%q) = %v, want %v", tc.command, got, tc.want)
			}
		})
	}
}

func TestApplyVerdict_EscalateLeavesTaskRunning(t *testing.T) {
	tasks, tk := newTestTasks(t)

	stopped := false
	w := &Watchdog{
		tasks:     tasks,
		logger:    slog.New(slog.DiscardHandler),
		stopAgent: func(string) error { stopped = true; return nil },
	}

	w.applyVerdict(&agent.Agent{ID: "a1", TaskID: tk.ID}, "stall", agent.InspectorVerdict{
		Stuck:          true,
		Reason:         "ambiguous environment churn",
		Recommendation: "escalate",
	})

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusInProgress)
	}
	if got.StatusReason != "" {
		t.Fatalf("status_reason = %q, want empty", got.StatusReason)
	}
	if stopped {
		t.Fatal("stopAgent called on escalate verdict")
	}
}

func TestApplyVerdict_StopSetsReasonAndStopsAgent(t *testing.T) {
	tasks, tk := newTestTasks(t)

	stopped := false
	w := &Watchdog{
		tasks:     tasks,
		logger:    slog.New(slog.DiscardHandler),
		stopAgent: func(string) error { stopped = true; return nil },
	}

	w.applyVerdict(&agent.Agent{ID: "a1", TaskID: tk.ID}, "loop", agent.InspectorVerdict{
		Stuck:          true,
		Reason:         "looping on toolchain setup",
		Recommendation: "stop",
	})

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusHumanRequired)
	}
	if got.StatusReason != "watchdog: looping on toolchain setup" {
		t.Fatalf("status_reason = %q, want watchdog reason", got.StatusReason)
	}
	if !stopped {
		t.Fatal("stopAgent not called on stop verdict")
	}
}

// TestApplyVerdict_StopWithBufferedResultRoutesThroughCompletion covers
// #1836: a headless agent's stream can already contain a non-error terminal
// result (e.g. a test-runner's PASS verdict) with only trailing, non-result
// events after it (a lingering forked subagent) — the exact shape that made
// CompletedSuccessfully's old last-event check miss it in tick() and fall
// through to judge inspection. A "stop" verdict on such an agent must route
// through stopCompletedAgent (so the normal completion path — e.g.
// route_test_result — processes the buffered result) instead of force-
// setting human-required and discarding it via the generic stopAgent.
func TestApplyVerdict_StopWithBufferedResultRoutesThroughCompletion(t *testing.T) {
	tasks, tk := newTestTasks(t)

	var stoppedGeneric, stoppedCompleted string
	w := &Watchdog{
		tasks:              tasks,
		logger:             slog.New(slog.DiscardHandler),
		stopAgent:          func(id string) error { stoppedGeneric = id; return nil },
		stopCompletedAgent: func(id string) error { stoppedCompleted = id; return nil },
	}

	ag := &agent.Agent{ID: "a1", TaskID: tk.ID, Mode: "headless"}
	ag.AppendOutput(agent.StreamEvent{Type: "result", Content: `{"verdict":"PASS"}`})
	ag.AppendOutput(agent.StreamEvent{Type: "assistant", Content: "lingering subagent chatter"})

	w.applyVerdict(ag, "budget", agent.InspectorVerdict{
		Stuck:          false,
		Reason:         "Agent completed verification successfully with PASS verdict; idle post-completion",
		Recommendation: "stop",
	})

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want %q unchanged (completion path owns the transition)", got.Status, task.StatusInProgress)
	}
	if got.StatusReason != "" {
		t.Fatalf("status_reason = %q, want empty (watchdog must not force human-required)", got.StatusReason)
	}
	if stoppedCompleted != "a1" {
		t.Fatalf("stopCompletedAgent called with %q, want a1", stoppedCompleted)
	}
	if stoppedGeneric != "" {
		t.Fatalf("generic stopAgent called with %q, want unused", stoppedGeneric)
	}
}

// TestApplyVerdict_StopWithErrorResultStillEscalates ensures the buffered-
// result fast path in applyVerdict only kicks in for a *successful* terminal
// result — an agent whose last result was an error must still take the
// normal human-required stop path.
func TestApplyVerdict_StopWithErrorResultStillEscalates(t *testing.T) {
	tasks, tk := newTestTasks(t)

	stopped := ""
	w := &Watchdog{
		tasks:              tasks,
		logger:             slog.New(slog.DiscardHandler),
		stopAgent:          func(id string) error { stopped = id; return nil },
		stopCompletedAgent: func(string) error { t.Fatal("stopCompletedAgent should not be called for an error result"); return nil },
	}

	ag := &agent.Agent{ID: "a1", TaskID: tk.ID, Mode: "headless"}
	ag.AppendOutput(agent.StreamEvent{Type: "result", Subtype: "error_during_execution"})

	w.applyVerdict(ag, "budget", agent.InspectorVerdict{
		Stuck:          true,
		Reason:         "stuck after an error",
		Recommendation: "stop",
	})

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusHumanRequired)
	}
	if stopped != "a1" {
		t.Fatalf("stopAgent called with %q, want a1", stopped)
	}
}

func TestApplyVerdict_StallStopMarksRetryableHang(t *testing.T) {
	tasks, tk := newTestTasks(t)

	stopped := false
	w := &Watchdog{
		tasks:     tasks,
		logger:    slog.New(slog.DiscardHandler),
		stopAgent: func(string) error { stopped = true; return nil },
	}

	w.applyVerdict(&agent.Agent{ID: "a1", TaskID: tk.ID}, "stall", agent.InspectorVerdict{
		Stuck:          true,
		Reason:         "no stream activity",
		Recommendation: "stop",
	})

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusInProgress)
	}
	if got.StatusReason != "watchdog hang: no stream activity" {
		t.Fatalf("status_reason = %q, want retryable watchdog hang marker", got.StatusReason)
	}
	if !stopped {
		t.Fatal("stopAgent not called on stall stop verdict")
	}
}

// TestApplyVerdict_LoopStopWithGenericStallMarksRetryableHang covers #1456: a
// "stop" verdict on the "loop" trigger whose ReasonKind is "generic_stall" (a
// benign command-repetition flake, not reward-hacking) must route through the
// same retryable watchdog-hang path as a "stall" stop, not straight to
// human-required.
func TestApplyVerdict_LoopStopWithGenericStallMarksRetryableHang(t *testing.T) {
	tasks, tk := newTestTasks(t)

	stopped := false
	w := &Watchdog{
		tasks:     tasks,
		logger:    slog.New(slog.DiscardHandler),
		stopAgent: func(string) error { stopped = true; return nil },
	}

	w.applyVerdict(&agent.Agent{ID: "a1", TaskID: tk.ID}, "loop", agent.InspectorVerdict{
		Stuck:          true,
		Reason:         "repeating identical investigation commands",
		Recommendation: "stop",
		ReasonKind:     "generic_stall",
	})

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusInProgress)
	}
	if got.StatusReason != "watchdog hang: repeating identical investigation commands" {
		t.Fatalf("status_reason = %q, want retryable watchdog hang marker", got.StatusReason)
	}
	if !stopped {
		t.Fatal("stopAgent not called on generic_stall loop stop verdict")
	}
}

// TestApplyVerdict_LoopStopWithRewardHackingEscalates covers #1456: a "stop"
// verdict on the "loop" trigger whose ReasonKind is "reward_hacking" (a
// genuine stuck loop, not a benign flake) must still escalate straight to
// human-required, not take the retryable watchdog-hang path reserved for
// "generic_stall".
func TestApplyVerdict_LoopStopWithRewardHackingEscalates(t *testing.T) {
	tasks, tk := newTestTasks(t)

	stopped := false
	w := &Watchdog{
		tasks:     tasks,
		logger:    slog.New(slog.DiscardHandler),
		stopAgent: func(string) error { stopped = true; return nil },
	}

	w.applyVerdict(&agent.Agent{ID: "a1", TaskID: tk.ID}, "loop", agent.InspectorVerdict{
		Stuck:          true,
		Reason:         "repeating the same failing fix with fabricated progress",
		Recommendation: "stop",
		ReasonKind:     "reward_hacking",
	})

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusHumanRequired)
	}
	if got.StatusReason != "watchdog: repeating the same failing fix with fabricated progress" {
		t.Fatalf("status_reason = %q, want watchdog reason", got.StatusReason)
	}
	if !stopped {
		t.Fatal("stopAgent not called on reward_hacking loop stop verdict")
	}
}

// TestApplyVerdict_BudgetStopWithGenericStallMarksRetryableHang covers the
// gap left by #1456: a "stop" verdict on the "budget" trigger whose
// ReasonKind is "generic_stall" (e.g. an agent legitimately polling
// TaskOutput/ScheduleWakeup for a long-running backgrounded verify/test gate)
// must route through the same retryable watchdog-hang path as a "stall" stop
// or a "loop"+generic_stall stop, not straight to human-required.
func TestApplyVerdict_BudgetStopWithGenericStallMarksRetryableHang(t *testing.T) {
	tasks, tk := newTestTasks(t)

	stopped := false
	w := &Watchdog{
		tasks:     tasks,
		logger:    slog.New(slog.DiscardHandler),
		stopAgent: func(string) error { stopped = true; return nil },
	}

	w.applyVerdict(&agent.Agent{ID: "a1", TaskID: tk.ID}, "budget", agent.InspectorVerdict{
		Stuck:          true,
		Reason:         "polling for backgrounded verify command to complete",
		Recommendation: "stop",
		ReasonKind:     "generic_stall",
	})

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusInProgress)
	}
	if got.StatusReason != "watchdog hang: polling for backgrounded verify command to complete" {
		t.Fatalf("status_reason = %q, want retryable watchdog hang marker", got.StatusReason)
	}
	if !stopped {
		t.Fatal("stopAgent not called on generic_stall budget stop verdict")
	}
}

// TestApplyVerdict_BudgetStopWithoutGenericStallEscalates ensures a "budget"
// trigger stop whose ReasonKind is anything other than "generic_stall"
// (including empty, for older judges) still escalates straight to
// human-required — only the explicit generic_stall reason gets the retry.
func TestApplyVerdict_BudgetStopWithoutGenericStallEscalates(t *testing.T) {
	for _, kind := range []string{"", "reward_hacking"} {
		t.Run(kind, func(t *testing.T) {
			tasks, tk := newTestTasks(t)

			stopped := false
			w := &Watchdog{
				tasks:     tasks,
				logger:    slog.New(slog.DiscardHandler),
				stopAgent: func(string) error { stopped = true; return nil },
			}

			w.applyVerdict(&agent.Agent{ID: "a1", TaskID: tk.ID}, "budget", agent.InspectorVerdict{
				Stuck:          true,
				Reason:         "burned through budget with no forward progress",
				Recommendation: "stop",
				ReasonKind:     kind,
			})

			got, err := tasks.Get(tk.ID)
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != task.StatusHumanRequired {
				t.Fatalf("status = %q, want %q", got.Status, task.StatusHumanRequired)
			}
			if got.StatusReason != "watchdog: burned through budget with no forward progress" {
				t.Fatalf("status_reason = %q, want watchdog reason", got.StatusReason)
			}
			if !stopped {
				t.Fatal("stopAgent not called on budget stop verdict")
			}
		})
	}
}

// TestApplyVerdict_RateLimitStopReschedulesInsteadOfEscalating covers #1428:
// a "stop" verdict with ReasonKind "rate_limit" must route through the
// provider-health signal path and leave the task in-progress, regardless of
// whether the trigger was "loop" (ToolLoopStreak) or "stall" (no stream
// activity) — both are symptoms of the same underlying rate-limit exhaustion.
func TestApplyVerdict_RateLimitStopReschedulesInsteadOfEscalating(t *testing.T) {
	for _, trigger := range []string{"loop", "stall"} {
		t.Run(trigger, func(t *testing.T) {
			tasks, tk := newTestTasks(t)

			stopped := false
			var signaledName, signaledReason string
			var signaledKind provider.Signal
			w := &Watchdog{
				tasks:     tasks,
				logger:    slog.New(slog.DiscardHandler),
				stopAgent: func(string) error { stopped = true; return nil },
				recordProviderSignal: func(ag *agent.Agent, sig provider.Signal, reason string, _ time.Duration) {
					ag.SetError("rate_limit", reason)
					signaledName, signaledKind, signaledReason = ag.Provider, sig, reason
				},
			}

			ag := &agent.Agent{ID: "a1", TaskID: tk.ID, Provider: "claude"}
			w.applyVerdict(ag, trigger, agent.InspectorVerdict{
				Stuck:          true,
				Reason:         "org-level rate limit exhausted",
				Recommendation: "stop",
				ReasonKind:     "rate_limit",
			})

			got, err := tasks.Get(tk.ID)
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != task.StatusInProgress {
				t.Fatalf("status = %q, want %q (rate limit is recoverable, not human-required)", got.Status, task.StatusInProgress)
			}
			if !strings.Contains(got.StatusReason, "org-level rate limit exhausted") {
				t.Fatalf("status_reason = %q, want it to include the verdict reason", got.StatusReason)
			}
			if !stopped {
				t.Fatal("stopAgent not called on rate-limit stop verdict")
			}
			if ag.GetErrorKind() != "rate_limit" {
				t.Fatalf("agent error kind = %q, want %q (so the completion handler reschedules it)", ag.GetErrorKind(), "rate_limit")
			}
			if signaledName != "claude" || signaledKind != provider.SignalRateLimit {
				t.Fatalf("reportProviderSignal(name=%q, sig=%v), want (claude, SignalRateLimit)", signaledName, signaledKind)
			}
			if !strings.Contains(signaledReason, "org-level rate limit exhausted") {
				t.Fatalf("signaled reason = %q, want it to include the verdict reason", signaledReason)
			}
		})
	}
}

func TestApplyVerdict_RateLimitStopWithoutTaskStillSignalsAndStops(t *testing.T) {
	stopped := false
	var signaledName string
	var signaledKind provider.Signal
	w := &Watchdog{
		logger:    slog.New(slog.DiscardHandler),
		stopAgent: func(string) error { stopped = true; return nil },
		recordProviderSignal: func(ag *agent.Agent, sig provider.Signal, reason string, _ time.Duration) {
			ag.SetError("rate_limit", reason)
			signaledName, signaledKind = ag.Provider, sig
			if reason == "" {
				t.Fatal("reason should not be empty")
			}
		},
	}

	ag := &agent.Agent{ID: "a1", Provider: "claude"}
	w.applyVerdict(ag, "loop", agent.InspectorVerdict{
		Stuck:          true,
		Reason:         "org-level rate limit exhausted",
		Recommendation: "stop",
		ReasonKind:     "rate_limit",
	})

	if !stopped {
		t.Fatal("stopAgent not called on taskless rate-limit stop verdict")
	}
	if ag.GetErrorKind() != "rate_limit" {
		t.Fatalf("agent error kind = %q, want rate_limit", ag.GetErrorKind())
	}
	if signaledName != "claude" || signaledKind != provider.SignalRateLimit {
		t.Fatalf("recordProviderSignal(provider=%q, sig=%v), want (claude, SignalRateLimit)", signaledName, signaledKind)
	}
}

// TestHandleZeroOutputStall_SignalsProviderAndReschedulesInsteadOfEscalating
// covers #1913: a headless agent that never produced a single byte of
// output (NDJSON or stderr) since launch must be treated as a provider
// startup failure — provider-health signal + retryable in-progress status —
// not a generic watchdog hang that would exhaust its same-provider retry
// budget and strand the task (and its umbrella parent) in human-required.
func TestHandleZeroOutputStall_SignalsProviderAndReschedulesInsteadOfEscalating(t *testing.T) {
	tasks, tk := newTestTasks(t)

	stopped := false
	var signaledName, signaledReason string
	var signaledKind provider.Signal
	w := &Watchdog{
		tasks:     tasks,
		logger:    slog.New(slog.DiscardHandler),
		stopAgent: func(string) error { stopped = true; return nil },
		recordProviderSignal: func(ag *agent.Agent, sig provider.Signal, reason string, _ time.Duration) {
			ag.SetError("rate_limit", reason)
			signaledName, signaledKind, signaledReason = ag.Provider, sig, reason
		},
	}

	ag := &agent.Agent{ID: "a1", TaskID: tk.ID, Provider: "claude"}
	w.handleZeroOutputStall(ag, 20*time.Minute, 20*time.Minute)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want %q (zero output at startup is recoverable, not human-required)", got.Status, task.StatusInProgress)
	}
	if !strings.Contains(got.StatusReason, zeroOutputReason) {
		t.Fatalf("status_reason = %q, want it to include %q", got.StatusReason, zeroOutputReason)
	}
	if !stopped {
		t.Fatal("stopAgent not called on zero-output stall")
	}
	if ag.GetErrorKind() != "rate_limit" {
		t.Fatalf("agent error kind = %q, want %q (so the completion handler reschedules it)", ag.GetErrorKind(), "rate_limit")
	}
	if signaledName != "claude" || signaledKind != provider.SignalRateLimit {
		t.Fatalf("recordProviderSignal(provider=%q, sig=%v), want (claude, SignalRateLimit)", signaledName, signaledKind)
	}
	if signaledReason != zeroOutputReason {
		t.Fatalf("signaled reason = %q, want %q", signaledReason, zeroOutputReason)
	}
}

func TestHandleZeroOutputStall_NoTaskStillSignalsAndStops(t *testing.T) {
	stopped := false
	w := &Watchdog{
		logger:               slog.New(slog.DiscardHandler),
		stopAgent:            func(string) error { stopped = true; return nil },
		recordProviderSignal: func(*agent.Agent, provider.Signal, string, time.Duration) {},
	}

	w.handleZeroOutputStall(&agent.Agent{ID: "a1", Provider: "claude"}, time.Hour, time.Hour)

	if !stopped {
		t.Fatal("stopAgent not called on taskless zero-output stall")
	}
}

// TestInspectHeadless_ZeroOutputStallSkipsJudge covers #1913 end to end
// through the real trigger path: a "stall" trigger on an agent that never
// emitted an event must route straight to handleZeroOutputStall and must
// NOT invoke the (pointless, since the log is empty) LLM judge.
func TestInspectHeadless_ZeroOutputStallSkipsJudge(t *testing.T) {
	tasks, tk := newTestTasks(t)

	judgeCalled := false
	stopped := false
	var signaledReason string
	w := &Watchdog{
		tasks:  tasks,
		logger: slog.New(slog.DiscardHandler),
		emit:   func(string, any) {},
		inspectAgent: func(context.Context, *slog.Logger, agent.InspectInput) (agent.InspectorVerdict, error) {
			judgeCalled = true
			return agent.InspectorVerdict{Recommendation: "continue"}, nil
		},
		stopAgent: func(string) error { stopped = true; return nil },
		recordProviderSignal: func(ag *agent.Agent, sig provider.Signal, reason string, _ time.Duration) {
			ag.SetError("rate_limit", reason)
			signaledReason = reason
		},
	}

	started := time.Now().Add(-20 * time.Minute)
	ag := &agent.Agent{
		ID:          "a1",
		TaskID:      tk.ID,
		Provider:    "claude",
		Mode:        "headless",
		StartedAt:   started,
		LastEventAt: started,
		LogPath:     "/tmp/does-not-matter.ndjson",
	}

	w.inspectHeadless(context.Background(), newState(), time.Now(), ag)

	if judgeCalled {
		t.Fatal("LLM judge invoked for a zero-output stall; should be skipped")
	}
	if !stopped {
		t.Fatal("stopAgent not called on zero-output stall")
	}
	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusInProgress)
	}
	if signaledReason != zeroOutputReason {
		t.Fatalf("signaled reason = %q, want %q", signaledReason, zeroOutputReason)
	}
}

// TestInspectHeadless_MidRunStallStillUsesJudge ensures an agent that DID
// produce output before stalling keeps using the normal LLM-judge path — the
// zero-output fast path must not swallow a genuine mid-task hang.
func TestInspectHeadless_MidRunStallStillUsesJudge(t *testing.T) {
	tasks, tk := newTestTasks(t)

	judgeCalled := false
	w := &Watchdog{
		tasks:  tasks,
		logger: slog.New(slog.DiscardHandler),
		emit:   func(string, any) {},
		wg:     &sync.WaitGroup{},
		inspectAgent: func(context.Context, *slog.Logger, agent.InspectInput) (agent.InspectorVerdict, error) {
			judgeCalled = true
			return agent.InspectorVerdict{Recommendation: "continue"}, nil
		},
		stopAgent: func(string) error { return nil },
	}

	started := time.Now().Add(-20 * time.Minute)
	ag := &agent.Agent{
		ID:        "a1",
		TaskID:    tk.ID,
		Provider:  "claude",
		Mode:      "headless",
		StartedAt: started,
		LogPath:   "/tmp/does-not-matter.ndjson",
	}
	// A real mid-run stall has produced at least one stream event; append one,
	// then rewind LastEventAt into the stall window (AppendOutput stamps it to
	// wall-clock).
	ag.AppendOutput(agent.StreamEvent{Type: "assistant"})
	ag.SetLastEventAt(started.Add(5 * time.Minute))

	w.inspectHeadless(context.Background(), newState(), time.Now(), ag)
	w.wg.Wait()

	if !judgeCalled {
		t.Fatal("LLM judge not invoked for a genuine mid-run stall")
	}
}

// TestInspectHeadless_ReattachedSurvivorZeroOutputSkipsJudge covers the
// survive-restart gap: a detached headless agent that produced nothing before
// crossing an app restart is rebuilt by fromRecord with LastEventAt bumped to
// reattach wall-clock (no longer == StartedAt), yet its output buffer stays
// empty. It must still route to handleZeroOutputStall, not the (pointless,
// empty-log) LLM judge.
func TestInspectHeadless_ReattachedSurvivorZeroOutputSkipsJudge(t *testing.T) {
	tasks, tk := newTestTasks(t)

	judgeCalled := false
	stopped := false
	var signaledReason string
	w := &Watchdog{
		tasks:  tasks,
		logger: slog.New(slog.DiscardHandler),
		emit:   func(string, any) {},
		inspectAgent: func(context.Context, *slog.Logger, agent.InspectInput) (agent.InspectorVerdict, error) {
			judgeCalled = true
			return agent.InspectorVerdict{Recommendation: "continue"}, nil
		},
		stopAgent: func(string) error { stopped = true; return nil },
		recordProviderSignal: func(ag *agent.Agent, sig provider.Signal, reason string, _ time.Duration) {
			ag.SetError("rate_limit", reason)
			signaledReason = reason
		},
	}

	// Empty log, StartedAt well in the past, LastEventAt set to reattach time —
	// exactly the fromRecord skeleton for an empty-log survivor.
	started := time.Now().Add(-40 * time.Minute)
	ag := &agent.Agent{
		ID:          "a1",
		TaskID:      tk.ID,
		Provider:    "claude",
		Mode:        "headless",
		StartedAt:   started,
		LastEventAt: time.Now().Add(-20 * time.Minute),
		LogPath:     "/tmp/does-not-matter.ndjson",
	}

	w.inspectHeadless(context.Background(), newState(), time.Now(), ag)

	if judgeCalled {
		t.Fatal("LLM judge invoked for a reattached zero-output stall; should be skipped")
	}
	if !stopped {
		t.Fatal("stopAgent not called on reattached zero-output stall")
	}
	if signaledReason != zeroOutputReason {
		t.Fatalf("signaled reason = %q, want %q", signaledReason, zeroOutputReason)
	}
}

func TestDecideTrigger(t *testing.T) {
	const (
		sl     = 15 * time.Minute
		budget = 45 * time.Minute
		thresh = 6
	)
	tests := []struct {
		name              string
		streak, threshold int
		acked             bool
		stall, total      time.Duration
		want              string
	}{
		{"none", 1, thresh, false, time.Minute, time.Minute, ""},
		{"loop over threshold", thresh, thresh, false, time.Minute, time.Minute, "loop"},
		{"loop below threshold", thresh - 1, thresh, false, time.Minute, time.Minute, ""},
		{"loop disabled by zero threshold", 100, 0, false, time.Minute, time.Minute, ""},
		{"loop wins over stall", thresh, thresh, false, 30 * time.Minute, time.Minute, "loop"},
		{"acked loop suppressed, none left", thresh, thresh, true, time.Minute, time.Minute, ""},
		{"acked loop falls through to stall", thresh, thresh, true, 30 * time.Minute, time.Minute, "stall"},
		{"stall", 0, thresh, false, 20 * time.Minute, time.Minute, "stall"},
		{"budget", 0, thresh, false, time.Minute, time.Hour, "budget"},
		{"stall wins over budget", 0, thresh, false, 20 * time.Minute, time.Hour, "stall"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decideTrigger(tc.streak, tc.threshold, tc.acked, tc.stall, sl, tc.total, budget)
			if got != tc.want {
				t.Fatalf("decideTrigger = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCheckCompletedHang_StopsAfterGrace covers the watchdog's hard backstop
// for a headless agent whose stream ended in a clean terminal result but
// whose process never exited — no live tailer catches this outside the
// detached/reattached path, so the watchdog must force-stop it directly once
// it has sat idle past completedHangGrace.
func TestCheckCompletedHang_StopsAfterGrace(t *testing.T) {
	stopped := ""
	w := &Watchdog{
		logger:             slog.New(slog.DiscardHandler),
		stopCompletedAgent: func(id string) error { stopped = id; return nil },
	}

	ag := &agent.Agent{ID: "a1"}
	ag.AppendOutput(agent.StreamEvent{Type: "result", Content: "done"})
	ag.SetLastEventAt(time.Now().Add(-10 * time.Minute))

	w.checkCompletedHang(ag, time.Now())

	if stopped != "a1" {
		t.Fatalf("stopCompletedAgent called with %q, want a1", stopped)
	}
}

// TestCheckCompletedHang_WithinGraceLeavesAgentRunning ensures a completed
// agent still within the grace window is left alone — the runner's own
// post-result reaper gets first crack at it.
func TestCheckCompletedHang_WithinGraceLeavesAgentRunning(t *testing.T) {
	stopped := false
	w := &Watchdog{
		logger:             slog.New(slog.DiscardHandler),
		stopCompletedAgent: func(string) error { stopped = true; return nil },
	}

	ag := &agent.Agent{ID: "a1"}
	ag.AppendOutput(agent.StreamEvent{Type: "result", Content: "done"})
	ag.SetLastEventAt(time.Now())

	w.checkCompletedHang(ag, time.Now())

	if stopped {
		t.Fatal("stopCompletedAgent called within grace window, want no-op")
	}
}

// TestCheckCompletedHang_LiveBackgroundTaskExtendsGrace locks in the fix for
// task 3aeabb65: a completed agent with a still-live CLI
// `run_in_background` task (e.g. npm ci) must not be force-stopped at the
// base completedHangGrace merely because it produced no further NDJSON
// activity — killing it mid-write corrupted node_modules in the original
// incident.
func TestCheckCompletedHang_LiveBackgroundTaskExtendsGrace(t *testing.T) {
	stopped := false
	w := &Watchdog{
		logger:             slog.New(slog.DiscardHandler),
		stopCompletedAgent: func(string) error { stopped = true; return nil },
	}

	ag := &agent.Agent{ID: "a1"}
	ag.AppendOutput(agent.StreamEvent{Type: "result", Content: "done"})
	ag.SetLastEventAt(time.Now().Add(-10 * time.Minute))
	ag.SetBackgroundTaskIDs([]string{"bpzdm25og"})

	w.checkCompletedHang(ag, time.Now())

	if stopped {
		t.Fatal("stopCompletedAgent called while a background task is still live, want no-op")
	}
}

func TestReapIdleInteractive_ReleasesHumanRequiredTaskAgent(t *testing.T) {
	tasks, tk := newTestTasks(t)
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
		t.Fatalf("set human-required: %v", err)
	}

	stopped := ""
	w := &Watchdog{
		tasks:     tasks,
		logger:    slog.New(slog.DiscardHandler),
		stopAgent: func(id string) error { stopped = id; return nil },
	}

	ag := &agent.Agent{ID: "a1", TaskID: tk.ID, Mode: "interactive", StartedAt: time.Now(), LastEventAt: time.Now()}
	w.reapIdleInteractive(ag, time.Now())

	if stopped != "a1" {
		t.Fatalf("stopAgent called with %q, want a1", stopped)
	}
}

func TestReapIdleInteractive_HardStopsHungTaskAgent(t *testing.T) {
	tasks, tk := newTestTasks(t)

	stopped := ""
	w := &Watchdog{
		tasks:     tasks,
		logger:    slog.New(slog.DiscardHandler),
		stopAgent: func(id string) error { stopped = id; return nil },
	}

	now := time.Now()
	ag := &agent.Agent{ID: "a1", TaskID: tk.ID, Mode: "interactive", StartedAt: now.Add(-time.Hour), LastEventAt: now.Add(-40 * time.Minute)}
	w.reapIdleInteractive(ag, now)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.StatusReason != "watchdog hang: idle deadline exceeded" {
		t.Fatalf("status_reason = %q, want watchdog idle deadline", got.StatusReason)
	}
	if stopped != "a1" {
		t.Fatalf("stopAgent called with %q, want a1", stopped)
	}
}

// TestApplyVerdict_NudgeLiveTransportDeliversInPlace covers the live-transport
// path: an agent with a working SendPromptToAgent (interactive/conversational)
// is steered in place and left running — no stop, no persisted steer.
func TestApplyVerdict_NudgeLiveTransportDeliversInPlace(t *testing.T) {
	tasks, tk := newTestTasks(t)

	var nudged string
	stopped := false
	w := &Watchdog{
		tasks:      tasks,
		logger:     slog.New(slog.DiscardHandler),
		stopAgent:  func(string) error { stopped = true; return nil },
		nudgeAgent: func(_, text string) error { nudged = text; return nil },
	}

	w.applyVerdict(&agent.Agent{ID: "a1", TaskID: tk.ID}, "loop", agent.InspectorVerdict{
		Recommendation: "nudge",
		Reason:         "drifting",
		Nudge:          "fix the root cause first",
	})

	if nudged != "⚠️ Supervisor: fix the root cause first" {
		t.Fatalf("nudge message = %q", nudged)
	}
	if stopped {
		t.Fatal("stopAgent called on a live-transport nudge")
	}
	got, _ := tasks.Get(tk.ID)
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want in-progress (nudge must not flip status)", got.Status)
	}
	if got.SupervisorSteer != "" {
		t.Fatalf("supervisor_steer = %q, want empty for a live nudge", got.SupervisorSteer)
	}
}

// TestApplyVerdict_NudgeHeadlessPersistsSteerAndStops covers the headless path:
// no live transport, so the steer is persisted on the task and the agent is
// stopped so the recovery loop re-dispatches with the correction. The task is
// left in-progress (not human-required) so recovery resumes it.
func TestApplyVerdict_NudgeHeadlessPersistsSteerAndStops(t *testing.T) {
	tasks, tk := newTestTasks(t)

	stopped := false
	w := &Watchdog{
		tasks:      tasks,
		logger:     slog.New(slog.DiscardHandler),
		stopAgent:  func(string) error { stopped = true; return nil },
		nudgeAgent: func(_, _ string) error { return errors.New("no active transport") },
	}

	w.applyVerdict(&agent.Agent{ID: "a1", TaskID: tk.ID}, "loop", agent.InspectorVerdict{
		Recommendation: "nudge",
		Reason:         "drifting",
		Nudge:          "stop retrying the failing command",
	})

	if !stopped {
		t.Fatal("headless nudge must stop the agent so recovery re-dispatches")
	}
	got, _ := tasks.Get(tk.ID)
	if got.SupervisorSteer != "stop retrying the failing command" {
		t.Fatalf("supervisor_steer = %q, want the steer persisted", got.SupervisorSteer)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want in-progress (recovery must resume, not park)", got.Status)
	}
}

func TestApplyVerdict_NudgeWithoutExplicitSteerUsesLoopEvidence(t *testing.T) {
	tasks, tk := newTestTasks(t)

	stopped := false
	w := &Watchdog{
		tasks:      tasks,
		logger:     slog.New(slog.DiscardHandler),
		stopAgent:  func(string) error { stopped = true; return nil },
		nudgeAgent: func(_, _ string) error { return errors.New("no active transport") },
	}

	ag := &agent.Agent{ID: "a1", TaskID: tk.ID}
	ag.NoteToolAction("check:mise run verify", "check:mise run verify")
	ag.NoteToolAction("read:verify.log", "read:verify.log")
	ag.NoteToolAction("check:mise run verify", "check:mise run verify")
	ag.NoteToolAction("read:verify.log", "read:verify.log")

	w.applyVerdict(ag, "loop", agent.InspectorVerdict{
		Recommendation: "nudge",
	})

	if !stopped {
		t.Fatal("headless nudge must stop the agent so recovery re-dispatches")
	}
	got, _ := tasks.Get(tk.ID)
	if !strings.Contains(got.SupervisorSteer, "check:mise run verify") {
		t.Fatalf("supervisor_steer = %q, want focused steer naming the repeated family", got.SupervisorSteer)
	}
	if !strings.Contains(got.SupervisorSteer, "inspect the latest error/output") {
		t.Fatalf("supervisor_steer = %q, want focused next action", got.SupervisorSteer)
	}
}

func TestInspect_LoopAckOnlyWhenLeftRunning(t *testing.T) {
	tests := []struct {
		name    string
		verdict string
		wantAck bool
	}{
		{"continue acks the loop", "continue", true},
		{"escalate acks the loop", "escalate", true},
		{"stop does not ack (stop may fail, keep inspecting)", "stop", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tasks, tk := newTestTasks(t)
			w := &Watchdog{
				tasks:     tasks,
				logger:    slog.New(slog.DiscardHandler),
				emit:      func(string, any) {},
				stopAgent: func(string) error { return nil },
				inspectAgent: func(context.Context, *slog.Logger, agent.InspectInput) (agent.InspectorVerdict, error) {
					return agent.InspectorVerdict{Recommendation: tc.verdict, Reason: "x"}, nil
				},
			}
			ag := &agent.Agent{ID: "a1", TaskID: tk.ID}
			ag.NoteToolSignature("sig") // arm a loop signature

			w.inspect(context.Background(), ag, tk, "loop", 1, 1)

			if got := ag.ToolLoopAcknowledged(); got != tc.wantAck {
				t.Fatalf("ToolLoopAcknowledged after %q = %v, want %v", tc.verdict, got, tc.wantAck)
			}
		})
	}
}

func TestHardDeadlineBreach(t *testing.T) {
	const (
		sl     = 15 * time.Minute
		budget = 45 * time.Minute
	)
	tests := []struct {
		name         string
		stall, total time.Duration
		background   bool
		want         string
	}{
		{"within bounds", 5 * time.Minute, 10 * time.Minute, false, ""},
		{"idle over ceiling", 40 * time.Minute, 10 * time.Minute, false, "idle"},
		{"wall clock over ceiling", 5 * time.Minute, 2 * time.Hour, false, "wall_clock"},
		{"idle within background grace", 40 * time.Minute, 10 * time.Minute, true, ""},
		{"idle over even with background grace", 60 * time.Minute, 10 * time.Minute, true, "idle"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ag := &agent.Agent{}
			if tc.background {
				ag.SetBackgroundTaskIDs([]string{"bg1"})
			}
			got := hardDeadlineBreach(ag, tc.stall, tc.total, sl, budget)
			if got != tc.want {
				t.Fatalf("hardDeadlineBreach = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHardStop_MarksRetryableHangAndStops(t *testing.T) {
	tests := []struct {
		name       string
		reason     string
		wantReason string
	}{
		{"idle", "idle", "watchdog hang: idle deadline exceeded"},
		{"wall_clock", "wall_clock", "watchdog hang: wall_clock deadline exceeded"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tasks, tk := newTestTasks(t)
			stopped := ""
			w := &Watchdog{
				tasks:     tasks,
				logger:    slog.New(slog.DiscardHandler),
				stopAgent: func(id string) error { stopped = id; return nil },
			}

			w.hardStop(&agent.Agent{ID: "a1", TaskID: tk.ID}, tc.reason, 40*time.Minute, time.Hour)

			got, err := tasks.Get(tk.ID)
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			if got.Status != task.StatusInProgress {
				t.Fatalf("status = %q, want %q", got.Status, task.StatusInProgress)
			}
			if got.StatusReason != tc.wantReason {
				t.Fatalf("status_reason = %q, want %q", got.StatusReason, tc.wantReason)
			}
			if stopped != "a1" {
				t.Fatalf("stopAgent called with %q, want a1", stopped)
			}
		})
	}
}

func TestHardStop_NoTaskStillFreesSlot(t *testing.T) {
	stopped := ""
	w := &Watchdog{
		logger:    slog.New(slog.DiscardHandler),
		stopAgent: func(id string) error { stopped = id; return nil },
	}

	w.hardStop(&agent.Agent{ID: "a1"}, "wall_clock", 0, 10*time.Hour)

	if stopped != "a1" {
		t.Fatalf("stopAgent called with %q, want a1", stopped)
	}
}

func newTestTasks(t *testing.T) (*task.Manager, task.Task) {
	t.Helper()

	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	tasks := task.NewManager(store, nil)
	tk, err := tasks.Create("watchdog test", "", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusInProgress)})
	if err != nil {
		t.Fatalf("set in-progress: %v", err)
	}
	return tasks, tk
}
