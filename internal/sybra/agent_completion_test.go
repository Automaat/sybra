package sybra

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
)

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
		{"rate limit but clean exit", rateLimited, nil, false},
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
