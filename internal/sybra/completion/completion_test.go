package completion

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/skillattr"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/watchdogreason"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

// sigkillErr SIGKILLs a live process and returns its Wait error, so tests can
// exercise the real signal-kill shape (WaitStatus.Signaled) rather than a
// hand-made error that isSignalKill would classify differently.
func sigkillErr(t *testing.T) error {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
		panic("unreachable")
	}
	time.Sleep(20 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("signal: %v", err)
		panic("unreachable")
	}
	return cmd.Wait()
}

type recordingCompletionWorkflow struct {
	completed chan workflow.AgentCompletion
}

func (w *recordingCompletionWorkflow) HandleAgentComplete(_ string, c workflow.AgentCompletion) {
	w.completed <- c
}

func (w *recordingCompletionWorkflow) ClearAgentStep(_, _ string) {}

func (w *recordingCompletionWorkflow) RescheduleInterruptedAgent(_, _ string) {}

func (w *recordingCompletionWorkflow) RescheduleRateLimitedAgent(_, _ string) {}

func (w *recordingCompletionWorkflow) RescheduleCheckpointedAgent(_, _ string) {}

func (w *recordingCompletionWorkflow) ReschedulePromptUndeliveredAgent(_, _ string) {}

func (w *recordingCompletionWorkflow) DispatchEvent(_, _ string, _, _ map[string]string) (string, error) {
	return "", nil
}

func TestRunOutcome(t *testing.T) {
	t.Parallel()
	errBoom := errors.New("boom")

	cases := []struct {
		name          string
		role          agent.Role
		agent         func() *agent.Agent
		exitErr       error
		resultContent string
		want          string
	}{
		{name: "clean_exit_any_role", role: agent.RoleImplementation, want: "completed"},
		{name: "non_test_runner_errors_are_failed", role: agent.RoleImplementation, exitErr: errBoom, resultContent: "TEST_VERDICT: PASS", want: "failed"},
		{
			name:          "test_runner_genuine_pass_survives_trailing_process_error",
			role:          agent.RoleTestRunner,
			exitErr:       errBoom,
			resultContent: "ran the app end to end\nTEST_VERDICT: PASS",
			want:          "completed",
		},
		{
			// A FAIL verdict means the test-runner did its job (proved a real
			// defect), so it's still "completed" for stats purposes — the
			// implementation's failure is tracked separately via the task's
			// TestOutcome/route_test_result, not this agent-run stat.
			name:          "test_runner_genuine_fail_still_completed_the_protocol",
			role:          agent.RoleTestRunner,
			exitErr:       errBoom,
			resultContent: "found a defect\nTEST_VERDICT: FAIL",
			want:          "completed",
		},
		{
			name:          "test_runner_no_verdict_is_a_real_failure",
			role:          agent.RoleTestRunner,
			exitErr:       errBoom,
			resultContent: "crashed before concluding anything",
			want:          "failed",
		},
		{
			// A process killed by an external shutdown signal is not a resolved
			// run, but we now preserve that sharper cause instead of flattening
			// it into the generic stalled bucket.
			name:    "signal_killed_run_is_cancelled_shutdown_not_failure",
			role:    agent.RoleImplementation,
			exitErr: sigkillErr(t),
			want:    "cancelled_shutdown",
		},
		{
			name: "stopped_before_result_is_a_stall",
			role: agent.RoleImplementation,
			agent: func() *agent.Agent {
				ag := &agent.Agent{}
				ag.MarkStopped()
				return ag
			},
			want: "stalled",
		},
		{
			name: "rate_limited_run_is_a_stall",
			role: agent.RoleImplementation,
			agent: func() *agent.Agent {
				ag := &agent.Agent{}
				ag.SetError("rate_limit", "rate_limited")
				return ag
			},
			exitErr: errBoom,
			want:    "stalled",
		},
		{
			name: "malformed_tool_call_is_a_stall",
			role: agent.RoleImplementation,
			agent: func() *agent.Agent {
				ag := &agent.Agent{}
				ag.SetError("malformed_tool_call", "tool rejected malformed input")
				return ag
			},
			want: "stalled",
		},
		{
			name: "tool_use_aborted_is_a_stall",
			role: agent.RoleImplementation,
			agent: func() *agent.Agent {
				ag := &agent.Agent{}
				ag.SetError(agent.ErrorKindToolUseAborted, "provider run aborted after tool use was rejected")
				return ag
			},
			exitErr: errBoom,
			want:    "stalled",
		},
		{
			name: "user_interrupted_is_a_stall",
			role: agent.RoleTestRunner,
			agent: func() *agent.Agent {
				ag := &agent.Agent{}
				ag.SetError(agent.ErrorKindUserInterrupted, "provider run was interrupted by the user before completion")
				return ag
			},
			exitErr: errBoom,
			want:    "stalled",
		},
		{
			// A budget breach is a deliberate hard-stop, not an infra stall —
			// it must stay countable as a real failure (classifyStall).
			name: "cost_guardrail_stop_stays_a_failure",
			role: agent.RoleImplementation,
			agent: func() *agent.Agent {
				ag := &agent.Agent{}
				ag.MarkStopped()
				ag.SetEscalationReason(agent.EscalationReasonCost)
				return ag
			},
			exitErr: errBoom,
			want:    "failed",
		},
		{
			// A runaway forked-subagent fan-out is hard-stopped outright, not an
			// infra stall — it must flow through the bounded failed-completion
			// path, not ResumeStalled's silent re-dispatch.
			name: "subagent_turns_guardrail_stop_stays_a_failure",
			role: agent.RoleImplementation,
			agent: func() *agent.Agent {
				ag := &agent.Agent{}
				ag.MarkStopped()
				ag.SetEscalationReason(agent.EscalationReasonSubagentTurns)
				return ag
			},
			exitErr: errBoom,
			want:    "failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ag := &agent.Agent{}
			if tc.agent != nil {
				ag = tc.agent()
			}
			if got := (&Handler{}).runOutcome(ag, tc.role, tc.exitErr, tc.resultContent); got != tc.want {
				t.Errorf("runOutcome(%v, %v, %q) = %q, want %q", tc.role, tc.exitErr, tc.resultContent, got, tc.want)
			}
		})
	}
}

func TestIsRateLimitedRun(t *testing.T) {
	t.Parallel()
	rateLimited := &agent.Agent{}
	rateLimited.SetError("rate_limit", "rate_limited")
	authFailed := &agent.Agent{}
	authFailed.SetError("auth", "logged_out")

	cases := []struct {
		name    string
		ag      *agent.Agent
		exitErr error
		want    bool
	}{
		{"rate limit with exit error", rateLimited, errors.New("exit status 1"), true},
		{"rate limit but clean exit", rateLimited, nil, true},
		{"auth failure is not retried", authFailed, errors.New("exit status 1"), false},
		{"plain crash", &agent.Agent{}, errors.New("exit status 1"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRateLimitedRun(tc.ag, tc.exitErr); got != tc.want {
				t.Errorf("isRateLimitedRun = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildRunPatchIncludesSkillAttributionMetadata(t *testing.T) {
	t.Parallel()

	ag := &agent.Agent{
		ID:                      "ag-1",
		TaskID:                  "task-1",
		Name:                    agent.RoleImplementation.AgentName("Impl"),
		Model:                   "gpt-5",
		Provider:                "codex",
		LogPath:                 "/tmp/agent.log",
		RequestedSkill:          "sybra-test",
		SkillExecutionMode:      skillattr.ExecutionModeInjected,
		ResolvedSkillSourceHash: "deadbeefcafebabe",
		SkillConformance:        skillattr.ConformanceExact,
	}
	ag.SetSessionID("sess-1")
	ag.NoteSubagentCall("tool-1")
	ag.NoteSubagentCall("tool-1")
	ag.NoteSubagentCall("tool-2")

	resultWithReceipt := "done\n" + skillattr.ReceiptMarker("sybra-test", "deadbeefcafebabe")
	patch := (&Handler{}).buildRunPatch(ag, agent.StateStopped, 1.25, 0, resultWithReceipt, nil)

	if patch.RequestedSkill == nil || *patch.RequestedSkill != "sybra-test" {
		t.Fatalf("RequestedSkill = %v, want sybra-test", patch.RequestedSkill)
		panic("unreachable")
	}
	if patch.SkillExecutionMode == nil || *patch.SkillExecutionMode != skillattr.ExecutionModeInjected {
		t.Fatalf("SkillExecutionMode = %v, want %q", patch.SkillExecutionMode, skillattr.ExecutionModeInjected)
		panic("unreachable")
	}
	if patch.ResolvedSkillSourceHash == nil || *patch.ResolvedSkillSourceHash != "deadbeefcafebabe" {
		t.Fatalf("ResolvedSkillSourceHash = %v, want deadbeefcafebabe", patch.ResolvedSkillSourceHash)
		panic("unreachable")
	}
	if patch.SkillConformance == nil || *patch.SkillConformance != skillattr.ConformanceExact {
		t.Fatalf("SkillConformance = %v, want %q", patch.SkillConformance, skillattr.ConformanceExact)
		panic("unreachable")
	}
	if patch.SubagentCallCount == nil || *patch.SubagentCallCount != 2 {
		t.Fatalf("SubagentCallCount = %v, want 2 distinct parents", patch.SubagentCallCount)
		panic("unreachable")
	}
}

// TestBuildRunPatchMarksResumeZeroOutputStall covers the circuit-breaker
// marker (see agentorch.PickImplementationResumeSession): buildRunPatch must
// set ResumeZeroOutputStall only for the specific errorKind/errorMsg pair the
// watchdog's zero-output-stall path records, never for a generic rate limit
// or any other terminal state.
func TestBuildRunPatchMarksResumeZeroOutputStall(t *testing.T) {
	t.Parallel()

	t.Run("zero-output stall sets the marker", func(t *testing.T) {
		t.Parallel()
		ag := &agent.Agent{ID: "ag-1", TaskID: "task-1"}
		ag.SetError(agent.ErrorKindSilentHang, watchdogreason.ZeroOutputBeforeStartup)

		patch := (&Handler{}).buildRunPatch(ag, agent.StateStopped, 0, 0, "", nil)

		if patch.ResumeZeroOutputStall == nil || !*patch.ResumeZeroOutputStall {
			t.Fatalf("ResumeZeroOutputStall = %v, want true", patch.ResumeZeroOutputStall)
			panic("unreachable")
		}
	})

	t.Run("legacy rate_limit kind from an in-flight run still sets the marker", func(t *testing.T) {
		t.Parallel()
		ag := &agent.Agent{ID: "ag-1b", TaskID: "task-1"}
		ag.SetError("rate_limit", watchdogreason.ZeroOutputBeforeStartup)

		patch := (&Handler{}).buildRunPatch(ag, agent.StateStopped, 0, 0, "", nil)

		if patch.ResumeZeroOutputStall == nil || !*patch.ResumeZeroOutputStall {
			t.Fatalf("ResumeZeroOutputStall = %v, want true", patch.ResumeZeroOutputStall)
			panic("unreachable")
		}
	})

	t.Run("generic rate limit does not set the marker", func(t *testing.T) {
		t.Parallel()
		ag := &agent.Agent{ID: "ag-2", TaskID: "task-1"}
		ag.SetError("rate_limit", "rate_limited")

		patch := (&Handler{}).buildRunPatch(ag, agent.StateStopped, 0, 0, "", nil)

		if patch.ResumeZeroOutputStall != nil {
			t.Fatalf("ResumeZeroOutputStall = %v, want nil", patch.ResumeZeroOutputStall)
			panic("unreachable")
		}
	})

	t.Run("wrapped zero-output reason (task status_reason form) does not set the marker", func(t *testing.T) {
		t.Parallel()
		ag := &agent.Agent{ID: "ag-3", TaskID: "task-1"}
		ag.SetError("rate_limit", watchdogreason.RateLimit(watchdogreason.ZeroOutputBeforeStartup))

		patch := (&Handler{}).buildRunPatch(ag, agent.StateStopped, 0, 0, "", nil)

		if patch.ResumeZeroOutputStall != nil {
			t.Fatalf("ResumeZeroOutputStall = %v, want nil", patch.ResumeZeroOutputStall)
			panic("unreachable")
		}
	})

	t.Run("zero-output message without rate_limit kind does not set the marker", func(t *testing.T) {
		t.Parallel()
		ag := &agent.Agent{ID: "ag-4", TaskID: "task-1"}
		ag.SetError("crash", watchdogreason.ZeroOutputBeforeStartup)

		patch := (&Handler{}).buildRunPatch(ag, agent.StateStopped, 0, 0, "", nil)

		if patch.ResumeZeroOutputStall != nil {
			t.Fatalf("ResumeZeroOutputStall = %v, want nil", patch.ResumeZeroOutputStall)
			panic("unreachable")
		}
	})
}

// TestOnComplete_SilentHangReschedulesInsteadOfClearing pins the routing the
// whole reschedule contract rests on. The disposition alone proves nothing:
// a silent hang that reaches the default branch still reads as "stalled", it
// just quietly loses its same-tick re-dispatch and waits for a later sweep.
func TestOnComplete_SilentHangReschedulesInsteadOfClearing(t *testing.T) {
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	tasks := task.NewManager(store, nil)
	logger := discardLogger()
	wm := worktree.New(worktree.Config{WorktreesDir: t.TempDir(), Tasks: tasks, Logger: logger})
	wf := &recordingWorkflow{}

	created, err := tasks.CreateWithStatus("hung task", "body", "headless", task.StatusInProgress, task.Update{})
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if err := tasks.AddRun(created.ID, task.AgentRun{AgentID: "ag-1", Role: "implementation", Mode: "headless"}); err != nil {
		t.Fatal(err)
	}

	ag := &agent.Agent{ID: "ag-1", TaskID: created.ID, Mode: "headless", Provider: "claude"}
	ag.SetError(agent.ErrorKindSilentHang, watchdogreason.ZeroOutputBeforeStartup)
	ag.MarkStopped()

	h := New(Config{Logger: logger, Tasks: tasks, Worktrees: wm, WorkflowEngine: wf})
	h.OnComplete(ag)

	if len(wf.rateLimited) != 1 || wf.rateLimited[0] != "ag-1" {
		t.Fatalf("RescheduleRateLimitedAgent calls = %v, want [ag-1] (a silent hang must re-drive its step now)", wf.rateLimited)
	}
	if len(wf.completed) != 0 {
		t.Fatalf("HandleAgentComplete called %d times, want 0 (a silent run carries no verdict)", len(wf.completed))
	}
	if len(wf.cleared) != 0 {
		t.Fatalf("ClearAgentStep called %d times, want 0 (clearing drops the immediate re-dispatch)", len(wf.cleared))
	}
}

// TestBuildRunPatchDowngradesConformanceWhenReceiptMissing covers #2009: a
// process can exit cleanly and produce a plausible result without the
// mandatory workflow skill's transcript ever proving it was followed. The
// pre-execution ConformanceExact/ConformanceFallback classification (set by
// resolveWorkflowSkillPrompt before the agent ran) must be downgraded to
// ConformanceUnverified when the result carries no matching receipt, so a
// fake/incomplete artifact can never be recorded as a first-pass conformant
// run.
func TestBuildRunPatchDowngradesConformanceWhenReceiptMissing(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		conformance string
	}{
		{"exact_without_receipt", skillattr.ConformanceExact},
		{"fallback_without_receipt", skillattr.ConformanceFallback},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ag := &agent.Agent{
				ID:                      "ag-1",
				TaskID:                  "task-1",
				Name:                    agent.RoleImplementation.AgentName("Impl"),
				RequestedSkill:          "sybra-test",
				ResolvedSkillSourceHash: "deadbeefcafebabe",
				SkillConformance:        tc.conformance,
			}
			patch := (&Handler{}).buildRunPatch(ag, agent.StateStopped, 0, 0, "looks done, no receipt here", nil)
			if patch.SkillConformance == nil || *patch.SkillConformance != skillattr.ConformanceUnverified {
				t.Fatalf("SkillConformance = %v, want %q", patch.SkillConformance, skillattr.ConformanceUnverified)
				panic("unreachable")
			}
		})
	}
}

// TestBuildRunPatchSkipsReceiptDowngradeUnderOutputSchema is the regression
// guard for #2235: a step enforcing OutputSchema constrains the agent's
// final response to a structured payload with no room for a trailing
// receipt line, so resolveWorkflowSkillPrompt never asks for one there. A
// result with no receipt marker must not be downgraded to
// ConformanceUnverified in that case — the schema enforcement itself stands
// in as the conformance signal. Provider is pinned to "claude" explicitly
// (rather than left empty) so this asserts the real provider-capability
// gate rather than incidentally passing via lookupProvider's unrelated
// empty-defaults-to-claude fallback — see
// TestBuildRunPatchStillDowngradesWhenProviderIgnoresOutputSchema for the
// sibling case that would catch a provider actually ignoring the schema.
func TestBuildRunPatchSkipsReceiptDowngradeUnderOutputSchema(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		conformance string
	}{
		{"exact_without_receipt", skillattr.ConformanceExact},
		{"fallback_without_receipt", skillattr.ConformanceFallback},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ag := &agent.Agent{
				ID:                      "ag-1",
				TaskID:                  "task-1",
				Name:                    agent.RoleTestRunner.AgentName("Test"),
				Provider:                "claude",
				RequestedSkill:          "sybra-test",
				ResolvedSkillSourceHash: "deadbeefcafebabe",
				SkillConformance:        tc.conformance,
				OutputSchema:            `{"type":"object","properties":{"verdict":{"type":"string"}}}`,
			}
			patch := (&Handler{}).buildRunPatch(ag, agent.StateStopped, 0, 0, `{"verdict":"PASS"}`, nil)
			if patch.SkillConformance == nil || *patch.SkillConformance != tc.conformance {
				t.Fatalf("SkillConformance = %v, want unchanged %q", patch.SkillConformance, tc.conformance)
				panic("unreachable")
			}
		})
	}
}

// TestBuildRunPatchStillDowngradesWhenProviderIgnoresOutputSchema guards the
// adversarial-review follow-up to #2235: copilot never applies
// RunConfig.OutputSchema, so a run routed to it under cross-provider
// failover still got the receipt instruction (resolveWorkflowSkillPrompt)
// and must still be verified here. Gating on OutputSchema's mere presence
// instead of the real provider would wrongly skip the downgrade and record a
// copilot run that ignored the skill as falsely conformant.
func TestBuildRunPatchStillDowngradesWhenProviderIgnoresOutputSchema(t *testing.T) {
	t.Parallel()

	ag := &agent.Agent{
		ID:                      "ag-1",
		TaskID:                  "task-1",
		Name:                    agent.RoleTestRunner.AgentName("Test"),
		Provider:                "copilot",
		RequestedSkill:          "sybra-test",
		ResolvedSkillSourceHash: "deadbeefcafebabe",
		SkillConformance:        skillattr.ConformanceFallback,
		OutputSchema:            `{"type":"object","properties":{"verdict":{"type":"string"}}}`,
	}
	patch := (&Handler{}).buildRunPatch(ag, agent.StateStopped, 0, 0, "TEST_VERDICT: PASS", nil)
	if patch.SkillConformance == nil || *patch.SkillConformance != skillattr.ConformanceUnverified {
		t.Fatalf("SkillConformance = %v, want %q", patch.SkillConformance, skillattr.ConformanceUnverified)
		panic("unreachable")
	}
}

func TestBuildRunPatchMarksVerifiedRecoveryAsRecovered(t *testing.T) {
	t.Parallel()

	ag := &agent.Agent{
		ID:                      "ag-1",
		TaskID:                  "task-1",
		Name:                    agent.RoleImplementation.AgentName("Impl"),
		RequestedSkill:          "sybra-test",
		ResolvedSkillSourceHash: "deadbeefcafebabe",
		SkillConformance:        skillattr.ConformanceExact,
	}
	ag.SetSkillRecoveryAttempt(true)

	result := "done\n" + skillattr.ReceiptMarker("sybra-test", "deadbeefcafebabe")
	patch := (&Handler{}).buildRunPatch(ag, agent.StateStopped, 0, 0, result, nil)
	if patch.SkillConformance == nil || *patch.SkillConformance != skillattr.ConformanceRecovered {
		t.Fatalf("SkillConformance = %v, want %q", patch.SkillConformance, skillattr.ConformanceRecovered)
		panic("unreachable")
	}
}

func TestBuildRunPatchFindsReceiptInEarlierAssistantMessage(t *testing.T) {
	t.Parallel()

	ag := &agent.Agent{
		ID:                      "ag-1",
		TaskID:                  "task-1",
		Name:                    agent.RoleTestRunner.AgentName("Test"),
		RequestedSkill:          "sybra-test",
		ResolvedSkillSourceHash: "deadbeefcafebabe",
		SkillConformance:        skillattr.ConformanceExact,
	}
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: "Followed the skill.\n" + skillattr.ReceiptMarker("sybra-test", "deadbeefcafebabe"),
	})
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: `{"verdict":"PASS","outcome":"pass"}`,
	})
	ag.AppendOutput(agent.StreamEvent{Type: "result", Content: ""})

	patch := (&Handler{}).buildRunPatch(ag, agent.StateStopped, 0, 0, `{"verdict":"PASS","outcome":"pass"}`, nil)
	if patch.SkillConformance == nil || *patch.SkillConformance != skillattr.ConformanceExact {
		t.Fatalf("SkillConformance = %v, want %q", patch.SkillConformance, skillattr.ConformanceExact)
		panic("unreachable")
	}
}

func TestIsSignalKill(t *testing.T) {
	t.Parallel()

	// Helper: run "sh -c 'exit N'" and assert the error has the expected code.
	exitErr := func(t *testing.T, code int) error {
		t.Helper()
		err := exec.Command("sh", "-c", "exit "+itoa(code)).Run()
		if err == nil {
			t.Fatalf("expected non-nil error for exit %d", code)
			panic("unreachable")
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("expected *exec.ExitError for exit %d, got %T: %v", code, err, err)
		}
		if ee.ExitCode() != code {
			t.Fatalf("exit code mismatch: got %d want %d", ee.ExitCode(), code)
		}
		return err
	}

	tests := []struct {
		name string
		err  func(t *testing.T) error
		want bool
	}{
		{
			name: "nil",
			err:  func(*testing.T) error { return nil },
			want: false,
		},
		{
			name: "non-ExitError",
			err:  func(*testing.T) error { return errors.New("boom") },
			want: false,
		},
		{
			name: "exit 1 (genuine failure)",
			err: func(t *testing.T) error {
				t.Helper()
				return exitErr(t, 1)
			},
			want: false,
		},
		{
			name: "exit 2 (normal failure, not a signal code)",
			err: func(t *testing.T) error {
				t.Helper()
				return exitErr(t, 2)
			},
			want: false,
		},
		{
			name: "exit 130 (128+SIGINT)",
			err: func(t *testing.T) error {
				t.Helper()
				return exitErr(t, 130)
			},
			want: true,
		},
		{
			name: "exit 143 (128+SIGTERM)",
			err: func(t *testing.T) error {
				t.Helper()
				return exitErr(t, 143)
			},
			want: true,
		},
		{
			name: "exit 137 (128+SIGKILL)",
			err: func(t *testing.T) error {
				t.Helper()
				return exitErr(t, 137)
			},
			want: true,
		},
		{
			name: "truly signaled process (ws.Signaled==true)",
			err:  sigkillErr,
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.err(t)
			got := isSignalKill(err)
			if got != tc.want {
				t.Errorf("isSignalKill(%v) = %v, want %v", err, got, tc.want)
			}
		})
	}
}

func TestClassifyStall_CheckpointDisposition(t *testing.T) {
	t.Parallel()

	t.Run("checkpoint is a retryable stall", func(t *testing.T) {
		ag := &agent.Agent{}
		ag.MarkStopped()
		ag.SetEscalationReason("checkpoint")

		stall := classifyStall(ag, nil)
		if !stall.Stalled || stall.RateLimited || stall.MalformedTool || stall.ToolUseAborted || stall.StopStalled || !stall.CheckpointStopped {
			t.Fatalf("classifyStall(checkpoint) = stalled=%v rateLimited=%v malformedTool=%v toolUseAborted=%v stopStalled=%v checkpointStopped=%v",
				stall.Stalled, stall.RateLimited, stall.MalformedTool, stall.ToolUseAborted, stall.StopStalled, stall.CheckpointStopped)
		}
	})

	t.Run("checkpoint_failed is not a stall", func(t *testing.T) {
		ag := &agent.Agent{}
		ag.MarkStopped()
		ag.SetEscalationReason("checkpoint_failed")

		stall := classifyStall(ag, errors.New("checkpoint commit failed"))
		if stall.Stalled || stall.RateLimited || stall.MalformedTool || stall.ToolUseAborted || stall.StopStalled || stall.CheckpointStopped {
			t.Fatalf("classifyStall(checkpoint_failed) = stalled=%v rateLimited=%v malformedTool=%v toolUseAborted=%v stopStalled=%v checkpointStopped=%v",
				stall.Stalled, stall.RateLimited, stall.MalformedTool, stall.ToolUseAborted, stall.StopStalled, stall.CheckpointStopped)
		}
	})

	t.Run("malformed tool call stalls for retry", func(t *testing.T) {
		ag := &agent.Agent{}
		ag.SetError("malformed_tool_call", "tool rejected malformed input")

		stall := classifyStall(ag, nil)
		if !stall.Stalled || stall.RateLimited || !stall.MalformedTool || stall.ToolUseAborted || stall.StopStalled || stall.CheckpointStopped {
			t.Fatalf("classifyStall(malformed_tool_call) = stalled=%v rateLimited=%v malformedTool=%v toolUseAborted=%v stopStalled=%v checkpointStopped=%v",
				stall.Stalled, stall.RateLimited, stall.MalformedTool, stall.ToolUseAborted, stall.StopStalled, stall.CheckpointStopped)
		}
	})

	// A silent hang carries no verdict either, and the watchdog no longer
	// borrows the rate-limit kind to reach this branch (#3154), so the
	// disposition has to recognize it on its own or the run stops being
	// re-dispatched at all.
	t.Run("silent hang stalls for retry without claiming a rate limit", func(t *testing.T) {
		ag := &agent.Agent{}
		ag.SetError(agent.ErrorKindSilentHang, watchdogreason.ZeroOutputBeforeStartup)

		stall := classifyStall(ag, nil)
		if !stall.Stalled || !stall.SilentHang || stall.RateLimited || stall.MalformedTool || stall.ToolUseAborted || stall.CheckpointStopped {
			t.Fatalf("classifyStall(silent_hang) = stalled=%v silentHang=%v rateLimited=%v malformedTool=%v toolUseAborted=%v checkpointStopped=%v",
				stall.Stalled, stall.SilentHang, stall.RateLimited, stall.MalformedTool, stall.ToolUseAborted, stall.CheckpointStopped)
		}
	})

	// An initial prompt that never reached the child leaves a run holding no
	// verdict about anything, so it must be re-dispatched rather than counted.
	t.Run("undelivered prompt stalls for retry", func(t *testing.T) {
		ag := &agent.Agent{}
		ag.SetError(agent.ErrorKindPromptUndelivered, "write stdin: timed out after 2m0s, pipe closed")

		stall := classifyStall(ag, errors.New("deliver initial prompt: write stdin: timed out after 2m0s, pipe closed"))
		if !stall.Stalled || !stall.PromptUndelivered || stall.RateLimited || stall.MalformedTool || stall.ToolUseAborted || stall.CheckpointStopped {
			t.Fatalf("classifyStall(prompt_undelivered) = stalled=%v promptUndelivered=%v rateLimited=%v malformedTool=%v toolUseAborted=%v checkpointStopped=%v",
				stall.Stalled, stall.PromptUndelivered, stall.RateLimited, stall.MalformedTool, stall.ToolUseAborted, stall.CheckpointStopped)
		}
	})

	t.Run("undelivered prompt is not a definitive outcome", func(t *testing.T) {
		ag := &agent.Agent{}
		ag.SetError(agent.ErrorKindPromptUndelivered, "write stdin: timed out after 2m0s, pipe closed")

		if got := runTerminalOutcome(ag, errors.New("deliver initial prompt: timed out")); got != "" {
			t.Fatalf("runTerminalOutcome(prompt_undelivered) = %q, want empty", got)
		}
	})

	t.Run("tool use aborted stalls for retry", func(t *testing.T) {
		ag := &agent.Agent{}
		ag.SetError(agent.ErrorKindToolUseAborted, "provider run aborted after tool use was rejected")

		stall := classifyStall(ag, errors.New("provider result error provider_error"))
		if !stall.Stalled || stall.RateLimited || stall.MalformedTool || !stall.ToolUseAborted || stall.UserInterrupted || stall.StopStalled || stall.CheckpointStopped {
			t.Fatalf("classifyStall(tool_use_aborted) = stalled=%v rateLimited=%v malformedTool=%v toolUseAborted=%v userInterrupted=%v stopStalled=%v checkpointStopped=%v",
				stall.Stalled, stall.RateLimited, stall.MalformedTool, stall.ToolUseAborted, stall.UserInterrupted, stall.StopStalled, stall.CheckpointStopped)
		}
	})

	t.Run("user interrupted stalls for retry", func(t *testing.T) {
		ag := &agent.Agent{}
		ag.SetError(agent.ErrorKindUserInterrupted, "provider run was interrupted by the user before completion")

		stall := classifyStall(ag, errors.New("provider result error provider_error"))
		if !stall.Stalled || stall.RateLimited || stall.MalformedTool || stall.ToolUseAborted || !stall.UserInterrupted || stall.StopStalled || stall.CheckpointStopped {
			t.Fatalf("classifyStall(user_interrupted) = stalled=%v rateLimited=%v malformedTool=%v toolUseAborted=%v userInterrupted=%v stopStalled=%v checkpointStopped=%v",
				stall.Stalled, stall.RateLimited, stall.MalformedTool, stall.ToolUseAborted, stall.UserInterrupted, stall.StopStalled, stall.CheckpointStopped)
		}
	})
}

func TestOnComplete_ImportsTestRunnerEvidenceBeforeTerminalStatus(t *testing.T) {
	taskMgr := newMinimalTaskManager(t)
	wt := t.TempDir()
	tk, err := taskMgr.Create("visual test", "", "headless")
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	status := task.StatusTesting
	tk, err = taskMgr.Update(tk.ID, task.Update{
		Status:      &status,
		WorktreeDir: &wt,
	})
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	evidenceDir := filepath.Join(wt, worktree.EvidenceDirName)
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	shotPath := filepath.Join(evidenceDir, "shot.png")
	if err := os.WriteFile(shotPath, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	artifactStore := artifact.New(t.TempDir())
	h := &Handler{
		logger:    discardLogger(),
		tasks:     taskMgr,
		worktrees: worktree.New(worktree.Config{WorktreesDir: t.TempDir(), Tasks: taskMgr}),
		artifacts: artifactStore,
	}
	ag := &agent.Agent{
		ID:        "agent-1",
		TaskID:    tk.ID,
		Name:      agent.RoleTestRunner.AgentName(tk.Title),
		Mode:      "headless",
		StartedAt: time.Now().Add(-time.Second),
	}

	h.OnComplete(ag)

	updated, err := taskMgr.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if updated.Status != task.StatusTesting {
		t.Fatalf("task status = %q, want non-terminal %q", updated.Status, task.StatusTesting)
	}
	allMetas, err := artifactStore.List(tk.ID)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	var metas []artifact.Meta
	for _, m := range allMetas {
		if m.Kind == artifact.KindGeneric {
			metas = append(metas, m)
		}
	}
	if len(metas) != 1 {
		t.Fatalf("expected test-runner evidence to import before terminal status, got %d: %+v", len(metas), metas)
	}
	if metas[0].SourcePath != shotPath {
		t.Fatalf("SourcePath = %q, want %q", metas[0].SourcePath, shotPath)
	}
}

func TestOnComplete_DefersWorkflowUntilLockTimeoutRunUpdatePersists(t *testing.T) {
	taskMgr := newMinimalTaskManager(t)
	tk, err := taskMgr.Create("locked completion", "", "headless")
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if err := taskMgr.AddRun(tk.ID, task.AgentRun{
		AgentID:   "agent-lock",
		Role:      string(agent.RoleImplementation),
		Mode:      "headless",
		State:     string(agent.StateRunning),
		StartedAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}

	unlock, err := fsutil.LockFile(tk.FilePath)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	defer func() { _ = unlock() }()

	wf := &recordingCompletionWorkflow{completed: make(chan workflow.AgentCompletion, 1)}
	h := &Handler{
		logger:         discardLogger(),
		tasks:          taskMgr,
		workflowEngine: wf,
	}
	ag := &agent.Agent{
		ID:        "agent-lock",
		TaskID:    tk.ID,
		Name:      agent.RoleImplementation.AgentName(tk.Title),
		Mode:      "headless",
		State:     agent.StateStopped,
		StartedAt: time.Now().Add(-time.Second),
	}
	ag.AppendOutput(agent.StreamEvent{Type: "result", Content: "done after lock"})

	h.OnComplete(ag)

	select {
	case c := <-wf.completed:
		t.Fatalf("workflow completed before terminal run persisted: %+v", c)
	default:
	}

	if err := unlock(); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	unlock = func() error { return nil }

	select {
	case c := <-wf.completed:
		if c.AgentID != "agent-lock" || c.Result != "done after lock" || !c.Success {
			t.Fatalf("workflow completion = %+v, want persisted successful agent result", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deferred completion did not reach workflow")
	}

	got, err := taskMgr.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if got.AgentRuns[0].State != string(agent.StateStopped) {
		t.Fatalf("run state = %q, want %q", got.AgentRuns[0].State, agent.StateStopped)
	}
	if got.AgentRuns[0].Result != "done after lock" {
		t.Fatalf("run result = %q, want deferred result", got.AgentRuns[0].Result)
	}
}

func TestOnComplete_SalvagesCostStoppedReviewAssistantTranscript(t *testing.T) {
	taskMgr := newMinimalTaskManager(t)
	tk, err := taskMgr.Create("review task", "", "headless")
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if err := taskMgr.AddRun(tk.ID, task.AgentRun{
		AgentID:   "review-agent",
		Role:      string(agent.RoleReview),
		Mode:      "headless",
		State:     string(agent.StateRunning),
		StartedAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}

	h := &Handler{logger: discardLogger(), tasks: taskMgr}
	ag := &agent.Agent{
		ID:        "review-agent",
		TaskID:    tk.ID,
		Name:      agent.RoleReview.AgentName(tk.Title),
		Mode:      "headless",
		LogPath:   "/tmp/sybra/agents/review-agent.ndjson",
		StartedAt: time.Now().Add(-time.Second),
	}
	ag.SetEscalationReason("cost")
	ag.AppendOutput(agent.StreamEvent{Type: "assistant", Content: "confirmed finding: nil map write"})
	ag.AppendOutput(agent.StreamEvent{Type: "assistant", Content: "waiting for verifier"})
	ag.AppendOutput(agent.StreamEvent{Type: "result", Content: "verifier still running"})

	h.OnComplete(ag)

	got, err := taskMgr.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	for _, want := range []string{
		"# Interrupted Code Review",
		"confirmed finding: nil map write",
		"waiting for verifier",
		"/tmp/sybra/agents/review-agent.ndjson",
	} {
		if !strings.Contains(got.CodeReview, want) {
			t.Fatalf("CodeReview missing %q:\n%s", want, got.CodeReview)
		}
	}
	if got.AgentRuns[0].EscalationReason != "cost" {
		t.Fatalf("run EscalationReason = %q, want cost", got.AgentRuns[0].EscalationReason)
	}
}

func TestSalvageInterruptedReviewKeepsExistingReview(t *testing.T) {
	taskMgr := newMinimalTaskManager(t)
	tk, err := taskMgr.Create("review task", "", "headless")
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	existing := "# Existing review\n\nDo not overwrite."
	if _, err := taskMgr.Update(tk.ID, task.Update{CodeReview: &existing}); err != nil {
		t.Fatal(err)
	}

	h := &Handler{logger: discardLogger(), tasks: taskMgr}
	ag := &agent.Agent{
		ID:     "review-agent",
		TaskID: tk.ID,
		Name:   agent.RoleReview.AgentName(tk.Title),
	}
	ag.SetEscalationReason("cost")
	ag.AppendOutput(agent.StreamEvent{Type: "assistant", Content: "new interrupted text"})

	h.salvageInterruptedReview(ag)

	got, err := taskMgr.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if got.CodeReview != existing {
		t.Fatalf("CodeReview overwritten:\n got %q\nwant %q", got.CodeReview, existing)
	}
}

// TestLastAssistantText verifies that lastAssistantText returns the last
// non-empty assistant event's content without sybra-verdict gating, and that
// a populated result event is preferred when present (guards against the
// fallback overriding a real result).
func TestLastAssistantText(t *testing.T) {
	t.Parallel()

	makeAgent := func(events ...agent.StreamEvent) *agent.Agent {
		ag := &agent.Agent{}
		for _, ev := range events {
			ag.AppendOutput(ev)
		}
		return ag
	}

	t.Run("returns_last_assistant", func(t *testing.T) {
		ag := makeAgent(
			agent.StreamEvent{Type: "assistant", Content: "first message"},
			agent.StreamEvent{Type: "assistant", Content: "final message"},
		)
		got := lastAssistantText(ag)
		if got != "final message" {
			t.Errorf("lastAssistantText = %q, want %q", got, "final message")
		}
	})

	t.Run("skips_trailing_empty_assistant", func(t *testing.T) {
		ag := makeAgent(
			agent.StreamEvent{Type: "assistant", Content: `{"verdict":"PASS"}`},
			agent.StreamEvent{Type: "assistant", Content: ""},
		)
		got := lastAssistantText(ag)
		if got != `{"verdict":"PASS"}` {
			t.Errorf("lastAssistantText = %q, want JSON verdict", got)
		}
	})

	t.Run("terminal_result_fallback_skips_trailing_empty_assistant", func(t *testing.T) {
		ag := makeAgent(
			agent.StreamEvent{Type: "assistant", Content: `{"verdict":"PASS"}`},
			agent.StreamEvent{Type: "assistant", Content: ""},
			agent.StreamEvent{Type: "result", Content: ""},
		)
		got := terminalResultContent(ag)
		if got != `{"verdict":"PASS"}` {
			t.Errorf("terminalResultContent = %q, want JSON verdict", got)
		}
	})

	t.Run("no_assistant_events_returns_empty", func(t *testing.T) {
		ag := makeAgent(
			agent.StreamEvent{Type: "result", Content: "some result"},
		)
		got := lastAssistantText(ag)
		if got != "" {
			t.Errorf("lastAssistantText = %q, want empty", got)
		}
	})

	t.Run("no_events_returns_empty", func(t *testing.T) {
		ag := &agent.Agent{}
		if got := lastAssistantText(ag); got != "" {
			t.Errorf("lastAssistantText = %q, want empty", got)
		}
	})

	t.Run("skips_trailing_empty_assistant", func(t *testing.T) {
		ag := makeAgent(
			agent.StreamEvent{Type: "assistant", Content: `{"verdict":"FAIL"}`},
			agent.StreamEvent{Type: "assistant", Content: ""},
		)
		got := lastAssistantText(ag)
		if got != `{"verdict":"FAIL"}` {
			t.Errorf("lastAssistantText = %q, want JSON verdict", got)
		}
	})

	// B3 fallback: result event empty → falls back to last assistant text.
	t.Run("b3_fallback_result_empty", func(t *testing.T) {
		ag := makeAgent(
			agent.StreamEvent{Type: "assistant", Content: `{"verdict":"PASS"}`},
			agent.StreamEvent{Type: "result", Content: ""},
		)
		// Confirm lastAssistantText gives the assistant body.
		got := lastAssistantText(ag)
		if got != `{"verdict":"PASS"}` {
			t.Errorf("lastAssistantText = %q, want JSON verdict", got)
		}
	})

	// B3 preserved: populated result event must NOT be overridden.
	t.Run("b3_preserved_result_populated", func(t *testing.T) {
		ag := makeAgent(
			agent.StreamEvent{Type: "assistant", Content: "assistant text"},
			agent.StreamEvent{Type: "result", Content: "real result"},
		)
		// lastAssistantText itself just returns assistant content; the
		// OnComplete guard (hasResultEvent && resultContent == "") keeps result intact.
		// Verify the result event's content is accessible as-is.
		out := ag.Output()
		var resultContent string
		for i := range out {
			if out[i].Type == "result" {
				resultContent = out[i].Content
			}
		}
		if resultContent != "real result" {
			t.Errorf("result content = %q, want %q", resultContent, "real result")
		}
		// lastAssistantText returns assistant text — the guard in OnComplete
		// ensures this would not replace a non-empty result.
		if last := lastAssistantText(ag); last != "assistant text" {
			t.Errorf("lastAssistantText = %q, want %q", last, "assistant text")
		}
	})
}

func TestEstimatedRunCost(t *testing.T) {
	t.Parallel()

	t.Run("reported cost wins", func(t *testing.T) {
		t.Parallel()
		ag := &agent.Agent{Provider: "codex", Model: "gpt-5"}
		if got := estimatedRunCost(ag, 0.42, 9); got != 0.42 {
			t.Errorf("estimatedRunCost reported = %g, want 0.42", got)
		}
	})

	t.Run("codex estimates from tokens", func(t *testing.T) {
		t.Parallel()
		ag := &agent.Agent{Provider: "codex", Model: "gpt-5"}
		ag.AddResultStats("", 0, 1_000_000, 100_000, 0)
		ag.AddCacheStats(0, 800_000)
		if got, want := estimatedRunCost(ag, 0, 0), 1.35; got != want {
			t.Errorf("estimatedRunCost codex = %g, want %g", got, want)
		}
	})

	t.Run("copilot estimates from credits", func(t *testing.T) {
		t.Parallel()
		ag := &agent.Agent{Provider: "copilot"}
		ag.AddPremiumRequests(7.5)
		if got, want := estimatedRunCost(ag, 0, ag.GetPremiumRequests()), 0.075; got != want {
			t.Errorf("estimatedRunCost copilot = %g, want %g", got, want)
		}
	})

	t.Run("uses started time for dated pricing", func(t *testing.T) {
		t.Parallel()
		ag := &agent.Agent{
			Provider:  "codex",
			Model:     "claude-sonnet-5",
			StartedAt: time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC),
		}
		ag.AddResultStats("", 0, 1_000_000, 1_000_000, 0)
		if got, want := estimatedRunCost(ag, 0, 0), 12.0; got != want {
			t.Errorf("estimatedRunCost dated = %g, want %g", got, want)
		}
	})
}

// itoa converts a small non-negative int to its decimal string representation
// without importing strconv (avoids an extra import just for test helpers).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
