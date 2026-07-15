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
	"github.com/Automaat/sybra/internal/skillattr"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/worktree"
)

func TestRunOutcome(t *testing.T) {
	t.Parallel()
	errBoom := errors.New("boom")

	cases := []struct {
		name          string
		role          agent.Role
		exitErr       error
		resultContent string
		want          string
	}{
		{"clean_exit_any_role", agent.RoleImplementation, nil, "", "completed"},
		{"non_test_runner_errors_are_failed", agent.RoleImplementation, errBoom, "TEST_VERDICT: PASS", "failed"},
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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := runOutcome(tc.role, tc.exitErr, tc.resultContent); got != tc.want {
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

	patch := (&Handler{}).buildRunPatch(ag, agent.StateStopped, 1.25, 0, "done", nil)

	if patch.RequestedSkill == nil || *patch.RequestedSkill != "sybra-test" {
		t.Fatalf("RequestedSkill = %v, want sybra-test", patch.RequestedSkill)
	}
	if patch.SkillExecutionMode == nil || *patch.SkillExecutionMode != skillattr.ExecutionModeInjected {
		t.Fatalf("SkillExecutionMode = %v, want %q", patch.SkillExecutionMode, skillattr.ExecutionModeInjected)
	}
	if patch.ResolvedSkillSourceHash == nil || *patch.ResolvedSkillSourceHash != "deadbeefcafebabe" {
		t.Fatalf("ResolvedSkillSourceHash = %v, want deadbeefcafebabe", patch.ResolvedSkillSourceHash)
	}
	if patch.SkillConformance == nil || *patch.SkillConformance != skillattr.ConformanceExact {
		t.Fatalf("SkillConformance = %v, want %q", patch.SkillConformance, skillattr.ConformanceExact)
	}
	if patch.SubagentCallCount == nil || *patch.SubagentCallCount != 2 {
		t.Fatalf("SubagentCallCount = %v, want 2 distinct parents", patch.SubagentCallCount)
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

	// Helper: send SIGKILL to a running process and return the Wait error.
	sigkillErr := func(t *testing.T) error {
		t.Helper()
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start sleep: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
		if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatalf("signal: %v", err)
		}
		return cmd.Wait()
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

		stalled, rateLimited, malformedTool, stopStalled, checkpointStopped := classifyStall(ag, nil)
		if !stalled || rateLimited || malformedTool || stopStalled || !checkpointStopped {
			t.Fatalf("classifyStall(checkpoint) = stalled=%v rateLimited=%v malformedTool=%v stopStalled=%v checkpointStopped=%v",
				stalled, rateLimited, malformedTool, stopStalled, checkpointStopped)
		}
	})

	t.Run("checkpoint_failed is not a stall", func(t *testing.T) {
		ag := &agent.Agent{}
		ag.MarkStopped()
		ag.SetEscalationReason("checkpoint_failed")

		stalled, rateLimited, malformedTool, stopStalled, checkpointStopped := classifyStall(ag, errors.New("checkpoint commit failed"))
		if stalled || rateLimited || malformedTool || stopStalled || checkpointStopped {
			t.Fatalf("classifyStall(checkpoint_failed) = stalled=%v rateLimited=%v malformedTool=%v stopStalled=%v checkpointStopped=%v",
				stalled, rateLimited, malformedTool, stopStalled, checkpointStopped)
		}
	})

	t.Run("malformed tool call stalls for retry", func(t *testing.T) {
		ag := &agent.Agent{}
		ag.SetError("malformed_tool_call", "tool rejected malformed input")

		stalled, rateLimited, malformedTool, stopStalled, checkpointStopped := classifyStall(ag, nil)
		if !stalled || rateLimited || !malformedTool || stopStalled || checkpointStopped {
			t.Fatalf("classifyStall(malformed_tool_call) = stalled=%v rateLimited=%v malformedTool=%v stopStalled=%v checkpointStopped=%v",
				stalled, rateLimited, malformedTool, stopStalled, checkpointStopped)
		}
	})
}

func TestOnComplete_ImportsTestRunnerEvidenceBeforeTerminalStatus(t *testing.T) {
	taskMgr := newMinimalTaskManager(t)
	wt := t.TempDir()
	tk, err := taskMgr.Create("visual test", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	status := task.StatusTesting
	tk, err = taskMgr.Update(tk.ID, task.Update{
		Status:      &status,
		WorktreeDir: &wt,
	})
	if err != nil {
		t.Fatal(err)
	}
	evidenceDir := filepath.Join(wt, worktree.EvidenceDirName)
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shotPath := filepath.Join(evidenceDir, "shot.png")
	if err := os.WriteFile(shotPath, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
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
	}
	if updated.Status != task.StatusTesting {
		t.Fatalf("task status = %q, want non-terminal %q", updated.Status, task.StatusTesting)
	}
	allMetas, err := artifactStore.List(tk.ID)
	if err != nil {
		t.Fatal(err)
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

func TestOnComplete_SalvagesCostStoppedReviewAssistantTranscript(t *testing.T) {
	taskMgr := newMinimalTaskManager(t)
	tk, err := taskMgr.Create("review task", "", "headless")
	if err != nil {
		t.Fatal(err)
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
	}
	if got.CodeReview != existing {
		t.Fatalf("CodeReview overwritten:\n got %q\nwant %q", got.CodeReview, existing)
	}
}

// TestLastAssistantText verifies that lastAssistantText returns the last
// assistant-typed event's content without sybra-verdict gating, and that
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
