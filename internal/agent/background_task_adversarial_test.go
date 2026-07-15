package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestAdversarial_LiveBackgroundTaskAtResultDoesNotCompleteCleanly is an
// adversarial regression test for task 3aeabb65: a headless CLI process
// exits as soon as it emits its final result, which kills any
// `run_in_background` bash task still running at that point and can leave
// the worktree silently corrupted (e.g. a killed `npm ci`). A run that ends
// this way must not be reported as a clean completion — Sybra must not hand
// the worktree off to the next workflow step as if nothing happened.
//
// The fake claude binary reports a live background task and then exits
// immediately after its terminal result, without ever clearing the task —
// simulating a provider that ignores the backgroundTaskGuardrail prompt
// instruction (see headless_background_guardrail.go).
func TestAdversarial_LiveBackgroundTaskAtResultDoesNotCompleteCleanly(t *testing.T) {
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' '{\"type\":\"system\",\"subtype\":\"background_tasks_changed\",\"session_id\":\"s-bg\"," +
		"\"tasks\":[{\"task_id\":\"bg1\",\"task_type\":\"bash\",\"description\":\"npm ci\"}]}'\n" +
		"printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"session_id\":\"s-bg\",\"total_cost_usd\":0," +
		"\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	if err := os.WriteFile(fakeClaude, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var logBuf logCapture
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	m := mustNewManager(t, context.Background(), func(string, any) {}, logger, t.TempDir(), ManagerConfig{
		Runtime:           ManagerRuntimeConfig{DefaultProvider: "claude"},
		SurviveRestartDir: t.TempDir(),
	})

	ag, err := m.Run(RunConfig{
		TaskID:             "task-bg",
		Name:               "implementation: bg",
		Mode:               "headless",
		Prompt:             "leave a background task running",
		Dir:                t.TempDir(),
		RequirePermissions: false,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	waitForAgentDone(t, ag, 5*time.Second)

	if ag.GetState() == StateStopped && ag.GetExitErr() == nil && ag.HasBackgroundTasks() {
		t.Fatalf("agent completed cleanly despite live background task: state=%s exitErr=%v hasBackgroundTasks=%v logs=%s",
			ag.GetState(), ag.GetExitErr(), ag.HasBackgroundTasks(), logBuf.String())
	}
	if !errors.Is(ag.GetExitErr(), errBackgroundTaskLiveAtExit) {
		t.Fatalf("ExitErr = %v, want errBackgroundTaskLiveAtExit", ag.GetExitErr())
	}
}

// TestAdversarial_LiveBackgroundTaskAtConvoOneShotExitDoesNotCompleteCleanly
// is the interactive-OneShot counterpart to
// TestAdversarial_LiveBackgroundTaskAtResultDoesNotCompleteCleanly, regression
// testing task e150a89b's lost_agent incident (monitor investigation
// 73474d71): an interactive OneShot implementation run's final turn deferred
// `make check` to the background ("I'll report back once it finishes") and
// ended anyway, since OneShot dispatch is a single detached conversational
// turn that exits exactly like headless once the result event lands —
// killing the deferred check and stranding the run as a "lost agent" instead
// of a loud, marked failure.
//
// The fake claude binary reports a live background task and then exits
// immediately after its terminal result, without ever clearing the task —
// simulating a provider that ignores the backgroundTaskGuardrail prompt
// instruction (see headless_background_guardrail.go).
func TestAdversarial_LiveBackgroundTaskAtConvoOneShotExitDoesNotCompleteCleanly(t *testing.T) {
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' '{\"type\":\"system\",\"subtype\":\"background_tasks_changed\",\"session_id\":\"s-bg\"," +
		"\"tasks\":[{\"task_id\":\"bg1\",\"task_type\":\"bash\",\"description\":\"make check\"}]}'\n" +
		"printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"session_id\":\"s-bg\",\"total_cost_usd\":0," +
		"\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	if err := os.WriteFile(fakeClaude, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var logBuf logCapture
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	m := mustNewManager(t, context.Background(), func(string, any) {}, logger, t.TempDir(), ManagerConfig{
		Runtime:           ManagerRuntimeConfig{DefaultProvider: "claude"},
		SurviveRestartDir: t.TempDir(),
	})

	ag, err := m.Run(RunConfig{
		TaskID:             "task-bg-convo",
		Name:               "implementation: bg convo",
		Mode:               "interactive",
		OneShot:            true,
		Prompt:             "leave a background task running",
		Dir:                t.TempDir(),
		RequirePermissions: false,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	waitForAgentDone(t, ag, 5*time.Second)

	if ag.GetState() == StateStopped && ag.GetExitErr() == nil && ag.HasBackgroundTasks() {
		t.Fatalf("agent completed cleanly despite live background task: state=%s exitErr=%v hasBackgroundTasks=%v logs=%s",
			ag.GetState(), ag.GetExitErr(), ag.HasBackgroundTasks(), logBuf.String())
	}
	if !ag.HasBackgroundTasks() {
		t.Fatal("expected the convo runner to track the reported background task")
	}
	if !errors.Is(ag.GetExitErr(), errBackgroundTaskLiveAtExit) {
		t.Fatalf("ExitErr = %v, want errBackgroundTaskLiveAtExit", ag.GetExitErr())
	}
}

// TestCheckLiveBackgroundTasksAtExit_IntentionalStopIsClean guards the
// exemption that keeps a normal StopAgent from being mislabeled as a failed
// run. A regular multi-turn interactive session can hold a live CLI
// background task across turns; stopping it closes stdin and produces a clean
// process exit, which must NOT be reported as errBackgroundTaskLiveAtExit —
// the guardrail (backgroundTaskGuardrail prompt) only targets one-shot exit
// shapes and was never injected into that session.
func TestCheckLiveBackgroundTasksAtExit_IntentionalStopIsClean(t *testing.T) {
	m := &Manager{logger: discardLogger()}

	live := &Agent{}
	live.SetBackgroundTaskIDs([]string{"bg1"})

	// Sanity: without a stop, a live background task at exit is still a failure.
	if err := checkLiveBackgroundTasksAtExit(m, live); !errors.Is(err, errBackgroundTaskLiveAtExit) {
		t.Fatalf("live task, not stopped: got %v, want errBackgroundTaskLiveAtExit", err)
	}

	live.MarkStopped()
	if err := checkLiveBackgroundTasksAtExit(m, live); err != nil {
		t.Fatalf("live task, intentionally stopped: got %v, want nil", err)
	}
}

// logCapture is a minimal concurrency-safe io.Writer for slog output,
// avoiding a data race between the runner goroutine's logging and the test
// goroutine's failure-message formatting.
type logCapture struct {
	mu  sync.Mutex
	buf []byte
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	return len(p), nil
}

func (l *logCapture) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.buf)
}
